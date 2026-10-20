package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// retryTeardownHint tells the user how to plan a failed teardown again (D7).
const retryTeardownHint = "change the " + lastv1alpha1.AnnotationRetry + " annotation to plan the teardown again"

// tearDown drives an Intent being deleted (design §3.4). A deletion overrides
// everything (§4.3): until the Intent is in stage Delete, whatever it was doing
// is abandoned and the teardown starts over.
func (r *IntentReconciler) tearDown(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Stage != lastv1alpha1.StageDelete {
		return r.beginDelete(ctx, in, "")
	}
	switch in.Status.Phase {
	case lastv1alpha1.PhasePlanning:
		return r.plan(ctx, in)
	case lastv1alpha1.PhaseAwaitingApproval:
		return r.awaitApproval(ctx, in)
	case lastv1alpha1.PhaseTearingDown:
		return r.execute(ctx, in)
	case lastv1alpha1.PhaseFailed:
		if in.Annotations[lastv1alpha1.AnnotationRetry] == in.Status.ObservedRetry {
			return ctrl.Result{}, nil // the Intent watch wakes us when the annotation changes
		}
		feedback := ""
		if c := meta.FindStatusCondition(in.Status.Conditions, CondReady); c != nil {
			feedback = c.Message
		}
		return r.beginDelete(ctx, in, feedback)
	}
	return r.beginDelete(ctx, in, "")
}

// beginDelete ends any phase the Intent left open, then decides from what it
// executed: an Intent with teardown commands plans its Delete; one with nothing
// to tear down is released at once. feedback, when set, is the failure the new
// Delete plan must address.
func (r *IntentReconciler) beginDelete(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	if err := r.abortOpenPhase(ctx, in); err != nil {
		return ctrl.Result{}, err
	}
	td, err := r.teardown(ctx, in)
	if err != nil {
		r.teardownBlocked(in, err)
		return ctrl.Result{}, err // the audit log or the API server: retried, never released on an error
	}
	switch {
	case td.source == teardownNone:
		return ctrl.Result{}, r.releaseUnexecuted(ctx, in, "Nothing ever executed")
	case len(td.commands) == 0:
		r.Recorder.Eventf(in, nil, corev1.EventTypeWarning, ReasonNoTeardown, "Teardown", "%s",
			"What the Intent created has no teardown commands; nothing was torn down")
		return ctrl.Result{}, r.release(ctx, in, "No teardown commands")
	case td.source == teardownManaged:
		r.Recorder.Eventf(in, nil, corev1.EventTypeWarning, ReasonExecutedPlanNotFound, "Teardown", "%s",
			"The executed plan was not found in the audit log; the teardown comes from the ManagedResources' teardown commands")
	}
	return ctrl.Result{}, r.startDelete(ctx, in, feedback)
}

// abortOpenPhase ends the phase an Intent being deleted may hold open (abortPhase).
func (r *IntentReconciler) abortOpenPhase(ctx context.Context, in *lastv1alpha1.Intent) error {
	return r.abortPhase(ctx, in, "Aborted: the Intent is being deleted")
}

// abortPhase ends, as failed for why, the phase the Intent may hold open: a
// running or lost Create, Update, Remediate or Delete phase, or an approved
// discovery command's phase. The gateway answers FailedPrecondition when none
// is open.
func (r *IntentReconciler) abortPhase(ctx context.Context, in *lastv1alpha1.Intent, why string) error {
	subject := ""
	switch in.Status.Phase {
	case lastv1alpha1.PhaseExecuting, lastv1alpha1.PhaseRemediating, lastv1alpha1.PhaseTearingDown:
		if in.Status.Plan == nil || checkpoint(in) == ReasonPhaseEnded {
			return nil
		}
		subject = in.Status.Plan.Hash
	case lastv1alpha1.PhaseAwaitingDiscoveryApproval:
		pc := in.Status.PendingCommand
		if pc == nil {
			return nil
		}
		if checkpoint(in) != ReasonPhaseOpen {
			return r.endApprovedPhase(ctx, in, why)
		}
		subject = pc.Hash
	default:
		return nil
	}
	err := r.endPhase(ctx, in, subject, false, why)
	if status.Code(err) == codes.FailedPrecondition {
		return nil // no phase was open
	}
	if err == nil {
		logf.FromContext(ctx).Info("Aborted open phase", "subject", subject, "reason", why)
	}
	return err
}

