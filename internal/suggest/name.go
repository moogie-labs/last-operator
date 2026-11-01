package suggest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

// Name is the suggestion's object name (S6): "sugg-" and the first 10 hex
// characters of the SHA-256 of the canonical JSON of {targetScope, namespace,
// proposed}, so the same proposal from any trigger is one object.
func Name(p Proposal) (string, error) {
	b, err := json.Marshal(struct {
		TargetScope lastv1alpha1.RuleScope       `json:"targetScope"`
		Namespace   string                       `json:"namespace"`
		Proposed    lastv1alpha1.CommandRuleSpec `json:"proposed"`
	}{p.TargetScope, p.Namespace, p.Spec})
	if err != nil {
		return "", fmt.Errorf("hashing the proposal: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sugg-" + hex.EncodeToString(sum[:])[:10], nil
}
