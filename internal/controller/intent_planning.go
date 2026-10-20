package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// planTimeout bounds one model turn: up to MaxAttempts calls, corrections
// included, each bounded by CallTimeout, and a minute to spare. The audit reads
// before it have their own bound.
const planTimeout = agent.MaxAttempts*agent.CallTimeout + time.Minute

// startPlanning discards the current plan and planning session and starts a
// new session for the current spec. feedback, when set, is a failure the new
// plan must address. The session plans an Update once the Intent has an
// executed plan, and a Create otherwise (sessionStage).
func (r *IntentReconciler) startPlanning(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	return r.startSession(ctx, in, "", feedback, causePlan)
}

// startSession is startPlanning for a session whose stage may already be
// known. A failed Create, Update or Remediate phase leaves the executed plan
// unchanged, so its replan keeps its stage without reading the audit log: a
// failed read there would leave the phase's checkpoint open and fail the
// Intent as interrupted. An empty stage is chosen by sessionStage.
//
// A Remediate session keeps observedGeneration, so a spec edit made meanwhile
// still plans an Update, and keeps status.plan (see startRemediate). A session
// a spec edit starts (a new generation) resets status.remediations,
// status.replans and status.retries: the edited Intent starts counting afresh.
// A replan after a failed step is no edit and keeps the counts. The write
// counts the session's cause (sessionCause.count).
//
// It adds the teardown finalizer first (D7). A deletion overrides everything
// (design §4.3): for an Intent being deleted, Delete planning starts instead.
// The write shows the Intent's effective modes in status.effectiveModes.
func (r *IntentReconciler) startSession(ctx context.Context, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage,
	feedback string, cause sessionCause) (ctrl.Result, error) {
	if !in.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.startDelete(ctx, in, feedback, cause)
	}
	if err := r.ensureFinalizer(ctx, in); err != nil {
		return ctrl.Result{}, err
	}
	if !in.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // deleted meanwhile: the next reconcile tears it down
	}
	if stage == "" {
		var err error
		if stage, err = r.sessionStage(ctx, in); err != nil {
			return ctrl.Result{}, err // the audit log could not be read: retried, never guessed
		}
	}
	l, err := r.readLayers(ctx, in.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.EffectiveModes = l.modes(in).Status()
		if stage != lastv1alpha1.StageRemediate {
			if in.Generation != in.Status.ObservedGeneration {
				in.Status.Remediations, in.Status.Replans, in.Status.Retries = 0, 0, 0
			}
			in.Status.ObservedGeneration = in.Generation
			in.Status.ObservedRequestHash = requestHash(in.Spec.Request)
			in.Status.Plan = nil
		}
		cause.count(in)
		resetSession(in, stage, feedback)
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM for a plan")
	})
}

// resetSession clears the Intent's planning session for a new one of stage.
// feedback, when set, is the failure the new plan must address.
func resetSession(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, feedback string) {
	in.Status.Stage = stage
	in.Status.PlanningSessionSeq = 0
	in.Status.Questions = nil
	in.Status.PendingCommand = nil
	meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
	if feedback == "" {
		meta.RemoveStatusCondition(&in.Status.Conditions, CondPlanned)
	} else {
		setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionFalse, ReasonReplanning, feedback)
	}
}

// sessionStage is the stage a new planning session plans for: Update when the
// audit log holds the Intent's executed plan, Create when it does not (never
// executed, or the index was trimmed past it). The executed plan is never read from
// status. A gateway without QueryAudit cannot say, so the session plans a
// Create, as before Update existed. Any other error is returned for a retry.
func (r *IntentReconciler) sessionStage(ctx context.Context, in *lastv1alpha1.Intent) (lastv1alpha1.Stage, error) {
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	defer cancel()
	_, _, found, err := agent.ExecutedPlan(actx, r.Gateway, intentRef(in))
	switch {
	case status.Code(err) == codes.Unimplemented:
		logf.FromContext(ctx).Info("Could not read the executed plan; planning a Create", "reason", err.Error())
		return lastv1alpha1.StageCreate, nil
	case err != nil:
		return "", err
	case found:
		return lastv1alpha1.StageUpdate, nil
	}
	return lastv1alpha1.StageCreate, nil
}

