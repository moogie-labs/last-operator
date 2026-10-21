package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// How the fake gateway authorized a phase (fakeGateway.authsOf).
const (
	authApproval      = "approval"
	authAutoCreate    = "auto:create"
	authAutoDiscovery = "auto:discovery"
	authAutoDelete    = "auto:delete"
)

// allStages sets every stage key.
var allStages = &lastv1alpha1.StageFlags{Discovery: true, Create: true, Reconcile: true, Delete: true}

// allowAuto lets every stage key be Auto at the defaults layer, with no mode
// set there: only an Intent that asks for Auto gets it, so the Intents of other
// specs stay Manual. The OperatorConfig goes when the spec ends.
func allowAuto() {
	op := &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: operatorConfigName},
		Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{AllowAutoModes: allStages}}}
	Expect(k8sClient.Create(ctx, op)).To(Succeed())
	DeferCleanup(func() { Expect(k8sClient.Delete(ctx, op)).To(Succeed()) })
}

// createIntentWithModes creates an Intent that asks to "run web", with spec.modes set to modes.
func createIntentWithModes(key types.NamespacedName, modes lastv1alpha1.StageModes) {
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentSpec{Request: "run web", Modes: &modes}}
	Expect(k8sClient.Create(ctx, in)).To(Succeed())
}

// approvalsIn returns the IntentApprovals in the namespace.
func approvalsIn(ns string) []lastv1alpha1.IntentApproval {
	var list lastv1alpha1.IntentApprovalList
	Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
	return list.Items
}

// effectiveModes returns the Intent's status.effectiveModes.
func effectiveModes(key types.NamespacedName) func() *lastv1alpha1.StageModes {
	return func() *lastv1alpha1.StageModes { return getIntent(key).Status.EffectiveModes }
}

// readyMessage returns the message of the Intent's Ready condition, or "" before it has one.
func readyMessage(key types.NamespacedName) func() string {
	return func() string {
		if c := meta.FindStatusCondition(getIntent(key).Status.Conditions, CondReady); c != nil {
			return c.Message
		}
		return ""
	}
}

// msgAutoRun is the transition note of a plan that runs without approval.
func msgAutoRun(subject string) string { return "Running plan " + subject + " without approval (Auto)" }

const (
	modeAuto   = lastv1alpha1.ModeAuto
	modeManual = lastv1alpha1.ModeManual
)

