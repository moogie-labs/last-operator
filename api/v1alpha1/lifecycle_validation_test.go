package v1alpha1_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const validHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestIntentRequiresRequest(t *testing.T) {
	obj := &lastv1alpha1.Intent{ObjectMeta: meta()}
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.request")
}

func TestIntentAcceptsModes(t *testing.T) {
	obj := &lastv1alpha1.Intent{ObjectMeta: meta(), Spec: lastv1alpha1.IntentSpec{
		Request: "run nginx serving team-x-site",
		Answers: []lastv1alpha1.Answer{{ID: "q1", Value: "internal only"}},
		Modes:   &lastv1alpha1.StageModes{Discovery: lastv1alpha1.ModeManual, Reconcile: lastv1alpha1.ModeAuto},
	}}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestIntentRejectsUnknownMode(t *testing.T) {
	obj := &lastv1alpha1.Intent{ObjectMeta: meta(), Spec: lastv1alpha1.IntentSpec{
		Request: "x",
		Modes:   &lastv1alpha1.StageModes{Create: "Sometimes"},
	}}
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.modes.create")
}

func managedResource(run lastv1alpha1.CommandRun) *lastv1alpha1.ManagedResource {
	return &lastv1alpha1.ManagedResource{ObjectMeta: meta(), Spec: lastv1alpha1.ManagedResourceSpec{
		IntentRef: &lastv1alpha1.LocalRef{Name: "web"},
		Probes: []lastv1alpha1.Probe{{
			ID:      "replicas",
			Command: lastv1alpha1.Command{Run: run},
			Assert:  "result.exitCode == 0",
		}},
	}}
}

func TestCommandRunRequiresExactlyOneOfArgvOrScript(t *testing.T) {
	both := managedResource(lastv1alpha1.CommandRun{Argv: []string{"kubectl"}, Script: "echo hi"})
	expectInvalid(t, k8sClient.Create(ctx, both), "exactly one of argv or script")

	neither := managedResource(lastv1alpha1.CommandRun{})
	expectInvalid(t, k8sClient.Create(ctx, neither), "exactly one of argv or script")

	argv := managedResource(lastv1alpha1.CommandRun{Argv: []string{"kubectl", "get", "pods"}})
	if err := k8sClient.Create(ctx, argv); err != nil {
		t.Fatalf("argv-only command rejected: %v", err)
	}
}

func TestManagedResourceKeepsFreeFormDesiredState(t *testing.T) {
	obj := managedResource(lastv1alpha1.CommandRun{Argv: []string{"true"}})
	obj.Spec.Desired = &runtime.RawExtension{Raw: []byte(`{"replicas":2,"image":"nginx:1.27"}`)}
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &lastv1alpha1.ManagedResource{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(got.Spec.Desired.Raw), `"replicas":2`) {
		t.Fatalf("desired state was pruned: %s", got.Spec.Desired.Raw)
	}
}

func validApproval() *lastv1alpha1.IntentApproval {
	return &lastv1alpha1.IntentApproval{ObjectMeta: meta(), Spec: lastv1alpha1.IntentApprovalSpec{
		IntentRef:   lastv1alpha1.LocalRef{Name: "web"},
		IntentUID:   "uid-web",
		Stage:       lastv1alpha1.StageCreate,
		SubjectHash: validHash,
		Decision:    lastv1alpha1.DecisionApprove,
		ApprovedBy:  "alice@example.com",
	}}
}

func TestIntentApprovalRejectsMalformedHash(t *testing.T) {
	obj := validApproval()
	obj.Spec.SubjectHash = "abc"
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.subjectHash")
}

func TestIntentApprovalRequiresIntentUID(t *testing.T) {
	obj := validApproval()
	obj.Spec.IntentUID = ""
	expectInvalid(t, k8sClient.Create(ctx, obj), "spec.intentUID")
}

func TestIntentApprovalIsImmutable(t *testing.T) {
	obj := validApproval()
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	obj.Spec.Reason = "changed my mind"
	expectInvalid(t, k8sClient.Update(ctx, obj), "spec is immutable")
}
