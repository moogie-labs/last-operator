//nolint:goconst // table-driven test data repeats argv words
package rules_test

import (
	"slices"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/rules"
)

func groupCmd(providers []string, argv ...string) lastv1alpha1.Command {
	return lastv1alpha1.Command{Providers: providers, Run: lastv1alpha1.CommandRun{Argv: argv}}
}

func TestGroupKey(t *testing.T) {
	create, update, del := lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDelete
	k8s := []string{"kubernetes"}
	key := func(stage lastv1alpha1.Stage, cmd lastv1alpha1.Command) string {
		t.Helper()
		k, ok := rules.GroupKey(stage, cmd)
		if !ok {
			t.Fatalf("GroupKey(%s, %q) = false, want a key", stage, cmd.Run.Argv)
		}
		return k
	}

	web := key(create, groupCmd(k8s, "kubectl", "get", "pods", "web", "-n", "a"))
	if api := key(create, groupCmd(k8s, "kubectl", "-n", "b", "get", "pods", "api")); api != web {
		t.Errorf("flags and later positionals changed the key: %q vs %q", web, api)
	}
	if again := key(create, groupCmd(k8s, "kubectl", "get", "pods", "web", "-n", "a")); again != web {
		t.Errorf("GroupKey is not deterministic: %q vs %q", web, again)
	}
	if up := key(update, groupCmd(k8s, "kubectl", "get", "pods", "x")); up != web {
		t.Errorf("Create and Update keys differ: %q vs %q", web, up)
	}
	if d := key(del, groupCmd(k8s, "kubectl", "get", "pods", "x")); d == web {
		t.Errorf("Delete shares Create's key %q", d)
	}
	if other := key(create, groupCmd(k8s, "kubectl", "get", "nodes", "x")); other == web {
		t.Errorf("a different third literal shares the key %q", other)
	}

	ab := key(create, groupCmd([]string{"b", "a"}, "kubectl", "get", "pods"))
	if ba := key(create, groupCmd([]string{"a", "b"}, "kubectl", "get", "pods")); ba != ab {
		t.Errorf("provider order changed the key: %q vs %q", ab, ba)
	}
	if a := key(create, groupCmd([]string{"a"}, "kubectl", "get", "pods")); a == ab {
		t.Errorf("different providers share the key %q", a)
	}

	ssh := key(create, groupCmd(nil, "ssh", "host", "uptime"))
	if again := key(create, groupCmd(nil, "ssh", "host", "uptime")); again != ssh {
		t.Errorf("an exact repeat changed the key: %q vs %q", ssh, again)
	}
	if other := key(create, groupCmd(nil, "ssh", "host", "uptime", "-p")); other == ssh {
		t.Errorf("a code argv is not keyed by its whole argv: %q", other)
	}
	exec := key(create, groupCmd(k8s, "kubectl", "exec", "web", "--", "date"))
	if other := key(create, groupCmd(k8s, "kubectl", "exec", "web", "--", "id")); other == exec {
		t.Errorf("a container exec is not keyed by its whole argv: %q", other)
	}

	if _, ok := rules.GroupKey(create, lastv1alpha1.Command{Run: lastv1alpha1.CommandRun{Script: "echo hi"}}); ok {
		t.Error("a script has a key")
	}
	if _, ok := rules.GroupKey(create, groupCmd(k8s, "kubectl", "get", "pods", "${steps.a.outputs.name}")); ok {
		t.Error("a ${steps. argv has a key")
	}
	if _, ok := rules.GroupKey(create, groupCmd(k8s)); ok {
		t.Error("an empty argv has a key")
	}
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name  string
		argvs [][]string
		want  []string // nil when no merge
	}{
		{"one differing value", [][]string{{"kubectl", "get", "pods", "web"}, {"kubectl", "get", "pods", "api"}},
			[]string{"kubectl", "get", "pods", "[!-]*"}},
		{"flag values differ", [][]string{{"kubectl", "-n", "a", "get", "pods"}, {"kubectl", "-n", "b", "get", "pods"}, {"kubectl", "-n", "a", "get", "pods"}},
			[]string{"kubectl", "-n", "[!-]*", "get", "pods"}},
		{"equal values stay escaped literals", [][]string{{"tool", "a*", "x"}, {"tool", "a*", "y"}}, []string{"tool", `a\*`, "[!-]*"}},
		{"one argv is its exact pattern", [][]string{{"tool", "re:x", "..."}}, []string{"tool", "[r]e:x", "[.].."}},
		{"no argvs", nil, nil},
		{"different lengths", [][]string{{"kubectl", "get", "pods"}, {"kubectl", "get", "pods", "web"}}, nil},
		{"a differing value starts with a dash", [][]string{{"kubectl", "get", "pods", "web"}, {"kubectl", "get", "pods", "-A"}}, nil},
		{"a differing value is empty", [][]string{{"tool", "x", ""}, {"tool", "x", "y"}}, nil},
		{"argv[0] differs", [][]string{{"kubectl", "get"}, {"oc", "get"}}, nil},
		{"difference after a StartsCode position", [][]string{{"kubectl", "exec", "web", "--", "date"}, {"kubectl", "exec", "web", "--", "id"}}, nil},
		{"difference at a StartsCode position", [][]string{{"tool", "x", "bash"}, {"tool", "x", "y"}}, nil},
		{"difference after a runner", [][]string{{"ssh", "a", "uptime"}, {"ssh", "b", "uptime"}}, nil},
		{"difference before a StartsCode position", [][]string{{"tool", "a", "--", "date"}, {"tool", "b", "--", "date"}},
			[]string{"tool", "[!-]*", "--", "date"}},
		{"a code flag's value differs", [][]string{{"tool", "-c", "x"}, {"tool", "-c", "y"}}, nil},
		{"a destructive verb differs", [][]string{{"kubectl", "delete", "pods", "web"}, {"kubectl", "get", "pods", "api"}}, nil},
		{"a destructive verb differs late", [][]string{{"gcloud", "compute", "instances", "delete", "a"},
			{"gcloud", "compute", "instances", "describe", "b"}}, nil},
		{"a terminate- verb differs", [][]string{{"aws", "ec2", "terminate-instances", "a"}, {"aws", "ec2", "describe-instances", "a"}}, nil},
		{"an equal destructive verb stays literal", [][]string{{"kubectl", "delete", "pods", "web"}, {"kubectl", "delete", "pods", "api"}},
			[]string{"kubectl", "delete", "pods", "[!-]*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rules.Merge(tt.argvs)
			if ok != (tt.want != nil) {
				t.Fatalf("Merge = %q, %v; want %q", got, ok, tt.want)
			}
			if !ok {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Merge = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Merge = %q, want %q", got, tt.want)
				}
			}
			for _, a := range tt.argvs {
				if !rules.Match(got, a) {
					t.Errorf("merged %q does not match example %q", got, a)
				}
			}
		})
	}
}

