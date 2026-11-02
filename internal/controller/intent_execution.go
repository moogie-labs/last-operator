package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	// stepTimeout bounds one RunCommand call, including the wait for a sandbox slot.
	// It covers the longest image build (images.MaxBuildTimeout, 30m) plus the command.
	stepTimeout = 60 * time.Minute
	// phaseCallTimeout bounds one BeginPhase, EndPhase or WriteOutputSecret
	// call, so a stuck gateway never holds a worker.
	phaseCallTimeout = time.Minute
	// retries bounds the attempts of one gateway call within a reconcile.
	retries = 5
)

// checkpoint returns the Executed condition's reason: how far the current
// plan's phase got.
func checkpoint(in *lastv1alpha1.Intent) string {
	if c := meta.FindStatusCondition(in.Status.Conditions, CondExecuted); c != nil {
		return c.Reason
	}
	return ""
}

func setExecuted(in *lastv1alpha1.Intent, s metav1.ConditionStatus, reason, msg string) {
	setCondition(&in.Status.Conditions, in.Generation, CondExecuted, s, reason, msg)
}

// execute runs the approved plan (spec §8.3): a Create or Update plan in phase
// Executing, a Remediate plan in phase Remediating, or a Delete plan in phase
// TearingDown. Step results live only in this process, so the Executed
// checkpoint decides what a reconcile may do: open the phase, abandon one this
// process lost, or confirm one whose steps all succeeded. A phase is never
// resumed. A spec edit abandons a Remediate that has not run its steps.
func (r *IntentReconciler) execute(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Plan == nil {
		return ctrl.Result{}, r.failRun(ctx, in, ReasonExecutionFailed, "The Intent has no plan to run")
	}
	stage := planStage(in)
	if stage == lastv1alpha1.StageRemediate && in.Generation != in.Status.ObservedGeneration && checkpoint(in) != ReasonStepsSucceeded {
		return r.abandonRemediation(ctx, in)
	}
	switch checkpoint(in) {
	case ReasonStepsSucceeded:
		return r.finishPhase(ctx, in)
	case ReasonPhaseOpen:
		return r.abandonPhase(ctx, in, "The operator lost track of the running phase")
	case ReasonPhaseEnded:
		if stage == lastv1alpha1.StageDelete {
			return ctrl.Result{}, r.release(ctx, in, "Teardown succeeded") // released before this process could
		}
	}
	if stage == lastv1alpha1.StageCreate {
		// startPlanning added it; an Intent planned by an older operator may lack it.
		if err := r.ensureFinalizer(ctx, in); err != nil {
			return ctrl.Result{}, err
		}
	}
	bctx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	_, err := r.Gateway.BeginPhase(bctx, &gatewayv1.BeginPhaseRequest{Intent: intentRef(in),
		Stage: string(stage), SubjectHash: in.Status.Plan.Hash})
	cancel()
	switch {
	case tornDown(in, err):
		return ctrl.Result{}, r.release(ctx, in, msgTornDown)
	case gatewayv1.NeedsApproval(err), autoRun(in) && gatewayv1.NoApproval(err):
		return r.needsApproval(ctx, in, err)
	case gatewayv1.Denied(err):
		return ctrl.Result{}, r.failDenied(ctx, in, "The gateway refused to open the phase: "+describe(err))
	case status.Code(err) == codes.AlreadyExists:
		return r.abandonPhase(ctx, in, "A phase from an earlier attempt was still open")
	case terminal(err):
		return ctrl.Result{}, r.failRun(ctx, in, ReasonExecutionFailed, "The gateway refused to open the phase: "+describe(err))
	case err != nil:
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Opened phase", "plan", in.Status.Plan.Hash, "stage", stage)
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonPhaseOpen, "Running plan "+in.Status.Plan.Hash)
	}); err != nil {
		return ctrl.Result{}, err // the next attempt finds the phase open (AlreadyExists) and abandons it
	}
	return r.runSteps(ctx, in)
}

