package rules

import (
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/policy"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// Verdict is a three-valued answer: a comparison that cannot be decided is
// Unknown, and callers fail closed on it.
type Verdict int

const (
	// No: the answer is decided and false.
	No Verdict = iota
	// Yes: the answer is decided and true.
	Yes
	// Unknown: the answer cannot be decided.
	Unknown
)

func (v Verdict) String() string {
	switch v {
	case No:
		return "No"
	case Yes:
		return "Yes"
	}
	return "Unknown"
}

// all combines verdicts: No if any is No, else Unknown if any is Unknown, else Yes.
func all(vs ...Verdict) Verdict {
	if slices.Contains(vs, No) {
		return No
	}
	if slices.Contains(vs, Unknown) {
		return Unknown
	}
	return Yes
}

// Contains reports whether every argv narrow matches is matched by broad (M7
// design S9). Positions are compared pairwise. A "..." in broad covers every
// remaining position of narrow, narrow's own "..." included; a "..." in narrow is
// contained only by a "...". Per position: a narrow position that matches exactly
// one string (literalOf) is decided by Match; identical positions are equal;
// anything is inside "*"; "[!-]*" contains a glob whose first character can't be
// "-"; a broad position that matches one string never contains a glob with a
// "*" or "?". Everything else (glob in glob, a non-literal "re:") is Unknown, as is a
// pattern that matches nothing (a KindNothing lint finding). No wins over Unknown.
func Contains(broad, narrow []string) Verdict {
	if matchesNothing(broad) || matchesNothing(narrow) {
		return Unknown
	}
	bFixed, bRest := cutRest(broad)
	nFixed, nRest := cutRest(narrow)
	switch {
	case bRest && len(nFixed) < len(bFixed):
		return No // narrow matches an argv shorter than broad's fixed positions
	case !bRest && (nRest || len(nFixed) != len(bFixed)):
		return No // narrow matches an argv of another length
	}
	vs := make([]Verdict, len(bFixed))
	for i, b := range bFixed {
		vs[i] = containsArg(b, nFixed[i])
	}
	return all(vs...)
}

func matchesNothing(argv []string) bool { return MatchesNothing(lintPattern("", argv)) }

// cutRest splits a trailing "..." off pattern.
func cutRest(pattern []string) ([]string, bool) {
	if n := len(pattern); n > 0 && pattern[n-1] == restToken {
		return pattern[:n-1], true
	}
	return pattern, false
}

// containsArg compares one position of patterns that match something; neither
// is "...".
func containsArg(b, n string) Verdict {
	if lit, ok := literalOf(n); ok {
		if matchArg(b, lit) {
			return Yes
		}
		return No
	}
	switch {
	case b == n, b == "*":
		return Yes
	case b == positionalValue && !strings.HasPrefix(n, regexPrefix) && !mayStartWithDash(n):
		return Yes
	}
	if _, ok := literalOf(b); ok && !strings.HasPrefix(n, regexPrefix) && hasWildcard(n) {
		// b matches one string. n matches something, and a "*" or "?" in it can
		// match another character too, so n matches at least two strings.
		return No
	}
	return Unknown
}

// hasWildcard reports whether glob holds a "*" or "?" outside an escape and a
// class. glob is well formed.
func hasWildcard(glob string) bool {
	rs := []rune(glob)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*', '?':
			return true
		case '\\':
			i++
		case '[':
			var scratch strings.Builder
			end, ok := writeClass(&scratch, rs, i+1)
			if !ok {
				return false
			}
			i = end
		}
	}
	return false
}

// mayStartWithDash reports whether glob's first element (a character, an escaped
// character, a class, "?" or "*") matches "-". glob is well formed.
func mayStartWithDash(glob string) bool {
	rs := []rune(glob)
	end := 0
	switch rs[0] {
	case '\\':
		end = 1
	case '[':
		var scratch strings.Builder
		end, _ = writeClass(&scratch, rs, 1)
	}
	first := string(rs[:end+1])
	re := compile("g:"+first, func() (string, bool) { return globToRegex(first) })
	return re == nil || re.MatchString("-")
}

// Subsumes reports whether rule n lets through every command that rule d does (M7
// design S8): d's match is contained in n's (Contains); each of d's stages is
// covered by n's, with Create and Update as one class (coversStage); d's
// providers are a subset of n's; n's egress covers d's; and each of d's install
// patterns is contained in one of n's. It is No if any of these is No, else
// Unknown if any is Unknown.
func Subsumes(n, d lastv1alpha1.CommandRuleSpec) Verdict {
	return all(
		Contains(n.Match.Argv, d.Match.Argv),
		decided(!slices.ContainsFunc(d.Stages, func(s lastv1alpha1.Stage) bool { return !coversStage(n.Stages, s) })),
		decided(!slices.ContainsFunc(d.Providers, func(p string) bool { return !slices.Contains(n.Providers, p) })),
		egressContains(n.Egress, d.Egress),
		installContains(n.Install, d.Install),
	)
}

func decided(b bool) Verdict {
	if b {
		return Yes
	}
	return No
}

// egressContains reports whether every destination a rule with egress narrow
// lets a command reach, a rule with egress broad does too, parsing both the way
// Covers does (policy.ParseEgress: host patterns allow nothing, and a malformed
// list allows no egress at all). A narrow entry covered by one broad entry
// (policy.EgressCovers) is contained. One that no broad entry covers but that
// covers some broad entry may be covered by several together, so it is Unknown.
func egressContains(broad, narrow []string) Verdict {
	nr, err := policy.ParseEgress(narrow)
	if err != nil {
		return Yes
	}
	br, err := policy.ParseEgress(broad)
	if err != nil {
		br = nil
	}
	v := Yes
	for _, r := range nr {
		switch {
		case slices.ContainsFunc(br, func(a sandbox.EgressRule) bool { return policy.EgressCovers(a, r) }):
		case slices.ContainsFunc(br, func(a sandbox.EgressRule) bool { return policy.EgressCovers(r, a) }):
			v = Unknown
		default:
			return No
		}
	}
	return v
}

// installContains reports whether each narrow install pattern is contained in
// one of the broad ones: Yes if some broad pattern contains it, else Unknown if
// one might, else No.
func installContains(broad, narrow []lastv1alpha1.ArgvPattern) Verdict {
	vs := make([]Verdict, len(narrow))
	for i, np := range narrow {
		vs[i] = No
		for _, bp := range broad {
			v := Contains(bp.Argv, np.Argv)
			if v == Yes {
				vs[i] = Yes
				break
			}
			if v == Unknown {
				vs[i] = Unknown
			}
		}
	}
	return all(vs...)
}
