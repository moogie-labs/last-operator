package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	testBaseDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testBase        = "localhost:5000/last/base@" + testBaseDigest
	testPullHost    = "localhost:5000"
	testRegistryNS  = "last-registry"
	testBuildNS     = "last-build"
	frontClusterIP  = "10.96.5.5"
	frontEndpointIP = "10.244.1.7"
	testPusherPW    = "pusher-s3cret-pw"
	goneDigest      = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	settle          = 200 * time.Millisecond
	testBuildKitImg = "buildkit:test"
	hostEgress      = "pypi.org:443"
)

// trueInstall and falseInstall are two distinct image specs.
var trueInstall, falseInstall = install([]string{"true"}), install([]string{"false"})

// fakeBuilder pushes a random image to the test registry for each build, so the
// digest it returns is one the registry has.
type fakeBuilder struct {
	host     string
	started  chan string   // receives each build's spec hash as it starts
	block    chan struct{} // when set, each build waits for one token
	fail     error         // when set, builds fail with this reason
	log      string        // the failed build's log
	buildErr error         // when set, Build returns it (outcome unknown)

	mu         sync.Mutex
	specs      []images.BuildSpec
	cfgs       []images.BuildPodConfig
	reaps      []bool
	running    int
	maxRunning int
	cancelled  int // builds that saw their context end
}

func (f *fakeBuilder) Build(ctx context.Context, s images.BuildSpec) (images.BuildOutcome, error) {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	f.running++
	f.maxRunning = max(f.maxRunning, f.running)
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.running--; f.mu.Unlock() }()
	f.started <- s.SpecHash
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled++
			f.mu.Unlock()
			return images.BuildOutcome{}, ctx.Err()
		}
	}
	if f.buildErr != nil {
		return images.BuildOutcome{}, f.buildErr
	}
	if f.fail != nil {
		return images.BuildOutcome{Err: f.fail, Log: f.log, LogTruncated: true, Duration: time.Second}, nil
	}
	img, err := random.Image(256, 1)
	if err != nil {
		return images.BuildOutcome{}, err
	}
	digest, err := images.NewClient(f.host, nil, true).Push(ctx, images.ImagesRepo, images.Tag(s.SpecHash), img)
	if err != nil {
		return images.BuildOutcome{}, err
	}
	return images.BuildOutcome{Digest: digest, Log: "built " + s.SpecHash, Duration: 1500 * time.Millisecond}, nil
}

func (f *fakeBuilder) ReapOrphans(_ context.Context, all bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reaps = append(f.reaps, all)
	return 0, nil
}

func (f *fakeBuilder) builds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

type imageEnv struct {
	*testEnv
	fb    *fakeBuilder
	srv   *httptest.Server
	host  string
	heads atomic.Int32
	// deletes counts admitted manifest DELETEs; deleteStatus, when set, answers them.
	deletes      atomic.Int32
	deleteStatus atomic.Int32
	// deleteRelease, when set, holds each DELETE until it closes; deleting
	// receives once per held DELETE as it arrives.
	deleteRelease atomic.Pointer[chan struct{}]
	deleting      chan struct{}
}

