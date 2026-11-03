package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// planTime is the PlanTime trigger (M7 design §4.1), run by a PlanTime worker
// for a plan that waited for an approval when its reconcile queued the step. It
// reads the Intent uncached and runs suggestPlan when the plan with job.hash
// still waits for an approval and is not marked suggested; otherwise the step
// is dropped. A deleted Intent ends it quietly.
func (s *SuggestRunner) planTime(ctx context.Context, job planJob) error {
	if s.Creator == nil {
		return nil
	}
	in := &lastv1alpha1.Intent{}
	switch err := s.Creator.Reader.Get(ctx, job.key, in); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if !waitsWith(in, job.hash) || in.Status.Plan.Suggested {
		return nil // a new plan gets its own step, and a step runs once per plan
	}
	return s.suggestPlan(ctx, in)
}

// waitsWith reports whether the Intent's plan with hash waits for an approval.
func waitsWith(in *lastv1alpha1.Intent, hash string) bool {
	return in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval && in.Status.Plan != nil && in.Status.Plan.Hash == hash
}

// suggestPlan asks the model, in one suggest call, for a pattern for each entry
// of the Intent's plan no allow rule covers, builds each answer into a rule as
// M6 R13 does, creates whatever survives SuggestionCreator.Create, and marks
// the plan suggested with the names it created or found in
// status.suggestedRules, if the same plan still waits for an approval. A failed
// call raises SuggestionFailed and is not retried; the plan is marked suggested
// all the same. Nothing here fails the Intent, and it runs off the Intent's
// reconcile, so it never holds up its approval. An Intent deleted meanwhile
// ends the step; the suggestions already created stay.
func (s *SuggestRunner) suggestPlan(ctx context.Context, in *lastv1alpha1.Intent) error {
	if s.Creator == nil || in.Status.Plan == nil {
		return nil
	}
	log := logf.FromContext(ctx)
	stage, subject := planStage(in), in.Status.Plan.Hash
	body := plan.FromStatus(in.Status.Plan)
	if stage != lastv1alpha1.StageCreate && stage != lastv1alpha1.StageUpdate {
		body.Resources = nil // only the steps of such a plan run (rules.PlanCoverage)
	}
	rs, err := s.Creator.readRules(ctx, suggest.Proposal{TargetScope: lastv1alpha1.RuleScopeNamespace, Namespace: in.Namespace})
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
		answers, err := s.Creator.Ask(ctx, s.Gateway, intentRef(in), asked)
		if err != nil {
			log.Info("Could not get rule suggestions for the plan", "intent", client.ObjectKeyFromObject(in), "plan", subject,
				"reason", err.Error())
			s.Creator.event(in, nil, corev1.EventTypeWarning, ReasonSuggestionFailed, "The suggest call for plan %s failed: %s",
				subject, describe(err))
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
			name, _, err := s.Creator.Create(ctx, p, in)
			switch {
			case err != nil:
				log.Info("Could not create a rule suggestion", "intent", client.ObjectKeyFromObject(in), "entry", e.id,
					"reason", err.Error())
			case name != "" && !slices.Contains(names, name):
				names = append(names, name)
			}
		}
	}
	key := client.ObjectKeyFromObject(in)
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur := &lastv1alpha1.Intent{}
		if err := s.Creator.Reader.Get(ctx, key, cur); err != nil {
			return err
		}
		if !waitsWith(cur, subject) {
			return nil // a new plan gets its own step
		}
		cur.Status.Plan.Suggested = true
		cur.Status.SuggestedRules = names
		return s.Client.Status().Update(ctx, cur)
	})
	if apierrors.IsNotFound(err) {
		return nil // deleted meanwhile
	}
	return err
}
