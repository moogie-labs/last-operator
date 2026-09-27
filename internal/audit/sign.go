package audit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrBadHash      = errors.New("audit: entry hash does not match its contents")
	ErrBadSignature = errors.New("audit: entry signature is invalid")
)

// Signer seals entries with the gateway's Ed25519 key.
type Signer struct{ key ed25519.PrivateKey }

func NewSigner(key ed25519.PrivateKey) *Signer { return &Signer{key: key} }

func (s *Signer) Public() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// Seal fills e.Hash and e.Sig.
func (s *Signer) Seal(e *Entry) error {
	h, err := ComputeHash(*e)
	if err != nil {
		return err
	}
	e.Hash = h
	e.Sig = "ed25519:" + base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(h)))
	return nil
}

// CheckSeal verifies e's hash, then its signature over that hash.
func CheckSeal(pub ed25519.PublicKey, e Entry) error {
	h, err := ComputeHash(e)
	if err != nil || h != e.Hash {
		return ErrBadHash
	}
	b64, ok := strings.CutPrefix(e.Sig, "ed25519:")
	if !ok {
		return ErrBadSignature
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !ed25519.Verify(pub, []byte(e.Hash), sig) {
		return ErrBadSignature
	}
	return nil
}

// Fingerprint identifies a public key in logs and ConfigMaps.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func MarshalPrivateKeyPEM(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func ParsePrivateKeyPEM(b []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("audit: no PEM block in private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("audit: private key is %T, want ed25519", k)
	}
	return key, nil
}

func MarshalPublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func ParsePublicKeyPEM(b []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("audit: no PEM block in public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("audit: public key is %T, want ed25519", k)
	}
	return pub, nil
}