func newImageEnv(t *testing.T, op *lastv1alpha1.OperatorConfig, objs ...client.Object) *imageEnv {
	t.Helper()
	ie := &imageEnv{deleting: make(chan struct{}, 16)}
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	ie.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			ie.heads.Add(1)
		case http.MethodDelete:
			// The front proxy admits DELETE with the admin credential only.
			if user, pw, ok := r.BasicAuth(); !ok || user != testAdminUser || pw != testAdminPW {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			ie.deletes.Add(1)
			if release := ie.deleteRelease.Load(); release != nil {
				ie.deleting <- struct{}{}
				select {
				case <-*release:
				case <-r.Context().Done():
					return
				}
			}
			if code := ie.deleteStatus.Load(); code != 0 {
				w.WriteHeader(int(code))
				return
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(ie.srv.Close)
	ie.host = strings.TrimPrefix(ie.srv.URL, "http://")
	ie.testEnv = newTestEnv(t, append([]client.Object{op}, objs...)...)
	ie.fb = &fakeBuilder{host: ie.host, started: make(chan string, 64)}

	ctx := context.Background()
	tcp := corev1.ProtocolTCP
	port := int32(5000)
	if _, err := ie.kube.CoreV1().Services(testRegistryNS).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: registryFrontService, Namespace: testRegistryNS},
		Spec:       corev1.ServiceSpec{ClusterIP: frontClusterIP, Ports: []corev1.ServicePort{{Port: port, Protocol: tcp}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ie.kube.DiscoveryV1().EndpointSlices(testRegistryNS).Create(ctx, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: registryFrontService + "-abc", Namespace: testRegistryNS,
			Labels: map[string]string{discoveryv1.LabelServiceName: registryFrontService}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{frontEndpointIP}}},
		Ports:       []discoveryv1.EndpointPort{{Port: &port, Protocol: &tcp}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ie.kube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryPusherSecret, Namespace: ns},
		Data:       map[string][]byte{"username": []byte("pusher"), "password": []byte(testPusherPW)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	g := ie.g
	g.cfg.RegistryFrontHost = ie.host
	g.cfg.RegistryPullHost = testPullHost
	g.cfg.RegistryNamespace = testRegistryNS
	g.cfg.BuildNamespace = testBuildNS
	g.cfg.BuildKitImage = testBuildKitImg
	g.registry = images.NewClient(ie.host, nil, true)
	g.newBuilder = func(cfg images.BuildPodConfig) imageBuilder {
		ie.fb.mu.Lock()
		defer ie.fb.mu.Unlock()
		ie.fb.cfgs = append(ie.fb.cfgs, cfg)
		return ie.fb
	}
	return ie
}

// imageOp is an OperatorConfig with a pinned base whose policy allows allowed.
func imageOp(buildEgress []string, allowed ...string) *lastv1alpha1.OperatorConfig {
	op := allowAll()
	op.Spec.Sandbox = &lastv1alpha1.SandboxConfig{BaseImage: testBase}
	op.Spec.Images = &lastv1alpha1.ImagesConfig{BuildEgress: buildEgress}
	if allowed != nil {
		op.Spec.Defaults.Egress.Allowed = allowed
	}
	return op
}

func install(argvs ...[]string) []lastv1alpha1.ArgvCommand {
	out := make([]lastv1alpha1.ArgvCommand, len(argvs))
	for i, a := range argvs {
		out[i] = lastv1alpha1.ArgvCommand{Argv: a}
	}
	return out
}

var toolInstall = install([]string{"apt-get", "install", "-y", "jq=1.6-2.1"})

func (ie *imageEnv) request(inst []lastv1alpha1.ArgvCommand) imageRequest {
	ie.t.Helper()
	op, err := ie.g.operatorConfig(ie.ctx)
	if err != nil {
		ie.t.Fatal(err)
	}
	eff, err := policy.Resolve(op, nil)
	if err != nil {
		ie.t.Fatal(err)
	}
	return imageRequest{Intent: envKey, Stage: testStage, Subject: "sha256:subject", Actor: testOperator,
		Install: inst, Policy: eff}
}

// resolve bounds each call, so a broken single flight or cap fails a test instead of hanging it.
func (ie *imageEnv) resolve(inst []lastv1alpha1.ArgvCommand) (string, error) {
	return ie.resolveCtx(ie.ctx, inst)
}

func (ie *imageEnv) resolveCtx(ctx context.Context, inst []lastv1alpha1.ArgvCommand) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ie.g.resolveImage(ctx, ie.g.writer.Load(), ie.request(inst))
}

func specHashOf(t *testing.T, inst []lastv1alpha1.ArgvCommand) string {
	t.Helper()
	h, err := images.SpecHash(testBaseDigest, inst)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (ie *imageEnv) entriesOf(typ string) []audit.Entry {
	var out []audit.Entry
	for _, e := range ie.entries() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (ie *imageEnv) waitStarted() string {
	ie.t.Helper()
	select {
	case h := <-ie.fb.started:
		return h
	case <-time.After(5 * time.Second):
		ie.t.Fatal("no build started")
		return ""
	}
}

// pushImage puts a random image into the registry and returns its digest.
func (ie *imageEnv) pushImage() string {
	ie.t.Helper()
	img, err := random.Image(128, 1)
	if err != nil {
		ie.t.Fatal(err)
	}
	d, err := images.NewClient(ie.host, nil, true).Push(context.Background(), images.ImagesRepo, "seed", img)
	if err != nil {
		ie.t.Fatal(err)
	}
	return d
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func TestResolveReturnsTheBaseWithoutInstall(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ref, err := ie.resolve(nil)
	if err != nil || ref != testBase {
		t.Fatalf("resolve = %q, %v; want the base", ref, err)
	}
	if ie.fb.builds() != 0 || len(ie.entriesOf(audit.TypeImageBuildStart)) != 0 {
		t.Fatal("resolving without install built or audited something")
	}
}

func TestResolveBuildsOnceAndAuditsTheBuild(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ref, err := ie.resolve(toolInstall)
	if err != nil {
		t.Fatal(err)
	}
	if ie.fb.builds() != 1 {
		t.Fatalf("%d builds, want 1", ie.fb.builds())
	}
	hash := specHashOf(t, toolInstall)
	res, resultSeq := ie.checkBuildAudit(hash)
	if ref != testPullHost+"/last/images@"+res.Digest {
		t.Fatalf("ref = %q, want %s/last/images@%s", ref, testPullHost, res.Digest)
	}
	e, ok := ie.g.images.get(hash)
	if !ok || e.Digest != res.Digest || e.BuildSeq != resultSeq {
		t.Fatalf("index entry = %+v, %v; want digest %s at seq %d", e, ok, res.Digest, resultSeq)
	}
	ie.checkBuildInputs(hash)

	again, err := ie.resolve(toolInstall)
	if err != nil || again != ref {
		t.Fatalf("second resolve = %q, %v; want %q", again, err, ref)
	}
	if ie.fb.builds() != 1 {
		t.Fatalf("%d builds after a second resolve, want 1", ie.fb.builds())
	}
}

// checkBuildAudit checks the one build's start and result entries and returns the
// result payload and its seq.
func (ie *imageEnv) checkBuildAudit(hash string) (buildResult, uint64) {
	t := ie.t
	t.Helper()
	starts, results := ie.entriesOf(audit.TypeImageBuildStart), ie.entriesOf(audit.TypeImageBuildResult)
	if len(starts) != 1 || len(results) != 1 || starts[0].Seq >= results[0].Seq {
		t.Fatalf("starts = %v, results = %v; want one of each, in order", starts, results)
	}
	for _, e := range []audit.Entry{starts[0], results[0]} {
		if e.Intent != envKey || e.Stage != testStage || e.SubjectHash != "sha256:subject" || e.Actor != testOperator {
			t.Fatalf("%s carries %q %q %q %q, want the request's intent, stage, subject and actor",
				e.Type, e.Intent, e.Stage, e.SubjectHash, e.Actor)
		}
	}
	var start buildStart
	if err := json.Unmarshal(starts[0].Payload, &start); err != nil {
		t.Fatal(err)
	}
	wantCF := "FROM " + ie.host + "/last/base@" + testBaseDigest + "\nUSER root\n" +
		`RUN ["apt-get","install","-y","jq=1.6-2.1"]` + "\nUSER 65532\n"
	wantEgress := []string{frontClusterIP + "/32:5000", frontEndpointIP + "/32:5000", kubeDNSGrant}
	if start.SpecHash != hash || start.BaseImage != testBase || start.Containerfile != wantCF ||
		start.PushRef != ie.host+"/last/images:"+images.Tag(hash) || !slices.Equal(start.Egress, wantEgress) {
		t.Fatalf("image.build.start = %+v", start)
	}
	var res buildResult
	if err := json.Unmarshal(results[0].Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != buildSucceeded || res.SpecHash != hash || res.StartSeq != starts[0].Seq ||
		res.Digest == "" || res.DurationMs != 1500 || res.Log != "built "+hash || res.Error != "" {
		t.Fatalf("image.build.result = %+v", res)
	}
	for _, en := range ie.entries() {
		if b, _ := json.Marshal(en); strings.Contains(string(b), testPusherPW) {
			t.Fatalf("audit entry %d holds the pusher password", en.Seq)
		}
	}
	return res, results[0].Seq
}

// checkBuildInputs checks that the build got the pinned base, the pusher credential
// and the configured pod settings.
func (ie *imageEnv) checkBuildInputs(hash string) {
	t := ie.t
	t.Helper()
	spec, cfg := ie.fb.specs[0], ie.fb.cfgs[0]
	if spec.SpecHash != hash || spec.Base != testBase || spec.PushUser != "pusher" || spec.PushPassword != testPusherPW ||
		len(spec.ID) != 16 || !slices.Equal(spec.Install[0].Argv, toolInstall[0].Argv) {
		t.Fatalf("BuildSpec = %+v", spec)
	}
	if cfg.Namespace != testBuildNS || cfg.BuildKitImage != testBuildKitImg || cfg.RunnerImage != testBase ||
		cfg.PullHost != testPullHost || cfg.FrontHost != ie.host || cfg.Timeout != images.DefaultBuildTimeout ||
		!slices.Equal(cfg.Canaries, []string{testCanary}) {
		t.Fatalf("BuildPodConfig = %+v", cfg)
	}
}

func TestConcurrentResolvesShareOneBuild(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.fb.block = make(chan struct{})
	const n = 5
	refs, errs := make([]string, n), make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { refs[i], errs[i] = ie.resolve(toolInstall) })
	}
	ie.waitStarted()
	time.Sleep(settle) // let the other callers reach the flight
	close(ie.fb.block)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || refs[i] != refs[0] || refs[0] == "" {
			t.Fatalf("caller %d: %q, %v; want %q", i, refs[i], errs[i], refs[0])
		}
	}
	if ie.fb.builds() != 1 || len(ie.entriesOf(audit.TypeImageBuildStart)) != 1 {
		t.Fatalf("%d builds, %d image.build.start; want 1 each", ie.fb.builds(), len(ie.entriesOf(audit.TypeImageBuildStart)))
	}
}

func TestAFailedBuildFailsEveryWaiterAndIsNotIndexed(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	buildLog := strings.Repeat("x", 70<<10) + "TAIL-END"
	ie.fb.fail, ie.fb.log = errors.New("build exited 1"), buildLog
	ie.fb.block = make(chan struct{}, 2)
	const n = 3
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, errs[i] = ie.resolve(toolInstall) })
	}
	ie.waitStarted()
	time.Sleep(settle)
	ie.fb.block <- struct{}{}
	wg.Wait()
	for i, err := range errs {
		var bf errImageBuildFailed
		if !errors.As(err, &bf) {
			t.Fatalf("caller %d: err = %v, want errImageBuildFailed", i, err)
		}
		if bf.Reason != "build exited 1" || bf.LogTail != buildLog[len(buildLog)-maxLogTail:] {
			t.Fatalf("caller %d: reason %q, tail of %d bytes", i, bf.Reason, len(bf.LogTail))
		}
		if err.Error() != "ImageBuildFailed: build exited 1" {
			t.Fatalf("Error() = %q", err.Error())
		}
	}
	results := ie.entriesOf(audit.TypeImageBuildResult)
	if len(results) != 1 {
		t.Fatalf("%d image.build.result entries, want 1", len(results))
	}
	var res buildResult
	if err := json.Unmarshal(results[0].Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed" || res.Digest != "" || res.Error != "build exited 1" || res.Log != buildLog || !res.LogTruncated {
		t.Fatalf("image.build.result = outcome %q digest %q error %q truncated %v", res.Outcome, res.Digest, res.Error, res.LogTruncated)
	}
	if _, ok := ie.g.images.get(specHashOf(t, toolInstall)); ok {
		t.Fatal("a failed build was indexed")
	}
	ie.fb.block <- struct{}{}
	if _, err := ie.resolve(toolInstall); err == nil {
		t.Fatal("the rebuild should fail too")
	}
	if ie.fb.builds() != 2 {
		t.Fatalf("%d builds, want a fresh build after the failure", ie.fb.builds())
	}
}

