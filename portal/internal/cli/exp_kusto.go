// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func newExpKustoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kusto",
		Short: "Build native Kusto queries for Tau experiment projections",
	}
	cmd.AddCommand(newExpKustoMetricsQueryCmd())
	cmd.AddCommand(newExpKustoLifecycleQueryCmd())
	cmd.AddCommand(newExpKustoSchemaCmd())
	return cmd
}

func newExpKustoMetricsQueryCmd() *cobra.Command {
	var opts expkusto.MetricsQueryOptions
	var endpoint, database, output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "metrics-query",
		Short: "Write an optimized Tau experiment metrics KQL query",
		Long: "Write the KQL that operators and Stellar's Kusto source use to validate and debug Tau scalar metrics in ADX.\n\n" +
			"The command does not execute Kusto itself; it emits the query shape for the configured ingestion path.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "kql", "json", "table")
			if err != nil {
				return err
			}
			query, err := expkusto.BuildMetricsQuery(opts)
			if err != nil {
				return err
			}
			if endpoint == "" {
				endpoint = expkusto.DefaultEndpoint
			}
			if database == "" {
				database = expkusto.DefaultDatabase
			}
			targetPoints := opts.TargetPoints
			if targetPoints == 0 {
				targetPoints = expkusto.DefaultTargetPoints
			}
			result := expkusto.MetricsQueryResult{
				Endpoint:     endpoint,
				Database:     database,
				Query:        query,
				TargetPoints: targetPoints,
			}
			switch out {
			case "json":
				return writeExpJSON(cmd.OutOrStdout(), result)
			case "table":
				return writeKustoMetricsQueryTable(cmd.OutOrStdout(), result)
			default:
				_, err := fmt.Fprint(cmd.OutOrStdout(), query)
				return err
			}
		},
	}
	cmd.Flags().StringVar(&opts.Project, "project", "", "optional project label filter")
	cmd.Flags().StringVar(&opts.WorkspaceID, "workspace", "", "optional TauWorkspace identity filter")
	cmd.Flags().StringVar(&opts.RunGroupID, "group", "", "run group id filter")
	cmd.Flags().StringArrayVar(&opts.RunIDs, "run", nil, "run id filter (repeatable)")
	cmd.Flags().StringArrayVar(&opts.MetricNames, "metric", nil, "metric name filter (repeatable)")
	cmd.Flags().StringVar(&opts.Since, "since", "7d", "exported_at lookback passed to ago(), such as 7d or 12h")
	cmd.Flags().StringVar(&opts.Ingestion, "ingestion", "projection", "Kusto ingestion shape: projection for TauExpMetrics or remote-write for hosted ExperimentMetrics")
	cmd.Flags().IntVar(&opts.TargetPoints, "target-points", expkusto.DefaultTargetPoints, "target downsampled points per query result")
	cmd.Flags().StringVar(&endpoint, "endpoint", expkusto.DefaultEndpoint, "Kusto endpoint for metadata output")
	cmd.Flags().StringVar(&database, "database", expkusto.DefaultDatabase, "Kusto database for metadata output")
	cmd.Flags().StringVarP(&output, "output", "o", "kql", "kql|json|table")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func newExpKustoLifecycleQueryCmd() *cobra.Command {
	var opts expkusto.RunLifecycleQueryOptions
	var endpoint, database, output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "lifecycle-query",
		Short: "Write a Stellar run lifecycle KQL query",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "kql", "json", "table")
			if err != nil {
				return err
			}
			query, err := expkusto.BuildRunLifecycleQuery(opts)
			if err != nil {
				return err
			}
			if endpoint == "" {
				endpoint = expkusto.DefaultEndpoint
			}
			if database == "" {
				database = expkusto.DefaultDatabase
			}
			staleAfter := opts.StaleAfter
			if strings.TrimSpace(staleAfter) == "" {
				staleAfter = expkusto.DefaultLifecycleStaleAfter
			}
			result := expkusto.RunLifecycleQueryResult{
				Endpoint:   endpoint,
				Database:   database,
				Query:      query,
				StaleAfter: staleAfter,
			}
			switch out {
			case "json":
				return writeExpJSON(cmd.OutOrStdout(), result)
			case "table":
				return writeKustoLifecycleQueryTable(cmd.OutOrStdout(), result)
			default:
				_, err := fmt.Fprint(cmd.OutOrStdout(), query)
				return err
			}
		},
	}
	cmd.Flags().StringVar(&opts.Project, "project", "", "optional project label filter")
	cmd.Flags().StringVar(&opts.WorkspaceID, "workspace", "", "optional TauWorkspace identity filter")
	cmd.Flags().StringVar(&opts.Target, "target", "", "experiment, run group, or run id filter")
	cmd.Flags().StringVar(&opts.TargetType, "target-type", "auto", "target shape: auto, experiment, run_group, or run")
	cmd.Flags().StringVar(&opts.RunGroupID, "group", "", "run group id filter")
	cmd.Flags().StringArrayVar(&opts.RunIDs, "run", nil, "run id filter (repeatable)")
	cmd.Flags().StringVar(&opts.Since, "since", "7d", "lookback passed to ago(), such as 7d or 12h")
	cmd.Flags().StringVar(&opts.Ingestion, "ingestion", "projection", "metric ingestion shape to join: projection for TauExpMetrics or remote-write for hosted ExperimentMetrics")
	cmd.Flags().StringVar(&opts.StaleAfter, "stale-after", expkusto.DefaultLifecycleStaleAfter, "mark non-terminal runs stale after no metrics for this duration")
	cmd.Flags().StringVar(&endpoint, "endpoint", expkusto.DefaultEndpoint, "Kusto endpoint for metadata output")
	cmd.Flags().StringVar(&database, "database", expkusto.DefaultDatabase, "Kusto database for metadata output")
	cmd.Flags().StringVarP(&output, "output", "o", "kql", "kql|json|table")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func writeKustoLifecycleQueryTable(w io.Writer, result expkusto.RunLifecycleQueryResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ENDPOINT\tDATABASE\tSTALE_AFTER\n")
	fmt.Fprintf(tw, "%s\t%s\t%s\n", result.Endpoint, result.Database, result.StaleAfter)
	return tw.Flush()
}

