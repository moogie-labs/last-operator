// Package rules implements allow rules (spec §9): human-written argv patterns that
// let matching commands skip approval. It is pure, with no cluster access, and is
// shared by the gateway, the operator and the CLI. A match that is too loose lets a
// command run without a human approving it, so every ambiguity here fails closed.
package rules

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	restToken   = "..."
	regexPrefix = "re:"
	// maxCachedRegexes bounds the compiled-pattern cache; past it patterns are
	// compiled on every use.
	maxCachedRegexes = 1024
)

var (
	regexCache  sync.Map // "r:"+expression or "g:"+glob -> *regexp.Regexp (nil when it does not compile)
	regexCached atomic.Int64
)

// Match reports whether argv satisfies pattern, position by position. A pattern
// argument is a literal (exact match); a glob when it contains *, ? or [ ("*" is any
// run of characters, "/" included, "?" one character, "[a-z]" / "[!a-z]" a class,
// "\\" escapes, only inside an argument that is a glob, i.e. contains *, ? or [;
// it never spans arguments); or "re:<RE2>", anchored to the whole
// argument. pattern[0] must be a literal, else nothing matches. "..." matches zero or more remaining arguments and is valid
// only as the last element; anywhere else the whole pattern matches nothing. A
// malformed glob or regex matches nothing.
func Match(pattern, argv []string) bool {
	if len(pattern) > 0 && (pattern[0] == restToken || strings.HasPrefix(pattern[0], regexPrefix) || isGlob(pattern[0])) {
		return false
	}
	for i, p := range pattern {
		if p == restToken && i != len(pattern)-1 {
			return false
		}
	}
	for i, p := range pattern {
		if p == restToken {
			return len(argv) >= i
		}
		if i >= len(argv) || !matchArg(p, argv[i]) {
			return false
		}
	}
	return len(argv) == len(pattern)
}

func matchArg(p, arg string) bool {
	if expr, ok := strings.CutPrefix(p, regexPrefix); ok {
		re := compile("r:"+expr, func() (string, bool) { return anchoredRegex(expr) })
		return re != nil && re.MatchString(arg)
	}
	if isGlob(p) {
		re := compile("g:"+p, func() (string, bool) { return globToRegex(p) })
		return re != nil && re.MatchString(arg)
	}
	return p == arg
}

// anchoredRegex wraps expr as ^(?:expr)$, but only after expr parses on its own: an
// unbalanced expression such as "a)|(b" would otherwise close the group early and
// escape the anchors.
func anchoredRegex(expr string) (string, bool) {
	if _, err := syntax.Parse(expr, syntax.Perl); err != nil {
		return "", false
	}
	wrapped := "^(?:" + expr + ")$"
	// Some inputs (such as \Q without \E) parse alone but not once wrapped; Match
	// and Lint must agree that those match nothing.
	if _, err := syntax.Parse(wrapped, syntax.Perl); err != nil {
		return "", false
	}
	return wrapped, true
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

// compile returns the cached regexp for key, building it with expr on a miss. It
// returns nil when the pattern is malformed.
func compile(key string, expr func() (string, bool)) *regexp.Regexp {
	if v, ok := regexCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	var re *regexp.Regexp
	if src, ok := expr(); ok {
		re, _ = regexp.Compile(src) // nil on error
	}
	if regexCached.Load() < maxCachedRegexes {
		if _, loaded := regexCache.LoadOrStore(key, re); !loaded {
			regexCached.Add(1)
		}
	}
	return re
}

// globToRegex translates a glob to an anchored regexp source. It reports false for a
// malformed glob (unterminated or empty class, bad range, trailing backslash).
func globToRegex(g string) (string, bool) {
	rs := []rune(g)
	var b strings.Builder
	b.WriteString("(?s)^")
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '\\':
			i++
			if i >= len(rs) {
				return "", false
			}
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		case '[':
			end, ok := writeClass(&b, rs, i+1)
			if !ok {
				return "", false
			}
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String(), true
}

// writeClass translates the class body starting after '[' and returns the index of
// its closing ']'.
func writeClass(b *strings.Builder, rs []rune, i int) (int, bool) {
	b.WriteString("[")
	if i < len(rs) && (rs[i] == '^' || rs[i] == '!') {
		b.WriteString("^")
		i++
	}
	items := 0
	for ; i < len(rs); i++ {
		if rs[i] == ']' && items > 0 {
			b.WriteString("]")
			return i, true
		}
		lo, next, ok := classChar(rs, i)
		if !ok {
			return 0, false
		}
		i = next
		hi := lo
		if i+1 < len(rs) && rs[i+1] == '-' {
			var ok bool
			if hi, next, ok = classChar(rs, i+2); !ok || hi < lo {
				return 0, false
			}
			i = next
		}
		fmt.Fprintf(b, `\x{%x}`, lo)
		if hi != lo {
			fmt.Fprintf(b, `-\x{%x}`, hi)
		}
		items++
	}
	return 0, false
}

// classChar reads one (possibly backslash-escaped) class character at i and returns
// it with the index of its last rune.
func classChar(rs []rune, i int) (rune, int, bool) {
	if i >= len(rs) || rs[i] == ']' {
		return 0, 0, false
	}
	if rs[i] == '\\' {
		i++
		if i >= len(rs) {
			return 0, 0, false
		}
	}
	return rs[i], i, true
}
