package sandbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func runMeta(name, id string, created time.Time) metav1.ObjectMeta {
	labels := sandbox.RunLabels(id)
	return metav1.ObjectMeta{Name: name, Namespace: sandboxNS, Labels: labels, CreationTimestamp: metav1.NewTime(created)}
}

func TestReapOrphansDeletesOnlyOldRunObjects(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-2*time.Hour), now.Add(-time.Minute)
	objs := []runtime.Object{
		&corev1.Pod{ObjectMeta: runMeta("last-run-old", "old", old)},
		&corev1.Secret{ObjectMeta: runMeta("last-run-old-creds", "old", old)},
		&networkingv1.NetworkPolicy{ObjectMeta: runMeta("last-run-old", "old", old)},
		&corev1.Pod{ObjectMeta: runMeta("last-run-new", "new", recent)},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: sandboxNS, CreationTimestamp: metav1.NewTime(old)}},
	}
	kube, _ := fakeCluster(corev1.PodSucceeded, objs...)
	aud, sink := newAudit(t)
	n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
	ctx := context.Background()
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "last-run-old", metav1.GetOptions{}); err == nil {
		t.Fatal("old pod survived")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "last-run-old-creds", metav1.GetOptions{}); err == nil {
		t.Fatal("old credential secret survived")
	}
	if _, err := kube.CoreV1().Pods(sandboxNS).Get(ctx, "last-run-new", metav1.GetOptions{}); err != nil {
		t.Fatal("recent pod was reaped")
	}
	if _, err := kube.CoreV1().Secrets(sandboxNS).Get(ctx, "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatal("unlabelled secret was reaped")
	}
	var revoked []audit.Entry
	for _, e := range entries(t, sink) {
		if e.Type == audit.TypeEgressRevoked {
			revoked = append(revoked, e)
		}
	}
	if len(revoked) != 1 || !strings.Contains(string(revoked[0].Payload), `"runId":"old"`) || !strings.Contains(string(revoked[0].Payload), `"reason":"reaped"`) {
		t.Fatalf("revoked entries = %+v", revoked)
	}
}

func TestReapOrphansWithZeroAgeReapsEverything(t *testing.T) {
	now := time.Now()
	kube, _ := fakeCluster(corev1.PodSucceeded, &corev1.Pod{ObjectMeta: runMeta("last-run-x", "x", now)})
	aud, _ := newAudit(t)
	if n, err := newExec(kube, aud, envelopeLogs()).ReapOrphans(context.Background(), 0, now.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
}
