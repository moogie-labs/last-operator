package controller

import (
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

// authRule is how the fake gateway records a phase allow rules authorized.
const authRule = "rule"

// ruleSpec is an allow rule for stages that matches argv, for any command
// whose providers are a subset of {kubernetes}.
func ruleSpec(stages []lastv1alpha1.Stage, argv ...string) lastv1alpha1.CommandRuleSpec {
	return lastv1alpha1.CommandRuleSpec{Stages: stages, Providers: []string{providerK8s},
		Match: lastv1alpha1.ArgvPattern{Argv: argv}}
}

// createRule creates a CommandRule in namespace ns, as a human would.
func createRule(ns, name string, stages []lastv1alpha1.Stage, argv ...string) {
	r := &lastv1alpha1.CommandRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: ruleSpec(stages, argv...)}
	Expect(k8sClient.Create(ctx, r)).To(Succeed())
}

// createClusterRule creates a ClusterCommandRule; it goes when the spec ends.
func createClusterRule(name string, stages []lastv1alpha1.Stage, argv ...string) {
	r := &lastv1alpha1.ClusterCommandRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: ruleSpec(stages, argv...)}
	Expect(k8sClient.Create(ctx, r)).To(Succeed())
	DeferCleanup(func() { Expect(k8sClient.Delete(ctx, r)).To(Succeed()) })
}

// stages is shorthand for a rule's stage list.
func stages(s ...lastv1alpha1.Stage) []lastv1alpha1.Stage { return s }

// uncoveredSteps returns the Intent plan's status.plan.uncoveredSteps.
func uncoveredSteps(key types.NamespacedName) []string {
	p := getIntent(key).Status.Plan
	Expect(p).NotTo(BeNil())
	return p.UncoveredSteps
}

// msgRuleRun is the transition note of a plan that allow rules let run without approval.
func msgRuleRun(subject string) string {
	return "Running plan " + subject + " without approval (allow rules)"
}

// noRules is a fake gateway rule view that sees none of the rules.
func noRules([]rules.Rule) []rules.Rule { return nil }

