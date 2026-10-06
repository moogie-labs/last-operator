package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

// DiscoveryStepID is the step ID every discovery command carries (M3 spec §3.4).
const DiscoveryStepID = "discovery"

// MaxPendingCommandBytes bounds a discovery command's canonical JSON.
const MaxPendingCommandBytes = 64 << 10

type discoverySubject struct {
	Command lastv1alpha1.Command `json:"command"`
	Nonce   string               `json:"nonce"`
}

// DiscoveryHash returns the approval subject of a discovery command,
// "sha256:<hex>" of the RFC 8785 canonical JSON of {command, nonce}, and that
// JSON's size. A fresh nonce per request makes every request a new subject, so
// an approval is good for exactly one run.
func DiscoveryHash(c lastv1alpha1.Command, nonce string) (string, int, error) {
	b, err := audit.Canonical(discoverySubject{Command: c, Nonce: nonce})
	if err != nil {
		return "", 0, fmt.Errorf("plan: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), len(b), nil
}
