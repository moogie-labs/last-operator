package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	testAdminUser  = "admin"
	testAdminPW    = "adm1n-s3cret-pw"
	testBytesFreed = 1234
	day            = 24 * time.Hour
)

// gcEnv is an image env with the admin Secret and a fake GC sidecar.
type gcEnv struct {
	*imageEnv
	sidecar *httptest.Server
	calls   atomic.Int32
	called  chan struct{}                 // receives once per sidecar call, as it arrives
	status  atomic.Int32                  // when set, the sidecar answers with it
	release atomic.Pointer[chan struct{}] // when set, each sidecar call waits for it to close
	answer  atomic.Pointer[string]        // when set, the sidecar answers 200 with it
}

func newGCEnv(t *testing.T, op *lastv1alpha1.OperatorConfig, objs ...client.Object) *gcEnv {
	t.Helper()
	ge := &gcEnv{imageEnv: newImageEnv(t, op, objs...), called: make(chan struct{}, 16)}
	if _, err := ge.kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryAdminSecret, Namespace: ns},
		Data:       map[string][]byte{"username": []byte(testAdminUser), "password": []byte(testAdminPW)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ge.sidecar = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/gc" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if user, pw, ok := r.BasicAuth(); !ok || user != testAdminUser || pw != testAdminPW {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ge.calls.Add(1)
		ge.called <- struct{}{}
		if release := ge.release.Load(); release != nil {
			select {
			case <-*release:
			case <-r.Context().Done():
				return
			}
		}
		if code := ge.status.Load(); code != 0 {
			http.Error(w, "garbage-collect exited 1", int(code))
			return
		}
		if answer := ge.answer.Load(); answer != nil {
			_, _ = io.WriteString(w, *answer)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bytesFreed": testBytesFreed, "log": "blob eligible for deletion"})
	}))
	t.Cleanup(ge.sidecar.Close)
	ge.g.cfg.RegistryGCURL = ge.sidecar.URL
	return ge
}

// hold makes sidecar calls wait until the returned func is called.
func (ge *gcEnv) hold() func() {
	release := make(chan struct{})
	ge.release.Store(&release)
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	ge.t.Cleanup(open)
	return open
}

// holdDeletes makes manifest DELETEs wait until the returned func is called.
func (ge *gcEnv) holdDeletes() func() {
	release := make(chan struct{})
	ge.deleteRelease.Store(&release)
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	ge.t.Cleanup(open)
	return open
}

func (ge *gcEnv) waitDeleting() {
	ge.t.Helper()
	select {
	case <-ge.deleting:
	case <-time.After(5 * time.Second):
		ge.t.Fatal("no manifest DELETE arrived")
	}
}

func (ge *gcEnv) waitSidecar() {
	ge.t.Helper()
	select {
	case <-ge.called:
	case <-time.After(5 * time.Second):
		ge.t.Fatal("the GC sidecar was never called")
	}
}

// seed indexes a verified build of inst, pushed to the registry, last used at lastUsed.
func (ge *gcEnv) seed(inst []lastv1alpha1.ArgvCommand, lastUsed time.Time) (string, string) {
	ge.t.Helper()
	hash, digest := specHashOf(ge.t, inst), ge.pushImage()
	seq := writeBuildResult(ge.t, ge.testEnv, audit.TypeImageBuildResult,
		buildResult{SpecHash: hash, StartSeq: 1, Outcome: buildSucceeded, Digest: digest})
	if err := ge.g.images.put(context.Background(), hash, imageEntry{Digest: digest, BuildSeq: seq, LastUsed: lastUsed}); err != nil {
		ge.t.Fatal(err)
	}
	return hash, digest
}

func (ge *gcEnv) inRegistry(digest string) bool {
	ge.t.Helper()
	found, err := ge.g.registry.Head(context.Background(), images.ImagesRepo, digest)
	if err != nil {
		ge.t.Fatal(err)
	}
	return found
}

func (ge *gcEnv) run() (gcResult, error) {
	return ge.g.runImageGC(context.Background(), ge.g.writer.Load())
}