// needsApproval handles the gateway's refusal to open a phase without an
// approval: an Auto stage's plan outside Auto mode's limits (design D9,
// gatewayv1.NeedsApproval), or a plan or command record sent to run that the
// gateway finds neither Auto nor covered by allow rules (gatewayv1.NoApproval,
// which callers pass only for what record sent to run without an approval): a
// stage that turned Manual after record found it Auto, or rules the gateway
// reads differently, or that changed since (M6 design R9). Nothing ran.
//
// What record sent to run without an approval waits for an IntentApproval: a
// plan moves to AwaitingApproval, and a discovery command stays in
// AwaitingDiscoveryApproval without ReasonRunningDiscovery. From there the
// Intent waits as in Manual mode, even though its own modes say Auto or its
// own read of the rules covers it: only record skips the wait, so the
// operator never asks the gateway again unapproved.
//
// What an approval sent to run stays where it is, and the gateway is asked
// again after RetryDelay: the operator saw an Approve that the gateway does
// not see yet, and moving to AwaitingApproval would bounce straight back.
func (r *IntentReconciler) needsApproval(ctx context.Context, in *lastv1alpha1.Intent, err error) (ctrl.Result, error) {
	from, subject := in.Status.Phase, subjectOf(in)
	log := logf.FromContext(ctx)
	if (from == lastv1alpha1.PhaseAwaitingDiscoveryApproval && !autoDiscovery(in)) ||
		(from != lastv1alpha1.PhaseAwaitingDiscoveryApproval && !autoRun(in)) {
		log.Info("Gateway needs an IntentApproval that the operator sees; retrying", "subject", subject,
			"stage", in.Status.Stage, "reason", describe(err), "after", r.RetryDelay)
		return ctrl.Result{RequeueAfter: r.RetryDelay}, nil
	}
	to, reason, noun := lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval, "plan"
	if from == lastv1alpha1.PhaseAwaitingDiscoveryApproval {
		to, reason, noun = from, ReasonAwaitingDiscoveryApproval, "command"
	}
	log.Info("Waiting for the IntentApproval the gateway needs", "subject", subject, "stage", in.Status.Stage)
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: move only the subject the gateway refused.
		if in.Status.Phase != from || subjectOf(in) != subject {
			return
		}
		setPhase(in, to, reason, "The gateway needs an approval for this "+noun+": "+describe(err))
	})
}

// subjectOf returns what the Intent's phase is about: the pending discovery
// command in AwaitingDiscoveryApproval, the plan otherwise, or "" for neither.
func subjectOf(in *lastv1alpha1.Intent) string {
	if in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval {
		if pc := in.Status.PendingCommand; pc != nil {
			return pc.Hash
		}
		return ""
	}
	if p := in.Status.Plan; p != nil {
		return p.Hash
	}
	return ""
}

// runSteps runs every step in order, resolving ${steps…} references from the
// results so far, then creates the ManagedResources and writes their outputs.
// A Remediate does neither: what is managed, and its outputs, do not change.
func (r *IntentReconciler) runSteps(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	results := plan.Results{}
	for _, step := range in.Status.Plan.Steps {
		cmd, err := plan.Resolve(step, results)
		if err != nil {
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %v", step.ID, err), true)
		}
		raw, err := json.Marshal(cmd)
		if err != nil {
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %v", step.ID, err), false)
		}
		res, err := r.runStep(ctx, in, in.Status.Plan.Hash, step.ID, raw)
		switch {
		case ctx.Err() != nil:
			return ctrl.Result{}, ctx.Err() // shutting down; the next process abandons the phase
		case tornDown(in, err):
			return ctrl.Result{}, r.release(ctx, in, msgTornDown)
		case gatewayv1.Denied(err):
			return r.endDenied(ctx, in, fmt.Sprintf("step %s: %s", step.ID, describe(err)))
		case err != nil:
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %s", step.ID, describe(err)), false)
		case res.GetExitCode() != 0 || res.GetError() != "":
			return r.endFailed(ctx, in, stepFailure(step.ID, res), true)
		}
		results[step.ID] = runnerapi.Result{ID: step.ID, Stdout: res.GetStdout(), StdoutTruncated: res.GetStdoutTruncated()}
	}
	if planStage(in) != lastv1alpha1.StageRemediate {
		if err := r.ensureManagedResources(ctx, in); err != nil {
			return r.endFailed(ctx, in, "Could not create ManagedResources: "+err.Error(), false)
		}
		if err := r.writeOutputs(ctx, in); err != nil {
			return r.endFailed(ctx, in, "Could not write an output Secret: "+err.Error(), false)
		}
	}
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonStepsSucceeded, "Every step succeeded; closing the phase")
	}); err != nil {
		return ctrl.Result{}, err
	}
	return r.finishPhase(ctx, in)
}

