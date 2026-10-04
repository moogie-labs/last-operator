// Package plan hashes agent plans and resolves ${steps…} references (spec §8.1–§8.3).
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Body is the hashed part of a plan: its steps and resources (spec §8.2).
type Body struct {
	Steps     []lastv1alpha1.Command             `json:"steps"`
	Resources []lastv1alpha1.ManagedResourceSpec `json:"resources,omitempty"`
}

// FromStatus returns the hashed body of an Intent's status plan.
func FromStatus(p *lastv1alpha1.Plan) Body {
	b := Body{Steps: p.Steps, Resources: p.Resources}
	if b.Steps == nil {
		b.Steps = []lastv1alpha1.Command{}
	}
	return b
}

// Canonical returns the RFC 8785 canonical JSON of the body.
func Canonical(b Body) ([]byte, error) {
	if b.Steps == nil {
		b.Steps = []lastv1alpha1.Command{}
	}
	return audit.Canonical(b)
}

// Hash returns "sha256:<hex>" of the canonical body; it is the plan's approval subject.
func Hash(b Body) (string, error) {
	c, err := Canonical(b)
	if err != nil {
		return "", fmt.Errorf("plan: %w", err)
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Validate checks what the gateway relies on: step IDs name the steps that
// ${steps…} references and RunCommand use, and probe IDs name probe outcomes.
func Validate(b Body) error {
	if len(b.Steps) == 0 {
		return errors.New("plan: a plan needs at least one step")
	}
	seen := map[string]bool{}
	for i, s := range b.Steps {
		if s.ID == "" {
			return fmt.Errorf("plan: step %d has no id", i)
		}
		if seen[s.ID] {
			return fmt.Errorf("plan: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
	}
	probes := map[string]bool{}
	for _, r := range b.Resources {
		for _, p := range r.Probes {
			if probes[p.ID] {
				return fmt.Errorf("plan: duplicate probe id %q", p.ID)
			}
			probes[p.ID] = true
		}
	}
	return nil
}

// ErrReference reports a ${steps…} reference that cannot be resolved.
var ErrReference = errors.New("plan: bad step reference")

var refPattern = regexp.MustCompile(`\$\{steps\.([A-Za-z0-9_-]+)\.(stdout|json((?:\.[A-Za-z0-9_-]+)+))\}`)

// Results holds the results of earlier steps of a phase, by step ID.
type Results map[string]runnerapi.Result

// Value returns the value of a single reference expression, either
// "${steps.<id>.stdout}" (trailing newlines removed) or "${steps.<id>.json.<path>}"
// (path segments are object keys or array indexes; the value must be a string,
// number, or boolean).
func (r Results) Value(expr string) (string, error) {
	m := refPattern.FindStringSubmatch(expr)
	if m == nil || m[0] != expr {
		return "", fmt.Errorf("%w: %q is not a single step reference", ErrReference, expr)
	}
	res, ok := r[m[1]]
	if !ok {
		return "", fmt.Errorf("%w: step %q has no result", ErrReference, m[1])
	}
	if res.ExitCode != 0 || res.Error != "" {
		return "", fmt.Errorf("%w: step %q did not succeed", ErrReference, m[1])
	}
	if m[2] == "stdout" {
		return strings.TrimRight(res.Stdout, "\n"), nil
	}
	if res.StdoutTruncated {
		return "", fmt.Errorf("%w: step %q output was truncated", ErrReference, m[1])
	}
	dec := json.NewDecoder(strings.NewReader(res.Stdout))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("%w: step %q output is not JSON", ErrReference, m[1])
	}
	for key := range strings.SplitSeq(strings.TrimPrefix(m[3], "."), ".") {
		switch node := v.(type) {
		case map[string]any:
			child, ok := node[key]
			if !ok {
				return "", fmt.Errorf("%w: %s: no key %q", ErrReference, expr, key)
			}
			v = child
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(node) {
				return "", fmt.Errorf("%w: %s: no index %q", ErrReference, expr, key)
			}
			v = node[i]
		default:
			return "", fmt.Errorf("%w: %s: %q is not inside an object or array", ErrReference, expr, key)
		}
	}
	switch val := v.(type) {
	case string:
		return val, nil
	case json.Number:
		return val.String(), nil
	case bool:
		return strconv.FormatBool(val), nil
	default:
		return "", fmt.Errorf("%w: %s is not a string, number, or boolean", ErrReference, expr)
	}
}

// Resolve returns a copy of c with every ${steps…} reference in its argv
// replaced. Files, scripts, and other fields are never resolved (spec §8.1).
func Resolve(c lastv1alpha1.Command, r Results) (lastv1alpha1.Command, error) {
	if len(c.Run.Argv) == 0 {
		return c, nil
	}
	argv := make([]string, len(c.Run.Argv))
	for i, arg := range c.Run.Argv {
		if strings.Contains(refPattern.ReplaceAllString(arg, ""), "${steps.") {
			return lastv1alpha1.Command{}, fmt.Errorf("%w: malformed reference in %q", ErrReference, arg)
		}
		var firstErr error
		argv[i] = refPattern.ReplaceAllStringFunc(arg, func(expr string) string {
			v, err := r.Value(expr)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			return v
		})
		if firstErr != nil {
			return lastv1alpha1.Command{}, firstErr
		}
	}
	out := c
	out.Run.Argv = argv
	return out, nil
}