// requestHash identifies a spec.request. Answers to clarifying questions do not
// start a new planning session; a changed request does (M3 spec §4.2).
func requestHash(request string) string {
	sum := sha256.Sum256([]byte(request))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// replanFeedback returns the failure the session's plan must address, if any.
func replanFeedback(in *lastv1alpha1.Intent) string {
	if c := meta.FindStatusCondition(in.Status.Conditions, CondPlanned); c != nil && c.Reason == ReasonReplanning {
		return c.Message
	}
	return ""
}

// plan continues the planning session (M3 spec §4.2): it sends the
// conversation, with the user turn the Intent's state owes the model, and
// records the model's next turn.
func (r *IntentReconciler) plan(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.Stage == lastv1alpha1.StageDelete {
		return r.planTeardown(ctx, in)
	}
	if in.Generation != in.Status.ObservedGeneration {
		return r.startPlanning(ctx, in, "")
	}
	// The calls inside bound themselves: a Remediate's confirming probe run
	// takes longer than the audit reads.
	conv, fresh, err := r.conversation(ctx, in)
	if res, done, err := r.sessionFailed(ctx, in, err); done {
		return res, err
	}
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout) // result and used-plan reads
	defer cancel()
	if !fresh {
		next, err := r.pendingTurn(actx, in)
		if err != nil {
			return ctrl.Result{}, err // transient: retried with backoff
		}
		if next == "" {
			// Nothing in the Intent's state continues the session, which ends
			// with the model's own answer: start a new one.
			if planStage(in) == lastv1alpha1.StageRemediate {
				return r.startSession(ctx, in, lastv1alpha1.StageRemediate, replanFeedback(in), causePlan)
			}
			return r.startPlanning(ctx, in, replanFeedback(in))
		}
		conv = conv.With(next)
	}
	feedback := replanFeedback(in)
	check, err := r.turnCheck(actx, in, feedback)
	if err != nil {
		return ctrl.Result{}, err // transient: retried with backoff
	}
	pctx, pcancel := context.WithTimeout(ctx, planTimeout)
	defer pcancel()
	turn, conv, seq, err := r.Planner.Next(pctx, intentRef(in), conv, check)
	var capped *agent.CapError
	switch {
	case errors.Is(err, agent.ErrNoTurn) && errors.Is(err, errPlanAlreadyRan):
		msg := "The model kept proposing the plan that already ran"
		if feedback != "" {
			msg = "The model kept proposing the plan that already failed: " + feedback
		}
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, msg)
	case errors.Is(err, agent.ErrNoTurn) || errors.Is(err, agent.ErrNoSeq) || errors.As(err, &capped) || terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err))
	case err != nil:
		// Transient, a deadline included: retried with backoff, never counted
		// (M3 spec §6).
		return ctrl.Result{}, err
	}
	if turn.Kind == agent.KindPlan && planStage(in) == lastv1alpha1.StageRemediate {
		var failure planningFailure
		err := r.completeRemediate(ctx, in, turn.Plan)
		switch {
		case errors.As(err, &failure):
			return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, string(failure))
		case err != nil:
			return ctrl.Result{}, err // transient: the model is asked again
		}
	}
	session := in.Status.PlanningSessionSeq
	if fresh {
		session = int64(seq)
	}
	return ctrl.Result{}, r.record(ctx, in, turn, conv, session)
}

// sessionFailed handles an error building the planning session's
// conversation; done reports that it did.
func (r *IntentReconciler) sessionFailed(ctx context.Context, in *lastv1alpha1.Intent, err error) (ctrl.Result, bool, error) {
	var failure planningFailure
	switch {
	case err == nil:
		return ctrl.Result{}, false, nil
	case errors.Is(err, errBadPolicy):
		return ctrl.Result{}, true, r.fail(ctx, in, ReasonPlanningFailed, err.Error())
	case errors.As(err, &failure):
		return ctrl.Result{}, true, r.fail(ctx, in, ReasonPlanningFailed, string(failure))
	case errors.Is(err, errNoExecutedPlan):
		// The executed plan went missing from the audit log since the stage was
		// chosen: choose it again.
		logf.FromContext(ctx).Info("Could not find the executed plan to update; planning again")
		res, err := r.startPlanning(ctx, in, replanFeedback(in))
		return res, true, err
	case errors.Is(err, errDriftCleared):
		return ctrl.Result{}, true, r.driftCleared(ctx, in)
	case errors.Is(err, errProbeInFlight):
		return ctrl.Result{RequeueAfter: r.RetryDelay}, true, nil // never overlap a probe run
	}
	return ctrl.Result{}, true, err // transient: retried with backoff
}

