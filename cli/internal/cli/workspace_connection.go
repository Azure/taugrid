// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type workspaceConnectionInspection struct {
	Path           string                         `json:"path" yaml:"path"`
	RepositoryRoot string                         `json:"repositoryRoot" yaml:"repositoryRoot"`
	Digest         string                         `json:"digest" yaml:"digest"`
	ConnectionKey  string                         `json:"connectionKey" yaml:"connectionKey"`
	Descriptor     workspaceconnection.Descriptor `json:"descriptor" yaml:"descriptor"`
}

func newWorkspaceConnectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connection",
		Short: "Inspect Tau repository workspace connection metadata",
	}
	cmd.AddCommand(newWorkspaceConnectionInspectCmd())
	return cmd
}

func newWorkspaceConnectionInspectCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "inspect [PATH]",
		Short: "Optionally inspect a workspace connection descriptor offline",
		Long: "Optionally discover and validate a non-secret workspace connection descriptor without contacting the cluster.\n" +
			"`tau run` discovers the descriptor automatically. On first cluster-backed use, Tau obtains normal AKS user credentials, verifies the live workspace contract, and pins local connection state.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := ""
			if len(args) == 1 {
				start = args[0]
			}
			discovery, err := workspaceconnection.Discover(start)
			if err != nil {
				return err
			}
			inspection := workspaceConnectionInspection{
				Path:           discovery.Path,
				RepositoryRoot: discovery.RepositoryRoot,
				Digest:         discovery.Digest,
				ConnectionKey:  workspaceconnection.ConnectionKey(discovery.Descriptor),
				Descriptor:     discovery.Descriptor,
			}
			switch output {
			case "json":
				return writeJSON(cmd.OutOrStdout(), inspection)
			case "yaml":
				raw, err := yaml.Marshal(inspection)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			case "table":
				fmt.Fprintf(cmd.OutOrStdout(), "Workspace: %s\n", inspection.Descriptor.Workspace)
				fmt.Fprintf(cmd.OutOrStdout(), "Cluster:   %s\n", inspection.Descriptor.Cluster.ContextName)
				fmt.Fprintf(cmd.OutOrStdout(), "Resource:  %s\n", inspection.Descriptor.Cluster.ResourceID)
				fmt.Fprintf(cmd.OutOrStdout(), "Tenant:    %s\n", inspection.Descriptor.Identity.TenantID)
				fmt.Fprintf(cmd.OutOrStdout(), "Path:      %s\n", inspection.Path)
				fmt.Fprintf(cmd.OutOrStdout(), "Digest:    %s\n", inspection.Digest)
				fmt.Fprintf(cmd.OutOrStdout(), "State key: %s\n", inspection.ConnectionKey)
				return nil
			default:
				return fmt.Errorf("-o/--output must be one of: table, json, yaml")
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json|yaml")
	return cmd
}
