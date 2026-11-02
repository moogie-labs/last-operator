//nolint:goconst // test data repeats words
package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	suggName  = "sugg-0123456789"
	verbSugg  = "suggestions"
	verbRules = "rules"
	sysNS     = "last-system"
)

var getProbe = []string{"kubectl", "get", "deployment", webName}

// testSuggestion is a Pending PlanTime suggestion in testNS whose rule covers
// rulePlan's probe "up" (kubectl get deployment web) at Create.
func testSuggestion(change func(*lastv1alpha1.CommandRuleSuggestion)) *lastv1alpha1.CommandRuleSuggestion {
	ex := argvCommand(getProbe...)
	ex.Providers = []string{k8sProv}
	s := &lastv1alpha1.CommandRuleSuggestion{
		ObjectMeta: metav1.ObjectMeta{Name: suggName, Namespace: testNS},
		Spec: lastv1alpha1.CommandRuleSuggestionSpec{
			TargetScope: lastv1alpha1.RuleScopeNamespace,
			Trigger:     lastv1alpha1.TriggerPlanTime,
			Rationale:   "The probe of web keeps needing approval.\nThis rule covers it.",
			Proposed: lastv1alpha1.CommandRuleSpec{
				Stages:    []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageRemediate},
				Providers: []string{k8sProv},
				Match:     lastv1alpha1.ArgvPattern{Argv: []string{"kubectl", "get", "deployment", "[!-]*"}},
			},
			Examples: []lastv1alpha1.Command{ex},
			Decision: lastv1alpha1.SuggestionPending,
		},
	}
	if change != nil {
		change(s)
	}
	return s
}

func getSugg(t *testing.T, c client.Client, ns string) lastv1alpha1.CommandRuleSuggestion {
	t.Helper()
	var s lastv1alpha1.CommandRuleSuggestion
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: suggName}, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func wantSource(name string) *lastv1alpha1.RuleSource {
	return &lastv1alpha1.RuleSource{CreatedFrom: lastv1alpha1.RuleOriginSuggestion,
		SuggestionRef: &lastv1alpha1.LocalRef{Name: name}, CreatedBy: testUser}
}

func TestListShowsPendingWithOwnLint(t *testing.T) {
	pending := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Spec.Proposed.Match.Argv = []string{"kubectl", "get", "*"} // one finding: * also matches flags
		s.Spec.BreadthWarnings = []string{"operator says it is fine", "b", "c"}
	})
	denied := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Name = "sugg-denied0000"
		s.Spec.Decision, s.Spec.DecidedBy = lastv1alpha1.SuggestionDenied, "bob"
	})
	elsewhere := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) { s.Name, s.Namespace = "sugg-elsewhere0", "other" })
	c := fakeClient(pending, denied, elsewhere)

	d, out := depsFor(c, "")
	if err := execute(d, verbRules, verbSugg); err != nil {
		t.Fatalf("list = %v\n%s", err, out)
	}
	got := out.String()
	for _, want := range []string{suggName, "Namespace", "PlanTime", "kubectl get *", "Create,Remediate", k8sProv, "1 warning"} {
		if !strings.Contains(got, want) {
			t.Errorf("list lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"operator says", "3 warning", "sugg-denied", "sugg-elsewhere"} {
		if strings.Contains(got, not) {
			t.Errorf("list shows %q:\n%s", not, got)
		}
	}

	d, out = depsFor(c, "")
	if err := execute(d, verbRules, verbSugg, "--all"); err != nil {
		t.Fatalf("list --all = %v\n%s", err, out)
	}
	if got := out.String(); !strings.Contains(got, "sugg-denied") || !strings.Contains(got, "Denied") || strings.Contains(got, "sugg-elsewhere") {
		t.Errorf("list --all:\n%s", got)
	}

	d, out = depsFor(c, "")
	if err := execute(d, verbRules, verbSugg, "-A"); err != nil {
		t.Fatalf("list -A = %v\n%s", err, out)
	}
	if got := out.String(); !strings.Contains(got, "sugg-elsewhere") || !strings.Contains(got, "other") || strings.Contains(got, "sugg-denied") {
		t.Errorf("list -A:\n%s", got)
	}
}

func TestShowLabelsOperatorTextAndBreadth(t *testing.T) {
	s := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Spec.Proposed.Match.Argv = []string{"kubectl", "get", "[!-]*", "[!-]*"}
		s.Spec.Examples = append(s.Spec.Examples, argvCommand("kubectl", "get", "pods", "-A"))
		s.Spec.Rationale = "Line one \x1b[31mDanger\x1b[0m.\nLine two."
	})
	c := fakeClient(s)
	d, out := depsFor(c, "")
	if err := execute(d, verbRules, verbSugg, "show", suggName); err != nil {
		t.Fatalf("show = %v\n%s", err, out)
	}
	got := out.String()
	for _, want := range []string{
		"operator-proposed",
		`match[2] "[!-]*" is broader than "deployment"`,
		"stage Remediate is not in the plain rule",
		"matches          | kubectl get deployment web",
		"does not match   | kubectl get pods -A",
		"  | Line one Danger.\n  | Line two.\n",
		"kubectl get [!-]* [!-]*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("show printed an escape:\n%q", got)
	}
	if getSugg(t, c, testNS).Spec.Decision != lastv1alpha1.SuggestionPending {
		t.Error("show changed the decision")
	}

	// Without examples there is nothing to compare with, and show says so.
	c = fakeClient(testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) { s.Spec.Examples = nil }))
	d, out = depsFor(c, "")
	if err := execute(d, verbRules, verbSugg, "show", suggName); err != nil {
		t.Fatalf("show = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "no examples") || strings.Contains(out.String(), "is not in the plain rule") {
		t.Errorf("show without examples:\n%s", out)
	}
}

