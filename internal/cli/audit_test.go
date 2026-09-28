package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

func testDeps(sink audit.Sink, out *bytes.Buffer) Deps {
	return Deps{
		Out: out, Err: out,
		Getenv:  func(string) string { return "" },
		NewSink: func(context.Context, audit.S3Config) (audit.Sink, error) { return sink, nil },
	}
}

func execute(d Deps, args ...string) error {
	cmd := NewRootCommand(d)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(context.Background())
}

// writeChain creates chain "c1": gateway.start + 3 command entries across two intents.
func writeChain(t *testing.T) (*audittest.MemSink, string) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer := audit.NewSigner(key)
	sink := audittest.NewMemSink()
	w, err := audit.Open(context.Background(), sink, signer, audit.WriterConfig{ChainID: "c1", Retention: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"team-x/web", "team-y/db", "team-x/web"} {
		if _, err := w.Append(context.Background(), audit.Draft{Type: audit.TypeCommandStart, Actor: "operator", Intent: intent, Stage: "Create"}); err != nil {
			t.Fatal(err)
		}
	}
	pubPEM, _ := audit.MarshalPublicKeyPEM(signer.Public())
	pubFile := filepath.Join(t.TempDir(), "pub.pem")
	if err := os.WriteFile(pubFile, pubPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return sink, pubFile
}

func TestAuditVerifyOK(t *testing.T) {
	sink, pub := writeChain(t)
	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "OK: verified 4 entries (seq 0..3)") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestAuditVerifyReportsFirstBadSeq(t *testing.T) {
	sink, pub := writeChain(t)
	sink.TamperDelete(audit.EntryKey("c1", 2))
	var out bytes.Buffer
	err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub)
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("err = %v, want ErrVerifyFailed", err)
	}
	if !strings.Contains(out.String(), "first problem at seq 2 (gap)") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestAuditVerifyExpectHead(t *testing.T) {
	sink, pub := writeChain(t)
	var out bytes.Buffer
	err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub, "--expect-head", "50")
	if !errors.Is(err, ErrVerifyFailed) || !strings.Contains(out.String(), "truncated") {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
}

func TestAuditVerifyRequiresChain(t *testing.T) {
	sink, pub := writeChain(t)
	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "verify", "--pubkey-file", pub); err == nil || !strings.Contains(err.Error(), "--chain") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuditLogFiltersByIntent(t *testing.T) {
	sink, _ := writeChain(t)
	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "log", "--chain", "c1", "--intent", "team-x/web"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Count(s, "team-x/web") != 2 || strings.Contains(s, "team-y/db") || strings.Contains(s, "gateway.start") {
		t.Fatalf("output = %s", s)
	}
}

// F2: verify prints the fingerprint of the key it verified against, on its own line
// after the (unchanged) OK/FAILED line, so it can be checked out of band.
func TestAuditVerifyPrintsKeyFingerprint(t *testing.T) {
	sink, pub := writeChain(t)
	pemBytes, _ := os.ReadFile(pub)
	key, err := audit.ParsePublicKeyPEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	want := "key fingerprint: " + audit.Fingerprint(key)

	var out bytes.Buffer
	if err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "OK: verified 4 entries (seq 0..3), head sha256:") || lines[1] != want {
		t.Fatalf("output = %q, want the OK line then %q", out.String(), want)
	}

	sink.TamperDelete(audit.EntryKey("c1", 2))
	out.Reset()
	if err := execute(testDeps(sink, &out), "audit", "verify", "--chain", "c1", "--pubkey-file", pub); !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("err = %v, want ErrVerifyFailed", err)
	}
	lines = strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.HasPrefix(lines[0], "FAILED: first problem at seq 2") || len(lines) < 2 || lines[1] != want {
		t.Fatalf("output = %q, want the FAILED line then %q", out.String(), want)
	}
}
