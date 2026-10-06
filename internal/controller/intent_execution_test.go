package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

var _ = Describe("Intent execution", func() {
	var key types.NamespacedName
	s1 := argvStep("s1", "kubectl", "get", "namespace", "x", "-o", "json")
	s2 := argvStep("s2", "echo", "${steps.s1.json.metadata.name}")
	resource := lastv1alpha1.ManagedResourceSpec{Description: "the namespace",
		Probes:           []lastv1alpha1.Probe{{ID: "p1", Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{trueCmd}}}, Assert: trueCmd}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
		Outputs:          map[string]string{"name": "${steps.s1.json.metadata.name}"}}
	stdout := map[string]string{"s1": `{"metadata":{"name":"x"}}`, "s2": "x\n"}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// awaitingApproval scripts the gateway and waits until the Intent asks for approval.
	awaitingApproval := func(s *script) string {
		gw.setScript(key, s)
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		return getIntent(key).Status.Plan.Hash
	}

	It("runs the approved plan through the gateway and creates its ManagedResources", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1, s2}, resource)), stdout: stdout})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:6]).To(Equal([]string{callLLM, beginPhase, runS1, "RunCommand:s2",
			"WriteOutputSecret:web-0", endSucceeded}))
		Expect(gw.argvOf(key, "s2")).To(Equal([]string{"echo", "x"}))
		Expect(gw.outputsOf(key)).To(Equal(map[string]map[string]string{"web-0": resource.Outputs}))

		mr := getMR(key.Namespace, "web-0")
		Expect(metav1.IsControlledBy(mr, getIntent(key))).To(BeTrue())
		Expect(mr.Spec.IntentRef).To(Equal(&lastv1alpha1.LocalRef{Name: intentName}))
		Expect(mr.Spec.Probes).To(Equal(resource.Probes))
		Expect(mr.Spec.Outputs).To(Equal(resource.Outputs))
	})

	It("plans again with the failure when a step fails", func() {
		failing := []lastv1alpha1.Command{argvStep("s1", "false")}
		fixed := []lastv1alpha1.Command{argvStep("s2", trueCmd)}
		awaitingApproval(&script{exit: map[string]int32{"s1": 1}, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "boom") {
				return answer(fixed), nil
			}
			return answer(failing), nil
		}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(planHash(key)).Should(Equal(hashOf(fixed)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginPhase, runS1, endFailed, callLLM}))
		Expect(requestText(gw.lastRequest(key))).To(ContainSubstring("step s1 exited 1: boom"))
		Expect(meta.FindStatusCondition(getIntent(key).Status.Conditions, CondExecuted)).To(BeNil())
	})

	It("ends the phase and fails when the gateway refuses a step", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1, s2})), stdout: stdout,
			runErr: map[string]error{"s2": status.Error(codes.PermissionDenied, "approval withdrawn")}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonExecutionFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("approval withdrawn"))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginPhase, runS1, "RunCommand:s2", endFailed}))
	})

	It("retries EndPhase when the gateway is briefly unavailable", func() {
		unavailable := status.Error(codes.Unavailable, "audit store unreachable")
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1})), stdout: stdout,
			endErrs: []error{unavailable, unavailable}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:6]).To(Equal([]string{callLLM, beginPhase, runS1,
			endSucceeded, endSucceeded, endSucceeded}))
	})

	It("abandons a phase that an earlier process opened and did not finish", func() {
		hash := awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		gw.setOpen(key, hash)
		executingAt(key, ReasonPhaseOpen)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, endFailed}))
	})

	It("ends a phase that was left open before its checkpoint was written", func() {
		hash := awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		gw.setOpen(key, hash)
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginPhase, endFailed}))
	})

	It("confirms with the gateway a phase whose end it did not record", func() {
		hash := awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		gw.setExecuted(key, hash)
		executingAt(key, ReasonStepsSucceeded)

		Eventually(executedReason(key)).Should(Equal(ReasonPhaseEnded))
		Expect(gw.calls(key)[:3]).To(Equal([]string{callLLM, endSucceeded, runProbes}))
	})

	It("ends the phase as failed when the gateway refuses to let it succeed", func() {
		refused := status.Error(codes.FailedPrecondition, "the phase cannot succeed")
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1})), stdout: stdout, endErrs: []error{refused}})
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		Expect(readyCondition(key).Message).To(ContainSubstring("the phase cannot succeed"))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginPhase, runS1, endSucceeded, runProbes, endFailed}))
		Expect(gw.openPhase(key)).To(BeEmpty())
	})

	It("fails when the gateway does not know the phase it was finishing", func() {
		awaitingApproval(&script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		executingAt(key, ReasonStepsSucceeded)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonInterrupted))
		// The EndPhase as failed is refused (no open phase) and changes nothing.
		Expect(gw.calls(key)).To(Equal([]string{callLLM, endSucceeded, runProbes, endFailed}))
		Expect(gw.openPhase(key)).To(BeEmpty())
	})
})
