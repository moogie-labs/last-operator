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
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/sandbox/runnerapi"
)

const (
	// stepTimeout bounds one RunCommand call, including the wait for a sandbox slot.
	stepTimeout = 30 * time.Minute
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

// execute runs the approved plan (spec §8.3). Step results live only in this
// process, so the Executed checkpoint decides what a reconcile may do: open
// the phase, abandon one this process lost, or confirm one whose steps all
// succeeded. A phase is never resumed.
func (r *IntentReconciler) execute(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Plan == nil {
		return ctrl.Result{}, r.fail(ctx, in, ReasonExecutionFailed, "The Intent has no plan to run")
	}
	switch checkpoint(in) {
	case ReasonStepsSucceeded:
		return r.finishPhase(ctx, in)
	case ReasonPhaseOpen:
		return r.abandonPhase(ctx, in, "The operator lost track of the running phase")
	}
	_, err := r.Gateway.BeginPhase(ctx, &gatewayv1.BeginPhaseRequest{Intent: intentRef(in),
		Stage: string(lastv1alpha1.StageCreate), SubjectHash: in.Status.Plan.Hash})
	switch {
	case status.Code(err) == codes.AlreadyExists:
		return r.abandonPhase(ctx, in, "A phase from an earlier attempt was still open")
	case terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonExecutionFailed, "The gateway refused to open the phase: "+describe(err))
	case err != nil:
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Opened phase", "plan", in.Status.Plan.Hash)
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonPhaseOpen, "Running plan "+in.Status.Plan.Hash)
	}); err != nil {
		return ctrl.Result{}, err // the next attempt finds the phase open (AlreadyExists) and abandons it
	}
	return r.runSteps(ctx, in)
}

// runSteps runs every step in order, resolving ${steps…} references from the
// results so far, then creates the ManagedResources and writes their outputs.
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
		res, err := r.runStep(ctx, in, step.ID, raw)
		switch {
		case ctx.Err() != nil:
			return ctrl.Result{}, ctx.Err() // shutting down; the next process abandons the phase
		case err != nil:
			return r.endFailed(ctx, in, fmt.Sprintf("step %s: %s", step.ID, describe(err)), false)
		case res.GetExitCode() != 0 || res.GetError() != "":
			return r.endFailed(ctx, in, stepFailure(step.ID, res), true)
		}
		results[step.ID] = runnerapi.Result{ID: step.ID, Stdout: res.GetStdout(), StdoutTruncated: res.GetStdoutTruncated()}
	}
	if err := r.ensureManagedResources(ctx, in); err != nil {
		return r.endFailed(ctx, in, "Could not create ManagedResources: "+err.Error(), false)
	}
	if err := r.writeOutputs(ctx, in); err != nil {
		return r.endFailed(ctx, in, "Could not write an output Secret: "+err.Error(), false)
	}
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		setExecuted(in, metav1.ConditionUnknown, ReasonStepsSucceeded, "Every step succeeded; closing the phase")
	}); err != nil {
		return ctrl.Result{}, err
	}
	return r.finishPhase(ctx, in)
}

// runStep calls RunCommand, retrying while another call of the phase holds it (Aborted).
func (r *IntentReconciler) runStep(ctx context.Context, in *lastv1alpha1.Intent, id string, raw []byte) (*gatewayv1.CommandResult, error) {
	req := &gatewayv1.RunCommandRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash, StepId: id, CommandJson: raw}
	for attempt := 1; ; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, stepTimeout)
		resp, err := r.Gateway.RunCommand(sctx, req)
		cancel()
		if status.Code(err) != codes.Aborted || attempt == retries {
			return resp.GetResult(), err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return nil, err
		}
	}
}

// stepFailure describes a failed step for the replanning conversation.
func stepFailure(id string, res *gatewayv1.CommandResult) string {
	if res.GetError() != "" {
		return fmt.Sprintf("step %s could not run: %s", id, res.GetError())
	}
	return fmt.Sprintf("step %s exited %d: %s", id, res.GetExitCode(), tail(res.GetStderr(), 512))
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
		name := managedResourceName(in.Name, i)
		req := &gatewayv1.WriteOutputSecretRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash,
			ManagedResource: name, Values: res.Outputs}
		for attempt := 1; ; attempt++ {
			_, err := r.Gateway.WriteOutputSecret(ctx, req)
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

// endPhase calls EndPhase, retrying Unavailable with the same outcome: the
// gateway treats a repeated EndPhase as a retry of its record save.
func (r *IntentReconciler) endPhase(ctx context.Context, in *lastv1alpha1.Intent, succeeded bool, reason string) error {
	req := &gatewayv1.EndPhaseRequest{Intent: intentRef(in), SubjectHash: in.Status.Plan.Hash, Succeeded: succeeded, Reason: reason}
	for attempt := 1; ; attempt++ {
		_, err := r.Gateway.EndPhase(ctx, req)
		if status.Code(err) != codes.Unavailable || attempt == retries {
			return err
		}
		if err := sleep(ctx, r.RetryDelay); err != nil {
			return err
		}
	}
}

// endFailed closes the phase as failed. A failure of the plan itself returns
// the Intent to Planning with the failure as context (spec §8.3); anything
// else fails the Intent.
func (r *IntentReconciler) endFailed(ctx context.Context, in *lastv1alpha1.Intent, reason string, replan bool) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Ending phase as failed", "plan", in.Status.Plan.Hash, "reason", clip(reason))
	if err := r.endPhase(ctx, in, false, reason); err != nil && status.Code(err) != codes.FailedPrecondition {
		return ctrl.Result{}, err // still PhaseOpen: the next attempt abandons the phase
	}
	if replan {
		return r.startPlanning(ctx, in, reason)
	}
	return ctrl.Result{}, r.fail(ctx, in, ReasonExecutionFailed, reason)
}

// abandonPhase closes a phase this process cannot continue and fails the Intent.
func (r *IntentReconciler) abandonPhase(ctx context.Context, in *lastv1alpha1.Intent, why string) (ctrl.Result, error) {
	if err := r.endPhase(ctx, in, false, why); err != nil && status.Code(err) != codes.FailedPrecondition {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.fail(ctx, in, ReasonInterrupted, why+"; edit the Intent's spec to plan again")
}

// finishPhase closes a phase whose steps all succeeded. If the gateway no
// longer has the phase open, an earlier EndPhase may have succeeded before its
// result was recorded here, or a gateway restart interrupted the phase:
// RunProbes reports which plan the gateway last executed.
func (r *IntentReconciler) finishPhase(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	err := r.endPhase(ctx, in, true, "")
	if status.Code(err) == codes.FailedPrecondition {
		executed, perr := r.executedPlan(ctx, in)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		if executed != in.Status.Plan.Hash {
			return ctrl.Result{}, r.fail(ctx, in, ReasonInterrupted,
				"The phase ended before it succeeded ("+describe(err)+"); edit the Intent's spec to plan again")
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

// executedPlan returns the hash of the plan the gateway last executed for the
// Intent, or "" when it has none.
func (r *IntentReconciler) executedPlan(ctx context.Context, in *lastv1alpha1.Intent) (string, error) {
	resp, err := r.Gateway.RunProbes(ctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	switch {
	case err == nil:
		return resp.GetPlanHash(), nil
	case terminal(err):
		return "", nil
	default:
		return "", err
	}
}
