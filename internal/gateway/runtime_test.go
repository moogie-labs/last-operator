package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := lastv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func openWriter(t *testing.T, sink audit.Sink) *audit.Writer {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	w, err := audit.Open(context.Background(), sink, audit.NewSigner(key),
		audit.WriterConfig{ChainID: testChainID, Retention: time.Hour, Actor: "gateway"}, func(audit.StartInfo) any { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestBeginRPCNeedsAWritableAuditLog(t *testing.T) {
	g := testGateway(t, audittest.NewMemSink())
	if _, _, err := g.beginRPC(); status.Code(err) != codes.Unavailable {
		t.Fatalf("without a writer: %v, want Unavailable", err)
	}
	g.writer.Store(openWriter(t, audittest.NewMemSink()))
	w, release, err := g.beginRPC()
	if err != nil || w == nil {
		t.Fatalf("with a writer: %v", err)
	}
	release()
}

func TestDrainWaitsForInflightRPCs(t *testing.T) {
	g := testGateway(t, audittest.NewMemSink())
	g.writer.Store(openWriter(t, audittest.NewMemSink()))
	_, release, err := g.beginRPC()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan bool)
	go func() { drained <- g.drain(5 * time.Second) }()
	select {
	case <-drained:
		t.Fatal("drain returned while an RPC was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if _, _, err := g.beginRPC(); status.Code(err) != codes.Unavailable {
		t.Fatalf("a new RPC was admitted while draining: %v", err)
	}
	release()
	if ok := <-drained; !ok {
		t.Fatal("drain reported a timeout")
	}
	g2 := testGateway(t, audittest.NewMemSink())
	g2.writer.Store(openWriter(t, audittest.NewMemSink()))
	if _, _, err := g2.beginRPC(); err != nil {
		t.Fatal(err)
	}
	if g2.drain(20 * time.Millisecond) {
		t.Fatal("drain must report a timeout when an RPC never finishes")
	}
}

func TestSandboxImagePrefersOperatorConfig(t *testing.T) {
	pinned := "ghcr.io/moogie-labs/last-sandbox-base@sha256:" + strings.Repeat("a", 64)
	g := testGatewayWith(t, &lastv1alpha1.OperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: lastv1alpha1.OperatorConfigSpec{Sandbox: &lastv1alpha1.SandboxConfig{BaseImage: pinned}}})
	if img, err := g.sandboxImage(context.Background()); err != nil || img != pinned {
		t.Fatalf("image = %q, %v", img, err)
	}
	g = testGatewayWith(t)
	g.cfg.SandboxImage = "sandbox:dev"
	if img, err := g.sandboxImage(context.Background()); err != nil || img != "sandbox:dev" {
		t.Fatalf("fallback image = %q, %v", img, err)
	}
	g.cfg.SandboxImage = ""
	if _, err := g.sandboxImage(context.Background()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no image: %v, want FailedPrecondition", err)
	}
}

func TestResolveCanaries(t *testing.T) {
	g := testGatewayWith(t)
	g.kube = fake.NewClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "last-gateway", Namespace: ns}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.7.7"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "n1"}, {Type: corev1.NodeInternalIP, Address: "172.18.0.2"}}}},
	)
	g.cfg.EgressCanaries = []string{"10.1.1.1:80", "service/last-gateway/last-gateway:8080", "node:10250"}
	got, err := g.resolveCanaries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "10.1.1.1:80,10.96.7.7:8080,172.18.0.2:10250" {
		t.Fatalf("canaries = %q", got)
	}
	g.cfg.EgressCanaries = []string{"service/last-gateway/missing:8080"}
	if _, err := g.resolveCanaries(context.Background()); err == nil {
		t.Fatal("a missing service must be an error")
	}
}

func TestLeadReapsOrphanedRunsBeforeServing(t *testing.T) {
	sink := audittest.NewMemSink()
	sink.SetLockConfig(audit.LockConfig{Enabled: true, Mode: "COMPLIANCE", DefaultRetention: 365 * 24 * time.Hour})
	g := testGateway(t, sink)
	orphan := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "last-run-o", Namespace: testSandboxNS,
		Labels: sandbox.RunLabels("o"), CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))}}
	g.kube = fake.NewClientset(orphan)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.lead(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for g.writer.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g.writer.Load() == nil {
		cancel()
		t.Fatalf("lead never published a writer: %v", <-done)
	}
	if _, err := g.kube.CoreV1().Pods(testSandboxNS).Get(ctx, "last-run-o", metav1.GetOptions{}); err == nil {
		t.Fatal("orphaned sandbox pod survived lead start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("lead = %v", err)
	}
}
