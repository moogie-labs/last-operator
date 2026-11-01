//nolint:goconst // test data repeats words
package cli

import (
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

const (
	applyArgs   = "kubectl apply -n team-x -f app.yaml"
	k8sProv     = "k8s"
	wordApply   = "apply"
	wordInstall = "install"
)

// rulePlan is a plan with a step that has providers, egress and an image install,
// a second step, a probe and a teardown command, none covered by any rule.
func rulePlan() *lastv1alpha1.Plan {
	apply := argvCommand("kubectl", wordApply, "-n", "team-x", "-f", "app.yaml")
	apply.ID, apply.Providers, apply.Egress = "s1", []string{k8sProv}, []string{"10.0.0.0/8:443"}
	apply.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", wordInstall, "kubernetes*"}}}}
	status := argvCommand("kubectl", "rollout", "status", "deployment", webName)
	status.ID, status.Providers = "s2", []string{k8sProv}
	get := argvCommand("kubectl", "get", "deployment", webName)
	get.Providers = []string{k8sProv}
	del := argvCommand("kubectl", "delete", "deployment", webName, "--ignore-not-found")
	del.Providers = []string{k8sProv}
	p := &lastv1alpha1.Plan{Summary: nginxName, Steps: []lastv1alpha1.Command{apply, status},
		Resources: []lastv1alpha1.ManagedResourceSpec{{Description: webName,
			Probes:   []lastv1alpha1.Probe{{ID: "up", Command: get, Assert: "true"}},
			Teardown: []lastv1alpha1.Command{del}}},
		UncoveredSteps: []string{"s1", "s2", "probe:up", "teardown:0.0"}}
	p.Hash = planHash(p)
	return p
}

func ruleIntent() *lastv1alpha1.Intent {
	return testIntent(webName, func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
		in.Status.Stage = lastv1alpha1.StageCreate
		in.Status.Plan = rulePlan()
	})
}

