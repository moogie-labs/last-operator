package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// planTimeout bounds one planning conversation.
const planTimeout = 10 * time.Minute

// startPlanning discards the current plan and asks for a new one for the
// current spec. feedback, when set, is a failure the new plan must address.
func (r *IntentReconciler) startPlanning(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = lastv1alpha1.StageCreate
		in.Status.ObservedGeneration = in.Generation
		in.Status.Plan = nil
		meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
		if feedback == "" {
			meta.RemoveStatusCondition(&in.Status.Conditions, CondPlanned)
		} else {
			setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionFalse, ReasonReplanning, feedback)
		}
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM for a plan")
	})
}

// plan runs the planning conversation (spec §7) and records the plan for approval.
func (r *IntentReconciler) plan(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration {
		return r.startPlanning(ctx, in, "")
	}
	feedback := ""
	if c := meta.FindStatusCondition(in.Status.Conditions, CondPlanned); c != nil && c.Reason == ReasonReplanning {
		feedback = c.Message
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	p, err := r.Planner.Plan(pctx, agent.Request{Namespace: in.Namespace, Name: in.Name, Text: in.Spec.Request,
		Answers: in.Spec.Answers, Feedback: feedback})
	if err != nil {
		if errors.Is(err, agent.ErrNoPlan) || terminal(err) {
			return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err))
		}
		return ctrl.Result{}, err // transient: retried with backoff
	}
	gen := in.Generation
	err = r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		if in.Generation != gen || in.Status.Phase != lastv1alpha1.PhasePlanning {
			return // the spec changed while planning; the next reconcile plans again
		}
		in.Status.Plan = p
		setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionTrue, ReasonPlanWritten, "Plan "+p.Hash)
		setPhase(in, lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval,
			"Waiting for an IntentApproval of plan "+p.Hash+" for stage Create")
	})
	if apierrors.IsInvalid(err) {
		// update left the rejected plan on in; start the Failed write from the
		// stored status, read uncached into a fresh object so nothing stale survives
		var fresh lastv1alpha1.Intent
		if gerr := r.Reader.Get(ctx, client.ObjectKeyFromObject(in), &fresh); gerr != nil {
			return ctrl.Result{}, gerr
		}
		*in = fresh
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, "The API server rejected the plan: "+err.Error())
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if storedPlanChanged(in, p.Hash) {
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, msgPlanChanged)
	}
	return ctrl.Result{}, nil
}

// msgPlanChanged explains a plan the API server did not store as written.
const msgPlanChanged = "The API server changed the plan when storing it; are the CRDs current?"

// storedPlanChanged reports whether in holds the plan hashed as hash, but the
// stored plan no longer hashes to it. An API server with a stale CRD prunes
// unknown fields silently, and the gateway would later refuse the plan with a
// hash mismatch.
func storedPlanChanged(in *lastv1alpha1.Intent, hash string) bool {
	if in.Status.Plan == nil || in.Status.Plan.Hash != hash {
		return false // the write was skipped; the spec changed while planning
	}
	got, err := plan.Hash(plan.FromStatus(in.Status.Plan))
	return err != nil || got != hash
}

// awaitApproval waits for an IntentApproval of the current plan. The gateway
// re-checks every approval itself (spec §8.3); this only decides when to try.
// A Deny wins over any Approve, as in the gateway.
func (r *IntentReconciler) awaitApproval(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Generation != in.Status.ObservedGeneration || in.Status.Plan == nil {
		return r.startPlanning(ctx, in, "")
	}
	var list lastv1alpha1.IntentApprovalList
	if err := r.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	approved := false
	for _, a := range list.Items {
		s := a.Spec
		if s.IntentRef.Name != in.Name || s.Stage != lastv1alpha1.StageCreate || s.SubjectHash != in.Status.Plan.Hash {
			continue
		}
		if s.Decision == lastv1alpha1.DecisionDeny {
			return ctrl.Result{}, r.fail(ctx, in, ReasonApprovalDenied, fmt.Sprintf("IntentApproval %s denied the plan: %s", a.Name, s.Reason))
		}
		approved = true
	}
	if !approved {
		return ctrl.Result{}, nil // the IntentApproval watch wakes us
	}
	gen, subject := in.Generation, in.Status.Plan.Hash
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: run only the plan that was approved.
		if in.Generation != gen || in.Status.Phase != lastv1alpha1.PhaseAwaitingApproval ||
			in.Status.Plan == nil || in.Status.Plan.Hash != subject {
			return
		}
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+in.Status.Plan.Hash)
	})
}