// runAsync starts a GC run and returns a channel that receives its outcome.
func (ge *gcEnv) runAsync(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := ge.g.runImageGC(ctx, ge.g.writer.Load())
		done <- err
	}()
	return done
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the GC run never finished")
		return nil
	}
}

type gcStartPayload struct {
	Retention  string   `json:"retention"`
	Candidates []string `json:"candidates"`
}

type imageDeletedPayload struct {
	SpecHash        string `json:"specHash"`
	Digest          string `json:"digest"`
	RegistryDeleted bool   `json:"registryDeleted"`
	Error           string `json:"error"`
}

type gcResultAudit struct {
	Deleted      int     `json:"deleted"`
	BytesFreed   int64   `json:"bytesFreed"`
	SidecarError *string `json:"sidecarError"`
	DurationMs   *int64  `json:"durationMs"`
}

func payloadOf[T any](t *testing.T, e audit.Entry) T {
	t.Helper()
	var p T
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// gcAudit returns the one run's start and result payloads and its image.deleted
// payloads, checking their order and that the gateway wrote them with no Intent.
func (ge *gcEnv) gcAudit() (gcStartPayload, []imageDeletedPayload, gcResultAudit) {
	t := ge.t
	t.Helper()
	starts, deleted, results := ge.entriesOf(audit.TypeImageGCStart), ge.entriesOf(audit.TypeImageDeleted), ge.entriesOf(audit.TypeImageGCResult)
	if len(starts) != 1 || len(results) != 1 {
		t.Fatalf("%d image.gc.start and %d image.gc.result entries, want one each", len(starts), len(results))
	}
	var dels []imageDeletedPayload
	for _, e := range append(append([]audit.Entry{starts[0]}, deleted...), results[0]) {
		if e.Actor != sandbox.GatewayActor || e.Intent != "" {
			t.Fatalf("%s written by %q for Intent %q, want the gateway and no Intent", e.Type, e.Actor, e.Intent)
		}
		if e.Type == audit.TypeImageDeleted {
			if e.Seq < starts[0].Seq || e.Seq > results[0].Seq {
				t.Fatalf("image.deleted at seq %d outside the run (%d..%d)", e.Seq, starts[0].Seq, results[0].Seq)
			}
			dels = append(dels, payloadOf[imageDeletedPayload](t, e))
		}
	}
	res := payloadOf[gcResultAudit](t, results[0])
	if res.DurationMs == nil {
		t.Fatal("image.gc.result has no durationMs")
	}
	return payloadOf[gcStartPayload](t, starts[0]), dels, res
}

func TestImageGCDeletesUnusedImages(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	now := time.Now()
	oldHash, oldDigest := ge.seed(trueInstall, now.Add(-31*day))
	freshHash, freshDigest := ge.seed(falseInstall, now.Add(-time.Hour))

	res, err := ge.run()
	if err != nil {
		t.Fatal(err)
	}
	if res != (gcResult{Deleted: 1, BytesFreed: testBytesFreed}) {
		t.Fatalf("result = %+v, want 1 deleted and %d bytes freed", res, testBytesFreed)
	}
	if ge.inRegistry(oldDigest) || !ge.inRegistry(freshDigest) {
		t.Fatal("want only the unused image's manifest deleted")
	}
	if _, ok := ge.g.images.get(oldHash); ok {
		t.Fatal("the unused image is still indexed")
	}
	if _, ok := ge.g.images.get(freshHash); !ok {
		t.Fatal("the image used an hour ago left the index")
	}
	saved := newImageIndex(ge.kube, ns)
	if err := saved.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.get(oldHash); ok {
		t.Fatal("the saved index still has the unused image")
	}

	start, dels, result := ge.gcAudit()
	if start.Retention != (720*time.Hour).String() || !slices.Equal(start.Candidates, []string{oldHash}) {
		t.Fatalf("image.gc.start = %+v, want retention 720h and the unused image", start)
	}
	if len(dels) != 1 || dels[0] != (imageDeletedPayload{SpecHash: oldHash, Digest: oldDigest, RegistryDeleted: true}) {
		t.Fatalf("image.deleted = %+v, want the unused image deleted from the registry", dels)
	}
	if result.Deleted != 1 || result.BytesFreed != testBytesFreed || result.SidecarError != nil {
		t.Fatalf("image.gc.result = %+v, want 1 deleted, %d bytes freed, no sidecar error", result, testBytesFreed)
	}
	if ge.calls.Load() != 1 {
		t.Fatalf("%d sidecar calls, want 1", ge.calls.Load())
	}
}

func TestImageGCUsesTheConfiguredRetention(t *testing.T) {
	op := imageOp(nil)
	op.Spec.Images.UnusedRetention = &metav1.Duration{Duration: time.Hour}
	ge := newGCEnv(t, op)
	now := time.Now()
	hash, digest := ge.seed(trueInstall, now.Add(-2*time.Hour))
	ge.seed(falseInstall, now.Add(-time.Minute))
	if res, err := ge.run(); err != nil || res.Deleted != 1 {
		t.Fatalf("run = %+v, %v; want 1 deleted", res, err)
	}
	if start, _, _ := ge.gcAudit(); start.Retention != time.Hour.String() || !slices.Equal(start.Candidates, []string{hash}) {
		t.Fatalf("image.gc.start = %+v, want retention 1h and %s", start, hash)
	}
	if ge.inRegistry(digest) {
		t.Fatal("the image unused for 2h survived a 1h retention")
	}
}

// Review Focus 2: references keep old images, however long unused.
func TestImageGCKeepsReferencedImages(t *testing.T) {
	inStep, inPlanProbe, inPlanTeardown := install([]string{"step"}), install([]string{"plan-probe"}), install([]string{"plan-teardown"})
	inResourceProbe, inResourceTeardown := install([]string{"mr-probe"}), install([]string{"mr-teardown"})
	unused := install([]string{"unused"})
	withImage := func(inst []lastv1alpha1.ArgvCommand) lastv1alpha1.Command {
		return withInstall(argvStep("c", trueBin), inst)
	}
	intent := intentWithPlan(t, []lastv1alpha1.Command{argvStep("s0", trueBin), withImage(inStep)},
		lastv1alpha1.ManagedResourceSpec{
			Probes:   []lastv1alpha1.Probe{{ID: "p", Command: withImage(inPlanProbe), Assert: trueBin}},
			Teardown: []lastv1alpha1.Command{withImage(inPlanTeardown)},
		})
	mr := &lastv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "team-y"},
		Spec: lastv1alpha1.ManagedResourceSpec{
			Probes:   []lastv1alpha1.Probe{{ID: "p", Command: withImage(inResourceProbe), Assert: trueBin}},
			Teardown: []lastv1alpha1.Command{withImage(inResourceTeardown)},
		}}
	ge := newGCEnv(t, imageOp(nil), intent, mr)
	old := time.Now().Add(-90 * day)
	kept := map[string]string{}
	for _, inst := range [][]lastv1alpha1.ArgvCommand{inStep, inPlanProbe, inPlanTeardown, inResourceProbe, inResourceTeardown} {
		h, d := ge.seed(inst, old)
		kept[h] = d
	}
	unusedHash, unusedDigest := ge.seed(unused, old)

	if res, err := ge.run(); err != nil || res.Deleted != 1 {
		t.Fatalf("run = %+v, %v; want only the unreferenced image deleted", res, err)
	}
	if start, _, _ := ge.gcAudit(); !slices.Equal(start.Candidates, []string{unusedHash}) {
		t.Fatalf("candidates = %v, want only %s", start.Candidates, unusedHash)
	}
	for h, d := range kept {
		if _, ok := ge.g.images.get(h); !ok || !ge.inRegistry(d) {
			t.Fatalf("referenced image %s was collected", h)
		}
	}
	if ge.inRegistry(unusedDigest) {
		t.Fatal("the unreferenced image survived")
	}
}

