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
//
// It adds the teardown finalizer first (D7). A deletion overrides everything
// (design §4.3): for an Intent being deleted, Delete planning starts instead.
func (r *IntentReconciler) startPlanning(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	if !in.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.startDelete(ctx, in, feedback)
	}
	if err := r.ensureFinalizer(ctx, in); err != nil {
		return ctrl.Result{}, err
	}
	if !in.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // deleted meanwhile: the next reconcile tears it down
	}
	stage, err := r.sessionStage(ctx, in)
	if err != nil {
		return ctrl.Result{}, err // the audit log could not be read: retried, never guessed
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = stage
		in.Status.ObservedGeneration = in.Generation
		in.Status.ObservedRequestHash = requestHash(in.Spec.Request)
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
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Asking the LLM for a plan")
	})
}

// sessionStage is the stage a new planning session plans for: Update when the
// audit log holds the Intent's executed plan, Create when it does not (never
// executed, or the index was truncated). The executed plan is never read from
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
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout) // reload and result reads
	defer cancel()
	conv, fresh, err := r.conversation(actx, in)
	switch {
	case errors.Is(err, errBadPolicy):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, err.Error())
	case errors.Is(err, errNoExecutedPlan):
		// The executed plan went missing from the audit log since the stage was
		// chosen: choose it again.
		logf.FromContext(ctx).Info("Could not find the executed plan to update; planning again")
		return r.startPlanning(ctx, in, replanFeedback(in))
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
	if !fresh {
		next, err := r.pendingTurn(actx, in)
		if err != nil {
			return ctrl.Result{}, err // transient: retried with backoff
		}
		if next == "" {
			// Nothing in the Intent's state continues the session, which ends
			// with the model's own answer: start a new one.
			return r.startPlanning(ctx, in, replanFeedback(in))
		}
		conv = conv.With(next)
	}
	used, err := agent.UsedPlans(actx, r.Gateway, intentRef(in))
	switch {
	case status.Code(err) == codes.Unimplemented:
		used = nil // a gateway without QueryAudit cannot say; its replay refusal stays the backstop
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
	feedback := replanFeedback(in)
	pctx, pcancel := context.WithTimeout(ctx, planTimeout)
	defer pcancel()
	turn, conv, seq, err := r.Planner.Next(pctx, intentRef(in), conv, both(answeredCheck(in), rerunCheck(used, feedback)))
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
	session := in.Status.PlanningSessionSeq
	if fresh {
		session = int64(seq)
	}
	return ctrl.Result{}, r.record(ctx, in, turn, conv, session)
}

// record writes the model's turn to the Intent's status and moves the Intent
// to the phase that waits on it: AwaitingApproval for a plan, Clarifying for
// questions, AwaitingDiscoveryApproval for a command. status.stage keeps the
// session's stage throughout (ruling R4): the phase and status.pendingCommand
// mark a discovery command, whose approval is for stage Discovery. The
// conversation stays in memory until a plan ends the session.
func (r *IntentReconciler) record(ctx context.Context, in *lastv1alpha1.Intent, turn agent.Turn, conv agent.Conversation, session int64) error {
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
	err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		applied = in.Generation == gen && in.Status.Phase == lastv1alpha1.PhasePlanning && in.Status.PlanningSessionSeq == prev &&
			planStage(in) == stage
		if !applied {
			return // the spec changed while planning; the next reconcile plans again
		}
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
			setPhase(in, lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval,
				"Waiting for an IntentApproval of plan "+subject+" for stage "+string(stage))
		case agent.KindQuestions:
			in.Status.Questions = turn.Questions
			setPhase(in, lastv1alpha1.PhaseClarifying, ReasonClarifying,
				fmt.Sprintf("Waiting for spec.answers to answer %d questions", len(turn.Questions)))
		case agent.KindCommand:
			in.Status.PendingCommand = pending
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
// or after one of its own steps failed: a Create or Update plan is. A Delete
// plan is not: a deletion raises the generation too, and a failed teardown
// waits for the retry annotation.
func replannable(stage lastv1alpha1.Stage) bool {
	return stage == lastv1alpha1.StageCreate || stage == lastv1alpha1.StageUpdate
}

// awaitApproval waits for an IntentApproval of the current plan for its stage.
// A spec edit plans a Create or Update again. A deletion raises the generation,
// and spec edits no longer matter, so a Delete plan never goes back to
// planning for one.
func (r *IntentReconciler) awaitApproval(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	stage := planStage(in)
	switch {
	case stage == lastv1alpha1.StageDelete && in.Status.Plan == nil:
		return ctrl.Result{}, r.startDelete(ctx, in, "")
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
		if stage == lastv1alpha1.StageDelete {
			setPhase(in, lastv1alpha1.PhaseTearingDown, ReasonTearingDown, "Running teardown plan "+subject)
			return
		}
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+subject)
	})
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
