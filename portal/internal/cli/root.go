// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/version"
)

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "taugrid-portal",
		Short: "Tau experiment tracking and observability portal",
		Long: `taugrid-portal hosts the read-only web surface of the Tau runtime.

experiment  manages the local-first experiment store and the Stellar dashboards.
portal      serves the unified observability shell that cross-links experiments,
            jobs and queues, cluster health, Ray, and cost.

Both groups were previously subcommands of tau and behave identically here.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}

	root.AddCommand(
		newExperimentCmd(),
		newPortalCmd(),
		newVersionCmd(),
	)
	root.AddCommand(hiddenLegacyRootCommands()...)

	return root
}

func hiddenLegacyRootCommands() []*cobra.Command {
	cmds := []*cobra.Command{
		newExpCmd(),
	}
	for _, cmd := range cmds {
		cmd.Hidden = true
	}
	return cmds
}
