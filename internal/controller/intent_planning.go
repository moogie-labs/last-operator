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
		// update left the rejected plan on in; start the Failed write from the stored status
		if gerr := r.Get(ctx, client.ObjectKeyFromObject(in), in); gerr != nil {
			return ctrl.Result{}, gerr
		}
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, "The API server rejected the plan: "+err.Error())
	}
	return ctrl.Result{}, err
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
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+in.Status.Plan.Hash)
	})
}
