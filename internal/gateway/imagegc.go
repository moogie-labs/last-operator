package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	// registryAdminSecret holds the bundled registry's admin credential (keys
	// username, password) in the gateway namespace. Only the gateway uses it: for
	// manifest DELETEs and the GC sidecar.
	registryAdminSecret = "last-registry-admin"
	// defaultImageGCInterval is how often the leader collects unused images.
	defaultImageGCInterval = 24 * time.Hour
	// defaultUnusedRetention is images.unusedRetention's default (30 days).
	defaultUnusedRetention = 30 * 24 * time.Hour
	// registryGCTimeout bounds the sidecar's run of registry garbage-collect.
	registryGCTimeout = 30 * time.Minute
	// maxSidecarError bounds the sidecar's answer quoted in a sidecarError.
	maxSidecarError = 1 << 10
	// maxSidecarAnswer bounds the sidecar's 200 answer the gateway reads; its log is
	// at most 64 KiB.
	maxSidecarAnswer = 1 << 20
	// defaultGCDeleteTimeout bounds each manifest DELETE, so a hung one cannot hold
	// builds paused.
	defaultGCDeleteTimeout = time.Minute
)

var (
	errImageGCRunning = errors.New("image GC is already running")
	errUnpinnedGCBase = errors.New("image GC needs a pinned base: set OperatorConfig.sandbox.baseImage by digest")
)

// imageGCDeleter deletes a manifest by digest; deleting a missing one is not an error.
// images.Client implements it: with the admin credential through the bundled front
// proxy, or with the push credential for an external registry.
type imageGCDeleter interface {
	Delete(ctx context.Context, repo, digest string) error
}

// gcResult is one run's outcome, as image.gc.result records it.
type gcResult struct {
	Deleted      int
	BytesFreed   int64
	SidecarError string
}

// gcStart is the image.gc.start audit payload.
type gcStart struct {
	Retention  string   `json:"retention"`
	Candidates []string `json:"candidates"`
}

// imageDeleted is the image.deleted audit payload.
type imageDeleted struct {
	SpecHash        string `json:"specHash"`
	Digest          string `json:"digest"`
	RegistryDeleted bool   `json:"registryDeleted"`
	Error           string `json:"error,omitempty"`
}

// gcResultPayload is the image.gc.result audit payload.
type gcResultPayload struct {
	Deleted      int    `json:"deleted"`
	BytesFreed   int64  `json:"bytesFreed"`
	SidecarError string `json:"sidecarError,omitempty"`
	DurationMs   int64  `json:"durationMs"`
}

// startImageGC runs on the leader's GC ticker. It starts one run in the background,
// counted in g.inflight so the drain waits for it, unless a run is still going.
func (g *Gateway) startImageGC(ctx context.Context, w *audit.Writer) bool {
	if !g.gcMu.TryLock() {
		slog.Info("Skipped image garbage collection: the previous run has not finished")
		return false
	}
	g.inflight.Go(func() {
		defer g.gcMu.Unlock()
		res, err := g.collectImages(ctx, w)
		if err != nil {
			slog.Error("Image garbage collection failed", "deleted", res.Deleted, "err", err)
			return
		}
		slog.Info("Collected unused images", "deleted", res.Deleted, "bytesFreed", res.BytesFreed,
			"sidecarError", res.SidecarError)
	})
	return true
}

// runImageGC performs one collection (spec §11.3, design §6.1):
//  1. write image.gc.start with the candidates;
//  2. pause new builds and wait for running ones;
//  3. for each candidate still unused, drop it from the index, delete its manifest,
//     and write image.deleted;
//  4. call the registry's GC sidecar (bundled mode, when there is one);
//  5. resume builds;
//  6. write image.gc.result.
//
// Builds resume on every path. Failures before step 1 return an error and change
// nothing. A failed DELETE is recorded in its image.deleted and a failed sidecar
// call in image.gc.result; neither stops the run. Cancellation stops it: builds
// resume, image.gc.result records what was done, and the error is returned.
func (g *Gateway) runImageGC(ctx context.Context, w *audit.Writer) (gcResult, error) {
	if !g.gcMu.TryLock() {
		return gcResult{}, errImageGCRunning
	}
	defer g.gcMu.Unlock()
	return g.collectImages(ctx, w)
}

