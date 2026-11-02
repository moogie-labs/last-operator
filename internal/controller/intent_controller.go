package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// operatorConfigName is the OperatorConfig singleton.
const operatorConfigName = "default"

func (r *IntentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// IntentReconciler drives an Intent through its lifecycle (spec §7). It acts
// only by calling the gateway, the one component that can cause side effects
// (spec §3).
type IntentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Gateway  gatewayv1.GatewayServiceClient
	Planner  *agent.Planner
	Recorder events.EventRecorder
	// Reader reads uncached. Every phase decides from the Intent's status,
	// which the informer cache may not yet show after the previous reconcile's writes.
	Reader client.Reader
	// RetryDelay is the pause between retries of a gateway call within one reconcile.
	RetryDelay time.Duration
	// Now is the clock approvals age by and Auto retries wait by; nil means
	// time.Now. Tests inject one.
	Now func() time.Time
	// Suggestions creates rule suggestions for plans that wait for an approval
	// (M7 design §4.1); nil makes none.
	Suggestions *SuggestionCreator
	// Runner runs the RepeatedApproval scans a successful EndPhase queues
	// (M7 design §4.2); nil queues none.
	Runner *SuggestRunner

	// sessions holds planning conversations in memory (M3 spec §4.3).
	sessions sessionStore
}

// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intents,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intents/finalizers,verbs=update
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=intentapprovals,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=providers;intentpolicies;operatorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=commandrules;clustercommandrules,verbs=get;list;watch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=commandrulesuggestions,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=last.moogie-labs.com,resources=managedresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile advances the Intent by one phase. Every status write triggers the
// next reconcile, so each phase handler does its work and records the result.
func (r *IntentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Read uncached: a stale read could re-plan, reopen a phase, or apply a
	// phase change over a newer status.
	var in lastv1alpha1.Intent
	if err := r.Reader.Get(ctx, req.NamespacedName, &in); err != nil {
		if apierrors.IsNotFound(err) {
			r.sessions.drop(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !in.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&in, lastv1alpha1.FinalizerTeardown) {
			return ctrl.Result{}, nil // nothing holds it: the API server removes it
		}
		return r.tearDown(ctx, &in)
	}
	switch in.Status.Phase {
	case "", lastv1alpha1.PhasePending:
		return r.startPlanning(ctx, &in, "")
	case lastv1alpha1.PhasePlanning:
		return r.plan(ctx, &in)
	case lastv1alpha1.PhaseClarifying:
		return r.clarify(ctx, &in)
	case lastv1alpha1.PhaseAwaitingDiscoveryApproval:
		return r.awaitDiscovery(ctx, &in)
	case lastv1alpha1.PhaseAwaitingApproval:
		return r.awaitApproval(ctx, &in)
	case lastv1alpha1.PhaseExecuting, lastv1alpha1.PhaseRemediating:
		return r.execute(ctx, &in)
	case lastv1alpha1.PhaseVerifying:
		return r.verify(ctx, &in)
	case lastv1alpha1.PhaseReady, lastv1alpha1.PhaseDrifted:
		if in.Generation != in.Status.ObservedGeneration {
			return r.startPlanning(ctx, &in, "") // a spec edit plans an Update
		}
		return r.followProbes(ctx, &in)
	case lastv1alpha1.PhaseFailed:
		if in.Generation != in.Status.ObservedGeneration {
			return r.startPlanning(ctx, &in, "") // a spec edit plans the failed Intent afresh
		}
		return r.retryFailed(ctx, &in)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. The Intent watch
// has no predicate, so a metadata change such as the retry annotation wakes it.
func (r *IntentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lastv1alpha1.Intent{}).
		Owns(&lastv1alpha1.ManagedResource{}).
		Watches(&lastv1alpha1.IntentApproval{}, handler.EnqueueRequestsFromMapFunc(approvedIntent)).
		Named("intent").
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}

// approvedIntent maps an IntentApproval to the Intent it names.
func approvedIntent(_ context.Context, o client.Object) []reconcile.Request {
	a, ok := o.(*lastv1alpha1.IntentApproval)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Spec.IntentRef.Name}}}
}
