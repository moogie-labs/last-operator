package controller

import (
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// Call log entries of a teardown.
const (
	runT1 = "RunCommand:t1"
	runT2 = "RunCommand:t2"
)

// deleting reports whether a CallLLM request belongs to a Delete planning session.
func deleting(req *gatewayv1.CallLLMRequest) bool {
	return strings.Contains(requestText(req), "is being deleted")
}

// lifecycle answers Create planning with create and Delete planning with del.
func lifecycle(create, del llmFunc) llmFunc {
	return func(req *gatewayv1.CallLLMRequest) (string, error) {
		if deleting(req) {
			return del(req)
		}
		return create(req)
	}
}

// deleteRequests returns the Intent's CallLLM requests of Delete planning sessions.
func deleteRequests(key types.NamespacedName) []*gatewayv1.CallLLMRequest {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	var out []*gatewayv1.CallLLMRequest
	for _, r := range gw.llmReqs[key] {
		if deleting(r) {
			out = append(out, r)
		}
	}
	return out
}

// gone reports whether the Intent no longer exists.
func gone(key types.NamespacedName) func() bool {
	return func() bool {
		var in lastv1alpha1.Intent
		return apierrors.IsNotFound(k8sClient.Get(ctx, key, &in))
	}
}

func deleteIntent(key types.NamespacedName) {
	Expect(k8sClient.Delete(ctx, &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})).
		To(Succeed())
}

// setRetry sets the retry annotation, as the user does to plan a teardown again.
func setRetry(key types.NamespacedName, value string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		if in.Annotations == nil {
			in.Annotations = map[string]string{}
		}
		in.Annotations[lastv1alpha1.AnnotationRetry] = value
		return k8sClient.Update(ctx, &in)
	})).To(Succeed())
}

// approveDelete approves the Intent's current plan for stage Delete.
func approveDelete(key types.NamespacedName) {
	in := getIntent(key)
	Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageDelete))
	createApproval(key, lastv1alpha1.StageDelete, in.Status.Plan.Hash, lastv1alpha1.DecisionApprove)
}

// eventReasons returns the reasons of the Events about the Intent, of one type.
func eventReasons(key types.NamespacedName, typ string) []string {
	var list eventsv1.EventList
	Expect(k8sClient.List(ctx, &list, client.InNamespace(key.Namespace))).To(Succeed())
	var out []string
	for _, e := range list.Items {
		if e.Regarding.Name == key.Name && e.Type == typ {
			out = append(out, e.Reason)
		}
	}
	return out
}

// eventNotes returns the notes of the Events about the Intent with reason.
func eventNotes(key types.NamespacedName, reason string) []string {
	var list eventsv1.EventList
	Expect(k8sClient.List(ctx, &list, client.InNamespace(key.Namespace))).To(Succeed())
	var out []string
	for _, e := range list.Items {
		if e.Regarding.Name == key.Name && e.Reason == reason {
			out = append(out, e.Note)
		}
	}
	return out
}

// withoutQueries drops the QueryAudit calls from a call log: the partial-plan
// read scans command.result entries, which the fake logs.
func withoutQueries(calls []string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return c == queryAudit })
}