// startDelete starts Delete planning: the model orders the teardown commands.
// It records the retry annotation's value, so that only a later change plans
// the teardown again.
func (r *IntentReconciler) startDelete(ctx context.Context, in *lastv1alpha1.Intent, feedback string) error {
	r.sessions.drop(client.ObjectKeyFromObject(in))
	return r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = lastv1alpha1.StageDelete
		in.Status.ObservedGeneration = in.Generation
		in.Status.ObservedRetry = in.Annotations[lastv1alpha1.AnnotationRetry]
		in.Status.PlanningSessionSeq = 0
		in.Status.Questions = nil
		in.Status.PendingCommand = nil
		in.Status.Plan = nil
		meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
		if feedback == "" {
			meta.RemoveStatusCondition(&in.Status.Conditions, CondPlanned)
		} else {
			setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionFalse, ReasonReplanning, feedback)
		}
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM to order the teardown")
	})
}

// planTeardown asks the model for the Delete plan (design D6). A Delete session
// is a single exchange: the check admits only a plan, which ends the session,
// so there is never a conversation to continue or reload.
func (r *IntentReconciler) planTeardown(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout) // audit and ManagedResource reads
	defer cancel()
	td, err := r.teardown(actx, in)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case td.source == teardownNone:
		return ctrl.Result{}, r.releaseUnexecuted(ctx, in, "Nothing left to tear down")
	case len(td.commands) == 0:
		return ctrl.Result{}, r.release(ctx, in, "Nothing left to tear down")
	}
	first, err := agent.TeardownMessage(agent.Teardown{Namespace: in.Namespace, Name: in.Name, Text: in.Spec.Request,
		Commands: td.commands, Feedback: replanFeedback(in)})
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, err.Error()+"; "+retryTeardownHint)
	}
	pctx, pcancel := context.WithTimeout(ctx, planTimeout)
	defer pcancel()
	turn, conv, seq, err := r.Planner.Next(pctx, intentRef(in), agent.NewConversation(first), teardownCheck(td.commands))
	var capped *agent.CapError
	switch {
	case errors.Is(err, agent.ErrNoTurn) || errors.Is(err, agent.ErrNoSeq) || errors.As(err, &capped) || terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err)+"; "+retryTeardownHint)
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
	return ctrl.Result{}, r.record(ctx, in, turn, conv, int64(seq))
}

// teardownSource says where an Intent's teardown commands come from.
type teardownSource int

const (
	teardownNone     teardownSource = iota // nothing executed, and no ManagedResource exists
	teardownExecuted                       // the executed plan, from the audit log (D3)
	teardownManaged                        // the ManagedResources, the audit log no longer holding the executed plan (§3.3)
)

type teardownPlan struct {
	source   teardownSource
	commands []lastv1alpha1.Command
}

// teardown returns the commands that tear down what the Intent created, in
// resource order. They come from the executed plan the audit log holds, never
// from status (design §7). When the audit log does not hold it, the Intent's
// ManagedResources supply theirs.
func (r *IntentReconciler) teardown(ctx context.Context, in *lastv1alpha1.Intent) (teardownPlan, error) {
	body, _, found, err := agent.ExecutedPlan(ctx, r.Gateway, intentRef(in))
	if err != nil {
		return teardownPlan{}, err
	}
	if found {
		td := teardownPlan{source: teardownExecuted}
		for _, res := range body.Resources {
			td.commands = append(td.commands, res.Teardown...)
		}
		return td, nil
	}
	mrs, err := r.ownedManagedResources(ctx, in)
	if err != nil || len(mrs) == 0 {
		return teardownPlan{}, err
	}
	td := teardownPlan{source: teardownManaged}
	for _, mr := range mrs {
		td.commands = append(td.commands, mr.Spec.Teardown...)
	}
	return td, nil
}

