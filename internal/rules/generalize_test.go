//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"reflect"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

func TestGeneralize(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{"kubectl", []string{"kubectl", "-n", "team-x", "get", "deployment", "web", "-o", "json"},
			[]string{"kubectl", "-n", "*", "get", "deployment", "[!-]*", "-o", "*"}},
		{"aws", []string{"aws", "s3", "cp", "a", "b"}, []string{"aws", "s3", "cp", "[!-]*", "[!-]*"}},
		// R11 applied literally: argv[0] plus two more positionals are literal (three in
		// all); "web" is the value of --install so it becomes *; "./chart" is the third
		// literal; "--set"'s value becomes *. The brief's sketch reorders flags; R11 wins.
		{"helm", []string{"helm", "upgrade", "--install", "web", "./chart", "--set", "image.tag=1.2"},
			[]string{"helm", "upgrade", "--install", "*", "./chart", "--set", "*"}},
		{"flag=value", []string{"kubectl", "get", "pods", "--namespace=team-x", "-o=json"},
			[]string{"kubectl", "get", "pods", "--namespace=*", "-o=*"}},
		{"flag followed by flag has no value", []string{"git", "commit", "-a", "-m", "msg"},
			[]string{"git", "commit", "-a", "-m", "*"}},
		{"path value", []string{"kubectl", "apply", "-f", "/tmp/x"}, []string{"kubectl", "apply", "-f", "*"}},
		{"flag name with a backslash", []string{"tool", "--a\\b=v"}, []string{"tool", `--a\\b=*`}},
		{"empty positional", []string{"cp", "a", "b", ""}, []string{"cp", "a", "b", "*"}},
		{"argv0 only", []string{"ls"}, []string{"ls"}},
		{"trailing flag", []string{"ls", "-l"}, []string{"ls", "-l"}},
		{"later positionals", []string{"cp", "a", "b", "c", "d"}, []string{"cp", "a", "b", "[!-]*", "[!-]*"}},
		{"glob characters in a kept literal are escaped", []string{"echo", "a*b"}, []string{"echo", `a\*b`}},
		{"literal starting with re: is kept literal", []string{"echo", "re:x"}, []string{"echo", "[r]e:x"}},
		{"literal ... is kept literal", []string{"echo", "..."}, []string{"echo", "[.].."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: tt.argv}}
			got := rules.Generalize(cmd).Argv
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Generalize = %q, want %q", got, tt.want)
			}
			if !rules.Match(got, tt.argv) {
				t.Errorf("generalized pattern %q does not match its own argv %q", got, tt.argv)
			}
		})
	}
	// a positional slot must not swallow a flag-shaped argument
	gen := rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"kubectl", "-n", "x", "get", "deployment", "web"}}}).Argv
	if rules.Match(gen, []string{"kubectl", "-n", "x", "get", "deployment", "--kubeconfig=/work/kc"}) {
		t.Errorf("%q matches a flag-shaped positional", gen)
	}
	if rules.Match([]string{"tool", `--a\\b=*`}, []string{"tool", "--ab=v"}) {
		t.Error("escaped flag name matches --ab=v")
	}
	if got := rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "x"}}); got.Argv != nil {
		t.Errorf("a script has no argv to generalize, got %q", got.Argv)
	}
}
