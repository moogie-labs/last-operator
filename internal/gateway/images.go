package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	// registryFrontService is the bundled registry's front proxy Service, in
	// Config.RegistryNamespace. Build pods may reach it.
	registryFrontService = "last-registry-front"
	// registryPusherSecret holds the push-only credential (keys username, password)
	// in the gateway namespace.
	registryPusherSecret = "last-registry-pusher"
	// headCacheTTL is how long a successful manifest HEAD is trusted.
	headCacheTTL = 10 * time.Minute
	// maxLogTail bounds the build log a failed build hands back to the caller.
	maxLogTail = 64 << 10

	buildFailed     = "failed"
	unpinnedBaseMsg = "image builds need OperatorConfig.sandbox.baseImage pinned by digest"
)

// imageBuilder runs build pods; images.Builder implements it, and tests inject fakes.
type imageBuilder interface {
	Build(ctx context.Context, s images.BuildSpec) (images.BuildOutcome, error)
	ReapOrphans(ctx context.Context, all bool) (int, error)
}

// imageRequest names who needs an image, for the build's audit entries.
type imageRequest struct {
	Intent, Stage, Subject, Actor string
	Install                       []lastv1alpha1.ArgvCommand
	Policy                        policy.Effective
}

// errImageBuildFailed carries a failed build's reason and its log tail (the last 64 KiB).
type errImageBuildFailed struct{ Reason, LogTail string }

func (e errImageBuildFailed) Error() string { return "ImageBuildFailed: " + e.Reason }

// buildStart is the image.build.start audit payload.
type buildStart struct {
	SpecHash      string `json:"specHash"`
	BaseImage     string `json:"baseImage"`
	Containerfile string `json:"containerfile"`
	PushRef       string `json:"pushRef"`
	// Egress is the build pod's grants: the registry rules, the allowed buildEgress
	// entries, and kubeDNSGrant for the DNS rule the build objects add.
	Egress []string `json:"egress"`
}

// kubeDNSGrant names the kube-dns rule (UDP and TCP 53) in image.build.start.
const kubeDNSGrant = "kube-dns:53"

func ruleStrings(rules []sandbox.EgressRule) []string {
	out := make([]string, 0, len(rules)+1)
	for _, r := range rules {
		out = append(out, r.String())
	}
	return out
}

// imageFlight is one resolve of a spec hash that callers share. It runs detached
// from any one caller and is cancelled only when every caller has gone.
type imageFlight struct {
	done   chan struct{}
	ref    string
	err    error
	refs   int // callers still waiting; guarded by imageBuilds.mu
	cancel context.CancelFunc
}

// imageBuilds is the resolver's state: in-flight resolves per spec hash, the build
// slots, and the cache of successful manifest HEADs.
type imageBuilds struct {
	now func() time.Time
	// track, when set, counts each detached flight so the gateway's RPC drain waits
	// for it. Only a caller already counted there starts a flight, so the counter is
	// above zero when a flight is added.
	track *sync.WaitGroup

	mu      sync.Mutex
	flights map[string]*imageFlight
	stopped bool // set by stop: no new flight starts
	sem     chan struct{}
	heads   map[string]time.Time // digest -> when its HEAD must be repeated
	// active counts builds holding a slot, of whichever semaphore they took it from.
	active int
	// paused is set by pause: acquire waits until resumed is closed. idle, when set,
	// is closed as active drops to zero.
	paused  bool
	resumed chan struct{}
	idle    chan struct{}
}

func newImageBuilds() *imageBuilds {
	return &imageBuilds{now: time.Now, flights: map[string]*imageFlight{},
		sem: make(chan struct{}, images.DefaultMaxConcurrentBuilds), heads: map[string]time.Time{}}
}

// resize replaces the build slots; builds holding a slot of the old set release it there.
func (b *imageBuilds) resize(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sem = make(chan struct{}, n)
}

