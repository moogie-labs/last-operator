package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/images/imagestest"
)

const (
	testExtUser    = "robot"
	testExtPW      = "ext-push-s3cret"
	testPullPW     = "ext-pull-s3cret"
	testPushSecret = "registry-push"
	testPullSecret = "registry-pull"
	testExtPath    = "acme/last-images"
	repoField      = "registry.external.repository"
	pushRefField   = "pushSecretRef"
)

// externalOp is imageOp in registry.mode external, pushing to repository.
func externalOp(repository string) *lastv1alpha1.OperatorConfig {
	op := imageOp(nil)
	op.Spec.Registry = &lastv1alpha1.RegistryConfig{Mode: lastv1alpha1.RegistryExternal,
		External: &lastv1alpha1.ExternalRegistry{Repository: repository,
			PushSecretRef: lastv1alpha1.LocalRef{Name: testPushSecret}, PullSecretRef: lastv1alpha1.LocalRef{Name: testPullSecret}}}
	return op
}

// dockerConfigSecret is a kubernetes.io/dockerconfigjson Secret in the gateway namespace.
func dockerConfigSecret(name string, config []byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: config}}
}

// useExternal points ie at ext: the push and pull Secrets exist, and the gateway
// and the fake builder trust ext's certificate.
func useExternal(t *testing.T, ie *imageEnv, ext *imagestest.Registry) {
	t.Helper()
	for _, s := range []*corev1.Secret{
		dockerConfigSecret(testPushSecret, images.DockerConfig(ext.Host, testExtUser, testExtPW)),
		dockerConfigSecret(testPullSecret, images.DockerConfig(ext.Host, "puller", testPullPW)),
	} {
		if _, err := ie.kube.CoreV1().Secrets(ns).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	ie.g.registryTransport = ext.Transport
	ie.fb.transport = ext.Transport
}

func newExternalEnv(t *testing.T) (*imageEnv, *imagestest.Registry, string) {
	t.Helper()
	ext := imagestest.New(t, testExtUser, testExtPW)
	repo := ext.Host + "/" + testExtPath
	ie := newImageEnv(t, externalOp(repo))
	useExternal(t, ie, ext)
	return ie, ext, repo
}

// noSecretsIn fails the test if any audit entry holds a registry credential.
func (e *testEnv) noSecretsIn(secrets ...string) {
	e.t.Helper()
	for _, en := range e.entries() {
		b, _ := json.Marshal(en)
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				e.t.Fatalf("audit entry %d (%s) holds a credential", en.Seq, en.Type)
			}
		}
	}
}

func extSecrets() []string {
	return []string{testExtPW, testPullPW, base64.StdEncoding.EncodeToString([]byte(testExtUser + ":" + testExtPW))}
}

func TestResolveBuildsIntoTheExternalRepository(t *testing.T) {
	ie, ext, repo := newExternalEnv(t)
	ref, err := ie.resolve(toolInstall)
	if err != nil {
		t.Fatal(err)
	}
	hash := specHashOf(t, toolInstall)
	e, ok := ie.g.images.get(hash)
	if !ok || ref != repo+"@"+e.Digest {
		t.Fatalf("ref = %q, index %+v %v; want %s@<digest>", ref, e, ok, repo)
	}
	cfg, spec := ie.fb.cfgs[len(ie.fb.cfgs)-1], ie.fb.specs[0]
	if cfg.PushRef != repo || cfg.Insecure {
		t.Fatalf("BuildPodConfig push %q insecure %v; want %s over HTTPS", cfg.PushRef, cfg.Insecure, repo)
	}
	if !bytes.Equal(spec.PushConfig, images.DockerConfig(ext.Host, testExtUser, testExtPW)) {
		t.Fatal("the build did not get the push Secret's config")
	}
	start := payloadOf[buildStart](t, ie.entriesOf(audit.TypeImageBuildStart)[0])
	// The base is used unchanged and no front-proxy rule is granted.
	if start.PushRef != repo+":"+images.Tag(hash) || !slices.Equal(start.Egress, []string{kubeDNSGrant}) ||
		!strings.HasPrefix(start.Containerfile, "FROM "+testBase+"\n") {
		t.Fatalf("image.build.start = %+v", start)
	}
	if len(spec.Egress) != 0 {
		t.Fatalf("build egress = %v, want only the (absent) buildEgress entries", spec.Egress)
	}
	if !ext.Has(t, testExtPath, e.Digest) {
		t.Fatal("the digest is not in the external repository")
	}
	if ie.heads.Load() != 0 {
		t.Fatalf("%d HEADs at the bundled registry, want none", ie.heads.Load())
	}
	ie.noSecretsIn(extSecrets()...)

	// Once the cached HEAD expires, a resolve checks the external repository with
	// the push credential (the registry answers nothing else) and does not build.
	later := time.Now().Add(headCacheTTL + time.Minute)
	ie.g.builds.now = func() time.Time { return later }
	ext.Heads.Store(0)
	again, err := ie.resolve(toolInstall)
	if err != nil || again != ref {
		t.Fatalf("second resolve = %q, %v; want %q", again, err, ref)
	}
	if ie.fb.builds() != 1 || ext.Heads.Load() != 1 {
		t.Fatalf("%d builds, %d authenticated HEADs; want 1 and 1", ie.fb.builds(), ext.Heads.Load())
	}
}

