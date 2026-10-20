package controller

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		retries int32
		want    time.Duration
	}{
		{-1, time.Minute}, // a hand-edited status: never shorter than the first delay
		{0, time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{5, 32 * time.Minute},
		{6, time.Hour}, // 64m, capped
		{7, time.Hour},
		{26, time.Hour}, // a minute shifted 27 times overflows int64
		{27, time.Hour},
		{63, time.Hour},
		{64, time.Hour},
		{math.MaxInt32, time.Hour},
	}
	for _, tc := range cases {
		if got := retryDelay(tc.retries); got != tc.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tc.retries, got, tc.want)
		}
	}
}

func TestFailedStage(t *testing.T) {
	cases := []struct {
		stage  lastv1alpha1.Stage
		reason string
		want   lastv1alpha1.Stage
	}{
		{lastv1alpha1.StageCreate, ReasonExecutionFailed, lastv1alpha1.StageCreate},
		{"", ReasonPlanningFailed, lastv1alpha1.StageCreate},
		{lastv1alpha1.StageDiscovery, ReasonPlanningFailed, lastv1alpha1.StageCreate},
		{lastv1alpha1.StageUpdate, ReasonApprovalDenied, lastv1alpha1.StageUpdate},
		{lastv1alpha1.StageDelete, ReasonTeardownFailed, lastv1alpha1.StageDelete},
		{lastv1alpha1.StageRemediate, ReasonExecutionFailed, lastv1alpha1.StageRemediate},
		// The cap fails the Intent before a Remediate session records its stage.
		{lastv1alpha1.StageCreate, ReasonRemediationFailed, lastv1alpha1.StageRemediate},
		{lastv1alpha1.StageUpdate, ReasonRemediationFailed, lastv1alpha1.StageRemediate},
	}
	for _, tc := range cases {
		in := &lastv1alpha1.Intent{}
		in.Status.Stage = tc.stage
		setPhase(in, lastv1alpha1.PhaseFailed, tc.reason, "it broke")
		if got := failedStage(in); got != tc.want {
			t.Errorf("failedStage(stage %q, reason %s) = %q, want %q", tc.stage, tc.reason, got, tc.want)
		}
	}
}

// pokeAnnotation is an annotation specs change only to wake the controller.
const pokeAnnotation = "test.last.moogie-labs.com/poke"

// poke changes the Intent's metadata, which wakes the controller, as the
// requeue after a backoff does in real time.
func poke(key types.NamespacedName) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		if in.Annotations == nil {
			in.Annotations = map[string]string{}
		}
		in.Annotations[pokeAnnotation] = time.Now().Format(time.RFC3339Nano)
		return k8sClient.Update(ctx, &in)
	})).To(Succeed())
}

