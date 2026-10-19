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
// summary, each step, and each resource with its probes and outputs. hash is
// the hash the CLI computed from what it shows; stage is the stage the plan
// is approved for; files shows file contents.
func renderPlan(w io.Writer, p *lastv1alpha1.Plan, stage lastv1alpha1.Stage, hash string, files bool) {
	writef(w, "Plan %s (stage %s)\n", hash, stage)
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
		renderCommand(w, "     ", s, files)
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
			renderCommand(w, "      ", pr.Command, files)
		}
		if r.OutputsSecretRef != nil {
			writef(w, "    outputs to Secret %s:\n", clean(r.OutputsSecretRef.Name))
			for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
				writef(w, "      %s = %s\n", clean(k), clean(r.Outputs[k]))
			}
		}
		for _, t := range r.Teardown {
			writef(w, "    teardown %s\n", clean(t.ID))
			renderCommand(w, "      ", t, files)
		}
	}
}

// renderPending writes a discovery command awaiting approval, with the
// model's reason for it, under hash as renderPlan does.
func renderPending(w io.Writer, pc *lastv1alpha1.PendingCommand, hash string, files bool) {
	writef(w, "Discovery command %s (stage Discovery)\n", hash)
	if pc.Reason != "" {
		writef(w, "Reason: %s\n", clean(pc.Reason))
	}
	renderCommand(w, "  ", pc.Command, files)
}

// shellWords quotes an argv the way a shell would need it, one word per element.
func shellWords(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellWord(a)
	}
	return strings.Join(words, " ")
}

// renderCommand writes one command: argv or script, files by path and size
// (with their contents if files), Providers, egress and timeout.
func renderCommand(w io.Writer, indent string, c lastv1alpha1.Command, files bool) {
	if len(c.Run.Argv) > 0 {
		writef(w, "%srun:       %s\n", indent, shellWords(c.Run.Argv))
	} else {
		writef(w, "%sscript:\n", indent)
		renderLines(w, indent, c.Run.Script)
	}
	for _, path := range slices.Sorted(maps.Keys(c.Files)) {
		writef(w, "%sfile:      %s (%d bytes)\n", indent, clean(path), len(c.Files[path]))
		if files {
			renderLines(w, indent, c.Files[path])
		}
	}
	if len(c.Providers) > 0 {
		writef(w, "%sproviders: %s\n", indent, clean(strings.Join(c.Providers, ", ")))
	}
	if len(c.Egress) > 0 {
		writef(w, "%segress:    %s\n", indent, clean(strings.Join(c.Egress, ", ")))
	}
	if c.Image != nil {
		for _, install := range c.Image.Install {
			writef(w, "%sinstall:   %s\n", indent, shellWords(install.Argv))
		}
	}
	if c.Timeout != nil {
		writef(w, "%stimeout:   %s\n", indent, c.Timeout.Duration)
	}
}

// renderLines writes a script or a file's content, each line escaped and
// marked, so no line of it passes for one of the CLI's own.
func renderLines(w io.Writer, indent, text string) {
	for line := range strings.SplitSeq(text, "\n") {
		writef(w, "%s  | %s\n", indent, clean(line))
	}
}

// renderSubject writes what the Intent asks its user to decide in stage,
// under hash, the hash verifyHash computed for it.
func renderSubject(w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, hash string, files bool) {
	if stage == lastv1alpha1.StageDiscovery {
		renderPending(w, in.Status.PendingCommand, hash, files)
		return
	}
	renderPlan(w, in.Status.Plan, stage, hash, files)
}

// hasFiles reports whether anything renderSubject shows for stage carries files.
func hasFiles(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) bool {
	if stage == lastv1alpha1.StageDiscovery {
		return len(in.Status.PendingCommand.Command.Files) > 0
	}
	cmds := slices.Clone(in.Status.Plan.Steps)
	for _, r := range in.Status.Plan.Resources {
		for _, pr := range r.Probes {
			cmds = append(cmds, pr.Command)
		}
		cmds = append(cmds, r.Teardown...)
	}
	return slices.ContainsFunc(cmds, func(c lastv1alpha1.Command) bool { return len(c.Files) > 0 })
}

// renderQuestion writes a clarifying question and its numbered options.
func renderQuestion(w io.Writer, q lastv1alpha1.Question) {
	writef(w, "%s\n", clean(q.Text))
	for i, o := range q.Options {
		writef(w, "  %d) %s\n", i+1, clean(o))
	}
}

// printDecisionCommands writes the approve and deny commands bound to hash,
// so a later decision covers exactly what was shown.
func printDecisionCommands(w io.Writer, in *lastv1alpha1.Intent, hash string) {
	writef(w, "Approve: kubectl last approve %s -n %s --hash %s\n", in.Name, in.Namespace, hash)
	writef(w, "Deny:    kubectl last deny %s -n %s --hash %s --reason R\n", in.Name, in.Namespace, hash)
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