// A mode switch needs no migration: an entry whose digest the current registry
// lacks fails its HEAD and is rebuilt, even while its old HEAD is still cached.
func TestSwitchingToExternalModeRebuildsIntoTheExternalRepository(t *testing.T) {
	ext := imagestest.New(t, testExtUser, testExtPW)
	repo := ext.Host + "/" + testExtPath
	ie := newImageEnv(t, imageOp(nil))
	useExternal(t, ie, ext)
	bundled, err := ie.resolve(toolInstall)
	if err != nil || !strings.HasPrefix(bundled, testPullHost+"/last/images@") {
		t.Fatalf("bundled resolve = %q, %v", bundled, err)
	}
	var op lastv1alpha1.OperatorConfig
	if err := ie.crd.Get(ie.ctx, client.ObjectKey{Name: testConfigName}, &op); err != nil {
		t.Fatal(err)
	}
	op.Spec.Registry = externalOp(repo).Spec.Registry
	if err := ie.crd.Update(ie.ctx, &op); err != nil {
		t.Fatal(err)
	}
	ref, err := ie.resolve(toolInstall)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := ie.g.images.get(specHashOf(t, toolInstall))
	if ie.fb.builds() != 2 || ref != repo+"@"+e.Digest || !ext.Has(t, testExtPath, e.Digest) {
		t.Fatalf("%d builds, ref %q; want a rebuild into %s", ie.fb.builds(), ref, repo)
	}
}

// podPullSecrets is, for every sandbox pod created so far, the Secrets it pulls with.
func (e *testEnv) podPullSecrets() [][]string {
	pods := e.createdPods()
	out := make([][]string, 0, len(pods))
	for _, p := range pods {
		names := make([]string, 0, len(p.Spec.ImagePullSecrets))
		for _, r := range p.Spec.ImagePullSecrets {
			names = append(names, r.Name)
		}
		out = append(out, names)
	}
	return out
}

