package controller

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// remediating reports whether a CallLLM request belongs to a Remediate planning session.
func remediating(req *gatewayv1.CallLLMRequest) bool {
	return strings.Contains(requestText(req), "has drifted from its executed plan")
}

// phaseEnds returns the reasons of the Intent's phase.end entries, oldest first.
func phaseEnds(key types.NamespacedName) []string {
	ends := gw.auditOf(key, audit.TypePhaseEnd)
	out := make([]string, 0, len(ends))
	for _, e := range ends {
		var p struct {
			Reason string `json:"reason"`
		}
		Expect(json.Unmarshal(e.GetPayload(), &p)).To(Succeed())
		out = append(out, p.Reason)
	}
	return out
}

// remediateBegins counts the Intent's phase.begin entries of stage Remediate.
func remediateBegins(key types.NamespacedName) int {
	n := 0
	for _, e := range gw.auditOf(key, audit.TypePhaseBegin) {
		if e.GetStage() == string(lastv1alpha1.StageRemediate) {
			n++
		}
	}
	return n
}

var _ = Describe("Intent remediation", func() {
	var key types.NamespacedName
	const assert = "result.json.replicas == 2"
	const desc = "nginx Deployment"
	web := lastv1alpha1.ManagedResourceSpec{Description: desc,
		Probes: []lastv1alpha1.Probe{{ID: "p1", Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{
			Argv: []string{argKubectl, argGet, "deployment", intentName, "-o", "json"}}}, Assert: assert}},
		OutputsSecretRef: &lastv1alpha1.LocalRef{Name: "nginx-out"}, Outputs: map[string]string{"applied": s1Stdout},
		Teardown: []lastv1alpha1.Command{argvStep("t1", argKubectl, "delete", "deployment", "web")}}
	createSteps := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web.yaml")}
	fixSteps := []lastv1alpha1.Command{argvStep("r1", argKubectl, "scale", "deployment/web", "--replicas=2")}
	updateSteps := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web-v2.yaml")}
	createHash, updateHash := hashOf(createSteps, web), hashOf(updateSteps, web)
	runR1 := "RunCommand:r1"

	// lifecycleOf answers a Remediate session with remediate (nil: the fix
	// steps), an edited request with the Update plan, and anything else with
	// the Create plan.
	lifecycleOf := func(remediate llmFunc) llmFunc {
		return func(req *gatewayv1.CallLLMRequest) (string, error) {
			switch {
			case remediating(req) && remediate == nil:
				return answer(fixSteps), nil
			case remediating(req):
				return remediate(req)
			case revised(req):
				return answer(updateSteps, web), nil
			}
			return answer(createSteps, web), nil
		}
	}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// ready scripts the gateway (a remediation answers fix unless s says
	// otherwise) and runs the Create plan until the Intent is Ready.
	ready := func(s *script) {
		if s.llm == nil {
			s.llm = lifecycleOf(nil)
		}
		s.stdout = map[string]string{"s1": intentName}
		gw.setScript(key, s)
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
	}

	// drift makes p1 fail with its output and marks web-0 out of sync, as a
	// scheduled probe run that found the drift does.
	drift := func() {
		gw.updateScript(key, func(s *script) {
			s.failing = map[string]bool{"p1": true}
			s.probeOut = map[string]string{"p1": `{"replicas":1}`}
		})
		setOutOfSync(key.Namespace, "web-0")
	}
	// repaired makes every probe pass again, as a remediation does.
	repaired := func() { gw.updateScript(key, func(s *script) { s.failing = nil }) }

	// awaitingRemediate waits for a Remediate plan other than not and returns it.
	awaitingRemediate := func(not string) *lastv1alpha1.Plan {
		Eventually(func() bool {
			in := getIntent(key)
			return in.Status.Stage == lastv1alpha1.StageRemediate && in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval &&
				in.Status.Plan != nil && in.Status.Plan.Hash != not
		}).Should(BeTrue())
		return getIntent(key).Status.Plan
	}

	remediations := func() int32 { return getIntent(key).Status.Remediations }

	// remediate runs one drift episode to Ready and returns its plan's subject.
	remediate := func(previous string) string {
		drift()
		p := awaitingRemediate(previous)
		repaired()
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
		Eventually(func() bool {
			in := getIntent(key)
			return in.Status.Phase == lastv1alpha1.PhaseReady && in.Status.Plan.Hash == p.Hash
		}).Should(BeTrue())
		return p.Hash
	}

	It("remediates drift through an approved Remediate plan", func() {
		ready(&script{})
		drift()
		p := awaitingRemediate("")

		first := requestText(gw.lastRequest(key))
		Expect(first).To(ContainSubstring("- " + desc + ", probe p1, assert: " + assert))
		Expect(first).To(ContainSubstring("The probe exited 1.\nstdout:\n{\"replicas\":1}\n"))
		Expect(first).To(ContainSubstring(`"description":"`+desc+`"`), "the executed plan, from the audit log")

		same, err := plan.SameResources(p.Resources, []lastv1alpha1.ManagedResourceSpec{web})
		Expect(err).NotTo(HaveOccurred())
		Expect(same).To(BeTrue(), "the executed plan's resources are copied in (R2)")
		Expect(plan.ValidNonce(p.Nonce)).To(BeTrue(), "a Remediate carries a nonce (R3)")
		h, err := plan.Hash(plan.FromStatus(p))
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Hash).To(Equal(h))
		Expect(p.Steps).To(Equal(fixSteps))
		Expect(readyCondition(key).Message).To(HaveSuffix("for stage Remediate"))
		Expect(remediations()).To(Equal(int32(1)))

		n := len(gw.calls(key))
		repaired()
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.calls(key)[n:]).To(Equal([]string{beginPhase, runR1, endSucceeded, runProbes}),
			"no ManagedResource or output Secret is written")
		Expect(eventReasons(key, corev1.EventTypeNormal)).To(ContainElements(
			string(lastv1alpha1.PhaseRemediating), string(lastv1alpha1.PhaseVerifying)))
		Expect(gw.executedOf(key)).To(Equal(createHash), "a Remediate never replaces the executed plan")
		Expect(remediateBegins(key)).To(Equal(1))
		Expect(remediations()).To(Equal(int32(1)))
		Expect(meta.IsStatusConditionTrue(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)).To(BeTrue())
	})

	It("returns to Ready when the confirming probe run passes", func() {
		ready(&script{})
		setOutOfSync(key.Namespace, "web-0") // the gateway's probes pass

		Eventually(func() []string { return eventReasons(key, corev1.EventTypeWarning) }).Should(ContainElement(ReasonDriftCleared))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(readyReason(key)).To(Equal(ReasonDriftCleared))
		Expect(countOf(gw.calls(key), callLLM)).To(Equal(1), "no Remediate is planned")
		Expect(planHash(key)()).To(Equal(createHash))
		Expect(remediations()).To(BeZero())
		Expect(meta.IsStatusConditionTrue(getMR(key.Namespace, "web-0").Status.Conditions, CondInSync)).To(BeTrue())
	})

	DescribeTable("corrects a Remediate answer with resources or questions",
		func(bad, correction string) {
			ready(&script{llm: lifecycleOf(func(*gatewayv1.CallLLMRequest) (string, error) { return bad, nil })})
			drift()
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
			Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
			Expect(countOf(gw.calls(key), callLLM)).To(Equal(1 + agent.MaxAttempts))
			Expect(lastUserMessage(key)).To(ContainSubstring(correction))
			Expect(countOf(eventReasons(key, corev1.EventTypeNormal), string(lastv1alpha1.PhaseAwaitingApproval))).To(Equal(1),
				"only the Create plan ever awaited approval")
			Expect(remediateBegins(key)).To(BeZero())
		},
		Entry("resources", answer(fixSteps, lastv1alpha1.ManagedResourceSpec{Description: desc}), `A Remediate plan answers "resources": []; what is managed does not change`),
		Entry("questions", questionsTurn(lastv1alpha1.Question{ID: "q1", Text: "Scale up?"}),
			"A Remediate cannot ask questions; plan from the probe results, or run a discovery command"),
	)

	It("fails with PlanningFailed when no executed plan is found", func() {
		ready(&script{})
		gw.updateScript(key, func(s *script) { s.auditTruncated = true })
		drift()
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal("no executed plan found to remediate"))
		Expect(countOf(gw.calls(key), callLLM)).To(Equal(1))
	})

	It("fails with RemediationFailed at the fourth consecutive drift", func() {
		ready(&script{})
		subject := ""
		for i := int32(1); i <= 3; i++ {
			subject = remediate(subject)
			Expect(remediations()).To(Equal(i))
		}
		calls := countOf(gw.calls(key), callLLM)
		drift()
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonRemediationFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("3 consecutive remediations"))
		Expect(countOf(gw.calls(key), callLLM)).To(Equal(calls), "nothing is planned")
	})

	// unrepaired approves the Remediate plan other than previous without
	// repairing the drift, so its Verifying run fails, and returns its subject.
	unrepaired := func(previous string) string {
		p := awaitingRemediate(previous)
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
		return p.Hash
	}

	It("counts drift that persists after a Remediate as drift, and remediates again", func() {
		ready(&script{})
		drift()
		first := unrepaired("")

		Eventually(func() []string { return eventNotes(key, string(lastv1alpha1.PhaseDrifted)) }).Should(
			ContainElement("Probes failed after the remediation: p1"))
		second := awaitingRemediate(first)
		Expect(remediations()).To(Equal(int32(2)))
		Expect(eventReasons(key, corev1.EventTypeWarning)).NotTo(ContainElement(string(lastv1alpha1.PhaseFailed)),
			"not VerificationFailed")

		repaired()
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
		Eventually(func() bool {
			in := getIntent(key)
			return in.Status.Phase == lastv1alpha1.PhaseReady && in.Status.Plan.Hash == second.Hash
		}).Should(BeTrue())
		Expect(remediations()).To(Equal(int32(2)))
	})

	It("fails with RemediationFailed when drift persists after the third Remediate", func() {
		ready(&script{})
		drift()
		subject := ""
		for range 3 {
			subject = unrepaired(subject)
		}
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonRemediationFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("3 consecutive remediations"))
		Expect(remediateBegins(key)).To(Equal(3))
	})

	It("resets remediations when a spec edit plans an Update, so the next drift is remediated", func() {
		ready(&script{})
		subject := ""
		for range 3 {
			subject = remediate(subject)
		}
		drift()
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonRemediationFailed))

		editRequest(key, "provision v2")
		Eventually(planHash(key)).Should(Equal(updateHash))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
		Expect(remediations()).To(BeZero(), "the spec edit started a new session")
		repaired()
		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

		drift()
		awaitingRemediate(subject)
		Expect(remediations()).To(Equal(int32(1)))
	})

	It("keeps remediations when a failed step replans in its own stage", func() {
		ready(&script{})
		remediate("")
		editRequest(key, "provision v2")
		Eventually(planHash(key)).Should(Equal(updateHash))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		// As if a drift episode counted while the Update awaited approval.
		setStatus(key, func(in *lastv1alpha1.Intent) { in.Status.Remediations = 2 })
		gw.updateScript(key, func(s *script) { s.exit = map[string]int32{"s1": 1} })
		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)

		Eventually(func() string { return replanFeedback(getIntent(key)) }).Should(ContainSubstring("step s1 exited 1"))
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
		Expect(remediations()).To(Equal(int32(2)), "a replan is no spec edit")
	})

	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, "deployments"}}}
	// discovering answers a Remediate session with a discovery command, then,
	// once its result is in, with the fix steps.
	discovering := lifecycleOf(func(req *gatewayv1.CallLLMRequest) (string, error) {
		if strings.Contains(conversationText(req), "The command exited 0.") {
			return answer(fixSteps), nil
		}
		return commandTurn(look), nil
	})

	It("runs a discovery command in a Remediate session and records a Remediate plan after it", func() {
		ready(&script{llm: discovering})
		drift()
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		in := getIntent(key)
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageRemediate))
		Expect(readyCondition(key).Message).To(HaveSuffix("for stage Discovery"))

		approveCommand(key, in.Status.PendingCommand.Hash, lastv1alpha1.DecisionApprove, "")
		p := awaitingRemediate("")
		Expect(gw.calls(key)).To(ContainElements(beginDiscovery, runDiscoveryStep))
		Expect(p.Steps).To(Equal(fixSteps))
		Expect(plan.ValidNonce(p.Nonce)).To(BeTrue())
		same, err := plan.SameResources(p.Resources, []lastv1alpha1.ManagedResourceSpec{web})
		Expect(err).NotTo(HaveOccurred())
		Expect(same).To(BeTrue())
		Expect(readyCondition(key).Message).To(HaveSuffix("for stage Remediate"))
	})

	It("an approval of the Remediate plan for another stage runs nothing", func() {
		ready(&script{})
		drift()
		p := awaitingRemediate("")
		createApproval(key, lastv1alpha1.StageCreate, p.Hash, lastv1alpha1.DecisionApprove)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(remediateBegins(key)).To(BeZero())
		Expect(gw.calls(key)).NotTo(ContainElement(runR1))
	})

	It("returns to Ready when the drift cleared before a reloaded Remediate session started over", func() {
		ready(&script{llm: discovering})
		drift()
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		subject := getIntent(key).Status.PendingCommand.Hash
		Expect(getIntent(key).Status.PlanningSessionSeq).NotTo(BeZero())

		// The operator restarts, and the audit log no longer gives back the session.
		Eventually(func() bool {
			intentReconciler.sessions.mu.Lock()
			defer intentReconciler.sessions.mu.Unlock()
			_, ok := intentReconciler.sessions.m[key]
			return ok
		}).Should(BeTrue())
		intentReconciler.sessions.drop(key)
		repaired()
		gw.updateScript(key, func(s *script) {
			s.queryHook = func(kinds []string) error {
				if slices.Equal(kinds, []string{audit.TypeLLMRequest}) {
					return status.Error(codes.NotFound, "the index no longer holds the session")
				}
				return nil
			}
		})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(readyReason(key)).To(Equal(ReasonDriftCleared))
		Expect(eventReasons(key, corev1.EventTypeWarning)).To(ContainElement(ReasonDriftCleared))
	})

	It("fails at the cap even when a conflict retry finds the count already there", func() {
		in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: intentName, Namespace: "capped", Generation: 1},
			Spec: lastv1alpha1.IntentSpec{Request: "provision"}}
		fc := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(in).WithStatusSubresource(in).Build()
		stored := func() *lastv1alpha1.Intent {
			var got lastv1alpha1.Intent
			Expect(fc.Get(ctx, client.ObjectKeyFromObject(in), &got)).To(Succeed())
			return &got
		}
		drifted := stored()
		drifted.Status = lastv1alpha1.IntentStatus{Phase: lastv1alpha1.PhaseDrifted, ObservedGeneration: drifted.Generation,
			Stage: lastv1alpha1.StageCreate, Remediations: 2}
		Expect(fc.Status().Update(ctx, drifted)).To(Succeed())
		stale := stored() // seen with 2 remediations
		// Another remediation was counted meanwhile.
		current := stored()
		current.Status.Remediations = 3
		Expect(fc.Status().Update(ctx, current)).To(Succeed())

		r := &IntentReconciler{Client: fc, Reader: fc, Scheme: scheme.Scheme, Gateway: gw,
			Recorder: events.NewFakeRecorder(10), RetryDelay: time.Millisecond}
		_, err := r.startRemediate(ctx, stale)
		Expect(err).NotTo(HaveOccurred())
		got := stored()
		Expect(got.Status.Remediations).To(Equal(int32(3)))
		Expect(got.Status.Phase).To(Equal(lastv1alpha1.PhaseFailed))
		Expect(meta.FindStatusCondition(got.Status.Conditions, CondReady).Reason).To(Equal(ReasonRemediationFailed))
	})

	It("resets remediations after a fully passing scheduled probe run", func() {
		ready(&script{})
		remediate("")
		Expect(remediations()).To(Equal(int32(1)))
		Consistently(remediations).Should(Equal(int32(1)), "the Verifying run does not reset it")

		mr := &ManagedResourceReconciler{Client: k8sClient, Reader: k8sClient, Scheme: scheme.Scheme, Gateway: gw,
			Now: func() time.Time { return time.Now().Add(10 * time.Minute) }}
		_, err := mr.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(getMR(key.Namespace, "web-0").Status.LastProbeTime.Time).To(BeTemporally(">", time.Now().Add(time.Minute)),
			"the scheduled run probed the Remediate's executed plan")
		Eventually(remediations).Should(BeZero())
		Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseReady))
	})

	It("replans a failed Remediate step in stage Remediate without counting it", func() {
		fixed := []lastv1alpha1.Command{argvStep("r2", argKubectl, "rollout", "restart", "deployment/web")}
		ready(&script{llm: lifecycleOf(func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "step r1 exited 1") {
				return answer(fixed), nil
			}
			return answer(fixSteps), nil
		})})
		drift()
		first := awaitingRemediate("")
		gw.updateScript(key, func(s *script) { s.exit = map[string]int32{"r1": 1} })
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)

		p := awaitingRemediate(first.Hash)
		Expect(p.Steps).To(Equal(fixed))
		Expect(requestText(gw.lastRequest(key))).To(ContainSubstring(
			"The previous remediation failed and was not completed:\nstep r1 exited 1: boom"))
		Expect(remediations()).To(Equal(int32(1)), "a replan is the same drift episode")
		Expect(gw.executedOf(key)).To(Equal(createHash))
	})

	DescribeTable("abandons a remediation on a spec edit and plans an Update",
		func(reach func() string) {
			ready(&script{})
			drift()
			subject := reach()
			editRequest(key, "provision v2")

			Eventually(planHash(key)).Should(Equal(updateHash))
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			in := getIntent(key)
			Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
			Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
			Expect(remediateBegins(key)).To(BeZero())
			Expect(gw.calls(key)).NotTo(ContainElement(runR1))
			Expect(gw.openPhase(key)).To(BeEmpty())
			if subject != "" {
				Expect(phaseEnds(key)).To(ContainElement("Aborted: the spec changed"))
			}
		},
		Entry("in Planning", func() string {
			gw.updateScript(key, func(s *script) {
				s.llm = lifecycleOf(func(*gatewayv1.CallLLMRequest) (string, error) {
					return "", status.Error(codes.Unavailable, "provider overloaded")
				})
			})
			Eventually(func() bool {
				return getIntent(key).Status.Stage == lastv1alpha1.StageRemediate && countOf(gw.calls(key), callLLM) > 1
			}).Should(BeTrue())
			Expect(phase(key)()).To(Equal(lastv1alpha1.PhasePlanning))
			return ""
		}),
		Entry("in AwaitingDiscoveryApproval", func() string {
			look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, "deployment", "web"}}}
			gw.updateScript(key, func(s *script) {
				s.llm = lifecycleOf(func(*gatewayv1.CallLLMRequest) (string, error) { return commandTurn(look), nil })
			})
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
			Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageRemediate), "a Remediate may run discovery commands")
			return ""
		}),
		Entry("in AwaitingApproval", func() string {
			awaitingRemediate("")
			return ""
		}),
		Entry("in Remediating, with a phase an earlier attempt opened", func() string {
			p := awaitingRemediate("")
			gw.updateScript(key, func(s *script) { s.beginErr = status.Error(codes.Unavailable, "gateway restarting") })
			approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseRemediating))
			gw.setOpen(key, p.Hash)
			return p.Hash
		}),
	)

	It("a deletion during Remediating aborts the phase and starts Delete", func() {
		ready(&script{})
		drift()
		p := awaitingRemediate("")
		gw.updateScript(key, func(s *script) { s.beginErr = status.Error(codes.Unavailable, "gateway restarting") })
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseRemediating))
		gw.setOpen(key, p.Hash)

		deleteIntent(key)
		Eventually(func() lastv1alpha1.Stage { return getIntent(key).Status.Stage }).Should(Equal(lastv1alpha1.StageDelete))
		Expect(gw.openPhase(key)).To(BeEmpty())
		Expect(phaseEnds(key)).To(ContainElement("Aborted: the Intent is being deleted"))
		Expect(gw.calls(key)).NotTo(ContainElement(runR1))
	})

	It("the same drift twice gets two subjects and two approvals", func() {
		ready(&script{})
		firstSubject := remediate("")

		calls := len(gw.calls(key))
		drift()
		p := awaitingRemediate(firstSubject)
		Expect(p.Steps).To(Equal(fixSteps), "the same repair")
		Expect(p.Hash).NotTo(Equal(firstSubject))
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval), "the first approval authorizes nothing")
		Expect(gw.calls(key)[calls:]).NotTo(ContainElement(beginPhase))

		repaired()
		approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(remediateBegins(key)).To(Equal(2))
		Expect(remediations()).To(Equal(int32(2)))
	})

	DescribeTable("a gateway restart mid-Remediate fails the Intent as interrupted unless the audit log shows the phase succeeded",
		func(restart func(*script), succeeded bool) {
			ready(&script{})
			drift()
			awaitingRemediate("")
			repaired()
			gw.updateScript(key, restart)
			approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionApprove)

			if succeeded {
				Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
				Expect(gw.calls(key)).NotTo(ContainElement(endFailed))
				return
			}
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
			Expect(readyReason(key)).To(Equal(ReasonInterrupted))
			Expect(readyCondition(key).Message).To(HaveSuffix("; edit the Intent's spec to plan again"))
			Expect(gw.openPhase(key)).To(BeEmpty())
		},
		Entry("the phase succeeded and its answer was lost", func(s *script) { s.loseEnd = true }, true),
		Entry("the gateway closed the phase before it succeeded", func(s *script) {
			s.endErrs = []error{status.Error(codes.FailedPrecondition, "no open phase for this plan")}
		}, false),
	)
})
