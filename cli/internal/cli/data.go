package cli

import "github.com/spf13/cobra"

func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Manage Tau datasets and model registries",
		Long: `Manage Tau datasets and model registries.

Tau groups durable asset registries under data so researchers have one place
to discover offline datasets, model checkpoints, aliases, and resolved
references.`,
		Args: cobra.NoArgs,
		RunE: showGroupHelp,
	}
	cmd.AddCommand(newDatasetCmd(), newModelCmd())
	return cmd
}