func nsRules(t *testing.T, c client.Client) []lastv1alpha1.CommandRule {
	t.Helper()
	var l lastv1alpha1.CommandRuleList
	if err := c.List(context.Background(), &l, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func clusterRules(t *testing.T, c client.Client) []lastv1alpha1.ClusterCommandRule {
	t.Helper()
	var l lastv1alpha1.ClusterCommandRuleList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// ruleFor returns the one rule whose name starts with prefix.
func ruleFor(t *testing.T, items []lastv1alpha1.CommandRule, prefix string) lastv1alpha1.CommandRule {
	t.Helper()
	var found []lastv1alpha1.CommandRule
	for _, r := range items {
		if strings.HasPrefix(r.Name, prefix) {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rules named %s*: %d, want 1 (all: %v)", prefix, len(found), items)
	}
	return found[0]
}

var hash8 = regexp.MustCompile(`^[0-9a-f]{8}$`)

func TestAlwaysAllowExactCreatesALiteralRule(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "e\no\no\no\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	if len(items) != 1 {
		t.Fatalf("rules = %d, want 1: %v", len(items), items)
	}
	r := items[0]
	if !strings.HasPrefix(r.Name, "web-s1-") || !hash8.MatchString(strings.TrimPrefix(r.Name, "web-s1-")) {
		t.Errorf("name = %q, want web-s1-<hash8>", r.Name)
	}
	if r.Namespace != testNS || r.Labels["last.moogie-labs.com/intent"] != webName {
		t.Errorf("namespace/labels = %q %v", r.Namespace, r.Labels)
	}
	want := lastv1alpha1.CommandRuleSpec{
		Description: r.Spec.Description,
		Stages:      []lastv1alpha1.Stage{lastv1alpha1.StageCreate},
		Providers:   []string{k8sProv},
		Match:       lastv1alpha1.ArgvPattern{Argv: []string{"kubectl", wordApply, "-n", "team-x", "-f", "app.yaml"}},
		Install:     []lastv1alpha1.ArgvPattern{{Argv: []string{"pip", wordInstall, `kubernetes\*`}}},
		Egress:      []string{"10.0.0.0/8:443"},
		Source:      &lastv1alpha1.RuleSource{CreatedFrom: lastv1alpha1.RuleOriginApproval},
	}
	if !reflect.DeepEqual(r.Spec, want) {
		t.Errorf("spec = %+v\nwant   %+v", r.Spec, want)
	}
	if !rules.Match(r.Spec.Install[0].Argv, []string{"pip", wordInstall, "kubernetes*"}) ||
		rules.Match(r.Spec.Install[0].Argv, []string{"pip", wordInstall, "kubernetes-evil"}) {
		t.Errorf("install pattern %q is not literal", r.Spec.Install[0].Argv)
	}
	if a := approvalsIn(t, c); len(a) != 1 || a[0].Spec.SubjectHash != rulePlan().Hash || a[0].Spec.Stage != lastv1alpha1.StageCreate {
		t.Errorf("approvals = %+v", a)
	}
	if !strings.Contains(out.String(), "Created CommandRule "+r.Name) {
		t.Errorf("output does not say what was created:\n%s", out)
	}
}

func TestAlwaysAllowGeneralizedUsesTheEditedPattern(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "g\nkubectl apply -n team-x -f *\n\no\no\no\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	r := ruleFor(t, nsRules(t, c), "web-s1-")
	if want := []string{"kubectl", wordApply, "-n", "team-x", "-f", "*"}; !reflect.DeepEqual(r.Spec.Match.Argv, want) {
		t.Errorf("match = %q, want %q", r.Spec.Match.Argv, want)
	}
	got := out.String()
	if !strings.Contains(got, "kubectl apply -n [!-]* -f [!-]*") { // the proposal
		t.Errorf("output lacks the proposed pattern:\n%s", got)
	}
	// The lint is printed for the edited pattern, and the examples are the plan commands it matches.
	i := strings.LastIndex(got, "rules match argv only, not files or stdin")
	if i < 0 {
		t.Fatalf("output lacks the lint:\n%s", got)
	}
	tail := got[i:]
	if !strings.Contains(tail, "s1: kubectl apply -n team-x -f app.yaml") || strings.Contains(tail[:strings.Index(tail, "Rule for")], "s2:") {
		t.Errorf("examples after the lint are wrong:\n%s", tail)
	}
}

func TestAlwaysAllowRefusesAPatternThatMatchesNothing(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "g\nkube* apply\n\nkubectl apply -n team-x -f app.yaml\n\no\no\no\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if got := ruleFor(t, nsRules(t, c), "web-s1-"); got.Spec.Match.Argv[0] != "kubectl" {
		t.Errorf("match = %q, want the later, valid edit", got.Spec.Match.Argv)
	}
	if !strings.Contains(out.String(), "cannot be accepted") {
		t.Errorf("Enter on a pattern that matches nothing was accepted:\n%s", out)
	}
	if !strings.Contains(out.String(), "matches nothing") {
		t.Errorf("output does not refuse the pattern:\n%s", out)
	}
}

func TestAlwaysAllowJustThisOnceCreatesNoRule(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "o\n\no\nonce\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if r := nsRules(t, c); len(r) != 0 {
		t.Errorf("rules = %v, want none", r)
	}
	if a := approvalsIn(t, c); len(a) != 1 {
		t.Errorf("approvals = %+v", a)
	}
}

func TestAlwaysAllowTeardownGetsDeleteAndProbeGetsThePlanStage(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "o\no\ne\ne\n")
	if err := execute(d, verbApprove, webName, "--always-allow"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	probe := ruleFor(t, items, "web-probe-up-")
	if !reflect.DeepEqual(probe.Spec.Stages, []lastv1alpha1.Stage{lastv1alpha1.StageCreate}) {
		t.Errorf("probe stages = %v, want the plan's stage", probe.Spec.Stages)
	}
	td := ruleFor(t, items, "web-teardown-0-0-")
	if !reflect.DeepEqual(td.Spec.Stages, []lastv1alpha1.Stage{lastv1alpha1.StageDelete}) {
		t.Errorf("teardown stages = %v, want Delete", td.Spec.Stages)
	}
	if !rules.Match(td.Spec.Match.Argv, []string{"kubectl", "delete", "deployment", webName, "--ignore-not-found"}) {
		t.Errorf("teardown match = %q", td.Spec.Match.Argv)
	}
}

func TestAlwaysAllowForADiscoveryCommandUsesItAsTheEntry(t *testing.T) {
	look := pendingIntent()
	look.Status.PendingCommand.Command.Providers = []string{k8sProv}
	look.Status.PendingCommand.Hash = discoveryHash(look.Status.PendingCommand)
	c := fakeClient(look)
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, "look", "--always-allow=exact"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	if len(items) != 1 || !strings.HasPrefix(items[0].Name, "look-discovery-") ||
		!reflect.DeepEqual(items[0].Spec.Stages, []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery}) {
		t.Fatalf("rules = %v", items)
	}
	if a := approvalsIn(t, c); len(a) != 1 || a[0].Spec.Stage != lastv1alpha1.StageDiscovery {
		t.Errorf("approvals = %+v", a)
	}
}