func TestLogTailCutsOnARuneBoundary(t *testing.T) {
	s := "é" + strings.Repeat("a", maxLogTail-1) // 'é' is 2 bytes; the cut falls inside it
	got := logTail(s)
	if got != strings.Repeat("a", maxLogTail-1) {
		t.Fatalf("tail is %d bytes starting %q", len(got), got[:2])
	}
	if logTail("short") != "short" {
		t.Fatal("a short log changed")
	}
}

// indexBuilt records a verified build of the spec as digest, as an earlier process would have.
func (ie *imageEnv) indexBuilt(hash, digest string) {
	ie.t.Helper()
	seq := writeBuildResult(ie.t, ie.testEnv, audit.TypeImageBuildResult,
		buildResult{SpecHash: hash, StartSeq: 1, Outcome: buildSucceeded, Digest: digest})
	if err := ie.g.images.put(context.Background(), hash, imageEntry{Digest: digest, BuildSeq: seq}); err != nil {
		ie.t.Fatal(err)
	}
}

func TestResolveUsesAVerifiedIndexEntryAndCachesTheManifestCheck(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	hash, digest := specHashOf(t, toolInstall), ie.pushImage()
	ie.indexBuilt(hash, digest)
	ie.heads.Store(0) // the seeding push sent HEADs of its own
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ie.g.builds.now = func() time.Time { return now }
	for range 2 {
		ref, err := ie.resolve(toolInstall)
		if err != nil || ref != testPullHost+"/last/images@"+digest {
			t.Fatalf("resolve = %q, %v", ref, err)
		}
	}
	if ie.fb.builds() != 0 || ie.heads.Load() != 1 {
		t.Fatalf("%d builds, %d HEADs; want 0 and 1 (cached)", ie.fb.builds(), ie.heads.Load())
	}
	if e, _ := ie.g.images.get(hash); !e.LastUsed.Equal(now) {
		t.Fatalf("lastUsed = %v, want %v", e.LastUsed, now)
	}
	now = now.Add(headCacheTTL + time.Second)
	if _, err := ie.resolve(toolInstall); err != nil {
		t.Fatal(err)
	}
	if ie.heads.Load() != 2 {
		t.Fatalf("%d HEADs after the cache expired, want 2", ie.heads.Load())
	}
}