// runStep calls RunCommand, retrying while another call of the phase holds it
// (Aborted) or the gateway is unavailable (Unavailable: it keeps the phase open
// when, say, the sandbox image could not be resolved). The gateway refuses a
// retry (FailedPrecondition) once an earlier attempt ran or failed the step, so
// that refusal after an Unavailable means the earlier attempt's outcome was
// lost, and its Unavailable error is returned instead.
func (r *IntentReconciler) runStep(ctx context.Context, in *lastv1alpha1.Intent, subject, id string, raw []byte) (*gatewayv1.CommandResult, error) {
	req := &gatewayv1.RunCommandRequest{Intent: intentRef(in), SubjectHash: subject, StepId: id, CommandJson: raw}
	var unavailable error
	for attempt := 1; ; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, stepTimeout)
		resp, err := r.Gateway.RunCommand(sctx, req)
		cancel()
		code := status.Code(err)
		if code == codes.FailedPrecondition && unavailable != nil {
			return nil, unavailable
		}
		if code == codes.Unavailable {
			unavailable = err
		}
		if (code != codes.Aborted && code != codes.Unavailable) || attempt == retries {
			return resp.GetResult(), err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return nil, err
		}
	}
}

// failureTail bounds the stderr a step failure carries into the replanning
// conversation, a failed build's log included: the feedback is a condition
// message, clipped to maxMessage (1 KiB).
const failureTail = 512

// stepFailure describes a failed step for the replanning conversation.
func stepFailure(id string, res *gatewayv1.CommandResult) string {
	if res.GetError() != "" {
		why := fmt.Sprintf("step %s could not run: %s", id, res.GetError())
		if res.GetStderr() != "" {
			why += "; stderr: " + tail(res.GetStderr(), failureTail)
		}
		return why
	}
	return fmt.Sprintf("step %s exited %d: %s", id, res.GetExitCode(), tail(res.GetStderr(), failureTail))
}

// tail returns the last n bytes of s, cut on a UTF-8 boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// writeOutputs writes each resource's declared outputs (spec §15).
func (r *IntentReconciler) writeOutputs(ctx context.Context, in *lastv1alpha1.Intent) error {
	for i, res := range in.Status.Plan.Resources {
		if len(res.Outputs) == 0 {
			continue
		}
		name := lastv1alpha1.ManagedResourceName(in.Name, i)
		req := &gatewayv1.WriteOutputSecretRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash,
			ManagedResource: name, Values: res.Outputs}
		for attempt := 1; ; attempt++ {
			wctx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
			_, err := r.Gateway.WriteOutputSecret(wctx, req)
			cancel()
			if err == nil {
				break
			}
			if status.Code(err) != codes.Aborted || attempt == retries {
				return fmt.Errorf("%s: %s", name, describe(err))
			}
			if err := sleep(ctx, r.RetryDelay); err != nil {
				return err
			}
		}
	}
	return nil
}

// endPhase calls EndPhase with the same outcome until it is recorded, retrying
// while the gateway is unavailable (it treats a repeated EndPhase as a retry of
// its record save) or a step of the phase still holds it (Aborted). A phase
// recorded as succeeded queues a RepeatedApproval scan of the namespace (M7
// design §4.2); the scan itself keeps only the commands an IntentApproval
// authorized.
func (r *IntentReconciler) endPhase(ctx context.Context, in *lastv1alpha1.Intent, subject string, succeeded bool, reason string) error {
	req := &gatewayv1.EndPhaseRequest{Intent: intentRef(in), SubjectHash: subject, Succeeded: succeeded, Reason: reason}
	for attempt := 1; ; attempt++ {
		ectx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
		_, err := r.Gateway.EndPhase(ectx, req)
		cancel()
		if succeeded && err == nil {
			r.Runner.Queue(in.Namespace)
		}
		if code := status.Code(err); (code != codes.Unavailable && code != codes.Aborted) || attempt == retries {
			return err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return err
		}
	}
}

