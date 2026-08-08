// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kube"

	"github.com/Azure/taugrid/portal/internal/autocapture"
)

func newExpAutocaptureCmd(storePath *string) *cobra.Command {
	var (
		namespace   string
		kubeContext string
		cluster     string
		project     string
		groupID     string
		owner       string
		output      string
		jsonOutput  bool
		watch       bool
		interval    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "autocapture",
		Short: "Reconcile Tau-labeled cluster workloads into the experiment store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			if namespace == "" {
				namespace = "default"
			}
			if interval <= 0 {
				return fmt.Errorf("--interval must be > 0")
			}
			reconcile := func(ctx context.Context) (autocapture.Result, error) {
				var result autocapture.Result
				store, err := openExpStore(ctx, storePath)
				if err != nil {
					return result, err
				}
				defer store.Close()
				reconciler := autocapture.Reconciler{Client: autocapture.NewKubectlClient(kubeContext)}
				result, err = reconciler.Reconcile(ctx, store, autocapture.Options{
					Namespace:  namespace,
					Cluster:    firstNonEmpty(cluster, kubeContext),
					Project:    project,
					RunGroupID: groupID,
					Owner:      owner,
				})
				return result, err
			}
			result, err := reconcile(cmd.Context())
			if err != nil {
				return err
			}
			if out == "json" {
				if err := writeExpJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else if err := writeAutocaptureTable(cmd.OutOrStdout(), result); err != nil {
				return err
			}
			if !watch {
				return nil
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-ticker.C:
					result, err := reconcile(cmd.Context())
					if err != nil {
						return err
					}
					if out == "json" {
						if err := writeExpJSON(cmd.OutOrStdout(), result); err != nil {
							return err
						}
					} else if err := writeAutocaptureTable(cmd.OutOrStdout(), result); err != nil {
						return err
					}
				}
			}
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace to reconcile (default: default; use '*' for all namespaces)")
	cmd.Flags().StringVar(&kubeContext, "context", kube.DefaultContext(), kube.ContextHelp())
	cmd.Flags().StringVar(&cluster, "cluster", "", "cluster name to record in run_context (default: --context)")
	cmd.Flags().StringVar(&project, "project", "", "project id for newly captured runs (default: store manifest project)")
	cmd.Flags().StringVar(&groupID, "group", "", "run group id for newly captured runs (default: default)")
	cmd.Flags().StringVar(&owner, "owner", "", "run owner for newly captured runs (default: tau-controller)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	cmd.Flags().BoolVar(&watch, "watch", false, "repeat reconciliation until interrupted")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "reconciliation interval when --watch is set")
	return cmd
}

func writeAutocaptureTable(w io.Writer, result autocapture.Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNS\tCREATED_RUNS\tUPDATED_RUNS\tCREATED_CONTEXTS\tUPDATED_CONTEXTS\tEVENTS\tTAGS\tREUSED")
	fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
		result.Runs,
		result.CreatedRuns,
		result.UpdatedRuns,
		result.CreatedRunContexts,
		result.UpdatedRunContexts,
		result.Events,
		result.Tags,
		result.Reused,
	)
	return tw.Flush()
}
