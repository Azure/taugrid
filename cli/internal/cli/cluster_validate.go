package cli

import "github.com/spf13/cobra"

func newClusterValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate cluster readiness for Tau workloads",
	}
	cmd.AddCommand(
		newClusterValidateInstallationCmd(),
		newClusterValidateNodesCmd(),
		newClusterValidateTopologyCmd(),
	)
	return cmd
}