func TestResolveRebuildsWhenTheManifestIsGone(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	hash := specHashOf(t, toolInstall)
	ie.indexBuilt(hash, goneDigest)
	ref, err := ie.resolve(toolInstall)
	if err != nil {
		t.Fatal(err)
	}
	if ie.fb.builds() != 1 {
		t.Fatalf("%d builds, want 1", ie.fb.builds())
	}
	if e, ok := ie.g.images.get(hash); !ok || e.Digest == goneDigest || ref != testPullHost+"/last/images@"+e.Digest {
		t.Fatalf("index entry = %+v, %v; ref %q; want the rebuilt digest", e, ok, ref)
	}
}

func TestResolveRebuildsWhenTheIndexEntryFailsVerification(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	hash, digest := specHashOf(t, toolInstall), ie.pushImage()
	// The entry points at a build of another spec: an edited index.
	unrelated := writeBuildResult(t, ie.testEnv, audit.TypeImageBuildResult,
		buildResult{SpecHash: "sha256:other", StartSeq: 1, Outcome: buildSucceeded, Digest: digest})
	if err := ie.g.images.put(context.Background(), hash, imageEntry{Digest: digest, BuildSeq: unrelated}); err != nil {
		t.Fatal(err)
	}
	ref, err := ie.resolve(toolInstall)
	if err != nil {
		t.Fatal(err)
	}
	if ie.fb.builds() != 1 {
		t.Fatalf("%d builds, want 1", ie.fb.builds())
	}
	if ref == testPullHost+"/last/images@"+digest {
		t.Fatal("ran the digest the audit log does not name")
	}
	fresh := newImageIndex(ie.kube, ns)
	if err := fresh.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e, ok := fresh.get(hash); !ok || e.BuildSeq == unrelated || e.Digest == digest {
		t.Fatalf("saved entry = %+v, %v; want the bad entry replaced by the rebuild", e, ok)
	}
}