// recordingClient records the kinds of the objects created and updated, in order.
func recordingClient(base client.WithWatch, log *[]string) client.Client {
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*log = append(*log, "create "+kindOf(obj))
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			*log = append(*log, "update "+kindOf(obj))
			return c.Update(ctx, obj, opts...)
		},
	})
}

func kindOf(obj client.Object) string {
	switch obj.(type) {
	case *lastv1alpha1.CommandRule:
		return "CommandRule"
	case *lastv1alpha1.ClusterCommandRule:
		return "ClusterCommandRule"
	case *lastv1alpha1.CommandRuleSuggestion:
		return "CommandRuleSuggestion"
	case *lastv1alpha1.IntentApproval:
		return "IntentApproval"
	}
	return "other"
}

func TestAcceptCreatesTheRuleThenDecides(t *testing.T) {
	var log []string
	base := fakeClient(testSuggestion(nil))
	d, out := depsFor(recordingClient(base, &log), "y\n")
	if err := execute(d, verbRules, verbSugg, "accept", suggName); err != nil {
		t.Fatalf("accept = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "operator-proposed") {
		t.Errorf("accept did not show the proposal first:\n%s", out)
	}
	items := nsRules(t, base)
	if len(items) != 1 || items[0].Name != suggName || items[0].Namespace != testNS {
		t.Fatalf("rules = %+v, want one named %s in %s", items, suggName, testNS)
	}
	want := testSuggestion(nil).Spec.Proposed
	want.Source = wantSource(suggName)
	if !reflect.DeepEqual(items[0].Spec, want) {
		t.Errorf("spec = %+v\nwant   %+v", items[0].Spec, want)
	}
	s := getSugg(t, base, testNS)
	if s.Spec.Decision != lastv1alpha1.SuggestionAccepted || s.Spec.DecidedBy != testUser || s.Spec.DecisionReason != "" {
		t.Errorf("suggestion = %+v", s.Spec)
	}
	if want := []string{"create CommandRule", "update CommandRuleSuggestion"}; !reflect.DeepEqual(log, want) {
		t.Errorf("calls = %v, want %v", log, want)
	}

	// Answering no does nothing.
	base = fakeClient(testSuggestion(nil))
	d, out = depsFor(base, "n\n")
	if err := execute(d, verbRules, verbSugg, "accept", suggName); err != nil {
		t.Fatalf("accept, answered no = %v\n%s", err, out)
	}
	if len(nsRules(t, base)) != 0 || getSugg(t, base, testNS).Spec.Decision != lastv1alpha1.SuggestionPending {
		t.Errorf("answering no changed something:\n%s", out)
	}

	// A Cluster suggestion lives in last-system and makes a ClusterCommandRule.
	base = fakeClient(testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Namespace, s.Spec.TargetScope, s.Spec.Trigger = sysNS, lastv1alpha1.RuleScopeCluster, lastv1alpha1.TriggerPeriodicReview
	}))
	d, out = depsFor(base, "")
	if err := execute(d, verbRules, verbSugg, "accept", suggName, "-n", sysNS, "--yes"); err != nil {
		t.Fatalf("accept cluster = %v\n%s", err, out)
	}
	cl := clusterRules(t, base)
	if len(cl) != 1 || cl[0].Name != suggName || !reflect.DeepEqual(cl[0].Spec.Source, wantSource(suggName)) {
		t.Fatalf("cluster rules = %+v", cl)
	}
	if !strings.Contains(out.String(), "operator-proposed") {
		t.Errorf("--yes did not still print the proposal:\n%s", out)
	}
	if s := getSugg(t, base, sysNS); s.Spec.Decision != lastv1alpha1.SuggestionAccepted || s.Spec.DecidedBy != testUser {
		t.Errorf("suggestion = %+v", s.Spec)
	}
}

