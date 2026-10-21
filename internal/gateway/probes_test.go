package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func probedResource() lastv1alpha1.ManagedResourceSpec {
	probe := func(id, assert string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Assert: assert, Command: lastv1alpha1.Command{
			Run: lastv1alpha1.CommandRun{Argv: []string{kubectlBin, "get", "ns", "team-x", "-o", "json"}}, Providers: []string{testProvider}}}
	}
	return lastv1alpha1.ManagedResourceSpec{Description: "namespace", Probes: []lastv1alpha1.Probe{
		probe("exists", "result.exitCode == 0"),
		probe("kind", `result.json.kind == "Namespace"`),
	}}
}

func (e *testEnv) probe() (*gatewayv1.RunProbesResponse, error) {
	return e.g.runProbes(e.ctx, &gatewayv1.RunProbesRequest{Intent: ref()})
}

func TestRunProbesNeedsAnExecutedPlan(t *testing.T) {
	env, _ := newRunEnv(t, twoSteps(), probedResource())
	_, err := env.probe()
	env.wantDenied(err, codes.FailedPrecondition)
}

func TestRunProbesRunsTheExecutedPlansProbes(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)

	// The operator rewrites the probes in status after execution; the gateway must ignore that.
	var cur lastv1alpha1.Intent
	if err := env.crd.Get(env.ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Plan.Resources[0].Probes[0].Command.Run.Argv = []string{"rm", "-rf", "/"}
	if err := env.crd.Update(env.ctx, &cur); err != nil {
		t.Fatal(err)
	}

	resp, err := env.probe()
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPlanHash() != in.Status.Plan.Hash || len(resp.GetOutcomes()) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	for _, o := range resp.GetOutcomes() {
		if !o.GetPassed() {
			t.Errorf("probe %s failed: %s", o.GetId(), o.GetError())
		}
	}
	var start string
	for _, e := range env.entries() {
		if e.Type == audit.TypeProbeStart {
			start = string(e.Payload)
			if e.Actor != testOperator || e.Intent != envKey {
				t.Fatalf("probe.start = %+v", e)
			}
		}
	}
	if strings.Contains(start, `"rm"`) || !strings.Contains(start, `"kubectl"`) {
		t.Fatalf("probe.start ran the operator's edited probe: %s", start)
	}
}

func TestRunProbesAppliesTheCurrentPolicy(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	op := allowAll()
	op.Spec.Defaults.Providers = []string{"other"}
	var cur lastv1alpha1.OperatorConfig
	if err := env.crd.Get(env.ctx, client.ObjectKey{Name: testConfigName}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec = op.Spec
	if err := env.crd.Update(env.ctx, &cur); err != nil {
		t.Fatal(err)
	}
	_, err := env.probe()
	env.wantDenied(err, codes.PermissionDenied)
	if env.last().SubjectHash != in.Status.Plan.Hash || env.last().Actor != testOperator {
		t.Fatalf("policy.denied = %+v", env.last())
	}
}

func TestRunProbesAuditsAnInvalidPolicy(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	var cur lastv1alpha1.OperatorConfig
	if err := env.crd.Get(env.ctx, client.ObjectKey{Name: testConfigName}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Defaults.Egress.Allowed = []string{"not a pattern"}
	if err := env.crd.Update(env.ctx, &cur); err != nil {
		t.Fatal(err)
	}
	_, err := env.probe()
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "invalid policy") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunProbesExecutorErrorWithholdsOutcomes(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	env.kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pod creation refused")
	})
	resp, err := env.probe()
	if status.Code(err) != codes.Internal || resp != nil {
		t.Fatalf("executor error = %v, %v; want Internal and no outcomes", resp, err)
	}
	env.g.probing.Range(func(k, _ any) bool { t.Fatalf("in-flight guard left set for %v", k); return false })
}

func TestRunProbesAllowsOneRunInFlight(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	env.g.probing.Store(envKey, struct{}{})
	if _, err := env.probe(); status.Code(err) != codes.Aborted {
		t.Fatalf("overlapping probe run = %v, want Aborted", err)
	}
	env.g.probing.Delete(envKey)
	if _, err := env.probe(); err != nil {
		t.Fatal(err)
	}
}

