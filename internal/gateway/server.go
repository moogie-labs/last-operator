package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/sandbox"
)

var errLostLeadership = errors.New("gateway: lost audit-writer leadership")

// Version is set at build time with -ldflags.
var Version = "dev"

type Gateway struct {
	cfg     Config
	kube    kubernetes.Interface
	crd     client.Reader
	sink    audit.Sink
	signer  *audit.Signer
	metrics *Metrics
	writer  atomic.Pointer[audit.Writer]

	epoch   string // per process; an open phase from another epoch was interrupted
	sched   atomic.Pointer[sandbox.Scheduler]
	logs    sandbox.LogReader
	execCfg sandbox.ExecutorConfig
	locks   keyedMutex
	state   *StateStore
	phases  phaseTable
	probing sync.Map // intent key -> in-flight probe run
	index   auditIndex
	images  *imageIndex // built-image digest cache

	rpcMu    sync.Mutex
	draining bool
	inflight sync.WaitGroup
}

func New(cfg Config, kube kubernetes.Interface, crd client.Reader, sink audit.Sink, signer *audit.Signer) *Gateway {
	g := &Gateway{cfg: cfg, kube: kube, crd: crd, sink: sink, signer: signer, metrics: NewMetrics(),
		epoch: newEpoch(), logs: sandbox.KubeLogReader(kube), state: NewStateStore(kube, cfg.Namespace),
		images: newImageIndex(kube, cfg.Namespace)}
	g.execCfg = sandbox.ExecutorConfig{
		Pod:               sandbox.PodConfig{Namespace: cfg.SandboxNamespace, ImagePullPolicy: corev1.PullIfNotPresent},
		ProviderNamespace: cfg.Namespace,
	}
	if cfg.AuditWriterSecret != "" {
		g.execCfg.ForbiddenSecrets = []string{cfg.AuditWriterSecret}
	}
	return g
}

// Handler serves liveness, readiness (leader with a healthy writer), and metrics.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		g.rpcMu.Lock()
		draining := g.draining
		g.rpcMu.Unlock()
		if wr := g.writer.Load(); draining || wr == nil || wr.Broken() {
			http.Error(w, "audit writer not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(g.metrics.registry, promhttp.HandlerOpts{}))
	return mux
}

// Run serves HTTP and competes for the audit-writer lease. It returns an error
// (so the process exits and restarts) if leading fails or leadership is lost.
func (g *Gateway) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	srv := &http.Server{Addr: g.cfg.HTTPAddr, Handler: g.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("gateway: http server: %w", err))
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	lis, err := net.Listen("tcp", g.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("gateway: grpc listen: %w", err)
	}
	grpcSrv := g.GRPCServer(NewAuthenticator(g.kube, GRPCAudience, g.cfg.OperatorIdentity))
	go func() {
		if err := grpcSrv.Serve(lis); err != nil {
			cancel(fmt.Errorf("gateway: grpc server: %w", err))
		}
	}()
	// stopGRPC runs after lead's drain, so in-flight RPCs are not cut short.
	stopGRPC := func() {
		done := make(chan struct{})
		go func() { grpcSrv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(g.cfg.DrainTimeout):
			grpcSrv.Stop()
		}
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: g.cfg.LeaseName, Namespace: g.cfg.Namespace},
		Client:     g.kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: g.cfg.PodName},
	}
	var started atomic.Bool
	leadDone := make(chan struct{})
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// ReleaseOnCancel defaults to false: on shutdown or a failed renewal we let the
		// lease expire naturally after LeaseDuration instead of releasing it early. A
		// successor otherwise could acquire the lease and call audit.Open while our own
		// detached in-flight write (up to PutTimeout) might still land. The lease is only
		// a contention-reducer, not the correctness guarantee — the real single-writer
		// fence is the create-only (If-None-Match) write of each chain entry key, which
		// makes a losing concurrent writer fail closed rather than corrupt the chain.
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(lctx context.Context) {
				started.Store(true)
				defer close(leadDone)
				if err := g.lead(lctx); shouldReportLeadError(lctx, err) {
					cancel(err)
				}
			},
			OnStoppedLeading: func() { cancel(errLostLeadership) },
		},
	})
	// client-go runs OnStartedLeading in its own goroutine and RunOrDie returns
	// without waiting for it; wait for lead's drain so outcomes are recorded
	// before the process exits.
	if started.Load() {
		select {
		case <-leadDone:
		case <-time.After(g.cfg.DrainTimeout + 10*time.Second):
			slog.Warn("Gave up waiting for the leader to drain")
		}
	}
	stopGRPC()
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// shouldReportLeadError reports whether an error returned by lead should cancel the
// outer context. It is false once lctx is already canceled: OnStoppedLeading cancels
// the outer context (with errLostLeadership) as soon as leadership is lost, and lead
// itself typically returns promptly afterward with an error that wraps context.Canceled.
// Without this check, that race could let a lead error "win" over errLostLeadership and
// make Run return nil (a clean exit) when leadership was actually lost.
func shouldReportLeadError(lctx context.Context, err error) bool {
	return err != nil && lctx.Err() == nil
}

