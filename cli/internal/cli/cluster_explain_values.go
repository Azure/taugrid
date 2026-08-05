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
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(cmd.OutOrStdout(), installvalues.ReferenceMarkdown())
			return nil
		},
	}
}
