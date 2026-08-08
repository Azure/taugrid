// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/queuequota"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/kube"
)

func newWorkspaceQuotaShowCmd() *cobra.Command {
	var namespace, kubeContext, output, clusterQueue string
	cmd := &cobra.Command{
		Use:   "show <workspace>",
		Short: "Show the Kueue quota and node placement backing a workspace",
		Long: `Report the quota a workspace can actually draw on.

For the workspace's ClusterQueue this prints, per ResourceFlavor and per
resource (cpu, memory, and GPU alike), the nominal quota, what is currently
reserved and used, what remains, and the borrowing limit. It also prints each
flavor's nodeLabels and tolerations, so it is clear which node pool a request
will land on, plus the workspace LocalQueue's admitted/pending/reserving counts.

This is read-only reporting. Kueue is a queueing system: a request larger than
the remaining quota is not an error, it simply waits for capacity.

Examples:
  tau workspace quota show pretraining-data
  tau workspace quota show pretraining-data -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("-o/--output must be one of: table, json")
			}
			resolvedContext, restore, err := resolveWorkspaceControlPlaneConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()

			workspace, err := fetchWorkspace(cmd, resolvedContext, namespace, args[0])
			if err != nil {
				return err
			}
			opts := queuequota.FetchOptions{
				Workspace:    args[0],
				Namespace:    workspaceTargetNamespace(workspace),
				LocalQueue:   workspaceLocalQueue(workspace),
				ClusterQueue: firstNonEmptyString(clusterQueue, workspace.Status.Queue.ClusterQueue),
			}
			report, err := queuequota.Fetch(cmd.Context(), kube.New(resolvedContext), opts)
			if err != nil {
				return err
			}
			if output == "json" {
				return writeJSON(cmd.OutOrStdout(), report)
			}
			fmt.Fprint(cmd.OutOrStdout(), queuequota.RenderTable(report))
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", tauworkspace.PlatformNamespace, "namespace containing TauWorkspace objects")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json")
	// The ClusterQueue normally comes from workspace status. The override
	// exists for the window where the controller has not published it yet.
	cmd.Flags().StringVar(&clusterQueue, "cluster-queue", "", "ClusterQueue to report (default: from workspace status)")
	return cmd
}

// workspaceLocalQueue prefers the queue the controller resolved and falls back
// to the declared spec, matching workspaceTargetNamespace.
func workspaceLocalQueue(w tauworkspace.Workspace) string {
	if q := strings.TrimSpace(w.Status.Queue.LocalQueue); q != "" {
		return q
	}
	return strings.TrimSpace(w.Spec.Queue)
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