func (b *imageBuilds) slots() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sem
}

// acquire waits for a build slot, and for any pause to end, and returns its release.
func (b *imageBuilds) acquire(ctx context.Context) (func(), error) {
	for {
		if err := b.waitResumed(ctx); err != nil {
			return nil, err
		}
		sem := b.slots()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		b.mu.Lock()
		if !b.paused {
			b.active++
			b.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { <-sem; b.finished() }) }, nil
		}
		b.mu.Unlock()
		<-sem // a pause began while this build waited for the slot: give it back and wait
	}
}

func (b *imageBuilds) waitResumed(ctx context.Context) error {
	b.mu.Lock()
	for b.paused {
		resumed := b.resumed
		b.mu.Unlock()
		select {
		case <-resumed:
		case <-ctx.Done():
			return ctx.Err()
		}
		b.mu.Lock()
	}
	b.mu.Unlock()
	return nil
}

func (b *imageBuilds) finished() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active--
	if b.active == 0 && b.idle != nil {
		close(b.idle)
		b.idle = nil
	}
}

// pause stops new builds from starting (they wait in acquire) and waits until every
// running build has released its slot. It counts builds, not slots, so a resize
// during a build or a pause changes nothing. On success the caller must call resume,
// which is safe to call more than once; on error builds are already resumed.
func (b *imageBuilds) pause(ctx context.Context) (resume func(), err error) {
	b.mu.Lock()
	if b.paused {
		b.mu.Unlock()
		return nil, errors.New("image builds are already paused")
	}
	b.paused, b.resumed = true, make(chan struct{})
	var idle chan struct{}
	if b.active > 0 {
		b.idle = make(chan struct{})
		idle = b.idle
	}
	b.mu.Unlock()
	var once sync.Once
	resume = func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.paused, b.idle = false, nil
			close(b.resumed)
		})
	}
	if idle != nil {
		select {
		case <-idle:
		case <-ctx.Done():
			resume()
			return nil, ctx.Err()
		}
	}
	return resume, nil
}

func (b *imageBuilds) headCached(digest string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	exp, ok := b.heads[digest]
	return ok && b.now().Before(exp)
}

func (b *imageBuilds) cacheHead(digest string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for d, exp := range b.heads {
		if !now.Before(exp) {
			delete(b.heads, d)
		}
	}
	b.heads[digest] = now.Add(headCacheTTL)
}

// forgetHead drops a cached HEAD, for a manifest the gateway deleted.
func (b *imageBuilds) forgetHead(digest string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.heads, digest)
}

// do runs fn once per key at a time: callers that arrive while it runs share its
// result (single flight). fn runs on its own goroutine, on a context that keeps the
// first caller's values but not its cancellation; it is cancelled only when the last
// waiting caller has gone. A caller whose own context ends stops waiting and gets
// its own context error; the others are unaffected.
func (b *imageBuilds) do(ctx context.Context, key string, fn func(context.Context) (string, error)) (string, error) {
	b.mu.Lock()
	f, ok := b.flights[key]
	if !ok && b.stopped {
		b.mu.Unlock()
		return "", status.Error(codes.Unavailable, "image resolve refused: the gateway is stepping down")
	}
	if !ok {
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &imageFlight{done: make(chan struct{}), cancel: cancel}
		b.flights[key] = f
		if b.track != nil {
			b.track.Add(1)
		}
		go b.run(fctx, key, f, fn)
	}
	f.refs++
	b.mu.Unlock()
	select {
	case <-f.done:
		return f.ref, f.err
	case <-ctx.Done():
		b.leave(key, f)
		return "", status.FromContextError(ctx.Err()).Err()
	}
}

