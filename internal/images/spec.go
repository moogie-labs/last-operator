package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
)

const (
	// DefaultBuildTimeout bounds one image build when OperatorConfig sets none.
	DefaultBuildTimeout = 15 * time.Minute
	// MaxBuildTimeout is the largest buildTimeout OperatorConfig accepts.
	MaxBuildTimeout = 30 * time.Minute
	// DefaultMaxConcurrentBuilds caps builds running at once when OperatorConfig sets none.
	DefaultMaxConcurrentBuilds = 2
)

var digestRE = regexp.MustCompile(`@(sha256:[0-9a-f]{64})$`)

// BaseDigest returns the "sha256:<64 hex>" digest of a reference pinned by digest, or an error.
func BaseDigest(ref string) (string, error) {
	m := digestRE.FindStringSubmatch(ref)
	if m == nil {
		return "", fmt.Errorf("image reference %q is not pinned by a sha256 digest", ref)
	}
	return m[1], nil
}

// SpecHash returns "sha256:<hex>" of the canonical {baseDigest, install}.
func SpecHash(baseDigest string, install []lastv1alpha1.ArgvCommand) (string, error) {
	cmds := make([]map[string]any, len(install))
	for i, c := range install {
		argv := c.Argv
		if argv == nil {
			argv = []string{}
		}
		cmds[i] = map[string]any{"argv": argv}
	}
	b, err := audit.Canonical(map[string]any{"baseDigest": baseDigest, "install": cmds})
	if err != nil {
		return "", fmt.Errorf("canonicalising image spec: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Tag is the push tag for a spec hash: "sha256-<hex>".
func Tag(specHash string) string {
	return strings.Replace(specHash, "sha256:", "sha256-", 1)
}

// Containerfile renders the build Containerfile for base (a full reference): one
// exec-form RUN per install command, run as root, ending as UID 65532.
func Containerfile(base string, install []lastv1alpha1.ArgvCommand) (string, error) {
	if len(install) == 0 {
		return "", errors.New("image install is empty")
	}
	var sb strings.Builder
	sb.WriteString("FROM " + base + "\nUSER root\n")
	for i, c := range install {
		if len(c.Argv) == 0 {
			return "", fmt.Errorf("image install command %d has an empty argv", i)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(c.Argv); err != nil {
			return "", fmt.Errorf("encoding install command %d: %w", i, err)
		}
		sb.WriteString("RUN " + strings.TrimSuffix(buf.String(), "\n") + "\n")
	}
	sb.WriteString("USER 65532\n")
	return sb.String(), nil
}
