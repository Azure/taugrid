// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"github.com/spf13/cobra"
)

// newPlatformCmd returns the hidden `tau platform` command group.
//
// This group is operator/platform-only tooling: it is not part of the
// researcher-facing surface documented in NewRoot's Long help, and nothing
// under it is ever invoked automatically by tau run / tau serve. Hidden (not
// removed) so operators who know it exists can
// still run `tau platform --help`.
func newPlatformCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "Operator-only platform and canary tooling (not for researcher use)",
		Long: `tau platform holds operator/platform-team tooling that is deliberately
kept out of the researcher-facing command surface (tau run, tau serve, ...).
Nothing under this group is invoked
automatically by those commands.

Every subcommand here is read-only against any cluster it touches, and
requires the operator to explicitly supply already-authenticated kube
contexts — this package never discovers, stores, or distributes cluster
credentials.`,
		Hidden: true,
	}
	cmd.AddCommand(newPlatformPreflightMultiKueueCmd())
	return cmd
}
