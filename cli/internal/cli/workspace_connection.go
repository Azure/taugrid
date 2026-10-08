// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/onboarding"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func newWorkspaceConnectionCmd() *cobra.Command {
	return newWorkspaceConnectionCmdWithEnsurer(nil)
}

func newWorkspaceConnectionCmdWithEnsurer(ensurer runConnectionEnsurer) *cobra.Command {
	var projectName string
	cmd := &cobra.Command{
		Use:   "connection [PATH]",
		Short: "Connect this project to its configured Tau workspace",
		Long: `Resolve this project's effective workspace connection and verify it.

The effective connection is a checked-in descriptor when present, otherwise
the exact machine-local project assignment created by the assign subcommand.
Tau resolves credentials, contacts Kubernetes, verifies the TauWorkspace,
LocalQueue, and authorization contract, and stores an isolated connection for
later commands. A repository's first connection must be reviewed and trusted
from an interactive terminal before Tau accesses credentials or the cluster.`,
		Example: `  tau workspace connection
  tau workspace connection ./my-project`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := connectionStartPath(args)
			if err != nil {
				return err
			}
			target, discovery, _, err := effectiveWorkspaceConnection(start, projectName)
			if err != nil {
				return err
			}
			activeEnsurer := ensurer
			if activeEnsurer == nil {
				activeEnsurer = defaultRunConnectionEnsurer(cmd)
			}
			connection, err := ensureRunConnection(cmd.Context(), activeEnsurer, runConnectionSource{
				StartDir:  start,
				Discovery: &discovery,
				Project:   target.Project,
			})
			if err != nil {
				return onboarding.Explain(err)
			}
			if provider, ok := activeEnsurer.(workspaceConfigDirectoryProvider); ok {
				configDir, configErr := provider.ConfigDirectory()
				if configErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not update current workspace connection: %v\n", configErr)
				} else if cacheErr := persistActiveWorkspaceCache(
					configDir,
					&discovery,
					connection,
					workspacePlacement{
						Workspace:  connection.Workspace,
						Namespace:  connection.Namespace,
						LocalQueue: connection.Queue,
					},
					time.Now(),
				); cacheErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not update current workspace connection: %v\n", cacheErr)
				}
			}
			printActiveConnection(cmd, target.Project, displayConnectionPath(discovery), connection)
			return nil
		},
	}
	cmd.Flags().StringVar(&projectName, "project", "", "Tau project name in tau.projects.yaml")
	cmd.AddCommand(
		newWorkspaceConnectionAssignCmd(),
		newWorkspaceConnectionInspectCmd(),
		newWorkspaceConnectionClearCmd(),
	)
	return cmd
}

func connectionStartPath(args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	return os.Getwd()
}

func printActiveConnection(cmd *cobra.Command, project, descriptorPath string, connection workspaceconnection.ActiveConnection) {
	fmt.Fprintln(cmd.OutOrStdout(), "Connected.")
	printConnectionIdentity(cmd, project, connection.Workspace, descriptorPath)
	fmt.Fprintf(cmd.OutOrStdout(), "Status:        Ready\n")
	fmt.Fprintf(cmd.OutOrStdout(), "Namespace:     %s\n", connection.Namespace)
	fmt.Fprintf(cmd.OutOrStdout(), "Queue:         %s\n", connection.Queue)
	fmt.Fprintf(cmd.OutOrStdout(), "Authorization: %s\n", connection.AuthorizationMode)
	fmt.Fprintln(cmd.OutOrStdout(), "Ready:         tau run can now use this workspace.")
}

func printConnectionIdentity(cmd *cobra.Command, project, workspace, descriptorPath string) {
	if strings.TrimSpace(project) != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Project:       %s\n", project)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Workspace:     %s\n", workspace)
	fmt.Fprintf(cmd.OutOrStdout(), "Descriptor:    %s\n", descriptorPath)
}

func displayConnectionPath(discovery workspaceconnection.Discovery) string {
	relative, err := filepath.Rel(discovery.RepositoryRoot, discovery.Path)
	if err == nil && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(relative)
	}
	return discovery.Path
}
