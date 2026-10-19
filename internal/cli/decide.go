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
var approvablePlanStages = []lastv1alpha1.Stage{lastv1alpha1.StageCreate, lastv1alpha1.StageUpdate, lastv1alpha1.StageDelete}

// subjectOf returns what the Intent asks its user to decide, with the hash
// the operator states for it: the pending discovery command in
// AwaitingDiscoveryApproval, the plan in AwaitingApproval. Only verifiedSubject
// makes it something to show or decide.
func subjectOf(in *lastv1alpha1.Intent) (lastv1alpha1.Stage, string, bool) {
	switch {
	case in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval && in.Status.PendingCommand != nil:
		return lastv1alpha1.StageDiscovery, in.Status.PendingCommand.Hash, true
	case in.Status.Phase == lastv1alpha1.PhaseAwaitingApproval && in.Status.Plan != nil:
		if in.Status.Stage == "" {
			return lastv1alpha1.StageCreate, in.Status.Plan.Hash, true // an Intent from before stages were recorded
		}
		return in.Status.Stage, in.Status.Plan.Hash, true
	}
	return "", "", false
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
	var hash, reason string
	cmd := &cobra.Command{
		Use:   "approve NAME",
		Short: "Approve the plan or discovery command that kubectl last plan shows",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if decision == lastv1alpha1.DecisionDeny && strings.TrimSpace(reason) == "" {
				return errors.New("--reason is required: the model reads it")
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
				return fmt.Errorf("intent %s/%s has nothing to decide (phase %s)", in.Namespace, in.Name, phaseOf(in))
			}
			if hash != "" && hash != subject {
				return fmt.Errorf("intent %s/%s asks about %s, not %s; nothing was recorded", in.Namespace, in.Name, subject, hash)
			}
			renderSubject(d.Out, in, stage, subject, false)
			return decide(cmd.Context(), k, d.Out, in, stage, subject, decision, reason)
		},
	}
	cmd.Flags().StringVar(&hash, "hash", "", "decide only if the Intent still asks about this hash")
	if decision == lastv1alpha1.DecisionDeny {
		cmd.Use = "deny NAME --reason REASON"
		cmd.Short = "Deny the plan or discovery command that kubectl last plan shows"
		cmd.Flags().StringVar(&reason, "reason", "", "why (required; the model reads it, and denying a plan fails the Intent)")
	}
	return cmd
}
