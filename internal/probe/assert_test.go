package probe_test

import (
	"context"
	"strings"
	"testing"
	"time"

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
			got, err := probe.Evaluate(context.Background(), c.expr, c.r)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("Evaluate(%q) = %v, %v; want %v, err=%v", c.expr, got, err, c.want, c.wantErr)
			}
		})
	}
}

func TestEvaluateEnforcesTheCostLimit(t *testing.T) {
	items := "[" + strings.Repeat("1,", 7) + "1]"
	expr := "result.json.all(a, result.json.all(b, result.json.all(c, result.json.all(d, result.json.all(e, result.json.all(f, a + b + c + d + e + f > 0))))))"
	got, err := probe.Evaluate(context.Background(), expr, probe.Result{Stdout: items})
	if err == nil || got || !strings.Contains(err.Error(), "cost limit") {
		t.Fatalf("Evaluate = %v, %v; want a cost-limit error", got, err)
	}
}

func flatList(n int) string {
	return "[" + strings.Repeat("1,", n-1) + "1]"
}

func TestEvaluateReturnsPromptlyOnAHugeJSONDocument(t *testing.T) {
	start := time.Now()
	got, err := probe.Evaluate(context.Background(), "result.json.exists(a, a == 2)", probe.Result{Stdout: flatList(100_000)})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Evaluate took %s on 100k elements", elapsed)
	}
	if got || err == nil {
		t.Fatalf("Evaluate = %v, %v; want an error because result.json is omitted above the node cap", got, err)
	}
}

func TestEvaluateHandlesDocumentsUnderTheNodeCap(t *testing.T) {
	doc := "[" + strings.Repeat("1,", 4998) + "2]"
	got, err := probe.Evaluate(context.Background(), "result.json.exists(a, a == 2)", probe.Result{Stdout: doc})
	if err != nil || !got {
		t.Fatalf("Evaluate = %v, %v; want true", got, err)
	}
}

func TestEvaluateOmitsJSONAboveTheNodeCap(t *testing.T) {
	_, err := probe.Evaluate(context.Background(), "size(result.json) > 0", probe.Result{Stdout: flatList(probe.MaxJSONNodes + 1)})
	if err == nil || !strings.Contains(err.Error(), "no such key") {
		t.Fatalf("err = %v, want no such key", err)
	}
	got, err := probe.Evaluate(context.Background(), "size(result.json) > 0", probe.Result{Stdout: flatList(probe.MaxJSONNodes - 1)})
	if err != nil || !got {
		t.Fatalf("Evaluate = %v, %v; want true just under the cap", got, err)
	}
}

func TestEvaluateRejectsADynNonBoolResult(t *testing.T) {
	got, err := probe.Evaluate(context.Background(), "result.json.spec.replicas", probe.Result{Stdout: `{"spec":{"replicas":2}}`})
	if err == nil || got {
		t.Fatalf("Evaluate = %v, %v; want an error for a non-bool result", got, err)
	}
}

func TestEvaluateHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := probe.Evaluate(ctx, "result.json.exists(a, a == 2)", probe.Result{Stdout: flatList(9000)})
	if err == nil || got {
		t.Fatalf("Evaluate = %v, %v; want a cancellation error", got, err)
	}
}

// A nested macro over a list just under the node cap stays within the cost limit
// but runs far longer than EvalTimeout, so only the interrupt can stop it.
const slowExpr = "result.json.exists(a, result.json.exists(b, a + b == -1))"

func TestEvaluateInterruptsAtEvalTimeout(t *testing.T) {
	start := time.Now()
	got, err := probe.Evaluate(context.Background(), slowExpr, probe.Result{Stdout: flatList(9000)})
	if elapsed := time.Since(start); elapsed > probe.EvalTimeout+time.Second {
		t.Fatalf("Evaluate took %v, want at most about %v", elapsed, probe.EvalTimeout)
	}
	if err == nil || got || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Evaluate = %v, %v; want a timeout error", got, err)
	}
}

func TestEvaluateStopsWhenTheCallerCancelsMidEvaluation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := probe.Evaluate(ctx, slowExpr, probe.Result{Stdout: flatList(9000)})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Evaluate took %v after the caller's 100ms deadline", elapsed)
	}
	if err == nil || got {
		t.Fatalf("Evaluate = %v, %v; want a cancellation error", got, err)
	}
}
