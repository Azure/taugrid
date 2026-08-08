// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import "github.com/spf13/cobra"

func showGroupHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}
