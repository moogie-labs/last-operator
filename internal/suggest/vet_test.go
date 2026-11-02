//nolint:goconst // table-driven test data repeats argv words
package suggest_test

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

func cmd(argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func proposal(match []string, examples ...[]string) suggest.Proposal {
	p := suggest.Proposal{
		TargetScope: lastv1alpha1.RuleScopeNamespace,
		Namespace:   "team-x",
		Trigger:     lastv1alpha1.TriggerPlanTime,
		Spec: lastv1alpha1.CommandRuleSpec{
			Stages:    []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery},
			Providers: []string{"aws"},
			Match:     lastv1alpha1.ArgvPattern{Argv: match},
		},
	}
	for _, e := range examples {
		c := cmd(e...)
		c.Providers = []string{"aws"}
		p.Examples = append(p.Examples, c)
	}
	return p
}

func TestVetDrops(t *testing.T) {
	withSource := proposal([]string{"aws", "s3", "ls"}, []string{"aws", "s3", "ls"})
	withSource.Spec.Source = &lastv1alpha1.RuleSource{CreatedFrom: lastv1alpha1.RuleOriginSuggestion}
	longRationale := proposal([]string{"aws", "s3", "ls"}, []string{"aws", "s3", "ls"})
	longRationale.Rationale = strings.Repeat("x", 501)
	tests := []struct {
		name string
		p    suggest.Proposal
		want string
	}{
		{"matches nothing", proposal([]string{"aws", "...", "ls"}, []string{"aws", "s3", "ls"}), "matches nothing"},
		{"code can vary", proposal([]string{"bash", "-c", "*"}, []string{"bash", "-c", "ls"}), "code"},
		{"wildcard after delete", proposal([]string{"kubectl", "delete", "*"}, []string{"kubectl", "delete", "pod"}), "destructive"},
		{"argv0 mismatch", proposal([]string{"aws", "s3", "ls"}, []string{"gcloud", "s3", "ls"}), "argv[0]"},
		{"example not matched", proposal([]string{"aws", "s3", "ls"}, []string{"aws", "s3", "ls"}, []string{"aws", "s3", "rb"}), "does not match"},
		{"source set", withSource, "source"},
		{"long rationale", longRationale, "rationale"},
		{"no examples", proposal([]string{"aws", "s3", "ls"}), "no examples"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason, by := suggest.Vet(tt.p, nil, nil)
			if ok || !strings.Contains(reason, tt.want) || by != nil {
				t.Errorf("Vet = %v, %q, %+v; want a drop naming %q", ok, reason, by, tt.want)
			}
		})
	}
	if ok, reason, _ := suggest.Vet(proposal([]string{"aws", "s3", "ls", "[!-]*"}, []string{"aws", "s3", "ls", "b"}), nil, nil); !ok {
		t.Errorf("a good proposal was dropped: %s", reason)
	}
}

func TestVetConsolidation(t *testing.T) {
	p := proposal([]string{"aws", "s3", "..."})
	p.Contains = [][]string{{"aws", "s3", "ls"}, {"aws", "s3", "cp", "a", "b"}}
	if ok, reason, _ := suggest.Vet(p, nil, nil); !ok {
		t.Errorf("consolidation dropped: %s", reason)
	}
	p.Contains = append(p.Contains, []string{"aws", "ec2", "..."})
	if ok, reason, _ := suggest.Vet(p, nil, nil); ok || !strings.Contains(reason, "does not contain") {
		t.Errorf("Vet = %v, %q; want a drop for a pattern the match does not contain", ok, reason)
	}
}

