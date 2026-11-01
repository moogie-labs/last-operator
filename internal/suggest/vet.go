// Package suggest vets, names, prompts for and displays operator rule suggestions
// (M7 design §2, S5, S6, S8, S10). It is shared by the operator and the CLI.
package suggest

import (
	"fmt"
	"slices"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

// MaxRationale is the longest rationale Vet accepts, in characters.
const MaxRationale = 500

// Proposal is a rule the operator would suggest. Contains holds the match
// patterns a consolidation must contain; when non-empty it is used in place of
// Examples.
type Proposal struct {
	TargetScope lastv1alpha1.RuleScope
	Namespace   string
	Trigger     lastv1alpha1.SuggestionTrigger
	Rationale   string
	Spec        lastv1alpha1.CommandRuleSpec
	Examples    []lastv1alpha1.Command
	Contains    [][]string
}

// Denied is a Denied suggestion that later proposals are compared against.
type Denied struct {
	Scope           lastv1alpha1.RuleScope
	Namespace, Name string
	Spec            lastv1alpha1.CommandRuleSpec
}

// Vet is the S5 gate followed by the S8 suppression check. It returns ok when the
// proposal may be created. Otherwise reason says why it was dropped or skipped, and
// suppressedBy names the Denied suggestion that suppressed it, if one did.
func Vet(p Proposal, existing []rules.Rule, denied []Denied) (ok bool, reason, suppressedBy string) {
	if p.Spec.Source != nil {
		return false, "the proposal sets source", ""
	}
	if len([]rune(p.Rationale)) > MaxRationale {
		return false, fmt.Sprintf("the rationale is longer than %d characters", MaxRationale), ""
	}
	findings := rules.LintFindings(p.Spec)
	for _, k := range []struct {
		kind rules.FindingKind
		what string
	}{
		{rules.KindNothing, "the pattern matches nothing"},
		{rules.KindCode, "the code the pattern allows can vary"},
		{rules.KindDestructive, "a wildcard follows a destructive verb"},
	} {
		if i := slices.IndexFunc(findings, func(f rules.Finding) bool { return f.Kind == k.kind }); i >= 0 {
			return false, k.what + ": " + findings[i].Message, ""
		}
	}
	match := p.Spec.Match.Argv
	if len(p.Contains) > 0 {
		for _, c := range p.Contains {
			if len(c) == 0 || len(match) == 0 || c[0] != match[0] {
				return false, "argv[0] differs from a consolidated pattern", ""
			}
			if rules.Contains(match, c) != rules.Yes {
				return false, fmt.Sprintf("the match does not contain %q", c), ""
			}
		}
	} else {
		if len(p.Examples) == 0 {
			return false, "the proposal has no examples", ""
		}
		for _, e := range p.Examples {
			if len(match) == 0 || len(e.Run.Argv) == 0 || e.Run.Argv[0] != match[0] {
				return false, "argv[0] differs from an example's argv[0]", ""
			}
			if !rules.Match(match, e.Run.Argv) {
				return false, fmt.Sprintf("the match does not match the example %q", e.Run.Argv), ""
			}
		}
	}
	if covered(p, existing) {
		return false, "already covered by existing rules", ""
	}
	for _, d := range denied {
		if p.TargetScope == lastv1alpha1.RuleScopeNamespace &&
			(d.Scope != lastv1alpha1.RuleScopeNamespace || d.Namespace != p.Namespace) {
			continue
		}
		if v := rules.Subsumes(p.Spec, d.Spec); v == rules.Yes || v == rules.Unknown {
			return false, "suppressed by a denied suggestion", d.Name
		}
	}
	return true, "", ""
}

// covered reports whether existing rules already allow everything the proposal is
// for: every example at every proposed stage, or, for a consolidation, a rule that
// subsumes the proposal.
func covered(p Proposal, existing []rules.Rule) bool {
	if len(p.Contains) > 0 {
		return slices.ContainsFunc(existing, func(r rules.Rule) bool { return rules.Subsumes(r.Spec, p.Spec) == rules.Yes })
	}
	for _, st := range p.Spec.Stages {
		for _, e := range p.Examples {
			if !slices.ContainsFunc(existing, func(r rules.Rule) bool { return rules.Covers(r.Spec, st, e) }) {
				return false
			}
		}
	}
	return len(p.Spec.Stages) > 0
}
