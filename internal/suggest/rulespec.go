package suggest

import (
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/rules"
)

// stepRef is the reference to another step's output; a command that uses it has
// an argv no human has seen, so no rule can hold it (M6 design R3).
const stepRef = "${steps."

// Unrulable says why no rule can hold cmd, or "" if one can: rules match argv
// only, and never a ${steps. reference.
func Unrulable(cmd lastv1alpha1.Command) string {
	switch {
	case len(cmd.Run.Argv) == 0:
		return "it runs a script, and rules match argv only"
	case slices.ContainsFunc(cmd.Run.Argv, hasStepRef) || cmd.Image != nil &&
		slices.ContainsFunc(cmd.Image.Install, func(c lastv1alpha1.ArgvCommand) bool { return slices.ContainsFunc(c.Argv, hasStepRef) }):
		return "it uses " + stepRef + "...} references, which no rule covers"
	}
	return ""
}

func hasStepRef(a string) bool { return strings.Contains(a, stepRef) }

// RuleSpecFor is the rule for cmd, one command of body (M6 design R13), without
// its match, description or source, which the caller sets: the stages given,
// cmd's providers, cmd's egress, and one exact pattern per image.install argv. A
// command that names no provider gets the sorted union of the providers of every
// command of body (its steps, and its resources' probes and teardown), and
// extraProviders when no command names one, since a rule needs at least one.
// Callers pass the body as the plan's stage authorizes it: a plan other than a
// Create or Update without its resources (rules.PlanCoverage). ok is false when
// no rule can hold cmd (Unrulable) or the rule would have no provider.
func RuleSpecFor(body plan.Body, cmd lastv1alpha1.Command, stages []lastv1alpha1.Stage,
	extraProviders []string) (spec lastv1alpha1.CommandRuleSpec, ok bool) {
	if Unrulable(cmd) != "" {
		return lastv1alpha1.CommandRuleSpec{}, false
	}
	providers := providersFor(body, cmd, extraProviders)
	if len(providers) == 0 {
		return lastv1alpha1.CommandRuleSpec{}, false
	}
	spec = lastv1alpha1.CommandRuleSpec{
		Stages:    slices.Clone(stages),
		Providers: providers,
		Egress:    slices.Clone(cmd.Egress),
	}
	if cmd.Image != nil {
		for _, ic := range cmd.Image.Install {
			spec.Install = append(spec.Install, rules.Exact(ic.Argv))
		}
	}
	return spec, true
}

// providersFor is cmd's providers; else the sorted union of the providers of
// every command of body; else extra.
func providersFor(body plan.Body, cmd lastv1alpha1.Command, extra []string) []string {
	if len(cmd.Providers) > 0 {
		return slices.Clone(cmd.Providers)
	}
	var union []string
	add := func(c lastv1alpha1.Command) {
		for _, p := range c.Providers {
			if !slices.Contains(union, p) {
				union = append(union, p)
			}
		}
	}
	for _, s := range body.Steps {
		add(s)
	}
	for _, r := range body.Resources {
		for _, p := range r.Probes {
			add(p.Command)
		}
		for _, t := range r.Teardown {
			add(t)
		}
	}
	slices.Sort(union)
	if len(union) == 0 {
		return slices.Clone(extra)
	}
	return union
}