func (b *imageBuilds) run(ctx context.Context, key string, f *imageFlight, fn func(context.Context) (string, error)) {
	defer func() {
		if b.track != nil {
			defer b.track.Done()
		}
		b.mu.Lock()
		if b.flights[key] == f {
			delete(b.flights, key)
		}
		b.mu.Unlock()
		f.cancel()
		close(f.done)
	}()
	f.ref, f.err = fn(ctx)
	if f.err != nil && ctx.Err() != nil && status.Code(f.err) != codes.Unavailable {
		// Only reachable when every caller has gone; keep §7's Unavailable for any
		// caller that might still read it.
		f.err = status.Errorf(codes.Unavailable, "image resolve cancelled: %v", f.err)
	}
}

// stop cancels every flight and refuses new ones. The gateway calls it when it
// starts to drain, so each build deletes its objects and records a failed
// image.build.result while the writer is still there, instead of running past
// the drain window and leaving image.build.start without a result.
func (b *imageBuilds) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	for _, f := range b.flights {
		f.cancel()
	}
}

// leave drops one caller from the flight; the last one cancels it and removes it,
// so a later call starts afresh.
func (b *imageBuilds) leave(key string, f *imageFlight) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f.refs--
	if f.refs == 0 {
		f.cancel()
		if b.flights[key] == f {
			delete(b.flights, key)
		}
	}
}

// resolveImage returns the reference a command runs on: the base when install is empty,
// else "<pullHost>/last/images@<digest>", building it (single flight, capped) when needed.
// gRPC errors: FailedPrecondition (unpinned base, external mode, hostname in buildEgress,
// a grant covering a canary), Unavailable (registry or Kubernetes errors);
// errImageBuildFailed for a failed build.
func (g *Gateway) resolveImage(ctx context.Context, w *audit.Writer, req imageRequest) (string, error) {
	base, err := g.sandboxImage(ctx)
	if err != nil {
		return "", err
	}
	return g.resolveImageOn(ctx, w, base, req)
}

// resolveImageOn is resolveImage for a sandbox base the caller already read.
func (g *Gateway) resolveImageOn(ctx context.Context, w *audit.Writer, base string, req imageRequest) (string, error) {
	if len(req.Install) == 0 {
		return base, nil
	}
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return "", err
	}
	if op != nil && op.Spec.Registry != nil && op.Spec.Registry.Mode == lastv1alpha1.RegistryExternal {
		return "", status.Error(codes.FailedPrecondition, "image builds do not support registry.mode external yet; use the bundled registry")
	}
	baseDigest, err := images.BaseDigest(base)
	if err != nil {
		return "", status.Error(codes.FailedPrecondition, unpinnedBaseMsg)
	}
	specHash, err := images.SpecHash(baseDigest, req.Install)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "image spec: %v", err)
	}
	return g.builds.do(ctx, specHash, func(ctx context.Context) (string, error) {
		if e, ok := g.images.get(specHash); ok {
			usable, err := g.checkIndexed(ctx, specHash, e)
			if err != nil {
				return "", err
			}
			// The touch succeeds only while the entry is still indexed: once image GC
			// drops an entry it deletes the manifest, so a resolve that lost that race
			// rebuilds, and an entry touched first is no longer unused, so GC keeps it.
			if usable && g.images.touchIfIndexed(specHash, e.Digest, g.builds.now()) {
				return g.runRef(e.Digest), nil
			}
		}
		return g.build(ctx, w, req, op, base, specHash)
	})
}

// checkIndexed reports whether an index entry may run: its build entry verifies and
// the registry still has the manifest. An entry that fails either check is dropped.
func (g *Gateway) checkIndexed(ctx context.Context, specHash string, e imageEntry) (bool, error) {
	if err := g.verifyBuild(ctx, specHash, e); err != nil {
		if errors.Is(err, errAuditUnreadable) {
			return false, status.Errorf(codes.Unavailable, "check the image index entry for %s: %v", specHash, err)
		}
		slog.Error("Image index entry failed verification; rebuilding", "specHash", specHash,
			"digest", e.Digest, "buildSeq", e.BuildSeq, "err", err)
		g.dropImage(ctx, specHash)
		return false, nil
	}
	if g.builds.headCached(e.Digest) {
		return true, nil
	}
	found, err := g.registry.Head(ctx, images.ImagesRepo, e.Digest)
	if err != nil {
		return false, status.Errorf(codes.Unavailable, "check image %s in the registry: %v", e.Digest, err)
	}
	if !found {
		slog.Info("Image manifest is gone from the registry; rebuilding", "specHash", specHash, "digest", e.Digest)
		g.dropImage(ctx, specHash)
		return false, nil
	}
	g.builds.cacheHead(e.Digest)
	return true, nil
}

