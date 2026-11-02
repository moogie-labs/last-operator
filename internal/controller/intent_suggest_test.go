package controller

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gateway/llm"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

// k8sStep is an argv step that names the kubernetes provider, so a rule can hold it.
func k8sStep(id string, argv ...string) lastv1alpha1.Command {
	c := argvStep(id, argv...)
	c.Providers = []string{providerK8s}
	return c
}

// suggestReply is a model's reply to a suggest call.
func suggestReply(answers ...suggest.Answer) string {
	type item struct {
		Entry     string   `json:"entry"`
		Argv      []string `json:"argv"`
		Rationale string   `json:"rationale"`
	}
	items := make([]item, 0, len(answers))
	for _, a := range answers {
		items = append(items, item{a.Entry, a.Argv, a.Rationale})
	}
	b, err := json.Marshal(map[string]any{"suggestions": items})
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

func suggestionsIn(ns string) []lastv1alpha1.CommandRuleSuggestion {
	var list lastv1alpha1.CommandRuleSuggestionList
	Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
	return list.Items
}

func getSuggestion(ns, name string) *lastv1alpha1.CommandRuleSuggestion {
	var s lastv1alpha1.CommandRuleSuggestion
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s)).To(Succeed())
	return &s
}

// createSuggestion creates a suggestion with a decision, as an earlier trigger
// and a human deciding it would leave it.
func createSuggestion(ns, name string, trigger lastv1alpha1.SuggestionTrigger, decision lastv1alpha1.SuggestionDecision,
	spec lastv1alpha1.CommandRuleSpec) {
	s := &lastv1alpha1.CommandRuleSuggestion{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: lastv1alpha1.CommandRuleSuggestionSpec{TargetScope: lastv1alpha1.RuleScopeNamespace, Trigger: trigger,
			Proposed: spec, Decision: decision}}
	if decision != lastv1alpha1.SuggestionPending {
		s.Spec.DecidedBy = approverAlice
	}
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
}

// suggestionName is the name the operator gives a Namespace proposal of spec in ns.
func suggestionName(ns string, spec lastv1alpha1.CommandRuleSpec) string {
	n, err := suggest.Name(suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeNamespace, Namespace: ns, Spec: spec})
	Expect(err).NotTo(HaveOccurred())
	return n
}

// planSuggested reports whether the Intent's plan is marked suggested.
func planSuggested(key types.NamespacedName) func() bool {
	return func() bool {
		p := getIntent(key).Status.Plan
		return p != nil && p.Suggested
	}
}

func suggestCalls(key types.NamespacedName) func() int {
	return func() int { return len(gw.suggestRequests(key)) }
}

