package sandbox_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	execChain        = "exec-test"
	tokenSubresource = "token"
	testEgress       = "10.1.2.3:443"
)

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
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-creds", Namespace: gatewayNS},
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
	return newExecWith(kube, aud, logs, envProvider(), k8sProvider(clusterName))
}

func newExecWith(kube *fake.Clientset, aud sandbox.Auditor, logs sandbox.LogReader, ps ...*lastv1alpha1.Provider) *sandbox.Executor {
	return sandbox.NewExecutor(kube, providers(ps...), aud, sandbox.NewScheduler(2), logs,
		sandbox.ExecutorConfig{
			Pod:               sandbox.PodConfig{Namespace: "last-sandbox", Image: probeImage},
			ProviderNamespace: gatewayNS,
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
		sandbox.RunRequest{Actor: testActor, Intent: "team-x/web", Stage: "Create", Command: cmd([]string{demoProvider}, []string{testEgress})})
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
		sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)})
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
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)})
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
	aud, sink := newAudit(t)
	garbage := func(context.Context, string, string) ([]byte, error) { return []byte("segfault\n"), nil }
	_, err := newExec(kube, aud, garbage).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)})
	if err == nil || !strings.Contains(err.Error(), "no result envelope") {
		t.Fatalf("err = %v", err)
	}
	if r := resultPayload(t, sink); r.Error == "" || r.ExitCode != -1 {
		t.Fatalf("command.result = %+v, want a recorded error", r)
	}
	assertCleaned(t, kube)
}

func TestRunFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	kube, created := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	sink.SetFailPuts(true)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, nil)})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if len(*created) != 0 {
		t.Fatalf("created %v although the audit log was down", *created)
	}
}

func TestRunWithKubernetesProviderAllowsTheAPIServer(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	withSecretUIDs(kube)
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{clusterName}, nil)})
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
	if _, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c}); !errors.Is(err, sandbox.ErrImageBuildsUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if len(entryTypes(entries(t, sink))) != 0 || len(*created) != 0 {
		t.Fatal("rejected command still produced audit entries or objects")
	}
}

