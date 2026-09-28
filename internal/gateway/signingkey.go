package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/moogie-labs/last-operator/internal/audit"
)

// EnsureSigningKey loads the audit signing key from its Secret, creating it on first
// start, and publishes the public key to a ConfigMap for verifiers.
func EnsureSigningKey(ctx context.Context, cs kubernetes.Interface, ns, secretName, cmName string) (ed25519.PrivateKey, error) {
	key, err := loadOrCreateKey(ctx, cs, ns, secretName, cmName)
	if err != nil {
		return nil, err
	}
	pub := key.Public().(ed25519.PublicKey)
	pubPEM, err := audit.MarshalPublicKeyPEM(pub)
	if err != nil {
		return nil, err
	}
	want := map[string]string{"ed25519.pub": string(pubPEM), "fingerprint": audit.Fingerprint(pub)}
	cms := cs.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, cmName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = cms.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns}, Data: want}, metav1.CreateOptions{})
	case err == nil && (cm.Data["ed25519.pub"] != want["ed25519.pub"] || cm.Data["fingerprint"] != want["fingerprint"]):
		cm.Data = want
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: publish public key: %w", err)
	}
	return key, nil
}

func loadOrCreateKey(ctx context.Context, cs kubernetes.Interface, ns, name, cmName string) (ed25519.PrivateKey, error) {
	secrets := cs.CoreV1().Secrets(ns)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return audit.ParsePrivateKeyPEM(sec.Data["ed25519.pem"])
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("gateway: read signing key: %w", err)
	}
	// The Secret is gone. If a public key is already published, generating and
	// publishing a fresh one here would silently replace the only verifier key for
	// the historical chain. Refuse and require an operator to restore the Secret
	// (or delete the ConfigMap too, to rotate deliberately).
	cm, cmErr := cs.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{})
	if cmErr == nil && (cm.Data["ed25519.pub"] != "" || cm.Data["fingerprint"] != "") {
		return nil, fmt.Errorf("gateway: signing key Secret %s/%s is missing but a public key is already published in ConfigMap %s; restore the Secret (or, to rotate deliberately, delete the ConfigMap too)", ns, name, cmName)
	}
	if cmErr != nil && !apierrors.IsNotFound(cmErr) {
		return nil, fmt.Errorf("gateway: read public key configmap: %w", cmErr)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pemBytes, err := audit.MarshalPrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	immutable := true
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Immutable:  &immutable,
		Data:       map[string][]byte{"ed25519.pem": pemBytes},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return loadOrCreateKey(ctx, cs, ns, name, cmName) // lost a creation race; use the winner's key
	}
	if err != nil {
		return nil, fmt.Errorf("gateway: create signing key: %w", err)
	}
	return key, nil
}
