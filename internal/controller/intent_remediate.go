package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/agent"
	"github.com/moogie-labs/last-operator/internal/gatewayapi/gatewayv1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// maxRemediations is how many consecutive remediations drift may start; the
// next drift fails the Intent with RemediationFailed (D11).
const maxRemediations = 3

// msgSpecChanged ends a phase that a spec edit abandons.
const msgSpecChanged = "Aborted: the spec changed"

// startRemediate starts a Remediate session for a Drifted Intent, unless it
// already remediated maxRemediations times in a row. It counts the drift
// episode here, once: a failed step's replan is the same episode, and a
// confirming probe run that finds no drift gives the count back
// (driftCleared). It keeps observedGeneration, so a spec edit made meanwhile
// still plans an Update, and it keeps status.plan, whose resources are the
// executed plan's: the confirming probe run is recorded against them, and
// probes keep following them if the drift has cleared.
func (r *IntentReconciler) startRemediate(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if n := in.Status.Remediations; n >= maxRemediations {
		return ctrl.Result{}, r.fail(ctx, in, ReasonRemediationFailed,
			fmt.Sprintf("The Intent drifted again after %d consecutive remediations%s", n, specEditHint(in)))
	}
	r.sessions.drop(client.ObjectKeyFromObject(in))
	gen, capped := in.Generation, false
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		// A conflict retry re-reads the Intent: start only from the drift that
		// was seen, and never past the cap. Each attempt decides afresh.
		capped = false
		if in.Status.Phase != lastv1alpha1.PhaseDrifted || in.Generation != gen || in.Generation != in.Status.ObservedGeneration {
			return
		}
		if capped = in.Status.Remediations >= maxRemediations; capped {
			return
		}
		in.Status.Remediations++
		resetSession(in, lastv1alpha1.StageRemediate, "")
		setPhase(in, lastv1alpha1.PhasePlanning, ReasonPlanning, "Confirming the drift before asking the LLM for a Remediate plan")
	}); err != nil || !capped {
		return ctrl.Result{}, err
	}
	return r.startRemediate(ctx, in) // in holds the Intent as stored: it fails at the cap
}

// errDriftCleared marks a Remediate session whose confirming probe run passed:
// there is nothing to remediate.
var errDriftCleared = errors.New("every probe passed; the drift has cleared")

// errProbeInFlight marks a confirming probe run the gateway refused because
// another run for the Intent is in flight; it is tried again shortly.
var errProbeInFlight = errors.New("a probe run for the Intent is already in flight")

// planningFailure is a reason a planning session cannot go on: the Intent fails
// with PlanningFailed and this message.
type planningFailure string

func (f planningFailure) Error() string { return string(f) }

// msgNothingToRemediate is why a Remediate fails when the audit log does not
// hold the executed plan (never executed, or an index trimmed past it before the
// gateway kept the executed plan's entries).
const msgNothingToRemediate planningFailure = "no executed plan found to remediate"

// remediateMessage confirms the drift with a probe run (ruling R5) and
// returns the Remediate session's first message: the executed plan, read
// from the audit log, and the probes that fail, with their output. The run's
// outcomes are recorded on the ManagedResources of the executed plan. It
// returns errDriftCleared when every probe passes.
func (r *IntentReconciler) remediateMessage(ctx context.Context, in *lastv1alpha1.Intent, env agent.Environment) (string, error) {
	executed, err := r.executedBody(ctx, in)
	if err != nil {
		return "", err
	}
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	resp, err := r.Gateway.RunProbes(pctx, &gatewayv1.RunProbesRequest{Intent: intentRef(in)})
	cancel()
	switch {
	case status.Code(err) == codes.Aborted:
		return "", errProbeInFlight
	case terminal(err):
		return "", planningFailure("The gateway refused the probe run that confirms the drift: " + describe(err))
	case err != nil:
		return "", err
	}
	// Recorded against the executed plan's resources, which the gateway probed.
	probed := in.DeepCopy()
	probed.Status.Plan = &lastv1alpha1.Plan{Resources: executed.Resources}
	if err := applyProbeOutcomes(ctx, r.Client, r.Reader, probed, resp.GetOutcomes(), start); err != nil {
		return "", err
	}
	failures := probeFailures(executed, resp.GetOutcomes())
	if len(failures) == 0 {
		return "", errDriftCleared
	}
	first, err := agent.RemediateMessage(agent.Remediation{Namespace: in.Namespace, Name: in.Name, Text: in.Spec.Request,
		Executed: executed, Failures: failures, Env: env, Feedback: replanFeedback(in)})
	if err != nil {
		return "", planningFailure("The executed plan cannot be shown to the model: " + err.Error())
	}
	return first, nil
}

