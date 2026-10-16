package cli

import (
	"github.com/spf13/cobra"

	lastv1alpha1 "github.com/moogie-labs/last-operator/api/v1alpha1"
)

func newPlanCommand(d Deps, kf *KubeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "plan NAME",
		Short: "Show an Intent's pending discovery command or its plan (read-only)",
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
			switch {
			case in.Status.Phase == lastv1alpha1.PhaseAwaitingDiscoveryApproval && in.Status.PendingCommand != nil:
				renderPending(d.Out, in.Status.PendingCommand)
			case in.Status.Plan != nil:
				renderPlan(d.Out, in.Status.Plan)
			default:
				writeln(d.Out, "No plan or pending command yet.")
			}
			return nil
		},
	}
}