// ownedManagedResources returns the ManagedResources the Intent controls, in
// resource order: by the index lastv1alpha1.ManagedResourceName gives them, then by name.
func (r *IntentReconciler) ownedManagedResources(ctx context.Context, in *lastv1alpha1.Intent) ([]lastv1alpha1.ManagedResource, error) {
	var list lastv1alpha1.ManagedResourceList
	if err := r.Reader.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return nil, err
	}
	mrs := slices.DeleteFunc(list.Items, func(mr lastv1alpha1.ManagedResource) bool { return !metav1.IsControlledBy(&mr, in) })
	index := func(mr lastv1alpha1.ManagedResource) int {
		if i, err := strconv.Atoi(strings.TrimPrefix(mr.Name, in.Name+"-")); err == nil && mr.Name == lastv1alpha1.ManagedResourceName(in.Name, i) {
			return i
		}
		return math.MaxInt
	}
	slices.SortFunc(mrs, func(a, b lastv1alpha1.ManagedResource) int {
		return cmp.Or(cmp.Compare(index(a), index(b)), strings.Compare(a.Name, b.Name))
	})
	return mrs, nil
}

// teardownCheck refuses a Delete turn that is not a plan whose steps are a
// permutation of the teardown commands, with no resources (design D6). Commands
// compare by the canonical JSON of everything but their step id, which the plan
// may change to keep its ids unique. The refusal names the problem, and the
// model gets it as a correction.
func teardownCheck(teardown []lastv1alpha1.Command) func(agent.Turn) error {
	return func(t agent.Turn) error {
		if t.Kind != agent.KindPlan || t.Plan == nil {
			return errors.New("the Intent is being deleted: answer with a plan that orders the teardown commands; " +
				"questions and discovery commands are not available")
		}
		if n := len(t.Plan.Resources); n > 0 {
			return fmt.Errorf(`a Delete plan has no resources, and this one has %d; set "resources" to []`, n)
		}
		want := map[string][]int{} // command key → indices of the teardown commands with it, in order
		for i, c := range teardown {
			k, err := plan.CommandKey(c)
			if err != nil {
				return fmt.Errorf("teardown command %d cannot be encoded: %w", i+1, err)
			}
			want[k] = append(want[k], i)
		}
		var added []string
		for i, s := range t.Plan.Steps {
			k, err := plan.CommandKey(s)
			if err != nil {
				return fmt.Errorf("step %d cannot be encoded: %w", i+1, err)
			}
			if len(want[k]) > 0 {
				want[k] = want[k][1:]
				continue
			}
			added = append(added, fmt.Sprintf("step %d (id %q)", i+1, s.ID))
		}
		var missing []string
		for i, c := range teardown {
			k, _ := plan.CommandKey(c) // encoded above
			if slices.Contains(want[k], i) {
				missing = append(missing, fmt.Sprintf("teardown command %d (id %q)", i+1, c.ID))
			}
		}
		var problems []string
		for len(added) > 0 && len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("the plan replaces %s with %s", missing[0], added[0]))
			added, missing = added[1:], missing[1:]
		}
		for _, a := range added {
			problems = append(problems, "the plan adds "+a)
		}
		for _, m := range missing {
			problems = append(problems, "the plan leaves out "+m)
		}
		if len(problems) == 0 {
			return nil
		}
		return fmt.Errorf("%s, which is not a permutation of the teardown commands. The steps are the teardown "+
			"commands, each exactly once and unchanged but for its id, in the order that tears things down safely",
			strings.Join(problems, "; "))
	}
}

// tornDown reports a gateway refusal of a Delete phase call saying the Intent
// was already torn down: a Delete phase of this Intent UID succeeded, and this
// operator lost the answer. The teardown is done, so the Intent is released.
// Trusting the refusal grants nothing: the operator may remove its own
// finalizer anyway.
func tornDown(in *lastv1alpha1.Intent, err error) bool {
	return planStage(in) == lastv1alpha1.StageDelete && gatewayv1.TornDown(err)
}

