package probe_test

import (
	"strings"
	"testing"

	"github.com/moogie-labs/last-operator/internal/probe"
)

func TestEvaluate(t *testing.T) {
	deploy := `{"spec":{"replicas":2},"status":{"readyReplicas":2},"ratio":0.75,"items":[1,2]}`
	cases := []struct {
		name, expr string
		r          probe.Result
		want       bool
		wantErr    bool
	}{
		{"exit ok", "result.exitCode == 0", probe.Result{}, true, false},
		{"exit failed", "result.exitCode == 0", probe.Result{ExitCode: 1}, false, false},
		{"json ints", "result.json.spec.replicas == 2 && result.json.status.readyReplicas == 2", probe.Result{Stdout: deploy}, true, false},
		{"json mismatch", "result.json.spec.replicas == 3", probe.Result{Stdout: deploy}, false, false},
		{"json float", "result.json.ratio > 0.5", probe.Result{Stdout: deploy}, true, false},
		{"json list", "size(result.json.items) == 2", probe.Result{Stdout: deploy}, true, false},
		{"stdout", `result.stdout.contains("ready")`, probe.Result{Stdout: "pod ready\n"}, true, false},
		{"stderr", `result.stderr == ""`, probe.Result{Stderr: "warn"}, false, false},
		{"no json", "result.json.x == 1", probe.Result{Stdout: "not json"}, false, true},
		{"not bool", "result.exitCode + 1", probe.Result{}, false, true},
		{"syntax", "result.exitCode ==", probe.Result{}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := probe.Evaluate(c.expr, c.r)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("Evaluate(%q) = %v, %v; want %v, err=%v", c.expr, got, err, c.want, c.wantErr)
			}
		})
	}
}

func TestEvaluateEnforcesTheCostLimit(t *testing.T) {
	items := "[" + strings.Repeat("1,", 99) + "1]"
	expr := "result.json.all(a, result.json.all(b, result.json.all(c, result.json.all(d, a + b + c + d > 0))))"
	got, err := probe.Evaluate(expr, probe.Result{Stdout: items})
	if err == nil || got {
		t.Fatalf("Evaluate = %v, %v; want a cost-limit error", got, err)
	}
}