func TestRunProbesRefusesATamperedExecutedRecord(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	rec, err := env.g.state.LoadAny(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	// Swap a probe's argv but keep PlanHash: the record no longer matches what was approved.
	rec.Executed.Plan = strings.Replace(rec.Executed.Plan, `"kubectl"`, `"rm"`, 1)
	if rec.Executed.Plan == "" || !strings.Contains(rec.Executed.Plan, `"rm"`) {
		t.Fatal("test did not tamper with the record")
	}
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	_, err = env.probe()
	env.wantDenied(err, codes.Internal)
	if !strings.Contains(err.Error(), "integrity check") {
		t.Fatalf("error = %v", err)
	}
	for _, e := range env.entries() {
		if e.Type == audit.TypeProbeStart {
			t.Fatal("a probe ran from a tampered record")
		}
	}
}

// runsProbes asserts that a RunProbes call ran both probes of probedResource.
func (e *testEnv) runsProbes() {
	e.t.Helper()
	before := len(e.entries())
	resp, err := e.probe()
	if err != nil || len(resp.GetOutcomes()) != 2 {
		e.t.Fatalf("RunProbes = %v, %v", resp, err)
	}
	ran := 0
	for _, en := range e.entries()[before:] {
		if en.Type == audit.TypeProbeStart {
			ran++
		}
	}
	if ran == 0 {
		e.t.Fatal("RunProbes ran no probe")
	}
}

// refusesProbes asserts that a RunProbes call was denied and ran nothing.
func (e *testEnv) refusesProbes() {
	e.t.Helper()
	before := len(e.entries())
	_, err := e.probe()
	e.wantDenied(err, codes.PermissionDenied)
	if !strings.Contains(err.Error(), "denies") {
		e.t.Fatalf("refusal = %v; want the Deny named", err)
	}
	wantDeniedByApproval(e.t, err)
	for _, en := range e.entries()[before:] {
		if en.Type == audit.TypeProbeStart {
			e.t.Fatal("a probe ran after a Deny")
		}
	}
}

func TestRunProbesStopsOnADenyForTheIntent(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	env.runsProbes()
	env.approve(approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob"))
	env.refusesProbes()
	// A Deny for the live UID counts at any age.
	env.g.builds.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	env.refusesProbes()
}

func TestRunProbesIgnoresADenyForAnotherUID(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	foreign := approvalFor("no", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")
	foreign.Spec.IntentUID = recreatedUID
	env.approve(foreign)
	legacy := approvalFor("no-legacy", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")
	legacy.Spec.IntentUID = ""
	env.approve(legacy)
	env.runsProbes()
}

// Probes are part of the executed plan: the gateway's own record that an approved
// phase succeeded authorizes them, not a standing Approve.
func TestRunProbesNeedNoStandingApproval(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	if err := env.crd.Delete(env.ctx, approvalFor("ok", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)); err != nil {
		t.Fatal(err)
	}
	// A pre-M5a approval names no UID and authorizes nothing new; probes still run.
	legacy := approvalFor("legacy", in.Status.Plan.Hash, lastv1alpha1.DecisionApprove, approver)
	legacy.Spec.IntentUID = ""
	env.approve(legacy)
	env.runsProbes()
}

func TestRunProbesRunAfterTheApprovalAges(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	env.g.builds.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	env.runsProbes()
}

// A Deny older than the executed phase predates its approval, which outlived
// it, and no longer stops probes (R6). One created since still does, and a
// record without Began counts every Deny.
func TestRunProbesIgnoresADenyOlderThanTheExecutedPhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps(), probedResource())
	executeAll(t, env, in)
	old := approvalFor("no-old", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	env.approve(old)
	env.runsProbes()

	rec, err := env.g.state.Load(env.ctx, envKey, envIntentUID)
	if err != nil {
		t.Fatal(err)
	}
	began := rec.Executed.Began
	rec.Executed.Began = "" // a record written before Began existed
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	env.refusesProbes()
	rec.Executed.Began = began
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	env.runsProbes()

	fresh := approvalFor("no-new", in.Status.Plan.Hash, lastv1alpha1.DecisionDeny, "bob")
	fresh.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Minute))
	env.approve(fresh)
	env.refusesProbes()
}