// msgTornDown is why an Intent is released on a torn-down refusal.
const msgTornDown = "The gateway already tore the Intent down"

// failRun fails the Intent for a phase that did not complete. A Delete phase
// fails as TeardownFailed whatever went wrong, and keeps the finalizer: the
// retry annotation plans the teardown again (D7).
func (r *IntentReconciler) failRun(ctx context.Context, in *lastv1alpha1.Intent, reason, msg string) error {
	if in.Status.Stage == lastv1alpha1.StageDelete {
		return r.fail(ctx, in, ReasonTeardownFailed, msg+"; "+retryTeardownHint)
	}
	return r.fail(ctx, in, reason, msg)
}

// release lets an Intent being deleted go by removing its finalizer. Its
// ManagedResources, and through them their output Secrets, then go by
// ownerReference garbage collection once the Intent is gone; the operator
// deletes none of them (spec §4.1). Callers have ended every phase: the
// teardown succeeded, or nothing was left to tear down.
func (r *IntentReconciler) release(ctx context.Context, in *lastv1alpha1.Intent, why string) error {
	key := client.ObjectKeyFromObject(in)
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var fresh lastv1alpha1.Intent
		if err := r.Reader.Get(ctx, key, &fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		if !controllerutil.RemoveFinalizer(&fresh, lastv1alpha1.FinalizerTeardown) {
			return nil
		}
		return r.Patch(ctx, &fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}); client.IgnoreNotFound(err) != nil {
		return err
	}
	r.sessions.drop(key)
	logf.FromContext(ctx).Info("Released Intent", "reason", why)
	return nil
}

// teardownBlocked tells the user why an Intent being deleted waits: what it
// executed could not be read. The deletion is retried.
func (r *IntentReconciler) teardownBlocked(in *lastv1alpha1.Intent, err error) {
	r.Recorder.Eventf(in, nil, corev1.EventTypeWarning, ReasonTeardownBlocked, "Teardown",
		"Could not read what the Intent executed, so the deletion waits and is retried: %s", describe(err))
}

// releaseUnexecuted releases an Intent with no executed plan and no
// ManagedResources (D6). A Create phase that ran steps and then failed may still
// have created something no teardown covers, so the release warns about it. An
// audit read error is returned, so the release is retried, never skipped.
func (r *IntentReconciler) releaseUnexecuted(ctx context.Context, in *lastv1alpha1.Intent, why string) error {
	subject, steps, found, err := agent.PartialCreate(ctx, r.Gateway, intentRef(in))
	if err != nil {
		r.teardownBlocked(in, err)
		return err
	}
	if found {
		r.Recorder.Eventf(in, nil, corev1.EventTypeWarning, ReasonPartialCreateNotTornDown, "Teardown",
			"Create plan %s ran %d steps and did not succeed; nothing was torn down", subject, steps)
		logf.FromContext(ctx).Info("Found a failed Create plan that ran steps; nothing was torn down",
			"plan", subject, "stepsRun", steps)
	}
	return r.release(ctx, in, why)
}

// ensureFinalizer adds the teardown finalizer before the Intent's first Create
// phase can begin (D7). It writes metadata only, re-reading the Intent first,
// and leaves in holding the written object. An Intent being deleted gets none.
func (r *IntentReconciler) ensureFinalizer(ctx context.Context, in *lastv1alpha1.Intent) error {
	if controllerutil.ContainsFinalizer(in, lastv1alpha1.FinalizerTeardown) {
		return nil
	}
	key := client.ObjectKeyFromObject(in)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var fresh lastv1alpha1.Intent
		if err := r.Reader.Get(ctx, key, &fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		if !fresh.DeletionTimestamp.IsZero() || !controllerutil.AddFinalizer(&fresh, lastv1alpha1.FinalizerTeardown) {
			*in = fresh
			return nil
		}
		if err := r.Patch(ctx, &fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		*in = fresh
		return nil
	})
}
