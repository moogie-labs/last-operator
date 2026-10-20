package gateway

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/policy"
)

// allKeys sets every stage key.
var allKeys = &lastv1alpha1.StageFlags{Discovery: true, Create: true, Reconcile: true, Delete: true}

// setDefaultModes makes the OperatorConfig's defaults allow Auto for every
// stage key, and set every key to mode ("" sets none).
func (e *testEnv) setDefaultModes(mode lastv1alpha1.Mode) {
	e.t.Helper()
	var cfg lastv1alpha1.OperatorConfig
	if err := e.crd.Get(e.ctx, client.ObjectKey{Name: testConfigName}, &cfg); err != nil {
		e.t.Fatal(err)
	}
	cfg.Spec.Defaults.AllowAutoModes = allKeys
	cfg.Spec.Defaults.Modes = nil
	if mode != "" {
		cfg.Spec.Defaults.Modes = &lastv1alpha1.StageModes{Discovery: mode, Create: mode, Reconcile: mode, Delete: mode}
	}
	if err := e.crd.Update(e.ctx, &cfg); err != nil {
		e.t.Fatal(err)
	}
}

// addPolicy creates an IntentPolicy in the Intent's namespace.
func (e *testEnv) addPolicy(name string, spec lastv1alpha1.IntentPolicySpec) {
	e.t.Helper()
	if err := e.crd.Create(e.ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envNS}, Spec: spec}); err != nil {
		e.t.Fatal(err)
	}
}

// phaseBeginAfter returns the only phase.begin written from entry index from on,
// and fails when an approval.observed was written there too and observed is false.
func (e *testEnv) phaseBeginAfter(from int, observed bool) audit.Entry {
	e.t.Helper()
	var begins []audit.Entry
	sawObserved := false
	for _, en := range e.entries()[from:] {
		switch en.Type {
		case audit.TypePhaseBegin:
			begins = append(begins, en)
		case audit.TypeApprovalObserved:
			sawObserved = true
		}
	}
	if len(begins) != 1 {
		e.t.Fatalf("phase.begin entries = %d, want 1", len(begins))
	}
	if sawObserved != observed {
		e.t.Fatalf("approval.observed written = %v, want %v", sawObserved, observed)
	}
	return begins[0]
}

// wantAuth asserts the phase.begin's authorization, and that the record's open
// phase and the in-memory phase carry the same one.
func (e *testEnv) wantAuth(begin audit.Entry, kind, name string) {
	e.t.Helper()
	if a := begin.AuthorizedBy; a == nil || a.Kind != kind || a.Name != name {
		e.t.Fatalf("phase.begin authorizedBy = %+v, want {%s %s}", a, kind, name)
	}
	rec, err := e.g.state.Load(e.ctx, envKey, envIntentUID)
	if err != nil || rec.Open == nil || rec.Open.Auth == nil || *rec.Open.Auth != *begin.AuthorizedBy {
		e.t.Fatalf("record open phase = %+v, %v", rec.Open, err)
	}
	if p := e.g.phases.get(envKey); p == nil || p.auth == nil || *p.auth != *begin.AuthorizedBy {
		e.t.Fatalf("in-memory phase = %+v", p)
	}
}

// wantNeedsApproval asserts the Auto refusal of a plan outside D9's limits.
func (e *testEnv) wantNeedsApproval(err error, before int) {
	e.t.Helper()
	e.refusedWith(err, codes.PermissionDenied, gatewayv1.MsgNeedsApproval, before)
	if !gatewayv1.NeedsApproval(err) {
		e.t.Fatalf("NeedsApproval(%v) = false", err)
	}
}

func TestAutoCreateRunsWithoutApproval(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newProviderEnv(t, in)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	before := len(env.entries())
	if resp, err := env.begin(h); err != nil || resp.GetSteps() != 2 {
		t.Fatalf("BeginPhase = %v, %v", resp, err)
	}
	env.wantAuth(env.phaseBeginAfter(before, false), "auto", string(policy.KeyCreate))
}

