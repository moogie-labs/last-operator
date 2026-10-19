package controller

import (
	"strings"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const intentName = "web"

// Strings the specs repeat; goconst counts them package-wide.
const (
	roleUser       = "user"
	namespacesArg  = "namespaces"
	whichNamespace = "Which namespace?"
	roleAssistant  = "assistant"
	roleSystem     = "system"
	argGet         = "get"
	argKubectl     = "kubectl"
)

func newNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func createIntent(key types.NamespacedName, request string) {
	in := &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentSpec{Request: request}}
	Expect(k8sClient.Create(ctx, in)).To(Succeed())
}

func getIntent(key types.NamespacedName) *lastv1alpha1.Intent {
	var in lastv1alpha1.Intent
	Expect(k8sClient.Get(ctx, key, &in)).To(Succeed())
	return &in
}

func phase(key types.NamespacedName) func() lastv1alpha1.Phase {
	return func() lastv1alpha1.Phase { return getIntent(key).Status.Phase }
}

func planHash(key types.NamespacedName) func() string {
	return func() string {
		if p := getIntent(key).Status.Plan; p != nil {
			return p.Hash
		}
		return ""
	}
}

func readyCondition(key types.NamespacedName) metav1.Condition {
	c := meta.FindStatusCondition(getIntent(key).Status.Conditions, CondReady)
	Expect(c).NotTo(BeNil())
	return *c
}

func readyReason(key types.NamespacedName) string { return readyCondition(key).Reason }

func editRequest(key types.NamespacedName, request string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		in.Spec.Request = request
		return k8sClient.Update(ctx, &in)
	})).To(Succeed())
}

// intentUID returns the live UID of the Intent at key, which approvals must name.
func intentUID(key types.NamespacedName) string {
	var in lastv1alpha1.Intent
	if err := k8sClient.Get(ctx, key, &in); err != nil {
		return "no-such-intent" // a decision about an Intent that does not exist names no live UID
	}
	return string(in.UID)
}

func createApproval(key types.NamespacedName, stage lastv1alpha1.Stage, subject string, decision lastv1alpha1.Decision) {
	a := &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-", Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: key.Name}, IntentUID: intentUID(key), Stage: stage,
			SubjectHash: subject, Decision: decision, ApprovedBy: "alice"}}
	Expect(k8sClient.Create(ctx, a)).To(Succeed())
}

// answer is a model answer holding a plan.
func answer(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	return encodeTurn(agent.Turn{Kind: agent.KindPlan,
		Plan: &lastv1alpha1.Plan{Summary: "test plan", Steps: steps, Resources: resources}})
}

// questionsTurn is a model answer asking questions.
func questionsTurn(qs ...lastv1alpha1.Question) string {
	return encodeTurn(agent.Turn{Kind: agent.KindQuestions, Questions: qs})
}

// commandTurn is a model answer asking to run a discovery command.
func commandTurn(c lastv1alpha1.Command) string {
	return encodeTurn(agent.Turn{Kind: agent.KindCommand, Command: &c, Reason: "see what exists"})
}

func encodeTurn(t agent.Turn) string {
	s, err := agent.Encode(t)
	Expect(err).NotTo(HaveOccurred())
	return s
}

// answerQuestions sets spec.answers, as the requester does.
func answerQuestions(key types.NamespacedName, answers ...lastv1alpha1.Answer) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		in.Spec.Answers = answers
		return k8sClient.Update(ctx, &in)
	})).To(Succeed())
}

// roles lists a conversation's message roles.
func roles(msgs []*gatewayv1.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.GetRole())
	}
	return out
}

// lastUserMessage returns the last message of the Intent's last CallLLM, which
// is always the user's.
func lastUserMessage(key types.NamespacedName) string {
	msgs := gw.lastRequest(key).GetMessages()
	Expect(msgs).NotTo(BeEmpty())
	last := msgs[len(msgs)-1]
	Expect(last.GetRole()).To(Equal(roleUser))
	return last.GetContent()
}

func argvStep(id string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

//nolint:unparam // later tests pass resources
func hashOf(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	h, err := plan.Hash(plan.Body{Steps: steps, Resources: resources})
	Expect(err).NotTo(HaveOccurred())
	return h
}

// requestText returns the first user message of a conversation: the request prompt.
func requestText(req *gatewayv1.CallLLMRequest) string {
	for _, m := range req.GetMessages() {
		if m.GetRole() == roleUser {
			return m.GetContent()
		}
	}
	return ""
}

// revised reports whether a CallLLM request plans an edited request: specs edit
// it by appending " v2". The space matters, as the prompt also names the
// Intent's namespace, whose random suffix can hold "v2".
func revised(req *gatewayv1.CallLLMRequest) bool { return strings.Contains(requestText(req), " v2") }

func zeroHash() string { return "sha256:" + strings.Repeat("0", 64) }

// setStatus rewrites an Intent's status, as an earlier operator process might
// have left it.
func setStatus(key types.NamespacedName, change func(*lastv1alpha1.Intent)) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var in lastv1alpha1.Intent
		if err := k8sClient.Get(ctx, key, &in); err != nil {
			return err
		}
		change(&in)
		return k8sClient.Status().Update(ctx, &in)
	})).To(Succeed())
}

// executedReason returns the Intent's execution checkpoint.
func executedReason(key types.NamespacedName) func() string {
	return func() string {
		if c := meta.FindStatusCondition(getIntent(key).Status.Conditions, CondExecuted); c != nil {
			return c.Reason
		}
		return ""
	}
}

// executingAt sets an Intent at phase Executing with the given checkpoint.
func executingAt(key types.NamespacedName, reason string) {
	setStatus(key, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseExecuting
		setCondition(&in.Status.Conditions, in.Generation, CondExecuted, metav1.ConditionUnknown, reason, "set by the test")
	})
}

func getMR(ns, name string) *lastv1alpha1.ManagedResource {
	var mr lastv1alpha1.ManagedResource
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &mr)).To(Succeed())
	return &mr
}

// approve files a decision on the Intent's current plan.
//
//nolint:unparam // later tests deny
func approve(key types.NamespacedName, decision lastv1alpha1.Decision) {
	p := getIntent(key).Status.Plan
	Expect(p).NotTo(BeNil())
	createApproval(key, lastv1alpha1.StageCreate, p.Hash, decision)
}

// setInSync sets a ManagedResource's InSync condition, as a probe run would.
func setInSync(ns, name string, s metav1.ConditionStatus) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mr := getMR(ns, name)
		setCondition(&mr.Status.Conditions, mr.Generation, CondInSync, s, ReasonProbesFailed, "set by the test")
		return k8sClient.Status().Update(ctx, mr)
	})).To(Succeed())
}

// approveCommand files a decision on a discovery command, as kubectl last does.
func approveCommand(key types.NamespacedName, subject string, decision lastv1alpha1.Decision, reason string) {
	a := &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-", Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: key.Name}, IntentUID: intentUID(key), Stage: lastv1alpha1.StageDiscovery,
			SubjectHash: subject, Decision: decision, ApprovedBy: "alice", Reason: reason}}
	Expect(k8sClient.Create(ctx, a)).To(Succeed())
}
