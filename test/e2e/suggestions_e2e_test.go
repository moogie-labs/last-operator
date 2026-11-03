//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

// suggested waits until the Intent ns/name awaits approval of a Create plan its
// PlanTime suggestion step has run for.
func (d *day2) suggested(name string) lastv1alpha1.Intent {
	d.t.Helper()
	var in lastv1alpha1.Intent
	waitFor(d.t, 5*time.Minute, "Intent "+name+" to await approval with its suggestions made", func() (bool, string) {
		if err := d.crc.Get(context.Background(), client.ObjectKey{Namespace: d.ns, Name: name}, &in); err != nil {
			return false, err.Error()
		}
		ok := in.Status.Stage == lastv1alpha1.StageCreate && in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval &&
			in.Status.Plan != nil && in.Status.Plan.Suggested
		return ok, fmt.Sprintf("stage %q, phase %q, plan %v", in.Status.Stage, in.Status.Phase, in.Status.Plan != nil)
	})
	return in
}

// suggestion reads the suggestion ns/name.
func (d *day2) suggestion(name string) lastv1alpha1.CommandRuleSuggestion {
	d.t.Helper()
	var s lastv1alpha1.CommandRuleSuggestion
	if err := d.crc.Get(context.Background(), client.ObjectKey{Namespace: d.ns, Name: name}, &s); err != nil {
		d.t.Fatalf("suggestion %s/%s: %v", d.ns, name, err)
	}
	return s
}

