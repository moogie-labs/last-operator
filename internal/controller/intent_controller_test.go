package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// directReconciler shares the suite's gateway and reads uncached, but writes
// through c. Specs call its phase handlers with an object of their choosing,
// as a reconcile racing the suite's controller would.
func directReconciler(c client.Client) *IntentReconciler {
	return &IntentReconciler{Client: c, Scheme: scheme.Scheme, Gateway: gw, Reader: k8sClient,
		Planner: &agent.Planner{LLM: gw}, Recorder: events.NewFakeRecorder(100), RetryDelay: 10 * time.Millisecond}
}

var _ = Describe("Intent planning", func() {
	var key types.NamespacedName
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	It("plans and waits for an approval of that exact plan", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.Plan.Hash).To(Equal(hashOf(steps)))
		Expect(in.Status.Plan.Summary).To(Equal("test plan"))
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageCreate))
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))

		By("ignoring approvals of another plan, stage, or Intent")
		createApproval(key, lastv1alpha1.StageCreate, zeroHash(), lastv1alpha1.DecisionApprove)
		createApproval(key, lastv1alpha1.StageDelete, in.Status.Plan.Hash, lastv1alpha1.DecisionApprove)
		createApproval(types.NamespacedName{Namespace: key.Namespace, Name: "someone-else"}, lastv1alpha1.StageCreate,
			in.Status.Plan.Hash, lastv1alpha1.DecisionApprove)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM}))
	})

	It("fails when the plan is denied, even next to an approval", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		// Both decisions exist before the Intent does, so one reconcile sees both.
		createApproval(key, lastv1alpha1.StageCreate, hashOf(steps), lastv1alpha1.DecisionApprove)
		createApproval(key, lastv1alpha1.StageCreate, hashOf(steps), lastv1alpha1.DecisionDeny)
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonApprovalDenied))
		Consistently(func() []string { return gw.calls(key) }).ShouldNot(ContainElement("BeginPhase"))
	})

	It("fails when the API server rejects the plan", func() {
		// plan.Validate and Parse pass a teardown command without argv or script; the CRD refuses it.
		bad := lastv1alpha1.ManagedResourceSpec{Description: "db", Teardown: []lastv1alpha1.Command{{ID: "t1"}}}
		gw.setScript(key, &script{llm: turns(answer(steps, bad))})
		createIntent(key, "bad plan")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("rejected the plan"))
		Expect(getIntent(key).Status.Plan).To(BeNil())
	})

	It("plans again when the spec changes before approval", func() {
		v1 := []lastv1alpha1.Command{argvStep("s1", "echo", "v1")}
		v2 := []lastv1alpha1.Command{argvStep("s1", "echo", "v2")}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(v2), nil
			}
			return answer(v1), nil
		}})
		createIntent(key, "say v1")
		Eventually(planHash(key)).Should(Equal(hashOf(v1)))

		editRequest(key, "say v2")
		Eventually(planHash(key)).Should(Equal(hashOf(v2)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
	})

	It("fails after MaxTurns answers that are not plans, and plans again after Failed when the spec changes", func() {
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "fixed") {
				return answer(steps), nil
			}
			return "not a plan", nil
		}})
		createIntent(key, "broken")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, callLLM, callLLM}))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))

		editRequest(key, "fixed")
		Eventually(planHash(key)).Should(Equal(hashOf(steps)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
	})

	It("does not run an approval granted before a spec edit", func() {
		other := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "pods")}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(other), nil
			}
			return answer(steps), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		stale := getIntent(key)

		editRequest(key, "list namespaces v2")
		Eventually(planHash(key)).Should(Equal(hashOf(other)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))

		// A reconcile that read the Intent before the edit sees an approval of
		// its plan; its status write conflicts and is retried on the new spec.
		approve := stale.Status.Plan.Hash
		createApproval(key, lastv1alpha1.StageCreate, approve, lastv1alpha1.DecisionApprove)
		_, err := directReconciler(k8sClient).awaitApproval(ctx, stale)
		Expect(err).NotTo(HaveOccurred())

		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(planHash(key)()).To(Equal(hashOf(other)))
		Expect(gw.calls(key)).NotTo(ContainElement(beginPhase))
	})

	It("fails when the API server changes the plan it stores", func() {
		resource := lastv1alpha1.ManagedResourceSpec{Description: "namespaces", OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
			Outputs: map[string]string{"name": "${steps.s1.stdout}"}}
		createIntent(key, "list namespaces") // no script yet: the suite's controller fails it and leaves it alone
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		gw.setScript(key, &script{llm: turns(answer(steps, resource))})

		// An API server with a stale CRD prunes the fields it does not know, such as outputs.
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		pruning := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if in, ok := obj.(*lastv1alpha1.Intent); ok && in.Status.Plan != nil {
					for i := range in.Status.Plan.Resources {
						in.Status.Plan.Resources[i].Outputs = nil
					}
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}})
		in := getIntent(key)
		in.Status.Phase = lastv1alpha1.PhasePlanning
		_, err = directReconciler(pruning).plan(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal("The API server changed the plan when storing it; are the CRDs current?"))
	})

	It("fails when the gateway has no LLM answer for the Intent", func() {
		createIntent(key, "no script")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("no script for this intent"))
	})
})