var _ = Describe("Auto modes", func() {
	var key types.NamespacedName
	s1 := argvStep("s1", argKubectl, "apply", "-f", "web.yaml")
	s2 := argvStep("s2", argKubectl, "rollout", "status", "deployment/web")
	steps := []lastv1alpha1.Command{s1}
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, namespacesArg}}}
	lookAgain := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, "pods"}}}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
		allowAuto()
	})

	It("runs an Auto Create without an approval", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authAutoCreate}))
		Expect(gw.calls(key)[:4]).To(Equal([]string{callLLM, beginPhase, runS1, endSucceeded}))
		Expect(gw.executedOf(key)).To(Equal(hashOf(steps)))
		Expect(approvalsIn(key.Namespace)).To(BeEmpty())
		Expect(effectiveModes(key)()).To(Equal(&lastv1alpha1.StageModes{Discovery: modeManual, Create: modeAuto,
			Reconcile: modeManual, Delete: modeManual}))
		Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseExecuting)) }).Should(
			Equal([]string{msgAutoRun(hashOf(steps))}))
		Expect(eventReasons(key, corev1.EventTypeNormal)).NotTo(ContainElement(string(lastv1alpha1.PhaseAwaitingApproval)))
	})

	It("runs Auto discovery commands without an approval", func() {
		gw.setScript(key, &script{llm: turns(commandTurn(look), commandTurn(lookAgain), answer(steps))})
		createIntentWithModes(key, lastv1alpha1.StageModes{Discovery: modeAuto})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval), "the Create stage is Manual")
		Expect(gw.authsOf(key)).To(Equal([]string{authAutoDiscovery, authAutoDiscovery}))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, runDiscoveryStep, endSucceeded, queryAudit, callLLM,
			beginDiscovery, runDiscoveryStep, endSucceeded, queryAudit, callLLM}))
		Expect(approvalsIn(key.Namespace)).To(BeEmpty())
		Expect(effectiveModes(key)()).To(HaveField("Discovery", modeAuto))
		Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseAwaitingDiscoveryApproval)) }).Should(
			HaveLen(2))
		for _, note := range eventNotes(key, string(lastv1alpha1.PhaseAwaitingDiscoveryApproval)) {
			Expect(note).To(MatchRegexp(`^Running discovery command sha256:[0-9a-f]{64} without approval \(Auto\)$`))
		}
	})

	It("waits for an approval when the gateway needs one for an Auto discovery command", func() {
		gw.setScript(key, &script{llm: turns(commandTurn(look), answer(steps)),
			beginErr: status.Error(codes.PermissionDenied, gatewayv1.MsgNeedsApproval)})
		createIntentWithModes(key, lastv1alpha1.StageModes{Discovery: modeAuto})

		Eventually(readyMessage(key)).Should(Equal("The gateway needs an approval for this plan: " + gatewayv1.MsgNeedsApproval))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingDiscoveryApproval))
		Consistently(func() int { return countOf(gw.calls(key), beginDiscovery) }).Should(Equal(1),
			"it waits for the approval, though its own modes say Auto")

		gw.updateScript(key, func(s *script) { s.beginErr = nil })
		approveCommand(key, getIntent(key).Status.PendingCommand.Hash, lastv1alpha1.DecisionApprove, "")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(ContainElement(runDiscoveryStep))
	})

	It("plans and runs an Auto Update on an edit", func() {
		updateSteps := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web-v2.yaml")}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if revised(req) {
				return answer(updateSteps), nil
			}
			return answer(steps), nil
		}})
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		editRequest(key, "run web v2")
		Eventually(func() string { return gw.executedOf(key) }).Should(Equal(hashOf(updateSteps)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
		Expect(gw.authsOf(key)).To(Equal([]string{authAutoCreate, authAutoCreate}))
		Expect(approvalsIn(key.Namespace)).To(BeEmpty())
		Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseExecuting)) }).Should(
			Equal([]string{msgAutoRun(hashOf(steps)), msgAutoRun(hashOf(updateSteps))}))
	})

	Describe("Remediate", func() {
		web := lastv1alpha1.ManagedResourceSpec{Description: "nginx Deployment",
			Probes: []lastv1alpha1.Probe{{ID: "p1", Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{
				Argv: []string{argKubectl, argGet, deployWeb, "-o", formatJSON}}}, Assert: replicasAssert}}}
		applied := argvStep("s1", argKubectl, "apply", "-f", "web.yaml")
		applied.Reason = "create the Deployment"

		// readyThenDrift runs an Auto Create of the applied step to Ready, then
		// makes p1 fail until a Remediate phase succeeds, and marks web-0 out of
		// sync, as a scheduled probe run that found the drift does. The model
		// answers the Remediate session with fix.
		readyThenDrift := func(fix []lastv1alpha1.Command) {
			s := &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
				if remediating(req) {
					return answer(fix), nil
				}
				return answer([]lastv1alpha1.Command{applied}, web), nil
			}}
			s.onEnd = func(succeeded bool) {
				if succeeded {
					s.failing = nil // the repair worked
				}
			}
			gw.setScript(key, s)
			createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto, Reconcile: modeAuto})
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
			gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
			setOutOfSync(key.Namespace, "web-0")
		}

		It("remediates drift in Auto when the repair re-runs executed steps", func() {
			rerun := applied
			rerun.ID = "r1" // the same command, as the gateway compares it
			readyThenDrift([]lastv1alpha1.Command{rerun})

			Eventually(func() int { return remediateBegins(key) }).Should(Equal(1))
			Eventually(func() bool {
				in := getIntent(key)
				return in.Status.Phase == lastv1alpha1.PhaseReady && in.Status.Stage == lastv1alpha1.StageRemediate
			}).Should(BeTrue())
			Expect(gw.authsOf(key)).To(Equal([]string{authAutoCreate, "auto:reconcile"}))
			Expect(gw.calls(key)).To(ContainElement("RunCommand:r1"))
			Expect(approvalsIn(key.Namespace)).To(BeEmpty())
			Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseRemediating)) }).Should(
				Equal([]string{msgAutoRun(getIntent(key).Status.Plan.Hash)}))
		})

		It("waits for an approval when an Auto Remediate adds a step", func() {
			scale := argvStep("r1", argKubectl, "scale", "deployment/web", "--replicas=2")
			readyThenDrift([]lastv1alpha1.Command{scale})

			Eventually(readyMessage(key)).Should(Equal("The gateway needs an approval for this plan: " + gatewayv1.MsgNeedsApproval))
			in := getIntent(key)
			Expect(in.Status.Phase).To(Equal(lastv1alpha1.PhaseAwaitingApproval))
			Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageRemediate))
			Expect(in.Status.EffectiveModes).To(HaveField("Reconcile", modeAuto))
			Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))
			Consistently(func() int { return countOf(gw.calls(key), beginPhase) }).Should(Equal(2),
				"it waits for the approval, though its own modes say Auto")
			Expect(gw.calls(key)).NotTo(ContainElement("RunCommand:r1"))
			Expect(remediateBegins(key)).To(BeZero())

			approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
			Eventually(func() int { return remediateBegins(key) }).Should(Equal(1))
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
			Expect(gw.authsOf(key)).To(Equal([]string{authAutoCreate, authApproval}))
		})
	})

	It("tears down in Auto when the Delete is a permutation", func() {
		t1 := argvStep("t1", argKubectl, "delete", "deployment", "web")
		t2 := argvStep("t2", argKubectl, "delete", "service", "web")
		resources := []lastv1alpha1.ManagedResourceSpec{
			{Description: "the web Deployment", Teardown: []lastv1alpha1.Command{t1}},
			{Description: "the web Service", Teardown: []lastv1alpha1.Command{t2}},
		}
		teardown := []lastv1alpha1.Command{t2, t1}
		gw.setScript(key, &script{llm: lifecycle(turns(answer(steps, resources...)), turns(answer(teardown)))})
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto, Delete: modeAuto})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		deleteIntent(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(gw.authsOf(key)).To(Equal([]string{authAutoCreate, authAutoDelete}))
		calls := gw.calls(key)
		Expect(strings.Join(calls, ",")).To(HaveSuffix(strings.Join([]string{beginPhase, runT2, runT1, endSucceeded}, ",")))
		Expect(approvalsIn(key.Namespace)).To(BeEmpty())
	})

	It("shows Manual in status.effectiveModes when a layer does not allow Auto", func() {
		Expect(k8sClient.Create(ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: key.Namespace},
			Spec: lastv1alpha1.IntentPolicySpec{AllowAutoModes: &lastv1alpha1.StageFlags{Discovery: true}}})).To(Succeed())
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntentWithModes(key, lastv1alpha1.StageModes{Discovery: modeAuto, Create: modeAuto, Reconcile: modeAuto, Delete: modeAuto})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(effectiveModes(key)()).To(Equal(&lastv1alpha1.StageModes{Discovery: modeAuto, Create: modeManual,
			Reconcile: modeManual, Delete: modeManual}))
		Consistently(func() []string { return gw.calls(key) }).ShouldNot(ContainElement(beginPhase))

		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authApproval}))
	})

	It("a Manual stage still waits for an approval", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntentWithModes(key, lastv1alpha1.StageModes{Discovery: modeAuto, Create: modeManual})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(readyCondition(key).Message).To(Equal("Waiting for an IntentApproval of plan " + hashOf(steps) + " for stage Create"))
		Expect(effectiveModes(key)()).To(Equal(&lastv1alpha1.StageModes{Discovery: modeAuto, Create: modeManual,
			Reconcile: modeManual, Delete: modeManual}))
		Consistently(func() []string { return gw.calls(key) }).ShouldNot(ContainElement(beginPhase))

		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.authsOf(key)).To(Equal([]string{authApproval}))
	})

	It("fails the step when Auto is withdrawn mid-phase", func() {
		withdraw := func(step string) { // called in the controller's goroutine
			defer GinkgoRecover()
			if step != "s2" {
				return
			}
			Expect(k8sClient.Create(ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: key.Namespace},
				Spec: lastv1alpha1.IntentPolicySpec{Modes: &lastv1alpha1.StageModes{Create: modeManual}}})).To(Succeed())
		}
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{s1, s2})), onRun: withdraw})
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonExecutionFailed))
		Expect(readyCondition(key).Message).To(Equal("step s2: auto authorization withdrawn: the Create stage is no longer " +
			"Auto for this Intent, and no IntentApproval approves this plan"))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginPhase, runS1, runS2, endFailed}))
	})
})
