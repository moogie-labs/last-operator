//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"reflect"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

func TestLintWarnings(t *testing.T) {
	const fileNote = "rules match argv only, not files or stdin"
	tests := []struct {
		name  string
		match []string
		want  []string // substrings, one per expected warning, in order
	}{
		{"clean", []string{"kubectl", "get", "pods"}, nil},
		{"clean with a safe positional value", []string{"kubectl", "get", "[!-]*"}, nil},
		{"star as a flag value is fine", []string{"kubectl", "-n", "*", "get", "pods"}, nil},
		{"trailing rest warns about flags", []string{"kubectl", "get", "..."}, []string{"position 2: `...` also accepts flags such as --kubeconfig"}},
		{"bare star positional warns about flags", []string{"kubectl", "get", "*"}, []string{"position 2: `*` also matches flags such as --kubeconfig; use `[!-]*` for a value"}},
		{"rest at position 1", []string{"kubectl", "..."}, []string{"'...' at position 1"}},
		{"glob after delete", []string{"kubectl", "delete", "*"}, []string{"after destructive verb \"delete\"", "position 2"}},
		{"regex after rm", []string{"aws", "s3", "rm", "re:.*"}, []string{"after destructive verb \"rm\""}},
		{"glob after terminate-*", []string{"aws", "ec2", "terminate-instances", "i-*"}, []string{"terminate-instances"}},
		{"glob after destroy", []string{"terraform", "destroy", "-target=*"}, []string{"destroy"}},
		{"glob after drop", []string{"psql", "drop", "t?"}, []string{"drop"}},
		{"glob after purge", []string{"x", "purge", "[ab]"}, []string{"purge"}},
		{"rm as argv[0]", []string{"rm", "-rf", "re:.*"}, []string{"destructive verb \"rm\""}},
		{"rest after a destructive verb", []string{"kubectl", "delete", "..."}, []string{"destructive verb \"delete\"", "position 2"}},
		{"invalid regex", []string{"git", "re:("}, []string{"invalid regex"}},
		{"regex that only fails once wrapped", []string{"git", `re:\Qa)|(b`}, []string{"invalid regex"}},
		{"malformed glob class", []string{"ls", "[a"}, []string{"malformed glob"}},
		{"rest not last", []string{"kubectl", "...", "pods"}, []string{"'...' is only valid", "'...' at position 1"}},
		{"non-literal argv[0]", []string{"k*", "get", "pods"}, []string{"argv[0] must be a literal"}},
		{"verb itself is not flagged", []string{"kubectl", "delete", "pod", "web"}, nil},
		{"glob before the verb is only the positional warning", []string{"kubectl", "*", "delete"}, []string{"position 1"}},
		{"file flag alone", []string{"kubectl", "apply", "-f", "deploy.yaml"}, []string{fileNote}},
		{"file flag with rest", []string{"kubectl", "apply", "-f", "deploy.yaml", "..."}, []string{"position 4", "file flag", fileNote}},
		{"file flag with glob", []string{"kubectl", "apply", "--filename", "*.yaml"}, []string{"file flag", fileNote}},
		{"from-file=value with glob", []string{"kubectl", "create", "secret", "--from-file=*"}, []string{"file flag", fileNote}},
		// C1: code never varies.
		{"interpreter code value slot", []string{"bash", "-c", "[!-]*"}, []string{`position 0: "bash" runs code, and position 2 is not a literal`}},
		{"interpreter code star", []string{"sh", "-c", "*"}, []string{`"sh" runs code, and position 2`}},
		{"interpreter script regex", []string{"python3", "re:.*"}, []string{`"python3" runs code, and position 1`}},
		{"interpreter by path", []string{"/bin/bash", "x.sh", "[!-]*"}, []string{`"/bin/bash" runs code, and position 2`}},
		{"interpreter code literal is clean", []string{"bash", "-c", "echo hi"}, nil},
		{"interpreter after --", []string{"tool", "x", "web", "--", "sh", "-c", "[!-]*"},
			[]string{`position 4: "sh" runs code, and position 6`, `position 6 is not a literal after "--" at position 3`}},
		{"value slot after --", []string{"tool", "x", "web", "--", "[!-]*"}, []string{`position 4 is not a literal after "--"`}},
		{"rest after --", []string{"tool", "x", "web", "--", "..."},
			[]string{"position 4: `...` also accepts flags", `position 4 is not a literal after "--"`}},
		{"non-literal before -- is fine", []string{"tool", "x", "[!-]*", "--", "date"}, nil},
		{"escaped literal after -- is clean", []string{"kubectl", "exec", "web", "--", "ls", `\*`, "[r]e:x"}, nil},
		{"code flag after a wrapper", []string{"env", "[!-]*", "-c", "[!-]*"}, []string{`position 0: "env" runs code, and position 1 is not a literal`}},
		{"escaped literal after a destructive verb is not a wildcard", []string{"kubectl", "exec", "web", "--", "rm", `a\*`}, nil},
		{"interpreter hidden in a class", []string{"tool", "x", "web", "--", "[b]ash", "-c", "[!-]*"}, []string{`"[b]ash" runs code, and position 6`, `position 6 is not a literal after "--"`}},
		{"container exec with --", []string{"kubectl", "exec", "web", "--", "[!-]*"}, []string{`position 1: "exec" runs code, and position 4`, `position 4 is not a literal after "--" at position 3`}},
		{"container exec target varies", []string{"kubectl", "exec", "[!-]*", "--", "date"}, []string{`position 1: "exec" runs code, and position 2`}},
		{"container exec after a global flag", []string{"docker", "-H", "h", "exec", "ctr", "[!-]*"}, []string{`position 3: "exec" runs code, and position 5`}},
		{"container exec without --", []string{"kubectl", "exec", "web", "[b]ash", "-c", "[!-]*"}, []string{`"exec" runs code, and position 5`}},
		{"remote shell", []string{"ssh", "host", "echo", "[!-]*"}, []string{`position 0: "ssh" runs code, and position 3`}},
		{"watch", []string{"watch", "-n", "5", "kubectl", "get", "[!-]*"}, []string{`"watch" runs code, and position 5`}},
		{"code flag value", []string{"su", "root", "-c", "[!-]*"}, []string{`position 0: "su" runs code, and position 3`}},
		{"code flag value outside a runner", []string{"tool", "root", "-c", "[!-]*"}, []string{`position 3: the value of code flag "-c" is not a literal`}},
		{"code flag=value", []string{"tool", "--eval=*"}, []string{`position 1: the value of code flag "--eval" is not a literal`}},
		{"a flag after a code flag is not its value", []string{"tool", "-c", "--x=*"}, nil},
		// I3: a path under /work/ is a file the command's files write.
		{"path under /work/", []string{"python3", "/work/x.py"}, []string{`position 1 names "/work/x.py" under /work/`}},
		{"glob under /work/", []string{"kubectl", "apply", "-f", "/work/*.yaml"},
			[]string{"file flag", fileNote, `position 3 names "/work/*.yaml" under /work/`}},
		{"flag=value under /work/", []string{"tool", "--config=/work/c.json"}, []string{`position 1 names "--config=/work/c.json" under /work/`}},
		{"regex under /work/", []string{"tool", "re:/work/.*"}, []string{`position 1 names "re:/work/.*" under /work/`}},
		{"/work itself and lookalikes are not under /work/", []string{"tool", "/work", "/workspace/x", "/tmp/work/x"}, nil},
		{"code flag literal is clean", []string{"su", "root", "-c", "id"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rules.Lint(lastv1alpha1.CommandRuleSpec{Match: lastv1alpha1.ArgvPattern{Argv: tt.match}})
			if len(got) != len(tt.want) {
				t.Fatalf("Lint = %q, want %d warning(s) %q", got, len(tt.want), tt.want)
			}
			for i, sub := range tt.want {
				if !strings.Contains(got[i], sub) {
					t.Errorf("warning %d = %q, want it to contain %q", i, got[i], sub)
				}
			}
		})
	}
}

