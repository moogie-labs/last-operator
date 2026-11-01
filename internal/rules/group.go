package rules

import (
	"encoding/json"
	"slices"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// groupKey is what GroupKey encodes. Exact marks a key made from a whole argv.
type groupKey struct {
	Stage     lastv1alpha1.Stage `json:"stage"`
	Providers []string           `json:"providers"`
	Exact     bool               `json:"exact"`
	Argv      []string           `json:"argv"`
}

// GroupKey returns the key RepeatedApproval groups cmd by at stage (M7 design
// §4.2): the stage class (Create and Update as one), the sorted provider set,
// and argv[0] with the next positional arguments, up to maxLiterals in all,
// skipping flags and their separate values: the literals Generalize keeps. An
// argv with a StartsCode position is keyed by its whole argv, so only exact
// repeats group. The key is a deterministic JSON text. It reports false for a
// script, an empty argv and an argv with a ${steps. reference.
func GroupKey(stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) (string, bool) {
	argv := cmd.Run.Argv
	if cmd.Run.Script != "" || len(argv) == 0 || hasStepRef(argv) {
		return "", false
	}
	if stage == lastv1alpha1.StageUpdate {
		stage = lastv1alpha1.StageCreate
	}
	k := groupKey{Stage: stage, Providers: slices.Compact(slices.Sorted(slices.Values(cmd.Providers)))}
	if k.Exact = startsCodeAt(argv) >= 0; k.Exact {
		k.Argv = argv
	} else {
		k.Argv = leadingPositionals(argv)
	}
	b, err := json.Marshal(k)
	if err != nil {
		return "", false // unreachable: strings only
	}
	return string(b), true
}

// startsCodeAt returns the first position of argv where StartsCode holds, or -1.
func startsCodeAt(argv []string) int {
	for i := range argv {
		if StartsCode(argv, i) {
			return i
		}
	}
	return -1
}

// leadingPositionals returns argv[0] and the positional arguments after it, up
// to maxLiterals in all, as Generalize counts them: a flag ("-" prefix) and the
// separate value of a flag without "=" are skipped.
func leadingPositionals(argv []string) []string {
	out := []string{argv[0]}
	wantValue := false
	for _, a := range argv[1:] {
		if len(out) == maxLiterals {
			break
		}
		switch {
		case strings.HasPrefix(a, "-"):
			_, _, hasValue := strings.Cut(a, "=")
			wantValue = !hasValue
		case wantValue:
			wantValue = false
		default:
			out = append(out, a)
		}
	}
	return out
}

// Merge is RepeatedApproval's deterministic merge (M7 design §4.2): argvs of the
// same length, with the same argv[0], become one pattern where a position equal
// in every argv stays an escaped literal and a differing position becomes
// "[!-]*". It reports false when there are no argvs, when lengths or argv[0]
// differ, when a differing position holds a value that starts with "-" or is
// empty ("[!-]*" matches neither), or when a differing position is at or after
// a StartsCode position of any argv. It also reports false when the result lets
// code vary (lintCode), such as a code flag's value. The result matches every
// argv.
func Merge(argvs [][]string) ([]string, bool) {
	if len(argvs) == 0 || len(argvs[0]) == 0 {
		return nil, false
	}
	n := len(argvs[0])
	codeFrom := n
	for _, a := range argvs {
		if len(a) != n {
			return nil, false
		}
		if i := startsCodeAt(a); i >= 0 {
			codeFrom = min(codeFrom, i)
		}
	}
	out := make([]string, n)
	for i := range n {
		first := argvs[0][i]
		if !slices.ContainsFunc(argvs, func(a []string) bool { return a[i] != first }) {
			out[i] = literal(first)
			continue
		}
		if i == 0 || i >= codeFrom || slices.ContainsFunc(argvs, func(a []string) bool {
			return a[i] == "" || strings.HasPrefix(a[i], "-")
		}) {
			return nil, false
		}
		out[i] = positionalValue
	}
	if len(lintCode("", out)) > 0 {
		return nil, false
	}
	return out, true
}