func TestAutoPhaseRunsEveryStepWithoutApproval(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newProviderEnv(t, in)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	executeAll(t, env, in)
	for _, en := range env.entries() {
		if en.Type == audit.TypeCommandStart && (en.AuthorizedBy == nil || en.AuthorizedBy.Kind != "auto") {
			t.Fatalf("command.start authorizedBy = %+v, want auto", en.AuthorizedBy)
		}
	}
	if got := env.executed(); got.PlanHash != h {
		t.Fatalf("executed = %+v, want %s", got, h)
	}
}

func TestAutoDiscoveryRunsWithoutApproval(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, nonce1)
	h := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	before := len(env.entries())
	if _, err := env.beginStage(lastv1alpha1.StageDiscovery, h); err != nil {
		t.Fatalf("BeginPhase(Discovery) = %v", err)
	}
	env.wantAuth(env.phaseBeginAfter(before, false), "auto", string(policy.KeyDiscovery))
	if r, err := env.run(h, c); err != nil || r.GetResult().GetExitCode() != 0 {
		t.Fatalf("RunCommand(discovery) = %v, %v", r, err)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase = %v", err)
	}
}

func TestAutoRemediateWithinTheExecutedStepsRuns(t *testing.T) {
	env, _ := executedEnv(t)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	// Each step equals the executed plan's one step but for its id; a step may repeat.
	steps := append(repairSteps(), argvStep("again", kubectlBin, "apply", "-f", "/work/web.yaml"))
	h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: steps, Resources: executedResources(), Nonce: nonce1})
	before := len(env.entries())
	if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
		t.Fatalf("BeginPhase(Remediate) = %v", err)
	}
	env.wantAuth(env.phaseBeginAfter(before, false), "auto", string(policy.KeyReconcile))
	for _, s := range steps {
		if _, err := env.run(h, s); err != nil {
			t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase = %v", err)
	}
}

func TestAutoRemediateWithANewStepNeedsApproval(t *testing.T) {
	env, _ := executedEnv(t)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	for i, steps := range [][]lastv1alpha1.Command{
		append(repairSteps(), argvStep("restart", kubectlBin, "rollout", "restart", "deployment/web", "-n", envNS)),
		{argvStep("fix", kubectlBin, "apply", "-f", "/work/other.yaml")}, // a changed argument
	} {
		nonce := []string{nonce1, nonce2}[i]
		h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: steps, Resources: executedResources(), Nonce: nonce})
		before := len(env.entries())
		_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
		env.wantNeedsApproval(err, before)

		// Its approval then authorizes it.
		env.approve(stageApproval("fix-"+nonce, lastv1alpha1.StageRemediate, h))
		before = len(env.entries())
		if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
			t.Fatalf("BeginPhase(approved Remediate) = %v", err)
		}
		env.wantAuth(env.phaseBeginAfter(before, true), "approval", envNS+"/fix-"+nonce)
		if _, err := env.end(h, false); err != nil {
			t.Fatalf("EndPhase = %v", err)
		}
	}
}

func TestAutoDeletePermutationRuns(t *testing.T) {
	env, _ := executedEnv(t)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	steps := teardownCommands()
	reordered := []lastv1alpha1.Command{steps[1], steps[0]}
	reordered[0].ID, reordered[1].ID = "down-1", "down-2" // ids do not matter
	h := env.startDelete(reordered)
	before := len(env.entries())
	env.runDelete(h, reordered)
	env.wantAuth(env.phaseBeginAfter(before, false), "auto", string(policy.KeyDelete))
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase(Delete, succeeded) = %v", err)
	}
	if env.recordExists() {
		t.Fatal("the record survived a succeeded Auto Delete")
	}
}

func TestAutoDeleteOutsideTheTeardownNeedsApproval(t *testing.T) {
	td := teardownCommands()
	extra := argvStep("t3", kubectlBin, "label", "namespace", envNS, "torn=down")
	changed := td[1]
	changed.Run.Argv = []string{kubectlBin, "delete", "service", "frontend", "-n", envNS, "--ignore-not-found"}
	again := td[0]
	again.ID = "t1-again"
	for name, steps := range map[string][]lastv1alpha1.Command{
		"an added step":     {td[0], td[1], extra},
		"a missing step":    {td[0]},
		"a duplicated step": {td[0], again},
		"a changed step":    {td[0], changed},
		"a step run twice":  {td[0], td[1], again},
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := executedEnv(t)
			env.setDefaultModes(lastv1alpha1.ModeAuto)
			h := env.startDelete(steps)
			before := len(env.entries())
			_, err := env.beginStage(lastv1alpha1.StageDelete, h)
			env.wantNeedsApproval(err, before)

			env.approve(deleteApproval("teardown", h))
			before = len(env.entries())
			if _, err := env.beginStage(lastv1alpha1.StageDelete, h); err != nil {
				t.Fatalf("BeginPhase(approved Delete) = %v", err)
			}
			env.wantAuth(env.phaseBeginAfter(before, true), "approval", envNS+"/teardown")
		})
	}
}

