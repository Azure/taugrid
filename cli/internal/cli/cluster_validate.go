// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import "github.com/spf13/cobra"

func newClusterValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate cluster readiness for Tau workloads",
		Long: `Run readiness checks against a Kubernetes cluster.

installation checks the Helm-managed TauGrid control plane; nodes checks GPU
node health; topology checks that the live cluster topology matches Kueue
ResourceFlavor expectations. The nodes check creates and deletes privileged
diagnostic Pods; installation and topology are read-only.`,
		Example: `  tau cluster validate installation
  tau cluster validate nodes --gpu-class h200-141gb
  tau cluster validate topology`,
		Args: cobra.NoArgs,
		RunE: showGroupHelp,
	}
	cmd.AddCommand(
		newClusterValidateInstallationCmd(),
		newClusterValidateNodesCmd(),
		newClusterValidateTopologyCmd(),
	)
	return cmd
}
