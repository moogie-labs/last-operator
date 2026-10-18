package images_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/random"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/moogie-labs/last-operator/internal/images"
	"github.com/moogie-labs/last-operator/internal/images/imagestest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// buildKube is a fake cluster whose pods finish in phase as soon as they are created.
func buildKube(phase corev1.PodPhase) *fake.Clientset {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.Status.Phase = phase
		_ = kube.Tracker().Add(pod)
		return true, pod, nil
	})
	return kube
}

func envelopeLog(res runnerapi.Result) []byte {
	res.ID = "build"
	raw, _ := json.Marshal(runnerapi.Envelope{Version: runnerapi.Version, Results: []runnerapi.Result{res}})
	return []byte("noise\n" + runnerapi.ResultPrefix + string(raw) + "\n")
}

func fixedLogs(log []byte) func(context.Context, string, string) ([]byte, error) {
	return func(context.Context, string, string) ([]byte, error) { return log, nil }
}

// pushed pushes a random image to a fresh registry and returns the registry client and digest.
func pushed(t *testing.T) (*images.Client, string) {
	t.Helper()
	host := newRegistry(t)
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := images.NewClient(host, pusher, true).Push(context.Background(), "last/images", "t", img)
	if err != nil {
		t.Fatal(err)
	}
	return images.NewClient(host, nil, true), digest
}

func meta(digest string) string { return `{"containerimage.digest":"` + digest + `"}` }

func leftovers(t *testing.T, kube *fake.Clientset) int {
	t.Helper()
	opts := metav1.ListOptions{LabelSelector: images.LabelBuild}
	pods, _ := kube.CoreV1().Pods(testNS).List(context.Background(), opts)
	secrets, _ := kube.CoreV1().Secrets(testNS).List(context.Background(), opts)
	nps, _ := kube.NetworkingV1().NetworkPolicies(testNS).List(context.Background(), opts)
	return len(pods.Items) + len(secrets.Items) + len(nps.Items)
}

func assertClean(t *testing.T, kube *fake.Clientset) {
	t.Helper()
	if n := leftovers(t, kube); n != 0 {
		t.Fatalf("%d build objects left behind", n)
	}
}

