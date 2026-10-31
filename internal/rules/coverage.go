package rules

import (
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// stepRef is the reference syntax for another step's output; a command that uses it
// has an argv no human has seen, so no rule covers it.
const stepRef = "${steps."

// Ref identifies a rule: its scope ("namespace" or "cluster"), namespace and name.
type Ref struct{ Scope, Namespace, Name string }

// String renders the ref as "<scope>/<namespace or ->/<name>".
func (r Ref) String() string {
	ns := r.Namespace
	if ns == "" {
		ns = "-"
	}
	return r.Scope + "/" + ns + "/" + r.Name
}

// Rule is a rule spec with its identity.
type Rule struct {
	Ref  Ref
	Spec lastv1alpha1.CommandRuleSpec
}

// Covers reports whether rule lets cmd skip approval at stage: the stage is listed,
// the command's providers are a subset of the rule's, every egress entry is covered
// by a rule egress entry, run.argv matches match.argv, every image.install argv
// matches an install pattern, and the command is an argv (not a script) with no
// ${steps. reference. Covering never widens policy; callers still run CheckCommand.
func Covers(rule lastv1alpha1.CommandRuleSpec, stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) bool {
	if !slices.Contains(rule.Stages, stage) {
		return false
	}
	if cmd.Run.Script != "" || len(cmd.Run.Argv) == 0 || hasStepRef(cmd.Run.Argv) {
		return false
	}
	for _, p := range cmd.Providers {
		if !slices.Contains(rule.Providers, p) {
			return false
		}
	}
	if !egressCovered(rule.Egress, cmd.Egress) {
		return false
	}
	if !Match(rule.Match.Argv, cmd.Run.Argv) {
		return false
	}
	if cmd.Image != nil {
		for _, ic := range cmd.Image.Install {
			if hasStepRef(ic.Argv) || !slices.ContainsFunc(rule.Install, func(p lastv1alpha1.ArgvPattern) bool {
				return Match(p.Argv, ic.Argv)
			}) {
				return false
			}
		}
	}
	return true
}

func hasStepRef(argv []string) bool {
	return slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, stepRef) })
}

// egressCovered reports whether every wanted entry is covered by some allowed entry.
// A rule whose egress fails to parse covers nothing; so does a wanted entry that does
// not parse (including host patterns, which policy cannot enforce yet).
func egressCovered(allowed, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	rules, err := policy.ParseEgress(allowed)
	if err != nil {
		return false
	}
	for _, w := range wanted {
		r, err := sandbox.ParseEgress(w)
		if err != nil || !slices.ContainsFunc(rules, func(a sandbox.EgressRule) bool { return policy.EgressCovers(a, r) }) {
			return false
		}
	}
	return true
}

// Coverage assigns each step the first rule, in input order, that covers it. It
// returns the covered steps by ID and the uncovered step IDs in plan order. Callers
// sort rules (cluster first, then by name) so the choice is deterministic.
func Coverage(rules []Rule, stage lastv1alpha1.Stage, steps []lastv1alpha1.Command) (covered map[string]Ref, uncovered []string) {
	covered = map[string]Ref{}
	for _, s := range steps {
		i := slices.IndexFunc(rules, func(r Rule) bool { return Covers(r.Spec, stage, s) })
		if i < 0 {
			uncovered = append(uncovered, s.ID)
			continue
		}
		covered[s.ID] = rules[i].Ref
	}
	return covered, uncovered
}