func TestReferencedSpecsUsesTheGivenBase(t *testing.T) {
	intent := intentWithPlan(t, []lastv1alpha1.Command{withInstall(argvStep("s", trueBin), toolInstall)})
	ge := newGCEnv(t, imageOp(nil), intent)
	refs, err := ge.g.referencedSpecs(context.Background(), testBaseDigest)
	if err != nil || !refs[specHashOf(t, toolInstall)] || len(refs) != 1 {
		t.Fatalf("referencedSpecs = %v, %v; want the step's spec hash", refs, err)
	}
	other := "sha256:" + strings.Repeat("2", 64)
	refs, err = ge.g.referencedSpecs(context.Background(), other)
	if err != nil || refs[specHashOf(t, toolInstall)] {
		t.Fatalf("referencedSpecs on another base = %v, %v; want the hash computed on that base", refs, err)
	}
}

// Review Focus 1: a build running when GC starts finishes before anything is deleted.
func TestImageGCWaitsForARunningBuild(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	ge.seed(trueInstall, time.Now().Add(-31*day))
	ge.fb.block = make(chan struct{})
	built := make(chan error, 1)
	go func() { _, err := ge.resolve(toolInstall); built <- err }()
	ge.waitStarted()

	done := ge.runAsync(context.Background())
	eventually(t, "image.gc.start", func() bool { return len(ge.entriesOf(audit.TypeImageGCStart)) == 1 })
	time.Sleep(settle)
	if n := len(ge.entriesOf(audit.TypeImageDeleted)); n != 0 || ge.calls.Load() != 0 {
		t.Fatalf("%d deletions and %d sidecar calls while a build was running", n, ge.calls.Load())
	}
	close(ge.fb.block)
	if err := <-built; err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	buildResult, deleted := ge.entriesOf(audit.TypeImageBuildResult), ge.entriesOf(audit.TypeImageDeleted)
	// One build result was seeded; the running build wrote the second.
	if len(buildResult) != 2 || len(deleted) != 1 || buildResult[1].Seq > deleted[0].Seq {
		t.Fatalf("build results %v, deletions %v; want the build to finish before the deletion", buildResult, deleted)
	}
}

