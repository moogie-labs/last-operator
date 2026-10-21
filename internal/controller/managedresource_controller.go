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
// It covers the longest image build (images.MaxBuildTimeout, 30m) plus the probe.
const probeTimeout = 45 * time.Minute

// ManagedResourceReconciler runs each Ready or Drifted Intent's probes on a
// adaptive, jittered interval and records the outcomes on its ManagedResources (spec §10,
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
	// Jitter returns the offset, a fraction in [-0.1, 0.1] of the interval, to
	// schedule the next run at; nil means a uniform random value.
	Jitter func() float64

	mu        sync.Mutex
	schedules map[types.NamespacedName]probeSchedule // each Intent's probe schedule
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
	fast, steady, err := r.intervals(ctx, in.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	sched, ok := r.schedule(req.NamespacedName)
	if !ok || sched.planHash != in.Status.Plan.Hash {
		// First sight, a restart, or a new plan: probe often again, resuming
		// from the newest recorded probe.
		last, err := r.lastProbe(ctx, &in)
		if err != nil {
			return ctrl.Result{}, err
		}
		sched = probeSchedule{interval: fast, planHash: in.Status.Plan.Hash, next: last.Add(fast)}
		r.setSchedule(req.NamespacedName, sched)
	}
	if now.Before(sched.next) {
		return ctrl.Result{RequeueAfter: sched.next.Sub(now)}, nil
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	resp, err := r.Gateway.RunProbes(pctx, &gatewayv1.RunProbesRequest{Intent: intentRef(&in)})
	cancel()
	finish := r.now()
	result := runFailed
	var recordErr error
	switch {
	case status.Code(err) == codes.Aborted:
		result = runAborted // a run for this Intent is still in flight: never overlap
	case err != nil:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Error(err, "Could not run probes")
	default:
		result, recordErr = r.record(ctx, &in, resp, now)
	}
	next, skipped := sched.after(now, finish, result, fast, steady, r.jitter())
	if result == runAborted {
		skipped++
	}
	r.setSchedule(req.NamespacedName, next)
	probeInterval.WithLabelValues(in.Namespace, in.Name).Set(next.interval.Seconds())
	if skipped > 0 {
		probeRunsSkipped.WithLabelValues(in.Namespace, in.Name).Add(float64(skipped))
	}
	if recordErr != nil {
		return ctrl.Result{}, recordErr
	}
	return ctrl.Result{RequeueAfter: next.next.Sub(finish)}, nil
}

// record records a probe run on the Intent's ManagedResources, once it is
// clear the gateway probed the plan they belong to: the Intent's plan, or the
// executed plan a Remediate plan repaired (probedPlanHash).
//
// It returns runPassed when the run was recorded and every probe passed, and
// runFailed when a probe failed or the run could not be recorded.
func (r *ManagedResourceReconciler) record(ctx context.Context, in *lastv1alpha1.Intent, resp *gatewayv1.RunProbesResponse, at time.Time) (runResult, error) {
	log := logf.FromContext(ctx)
	want, err := probedPlanHash(ctx, r.Gateway, in)
	result := runFailed
	switch {
	case err != nil:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Error(err, "Could not read the executed plan to record probes against")
	case resp.GetPlanHash() != want:
		probeRuns.WithLabelValues(in.Namespace, in.Name, "error").Inc()
		log.Info("Gateway probed a plan other than the Intent's", "probed", resp.GetPlanHash(), "plan", want)
	default:
		label := "passed"
		if len(failedProbes(resp.GetOutcomes())) > 0 {
			label = "failed"
		} else {
			result = runPassed
		}
		probeRuns.WithLabelValues(in.Namespace, in.Name, label).Inc()
		return result, applyProbeOutcomes(ctx, r.Client, r.Reader, in, resp.GetOutcomes(), at)
	}
	return result, nil
}

func (r *ManagedResourceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ManagedResourceReconciler) jitter() float64 {
	if r.Jitter != nil {
		return r.Jitter()
	}
	return randomJitter()
}

func (r *ManagedResourceReconciler) schedule(key types.NamespacedName) (probeSchedule, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.schedules[key]
	return s, ok
}

func (r *ManagedResourceReconciler) setSchedule(key types.NamespacedName, s probeSchedule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.schedules == nil {
		r.schedules = map[types.NamespacedName]probeSchedule{}
	}
	r.schedules[key] = s
}

func (r *ManagedResourceReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.schedules, key)
	probeInterval.DeleteLabelValues(key.Namespace, key.Name)
}

// intervals resolves the namespace's fast and steady probe intervals (spec §10.1).
func (r *ManagedResourceReconciler) intervals(ctx context.Context, ns string) (fast, steady time.Duration, err error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: operatorConfigName}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return 0, 0, err
	}
	var policies lastv1alpha1.IntentPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(ns)); err != nil {
		return 0, 0, err
	}
	fast, steady = policy.ProbeIntervals(op, policies.Items)
	return fast, steady, nil
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
