package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

// defaultMaxConcurrent is the sandbox cap when OperatorConfig does not set one (spec §6).
const defaultMaxConcurrent = 20

func newEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// beginRPC admits a side-effecting RPC: it returns the audit writer and a release
// func, or Unavailable when this replica cannot write the audit log or is draining.
func (g *Gateway) beginRPC() (*audit.Writer, func(), error) {
	g.rpcMu.Lock()
	defer g.rpcMu.Unlock()
	if g.draining {
		return nil, nil, status.Error(codes.Unavailable, "gateway is stepping down")
	}
	w := g.writer.Load()
	if w == nil || w.Broken() {
		return nil, nil, status.Error(codes.Unavailable, "audit log is not writable on this replica")
	}
	g.inflight.Add(1)
	return w, g.inflight.Done, nil
}

// drain refuses new side-effecting RPCs, cancels every image build, and waits up
// to timeout for in-flight ones, so their outcome entries are written before the
// writer is released.
//
// lead runs once per process (a lost lease exits the process), so the WaitGroup
// and draining flag are never reused across leadership terms.
func (g *Gateway) drain(timeout time.Duration) bool {
	g.rpcMu.Lock()
	g.draining = true
	g.rpcMu.Unlock()
	// A build can outlast the window; cancelled, each records its result within it.
	g.builds.stop()
	done := make(chan struct{})
	go func() { g.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		slog.Warn("Could not drain in-flight RPCs before stepping down", "timeout", timeout)
		return false
	}
}

func (g *Gateway) providerGetter(ctx context.Context, name string) (*lastv1alpha1.Provider, error) {
	var p lastv1alpha1.Provider
	if err := g.crd.Get(ctx, client.ObjectKey{Name: name}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// executor builds a sandbox executor for one RPC; every executor shares the
// scheduler. Its pods run on image, start the runner copied from base, the pinned
// sandbox base (spec §11.1), and pull with pullSecrets.
func (g *Gateway) executor(w *audit.Writer, base, image string, pullSecrets ...string) *sandbox.Executor {
	cfg := g.execCfg
	cfg.Pod.Image = image
	cfg.Pod.RunnerImage = base
	cfg.Pod.ImagePullSecrets = pullSecrets
	return sandbox.NewExecutor(g.kube, g.providerGetter, w, g.sched.Load(), g.logs, cfg)
}

// runExecutor is executor for a sandbox run. In registry.mode external it first
// copies the pull Secret into the sandbox namespace, and the run's pods pull with
// it. Errors are ensurePullSecret's, or Unavailable when OperatorConfig cannot be read.
func (g *Gateway) runExecutor(ctx context.Context, w *audit.Writer, base, image string) (*sandbox.Executor, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !externalRegistry(op) {
		return g.executor(w, base, image), nil
	}
	ext, err := externalSettings(op)
	if err != nil {
		return nil, err
	}
	if err := g.ensurePullSecret(ctx, ext); err != nil {
		return nil, err
	}
	return g.executor(w, base, image, registryPullSecret), nil
}

// operatorConfig returns the cluster OperatorConfig, or nil when none exists.
func (g *Gateway) operatorConfig(ctx context.Context) (*lastv1alpha1.OperatorConfig, error) {
	var op lastv1alpha1.OperatorConfig
	err := g.crd.Get(ctx, client.ObjectKey{Name: "default"}, &op)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read OperatorConfig: %v", err)
	}
	return &op, nil
}

// sandboxImage returns OperatorConfig's digest-pinned base image, or the
// configured fallback (development installs only).
func (g *Gateway) sandboxImage(ctx context.Context) (string, error) {
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return "", err
	}
	if op != nil && op.Spec.Sandbox != nil && op.Spec.Sandbox.BaseImage != "" {
		return op.Spec.Sandbox.BaseImage, nil
	}
	if g.cfg.SandboxImage != "" {
		return g.cfg.SandboxImage, nil
	}
	return "", status.Error(codes.FailedPrecondition, "no sandbox image: set OperatorConfig spec.sandbox.baseImage")
}