var _ = Describe("Intent retries", func() {
	var key types.NamespacedName
	steps := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web.yaml")}
	stepsAgain := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web-again.yaml")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
		DeferCleanup(resetClock)
	})

	retries := func() int32 { return getIntent(key).Status.Retries }
	replans := func() int32 { return getIntent(key).Status.Replans }
	llmCalls := func() int { return countOf(gw.calls(key), callLLM) }

	// failedWith reports whether the Intent is Failed after retries retries.
	failedWith := func(n int32) func() bool {
		return func() bool {
			in := getIntent(key)
			return in.Status.Phase == lastv1alpha1.PhaseFailed && in.Status.Retries == n
		}
	}

	It("fails with PlanningFailed after 3 replans", func() {
		allowAuto()
		var plans atomic.Int32
		gw.setScript(key, &script{exit: map[string]int32{"s1": 1}, llm: func(*gatewayv1.CallLLMRequest) (string, error) {
			// A new plan each time, so that none repeats a plan that ran.
			return answer([]lastv1alpha1.Command{argvStep("s1", "false", fmt.Sprint(plans.Add(1)))}), nil
		}})
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal(
			"The plan failed 3 times; last failure: step s1 exited 1: boom; edit the Intent's spec to plan again"))
		Expect(countOf(gw.calls(key), beginPhase)).To(Equal(4), "the first plan and 3 replans ran")
		Expect(countOf(gw.calls(key), endFailed)).To(Equal(4))
		Expect(replans()).To(Equal(int32(3)))

		By("resetting replans on a retry")
		gw.updateScript(key, func(s *script) {
			s.llm = func(*gatewayv1.CallLLMRequest) (string, error) {
				return "", status.Error(codes.FailedPrecondition, "llm: no more answers")
			}
		})
		setRetry(key, "1")
		Eventually(failedWith(1)).Should(BeTrue())
		in := getIntent(key)
		Expect(in.Status.Replans).To(BeZero())
		Expect(in.Status.ObservedRetry).To(Equal("1"))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
	})

	It("resets replans on a spec edit and on success", func() {
		failing := func(v string) []lastv1alpha1.Command { return []lastv1alpha1.Command{argvStep("s1", "false", v)} }
		fixed := []lastv1alpha1.Command{argvStep("s2", trueCmd)}
		gw.setScript(key, &script{exit: map[string]int32{"s1": 1}, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			failed := strings.Contains(requestText(req), "boom")
			switch {
			case revised(req) && failed:
				return answer(fixed), nil
			case revised(req):
				return answer(failing("v2")), nil
			case failed:
				return answer(failing("v1 again")), nil
			}
			return answer(failing("v1")), nil
		}})
		createIntent(key, "provision")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(planHash(key)).Should(Equal(hashOf(failing("v1 again"))))
		Expect(replans()).To(Equal(int32(1)))

		By("resetting replans on a spec edit")
		editRequest(key, "provision v2")
		Eventually(planHash(key)).Should(Equal(hashOf(failing("v2"))))
		Expect(replans()).To(BeZero())

		By("resetting replans on success")
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(planHash(key)).Should(Equal(hashOf(fixed)))
		Expect(replans()).To(Equal(int32(1)))
		approve(key, lastv1alpha1.DecisionApprove)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		Expect(replans()).To(BeZero())
	})

	It("retries a failed Auto Intent after 1m, then 2m, then 4m", func() {
		allowAuto()
		gw.setScript(key, &script{llm: turns()}) // every turn fails: PlanningFailed
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		perAttempt := llmCalls()
		Expect(perAttempt).To(BeNumerically(">", 0))

		const early = 20 * time.Second
		for i, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
			n := int32(i)
			By(fmt.Sprintf("waiting %v before retry %d", delay, i+1))
			advanceClock(delay - early)
			poke(key)
			Consistently(llmCalls).Should(Equal((i + 1) * perAttempt))
			Expect(failedWith(n)()).To(BeTrue())
			res, err := intentReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", early-5*time.Second), "it requeues for the time left")
			Expect(res.RequeueAfter).To(BeNumerically("<=", early))

			advanceClock(early)
			poke(key)
			Eventually(llmCalls).Should(Equal((i + 2) * perAttempt))
			Eventually(failedWith(n + 1)).Should(BeTrue())
		}
	})

	It("never retries ApprovalDenied automatically", func() {
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(conversationText(req), "denied the plan") {
				return answer(stepsAgain), nil
			}
			return answer(steps), nil
		}})
		createIntentWithModes(key, lastv1alpha1.StageModes{Create: modeAuto}) // no layer allows Auto yet: it waits
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		allowAuto()
		approve(key, lastv1alpha1.DecisionDeny)
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonApprovalDenied))

		advanceClock(2 * time.Hour)
		poke(key)
		Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed), "the Create stage is Auto, but the plan was denied")
		Expect(llmCalls()).To(Equal(1))
		Expect(retries()).To(BeZero())
		res, err := intentReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero(), "nothing is scheduled")

		By("retrying when the annotation changes")
		setRetry(key, "1")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady), "the Create stage is Auto now")
		Expect(gw.executedOf(key)).To(Equal(hashOf(stepsAgain)))
		Expect(getIntent(key).Status.ObservedRetry).To(Equal("1"))
	})

	Describe("with the retry annotation", func() {
		web := lastv1alpha1.ManagedResourceSpec{Description: "nginx Deployment",
			Probes: []lastv1alpha1.Probe{{ID: "p1", Command: lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{
				Argv: []string{argKubectl, argGet, "deploy/web", "-o", formatJSON}}}, Assert: "result.json.replicas == 2"}},
			Teardown: []lastv1alpha1.Command{argvStep("t1", argKubectl, "delete", "deployment", "web")}}
		update := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web-v2.yaml")}
		updateAgain := []lastv1alpha1.Command{argvStep("s1", argKubectl, "apply", "-f", "web-v3.yaml")}
		fix := []lastv1alpha1.Command{argvStep("r1", argKubectl, "scale", "deployment/web", "--replicas=2")}
		const denied = "denied the plan"

		// llm answers each stage, with another plan once a plan was denied.
		llm := llmFunc(func(req *gatewayv1.CallLLMRequest) (string, error) {
			again := strings.Contains(conversationText(req), denied)
			switch {
			case deleting(req):
				return answer(web.Teardown), nil
			case remediating(req):
				return answer(fix), nil
			case revised(req) && again:
				return answer(updateAgain, web), nil
			case revised(req):
				return answer(update, web), nil
			case again:
				return answer(stepsAgain, web), nil
			}
			return answer(steps, web), nil
		})

		// ready runs the Create plan until the Intent is Ready.
		ready := func(s *script) {
			s.llm = llm
			gw.setScript(key, s)
			createIntent(key, "provision")
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionApprove)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
		}

		// awaiting waits for a plan of stage to await approval.
		awaiting := func(stage lastv1alpha1.Stage) {
			Eventually(func() bool {
				in := getIntent(key)
				return in.Status.Stage == stage && in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval
			}).Should(BeTrue())
		}

		// drift makes p1 fail and marks web-0 out of sync, as a scheduled probe run that found the drift does.
		drift := func() {
			gw.updateScript(key, func(s *script) { s.failing = map[string]bool{"p1": true} })
			setOutOfSync(key.Namespace, "web-0")
		}

		DescribeTable("retries a failed Manual Intent when the retry annotation changes",
			func(stage lastv1alpha1.Stage, fail func(), why string) {
				fail()
				Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
				Expect(getIntent(key).Status.Stage).To(Equal(stage))
				calls := llmCalls()
				advanceClock(2 * time.Hour)
				poke(key)
				Consistently(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed), "a Manual Intent waits for the annotation")
				Expect(retries()).To(BeZero())

				setRetry(key, "1")
				Eventually(func() int32 { return getIntent(key).Status.Retries }).Should(Equal(int32(1)))
				in := getIntent(key)
				Expect(in.Status.ObservedRetry).To(Equal("1"))
				Expect(in.Status.Stage).To(Equal(stage))
				Eventually(llmCalls).Should(BeNumerically(">", calls))
				Expect(conversationText(gw.lastRequest(key))).To(ContainSubstring(why), "the model hears why")
				Consistently(retries).Should(Equal(int32(1)), "one annotation change retries once")
			},
			Entry("Create", lastv1alpha1.StageCreate, func() {
				gw.setScript(key, &script{llm: llm})
				createIntent(key, "provision")
				Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
				approve(key, lastv1alpha1.DecisionDeny)
			}, denied),
			Entry("Update", lastv1alpha1.StageUpdate, func() {
				ready(&script{})
				editRequest(key, "provision v2")
				awaiting(lastv1alpha1.StageUpdate)
				approveAs(key, lastv1alpha1.StageUpdate, lastv1alpha1.DecisionDeny)
			}, denied),
			Entry("Remediate", lastv1alpha1.StageRemediate, func() {
				ready(&script{})
				drift()
				awaiting(lastv1alpha1.StageRemediate)
				approveAs(key, lastv1alpha1.StageRemediate, lastv1alpha1.DecisionDeny)
			}, denied),
			Entry("Delete", lastv1alpha1.StageDelete, func() {
				ready(&script{exit: map[string]int32{"t1": 1}})
				deleteIntent(key)
				awaiting(lastv1alpha1.StageDelete)
				approveDelete(key)
			}, "step t1 exited 1: boom"),
		)

		It("a retried RemediationFailed starts with remediations 0", func() {
			ready(&script{})
			setStatus(key, func(in *lastv1alpha1.Intent) { in.Status.Remediations = maxRemediations })
			drift()
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
			Expect(readyReason(key)).To(Equal(ReasonRemediationFailed))

			setRetry(key, "1")
			awaiting(lastv1alpha1.StageRemediate)
			in := getIntent(key)
			Expect(in.Status.Remediations).To(Equal(int32(1)), "counted from 0: the retried remediation is the first")
			Expect(in.Status.Retries).To(Equal(int32(1)))
			Expect(in.Status.Plan.Steps).To(Equal(fix))
		})

		It("resets retries when the Intent reaches Ready", func() {
			gw.setScript(key, &script{llm: llm})
			createIntent(key, "provision")
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
			approve(key, lastv1alpha1.DecisionDeny)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))

			setRetry(key, "1")
			Eventually(planHash(key)).Should(Equal(hashOf(stepsAgain, web)))
			Expect(phase(key)()).To(Equal(lastv1alpha1.PhaseAwaitingApproval))
			Expect(retries()).To(Equal(int32(1)))
			approve(key, lastv1alpha1.DecisionApprove)
			Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseReady))
			Expect(retries()).To(BeZero())
			Expect(replans()).To(BeZero())
		})
	})
})
