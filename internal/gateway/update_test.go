package gateway

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const (
	nonce1 = "0123456789abcdef0123456789abcdef"
	nonce2 = "fedcba9876543210fedcba9876543210"
)

// executedResources are the resources of executedEnv's executed Create plan.
func executedResources() []lastv1alpha1.ManagedResourceSpec {
	res := probedResource()
	res.Teardown = teardownCommands()
	return []lastv1alpha1.ManagedResourceSpec{res}
}

func repairSteps() []lastv1alpha1.Command {
	return []lastv1alpha1.Command{argvStep("fix", kubectlBin, "apply", "-f", "/work/web.yaml")}
}

func stageApproval(name string, stage lastv1alpha1.Stage, subject string) *lastv1alpha1.IntentApproval {
	a := approvalFor(name, subject, lastv1alpha1.DecisionApprove, approver)
	a.Spec.Stage = stage
	return a
}

// recordPlan records b as the stored Intent's plan in stage, as the operator
// does, and returns its subject.
func (e *testEnv) recordPlan(stage lastv1alpha1.Stage, b plan.Body) string {
	e.t.Helper()
	var in lastv1alpha1.Intent
	if err := e.crd.Get(e.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &in); err != nil {
		e.t.Fatal(err)
	}
	h, err := plan.Hash(b)
	if err != nil {
		e.t.Fatal(err)
	}
	in.Status.Stage, in.Status.Phase = stage, lastv1alpha1.PhaseAwaitingApproval
	in.Status.Plan = &lastv1alpha1.Plan{Hash: h, Steps: b.Steps, Resources: b.Resources, Nonce: b.Nonce}
	if err := e.crd.Update(e.ctx, &in); err != nil {
		e.t.Fatal(err)
	}
	return h
}

// remediation records a Remediate plan of repairSteps with the executed
// resources and nonce, approves it, and returns its subject.
func (e *testEnv) remediation(name, nonce string) string {
	e.t.Helper()
	h := e.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: executedResources(), Nonce: nonce})
	e.approve(stageApproval(name, lastv1alpha1.StageRemediate, h))
	return h
}

// runPhase opens stage's phase for subject, runs steps, and ends it as succeeded.
func (e *testEnv) runPhase(stage lastv1alpha1.Stage, subject string, steps []lastv1alpha1.Command) {
	e.t.Helper()
	if _, err := e.beginStage(stage, subject); err != nil {
		e.t.Fatalf("BeginPhase(%s) = %v", stage, err)
	}
	for _, s := range steps {
		if _, err := e.run(subject, s); err != nil {
			e.t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	if _, err := e.end(subject, true); err != nil {
		e.t.Fatalf("EndPhase(%s) = %v", stage, err)
	}
}

func (e *testEnv) executed() ExecutedPlan {
	e.t.Helper()
	rec, err := e.g.state.Load(e.ctx, envKey, envIntentUID)
	if err != nil || rec.Executed == nil {
		e.t.Fatalf("record = %+v, %v", rec, err)
	}
	return *rec.Executed
}

// refusedWith asserts a BeginPhase refusal: its code and reason, audited as
// policy.denied, and no phase.begin written since before.
func (e *testEnv) refusedWith(err error, code codes.Code, reason string, before int) {
	e.t.Helper()
	e.wantDenied(err, code)
	if !strings.Contains(err.Error(), reason) {
		e.t.Fatalf("BeginPhase = %v, want %q", err, reason)
	}
	for _, en := range e.entries()[before:] {
		if en.Type == audit.TypePhaseBegin {
			e.t.Fatal("a refused BeginPhase wrote phase.begin")
		}
	}
}

func TestUpdateNeedsAnExecutedPlan(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newProviderEnv(t, in, stageApproval("ok", lastv1alpha1.StageUpdate, in.Status.Plan.Hash))
	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: twoSteps()})
	_, err := env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.FailedPrecondition, "intent has no executed plan to update", 0)
}

