// Package rules implements allow rules (spec §9): human-written argv patterns that
// let matching commands skip approval. It is pure, with no cluster access, and is
// shared by the gateway, the operator and the CLI. A match that is too loose lets a
// command run without a human approving it, so every ambiguity here fails closed.
package rules

import (
	"path"
	"regexp"
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
	regexCache  sync.Map // expression -> *regexp.Regexp (nil when it does not compile)
	regexCached atomic.Int64
)

// Match reports whether argv satisfies pattern, position by position. A pattern
// argument is a literal (exact match); a glob when it contains *, ? or [ (path.Match
// semantics, within one argument, never spanning arguments); or "re:<RE2>", anchored
// to the whole argument. "..." matches zero or more remaining arguments and is valid
// only as the last element; anywhere else the whole pattern matches nothing. A
// malformed glob or regex matches nothing.
func Match(pattern, argv []string) bool {
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
		re := compile(expr)
		return re != nil && re.MatchString(arg)
	}
	if isGlob(p) {
		ok, err := path.Match(p, arg)
		return err == nil && ok
	}
	return p == arg
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

func compile(expr string) *regexp.Regexp {
	if v, ok := regexCache.Load(expr); ok {
		return v.(*regexp.Regexp)
	}
	re, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		re = nil
	}
	if regexCached.Load() < maxCachedRegexes {
		if _, loaded := regexCache.LoadOrStore(expr, re); !loaded {
			regexCached.Add(1)
		}
	}
	return re
}
