package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

var _ = Describe("Intent planning after an operator restart", func() {
	var key types.NamespacedName
	q1 := lastv1alpha1.Question{ID: "q1", Text: "Which namespace?"}
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "namespaces"}}}
	steps := []lastv1alpha1.Command{argvStep("s1", "kubectl", "get", "namespaces")}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// restart drops the suite controller's conversation, as an operator restart
	// or a leader change would.
	restart := func() { intentReconciler.sessions.drop(key) }

	It("reloads the conversation from the audit log", func() {
		gw.setScript(key, &script{llm: turns(questionsTurn(q1), answer(steps))})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		session := getIntent(key).Status.PlanningSessionSeq
		first := gw.lastRequest(key).GetMessages()

		restart()
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, queryAudit, queryAudit, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{roleSystem, roleUser, roleAssistant, roleUser}))
		Expect(msgs[0].GetContent()).To(Equal(first[0].GetContent()))
		Expect(msgs[1].GetContent()).To(Equal(first[1].GetContent()))
		Expect(msgs[2].GetContent()).To(Equal(questionsTurn(q1)))
		Expect(msgs[3].GetContent()).To(Equal("Answers:\n- q1: web\n"))
		Expect(getIntent(key).Status.PlanningSessionSeq).To(Equal(session))
	})

	It("reloads the conversation and a discovery command's result", func() {
		gw.setScript(key, &script{llm: turns(commandTurn(look), answer(steps)),
			stdout: map[string]string{plan.DiscoveryStepID: "default\n"}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))

		restart()
		approveCommand(key, getIntent(key).Status.PendingCommand.Hash, lastv1alpha1.DecisionApprove, "")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, runDiscoveryStep, endSucceeded,
			queryAudit, queryAudit, queryAudit, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{roleSystem, roleUser, roleAssistant, roleUser}))
		Expect(msgs[3].GetContent()).To(Equal("The command exited 0.\nstdout:\ndefault\nstderr: empty\n"))
	})

	It("starts a new session, saying why, when the audit log no longer holds the conversation", func() {
		gw.setScript(key, &script{auditTruncated: true, llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "could not reload") {
				return answer(steps), nil
			}
			return questionsTurn(q1), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseClarifying))
		session := getIntent(key).Status.PlanningSessionSeq

		restart()
		answerQuestions(key, lastv1alpha1.Answer{ID: "q1", Value: "web"})
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, queryAudit, callLLM}))
		msgs := gw.lastRequest(key).GetMessages()
		Expect(roles(msgs)).To(Equal([]string{roleSystem, roleUser}))
		Expect(msgs[1].GetContent()).To(HavePrefix("Note: " + noteRestarted))
		Expect(msgs[1].GetContent()).To(ContainSubstring("- q1: web"))
		in := getIntent(key)
		Expect(in.Status.PlanningSessionSeq).NotTo(Equal(session))
		Expect(in.Status.PlanningSessionSeq).To(Equal(int64(gw.auditOf(key, "llm.request")[1].GetSeq())))
	})
})