func TestBuildsAreCappedByMaxConcurrentBuilds(t *testing.T) {
	op := imageOp(nil)
	op.Spec.Images.MaxConcurrentBuilds = 1
	ie := newImageEnv(t, op)
	if err := ie.g.prepareLeading(context.Background(), ie.g.writer.Load()); err != nil {
		t.Fatal(err)
	}
	ie.fb.block = make(chan struct{})
	first, second := trueInstall, falseInstall
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() { _, errs[0] = ie.resolve(first) })
	if got := ie.waitStarted(); got != specHashOf(t, first) {
		t.Fatalf("started %s first", got)
	}
	wg.Go(func() { _, errs[1] = ie.resolve(second) })
	select {
	case h := <-ie.fb.started:
		t.Fatalf("build %s started while another was running", h)
	case <-time.After(settle):
	}
	ie.fb.block <- struct{}{}
	if got := ie.waitStarted(); got != specHashOf(t, second) {
		t.Fatalf("started %s second", got)
	}
	ie.fb.block <- struct{}{}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || ie.fb.maxRunning != 1 {
		t.Fatalf("errs = %v, max running = %d; want both built, one at a time", errs, ie.fb.maxRunning)
	}
}

func TestPrepareLeadingLoadsTheImageIndexAndReapsEveryBuild(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	hash := specHashOf(t, toolInstall)
	saved := newImageIndex(ie.kube, ns)
	if err := saved.put(context.Background(), hash, imageEntry{Digest: testDigest, BuildSeq: 3}); err != nil {
		t.Fatal(err)
	}
	if err := ie.g.prepareLeading(context.Background(), ie.g.writer.Load()); err != nil {
		t.Fatal(err)
	}
	if e, ok := ie.g.images.get(hash); !ok || e.Digest != testDigest {
		t.Fatalf("index after prepareLeading = %+v, %v", e, ok)
	}
	if !slices.Equal(ie.fb.reaps, []bool{true}) || ie.fb.cfgs[0].Namespace != testBuildNS {
		t.Fatalf("reaps = %v (cfgs %+v), want one reap of every build in %s", ie.fb.reaps, ie.fb.cfgs, testBuildNS)
	}
	if cap(ie.g.builds.slots()) != images.DefaultMaxConcurrentBuilds {
		t.Fatalf("build slots = %d, want %d", cap(ie.g.builds.slots()), images.DefaultMaxConcurrentBuilds)
	}
}

