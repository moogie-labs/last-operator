package sandbox_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const execChain = "exec-test"

func newAudit(t *testing.T) (*audit.Writer, *audittest.MemSink) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	sink := audittest.NewMemSink()
	w, err := audit.Open(context.Background(), sink, audit.NewSigner(key),
		audit.WriterConfig{ChainID: execChain, Retention: time.Hour}, func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return w, sink
}

func entries(t *testing.T, sink *audittest.MemSink) []audit.Entry {
	t.Helper()
	ctx := context.Background()
	objs, err := sink.List(ctx, audit.EntryPrefix(execChain), "")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]audit.Entry, 0, len(objs))
	for _, o := range objs {
		body, err := sink.Get(ctx, o.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		var e audit.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func entryTypes(es []audit.Entry) []string {
	var ts []string
	for _, e := range es {
		if e.Type != audit.TypeGatewayStart && e.Type != audit.TypeCheckpoint {
			ts = append(ts, e.Type)
		}
	}
	return ts
}

// fakeCluster records created resource kinds and gives created pods the given phase.
func fakeCluster(phase corev1.PodPhase, objs ...runtime.Object) (*fake.Clientset, *[]string) {
	kube := fake.NewClientset(objs...)
	created := &[]string{}
	kube.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		*created = append(*created, a.GetResource().Resource)
		if pod, ok := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Status.Phase = phase
		}
		return false, nil, nil
	})
	return kube, created
}

func envelopeLogs(results ...runnerapi.Result) sandbox.LogReader {
	return func(context.Context, string, string) ([]byte, error) {
		b, _ := json.Marshal(runnerapi.Envelope{Version: runnerapi.Version, Results: results})
		return append([]byte(runnerapi.ResultPrefix), b...), nil
	}
}

func demoSecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-creds", Namespace: "last-gateway"},
		Data: map[string][]byte{tokenKey: []byte(secretValue), configKey: []byte("cfg-" + secretValue)}}
}

func apiServerObjects() []runtime.Object {
	return []runtime.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: kubernetesService, Namespace: metav1.NamespaceDefault},
			Spec: corev1.ServiceSpec{ClusterIP: apiServerHost, Ports: []corev1.ServicePort{{Name: "https", Port: 443}}}},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: kubernetesService, Namespace: metav1.NamespaceDefault, Labels: map[string]string{discoveryv1.LabelServiceName: kubernetesService}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"}}},
			Ports:      []discoveryv1.EndpointPort{{Name: new("https"), Port: new(int32(6443))}},
		},
	}
}

func newExec(kube *fake.Clientset, aud sandbox.Auditor, logs sandbox.LogReader) *sandbox.Executor {
	return sandbox.NewExecutor(kube, providers(envProvider(), k8sProvider("cluster")), aud, sandbox.NewScheduler(2), logs,
		sandbox.ExecutorConfig{
			Pod:               sandbox.PodConfig{Namespace: "last-sandbox", Image: "img:test"},
			ProviderNamespace: "last-gateway",
			PollInterval:      time.Millisecond,
			DeadlineOverhead:  time.Millisecond,
			StartupGrace:      50 * time.Millisecond,
			CleanupTimeout:    time.Second,
			NewRunID:          func() string { return runID },
		})
}

func assertCleaned(t *testing.T, kube *fake.Clientset) {
	t.Helper()
	ctx := context.Background()
	pods, _ := kube.CoreV1().Pods("last-sandbox").List(ctx, metav1.ListOptions{})
	secrets, _ := kube.CoreV1().Secrets("last-sandbox").List(ctx, metav1.ListOptions{})
	nps, _ := kube.NetworkingV1().NetworkPolicies("last-sandbox").List(ctx, metav1.ListOptions{})
	if len(pods.Items)+len(secrets.Items)+len(nps.Items) != 0 {
		t.Fatalf("leftovers: %d pods, %d secrets, %d policies", len(pods.Items), len(secrets.Items), len(nps.Items))
	}
}

func cmd(providers []string, egress []string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: "c1", Run: lastv1alpha1.CommandRun{Argv: []string{"echo", "ok"}},
		Providers: providers, Egress: egress, Timeout: &metav1.Duration{Duration: 10 * time.Millisecond}}
}