func TestAlwaysAllowGeneralizedWithNoStdin(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "") // a prompt would end the input
	if err := execute(d, verbApprove, webName, "--always-allow=generalized"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	if len(items) != 4 {
		t.Fatalf("rules = %d, want 4", len(items))
	}
	s1 := ruleFor(t, items, "web-s1-")
	if want := []string{"kubectl", wordApply, "-n", "[!-]*", "-f", "[!-]*"}; !reflect.DeepEqual(s1.Spec.Match.Argv, want) {
		t.Errorf("s1 match = %q, want %q", s1.Spec.Match.Argv, want)
	}
	if a := approvalsIn(t, c); len(a) != 1 {
		t.Errorf("approvals = %+v", a)
	}
}

// TestAlwaysAllowIgnoresTheStoredGeneralization: the operator writes
// status.plan.generalizations and pendingCommand.generalization outside the plan
// hash, and the operator is untrusted, so the CLI never offers them (M6 design §10, I2).
func TestAlwaysAllowIgnoresTheStoredGeneralization(t *testing.T) {
	in := ruleIntent()
	broad := []string{"kubectl", "..."}
	in.Status.Plan.Generalizations = map[string]lastv1alpha1.Generalization{
		"s1": {Argv: broad}, "s2": {Argv: broad}, "probe:up": {Argv: broad}, "teardown:0.0": {Argv: broad}}
	c := fakeClient(in)
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=generalized"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	for _, prefix := range []string{"web-s1-", "web-s2-", "web-probe-up-", "web-teardown-0-0-"} {
		if r := ruleFor(t, items, prefix); slices.Contains(r.Spec.Match.Argv, "...") {
			t.Errorf("%s match = %q, want rules.Generalize, not the stored generalization", prefix, r.Spec.Match.Argv)
		}
	}
	s2 := ruleFor(t, items, "web-s2-")
	if want := []string{"kubectl", "rollout", "status", "[!-]*", "[!-]*"}; !reflect.DeepEqual(s2.Spec.Match.Argv, want) {
		t.Errorf("s2 match = %q, want %q", s2.Spec.Match.Argv, want)
	}
	if strings.Contains(out.String(), "kubectl ...") {
		t.Errorf("the stored generalization was offered:\n%s", out)
	}

	look := pendingIntent()
	look.Status.PendingCommand.Command.Providers = []string{k8sProv}
	look.Status.PendingCommand.Generalization = &lastv1alpha1.Generalization{Argv: broad}
	look.Status.PendingCommand.Hash = discoveryHash(look.Status.PendingCommand)
	c = fakeClient(look)
	d, out = depsFor(c, "")
	if err := execute(d, verbApprove, "look", "--always-allow=generalized"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items = nsRules(t, c)
	if len(items) != 1 || !reflect.DeepEqual(items[0].Spec.Match.Argv, []string{"kubectl", "get", "ns"}) {
		t.Errorf("discovery rules = %+v, want one matching kubectl get ns", items)
	}
}

