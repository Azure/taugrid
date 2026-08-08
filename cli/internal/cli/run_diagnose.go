package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/rundiagnose"
	"github.com/Azure/taugrid/core/kube"
)

func newRunDiagnoseCmd() *cobra.Command {
	var (
		connection runLifecycleConnectionFlags
		output     string
	)
	cmd := &cobra.Command{
		Use:   "diagnose <run-name>",
		Short: "Capture a bounded, redacted run diagnostic snapshot",
		Long: `Capture the live Kubernetes metadata for a Tau run and its owned
Job/RayJob, Kueue Workloads, Pods, Events, container termination state, and
bounded container logs. This is a point-in-time cluster snapshot, not the
durable Tau run-history record.

The command is read-only. It excludes objects that do not carry matching Tau
ownership, never includes environment values or Secret objects, redacts common
credential forms, identifies the Ray driver-log sidecar when present, and
records absent resources and RBAC denials in the bundle.

Examples:
  tau run diagnose train-001 -n ray
  tau run diagnose train-001 -n ray -o json > train-001-diagnostic.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			resolvedContext, namespace, restore, err := connection.resolve(cmd)
			if err != nil {
				return err
			}
			defer restore()
			snapshot, err := rundiagnose.Gather(cmd.Context(), kube.New(resolvedContext), args[0], rundiagnose.Options{
				Namespace:     namespace,
				Context:       resolvedContext,
				TailLines:     rundiagnose.DefaultTailLines,
				LogLimitBytes: rundiagnose.DefaultLogLimitBytes,
				EventLimit:    rundiagnose.DefaultEventLimit,
			})
			if err != nil {
				return err
			}
			if output == "json" {
				return rundiagnose.WriteJSON(cmd.OutOrStdout(), snapshot)
			}
			return rundiagnose.WriteText(cmd.OutOrStdout(), snapshot)
		},
	}
	connection.add(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	return cmd
}
