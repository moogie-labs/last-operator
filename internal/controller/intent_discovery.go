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
			agent.LostTurn("the operator lost track of it while it ran; ask again if you still need it"))
	}
	if in.Generation != in.Status.ObservedGeneration || pc == nil {
		if err := r.endApprovedPhase(ctx, in); err != nil {
			return ctrl.Result{}, err
		}
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

// endApprovedPhase ends the pending command's phase, if it is approved, before
// a spec edit discards the command. An earlier attempt may have opened the phase
// and stopped before recording it; nothing else would ever end it, and the
// gateway opens no other phase for the Intent while it is open. An unapproved
// command never opened a phase.
func (r *IntentReconciler) endApprovedPhase(ctx context.Context, in *lastv1alpha1.Intent) error {
	pc := in.Status.PendingCommand
	if pc == nil {
		return nil
	}
	approved, _, err := r.decision(ctx, in, lastv1alpha1.StageDiscovery, pc.Hash)
	if err != nil || !approved {
		return err
	}
	err = r.endPhase(ctx, in, pc.Hash, false, "The Intent's spec changed before the discovery command ran")
	if status.Code(err) == codes.FailedPrecondition {
		return nil // no phase was open
	}
	return err
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
	case terminal(err):
		why = "the command could not run: " + describe(err)
	case err != nil:
		why = "the command's result was lost: " + describe(err)
	case !succeeded:
		why = stepFailure(plan.DiscoveryStepID, res)
	}
	if eerr := r.endPhase(ctx, in, subject, succeeded, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
		return eerr // still PhaseOpen: the next attempt reports the command as lost
	}
	switch {
	case terminal(err):
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.NotRunTurn(describe(err)))
	case err != nil:
		// The call failed in transit or timed out: the command may have run.
		return r.discovered(ctx, in, ReasonDiscoveryFailed, agent.LostTurn(describe(err)))
	case res.GetRunId() == "":
		// The gateway never started a run (its image build failed): there is no
		// command.result entry, so the turn carries the error and the log tail.
		return r.discovered(ctx, in, ReasonDiscoveryFailed, notRunTurn(res))
	}
	logf.FromContext(ctx).Info("Ran discovery command", "command", subject, "exitCode", res.GetExitCode())
	return r.discovered(ctx, in, ReasonDiscoveryRan, "Discovery command "+subject+" ran")
}

// notRunTurn reports a discovery command the gateway never started: its error
// and the tail of its stderr (a failed build's log), within maxTurnMessage.
func notRunTurn(res *gatewayv1.CommandResult) string {
	msg := agent.NotRunTurn(res.GetError())
	if res.GetStderr() == "" {
		return clipTo(msg, maxTurnMessage)
	}
	const label = "\nstderr (last bytes):\n"
	if room := maxTurnMessage - len(msg) - len(label) - len("…"); room > 0 {
		msg += label + tail(res.GetStderr(), room)
	}
	return clipTo(msg, maxTurnMessage)
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
		setExecutedTurn(in, s, reason, msg)
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM to continue")
	})
}