func TestAcceptEditRelintsAndRecordsTheEdit(t *testing.T) {
	c := fakeClient(testSuggestion(nil))
	d, out := depsFor(c, "sh -c *\nkubectl get deployment web\n\n")
	if err := execute(d, verbRules, verbSugg, "accept", suggName, "--edit", "--yes"); err != nil {
		t.Fatalf("accept --edit = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "can vary") {
		t.Errorf("the code-varying edit was not refused:\n%s", out)
	}
	items := nsRules(t, c)
	if len(items) != 1 || !reflect.DeepEqual(items[0].Spec.Match.Argv, getProbe) {
		t.Fatalf("rules = %+v, want the edited match", items)
	}
	s := getSugg(t, c, testNS)
	if s.Spec.Decision != lastv1alpha1.SuggestionAccepted ||
		s.Spec.DecisionReason != "accepted with an edited match: kubectl get deployment web" {
		t.Errorf("suggestion = %+v", s.Spec)
	}

	// Keeping the proposed match is no edit.
	c = fakeClient(testSuggestion(nil))
	d, out = depsFor(c, "\n")
	if err := execute(d, verbRules, verbSugg, "accept", suggName, "--edit", "--yes"); err != nil {
		t.Fatalf("accept --edit = %v\n%s", err, out)
	}
	if s := getSugg(t, c, testNS); s.Spec.DecisionReason != "" {
		t.Errorf("reason = %q, want none", s.Spec.DecisionReason)
	}
}

func TestAcceptRerunAfterAFailedDecisionFinishes(t *testing.T) {
	base := fakeClient(testSuggestion(nil))
	failing := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*lastv1alpha1.CommandRuleSuggestion); ok {
				return errors.New("etcd hiccup")
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	d, out := depsFor(failing, "")
	err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes")
	if err == nil || !strings.Contains(err.Error(), "etcd hiccup") || !strings.Contains(err.Error(), "again") {
		t.Fatalf("accept = %v, want the update error and a pointer to run it again\n%s", err, out)
	}
	if len(nsRules(t, base)) != 1 || getSugg(t, base, testNS).Spec.Decision != lastv1alpha1.SuggestionPending {
		t.Fatalf("after the failed update: rules %v", nsRules(t, base))
	}
	d, out = depsFor(base, "")
	if err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes"); err != nil {
		t.Fatalf("second accept = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Errorf("second accept did not find the rule:\n%s", out)
	}
	if s := getSugg(t, base, testNS); s.Spec.Decision != lastv1alpha1.SuggestionAccepted || s.Spec.DecidedBy != testUser {
		t.Errorf("suggestion = %+v", s.Spec)
	}
	if n := len(nsRules(t, base)); n != 1 {
		t.Errorf("rules = %d, want 1", n)
	}
}

// decidedMeanwhile is a client on which bob decides the suggestion while the
// CLI creates the rule, so the CLI's decision update conflicts.
func decidedMeanwhile(t *testing.T, base client.WithWatch, decision lastv1alpha1.SuggestionDecision) client.Client {
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*lastv1alpha1.CommandRule); ok {
				s := getSugg(t, c, testNS)
				s.Spec.Decision, s.Spec.DecidedBy = decision, "bob"
				if err := c.Update(ctx, &s); err != nil {
					t.Fatal(err)
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

func TestConcurrentAcceptReportsTheOtherDecider(t *testing.T) {
	base := fakeClient(testSuggestion(nil))
	d, out := depsFor(decidedMeanwhile(t, base, lastv1alpha1.SuggestionAccepted), "")
	if err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes"); err != nil {
		t.Fatalf("accept = %v, want success: bob accepted it\n%s", err, out)
	}
	if !strings.Contains(out.String(), "already Accepted by bob") {
		t.Errorf("output does not name bob:\n%s", out)
	}
	if s := getSugg(t, base, testNS); s.Spec.DecidedBy != "bob" {
		t.Errorf("decidedBy = %q, want bob's decision kept", s.Spec.DecidedBy)
	}

	base = fakeClient(testSuggestion(nil))
	d, out = depsFor(decidedMeanwhile(t, base, lastv1alpha1.SuggestionDenied), "")
	err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes")
	if err == nil || !strings.Contains(err.Error(), "Denied by bob") || !strings.Contains(err.Error(), suggName) {
		t.Fatalf("accept = %v, want the denial by bob reported\n%s", err, out)
	}
}

func TestAcceptWithoutRuleRBACLeavesItPending(t *testing.T) {
	var log []string
	base := fakeClient(testSuggestion(nil))
	forbidden := interceptor.NewClient(recordingClient(base, &log).(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*lastv1alpha1.CommandRule); ok {
				return apierrors.NewForbidden(schema.GroupResource{Group: "last.moogie-labs.com", Resource: "commandrules"}, suggName,
					errors.New("you may not create rules"))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	d, out := depsFor(forbidden, "")
	err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes")
	if err == nil || !strings.Contains(err.Error(), "you may not create rules") || !strings.Contains(err.Error(), "Pending") {
		t.Fatalf("accept = %v\n%s", err, out)
	}
	s := getSugg(t, base, testNS)
	if s.Spec.Decision != lastv1alpha1.SuggestionPending || s.Spec.DecidedBy != "" {
		t.Errorf("suggestion = %+v, want it left Pending", s.Spec)
	}
	if len(log) != 0 {
		t.Errorf("calls = %v, want none to get through", log)
	}
}

func TestAcceptRefusesCodeVaryingOrMatchNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		match []string
		why   string
	}{
		{"code varies", []string{"sh", "-c", "*"}, "can vary"},
		{"matches nothing", []string{"kubectl", "...", "get"}, "pattern matches nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClient(testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) { s.Spec.Proposed.Match.Argv = tc.match }))
			d, out := depsFor(c, "")
			err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes")
			if err == nil || !strings.Contains(err.Error(), tc.why) || !strings.Contains(err.Error(), "by hand") {
				t.Fatalf("accept = %v, want a refusal pointing at writing the rule by hand\n%s", err, out)
			}
			if len(nsRules(t, c)) != 0 || getSugg(t, c, testNS).Spec.Decision != lastv1alpha1.SuggestionPending {
				t.Error("a refused accept changed something")
			}
		})
	}
}

func TestDenySetsReasonAndDecider(t *testing.T) {
	c := fakeClient(testSuggestion(nil))
	d, out := depsFor(c, "")
	if err := execute(d, verbRules, verbSugg, "deny", suggName, "--reason", "broader than we want"); err != nil {
		t.Fatalf("deny = %v\n%s", err, out)
	}
	s := getSugg(t, c, testNS)
	if s.Spec.Decision != lastv1alpha1.SuggestionDenied || s.Spec.DecidedBy != testUser || s.Spec.DecisionReason != "broader than we want" {
		t.Errorf("suggestion = %+v", s.Spec)
	}
	if len(nsRules(t, c)) != 0 {
		t.Error("deny created a rule")
	}
	// Accepting a Denied suggestion is refused.
	d, out = depsFor(c, "")
	if err := execute(d, verbRules, verbSugg, "accept", suggName, "--yes"); err == nil || !strings.Contains(err.Error(), "Denied") {
		t.Errorf("accept of a Denied suggestion = %v\n%s", err, out)
	}
}

func TestApproveOffersSuggestedChoice(t *testing.T) {
	var log []string
	base := fakeClient(ruleIntent(), testSuggestion(nil))
	// s1 and s2 just this once; probe:up the suggestion; teardown:0.0 just this once.
	d, out := depsFor(recordingClient(base, &log), "o\no\ns\ny\no\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	got := out.String()
	if strings.Count(got, "[s]uggested") != 1 {
		t.Errorf("s is offered %d times, want once (for probe:up):\n%s", strings.Count(got, "[s]uggested"), got)
	}
	if !strings.Contains(got, "operator-proposed") || !strings.Contains(got, "stage Remediate is not in the plain rule") {
		t.Errorf("choosing s did not show the proposal against g:\n%s", got)
	}
	items := nsRules(t, base)
	if len(items) != 1 || items[0].Name != suggName || !reflect.DeepEqual(items[0].Spec.Source, wantSource(suggName)) {
		t.Fatalf("rules = %+v", items)
	}
	if s := getSugg(t, base, testNS); s.Spec.Decision != lastv1alpha1.SuggestionAccepted || s.Spec.DecidedBy != testUser {
		t.Errorf("suggestion = %+v", s.Spec)
	}
	if a := approvalsIn(t, base); len(a) != 1 {
		t.Errorf("approvals = %+v, want 1", a)
	}
	if want := []string{"create CommandRule", "update CommandRuleSuggestion", "create IntentApproval"}; !reflect.DeepEqual(log, want) {
		t.Errorf("calls = %v, want %v", log, want)
	}
}

func TestApproveDoesNotOfferRefusedSuggestions(t *testing.T) {
	refused := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Spec.Proposed.Match.Argv = []string{"kubectl", "get", "deployment", "[!-]*", "--", "sh", "-c", "..."}
		s.Spec.Examples = nil
	})
	in := ruleIntent()
	get := argvCommand("kubectl", "get", "deployment", webName, "--", "sh", "-c", "x")
	get.Providers = []string{k8sProv}
	in.Status.Plan.Resources[0].Probes[0].Command = get
	in.Status.Plan.Hash = planHash(in.Status.Plan)
	c := fakeClient(in, refused)
	d, out := depsFor(c, "o\no\no\no\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if strings.Contains(out.String(), "[s]uggested") || !strings.Contains(out.String(), "not offered") {
		t.Errorf("a refused suggestion was offered, or silently dropped:\n%s", out)
	}
}