// executedBody reads the executed plan from the audit log, never from status.
// Its absence, or a gateway that cannot read the audit log, fails the
// Remediate; other errors are returned for a retry.
func (r *IntentReconciler) executedBody(ctx context.Context, in *lastv1alpha1.Intent) (plan.Body, error) {
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	defer cancel()
	executed, _, found, err := agent.ExecutedPlan(actx, r.Gateway, intentRef(in))
	switch {
	case err != nil && status.Code(err) != codes.Unimplemented:
		return plan.Body{}, err
	case err != nil || !found:
		return plan.Body{}, msgNothingToRemediate
	}
	return executed, nil
}

// probeFailures describes each probe that did not pass, by the resource and
// assert the executed plan gives it.
func probeFailures(executed plan.Body, outcomes []*gatewayv1.ProbeOutcome) []agent.ProbeFailure {
	type source struct{ resource, assert string }
	byID := map[string]source{}
	for i, res := range executed.Resources {
		name := res.Description
		if name == "" {
			name = fmt.Sprintf("resource %d", i)
		}
		for _, p := range res.Probes {
			byID[p.ID] = source{resource: name, assert: p.Assert}
		}
	}
	var failures []agent.ProbeFailure
	for _, o := range outcomes {
		if o.GetPassed() {
			continue
		}
		s, ok := byID[o.GetId()]
		if !ok {
			s.resource = "a resource the executed plan does not list"
		}
		failures = append(failures, agent.ProbeFailure{Resource: s.resource, ProbeID: o.GetId(), Assert: s.assert,
			Result: o.GetResult(), Error: o.GetError()})
	}
	return failures
}

// driftCleared returns an Intent whose confirming probe run passed to Ready
// with a DriftCleared event, planning nothing. The first session of a drift
// episode gives back the count startRemediate took, since nothing was
// remediated; a replan after a failed Remediate step does not, since that
// Remediate ran.
//
// The session may be a fresh one, or one that started over because its
// conversation could not be reloaded: either way it is the session the
// Intent's status recorded when it was built, which the write checks.
func (r *IntentReconciler) driftCleared(ctx context.Context, in *lastv1alpha1.Intent) error {
	giveBack, prev := replanFeedback(in) == "", in.Status.PlanningSessionSeq
	const why = "The probe run that confirms the drift passed; nothing to remediate"
	applied := false
	if err := r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		applied = in.Status.Phase == lastv1alpha1.PhasePlanning && planStage(in) == lastv1alpha1.StageRemediate &&
			in.Status.PlanningSessionSeq == prev
		if !applied {
			return
		}
		if giveBack && in.Status.Remediations > 0 {
			in.Status.Remediations--
		}
		setPhase(in, lastv1alpha1.PhaseReady, ReasonDriftCleared, why)
	}); err != nil || !applied {
		return err
	}
	logf.FromContext(ctx).Info("Found no drift to remediate")
	r.Recorder.Eventf(in, nil, corev1.EventTypeWarning, ReasonDriftCleared, "Remediate", "%s", why)
	return nil
}

