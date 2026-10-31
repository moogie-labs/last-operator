package rules

import (
	"fmt"
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

var fileFlags = []string{"-f", "--filename", "--from-file"}

// Lint returns one human-readable warning per breadth finding in the rule's match
// and install patterns: "..." at position 1; a glob or regex after a destructive verb;
// a file flag together with "..." or a glob or regex; and, for any pattern with a
// file flag, that rules match argv only. Match comes first, then install patterns in
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

	if len(argv) > 1 && argv[1] == restToken {
		warn("'...' at position 1 accepts any arguments after %q", argv[0])
	}
	if verb, at := destructiveVerb(argv); verb != "" {
		warn("glob or regex %q after destructive verb %q", argv[at], verb)
	}
	hasFile := slices.ContainsFunc(argv, isFileFlag)
	if hasFile {
		if slices.ContainsFunc(argv, func(a string) bool { return a == restToken || isGlob(a) || strings.HasPrefix(a, regexPrefix) }) {
			warn("file flag combined with '...', a glob or a regex lets the file argument vary")
		}
		warn("rules match argv only, not files or stdin")
	}
	return out
}

// destructiveVerb returns the first literal destructive verb that has a glob or
// regex somewhere after it, and the index of that first glob or regex.
func destructiveVerb(argv []string) (verb string, at int) {
	for i := 1; i < len(argv); i++ {
		if !isDestructive(argv[i]) {
			continue
		}
		for j := i + 1; j < len(argv); j++ {
			if argv[j] != restToken && (isGlob(argv[j]) || strings.HasPrefix(argv[j], regexPrefix)) {
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
