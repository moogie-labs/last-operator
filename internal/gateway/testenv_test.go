package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	envNS        = "team-x"
	envIntent    = "web"
	envKey       = envNS + "/" + envIntent
	envIntentUID = "intent-uid"
	approver     = "alice@example.com"

	testConfigName = "default"
	testProvider   = "cluster"
	kubectlBin     = "kubectl"
)

// testEnv is a leading gateway on a fake cluster whose sandbox pods succeed at
// once; result decides what each sandboxed command printed and returned.
type testEnv struct {
	t      *testing.T
	g      *Gateway
	kube   *fake.Clientset
	crd    client.Client
	sink   *audittest.MemSink
	signer *audit.Signer
	ctx    context.Context
	result func(c runnerapi.Command) runnerapi.Result
}

func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	kube := fake.NewClientset()
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	kube.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); s.UID == "" {
			s.UID = types.UID("uid-" + s.Name)
		}
		return false, nil, nil
	})
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	env := &testEnv{t: t, kube: kube, sink: audittest.NewMemSink(), signer: audit.NewSigner(key),
		crd: crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()}
	env.g = env.newGateway()
	env.ctx = context.WithValue(context.Background(), callerKey{}, testOperator)
	return env
}

// newGateway starts a gateway process (a new epoch) on the env's cluster, audit
// chain, and signing key, as a restart would.
func (e *testEnv) newGateway() *Gateway {
	e.t.Helper()
	cfg := Config{Namespace: ns, PodName: testPodName, ChainID: testChainID, Retention: time.Hour,
		SandboxNamespace: testSandboxNS, SandboxImage: "sandbox:test", EgressCanaries: []string{testCanary},
		OperatorIdentity: testOperator, DrainTimeout: time.Second}
	g := New(cfg, e.kube, e.crd, e.sink, e.signer)
	g.execCfg.EgressCanaries = cfg.EgressCanaries
	g.execCfg.PollInterval = time.Millisecond
	g.execCfg.DeadlineOverhead = time.Millisecond
	g.execCfg.StartupGrace = 200 * time.Millisecond
	g.execCfg.CleanupTimeout = time.Second
	g.logs = e.logs
	g.sched.Store(sandbox.NewScheduler(4))
	w, err := audit.Open(context.Background(), e.sink, e.signer,
		audit.WriterConfig{ChainID: testChainID, Retention: time.Hour, Actor: sandbox.GatewayActor}, func(audit.StartInfo) any { return nil })
	if err != nil {
		e.t.Fatal(err)
	}
	g.writer.Store(w)
	g.observeWrites(w)
	return g
}

// logs plays the runner: it reads the run's spec Secret and answers every command.
func (e *testEnv) logs(ctx context.Context, namespace, pod string) ([]byte, error) {
	sec, err := e.kube.CoreV1().Secrets(namespace).Get(ctx, pod+"-spec", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var spec runnerapi.Spec
	if err := json.Unmarshal(sec.Data["spec.json"], &spec); err != nil {
		return nil, err
	}
	envl := runnerapi.Envelope{Version: runnerapi.Version}
	for _, c := range spec.Commands {
		r := runnerapi.Result{}
		if e.result != nil {
			r = e.result(c)
		}
		r.ID = c.ID
		envl.Results = append(envl.Results, r)
	}
	b, _ := json.Marshal(envl)
	return append([]byte(runnerapi.ResultPrefix), b...), nil
}

func (e *testEnv) entries() []audit.Entry {
	e.t.Helper()
	objs, err := e.sink.List(context.Background(), audit.EntryPrefix(testChainID), "")
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]audit.Entry, 0, len(objs))
	for _, o := range objs {
		body, err := e.sink.Get(context.Background(), o.Key, "")
		if err != nil {
			e.t.Fatal(err)
		}
		var en audit.Entry
		if err := json.Unmarshal(body, &en); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, en)
	}
	return out
}

func (e *testEnv) last() audit.Entry {
	e.t.Helper()
	es := e.entries()
	return es[len(es)-1]
}

func (e *testEnv) begin(subject string) (*gatewayv1.BeginPhaseResponse, error) {
	return e.g.beginPhase(e.ctx, &gatewayv1.BeginPhaseRequest{Intent: ref(), Stage: testStage, SubjectHash: subject})
}

func (e *testEnv) end(subject string, succeeded bool) (*gatewayv1.EndPhaseResponse, error) {
	return e.g.endPhase(e.ctx, &gatewayv1.EndPhaseRequest{Intent: ref(), SubjectHash: subject, Succeeded: succeeded})
}

func ref() *gatewayv1.IntentRef { return &gatewayv1.IntentRef{Namespace: envNS, Name: envIntent} }

func argvStep(id string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func intentWithPlan(t *testing.T, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) *lastv1alpha1.Intent {
	t.Helper()
	h, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	return &lastv1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: envIntent, Namespace: envNS, UID: envIntentUID},
		Spec:       lastv1alpha1.IntentSpec{Request: "run a web server"},
		Status: lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseAwaitingApproval, Stage: lastv1alpha1.StageCreate,
			Plan: &lastv1alpha1.Plan{Hash: h, Steps: steps, Resources: resources}},
	}
}

func approvalFor(name, subject string, decision lastv1alpha1.Decision, by string) *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNS, UID: types.UID(name + "-uid"), CreationTimestamp: metav1.Now()},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: envIntent}, IntentUID: envIntentUID, Stage: lastv1alpha1.StageCreate,
			SubjectHash: subject, Decision: decision, ApprovedBy: by},
	}
}

// allowAll is an OperatorConfig whose defaults allow provider "cluster" and egress to 10.0.0.0/8:443.
func allowAll() *lastv1alpha1.OperatorConfig {
	return &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: testConfigName},
		Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{
			Providers: []string{testProvider}, Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.0.0.0/8:443"}}}}}
}
