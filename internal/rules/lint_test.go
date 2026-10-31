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
		{"malformed glob class", []string{"ls", "[a"}, []string{"malformed glob"}},
		{"rest not last", []string{"kubectl", "...", "pods"}, []string{"'...' is only valid", "'...' at position 1"}},
		{"non-literal argv[0]", []string{"k*", "get", "pods"}, []string{"argv[0] must be a literal"}},
		{"verb itself is not flagged", []string{"kubectl", "delete", "pod", "web"}, nil},
		{"glob before the verb is only the positional warning", []string{"kubectl", "*", "delete"}, []string{"position 1"}},
		{"file flag alone", []string{"kubectl", "apply", "-f", "deploy.yaml"}, []string{fileNote}},
		{"file flag with rest", []string{"kubectl", "apply", "-f", "deploy.yaml", "..."}, []string{"position 4", "file flag", fileNote}},
		{"file flag with glob", []string{"kubectl", "apply", "--filename", "*.yaml"}, []string{"file flag", fileNote}},
		{"from-file=value with glob", []string{"kubectl", "create", "secret", "--from-file=*"}, []string{"file flag", fileNote}},
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
