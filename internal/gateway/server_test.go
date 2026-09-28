package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

func testGateway(t *testing.T, sink audit.Sink) *Gateway {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	cfg := Config{Namespace: ns, PodName: "gw-0", ChainID: "chain-1", Retention: 365 * 24 * time.Hour, HTTPAddr: ":0", LeaseName: "l"}
	return New(cfg, fake.NewClientset(), sink, audit.NewSigner(key))
}

func get(t *testing.T, g *Gateway, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestLeadRefusesBucketWithoutComplianceLock(t *testing.T) {
	sink := audittest.NewMemSink()
	sink.SetLockConfig(audit.LockConfig{Enabled: true, Mode: "GOVERNANCE", DefaultRetention: 365 * 24 * time.Hour})
	err := testGateway(t, sink).lead(context.Background())
	if err == nil || !strings.Contains(err.Error(), "COMPLIANCE") {
		t.Fatalf("lead = %v, want refusal mentioning COMPLIANCE", err)
	}
	if len(sink.Keys()) != 0 {
		t.Fatalf("wrote to a non-compliant bucket: %v", sink.Keys())
	}
}

func TestLeadOpensChainAndReportsReady(t *testing.T) {
	sink := audittest.NewMemSink()
	g := testGateway(t, sink)
	if code, _ := get(t, g, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before leading = %d, want 503", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.lead(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _ := get(t, g, "/readyz"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, err := sink.Get(context.Background(), audit.EntryKey("chain-1", 0), "")
	if err != nil {
		t.Fatalf("no gateway.start entry: %v", err)
	}
	var e audit.Entry
	_ = json.Unmarshal(body, &e)
	if e.Type != audit.TypeGatewayStart {
		t.Fatalf("first entry type = %s", e.Type)
	}
	if _, metrics := get(t, g, "/metrics"); !strings.Contains(metrics, "last_audit_chain_seq 0") ||
		!strings.Contains(metrics, `last_audit_writes_total{type="gateway.start"} 1`) {
		t.Fatalf("metrics missing audit series:\n%s", metrics)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead returned %v after cancel", err)
	}
	if code, _ := get(t, g, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after stepping down = %d, want 503", code)
	}
}

// TestShouldReportLeadError exercises the guard in OnStartedLeading (server.go) that
// stops a lead error from beating OnStoppedLeading's own cancel(errLostLeadership) when
// leadership is lost: once lctx is canceled, a lead error must not be reported even if
// it wraps context.Canceled. This is a pure-function check (no leader-election timing
// involved) so it isn't flaky.
func TestShouldReportLeadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !shouldReportLeadError(ctx, errors.New("boom")) {
		t.Error("want true: live context, real error")
	}
	if shouldReportLeadError(ctx, nil) {
		t.Error("want false: no error")
	}

	cancel()
	if shouldReportLeadError(ctx, fmt.Errorf("gateway: checkpoint: %w", context.Canceled)) {
		t.Error("want false: lctx already canceled, even though err wraps context.Canceled")
	}
	if shouldReportLeadError(ctx, errors.New("unrelated failure")) {
		t.Error("want false: lctx already canceled, regardless of the error's content")
	}
}

// startLead runs g.lead in the background and waits until the gateway is ready.
func startLead(t *testing.T, g *Gateway) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.lead(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _ := get(t, g, "/readyz"); code == http.StatusOK {
			return cancel, done
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("lead returned before becoming ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("gateway never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// recordingSink records the retainUntil of every Put.
type recordingSink struct {
	*audittest.MemSink
	mu          sync.Mutex
	retainUntil []time.Time
}

func (r *recordingSink) Put(ctx context.Context, key string, body []byte, until time.Time) error {
	r.mu.Lock()
	r.retainUntil = append(r.retainUntil, until)
	r.mu.Unlock()
	return r.MemSink.Put(ctx, key, body, until)
}

// F1: an explicit per-object retention overrides the bucket default, so the gateway
// must lock entries for max(configured, bucket default).
func TestLeadUsesLongerBucketDefaultRetention(t *testing.T) {
	const year = 365 * 24 * time.Hour
	sink := &recordingSink{MemSink: audittest.NewMemSink()}
	sink.SetLockConfig(audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 2 * year})
	g := testGateway(t, sink) // configured retention: 1 year
	start := time.Now()
	cancel, done := startLead(t, g)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.retainUntil) == 0 {
		t.Fatal("no Put recorded")
	}
	for _, until := range sink.retainUntil {
		if d := until.Sub(start); d < 2*year-time.Minute || d > 2*year+time.Minute {
			t.Fatalf("retainUntil = now+%s, want now+%s (the bucket default)", d, 2*year)
		}
	}
}

// startPayload runs one lead cycle and returns the gateway.start payload at seq.
func startPayload(t *testing.T, g *Gateway, sink audit.Sink, seq uint64) map[string]json.RawMessage {
	t.Helper()
	cancel, done := startLead(t, g)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead: %v", err)
	}
	body, err := sink.Get(context.Background(), audit.EntryKey("chain-1", seq), "")
	if err != nil {
		t.Fatal(err)
	}
	var e audit.Entry
	if err := json.Unmarshal(body, &e); err != nil || e.Type != audit.TypeGatewayStart {
		t.Fatalf("seq %d: type %s (%v)", seq, e.Type, err)
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// F2: gateway.start anchors the full public key (and its fingerprint) in WORM storage.
func TestLeadStartPayloadRecordsPublicKey(t *testing.T) {
	sink := audittest.NewMemSink()
	g := testGateway(t, sink)
	p := startPayload(t, g, sink, 0)
	var pemStr, fp string
	_ = json.Unmarshal(p["publicKey"], &pemStr)
	_ = json.Unmarshal(p["keyFingerprint"], &fp)
	pub, err := audit.ParsePublicKeyPEM([]byte(pemStr))
	if err != nil || !pub.Equal(g.signer.Public()) || fp != audit.Fingerprint(g.signer.Public()) {
		t.Fatalf("publicKey/keyFingerprint do not identify the signing key: %v (err=%v)", p, err)
	}
}
