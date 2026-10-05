package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

func (e *testEnv) run(subject string, c lastv1alpha1.Command) (*gatewayv1.RunCommandResponse, error) {
	e.t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.runRaw(subject, c.ID, raw)
}

func (e *testEnv) runRaw(subject, stepID string, raw []byte) (*gatewayv1.RunCommandResponse, error) {
	return e.g.runCommand(e.ctx, &gatewayv1.RunCommandRequest{Intent: ref(), SubjectHash: subject, StepId: stepID, CommandJson: raw})
}

// runAll opens the intent's plan and runs every step as the operator would,
// resolving references from earlier results. The phase stays open.
func runAll(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) plan.Results {
	t.Helper()
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	results := plan.Results{}
	for _, s := range in.Status.Plan.Steps {
		c, err := plan.Resolve(s, results)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := env.run(h, c)
		if err != nil {
			t.Fatalf("step %s: %v", s.ID, err)
		}
		results[s.ID] = runnerapi.Result{ID: s.ID, ExitCode: int(resp.GetResult().GetExitCode()), Stdout: resp.GetResult().GetStdout()}
	}
	return results
}

// executeAll runs the whole plan and ends the phase successfully.
func executeAll(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) {
	t.Helper()
	runAll(t, env, in)
	if _, err := env.end(in.Status.Plan.Hash, true); err != nil {
		t.Fatalf("EndPhase: %v", err)
	}
}

// kubectlJSON makes every kubectl command print a Namespace object and every
// other command print "ok".
func kubectlJSON(c runnerapi.Command) runnerapi.Result {
	if len(c.Argv) > 0 && c.Argv[0] == kubectlBin {
		return runnerapi.Result{Stdout: `{"kind":"Namespace","metadata":{"name":"team-x"},"password":"s3cr3t-value-123"}` + "\n"}
	}
	return runnerapi.Result{Stdout: "ok\n"}
}

// newRunEnv is an approved intent plus provider "cluster": an env-secret Provider
// whose Secret lives in last-gateway (probes in Task 10 use it).
func newRunEnv(t *testing.T, steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) (*testEnv, *lastv1alpha1.Intent) {
	t.Helper()
	in := intentWithPlan(t, steps, resources...)
	provider := &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: testProvider}, Spec: lastv1alpha1.ProviderSpec{
		Type: lastv1alpha1.ProviderEnvSecret, SecretRef: &lastv1alpha1.LocalRef{Name: "cluster-creds"}, Env: map[string]string{"CLUSTER_TOKEN": "token"}}}
	env := newTestEnv(t, in, allowAll(), provider, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver))
	if _, err := env.kube.CoreV1().Secrets("last-gateway").Create(env.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds", Namespace: "last-gateway"},
		Data:       map[string][]byte{"token": []byte("probe-token-value")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	env.result = kubectlJSON
	return env, in
}

func TestRunCommandRunsTheResolvedPlanAndEndsThePhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	r1, err := env.run(h, in.Status.Plan.Steps[0])
	if err != nil || r1.GetResult().GetExitCode() != 0 || !strings.Contains(r1.GetResult().GetStdout(), "Namespace") {
		t.Fatalf("s1 = %v, %v", r1, err)
	}
	r2, err := env.run(h, argvStep("s2", "echo", "Namespace"))
	if err != nil || r2.GetResult().GetStdout() != "ok\n" || r2.GetResult().GetRunId() == "" {
		t.Fatalf("s2 = %v, %v", r2, err)
	}
	var s2Start audit.Entry
	for _, e := range env.entries() {
		if e.Type == audit.TypeCommandStart && strings.Contains(string(e.Payload), `"echo"`) {
			s2Start = e
		}
	}
	if s2Start.Actor != testOperator || s2Start.SubjectHash != h || s2Start.AuthorizedBy == nil ||
		s2Start.AuthorizedBy.Kind != "approval" || !strings.Contains(string(s2Start.Payload), `"Namespace"`) {
		t.Fatalf("s2 command.start = %+v %s", s2Start, s2Start.Payload)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatal(err)
	}
	rec, _ := env.g.state.Load(env.ctx, envKey)
	canonical, _ := plan.Canonical(plan.FromStatus(in.Status.Plan))
	if rec.Executed == nil || rec.Executed.PlanHash != h || rec.Executed.Plan != string(canonical) {
		t.Fatalf("executed record = %+v", rec.Executed)
	}
}

