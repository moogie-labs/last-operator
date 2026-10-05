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
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

const (
	testChainID = "chain-1"
	testPodName = "gw-0"
	testStage   = "Create"
)

const (
	testCanary    = "10.96.0.10:8080"
	testSandboxNS = "last-sandbox"
	testLockMode  = "COMPLIANCE"
)

const testOperator = "system:serviceaccount:last-system:last-operator"

func testGateway(t *testing.T, sink audit.Sink, objs ...client.Object) *Gateway {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	cfg := Config{Namespace: ns, PodName: testPodName, ChainID: testChainID, Retention: 365 * 24 * time.Hour, HTTPAddr: ":0",
		LeaseName: "l", SandboxNamespace: testSandboxNS, EgressCanaries: []string{testCanary},
		OperatorIdentity: testOperator, DrainTimeout: time.Second}
	crd := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return New(cfg, fake.NewClientset(), crd, sink, audit.NewSigner(key))
}

func testGatewayWith(t *testing.T, objs ...client.Object) *Gateway {
	t.Helper()
	return testGateway(t, audittest.NewMemSink(), objs...)
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
	sink.SetLockConfig(audit.LockConfig{Enabled: true, Mode: testLockMode, DefaultRetention: 2 * year})
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

// F3: gateway.start records the verified head it resumed from (absent on a new chain).
func TestLeadStartPayloadRecordsResumption(t *testing.T) {
	sink := audittest.NewMemSink()
	g := testGateway(t, sink)
	if p := startPayload(t, g, sink, 0); p["resumedFrom"] != nil {
		t.Fatalf("new chain recorded resumedFrom %s", p["resumedFrom"])
	}
	body, _ := sink.Get(context.Background(), audit.EntryKey("chain-1", 0), "")
	var first audit.Entry
	_ = json.Unmarshal(body, &first)
	p := startPayload(t, g, sink, 1) // restart on the same chain
	if want := fmt.Sprintf(`{"hash":%q,"seq":0}`, first.Hash); string(p["resumedFrom"]) != want {
		t.Fatalf("resumedFrom = %s, want %s", p["resumedFrom"], want)
	}
}

// F3: lead refuses to start a new chain in a bucket that already holds another one,
// unless AllowNewChain is set.
func TestLeadRefusesSilentNewChain(t *testing.T) {
	sink := audittest.NewMemSink()
	old := testGateway(t, sink)
	old.cfg.ChainID = "chain-0"
	cancel, done := startLead(t, old)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, sink)
	ctx, cancelLead := context.WithTimeout(context.Background(), 3*time.Second) // bound the test if lead does not refuse
	defer cancelLead()
	if err := g.lead(ctx); err == nil || !strings.Contains(err.Error(), "chain-0") {
		t.Fatalf("lead = %v, want a refusal naming chain-0", err)
	}
	g.cfg.AllowNewChain = true
	cancel, done = startLead(t, g)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// F5: once the writer breaks, lead must return an error so Run exits and the pod
// restarts (Open then re-reads and re-verifies the chain) instead of staying unready.
func TestLeadReturnsErrorWhenWriterBreaks(t *testing.T) {
	sink := audittest.NewMemSink()
	g := testGateway(t, sink)
	cancel, done := startLead(t, g)
	defer cancel()
	sink.SetFailPuts(true)
	if _, err := g.writer.Load().Append(context.Background(), audit.Draft{Type: audit.TypeCommandStart, Actor: "operator"}); err == nil {
		t.Fatal("append succeeded on a failing store")
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, audit.ErrWriterBroken) {
			t.Fatalf("lead = %v, want an error wrapping ErrWriterBroken", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lead kept running with a broken writer")
	}
	if code, _ := get(t, g, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after the writer broke = %d, want 503", code)
	}
}
