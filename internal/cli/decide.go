package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
	"github.com/moogie-labs/last-operator/internal/plan"
)

// approvablePlanStages are the stages this CLI knows how to put a plan to the
// user for. Any other stage is refused, never approved as Create.
var approvablePlanStages = []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDelete,
	lastv1alpha1.StageRemediate}

// subjectOf returns what the Intent asks its user to decide, with the hash
// the operator states for it: the pending discovery command in
// AwaitingDiscoveryApproval, the plan in AwaitingApproval. A discovery command
// that runs without approval (Auto, Ready reason RunningDiscovery) is none: it is
// already running, so there is nothing to approve or deny. Only verifiedSubject
// makes it something to show or decide.
func subjectOf(in *lastv1alpha1.Intent) (lastv1alpha1.Stage, string, bool) {
	switch {
	case in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval && in.Status.PendingCommand != nil && !runsWithoutApproval(in):
		return lastv1alpha1.StageDiscovery, in.Status.PendingCommand.Hash, true
	case in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval && in.Status.Plan != nil:
		return planStageOf(in), in.Status.Plan.Hash, true
	}
	return "", "", false
}

// planStageOf returns the stage the Intent's plan is for. A plan that carries a
// nonce is a Remediate plan, whatever status.stage says (main spec §7). Else it
// is status.stage, which
// holds the planning session's stage even while a discovery command is pending
// (main spec §7). An empty stage, from before stages were recorded, or the
// Discovery stage an older operator recorded during a session, means Create.
func planStageOf(in *lastv1alpha1.Intent) lastv1alpha1.Stage {
	if in.Status.Plan != nil && in.Status.Plan.Nonce != "" {
		return lastv1alpha1.StageRemediate
	}
	if s := in.Status.Stage; s != "" && s != lastv1alpha1.StageDiscovery {
		return s
	}
	return lastv1alpha1.StageCreate
}

// verifiedSubject returns what the Intent asks its user to decide, like
// subjectOf, with the hash recomputed from the content the CLI shows. The
// operator is untrusted (main spec §3): it could show one plan under the hash
// of another and have the user approve the other. So a subject whose content
// does not hash to its stated hash is never shown or decided: err says so,
// and hash is then the stated one.
func verifiedSubject(in *lastv1alpha1.Intent) (stage lastv1alpha1.Stage, hash string, ok bool, err error) {
	stage, stated, ok := subjectOf(in)
	if !ok {
		return "", "", false, nil
	}
	if stage != lastv1alpha1.StageDiscovery && !slices.Contains(approvablePlanStages, stage) {
		return stage, stated, true, fmt.Errorf("intent %s/%s: the plan is for stage %s, which this kubectl-last cannot approve "+
			"(is kubectl-last as new as the operator?)", in.Namespace, in.Name, clean(string(stage)))
	}
	if stage == lastv1alpha1.StageRemediate && in.Status.Plan.Nonce == "" {
		// status.stage stays Remediate after a DriftCleared return, while the plan is the earlier one.
		return stage, stated, true, fmt.Errorf("intent %s/%s: the plan is the executed plan, not a Remediate plan "+
			"(it carries no nonce); refusing to decide it", in.Namespace, in.Name)
	}
	hash, err = verifyHash(in, stage)
	if err != nil {
		return stage, stated, true, err
	}
	return stage, hash, true, nil
}

// verifyHash recomputes the hash of stage's subject, the pending command or
// the plan, as the gateway does, and returns it if it is the stated one.
func verifyHash(in *lastv1alpha1.Intent, stage lastv1alpha1.Stage) (string, error) {
	var got, stated string
	var err error
	if stage == lastv1alpha1.StageDiscovery {
		pc := in.Status.PendingCommand
		stated = pc.Hash
		got, _, err = plan.DiscoveryHash(pc.Command, pc.Nonce)
	} else {
		stated = in.Status.Plan.Hash
		got, err = plan.Hash(plan.FromStatus(in.Status.Plan))
	}
	if err == nil && got == stated {
		return got, nil
	}
	problem := "does not hash to its stated hash " + clean(stated)
	if err != nil {
		problem = "cannot be hashed (" + clean(err.Error()) + ")"
	}
	return "", fmt.Errorf("intent %s/%s: the %s %s; refusing to show or decide it (is kubectl-last as new as the operator?)",
		in.Namespace, in.Name, subjectNoun(stage), problem)
}

func subjectNoun(stage lastv1alpha1.Stage) string {
	if stage == lastv1alpha1.StageDiscovery {
		return "discovery command"
	}
	return "plan"
}

