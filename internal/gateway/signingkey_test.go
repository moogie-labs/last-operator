package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

// F2: a published key that differs from the Secret's key is never overwritten: it may
// be the only verifier key for the historical chain, or evidence of a key swap.
func TestEnsureSigningKeyRefusesMismatchedPublishedKey(t *testing.T) {
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPEM, _ := audit.MarshalPublicKeyPEM(other)
	for name, tamper := range map[string]func(map[string]string){
		"public key":  func(d map[string]string) { d["ed25519.pub"] = string(otherPEM) },
		"fingerprint": func(d map[string]string) { d["fingerprint"] = audit.Fingerprint(other) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cs := fake.NewClientset()
			if _, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey"); err != nil {
				t.Fatal(err)
			}
			cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
			tamper(cm.Data)
			if _, err := cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(cm.Data)

			_, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
			if err == nil || !strings.Contains(err.Error(), "does not match") ||
				!strings.Contains(err.Error(), "last-audit-signing") || !strings.Contains(err.Error(), "last-audit-pubkey") {
				t.Fatalf("err = %v, want a refusal naming the ConfigMap and the Secret", err)
			}
			cm, _ = cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
			if !maps.Equal(cm.Data, want) {
				t.Fatalf("ConfigMap changed despite the refusal: %v", cm.Data)
			}
		})
	}
}

func TestEnsureSigningKeyFillsEmptyConfigMap(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "last-audit-pubkey", Namespace: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	key, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	pub, err := audit.ParsePublicKeyPEM([]byte(cm.Data["ed25519.pub"]))
	if err != nil || !pub.Equal(key.Public()) || cm.Data["fingerprint"] != audit.Fingerprint(pub) {
		t.Fatalf("empty ConfigMap not filled with the signing key: %v (err=%v)", cm.Data, err)
	}
}

func TestEnsureSigningKeyRefusesToRegenerateWhenSecretMissingButKeyPublished(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	if _, err := EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey"); err != nil {
		t.Fatal(err)
	}
	cmBefore, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantPub, wantFingerprint := cmBefore.Data["ed25519.pub"], cmBefore.Data["fingerprint"]

	if err := cs.CoreV1().Secrets(ns).Delete(ctx, "last-audit-signing", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err = EnsureSigningKey(ctx, cs, ns, "last-audit-signing", "last-audit-pubkey")
	if err == nil {
		t.Fatal("want an error when the Secret is missing but a public key is already published")
	}
	if !strings.Contains(err.Error(), "last-audit-signing") || !strings.Contains(err.Error(), "last-audit-pubkey") {
		t.Fatalf("err = %v, want it to name the missing Secret and the published ConfigMap", err)
	}

	cmAfter, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "last-audit-pubkey", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cmAfter.Data["ed25519.pub"] != wantPub || cmAfter.Data["fingerprint"] != wantFingerprint {
		t.Fatal("published key was overwritten despite the missing-Secret error")
	}
}
