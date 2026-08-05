// Package cli wires the taugrid-portal cobra command tree.
//
// The surface is deliberately identical to the `experiment` and `portal`
// groups the tau CLI used to carry, including the hidden `exp` alias, so
// existing manifests, sidecar args, and scripts keep working after the split.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/version"
)

// NewRoot constructs the top-level `taugrid-portal` command.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "taugrid-portal",
		Short: "Tau experiment tracking and observability portal",
		Long: `taugrid-portal hosts the read-only web surface of the Tau runtime.

experiment  manages the local-first experiment store and the Stellar dashboards.
portal      serves the unified observability shell that cross-links experiments,
            jobs and queues, cluster health, Ray, and cost.

Both groups were previously subcommands of tau and behave identically here.`,
		SilenceUsage: true,
		// main() prints `error: %v` and exits 1, so cobra must not print the
		// error too — without this every failure is reported twice.
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

// hiddenLegacyRootCommands preserves the pre-split spelling. `tau exp` was a
// hidden alias for `tau experiment`; dropping it here would break scripts that
// the split is otherwise invisible to.
func hiddenLegacyRootCommands() []*cobra.Command {
	cmds := []*cobra.Command{
		newExpCmd(),
	}
	for _, cmd := range cmds {
		cmd.Hidden = true
	}
	return cmds
}