// Review Focus 3: a resolve that arrives once GC dropped its image waits for GC, then rebuilds.
func TestImageGCMakesAResolveDuringTheRunWaitAndRebuild(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	hash, oldDigest := ge.seed(toolInstall, time.Now().Add(-31*day))
	open := ge.hold()
	done := ge.runAsync(context.Background())
	ge.waitSidecar() // the image is dropped and its manifest deleted

	type outcome struct {
		ref string
		err error
	}
	resolved := make(chan outcome, 1)
	go func() { ref, err := ge.resolve(toolInstall); resolved <- outcome{ref, err} }()
	select {
	case h := <-ge.fb.started:
		t.Fatalf("build %s started during GC", h)
	case o := <-resolved:
		t.Fatalf("resolve returned %+v during GC", o)
	case <-time.After(settle):
	}
	open()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	o := <-resolved
	if o.err != nil {
		t.Fatal(o.err)
	}
	e, ok := ge.g.images.get(hash)
	if !ok || e.Digest == oldDigest || o.ref != ge.g.runRef(e.Digest) || ge.fb.builds() != 1 {
		t.Fatalf("resolve = %q, entry %+v, %d builds; want a rebuild, not %s", o.ref, e, ge.fb.builds(), oldDigest)
	}
	if ge.inRegistry(oldDigest) {
		t.Fatal("the old manifest survived")
	}
}

