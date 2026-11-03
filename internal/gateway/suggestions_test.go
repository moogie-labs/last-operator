package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// decisionUIDs returns the UIDs last-suggestion-decisions holds.
func decisionUIDs(t *testing.T, e *testEnv) []string {
	t.Helper()
	cm, err := e.kube.CoreV1().ConfigMaps(ns).Get(context.Background(), suggestionDecisionsName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(cm.Data))
}

func TestADeletedSuggestionIsPrunedFromTheRecord(t *testing.T) {
	const uid2 = "sug-uid-s2"
	s1 := decidedSuggestion(lastv1alpha1.SuggestionAccepted)
	s2 := decidedSuggestion(lastv1alpha1.SuggestionDenied)
	s2.Name, s2.UID = "s2", uid2
	e := newTestEnv(t, s1, s2)
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	if got := decisionUIDs(t, e); !slices.Equal(got, []string{"sug-uid-s1", uid2}) {
		t.Fatalf("recorded = %v", got)
	}
	if err := e.crd.Delete(context.Background(), s1); err != nil {
		t.Fatal(err)
	}
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	if got := decisionUIDs(t, e); !slices.Equal(got, []string{uid2}) {
		t.Fatalf("recorded after s1 went = %v, want only s2's", got)
	}
	if n := len(decidedEntries(e)); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
}

// pagedSuggestions serves suggestion lists two at a time, by name, whatever
// limit is asked for, as an API server may, and counts the calls.
func pagedSuggestions(base client.WithWatch, calls *int) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			sl, ok := list.(*lastv1alpha1.CommandRuleSuggestionList)
			if !ok {
				return c.List(ctx, list, opts...)
			}
			*calls++
			o := (&client.ListOptions{}).ApplyOptions(opts)
			if o.Limit <= 0 {
				return errors.New("listed suggestions without a limit")
			}
			if err := c.List(ctx, sl); err != nil {
				return err
			}
			slices.SortFunc(sl.Items, func(a, b lastv1alpha1.CommandRuleSuggestion) int { return strings.Compare(a.Name, b.Name) })
			from := 0
			if o.Continue != "" {
				from, _ = strconv.Atoi(o.Continue)
			}
			total := len(sl.Items)
			to := min(from+2, total)
			sl.Items, sl.Continue = sl.Items[from:to], ""
			if to < total {
				sl.Continue = strconv.Itoa(to)
			}
			return nil
		},
	})
}

func TestRecordDecisionsReadsEveryPage(t *testing.T) {
	objs := make([]client.Object, 0, 5)
	for i := range 5 {
		s := decidedSuggestion(lastv1alpha1.SuggestionAccepted)
		s.Name, s.UID = fmt.Sprintf("s%d", i), types.UID(fmt.Sprintf("sug-uid-%d", i))
		objs = append(objs, s)
	}
	e := newTestEnv(t, objs...)
	calls := 0
	e.crd = pagedSuggestions(e.crd.(client.WithWatch), &calls)
	e.g = e.newGateway()
	if err := record(t, e); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("list calls = %d, want 3 pages", calls)
	}
	if n := len(decidedEntries(e)); n != 5 {
		t.Fatalf("entries = %d, want 5", n)
	}
	if got := decisionUIDs(t, e); len(got) != 5 {
		t.Fatalf("recorded = %v, want all 5", got)
	}
}