func TestRulesAreCreatedBeforeTheApproval(t *testing.T) {
	var order []string
	c := interceptCreates(ruleIntent(), func(obj client.Object) error {
		switch obj.(type) {
		case *lastv1alpha1.CommandRule:
			order = append(order, "rule")
		case *lastv1alpha1.IntentApproval:
			order = append(order, "approval")
		}
		return nil
	})
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if want := []string{"rule", "rule", "rule", "rule", "approval"}; !reflect.DeepEqual(order, want) {
		t.Errorf("creation order = %v, want %v", order, want)
	}
}

func TestAFailedRuleCreationStopsBeforeTheApproval(t *testing.T) {
	n := 0
	c := interceptCreates(ruleIntent(), func(obj client.Object) error {
		if _, ok := obj.(*lastv1alpha1.CommandRule); ok {
			if n++; n == 2 {
				return errors.New("forbidden: you may not create rules")
			}
		}
		return nil
	})
	d, out := depsFor(c, "")
	err := execute(d, verbApprove, webName, "--always-allow=exact")
	if err == nil || !strings.Contains(err.Error(), "you may not create rules") {
		t.Fatalf("approve = %v, want the creation error\n%s", err, out)
	}
	if a := approvalsIn(t, c); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
	if got := nsRules(t, c); len(got) != 1 || !strings.Contains(err.Error(), got[0].Name) {
		t.Errorf("rules = %v; error %q does not report what was created", got, err)
	}
}

func interceptCreates(in *lastv1alpha1.Intent, f func(client.Object) error) client.Client {
	base := fakeClient(in)
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := f(obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

func TestClusterAndStagesFlags(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact", "--cluster", "--stages", "Create,Update,Delete"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if n := len(nsRules(t, c)); n != 0 {
		t.Errorf("namespaced rules = %d, want none", n)
	}
	items := clusterRules(t, c)
	if len(items) != 4 {
		t.Fatalf("cluster rules = %d, want 4", len(items))
	}
	for _, r := range items {
		if want := []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDelete}; !reflect.DeepEqual(r.Spec.Stages, want) {
			t.Errorf("%s stages = %v, want --stages for every rule, teardown included", r.Name, r.Spec.Stages)
		}
		if r.Labels["last.moogie-labs.com/intent"] != webName || r.Spec.Source.CreatedFrom != lastv1alpha1.RuleOriginApproval {
			t.Errorf("%s labels/source = %v %+v", r.Name, r.Labels, r.Spec.Source)
		}
	}
}

func TestStagesFlagIsValidated(t *testing.T) {
	for _, args := range [][]string{
		{"--always-allow=exact", "--stages", "Create,Bogus"},
		{"--stages", "Create"},
		{"--cluster"},
		{"--providers", "k8s"},
		{"--always-allow=sometimes"},
	} {
		c := fakeClient(ruleIntent())
		d, out := depsFor(c, "")
		if err := execute(d, append([]string{verbApprove, webName}, args...)...); err == nil {
			t.Errorf("approve %v succeeded\n%s", args, out)
		}
		if a := approvalsIn(t, c); len(a) != 0 || len(nsRules(t, c)) != 0 {
			t.Errorf("approve %v recorded something", args)
		}
	}
}