// lead checks the bucket's lock, opens the chain (verifying it), and writes
// checkpoints until ctx ends. It refuses to write to a non-compliant bucket, and
// returns an error as soon as the writer breaks so the process restarts and Open
// re-reads the chain.
func (g *Gateway) lead(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	lc, err := g.sink.LockConfig(ctx)
	if err != nil {
		return fmt.Errorf("gateway: read bucket lock config: %w", err)
	}
	if err := audit.CheckLockConfig(lc, g.cfg.Retention); err != nil {
		return err
	}
	// An explicit per-object retain-until overrides the bucket default, so lock each
	// entry for the longer of the two rather than silently undercutting the bucket.
	retention := max(g.cfg.Retention, lc.DefaultRetention)
	slog.Info("audit retention", "configured", g.cfg.Retention, "bucketDefault", lc.DefaultRetention, "effective", retention)
	pubPEM, err := audit.MarshalPublicKeyPEM(g.signer.Public())
	if err != nil {
		return err
	}
	w, err := audit.Open(ctx, g.sink, g.signer, audit.WriterConfig{
		ChainID: g.cfg.ChainID, Retention: retention, Actor: "gateway", AllowNewChain: g.cfg.AllowNewChain,
	}, func(info audit.StartInfo) any {
		p := map[string]any{
			"pod": g.cfg.PodName, "version": Version,
			// Anchor the full verifier key in WORM storage, not only in a mutable ConfigMap.
			"publicKey": string(pubPEM), "keyFingerprint": audit.Fingerprint(g.signer.Public()),
		}
		if info.ResumedFrom != nil { // omitted on a new chain
			p["resumedFrom"] = info.ResumedFrom
		}
		return p
	})
	if err != nil {
		return err
	}
	// Metrics hooks are set after Open, so count gateway.start explicitly.
	if seq, _, ok := w.Head(); ok {
		g.metrics.ObserveWrite(audit.Entry{Seq: seq, Type: audit.TypeGatewayStart})
	}
	g.observeWrites(w)
	// The hook runs under the writer's mutex: only record the metric and cancel here.
	w.SetOnFailure(func(err error) {
		g.metrics.ObserveFailure(err)
		cancel(fmt.Errorf("gateway: audit writer failed: %w", err))
	})
	if err := g.prepareLeading(ctx, w); err != nil {
		return err
	}
	g.writer.Store(w)
	defer g.writer.Store(nil)
	// Deferred later, so it runs first. drain refuses new RPCs and lets in-flight
	// ones finish and record their outcomes. Then the index entries those writes
	// produced are saved: the writer is still stored, but nothing new can use it.
	// Only then is the writer withdrawn.
	defer func() {
		g.drain(g.cfg.DrainTimeout)
		g.flushIndexesOnStepDown(ctx)
	}()
	slog.Info("audit writer ready", "chain", g.cfg.ChainID, "pod", g.cfg.PodName)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	flush := time.NewTicker(indexFlushInterval)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil // stepping down or shutting down
			}
			return context.Cause(ctx)
		case <-flush.C:
			g.flushIndexes(ctx)
		case <-ticker.C:
			if err := w.CheckpointIfDue(ctx); err != nil {
				return fmt.Errorf("gateway: checkpoint: %w", err)
			}
		}
	}
}
