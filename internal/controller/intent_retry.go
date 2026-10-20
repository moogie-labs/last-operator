package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

const (
	// maxReplans is how many replans failed steps may start in a row; the next
	// failed step fails the Intent with PlanningFailed (D10).
	maxReplans = 3
	// firstRetryDelay and maxRetryDelay bound the backoff of automatic retries (D10).
	firstRetryDelay = time.Minute
	maxRetryDelay   = time.Hour
)

// retryDelay is how long a Failed Intent in an Auto stage waits before its
// next automatic retry, after retries retries: a minute, doubled for each
// retry so far, and at most an hour (D10). It doubles step by step up to the
// cap, so no shift can overflow.
func retryDelay(retries int32) time.Duration {
	d := firstRetryDelay
	for i := int32(0); i < retries && d < maxRetryDelay; i++ {
		d *= 2
	}
	return min(d, maxRetryDelay)
}

// sessionCause is why a planning session starts, which decides how the
// Intent's counters change in the write that starts it (D10).
type sessionCause int

const (
	// causePlan is a new Intent, a spec edit, new drift, a deletion, or a
	// session that starts over. A spec edit resets the counters itself.
	causePlan sessionCause = iota
	// causeReplan is a failed step of the plan: counted in status.replans.
	causeReplan
	// causeRetry is a retry of a Failed Intent: counted in status.retries, and
	// the replans start over.
	causeRetry
)

// count applies the cause to the Intent's counters and records the retry
// annotation's value the session acted on, so that only a later change retries.
func (c sessionCause) count(in *lastv1alpha1.Intent) {
	switch c {
	case causeReplan:
		in.Status.Replans++
	case causeRetry:
		in.Status.Retries++
		in.Status.Replans = 0
	}
	in.Status.ObservedRetry = in.Annotations[lastv1alpha1.AnnotationRetry]
}

// retryFailed retries a Failed Intent whose spec has not changed (D10, ruling
// R5), in any stage, Delete included. A change to the retry annotation retries
// it at once, in Manual or Auto mode. Without one, an Intent whose failed stage
// is Auto retries once retryDelay has passed since it failed, unless an
// IntentApproval denied its plan; until then it is requeued for the time left.
// A Manual Intent waits: the Intent watch wakes it when the annotation changes.
func (r *IntentReconciler) retryFailed(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	trigger := "annotation"
	if in.Annotations[lastv1alpha1.AnnotationRetry] == in.Status.ObservedRetry {
		due, wait, err := r.autoRetry(ctx, in)
		if err != nil || !due {
			return ctrl.Result{RequeueAfter: wait}, err
		}
		trigger = "backoff"
	}
	logf.FromContext(ctx).Info("Retrying failed Intent", "stage", failedStage(in), "trigger", trigger,
		"retries", in.Status.Retries)
	return r.retry(ctx, in)
}

// autoRetry reports whether a Failed Intent retries by itself now, or else how
// long until it does (wait is 0 when it never does): its failed stage must be
// Auto, and its plan not denied. The delay counts from when the Intent failed,
// the Ready condition's lastTransitionTime (fail sets it).
func (r *IntentReconciler) autoRetry(ctx context.Context, in *lastv1alpha1.Intent) (due bool, wait time.Duration, err error) {
	failed := meta.FindStatusCondition(in.Status.Conditions, CondReady)
	if failed == nil || failed.Reason == ReasonApprovalDenied {
		return false, 0, nil
	}
	l, err := r.readLayers(ctx, in.Namespace)
	if err != nil {
		return false, 0, err
	}
	if l.modes(in).For(failedStage(in)) != lastv1alpha1.ModeAuto {
		return false, 0, nil
	}
	wait = failed.LastTransitionTime.Add(retryDelay(in.Status.Retries)).Sub(r.now())
	if wait > 0 {
		return false, wait, nil
	}
	return true, 0, nil
}

// failedStage is the stage a Failed Intent failed in: Remediate for
// RemediationFailed, which the cap raises before a Remediate session records
// its stage, and planStage otherwise.
func failedStage(in *lastv1alpha1.Intent) lastv1alpha1.Stage {
	if c := meta.FindStatusCondition(in.Status.Conditions, CondReady); c != nil && c.Reason == ReasonRemediationFailed {
		return lastv1alpha1.StageRemediate
	}
	return planStage(in)
}

// retry restarts what the failed stage's trigger started, with the failure as
// feedback, in a write that counts the retry (causeRetry):
//   - Delete: beginDelete, as a deletion does.
//   - Remediate: a Remediate session, the remediation count starting over.
//   - Update: an Update session; it plans again from the stage choice when the
//     audit log no longer holds the executed plan.
//   - Create, and a stage with no restart path of its own: a session whose
//     stage the audit log decides, as for a spec edit. A Create that ran and
//     then failed verification has an executed plan, so it plans an Update.
func (r *IntentReconciler) retry(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	failure := ""
	if c := meta.FindStatusCondition(in.Status.Conditions, CondReady); c != nil {
		failure = c.Message
	}
	switch stage := failedStage(in); {
	case !in.DeletionTimestamp.IsZero():
		return r.beginDelete(ctx, in, failure, causeRetry)
	case stage == lastv1alpha1.StageRemediate:
		return r.beginRemediate(ctx, in, failure, causeRetry)
	case stage == lastv1alpha1.StageUpdate:
		return r.startSession(ctx, in, lastv1alpha1.StageUpdate, failure, causeRetry)
	}
	return r.startSession(ctx, in, "", failure, causeRetry)
}