func (e *testEnv) pullSecretCopy() *corev1.Secret {
	e.t.Helper()
	s, err := e.kube.CoreV1().Secrets(testSandboxNS).Get(context.Background(), registryPullSecret, metav1.GetOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestExternalModeCopiesThePullSecret(t *testing.T) {
	ext := imagestest.New(t, testExtUser, testExtPW)
	repo := ext.Host + "/" + testExtPath
	ie, in := newImageRunEnv(t, externalOp(repo), imageSteps())
	useExternal(t, ie, ext)
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	if r, err := ie.run(h, in.Status.Plan.Steps[0]); err != nil || r.GetResult().GetExitCode() != 0 {
		t.Fatalf("run = %v, %v", r, err)
	}
	copied := ie.pullSecretCopy()
	if copied.Type != corev1.SecretTypeDockerConfigJson ||
		!bytes.Equal(copied.Data[corev1.DockerConfigJsonKey], images.DockerConfig(ext.Host, "puller", testPullPW)) || len(copied.Data) != 1 {
		t.Fatalf("copied pull Secret = type %q, %d keys", copied.Type, len(copied.Data))
	}
	if imgs := ie.podImages(); len(imgs) != 1 || !strings.HasPrefix(imgs[0], repo+"@sha256:") {
		t.Fatalf("pod images = %v, want the external digest", imgs)
	}

	// The source changes: the next run copies it again.
	source := dockerConfigSecret(testPullSecret, images.DockerConfig(ext.Host, "puller", "rotated-pw"))
	if _, err := ie.kube.CoreV1().Secrets(ns).Update(context.Background(), source, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if r, err := ie.run(h, in.Status.Plan.Steps[1]); err != nil || r.GetResult().GetExitCode() != 0 {
		t.Fatalf("run = %v, %v", r, err)
	}
	if got := ie.pullSecretCopy().Data[corev1.DockerConfigJsonKey]; !bytes.Equal(got, source.Data[corev1.DockerConfigJsonKey]) {
		t.Fatal("the changed pull Secret was not copied again")
	}
	if ps := ie.podPullSecrets(); !slices.EqualFunc(ps, [][]string{{registryPullSecret}, {registryPullSecret}}, slices.Equal) {
		t.Fatalf("pod imagePullSecrets = %v, want %s on both pods", ps, registryPullSecret)
	}
	ie.noSecretsIn(append(extSecrets(), "rotated-pw")...)
}

func TestBundledModeListsNoPullSecret(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), imageSteps())
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	if _, err := ie.run(h, in.Status.Plan.Steps[0]); err != nil {
		t.Fatal(err)
	}
	if ps := ie.podPullSecrets(); len(ps) != 1 || len(ps[0]) != 0 {
		t.Fatalf("pod imagePullSecrets = %v, want none", ps)
	}
	if _, err := ie.kube.CoreV1().Secrets(testSandboxNS).Get(context.Background(), registryPullSecret, metav1.GetOptions{}); err == nil {
		t.Fatal("bundled mode copied a pull Secret")
	}
}

func TestRunProbesInExternalModeUseThePullSecret(t *testing.T) {
	ext := imagestest.New(t, testExtUser, testExtPW)
	ie, in := newImageRunEnv(t, externalOp(ext.Host+"/"+testExtPath), []lastv1alpha1.Command{argvStep("s1", trueBin)}, probesWithImages())
	useExternal(t, ie, ext)
	executeAll(t, ie.testEnv, in)
	if _, err := ie.probe(); err != nil {
		t.Fatal(err)
	}
	for i, ps := range ie.podPullSecrets() {
		if !slices.Equal(ps, []string{registryPullSecret}) {
			t.Fatalf("pod %d imagePullSecrets = %v", i, ps)
		}
	}
}

func TestExternalModeRefusesAMissingPushSecret(t *testing.T) {
	ext := imagestest.New(t, testExtUser, testExtPW)
	op := externalOp(ext.Host + "/" + testExtPath)
	op.Spec.Registry.External.PushSecretRef.Name = "absent"
	ie, in := newImageRunEnv(t, op, imageSteps())
	useExternal(t, ie, ext)
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	_, err := ie.run(h, in.Status.Plan.Steps[0])
	wantCode(t, err, codes.FailedPrecondition)
	if ds := ie.denials(); len(ds) != 1 || !strings.HasPrefix(ds[0], "image: ") || !strings.Contains(ds[0], pushRefField) ||
		!strings.Contains(ds[0], "absent") {
		t.Fatalf("policy.denied reasons = %v", ds)
	}
	if ie.fb.builds() != 0 || !ie.g.phases.get(envKey).failed {
		t.Fatalf("%d builds, phase failed %v; want no build and a failed phase", ie.fb.builds(), ie.g.phases.get(envKey).failed)
	}
}

func TestExternalModeRefusesABadConfiguration(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		name, want string
		repo       func(host string) string
		push       func(host string) []byte // nil keeps the good push config; "" key when empty
		leaks      []string
	}{
		{name: "a scheme", repo: func(h string) string { return "https://" + h + "/x" }, want: repoField},
		{name: "a credential", repo: func(h string) string { return "user:hunter2@" + h + "/x" }, want: repoField,
			leaks: []string{"hunter2"}},
		{name: "a comma", repo: func(h string) string { return h + "/x,push=false" }, want: repoField,
			leaks: []string{"push=false"}},
		{name: "a shell metacharacter", repo: func(h string) string { return h + "/x;id" }, want: repoField},
		{name: "whitespace", repo: func(h string) string { return h + "/x y" }, want: repoField},
		{name: "no path", repo: func(h string) string { return h }, want: repoField},
		{name: "a dotless host (Docker Hub)", repo: func(string) string { return "harbor/acme/x" }, want: repoField},
		{name: "no .dockerconfigjson", push: func(string) []byte { return nil }, want: pushRefField},
		{name: "invalid JSON", push: func(string) []byte { return []byte("{" + testExtPW) }, want: pushRefField,
			leaks: []string{testExtPW}},
		{name: "no credential for the host", push: func(string) []byte { return images.DockerConfig("other.example", "u", testExtPW) },
			want: pushRefField, leaks: []string{testExtPW}},
		{name: "conflicting credentials", push: func(h string) []byte {
			return []byte(`{"auths":{"https://` + h + `":{"auth":"` + b64("a:"+testExtPW) + `"},"` + h + `/v1/":{"auth":"` + b64("b:other-pw") + `"}}}`)
		}, want: pushRefField, leaks: []string{testExtPW, "other-pw"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ie, ext, _ := newExternalEnv(t)
			if tc.repo != nil {
				var op lastv1alpha1.OperatorConfig
				if err := ie.crd.Get(ie.ctx, client.ObjectKey{Name: testConfigName}, &op); err != nil {
					t.Fatal(err)
				}
				op.Spec.Registry.External.Repository = tc.repo(ext.Host)
				if err := ie.crd.Update(ie.ctx, &op); err != nil {
					t.Fatal(err)
				}
			}
			if tc.push != nil {
				s := dockerConfigSecret(testPushSecret, tc.push(ext.Host))
				if s.Data[corev1.DockerConfigJsonKey] == nil {
					s.Data = map[string][]byte{"config.json": images.DockerConfig(ext.Host, testExtUser, testExtPW)}
				}
				if _, err := ie.kube.CoreV1().Secrets(ns).Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ie.resolve(toolInstall)
			wantCode(t, err, codes.FailedPrecondition)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
			for _, leak := range append(tc.leaks, b64(testExtUser+":"+testExtPW)) {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("err = %v, which echoes %q", err, leak)
				}
			}
			if ie.fb.builds() != 0 {
				t.Fatal("a refused resolve built")
			}
		})
	}
}