func TestAutoDeleteWithoutAnExecutedPlanNeedsApproval(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newProviderEnv(t, in)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	// The operator fell back to the ManagedResources' teardown.
	h := env.startDelete(teardownCommands())
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantNeedsApproval(err, before)

	env.approve(deleteApproval("teardown", h))
	before = len(env.entries())
	if _, err := env.beginStage(lastv1alpha1.StageDelete, h); err != nil {
		t.Fatalf("BeginPhase(approved Delete) = %v", err)
	}
	env.wantAuth(env.phaseBeginAfter(before, true), "approval", envNS+"/teardown")
}

// corruptExecuted breaks the record's executed plan so it fails its integrity
// check, and returns the record.
func (e *testEnv) corruptExecuted() *LoadedRecord {
	e.t.Helper()
	rec, err := e.g.state.Load(e.ctx, envKey, envIntentUID)
	if err != nil {
		e.t.Fatal(err)
	}
	rec.Executed.Plan += " "
	if err := e.g.saveRecord(e.ctx, rec); err != nil {
		e.t.Fatal(err)
	}
	return rec
}

func TestAutoDeleteRefusesAnExecutedPlanThatFailsItsIntegrityCheck(t *testing.T) {
	env, _ := executedEnv(t)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	h := env.startDelete(teardownCommands())
	env.corruptExecuted()
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantDenied(err, codes.Internal)
}

func TestAutoLimitsRefuseARemediateAgainstAnExecutedPlanThatFailsItsIntegrityCheck(t *testing.T) {
	// BeginPhase's Remediate rules decode the executed plan before the limits do,
	// so the limits are checked directly.
	env, _ := executedEnv(t)
	rec := env.corruptExecuted()
	d := denial{ctx: env.ctx, w: env.g.writer.Load(), actor: testOperator, intent: envKey,
		stage: string(lastv1alpha1.StageRemediate), rpc: "BeginPhase"}
	_, err := withinAutoLimits(d, rec, lastv1alpha1.StageRemediate, plan.Body{Steps: repairSteps()})
	env.wantDenied(err, codes.Internal)
}

func TestADenyStopsAnAutoPhase(t *testing.T) {
	deny := func(env *testEnv, h string) {
		env.t.Helper()
		env.approve(approvalFor("no", h, lastv1alpha1.DecisionDeny, "bob"))
	}
	t.Run("at BeginPhase", func(t *testing.T) {
		in := intentWithPlan(t, twoSteps())
		h := in.Status.Plan.Hash
		env := newProviderEnv(t, in, approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
		env.setDefaultModes(lastv1alpha1.ModeAuto)
		deny(env, h)
		before := len(env.entries())
		_, err := env.begin(h)
		env.refusedWith(err, codes.PermissionDenied, "approval no denies this plan", before)
	})
	t.Run("at RunCommand", func(t *testing.T) {
		in := intentWithPlan(t, twoSteps())
		h := in.Status.Plan.Hash
		env := newProviderEnv(t, in)
		env.setDefaultModes(lastv1alpha1.ModeAuto)
		if _, err := env.begin(h); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(h, twoSteps()[0]); err != nil {
			t.Fatal(err)
		}
		deny(env, h)
		before := len(env.entries())
		_, err := env.run(h, argvStep("s2", "echo", "Namespace"))
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), "no denies this plan") {
			t.Fatalf("RunCommand = %v, want the Deny named", err)
		}
		wantNoCommandStart(t, env, before)
		_, err = env.end(h, true)
		env.wantDenied(err, codes.FailedPrecondition)
	})
}

func wantNoCommandStart(t *testing.T, env *testEnv, from int) {
	t.Helper()
	for _, en := range env.entries()[from:] {
		if en.Type == audit.TypeCommandStart {
			t.Fatal("a refused step started")
		}
	}
}

