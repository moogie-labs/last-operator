package audit_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/moogie-labs/last-operator/internal/audit"
)

func newSigner(t *testing.T) *audit.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return audit.NewSigner(key)
}

func sampleEntry() audit.Entry {
	return audit.Entry{
		Seq: 7, ChainID: "c1", PrevHash: audit.GenesisHash,
		TS: "2026-09-27T00:00:00.000000000Z", Type: audit.TypeCommandStart,
		Actor: "operator", Intent: "team-x/web",
		Payload: json.RawMessage(`{"argv":["kubectl","get","pods"]}`),
	}
}

func TestCanonicalSortsKeys(t *testing.T) {
	got, err := audit.Canonical(map[string]int{"b": 1, "a": 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2,"b":1}` {
		t.Fatalf("canonical = %s", got)
	}
}

func TestComputeHashIgnoresHashAndSig(t *testing.T) {
	e := sampleEntry()
	h1, err := audit.ComputeHash(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Hash, e.Sig = "sha256:whatever", "ed25519:whatever"
	h2, _ := audit.ComputeHash(e)
	if h1 != h2 {
		t.Fatalf("hash changed when only hash/sig changed: %s vs %s", h1, h2)
	}
}

func TestComputeHashCoversPayload(t *testing.T) {
	a := sampleEntry()
	b := sampleEntry()
	b.Payload = json.RawMessage(`{"argv":["kubectl","delete","pods"]}`)
	ha, _ := audit.ComputeHash(a)
	hb, _ := audit.ComputeHash(b)
	if ha == hb {
		t.Fatal("different payloads produced the same hash")
	}
}

func TestSealSurvivesJSONRoundTripAndWhitespace(t *testing.T) {
	s := newSigner(t)
	e := sampleEntry()
	e.Payload = json.RawMessage(`{ "b": 1,   "a": [1, 2] }`)
	if err := s.Seal(&e); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back audit.Entry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckSeal(s.Public(), back); err != nil {
		t.Fatalf("round-tripped entry failed CheckSeal: %v", err)
	}
}

func TestCheckSealDetectsTampering(t *testing.T) {
	s := newSigner(t)
	e := sampleEntry()
	if err := s.Seal(&e); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckSeal(s.Public(), e); err != nil {
		t.Fatalf("fresh entry: %v", err)
	}

	tampered := e
	tampered.Payload = json.RawMessage(`{"argv":["rm","-rf","/"]}`)
	if err := audit.CheckSeal(s.Public(), tampered); !errors.Is(err, audit.ErrBadHash) {
		t.Fatalf("tampered payload: got %v, want ErrBadHash", err)
	}

	badSig := e
	badSig.Sig = "ed25519:AAAA"
	if err := audit.CheckSeal(s.Public(), badSig); !errors.Is(err, audit.ErrBadSignature) {
		t.Fatalf("bad sig: got %v, want ErrBadSignature", err)
	}

	forged := sampleEntry()
	if err := newSigner(t).Seal(&forged); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckSeal(s.Public(), forged); !errors.Is(err, audit.ErrBadSignature) {
		t.Fatalf("other key: got %v, want ErrBadSignature", err)
	}
}

func TestKeyPEMRoundTrip(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	privPEM, err := audit.MarshalPrivateKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	back, err := audit.ParsePrivateKeyPEM(privPEM)
	if err != nil || !back.Equal(key) {
		t.Fatalf("private key round trip failed: %v", err)
	}
	pubPEM, err := audit.MarshalPublicKeyPEM(key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := audit.ParsePublicKeyPEM(pubPEM)
	if err != nil || !pub.Equal(key.Public()) {
		t.Fatalf("public key round trip failed: %v", err)
	}
	if _, err := audit.ParsePublicKeyPEM([]byte("junk")); err == nil {
		t.Fatal("parsed junk as a public key")
	}
}