func TestRunRaisesTheKubernetesTokenTTLAboveThePodLifetime(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	withSecretUIDs(kube)
	var expiration int64
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
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
	if _, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c}); err != nil {
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

// ---- fix round 1 helpers and tests ----

type resultEntry struct {
	Error        string `json:"error"`
	CleanupError string `json:"cleanupError"`
	ExitCode     int    `json:"exitCode"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
}

func resultPayload(t *testing.T, sink *audittest.MemSink) resultEntry {
	t.Helper()
	var out resultEntry
	found := false
	for _, e := range entries(t, sink) {
		if e.Type == audit.TypeCommandResult {
			found = true
			if err := json.Unmarshal(e.Payload, &out); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("no command.result entry was written")
	}
	return out
}

func hasType(sink *audittest.MemSink, t *testing.T, typ string) bool {
	t.Helper()
	return slices.Contains(entryTypes(entries(t, sink)), typ)
}

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ev)
}

func (l *eventLog) count(prefix string) int {
	n := 0
	for _, e := range l.all() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// recordKube logs every kube action. Call it last so its reactor runs first.
func recordKube(kube *fake.Clientset, l *eventLog) {
	kube.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		res := a.GetResource().Resource
		if a.GetSubresource() != "" {
			res += "/" + a.GetSubresource()
		}
		l.add("kube:" + a.GetVerb() + ":" + res + ":" + a.GetNamespace())
		return false, nil, nil
	})
}

// recAuditor wraps a real writer, logs audit events, can fail the Nth Append and, like
// a real sink client, refuses a cancelled context.
type recAuditor struct {
	inner  sandbox.Auditor
	log    *eventLog
	failAt int // 1-based Append number that fails; 0 never
	n      int
}

var errSinkDown = errors.New("sink down")

func (r *recAuditor) Append(ctx context.Context, d audit.Draft) (audit.Entry, error) {
	r.n++
	if ctx.Err() != nil {
		return audit.Entry{}, ctx.Err()
	}
	if r.failAt != 0 && r.n == r.failAt {
		return audit.Entry{}, errSinkDown
	}
	e, err := r.inner.Append(ctx, d)
	if err == nil {
		r.log.add("audit:" + d.Type)
	}
	return e, err
}

func (r *recAuditor) Do(ctx context.Context, before audit.Draft, action func(context.Context) (any, error),
	after func(audit.Entry, any, error) audit.Draft) error {
	return r.inner.Do(ctx, before, func(c context.Context) (any, error) {
		r.log.add("audit:" + before.Type)
		return action(c)
	}, func(b audit.Entry, res any, err error) audit.Draft {
		d := after(b, res, err)
		r.log.add("audit:" + d.Type)
		return d
	})
}

func newRecAudit(t *testing.T, failAt int) (*recAuditor, *audittest.MemSink, *eventLog) {
	t.Helper()
	w, sink := newAudit(t)
	l := &eventLog{}
	return &recAuditor{inner: w, log: l, failAt: failAt}, sink, l
}

func TestRunRedactsIssuedCredentialsFromOutput(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretValue))
	raw := base64.RawStdEncoding.EncodeToString([]byte(secretValue))
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink := newAudit(t)
	leak := runnerapi.Result{ID: "c1",
		Stdout: "a " + secretValue + " b " + b64 + " c " + raw + "\n",
		Stderr: "e " + secretValue + " " + b64, Error: "err " + secretValue}
	res, err := newExec(kube, aud, envelopeLogs(leak)).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries(t, sink) {
		for _, bad := range []string{secretValue, b64, raw} {
			if strings.Contains(string(e.Payload), bad) {
				t.Fatalf("entry %s leaks %q: %s", e.Type, bad, e.Payload)
			}
		}
	}
	for _, got := range []string{res.Result.Stdout, res.Result.Stderr, res.Result.Error} {
		for _, bad := range []string{secretValue, b64, raw} {
			if strings.Contains(got, bad) {
				t.Fatalf("RunResult leaks %q: %+v", bad, res.Result)
			}
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("%q lacks [REDACTED]", got)
		}
	}
	if r := resultPayload(t, sink); !strings.Contains(r.Stdout, "[REDACTED]") || !strings.Contains(r.Stderr, "[REDACTED]") {
		t.Fatalf("command.result = %+v", r)
	}
}

func TestRunRedactsMintedKubernetesTokens(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	withSecretUIDs(kube)
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, sink := newAudit(t)
	res, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1", Stdout: "token=tok-" + secretValue})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{clusterName}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Result.Stdout, secretValue) || !strings.Contains(res.Result.Stdout, "[REDACTED]") {
		t.Fatalf("stdout = %q", res.Result.Stdout)
	}
	if r := resultPayload(t, sink); strings.Contains(r.Stdout, secretValue) {
		t.Fatalf("command.result leaks the token: %q", r.Stdout)
	}
}

func TestRunDoesNotRedactValuesShorterThanEightBytes(t *testing.T) {
	short := &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "short"}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "short-creds"},
		Env: map[string]string{"SHORT": tokenKey}}}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "short-creds", Namespace: gatewayNS},
		Data: map[string][]byte{tokenKey: []byte("abc1234")}} // 7 bytes
	kube, _ := fakeCluster(corev1.PodSucceeded, sec)
	aud, _ := newAudit(t)
	res, err := newExecWith(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1", Stdout: "v=abc1234"}), short).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{"short"}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result.Stdout != "v=abc1234" {
		t.Fatalf("stdout = %q, want it untouched", res.Result.Stdout)
	}
}

func TestRunFailsClosedAtCredentialIssued(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, append(apiServerObjects(), demoSecret())...)
	aud, sink, l := newRecAudit(t, 1)
	recordKube(kube, l)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider, clusterName}, []string{testEgress})})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if n := l.count("kube:get:secrets:" + gatewayNS); n != 0 {
		t.Fatalf("read %d provider secrets before credential.issued was durable", n)
	}
	if n := l.count("kube:create:serviceaccounts/token"); n != 0 {
		t.Fatalf("minted %d tokens", n)
	}
	if n := l.count("kube:create:") - l.count("kube:create:serviceaccounts/token"); n != 0 {
		t.Fatalf("created %d objects: %v", n, l.all())
	}
	if r := resultPayload(t, sink); r.Error == "" {
		t.Fatal("command.result lacks the error")
	}
}

func TestRunFailsClosedAtEgressGranted(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, sink, l := newRecAudit(t, 2)
	recordKube(kube, l)
	_, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, []string{testEgress})})
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if n := l.count("kube:create:networkpolicies"); n != 0 {
		t.Fatalf("created %d network policies without an egress.granted entry", n)
	}
	if l.count("kube:create:secrets:"+sandboxNS) == 0 {
		t.Fatal("expected the credential secret to have been created first")
	}
	if hasType(sink, t, audit.TypeEgressGranted) {
		t.Fatal("egress.granted should not have been recorded")
	}
	if r := resultPayload(t, sink); r.Error == "" {
		t.Fatal("command.result lacks the error")
	}
	assertCleaned(t, kube)
}

func TestRunInterleavesAuditAndKubeActionsInOrder(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	aud, _, l := newRecAudit(t, 0)
	recordKube(kube, l)
	if _, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, []string{testEgress})}); err != nil {
		t.Fatal(err)
	}
	ev := l.all()
	at := func(s string) int {
		i := slices.Index(ev, s)
		if i < 0 {
			t.Fatalf("event %q missing from %v", s, ev)
		}
		return i
	}
	if at("audit:"+audit.TypeCommandStart) >= at("audit:"+audit.TypeCredentialIssued) {
		t.Fatalf("command.start must come first: %v", ev)
	}
	if at("audit:"+audit.TypeCredentialIssued) >= at("kube:get:secrets:"+gatewayNS) {
		t.Fatalf("credential.issued must precede the provider secret read: %v", ev)
	}
	if at("audit:"+audit.TypeEgressGranted) >= at("kube:create:networkpolicies:"+sandboxNS) {
		t.Fatalf("egress.granted must precede the policy create: %v", ev)
	}
	if at("kube:delete:networkpolicies:"+sandboxNS) >= at("audit:"+audit.TypeEgressRevoked) {
		t.Fatalf("the policy delete must precede egress.revoked: %v", ev)
	}
	if ev[len(ev)-1] != "audit:"+audit.TypeCommandResult {
		t.Fatalf("command.result must be last: %v", ev)
	}
}

func TestRunCancelledWhilePendingStillCleansUpAndAudits(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending, demoSecret())
	aud, sink, l := newRecAudit(t, 0)
	c := cmd([]string{demoProvider}, []string{testEgress})
	c.Timeout = &metav1.Duration{Duration: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	res, err := newExec(kube, aud, envelopeLogs()).Run(ctx, sandbox.RunRequest{Actor: testActor, Command: c})
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("err = %v, want a cancellation error", err)
	}
	if strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("cancellation reported as a timeout: %v", err)
	}
	if res.CleanupError != "" {
		t.Fatalf("cleanup error: %s", res.CleanupError)
	}
	for _, typ := range []string{audit.TypeEgressRevoked, audit.TypeCommandResult} {
		if !hasType(sink, t, typ) {
			t.Fatalf("missing %s; events %v", typ, l.all())
		}
	}
	if r := resultPayload(t, sink); !strings.Contains(r.Error, "cancel") {
		t.Fatalf("command.result error = %q", r.Error)
	}
	assertCleaned(t, kube)
}

func TestRunRecordsCleanupErrorWithoutRevokedWhenPolicyDeleteFails(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	kube.PrependReactor("delete", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver says no")
	})
	aud, sink := newAudit(t)
	res, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, []string{testEgress})})
	if err != nil {
		t.Fatal(err)
	}
	if res.CleanupError == "" {
		t.Fatal("RunResult.CleanupError is empty")
	}
	if r := resultPayload(t, sink); r.CleanupError == "" {
		t.Fatal("command.result.cleanupError is empty")
	}
	if hasType(sink, t, audit.TypeEgressRevoked) {
		t.Fatal("egress.revoked was written although the policy delete failed")
	}
}

func TestRunCleansUpWhenPodCreateFails(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, demoSecret())
	kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	aud, sink := newAudit(t)
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{demoProvider}, []string{testEgress})})
	if err == nil || !strings.Contains(err.Error(), "create pod") {
		t.Fatalf("err = %v", err)
	}
	if r := resultPayload(t, sink); !strings.Contains(r.Error, "create pod") {
		t.Fatalf("command.result error = %q", r.Error)
	}
	if !hasType(sink, t, audit.TypeEgressRevoked) {
		t.Fatal("egress.revoked missing")
	}
	assertCleaned(t, kube)
}

func TestRunFailsFastOnImagePullBackOff(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
		return false, nil, nil
	})
	aud, sink := newAudit(t)
	c := cmd(nil, nil)
	c.Timeout = &metav1.Duration{Duration: time.Minute}
	start := time.Now()
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c})
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("did not fail fast: %s", time.Since(start))
	}
	if r := resultPayload(t, sink); !strings.Contains(r.Error, "ImagePullBackOff") {
		t.Fatalf("command.result error = %q", r.Error)
	}
	assertCleaned(t, kube)
}

func TestRunFailsFastWhenThePodIsDeletedExternally(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodPending)
	kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), "last-run-run1")
	})
	aud, _ := newAudit(t)
	c := cmd(nil, nil)
	c.Timeout = &metav1.Duration{Duration: time.Minute}
	start := time.Now()
	_, err := newExec(kube, aud, envelopeLogs()).Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: c})
	if err == nil || !strings.Contains(err.Error(), "deleted externally") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("did not fail fast: %s", time.Since(start))
	}
}

func TestRunDefaultRunIDIs32HexCharacters(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded)
	aud, _ := newAudit(t)
	ex := sandbox.NewExecutor(kube, providers(), aud, sandbox.NewScheduler(1), envelopeLogs(runnerapi.Result{ID: "c1"}),
		sandbox.ExecutorConfig{Pod: sandbox.PodConfig{Namespace: sandboxNS, Image: probeImage}, PollInterval: time.Millisecond})
	res, err := ex.Run(context.Background(), sandbox.RunRequest{Actor: testActor, Command: cmd(nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RunID) != 32 {
		t.Fatalf("run id %q has %d chars", res.RunID, len(res.RunID))
	}
	if _, err := hex.DecodeString(res.RunID); err != nil {
		t.Fatal(err)
	}
}

// withSecretUIDs makes the fake clientset assign UIDs to created Secrets, as a real
// API server does; token anchors need one for boundObjectRef.
func withSecretUIDs(kube *fake.Clientset) {
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		s.UID = types.UID("uid-" + s.Name)
		return false, nil, nil
	})
}

func TestRunDeletesTheTokenAnchorOnCleanup(t *testing.T) {
	kube, _ := fakeCluster(corev1.PodSucceeded, apiServerObjects()...)
	withSecretUIDs(kube)
	kube.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != tokenSubresource {
			return false, nil, nil
		}
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if tr.Spec.BoundObjectRef == nil || tr.Spec.BoundObjectRef.Name != "last-run-"+runID+sandbox.AnchorSuffix {
			t.Errorf("token request is not bound to the run anchor: %+v", tr.Spec.BoundObjectRef)
		}
		tr.Status.Token = "tok-" + secretValue
		return true, tr, nil
	})
	aud, _ := newAudit(t)
	if _, err := newExec(kube, aud, envelopeLogs(runnerapi.Result{ID: "c1"})).Run(context.Background(),
		sandbox.RunRequest{Actor: testActor, Command: cmd([]string{clusterName}, nil)}); err != nil {
		t.Fatal(err)
	}
	left, err := kube.CoreV1().Secrets(gatewayNS).List(context.Background(),
		metav1.ListOptions{LabelSelector: sandbox.LabelRun + "=" + runID})
	if err != nil || len(left.Items) != 0 {
		t.Fatalf("anchors left in last-gateway: %v (err %v)", left, err)
	}
	deleted := false
	for _, a := range kube.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "secrets" && a.GetNamespace() == gatewayNS {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("cleanup never deleted the anchor in last-gateway")
	}
}
