// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"github.com/spf13/cobra"
)

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