func TestTheLeaderReapsOnlyExpiredBuilds(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.g.reapExpiredBuilds(context.Background())
	if !slices.Equal(ie.fb.reaps, []bool{false}) || ie.fb.cfgs[0].Namespace != testBuildNS {
		t.Fatalf("reaps = %v (cfgs %+v), want one reap of expired builds in %s", ie.fb.reaps, ie.fb.cfgs, testBuildNS)
	}
}

func TestResolveRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		op         *lastv1alpha1.OperatorConfig
	}{
		{name: "unpinned base", op: allowAll(), want: "image builds need OperatorConfig.sandbox.baseImage pinned by digest"},
		{name: "external registry", op: func() *lastv1alpha1.OperatorConfig {
			op := imageOp(nil)
			op.Spec.Registry = &lastv1alpha1.RegistryConfig{Mode: lastv1alpha1.RegistryExternal}
			return op
		}(), want: "external"},
		{name: "hostname in buildEgress", op: imageOp([]string{hostEgress}), want: hostEgress},
		{name: "grant covers a canary", op: imageOp([]string{"10.96.0.0/16:8080"}, "10.96.0.0/16:8080"), want: testCanary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ie := newImageEnv(t, tc.op)
			_, err := ie.resolve(toolInstall)
			wantCode(t, err, codes.FailedPrecondition)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if ie.fb.builds() != 0 || len(ie.entriesOf(audit.TypeImageBuildStart)) != 0 {
				t.Fatal("a refused resolve built or audited a build")
			}
		})
	}
}

func TestResolveIsUnavailableWhenTheRegistryIs(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.indexBuilt(specHashOf(t, toolInstall), ie.pushImage())
	ie.srv.Close()
	_, err := ie.resolve(toolInstall)
	wantCode(t, err, codes.Unavailable)
	if ie.fb.builds() != 0 {
		t.Fatal("built although the registry could not be checked")
	}
}

// getFailSink fails every Get, as an unreachable audit store would.
type getFailSink struct{ *audittest.MemSink }

func (getFailSink) Get(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("store unavailable")
}