// seedExternal indexes a verified build of inst pushed to the external repository.
func (ge *gcEnv) seedExternal(ext *imagestest.Registry, inst []lastv1alpha1.ArgvCommand, lastUsed time.Time) (string, string) {
	ge.t.Helper()
	hash, digest := specHashOf(ge.t, inst), ext.Push(ge.t, testExtPath)
	seq := writeBuildResult(ge.t, ge.testEnv, audit.TypeImageBuildResult,
		buildResult{SpecHash: hash, StartSeq: 1, Outcome: buildSucceeded, Digest: digest})
	if err := ge.g.images.put(context.Background(), hash,
		imageEntry{Digest: digest, BuildSeq: seq, LastUsed: lastUsed, Registry: ext.Host + "/" + testExtPath}); err != nil {
		ge.t.Fatal(err)
	}
	return hash, digest
}

// An external registry has no sidecar to protect, so a GC run there never pauses
// builds: it finishes while a build is still running.
func TestImageGCInExternalModeDoesNotPauseBuilds(t *testing.T) {
	ext := imagestest.New(t, testExtUser, testExtPW)
	ge := newGCEnv(t, externalOp(ext.Host+"/"+testExtPath))
	useExternal(t, ge.imageEnv, ext)
	_, digest := ge.seedExternal(ext, trueInstall, time.Now().Add(-31*day))
	ge.fb.block = make(chan struct{})
	built := make(chan error, 1)
	go func() { _, err := ge.resolve(toolInstall); built <- err }()
	ge.waitStarted()

	if err := waitDone(t, ge.runAsync(context.Background())); err != nil {
		t.Fatal(err)
	}
	if ge.g.builds.isPaused() {
		t.Fatal("builds are paused in external mode")
	}
	if _, dels, r := ge.gcAudit(); len(dels) != 1 || !dels[0].RegistryDeleted || r.Deleted != 1 || ext.Has(t, testExtPath, digest) {
		t.Fatalf("deletions %+v, result %+v; want the candidate deleted while the build ran", dels, r)
	}
	close(ge.fb.block)
	if err := <-built; err != nil {
		t.Fatal(err)
	}
}