func TestAnExistingIdenticalRuleCountsAsDoneAndADifferingOneErrors(t *testing.T) {
	base := fakeClient(ruleIntent())
	d, out := depsFor(base, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("first approve = %v\n%s", err, out)
	}
	// A reader that sees no rules, as if they were created after it read: the
	// rules exist when the CLI creates them.
	stale := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			switch list.(type) {
			case *lastv1alpha1.CommandRuleList, *lastv1alpha1.ClusterCommandRuleList:
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	})
	d, out = depsFor(stale, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("approve with the rules already there = %v\n%s", err, out)
	}
	if n := len(nsRules(t, base)); n != 4 {
		t.Errorf("rules = %d, want 4", n)
	}
	if n := strings.Count(out.String(), "already exists"); n != 4 {
		t.Errorf("output says %d rules exist, want 4:\n%s", n, out)
	}
	if n := len(approvalsIn(t, base)); n != 2 {
		t.Errorf("approvals = %d, want 2", n)
	}

	r := ruleFor(t, nsRules(t, base), "web-s2-")
	r.Spec.Providers = []string{"other"} // a different spec under the same name
	if err := base.Update(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	d, _ = depsFor(stale, "")
	err := execute(d, verbApprove, webName, "--always-allow=exact")
	if err == nil || !strings.Contains(err.Error(), r.Name) || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("approve over a differing rule = %v, want an error naming it", err)
	}
	if n := len(approvalsIn(t, base)); n != 2 {
		t.Errorf("approvals = %d, want no new one", n)
	}
}

func TestAlwaysAllowWithEverythingCoveredOnlyApproves(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("first approve = %v\n%s", err, out)
	}
	out.Reset()
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("second approve = %v\n%s", err, out)
	}
	if n := len(nsRules(t, c)); n != 4 {
		t.Errorf("rules = %d, want 4", n)
	}
	if !strings.Contains(out.String(), "nothing to add") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAlwaysAllowSkipsCoveredEntries(t *testing.T) {
	in := ruleIntent()
	cover := &lastv1alpha1.CommandRule{ObjectMeta: metav1.ObjectMeta{Name: "status", Namespace: testNS},
		Spec: lastv1alpha1.CommandRuleSpec{Stages: []lastv1alpha1.Stage{lastv1alpha1.StageUpdate}, Providers: []string{k8sProv},
			Match: lastv1alpha1.ArgvPattern{Argv: []string{"kubectl", "rollout", "..."}}}}
	elsewhere := &lastv1alpha1.CommandRule{ObjectMeta: metav1.ObjectMeta{Name: "get", Namespace: "other"},
		Spec: lastv1alpha1.CommandRuleSpec{Stages: []lastv1alpha1.Stage{lastv1alpha1.StageCreate}, Providers: []string{k8sProv},
			Match: lastv1alpha1.ArgvPattern{Argv: []string{"kubectl", "get", "..."}}}}
	c := fakeClient(in, cover, elsewhere)
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if got := nsRules(t, c); len(got) != 4 { // status, plus s1, probe and teardown
		t.Errorf("rules = %d, want 4 (s2 is covered, the probe's rule is in another namespace)", len(got))
	}
}

func providerless(in *lastv1alpha1.Intent, ids ...string) *lastv1alpha1.Intent {
	p := in.Status.Plan
	for _, id := range ids {
		switch id {
		case "s2":
			p.Steps[1].Providers = nil
		case "probe":
			p.Resources[0].Probes[0].Command.Providers = nil
		default: // all
			p.Steps[0].Providers, p.Resources[0].Teardown[0].Providers = nil, nil
		}
	}
	p.Hash = planHash(p)
	return in
}

func TestAProviderlessProbeGetsThePlansProviders(t *testing.T) {
	in := providerless(ruleIntent(), "probe")
	in.Status.Plan.Steps[1].Providers = []string{"aws"}
	in.Status.Plan.Hash = planHash(in.Status.Plan)
	c := fakeClient(in)
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact", "--providers", "ignored"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	if got := ruleFor(t, items, "web-probe-up-").Spec.Providers; !reflect.DeepEqual(got, []string{"aws", k8sProv}) {
		t.Errorf("probe providers = %v, want the sorted union", got)
	}
	if got := ruleFor(t, items, "web-s2-").Spec.Providers; !reflect.DeepEqual(got, []string{"aws"}) {
		t.Errorf("s2 providers = %v, want its own", got)
	}
	if got := ruleFor(t, items, "web-s1-").Spec.Providers; !reflect.DeepEqual(got, []string{k8sProv}) {
		t.Errorf("s1 providers = %v, want its own, not the flag", got)
	}
}