func TestASucceededUpdateReplacesTheExecutedPlan(t *testing.T) {
	env, in := executedEnv(t)
	before := env.executed()
	if before.Began == "" {
		t.Fatal("the executed Create plan has no Began")
	}
	// The Update manages a different resource with a single probe.
	res := probedResource()
	res.Description = "namespace, relabelled"
	res.Probes = res.Probes[:1]
	steps := []lastv1alpha1.Command{argvStep("u1", kubectlBin, "label", "ns", "team-x", "tier=web")}
	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: steps, Resources: []lastv1alpha1.ManagedResourceSpec{res}})
	if h == in.Status.Plan.Hash {
		t.Fatal("the Update plan has the Create plan's subject")
	}
	env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	begun := time.Now().UTC().Truncate(time.Second)
	env.runPhase(lastv1alpha1.StageUpdate, h, steps)

	got := env.executed()
	if got.Stage != string(lastv1alpha1.StageUpdate) || got.PlanHash != h {
		t.Fatalf("executed = %s %s, want the Update %s", got.Stage, got.PlanHash, h)
	}
	if at, err := time.Parse(time.RFC3339, got.Began); err != nil || at.Before(begun) || at.After(time.Now()) {
		t.Fatalf("Began = %q, %v; want the Update's BeginPhase time", got.Began, err)
	}
	resp, err := env.probe()
	if err != nil || resp.GetPlanHash() != h || len(resp.GetOutcomes()) != 1 {
		t.Fatalf("RunProbes = %+v, %v; want the Update plan's one probe", resp, err)
	}
	// The Update ran once; its approval opens it no more.
	_, err = env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.FailedPrecondition, "runs only once", len(env.entries()))
}

func TestRemediateNeedsTheExecutedResources(t *testing.T) {
	// No executed plan: nothing to remediate.
	fresh, _ := newRunEnv(t, twoSteps(), probedResource())
	h := fresh.remediation("fix", nonce1)
	_, err := fresh.beginStage(lastv1alpha1.StageRemediate, h)
	fresh.refusedWith(err, codes.FailedPrecondition, "intent has no executed plan to remediate", 0)

	env, _ := executedEnv(t)
	for name, resources := range map[string][]lastv1alpha1.ManagedResourceSpec{
		"changed": {probedResource()}, // the teardown is gone
		"dropped": nil,
		"added":   append(executedResources(), lastv1alpha1.ManagedResourceSpec{Description: "extra"}),
	} {
		h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: resources, Nonce: nonce1})
		env.approve(stageApproval("fix-"+name, lastv1alpha1.StageRemediate, h))
		before := len(env.entries())
		_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
		env.refusedWith(err, codes.FailedPrecondition, "a Remediate plan must carry the executed plan's resources unchanged", before)
	}

	// The IntentRef the operator fills in is not a change.
	resources := executedResources()
	resources[0].IntentRef = &lastv1alpha1.LocalRef{Name: envIntent}
	h = env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: resources, Nonce: nonce2})
	env.approve(stageApproval("fix-ref", lastv1alpha1.StageRemediate, h))
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
		t.Fatalf("BeginPhase(Remediate with intentRef) = %v", err)
	}
}

func TestASucceededRemediateKeepsTheExecutedPlan(t *testing.T) {
	env, in := executedEnv(t)
	before := env.executed()
	h := env.remediation("fix", nonce1)
	env.runPhase(lastv1alpha1.StageRemediate, h, repairSteps())

	if got := env.executed(); got != before {
		t.Fatalf("executed = %+v, want the Create plan kept: %+v", got, before)
	}
	resp, err := env.probe()
	if err != nil || resp.GetPlanHash() != in.Status.Plan.Hash {
		t.Fatalf("RunProbes = %+v, %v; want the Create plan's probes", resp, err)
	}
}

func TestARemediateWritesNoOutputs(t *testing.T) {
	env, _ := executedEnv(t)
	h := env.remediation("fix", nonce1)
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, repairSteps()[0]); err != nil {
		t.Fatal(err)
	}
	_, err := env.g.writeOutputSecret(env.ctx, &gatewayv1.WriteOutputSecretRequest{Intent: ref(), SubjectHash: h,
		ManagedResource: envIntent + "-0", Values: map[string]string{"k": "${steps.fix.stdout}"}})
	env.wantDenied(err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "Remediate") {
		t.Fatalf("WriteOutputSecret = %v", err)
	}
}