// Review Focus 3: a resolve that uses a candidate before GC drops it keeps it: GC
// never deletes a digest a resolve has just handed out.
func TestImageGCKeepsAnImageUsedDuringThePause(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	hash, digest := ge.seed(trueInstall, time.Now().Add(-31*day))
	ge.fb.block = make(chan struct{})
	built := make(chan error, 1)
	go func() { _, err := ge.resolve(falseInstall); built <- err }()
	ge.waitStarted()
	done := ge.runAsync(context.Background())
	eventually(t, "image.gc.start", func() bool { return len(ge.entriesOf(audit.TypeImageGCStart)) == 1 })

	ref, err := ge.resolve(trueInstall) // GC is paused, waiting for the build
	if err != nil || ref != ge.g.runRef(digest) {
		t.Fatalf("resolve during the pause = %q, %v; want the indexed image", ref, err)
	}
	close(ge.fb.block)
	if err := <-built; err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	start, dels, result := ge.gcAudit()
	if !slices.Equal(start.Candidates, []string{hash}) || len(dels) != 0 || result.Deleted != 0 {
		t.Fatalf("start %+v, deletions %+v, result %+v; want the candidate kept", start, dels, result)
	}
	if _, ok := ge.g.images.get(hash); !ok || !ge.inRegistry(digest) {
		t.Fatal("GC deleted an image a resolve had just returned")
	}
}

// Review Focus 1: every way a run can fail still resumes the build waiting on it.
func TestImageGCResumesBuildsWhenItFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(ge *gcEnv)
		// atDelete holds the run at its first manifest DELETE instead of at the sidecar call.
		atDelete bool
		// fail ends the held call (open) or cancels the run's context (cancel).
		fail       func(ge *gcEnv, open, cancel func())
		wantErr    bool
		wantResult func(t *testing.T, dels []imageDeletedPayload, r gcResultAudit)
	}{
		{
			name:  "a manifest DELETE fails",
			setup: func(ge *gcEnv) { ge.deleteStatus.Store(http.StatusInternalServerError) },
			fail:  func(_ *gcEnv, open, _ func()) { open() },
			wantResult: func(t *testing.T, dels []imageDeletedPayload, r gcResultAudit) {
				if len(dels) != 1 || dels[0].RegistryDeleted || !strings.Contains(dels[0].Error, "500") || r.Deleted != 0 {
					t.Fatalf("deletions %+v, result %+v; want the failed DELETE recorded", dels, r)
				}
			},
		},
		{
			name:  "the sidecar fails",
			setup: func(ge *gcEnv) { ge.status.Store(http.StatusInternalServerError) },
			fail:  func(_ *gcEnv, open, _ func()) { open() },
			wantResult: func(t *testing.T, _ []imageDeletedPayload, r gcResultAudit) {
				if r.SidecarError == nil || !strings.Contains(*r.SidecarError, "500") || r.BytesFreed != 0 {
					t.Fatalf("result %+v, want the sidecar's failure", r)
				}
			},
		},
		{
			name:    "the run is cancelled",
			setup:   func(*gcEnv) {},
			fail:    func(_ *gcEnv, _, cancel func()) { cancel() },
			wantErr: true,
			wantResult: func(t *testing.T, dels []imageDeletedPayload, r gcResultAudit) {
				if len(dels) != 1 || r.Deleted != 1 || r.SidecarError == nil {
					t.Fatalf("deletions %+v, result %+v; want the deletion and the interrupted sidecar call", dels, r)
				}
			},
		},
		{
			name:     "the run is cancelled during a DELETE",
			setup:    func(ge *gcEnv) { ge.seed(falseInstall, time.Now().Add(-31*day)) },
			atDelete: true,
			fail:     func(_ *gcEnv, _, cancel func()) { cancel() },
			wantErr:  true,
			wantResult: func(t *testing.T, dels []imageDeletedPayload, r gcResultAudit) {
				if len(dels) != 1 || dels[0].RegistryDeleted || r.Deleted != 0 || r.SidecarError == nil ||
					!strings.HasPrefix(*r.SidecarError, "not called") {
					t.Fatalf("deletions %+v, result %+v; want one interrupted DELETE and no sidecar call", dels, r)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ge := newGCEnv(t, imageOp(nil))
			ge.seed(trueInstall, time.Now().Add(-31*day))
			tc.setup(ge)
			var open func()
			if tc.atDelete {
				open = ge.holdDeletes()
			} else {
				open = ge.hold()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := ge.runAsync(ctx)
			if tc.atDelete {
				ge.waitDeleting()
			} else {
				ge.waitSidecar()
			}
			built := make(chan error, 1)
			go func() { _, err := ge.resolve(toolInstall); built <- err }()
			time.Sleep(settle) // the resolve queues behind the pause
			if ge.fb.builds() != 0 {
				t.Fatal("a build started during GC")
			}
			tc.fail(ge, open, cancel)
			if err := waitDone(t, done); (err != nil) != tc.wantErr {
				t.Fatalf("run = %v, want error %v", err, tc.wantErr)
			}
			if err := <-built; err != nil {
				t.Fatalf("the waiting build failed after GC: %v", err)
			}
			_, dels, r := ge.gcAudit()
			tc.wantResult(t, dels, r)
			if ge.g.builds.isPaused() {
				t.Fatal("builds are still paused")
			}
		})
	}
}

// Review Focus 1: cancelling a run that waits for running builds resumes the queued ones.
func TestImageGCCancelledWhileWaitingForBuildsResumesThem(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	ge.seed(trueInstall, time.Now().Add(-31*day))
	ge.fb.block = make(chan struct{}, 2)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Go(func() { _, errs[0] = ge.resolve(falseInstall) })
	ge.waitStarted()
	ctx, cancel := context.WithCancel(context.Background())
	done := ge.runAsync(ctx)
	eventually(t, "image.gc.start", func() bool { return len(ge.entriesOf(audit.TypeImageGCStart)) == 1 })
	wg.Go(func() { _, errs[1] = ge.resolve(toolInstall) })
	select {
	case h := <-ge.fb.started:
		t.Fatalf("build %s started while GC was pausing builds", h)
	case <-time.After(settle):
	}
	cancel()
	if err := waitDone(t, done); err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if got := ge.waitStarted(); got != specHashOf(t, toolInstall) {
		t.Fatalf("started %s, want the queued build", got)
	}
	ge.fb.block <- struct{}{}
	ge.fb.block <- struct{}{}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("builds = %v", errs)
	}
	_, dels, r := ge.gcAudit()
	if len(dels) != 0 || r.Deleted != 0 || ge.calls.Load() != 0 {
		t.Fatalf("deletions %+v, result %+v, %d sidecar calls; want nothing done", dels, r, ge.calls.Load())
	}
}

