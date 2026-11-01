//nolint:goconst // table-driven test data repeats argv words
package suggest_test

import (
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

func TestBroader(t *testing.T) {
	g := lastv1alpha1.CommandRuleSpec{
		Stages: []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery}, Providers: []string{"aws"},
		Match:   lastv1alpha1.ArgvPattern{Argv: []string{"aws", "s3", "ls", "bucket"}},
		Install: []lastv1alpha1.ArgvPattern{{Argv: []string{"pip", "install", "x"}}},
		Egress:  []string{"a.example:443"},
	}
	p := *g.DeepCopy()
	if got := suggest.Broader(p, g); len(got) != 0 {
		t.Errorf("equal proposal: %q", got)
	}
	p.Stages = append(p.Stages, lastv1alpha1.StageRemediate)
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("extra stage: %q", got)
	}
	p = *g.DeepCopy()
	p.Providers = append(p.Providers, "gcp")
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("extra provider: %q", got)
	}
	p = *g.DeepCopy()
	p.Match.Argv = []string{"aws", "s3", "ls", "[!-]*"}
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("wildcard position: %q", got)
	}
	p = *g.DeepCopy()
	p.Egress = append(p.Egress, "b.example:443")
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("extra egress: %q", got)
	}
	p = *g.DeepCopy()
	p.Install = []lastv1alpha1.ArgvPattern{{Argv: []string{"pip", "install", "*"}}}
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("inexact install: %q", got)
	}
	p = *g.DeepCopy()
	p.Match.Argv = []string{"aws", "..."}
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("rest: %q", got)
	}
}

func TestSanitize(t *testing.T) {
	if got := suggest.Sanitize("a\x1b[31mb\x07c\u009bd"); got != "abcd" {
		t.Errorf("Sanitize = %q", got)
	}
	if got := suggest.Sanitize("a\nb\tc"); got != "a\nb\tc" {
		t.Errorf("Sanitize dropped whitespace: %q", got)
	}
	if got := suggest.Sanitize("x\x1b]0;title\x07y\r\u202ez"); got != "xyz" {
		t.Errorf("Sanitize = %q", got)
	}
}

func TestBroaderOnGeneralizeOutput(t *testing.T) {
	gen := func(argv ...string) lastv1alpha1.CommandRuleSpec {
		return lastv1alpha1.CommandRuleSpec{
			Stages: []lastv1alpha1.Stage{lastv1alpha1.StageDiscovery}, Providers: []string{"aws"},
			Match: rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: argv}}),
		}
	}
	g := gen("aws", "s3", "ls", "bucket", "--region=us-east-1")
	if got := suggest.Broader(g, g); len(got) != 0 {
		t.Errorf("Broader(g, g) = %q", got)
	}
	p := *g.DeepCopy()
	p.Match.Argv = []string{"aws", "s3", "ls", "*", "--region=*"}
	if got := suggest.Broader(p, g); len(got) != 1 {
		t.Errorf("* against [!-]*: %q", got)
	}
	r := *g.DeepCopy()
	r.Match.Argv = []string{"aws", "s3", "..."}
	if got := suggest.Broader(r, r); len(got) != 0 {
		t.Errorf("... against ...: %q", got)
	}
}
