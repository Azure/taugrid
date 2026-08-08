// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/version"
)

func newVersionCmd() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print taugrid-portal version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if short {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Info(cmd.Root().Name()))
			return err
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "Print only the bare version string")
	return cmd
}

func showGroupHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}
