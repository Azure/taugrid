// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/installvalues"
)

func newClusterExplainValuesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain-values",
		Short: "Print the TauGrid install values reference (human-readable Markdown)",
		Long: `Print the full Helm values reference for the TauGrid chart as Markdown,
including every supported values.yaml key, its type, default, and description.
Use this to discover flags for "tau cluster install --set ..." or to author a
values file before installing.`,
		Example: `  tau cluster explain-values
  tau cluster explain-values > taugrid-values-reference.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(cmd.OutOrStdout(), installvalues.ReferenceMarkdown())
			return nil
		},
	}
}
