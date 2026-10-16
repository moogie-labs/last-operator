package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/moogie-labs/last-operator/internal/plan"
)

// planTimeout bounds one model turn, corrections included.
const planTimeout = 10 * time.Minute

// startPlanning discards the current plan and planning session and starts a
// new session for the current spec. feedback, when set, is a failure the new
// plan must address.
func (r *IntentReconciler) startPlanning(ctx context.Context, in *lastv1alpha1.Intent, feedback string) (ctrl.Result, error) {
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		in.Status.Stage = lastv1alpha1.StageCreate
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
	if in.Generation != in.Status.ObservedGeneration {
		return r.startPlanning(ctx, in, "")
	}
	conv, fresh := r.conversation(in)
	if !fresh {
		next := pendingTurn(in)
		if next == "" {
			// Nothing in the Intent's state continues the session, which ends
			// with the model's own answer: start a new one.
			return r.startPlanning(ctx, in, replanFeedback(in))
		}
		conv = conv.With(next)
	}
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	turn, conv, seq, err := r.Planner.Next(pctx, intentRef(in), conv, answeredCheck(in))
	var capped *agent.CapError
	switch {
	case errors.Is(err, agent.ErrNoTurn) || errors.Is(err, agent.ErrNoSeq) || errors.As(err, &capped) || terminal(err):
		return ctrl.Result{}, r.fail(ctx, in, ReasonPlanningFailed, describe(err))
	case err != nil:
		return ctrl.Result{}, err // transient: retried with backoff
	}
	session := in.Status.PlanningSessionSeq
	if fresh {
		session = int64(seq)
	}
	return ctrl.Result{}, r.record(ctx, in, turn, conv, session)
}

// record writes the model's turn to the Intent's status and moves the Intent
// to the phase that waits on it: AwaitingApproval for a plan, Clarifying for
// questions, AwaitingDiscoveryApproval for a command. The conversation stays
// in memory until a plan ends the session.
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
	gen, prev := in.Generation, in.Status.PlanningSessionSeq
	applied := false
	err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		applied = in.Generation == gen && in.Status.Phase == lastv1alpha1.PhasePlanning && in.Status.PlanningSessionSeq == prev
		if !applied {
			return // the spec changed while planning; the next reconcile plans again
		}
		in.Status.PlanningSessionSeq = session
		in.Status.Questions = nil
		in.Status.PendingCommand = nil
		meta.RemoveStatusCondition(&in.Status.Conditions, CondExecuted)
		switch turn.Kind {
		case agent.KindPlan:
			in.Status.Stage = lastv1alpha1.StageCreate
			in.Status.Plan = turn.Plan
			setCondition(&in.Status.Conditions, in.Generation, CondPlanned, metav1.ConditionTrue, ReasonPlanWritten, "Plan "+subject)
			setPhase(in, lastv1alpha1.PhaseAwaitingApproval, ReasonAwaitingApproval,
				"Waiting for an IntentApproval of plan "+subject+" for stage Create")
		case agent.KindQuestions:
			in.Status.Questions = turn.Questions
			setPhase(in, lastv1alpha1.PhaseClarifying, ReasonClarifying,
				fmt.Sprintf("Waiting for spec.answers to answer %d questions", len(turn.Questions)))
		case agent.KindCommand:
			in.Status.Stage = lastv1alpha1.StageDiscovery
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
	gen, subject := in.Generation, in.Status.Plan.Hash
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: run only the plan that was approved.
		if in.Generation != gen || in.Status.Phase != lastv1alpha1.PhaseAwaitingApproval ||
			in.Status.Plan == nil || in.Status.Plan.Hash != subject {
			return
		}
		setPhase(in, lastv1alpha1.PhaseExecuting, ReasonExecuting, "Running plan "+in.Status.Plan.Hash)
	})
}