func writeKustoMetricsQueryTable(w io.Writer, result expkusto.MetricsQueryResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ENDPOINT\tDATABASE\tTARGET_POINTS\n")
	fmt.Fprintf(tw, "%s\t%s\t%d\n", result.Endpoint, result.Database, result.TargetPoints)
	return tw.Flush()
}

func newExpKustoSchemaCmd() *cobra.Command {
	var ingestion string
	var remoteWriteTable string
	var runLifecycleTable string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Write Kusto schema and Stellar normalization functions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ingestion = strings.ToLower(strings.TrimSpace(ingestion))
			if ingestion == "" {
				ingestion = "all"
			}
			var sections []string
			switch ingestion {
			case "projection":
				sections = append(sections, expstore.ADXProjectionKQL(), expkusto.BuildProjectionDashboardSchemaKQL())
			case "remote-write":
				kql, err := expkusto.BuildRemoteWriteSchemaKQL(expkusto.SchemaOptions{RemoteWriteTable: remoteWriteTable})
				if err != nil {
					return err
				}
				sections = append(sections, kql)
			case "lifecycle":
				kql, err := expkusto.BuildRunLifecycleSchemaKQL(expkusto.SchemaOptions{RunLifecycleTable: runLifecycleTable})
				if err != nil {
					return err
				}
				sections = append(sections, kql)
			case "all":
				kql, err := expkusto.BuildRemoteWriteSchemaKQL(expkusto.SchemaOptions{RemoteWriteTable: remoteWriteTable})
				if err != nil {
					return err
				}
				lifecycle, err := expkusto.BuildRunLifecycleSchemaKQL(expkusto.SchemaOptions{RunLifecycleTable: runLifecycleTable})
				if err != nil {
					return err
				}
				sections = append(sections, expstore.ADXProjectionKQL(), expkusto.BuildProjectionDashboardSchemaKQL(), kql, lifecycle)
			default:
				return fmt.Errorf("--ingestion must be projection, remote-write, lifecycle, or all")
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), strings.Join(sections, "\n"))
			return err
		},
	}
	cmd.Flags().StringVar(&ingestion, "ingestion", "all", "schema shape to emit: projection, remote-write, lifecycle, or all")
	cmd.Flags().StringVar(&remoteWriteTable, "remote-write-table", expkusto.DefaultRemoteWriteTable, "adx-mon table for experiment_metrics remote-write samples")
	cmd.Flags().StringVar(&runLifecycleTable, "run-lifecycle-table", expkusto.DefaultRunLifecycleTable, "Kusto table for Tau/Stellar run lifecycle rows")
	return cmd
}
