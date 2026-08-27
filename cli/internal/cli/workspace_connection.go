// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/projectcatalog"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func newWorkspaceConnectionCmd() *cobra.Command {
	return newWorkspaceConnectionCmdWithEnsurer(nil)
}

func newWorkspaceConnectionCmdWithEnsurer(ensurer runConnectionEnsurer) *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "connection [PATH]",
		Short: "Connect this project to its configured Tau workspace",
		Long: `Resolve this project's checked-in workspace connection and verify it.

By default Tau resolves credentials, contacts Kubernetes, verifies the
TauWorkspace, LocalQueue, and authorization contract, and stores an isolated
connection for later commands. Use --offline to validate only the repository
mapping and descriptor.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := connectionStartPath(args)
			if err != nil {
				return err
			}
			project, discovery, err := resolveProjectConnection(start)
			if err != nil {
				return err
			}
			if offline {
				printOfflineConnection(cmd, project, discovery)
				return nil
			}
			activeEnsurer := ensurer
			if activeEnsurer == nil {
				activeEnsurer = defaultRunConnectionEnsurer(cmd)
			}
			connection, err := ensureRunConnection(cmd.Context(), activeEnsurer, runConnectionSource{
				StartDir:  start,
				Discovery: &discovery,
				Project:   project,
			})
			if err != nil {
				return err
			}
			printActiveConnection(cmd, project, displayConnectionPath(discovery), connection)
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "validate repository connection configuration without contacting Kubernetes")
	return cmd
}

func connectionStartPath(args []string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	return os.Getwd()
}

func resolveProjectConnection(start string) (string, workspaceconnection.Discovery, error) {
	repository, err := projectcatalog.Discover(start)
	if err != nil {
		return "", workspaceconnection.Discovery{}, err
	}
	if repository.Catalog == nil {
		discovery, err := workspaceconnection.Discover(start)
		return "", discovery, err
	}
	project, err := repository.Catalog.SelectLifecycleProject("", start)
	if err != nil {
		return "", workspaceconnection.Discovery{}, fmt.Errorf("%w; pass a path inside the intended project", err)
	}
	return project.Name, project.Connection, nil
}

func printOfflineConnection(cmd *cobra.Command, project string, discovery workspaceconnection.Discovery) {
	fmt.Fprintln(cmd.OutOrStdout(), "Workspace connection configuration is valid.")
	printConnectionIdentity(cmd, project, discovery.Descriptor.Workspace, displayConnectionPath(discovery))
	fmt.Fprintln(cmd.OutOrStdout(), "Cluster access was not checked.")
}

func printActiveConnection(cmd *cobra.Command, project, descriptorPath string, connection workspaceconnection.ActiveConnection) {
	fmt.Fprintln(cmd.OutOrStdout(), "Connected.")
	printConnectionIdentity(cmd, project, connection.Workspace, descriptorPath)
	fmt.Fprintf(cmd.OutOrStdout(), "Status:        Ready\n")
	fmt.Fprintf(cmd.OutOrStdout(), "Namespace:     %s\n", connection.Namespace)
	fmt.Fprintf(cmd.OutOrStdout(), "Queue:         %s\n", connection.Queue)
	fmt.Fprintf(cmd.OutOrStdout(), "Authorization: %s\n", connection.AuthorizationMode)
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
