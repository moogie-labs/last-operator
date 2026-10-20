// Package plan hashes agent plans and resolves ${steps…} references (spec §8.1–§8.3).
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

// Body is the hashed part of a plan: its steps and resources (spec §8.2), and
// a Remediate plan's nonce. An empty nonce is left out of the canonical JSON,
// so a plan without one hashes as it did before nonces existed.
type Body struct {
	Steps     []lastv1alpha1.Command             `json:"steps"`
	Resources []lastv1alpha1.ManagedResourceSpec `json:"resources,omitempty"`
	Nonce     string                             `json:"nonce,omitempty"`
}

// FromStatus returns the hashed body of an Intent's status plan.
func FromStatus(p *lastv1alpha1.Plan) Body {
	b := Body{Steps: p.Steps, Resources: p.Resources, Nonce: p.Nonce}
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

// CommandKey identifies a command as written, before ${steps…} resolution: its
// canonical JSON without the step id. Two commands with the same key run the
// same thing. The operator checks a Delete plan against the teardown with it,
// and the gateway checks Auto's limits (design D9).
func CommandKey(c lastv1alpha1.Command) (string, error) {
	c.ID = ""
	b, err := audit.Canonical(c)
	return string(b), err
}

// Validate checks what the gateway relies on: step IDs name the steps that
// ${steps…} references and RunCommand use, probe IDs name probe outcomes, each
// output Secret name is a DNS-1123 subdomain, and each declared output has a
// valid Secret key and is a single reference to a step of the plan. A plan
// that passes cannot fail at WriteOutputSecret for its shape alone.
func Validate(b Body) error {
	seen, err := validateSteps(b.Steps)
	if err != nil {
		return err
	}
	return validateResources(b.Resources, seen)
}

// ErrResourcesChanged reports a Remediate plan whose resources are not the
// executed plan's.
var ErrResourcesChanged = errors.New("plan: a Remediate plan must carry the executed plan's resources unchanged")

var noncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidNonce reports whether s is a plan nonce: 32 lowercase hex characters.
func ValidNonce(s string) bool { return noncePattern.MatchString(s) }

// ValidateRemediate is Validate for a Remediate plan b, given the executed plan.
// A remediation repairs and never redesigns (design D5): b must carry the
// executed plan's resources unchanged (ErrResourcesChanged otherwise) and a
// nonce. Its steps are checked as Validate checks them, while its resources'
// outputs must name steps of the executed plan, which wrote them.
func ValidateRemediate(b, executed Body) error {
	if _, err := validateSteps(b.Steps); err != nil {
		return err
	}
	if !ValidNonce(b.Nonce) {
		return errors.New("plan: a Remediate plan needs a nonce of 32 lowercase hex characters")
	}
	same, err := SameResources(b.Resources, executed.Resources)
	if err != nil {
		return err
	}
	if !same {
		return ErrResourcesChanged
	}
	executedSteps := map[string]bool{}
	for _, s := range executed.Steps {
		executedSteps[s.ID] = true
	}
	return validateResources(b.Resources, executedSteps)
}

// SameResources reports whether a and b are the same resources, comparing
// their canonical JSON with IntentRef cleared (the operator fills it in when it
// creates the ManagedResources). Nil and empty are the same.
func SameResources(a, b []lastv1alpha1.ManagedResourceSpec) (bool, error) {
	ca, err := canonicalResources(a)
	if err != nil {
		return false, err
	}
	cb, err := canonicalResources(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

func canonicalResources(rs []lastv1alpha1.ManagedResourceSpec) ([]byte, error) {
	cleared := make([]lastv1alpha1.ManagedResourceSpec, len(rs))
	for i, r := range rs {
		r.IntentRef = nil
		cleared[i] = r
	}
	c, err := audit.Canonical(cleared)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return c, nil
}

// validateSteps checks that there is a step and that step IDs are present and
// unique; it returns the set of IDs.
func validateSteps(steps []lastv1alpha1.Command) (map[string]bool, error) {
	if len(steps) == 0 {
		return nil, errors.New("plan: a plan needs at least one step")
	}
	seen := map[string]bool{}
	for i, s := range steps {
		if s.ID == "" {
			return nil, fmt.Errorf("plan: step %d has no id", i)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("plan: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
	}
	return seen, nil
}

// validateResources checks resources whose outputs may name the steps in seen.
func validateResources(resources []lastv1alpha1.ManagedResourceSpec, seen map[string]bool) error {
	probes := map[string]bool{}
	secrets := map[string]bool{}
	for i, r := range resources {
		for _, p := range r.Probes {
			if probes[p.ID] {
				return fmt.Errorf("plan: duplicate probe id %q", p.ID)
			}
			probes[p.ID] = true
		}
		for j, c := range r.Teardown {
			where := fmt.Sprintf("plan: resource %d teardown command %d", i, j)
			if err := CheckCommand(where, c); err != nil {
				return err
			}
			if err := checkNoStepRefs(where, c); err != nil {
				return err
			}
		}
		if r.OutputsSecretRef != nil {
			if errs := validation.IsDNS1123Subdomain(r.OutputsSecretRef.Name); len(errs) > 0 {
				return fmt.Errorf("plan: resource %d output secret %q is not a valid Secret name: %s",
					i, r.OutputsSecretRef.Name, strings.Join(errs, "; "))
			}
			if secrets[r.OutputsSecretRef.Name] {
				return fmt.Errorf("plan: duplicate output secret %q", r.OutputsSecretRef.Name)
			}
			secrets[r.OutputsSecretRef.Name] = true
		}
		if len(r.Outputs) > 0 && r.OutputsSecretRef == nil {
			return fmt.Errorf("plan: resource %d has outputs but no outputsSecretRef", i)
		}
		for _, k := range slices.Sorted(maps.Keys(r.Outputs)) {
			// Errors never repeat expr: a mistaken literal could be a credential.
			expr := r.Outputs[k]
			if !ValidSecretKey(k) {
				return fmt.Errorf("plan: resource %d output %q is not a valid Secret key", i, k)
			}
			m := refPattern.FindStringSubmatch(expr)
			if m == nil || m[0] != expr {
				return fmt.Errorf("plan: resource %d output %q is not a single step reference", i, k)
			}
			if !seen[m[1]] {
				return fmt.Errorf("plan: resource %d output %q names unknown step %q", i, k, m[1])
			}
		}
	}
	return nil
}

// CheckCommand mirrors the CRD's rule that a command sets exactly one of
// run.argv and run.script. where names the command in the error.
func CheckCommand(where string, c lastv1alpha1.Command) error {
	if (len(c.Run.Argv) > 0) == (c.Run.Script != "") {
		return fmt.Errorf("%s: set exactly one of run.argv and run.script", where)
	}
	return nil
}

// checkNoStepRefs refuses a teardown command with a ${steps…} reference: it runs
// in a Delete phase, when the Create steps' results are gone, so it could never
// resolve and the Intent could never be deleted. The error never repeats the
// command's text: a mistaken literal could be a credential.
func checkNoStepRefs(where string, c lastv1alpha1.Command) error {
	refs := strings.Contains(c.Run.Script, "${steps.")
	for _, a := range c.Run.Argv {
		refs = refs || strings.Contains(a, "${steps.")
	}
	if refs {
		return fmt.Errorf("%s uses a ${steps…} reference; a teardown runs when the Intent is deleted, "+
			"without the plan's step results, so write the value itself", where)
	}
	return nil
}

// maxSecretKeyLen is the longest Secret data key the API server accepts.
const maxSecretKeyLen = 253

var secretKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// ValidSecretKey reports whether k is a valid Secret data key. Plan validation
// and the gateway's output write share this one definition.
func ValidSecretKey(k string) bool {
	return len(k) <= maxSecretKeyLen && k != "." && k != ".." && secretKeyPattern.MatchString(k)
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
	if res.StdoutTruncated {
		return "", fmt.Errorf("%w: step %q output was truncated", ErrReference, m[1])
	}
	if m[2] == "stdout" {
		return strings.TrimRight(res.Stdout, "\n"), nil
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