var _ = Describe("PlanTime suggestions", func() {
	var key types.NamespacedName
	create, remediate, del := lastv1alpha1.StageCreate, lastv1alpha1.StageRemediate, lastv1alpha1.StageDelete
	s1 := k8sStep("s1", argKubectl, "apply", "-f", "web.yaml")
	steps := []lastv1alpha1.Command{s1}
	applyAny := []string{argKubectl, "apply", "-f", anyValue}
	applyAnySpec := lastv1alpha1.CommandRuleSpec{Stages: stages(create, remediate), Providers: []string{providerK8s},
		Match: lastv1alpha1.ArgvPattern{Argv: applyAny}}
	applyWebSpec := lastv1alpha1.CommandRuleSpec{Stages: stages(create, remediate), Providers: []string{providerK8s},
		Match: lastv1alpha1.ArgvPattern{Argv: s1.Run.Argv}}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
		DeferCleanup(func() { gw.setOnSuggest(nil) })
	})

	It("creates a PlanTime suggestion per uncovered entry with Remediate added for a Create plan", func() {
		probe := k8sStep("", argKubectl, argGet, "deployment", intentName, "-o", formatJSON)
		teardown := k8sStep("", argKubectl, "delete", "deployment", intentName)
		res := lastv1alpha1.ManagedResourceSpec{Description: intentName,
			Probes: []lastv1alpha1.Probe{{ID: "p1", Command: probe, Assert: replicasAssert}}, Teardown: []lastv1alpha1.Command{teardown}}
		gw.setScript(key, &script{llm: turns(answer(steps, res))})
		gw.setSuggestScript(key, suggestReply(
			suggest.Answer{Entry: "s1", Argv: applyAny, Rationale: "any manifest"},
			suggest.Answer{Entry: "teardown:0.0", Argv: teardown.Run.Argv, Rationale: "the teardown"}))
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingApproval))

		reqs := gw.suggestRequests(key)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].GetPurpose()).To(Equal(gatewayv1.PurposeSuggest))
		Expect(reqs[0].GetResponseSchema()).To(Equal(suggest.Schema()))
		Expect(roles(reqs[0].GetMessages())).To(Equal([]string{roleSystem, roleUser}))
		Expect(reqs[0].GetMessages()[0].GetContent()).To(Equal(suggest.SystemPrompt))
		Expect(reqs[0].GetMessages()[1].GetContent()).To(Equal(suggest.Request([]suggest.Entry{
			{ID: "s1", Stage: create, Argv: s1.Run.Argv},
			{ID: "probe:p1", Stage: create, Argv: probe.Run.Argv},
			{ID: "teardown:0.0", Stage: del, Argv: teardown.Run.Argv},
		})))

		teardownSpec := lastv1alpha1.CommandRuleSpec{Stages: stages(del), Providers: []string{providerK8s},
			Match: lastv1alpha1.ArgvPattern{Argv: teardown.Run.Argv}}
		stepName, teardownName := suggestionName(key.Namespace, applyAnySpec), suggestionName(key.Namespace, teardownSpec)
		Expect(getIntent(key).Status.SuggestedRules).To(Equal([]string{stepName, teardownName}))

		got := getSuggestion(key.Namespace, stepName)
		Expect(got.Labels).To(HaveKeyWithValue("last.moogie-labs.com/intent", intentName))
		Expect(got.Spec.TargetScope).To(Equal(lastv1alpha1.RuleScopeNamespace))
		Expect(got.Spec.Trigger).To(Equal(lastv1alpha1.TriggerPlanTime))
		Expect(got.Spec.Decision).To(Equal(lastv1alpha1.SuggestionPending))
		Expect(got.Spec.Rationale).To(Equal("any manifest"))
		Expect(got.Spec.Proposed).To(Equal(applyAnySpec))
		Expect(got.Spec.Examples).To(Equal([]lastv1alpha1.Command{s1}))
		td := getSuggestion(key.Namespace, teardownName)
		Expect(td.Spec.Proposed).To(Equal(teardownSpec))
		Expect(td.Spec.Examples).To(Equal([]lastv1alpha1.Command{teardown}))
		Expect(suggestionsIn(key.Namespace)).To(HaveLen(2))
		Eventually(func() []string { return eventNotes(key, "SuggestionCreated") }).Should(HaveLen(2))
	})

	It("asks the LLM once per plan", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: applyAny}))
		createIntent(key, "run web")
		Eventually(planSuggested(key)).Should(BeTrue())

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			in := getIntent(key)
			metav1.SetMetaDataAnnotation(&in.ObjectMeta, "test/wake", "1")
			return k8sClient.Update(ctx, in)
		})).To(Succeed())
		Consistently(suggestCalls(key)).Should(Equal(1))

		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(suggestCalls(key)()).To(Equal(1))
	})

	It("drops a code-varying answer with SuggestionDropped", func() {
		code := k8sStep("s1", "bash", "-c", "echo hi")
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{code}))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: []string{"bash", "-c", "*"}}))
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Eventually(func() []string { return eventNotes(key, "SuggestionDropped") }).Should(
			ConsistOf(ContainSubstring("the code the pattern allows can vary")))
		Expect(suggestionsIn(key.Namespace)).To(BeEmpty())
		Expect(getIntent(key).Status.SuggestedRules).To(BeEmpty())
	})

	It("suppresses a proposal broader than a denied one with SuggestionSuppressed naming it", func() {
		createSuggestion(key.Namespace, "denied-apply", lastv1alpha1.TriggerPlanTime, lastv1alpha1.SuggestionDenied, applyWebSpec)
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: applyAny}))
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Eventually(func() []string { return eventNotes(key, "SuggestionSuppressed") }).Should(
			ConsistOf(ContainSubstring("denied-apply")))
		Expect(suggestionsIn(key.Namespace)).To(HaveLen(1))
		Expect(getIntent(key).Status.SuggestedRules).To(BeEmpty())
	})

	It("suppresses a proposal identical to a denied one from another trigger", func() {
		denied := suggestionName(key.Namespace, applyAnySpec)
		createSuggestion(key.Namespace, denied, lastv1alpha1.TriggerRepeatedApproval, lastv1alpha1.SuggestionDenied, applyAnySpec)
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: applyAny}))
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Eventually(func() []string { return eventNotes(key, "SuggestionSuppressed") }).Should(
			ConsistOf(ContainSubstring(denied)))
		Expect(eventNotes(key, "SuggestionCreated")).To(BeEmpty())
		Expect(getIntent(key).Status.SuggestedRules).To(BeEmpty(), "never found as an existing suggestion")
		got := getSuggestion(key.Namespace, denied)
		Expect(got.Spec.Trigger).To(Equal(lastv1alpha1.TriggerRepeatedApproval))
		Expect(got.Spec.Decision).To(Equal(lastv1alpha1.SuggestionDenied))
	})

	It("creates a narrower proposal than a denied one", func() {
		createSuggestion(key.Namespace, "denied-apply", lastv1alpha1.TriggerPlanTime, lastv1alpha1.SuggestionDenied, applyAnySpec)
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: s1.Run.Argv}))
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		name := suggestionName(key.Namespace, applyWebSpec)
		Expect(getIntent(key).Status.SuggestedRules).To(Equal([]string{name}))
		Expect(getSuggestion(key.Namespace, name).Spec.Proposed).To(Equal(applyWebSpec))
		Expect(eventNotes(key, "SuggestionSuppressed")).To(BeEmpty())
	})

	It("stops at 100 pending suggestions with SuggestionLimit", func() {
		other := lastv1alpha1.CommandRuleSpec{Stages: stages(create), Providers: []string{providerK8s},
			Match: lastv1alpha1.ArgvPattern{Argv: []string{argKubectl, argGet, "pods"}}}
		for i := range 100 {
			createSuggestion(key.Namespace, fmt.Sprintf("pending-%03d", i), lastv1alpha1.TriggerPlanTime,
				lastv1alpha1.SuggestionPending, other)
		}
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: applyAny}))
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Eventually(func() []string { return eventNotes(key, "SuggestionLimit") }).Should(
			ConsistOf(ContainSubstring("100 Pending suggestions")))
		Expect(suggestionsIn(key.Namespace)).To(HaveLen(100))
		Expect(getIntent(key).Status.SuggestedRules).To(BeEmpty())
	})

	It("an LLM failure raises SuggestionFailed, marks the plan suggested, and the Intent still waits for approval", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, "not json")
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Eventually(func() []string { return eventNotes(key, "SuggestionFailed") }).Should(HaveLen(1))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(suggestionsIn(key.Namespace)).To(BeEmpty())

		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(suggestCalls(key)()).To(Equal(1), "never retried")
	})

	It("treats a suggest call with no script as no proposal, with no SuggestionFailed", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "run web")

		Eventually(planSuggested(key)).Should(BeTrue())
		Expect(suggestCalls(key)()).To(Equal(1))
		Consistently(func() []string { return eventNotes(key, "SuggestionFailed") }).Should(BeEmpty())
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingApproval))
	})

	It("the Intent deleted during the suggest call ends the step without error", func() {
		gw.setScript(key, &script{llm: turns(answer(steps))})
		createIntent(key, "run web")
		Eventually(planSuggested(key)).Should(BeTrue(), "the first step had no script")

		in := getIntent(key)
		gw.setSuggestScript(key, "{}", suggestReply(suggest.Answer{Entry: "s1", Argv: applyAny}))
		gw.setOnSuggest(func(k types.NamespacedName) {
			defer GinkgoRecover()
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cur := getIntent(k)
				controllerutil.RemoveFinalizer(cur, lastv1alpha1.FinalizerTeardown)
				return k8sClient.Update(ctx, cur)
			})).To(Succeed())
			Expect(k8sClient.Delete(ctx, getIntent(k))).To(Succeed())
		})
		Expect(intentReconciler.planTime(ctx, in)).To(Succeed())
		Expect(suggestionsIn(key.Namespace)).To(ConsistOf(HaveField("ObjectMeta.Name", suggestionName(key.Namespace, applyAnySpec))),
			"a suggestion already created stays")
	})

	It("makes no suggest call for a plan that runs without approval", func() {
		createRule(key.Namespace, "apply", stages(create), argKubectl, "apply", "-f", "*")
		gw.setScript(key, &script{llm: turns(answer(steps))})
		gw.setSuggestScript(key, suggestReply(suggest.Answer{Entry: "s1", Argv: applyAny}))
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Consistently(suggestCalls(key)).Should(BeZero())
		Expect(getIntent(key).Status.Plan.Suggested).To(BeFalse())
	})
})

var _ = Describe("noScript", func() {
	It("recognizes the gateway's answer to a suggest call with no script, and nothing else", func() {
		Expect(noScript(status.Errorf(codes.FailedPrecondition, "%v", fmt.Errorf("%w: _suggest.ns.web", llm.ErrNoScript)))).To(BeTrue())
		Expect(noScript(status.Error(codes.Unavailable, llm.ErrNoScript.Error()))).To(BeFalse())
		Expect(noScript(status.Error(codes.FailedPrecondition, "llm: the model refused"))).To(BeFalse())
		Expect(noScript(nil)).To(BeFalse())
	})
})
