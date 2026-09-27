package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gowebpki/jcs"
)

// Canonical returns the RFC 8785 canonical JSON encoding of v.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal: %w", err)
	}
	out, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize: %w", err)
	}
	return out, nil
}

// ComputeHash returns "sha256:<hex>" over the canonical entry with hash and sig removed.
func ComputeHash(e Entry) (string, error) {
	e.Hash, e.Sig = "", ""
	b, err := Canonical(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
