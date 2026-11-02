package gateway

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

func decidedSuggestion(d lastv1alpha1.SuggestionDecision) *lastv1alpha1.CommandRuleSuggestion {
	s := &lastv1alpha1.CommandRuleSuggestion{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: envNS, UID: types.UID("sug-uid-s1"), ResourceVersion: "7"},
		Spec: lastv1alpha1.CommandRuleSuggestionSpec{TargetScope: lastv1alpha1.RuleScopeNamespace,
			Trigger: lastv1alpha1.TriggerPlanTime, Decision: d,
			Proposed: ruleSpec(create, kubectlBin, "get", "ns", "*")},
	}
	if d != lastv1alpha1.SuggestionPending {
		s.Spec.DecidedBy, s.Spec.DecisionReason = approver, "looks fine"
	}
	return s
}

func decidedEntries(e *testEnv) []audit.Entry {
	var out []audit.Entry
	for _, en := range e.entries() {
		if en.Type == audit.TypeSuggestionDecided {
			out = append(out, en)
		}
	}
	return out
}

func record(t *testing.T, e *testEnv) error {
	t.Helper()
	return e.g.recordDecisions(context.Background(), e.g.writer.Load())
}

func TestDecidedSuggestionIsAuditedOnce(t *testing.T) {
	s := decidedSuggestion(lastv1alpha1.SuggestionAccepted)
	e := newTestEnv(t, s)
	for range 2 {
		if err := record(t, e); err != nil {
			t.Fatal(err)
		}
	}
	got := decidedEntries(e)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Actor != "gateway" || got[0].Intent != "" {
		t.Errorf("actor=%q intent=%q", got[0].Actor, got[0].Intent)
	}
	var p suggestionDecided
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	canon, _ := audit.Canonical(s.Spec.Proposed)
	if p.TargetScope != "Namespace" || p.Namespace != envNS || p.Name != "s1" || p.UID != "sug-uid-s1" ||
		p.ResourceVersion == "" || p.Trigger != "PlanTime" || p.Decision != "Accepted" ||
		p.DecidedBy != approver || p.DecisionReason != "looks fine" || string(p.Proposed) != string(canon) ||
		len(p.ProposedHash) != len("sha256:")+64 {
		t.Errorf("payload = %+v", p)
	}
}

func TestPendingSuggestionIsNotAudited(t *testing.T) {
	e := newTestEnv(t, decidedSuggestion(lastv1alpha1.SuggestionPending))
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	if n := len(decidedEntries(e)); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}

func TestRecordedDecisionsSurviveARestart(t *testing.T) {
	e := newTestEnv(t, decidedSuggestion(lastv1alpha1.SuggestionDenied))
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	e.g = e.newGateway()
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	if n := len(decidedEntries(e)); n != 1 {
		t.Fatalf("entries = %d, want 1", n)
	}
}

func TestAWriteFailureLeavesTheUIDUnrecorded(t *testing.T) {
	e := newTestEnv(t, decidedSuggestion(lastv1alpha1.SuggestionAccepted))
	e.sink.SetFailPuts(true)
	if err := record(t, e); err == nil {
		t.Fatal("want an error")
	}
	e.sink.SetFailPuts(false)
	e.g = e.newGateway() // a failed append breaks the writer until the gateway restarts
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	if n := len(decidedEntries(e)); n != 1 {
		t.Fatalf("entries = %d, want 1", n)
	}
}
