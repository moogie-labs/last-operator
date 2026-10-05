package controller

import (
	"context"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	// CondReady summarizes the lifecycle: True only in phase Ready. Its reason
	// says why an Intent is not Ready, including why it failed (spec §7).
	CondReady = "Ready"
	// CondPlanned holds the failure a new plan must address while replanning.
	CondPlanned = "Planned"
	// CondExecuted checkpoints the current plan's phase across operator restarts.
	CondExecuted = "Executed"
	// CondInSync on a ManagedResource reports whether its probes last passed (spec §5.3).
	CondInSync = "InSync"
)

// Ready reasons.
const (
	ReasonPlanning           = "Planning"
	ReasonAwaitingApproval   = "AwaitingApproval"
	ReasonExecuting          = "Executing"
	ReasonVerifying          = "Verifying"
	ReasonReady              = "Ready"
	ReasonDrifted            = "Drifted"
	ReasonPlanningFailed     = "PlanningFailed"
	ReasonApprovalDenied     = "ApprovalDenied"
	ReasonExecutionFailed    = "ExecutionFailed"
	ReasonInterrupted        = "ExecutionInterrupted"
	ReasonVerificationFailed = "VerificationFailed"
)

// Planned reasons.
const (
	ReasonPlanWritten = "PlanWritten"
	ReasonReplanning  = "Replanning"
)

// Executed reasons: the checkpoints of a phase, in order.
const (
	ReasonPhaseOpen      = "PhaseOpen"      // BeginPhase succeeded; steps are running
	ReasonStepsSucceeded = "StepsSucceeded" // every step succeeded and outputs are written
	ReasonPhaseEnded     = "PhaseEnded"     // the gateway recorded the phase as succeeded
)

// InSync reasons.
const (
	ReasonProbesPassed = "ProbesPassed"
	ReasonProbesFailed = "ProbesFailed"
)

// maxMessage bounds condition messages; command output stays in the audit log.
const maxMessage = 1024

// clip cuts s to maxMessage bytes on a UTF-8 boundary.
func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func setCondition(conds *[]metav1.Condition, gen int64, typ string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: clip(msg), ObservedGeneration: gen})
}

// setPhase moves an Intent to phase p and records why in its Ready condition.
func setPhase(in *lastv1alpha1.Intent, p lastv1alpha1.Phase, reason, msg string) {
	in.Status.Phase = p
	s := metav1.ConditionFalse
	if p == lastv1alpha1.PhaseReady {
		s = metav1.ConditionTrue
	}
	setCondition(&in.Status.Conditions, in.Generation, CondReady, s, reason, msg)
}

// update applies change to in and writes its status. On a conflict it
// re-reads the Intent and applies change again, so change must be a function
// of the object it is given. in holds the written object afterwards, so
// consecutive updates in one reconcile never wait for the informer cache. A
// phase change emits an Event (spec §16).
func (r *IntentReconciler) update(ctx context.Context, in *lastv1alpha1.Intent, change func(*lastv1alpha1.Intent)) error {
	from := in.Status.Phase
	key := client.ObjectKeyFromObject(in)
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		change(in)
		err := r.Status().Update(ctx, in)
		if apierrors.IsConflict(err) {
			if gerr := r.Get(ctx, key, in); gerr != nil {
				return gerr
			}
		}
		return err
	})
	if err != nil || in.Status.Phase == from {
		return err
	}
	kind := corev1.EventTypeNormal
	if in.Status.Phase == lastv1alpha1.PhaseFailed {
		kind = corev1.EventTypeWarning
	}
	note := "Moved to " + string(in.Status.Phase)
	if c := meta.FindStatusCondition(in.Status.Conditions, CondReady); c != nil && c.Message != "" {
		note = c.Message
	}
	r.Recorder.Eventf(in, nil, kind, string(in.Status.Phase), "Transition", "%s", note)
	return nil
}

// fail moves the Intent to Failed; a spec edit retries it (spec §7).
func (r *IntentReconciler) fail(ctx context.Context, in *lastv1alpha1.Intent, reason, msg string) error {
	logf.FromContext(ctx).Info("Intent failed", "reason", reason, "message", clip(msg))
	return r.update(ctx, in, func(in *lastv1alpha1.Intent) { setPhase(in, lastv1alpha1.PhaseFailed, reason, msg) })
}
