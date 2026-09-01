// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type runListRawReader interface {
	Raw(context.Context, []string, []byte) (string, error)
}

type runListReader struct{ raw runListRawReader }

func (r runListReader) ListJobs(ctx context.Context, namespace string) ([]byte, error) {
	out, err := r.raw.Raw(ctx, []string{"-n", namespace, "get", "jobs.batch", "-o", "json"}, nil)
	return []byte(out), err
}

func (r runListReader) ListRayJobs(ctx context.Context, namespace string) ([]byte, error) {
	out, err := r.raw.Raw(ctx, []string{"-n", namespace, "get", "rayjobs.ray.io", "-o", "json"}, nil)
	return []byte(out), err
}

func newRunListCmd() *cobra.Command {
	var (
		namespace         string
		kubeContext       string
		queue             string
		output            string
		kustoEndpoint     string
		kustoDatabase     string
		kustoQueryCommand string
		kustoQueryArgs    []string
		kustoTable        string
		kustoCluster      string
		kustoWorkspaceID  string
		historyLimit      int
		includeExternal   bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List submitted jobs (Jobs and RayJobs)",
		Long: `List Tau-managed batch/v1 Jobs and rayjob.ray.io RayJobs in a namespace.

Only workloads created by Tau (carrying an ` + workloadmeta.Domain + ` label) are shown.
KubeRay-internal Jobs (owned by a RayJob) are excluded to avoid duplicates.

Results are sorted by creation time, newest first.`,
		Example: `  tau run list -n ray
  tau run list --context research-admin -n ray
  tau run list -n ray --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedContext, ns, restore, err := resolveRunLifecycleConnection(
				cmd,
				kubeContext,
				namespace,
				runContextExplicit(cmd),
				cmd.Flags().Changed("namespace"),
			)
			if err != nil {
				return err
			}
			defer restore()
			r := kube.New(resolvedContext)
			var history runs.HistoryReader
			if kustoQueryCommand != "" {
				if kustoCluster == "" {
					return fmt.Errorf("--kusto-cluster is required with --kusto-query-command to keep durable history cluster-scoped")
				}
				history = runs.NewKustoHistoryReader(kustoquery.Client{
					Command: kustoQueryCommand, Args: kustoQueryArgs,
					Endpoint: kustoEndpoint, Database: kustoDatabase,
				})
			}
			snap, err := runs.Board(cmd.Context(), runListReader{raw: r}, runs.Options{
				Namespace:       ns,
				Queue:           queue,
				IncludeExternal: includeExternal,
				History:         history,
				HistoryScope: runs.HistoryScope{
					Table:       kustoTable,
					Cluster:     kustoCluster,
					Namespace:   ns,
					LocalQueue:  queue,
					WorkspaceID: kustoWorkspaceID,
					Limit:       historyLimit,
				},
			})
			if err != nil {
				return err
			}
			return writeRunList(cmd.OutOrStdout(), cmd.ErrOrStderr(), ns, output, snap)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&queue, "queue", "", "optional Kueue LocalQueue filter")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table (human-readable) or json (machine-readable)")
	cmd.Flags().StringVar(&kustoEndpoint, "kusto-endpoint", "", "Kusto endpoint metadata passed to --kusto-query-command placeholders")
	cmd.Flags().StringVar(&kustoDatabase, "kusto-database", "", "Kusto database metadata passed to --kusto-query-command placeholders")
	cmd.Flags().StringVar(&kustoQueryCommand, "kusto-query-command", "", "executable that queries durable run history; KQL is passed on stdin unless an arg contains {query}")
	cmd.Flags().StringArrayVar(&kustoQueryArgs, "kusto-query-arg", nil, "argument for --kusto-query-command; supports {endpoint}, {database}, and {query} placeholders")
	cmd.Flags().StringVar(&kustoTable, "kusto-table", "TauExpRunLifecycle", "Kusto table containing durable Tau lifecycle history")
	cmd.Flags().StringVar(&kustoCluster, "kusto-cluster", "", "cluster identifier required when querying durable run history")
	cmd.Flags().StringVar(&kustoWorkspaceID, "kusto-workspace-id", "", "optional durable workspace ID filter")
	cmd.Flags().IntVar(&historyLimit, "history-limit", 200, "maximum durable history rows to merge")
	cmd.Flags().BoolVar(&includeExternal, "include-external", false, "include non-Tau Jobs and RayJobs in the namespace")
	return cmd
}

func writeRunList(out, errOut io.Writer, namespace, output string, snap runs.Snapshot) error {
	switch output {
	case "json":
		return json.NewEncoder(out).Encode(snap)
	case "table":
	default:
		return fmt.Errorf("--output must be table or json")
	}
	fmt.Fprintf(out, "History: %s\n", snap.HistoryState)
	if snap.HistoryDiagnostic != "" {
		fmt.Fprintf(errOut, "warning: %s; showing live Kubernetes rows only\n", snap.HistoryDiagnostic)
	}
	if snap.Total == 0 {
		fmt.Fprintf(out, "No jobs found in namespace %q\n", namespace)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	showSource := false
	for _, run := range snap.Runs {
		if run.Source != "" {
			showSource = true
			break
		}
	}
	if showSource {
		fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tAGE\tSOURCE")
		for _, run := range snap.Runs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", run.Name, run.Kind, run.Status, run.Age, run.Source)
		}
		return nil
	}
	fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tAGE")
	for _, run := range snap.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", run.Name, run.Kind, run.Status, run.Age)
	}
	return nil
}