// turnCheck returns the check the model's next turn must pass. A Remediate
// turn may not ask questions or name resources (remediateCheck); it is never
// refused as a rerun, since each Remediate plan carries a new nonce. Other
// turns may not reuse answered question ids or repeat a plan that already ran.
func (r *IntentReconciler) turnCheck(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (func(agent.Turn) error, error) {
	if planStage(in) == lastv1alpha1.StageRemediate {
		return remediateCheck, nil
	}
	used, err := agent.UsedPlans(ctx, r.Gateway, intentRef(in))
	switch {
	case status.Code(err) == codes.Unimplemented:
		used = nil // a gateway without QueryAudit cannot say; its replay refusal stays the backstop
	case err != nil:
		return nil, err
	}
	return both(answeredCheck(in), rerunCheck(used, feedback)), nil
}

// record writes the model's turn to the Intent's status and moves the Intent
// to the phase that waits on it: AwaitingApproval for a plan, Clarifying for
// questions, AwaitingDiscoveryApproval for a command. status.stage keeps the
// session's stage throughout (ruling R4): the phase and status.pendingCommand
// mark a discovery command, whose approval is for stage Discovery. The
// conversation stays in memory until a plan ends the session.
//
// When the Intent's effective mode for the stage is Auto, record does not wait
// for an approval: a plan moves straight to the phase that runs it, and a
// command is marked ReasonRunningDiscovery, which runs it. This is the only
// place the operator skips the wait; the gateway still decides, and if it needs
// an approval the Intent waits for one (needsApproval). The write also shows
// the effective modes in status.effectiveModes.
func (r *IntentReconciler) record(ctx context.Context, in *lastv1alpha1.Intent, turn agent.Turn, conv agent.Conversation, session int64) error {
	l, err := r.readLayers(ctx, in.Namespace)
	if err != nil {
		return err // transient: the model is asked again
	}
	subject := ""
	var pending *lastv1alpha1.PendingCommand
	switch turn.Kind {
	case agent.KindPlan:
		subject = turn.Plan.Hash
	case agent.KindCommand:
		nonce, err := agent.NewNonce()
		if err != nil {
			return err
		}
		if subject, _, err = plan.DiscoveryHash(*turn.Command, nonce); err != nil {
			return err
		}
		pending = &lastv1alpha1.PendingCommand{Hash: subject, Command: *turn.Command, Nonce: nonce, Reason: turn.Reason}
	}
	gen, prev, stage := in.Generation, in.Status.PlanningSessionSeq, planStage(in)
	applied := false
	err = r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		applied = in.Generation == gen && in.Status.Phase == lastv1alpha1.PhasePlanning && in.Status.PlanningSessionSeq == prev &&
			planStage(in) == stage
		if !applied {
			return // the spec changed while planning; the next reconcile plans again
		}
		modes := l.modes(in)
		in.Status.EffectiveModes = modes.Status()
		in.Status.PlanningSessionSeq = session
		if in.Status.ObservedRequestHash == "" {
			// An Intent an M2 operator left in Planning; clarify would discard
			// its questions.
			in.Status.ObservedRequestHash = requestHash(in.Spec.Request)
		}
		in.Status.Questions = nil
		in.Status.PendingCommand = nil
		meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
		switch turn.Kind {
		case agent.KindPlan:
			in.Status.Stage = stage
			in.Status.Plan = turn.Plan
			setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionTrue, ReasonPlanWritten, "Plan "+subject)
			if modes.For(stage) == lastv1alpha1.ModeAuto {
				setRunning(in, stage, "Running plan "+subject+" without approval (Auto)")
				break
			}
			setPhase(in, lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval,
				"Waiting for an IntentApproval of plan "+subject+" for stage "+string(stage))
		case agent.KindQuestions:
			in.Status.Questions = turn.Questions
			setPhase(in, lastv1alpha1.PhaseClarifying, ReasonClarifying,
				fmt.Sprintf("Waiting for spec.answers to answer %d questions", len(turn.Questions)))
		case agent.KindCommand:
			in.Status.PendingCommand = pending
			if modes.For(lastv1alpha1.StageDiscovery) == lastv1alpha1.ModeAuto {
				setPhase(in, lastv1alpha1.PhaseAwaitingDiscoveryApproval, ReasonRunningDiscovery,
					"Running discovery command "+subject+" without approval (Auto)")
				break
			}
			setPhase(in, lastv1alpha1.PhaseAwaitingDiscoveryApproval, ReasonAwaitingDiscoveryApproval,
				"Waiting for an IntentApproval of discovery command "+subject+" for stage Discovery")
		}
	})
	if apierrors.IsInvalid(err) {
		// update left the rejected turn on in; start the Failed write from the
		// stored status, read uncached into a fresh object so nothing stale survives
		var fresh lastv1alpha1.Intent
		if gerr := r.Reader.Get(ctx, client.ObjectKeyFromObject(in), &fresh); gerr != nil {
			return gerr
		}
		*in = fresh
		return r.fail(ctx, in, ReasonPlanningFailed, fmt.Sprintf("The API server rejected the %s: %v", turn.Kind, err))
	}
	if err != nil || !applied {
		return err
	}
	if turn.Kind == agent.KindPlan {
		r.sessions.drop(client.ObjectKeyFromObject(in)) // a plan ends the session
	} else {
		r.sessions.put(in, conv)
	}
	if turn.Kind == agent.KindQuestions {
		return nil
	}
	// Hash what was stored, read uncached into a fresh object: the write's
	// response was decoded into in, and a decode merges, so a field the API
	// server pruned would still be there.
	var stored lastv1alpha1.Intent
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(in), &stored); err != nil {
		return err
	}
	if storedTurnChanged(&stored, turn.Kind, subject) {
		*in = stored
		return r.fail(ctx, in, ReasonPlanningFailed, fmt.Sprintf(msgTurnChanged, turn.Kind))
	}
	return nil
}