// Review Focus 5: an external registry that refuses DELETE does not wedge GC.
func TestImageGCInExternalMode(t *testing.T) {
	ext := imagestest.New(t, testExtUser, testExtPW)
	ge := newGCEnv(t, externalOp(ext.Host+"/"+testExtPath))
	useExternal(t, ge.imageEnv, ext)
	old := time.Now().Add(-31 * day)
	h405, d405 := ge.seedExternal(ext, trueInstall, old)
	hUnsupported, dUnsupported := ge.seedExternal(ext, falseInstall, old)
	hDeleted, dDeleted := ge.seedExternal(ext, install([]string{"echo", "ok"}), old)
	ext.RefuseDelete(d405, http.StatusMethodNotAllowed, "")
	ext.RefuseDelete(dUnsupported, http.StatusBadRequest, `{"errors":[{"code":"UNSUPPORTED","message":"The operation is unsupported."}]}`)

	res, err := ge.run()
	if err != nil {
		t.Fatal(err)
	}
	if res != (gcResult{Deleted: 1}) {
		t.Fatalf("result = %+v, want 1 deleted, nothing freed, no sidecar error", res)
	}
	for _, h := range []string{h405, hUnsupported, hDeleted} {
		if _, ok := ge.g.images.get(h); ok {
			t.Fatalf("%s is still indexed", h)
		}
	}
	if ext.Deletes.Load() != 3 {
		t.Fatalf("%d authenticated DELETEs, want 3 with the push credential", ext.Deletes.Load())
	}
	if ext.Has(t, testExtPath, dDeleted) || !ext.Has(t, testExtPath, d405) || !ext.Has(t, testExtPath, dUnsupported) {
		t.Fatal("want only the accepted DELETE to remove its manifest")
	}
	_, dels, result := ge.gcAudit()
	got := map[string]imageDeletedPayload{}
	for _, d := range dels {
		got[d.Digest] = d
	}
	for _, d := range []string{d405, dUnsupported} {
		if p, ok := got[d]; !ok || p.RegistryDeleted || !strings.Contains(p.Error, images.ErrDeleteUnsupported.Error()) {
			t.Fatalf("image.deleted for a refused DELETE = %+v, %v", p, ok)
		}
	}
	if p := got[dDeleted]; !p.RegistryDeleted || p.Error != "" {
		t.Fatalf("image.deleted for the accepted DELETE = %+v", p)
	}
	if result.Deleted != 1 || result.BytesFreed != 0 || result.SidecarError != nil {
		t.Fatalf("image.gc.result = %+v", result)
	}
	if ge.calls.Load() != 0 {
		t.Fatalf("%d sidecar calls in external mode, want none", ge.calls.Load())
	}
	ge.noSecretsIn(append(extSecrets(), testAdminPW)...)
}

// Carried from Task 3's review: a hung DELETE cannot hold builds paused. The first
// DELETE that times out ends the DELETE phase: the remaining candidates stay
// indexed, untouched, for the next run, and the sidecar is still called.
func TestImageGCBoundsEachManifestDelete(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	ge.g.gcDeleteTimeout = 100 * time.Millisecond
	old := time.Now().Add(-31 * day)
	h1, d1 := ge.seed(trueInstall, old)
	h2, d2 := ge.seed(falseInstall, old)
	h3, d3 := ge.seed(toolInstall, old)
	first := slices.Min([]string{h1, h2, h3}) // candidates go in sorted order
	release := ge.holdDeletes()               // only the timeout ends the first DELETE
	done := ge.runAsync(context.Background())
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	release()
	start, dels, result := ge.gcAudit()
	if len(start.Candidates) != 3 || len(dels) != 1 || dels[0].SpecHash != first || dels[0].RegistryDeleted ||
		!strings.Contains(dels[0].Error, "deadline exceeded") {
		t.Fatalf("image.deleted = %+v, want only the first, timed-out DELETE recorded", dels)
	}
	if result.Deleted != 0 || *result.Deferred != 2 || *result.Kept != 0 || ge.calls.Load() != 1 {
		t.Fatalf("image.gc.result = %+v with %d sidecar calls; want 2 deferred and the sidecar called", result, ge.calls.Load())
	}
	for h, d := range map[string]string{h1: d1, h2: d2, h3: d3} {
		if h == first {
			continue
		}
		if _, ok := ge.g.images.get(h); !ok || !ge.inRegistry(d) {
			t.Fatalf("deferred candidate %s left the index or the registry", h)
		}
	}
	if _, ok := ge.g.images.get(first); ok {
		t.Fatal("the timed-out candidate is still indexed")
	}
	if ge.g.builds.isPaused() {
		t.Fatal("builds are still paused")
	}
}