func TestRunCommandRefusesAnythingButTheNextApprovedStep(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	_, err := env.run(h, argvStep("s2", "echo", "x"))
	env.wantDenied(err, codes.FailedPrecondition)
	tampered := in.Status.Plan.Steps[0]
	tampered.Run.Argv = append(append([]string(nil), tampered.Run.Argv...), "--all-namespaces")
	_, err = env.run(h, tampered)
	env.wantDenied(err, codes.PermissionDenied)
	if _, err := env.run(h, in.Status.Plan.Steps[0]); err != nil {
		t.Fatalf("a denied mismatch must leave the phase usable: %v", err)
	}
	_, err = env.run(h, in.Status.Plan.Steps[1])
	env.wantDenied(err, codes.PermissionDenied)
	_, err = env.runRaw(h, "s2", []byte(`{"id":"s2","run":{"argv":["echo","Namespace"]},"surprise":1}`))
	env.wantDenied(err, codes.InvalidArgument)
	for _, bad := range []string{`{"id":"s2","run":{"argv":["echo","Namespace"]}}}`, `{"id":"s2","run":{"argv":["echo","Namespace"]}}]`, `{"id":"s2"} {"id":"s2"}`} {
		_, err = env.runRaw(h, "s2", []byte(bad))
		env.wantDenied(err, codes.InvalidArgument)
	}
}

func TestRunCommandComparesCanonicalJSON(t *testing.T) {
	s1 := argvStep("s1", "true")
	s1.Files = map[string]string{"/work/a": "1", "/work/b": "2"}
	s2 := argvStep("s2", "true")
	s2.Files = map[string]string{"/work/x": "data"}
	env, in := newRunEnv(t, []lastv1alpha1.Command{s1, s2})
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	reordered := []byte(`{"files":{"/work/b":"2","/work/a":"1"},"egress":[],"providers":null,"run":{"argv":["true"]},"id":"s1"}`)
	if _, err := env.runRaw(h, "s1", reordered); err != nil {
		t.Fatalf("map order and nil-versus-empty must not matter: %v", err)
	}
	newline := []byte(`{"id":"s2","run":{"argv":["true"]},"files":{"/work/x":"data\n"}}`)
	_, err := env.runRaw(h, "s2", newline)
	env.wantDenied(err, codes.PermissionDenied)
	exact := []byte(`{"id":"s2","run":{"argv":["true"]},"files":{"/work/x":"data"}}`)
	if _, err := env.runRaw(h, "s2", exact); err != nil {
		t.Fatalf("the approved file content must run: %v", err)
	}
}

func TestRunCommandFailedStepEndsOnlyAsFailed(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	env.result = func(runnerapi.Command) runnerapi.Result { return runnerapi.Result{ExitCode: 3, Stderr: "boom"} }
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	r, err := env.run(h, in.Status.Plan.Steps[0])
	if err != nil || r.GetResult().GetExitCode() != 3 {
		t.Fatalf("failing step = %v, %v", r, err)
	}
	_, err = env.run(h, argvStep("s2", "echo", "x"))
	env.wantDenied(err, codes.FailedPrecondition)
	if _, err := env.end(h, true); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EndPhase(succeeded) after a failure = %v", err)
	}
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
}

func TestRunCommandIsSerializedAndFailsClosed(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	p.mu.Lock()
	if _, err := env.run(h, in.Status.Plan.Steps[0]); status.Code(err) != codes.Aborted {
		t.Fatalf("concurrent step = %v, want Aborted", err)
	}
	p.mu.Unlock()
	env.sink.SetFailPuts(true)
	resp, err := env.run(h, in.Status.Plan.Steps[0])
	if status.Code(err) != codes.Unavailable || resp != nil {
		t.Fatalf("with the audit log down = %v, %v; want Unavailable and no output", resp, err)
	}
	// The broken writer fails closed; give the same gateway a healthy one and
	// check the phase itself stayed failed.
	env.sink.SetFailPuts(false)
	w, err := audit.Open(context.Background(), env.sink, env.signer,
		audit.WriterConfig{ChainID: testChainID, Retention: time.Hour, Actor: sandbox.GatewayActor}, func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	env.g.writer.Store(w)
	_, err = env.run(h, in.Status.Plan.Steps[0])
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestRunCommandExecutorErrorWithholdsOutputAndFailsThePhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	env.kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pod creation refused")
	})
	resp, err := env.run(h, in.Status.Plan.Steps[0])
	if status.Code(err) != codes.Internal || resp != nil {
		t.Fatalf("executor error = %v, %v; want Internal and no output", resp, err)
	}
	_, err = env.run(h, in.Status.Plan.Steps[0])
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestRunCommandResolveFailureDeniesAndFailsThePhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	env.result = func(runnerapi.Command) runnerapi.Result { return runnerapi.Result{Stdout: "not json\n"} }
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, in.Status.Plan.Steps[0]); err != nil {
		t.Fatal(err)
	}
	_, err := env.run(h, argvStep("s2", "echo", "Namespace"))
	env.wantDenied(err, codes.FailedPrecondition)
	if !env.g.phases.get(envKey).failed {
		t.Fatal("a resolve failure must fail the phase")
	}
}

func TestRunCommandRefusesAClosedPhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	if _, err := env.end(h, false); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	if _, err := env.run(h, in.Status.Plan.Steps[0]); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("after EndPhase = %v, want FailedPrecondition", err)
	}
	env.g.phases.put(envKey, p) // a stale pointer to the closed phase
	if _, err := env.run(h, in.Status.Plan.Steps[0]); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("closed phase = %v, want FailedPrecondition", err)
	}
	es := env.entries()
	if env.last().Type != audit.TypePolicyDenied || !strings.Contains(string(env.last().Payload), "phase is closed") {
		t.Fatalf("closed phase did not write policy.denied: %+v", env.last())
	}
	for _, e := range es[before:] {
		if e.Type == audit.TypeCommandStart {
			t.Fatal("a closed phase ran a command")
		}
	}
}