// msgTurnChanged explains a plan or command the API server did not store as written.
const msgTurnChanged = "The API server changed the %s when storing it; are the CRDs current?"

// storedTurnChanged reports whether stored, as read back from the API server,
// holds the plan or discovery command hashed as hash but no longer hashes to
// it. An API server with a stale CRD prunes unknown fields silently, and the
// gateway would later refuse the subject with a hash mismatch.
func storedTurnChanged(stored *lastv1alpha1.Intent, kind agent.Kind, hash string) bool {
	switch kind {
	case agent.KindPlan:
		p := stored.Status.Plan
		if p == nil || p.Hash != hash {
			return false // the write was skipped; the spec changed while planning
		}
		got, err := plan.Hash(plan.FromStatus(p))
		return err != nil || got != hash
	case agent.KindCommand:
		pc := stored.Status.PendingCommand
		if pc == nil || pc.Hash != hash {
			return false
		}
		got, _, err := plan.DiscoveryHash(pc.Command, pc.Nonce)
		return err != nil || got != hash
	}
	return false
}

// planStage is the stage of the Intent's planning session and its plan:
// Update, Remediate or Delete as status.stage records it, and Create for
// anything else, including the Discovery or empty stage an older operator
// left (ruling R4).
func planStage(in *lastv1alpha1.Intent) lastv1alpha1.Stage {
	switch s := in.Status.Stage; s {
	case lastv1alpha1.StageUpdate, lastv1alpha1.StageRemediate, lastv1alpha1.StageDelete:
		return s
	}
	return lastv1alpha1.StageCreate
}

