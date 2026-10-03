// Package probe evaluates probe assertions (spec §10).
package probe

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/cel-go/cel"
)

// Result is what an assertion sees as `result`.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

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
func Evaluate(expr string, r Result) (bool, error) {
	ast, iss := celEnv.Compile(expr)
	if iss.Err() != nil {
		return false, fmt.Errorf("probe: compile %q: %w", expr, iss.Err())
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return false, fmt.Errorf("probe: assertion %q must be a bool expression, not %s", expr, t)
	}
	prg, err := celEnv.Program(ast, cel.CostLimit(1_000_000))
	if err != nil {
		return false, fmt.Errorf("probe: program %q: %w", expr, err)
	}
	vars := map[string]any{"exitCode": int64(r.ExitCode), "stdout": r.Stdout, "stderr": r.Stderr}
	var doc any
	if err := json.Unmarshal([]byte(r.Stdout), &doc); err == nil {
		vars["json"] = normalize(doc)
	}
	out, _, err := prg.Eval(map[string]any{"result": vars})
	if err != nil {
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
