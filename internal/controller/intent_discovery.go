package controller

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// awaitDiscovery waits for an IntentApproval of the pending discovery command
// and runs it as a one-step Discovery phase (M3 spec §3.4, §4.2). However it
// ends, with a result, a denial or a refusal, the model hears about it in
// Planning.
func (r *IntentReconciler) awaitDiscovery(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	pc := in.Status.PendingCommand
	if pc != nil && checkpoint(in) == ReasonPhaseOpen {
		// This process lost the running command, and a phase is never resumed.
		// End it before anything else, a spec edit included, so no phase stays open.
		if err := r.endPhase(ctx, in, pc.Hash, false, "The operator lost track of the running command"); err != nil &&
			status.Code(err) != codes.FailedPrecondition {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.discovered(ctx, in, ReasonDiscoveryFailed,
			agent.NotRunTurn("the operator lost track of it while it ran; ask again if you still need it"))
	}
	if in.Generation != in.Status.ObservedGeneration || pc == nil {
		return r.startPlanning(ctx, in, "")
	}
	approved, denial, err := r.decision(ctx, in, lastv1alpha1.StageDiscovery, pc.Hash)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case denial != nil:
		return ctrl.Result{}, r.discovered(ctx, in, ReasonDiscoveryDenied, agent.DeniedTurn(denial.Spec.Reason))
	case !approved:
		return ctrl.Result{}, nil // the IntentApproval watch wakes us
	}
	return ctrl.Result{}, r.runDiscovery(ctx, in)
}

// runDiscovery runs the approved command as a one-step Discovery phase. A
// command that exits non-zero still completes the phase: the model reads its
// result.
func (r *IntentReconciler) runDiscovery(ctx context.Context, in *lastv1alpha1.Intent) error {
	subject := in.Status.PendingCommand.Hash
	raw, err := json.Marshal(in.Status.PendingCommand.Command)
	if err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	_, err = r.Gateway.BeginPhase(bctx, &gatewayv1.BeginPhaseRequest{Intent: intentRef(in),
		Stage: string(lastv1alpha1.StageDiscovery), SubjectHash: subject})
	cancel()
	switch {
	case status.Code(err) == codes.AlreadyExists:
		const why = "a phase from an earlier attempt was still open"
		if eerr := r.endPhase(ctx, in, subject, false, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
			return eerr
		}
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn(why+"; ask again if you still need it"))
	case terminal(err):
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn("the gateway refused it: "+describe(err)))
	case err != nil:
		return err
	}
	logf.FromContext(ctx).Info("Opened discovery phase", "command", subject)
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonPhaseOpen, "Running discovery command "+subject)
	}); err != nil {
		return err // the next attempt finds the phase open (AlreadyExists) and ends it
	}
	res, err := r.runStep(ctx, in, subject, plan.DiscoveryStepID, raw)
	if ctx.Err() != nil {
		return ctx.Err() // shutting down; the next process ends the phase as lost
	}
	succeeded := err == nil && res.GetExitCode() == 0 && res.GetError() == ""
	why := ""
	switch {
	case err != nil:
		why = "the command could not run: " + describe(err)
	case !succeeded:
		why = stepFailure(plan.DiscoveryStepID, res)
	}
	if eerr := r.endPhase(ctx, in, subject, succeeded, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
		return eerr // still PhaseOpen: the next attempt reports the command as lost
	}
	if err != nil {
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn(describe(err)))
	}
	logf.FromContext(ctx).Info("Ran discovery command", "command", subject, "exitCode", res.GetExitCode())
	return r.discovered(ctx, in, ReasonDiscoveryRan, "Discovery command "+subject+" ran")
}

// discovered records how the pending discovery command ended in the Executed
// condition and returns the Intent to Planning, where pendingTurn tells the
// model. For ReasonDiscoveryDenied and ReasonDiscoveryFailed, msg is that turn.
func (r *IntentReconciler) discovered(ctx context.Context, in *lastv1alpha1.Intent, reason, msg string) error {
	subject := in.Status.PendingCommand.Hash
	s := metav1.ConditionFalse
	if reason == ReasonDiscoveryRan {
		s = metav1.ConditionTrue
	}
	return r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: record only the command that ended.
		if in.Status.Phase != lastv1alpha1.PhaseAwaitingDiscoveryApproval || in.Status.PendingCommand == nil ||
			in.Status.PendingCommand.Hash != subject {
			return
		}
		setExecuted(in, s, reason, msg)
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM to continue")
	})
}