func TestRunSucceedsAndAuditsEveryStepInOrder(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	res, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1", ExitCode: 0, Stdout: "ok\n"})).Run(context.Background(),
		sandbox.RunRequest{Intent: "team-x/web", Stage: "Create", Command: cmd([]string{"demo"}, []string{"10.1.2.3:443"})})
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID != runID || res.Result.Stdout != "ok\n" || res.Result.ExitCode != 0 || res.CleanupError != "" {
		t.Fatalf("result = %+v", res)
	}
	es := entries(t, sink)
	want := []string{audit.TypeCommandStart, audit.TypeCredentialIssued, audit.TypeEgressGranted, audit.TypeEgressRevoked, audit.TypeCommandResult}
	if got := entryTypes(es); !slices.Equal(got, want) {
		t.Fatalf("audit types = %v, want %v", got, want)
	}
	for _, e := range es {
		if strings.Contains(string(e.Payload), secretValue) {
			t.Fatalf("entry %s leaks a credential value: %s", e.Type, e.Payload)
		}
		if e.Type != audit.TypeGatewayStart && (e.Intent != "team-x/web" || e.Stage != "Create") {
			t.Fatalf("entry %s lacks intent/stage", e.Type)
		}
	}
	last := es[len(es)-1]
	if !strings.Contains(string(last.Payload), `"stdout":"ok\n"`) || !strings.Contains(string(last.Payload), `"startSeq":1`) {
		t.Fatalf("command.result payload = %s", last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunReportsNonZeroExitWithoutError(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	res, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1", ExitCode: 3, Stderr: "boom"})).Run(context.Background(),
		sandbox.RunRequest{Command: cmd(nil, nil)})
	if err != nil || res.Result.ExitCode != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	es := entries(t, sink)
	if !strings.Contains(string(es[len(es)-1].Payload), `"exitCode":3`) {
		t.Fatalf("result payload = %s", es[len(es)-1].Payload)
	}
	assertCleaned(t, kube)
}

func TestRunTimesOutWhenThePodNeverFinishes(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	aud, sink := newAudit(t)
	start := time.Now()
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Command: cmd(nil, nil)})
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	es := entries(t, sink)
	if last := es[len(es)-1]; last.Type != audit.TypeCommandResult || !strings.Contains(string(last.Payload), "did not finish") {
		t.Fatalf("last entry = %s %s", last.Type, last.Payload)
	}
	assertCleaned(t, kube)
}

func TestRunWithoutAnEnvelopeFails(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodFailed)
	aud, _ := newAudit(t)
	garbage := func(context.Context, string, string) ([]byte, error) { return []byte("segfault\n"), nil }
	_, err := newExec(kube, aud, garbage).Run(context.Background(), sandbox.RunRequest{Command: cmd(nil, nil)})
	if err == nil || !strings.Contains(err.Error(), "no result envelope") {
		t.Fatalf("err = %v", err)
	}
	assertCleaned(t, kube)
}

func TestRunFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	sink.SetFailPuts(true)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Command: cmd([]string{"demo"}, nil)})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if len(*created) != 0 {
		t.Fatalf("created %v although the audit log was down", *created)
	}
}

func TestRunWithKubernetesProviderAllowsTheAPIServer(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenKey {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Command: cmd([]string{clusterName}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	var granted string
	for _, e := range entries(t, sink) {
		if strings.Contains(string(e.Payload), secretValue) {
			t.Fatalf("entry %s leaks the token", e.Type)
		}
		if e.Type == audit.TypeEgressGranted {
			granted = string(e.Payload)
		}
	}
	if !strings.Contains(granted, "10.96.0.1/32:443") || !strings.Contains(granted, "172.18.0.2/32:6443") {
		t.Fatalf("egress.granted = %s", granted)
	}
	assertCleaned(t, kube)
}

func TestRunRejectsImageBuildsBeforeAuditing(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded)
	aud, sink := newAudit(t)
	c := cmd(nil, nil)
	c.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "x"}}}}
	if _, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Command: c}); !errors.Is(err, sandbox.ErrImageBuildsUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if len(entryTypes(entries(t, sink))) != 0 || len(*created) != 0 {
		t.Fatal("rejected command still produced audit entries or objects")
	}
}

func TestRunRaisesTheKubernetesTokenTTLAboveThePodLifetime(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	var expiration int64
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenKey {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if tr.Spec.ExpirationSeconds != nil {
			expiration = *tr.Spec.ExpirationSeconds
		}
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, sink := newAudit(t)
	c := cmd([]string{clusterName}, nil)
	c.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	if _, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(), sandbox.RunRequest{Command: c}); err != nil {
		t.Fatal(err)
	}
	// 30m command + 1ms overhead + 50ms grace, rounded up to whole seconds.
	minSecs := int64((30*time.Minute + time.Millisecond + 50*time.Millisecond + time.Second - 1) / time.Second)
	if expiration < minSecs {
		t.Fatalf("ExpirationSeconds = %d, want >= %d", expiration, minSecs)
	}
	var ttl string
	for _, e := range entries(t, sink) {
		if e.Type == audit.TypeCredentialIssued {
			var p struct {
				TTL string `json:"ttl"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			ttl = p.TTL
		}
	}
	if want := (time.Duration(expiration) * time.Second).String(); ttl != want {
		t.Fatalf("credential.issued ttl = %q, want %q", ttl, want)
	}
	assertCleaned(t, kube)
}