// replannable reports whether a plan of stage is planned again for a spec edit
// or after one of its own steps failed: a Create, Update or Remediate plan is.
// A spec edit plans a Create or an Update; a failed step plans its own stage
// again. A Delete plan is not: a deletion raises the generation too, and a
// failed teardown waits for the retry annotation.
func replannable(stage lastv1alpha1.Stage) bool {
	return stage == lastv1alpha1.StageCreate || stage == lastv1alpha1.StageUpdate || stage == lastv1alpha1.StageRemediate
}

// awaitApproval waits for an IntentApproval of the current plan for its stage.
// A spec edit plans a Create or Update again, abandoning a Remediate. A deletion raises the generation,
// and spec edits no longer matter, so a Delete plan never goes back to
// planning for one.
func (r *IntentReconciler) awaitApproval(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	stage := planStage(in)
	switch {
	case stage == lastv1alpha1.StageDelete && in.Status.Plan == nil:
		return ctrl.Result{}, r.startDelete(ctx, in, "", causePlan)
	case in.Generation != in.Status.ObservedGeneration && replannable(stage), in.Status.Plan == nil:
		return r.startPlanning(ctx, in, "")
	}
	approved, denial, err := r.decision(ctx, in, stage, in.Status.Plan.Hash)
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case denial != nil:
		msg := fmt.Sprintf("IntentApproval %s denied the plan: %s", denial.Name, denial.Spec.Reason)
		if stage == lastv1alpha1.StageDelete {
			msg += "; " + retryTeardownHint
		}
		return ctrl.Result{}, r.fail(ctx, in, ReasonApprovalDenied, msg)
	case !approved:
		return ctrl.Result{}, nil // the IntentApproval watch wakes us
	}
	gen, subject := in.Generation, in.Status.Plan.Hash
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: run only the plan that was approved.
		if (replannable(stage) && in.Generation != gen) || planStage(in) != stage ||
			in.Status.Phase != lastv1alpha1.PhaseAwaitingApproval || in.Status.Plan == nil || in.Status.Plan.Hash != subject {
			return
		}
		switch stage {
		case lastv1alpha1.StageDelete:
			setRunning(in, stage, "Running teardown plan "+subject)
		case lastv1alpha1.StageRemediate:
			setRunning(in, stage, "Running Remediate plan "+subject)
		default:
			setRunning(in, stage, "Running plan "+subject)
		}
	})
}

// runningPhase returns the phase that runs a plan of stage, and its Ready reason.
func runningPhase(stage lastv1alpha1.Stage) (lastv1alpha1.Phase, string) {
	switch stage {
	case lastv1alpha1.StageDelete:
		return lastv1alpha1.PhaseTearingDown, ReasonTearingDown
	case lastv1alpha1.StageRemediate:
		return lastv1alpha1.PhaseRemediating, ReasonRemediating
	}
	return lastv1alpha1.PhaseExecuting, ReasonExecuting
}

// setRunning moves the Intent to the phase that runs its plan of stage.
func setRunning(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage, msg string) {
	p, reason := runningPhase(stage)
	setPhase(in, p, reason, msg)
}

// decision reads the IntentApprovals of subject for the Intent and stage. The
// gateway re-checks every approval itself (spec §8.3); this only decides when
// to try. A Deny wins over any Approve, as in the gateway.
func (r *IntentReconciler) decision(ctx context.Context, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage,
	subject string) (approved bool, denial *lastv1alpha1.IntentApproval, err error) {
	op := &lastv1alpha1.OperatorConfig{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: operatorConfigName}, op); apierrors.IsNotFound(err) {
		op = nil
	} else if err != nil {
		return false, nil, err
	}
	maxAge, now := lastv1alpha1.ApprovalMaxAge(op), r.now()
	var list lastv1alpha1.IntentApprovalList
	if err := r.List(ctx, &list, client.InNamespace(in.Namespace)); err != nil {
		return false, nil, err
	}
	for i := range list.Items {
		s := list.Items[i].Spec
		if !lastv1alpha1.ApprovalApplies(&list.Items[i], in, now, maxAge) {
			continue
		}
		if s.IntentRef.Name != in.Name || s.Stage != stage || s.SubjectHash != subject {
			continue
		}
		if s.Decision == lastv1alpha1.DecisionDeny {
			return false, &list.Items[i], nil
		}
		approved = true
	}
	return approved, nil, nil
}
