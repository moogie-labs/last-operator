package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// directReconciler shares the suite's gateway and reads uncached, but writes
// through c. Specs call its phase handlers with an object of their choosing,
// as a reconcile racing the suite's controller would.
func directReconciler(c client.Client) *IntentReconciler {
	return &IntentReconciler{Client: c, Scheme: scheme.Scheme, Gateway: gw, Reader: k8sClient,
		Planner: &agent.Planner{LLM: gw}, Recorder: events.NewFakeRecorder(100), RetryDelay: 10 * time.Millisecond}
}

// pruningClient writes Intent status as an API server with a stale CRD would:
// it stores the copy prune changed, and hands back only the new
// resourceVersion. The client decodes a response into the object it sent, and
// a decode merges, so the pruned field survives in memory.
func pruningClient(prune func(*lastv1alpha1.Intent) bool) client.Client {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			in, ok := obj.(*lastv1alpha1.Intent)
			if !ok {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			pruned := in.DeepCopy()
			if !prune(pruned) {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			if err := c.SubResource(sub).Update(ctx, pruned, opts...); err != nil {
				return err
			}
			in.SetResourceVersion(pruned.GetResourceVersion())
			return nil
		}})
}

var _ = Describe("Intent planning", func() {
	var key types.NamespacedName
	steps := []lastv1alpha1.Command{argvStep("s1", argKubectl, argGet, namespacesArg)}

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
		Expect(in.Status.ObservedRequestHash).To(Equal(requestHash("list namespaces")))
		Expect(in.Status.PlanningSessionSeq).To(Equal(int64(gw.auditOf(key, "llm.request")[0].GetSeq())))
		Expect(gw.lastRequest(key).GetResponseSchema()).To(Equal(agent.Schema()))

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
		// Approvals name the Intent's UID, so they come after it. The Deny comes first:
		// a Deny wins over an Approve whenever the reconcile sees both.
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		createApproval(key, lastv1alpha1.StageCreate, hashOf(steps), lastv1alpha1.DecisionDeny)
		createApproval(key, lastv1alpha1.StageCreate, hashOf(steps), lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonApprovalDenied))
		Consistently(func() []string { return gw.calls(key) }).ShouldNot(ContainElement("BeginPhase"))
	})

	It("ignores foreign-UID and stale decisions", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		h := hashOf(steps)

		By("ignoring a Deny and an Approve for a previous Intent of the same name")
		createApprovalForUID(key, "a-previous-intent", lastv1alpha1.StageCreate, h, lastv1alpha1.DecisionDeny)
		createApprovalForUID(key, "a-previous-intent", lastv1alpha1.StageCreate, h, lastv1alpha1.DecisionApprove)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).NotTo(ContainElement(beginPhase))

		By("ignoring a stale Approve alone")
		DeferCleanup(resetClock)
		advanceClock(25 * time.Hour) // the operator's clock runs ahead, so the next Approve is already stale
		createApproval(key, lastv1alpha1.StageCreate, h, lastv1alpha1.DecisionApprove)
		Expect(intentReconciler.awaitApproval(ctx, getIntent(key))).Error().NotTo(HaveOccurred())
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).NotTo(ContainElement(beginPhase))
	})

	It("counts a valid Approve despite a stale Deny", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		h := hashOf(steps)
		// Server timestamps have one-second resolution: the Approve is a whole second younger than the Deny.
		createApprovalForUID(key, string(in.UID), lastv1alpha1.StageCreate, h, lastv1alpha1.DecisionDeny)
		time.Sleep(2100 * time.Millisecond)
		createApproval(key, lastv1alpha1.StageCreate, h, lastv1alpha1.DecisionApprove)
		var list lastv1alpha1.IntentApprovalList
		Expect(k8sClient.List(ctx, &list, client.InNamespace(key.Namespace))).To(Succeed())
		var denyAt time.Time
		for _, a := range list.Items {
			if a.Spec.Decision == lastv1alpha1.DecisionDeny {
				denyAt = a.CreationTimestamp.Time
			}
		}
		r := directReconciler(k8sClient)
		r.Now = func() time.Time { return denyAt.Add(lastv1alpha1.DefaultApprovalMaxAge + time.Second) }
		approved, denial, err := r.decision(ctx, in, lastv1alpha1.StageCreate, h)
		Expect(err).NotTo(HaveOccurred())
		Expect(denial).To(BeNil())
		Expect(approved).To(BeTrue())
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
			if revised(req) {
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

	It("fails after MaxAttempts answers that are not turns, and plans again after Failed when the spec changes", func() {
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
		other := []lastv1alpha1.Command{argvStep("s1", argKubectl, argGet, "pods")}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if revised(req) {
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
		resource := lastv1alpha1.ManagedResourceSpec{Description: namespacesArg, OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "web-outputs"},
			Outputs: map[string]string{"name": "${steps.s1.stdout}"}}
		createIntent(key, "list namespaces") // no script yet: the suite's controller fails it and leaves it alone
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		gw.setScript(key, &script{llm: turns(answer(steps, resource))})

		// A stale CRD prunes the fields it does not know, such as outputs.
		pruning := pruningClient(func(in *lastv1alpha1.Intent) bool {
			if in.Status.Plan == nil {
				return false
			}
			for i := range in.Status.Plan.Resources {
				in.Status.Plan.Resources[i].Outputs = nil
			}
			return true
		})
		in := getIntent(key)
		in.Status.Phase = lastv1alpha1.PhasePlanning
		_, err := directReconciler(pruning).plan(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal("The API server changed the plan when storing it; are the CRDs current?"))
	})

	It("fails when the API server changes the discovery command it stores", func() {
		look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, namespacesArg}}}
		createIntent(key, "list namespaces") // no script yet: the suite's controller fails it and leaves it alone
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		gw.setScript(key, &script{llm: turns(commandTurn(look))})

		// A CRD from before M3 has no pendingCommand.nonce.
		pruning := pruningClient(func(in *lastv1alpha1.Intent) bool {
			if in.Status.PendingCommand == nil {
				return false
			}
			in.Status.PendingCommand.Nonce = ""
			return true
		})
		in := getIntent(key)
		in.Status.Phase = lastv1alpha1.PhasePlanning
		_, err := directReconciler(pruning).plan(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyCondition(key).Message).To(Equal("The API server changed the command when storing it; are the CRDs current?"))
	})

	It("fails when the gateway has no LLM answer for the Intent", func() {
		createIntent(key, "no script")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("no script for this intent"))
	})

	It("asks questions, waits for every answer, and continues the same session", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: whichNamespace, Options: []string{"default", intentName}}
		q2 := lastv1alpha1.Question{ID: "q2", Text: "How many replicas?"}
		gw.setScript(key, &script{llm: turns(questionsTurn(q1, q2), answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		in := getIntent(key)
		Expect(in.Status.Questions).To(Equal([]lastv1alpha1.Question{q1, q2}))
		Expect(readyReason(key)).To(Equal(ReasonClarifying))
		session := in.Status.PlanningSessionSeq
		Expect(session).To(Equal(int64(gw.auditOf(key, "llm.request")[0].GetSeq())))

		By("waiting while a question has no answer")
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: intentName})
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))

		By("continuing the session once every question has an answer")
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: intentName}, lastv1alpha1.Answer{ID: "q2", Value: "2"})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in = getIntent(key)
		Expect(in.Status.PlanningSessionSeq).To(Equal(session))
		Expect(in.Status.Questions).To(BeEmpty())
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{roleSystem, roleUser, roleAssistant, roleUser}))
		Expect(msgs[3].GetContent()).To(Equal("Answers:\n- q1: web\n- q2: 2\n"))
	})

	It("starts a new session when the request changes while clarifying", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: whichNamespace}
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if revised(req) {
				return answer(steps), nil
			}
			return questionsTurn(q1), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		first := getIntent(key).Status.PlanningSessionSeq

		editRequest(key, "list namespaces v2")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.PlanningSessionSeq).NotTo(Equal(first))
		Expect(in.Status.ObservedRequestHash).To(Equal(requestHash("list namespaces v2")))
		Expect(in.Status.Questions).To(BeEmpty())
		Expect(roles(gw.lastRequest(key).GetMessages())).To(Equal([]string{roleSystem, roleUser}))
	})

	It("keeps the session of an Intent an M2 operator left in Planning", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: whichNamespace}
		createIntent(key, "list namespaces") // no script yet: the suite's controller fails it and leaves it alone
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		gw.setScript(key, &script{llm: turns(questionsTurn(q1), answer(steps))})

		// An M2 operator recorded no request hash.
		setStatus(key, func(in *lastv1alpha1.Intent) {
			in.Status.ObservedRequestHash = ""
			setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "set by the test")
		})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		session := getIntent(key).Status.PlanningSessionSeq
		Expect(getIntent(key).Status.ObservedRequestHash).To(Equal(requestHash("list namespaces")))

		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: intentName})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.PlanningSessionSeq).To(Equal(session))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, callLLM, callLLM}))
	})

	It("corrects questions that reuse an answered id", func() {
		q1 := lastv1alpha1.Question{ID: "q1", Text: whichNamespace}
		again := lastv1alpha1.Question{ID: "q1", Text: "Which port?"}
		q2 := lastv1alpha1.Question{ID: "q2", Text: "Which port?"}
		gw.setScript(key, &script{llm: turns(questionsTurn(q1), questionsTurn(again), questionsTurn(q2))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))

		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: intentName})
		Eventually(func() []lastv1alpha1.Question { return getIntent(key).Status.Questions }).
			Should(Equal([]lastv1alpha1.Question{q2}))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseClarifying))
		Expect(lastUserMessage(key)).To(ContainSubstring(`question id "q1" was answered earlier`))
	})

	It("asks for approval of a discovery command", func() {
		look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, namespacesArg}}}
		gw.setScript(key, &script{llm: turns(commandTurn(look))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		in := getIntent(key)
		pc := in.Status.PendingCommand
		Expect(pc).NotTo(BeNil())
		Expect(pc.Command.ID).To(Equal(plan.DiscoveryStepID))
		Expect(pc.Command.Run.Argv).To(Equal(look.Run.Argv))
		Expect(pc.Reason).To(Equal("see what exists"))
		Expect(pc.Nonce).To(MatchRegexp("^[0-9a-f]{32}$"))
		hash, _, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
		Expect(err).NotTo(HaveOccurred())
		Expect(pc.Hash).To(Equal(hash))
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageDiscovery))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingDiscoveryApproval))
	})
})

