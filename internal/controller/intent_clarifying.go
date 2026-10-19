package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
)

// clarify waits until spec.answers answers every question, then continues the
// planning session. Answers bump the generation without starting a new
// session; only a changed spec.request does (M3 spec §4.2).
func (r *IntentReconciler) clarify(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if in.Status.ObservedRequestHash != requestHash(in.Spec.Request) || len(in.Status.Questions) == 0 {
		return r.startPlanning(ctx, in, "")
	}
	if !answered(in) {
		return ctrl.Result{}, nil // the Intent watch wakes us when spec.answers changes
	}
	return ctrl.Result{}, r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: continue only on what was checked.
		if in.Status.Phase != lastv1alpha1.PhaseClarifying || in.Status.ObservedRequestHash != requestHash(in.Spec.Request) ||
			!answered(in) {
			return
		}
		in.Status.ObservedGeneration = in.Generation
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Answers received; asking the LLM to continue")
	})
}

// answered reports whether spec.answers holds an answer for every question.
func answered(in *lastv1alpha1.Intent) bool {
	for _, q := range in.Status.Questions {
		if !hasAnswer(in, q.ID) {
			return false
		}
	}
	return true
}

func hasAnswer(in *lastv1alpha1.Intent, id string) bool {
	return slices.ContainsFunc(in.Spec.Answers, func(a lastv1alpha1.Answer) bool { return a.ID == id })
}

// answeredCheck refuses questions whose ids spec.answers already holds: the
// old answers would answer them at once.
func answeredCheck(in *lastv1alpha1.Intent) func(agent.Turn) error {
	return func(t agent.Turn) error {
		for _, q := range t.Questions {
			if hasAnswer(in, q.ID) {
				return fmt.Errorf("question id %q was answered earlier in this conversation; give every question a new id", q.ID)
			}
		}
		return nil
	}
}

// errPlanAlreadyRan marks a plan the gateway would refuse because an earlier
// phase of the Intent already ran it.
var errPlanAlreadyRan = errors.New("the plan already ran")

// ranError is the model-facing message of a rerun refusal; it matches errPlanAlreadyRan.
type ranError string

func (e ranError) Error() string      { return string(e) }
func (ranError) Is(target error) bool { return target == errPlanAlreadyRan }

// rerunCheck refuses a plan whose hash is in used, telling the model why. The
// feedback is the failure the session was asked to address, if any. Delete
// planning never applies it: a retried or interrupted teardown re-proposes a
// permutation of the same commands, often the very plan that ran, and the
// gateway exempts Delete from its run-once rule (teardownCheck applies instead).
func rerunCheck(used map[string]bool, feedback string) func(agent.Turn) error {
	return func(t agent.Turn) error {
		if t.Kind != agent.KindPlan || t.Plan == nil || !used[t.Plan.Hash] {
			return nil
		}
		msg := "This plan is identical to plan " + t.Plan.Hash + ", which already ran"
		if feedback != "" {
			msg += " and failed: " + feedback
		}
		return ranError(msg + ". A plan runs only once: change it so it addresses what went wrong")
	}
}

// both runs each check in turn and returns the first refusal.
func both(checks ...func(agent.Turn) error) func(agent.Turn) error {
	return func(t agent.Turn) error {
		for _, c := range checks {
			if err := c(t); err != nil {
				return err
			}
		}
		return nil
	}
}
