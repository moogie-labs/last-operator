package rules

import (
	"fmt"
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

var fileFlags = []string{"-f", "--filename", "--from-file"}

// Lint returns one human-readable warning per breadth finding in the rule's match
// and install patterns: patterns that match nothing (non-literal argv[0], misplaced
// "...", invalid regex, malformed glob); "..." at position 1; a wildcard after a
// destructive verb; a positional "*" or "..." that also matches flag-shaped arguments;
// a file flag together with "..." or a glob or regex; and, for any pattern with a file
// flag, that rules match argv only. Match comes first, then install patterns in
// order; within a pattern the findings follow that order.
func Lint(spec lastv1alpha1.CommandRuleSpec) []string {
	out := lintPattern("match", spec.Match.Argv)
	for i, p := range spec.Install {
		out = append(out, lintPattern(fmt.Sprintf("install[%d]", i), p.Argv)...)
	}
	return out
}

func lintPattern(where string, argv []string) []string {
	var out []string
	warn := func(format string, a ...any) { out = append(out, where+": "+fmt.Sprintf(format, a...)) }

	if len(argv) > 0 && (argv[0] == restToken || strings.HasPrefix(argv[0], regexPrefix) || isGlob(argv[0])) {
		warn("argv[0] must be a literal, so this pattern matches nothing")
	}
	if i := slices.Index(argv, restToken); i >= 0 && i != len(argv)-1 {
		warn("'...' is only valid as the last element, so this pattern matches nothing")
	}
	for i, a := range argv {
		if expr, ok := strings.CutPrefix(a, regexPrefix); ok {
			if _, ok := anchoredRegex(expr); !ok && i > 0 {
				warn("invalid regex %q at position %d matches nothing", a, i)
			}
		} else if i > 0 && isGlob(a) {
			if _, ok := globToRegex(a); !ok {
				warn("malformed glob %q at position %d matches nothing", a, i)
			}
		}
	}
	if len(argv) > 1 && argv[1] == restToken {
		warn("'...' at position 1 accepts any arguments after %q", argv[0])
	}
	if verb, at := destructiveVerb(argv); verb != "" {
		warn("wildcard %q after destructive verb %q", argv[at], verb)
	}
	for i := 1; i < len(argv); i++ {
		switch {
		case argv[i] == "*" && !isFlagWithoutValue(argv[i-1]):
			warn("position %d: `*` also matches flags such as --kubeconfig; use `[!-]*` for a value", i)
		case argv[i] == restToken && i != 1:
			warn("position %d: `...` also accepts flags such as --kubeconfig", i)
		}
	}
	if slices.ContainsFunc(argv, isFileFlag) {
		if slices.ContainsFunc(argv, func(a string) bool { return a == restToken || isGlob(a) || strings.HasPrefix(a, regexPrefix) }) {
			warn("file flag combined with '...', a glob or a regex lets the file argument vary")
		}
		warn("rules match argv only, not files or stdin")
	}
	return out
}

// isFlagWithoutValue reports whether a is a flag that takes its value from the next
// argument (starts with "-", no "="), making a following "*" a flag value.
func isFlagWithoutValue(a string) bool {
	return strings.HasPrefix(a, "-") && !strings.Contains(a, "=")
}

// destructiveVerb returns the first literal destructive verb (argv[0] included) that
// has a wildcard (glob, regex or "...") somewhere after it, and the index of that
// first wildcard.
func destructiveVerb(argv []string) (verb string, at int) {
	for i := range argv {
		if !isDestructive(argv[i]) {
			continue
		}
		for j := i + 1; j < len(argv); j++ {
			if argv[j] == restToken || isGlob(argv[j]) || strings.HasPrefix(argv[j], regexPrefix) {
				return argv[i], j
			}
		}
	}
	return "", 0
}

func isDestructive(a string) bool {
	switch a {
	case "delete", "rm", "destroy", "drop", "purge":
		return true
	}
	return strings.HasPrefix(a, "terminate-")
}

func isFileFlag(a string) bool {
	for _, f := range fileFlags {
		if a == f || strings.HasPrefix(a, f+"=") {
			return true
		}
	}
	return false
}