func allProviderless() *lastv1alpha1.Intent {
	in := providerless(ruleIntent(), "all", "s2", "probe")
	return in
}

func TestAnAllProviderlessPlanSkipsWithoutTheFlag(t *testing.T) {
	c := fakeClient(allProviderless())
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if got := nsRules(t, c); len(got) != 0 {
		t.Errorf("rules = %v, want none", got)
	}
	if !strings.Contains(out.String(), "pass --providers") {
		t.Errorf("output does not say to pass --providers:\n%s", out)
	}
	if a := approvalsIn(t, c); len(a) != 1 {
		t.Errorf("approvals = %+v", a)
	}
}

func TestProvidersFlagFillsAProviderlessPlan(t *testing.T) {
	c := fakeClient(allProviderless())
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact", "--providers", "web, aws"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	items := nsRules(t, c)
	if len(items) != 4 {
		t.Fatalf("rules = %d, want 4", len(items))
	}
	for _, r := range items {
		if !reflect.DeepEqual(r.Spec.Providers, []string{"web", "aws"}) {
			t.Errorf("%s providers = %v", r.Name, r.Spec.Providers)
		}
	}
}

func TestFollowOffersApproveAndAlwaysAllow(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "a\ne\no\no\no\n")
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Stage = lastv1alpha1.StageCreate
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = rulePlan()
		}},
		{ready: w.decided(rulePlan().Hash), apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseFailed }},
	}
	d.Wait = w.wait
	if err := execute(d, "run an nginx web server", "--name", webName); !errors.Is(err, ErrIntentFailed) {
		t.Fatalf("kubectl last = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "Approve? [y/N/d/a]: ") || !strings.Contains(out.String(), "approve and always allow") {
		t.Errorf("the prompt does not offer always allow:\n%s", out)
	}
	if got := nsRules(t, c); len(got) != 1 || !strings.HasPrefix(got[0].Name, "web-s1-") {
		t.Errorf("rules = %v", got)
	}
	if a := approvalsIn(t, c); len(a) != 1 {
		t.Errorf("approvals = %+v", a)
	}
}

func TestFollowDoesNotOfferAlwaysAllowWithoutUncoveredEntries(t *testing.T) {
	c := fakeClient()
	d, out := depsFor(c, "y\n")
	w := &world{t: t, c: c}
	w.steps = []step{
		{apply: func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseAwaitingApproval
			in.Status.Plan = webPlan()
		}},
		{ready: w.decided(h2), apply: func(in *lastv1alpha1.Intent) { in.Status.Phase = lastv1alpha1.PhaseFailed }},
	}
	d.Wait = w.wait
	_ = execute(d, "run an nginx web server", "--name", webName)
	if !strings.Contains(out.String(), "Approve? [y/N/d]: ") {
		t.Errorf("the prompt changed:\n%s", out)
	}
}

