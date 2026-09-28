package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
