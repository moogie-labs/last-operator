package cli

import (
	"github.com/spf13/cobra"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func newPlanCommand(d Deps, kf *KubeFlags) *cobra.Command {
	var files bool
	cmd := &cobra.Command{
		Use:   "plan NAME",
		Short: "Show an Intent's open questions, pending discovery command or plan (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := d.Kube(*kf)
			if err != nil {
				return err
			}
			in, err := getIntent(cmd.Context(), k, args[0])
			if err != nil {
				return err
			}
			printStatus(d.Out, in)
			if in.Status.Phase == lastv1alpha1.PhaseClarifying && len(in.Status.Questions) > 0 {
				for _, q := range in.Status.Questions {
					writeln(d.Out)
					renderQuestion(d.Out, q)
				}
				writef(d.Out, "\nAnswer them with kubectl last answer %s -n %s\n", in.Name, in.Namespace)
				return nil
			}
			var stage lastv1alpha1.Stage
			switch {
			case in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval && in.Status.PendingCommand != nil:
				stage = lastv1alpha1.StageDiscovery
			case in.Status.Plan != nil:
				stage = planStageOf(in)
			default:
				writeln(d.Out, "No plan or pending command yet.")
				return nil
			}
			hash, err := verifyHash(in, stage)
			if err != nil {
				writef(d.Out, "Warning: %s\n", err)
				return nil
			}
			renderSubject(d.Out, in, stage, hash, files)
			if decidable, _, ok, err := verifiedSubject(in); ok && err == nil && decidable == stage {
				printDecisionCommands(d.Out, in, hash)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&files, "files", false, "show the contents of the files, not only their paths and sizes")
	return cmd
}
