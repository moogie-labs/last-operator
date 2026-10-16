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
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

var _ = Describe("Intent discovery", func() {
	var key types.NamespacedName
	look := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{argKubectl, argGet, namespacesArg}}}
	steps := []lastv1alpha1.Command{argvStep("s1", argKubectl, argGet, namespacesArg)}

	BeforeEach(func() {
		key = types.NamespacedName{Namespace: newNamespace(), Name: intentName}
	})

	// awaitingDiscovery scripts the gateway and waits until the Intent asks to
	// run a discovery command, whose subject it returns.
	awaitingDiscovery := func(s *script) string {
		gw.setScript(key, s)
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))
		return getIntent(key).Status.PendingCommand.Hash
	}

	It("runs an approved command and gives the model its result", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn(look), answer(steps)),
			stdout: map[string]string{plan.DiscoveryStepID: "default\nkube-system\n"}})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, runDiscoveryStep, endSucceeded, queryAudit, callLLM}))
		Expect(gw.argvOf(key, plan.DiscoveryStepID)).To(Equal(look.Run.Argv))
		Expect(roles(gw.lastRequest(key).GetMessages())).To(Equal([]string{roleSystem, roleUser, roleAssistant, roleUser}))
		Expect(lastUserMessage(key)).To(Equal("The command exited 0.\nstdout:\ndefault\nkube-system\nstderr: empty\n"))
		in := getIntent(key)
		Expect(in.Status.PendingCommand).To(BeNil())
		Expect(in.Status.Stage).To(Equal(lastv1alpha1.StageCreate))
		Expect(meta.FindStatusCondition(in.Status.Conditions, CondExecuted)).To(BeNil())
		Expect(gw.executedOf(key)).To(BeEmpty(), "a Discovery phase never becomes the executed plan")
	})

	It("gives the model a failing command's exit code and output", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn(look), answer(steps)),
			exit: map[string]int32{plan.DiscoveryStepID: 2}})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(ContainElement(endFailed))
		Expect(lastUserMessage(key)).To(Equal("The command exited 2.\nstdout: empty\nstderr:\nboom\n"))
	})

	It("tells the model when the command is denied", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn(look), answer(steps))})
		approveCommand(key, subject, lastv1alpha1.DecisionDeny, "too broad")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, callLLM}))
		Expect(lastUserMessage(key)).To(Equal("The command was denied: too broad"))
	})

	It("tells the model when the gateway refuses the command", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn(look), answer(steps)),
			beginErr: status.Error(codes.FailedPrecondition, "egress 10.0.0.1:443 is not allowed")})
		approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, beginDiscovery, callLLM}))
		Expect(lastUserMessage(key)).To(Equal("The command did not run: the gateway refused it: egress 10.0.0.1:443 is not allowed"))
	})

	It("ends a discovery phase the operator lost and tells the model", func() {
		subject := awaitingDiscovery(&script{llm: turns(commandTurn(look), answer(steps))})
		// As if an earlier process opened the phase and stopped while the command ran.
		gw.setOpen(key, subject)
		setStatus(key, func(in *lastv1alpha1.Intent) {
			setCondition(&in.Status.Conditions, in.Generation, CondExecuted, metav1.ConditionUnknown, ReasonPhaseOpen, "set by the test")
		})

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(gw.calls(key)).To(Equal([]string{callLLM, endFailed, callLLM}))
		Expect(gw.openPhase(key)).To(BeEmpty())
		Expect(lastUserMessage(key)).To(ContainSubstring("the operator lost track of it"))
	})

	It("starts a new session when the request changes while a command waits", func() {
		gw.setScript(key, &script{llm: func(req *gatewayv1.CallLLMRequest) (string, error) {
			if strings.Contains(requestText(req), "v2") {
				return answer(steps), nil
			}
			return commandTurn(look), nil
		}})
		createIntent(key, "list namespaces")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingDiscoveryApproval))

		editRequest(key, "list namespaces v2")
		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseAwaitingApproval))
		Expect(getIntent(key).Status.PendingCommand).To(BeNil())
		Expect(roles(gw.lastRequest(key).GetMessages())).To(Equal([]string{roleSystem, roleUser}))
	})

	It("fails when the model asks for more than MaxCommands commands", func() {
		gw.setScript(key, &script{llm: func(*gatewayv1.CallLLMRequest) (string, error) {
			return commandTurn(look), nil
		}})
		createIntent(key, "list namespaces")
		approved := map[string]bool{}
		for range agent.MaxCommands {
			var subject string
			Eventually(func() bool {
				in := getIntent(key)
				if in.Status.Phase != lastv1alpha1.PhaseAwaitingDiscoveryApproval || in.Status.PendingCommand == nil {
					return false
				}
				subject = in.Status.PendingCommand.Hash
				return !approved[subject]
			}).Should(BeTrue())
			approved[subject] = true
			approveCommand(key, subject, lastv1alpha1.DecisionApprove, "")
		}

		Eventually(phase(key)).Should(Equal(lastv1alpha1.PhaseFailed))
		Expect(readyReason(key)).To(Equal(ReasonPlanningFailed))
		Expect(readyCondition(key).Message).To(Equal("planning exceeded 10 discovery commands"))
	})
})
