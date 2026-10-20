package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

const (
	testHashA        = "sha256:a"
	testOldUID       = "old-uid"
	testRecreatedUID = "recreated"
)

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

func TestIndexTrimmingKeepsTheExecutedPlansEntries(t *testing.T) {
	r := IntentRecord{Executed: &ExecutedPlan{BeginSeq: 3, EndSeq: 7}}
	var es []IndexEntry
	for seq := uint64(1); seq <= maxIndexEntries+10; seq++ {
		es = append(es, IndexEntry{Seq: seq, Type: audit.TypePhaseBegin})
	}
	r.AddIndex(es)
	if len(r.Index) != maxIndexEntries || r.Index[0].Seq != 3 || r.Index[1].Seq != 7 || r.Index[2].Seq != 13 || r.IndexFloor != 12 {
		t.Fatalf("index has %d entries starting %v, floor %d; want 3 and 7 kept below floor 12", len(r.Index), r.Index[:3], r.IndexFloor)
	}
	// Only pinned entries left: nothing more to drop, so saveRecord stops.
	r.Index = r.Index[:2]
	if n := r.DropIndex(indexDropStep); n != 0 || len(r.Index) != 2 {
		t.Fatalf("DropIndex = %d, index %v; want the pinned entries kept", n, r.Index)
	}
	// The open phase's phase.begin is kept too.
	r.Open = &OpenPhase{BeginSeq: 150}
	r.Index = []IndexEntry{{Seq: 3}, {Seq: 7}, {Seq: 140}, {Seq: 150}, {Seq: 160}}
	if r.DropIndex(2); !slices.Equal(r.Index, []IndexEntry{{Seq: 3}, {Seq: 7}, {Seq: 150}}) || r.IndexFloor != 160 {
		t.Fatalf("index %v, floor %d; want 3, 7 and 150 kept", r.Index, r.IndexFloor)
	}
	// A record from before BeginSeq was recorded pins nothing.
	old := IntentRecord{Executed: &ExecutedPlan{EndSeq: 7}, Index: []IndexEntry{{Seq: 7}, {Seq: 9}}}
	if n := old.DropIndex(1); n != 1 || old.Index[0].Seq != 9 {
		t.Fatalf("DropIndex = %d, index %v; want the oldest dropped", n, old.Index)
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

func phaseEndsFor(env *testEnv, intent string) []audit.Entry {
	var out []audit.Entry
	for _, e := range env.entries() {
		if e.Type == audit.TypePhaseEnd && e.Intent == intent {
			out = append(out, e)
		}
	}
	return out
}

func endBeginSeq(t *testing.T, e audit.Entry) uint64 {
	t.Helper()
	var p phaseEnd
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Outcome != outcomeInterrupted {
		t.Fatalf("phase.end outcome = %q, want interrupted", p.Outcome)
	}
	return p.BeginSeq
}

func TestBeginPhaseClosesAPredecessorsOpenPhase(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			in := intentWithPlan(t, twoSteps())
			h := in.Status.Plan.Hash
			env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
			ctx := context.Background()
			var oldSeq uint64
			if live {
				if _, err := env.begin(h); err != nil {
					t.Fatal(err)
				}
				rec, _ := env.g.state.LoadAny(ctx, envKey)
				oldSeq = rec.Open.BeginSeq
				rec.IntentUID = testOldUID
				if err := env.g.state.Save(ctx, rec); err != nil {
					t.Fatal(err)
				}
			} else {
				oldSeq = 99
				rec, _ := env.g.state.LoadAny(ctx, envKey)
				rec.IntentUID = testOldUID
				rec.Open = &OpenPhase{Stage: string(testStage), SubjectHash: h, BeginSeq: oldSeq, Epoch: "other-epoch"}
				if err := env.g.state.Save(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := env.begin(h); err != nil {
				t.Fatalf("BeginPhase for the recreated Intent = %v", err)
			}
			ends := phaseEndsFor(env, envKey)
			if len(ends) != 1 || endBeginSeq(t, ends[0]) != oldSeq {
				t.Fatalf("phase.end entries = %d, want one interrupted end of beginSeq %d", len(ends), oldSeq)
			}
			rec, _ := env.g.state.LoadAny(ctx, envKey)
			if rec.IntentUID != envIntentUID || rec.Open == nil || rec.Open.BeginSeq == oldSeq {
				t.Fatalf("record = %+v", rec.IntentRecord)
			}
		})
	}
}

func TestAResetRecordReportsTruncationAndHidesThePredecessorsIndex(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	env := newTestEnv(t, in, allowAll())
	ctx := context.Background()
	rec, _ := env.g.state.LoadAny(ctx, envKey)
	rec.IntentUID = testOldUID
	rec.AddIndex([]IndexEntry{{Seq: 5, Type: audit.TypePhaseBegin}, {Seq: 8, Type: audit.TypePhaseBegin}})
	if err := env.g.state.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := env.g.state.Load(ctx, envKey, envIntentUID)
	if err != nil || got.IndexFloor != 8 || len(got.Index) != 0 {
		t.Fatalf("reset record = %+v, %v; want floor 8 and no index", got.IntentRecord, err)
	}
	resp, err := env.query(&gatewayv1.QueryAuditRequest{Intent: &gatewayv1.IntentRef{Namespace: envNS, Name: envIntent}})
	if err != nil || len(resp.Entries) != 0 || !resp.Truncated {
		t.Fatalf("QueryAudit = %+v, %v; want no entries, truncated", resp, err)
	}
}

func TestLoadRefusesAnEmptyUID(t *testing.T) {
	s := NewStateStore(fake.NewClientset(), GRPCAudience)
	if _, err := s.Load(context.Background(), "a/b", ""); err == nil {
		t.Fatal("Load with an empty UID must fail")
	}
}

func TestLeadClosesInterruptedPhases(t *testing.T) {
	ie := newImageEnv(t, imageOp(nil))
	ctx := context.Background()
	save := func(k string, open *OpenPhase) {
		r, _ := ie.g.state.LoadAny(ctx, k)
		r.Open = open
		if err := ie.g.state.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	save("team-x/one", &OpenPhase{Stage: string(testStage), SubjectHash: testHashA, BeginSeq: 11, Epoch: "old-epoch"})
	save("team-x/two", &OpenPhase{Stage: string(testStage), SubjectHash: testHashA, BeginSeq: 12, Epoch: "old-epoch"})
	save("team-x/same", &OpenPhase{Stage: string(testStage), SubjectHash: testHashA, BeginSeq: 13, Epoch: ie.g.epoch})
	save("team-x/idle", nil)
	// A corrupt record and a ConfigMap that is no record must not stop the lead.
	for name, data := range map[string]string{recordName("team-x/bad"): "{not json", "someone-elses": "x"} {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: GRPCAudience}, Data: map[string]string{stateKey: data}}
		if name == recordName("team-x/bad") {
			cm.Labels = map[string]string{labelComponent: stateComponent}
		}
		if _, err := ie.kube.CoreV1().ConfigMaps(GRPCAudience).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	idleBefore, _ := ie.g.state.LoadAny(ctx, "team-x/idle")
	if err := ie.g.prepareLeading(ctx, ie.g.writer.Load()); err != nil {
		t.Fatalf("prepareLeading = %v; a bad record must not fail the lead", err)
	}
	for k, seq := range map[string]uint64{"team-x/one": 11, "team-x/two": 12} {
		r, _ := ie.g.state.LoadAny(ctx, k)
		ends := phaseEndsFor(ie.testEnv, k)
		if r.Open != nil || len(ends) != 1 || endBeginSeq(t, ends[0]) != seq {
			t.Fatalf("%s: open = %v, phase.end entries = %d; want closed as interrupted for beginSeq %d", k, r.Open, len(ends), seq)
		}
	}
	if r, _ := ie.g.state.LoadAny(ctx, "team-x/same"); r.Open == nil || len(phaseEndsFor(ie.testEnv, "team-x/same")) != 0 {
		t.Fatal("a phase of this epoch must be left alone")
	}
	if r, _ := ie.g.state.LoadAny(ctx, "team-x/idle"); r.cm.ResourceVersion != idleBefore.cm.ResourceVersion {
		t.Fatal("a record with no open phase must not be rewritten")
	}
}

func TestBeginPhaseDoesNotWaitForAPredecessorsRunningStep(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	ctx := context.Background()
	if _, err := env.begin(h); err != nil {
		t.Fatal(err)
	}
	rec, _ := env.g.state.LoadAny(ctx, envKey)
	rec.IntentUID = testOldUID
	if err := env.g.state.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	p := env.g.phases.get(envKey)
	p.mu.Lock() // a step of the old phase is running
	before := len(env.entries())
	if _, err := env.begin(h); status.Code(err) != codes.Aborted {
		t.Fatalf("BeginPhase = %v, want Aborted", err)
	}
	if len(env.entries()) != before {
		t.Fatal("an entry was written while the old step ran")
	}
	p.mu.Unlock()
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase after the step ended = %v", err)
	}
}

