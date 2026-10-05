package controller

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const intentName = "web"

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

func createApproval(key types.NamespacedName, stage lastv1alpha1.Stage, subject string, decision lastv1alpha1.Decision) {
	a := &lastv1alpha1.IntentApproval{ObjectMeta: metav1.ObjectMeta{GenerateName: "approval-", Namespace: key.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: key.Name}, Stage: stage,
			SubjectHash: subject, Decision: decision, ApprovedBy: "alice"}}
	Expect(k8sClient.Create(ctx, a)).To(Succeed())
}

// answer is a model answer holding a plan.
//
//nolint:unparam // later tests pass resources
func answer(steps []lastv1alpha1.Command, resources ...lastv1alpha1.ManagedResourceSpec) string {
	b, err := json.Marshal(map[string]any{"summary": "test plan", "steps": steps, "resources": resources})
	Expect(err).NotTo(HaveOccurred())
	return string(b)
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
		if m.GetRole() == "user" {
			return m.GetContent()
		}
	}
	return ""
}

func zeroHash() string { return "sha256:" + strings.Repeat("0", 64) }
