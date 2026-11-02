package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
	"github.com/moogie-labs/last-operator/internal/rules"
	"github.com/moogie-labs/last-operator/internal/suggest"
)

// planEntry is one command a plan authorizes, as rules.PlanCoverage names it,
// with the stages a rule suggested for it lists (M7 design §4.1).
type planEntry struct {
	id     string
	cmd    lastv1alpha1.Command
	stage  lastv1alpha1.Stage // the stage it runs at
	stages []lastv1alpha1.Stage
}

// planEntries lists every command a plan of stage authorizes, in coverage
// order: its steps and, for a Create or Update plan, each resource's probes and
// teardown. Steps and probes of a Create or Update plan get [stage, Remediate],
// so the rule also lets its remediation run; steps of any other plan get
// [stage]; teardown gets [Delete].
func planEntries(stage lastv1alpha1.Stage, body plan.Body) []planEntry {
	own := []lastv1alpha1.Stage{stage}
	createOrUpdate := stage == lastv1alpha1.StageCreate || stage == lastv1alpha1.StageUpdate
	if createOrUpdate {
		own = []lastv1alpha1.Stage{stage, lastv1alpha1.StageRemediate}
	}
	out := make([]planEntry, 0, len(body.Steps))
	for _, s := range body.Steps {
		out = append(out, planEntry{id: s.ID, cmd: s, stage: stage, stages: own})
	}
	if !createOrUpdate {
		return out
	}
	for ri, r := range body.Resources {
		for _, p := range r.Probes {
			out = append(out, planEntry{id: "probe:" + p.ID, cmd: p.Command, stage: stage, stages: own})
		}
		for i, c := range r.Teardown {
			out = append(out, planEntry{id: fmt.Sprintf("teardown:%d.%d", ri, i), cmd: c, stage: lastv1alpha1.StageDelete,
				stages: []lastv1alpha1.Stage{lastv1alpha1.StageDelete}})
		}
	}
	return out
}

// planTime is the PlanTime trigger (M7 design §4.1), run once for a plan that
// waits for an approval. It asks the model, in one suggest call, for a pattern
// for each entry of the plan no allow rule covers, builds each answer into a
// rule as M6 R13 does, creates whatever survives SuggestionCreator.Create, and
// marks the plan suggested with the names it created or found in
// status.suggestedRules. A failed call raises SuggestionFailed and is not
// retried; the plan is marked suggested all the same. Nothing here fails the
// Intent or holds up its approval. An Intent deleted meanwhile ends the step;
// the suggestions already created stay.
func (r *IntentReconciler) planTime(ctx context.Context, in *lastv1alpha1.Intent) error {
	if r.Suggestions == nil || in.Status.Plan == nil {
		return nil
	}
	log := logf.FromContext(ctx)
	stage, subject := planStage(in), in.Status.Plan.Hash
	body := plan.FromStatus(in.Status.Plan)
	if stage != lastv1alpha1.StageCreate && stage != lastv1alpha1.StageUpdate {
		body.Resources = nil // only the steps of such a plan run (rules.PlanCoverage)
	}
	rs, err := r.readRules(ctx, in.Namespace)
	if err != nil {
		return err
	}
	_, uncovered := rules.PlanCoverage(rs, stage, body)
	byID := map[string]planEntry{}
	var asked []suggest.Entry
	for _, e := range planEntries(stage, body) {
		if !slices.Contains(uncovered, e.id) {
			continue
		}
		if _, ok := suggest.RuleSpecFor(body, e.cmd, e.stages, nil); !ok {
			continue // no rule can hold it, or no command names a provider
		}
		byID[e.id] = e
		asked = append(asked, suggest.Entry{ID: e.id, Stage: e.stage, Argv: e.cmd.Run.Argv})
	}
	var names []string
	if len(asked) > 0 {
		answers, err := r.Suggestions.Ask(ctx, r.Gateway, intentRef(in), asked)
		if err != nil {
			log.Info("Could not get rule suggestions for the plan", "plan", subject, "reason", err.Error())
			r.Recorder.Eventf(in, nil, corev1.EventTypeWarning, ReasonSuggestionFailed, actionSuggest,
				"The suggest call for plan %s failed: %s", subject, clip(describe(err)))
		}
		for _, a := range answers {
			e, ok := byID[a.Entry]
			if !ok {
				continue // an entry the call did not ask about
			}
			spec, _ := suggest.RuleSpecFor(body, e.cmd, e.stages, nil)
			spec.Match = lastv1alpha1.ArgvPattern{Argv: a.Argv}
			p := suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeNamespace, Namespace: in.Namespace,
				Trigger: lastv1alpha1.TriggerPlanTime, Rationale: a.Rationale, Spec: spec, Examples: []lastv1alpha1.Command{e.cmd}}
			name, _, err := r.Suggestions.Create(ctx, p, in)
			switch {
			case err != nil:
				log.Info("Could not create a rule suggestion", "entry", e.id, "reason", err.Error())
			case name != "" && !slices.Contains(names, name):
				names = append(names, name)
			}
		}
	}
	err = r.update(ctx, in, func(in *lastv1alpha1.Intent) {
		if in.Status.Phase != lastv1alpha1.PhaseAwaitingApproval || in.Status.Plan == nil || in.Status.Plan.Hash != subject {
			return // a new plan gets its own step
		}
		in.Status.Plan.Suggested = true
		in.Status.SuggestedRules = names
	})
	if apierrors.IsNotFound(err) {
		return nil // deleted meanwhile
	}
	return err
}