func TestImageGCRecordsASidecarFailure(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	_, digest := ge.seed(trueInstall, time.Now().Add(-31*day))
	ge.status.Store(http.StatusInternalServerError)
	res, err := ge.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.BytesFreed != 0 || !strings.Contains(res.SidecarError, "500") {
		t.Fatalf("result = %+v, want 1 deleted and the sidecar's 500", res)
	}
	if _, _, r := ge.gcAudit(); r.SidecarError == nil || *r.SidecarError != res.SidecarError {
		t.Fatalf("image.gc.result = %+v, want sidecarError %q", r, res.SidecarError)
	}
	if ge.inRegistry(digest) {
		t.Fatal("the manifest survived a sidecar failure")
	}
	if ge.g.builds.isPaused() {
		t.Fatal("builds are still paused")
	}
	if _, err := ge.resolve(toolInstall); err != nil {
		t.Fatalf("build after a sidecar failure: %v", err)
	}
}

func TestImageGCSkipsTheSidecarWithoutAURL(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	ge.g.cfg.RegistryGCURL = ""
	_, digest := ge.seed(trueInstall, time.Now().Add(-31*day))
	res, err := ge.run()
	if err != nil || res != (gcResult{Deleted: 1}) {
		t.Fatalf("run = %+v, %v; want 1 deleted, nothing freed, no sidecar error", res, err)
	}
	if _, _, r := ge.gcAudit(); r.BytesFreed != 0 || r.SidecarError != nil || ge.calls.Load() != 0 {
		t.Fatalf("image.gc.result = %+v with %d sidecar calls; want no call", r, ge.calls.Load())
	}
	if ge.inRegistry(digest) {
		t.Fatal("the manifest survived")
	}
}

