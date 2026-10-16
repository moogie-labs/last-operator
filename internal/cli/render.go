package cli

import (
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/api/meta"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// condReady is the Intent condition whose message says what the Intent waits for.
const condReady = "Ready"

// clean makes text the model wrote safe to show an approver. Control
// characters (escape sequences, carriage returns, newlines) and bidirectional
// overrides could hide or reorder what is shown, so they are written as
// \uXXXX.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || isBidi(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isBidi(r rune) bool {
	return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellWord writes one argv element, quoted when it would be ambiguous
// unquoted. strconv.Quote escapes control characters and overrides too.
func shellWord(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// renderPlan writes a plan as its approver reviews it (M3 spec §5): the
// summary, each step, and each resource with its probes and outputs.
func renderPlan(w io.Writer, p *lastv1alpha1.Plan) {
	pf(w, "Plan %s (stage Create)\n", p.Hash)
	if p.Summary != "" {
		pf(w, "Summary: %s\n", clean(p.Summary))
	}
	pln(w, "Steps:")
	for i, s := range p.Steps {
		pf(w, "  %d. %s", i+1, clean(s.ID))
		if s.Reason != "" {
			pf(w, ": %s", clean(s.Reason))
		}
		pln(w)
		renderCommand(w, "     ", s)
	}
	if len(p.Resources) > 0 {
		pln(w, "Resources:")
	}
	for _, r := range p.Resources {
		pf(w, "  - %s\n", clean(r.Description))
		if r.Identity != nil {
			pf(w, "    identity: %s\n", clean(string(r.Identity.Raw)))
		}
		if r.Desired != nil {
			pf(w, "    desired:  %s\n", clean(string(r.Desired.Raw)))
		}
		for _, pr := range r.Probes {
			pf(w, "    probe %s, assert %s\n", clean(pr.ID), clean(pr.Assert))
			renderCommand(w, "      ", pr.Command)
		}
		if r.OutputsSecretRef != nil {
			pf(w, "    outputs to Secret %s:\n", clean(r.OutputsSecretRef.Name))
			for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
				pf(w, "      %s = %s\n", clean(k), clean(r.Outputs[k]))
			}
		}
	}
}

// renderPending writes a discovery command awaiting approval, with the
// model's reason for it.
func renderPending(w io.Writer, pc *lastv1alpha1.PendingCommand) {
	pf(w, "Discovery command %s (stage Discovery)\n", pc.Hash)
	if pc.Reason != "" {
		pf(w, "Reason: %s\n", clean(pc.Reason))
	}
	renderCommand(w, "  ", pc.Command)
}

// renderCommand writes one command: argv or script, files by path and size,
// Providers, egress and timeout.
func renderCommand(w io.Writer, indent string, c lastv1alpha1.Command) {
	if len(c.Run.Argv) > 0 {
		words := make([]string, len(c.Run.Argv))
		for i, a := range c.Run.Argv {
			words[i] = shellWord(a)
		}
		pf(w, "%srun:       %s\n", indent, strings.Join(words, " "))
	} else {
		pf(w, "%sscript:\n", indent)
		for line := range strings.SplitSeq(c.Run.Script, "\n") {
			pf(w, "%s  | %s\n", indent, clean(line))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(c.Files)) {
		pf(w, "%sfile:      %s (%d bytes)\n", indent, clean(path), len(c.Files[path]))
	}
	if len(c.Providers) > 0 {
		pf(w, "%sproviders: %s\n", indent, clean(strings.Join(c.Providers, ", ")))
	}
	if len(c.Egress) > 0 {
		pf(w, "%segress:    %s\n", indent, clean(strings.Join(c.Egress, ", ")))
	}
	if c.Timeout != nil {
		pf(w, "%stimeout:   %s\n", indent, c.Timeout.Duration)
	}
}

// renderSubject writes what the Intent asks its user to decide in stage.
//
//nolint:unused // used by the approve and deny commands (Task 3)
func renderSubject(w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) {
	if stage == lastv1alpha1.StageDiscovery {
		renderPending(w, in.Status.PendingCommand)
		return
	}
	renderPlan(w, in.Status.Plan)
}

// printStatus writes the Intent's phase and, from its Ready condition, why.
func printStatus(w io.Writer, in *lastv1alpha1.Intent) {
	pf(w, "Intent %s/%s: %s", in.Namespace, in.Name, phaseOf(in))
	if c := meta.FindStatusCondition(in.Status.Conditions, condReady); c != nil && c.Message != "" {
		pf(w, " (%s)", clean(c.Message))
	}
	pln(w)
}

// phaseOf is the Intent's phase; a new Intent the operator has not seen yet is Pending.
func phaseOf(in *lastv1alpha1.Intent) lastv1alpha1.Phase {
	if in.Status.Phase == "" {
		return lastv1alpha1.PhasePending
	}
	return in.Status.Phase
}

// pf and pln write to w, ignoring write errors as fmt.Println does.
func pf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func pln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }
