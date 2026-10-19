package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// MaxPlanBytes bounds a plan's canonical JSON. It stays well under the gateway's
// bounds (512 KiB per plan, about 900 KiB per record) and the Intent object size.
const MaxPlanBytes = 256 << 10

var fence = regexp.MustCompile("(?s)^\\s*```(?:json)?[ \\t]*\\n(.*?)\\n?```\\s*$")

// decodeOne reads content as exactly one JSON object, optionally inside a
// ```json fence, refusing unknown fields. what names the object in errors.
func decodeOne(content string, v any, what string) error {
	raw := strings.TrimSpace(content)
	if m := fence.FindStringSubmatch(content); m != nil {
		raw = m[1]
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the answer is not a %s object: %w", what, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the answer has text after the %s object", what)
	}
	return nil
}

// checkPlan checks a plan for everything the gateway and the CRD schema would
// refuse, and hashes it.
func checkPlan(summary string, steps []lastv1alpha1.Command, resources []lastv1alpha1.ManagedResourceSpec) (*lastv1alpha1.Plan, error) {
	for _, s := range steps {
		if err := checkCommand("step "+s.ID, s); err != nil {
			return nil, err
		}
	}
	for i, r := range resources {
		if r.IntentRef != nil {
			return nil, fmt.Errorf("resource %d sets intentRef; the operator sets it", i)
		}
		for j, p := range r.Probes {
			if p.ID == "" {
				return nil, fmt.Errorf("probe %d of resource %d has no id", j, i)
			}
			if p.Assert == "" {
				return nil, fmt.Errorf("probe %q has no assert", p.ID)
			}
			if err := checkCommand("probe "+p.ID, p.Command); err != nil {
				return nil, err
			}
		}
	}
	body := plan.Body{Steps: steps, Resources: resources}
	if err := plan.Validate(body); err != nil {
		return nil, err
	}
	canonical, err := plan.Canonical(body)
	if err != nil {
		return nil, err
	}
	if len(canonical) > MaxPlanBytes {
		return nil, fmt.Errorf("the plan is %d bytes; the limit is %d", len(canonical), MaxPlanBytes)
	}
	hash, err := plan.Hash(body)
	if err != nil {
		return nil, err
	}
	return &lastv1alpha1.Plan{Hash: hash, Summary: summary, Steps: steps, Resources: resources}, nil
}

// checkCommand mirrors the CRD's rule for commands.
func checkCommand(where string, c lastv1alpha1.Command) error {
	return plan.CheckCommand(where, c)
}