func TestLintOrderAndInstall(t *testing.T) {
	spec := lastv1alpha1.CommandRuleSpec{
		Match: lastv1alpha1.ArgvPattern{Argv: []string{"kubectl", "delete", "*", "-f", "..."}},
		Install: []lastv1alpha1.ArgvPattern{
			{Argv: []string{"pip", "install", "boto3"}},
			{Argv: []string{"curl", "..."}},
		},
	}
	got := rules.Lint(spec)
	if len(got) != 6 {
		t.Fatalf("Lint = %q, want 6 warnings", got)
	}
	for i, prefix := range []string{"match: ", "match: ", "match: ", "match: ", "match: ", "install[1]: "} {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("warning %d = %q, want prefix %q", i, got[i], prefix)
		}
	}
	if !strings.Contains(got[0], "destructive") || !strings.Contains(got[1], "position 2") || !strings.Contains(got[2], "position 4") ||
		!strings.Contains(got[3], "file flag") || !strings.Contains(got[4], "argv only") {
		t.Errorf("warnings out of order: %q", got)
	}
	if again := rules.Lint(spec); !reflect.DeepEqual(got, again) {
		t.Errorf("Lint is not stable: %q vs %q", got, again)
	}
}

func TestLintFindingsSeparateMatchesNothingFromBroad(t *testing.T) {
	tests := []struct {
		name    string
		match   []string
		nothing bool
	}{
		{"broad only", []string{"kubectl", "get", "..."}, false},
		{"clean", []string{"kubectl", "get", "pods"}, false},
		{"glob argv0", []string{"kube*", "get"}, true},
		{"misplaced rest", []string{"kubectl", "...", "get"}, true},
		{"invalid regex", []string{"kubectl", "get", "re:("}, true},
		{"malformed glob", []string{"kubectl", "get", "[a"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := lastv1alpha1.CommandRuleSpec{Match: lastv1alpha1.ArgvPattern{Argv: tc.match}}
			fs := rules.LintFindings(spec)
			if got := rules.MatchesNothing(fs); got != tc.nothing {
				t.Errorf("MatchesNothing = %v, want %v: %+v", got, tc.nothing, fs)
			}
			if !reflect.DeepEqual(len(fs), len(rules.Lint(spec))) {
				t.Errorf("LintFindings and Lint disagree: %+v vs %v", fs, rules.Lint(spec))
			}
		})
	}
}