// resolveCanaries turns canary specs into IP:port literals. Services resolve to
// their ClusterIP, "node" to the first node InternalIP.
func (g *Gateway) resolveCanaries(ctx context.Context) ([]string, error) {
	out := make([]string, 0, len(g.cfg.EgressCanaries))
	for _, spec := range g.cfg.EgressCanaries {
		head, port, _ := strings.Cut(spec, ":")
		switch {
		case head == "node":
			nodes, err := g.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("gateway: canary %s: list nodes: %w", spec, err)
			}
			ip := firstInternalIP(nodes.Items)
			if ip == "" {
				return nil, fmt.Errorf("gateway: canary %s: no node has an InternalIP", spec)
			}
			if err := checkResolvedIP(spec, ip); err != nil {
				return nil, err
			}
			out = append(out, net.JoinHostPort(ip, port))
		case strings.HasPrefix(head, "service/"):
			parts := strings.Split(head, "/")
			if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
				return nil, fmt.Errorf("gateway: canary %s: want service/<namespace>/<name>:<port>", spec)
			}
			svc, err := g.kube.CoreV1().Services(parts[1]).Get(ctx, parts[2], metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("gateway: canary %s: %w", spec, err)
			}
			if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
				return nil, fmt.Errorf("gateway: canary %s: service has no ClusterIP", spec)
			}
			if err := checkResolvedIP(spec, svc.Spec.ClusterIP); err != nil {
				return nil, err
			}
			out = append(out, net.JoinHostPort(svc.Spec.ClusterIP, port))
		default:
			out = append(out, spec)
		}
	}
	return out, nil
}

func checkResolvedIP(spec, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("gateway: canary %s: resolved %q is not an IP address", spec, ip)
	}
	if err := checkCanaryAddr(addr); err != nil {
		return fmt.Errorf("gateway: canary %s: %w", spec, err)
	}
	return nil
}

func firstInternalIP(nodes []corev1.Node) string {
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				return a.Address
			}
		}
	}
	return ""
}

// prepareLeading runs before this replica publishes its writer: canaries are
// resolved, the scheduler is sized, every orphaned run is reaped, the image index
// is loaded and every leftover build is reaped. Any error ends leadership, so
// nothing runs without an egress gate or over stale grants.
func (g *Gateway) prepareLeading(ctx context.Context, w *audit.Writer) error {
	canaries, err := g.resolveCanaries(ctx)
	if err != nil {
		return err
	}
	if len(canaries) == 0 {
		return errors.New("gateway: no egress canaries resolved; refusing to lead without an egress gate")
	}
	g.execCfg.EgressCanaries = canaries
	maxConcurrent := defaultMaxConcurrent
	op, err := g.operatorConfig(ctx)
	if err != nil {
		return err
	}
	if op != nil && op.Spec.Sandbox != nil && op.Spec.Sandbox.MaxConcurrent > 0 {
		maxConcurrent = int(op.Spec.Sandbox.MaxConcurrent)
	}
	g.sched.Store(sandbox.NewScheduler(maxConcurrent))
	reaped, err := g.executor(w, "", "").ReapOrphans(ctx, 0, time.Now())
	if err != nil {
		return fmt.Errorf("gateway: reap orphaned sandbox runs: %w", err)
	}
	slog.Info("Reaped orphaned sandbox runs", "count", reaped, "maxConcurrent", maxConcurrent, "canaries", len(canaries))
	if err := g.prepareImages(ctx, op); err != nil {
		return err
	}
	g.rpcMu.Lock()
	g.draining = false
	g.rpcMu.Unlock()
	return nil
}

// keyedMutex serializes work per key (per Intent).
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) get(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l, ok := k.m[key]
	if !ok {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	return l
}

// lock waits for key's lock and returns its unlock.
func (k *keyedMutex) lock(key string) func() {
	l := k.get(key)
	l.Lock()
	return l.Unlock
}

// tryLock takes key's lock only if it is free; ok is false when it is held.
func (k *keyedMutex) tryLock(key string) (unlock func(), ok bool) {
	l := k.get(key)
	if !l.TryLock() {
		return nil, false
	}
	return l.Unlock, true
}
