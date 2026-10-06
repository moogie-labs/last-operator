package controller

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/policy"
)

// probeTimeout bounds one RunProbes call, including the wait for a sandbox slot.
const probeTimeout = 15 * time.Minute

// ManagedResourceReconciler runs each Ready or Drifted Intent's probes on a
// fixed interval and records the outcomes on its ManagedResources (spec §10,
// §10.1). One RunProbes call covers all of an Intent's ManagedResources, so the
// reconcile key is the Intent. It never calls the LLM.
type ManagedResourceReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Gateway gatewayv1.GatewayServiceClient
	// Reader reads ManagedResources uncached, so a probe run is never recorded
	// on, or scheduled from, a stale or missing copy.
	Reader client.Reader
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu   sync.Mutex
	next map[types.NamespacedName]time.Time // when each Intent's next probe run is due
}

// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intentpolicies;operatorconfigs,verbs=get;list;watch

// Reconcile runs the Intent's probes when they are due and requeues for the next run.
func (r *ManagedResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var in lastv1alpha1.Intent
	if err := r.Get(ctx, req.NamespacedName, &in); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if (in.Status.Phase != lastv1alpha1.PhaseReady && in.Status.Phase != lastv1alpha1.PhaseDrifted) ||
		!hasProbes(in.Status.Plan) {
		r.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	interval, err := r.interval(ctx, in.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	due, ok := r.due(req.NamespacedName)
	if !ok {
		last, err := r.lastProbe(ctx, &in)
		if err != nil {
			return ctrl.Result{}, err
		}
		due = last.Add(interval)
		r.setDue(req.NamespacedName, due)
	}
	if now.Before(due) {
		return ctrl.Result{RequeueAfter: due.Sub(now)}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	resp, err := r.Gateway.RunProbes(pctx, &gatewayv1.RunProbesRequest{Intent: intentRef(&in)})
	cancel()
	finish := r.now()
	next, skipped := nextRun(now, finish, interval)
	r.setDue(req.NamespacedName, next)
	switch {
	case status.Code(err) == codes.Aborted:
		skipped++ // a run for this Intent is still in flight: never overlap
	case err != nil:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Error(err, "Could not run probes")
	case resp.GetPlanHash() != in.Status.Plan.Hash:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Info("Gateway probed a plan other than the Intent's", "probed", resp.GetPlanHash(), "plan", in.Status.Plan.Hash)
	default:
		result := "passed"
		if len(failedProbes(resp.GetOutcomes())) > 0 {
			result = "failed"
		}
		probeRuns.WithLabelValues(in.Namespace, in.Name, result).Inc()
		if err := applyProbeOutcomes(ctx, r.Client, r.Reader, &in, resp.GetOutcomes(), now); err != nil {
			return ctrl.Result{}, err
		}
	}
	if skipped > 0 {
		probeRunsSkipped.WithLabelValues(in.Namespace, in.Name).Add(float64(skipped))
	}
	return ctrl.Result{RequeueAfter: next.Sub(finish)}, nil
}

func (r *ManagedResourceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ManagedResourceReconciler) due(key types.NamespacedName) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.next[key]
	return t, ok
}

func (r *ManagedResourceReconciler) setDue(key types.NamespacedName, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == nil {
		r.next = map[types.NamespacedName]time.Time{}
	}
	r.next[key] = t
}

func (r *ManagedResourceReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.next, key)
}

// interval resolves the namespace's fixed probe interval (spec §10.1).
func (r *ManagedResourceReconciler) interval(ctx context.Context, ns string) (time.Duration, error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default"}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return 0, err
	}
	var policies lastv1alpha1.IntentPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	return policy.ProbeInterval(op, policies.Items), nil
}

// lastProbe returns the newest probe time among the ManagedResources of the
// Intent's plan, or the zero time when none was probed. After a restart, the
// schedule resumes from it.
func (r *ManagedResourceReconciler) lastProbe(ctx context.Context, in *lastv1alpha1.Intent) (time.Time, error) {
	mrs, err := planManagedResources(ctx, r.Reader, in)
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, mr := range mrs {
		if t := mr.Status.LastProbeTime; t != nil && t.After(last) {
			last = t.Time
		}
	}
	return last, nil
}

func hasProbes(p *lastv1alpha1.Plan) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Resources {
		if len(r.Probes) > 0 {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager. It watches
// Intents: a phase change to Ready starts the schedule, and RequeueAfter keeps it.
func (r *ManagedResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("managedresource").
		Watches(&lastv1alpha1.Intent{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}