func TestTurningAutoOffMidPhaseNeedsAnApproval(t *testing.T) {
	s2 := argvStep("s2", "echo", "Namespace")
	for _, tc := range []struct {
		name     string
		approval string // who approves the plan once Auto is off; "" for no one
		refused  string
	}{
		{"without an approval", "", "no IntentApproval approves this plan"},
		{"with an approval", approver, ""},
		{"with the operator's approval", testOperator, "names the operator"},
		{"with a gateway ServiceAccount's approval", "system:serviceaccount:" + ns + ":anything", "gateway-namespace ServiceAccount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := intentWithPlan(t, twoSteps())
			h := in.Status.Plan.Hash
			env := newProviderEnv(t, in)
			env.setDefaultModes(lastv1alpha1.ModeAuto)
			if _, err := env.begin(h); err != nil {
				t.Fatal(err)
			}
			if _, err := env.run(h, twoSteps()[0]); err != nil {
				t.Fatal(err)
			}
			// A namespace admin turns Create to Manual between the steps.
			env.addPolicy("manual", lastv1alpha1.IntentPolicySpec{Modes: &lastv1alpha1.StageModes{Create: lastv1alpha1.ModeManual}})
			if tc.approval != "" {
				env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, tc.approval))
			}
			before := len(env.entries())
			_, err := env.run(h, s2)
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("RunCommand(s2) = %v", err)
				}
				if _, err := env.end(h, true); err != nil {
					t.Fatalf("EndPhase = %v", err)
				}
				return
			}
			env.wantDenied(err, codes.PermissionDenied)
			if !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("RunCommand = %v, want %q", err, tc.refused)
			}
			wantNoCommandStart(t, env, before)
			_, err = env.end(h, true)
			env.wantDenied(err, codes.FailedPrecondition)
		})
	}
}

func TestManualIsUnchanged(t *testing.T) {
	for name, setup := range map[string]func(*testEnv){
		"no modes set":             func(*testEnv) {},
		"Auto allowed, Manual set": func(e *testEnv) { e.setDefaultModes(lastv1alpha1.ModeManual) },
		"Auto allowed, none set":   func(e *testEnv) { e.setDefaultModes("") },
	} {
		t.Run(name, func(t *testing.T) {
			in := intentWithPlan(t, twoSteps())
			h := in.Status.Plan.Hash
			env := newProviderEnv(t, in)
			setup(env)
			before := len(env.entries())
			_, err := env.begin(h)
			env.refusedWith(err, codes.FailedPrecondition, msgNoApproval, before)
			if gatewayv1.NeedsApproval(err) {
				t.Fatal("a Manual refusal reads as needing approval in Auto mode")
			}

			env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
			before = len(env.entries())
			if _, err := env.begin(h); err != nil {
				t.Fatalf("BeginPhase(approved) = %v", err)
			}
			env.wantAuth(env.phaseBeginAfter(before, true), "approval", envNS+"/ok")
		})
	}
}

func TestAnIntentAskingForAutoGetsOnlyWhatThePolicyAllows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testEnv)
		auto  bool
	}{
		{"no layer allows Auto", func(*testEnv) {}, false},
		{"an IntentPolicy does not allow Auto", func(e *testEnv) {
			e.setDefaultModes("")
			e.addPolicy("strict", lastv1alpha1.IntentPolicySpec{})
		}, false},
		{"an IntentPolicy sets Manual", func(e *testEnv) {
			e.setDefaultModes("")
			e.addPolicy("manual", lastv1alpha1.IntentPolicySpec{AllowAutoModes: allKeys,
				Modes: &lastv1alpha1.StageModes{Create: lastv1alpha1.ModeManual}})
		}, false},
		{"every layer allows Auto", func(e *testEnv) {
			e.setDefaultModes("")
			e.addPolicy("loose", lastv1alpha1.IntentPolicySpec{AllowAutoModes: allKeys})
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := intentWithPlan(t, twoSteps())
			in.Spec.Modes = &lastv1alpha1.StageModes{Create: lastv1alpha1.ModeAuto}
			h := in.Status.Plan.Hash
			env := newProviderEnv(t, in)
			tc.setup(env)
			before := len(env.entries())
			_, err := env.begin(h)
			if !tc.auto {
				env.refusedWith(err, codes.FailedPrecondition, msgNoApproval, before)
				return
			}
			if err != nil {
				t.Fatalf("BeginPhase = %v", err)
			}
			env.wantAuth(env.phaseBeginAfter(before, false), "auto", string(policy.KeyCreate))
		})
	}
}

