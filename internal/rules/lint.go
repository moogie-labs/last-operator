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
// code that can vary (an interpreter followed by a non-literal position, a
// non-literal position after "--", a code flag whose value is not a literal); a
// file flag together with "..." or a glob or regex; for any pattern with a file
// flag, that rules match argv only; and each position that names a path under
// /work/, where a command's files are written. Match comes first, then install
// patterns in order; within a pattern the findings follow that order.
func Lint(spec lastv1alpha1.CommandRuleSpec) []string {
	fs := LintFindings(spec)
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Message
	}
	return out
}

// Finding is one lint warning. Nothing is set when the pattern can never match
// a command (non-literal argv[0], misplaced "...", invalid regex, malformed
// glob); every other finding is about breadth.
type Finding struct {
	Message string
	Nothing bool
}

// MatchesNothing reports whether any finding says its pattern matches nothing.
func MatchesNothing(fs []Finding) bool {
	return slices.ContainsFunc(fs, func(f Finding) bool { return f.Nothing })
}

// LintFindings is Lint with each warning's kind; Lint returns its messages.
func LintFindings(spec lastv1alpha1.CommandRuleSpec) []Finding {
	out := lintPattern("match", spec.Match.Argv)
	for i, p := range spec.Install {
		out = append(out, lintPattern(fmt.Sprintf("install[%d]", i), p.Argv)...)
	}
	return out
}

func lintPattern(where string, argv []string) []Finding {
	var out []Finding
	warn := func(format string, a ...any) {
		out = append(out, Finding{Message: where + ": " + fmt.Sprintf(format, a...)})
	}
	never := func(format string, a ...any) {
		out = append(out, Finding{Message: where + ": " + fmt.Sprintf(format, a...), Nothing: true})
	}

	if len(argv) > 0 && (argv[0] == restToken || strings.HasPrefix(argv[0], regexPrefix) || isGlob(argv[0])) {
		never("argv[0] must be a literal, so this pattern matches nothing")
	}
	if i := slices.Index(argv, restToken); i >= 0 && i != len(argv)-1 {
		never("'...' is only valid as the last element, so this pattern matches nothing")
	}
	for i, a := range argv {
		if expr, ok := strings.CutPrefix(a, regexPrefix); ok {
			if _, ok := anchoredRegex(expr); !ok && i > 0 {
				never("invalid regex %q at position %d matches nothing", a, i)
			}
		} else if i > 0 && isGlob(a) {
			if _, ok := globToRegex(a); !ok {
				never("malformed glob %q at position %d matches nothing", a, i)
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
	out = append(out, lintCode(where, argv)...)
	if slices.ContainsFunc(argv, isFileFlag) {
		if slices.ContainsFunc(argv, nonLiteral) {
			warn("file flag combined with '...', a glob or a regex lets the file argument vary")
		}
		warn("rules match argv only, not files or stdin")
	}
	for i := 1; i < len(argv); i++ {
		if namesWorkPath(argv[i]) {
			warn("position %d names %q under %s, where a command's files are written; the rule matches the path, not the file's contents", i, argv[i], filesRoot)
		}
	}
	return out
}

// filesRoot is where the sandbox writes a command's files (the /work mount of
// internal/sandbox; Command.Files keys must be under it).
const filesRoot = "/work/"

// namesWorkPath reports whether pattern argument p, or the value of a --flag=p,
// is a literal, glob or regex that starts with filesRoot.
func namesWorkPath(p string) bool {
	if strings.HasPrefix(p, "-") {
		_, v, ok := strings.Cut(p, "=")
		if !ok {
			return false
		}
		p = v
	}
	p = strings.TrimPrefix(p, regexPrefix)
	return strings.HasPrefix(p, filesRoot)
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
			if nonLiteral(argv[j]) {
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

// lintCode warns where the code a pattern allows can vary (M6 design §10, C1): the
// first interpreter (a literal one, at any position) followed by a non-literal
// position; the first "--" followed by one; and each
// code flag whose value is not a literal, outside those two.
func lintCode(where string, argv []string) []Finding {
	var out []Finding
	warn := func(format string, a ...any) {
		out = append(out, Finding{Message: where + ": " + fmt.Sprintf(format, a...)})
	}
	from := len(argv) // code flags at or after this position are already reported
	for i, a := range argv {
		if lit, ok := literalOf(a); !ok || !IsInterpreter(lit) {
			continue
		}
		if j := nextNonLiteral(argv, i+1); j > 0 {
			warn("position %d: %q runs code, and position %d is not a literal, so the code can vary", i, a, j)
			from = i
		}
		break
	}
	if i := slices.IndexFunc(argv, func(a string) bool { lit, ok := literalOf(a); return ok && lit == endOfFlags }); i > 0 {
		if j := nextNonLiteral(argv, i+1); j > 0 {
			warn("position %d is not a literal after \"--\" at position %d, so the command after \"--\" can vary", j, i)
			from = min(from, i)
		}
	}
	for i := 1; i < from; i++ {
		name, value, hasValue := strings.Cut(argv[i], "=")
		lit, ok := literalOf(name)
		if !ok || !isCodeFlag(lit) {
			continue
		}
		// a next argument that starts with "-" is another flag, not this one's value
		if hasValue && nonLiteral(value) || !hasValue && i+1 < from && nonLiteral(argv[i+1]) && !strings.HasPrefix(argv[i+1], "-") {
			warn("position %d: the value of code flag %q is not a literal, so the code can vary", i+boolInt(!hasValue), lit)
		}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nextNonLiteral returns the first position at or after from whose pattern matches
// more than one string, or -1.
func nextNonLiteral(argv []string, from int) int {
	for j := from; j < len(argv); j++ {
		if nonLiteral(argv[j]) {
			return j
		}
	}
	return -1
}

func nonLiteral(p string) bool {
	_, ok := literalOf(p)
	return !ok
}

// literalOf returns the one string pattern argument p matches, when it matches
// exactly one: a plain literal, or a glob made only of escaped characters and
// one-character classes such as Generalize writes ("a\*b", "[r]e:x", "[.]..").
func literalOf(p string) (string, bool) {
	if p == restToken || strings.HasPrefix(p, regexPrefix) {
		return "", false
	}
	if !isGlob(p) {
		return p, true
	}
	rs := []rune(p)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*', '?':
			return "", false
		case '\\':
			i++
			if i >= len(rs) {
				return "", false
			}
			b.WriteRune(rs[i])
		case '[':
			j := i + 1
			if j < len(rs) && rs[j] == '\\' {
				j++
			} else if j < len(rs) && (rs[j] == '!' || rs[j] == '^' || rs[j] == ']') {
				return "", false
			}
			if j+1 >= len(rs) || rs[j+1] != ']' {
				return "", false
			}
			b.WriteRune(rs[j])
			i = j + 1
		default:
			b.WriteRune(rs[i])
		}
	}
	return b.String(), true
}