// suggestionEvents returns the suggestion events about Intent name, by reason,
// each counted as often as its series says the recorder saw it: the recorder
// folds events that differ only in their note into one series.
func suggestionEvents(t *testing.T, ns, name string) (map[string]int, []eventsv1.Event) {
	t.Helper()
	_, kube := e2eClients(t)
	list, err := kube.EventsV1().Events(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var evs []eventsv1.Event
	for _, e := range list.Items {
		if e.Regarding.Kind != "Intent" || e.Regarding.Name != name || !strings.HasPrefix(e.Reason, "Suggestion") {
			continue
		}
		counts[e.Reason] += seriesCount(e)
		evs = append(evs, e)
	}
	return counts, evs
}

// TestDeniedSuggestionIsNotRepeated is the M7 done-criterion (M7 design §1, §10).
// Intents A, B and C have the same plan shape, an nginx Deployment. A's
// PlanTime suggestion P lets the Deployment's name vary, and a person denies it.
// B's model proposes P again and P+, broader still; the denial suppresses both.
// C's model proposes N, which has C's own name where P has [!-]*; N is narrower
// than P, so it is offered, and a person accepts it. Both decisions reach the
// audit chain, which verifies.
func TestDeniedSuggestionIsNotRepeated(t *testing.T) {
	d := newDay2(t, "e2e-sugg-")
	ctx := context.Background()
	user := kubectl(t, "auth", "whoami", "-o", "jsonpath={.status.userInfo.username}")
	if user == "" {
		t.Fatal("kubectl auth whoami returned no username")
	}

	// The patterns are for the plan's first step, kubectl create deployment <name> --image=... -n <ns>.
	p := []string{"kubectl", "create", "deployment", "[!-]*", "--image=" + nginxImage, "-n", d.ns}
	pPlus := []string{"kubectl", "create", "deployment", "[!-]*", "..."}
	n := []string{"kubectl", "create", "deployment", "web-c", "--image=" + nginxImage, "-n", d.ns}
	for _, name := range []string{"web-a", "web-b", "web-c"} {
		pl, _ := d.nginxPlan(name)
		d.scriptFor(name, pl)
	}
	d.suggestFor("web-a", "create", p)
	d.suggestFor("web-b", "create", p, pPlus)
	d.suggestFor("web-c", "create", n)

	// A: P is suggested, and a person denies it.
	d.request("web-a")
	inA := d.suggested("web-a")
	if len(inA.Status.SuggestedRules) != 1 {
		t.Fatalf("Intent A suggestedRules = %v, want one suggestion", inA.Status.SuggestedRules)
	}
	nameA := inA.Status.SuggestedRules[0]
	sA := d.suggestion(nameA)
	if !slices.Equal(sA.Spec.Proposed.Match.Argv, p) || sA.Spec.Decision != lastv1alpha1.SuggestionPending ||
		sA.Spec.Trigger != lastv1alpha1.TriggerPlanTime || sA.Spec.TargetScope != lastv1alpha1.RuleScopeNamespace ||
		sA.Labels["last.moogie-labs.com/intent"] != "web-a" {
		t.Fatalf("suggestion %s = match %v, decision %q, trigger %q, scope %q, labels %v; want P, Pending, PlanTime, Namespace, intent web-a",
			nameA, sA.Spec.Proposed.Match.Argv, sA.Spec.Decision, sA.Spec.Trigger, sA.Spec.TargetScope, sA.Labels)
	}
	wantStages := []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageRemediate}
	if !slices.Equal(sA.Spec.Proposed.Stages, wantStages) {
		t.Errorf("suggestion %s stages = %v, want %v", nameA, sA.Spec.Proposed.Stages, wantStages)
	}
	out, err := runLast(nil, "rules", "suggestions", "-n", d.ns)
	if err != nil || !strings.Contains(out, nameA) {
		t.Fatalf("kubectl last rules suggestions = %v:\n%s", err, out)
	}
	const denyReason = "too broad: any Deployment name"
	out, err = runLast(nil, "rules", "suggestions", "deny", nameA, "-n", d.ns, "--reason", denyReason)
	if err != nil {
		t.Fatalf("kubectl last rules suggestions deny: %v\n%s", err, out)
	}
	t.Logf("kubectl last rules suggestions deny:\n%s", out)
	sA = d.suggestion(nameA)
	if sA.Spec.Decision != lastv1alpha1.SuggestionDenied || sA.Spec.DecidedBy != user || sA.Spec.DecisionReason != denyReason {
		t.Fatalf("suggestion %s after deny = decision %q by %q (%q); want Denied by %s (%q)",
			nameA, sA.Spec.Decision, sA.Spec.DecidedBy, sA.Spec.DecisionReason, user, denyReason)
	}

	// B and C plan after the denial.
	d.request("web-b")
	d.request("web-c")

	// B: P and P+ are both suppressed by the denial of A's suggestion, and nothing is created.
	inB := d.suggested("web-b")
	if len(inB.Status.SuggestedRules) != 0 {
		t.Errorf("Intent B suggestedRules = %v, want none", inB.Status.SuggestedRules)
	}
	var evsB []eventsv1.Event
	var countsB map[string]int
	waitFor(t, 2*time.Minute, "two SuggestionSuppressed events for Intent B", func() (bool, string) {
		countsB, evsB = suggestionEvents(t, d.ns, "web-b")
		return countsB["SuggestionSuppressed"] >= 2, fmt.Sprint(countsB)
	})
	if countsB["SuggestionSuppressed"] != 2 || len(countsB) != 1 {
		t.Errorf("suggestion events for Intent B = %v, want exactly two SuggestionSuppressed", countsB)
	}
	for _, e := range evsB {
		t.Logf("Intent B event %s (count %d): %s", e.Reason, seriesCount(e), e.Note)
		if e.Reason != "SuggestionSuppressed" {
			continue
		}
		if e.Related == nil || e.Related.Name != nameA || !strings.Contains(e.Note, d.ns+"/"+nameA) {
			t.Errorf("SuggestionSuppressed event for Intent B relates to %+v with note %q; want suggestion %s", e.Related, e.Note, nameA)
		}
	}
	var all lastv1alpha1.CommandRuleSuggestionList
	if err := d.crc.List(ctx, &all, client.InNamespace(d.ns)); err != nil {
		t.Fatal(err)
	}
	for _, s := range all.Items {
		if s.Labels["last.moogie-labs.com/intent"] == "web-b" {
			t.Errorf("suggestion %s was created for Intent B: match %v", s.Name, s.Spec.Proposed.Match.Argv)
		}
	}

	// C: N is narrower than P, so it is suggested; a person accepts it.
	inC := d.suggested("web-c")
	if len(inC.Status.SuggestedRules) != 1 {
		t.Fatalf("Intent C suggestedRules = %v, want one suggestion", inC.Status.SuggestedRules)
	}
	nameC := inC.Status.SuggestedRules[0]
	if sC := d.suggestion(nameC); !slices.Equal(sC.Spec.Proposed.Match.Argv, n) || sC.Spec.Decision != lastv1alpha1.SuggestionPending {
		t.Fatalf("suggestion %s = match %v, decision %q; want N, Pending", nameC, sC.Spec.Proposed.Match.Argv, sC.Spec.Decision)
	}
	out, err = runLast(nil, "rules", "suggestions", "accept", nameC, "-n", d.ns, "--yes")
	if err != nil {
		t.Fatalf("kubectl last rules suggestions accept --yes: %v\n%s", err, out)
	}
	t.Logf("kubectl last rules suggestions accept --yes:\n%s", out)
	var rule lastv1alpha1.CommandRule
	if err := d.crc.Get(ctx, client.ObjectKey{Namespace: d.ns, Name: nameC}, &rule); err != nil {
		t.Fatalf("CommandRule %s/%s after accept: %v", d.ns, nameC, err)
	}
	src := rule.Spec.Source
	if !slices.Equal(rule.Spec.Match.Argv, n) || src == nil || src.CreatedFrom != lastv1alpha1.RuleOriginSuggestion ||
		src.SuggestionRef == nil || src.SuggestionRef.Name != nameC || src.CreatedBy != user {
		t.Fatalf("CommandRule %s = match %v, source %+v; want N, created from suggestion %s by %s", nameC, rule.Spec.Match.Argv, src, nameC, user)
	}
	sC := d.suggestion(nameC)
	if sC.Spec.Decision != lastv1alpha1.SuggestionAccepted || sC.Spec.DecidedBy != user {
		t.Fatalf("suggestion %s after accept = decision %q by %q; want Accepted by %s", nameC, sC.Spec.Decision, sC.Spec.DecidedBy, user)
	}

	// Audit: suggestion.decided has no Intent, so QueryAudit cannot see it; the
	// chain is read with kubectl last audit log. The gateway records the seq of
	// each entry it wrote in last-suggestion-decisions.
	seqs := map[string]uint64{}
	waitFor(t, 3*time.Minute, "the gateway to audit both decisions", func() (bool, string) {
		var cm corev1.ConfigMap
		if err := d.crc.Get(ctx, client.ObjectKey{Namespace: "last-gateway", Name: "last-suggestion-decisions"}, &cm); err != nil {
			return false, err.Error()
		}
		for name, uid := range map[string]string{nameA: string(sA.UID), nameC: string(sC.UID)} {
			if v, ok := cm.Data[uid]; ok {
				seq, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					return false, fmt.Sprintf("seq %q for %s: %v", v, name, err)
				}
				seqs[name] = seq
			}
		}
		return len(seqs) == 2, fmt.Sprintf("audited %v", seqs)
	})
	chain := kubectl(t, "get", "cm", "-n", "last-gateway", "last-audit-config", "-o", "jsonpath={.data.chainId}")
	from := min(seqs[nameA], seqs[nameC])
	out, err = runLast(auditStoreEnv(t, chain), "audit", "log", "--type", audit.TypeSuggestionDecided,
		"--from-seq", strconv.FormatUint(from, 10), "--json")
	if err != nil {
		t.Fatalf("kubectl last audit log: %v\n%s", err, out)
	}
	type decided struct {
		Namespace, Name, Decision, DecidedBy, DecisionReason string
	}
	found := map[string][]audit.Entry{}
	payloads := map[string]decided{}
	for line := range strings.Lines(out) {
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("kubectl last audit log --json line %q: %v", line, err)
		}
		var p decided
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("suggestion.decided payload at seq %d: %v", e.Seq, err)
		}
		if e.Type != audit.TypeSuggestionDecided || e.Intent != "" {
			t.Errorf("audit log --type %s printed a %s entry for Intent %q at seq %d", audit.TypeSuggestionDecided, e.Type, e.Intent, e.Seq)
		}
		if p.Namespace == d.ns {
			found[p.Name] = append(found[p.Name], e)
			payloads[p.Name] = p
		}
	}
	for name, want := range map[string]decided{
		nameA: {Namespace: d.ns, Name: nameA, Decision: string(lastv1alpha1.SuggestionDenied), DecidedBy: user, DecisionReason: denyReason},
		nameC: {Namespace: d.ns, Name: nameC, Decision: string(lastv1alpha1.SuggestionAccepted), DecidedBy: user},
	} {
		if len(found[name]) != 1 || found[name][0].Seq != seqs[name] {
			t.Errorf("suggestion.decided entries for %s = %d, want one at seq %d", name, len(found[name]), seqs[name])
			continue
		}
		if payloads[name] != want {
			t.Errorf("suggestion.decided for %s = %+v, want %+v", name, payloads[name], want)
		}
	}
	t.Logf("denied %s (P), suppressed P and P+ for Intent B, accepted %s (N) as %s; suggestion.decided at seqs %v", nameA, nameC, user, seqs)
	verifyAuditChain(t)
}

// seriesCount is how often the recorder saw e.
func seriesCount(e eventsv1.Event) int {
	if e.Series == nil {
		return 1
	}
	return int(e.Series.Count)
}