func TestRemediateNeedsANonceAndOtherStagesRefuseOne(t *testing.T) {
	env, _ := executedEnv(t)
	for _, nonce := range []string{"", "not-hex", strings.ToUpper(nonce1), nonce1 + "00"} {
		h := env.remediation("fix-"+nonce, nonce)
		before := len(env.entries())
		_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
		env.refusedWith(err, codes.InvalidArgument, "nonce", before)
	}

	withNonce := plan.Body{Steps: repairSteps(), Resources: executedResources(), Nonce: nonce1}
	for _, stage := range []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate} {
		h := env.recordPlan(stage, withNonce)
		env.approve(stageApproval("nonce-"+strings.ToLower(string(stage)), stage, h))
		before := len(env.entries())
		_, err := env.beginStage(stage, h)
		env.refusedWith(err, codes.InvalidArgument, "nonce", before)
	}
	env.startDelete(teardownCommands())
	h := env.recordPlan(lastv1alpha1.StageDelete, plan.Body{Steps: teardownCommands(), Nonce: nonce1})
	env.approve(deleteApproval("nonce-delete", h))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.refusedWith(err, codes.InvalidArgument, "nonce", before)
}

// The same drift twice gets two remediations with identical steps: each has its
// own subject, and the first approval never authorizes the second (Review Focus 5).
func TestTheSameRemediationTwiceNeedsTwoApprovals(t *testing.T) {
	env, _ := executedEnv(t)
	first := env.remediation("fix-1", nonce1)
	env.runPhase(lastv1alpha1.StageRemediate, first, repairSteps())

	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageRemediate, first)
	env.refusedWith(err, codes.FailedPrecondition, "runs only once", before)

	second := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps(), Resources: executedResources(), Nonce: nonce2})
	if second == first {
		t.Fatal("a new nonce kept the subject")
	}
	before = len(env.entries())
	_, err = env.beginStage(lastv1alpha1.StageRemediate, second)
	env.refusedWith(err, codes.FailedPrecondition, msgNoApproval, before)

	env.approve(stageApproval("fix-2", lastv1alpha1.StageRemediate, second))
	env.runPhase(lastv1alpha1.StageRemediate, second, repairSteps())
}

func TestUpdateAndRemediateRespectTheRecordLimit(t *testing.T) {
	env, _ := executedEnv(t)
	big := bigFilePlan(t, `"\`, MaxPlanBytes/4-200).Status.Plan.Steps // every byte is escaped twice in the record

	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: big})
	env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.InvalidArgument, "plan too large to record", before)

	// A Remediate is never recorded as the executed plan, so the same steps open.
	kept := env.executed()
	h = env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: big, Resources: executedResources(), Nonce: nonce1})
	env.approve(stageApproval("fix", lastv1alpha1.StageRemediate, h))
	env.runPhase(lastv1alpha1.StageRemediate, h, big)
	if got := env.executed(); got != kept {
		t.Fatalf("executed = %+v, want %+v", got, kept)
	}
}

func TestAnIntentBeingDeletedOpensNoUpdateOrRemediate(t *testing.T) {
	env, _ := executedEnv(t)
	env.startDelete(teardownCommands())
	h := env.recordPlan(lastv1alpha1.StageUpdate, plan.Body{Steps: repairSteps(), Resources: executedResources()})
	env.approve(stageApproval("update", lastv1alpha1.StageUpdate, h))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageUpdate, h)
	env.refusedWith(err, codes.FailedPrecondition, "intent is being deleted", before)

	h = env.remediation("fix", nonce1)
	before = len(env.entries())
	_, err = env.beginStage(lastv1alpha1.StageRemediate, h)
	env.refusedWith(err, codes.FailedPrecondition, "intent is being deleted", before)
}