func TestARecreatedIntentDoesNotSeeThePredecessorsPendingIndex(t *testing.T) {
	in := intentWithPlan(t, twoSteps())
	h := in.Status.Plan.Hash
	env := newTestEnv(t, in, allowAll(), approvalFor("ok", h, lastv1alpha1.DecisionApprove, approver))
	ctx := context.Background()
	if _, err := env.begin(h); err != nil { // the index is not flushed
		t.Fatal(err)
	}
	var cur lastv1alpha1.Intent
	if err := env.crd.Get(ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
		t.Fatal(err)
	}
	cur.UID = testRecreatedUID
	if err := env.crd.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	// The approval is the old Intent's, so approve again for the new UID.
	fresh := approvalFor("ok2", h, lastv1alpha1.DecisionApprove, approver)
	fresh.Spec.IntentUID = testRecreatedUID
	if err := env.crd.Create(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := env.begin(h); err != nil {
		t.Fatalf("BeginPhase for the recreated Intent = %v", err)
	}
	resp, err := env.query(&gatewayv1.QueryAuditRequest{Intent: &gatewayv1.IntentRef{Namespace: envNS, Name: envIntent}})
	if err != nil || !resp.Truncated {
		t.Fatalf("QueryAudit = %+v, %v; want truncated", resp, err)
	}
	ends := phaseEndsFor(env, envKey)
	if len(ends) != 1 {
		t.Fatalf("%d phase.end entries, want the predecessor's interrupted one", len(ends))
	}
	for _, e := range resp.Entries {
		if e.GetSeq() <= ends[0].Seq { // the predecessor's phase.begin and interrupted phase.end
			t.Fatalf("predecessor entry %d served to the recreated Intent", e.GetSeq())
		}
	}
}

// deleteIntent deletes the env's Intent; it holds no finalizer, so it is gone at once.
func (e *testEnv) deleteIntent() {
	e.t.Helper()
	if err := e.crd.Delete(e.ctx, &lastv1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: envIntent}}); err != nil {
		e.t.Fatal(err)
	}
}

