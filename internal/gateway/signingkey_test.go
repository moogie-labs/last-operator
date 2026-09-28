package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/audit"
)

const ns = "last-gateway"

func TestEnsureSigningKeyCreatesThenReuses(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	k1, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("second call returned a different key (err=%v)", err)
	}
	sec, _ := cs.CoreV1().Secrets(ns).Get(ctx, "last-audit-signing", metav1.GetOptions{})
	if sec.Immutable == nil || !*sec.Immutable {
		t.Fatal("signing key Secret must be immutable")
	}
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	pub, err := audit.ParsePublicKeyPEM([]byte(cm.Data["ed25519.pub"]))
	if err != nil || !pub.Equal(k1.Public()) {
		t.Fatalf("published key does not match (err=%v)", err)
	}
	if cm.Data["fingerprint"] != audit.Fingerprint(pub) {
		t.Fatal("fingerprint mismatch")
	}
}

func TestEnsureSigningKeyRepairsPublishedKey(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	key, _ := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPEM, _ := audit.MarshalPublicKeyPEM(other)
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	cm.Data["ed25519.pub"] = string(otherPEM)
	_, _ = cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})

	if _, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey"); err != nil {
		t.Fatal(err)
	}
	cm, _ = cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	pub, _ := audit.ParsePublicKeyPEM([]byte(cm.Data["ed25519.pub"]))
	if !pub.Equal(key.Public()) {
		t.Fatal("published key was not repaired to match the signing key")
	}
}