const rulesYAML = `apiVersion: last.moogie-labs.com/v1alpha1
kind: CommandRule
metadata: {name: broad, namespace: team-x}
spec:
  stages: [Create]
  providers: [k8s]
  match: {argv: [kubectl, get, "..."]}
---
apiVersion: last.moogie-labs.com/v1alpha1
kind: ClusterCommandRule
metadata: {name: fine}
spec:
  stages: [Create]
  providers: [k8s]
  match: {argv: [kubectl, get, pods]}
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := t.TempDir() + "/rules.yaml"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRulesLintReadsAFile(t *testing.T) {
	d, out := depsFor(fakeClient(), "")
	if err := execute(d, "rules", "lint", writeTemp(t, rulesYAML)); err != nil {
		t.Fatalf("lint of a merely broad rule = %v, want exit 0\n%s", err, out)
	}
	got := out.String()
	for _, want := range []string{"CommandRule team-x/broad", "also accepts flags", "ClusterCommandRule fine: ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestRulesLintExitsNonZeroOnAPatternThatMatchesNothing(t *testing.T) {
	bad := strings.Replace(rulesYAML, "[kubectl, get, pods]", `["kube*", get, pods]`, 1)
	d, out := depsFor(fakeClient(), "")
	err := execute(d, "rules", "lint", writeTemp(t, bad))
	if err == nil {
		t.Fatalf("lint succeeded\n%s", out)
	}
	if !strings.Contains(out.String(), "argv[0] must be a literal") || !strings.Contains(out.String(), "also accepts flags") {
		t.Errorf("lint stopped at the first problem:\n%s", out)
	}
}

func TestRulesLintReadsStdin(t *testing.T) {
	d, out := depsFor(fakeClient(), rulesYAML)
	if err := execute(d, "rules", "lint", "-"); err != nil {
		t.Fatalf("lint - = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "ClusterCommandRule fine: ok") {
		t.Errorf("output:\n%s", out)
	}
	d, out = depsFor(fakeClient(), "kind: Pod\nmetadata: {name: x}\n")
	if err := execute(d, "rules", "lint", "-"); err == nil {
		t.Errorf("lint of a Pod succeeded\n%s", out)
	}
}

func TestPlanShowsUncoveredStepsAndTheRuleThatCoveredTheRest(t *testing.T) {
	in := ruleIntent()
	in.Status.Plan.UncoveredSteps = []string{"s1", "probe:up", "teardown:0.0"}
	cover := &lastv1alpha1.CommandRule{ObjectMeta: metav1.ObjectMeta{Name: "status", Namespace: testNS},
		Spec: lastv1alpha1.CommandRuleSpec{Stages: []lastv1alpha1.Stage{lastv1alpha1.StageCreate}, Providers: []string{k8sProv},
			Match: lastv1alpha1.ArgvPattern{Argv: []string{"kubectl", "rollout", "..."}}}}
	d, out := depsFor(fakeClient(in, cover), "")
	if err := execute(d, "plan", webName); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Not covered by allow rules: s1, probe:up, teardown:0.0", "Covered by allow rules:", "s2: namespace/team-x/status"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "--reason R\n") {
		t.Errorf("the decision commands are no longer last:\n%s", got)
	}
}

func TestPlanShowsOnlyTheUncoveredListWhenRulesCannotBeRead(t *testing.T) {
	in := ruleIntent()
	base := fakeClient(in)
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			switch list.(type) {
			case *lastv1alpha1.CommandRuleList, *lastv1alpha1.ClusterCommandRuleList:
				return errors.New("forbidden")
			}
			return c.List(ctx, list, opts...)
		},
	})
	d, out := depsFor(c, "")
	if err := execute(d, "plan", webName); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Not covered by allow rules: s1, s2, probe:up, teardown:0.0") || strings.Contains(got, "Covered by allow rules") {
		t.Errorf("plan:\n%s", got)
	}
}

func TestAllowRulesAndAutoAreNamedApart(t *testing.T) {
	for _, tc := range []struct{ suffix, want, not string }{
		{" without approval (allow rules)", "(allow rules)", "(Auto)"},
		{" without approval (Auto)", "(Auto)", "(allow rules)"},
	} {
		in := testIntent(webName, func(in *lastv1alpha1.Intent) {
			in.Status.Phase = lastv1alpha1.PhaseExecuting
			in.Status.Plan = webPlan()
			setReady(in, "Executing", "Running plan "+h2+tc.suffix)
		})
		var b strings.Builder
		noteAuto(&b, in)
		if !strings.Contains(b.String(), "may run without approval "+tc.want) || strings.Contains(b.String(), tc.not) {
			t.Errorf("noteAuto for %q = %q", tc.suffix, b.String())
		}
		d, out := depsFor(fakeClient(in), "")
		if err := execute(d, "plan", webName); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "without approval "+tc.want+";") || strings.Contains(out.String(), tc.not) {
			t.Errorf("plan for %q:\n%s", tc.suffix, out)
		}
	}
	look := testIntent("look", func(in *lastv1alpha1.Intent) {
		in.Status.Phase = lastv1alpha1.PhaseAwaitingDiscoveryApproval
		in.Status.PendingCommand = lookCommand()
		setReady(in, reasonRunningDiscovery, "Running discovery command "+h1+" without approval (allow rules)")
	})
	var b strings.Builder
	noteAuto(&b, look)
	if !strings.Contains(b.String(), "(allow rules)") {
		t.Errorf("noteAuto for a covered discovery command = %q", b.String())
	}
}

func TestAlwaysAllowSkipsEntriesNoRuleCanCover(t *testing.T) {
	ref := "${steps.x.stdout}"
	for _, tc := range []struct {
		name   string
		change func(*lastv1alpha1.Command)
		why    string
	}{
		{"a script", func(c *lastv1alpha1.Command) { c.Run = lastv1alpha1.CommandRun{Script: "echo hi"} }, "runs a script"},
		{"a step reference in argv", func(c *lastv1alpha1.Command) { c.Run.Argv = []string{"echo", ref} }, "references"},
		{"a step reference in image.install", func(c *lastv1alpha1.Command) {
			c.Image = &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", wordInstall, ref}}}}
		}, "references"},
	} {
		for _, mode := range []string{"--always-allow=exact", "--always-allow=generalized"} {
			t.Run(tc.name+" "+mode, func(t *testing.T) {
				step := argvCommand("echo", "hi")
				step.ID, step.Providers = "s1", []string{k8sProv}
				tc.change(&step)
				p := &lastv1alpha1.Plan{Steps: []lastv1alpha1.Command{step}, UncoveredSteps: []string{"s1"}}
				p.Hash = planHash(p)
				in := testIntent(webName, func(in *lastv1alpha1.Intent) {
					in.Status.Phase, in.Status.Stage, in.Status.Plan = lastv1alpha1.PhaseAwaitingApproval, lastv1alpha1.StageCreate, p
				})
				c := fakeClient(in)
				d, out := depsFor(c, "")
				if err := execute(d, verbApprove, webName, mode); err != nil {
					t.Fatalf("approve = %v\n%s", err, out)
				}
				if got := nsRules(t, c); len(got) != 0 {
					t.Errorf("rules = %v, want none", got)
				}
				if !strings.Contains(out.String(), "No rule for s1: ") || !strings.Contains(out.String(), tc.why) ||
					!strings.Contains(out.String(), "just this once") {
					t.Errorf("output lacks the reason (%q):\n%s", tc.why, out)
				}
				if a := approvalsIn(t, c); len(a) != 1 {
					t.Errorf("approvals = %+v, want 1", a)
				}
			})
		}
	}
}

func TestStagesWithoutDeleteNoteThatTeardownIsNotCovered(t *testing.T) {
	c := fakeClient(ruleIntent())
	d, out := depsFor(c, "")
	if err := execute(d, verbApprove, webName, "--always-allow=exact", "--stages", "Create"); err != nil {
		t.Fatalf("approve = %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "the rule for teardown:0.0 does not list stage Delete") {
		t.Errorf("no note about the teardown rule:\n%s", out)
	}
}

func TestAFailedApprovalNamesTheRulesLeftBehind(t *testing.T) {
	c := interceptCreates(ruleIntent(), func(obj client.Object) error {
		if _, ok := obj.(*lastv1alpha1.IntentApproval); ok {
			return errors.New("the Intent changed")
		}
		return nil
	})
	d, out := depsFor(c, "")
	err := execute(d, verbApprove, webName, "--always-allow=exact")
	if err == nil {
		t.Fatalf("approve succeeded\n%s", out)
	}
	items := nsRules(t, c)
	if len(items) != 4 {
		t.Fatalf("rules = %d", len(items))
	}
	for _, r := range items {
		if !strings.Contains(err.Error(), r.Name) {
			t.Errorf("error %q does not name %s", err, r.Name)
		}
	}
}
