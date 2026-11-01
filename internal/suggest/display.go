package suggest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

// Broader returns one human-readable line for each place p is broader than g (the
// rule the CLI's own choice would create): a pattern position g's does not
// contain, an extra stage, an extra provider, extra egress, or an install entry
// that no install entry of g contains.
func Broader(p, g lastv1alpha1.CommandRuleSpec) []string {
	var out []string
	pm, gm := p.Match.Argv, g.Match.Argv
	for i, a := range pm {
		switch {
		case i >= len(gm):
			out = append(out, fmt.Sprintf("match[%d] %q allows arguments the plain rule does not", i, a))
		case a == "...":
			if rules.Contains(gm[i:], pm[i:]) != rules.Yes {
				out = append(out, fmt.Sprintf("match[%d] \"...\" allows arguments the plain rule does not", i))
			}
		case rules.Contains(gm[i:i+1], []string{a}) != rules.Yes:
			out = append(out, fmt.Sprintf("match[%d] %q is broader than %q", i, a, gm[i]))
		}
	}
	for _, s := range p.Stages {
		if !slices.ContainsFunc(g.Stages, func(x lastv1alpha1.Stage) bool { return sameStage(x, s) }) {
			out = append(out, fmt.Sprintf("stage %s is not in the plain rule", s))
		}
	}
	for _, pr := range p.Providers {
		if !slices.Contains(g.Providers, pr) {
			out = append(out, fmt.Sprintf("provider %q is not in the plain rule", pr))
		}
	}
	for _, e := range p.Egress {
		if !slices.Contains(g.Egress, e) {
			out = append(out, fmt.Sprintf("egress %q is not in the plain rule", e))
		}
	}
	for i, in := range p.Install {
		if !slices.ContainsFunc(g.Install, func(x lastv1alpha1.ArgvPattern) bool { return rules.Contains(x.Argv, in.Argv) == rules.Yes }) {
			out = append(out, fmt.Sprintf("install[%d] %q is not exact", i, in.Argv))
		}
	}
	return out
}

// sameStage treats Create and Update as one class, as rules do.
func sameStage(a, b lastv1alpha1.Stage) bool {
	isCU := func(s lastv1alpha1.Stage) bool { return s == lastv1alpha1.StageCreate || s == lastv1alpha1.StageUpdate }
	return a == b || isCU(a) && isCU(b)
}

// escapes matches OSC sequences (ESC ] ... BEL or ESC \) and CSI sequences (ESC [
// params final).
var escapes = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b\[[0-?]*[ -/]*[@-~]`)

// Sanitize strips terminal escape sequences, control characters (newline and tab
// stay) and invisible format characters such as bidi overrides, so operator text
// cannot drive the terminal.
func Sanitize(s string) string {
	s = escapes.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