func TestResolveIsUnavailableWhenTheAuditLogCannotBeRead(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	hash := specHashOf(t, toolInstall)
	ie.indexBuilt(hash, ie.pushImage())
	ie.g.sink = getFailSink{ie.sink}
	_, err := ie.resolve(toolInstall)
	wantCode(t, err, codes.Unavailable)
	if ie.fb.builds() != 0 {
		t.Fatal("built although the index entry could not be checked")
	}
	if _, ok := ie.g.images.get(hash); !ok {
		t.Fatal("dropped an entry that was never shown to be bad")
	}
}

func TestBuildEgressIsIntersectedWithPolicy(t *testing.T) {
	ie := newImageEnv(t, imageOp([]string{"0.0.0.0/0:443", "10.0.0.0/8:80"}, "0.0.0.0/0:443"))
	if _, err := ie.resolve(toolInstall); err != nil {
		t.Fatal(err)
	}
	want := []sandbox.EgressRule{{CIDR: frontClusterIP + "/32", Port: 5000}, {CIDR: frontEndpointIP + "/32", Port: 5000},
		{CIDR: "0.0.0.0/0", Port: 443}}
	if got := ie.fb.specs[0].Egress; !slices.Equal(got, want) {
		t.Fatalf("build egress = %v, want %v", got, want)
	}
}

func TestBuildLimitsFromOperatorConfig(t *testing.T) {
	d := func(v time.Duration) *metav1.Duration { return &metav1.Duration{Duration: v} }
	for _, tc := range []struct {
		in   *lastv1alpha1.ImagesConfig
		want time.Duration
		max  int
	}{
		{in: nil, want: images.DefaultBuildTimeout, max: images.DefaultMaxConcurrentBuilds},
		{in: &lastv1alpha1.ImagesConfig{BuildTimeout: d(0), MaxConcurrentBuilds: -1}, want: images.DefaultBuildTimeout, max: images.DefaultMaxConcurrentBuilds},
		{in: &lastv1alpha1.ImagesConfig{BuildTimeout: d(-time.Minute)}, want: images.DefaultBuildTimeout, max: images.DefaultMaxConcurrentBuilds},
		{in: &lastv1alpha1.ImagesConfig{BuildTimeout: d(20 * time.Minute), MaxConcurrentBuilds: 5}, want: 20 * time.Minute, max: 5},
		{in: &lastv1alpha1.ImagesConfig{BuildTimeout: d(45 * time.Minute)}, want: images.MaxBuildTimeout, max: images.DefaultMaxConcurrentBuilds},
	} {
		op := &lastv1alpha1.OperatorConfig{Spec: lastv1alpha1.OperatorConfigSpec{Images: tc.in}}
		if got := buildTimeout(op); got != tc.want {
			t.Errorf("buildTimeout(%+v) = %v, want %v", tc.in, got, tc.want)
		}
		if got := maxConcurrentBuilds(op); got != tc.max {
			t.Errorf("maxConcurrentBuilds(%+v) = %d, want %d", tc.in, got, tc.max)
		}
	}
	if buildTimeout(nil) != images.DefaultBuildTimeout || maxConcurrentBuilds(nil) != images.DefaultMaxConcurrentBuilds {
		t.Error("no OperatorConfig must mean the defaults")
	}
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (ie *imageEnv) flights() int {
	ie.g.builds.mu.Lock()
	defer ie.g.builds.mu.Unlock()
	return len(ie.g.builds.flights)
}

func TestAFirstCallerCancellingDoesNotStopTheSharedBuild(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.fb.block = make(chan struct{})
	first, cancelFirst := context.WithCancel(ie.ctx)
	defer cancelFirst()
	var firstErr, secondErr error
	var ref string
	var wg sync.WaitGroup
	wg.Go(func() { _, firstErr = ie.resolveCtx(first, toolInstall) })
	ie.waitStarted()
	wg.Go(func() { ref, secondErr = ie.resolve(toolInstall) })
	time.Sleep(settle) // let the second caller join the flight
	cancelFirst()
	time.Sleep(settle)
	close(ie.fb.block)
	wg.Wait()
	wantCode(t, firstErr, codes.Canceled)
	if secondErr != nil || ref == "" {
		t.Fatalf("second caller = %q, %v; want the digest", ref, secondErr)
	}
	if ie.fb.builds() != 1 || ie.fb.cancelled != 0 {
		t.Fatalf("%d builds, %d cancelled; want 1 build, not cancelled", ie.fb.builds(), ie.fb.cancelled)
	}
}

func TestEveryCallerCancellingCancelsTheBuild(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.fb.block = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(ie.ctx)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { _, errs[i] = ie.resolveCtx(ctx, toolInstall) })
	}
	ie.waitStarted()
	time.Sleep(settle)
	cancel()
	wg.Wait()
	for _, err := range errs {
		wantCode(t, err, codes.Canceled)
	}
	eventually(t, "the build to see its context end", func() bool {
		ie.fb.mu.Lock()
		defer ie.fb.mu.Unlock()
		return ie.fb.cancelled == 1
	})
	if n := ie.flights(); n != 0 {
		t.Fatalf("%d flights left, want the cancelled one removed", n)
	}
	// The cancelled build still gets its result entry (the audit ignores cancellation).
	eventually(t, "image.build.result", func() bool { return len(ie.entriesOf(audit.TypeImageBuildResult)) == 1 })
	var res buildResult
	if err := json.Unmarshal(ie.entriesOf(audit.TypeImageBuildResult)[0].Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != buildFailed || res.Error == "" {
		t.Fatalf("image.build.result = %+v, want a failure with an error", res)
	}
	ie.fb.block <- struct{}{}
	if _, err := ie.resolve(toolInstall); err != nil {
		t.Fatal(err)
	}
	if ie.fb.builds() != 2 {
		t.Fatalf("%d builds, want a fresh build after the cancelled one", ie.fb.builds())
	}
}

