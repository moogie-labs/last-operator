package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const testHashA = "sha256:a"

func TestStateStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	s := NewStateStore(kube, GRPCAudience)
	r, err := s.Load(ctx, "team-x/web")
	if err != nil || r.Intent != "team-x/web" || r.Open != nil || r.Executed != nil {
		t.Fatalf("missing record = %+v, %v", r, err)
	}
	r.Open = &OpenPhase{Stage: testStage, SubjectHash: testHashA, BeginSeq: 7, Epoch: "e1"}
	r.MarkUsed(testStage, testHashA)
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Open = nil
	r.Executed = &ExecutedPlan{Stage: testStage, PlanHash: testHashA, Plan: `{"steps":[]}`, EndSeq: 9}
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
		r.MarkUsed(testStage, "sha256:"+strings.Repeat("0", i+1))
	}
	if len(r.Used) != maxUsedSubjects || r.Used[0] != "sha256:"+strings.Repeat("0", 6) {
		t.Fatalf("used = %d entries, first %q", len(r.Used), r.Used[0])
	}
	r.MarkUsed(testStage, r.Used[3])
	if len(r.Used) != maxUsedSubjects {
		t.Fatal("re-marking a subject must not grow the list")
	}
}

func TestStateStoreSaveRefusesAnOversizedRecordAndDoesNotEscapeMarkup(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(fake.NewClientset(), GRPCAudience)
	r, _ := s.Load(ctx, "team-x/web")
	r.Executed = &ExecutedPlan{Stage: testStage, PlanHash: testHashA, Plan: strings.Repeat("a", MaxRecordBytes), EndSeq: 1}
	if err := s.Save(ctx, r); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Save of an oversized record = %v, want a too-large error", err)
	}
	r.Executed.Plan = strings.Repeat("<>&", 1000)
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "team-x/web")
	if err != nil || got.Executed.Plan != r.Executed.Plan {
		t.Fatalf("round trip = %v", err)
	}
	if n := len(r.cm.Data[stateKey]); n > 3100 || strings.HasSuffix(r.cm.Data[stateKey], "\n") {
		t.Fatalf("record is %d bytes (markup escaped, or trailing newline kept)", n)
	}
}

func TestDiscoverySubjectsNeverEvictPlanSubjects(t *testing.T) {
	var r IntentRecord
	r.MarkUsed(lastv1alpha1.StageCreate, testHashA)
	for i := range 60 {
		r.MarkUsed(lastv1alpha1.StageDiscovery, fmt.Sprintf("sha256:d%d", i))
	}
	if !slices.Contains(r.Used, testHashA) || !r.WasUsed(testHashA) || len(r.Used) != 1 {
		t.Fatalf("used = %v, want the Create subject kept", r.Used)
	}
}

func TestUsedDiscoveryKeepsTheNewest200(t *testing.T) {
	var r IntentRecord
	for i := range maxUsedDiscoverySubjects + 10 {
		r.MarkUsed(lastv1alpha1.StageDiscovery, fmt.Sprintf("sha256:d%d", i))
	}
	if len(r.UsedDiscovery) != maxUsedDiscoverySubjects || r.WasUsed("sha256:d9") || !r.WasUsed("sha256:d10") ||
		!r.WasUsed(fmt.Sprintf("sha256:d%d", maxUsedDiscoverySubjects+9)) {
		t.Fatalf("usedDiscovery = %d entries, first %q", len(r.UsedDiscovery), r.UsedDiscovery[0])
	}
}

func TestWasUsedSeesBothLists(t *testing.T) {
	var r IntentRecord
	r.MarkUsed(lastv1alpha1.StageCreate, "sha256:p")
	r.MarkUsed(lastv1alpha1.StageDiscovery, "sha256:q")
	if !r.WasUsed("sha256:p") || !r.WasUsed("sha256:q") || r.WasUsed("sha256:r") {
		t.Fatalf("record = %+v", r)
	}
}

func TestRecordIndexKeepsTheNewestEntriesAndRecordsTheFloor(t *testing.T) {
	var r IntentRecord
	var es []IndexEntry
	for seq := uint64(1); seq <= maxIndexEntries+5; seq++ {
		es = append(es, IndexEntry{Seq: seq, Type: audit.TypeLLMRequest})
	}
	r.AddIndex(es)
	r.AddIndex([]IndexEntry{{Seq: 100, Type: audit.TypeLLMRequest}}) // a duplicate is ignored
	if len(r.Index) != maxIndexEntries || r.Index[0].Seq != 6 || r.IndexFloor != 5 {
		t.Fatalf("index has %d entries from seq %d, floor %d", len(r.Index), r.Index[0].Seq, r.IndexFloor)
	}
	r.DropIndex(10)
	if len(r.Index) != maxIndexEntries-10 || r.IndexFloor != 15 {
		t.Fatalf("after DropIndex: %d entries, floor %d", len(r.Index), r.IndexFloor)
	}
}
