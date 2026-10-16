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
		if r != '\t' && (!unicode.IsGraphic(r) || isBidi(r)) {
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
// unquoted. The quoting is Go-style (strconv.Quote, which also escapes control
// characters and overrides) and for display only: argv is executed directly,
// never through a shell.
func shellWord(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// renderPlan writes a plan as its approver reviews it (M3 spec §5): the
// summary, each step, and each resource with its probes and outputs.
func renderPlan(w io.Writer, p *lastv1alpha1.Plan) {
	writef(w, "Plan %s (stage Create)\n", p.Hash)
	if p.Summary != "" {
		writef(w, "Summary: %s\n", clean(p.Summary))
	}
	writeln(w, "Steps:")
	for i, s := range p.Steps {
		writef(w, "  %d. %s", i+1, clean(s.ID))
		if s.Reason != "" {
			writef(w, ": %s", clean(s.Reason))
		}
		writeln(w)
		renderCommand(w, "     ", s)
	}
	if len(p.Resources) > 0 {
		writeln(w, "Resources:")
	}
	for _, r := range p.Resources {
		writef(w, "  - %s\n", clean(r.Description))
		if r.Identity != nil {
			writef(w, "    identity: %s\n", clean(string(r.Identity.Raw)))
		}
		if r.Desired != nil {
			writef(w, "    desired:  %s\n", clean(string(r.Desired.Raw)))
		}
		for _, pr := range r.Probes {
			writef(w, "    probe %s, assert %s\n", clean(pr.ID), clean(pr.Assert))
			renderCommand(w, "      ", pr.Command)
		}
		if r.OutputsSecretRef != nil {
			writef(w, "    outputs to Secret %s:\n", clean(r.OutputsSecretRef.Name))
			for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
				writef(w, "      %s = %s\n", clean(k), clean(r.Outputs[k]))
			}
		}
		for _, t := range r.Teardown {
			writef(w, "    teardown %s\n", clean(t.ID))
			renderCommand(w, "      ", t)
		}
	}
}

// renderPending writes a discovery command awaiting approval, with the
// model's reason for it.
func renderPending(w io.Writer, pc *lastv1alpha1.PendingCommand) {
	writef(w, "Discovery command %s (stage Discovery)\n", pc.Hash)
	if pc.Reason != "" {
		writef(w, "Reason: %s\n", clean(pc.Reason))
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
		writef(w, "%srun:       %s\n", indent, strings.Join(words, " "))
	} else {
		writef(w, "%sscript:\n", indent)
		for line := range strings.SplitSeq(c.Run.Script, "\n") {
			writef(w, "%s  | %s\n", indent, clean(line))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(c.Files)) {
		writef(w, "%sfile:      %s (%d bytes)\n", indent, clean(path), len(c.Files[path]))
	}
	if len(c.Providers) > 0 {
		writef(w, "%sproviders: %s\n", indent, clean(strings.Join(c.Providers, ", ")))
	}
	if len(c.Egress) > 0 {
		writef(w, "%segress:    %s\n", indent, clean(strings.Join(c.Egress, ", ")))
	}
	if c.Image != nil {
		writef(w, "%simage:     %d install commands\n", indent, len(c.Image.Install))
	}
	if c.Timeout != nil {
		writef(w, "%stimeout:   %s\n", indent, c.Timeout.Duration)
	}
}

// renderSubject writes what the Intent asks its user to decide in stage.
func renderSubject(w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) {
	if stage == lastv1alpha1.StageDiscovery {
		renderPending(w, in.Status.PendingCommand)
		return
	}
	renderPlan(w, in.Status.Plan)
}

// printStatus writes the Intent's phase and, from its Ready condition, why.
func printStatus(w io.Writer, in *lastv1alpha1.Intent) {
	writef(w, "Intent %s/%s: %s", in.Namespace, in.Name, phaseOf(in))
	if c := meta.FindStatusCondition(in.Status.Conditions, condReady); c != nil && c.Message != "" {
		writef(w, " (%s)", clean(c.Message))
	}
	writeln(w)
}

// phaseOf is the Intent's phase; a new Intent the operator has not seen yet is Pending.
func phaseOf(in *lastv1alpha1.Intent) lastv1alpha1.Phase {
	if in.Status.Phase == "" {
		return lastv1alpha1.PhasePending
	}
	return in.Status.Phase
}

// writef and writeln write to w and discard the write error, which errcheck
// would flag on a bare fmt.Fprint*.
func writef(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func writeln(w io.Writer, a ...any) { _, _ = fmt.Fprintln(w, a...) }