func TestAnAutoPlanWithinTheLimitsNeedsNoApprovalEvenWithOne(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	// Auto authorizes the plan; the Approve does not matter, even the operator's.
	env := newProviderEnv(t, in, approvalFor("ok", h, lastv1alpha1.DecisionApprove, testOperator))
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	before := len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	env.wantAuth(env.phaseBeginAfter(before, false), "auto", string(policy.KeyCreate))
}

func TestApproverRefusalsStillApplyToApprovalAuthorizedPhases(t *testing.T) {
	for _, by := range []string{testOperator, "system:serviceaccount:" + ns + ":anything"} {
		t.Run(by, func(t *testing.T) {
			env, _ := executedEnv(t)
			env.setDefaultModes(lastv1alpha1.ModeAuto)
			steps := append(repairSteps(), argvStep("restart", kubectlBin, "rollout", "restart", "deployment/web", "-n", envNS))
			h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: steps, Resources: executedResources(), Nonce: nonce1})
			a := stageApproval("fix", lastv1alpha1.StageRemediate, h)
			a.Spec.ApprovedBy = by
			env.approve(a)
			before := len(env.entries())
			_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
			env.refusedWith(err, codes.PermissionDenied, "approval fix names", before)
		})
	}
}

func TestNeedsApprovalMatchesOnlyItsRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{status.Error(codes.PermissionDenied, gatewayv1.MsgNeedsApproval), true},
		{status.Error(codes.FailedPrecondition, gatewayv1.MsgNeedsApproval), false},
		{status.Error(codes.PermissionDenied, gatewayv1.MsgNeedsApproval+"!"), false},
	} {
		if got := gatewayv1.NeedsApproval(tc.err); got != tc.want {
			t.Errorf("NeedsApproval(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestAnAutoPhaseStopsWhenItsIntentIsRecreated(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newProviderEnv(t, in)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(h, twoSteps()[0]); err != nil {
		t.Fatal(err)
	}
	// Another Intent of the same name and plan, also Auto, replaces this one.
	if err := env.crd.Delete(env.ctx, in); err != nil {
		t.Fatal(err)
	}
	recreated := intentWithPlan(t, twoSteps())
	recreated.UID = recreatedUID
	if err := env.crd.Create(env.ctx, recreated); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	_, err := env.run(h, argvStep("s2", "echo", "Namespace"))
	env.wantDenied(err, codes.PermissionDenied)
	wantNoCommandStart(t, env, before)
}

func TestAnApprovalPhaseRefusesAStepWhoseOnlyApproverIsTheOperator(t *testing.T) {
	for _, by := range []string{testOperator, "system:serviceaccount:" + ns + ":anything"} {
		t.Run(by, func(t *testing.T) {
			in := intentWithPlan(t, twoSteps())
			h := in.Status.Plan.Hash
			original := approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver)
			env := newProviderEnv(t, in, original)
			if _, err := env.begin(h); err != nil {
				t.Fatal(err)
			}
			if _, err := env.run(h, twoSteps()[0]); err != nil {
				t.Fatal(err)
			}
			// The human's Approve goes; only one the gateway never accepts is left.
			if err := env.crd.Delete(env.ctx, original); err != nil {
				t.Fatal(err)
			}
			env.approve(approvalFor("self", h, lastv1alpha1.DecisionApprove, by))
			before := len(env.entries())
			_, err := env.run(h, argvStep("s2", "echo", "Namespace"))
			env.wantDenied(err, codes.PermissionDenied)
			if !strings.Contains(err.Error(), "approval self names") {
				t.Fatalf("RunCommand = %v, want the approver refused", err)
			}
			wantNoCommandStart(t, env, before)
			if p := env.g.phases.get(envKey); p == nil || !p.failed {
				t.Fatal("the phase is not failed")
			}
			_, err = env.end(h, true)
			env.wantDenied(err, codes.FailedPrecondition)
		})
	}
}
