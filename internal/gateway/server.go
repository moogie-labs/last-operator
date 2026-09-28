package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/moogie-labs/last-operator/internal/audit"
)

var errLostLeadership = errors.New("gateway: lost audit-writer leadership")

// Version is set at build time with -ldflags.
var Version = "dev"

type Gateway struct {
	cfg     Config
	kube    kubernetes.Interface
	sink    audit.Sink
	signer  *audit.Signer
	metrics *Metrics
	writer  atomic.Pointer[audit.Writer]
}

func New(cfg Config, kube kubernetes.Interface, sink audit.Sink, signer *audit.Signer) *Gateway {
	return &Gateway{cfg: cfg, kube: kube, sink: sink, signer: signer, metrics: NewMetrics()}
}

// Handler serves liveness, readiness (leader with a healthy writer), and metrics.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if wr := g.writer.Load(); wr == nil || wr.Broken() {
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

	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: g.cfg.LeaseName, Namespace: g.cfg.Namespace},
		Client:     g.kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: g.cfg.PodName},
	}
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
				if err := g.lead(lctx); shouldReportLeadError(lctx, err) {
					cancel(err)
				}
			},
			OnStoppedLeading: func() { cancel(errLostLeadership) },
		},
	})
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
// checkpoints until ctx ends. It refuses to write to a non-compliant bucket.
func (g *Gateway) lead(ctx context.Context) error {
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
	w, err := audit.Open(ctx, g.sink, g.signer, audit.WriterConfig{
		ChainID: g.cfg.ChainID, Retention: retention, Actor: "gateway",
	}, map[string]string{
		"pod": g.cfg.PodName, "version": Version, "keyFingerprint": audit.Fingerprint(g.signer.Public()),
	})
	if err != nil {
		return err
	}
	// Metrics hooks are set after Open, so count gateway.start explicitly.
	if seq, _, ok := w.Head(); ok {
		g.metrics.ObserveWrite(audit.Entry{Seq: seq, Type: audit.TypeGatewayStart})
	}
	w.SetOnWrite(g.metrics.ObserveWrite)
	w.SetOnFailure(g.metrics.ObserveFailure)
	g.writer.Store(w)
	defer g.writer.Store(nil)
	slog.Info("audit writer ready", "chain", g.cfg.ChainID, "pod", g.cfg.PodName)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.CheckpointIfDue(ctx); err != nil {
				return fmt.Errorf("gateway: checkpoint: %w", err)
			}
		}
	}
}