func TestBuildPodMainContainerIsTheRunnerContainer(t *testing.T) {
	_, _, pod, err := images.BuildObjects(testCfg(), testBuild(), time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != sandbox.RunnerContainer {
		t.Fatalf("containers = %+v, want one named %q", pod.Spec.Containers, sandbox.RunnerContainer)
	}
}

func TestBuildReturnsTheDigestFromTheMetadata(t *testing.T) {
	reg, digest := pushed(t)
	kube := buildKube(corev1.PodSucceeded)
	log := envelopeLog(runnerapi.Result{Stdout: meta(digest), Stderr: "step 1\nstep 2\n"})
	b := images.NewBuilder(kube, fixedLogs(log), reg, testCfg())

	out, err := b.Build(context.Background(), testBuild())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.Err != nil || out.Digest != digest || out.Log != "step 1\nstep 2\n" {
		t.Fatalf("outcome = %+v, want digest %s and the log", out, digest)
	}
	if n := leftovers(t, kube); n != 0 {
		t.Fatalf("%d build objects left behind", n)
	}
}

func TestBuildFailsWhenTheDigestIsNotInTheRegistry(t *testing.T) {
	reg, _ := pushed(t)
	kube := buildKube(corev1.PodSucceeded)
	log := envelopeLog(runnerapi.Result{Stdout: meta("sha256:" + strings.Repeat("c", 64))})
	out, err := images.NewBuilder(kube, fixedLogs(log), reg, testCfg()).Build(context.Background(), testBuild())
	if err != nil || out.Err == nil || out.Digest != "" {
		t.Fatalf("Build = %+v, %v; want a failed outcome and nil error", out, err)
	}
	if n := leftovers(t, kube); n != 0 {
		t.Fatalf("%d build objects left behind", n)
	}
}

func TestBuildFailsOnANonZeroExit(t *testing.T) {
	reg, _ := pushed(t)
	kube := buildKube(corev1.PodFailed)
	log := envelopeLog(runnerapi.Result{ExitCode: 1, Stderr: "boom: no such package\n"})
	out, err := images.NewBuilder(kube, fixedLogs(log), reg, testCfg()).Build(context.Background(), testBuild())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.Err == nil || !strings.Contains(out.Err.Error(), "build exited 1") || out.Log != "boom: no such package\n" {
		t.Fatalf("outcome = %+v", out)
	}
	assertClean(t, kube)
}

func TestBuildFailsWithoutAnEnvelope(t *testing.T) {
	reg, _ := pushed(t)
	kube := buildKube(corev1.PodFailed)
	out, err := images.NewBuilder(kube, fixedLogs([]byte("kubelet noise\n")), reg, testCfg()).Build(context.Background(), testBuild())
	if err != nil || out.Err == nil {
		t.Fatalf("Build = %+v, %v; want a failed outcome and nil error", out, err)
	}
	if n := leftovers(t, kube); n != 0 {
		t.Fatalf("%d build objects left behind", n)
	}
}

func TestBuildFailsOnMalformedMetadata(t *testing.T) {
	reg, digest := pushed(t)
	for name, stdout := range map[string]string{
		"not json":   "hello",
		"no digest":  `{"other":"x"}`,
		"bad digest": meta(strings.ToUpper(digest)),
		"short":      meta("sha256:abc"),
	} {
		t.Run(name, func(t *testing.T) {
			kube := buildKube(corev1.PodSucceeded)
			log := envelopeLog(runnerapi.Result{Stdout: stdout})
			out, err := images.NewBuilder(kube, fixedLogs(log), reg, testCfg()).Build(context.Background(), testBuild())
			if err != nil || out.Err == nil || out.Digest != "" {
				t.Fatalf("Build = %+v, %v; want a failed outcome", out, err)
			}
			assertClean(t, kube)
		})
	}
}

func TestBuildRedactsThePushCredential(t *testing.T) {
	reg, _ := pushed(t)
	b := testBuild()
	stderr := "pw=" + testPW +
		" auth=" + base64.StdEncoding.EncodeToString([]byte(testPusher+":"+testPW)) +
		" b64=" + base64.StdEncoding.EncodeToString([]byte(testPW))
	kube := buildKube(corev1.PodFailed)
	log := envelopeLog(runnerapi.Result{ExitCode: 1, Stderr: stderr, Error: "also " + testPW})
	out, err := images.NewBuilder(kube, fixedLogs(log), reg, testCfg()).Build(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	want := "pw=[REDACTED] auth=[REDACTED] b64=[REDACTED]"
	if out.Log != want {
		t.Fatalf("Log = %q, want %q", out.Log, want)
	}
	if out.Err == nil || strings.Contains(out.Err.Error(), testPW) {
		t.Fatalf("Err = %v, want one without the password", out.Err)
	}
	assertClean(t, kube)
}

func TestReapOrphansDeletesExpiredBuildObjects(t *testing.T) {
	reg, _ := pushed(t)
	cfg := testCfg()
	mk := func(id string, expires time.Time, labelled bool) []runtime.Object {
		sec, np, pod, err := images.BuildObjects(cfg, func() images.BuildSpec { b := testBuild(); b.ID = id; return b }(), expires)
		if err != nil {
			t.Fatal(err)
		}
		if !labelled {
			for _, m := range []*metav1.ObjectMeta{&sec.ObjectMeta, &np.ObjectMeta, &pod.ObjectMeta} {
				delete(m.Labels, images.LabelBuild)
			}
		}
		return []runtime.Object{sec, np, pod}
	}
	objs := mk("old", time.Now().Add(-time.Hour), true)
	objs = append(objs, mk("live", time.Now().Add(time.Hour), true)...)
	objs = append(objs, mk("other", time.Now().Add(-time.Hour), false)...)
	kube := fake.NewClientset(objs...)
	b := images.NewBuilder(kube, nil, reg, cfg)

	n, err := b.ReapOrphans(context.Background(), false)
	if err != nil || n != 1 {
		t.Fatalf("ReapOrphans = %d, %v; want 1, nil", n, err)
	}
	if got := leftovers(t, kube); got != 3 {
		t.Fatalf("%d labelled objects left, want the 3 of the live build", got)
	}
	n, err = b.ReapOrphans(context.Background(), true)
	if err != nil || n != 1 {
		t.Fatalf("ReapOrphans(all) = %d, %v; want 1, nil", n, err)
	}
	if got := leftovers(t, kube); got != 0 {
		t.Fatalf("%d labelled objects left after reaping all", got)
	}
	pods, _ := kube.CoreV1().Pods(testNS).List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("unlabelled pods = %d, want 1 untouched", len(pods.Items))
	}
}

func TestBuildReturnsAnErrorWhenCancelledDuringTheWait(t *testing.T) {
	reg, _ := pushed(t)
	kube := buildKube(corev1.PodRunning)
	b := images.NewBuilder(kube, fixedLogs(nil), reg, testCfg())
	b.Tune(5*time.Millisecond, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := b.Build(ctx, testBuild())
	if err == nil || out.Err != nil {
		t.Fatalf("Build = %+v, %v; want a returned error", out, err)
	}
	assertClean(t, kube)
}

func TestBuildCleansUpWhenALaterCreateFails(t *testing.T) {
	reg, _ := pushed(t)
	kube := fake.NewClientset()
	kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pod rejected")
	})
	out, err := images.NewBuilder(kube, fixedLogs(nil), reg, testCfg()).Build(context.Background(), testBuild())
	if err == nil || out.Err != nil {
		t.Fatalf("Build = %+v, %v; want a returned error", out, err)
	}
	assertClean(t, kube)
}

