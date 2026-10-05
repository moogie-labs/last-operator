package gateway

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func TestStateStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	s := NewStateStore(kube, GRPCAudience)
	r, err := s.Load(ctx, "team-x/web")
	if err != nil || r.Intent != "team-x/web" || r.Open != nil || r.Executed != nil {
		t.Fatalf("missing record = %+v, %v", r, err)
	}
	r.Open = &OpenPhase{Stage: "Create", SubjectHash: "sha256:a", BeginSeq: 7, Epoch: "e1"}
	r.MarkUsed("sha256:a")
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Open = nil
	r.Executed = &ExecutedPlan{Stage: "Create", PlanHash: "sha256:a", Plan: `{"steps":[]}`, EndSeq: 9}
	if err := s.Save(ctx, r); err != nil {
		t.Fatalf("second save (update): %v", err)
	}
	got, err := s.Load(ctx, "team-x/web")
	if err != nil || got.Open != nil || got.Executed == nil || got.Executed.EndSeq != 9 || len(got.Used) != 1 {
		t.Fatalf("reloaded = %+v, %v", got.IntentRecord, err)
	}
	cms, _ := kube.CoreV1().ConfigMaps(GRPCAudience).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=intent-state"})
	if len(cms.Items) != 1 || !strings.HasPrefix(cms.Items[0].Name, "last-intent-") || len(cms.Items[0].Name) != len("last-intent-")+20 ||
		cms.Items[0].Labels[sandbox.LabelManagedBy] != sandbox.ManagedByValue {
		t.Fatalf("configmaps = %+v", cms.Items)
	}
}

func TestStateStoreRejectsForeignAndCorruptRecords(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	s := NewStateStore(kube, GRPCAudience)
	_, _ = kube.CoreV1().ConfigMaps(GRPCAudience).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: recordName("a/b"), Namespace: GRPCAudience},
		Data:       map[string]string{"record.json": `{"intent":"other/x"}`},
	}, metav1.CreateOptions{})
	if _, err := s.Load(ctx, "a/b"); err == nil {
		t.Fatal("a record for another intent must be refused")
	}
	_, _ = kube.CoreV1().ConfigMaps(GRPCAudience).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: recordName("c/d"), Namespace: GRPCAudience},
		Data:       map[string]string{"record.json": `{not json`},
	}, metav1.CreateOptions{})
	if _, err := s.Load(ctx, "c/d"); err == nil {
		t.Fatal("a corrupt record must be an error")
	}
}

func TestMarkUsedIsBounded(t *testing.T) {
	var r IntentRecord
	for i := range maxUsedSubjects + 5 {
		r.MarkUsed("sha256:" + strings.Repeat("0", i+1))
	}
	if len(r.Used) != maxUsedSubjects || r.Used[0] != "sha256:"+strings.Repeat("0", 6) {
		t.Fatalf("used = %d entries, first %q", len(r.Used), r.Used[0])
	}
	r.MarkUsed(r.Used[3])
	if len(r.Used) != maxUsedSubjects {
		t.Fatal("re-marking a subject must not grow the list")
	}
}