// collectImages is runImageGC for a caller holding gcMu.
func (g *Gateway) collectImages(ctx context.Context, w *audit.Writer) (gcResult, error) {
	began := time.Now()
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return gcResult{}, err
	}
	base, err := g.sandboxImage(ctx)
	if err != nil {
		return gcResult{}, err
	}
	baseDigest, err := images.BaseDigest(base)
	if err != nil {
		// Without the base no reference can be computed, so nothing is known unused.
		return gcResult{}, errUnpinnedGCBase
	}
	refs, err := g.referencedSpecs(ctx, baseDigest)
	if err != nil {
		return gcResult{}, err
	}
	retention := unusedRetention(op)
	cutoff := g.builds.now().Add(-retention)
	entries := g.images.snapshot()
	var candidates []string
	for h, e := range entries {
		if e.LastUsed.Before(cutoff) && !refs[h] {
			candidates = append(candidates, h)
		}
	}
	slices.Sort(candidates)
	// An external registry has no sidecar; its own lifecycle policy reclaims storage.
	callSidecar := !externalRegistry(op) && g.cfg.RegistryGCURL != ""
	var target registryTarget
	var deleter imageGCDeleter
	var admin *images.Credentials
	if len(candidates) > 0 || callSidecar {
		if target, err = g.registryTarget(ctx, op); err != nil {
			return gcResult{}, err
		}
		// External manifests are deleted with the push credential, over HTTPS.
		deleter = target.client
		if !target.external {
			if admin, err = g.adminCredential(ctx); err != nil {
				return gcResult{}, err
			}
			deleter = g.registryClient(g.cfg.RegistryFrontHost, admin, true)
		}
	}

	// The audit writes ignore cancellation, so a run that started always records its result.
	actx := context.WithoutCancel(ctx)
	draft := func(typ string, payload any) audit.Draft {
		return audit.Draft{Type: typ, Actor: sandbox.GatewayActor, Payload: payload}
	}
	if _, err := w.Append(actx, draft(audit.TypeImageGCStart,
		gcStart{Retention: retention.String(), Candidates: append([]string{}, candidates...)})); err != nil {
		return gcResult{}, fmt.Errorf("gateway: write %s: %w", audit.TypeImageGCStart, err)
	}

	var res gcResult
	runErr := func() error {
		resume, err := g.builds.pause(ctx)
		if err != nil {
			res.SidecarError = "not called: " + err.Error()
			return fmt.Errorf("gateway: pause image builds: %w", err)
		}
		defer resume()
		for _, h := range candidates {
			if err := ctx.Err(); err != nil {
				res.SidecarError = "not called: " + err.Error()
				return err
			}
			if err := g.deleteImage(ctx, actx, w, deleter, target, h, entries[h].Digest, cutoff, &res); err != nil {
				res.SidecarError = "not called: " + err.Error()
				return err
			}
		}
		if callSidecar {
			freed, err := g.callRegistryGC(ctx, admin)
			if err != nil {
				res.SidecarError = err.Error()
				slog.Warn("Registry garbage collection failed", "err", err)
			}
			res.BytesFreed = freed
		}
		resume()
		return ctx.Err()
	}()

	if _, err := w.Append(actx, draft(audit.TypeImageGCResult, gcResultPayload{Deleted: res.Deleted,
		BytesFreed: res.BytesFreed, SidecarError: res.SidecarError, DurationMs: time.Since(began).Milliseconds()})); err != nil {
		return res, errors.Join(runErr, fmt.Errorf("gateway: write %s: %w", audit.TypeImageGCResult, err))
	}
	return res, runErr
}

