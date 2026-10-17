package images_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		})
	}
}

func TestBuildRedactsThePushCredential(t *testing.T) {
	reg, _ := pushed(t)
	b := testBuild()
	stderr := "pw=" + b.PushPassword +
		" auth=" + base64.StdEncoding.EncodeToString([]byte(b.PushUser+":"+b.PushPassword)) +
		" b64=" + base64.StdEncoding.EncodeToString([]byte(b.PushPassword))
	kube := buildKube(corev1.PodFailed)
	log := envelopeLog(runnerapi.Result{ExitCode: 1, Stderr: stderr, Error: "also " + b.PushPassword})
	out, err := images.NewBuilder(kube, fixedLogs(log), reg, testCfg()).Build(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	want := "pw=[REDACTED] auth=[REDACTED] b64=[REDACTED]"
	if out.Log != want {
		t.Fatalf("Log = %q, want %q", out.Log, want)
	}
	if out.Err == nil || strings.Contains(out.Err.Error(), b.PushPassword) {
		t.Fatalf("Err = %v, want one without the password", out.Err)
	}
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