func TestMergePatterns(t *testing.T) {
	tests := []struct {
		name     string
		patterns [][]string
		want     []string // nil when no merge
	}{
		{"one differing literal", [][]string{{"kubectl", "get", "pods", "web"}, {"kubectl", "get", "pods", "api"}},
			[]string{"kubectl", "get", "pods", "[!-]*"}},
		{"equal globs stay as they are", [][]string{{"aws", "s3", "ls", "[!-]*", "a"}, {"aws", "s3", "ls", "[!-]*", "b"}},
			[]string{"aws", "s3", "ls", "[!-]*", "[!-]*"}},
		{"a trailing ... stays", [][]string{{"aws", "s3", "a", "..."}, {"aws", "s3", "b", "..."}}, []string{"aws", "s3", "[!-]*", "..."}},
		{"a differing glob that cannot start with a dash", [][]string{{"aws", "s3", "a*"}, {"aws", "s3", "b"}}, []string{"aws", "s3", "[!-]*"}},
		{"escaped literals stay escaped", [][]string{{"tool", `a\*`, "x"}, {"tool", `a\*`, "y"}}, []string{"tool", `a\*`, "[!-]*"}},
		{"a differing glob that may start with a dash", [][]string{{"aws", "s3", "*"}, {"aws", "s3", "b"}}, nil},
		{"a differing literal flag", [][]string{{"kubectl", "get", "-A"}, {"kubectl", "get", "web"}}, nil},
		{"a differing regex", [][]string{{"tool", "re:a.*"}, {"tool", "b"}}, nil},
		{"... against a value", [][]string{{"aws", "s3", "..."}, {"aws", "s3", "b"}}, nil},
		{"different lengths", [][]string{{"aws", "s3"}, {"aws", "s3", "ls"}}, nil},
		{"argv[0] differs", [][]string{{"kubectl", "get"}, {"oc", "get"}}, nil},
		{"difference after a runner", [][]string{{"ssh", "a", "uptime"}, {"ssh", "b", "uptime"}}, nil},
		{"difference after an exec", [][]string{{"kubectl", "exec", "web", "--", "date"}, {"kubectl", "exec", "web", "--", "id"}}, nil},
		{"a code flag's value differs", [][]string{{"tool", "-c", "x"}, {"tool", "-c", "y"}}, nil},
		{"no patterns", nil, nil},
		{"a destructive verb differs", [][]string{{"kubectl", "delete", "pods", "web"}, {"kubectl", "get", "pods", "api"}}, nil},
		{"a destructive verb differs late", [][]string{{"gcloud", "compute", "instances", "delete", "a"},
			{"gcloud", "compute", "instances", "describe", "b"}}, nil},
		{"an escaped destructive verb differs", [][]string{{"tool", "[r]m", "x"}, {"tool", "ls", "x"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rules.MergePatterns(tt.patterns)
			if ok != (tt.want != nil) || ok && !slices.Equal(got, tt.want) {
				t.Fatalf("MergePatterns = %q, %v; want %q", got, ok, tt.want)
			}
			for _, p := range tt.patterns {
				if ok && rules.Contains(got, p) != rules.Yes {
					t.Errorf("merged %q does not contain %q", got, p)
				}
			}
		})
	}
}
