package cli

import "github.com/spf13/cobra"

func newExperimentCmd() *cobra.Command {
	cmd := newExpCmd()
	cmd.Use = "experiment"
	cmd.Short = "Track, query, and visualize Tau experiment state"
	cmd.Args = cobra.NoArgs
	cmd.RunE = showGroupHelp
	return cmd
}