func TestBuildFailsWhenThePodCannotStart(t *testing.T) {
	reg, _ := pushed(t)
	kube := fake.NewClientset()
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "runner", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "InvalidImageName"}},
		}}
		_ = kube.Tracker().Add(pod)
		return true, pod, nil
	})
	out, err := images.NewBuilder(kube, fixedLogs(nil), reg, testCfg()).Build(context.Background(), testBuild())
	if err != nil || out.Err == nil {
		t.Fatalf("Build = %+v, %v; want a failed outcome and nil error", out, err)
	}
	assertClean(t, kube)
}

func TestBuildFailsWhenTheWaitLimitIsReached(t *testing.T) {
	reg, _ := pushed(t)
	kube := buildKube(corev1.PodRunning)
	cfg := testCfg()
	cfg.Timeout = time.Millisecond
	b := images.NewBuilder(kube, fixedLogs(nil), reg, cfg)
	b.Tune(5*time.Millisecond, 50*time.Millisecond)
	out, err := b.Build(context.Background(), testBuild())
	if err != nil || out.Err == nil {
		t.Fatalf("Build = %+v, %v; want a failed outcome and nil error", out, err)
	}
	assertClean(t, kube)
}

func TestBuildReturnsAnErrorWhenTheRegistryIsUnreachable(t *testing.T) {
	_, digest := pushed(t)
	srv := httptest.NewServer(nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	kube := buildKube(corev1.PodSucceeded)
	log := envelopeLog(runnerapi.Result{Stdout: meta(digest)})
	out, err := images.NewBuilder(kube, fixedLogs(log), images.NewClient(host, nil, true), testCfg()).Build(context.Background(), testBuild())
	if err == nil || out.Err != nil {
		t.Fatalf("Build = %+v, %v; want a returned error", out, err)
	}
	assertClean(t, kube)
}

func TestBuildReturnsAnErrorWhenASucceededPodsLogIsUnreadable(t *testing.T) {
	reg, _ := pushed(t)
	kube := buildKube(corev1.PodSucceeded)
	logs := func(context.Context, string, string) ([]byte, error) { return nil, errors.New("log gone") }
	out, err := images.NewBuilder(kube, logs, reg, testCfg()).Build(context.Background(), testBuild())
	if err == nil || out.Err != nil {
		t.Fatalf("Build = %+v, %v; want a returned error", out, err)
	}
	assertClean(t, kube)
}

func TestBuilderRedactsEveryPushConfigCredential(t *testing.T) {
	reg, _ := pushed(t)
	cfgJSON := []byte(`{"auths":{"a.example":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("ua:pw-one")) +
		`"},"b.example":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("ub:pw-two")) + `"}}}`)
	cfg := testCfg()
	cfg.PushRef, cfg.Insecure = "a.example/last-images", false
	b := testBuild()
	b.PushConfig = cfgJSON
	parts := make([]string, 0, 8)
	for _, v := range []string{"ua:pw-one", "ub:pw-two", "pw-one", "pw-two"} {
		parts = append(parts, v, base64.StdEncoding.EncodeToString([]byte(v)))
	}
	kube := buildKube(corev1.PodFailed)
	log := envelopeLog(runnerapi.Result{ExitCode: 1, Stderr: strings.Join(parts, " "), Error: "x pw-two"})
	out, err := images.NewBuilder(kube, fixedLogs(log), reg, cfg).Build(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"pw-one", "pw-two", "dWE6cHctb25l", "dWI6cHctdHdv"} {
		if strings.Contains(out.Log, leak) || (out.Err != nil && strings.Contains(out.Err.Error(), leak)) {
			t.Fatalf("%q leaked: log %q err %v", leak, out.Log, out.Err)
		}
	}
}

// An external build is confirmed in the repository it pushed to, with the push
// credential, over HTTPS: never in the bundled registry.
func TestBuildConfirmsAnExternalPushInItsRepository(t *testing.T) {
	ext := imagestest.New(t, "robot", "ext-s3cret")
	const repo = "acme/last-images"
	digest := ext.Push(t, repo)
	cfg := testCfg()
	cfg.PushRef, cfg.Insecure = ext.Host+"/"+repo, false
	b := testBuild()
	b.PushConfig = images.DockerConfig(ext.Host, ext.User, ext.Password)
	// The bundled client points elsewhere: an external build must not use it.
	bundled, _ := pushed(t)
	bundled = bundled.WithTransport(ext.Transport)

	ext.Heads.Store(0)
	kube := buildKube(corev1.PodSucceeded)
	out, err := images.NewBuilder(kube, fixedLogs(envelopeLog(runnerapi.Result{Stdout: meta(digest)})), bundled, cfg).
		Build(context.Background(), b)
	if err != nil || out.Err != nil || out.Digest != digest {
		t.Fatalf("Build = %+v, %v; want digest %s", out, err, digest)
	}
	if ext.Heads.Load() != 1 {
		t.Fatalf("%d authenticated HEADs at the external registry, want 1", ext.Heads.Load())
	}
	assertClean(t, kube)

	missing := "sha256:" + strings.Repeat("e", 64)
	kube = buildKube(corev1.PodSucceeded)
	out, err = images.NewBuilder(kube, fixedLogs(envelopeLog(runnerapi.Result{Stdout: meta(missing)})), bundled, cfg).
		Build(context.Background(), b)
	if err != nil || out.Err == nil || !strings.Contains(out.Err.Error(), "not in the registry") || out.Digest != "" {
		t.Fatalf("Build = %+v, %v; want a failed build for a digest the repository lacks", out, err)
	}
	assertClean(t, kube)
}

func TestBuildConfirmsAnExternalPushAtAPrivateAddress(t *testing.T) {
	ext := imagestest.New(t, "robot", "ext-s3cret")
	const repo = "acme/last-images"
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	creds := &images.Credentials{Username: ext.User, Password: ext.Password}
	digest, err := images.NewClient(ext.Loopback, creds, false).WithTransport(ext.Transport).Push(context.Background(), repo, "t", img)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg()
	cfg.PushRef, cfg.Insecure = ext.Loopback+"/"+repo, false
	b := testBuild()
	b.PushConfig = images.DockerConfig(ext.Loopback, ext.User, ext.Password)
	kube := buildKube(corev1.PodSucceeded)
	reg := images.NewClient("unused:5000", nil, true).WithTransport(ext.Transport)
	out, err := images.NewBuilder(kube, fixedLogs(envelopeLog(runnerapi.Result{Stdout: meta(digest)})), reg, cfg).
		Build(context.Background(), b)
	if err != nil || out.Err != nil || out.Digest != digest {
		t.Fatalf("Build = %+v, %v; want digest %s", out, err, digest)
	}
}