// Carried from Task 3's review: the sidecar's answer is read up to 1 MiB.
func TestImageGCLimitsTheSidecarAnswer(t *testing.T) {
	ge := newGCEnv(t, imageOp(nil))
	huge := `{"bytesFreed": 5, "log": "` + strings.Repeat("x", 2<<20) + `"}`
	ge.answer.Store(&huge)
	res, err := ge.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesFreed != 0 || res.SidecarError == "" {
		t.Fatalf("result = %+v, want an oversized answer refused", res)
	}
	small := `{"bytesFreed": 5, "log": "ok"}`
	ge.answer.Store(&small)
	if res, err := ge.run(); err != nil || res.BytesFreed != 5 || res.SidecarError != "" {
		t.Fatalf("result = %+v, %v; want 5 bytes freed", res, err)
	}
}

// An entry built into another registry (before a mode switch) leaves the index
// without a DELETE: the current registry's 404 would claim a deletion that did not
// happen. A current-registry 404 still counts as deleted.
func TestImageGCLeavesAnotherRegistrysImagesAlone(t *testing.T) {
	old := time.Now().Add(-31 * day)
	t.Run("external mode, bundled entry", func(t *testing.T) {
		ext := imagestest.New(t, testExtUser, testExtPW)
		ge := newGCEnv(t, externalOp(ext.Host+"/"+testExtPath))
		useExternal(t, ge.imageEnv, ext)
		bundledHash, bundledDigest := ge.seed(trueInstall, old) // no Registry: the bundled one
		_, extDigest := ge.seedExternal(ext, falseInstall, old)
		if _, err := ge.run(); err != nil {
			t.Fatal(err)
		}
		got := ge.deletedByDigest()
		want := "built into " + ge.host + "/last/images, not the current registry"
		if p := got[bundledDigest]; p.RegistryDeleted || p.Error != want {
			t.Fatalf("image.deleted for the bundled entry = %+v, want error %q", p, want)
		}
		if _, ok := ge.g.images.get(bundledHash); ok || !ge.inRegistry(bundledDigest) || ge.deletes.Load() != 0 {
			t.Fatalf("bundled entry indexed %v, in registry %v, %d DELETEs; want it unindexed and untouched",
				ok, ge.inRegistry(bundledDigest), ge.deletes.Load())
		}
		if p := got[extDigest]; !p.RegistryDeleted || ext.Deletes.Load() != 1 {
			t.Fatalf("image.deleted for the external entry = %+v with %d DELETEs", p, ext.Deletes.Load())
		}
	})
	t.Run("bundled mode, external entry", func(t *testing.T) {
		ext := imagestest.New(t, testExtUser, testExtPW)
		ge := newGCEnv(t, imageOp(nil))
		useExternal(t, ge.imageEnv, ext)
		extHash, extDigest := ge.seedExternal(ext, trueInstall, old)
		goneHash := specHashOf(t, falseInstall)
		seq := writeBuildResult(t, ge.testEnv, audit.TypeImageBuildResult,
			buildResult{SpecHash: goneHash, StartSeq: 1, Outcome: buildSucceeded, Digest: goneDigest})
		if err := ge.g.images.put(context.Background(), goneHash, imageEntry{Digest: goneDigest, BuildSeq: seq, LastUsed: old}); err != nil {
			t.Fatal(err)
		}
		if _, err := ge.run(); err != nil {
			t.Fatal(err)
		}
		got := ge.deletedByDigest()
		want := "built into " + ext.Host + "/" + testExtPath + ", not the current registry"
		if p := got[extDigest]; p.RegistryDeleted || p.Error != want {
			t.Fatalf("image.deleted for the external entry = %+v, want error %q", p, want)
		}
		if _, ok := ge.g.images.get(extHash); ok || ext.Deletes.Load() != 0 || !ext.Has(t, testExtPath, extDigest) {
			t.Fatalf("external entry indexed %v, %d DELETEs; want it unindexed and untouched", ok, ext.Deletes.Load())
		}
		if p := got[goneDigest]; !p.RegistryDeleted || p.Error != "" {
			t.Fatalf("image.deleted for a current-registry 404 = %+v, want registryDeleted", p)
		}
	})
}

