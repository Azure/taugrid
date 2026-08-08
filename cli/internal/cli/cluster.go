// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"github.com/spf13/cobra"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Install, uninstall, or validate TauGrid",
		Long: `Manage the TauGrid distribution on an existing Kubernetes cluster.

Tau does not provision or delete cloud infrastructure. Bring or create the
cluster outside Tau, then use Helm-backed install and uninstall commands.
Cluster-scoped desired state belongs to the TauGrid chart; workspace-specific
state is reconciled from TauWorkspace resources by tau-core-controller.`,
	}
	cmd.AddCommand(newClusterInstallCmd(), newClusterUninstallCmd(), newClusterValidateCmd(), newClusterExplainValuesCmd())
	return cmd
}