// endFailed closes the phase as failed. A failure of a Create, Update or
// Remediate plan itself returns the Intent to Planning in the same stage, with
// the failure as context (spec §8.3), counted in status.replans; after
// maxReplans replans in a row it fails the Intent with PlanningFailed instead
// (D10). Anything else, and any failed teardown, fails the Intent. A Remediate
// replan is the same drift episode: it is not counted as a remediation again.
func (r *IntentReconciler) endFailed(ctx context.Context, in *lastv1alpha1.Intent, reason string, replan bool) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Ending phase as failed", "plan", in.Status.Plan.Hash, "reason", clip(reason))
	err := r.endPhase(ctx, in, in.Status.Plan.Hash, false, reason)
	switch {
	case tornDown(in, err):
		return ctrl.Result{}, r.release(ctx, in, msgTornDown)
	case err != nil && status.Code(err) != codes.FailedPrecondition:
		return ctrl.Result{}, err // still PhaseOpen: the next attempt abandons the phase
	}
	if stage := planStage(in); replan && replannable(stage) {
		if in.Status.Replans >= maxReplans {
			return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed,
				fmt.Sprintf("The plan failed after %d replans; last failure: %s%s", maxReplans, reason, specEditHint(in)))
		}
		return r.startSession(ctx, in, stage, reason, causeReplan) // the failed phase left the executed plan as it was
	}
	return ctrl.Result{}, r.failRun(ctx, in, ReasonExecutionFailed, reason)
}

// endDenied closes the phase as failed for a step the gateway refused because
// an IntentApproval denies the plan (gatewayv1.Denied), then fails the Intent
// as denied (failDenied).
func (r *IntentReconciler) endDenied(ctx context.Context, in *lastv1alpha1.Intent, reason string) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Ending denied phase as failed", "plan", in.Status.Plan.Hash, "reason", clip(reason))
	err := r.endPhase(ctx, in, in.Status.Plan.Hash, false, reason)
	switch {
	case tornDown(in, err):
		return ctrl.Result{}, r.release(ctx, in, msgTornDown)
	case err != nil && status.Code(err) != codes.FailedPrecondition:
		return ctrl.Result{}, err // still PhaseOpen: the next attempt abandons the phase
	}
	return ctrl.Result{}, r.failDenied(ctx, in, reason)
}

// failDenied fails the Intent with ApprovalDenied for a gateway refusal that a
// Deny caused, at BeginPhase or at a step (gatewayv1.Denied), as a Deny seen
// in AwaitingApproval does. It is never ExecutionFailed: a denied plan never
// retries by itself, whatever the stage's mode (D10). A teardown keeps the
// finalizer, and the message says how to plan it again.
func (r *IntentReconciler) failDenied(ctx context.Context, in *lastv1alpha1.Intent, msg string) error {
	if planStage(in) == lastv1alpha1.StageDelete {
		msg += "; " + retryTeardownHint
	}
	return r.fail(ctx, in, ReasonApprovalDenied, msg)
}

// specEditHint tells the user how to plan again after a failed Create, Update
// or Remediate; a teardown has the retry annotation instead (failRun).
func specEditHint(in *lastv1alpha1.Intent) string {
	if planStage(in) == lastv1alpha1.StageDelete {
		return ""
	}
	return "; edit the Intent's spec to plan again"
}