func TestRunCommandRejectsAMalformedSubject(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	if _, err := env.begin(in.Status.Plan.Hash); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	if _, err := env.run("nope", in.Status.Plan.Steps[0]); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed subject = %v, want InvalidArgument", err)
	}
	if len(env.entries()) != before {
		t.Fatal("a malformed subject wrote an audit entry")
	}
}

func TestExecuteAllRunsAndEndsAPlan(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	if env.g.phases.get(envKey) != nil {
		t.Fatal("the phase is still open after executeAll")
	}
}

// wantDenied asserts err has the code and the newest audit entry is policy.denied.
func (e *testEnv) wantDenied(err error, code codes.Code) {
	e.t.Helper()
	if status.Code(err) != code {
		e.t.Fatalf("error = %v, want code %v", err, code)
	}
	if e.last().Type != audit.TypePolicyDenied {
		e.t.Fatalf("last entry = %s, want policy.denied", e.last().Type)
	}
}

func TestRunCommandRechecksApprovalAndPolicyOnEveryStep(t *testing.T) {
	s1 := argvStep("s1", "true")
	s2 := argvStep("s2", "true")
	s2.Providers = []string{testProvider}
	withdraw := func(t *testing.T, env *testEnv, in *lastv1alpha1.Intent) {
		t.Helper()
		if err := env.crd.Create(env.ctx, approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")); err != nil {
			t.Fatal(err)
		}
	}
	narrow := func(t *testing.T, env *testEnv, _ *lastv1alpha1.Intent) {
		t.Helper()
		var cfg lastv1alpha1.OperatorConfig
		if err := env.crd.Get(env.ctx, client.ObjectKey{Name: testConfigName}, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.Spec.Defaults.Providers = []string{tokOther}
		if err := env.crd.Update(env.ctx, &cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		change  func(*testing.T, *testEnv, *lastv1alpha1.Intent)
		message string
	}{{"approval withdrawn", withdraw, "approval withdrawn"}, {"policy narrowed", narrow, "policy:"}} {
		t.Run(tc.name, func(t *testing.T) {
			env, in := newRunEnv(t, []lastv1alpha1.Command{s1, s2})
			h := in.Status.Plan.Hash
			if _, err := env.begin(h); err != nil {
				t.Fatal(err)
			}
			if _, err := env.run(h, s1); err != nil {
				t.Fatal(err)
			}
			tc.change(t, env, in)
			before := len(env.entries())
			_, err := env.run(h, s2)
			env.wantDenied(err, codes.PermissionDenied)
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want %q", err, tc.message)
			}
			for _, e := range env.entries()[before:] {
				if e.Type == audit.TypeCommandStart {
					t.Fatal("a denied step started")
				}
			}
			_, err = env.end(h, true)
			env.wantDenied(err, codes.FailedPrecondition)
			if _, err := env.end(h, false); err != nil {
				t.Fatalf("EndPhase(failed) = %v", err)
			}
		})
	}
}

func TestRunCommandRefusesAProviderThatNamesTheExternalWriterSecret(t *testing.T) {
	const writer = "ext-writer"
	step := argvStep("s1", "true")
	step.Providers = []string{testProvider}
	env, in := newRunEnv(t, []lastv1alpha1.Command{step})
	var prov lastv1alpha1.Provider
	if err := env.crd.Get(env.ctx, client.ObjectKey{Name: testProvider}, &prov); err != nil {
		t.Fatal(err)
	}
	prov.Spec.SecretRef.Name = writer
	if err := env.crd.Update(env.ctx, &prov); err != nil {
		t.Fatal(err)
	}
	env.g.cfg.AuditWriterSecret = writer
	env.g.execCfg.ForbiddenSecrets = []string{writer}
	h := in.Status.Plan.Hash
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	_, err := env.run(h, step)
	env.wantDenied(err, codes.PermissionDenied)
	for _, e := range env.entries() {
		if e.Type == audit.TypeCommandStart || e.Type == audit.TypeCredentialIssued {
			t.Fatalf("a refused provider wrote %s", e.Type)
		}
	}
}

func TestNewForbidsTheConfiguredWriterSecret(t *testing.T) {
	g := New(Config{Namespace: ns, AuditWriterSecret: "ext-writer"}, nil, nil, nil, nil)
	if len(g.execCfg.ForbiddenSecrets) != 1 || g.execCfg.ForbiddenSecrets[0] != "ext-writer" {
		t.Fatalf("ForbiddenSecrets = %v", g.execCfg.ForbiddenSecrets)
	}
	if g := New(Config{Namespace: ns}, nil, nil, nil, nil); len(g.execCfg.ForbiddenSecrets) != 0 {
		t.Fatalf("ForbiddenSecrets = %v", g.execCfg.ForbiddenSecrets)
	}
}
