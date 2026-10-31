//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"testing"

	"github.com/moogie-labs/last-operator/internal/rules"
)

func TestMatchLiteralsGlobsRegexAndRest(t *testing.T) {
	tests := []struct {
		name    string
		pattern []string
		argv    []string
		want    bool
	}{
		{"literal equal", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get", "pods"}, true},
		{"literal differs", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get", "nodes"}, false},
		{"argv shorter", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get"}, false},
		{"argv longer without rest", []string{"kubectl", "get", "pods"}, []string{"kubectl", "get", "pods", "-A"}, false},
		{"empty both", nil, nil, true},
		{"empty pattern, argv present", nil, []string{"x"}, false},
		{"star", []string{"kubectl", "get", "*"}, []string{"kubectl", "get", "pods"}, true},
		{"star never spans arguments", []string{"kubectl", "get", "*"}, []string{"kubectl", "get", "pods", "-A"}, false},
		{"star needs an argument", []string{"kubectl", "get", "*"}, []string{"kubectl", "get"}, false},
		{"star does not cross a slash", []string{"cat", "*"}, []string{"cat", "a/b"}, false},
		{"question mark", []string{"ls", "?.txt"}, []string{"ls", "a.txt"}, true},
		{"question mark is one char", []string{"ls", "?.txt"}, []string{"ls", "ab.txt"}, false},
		{"class", []string{"ls", "[ab].txt"}, []string{"ls", "b.txt"}, true},
		{"class miss", []string{"ls", "[ab].txt"}, []string{"ls", "c.txt"}, false},
		{"malformed glob matches nothing", []string{"ls", "[a"}, []string{"ls", "[a"}, false},
		{"escaped glob char is literal", []string{"ls", `\*`}, []string{"ls", "*"}, true},
		{"regex", []string{"git", "re:(status|log)"}, []string{"git", "log"}, true},
		{"regex miss", []string{"git", "re:(status|log)"}, []string{"git", "push"}, false},
		{"regex is anchored at the end", []string{"x", "re:a"}, []string{"x", "ab"}, false},
		{"regex is anchored at the start", []string{"x", "re:a"}, []string{"x", "ba"}, false},
		{"regex alternation stays anchored", []string{"x", "re:a|b"}, []string{"x", "ab"}, false},
		{"invalid regex matches nothing", []string{"x", "re:("}, []string{"x", "("}, false},
		{"rest matches zero", []string{"kubectl", "get", "..."}, []string{"kubectl", "get"}, true},
		{"rest matches many", []string{"kubectl", "get", "..."}, []string{"kubectl", "get", "pods", "-A"}, true},
		{"rest still needs the prefix", []string{"kubectl", "get", "..."}, []string{"kubectl", "delete"}, false},
		{"rest alone", []string{"..."}, []string{"a", "b"}, true},
		{"rest not last matches nothing", []string{"kubectl", "...", "pods"}, []string{"kubectl", "pods"}, false},
		{"two rests match nothing", []string{"kubectl", "...", "..."}, []string{"kubectl"}, false},
		{"rest anywhere else even for empty argv", []string{"...", "x"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rules.Match(tt.pattern, tt.argv); got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.argv, got, tt.want)
			}
		})
	}
}

func TestRefString(t *testing.T) {
	if got := (rules.Ref{Scope: "namespace", Namespace: "team-x", Name: "r"}).String(); got != "namespace/team-x/r" {
		t.Errorf("namespace ref = %q", got)
	}
	if got := (rules.Ref{Scope: "cluster", Name: "r"}).String(); got != "cluster/-/r" {
		t.Errorf("cluster ref = %q", got)
	}
}