func countOf(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

var _ = Describe("Intent deletion", func() {
	var key types.NamespacedName
	s1 := argvStep("s1", argKubectl, "apply", "-f", "/work/app.yaml")
	t1 := argvStep("t1", argKubectl, "delete", "deployment", "web")
	t2 := argvStep("t2", argKubectl, "delete", "service", "web")
	resources := []lastv1alpha1.ManagedResourceSpec{
		{Description: "Deployment web", Teardown: []lastv1alpha1.Command{t1}},
		{Description: "Service web", Teardown: []lastv1alpha1.Command{t2}},
	}
	createPlan := answer([]lastv1alpha1.Command{s1}, resources...)
	// The model orders the teardown: the Service first, then the Deployment.
	teardown := []lastv1alpha1.Command{t2, t1}
	deletePlan := answer(teardown)

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// executed runs the Create plan to Ready; del answers Delete planning.
	executed := func(s *script, del llmFunc) {
		s.llm = lifecycle(turns(createPlan), del)
		gw.setScript(key, s)
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		getMR(key.Namespace, "web-0")
		getMR(key.Namespace, web1)
	}

	// awaitingDelete deletes an executed Intent and waits for its Delete plan.
	awaitingDelete := func() {
		deleteIntent(key)
		Eventually(func() lastv1alpha1.Stage { return getIntent(key).Status.Stage }).Should(Equal(lastv1alpha1.StageDelete))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
	}

	It("adds the teardown finalizer before the first phase begins", func() {
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		createIntent(key, "run web")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))
		Expect(gw.calls(key)).NotTo(ContainElement(beginPhase))
	})

	Describe("releases an Intent that never executed at once", func() {
		It("while it awaits approval", func() {
			gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{s1}, resources...))})
			createIntent(key, "run web")
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))

			deleteIntent(key)
			Eventually(gone(key)).Should(BeTrue())
			Expect(gw.calls(key)).To(Equal([]string{callLLM}), "no Delete planning, and no phase")
		})

		It("when it failed before executing", func() {
			gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{s1}, resources...))})
			createIntent(key, "run web")
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionDeny)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
			Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))

			deleteIntent(key)
			Eventually(gone(key)).Should(BeTrue())
			Expect(gw.calls(key)).To(Equal([]string{callLLM}))
			Expect(eventReasons(key, "Warning")).NotTo(ContainElement(ReasonNoTeardown))
		})

		It("when its failed Create ran no step, without a warning", func() {
			// A failed image build runs nothing, so the audit log holds no command.result.
			gw.setScript(key, &script{buildLog: map[string]string{"s1": "E: boom"},
				llm: turns(answer([]lastv1alpha1.Command{s1}, resources...), answer([]lastv1alpha1.Command{t1}))})
			createIntent(key, "run web")
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionApprove)
			Eventually(planHash(key)).Should(Equal(hashOf([]lastv1alpha1.Command{t1})))

			deleteIntent(key)
			Eventually(gone(key)).Should(BeTrue())
			Expect(deleteRequests(key)).To(BeEmpty(), "no step ran, so there is nothing to tear down")
			Expect(eventReasons(key, "Warning")).NotTo(ContainElement(ReasonNoTeardown))
		})
	})

	// partialCreate runs a Create whose second step fails after its first ran,
	// and waits for the replanned Create to await approval; del answers Delete planning.
	partialCreate := func(res []lastv1alpha1.ManagedResourceSpec, del llmFunc) string {
		partial := []lastv1alpha1.Command{s1, argvStep("s2", "false")}
		replanned := []lastv1alpha1.Command{argvStep("s3", argKubectl, "apply", "-f", "/work/fixed.yaml")}
		gw.setScript(key, &script{exit: map[string]int32{"s2": 1},
			llm: lifecycle(turns(answer(partial, res...), answer(replanned, res...)), del)})
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(planHash(key)).Should(Equal(hashOf(replanned, res...)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		return hashOf(partial, res...)
	}

	It("tears down a partial Create when deleted", func() {
		partialCreate(resources, turns(deletePlan))
		creation := len(gw.calls(key))
		awaitingDelete()

		By("asking the model to order the partial plan's teardown commands")
		reqs := deleteRequests(key)
		Expect(reqs).To(HaveLen(1))
		first := requestText(reqs[0])
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","deployment","web"]`))
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","service","web"]`))
		Expect(first).To(ContainSubstring("come from plans that did not finish"))
		Expect(first).NotTo(ContainSubstring("--ignore-not-found"), "the commands can only be reordered here")
		Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(teardown)))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval), "a Delete with no executed plan needs approval")
		Expect(readyCondition(key).Message).To(ContainSubstring("for stage Delete"))
		Expect(eventNotes(key, ReasonPartialTeardown)).To(ConsistOf(
			"The teardown includes the teardown commands of 1 plan that did not finish"))
		Expect(withoutQueries(gw.calls(key)[creation:])).To(Equal([]string{callLLM}), "nothing runs before the approval")

		By("running the approved Delete plan, then releasing the Intent")
		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(withoutQueries(gw.calls(key)[creation:])).To(Equal([]string{callLLM, beginPhase, runT2, runT1, endSucceeded}))
		Expect(gw.authsOf(key)).To(Equal([]string{authApproval, authApproval}))
	})

	It("releases with NoTeardown when the partial plan has no teardown", func() {
		partialCreate(nil, turns(deletePlan))
		deleteIntent(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(deleteRequests(key)).To(BeEmpty(), "no Delete planning")
		Expect(eventReasons(key, "Warning")).To(ContainElement(ReasonNoTeardown))
	})

	// olderPartial runs a Create C1 whose step s1 ran before s2 failed, then
	// the replanned C2, which succeeds; createApproved approves C1 and C2 when
	// Create is Manual. C1 alone makes the ConfigMap foo-a, which only its
	// teardown t4 removes. It returns t4.
	olderPartial := func(createApproved bool, del llmFunc, create func()) lastv1alpha1.Command {
		t4 := argvStep("t4", argKubectl, "delete", "configmap", "foo-a")
		c1 := answer([]lastv1alpha1.Command{s1, argvStep("s2", "false")},
			lastv1alpha1.ManagedResourceSpec{Description: "ConfigMap foo-a", Teardown: []lastv1alpha1.Command{t4}})
		gw.setScript(key, &script{exit: map[string]int32{"s2": 1}, llm: lifecycle(turns(c1, createPlan), del)})
		create()
		if createApproved {
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionApprove)
			Eventually(planHash(key)).Should(Equal(hashOf([]lastv1alpha1.Command{s1}, resources...)))
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionApprove)
		}
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.executedOf(key)).To(Equal(hashOf([]lastv1alpha1.Command{s1}, resources...)))
		return t4
	}

	It("tears down a partial Create older than the executed plan", func() {
		olderUnion := []lastv1alpha1.Command{argvStep("t4", argKubectl, "delete", "configmap", "foo-a"), t2, t1}
		t4 := olderPartial(true, turns(answer(olderUnion)), func() { createIntent(key, "run web") })
		Expect(t4).To(Equal(olderUnion[0]))
		creation := len(gw.calls(key))
		awaitingDelete()

		first := requestText(deleteRequests(key)[0])
		deployment := strings.Index(first, `"argv":["kubectl","delete","deployment","web"]`)
		service := strings.Index(first, `"argv":["kubectl","delete","service","web"]`)
		fooA := strings.Index(first, `"argv":["kubectl","delete","configmap","foo-a"]`)
		Expect(deployment).To(BeNumerically(">", 0))
		Expect(service).To(BeNumerically(">", deployment))
		Expect(fooA).To(BeNumerically(">", service), "the executed teardown first, then the older partial plan's")
		Expect(first).To(ContainSubstring("come from plans that did not finish"))
		Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(olderUnion)))

		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(withoutQueries(gw.calls(key)[creation:])).To(Equal([]string{callLLM, beginPhase, "RunCommand:t4", runT2, runT1, endSucceeded}))
	})

	It("an Auto Delete with a partial union waits for approval", func() {
		allowAuto()
		olderUnion := []lastv1alpha1.Command{argvStep("t4", argKubectl, "delete", "configmap", "foo-a"), t2, t1}
		olderPartial(false, turns(answer(olderUnion)), func() {
			in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: lastv1alpha1.IntentSpec{Request: "run web",
					Modes: &lastv1alpha1.StageModes{Create: lastv1alpha1.ModeAuto, Delete: lastv1alpha1.ModeAuto}}}
			Expect(k8sClient.Create(ctx, in)).To(Succeed())
		})
		deleteIntent(key)
		Eventually(func() lastv1alpha1.Stage { return getIntent(key).Status.Stage }).Should(Equal(lastv1alpha1.StageDelete))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval), "not Failed")
		Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(olderUnion)))
		Expect(gw.calls(key)).NotTo(ContainElement("RunCommand:t4"))

		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(gw.authsOf(key)).To(HaveLen(3))
		Expect(gw.authsOf(key)[2]).To(Equal(authApproval), "the union is not the executed teardown, so Auto does not cover it")
	})

	It("deleting while Executing after a step ran tears that plan down", func() {
		two := []lastv1alpha1.Command{s1, argvStep("s2", argKubectl, "apply", "-f", "/work/svc.yaml")}
		deleted := false
		gw.setScript(key, &script{
			runErr: map[string]error{"s2": status.Error(codes.Unavailable, "the gateway is restarting")},
			onRun: func(step string) {
				if step == "s2" && !deleted {
					deleted = true
					deleteIntent(key) // the deletion lands while the Create is mid-plan
				}
			},
			llm: lifecycle(turns(answer(two, resources...)), turns(deletePlan)),
		})
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)

		Eventually(func() lastv1alpha1.Stage { return getIntent(key).Status.Stage }).Should(Equal(lastv1alpha1.StageDelete))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.openPhase(key)).To(BeEmpty(), "the Create phase was closed without succeeding")
		Expect(gw.executedOf(key)).To(BeEmpty())
		first := requestText(deleteRequests(key)[0])
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","deployment","web"]`))
		Expect(first).To(ContainSubstring("come from plans that did not finish"))
		Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(teardown)))

		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(countOf(gw.calls(key), runT1)).To(Equal(1))
	})

	It("unions the executed and partial teardowns, deduped, in order", func() {
		t3 := argvStep("t3", argKubectl, "delete", "configmap", "web")
		// The partial Update lists its resources in another order, and repeats two of the executed teardown commands.
		updateResources := []lastv1alpha1.ManagedResourceSpec{
			{Description: "ConfigMap web", Teardown: []lastv1alpha1.Command{t3}}, resources[1], resources[0],
		}
		partial := []lastv1alpha1.Command{argvStep("u1", argKubectl, "apply", "-f", "/work/cm.yaml"), argvStep("u2", "false")}
		replanned := []lastv1alpha1.Command{argvStep("u3", argKubectl, "apply", "-f", "/work/cm-fixed.yaml")}
		union := []lastv1alpha1.Command{t3, t2, t1}
		executed(&script{exit: map[string]int32{"u2": 1}}, turns(answer(union)))
		gw.updateScript(key, func(s *script) {
			create := s.llm
			update := turns(answer(partial, updateResources...), answer(replanned, updateResources...))
			s.llm = func(req *gatewayv1.CallLLMRequest) (string, error) {
				if revised(req) && !deleting(req) {
					return update(req)
				}
				return create(req)
			}
		})
		editRequest(key, "run web v2")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)
		Eventually(planHash(key)).Should(Equal(hashOf(replanned, updateResources...)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		creation := len(gw.calls(key))
		awaitingDelete()

		By("listing the executed plan's teardown first, in its order, then the partial plan's new command")
		first := requestText(deleteRequests(key)[0])
		deployment := strings.Index(first, `"argv":["kubectl","delete","deployment","web"]`)
		service := strings.Index(first, `"argv":["kubectl","delete","service","web"]`)
		configMap := strings.Index(first, `"argv":["kubectl","delete","configmap","web"]`)
		Expect(deployment).To(BeNumerically(">", 0))
		Expect(service).To(BeNumerically(">", deployment))
		Expect(configMap).To(BeNumerically(">", service))
		Expect(strings.Count(first, `"argv":["kubectl","delete","service","web"]`)).To(Equal(1), "deduped")
		Expect(strings.Count(first, `"argv":["kubectl","delete","deployment","web"]`)).To(Equal(1), "deduped")
		Expect(first).To(ContainSubstring("come from plans that did not finish"))
		Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(union)))
		Expect(eventNotes(key, ReasonPartialTeardown)).To(ConsistOf(
			"The teardown includes the teardown commands of 1 plan that did not finish"))

		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(withoutQueries(gw.calls(key)[creation:])).To(Equal([]string{callLLM, beginPhase, "RunCommand:t3", runT2, runT1, endSucceeded}))
	})

	It("holds the release of an Intent that never executed while the audit log cannot be read", func() {
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		// The executed-plan read scans phase.begin, then phase.end; the partial-plan
		// read scans phase.begin again: fail only that second phase.begin read.
		previous := ""
		gw.updateScript(key, func(s *script) {
			s.queryHook = func(types []string) error {
				typ := strings.Join(types, ",")
				defer func() { previous = typ }()
				if typ == audit.TypePhaseBegin && previous == audit.TypePhaseEnd {
					return status.Error(codes.Unavailable, "audit log unavailable")
				}
				return nil
			}
		})
		deleteIntent(key)
		Consistently(gone(key)).Should(BeFalse())
		gw.updateScript(key, func(s *script) { s.queryHook = nil })
		setRetry(key, "poke")
		Eventually(gone(key)).Should(BeTrue())
	})

	It("tears down an executed Intent through an approved Delete plan", func() {
		executed(&script{}, turns(deletePlan))
		creation, uid := len(gw.calls(key)), getIntent(key).UID
		awaitingDelete()

		By("asking the model to order the executed plan's teardown commands")
		reqs := deleteRequests(key)
		Expect(reqs).To(HaveLen(1))
		first := requestText(reqs[0])
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","deployment","web"]`))
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","service","web"]`))
		Expect(strings.Index(first, `"deployment"`)).To(BeNumerically("<", strings.Index(first, `"service"`)),
			"the teardown is listed in resource order")

		in := getIntent(key)
		Expect(in.Status.Plan.Hash).To(Equal(hashOf(teardown)))
		Expect(in.Status.Plan.Resources).To(BeEmpty())
		Expect(readyReason(key)).To(Equal(ReasonAwaitingApproval))
		Expect(readyCondition(key).Message).To(ContainSubstring("for stage Delete"))
		Expect(in.Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))

		By("running the approved Delete plan, then releasing the Intent")
		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(withoutQueries(gw.calls(key)[creation:])).To(Equal([]string{callLLM, beginPhase, runT2, runT1, endSucceeded}))
		Expect(eventReasons(key, "Normal")).To(ContainElement(string(lastv1alpha1.PhaseTearingDown)))
		// The ManagedResources go by ownerReference garbage collection, which envtest does not run.
		Expect(metav1.IsControlledBy(getMR(key.Namespace, "web-0"), &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{UID: uid}})).
			To(BeTrue())
	})

	It("releases the Intent when the answer to the EndPhase that ended its teardown was lost", func() {
		executed(&script{}, turns(deletePlan))
		awaitingDelete()
		gw.updateScript(key, func(s *script) { s.loseEnd = true })
		approveDelete(key)

		Eventually(gone(key)).Should(BeTrue())
		calls := gw.calls(key)
		Expect(calls[len(calls)-2:]).To(Equal([]string{endSucceeded, endSucceeded}),
			"the retried EndPhase is refused as torn down, which means the teardown succeeded")
	})

	It("releases an executed Intent with no teardown commands, with a warning", func() {
		bare := []lastv1alpha1.ManagedResourceSpec{{Description: "a Deployment with no teardown"}}
		gw.setScript(key, &script{llm: lifecycle(turns(answer([]lastv1alpha1.Command{s1}, bare...)), turns(deletePlan))})
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		deleteIntent(key)
		Eventually(gone(key)).Should(BeTrue())
		Expect(deleteRequests(key)).To(BeEmpty(), "no Delete planning")
		Expect(eventReasons(key, "Warning")).To(ContainElement(ReasonNoTeardown))
	})

	It("falls back to the ManagedResources' teardown when the audit log lost the executed plan", func() {
		executed(&script{}, turns(deletePlan))
		gw.updateScript(key, func(s *script) { s.auditTruncated = true })
		awaitingDelete()

		first := requestText(deleteRequests(key)[0])
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","deployment","web"]`))
		Expect(first).To(ContainSubstring(`"argv":["kubectl","delete","service","web"]`))
		Expect(eventReasons(key, "Warning")).To(ContainElement(ReasonExecutedPlanNotFound))

		approveDelete(key)
		Eventually(gone(key)).Should(BeTrue())
	})

	DescribeTable("corrects a Delete plan that is not a permutation of the teardown",
		func(bad string, problem string) {
			executed(&script{}, turns(bad, deletePlan))
			awaitingDelete()

			Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(teardown)), "only the permutation is recorded")
			reqs := deleteRequests(key)
			Expect(reqs).To(HaveLen(2))
			Expect(lastUserMessage(key)).To(ContainSubstring(problem))
		},
		Entry("adds a command", answer([]lastv1alpha1.Command{t2, t1, argvStep("x", argKubectl, "delete", "namespace", "prod")}),
			`adds step 3 (id "x")`),
		Entry("drops a command", answer([]lastv1alpha1.Command{t2}), `leaves out teardown command 1 (id "t1")`),
		Entry("replaces a command", answer([]lastv1alpha1.Command{t2, argvStep("t1", argKubectl, "delete", "deployment", "api")}),
			`replaces teardown command 1 (id "t1") with step 2 (id "t1")`),
		Entry("keeps resources", answer(teardown, lastv1alpha1.ManagedResourceSpec{Description: "Deployment web"}),
			"a Delete plan has no resources"),
	)

	It("fails Delete planning after MaxAttempts but keeps the finalizer, and retries on the annotation", func() {
		reorderedWrongly := answer([]lastv1alpha1.Command{t2})
		bad := make([]string, agent.MaxAttempts)
		for i := range bad {
			bad[i] = reorderedWrongly
		}
		executed(&script{}, turns(bad...))
		deleteIntent(key)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		in := getIntent(key)
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageDelete))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(in.Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))
		Expect(in.Status.Plan).To(BeNil(), "no foreign plan is ever up for approval")

		By("planning the teardown again when the retry annotation changes")
		gw.updateScript(key, func(s *script) { s.llm = lifecycle(turns(createPlan), turns(deletePlan)) })
		setRetry(key, "1")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.ObservedRetry).To(Equal("1"))
		Expect(getIntent(key).Status.Plan.Hash).To(Equal(hashOf(teardown)))
	})

	It("tells the user how to plan the teardown again when its Delete plan is denied", func() {
		executed(&script{}, turns(deletePlan))
		awaitingDelete()
		in := getIntent(key)
		createApproval(key, lastv1alpha1.StageDelete, in.Status.Plan.Hash, lastv1alpha1.DecisionDeny)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonApprovalDenied))
		Expect(readyCondition(key).Message).To(ContainSubstring(lastv1alpha1.AnnotationRetry))
		Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))
	})

	It("keeps the finalizer and reports TeardownFailed when a teardown step fails", func() {
		executed(&script{exit: map[string]int32{"t1": 1}}, turns(deletePlan))
		awaitingDelete()
		approveDelete(key)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonTeardownFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("step t1 exited 1: boom"))
		Expect(readyCondition(key).Message).To(ContainSubstring(lastv1alpha1.AnnotationRetry))
		Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))
		Expect(gw.calls(key)).To(ContainElement(endFailed))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed), "only the annotation plans the teardown again")

		By("planning the teardown again when the retry annotation is set")
		gw.updateScript(key, func(s *script) { s.exit = nil })
		setRetry(key, "1")
		Eventually(gone(key)).Should(BeTrue())
		Expect(deleteRequests(key)).To(HaveLen(2))
		Expect(requestText(deleteRequests(key)[1])).To(ContainSubstring("step t1 exited 1: boom"), "the model hears why")
		Expect(countOf(gw.calls(key), runT1)).To(Equal(2))
		Expect(gw.calls(key)[len(gw.calls(key))-1]).To(Equal(endSucceeded))
	})

	DescribeTable("replans the Delete after the gateway closes its phase as interrupted", func(restart func(*script)) {
		executed(&script{}, turns(deletePlan))
		awaitingDelete()
		gw.updateScript(key, restart)
		approveDelete(key)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonTeardownFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("no open phase"))
		Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown))
		Expect(gw.openPhase(key)).To(BeEmpty())

		By("planning the teardown again when the retry annotation is set")
		setRetry(key, "after-restart")
		Eventually(gone(key)).Should(BeTrue())
		Expect(deleteRequests(key)).To(HaveLen(2))
		Expect(countOf(gw.calls(key), runT1)).To(Equal(2))
		Expect(gw.calls(key)[len(gw.calls(key))-1]).To(Equal(endSucceeded))
	},
		Entry("before a step runs", func(s *script) { s.interrupt = map[string]bool{"t1": true} }),
		Entry("before the phase ends", func(s *script) {
			s.endErrs = []error{status.Error(codes.FailedPrecondition, "no open phase for this plan")}
		}),
	)

	It("aborts a phase left open when the Intent is deleted", func() {
		gw.setScript(key, &script{llm: turns(answer([]lastv1alpha1.Command{s1}))})
		createIntent(key, "run web")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		subject := getIntent(key).Status.Plan.Hash

		By("holding the deletion while the audit log cannot be read")
		gw.updateScript(key, func(s *script) { s.queryErr = status.Error(codes.Unavailable, "audit log unavailable") })
		deleteIntent(key)
		Consistently(gone(key)).Should(BeFalse())
		Eventually(func() []string { return eventNotes(key, ReasonTeardownBlocked) }).Should(
			ContainElement(ContainSubstring("audit log unavailable")), "a stuck terminating Intent says why")

		By("finding the Create phase an earlier operator process opened")
		gw.setOpen(key, subject)
		executingAt(key, ReasonPhaseOpen)
		Eventually(func() string { return gw.openPhase(key) }).Should(BeEmpty())
		ends := gw.auditOf(key, "phase.end")
		Expect(ends).NotTo(BeEmpty())
		Expect(string(ends[len(ends)-1].GetPayload())).To(ContainSubstring("being deleted"))
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseExecuting), "the deletion, not execute, ended the phase")
		Expect(getIntent(key).Finalizers).To(ContainElement(lastv1alpha1.FinalizerTeardown), "never released on an error")

		By("releasing it once the audit log shows nothing executed")
		gw.updateScript(key, func(s *script) { s.queryErr = nil })
		setRetry(key, "poke") // any metadata change wakes the controller
		Eventually(gone(key)).Should(BeTrue())
		Expect(gw.calls(key)).NotTo(ContainElement(beginPhase))
	})
})
