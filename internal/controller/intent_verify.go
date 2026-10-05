package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// verify runs every probe of the executed plan once; all must pass (spec §7).
// A failure here surfaces a bad probe at creation time instead of as drift.
func (r *IntentReconciler) verify(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Plan == nil {
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The Intent has no plan to verify")
	}
	start := time.Now()
	resp, err := r.Gateway.RunProbes(ctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	switch {
	case status.Code(err) == codes.Aborted:
		return ctrl.Result{RequeueAfter: r.RetryDelay}, nil // a probe run is in flight; never overlap
	case terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The gateway refused the probe run: "+describe(err))
	case err != nil:
		return ctrl.Result{}, err
	case resp.GetPlanHash() != in.Status.Plan.Hash:
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed,
			fmt.Sprintf("The gateway probed plan %s, not %s", resp.GetPlanHash(), in.Status.Plan.Hash))
	}
	if err := applyProbeOutcomes(ctx, r.Client, in, resp.GetOutcomes(), start); err != nil {
		return ctrl.Result{}, err
	}
	if failed := failedProbes(resp.GetOutcomes()); len(failed) > 0 {
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "Probes failed: "+strings.Join(failed, ", "))
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setPhase(in, lastv1alpha1.PhaseReady, ReasonReady, "Every probe passed")
	})
}

// followProbes moves a Ready Intent to Drifted when any of its ManagedResources
// is out of sync, and back when all are in sync again. Remediation arrives in M5.
func (r *IntentReconciler) followProbes(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	mrs, err := ownedManagedResources(ctx, r.Client, in)
	if err != nil {
		return ctrl.Result{}, err
	}
	var drifted []string
	for i := range mrs {
		if meta.IsStatusConditionFalse(mrs[i].Status.Conditions, CondInSync) {
			drifted = append(drifted, mrs[i].Name)
		}
	}
	switch {
	case len(drifted) > 0 && in.Status.Phase == lastv1alpha1.PhaseReady:
		return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
			setPhase(in, lastv1alpha1.PhaseDrifted, ReasonDrifted, "Probes failed on "+strings.Join(drifted, ", "))
		})
	case len(drifted) == 0 && in.Status.Phase == lastv1alpha1.PhaseDrifted:
		return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
			setPhase(in, lastv1alpha1.PhaseReady, ReasonReady, "Every probe passed")
		})
	}
	return ctrl.Result{}, nil
}