func TestBatchAlwaysAllowNeverUsesSuggestions(t *testing.T) {
	for _, mode := range []string{"--always-allow=exact", "--always-allow=generalized"} {
		t.Run(mode, func(t *testing.T) {
			base := fakeClient(ruleIntent(), testSuggestion(nil))
			c := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*lastv1alpha1.CommandRuleSuggestionList); ok {
						t.Errorf("%s read suggestions", mode)
					}
					return c.List(ctx, list, opts...)
				},
			})
			d, out := depsFor(c, "")
			if err := execute(d, verbApprove, webName, mode); err != nil {
				t.Fatalf("approve = %v\n%s", err, out)
			}
			for _, r := range nsRules(t, base) {
				if r.Name == suggName || r.Spec.Source.CreatedFrom != lastv1alpha1.RuleOriginApproval {
					t.Errorf("rule %s from %+v", r.Name, r.Spec.Source)
				}
			}
			if s := getSugg(t, base, testNS); s.Spec.Decision != lastv1alpha1.SuggestionPending {
				t.Errorf("suggestion decided: %+v", s.Spec)
			}
			if strings.Contains(out.String(), suggName) {
				t.Errorf("batch mode mentioned the suggestion:\n%s", out)
			}
		})
	}
}

func TestTerminalEscapesAreStripped(t *testing.T) {
	const esc = "\x1b]0;pwned\x07\x1b[2J"
	s := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Spec.Rationale = "Fine" + esc + "\nCreated CommandRule team-x/evil\u202e"
		s.Spec.Proposed.Description = "desc" + esc
		s.Spec.Proposed.Match.Argv = []string{"kubectl", "get", "deployment", "[!-]*" + esc}
		ex := argvCommand("kubectl", "get", "deployment", "web"+esc)
		ex.Providers = []string{k8sProv}
		s.Spec.Examples = []lastv1alpha1.Command{ex}
		s.Spec.Proposed.Providers = []string{k8sProv + esc}
	})
	for _, args := range [][]string{
		{verbRules, verbSugg},
		{verbRules, verbSugg, "show", suggName},
		{verbRules, verbSugg, "accept", suggName, "--yes"},
	} {
		c := fakeClient(s.DeepCopy())
		d, out := depsFor(c, "")
		_ = execute(d, args...)
		got := out.String()
		for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
			if strings.Contains(got, bad) {
				t.Errorf("%v printed %q:\n%q", args, bad, got)
			}
		}
		for line := range strings.SplitSeq(got, "\n") {
			if strings.HasPrefix(line, "Created CommandRule team-x/evil") {
				t.Errorf("%v: operator text passes for CLI output: %q", args, line)
			}
		}
		if len(args) > 2 && !strings.Contains(got, "  | Created CommandRule team-x/evil") {
			t.Errorf("%v: the rationale's second line is not shown, labelled:\n%s", args, got)
		}
	}
}

