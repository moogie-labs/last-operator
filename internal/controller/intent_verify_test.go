package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

var _ = Describe("Intent verification", func() {
	var key types.NamespacedName
	probe := func(id string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{trueCmd}}}, Assert: trueCmd}
	}
	resource := lastv1alpha1.ManagedResourceSpec{Description: intentName, Probes: []lastv1alpha1.Probe{probe("p1"), probe("p2")}}
	steps := []lastv1alpha1.Command{argvStep("s1", trueCmd)}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// run scripts the gateway, plans, and approves the plan.
	run := func(s *script) {
		s.llm = turns(answer(steps, resource))
		gw.setScript(key, s)
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
	}

	It("runs every probe once and becomes Ready", func() {
		run(&script{})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(readyCondition(key).Status).To(Equal(metav1.ConditionTrue))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginPhase, runS1, endSucceeded, runProbes}))

		mr := getMR(key.Namespace, "web-0")
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		Expect(mr.Status.LastProbeTime).NotTo(BeNil())
		Expect(mr.Status.Observed.Raw).To(MatchJSON(`{"probes":{"p1":{"passed":true,"exitCode":0},"p2":{"passed":true,"exitCode":0}}}`))
	})

	It("fails verification when a probe fails", func() {
		run(&script{failing: map[string]bool{"p2": true}})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonVerificationFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("p2"))

		c := meta.FindStatusCondition(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Message).To(Equal("Probes failed: p2"))
	})

	It("follows its ManagedResources between Ready and Drifted", func() {
		run(&script{})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		setInSync(key.Namespace, "web-0", metav1.ConditionFalse)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseDrifted))
		Expect(readyReason(key)).To(Equal(ReasonDrifted))

		setInSync(key.Namespace, "web-0", metav1.ConditionTrue)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
	})

	It("ignores a ManagedResource that an earlier plan left out of sync", func() {
		single := lastv1alpha1.ManagedResourceSpec{Description: "web server", Probes: []lastv1alpha1.Probe{probe("p1")}}
		extra := lastv1alpha1.ManagedResourceSpec{Description: "cache", Probes: []lastv1alpha1.Probe{probe("p2")}}
		gw.setScript(key, &script{failing: map[string]bool{"p2": true}, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(steps, single), nil
			}
			return answer(steps, single, extra), nil
		}})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonVerificationFailed))
		Expect(meta.IsStatusConditionFalse(getMR(key.Namespace, "web-1").Status.Conditions, CondInSync)).To(BeTrue())

		editRequest(key, "provision v2")
		Eventually(planHash(key)).Should(Equal(hashOf(steps, single)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(readyReason(key)).To(Equal(ReasonReady))

		// The stale web-1 stays until the Delete stage, untouched by the new plan.
		Expect(meta.IsStatusConditionFalse(getMR(key.Namespace, "web-1").Status.Conditions, CondInSync)).To(BeTrue())
		mr := getMR(key.Namespace, "web-0")
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		Expect(mr.Status.Observed.Raw).To(MatchJSON(`{"probes":{"p1":{"passed":true,"exitCode":0}}}`))
	})

	It("clears an earlier plan's probe outcomes when a new plan rewrites a ManagedResource", func() {
		probed := lastv1alpha1.ManagedResourceSpec{Description: "web server", Probes: []lastv1alpha1.Probe{probe("p1")}}
		unprobed := lastv1alpha1.ManagedResourceSpec{Description: "web without probes"}
		gw.setScript(key, &script{failing: map[string]bool{"p1": true}, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(steps, unprobed), nil
			}
			return answer(steps, probed), nil
		}})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(meta.IsStatusConditionFalse(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)).To(BeTrue())

		editRequest(key, "provision v2")
		Eventually(planHash(key)).Should(Equal(hashOf(steps, unprobed)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		mr := getMR(key.Namespace, "web-0")
		Expect(mr.Spec.Description).To(Equal("web without probes"))
		Expect(meta.FindStatusCondition(mr.Status.Conditions, CondInSync)).To(BeNil())
		Expect(mr.Status.Observed).To(BeNil())
		Expect(mr.Status.LastProbeTime).To(BeNil())
	})

	It("ignores a spec edit at Ready until the Update stage exists", func() {
		run(&script{})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		n := len(gw.calls(key))

		editRequest(key, "something else")
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.calls(key)).To(HaveLen(n))
	})
})