func TestImageGCNeedsAPinnedBase(t *testing.T) {
	op := imageOp(nil)
	op.Spec.Sandbox = nil // the gateway falls back to its unpinned development image
	ge := newGCEnv(t, op)
	_, digest := ge.seed(trueInstall, time.Now().Add(-31*day))
	if _, err := ge.run(); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("run = %v, want the unpinned base refused", err)
	}
	if n := len(ge.entriesOf(audit.TypeImageGCStart)); n != 0 || !ge.inRegistry(digest) || ge.calls.Load() != 0 {
		t.Fatalf("%d image.gc.start entries, sidecar calls %d; want nothing done", n, ge.calls.Load())
	}
}

func TestImageGCRunsOnlyOneAtATime(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	ge.seed(trueInstall, time.Now().Add(-31*day))
	open := ge.hold()
	w := ge.g.writer.Load()
	if !ge.g.startImageGC(context.Background(), w) {
		t.Fatal("the first tick did not start a run")
	}
	ge.waitSidecar()
	if ge.g.startImageGC(context.Background(), w) {
		t.Fatal("a second tick started a run while one was running")
	}
	if _, err := ge.run(); err == nil {
		t.Fatal("runImageGC ran while another run was running")
	}
	open()
	ge.g.inflight.Wait()
	if n := len(ge.entriesOf(audit.TypeImageGCStart)); n != 1 || ge.calls.Load() != 1 {
		t.Fatalf("%d runs, %d sidecar calls; want 1", n, ge.calls.Load())
	}
	if !ge.g.startImageGC(context.Background(), w) {
		t.Fatal("no run started after the first finished")
	}
	ge.g.inflight.Wait()
}

func TestImageGCNeverAuditsTheAdminPassword(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ge := newGCEnv(t, imageOp(nil))
	old := time.Now().Add(-31 * day)
	ge.seed(trueInstall, old)
	errs := make([]error, 0, 3)
	for _, failing := range []bool{false, true} {
		if failing {
			ge.seed(falseInstall, old)
			ge.deleteStatus.Store(http.StatusInternalServerError)
			ge.status.Store(http.StatusUnauthorized)
		}
		_, err := ge.run()
		errs = append(errs, err)
	}
	// A run that fails before it starts: the admin Secret is gone.
	ge.seed(toolInstall, old)
	if err := ge.kube.CoreV1().Secrets(ns).Delete(context.Background(), registryAdminSecret, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := ge.run()
	errs = append(errs, err)
	ge.g.inflight.Wait()

	if n := len(ge.entriesOf(audit.TypeImageGCResult)); n != 2 {
		t.Fatalf("%d runs, want 2", n)
	}
	assertNoSecret(t, ge.sink, logs.String(), errs)
}

func assertNoSecret(t *testing.T, sink *audittest.MemSink, logs string, errs []error) {
	t.Helper()
	objs, err := sink.List(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		body, err := sink.Get(context.Background(), o.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte(testAdminPW)) {
			t.Fatalf("audit object %s holds the admin password", o.Key)
		}
	}
	if strings.Contains(logs, testAdminPW) {
		t.Fatal("the logs hold the admin password")
	}
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), testAdminPW) {
			t.Fatalf("error %q holds the admin password", err)
		}
	}
}

func TestLeadRunsImageGCEachInterval(t *testing.T) {
	sink := audittest.NewMemSink()
	g := testGateway(t, sink, imageOp(nil))
	g.cfg.ImageGCInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.lead(ctx) }()
	eventually(t, "two GC runs", func() bool {
		n := 0
		objs, _ := sink.List(context.Background(), audit.EntryPrefix(testChainID), "")
		for _, o := range objs {
			body, _ := sink.Get(context.Background(), o.Key, "")
			var e audit.Entry
			if json.Unmarshal(body, &e) == nil && e.Type == audit.TypeImageGCResult {
				n++
			}
		}
		return n >= 2
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead returned %v after cancel", err)
	}
}

func (b *imageBuilds) isPaused() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paused
}