func TestPlanListsCoveringSuggestions(t *testing.T) {
	cluster := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Name, s.Namespace, s.Spec.TargetScope = "sugg-cluster000", sysNS, lastv1alpha1.RuleScopeCluster
	})
	denied := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Name, s.Spec.Decision, s.Spec.DecidedBy = "sugg-denied0000", lastv1alpha1.SuggestionDenied, "bob"
	})
	other := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) { s.Name, s.Namespace = "sugg-other00000", "other" })
	wrongStage := testSuggestion(func(s *lastv1alpha1.CommandRuleSuggestion) {
		s.Name, s.Spec.Proposed.Stages = "sugg-delete0000", []lastv1alpha1.Stage{lastv1alpha1.StageDelete}
	})
	c := fakeClient(ruleIntent(), testSuggestion(nil), cluster, denied, other, wrongStage)
	d, out := depsFor(c, "")
	if err := execute(d, "plan", webName); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "probe:up: "+suggName+", "+sysNS+"/sugg-cluster000") {
		t.Errorf("plan does not list the covering suggestions:\n%s", got)
	}
	for _, not := range []string{"sugg-denied", "sugg-other", "sugg-delete"} {
		if strings.Contains(got, not) {
			t.Errorf("plan lists %s:\n%s", not, got)
		}
	}
	if !strings.HasSuffix(got, "--reason R\n") {
		t.Errorf("the decision commands are no longer last:\n%s", got)
	}

	// A user who cannot read suggestions sees the plan as before.
	forbidden := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*lastv1alpha1.CommandRuleSuggestionList); ok {
				return errors.New("forbidden")
			}
			return c.List(ctx, list, opts...)
		},
	})
	d, out = depsFor(forbidden, "")
	if err := execute(d, "plan", webName); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "sugg-") {
		t.Errorf("plan:\n%s", out)
	}
}
