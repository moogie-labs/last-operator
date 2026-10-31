package rules

import (
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// maxLiterals is how many positional arguments stay literal, argv[0] included.
const maxLiterals = 3

// Generalize derives an editable pattern from a command's argv. argv[0] and the next
// positionals up to three in all stay literal (flag values never count); flag names
// stay literal; a flag's value (the next argument when it is not itself a flag, or the
// part after "=") becomes "*"; every later positional becomes "*". Literals are escaped
// so they match only themselves. A script has no argv, so the result is empty.
func Generalize(cmd lastv1alpha1.Command) lastv1alpha1.ArgvPattern {
	argv := cmd.Run.Argv
	if len(argv) == 0 {
		return lastv1alpha1.ArgvPattern{}
	}
	out := make([]string, 0, len(argv))
	out = append(out, literal(argv[0]))
	literals, wantValue := 1, false
	for _, a := range argv[1:] {
		switch {
		case strings.HasPrefix(a, "-"):
			wantValue = false
			if name, _, ok := strings.Cut(a, "="); ok {
				out = append(out, literal(name)+"=*")
			} else {
				out = append(out, literal(a))
				wantValue = true
			}
		case wantValue:
			wantValue = false
			out = append(out, "*")
		case literals < maxLiterals:
			literals++
			out = append(out, literal(a))
		default:
			out = append(out, "*")
		}
	}
	return lastv1alpha1.ArgvPattern{Argv: out}
}

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
