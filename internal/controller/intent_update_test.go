package controller

import (
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// revisions answers an edited request (see revised) with edited, and any other
// request with original.
func revisions(original, edited string) llmFunc {
	return func(req *gatewayv1.CallLLMRequest) (string, error) {
		if revised(req) {
			return edited, nil
		}
		return original, nil
	}
}

var _ = Describe("Intent updates", func() {
	var key types.NamespacedName
	probe := func(id string) lastv1alpha1.Probe {
		return lastv1alpha1.Probe{ID: id, Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{trueCmd}}}, Assert: trueCmd}
	}
	web := lastv1alpha1.ManagedResourceSpec{Description: "web server", Probes: []lastv1alpha1.Probe{probe("p1")}}
	cache := lastv1alpha1.ManagedResourceSpec{Description: "cache", Probes: []lastv1alpha1.Probe{probe("p2")}}
	webV2 := lastv1alpha1.ManagedResourceSpec{Description: "web server v2", Probes: []lastv1alpha1.Probe{probe("p1")}}
	createSteps := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web.yaml")}
	updateSteps := []lastv1alpha1.Command{argvStep("s2", argKubectl, "delete", "configmap", "cache")}
	createHash, updateHash := hashOf(createSteps, web, cache), hashOf(updateSteps, webV2)

	// plans answers the edited request with the Update plan, which drops the
	// cache, and the first request with the Create plan.
	plans := revisions(answer(createSteps, web, cache), answer(updateSteps, webV2))

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// ready scripts the gateway (with plans unless s says otherwise) and runs
	// the Create plan until the Intent is Ready.
	ready := func(s *script) {
		if s.llm == nil {
			s.llm = plans
		}
		gw.setScript(key, s)
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
	}

	// editToUpdate edits the request and waits for the Update plan.
	editToUpdate := func() {
		editRequest(key, "provision v2")
		Eventually(planHash(key)).Should(Equal(updateHash))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
	}

	count := func(call string) int {
		n := 0
		for _, c := range gw.calls(key) {
			if c == call {
				n++
			}
		}
		return n
	}

	DescribeTable("plans an Update when the spec is edited after a plan executed",
		func(reach func()) {
			reach()
			Expect(gw.executedOf(key)).To(Equal(createHash))

			editToUpdate()
			in := getIntent(key)
			Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
			Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
			Expect(readyCondition(key).Message).To(HaveSuffix("for stage Update"))
			first := requestText(gw.lastRequest(key))
			Expect(first).To(ContainSubstring("This Intent already executed this plan (do not repeat it as your answer):\n"))
			Expect(first).To(ContainSubstring(`"description":"cache"`))
			Expect(first).To(ContainSubstring("Plan an Update"))
		},
		Entry("from Ready", func() { ready(&script{}) }),
		Entry("from Drifted, while its remediation is planned", func() {
			// The model is unreachable for the Remediate, so it stays in Planning.
			ready(&script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
				if strings.Contains(requestText(req), "has drifted") {
					return "", status.Error(codes.Unavailable, "provider overloaded")
				}
				return plans(req)
			}})
			gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
			setOutOfSync(key.Namespace, "web-0")
			Eventually(func() lastv1alpha1.Stage { return getIntent(key).Status.Stage }).Should(Equal(lastv1alpha1.StageRemediate))
		}),
		Entry("from Failed after the plan executed", func() {
			gw.setScript(key, &script{llm: plans, failing: map[string]bool{"p2": true}})
			createIntent(key, "provision")
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionApprove)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
			Expect(readyReason(key)).To(Equal(ReasonVerificationFailed))
		}),
	)

	It("keeps stage Create for an edit before any plan executed", func() {
		gw.setScript(key, &script{llm: plans})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionDeny)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))

		editToUpdate()
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageCreate))
		Expect(readyCondition(key).Message).To(HaveSuffix("for stage Create"))
		Expect(requestText(gw.lastRequest(key))).NotTo(ContainSubstring("already executed"))
	})

	It("runs an approved Update and makes it the executed plan", func() {
		ready(&script{})
		stale := getMR(key.Namespace, web1)
		Expect(stale.Spec.Description).To(Equal("cache"))

		editToUpdate()
		n := len(gw.calls(key))
		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.calls(key)[n:]).To(Equal([]string{beginPhase, runS2, endSucceeded, runProbes}))
		Expect(gw.executedOf(key)).To(Equal(updateHash))
		_, hash, found, err := agent.ExecutedPlan(ctx, gw, intentRef(getIntent(key)))
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(hash).To(Equal(updateHash))

		mr := getMR(key.Namespace, "web-0")
		Expect(mr.Spec.Description).To(Equal("web server v2"))
		Expect(meta.IsStatusConditionTrue(mr.Status.Conditions, CondInSync)).To(BeTrue())
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: web1}, &lastv1alpha1.ManagedResource{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the gateway deleted the ManagedResource the Update dropped")
		deleted := gw.auditOf(key, audit.TypeManagedResourceDeleted)
		Expect(deleted).To(HaveLen(1))
		Expect(deleted[0].GetStage()).To(Equal(string(lastv1alpha1.StageUpdate)))
		Expect(deleted[0].GetSubjectHash()).To(Equal(updateHash))
		Expect(deleted[0].GetPayload()).To(MatchJSON(`{"managedResource":"web-1","uid":"` + string(stale.UID) + `"}`))
	})

	It("replans an Update after a failed step, still in stage Update", func() {
		fixed := []lastv1alpha1.Command{argvStep("s3", argKubectl, "delete", "configmap", "cache", "--ignore-not-found")}
		ready(&script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "step s2 exited 1") {
				return answer(fixed, webV2), nil
			}
			return plans(req)
		}})
		editToUpdate()
		gw.updateScript(key, func(s *script) { s.exit = map[string]int32{"s2": 1} })
		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)

		Eventually(planHash(key)).Should(Equal(hashOf(fixed, webV2)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
		first := requestText(gw.lastRequest(key))
		Expect(first).To(ContainSubstring("This Intent already executed this plan (do not repeat it as your answer):\n"))
		Expect(first).To(ContainSubstring("The previous plan failed and was not completed:\nstep s2 exited 1: boom"))
		Expect(gw.executedOf(key)).To(Equal(createHash), "a failed Update leaves the executed plan as it was")

		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.executedOf(key)).To(Equal(hashOf(fixed, webV2)))
	})

	DescribeTable("replans a failed step in its own stage when the audit log cannot be read just after",
		func(stage lastv1alpha1.Stage) {
			fixed := []lastv1alpha1.Command{argvStep("s3", argKubectl, "delete", "configmap", "cache", "--ignore-not-found")}
			failing := "s2"
			replanned := hashOf(fixed, webV2)
			llm := func(req *gatewayv1.CallLLMRequest) (string, error) {
				if strings.Contains(requestText(req), "step "+failing+" exited 1") {
					return answer(fixed, webV2), nil
				}
				return plans(req)
			}
			if stage == lastv1alpha1.StageUpdate {
				ready(&script{llm: llm})
				editToUpdate()
			} else {
				failing, llm = "s1", func(req *gatewayv1.CallLLMRequest) (string, error) {
					if strings.Contains(requestText(req), "step s1 exited 1") {
						return answer(fixed, webV2), nil
					}
					return answer(createSteps, web, cache), nil
				}
				gw.setScript(key, &script{llm: llm})
				createIntent(key, "provision")
				Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			}
			// The first audit read after the failed phase ends fails once.
			armed := false
			gw.updateScript(key, func(s *script) {
				s.exit = map[string]int32{failing: 1}
				s.onEnd = func(succeeded bool) { armed = armed || !succeeded }
				s.queryHook = func([]string) error {
					if armed {
						armed = false
						return status.Error(codes.Unavailable, "index unreachable")
					}
					return nil
				}
			})
			approveAs(key, stage, lastv1alpha1.DecisionApprove)

			Eventually(planHash(key)).Should(Equal(replanned))
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			Expect(getIntent(key).Status.Stage).To(Equal(stage))
			Expect(requestText(gw.lastRequest(key))).To(ContainSubstring(
				"The previous plan failed and was not completed:\nstep " + failing + " exited 1: boom"))
		},
		Entry("Create", lastv1alpha1.StageCreate),
		Entry("Update", lastv1alpha1.StageUpdate),
	)

	It("keeps stage Update across a discovery command", func() {
		look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, "configmaps"}}}
		ready(&script{stdout: map[string]string{plan.DiscoveryStepID: "cache\n"}, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if revised(req) && !strings.Contains(conversationText(req), "The command exited 0.") {
				return commandTurn(look), nil
			}
			return plans(req)
		}})

		editRequest(key, "provision v2")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		in := getIntent(key)
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageUpdate), "status.stage keeps the session's stage (R4)")
		Expect(in.Status.PendingCommand).NotTo(BeNil())
		subject := in.Status.PendingCommand.Hash
		Expect(readyCondition(key).Message).To(HaveSuffix("for stage Discovery"))

		// An approval of the command for the session's stage authorizes nothing.
		createApproval(key, lastv1alpha1.StageUpdate, subject, lastv1alpha1.DecisionApprove)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		Expect(gw.calls(key)).NotTo(ContainElement(beginDiscovery))

		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")
		Eventually(planHash(key)).Should(Equal(updateHash))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
		Expect(readyCondition(key).Message).To(HaveSuffix("for stage Update"))
		Expect(gw.calls(key)).To(ContainElements(beginDiscovery, runDiscoveryStep))

		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(gw.executedOf(key)).To(Equal(updateHash))
	})

	It("an Approve for stage Create does not run an Update plan", func() {
		ready(&script{})
		editToUpdate()
		createApproval(key, lastv1alpha1.StageCreate, updateHash, lastv1alpha1.DecisionApprove)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(count(beginPhase)).To(Equal(1), "only the Create phase opened")
	})

	It("plans again when the spec is edited while an Update awaits approval", func() {
		v3 := []lastv1alpha1.Command{argvStep("s4", argKubectl, "delete", "configmap", "cache", "--wait")}
		ready(&script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "provision v3") {
				return answer(v3, webV2), nil
			}
			return plans(req)
		}})
		editToUpdate()

		editRequest(key, "provision v3")
		Eventually(planHash(key)).Should(Equal(hashOf(v3, webV2)))
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		in := getIntent(key)
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageUpdate))
		Expect(in.Status.ObservedGeneration).To(Equal(in.Generation))
	})

	It("waits to choose the stage while the gateway cannot read the audit log", func() {
		gw.setScript(key, &script{llm: plans, queryErr: status.Error(codes.Unavailable, "index unreachable")})
		createIntent(key, "provision")
		Consistently(phase(key), "2s").Should(BeEmpty())
		Expect(gw.calls(key)).NotTo(ContainElement(callLLM))

		gw.updateScript(key, func(s *script) { s.queryErr = nil })
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageCreate))
	})

	It("chooses the stage again when the executed plan leaves the audit log before the session starts", func() {
		ready(&script{})
		// The stage choice reads phase.begin, then phase.end; the index is
		// truncated for every read after that phase.end read.
		chosen := false
		gw.updateScript(key, func(s *script) {
			s.queryHook = func(kinds []string) error {
				s.auditTruncated = chosen
				chosen = chosen || slices.Equal(kinds, []string{audit.TypePhaseEnd})
				return nil
			}
		})

		editToUpdate()
		Expect(getIntent(key).Status.Stage).To(Equal(lastv1alpha1.StageCreate))
		Expect(requestText(gw.lastRequest(key))).NotTo(ContainSubstring("already executed"))
	})

	It("is refused an Update by the gateway when nothing executed", func() {
		gw.setScript(key, &script{llm: plans})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		// As if the operator had misjudged the stage.
		setStatus(key, func(in *lastv1alpha1.Intent) { in.Status.Stage = lastv1alpha1.StageUpdate })
		approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonExecutionFailed))
		Expect(readyCondition(key).Message).To(ContainSubstring("no executed plan to update"))
	})
	DescribeTable("drops the first resource and keeps the second, whose output Secret moves to web-0",
		func(withOutputs bool) {
			const secretName = "web-outputs"
			webOut := web
			webOut.OutputsSecretRef, webOut.Outputs = &lastv1alpha1.LocalRef{Name: secretName}, map[string]string{"url": s1Stdout}
			kept := webOut
			kept.Outputs = nil
			if withOutputs {
				kept.Outputs = map[string]string{"url": "${steps.s2.stdout}"}
			}
			ready(&script{llm: revisions(answer(createSteps, cache, webOut), answer(updateSteps, kept))})
			secret := func() *corev1.Secret {
				var s corev1.Secret
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: secretName}, &s)).To(Succeed())
				return &s
			}
			Expect(metav1.IsControlledBy(secret(), getMR(key.Namespace, web1))).To(BeTrue())

			editRequest(key, "provision v2")
			Eventually(planHash(key)).Should(Equal(hashOf(updateSteps, kept)))
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			n := len(gw.calls(key))
			approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionApprove)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))

			web0 := getMR(key.Namespace, "web-0")
			Expect(web0.Spec.OutputsSecretRef).To(Equal(&lastv1alpha1.LocalRef{Name: secretName}))
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: web1}, &lastv1alpha1.ManagedResource{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			s := secret()
			Expect(metav1.IsControlledBy(s, web0)).To(BeTrue(), "the Secret moved to web-0")
			if withOutputs {
				Expect(gw.calls(key)[n:]).To(ContainElement("WriteOutputSecret:web-0"))
				Expect(string(s.Data["url"])).To(Equal("${steps.s2.stdout}"))
			} else {
				Expect(gw.calls(key)[n:]).NotTo(ContainElement("WriteOutputSecret:web-0"))
				Expect(string(s.Data["url"])).To(Equal(s1Stdout), "the Secret is kept as it is")
			}
		},
		Entry("with outputs from the Update's steps", true),
		Entry("without outputs", false),
	)
})