func TestACallerCancelledWhileQueuedDoesNotAffectItsFlight(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.g.builds.resize(1)
	ie.fb.block = make(chan struct{}, 2)
	running, queued := trueInstall, falseInstall
	var wg sync.WaitGroup
	var runErr, waitErr, leftErr error
	var ref string
	wg.Go(func() { _, runErr = ie.resolve(running) })
	ie.waitStarted()
	leaving, leave := context.WithCancel(ie.ctx)
	defer leave()
	wg.Go(func() { _, leftErr = ie.resolveCtx(leaving, queued) })
	time.Sleep(settle) // the leaving caller starts the flight
	wg.Go(func() { ref, waitErr = ie.resolve(queued) })
	time.Sleep(settle) // both queued callers share the flight waiting for the slot
	leave()
	time.Sleep(settle)
	ie.fb.block <- struct{}{}
	ie.fb.block <- struct{}{}
	wg.Wait()
	wantCode(t, leftErr, codes.Canceled)
	if runErr != nil || waitErr != nil || ref == "" {
		t.Fatalf("running %v, waiting %q %v; want both built", runErr, ref, waitErr)
	}
	if ie.fb.builds() != 2 {
		t.Fatalf("%d builds, want 2", ie.fb.builds())
	}
}

func TestAConfigRefusalDoesNotQueueBehindBuilds(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.g.builds.resize(1)
	ie.fb.block = make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = ie.resolve(trueInstall) })
	defer func() { close(ie.fb.block); wg.Wait() }()
	ie.waitStarted()
	var op lastv1alpha1.OperatorConfig
	if err := ie.crd.Get(ie.ctx, client.ObjectKey{Name: testConfigName}, &op); err != nil {
		t.Fatal(err)
	}
	op.Spec.Images.BuildEgress = []string{hostEgress}
	if err := ie.crd.Update(ie.ctx, &op); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := ie.resolve(toolInstall)
	wantCode(t, err, codes.FailedPrecondition)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the refusal took %v; it queued behind the running build", d)
	}
}

func TestABuilderErrorIsAuditedAndUnavailable(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ie.fb.buildErr = errors.New("create build pod: forbidden")
	_, err := ie.resolve(toolInstall)
	wantCode(t, err, codes.Unavailable)
	results := ie.entriesOf(audit.TypeImageBuildResult)
	if len(results) != 1 {
		t.Fatalf("%d image.build.result entries, want 1", len(results))
	}
	var res buildResult
	if err := json.Unmarshal(results[0].Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != buildFailed || !strings.Contains(res.Error, "create build pod: forbidden") || res.Digest != "" {
		t.Fatalf("image.build.result = %+v", res)
	}
	if _, ok := ie.g.images.get(specHashOf(t, toolInstall)); ok {
		t.Fatal("an unknown outcome was indexed")
	}
}