// dropImage removes an unusable index entry. A failed save is only logged: the
// rebuild's put replaces the entry, and verification guards whatever stays saved.
func (g *Gateway) dropImage(ctx context.Context, specHash string) {
	if err := g.images.drop(context.WithoutCancel(ctx), specHash); err != nil {
		slog.Warn("Could not save the image index after dropping an entry", "specHash", specHash, "err", err)
	}
}

// build runs one audited build of specHash and indexes the digest it produced.
func (g *Gateway) build(ctx context.Context, w *audit.Writer, req imageRequest, op *lastv1alpha1.OperatorConfig, base, specHash string) (string, error) {
	// Configuration refusals come before the slot, so they never queue behind builds.
	egress, err := g.buildEgress(ctx, op, req.Policy)
	if err != nil {
		return "", err
	}
	user, password, err := g.pusherCredential(ctx)
	if err != nil {
		return "", err
	}
	release, err := g.builds.acquire(ctx)
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "wait for a build slot: %v", err)
	}
	defer release()
	cfg := g.buildPodConfig(op, base)
	containerfile, err := images.Containerfile(cfg.BuildBase(base), req.Install)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "image install: %v", err)
	}
	spec := images.BuildSpec{ID: randomBuildID(), SpecHash: specHash, Base: base, Install: req.Install,
		PushConfig: images.DockerConfig(g.cfg.RegistryFrontHost, user, password), Egress: egress}
	builder := g.newBuilder(cfg)

	draft := func(typ string, payload any) audit.Draft {
		return audit.Draft{Type: typ, Actor: req.Actor, Intent: req.Intent, Stage: req.Stage,
			SubjectHash: req.Subject, Payload: payload}
	}
	var out images.BuildOutcome
	var failure string // why the build failed; "" when it produced a digest
	buildCtx := ctx
	// The result entry must be written even when the request was cancelled, so the
	// audit calls ignore cancellation while the build itself still honors it.
	result, err := w.DoEntry(context.WithoutCancel(ctx),
		draft(audit.TypeImageBuildStart, buildStart{SpecHash: specHash, BaseImage: base, Containerfile: containerfile,
			PushRef: cfg.FrontHost + "/" + images.ImagesRepo + ":" + images.Tag(specHash),
			Egress:  append(ruleStrings(egress), kubeDNSGrant)}),
		func(context.Context) (any, error) {
			var buildErr error
			out, buildErr = builder.Build(buildCtx, spec)
			return nil, buildErr
		},
		func(start audit.Entry, _ any, actionErr error) audit.Draft {
			r := buildResult{SpecHash: specHash, StartSeq: start.Seq, Outcome: buildSucceeded, Digest: out.Digest,
				DurationMs: out.Duration.Milliseconds(), Log: out.Log, LogTruncated: out.LogTruncated}
			switch {
			case actionErr != nil:
				r.Error = actionErr.Error()
			case out.Err != nil:
				failure = out.Err.Error()
			case out.Digest == "":
				failure = "build reported no digest"
			}
			if r.Error == "" {
				r.Error = failure
			}
			if r.Error != "" {
				r.Outcome, r.Digest = buildFailed, ""
			}
			return draft(audit.TypeImageBuildResult, r)
		})
	if err != nil {
		// The build's outcome is unknown (Kubernetes, registry or audit errors, or
		// cancellation); the caller may retry.
		return "", status.Errorf(codes.Unavailable, "image build %s: %v", spec.ID, err)
	}
	if failure != "" {
		return "", errImageBuildFailed{Reason: failure, LogTail: logTail(out.Log)}
	}
	if err := g.images.put(context.WithoutCancel(ctx), specHash,
		imageEntry{Digest: out.Digest, BuildSeq: result.Seq, LastUsed: g.builds.now()}); err != nil {
		// The entry is kept in memory, so this process reuses it; another rebuilds.
		slog.Warn("Could not save the image index", "specHash", specHash, "err", err)
	}
	g.builds.cacheHead(out.Digest)
	return g.runRef(out.Digest), nil
}

