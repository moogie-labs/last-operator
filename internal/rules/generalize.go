package rules

import (
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// maxLiterals is how many positional arguments stay literal, argv[0] included.
const maxLiterals = 3

// positionalValue stands for a positional value slot: any argument that does not
// start with "-", so a wildcard cannot swallow a flag such as --kubeconfig.
const positionalValue = "[!-]*"

// Generalize derives an editable pattern from a command's argv. argv[0] and the next
// positionals up to three in all stay literal (flag values never count); flag names
// stay literal; a flag's separate value (the next argument when it is not itself a
// flag) becomes "[!-]*", so it can never stand for a flag, and the part after "=" of
// a --flag=value becomes "*" (it cannot add an argument); every later positional
// becomes "[!-]*". Code never varies (M6 design §10, C1): the value of a code flag
// (codeFlags), in either form, stays literal, and from an interpreter word
// (IsInterpreter, at any position) or a "--" argument on, every argument stays
// literal. Literals are escaped so they match only themselves. The empty string
// stays the empty literal. A script has no argv, so the result is empty.
func Generalize(cmd lastv1alpha1.Command) lastv1alpha1.ArgvPattern {
	argv := cmd.Run.Argv
	if len(argv) == 0 {
		return lastv1alpha1.ArgvPattern{}
	}
	out := make([]string, 0, len(argv))
	out = append(out, literal(argv[0]))
	fixed := IsInterpreter(argv[0])
	literals, wantValue, codeValue := 1, false, false
	for _, a := range argv[1:] {
		switch {
		case fixed:
			out = append(out, literal(a))
		case a == endOfFlags || IsInterpreter(a):
			fixed = true
			out = append(out, literal(a))
		case strings.HasPrefix(a, "-"):
			name, _, hasValue := strings.Cut(a, "=")
			wantValue, codeValue = !hasValue, isCodeFlag(name)
			if hasValue && !codeValue {
				out = append(out, escapeGlob(name, "")+"=*")
			} else {
				out = append(out, literal(a))
			}
		case wantValue:
			wantValue = false
			switch {
			case codeValue || a == "":
				out = append(out, literal(a))
			default:
				out = append(out, positionalValue)
			}
		case literals < maxLiterals:
			literals++
			out = append(out, literal(a))
		case a == "":
			out = append(out, "") // the empty literal matches only itself
		default:
			out = append(out, positionalValue)
		}
	}
	return lastv1alpha1.ArgvPattern{Argv: out}
}

// endOfFlags is the "--" argument: what follows is often another command
// (kubectl exec, docker run), so nothing after it is generalized.
const endOfFlags = "--"

// interpreters are programs that run code given in their arguments. A shell or
// interpreter word anywhere in an argv (kubectl exec ... -- sh, docker exec ctr
// bash, env bash, xargs sh) makes the rest of it code.
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true, "ksh": true, "mksh": true, "csh": true, "tcsh": true,
	"fish": true, "python": true, "node": true, "nodejs": true, "deno": true, "bun": true, "perl": true, "ruby": true,
	"pwsh": true, "powershell": true, "php": true, "lua": true, "luajit": true, "rscript": true, "osascript": true,
	"tclsh": true, "awk": true, "gawk": true, "mawk": true, "nawk": true,
}

// IsInterpreter reports whether a names a known interpreter: its base name, lower
// case, without ".exe" or a trailing version (python3.12, lua5.4), is in the list.
func IsInterpreter(a string) bool {
	base := strings.ToLower(a[strings.LastIndexAny(a, `/\`)+1:])
	base = strings.TrimSuffix(base, ".exe")
	return interpreters[strings.TrimRight(base, "0123456789.")]
}

// codeFlags take code, or a command line, as their value: -c (sh, python, su),
// -e/-E (perl, ruby, node, osascript), --eval, -Command (pwsh), -r (php), and a
// few more. Their value stays literal wherever they appear.
var codeFlags = []string{"-c", "-e", "-E", "--eval", "-Command", "-command", "--command", "-r", "-p", "--print",
	"-EncodedCommand", "-encodedcommand", "-S", "--split-string", "-exec", "-execdir"}

func isCodeFlag(name string) bool { return slices.Contains(codeFlags, name) }

// literal escapes s so Match treats it as an exact string: glob characters are
// backslash-escaped, and a leading "re:" or a lone "..." is defused with a class.
func literal(s string) string {
	switch {
	case s == restToken:
		return "[.].."
	case strings.HasPrefix(s, regexPrefix):
		s = "[r]" + s[1:]
		return escapeGlob(s[3:], "[r]")
	case isGlob(s):
		return escapeGlob(s, "")
	}
	return s
}

func escapeGlob(s, prefix string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range s {
		if r == '*' || r == '?' || r == '[' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Exact returns the pattern that matches argv and nothing else: each argument
// is escaped like Generalize's literals. An empty argv gives an empty pattern.
func Exact(argv []string) lastv1alpha1.ArgvPattern {
	if len(argv) == 0 {
		return lastv1alpha1.ArgvPattern{}
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = literal(a)
	}
	return lastv1alpha1.ArgvPattern{Argv: out}
}
