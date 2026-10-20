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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
)

// verify runs every probe of the executed plan once; all must pass (spec §7).
// A failure here surfaces a bad probe at creation time instead of as drift.
// Reaching Ready resets status.replans and status.retries (D10).
// After a Remediate, the probed plan is the executed plan it repaired, and a
// failure is drift that persists (D11): the Intent goes Drifted, so the next
// remediation is counted, or the cap fails it with RemediationFailed.
func (r *IntentReconciler) verify(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Plan == nil {
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The Intent has no plan to verify")
	}
	want, err := probedPlanHash(ctx, r.Gateway, in)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case want == "":
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The audit log holds no executed plan to verify the remediation against")
	}
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	resp, err := r.Gateway.RunProbes(pctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	cancel()
	switch {
	case status.Code(err) == codes.Aborted:
		return ctrl.Result{RequeueAfter: r.RetryDelay}, nil // a probe run is in flight; never overlap
	case terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "The gateway refused the probe run: "+describe(err))
	case err != nil:
		return ctrl.Result{}, err
	case resp.GetPlanHash() != want:
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed,
			fmt.Sprintf("The gateway probed plan %s, not %s", resp.GetPlanHash(), want))
	}
	if err := applyProbeOutcomes(ctx, r.Client, r.Reader, in, resp.GetOutcomes(), start); err != nil {
		return ctrl.Result{}, err
	}
	if failed := failedProbes(resp.GetOutcomes()); len(failed) > 0 {
		if planStage(in) == lastv1alpha1.StageRemediate {
			return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
				if in.Status.Phase == lastv1alpha1.PhaseVerifying {
					setPhase(in, lastv1alpha1.PhaseDrifted, ReasonDrifted, "Probes failed after the remediation: "+strings.Join(failed, ", "))
				}
			})
		}
		return ctrl.Result{}, r.fail(ctx, in, ReasonVerificationFailed, "Probes failed: "+strings.Join(failed, ", "))
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Replans, in.Status.Retries = 0, 0 // the execution succeeded (D10)
		setPhase(in, lastv1alpha1.PhaseReady, ReasonReady, "Every probe passed")
	})
}

// followProbes moves a Ready Intent to Drifted when any ManagedResource of its
// plan is out of sync, and starts a remediation for a Drifted one
// (startRemediate); a spec edit has already planned an Update instead.
// ManagedResources an earlier plan left behind do not count. A scheduled probe
// run that passed everywhere since the Intent became Ready resets
// status.remediations (D11).
func (r *IntentReconciler) followProbes(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	mrs, err := planManagedResources(ctx, r.Reader, in)
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
	case in.Status.Phase == lastv1alpha1.PhaseDrifted:
		return r.startRemediate(ctx, in)
	case len(drifted) > 0:
		return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
			setPhase(in, lastv1alpha1.PhaseDrifted, ReasonDrifted, "Probes failed on "+strings.Join(drifted, ", "))
		})
	case in.Status.Remediations > 0 && probedSinceReady(in, mrs):
		logf.FromContext(ctx).Info("Reset the remediation count", "remediations", in.Status.Remediations)
		return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
			if in.Status.Phase == lastv1alpha1.PhaseReady {
				in.Status.Remediations = 0
			}
		})
	}
	return ctrl.Result{}, nil
}