var _ = Describe("Allow rules", func() {
	var key types.NamespacedName
	create, update, remediate := lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageRemediate
	discovery, del := lastv1alpha1.StageDiscovery, lastv1alpha1.StageDelete
	s1 := argvStep("s1", argKubectl, "apply", "-f", "web.yaml")
	s2 := argvStep("s2", argKubectl, "rollout", "status", "deployment/rules-web")
	steps := []lastv1alpha1.Command{s1, s2}
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, namespacesArg}}}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	It("writes uncoveredSteps for a partly covered plan and waits", func() {
		createRule(key.Namespace, "apply", stages(create), argKubectl, "apply", "-f", "*")
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(uncoveredSteps(key)).To(Equal([]string{"s2"}))
		Expect(readyMessage(key)()).To(Equal("Waiting for an IntentApproval of plan " + hashOf(steps) + " for stage Create"))
		Consistently(func() int { return countOf(gw.calls(key), beginPhase) }).Should(BeZero())

		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authApproval}))
	})

	It("runs a covered plan without an approval", func() {
		createRule(key.Namespace, "apply", stages(create), argKubectl, "apply", "-f", "*")
		createClusterRule("rules-rollout", stages(create), argKubectl, "rollout", "status", "deployment/rules-web")
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authRule}))
		Expect(gw.calls(key)[:5]).To(Equal([]string{callLLM, beginPhase, runS1, runS2, endSucceeded}))
		Expect(uncoveredSteps(key)).To(BeEmpty())
		Expect(approvalsIn(key.Namespace)).To(BeEmpty())
		Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseExecuting)) }).Should(
			Equal([]string{msgRuleRun(hashOf(steps))}))
		Expect(eventNotes(key, string(lastv1alpha1.PhaseAwaitingApproval))).To(BeEmpty())
	})

	It("runs a Create plan an Update rule covers", func() {
		createRule(key.Namespace, "apply", stages(update), argKubectl, "...")
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authRule}), "Create and Update are one class (R15)")
		Expect(uncoveredSteps(key)).To(BeEmpty())
	})

	It("writes the uncovered probe of a plan whose steps are covered, and waits", func() {
		res := lastv1alpha1.ManagedResourceSpec{Description: intentName,
			Probes: []lastv1alpha1.Probe{{ID: "p1", Command: argvStep("", argKubectl, argGet, "deployment", intentName, "-o", formatJSON),
				Assert: replicasAssert}},
			Teardown: []lastv1alpha1.Command{argvStep("", argKubectl, "delete", "deployment", intentName)}}
		createRule(key.Namespace, "apply", stages(create), argKubectl, "apply", "-f", "*")
		createRule(key.Namespace, "rollout", stages(create), argKubectl, "rollout", "...")
		createRule(key.Namespace, "teardown", stages(del), argKubectl, "delete", "deployment", "*")
		gw.setScript(key, &script{llm: turns(answer(steps, res))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(uncoveredSteps(key)).To(Equal([]string{"probe:p1"}))
		Consistently(func() int { return countOf(gw.calls(key), beginPhase) }).Should(BeZero())
	})

	It("runs a covered discovery command without an approval", func() {
		createRule(key.Namespace, "look", stages(discovery), argKubectl, argGet, namespacesArg)
		gw.setScript(key, &script{llm: turns(commandTurn(look), answer(steps))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval), "no rule covers the plan")
		Expect(gw.authsOf(key)).To(Equal([]string{authRule}))
		Expect(gw.calls(key)[:4]).To(Equal([]string{callLLM, beginDiscovery, runDiscoveryStep, endSucceeded}))
		Expect(approvalsIn(key.Namespace)).To(BeEmpty())
		Expect(uncoveredSteps(key)).To(Equal([]string{"s1", "s2"}))
		Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseAwaitingDiscoveryApproval)) }).Should(
			ConsistOf(MatchRegexp(`^Running discovery command sha256:[0-9a-f]{64} without approval \(allow rules\)$`)))
	})

	It("waits for an approval when the gateway does not see the rule", func() {
		createRule(key.Namespace, "all", stages(create), argKubectl, "...")
		gw.setScript(key, &script{llm: turns(answer(steps)), ruleView: noRules})
		createIntent(key, "run web")

		Eventually(readyMessage(key)).Should(Equal("The gateway needs an approval for this plan: " +
			gatewayv1.MsgNoApprovalPrefix + "plan"))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))
		Consistently(func() int { return countOf(gw.calls(key), beginPhase) }).Should(Equal(1),
			"it waits for the approval, though its own rules cover the plan")
		Expect(phase(key)()).NotTo(Equal(lastv1alpha1.PhaseFailed))

		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authApproval}))
	})

	It("waits for an approval of a discovery command when the gateway does not see the rule", func() {
		createRule(key.Namespace, "look", stages(discovery), argKubectl, argGet, namespacesArg)
		gw.setScript(key, &script{llm: turns(commandTurn(look), answer(steps)), ruleView: noRules})
		createIntent(key, "run web")

		Eventually(readyMessage(key)).Should(Equal("The gateway needs an approval for this command: " +
			gatewayv1.MsgNoApprovalPrefix + "discovery command"))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingDiscoveryApproval))
		Consistently(func() int { return countOf(gw.calls(key), beginDiscovery) }).Should(Equal(1))
		Expect(countOf(gw.calls(key), callLLM)).To(Equal(1), "the model is not told")

		approveCommand(key, getIntent(key).Status.PendingCommand.Hash, lastv1alpha1.DecisionApprove, "")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.authsOf(key)).To(Equal([]string{authApproval}))
	})

	It("a Manual stage with no rules still waits", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(uncoveredSteps(key)).To(Equal([]string{"s1", "s2"}))
		Expect(readyMessage(key)()).To(Equal("Waiting for an IntentApproval of plan " + hashOf(steps) + " for stage Create"))
		Consistently(func() int { return countOf(gw.calls(key), beginPhase) }).Should(BeZero())
	})

	It("runs a covered Remediate in a Manual stage without an approval", func() {
		web := lastv1alpha1.ManagedResourceSpec{Description: intentName,
			Probes: []lastv1alpha1.Probe{{ID: "p1", Command: argvStep("", argKubectl, argGet, "deployment", intentName, "-o", formatJSON),
				Assert: replicasAssert}},
			Teardown: []lastv1alpha1.Command{argvStep("", argKubectl, "delete", "deployment", intentName)}}
		createSteps := []lastv1alpha1.Command{s1}
		fixSteps := []lastv1alpha1.Command{argvStep("r1", argKubectl, "scale", deployWeb, "--replicas=2")}
		createRule(key.Namespace, "scale", stages(remediate), argKubectl, "scale", deployWeb, "*")
		s := &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if remediating(req) {
				return answer(fixSteps), nil
			}
			return answer(createSteps, web), nil
		}}
		s.onRun = func(step string) { // with f.mu held: the repair fixes the drift
			if step == "r1" {
				s.failing = nil
			}
		}
		gw.setScript(key, s)
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval), "the rule covers Remediate only")
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		gw.updateScript(key, func(s *script) {
			s.failing = map[string]bool{"p1": true}
			s.probeOut = map[string]string{"p1": `{"replicas":1}`}
		})
		setOutOfSync(key.Namespace, "web-0")
		Eventually(func() []string { return gw.authsOf(key) }).Should(Equal([]string{authApproval, authRule}))
		Eventually(func() bool {
			in := getIntent(key)
			return in.Status.Phase == lastv1alpha1.PhaseReady && in.Status.Plan != nil && in.Status.Plan.Nonce != ""
		}).Should(BeTrue(), "Ready after the Remediate plan")
		p := getIntent(key).Status.Plan
		Expect(p.Steps).To(Equal(fixSteps))
		Expect(p.UncoveredSteps).To(BeEmpty())
		Expect(slices.ContainsFunc(approvalsIn(key.Namespace), func(a lastv1alpha1.IntentApproval) bool {
			return a.Spec.Stage == remediate
		})).To(BeFalse())
		Expect(eventNotes(key, string(lastv1alpha1.PhaseRemediating))).To(Equal([]string{msgRuleRun(p.Hash)}))
	})
})