func TestSweepDeletesTheRecordOfAGoneIntent(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	env.deleteIntent()
	env.indexedEntry() // a pending index entry of the gone Intent
	env.g.sweepRecords(env.ctx)
	if env.recordExists() {
		t.Fatal("the record of a deleted Intent survived the sweep")
	}
	if pending := env.pendingIndex(); len(pending) != 0 {
		t.Fatalf("pending index entries of the gone Intent = %v", pending)
	}
	env.g.flushIndexes(env.ctx)
	if env.recordExists() {
		t.Fatal("a flush recreated the swept record")
	}
}

func TestSweepDeletesTheRecordOfAnotherUID(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	env.deleteIntent()
	recreated := intentWithPlan(t, twoSteps())
	recreated.UID = recreatedUID
	if err := env.crd.Create(env.ctx, recreated); err != nil {
		t.Fatal(err)
	}
	env.g.sweepRecords(env.ctx)
	if env.recordExists() {
		t.Fatal("the predecessor's record survived the sweep")
	}
}

func TestSweepKeepsALiveIntentsRecord(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	env.g.sweepRecords(env.ctx)
	if !env.recordExists() {
		t.Fatal("the sweep deleted the record of the live Intent")
	}
	// A record with no UID yet is adopted by the Intent's next Load; it stays too.
	rec, err := env.g.state.LoadAny(env.ctx, envKey)
	if err != nil {
		t.Fatal(err)
	}
	rec.IntentUID = ""
	if err := env.g.state.Save(env.ctx, rec); err != nil {
		t.Fatal(err)
	}
	env.g.sweepRecords(env.ctx)
	if !env.recordExists() {
		t.Fatal("the sweep deleted an unbound record of an existing Intent")
	}
}

func TestSweepKeepsARecordWithAnOpenPhase(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	if _, err := env.begin(in.Status.Plan.Hash); err != nil {
		t.Fatal(err)
	}
	env.deleteIntent()
	env.g.sweepRecords(env.ctx)
	if !env.recordExists() {
		t.Fatal("the sweep deleted a record with an open phase")
	}
}

func TestSweepKeepsTheRecordWhenTheIntentCannotBeRead(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	env.deleteIntent()
	env.g.crd = interceptor.NewClient(env.crd.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("api server unavailable")
		},
	})
	env.g.sweepRecords(env.ctx)
	if !env.recordExists() {
		t.Fatal("the sweep deleted a record whose Intent it could not read")
	}
}

func TestSweepSkipsABusyIntent(t *testing.T) {
	env, in := newRunEnv(t, twoSteps())
	executeAll(t, env, in)
	env.deleteIntent()
	unlock := env.g.locks.lock(envKey)
	env.g.sweepRecords(env.ctx)
	unlock()
	if !env.recordExists() {
		t.Fatal("the sweep deleted a record while its Intent's lock was held")
	}
	env.g.sweepRecords(env.ctx)
	if env.recordExists() {
		t.Fatal("the next sweep kept the record of the deleted Intent")
	}
}