// abandonPhase closes a phase this process cannot continue and fails the Intent.
func (r *IntentReconciler) abandonPhase(ctx context.Context, in *lastv1alpha1.Intent, why string) (ctrl.Result, error) {
	err := r.endPhase(ctx, in, in.Status.Plan.Hash, false, why)
	switch {
	case tornDown(in, err):
		return ctrl.Result{}, r.release(ctx, in, msgTornDown)
	case err != nil && status.Code(err) != codes.FailedPrecondition:
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.failRun(ctx, in, ReasonInterrupted, why+specEditHint(in))
}

// finishPhase closes a phase whose steps all succeeded. If the gateway refuses
// (FailedPrecondition), an earlier EndPhase may have succeeded before its
// result was recorded here, a gateway restart may have interrupted the phase,
// or the gateway may hold the phase open but refuse to let it succeed
// (succeededBefore says which). Unless the phase succeeded, it is ended as
// failed, in case it is still open.
//
// A Delete phase has no executed plan to compare: a refusal fails the teardown.
// Once the gateway records it as succeeded, the Intent is released.
func (r *IntentReconciler) finishPhase(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	err := r.endPhase(ctx, in, in.Status.Plan.Hash, true, "")
	if planStage(in) == lastv1alpha1.StageDelete {
		return r.finishTeardown(ctx, in, err)
	}
	if status.Code(err) == codes.FailedPrecondition {
		succeeded, perr := r.succeededBefore(ctx, in)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		if !succeeded {
			why := "The phase ended before it succeeded (" + describe(err) + ")"
			if eerr := r.endPhase(ctx, in, in.Status.Plan.Hash, false, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
				return ctrl.Result{}, eerr // still StepsSucceeded: the next attempt asks again
			}
			return ctrl.Result{}, r.fail(ctx, in, ReasonInterrupted, why+specEditHint(in))
		}
		err = nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Executed plan", "plan", in.Status.Plan.Hash)
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionTrue, ReasonPhaseEnded, "Plan "+in.Status.Plan.Hash+" executed")
		setPhase(in, lastv1alpha1.PhaseVerifying, ReasonVerifying, "Running every probe once")
	})
}

// finishTeardown records how ending a Delete phase as succeeded went (err is
// EndPhase's answer) and releases the Intent once the gateway recorded it. The
// finalizer is never removed while the phase may still be open. A torn-down
// refusal means an earlier EndPhase succeeded and its answer was lost.
func (r *IntentReconciler) finishTeardown(ctx context.Context, in *lastv1alpha1.Intent, err error) (ctrl.Result, error) {
	if tornDown(in, err) {
		err = nil
	}
	if status.Code(err) == codes.FailedPrecondition {
		why := "The teardown phase ended before it succeeded (" + describe(err) + ")"
		if eerr := r.endPhase(ctx, in, in.Status.Plan.Hash, false, why); eerr != nil && status.Code(eerr) != codes.FailedPrecondition {
			return ctrl.Result{}, eerr // still StepsSucceeded: the next attempt asks again
		}
		return ctrl.Result{}, r.failRun(ctx, in, ReasonInterrupted, why)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Executed teardown plan", "plan", in.Status.Plan.Hash)
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionTrue, ReasonPhaseEnded, "Teardown plan "+in.Status.Plan.Hash+" executed")
	}); err != nil {
		return ctrl.Result{}, err // the next attempt's EndPhase is refused as torn down, which releases the Intent
	}
	return ctrl.Result{}, r.release(ctx, in, "Teardown succeeded")
}

// succeededBefore reports whether the gateway recorded the Intent's current
// phase as succeeded. RunProbes reports which plan it last executed, which a
// succeeded Create or Update plan becomes. A Remediate never becomes the
// executed plan, so the audit log says instead (agent.PhaseSucceeded); a
// gateway that cannot read it cannot vouch for the phase.
func (r *IntentReconciler) succeededBefore(ctx context.Context, in *lastv1alpha1.Intent) (bool, error) {
	if planStage(in) != lastv1alpha1.StageRemediate {
		executed, err := r.executedPlan(ctx, in)
		return executed == in.Status.Plan.Hash, err
	}
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	defer cancel()
	ok, err := agent.PhaseSucceeded(actx, r.Gateway, intentRef(in), lastv1alpha1.StageRemediate, in.Status.Plan.Hash)
	if terminal(err) {
		return false, nil
	}
	return ok, err
}

// executedPlan returns the hash of the plan the gateway last executed for the
// Intent, or "" when it has none.
func (r *IntentReconciler) executedPlan(ctx context.Context, in *lastv1alpha1.Intent) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	resp, err := r.Gateway.RunProbes(pctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	switch {
	case err == nil:
		return resp.GetPlanHash(), nil
	case terminal(err):
		return "", nil
	default:
		return "", err
	}
}
