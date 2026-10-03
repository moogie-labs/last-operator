// Package probe evaluates probe assertions (spec §10).
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"cel.dev/cel-go/cel"
)

// Result is what an assertion sees as `result`.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

const (
	// EvalTimeout bounds the wall time of one assertion.
	EvalTimeout = 2 * time.Second
	// MaxJSONNodes caps the size of the JSON document exposed as result.json.
	MaxJSONNodes = 10000
)

var celEnv = mustEnv()

func mustEnv() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("result", cel.MapType(cel.StringType, cel.DynType)),
		cel.CrossTypeNumericComparisons(true),
	)
	if err != nil {
		panic(err)
	}
	return env
}

// Evaluate runs a CEL assertion against a probe result. `result` has exitCode (int),
// stdout and stderr (strings) and, when stdout is a JSON document, json.
//
// Expressions are untrusted, so evaluation is bounded by a cost limit, by EvalTimeout and
// by ctx. result.json is omitted (expressions using it fail with "no such key") when the
// document has more than MaxJSONNodes values (objects, arrays and scalars each count one).
// Probes that need large documents should narrow them in the command, for example with
// jq or -o jsonpath.
func Evaluate(ctx context.Context, expr string, r Result) (bool, error) {
	ast, iss := celEnv.Compile(expr)
	if iss.Err() != nil {
		return false, fmt.Errorf("probe: compile %q: %w", expr, iss.Err())
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return false, fmt.Errorf("probe: assertion %q must be a bool expression, not %s", expr, t)
	}
	prg, err := celEnv.Program(ast, cel.CostLimit(1_000_000), cel.InterruptCheckFrequency(100))
	if err != nil {
		return false, fmt.Errorf("probe: program %q: %w", expr, err)
	}
	vars := map[string]any{"exitCode": int64(r.ExitCode), "stdout": r.Stdout, "stderr": r.Stderr}
	var doc any
	if err := json.Unmarshal([]byte(r.Stdout), &doc); err == nil {
		if countNodes(doc, MaxJSONNodes) <= MaxJSONNodes {
			vars["json"] = normalize(doc)
		}
	}
	ectx, cancel := context.WithTimeout(ctx, EvalTimeout)
	defer cancel()
	out, _, err := prg.ContextEval(ectx, map[string]any{"result": vars})
	if err != nil {
		if errors.Is(ectx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return false, fmt.Errorf("probe: evaluate %q: timed out after %s", expr, EvalTimeout)
		}
		if ctx.Err() != nil {
			return false, fmt.Errorf("probe: evaluate %q: %w", expr, ctx.Err())
		}
		return false, fmt.Errorf("probe: evaluate %q: %w", expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("probe: assertion %q returned %T, not bool", expr, out.Value())
	}
	return b, nil
}

// normalize turns integral JSON numbers into int64 so `== 2` behaves as written.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
		return t
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
		return t
	default:
		return v
	}
}

// countNodes counts JSON values, stopping once the count passes limit.
func countNodes(v any, limit int) int {
	n := 1
	switch t := v.(type) {
	case map[string]any:
		for _, x := range t {
			if n > limit {
				return n
			}
			n += countNodes(x, limit-n)
		}
	case []any:
		for _, x := range t {
			if n > limit {
				return n
			}
			n += countNodes(x, limit-n)
		}
	}
	return n
}