func (ge *gcEnv) deletedByDigest() map[string]imageDeletedPayload {
	_, dels, _ := ge.gcAudit()
	got := map[string]imageDeletedPayload{}
	for _, d := range dels {
		got[d.Digest] = d
	}
	return got
}

// A resolve records the registry each build went to.
func TestTheIndexRecordsTheRegistryOfEachBuild(t *testing.T) {
	ie, _, repo := newExternalEnv(t)
	if _, err := ie.resolve(toolInstall); err != nil {
		t.Fatal(err)
	}
	if e, _ := ie.g.images.get(specHashOf(t, toolInstall)); e.Registry != repo {
		t.Fatalf("entry registry = %q, want %q", e.Registry, repo)
	}
}

// No Provider may mount a registry credential: the bundled admin and pusher
// Secrets, or the Secrets OperatorConfig names for an external registry.
func TestRunCommandRefusesAProviderThatNamesARegistrySecret(t *testing.T) {
	for _, secret := range []string{testPushSecret, testPullSecret, registryAdminSecret, registryPusherSecret} {
		t.Run(secret, func(t *testing.T) {
			step := argvStep("s1", "true")
			step.Providers = []string{testProvider}
			env, in := newRunEnv(t, []lastv1alpha1.Command{step})
			var op lastv1alpha1.OperatorConfig
			if err := env.crd.Get(env.ctx, client.ObjectKey{Name: testConfigName}, &op); err != nil {
				t.Fatal(err)
			}
			op.Spec.Registry = externalOp("reg.example/acme/x").Spec.Registry
			op.Spec.Registry.Mode = lastv1alpha1.RegistryBundled // the names are forbidden whatever the mode
			if err := env.crd.Update(env.ctx, &op); err != nil {
				t.Fatal(err)
			}
			var prov lastv1alpha1.Provider
			if err := env.crd.Get(env.ctx, client.ObjectKey{Name: testProvider}, &prov); err != nil {
				t.Fatal(err)
			}
			prov.Spec.SecretRef.Name = secret
			if err := env.crd.Update(env.ctx, &prov); err != nil {
				t.Fatal(err)
			}
			h := in.Status.Plan.Hash
			if _, err := env.begin(h); err != nil {
				t.Fatal(err)
			}
			_, err := env.run(h, step)
			env.wantDenied(err, codes.PermissionDenied)
		})
	}
}

// Back in bundled mode, a run removes the pull Secret copy an external mode left,
// once per process.
func TestBundledModeRemovesALeftoverPullSecret(t *testing.T) {
	ie, in := newImageRunEnv(t, imageOp(nil), imageSteps())
	if _, err := ie.kube.CoreV1().Secrets(testSandboxNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryPullSecret, Namespace: testSandboxNS}, Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h := in.Status.Plan.Hash
	if _, err := ie.begin(h); err != nil {
		t.Fatal(err)
	}
	for _, step := range in.Status.Plan.Steps {
		if _, err := ie.run(h, step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ie.kube.CoreV1().Secrets(testSandboxNS).Get(context.Background(), registryPullSecret, metav1.GetOptions{}); err == nil {
		t.Fatal("the leftover pull Secret is still there")
	}
	deletes := 0
	for _, act := range ie.kube.Actions() {
		if da, ok := act.(k8stesting.DeleteAction); ok && act.GetResource().Resource == "secrets" && da.GetName() == registryPullSecret {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("%d deletes of %s over two runs, want 1", deletes, registryPullSecret)
	}
}
