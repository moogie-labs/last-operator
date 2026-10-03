package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const foreignManagedBy = "someone-else"

func runMeta(name, id string, created time.Time) metav1.ObjectMeta {
	labels := sandbox.RunLabels(id)
	return metav1.ObjectMeta{Name: name, Namespace: sandboxNS, Labels: labels, CreationTimestamp: metav1.NewTime(created)}
}

func TestReapOrphansReapsByNewestObjectAge(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-2*time.Hour), now.Add(-time.Minute)
	objs := []runtime.Object{
		// Run "old" - all old - reaped
		&corev1.Pod{ObjectMeta: runMeta("last-run-old-pod", "old", old)},
		&corev1.Secret{ObjectMeta: runMeta("last-run-old-creds", "old", old)},
		&networkingv1.NetworkPolicy{ObjectMeta: runMeta("last-run-old-policy", "old", old)},
		// Run "mixed" - has a new object - NOT reaped
		&corev1.Secret{ObjectMeta: runMeta("last-run-mixed-old-creds", "mixed", old)},
		&corev1.Pod{ObjectMeta: runMeta("last-run-mixed-pod", "mixed", recent)},
		// Run "new" - all new - NOT reaped
		&corev1.Pod{ObjectMeta: runMeta("last-run-new", "new", recent)},
		// Unlabelled - NOT reaped
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: sandboxNS, CreationTimestamp: metav1.NewTime(old)}},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, sink := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
	ctx := context.Background()

	// "old" run should be gone
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "last-run-old-pod", metav1.GetOptions{}); err == nil {
		t.Fatal("old run pod survived")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "last-run-old-creds", metav1.GetOptions{}); err == nil {
		t.Fatal("old run secret survived")
	}

	// "mixed" run should survive because its Pod is new
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "last-run-mixed-pod", metav1.GetOptions{}); err != nil {
		t.Fatal("mixed run pod was reaped")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "last-run-mixed-old-creds", metav1.GetOptions{}); err != nil {
		t.Fatal("mixed run secret was reaped")
	}

	// "new" run should survive
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "last-run-new", metav1.GetOptions{}); err != nil {
		t.Fatal("new run was reaped")
	}

	// Unlabelled should survive
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled secret was reaped")
	}

	// Check that egress.revoked was written only for "old" run
	var revoked []audit.Entry
	for _, e := range entries(t, sink) {
		if e.Type == audit.TypeEgressRevoked {
			revoked = append(revoked, e)
		}
	}
	if len(revoked) != 1 || !strings.Contains(string(revoked[0].Payload), `"runId":"old"`) {
		t.Fatalf("revoked entries = %+v", revoked)
	}

	// Verify the old run's policy was deleted
	if _, err := kube.NetworkingV1().NetworkPolicies(sandboxNS).Get(ctx, "last-run-old-policy", metav1.GetOptions{}); err == nil {
		t.Fatal("old run policy survived")
	}
}

func TestReapOrphansWithZeroAgeReapsEverything(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	future := now.Add(5 * time.Second)
	kube, _ := fakeCluster(corev1.PodSucceeded, &corev1.Pod{ObjectMeta: runMeta("last-run-x", "x", future)})
	aud, _ := newAudit(t)
	if n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), 0, now); err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
}

func TestReapOrphansCountsOnlySuccessfulCleanups(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objs := []runtime.Object{
		&corev1.Pod{ObjectMeta: runMeta("pod-a", "a", old)},
		&corev1.Pod{ObjectMeta: runMeta("pod-b", "b", old)},
		&corev1.Secret{ObjectMeta: runMeta("secret-b", "b", old)},
		&networkingv1.NetworkPolicy{ObjectMeta: runMeta("policy-b", "b", old)},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	// Make Pod deletion fail for run "a"
	kube.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == "pod-a" {
			return true, nil, errors.New("api server failure")
		}
		return false, nil, nil
	})
	aud, _ := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if n != 1 {
		t.Fatalf("reaped=%d, want 1 (only run b succeeded)", n)
	}
	if err == nil || !strings.Contains(err.Error(), "reap run a:") {
		t.Fatalf("error should name run a: %v", err)
	}
	// Every object of run "b" should be deleted
	ctx := context.Background()
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "pod-b", metav1.GetOptions{}); err == nil {
		t.Fatal("run b pod survived")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "secret-b", metav1.GetOptions{}); err == nil {
		t.Fatal("run b secret survived")
	}
	if _, err := kube.NetworkingV1().NetworkPolicies(sandboxNS).Get(ctx, "policy-b", metav1.GetOptions{}); err == nil {
		t.Fatal("run b policy survived")
	}
}