// buildEgress returns a build's egress grants: the registry front proxy's Service and
// endpoints, and each images.buildEgress entry the requesting Intent's policy allows.
// Hostname entries, and any grant covering an egress canary, are FailedPrecondition.
func (g *Gateway) buildEgress(ctx context.Context, op *lastv1alpha1.OperatorConfig, pol policy.Effective) ([]sandbox.EgressRule, error) {
	var allowed []sandbox.EgressRule
	if op != nil && op.Spec.Images != nil {
		for _, entry := range op.Spec.Images.BuildEgress {
			r, err := sandbox.ParseEgress(entry)
			if errors.Is(err, sandbox.ErrHostEgressUnsupported) {
				return nil, status.Errorf(codes.FailedPrecondition,
					"OperatorConfig images.buildEgress entry %q is a hostname; build egress takes IP and CIDR entries only", entry)
			}
			if err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "OperatorConfig images.buildEgress entry %q: %v", entry, err)
			}
			if !pol.AllowsEgress(r) {
				slog.Info("Dropped build egress the requesting Intent's policy does not allow", "entry", entry)
				continue
			}
			allowed = append(allowed, r)
		}
	}
	rules, err := sandbox.ServiceEgress(ctx, g.kube, g.cfg.RegistryNamespace, registryFrontService)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "registry egress: %v", err)
	}
	rules = append(rules, allowed...)
	for _, c := range g.execCfg.EgressCanaries {
		ap, err := netip.ParseAddrPort(c)
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "egress canary %q is not an IP:port literal", c)
		}
		for _, r := range rules {
			if pfx, err := netip.ParsePrefix(r.CIDR); err == nil && r.Port == int32(ap.Port()) && pfx.Contains(ap.Addr().Unmap()) {
				return nil, status.Errorf(codes.FailedPrecondition, "build egress grant %s would allow the egress canary %s", r, c)
			}
		}
	}
	return rules, nil
}

// pusherCredential reads the bundled registry's push-only credential. Neither value
// may appear in an error, a log line or an audit payload.
func (g *Gateway) pusherCredential(ctx context.Context) (string, string, error) {
	sec, err := g.kube.CoreV1().Secrets(g.cfg.Namespace).Get(ctx, registryPusherSecret, metav1.GetOptions{})
	if err != nil {
		return "", "", status.Errorf(codes.Unavailable, "read the registry pusher Secret %s/%s: %v", g.cfg.Namespace, registryPusherSecret, err)
	}
	user, password := string(sec.Data["username"]), string(sec.Data["password"])
	if user == "" || password == "" {
		return "", "", status.Errorf(codes.Unavailable, "the registry pusher Secret %s/%s has no username or password", g.cfg.Namespace, registryPusherSecret)
	}
	return user, password, nil
}