// Corrections for a Remediate turn (ruling 3); the model reads them.
const (
	msgRemediateQuestions correction = "A Remediate cannot ask questions; plan from the probe results, or run a discovery command"
	msgRemediateResources correction = `A Remediate plan answers "resources": []; what is managed does not change`
)

// correction is a check's refusal of a model turn, sent to the model as written.
type correction string

func (c correction) Error() string { return string(c) }

// remediateCheck refuses a Remediate turn that asks questions or names
// resources: a remediation repairs what the executed plan manages and never
// changes it (design D5). Discovery commands are allowed.
func remediateCheck(t agent.Turn) error {
	switch {
	case t.Kind == agent.KindQuestions:
		return msgRemediateQuestions
	case t.Kind == agent.KindPlan && t.Plan != nil && len(t.Plan.Resources) > 0:
		return msgRemediateResources
	}
	return nil
}

// completeRemediate makes the model's Remediate plan the subject to approve
// (rulings R2, R3): it copies in the executed plan's resources, read from the
// audit log, sets a fresh nonce, so the same repair for a recurring drift
// needs a new approval, and hashes the result.
func (r *IntentReconciler) completeRemediate(ctx context.Context, in *lastv1alpha1.Intent, p *lastv1alpha1.Plan) error {
	executed, err := r.executedBody(ctx, in)
	if err != nil {
		return err
	}
	nonce, err := agent.NewNonce()
	if err != nil {
		return err
	}
	p.Resources, p.Nonce = executed.Resources, nonce
	body := plan.FromStatus(p)
	if err := plan.ValidateRemediate(body, executed); err != nil {
		return planningFailure("The Remediate plan cannot carry the executed plan: " + err.Error())
	}
	if p.Hash, err = plan.Hash(body); err != nil {
		return planningFailure(err.Error())
	}
	return nil
}

// abandonRemediation abandons a remediation for a spec edit (design §4.3):
// it ends any phase the remediation holds open as failed, then plans again,
// which plans an Update.
func (r *IntentReconciler) abandonRemediation(ctx context.Context, in *lastv1alpha1.Intent) (ctrl.Result, error) {
	if err := r.abortPhase(ctx, in, msgSpecChanged); err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("Abandoned the remediation for a spec edit")
	return r.startPlanning(ctx, in, "")
}

// probedPlanHash returns the hash RunProbes reports when it probes the plan in
// the Intent's status: the plan's own hash, or, for a Remediate plan (the one
// plan that carries a nonce), the executed plan's, read from the audit log,
// since a Remediate never replaces the executed plan. It returns "" when the
// audit log does not hold the executed plan.
func probedPlanHash(ctx context.Context, a agent.Auditor, in *lastv1alpha1.Intent) (string, error) {
	if in.Status.Plan.Nonce == "" {
		return in.Status.Plan.Hash, nil
	}
	actx, cancel := context.WithTimeout(ctx, phaseCallTimeout)
	defer cancel()
	_, hash, found, err := agent.ExecutedPlan(actx, a, intentRef(in))
	switch {
	case err != nil && status.Code(err) != codes.Unimplemented:
		return "", err
	case err != nil || !found:
		return "", nil
	}
	return hash, nil
}

// probedSinceReady reports whether a scheduled probe run passed on every
// probed ManagedResource of the Intent's plan after the Intent last became
// Ready, which resets status.remediations (D11). The Verifying run started
// before that, so it never counts.
func probedSinceReady(in *lastv1alpha1.Intent, mrs []lastv1alpha1.ManagedResource) bool {
	ready := meta.FindStatusCondition(in.Status.Conditions, CondReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return false
	}
	probed := 0
	for i := range mrs {
		if len(mrs[i].Spec.Probes) == 0 {
			continue // never probed, so never in sync
		}
		t := mrs[i].Status.LastProbeTime
		if !meta.IsStatusConditionTrue(mrs[i].Status.Conditions, CondInSync) || t == nil || !t.After(ready.LastTransitionTime.Time) {
			return false
		}
		probed++
	}
	return probed > 0
}