var _ = Describe("Intent planning context", func() {
	var key types.NamespacedName
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	createCleanly := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, obj)).To(Succeed()) })
	}
	provider := func(name, description string) *lastv1alpha1.Provider {
		return &lastv1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: lastv1alpha1.ProviderSpec{
			Type: lastv1alpha1.ProviderEnvSecret, Description: description, SecretRef: &lastv1alpha1.LocalRef{Name: name}}}
	}

	It("tells the model which Providers and egress the namespace may use", func() {
		scripts := false
		createCleanly(&lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: lastv1alpha1.OperatorConfigSpec{Defaults: &lastv1alpha1.IntentPolicySpec{
				Providers: []string{"ctx-k8s", "ctx-aws"}, AllowScripts: &scripts,
				Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.0.0.0/8:443"}}}}})
		createCleanly(provider("ctx-k8s", "cluster-admin on this cluster"))
		createCleanly(provider("ctx-db", "the orders database")) // exists, but the policy does not allow it
		Expect(k8sClient.Create(ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: key.Namespace},
			Spec: lastv1alpha1.IntentPolicySpec{Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"10.1.0.0/16:443"}}}})).To(Succeed())
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		first := requestText(gw.lastRequest(key))
		Expect(first).To(ContainSubstring("Providers commands may use:\n- ctx-k8s: cluster-admin on this cluster\n\n"))
		Expect(first).NotTo(ContainSubstring("ctx-db"))
		Expect(first).NotTo(ContainSubstring("ctx-aws"), "allowed, but no such Provider exists")
		Expect(first).To(ContainSubstring("every one of these lists:\n- 10.0.0.0/8:443\n- 10.1.0.0/16:443\n"))
		Expect(first).To(ContainSubstring("Scripts are not allowed"))
	})

	It("fails planning when the namespace's policy is invalid", func() {
		Expect(k8sClient.Create(ctx, &lastv1alpha1.IntentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: key.Namespace},
			Spec: lastv1alpha1.IntentPolicySpec{Egress: &lastv1alpha1.EgressPolicy{Allowed: []string{"nope"}}}})).To(Succeed())
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "list namespaces")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("effective policy is invalid"))
		Expect(gw.calls(key)).To(BeEmpty())
	})
})