func TestVetSkipsCoveredAndSuppressed(t *testing.T) {
	spec := func(match ...string) lastv1alpha1.CommandRuleSpec {
		return lastv1alpha1.CommandRuleSpec{
			Stages: []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery}, Providers: []string{"aws"},
			Match: lastv1alpha1.ArgvPattern{Argv: match},
		}
	}
	p := proposal([]string{"aws", "ec2", "..."}, []string{"aws", "ec2", "describe-instances"})
	existing := []rules.Rule{{Ref: rules.Ref{Scope: "namespace", Namespace: "team-x", Name: "r"}, Spec: spec("aws", "ec2", "...")}}
	if ok, reason, _ := suggest.Vet(p, existing, nil); ok || !strings.Contains(reason, "already covered") {
		t.Errorf("Vet = %v, %q; want already covered", ok, reason)
	}
	// a rule at another stage does not cover
	other := spec("aws", "ec2", "...")
	other.Stages = []lastv1alpha1.Stage{lastv1alpha1.StageDelete}
	if ok, reason, _ := suggest.Vet(p, []rules.Rule{{Spec: other}}, nil); !ok {
		t.Errorf("a rule at another stage covered: %s", reason)
	}

	denied := []suggest.Denied{{Scope: lastv1alpha1.RuleScopeNamespace, Namespace: "team-x", Name: "sugg-1", Spec: spec("aws", "ec2", "...")}}
	for _, tt := range []struct {
		name  string
		match []string
		ex    []string
		sup   bool
	}{
		{"same", []string{"aws", "ec2", "..."}, []string{"aws", "ec2", "x"}, true},
		{"broader", []string{"aws", "..."}, []string{"aws", "ec2", "x"}, true},
		{"narrower", []string{"aws", "ec2", "describe-*", "..."}, []string{"aws", "ec2", "describe-x"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason, by := suggest.Vet(proposal(tt.match, tt.ex), nil, denied)
			if tt.sup && (ok || by == nil || by.Name != "sugg-1" || reason == "") {
				t.Errorf("Vet = %v, %q, %+v; want suppressed by sugg-1", ok, reason, by)
			}
			if !tt.sup && (!ok || by != nil) {
				t.Errorf("Vet = %v, %q, %+v; want ok", ok, reason, by)
			}
		})
	}

	// scope
	clusterDenied := []suggest.Denied{{Scope: lastv1alpha1.RuleScopeCluster, Name: "sugg-c", Spec: spec("aws", "ec2", "...")}}
	if ok, _, _ := suggest.Vet(p, nil, clusterDenied); !ok {
		t.Error("a denied Cluster suggestion suppressed a Namespace proposal")
	}
	elsewhere := []suggest.Denied{{Scope: lastv1alpha1.RuleScopeNamespace, Namespace: "team-y", Name: "sugg-y", Spec: spec("aws", "ec2", "...")}}
	if ok, _, _ := suggest.Vet(p, nil, elsewhere); !ok {
		t.Error("a denied suggestion in another namespace suppressed a Namespace proposal")
	}
	cp := p
	cp.TargetScope = lastv1alpha1.RuleScopeCluster
	cp.Namespace = ""
	if ok, _, by := suggest.Vet(cp, nil, elsewhere); ok || by == nil || by.Namespace != "team-y" || by.Name != "sugg-y" {
		t.Errorf("Vet = %v, %+v; want a Cluster proposal suppressed by team-y/sugg-y", ok, by)
	}
	// a Cluster proposal names the Denied suggestion that suppressed it by
	// namespace too: same-named denials elsewhere are not confused with it
	sameName := []suggest.Denied{
		{Scope: lastv1alpha1.RuleScopeNamespace, Namespace: "team-a", Name: "sugg-d", Spec: spec("gcloud", "...")},
		{Scope: lastv1alpha1.RuleScopeNamespace, Namespace: "team-b", Name: "sugg-d", Spec: spec("aws", "ec2", "...")},
	}
	if ok, _, by := suggest.Vet(cp, nil, sameName); ok || by == nil || by.Namespace != "team-b" || by.Name != "sugg-d" {
		t.Errorf("Vet = %v, %+v; want a Cluster proposal suppressed by team-b/sugg-d", ok, by)
	}
	// undecidable counts as suppressing
	und := []suggest.Denied{{Scope: lastv1alpha1.RuleScopeNamespace, Namespace: "team-x", Name: "sugg-u", Spec: spec("aws", "re:e.*", "...")}}
	if ok, _, by := suggest.Vet(proposal([]string{"aws", "ec2", "..."}, []string{"aws", "ec2", "x"}), nil, und); ok || by == nil || by.Name != "sugg-u" {
		t.Errorf("Vet = %v, %+v; want an undecidable comparison to suppress", ok, by)
	}
}

func TestVetPassesGeneralizeOutput(t *testing.T) {
	words := []string{"kubectl", "get", "bash", "-c", "-n", "--", "--x=y", "--eval=1", "", "*", "a?b", "[c]", `d\e`, "re:x",
		"...", "-", "web", "delete", "rm", "destroy", "terminate-x", "/work/kc", "node", "python3", "-e", "exec", "run", "--all", "=", "-=", "x=y"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		argv := make([]string, 1+r.IntN(8))
		for i := range argv {
			argv[i] = words[r.IntN(len(words))]
		}
		for strings.ContainsAny(argv[0], "*?[") || argv[0] == "..." || strings.HasPrefix(argv[0], "re:") {
			argv[0] = words[r.IntN(len(words))]
		}
		c := cmd(argv...)
		p := proposal(rules.Generalize(c).Argv)
		p.Examples = []lastv1alpha1.Command{c}
		p.Spec.Providers = nil
		for _, f := range rules.LintFindings(p.Spec) {
			if f.Kind == rules.KindNothing || f.Kind == rules.KindCode {
				t.Fatalf("Generalize(%q) = %q has a %s finding: %s", argv, p.Spec.Match.Argv, f.Kind, f.Message)
			}
		}
		ok, reason, _ := suggest.Vet(p, nil, nil)
		if !ok {
			hasVerb := slices.ContainsFunc(argv, func(a string) bool {
				return a == "delete" || a == "rm" || a == "destroy" || strings.HasPrefix(a, "terminate-")
			})
			if !hasVerb || !strings.Contains(reason, "destructive verb") {
				t.Fatalf("Vet(Generalize(%q) = %q) dropped: %s", argv, p.Spec.Match.Argv, reason)
			}
		}
	}
}