// deleteImage drops one candidate, if it is still unused, then deletes its manifest
// from target and writes image.deleted. Dropping first means no resolve can hand out
// the digest once the DELETE is sent; a resolve that used the entry before the drop
// keeps it. Each DELETE gets gcDeleteTimeout. A registry that refuses DELETE (405 or
// UNSUPPORTED) is recorded as registryDeleted false, like any failure, and the entry
// stays out of the index. The error is set only when image.deleted could not be
// written.
func (g *Gateway) deleteImage(ctx, actx context.Context, w *audit.Writer, deleter imageGCDeleter, target registryTarget,
	specHash, digest string, cutoff time.Time, res *gcResult) error {
	dropped, err := g.images.dropUnused(actx, specHash, digest, cutoff)
	if err != nil {
		// The entry is gone from memory; a later leader that still loads it finds
		// the manifest missing and rebuilds.
		slog.Warn("Could not save the image index after dropping an entry", "specHash", specHash, "err", err)
	}
	if !dropped {
		slog.Info("Kept an image used since GC started", "specHash", specHash, "digest", digest)
		return nil
	}
	g.builds.forgetHead(target.headKey(digest))
	rec := imageDeleted{SpecHash: specHash, Digest: digest}
	timeout := g.gcDeleteTimeout
	if timeout <= 0 {
		timeout = defaultGCDeleteTimeout
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	err = deleter.Delete(dctx, target.repo, digest)
	cancel()
	switch {
	case errors.Is(err, images.ErrDeleteUnsupported):
		rec.Error = err.Error()
		slog.Info("Registry refused to delete an unused image's manifest; its lifecycle policy must reclaim it",
			"specHash", specHash, "digest", digest, "err", err)
	case err != nil:
		rec.Error = err.Error()
		slog.Warn("Could not delete an unused image's manifest", "specHash", specHash, "digest", digest, "err", err)
	default:
		rec.RegistryDeleted = true
		res.Deleted++
	}
	if _, err := w.Append(actx, audit.Draft{Type: audit.TypeImageDeleted, Actor: sandbox.GatewayActor, Payload: rec}); err != nil {
		return fmt.Errorf("gateway: write %s: %w", audit.TypeImageDeleted, err)
	}
	return nil
}

// adminCredential reads the bundled registry's admin credential. Neither value may
// appear in an error, a log line or an audit payload.
func (g *Gateway) adminCredential(ctx context.Context) (*images.Credentials, error) {
	sec, err := g.kube.CoreV1().Secrets(g.cfg.Namespace).Get(ctx, registryAdminSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("gateway: read the registry admin Secret %s/%s: %w", g.cfg.Namespace, registryAdminSecret, err)
	}
	user, password := string(sec.Data["username"]), string(sec.Data["password"])
	if user == "" || password == "" {
		return nil, fmt.Errorf("gateway: the registry admin Secret %s/%s has no username or password", g.cfg.Namespace, registryAdminSecret)
	}
	return &images.Credentials{Username: user, Password: password}, nil
}

// callRegistryGC asks the GC sidecar to run registry garbage-collect and returns
// the bytes it freed.
func (g *Gateway) callRegistryGC(ctx context.Context, admin *images.Credentials) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, registryGCTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(g.cfg.RegistryGCURL, "/")+"/gc", nil)
	if err != nil {
		return 0, fmt.Errorf("registry GC request: %w", err)
	}
	req.SetBasicAuth(admin.Username, admin.Password)
	resp, err := (&http.Client{Timeout: registryGCTimeout}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("registry GC: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxSidecarError))
		return 0, fmt.Errorf("registry GC answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var reply struct {
		BytesFreed int64  `json:"bytesFreed"`
		Log        string `json:"log"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSidecarAnswer)).Decode(&reply); err != nil {
		return 0, fmt.Errorf("registry GC answer: %w", err)
	}
	slog.Debug("Registry garbage collection log", "log", reply.Log)
	return reply.BytesFreed, nil
}

// referencedSpecs returns the spec hashes, on baseDigest, of every image install in
// the cluster's Intents' status.plan (steps, and each resource's probes and
// teardown) and ManagedResources (probes and teardown). They are only a hint: an
// Intent's status is operator-written, so a false reference keeps an image longer
// and a missing one causes a rebuild.
func (g *Gateway) referencedSpecs(ctx context.Context, baseDigest string) (map[string]bool, error) {
	refs := map[string]bool{}
	add := func(cmds ...lastv1alpha1.Command) {
		for _, c := range cmds {
			if c.Image == nil || len(c.Image.Install) == 0 {
				continue
			}
			// An install that cannot be hashed cannot have been built.
			if h, err := images.SpecHash(baseDigest, c.Image.Install); err == nil {
				refs[h] = true
			}
		}
	}
	addResource := func(r lastv1alpha1.ManagedResourceSpec) {
		for _, p := range r.Probes {
			add(p.Command)
		}
		add(r.Teardown...)
	}
	var intents lastv1alpha1.IntentList
	if err := g.crd.List(ctx, &intents); err != nil {
		return nil, fmt.Errorf("gateway: list Intents for image GC: %w", err)
	}
	for _, in := range intents.Items {
		if p := in.Status.Plan; p != nil {
			add(p.Steps...)
			for _, r := range p.Resources {
				addResource(r)
			}
		}
	}
	var resources lastv1alpha1.ManagedResourceList
	if err := g.crd.List(ctx, &resources); err != nil {
		return nil, fmt.Errorf("gateway: list ManagedResources for image GC: %w", err)
	}
	for _, r := range resources.Items {
		addResource(r.Spec)
	}
	return refs, nil
}

// unusedRetention is images.unusedRetention: unset or not positive means the default.
func unusedRetention(op *lastv1alpha1.OperatorConfig) time.Duration {
	if op == nil || op.Spec.Images == nil || op.Spec.Images.UnusedRetention == nil || op.Spec.Images.UnusedRetention.Duration <= 0 {
		return defaultUnusedRetention
	}
	return op.Spec.Images.UnusedRetention.Duration
}
