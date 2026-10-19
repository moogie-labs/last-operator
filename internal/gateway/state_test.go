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
	r, err := s.LoadAny(ctx, "team-x/web")
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
	got, err := s.LoadAny(ctx, "team-x/web")
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
	if _, err := s.LoadAny(ctx, "a/b"); err == nil {
		t.Fatal("a record for another intent must be refused")
	}
	_, _ = kube.CoreV1().ConfigMaps(GRPCAudience).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: recordName("c/d"), Namespace: GRPCAudience},
		Data:       map[string]string{"record.json": `{not json`},
	}, metav1.CreateOptions{})
	if _, err := s.LoadAny(ctx, "c/d"); err == nil {
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
	r, _ := s.LoadAny(ctx, "team-x/web")
	r.Executed = &ExecutedPlan{Stage: testStage, PlanHash: testHashA, Plan: strings.Repeat("a", MaxRecordBytes), EndSeq: 1}
	if err := s.Save(ctx, r); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Save of an oversized record = %v, want a too-large error", err)
	}
	r.Executed.Plan = strings.Repeat("<>&", 1000)
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadAny(ctx, "team-x/web")
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

func TestARecordForAnotherUIDStartsClean(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	ctx := context.Background()
	old, err := env.g.state.LoadAny(ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	old.IntentUID = "uid-a"
	old.MarkUsed(testStage, h)
	old.Executed = &ExecutedPlan{Stage: string(testStage), PlanHash: h, Plan: `{}`, EndSeq: 4}
	old.AddIndex([]IndexEntry{{Seq: 3, Type: "phase.begin"}})
	if err := env.g.state.Save(ctx, old); err != nil {
		t.Fatal(err)
	}
	got, err := env.g.state.Load(ctx, envKey, envIntentUID)
	if err != nil || got.IntentUID != envIntentUID || got.Executed != nil || len(got.Used) != 0 || len(got.Index) != 0 || got.cm == nil {
		t.Fatalf("record for another UID = %+v, %v; want empty, bound to the Intent, ConfigMap kept", got.IntentRecord, err)
	}
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase for the recreated Intent = %v; the old UID's used subject must not count", err)
	}
	after, _ := env.g.state.LoadAny(ctx, envKey)
	if after.IntentUID != string(envIntentUID) || after.Open == nil {
		t.Fatalf("saved record = %+v", after.IntentRecord)
	}
}

func TestLoadAnyKeepsTheUIDAndLoadAdoptsIntoAnUnboundRecord(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(fake.NewClientset(), GRPCAudience)
	r, _ := s.LoadAny(ctx, "a/b")
	r.MarkUsed(testStage, testHashA)
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "a/b", "uid-1")
	if err != nil || len(got.Used) != 1 || got.IntentUID != "uid-1" {
		t.Fatalf("adopting Load = %+v, %v", got.IntentRecord, err)
	}
	if err := s.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	any, _ := s.LoadAny(ctx, "a/b")
	if any.IntentUID != "uid-1" {
		t.Fatalf("LoadAny dropped the UID: %+v", any.IntentRecord)
	}
	if err := s.Save(ctx, any); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Load(ctx, "a/b", "uid-1"); len(again.Used) != 1 {
		t.Fatal("a record saved through LoadAny lost its contents")
	}
}

func TestDeleteRemovesTheRecord(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(fake.NewClientset(), GRPCAudience)
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Fatalf("Delete of a missing record = %v, want nil", err)
	}
	for _, k := range []string{"a/b", "c/d"} {
		r, _ := s.LoadAny(ctx, k)
		if err := s.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if recs, err := s.List(ctx); err != nil || len(recs) != 2 {
		t.Fatalf("List = %d records, %v", len(recs), err)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	recs, err := s.List(ctx)
	if err != nil || len(recs) != 1 || recs[0].Intent != "c/d" {
		t.Fatalf("List after Delete = %+v, %v", recs, err)
	}
}

func TestLeadClosesInterruptedPhases(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ctx := context.Background()
	for i, k := range []string{"team-x/one", "team-x/two"} {
		r, _ := ie.g.state.LoadAny(ctx, k)
		r.Open = &OpenPhase{Stage: string(testStage), SubjectHash: testHashA, BeginSeq: uint64(i + 1), Epoch: "old-epoch"}
		if err := ie.g.state.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := ie.g.prepareLeading(ctx, ie.g.writer.Load()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"team-x/one", "team-x/two"} {
		r, _ := ie.g.state.LoadAny(ctx, k)
		if r.Open != nil {
			t.Fatalf("%s still has an open phase", k)
		}
		found := false
		for _, e := range ie.entries() {
			if e.Type == audit.TypePhaseEnd && e.Intent == k && strings.Contains(string(e.Payload), "interrupted") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no interrupted phase.end for %s", k)
		}
	}
}