// decide records the user's decision on exactly subject, the hash they were
// shown, never on whatever the Intent holds by now (M3 spec §5). If the
// Intent has moved on to another subject, the decision matches nothing, and
// decide says so.
func decide(ctx context.Context, k *Kube, w io.Writer, in *lastv1alpha1.Intent, stage lastv1alpha1.Stage,
	subject string, decision lastv1alpha1.Decision, reason string) error {
	user, err := k.WhoAmI(ctx)
	if err != nil {
		return err
	}
	a := &lastv1alpha1.IntentApproval{
		ObjectMeta: metav1.ObjectMeta{GenerateName: in.Name + "-" + strings.ToLower(string(stage)) + "-", Namespace: in.Namespace},
		Spec: lastv1alpha1.IntentApprovalSpec{IntentRef: lastv1alpha1.LocalRef{Name: in.Name}, IntentUID: string(in.UID), Stage: stage,
			SubjectHash: subject, Decision: decision, Reason: reason, ApprovedBy: user},
	}
	if err := k.Client.Create(ctx, a); err != nil {
		return fmt.Errorf("record the decision: %w", err)
	}
	verb := "Approved"
	if decision == lastv1alpha1.DecisionDeny {
		verb = "Denied"
	}
	writef(w, "%s %s %s as %s (IntentApproval %s)\n", verb, subjectNoun(stage), subject, user, a.Name)
	var cur lastv1alpha1.Intent
	if err := k.Client.Get(ctx, client.ObjectKeyFromObject(in), &cur); err != nil {
		return err
	}
	if _, now, ok := subjectOf(&cur); ok && now != subject {
		writef(w, "The Intent changed while you decided: it now asks about %s, so this decision matches nothing. "+
			"See it with kubectl last plan %s -n %s.\n", clean(now), in.Name, in.Namespace)
	}
	return nil
}

// newDecisionCommand builds approve or deny: both decide exactly what
// kubectl last plan shows.
func newDecisionCommand(d Deps, kf *KubeFlags, decision lastv1alpha1.Decision) *cobra.Command {
	var hash, reason, always, stagesList, providersList string
	var cluster bool
	cmd := &cobra.Command{
		Use:   "approve NAME",
		Short: "Approve the plan or discovery command that kubectl last plan shows",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if decision == lastv1alpha1.DecisionDeny && strings.TrimSpace(reason) == "" {
				return errors.New("--reason is required: the model reads it")
			}
			opts, err := allowOptsFrom(always, stagesList, providersList, cluster)
			if err != nil {
				return err
			}
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			in, err := getIntent(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			stage, subject, ok, err := verifiedSubject(in)
			if err != nil {
				return err
			}
			if !ok {
				if in.Status.Plan != nil {
					return fmt.Errorf("intent %s/%s has nothing to decide (phase %s): status.plan is not awaiting a decision, "+
						"a plan can be approved or denied only while it is AwaitingApproval", in.Namespace, in.Name, phaseOf(in))
				}
				return fmt.Errorf("intent %s/%s has nothing to decide (phase %s)", in.Namespace, in.Name, phaseOf(in))
			}
			if hash != "" && hash != subject {
				return fmt.Errorf("intent %s/%s asks about %s, not %s; nothing was recorded", in.Namespace, in.Name, subject, hash)
			}
			renderSubject(d.Out, in, stage, subject, false)
			if always != "" {
				var p *prompter
				if opts.mode == allowPrompt {
					p = newPrompter(d.In, d.Out)
				}
				if err := alwaysAllow(cmd.Context(), k, p, d.Out, in, stage, opts); err != nil {
					return err
				}
			}
			return decide(cmd.Context(), k, d.Out, in, stage, subject, decision, reason)
		},
	}
	cmd.Flags().StringVar(&hash, "hash", "", "decide only if the Intent still asks about this hash")
	if decision == lastv1alpha1.DecisionApprove {
		cmd.Flags().StringVar(&always, "always-allow", "", "also create allow rules for the commands no rule covers: prompt for each, "+
			"or --always-allow=exact|generalized to apply one choice to all of them")
		cmd.Flags().Lookup("always-allow").NoOptDefVal = allowPrompt
		cmd.Flags().StringVar(&stagesList, "stages", "", "with --always-allow, the stages every rule applies to, comma-separated "+
			"(default: the plan's stage, and Delete for teardown commands)")
		cmd.Flags().StringVar(&providersList, "providers", "", "with --always-allow, the providers of a rule for a command that names none, "+
			"when no command in the plan names any, comma-separated")
		cmd.Flags().BoolVar(&cluster, "cluster", false, "with --always-allow, create ClusterCommandRules instead of CommandRules")
	}
	if decision == lastv1alpha1.DecisionDeny {
		cmd.Use = "deny NAME --reason REASON"
		cmd.Short = "Deny the plan or discovery command that kubectl last plan shows"
		cmd.Flags().StringVar(&reason, "reason", "", "why (required; the model reads a discovery command's, and denying a plan fails the Intent)")
	}
	return cmd
}

// allowOptsFrom checks the flags of approve --always-allow.
func allowOptsFrom(always, stagesList, providersList string, cluster bool) (allowOpts, error) {
	opts := allowOpts{mode: always, cluster: cluster}
	if always == "" {
		if stagesList != "" || providersList != "" || cluster {
			return opts, errors.New("--stages, --providers and --cluster need --always-allow")
		}
		return opts, nil
	}
	if !slices.Contains([]string{allowPrompt, allowExact, allowGeneralized}, always) {
		return opts, fmt.Errorf("--always-allow=%s: want exact or generalized, or no value to be asked for each command", clean(always))
	}
	for p := range strings.SplitSeq(providersList, ",") {
		if p = strings.TrimSpace(p); p != "" {
			opts.providers = append(opts.providers, p)
		}
	}
	if stagesList != "" {
		stages, err := parseStages(stagesList)
		if err != nil {
			return opts, err
		}
		opts.stages = stages
	}
	return opts, nil
}
