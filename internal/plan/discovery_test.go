package plan_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/plan"
)

const (
	kubectl = "kubectl"
	get     = "get"
	ns      = "ns"
)

func discovery() lastv1alpha1.Command {
	return lastv1alpha1.Command{ID: plan.DiscoveryStepID, Run: lastv1alpha1.CommandRun{Argv: []string{kubectl, get, ns}}}
}

func TestDiscoveryHashIsSHA256OfCommandAndNonce(t *testing.T) {
	h, size, err := plan.DiscoveryHash(discovery(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := audit.Canonical(map[string]any{"command": discovery(), "nonce": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if h != "sha256:"+hex.EncodeToString(sum[:]) || size != len(canonical) {
		t.Fatalf("DiscoveryHash = %s, %d; want the hash and size of %s", h, size, canonical)
	}
}

func TestDiscoveryHashDependsOnNonceAndCommand(t *testing.T) {
	a, _, _ := plan.DiscoveryHash(discovery(), "n1")
	b, _, _ := plan.DiscoveryHash(discovery(), "n2")
	other := discovery()
	other.Run.Argv = []string{kubectl, get, "pods"}
	c, _, _ := plan.DiscoveryHash(other, "n1")
	again, _, _ := plan.DiscoveryHash(discovery(), "n1")
	if a == b || a == c || a != again || !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("hashes: same=%s nonce=%s command=%s again=%s", a, b, c, again)
	}
}