func TestReapOrphansIgnoresForeignManagedByObjects(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objs := []runtime.Object{
		// Old Pod with foreign managed-by
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "foreign-pod", Namespace: sandboxNS,
			Labels:            map[string]string{sandbox.LabelRun: "x", sandbox.LabelManagedBy: foreignManagedBy},
			CreationTimestamp: metav1.NewTime(old),
		}},
		// Old Secret with foreign managed-by
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "foreign-secret", Namespace: sandboxNS,
			Labels:            map[string]string{sandbox.LabelRun: "x", sandbox.LabelManagedBy: foreignManagedBy},
			CreationTimestamp: metav1.NewTime(old),
		}},
		// Old NetworkPolicy with foreign managed-by
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name: "foreign-policy", Namespace: sandboxNS,
			Labels:            map[string]string{sandbox.LabelRun: "x", sandbox.LabelManagedBy: foreignManagedBy},
			CreationTimestamp: metav1.NewTime(old),
		}},
		// Old objects of each kind with no labels at all
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare-pod", Namespace: sandboxNS, CreationTimestamp: metav1.NewTime(old)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bare-secret", Namespace: sandboxNS, CreationTimestamp: metav1.NewTime(old)}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "bare-policy", Namespace: sandboxNS, CreationTimestamp: metav1.NewTime(old)}},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, _ := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if err != nil || n != 0 {
		t.Fatalf("reaped=%d err=%v, want 0 and nil", n, err)
	}
	ctx := context.Background()
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "foreign-pod", metav1.GetOptions{}); err != nil {
		t.Fatal("foreign pod was deleted")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "foreign-secret", metav1.GetOptions{}); err != nil {
		t.Fatal("foreign secret was deleted")
	}
	if _, err := kube.NetworkingV1().NetworkPolicies(sandboxNS).Get(ctx, "foreign-policy", metav1.GetOptions{}); err != nil {
		t.Fatal("foreign policy was deleted")
	}
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "bare-pod", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled pod was deleted")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "bare-secret", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled secret was deleted")
	}
	if _, err := kube.NetworkingV1().NetworkPolicies(sandboxNS).Get(ctx, "bare-policy", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled policy was deleted")
	}
}

func TestReapOrphansIgnoresObjectsWithoutRunLabel(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objs := []runtime.Object{
		// Old Pod with no labels
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "no-label-pod", Namespace: sandboxNS,
			CreationTimestamp: metav1.NewTime(old),
		}},
		// Old Pod with empty run label but managed-by set
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "empty-run-pod", Namespace: sandboxNS,
			Labels:            map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, sandbox.LabelRun: ""},
			CreationTimestamp: metav1.NewTime(old),
		}},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, _ := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if err != nil || n != 0 {
		t.Fatalf("reaped=%d err=%v, want 0 and nil", n, err)
	}
	ctx := context.Background()
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "no-label-pod", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled pod was deleted")
	}
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "empty-run-pod", metav1.GetOptions{}); err != nil {
		t.Fatal("pod with empty run label was deleted")
	}
}

func TestReapOrphansDoesNotRevokeWhenNoPolicy(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objs := []runtime.Object{
		&corev1.Pod{ObjectMeta: runMeta("pod-x", "x", old)},
		// No NetworkPolicy for this run
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, sink := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
	var revoked []audit.Entry
	for _, e := range entries(t, sink) {
		if e.Type == audit.TypeEgressRevoked {
			revoked = append(revoked, e)
		}
	}
	if len(revoked) != 0 {
		t.Fatalf("expected no egress.revoked entries, got %d", len(revoked))
	}
}

func TestReapOrphansReportsAuditFailure(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objs := []runtime.Object{
		&corev1.Pod{ObjectMeta: runMeta("pod-x", "x", old)},
		&networkingv1.NetworkPolicy{ObjectMeta: runMeta("policy-x", "x", old)},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, sink := newAudit(t)
	sink.SetFailPuts(true) // Make Append fail
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if !errors.Is(err, audit.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	// cleanup failed due to audit error, so count is 0
	if n != 0 {
		t.Fatalf("reaped=%d, want 0 (cleanup failed due to audit)", n)
	}
}