// buildPodConfig is a build's pod configuration, read fresh for each build: the
// timeout comes from OperatorConfig and the canaries from the latest prepareLeading.
func (g *Gateway) buildPodConfig(op *lastv1alpha1.OperatorConfig, base string) images.BuildPodConfig {
	return images.BuildPodConfig{Namespace: g.cfg.BuildNamespace, BuildKitImage: g.cfg.BuildKitImage, RunnerImage: base,
		PullHost: g.cfg.RegistryPullHost, FrontHost: g.cfg.RegistryFrontHost,
		PushRef: g.cfg.RegistryFrontHost + "/" + images.ImagesRepo, Insecure: true, Timeout: buildTimeout(op),
		Canaries: slices.Clone(g.execCfg.EgressCanaries)}
}

// runRef is the reference sandbox pods run a built image by.
func (g *Gateway) runRef(digest string) string {
	return g.cfg.RegistryPullHost + "/" + images.ImagesRepo + "@" + digest
}

// buildTimeout is images.buildTimeout: unset or not positive means the default, and
// it is clamped to the maximum.
func buildTimeout(op *lastv1alpha1.OperatorConfig) time.Duration {
	if op == nil || op.Spec.Images == nil || op.Spec.Images.BuildTimeout == nil || op.Spec.Images.BuildTimeout.Duration <= 0 {
		return images.DefaultBuildTimeout
	}
	return min(op.Spec.Images.BuildTimeout.Duration, images.MaxBuildTimeout)
}

// maxConcurrentBuilds is images.maxConcurrentBuilds: unset or not positive means the default.
func maxConcurrentBuilds(op *lastv1alpha1.OperatorConfig) int {
	if op == nil || op.Spec.Images == nil || op.Spec.Images.MaxConcurrentBuilds <= 0 {
		return images.DefaultMaxConcurrentBuilds
	}
	return int(op.Spec.Images.MaxConcurrentBuilds)
}

// warnBuildEgress logs images.buildEgress entries that every build would refuse.
func warnBuildEgress(op *lastv1alpha1.OperatorConfig) {
	if op == nil || op.Spec.Images == nil {
		return
	}
	for _, entry := range op.Spec.Images.BuildEgress {
		if _, err := sandbox.ParseEgress(entry); err != nil {
			slog.Warn("OperatorConfig images.buildEgress entry is invalid; image builds will fail until it is fixed",
				"entry", entry, "err", err)
		}
	}
}

// logTail returns at most the last maxLogTail bytes of s, starting on a rune boundary.
func logTail(s string) string {
	if len(s) <= maxLogTail {
		return s
	}
	s = s[len(s)-maxLogTail:]
	for i := 0; i < len(s) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(s[i]) {
			return s[i:]
		}
	}
	return s
}

func randomBuildID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// prepareImages runs in prepareLeading: it loads the image index, sizes the build
// slots from OperatorConfig, and deletes every build object a previous leader left.
func (g *Gateway) prepareImages(ctx context.Context, op *lastv1alpha1.OperatorConfig) error {
	if err := g.images.load(ctx); err != nil {
		return err
	}
	slots := maxConcurrentBuilds(op)
	g.builds.resize(slots)
	reaped, err := g.newBuilder(g.buildPodConfig(op, "")).ReapOrphans(ctx, true)
	if err != nil {
		return fmt.Errorf("gateway: reap leftover image builds: %w", err)
	}
	warnBuildEgress(op)
	slog.Info("Reaped leftover image builds", "count", reaped, "maxConcurrentBuilds", slots)
	return nil
}

// reapExpiredBuilds runs on the leader's ticker: it deletes the objects of
// builds whose expiry passed (a running build's has not), so a leftover push
// Secret does not wait for the next leader. Errors are only logged.
func (g *Gateway) reapExpiredBuilds(ctx context.Context) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		slog.Warn("Could not read OperatorConfig to reap expired image builds", "err", err)
		return
	}
	reaped, err := g.newBuilder(g.buildPodConfig(op, "")).ReapOrphans(ctx, false)
	if err != nil {
		slog.Warn("Could not reap expired image builds", "err", err)
		return
	}
	if reaped > 0 {
		slog.Info("Reaped expired image builds", "count", reaped)
	}
}
