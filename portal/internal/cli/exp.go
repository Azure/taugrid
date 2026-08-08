// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/portal/internal/expcockpit"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/portalbin"
)

func newExpCmd() *cobra.Command {
	var storePath string
	cmd := &cobra.Command{
		Use:   "exp",
		Short: "Track local experiment metadata for agents and researchers",
		Long: `Track local experiment metadata for agents and researchers.

The v0 experiment store is path backed: a portable packet with
manifest.json, index.sqlite, JSONL mirrors, metrics/, and artifacts/.
Use --store for an explicit packet/root, TAU_EXP_STORE for an exact root,
or TAU_EXP_STORE_ROOT with TAU_CONTEXT, TAU_TEAM, and TAU_PROJECT for
a cluster-local team/project default.`,
	}
	cmd.PersistentFlags().StringVar(&storePath, "store", "", "experiment store root")
	cmd.AddCommand(
		newExpInitCmd(&storePath),
		newExpTrackCmd(&storePath),
		newExpImportCmd(&storePath),
		newExpStellarCmd(&storePath),
		newExpCompareCmd(&storePath),
		newExpPlotCmd(&storePath),
		newExpOpenCmd(&storePath),
		newExpServeCmd(&storePath),
		newExpObserveCmd(&storePath),
		newExpCaptureCmd(&storePath),
		newExpAutocaptureCmd(&storePath),
		newExpOffloadCmd(&storePath),
		newExpKustoCmd(),
		newExpExperimentsCmd(&storePath),
		newExpSearchCmd(&storePath),
		newExpListCmd(&storePath),
		newExpStatusCmd(&storePath),
		newExpSQLCmd(&storePath),
		newExpExportCmd(&storePath),
	)
	return cmd
}

func newExpSearchCmd(storePath *string) *cobra.Command {
	var query, target, project, groupID, state, lifecycle, since, minStep, output string
	var limit int
	var jsonOutput bool
	var tags, metricNames, metricFilters []string
	cmd := &cobra.Command{
		Use:     "search",
		Aliases: []string{"runs"},
		Short:   "Search indexed experiment runs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json", "jsonl", "csv")
			if err != nil {
				return err
			}
			tagFilters, err := parseExpTags(tags)
			if err != nil {
				return err
			}
			parsedFilters, err := parseExpMetricFilters(metricFilters)
			if err != nil {
				return err
			}
			var parsedMinStep *int64
			if strings.TrimSpace(minStep) != "" {
				value, err := strconv.ParseInt(strings.TrimSpace(minStep), 10, 64)
				if err != nil {
					return fmt.Errorf("--min-step must be an integer")
				}
				parsedMinStep = &value
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.SearchRuns(cmd.Context(), expstore.RunSearchOptions{
				Target:        target,
				Query:         query,
				Project:       project,
				RunGroupID:    groupID,
				State:         state,
				Lifecycle:     lifecycle,
				Tags:          tagFilters,
				MetricNames:   metricNames,
				MetricFilters: parsedFilters,
				Since:         since,
				Limit:         limit,
				MinStep:       parsedMinStep,
			})
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeQueryResult(cmd.OutOrStdout(), runSearchQueryResult(result), out)
		},
	}
	cmd.Flags().StringVarP(&query, "query", "q", "", "free-text search over run id, project, experiment, group, tags, and metric names")
	cmd.Flags().StringVar(&target, "target", "", "limit search to an experiment, run group, or run id")
	cmd.Flags().StringVar(&project, "project", "", "filter by project")
	cmd.Flags().StringVar(&groupID, "group", "", "filter by run group id")
	cmd.Flags().StringVar(&state, "state", "", "filter by raw run state")
	cmd.Flags().StringVar(&lifecycle, "lifecycle", "", "filter by derived lifecycle: pending|running|failed|succeeded|incomplete")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "filter by tag key=value (repeatable)")
	cmd.Flags().StringArrayVar(&metricNames, "metric-name", nil, "filter to runs containing metric name (repeatable)")
	cmd.Flags().StringArrayVar(&metricFilters, "metric-filter", nil, "filter by metric threshold, e.g. eval/score>=0.8 or loss@max<1")
	cmd.Flags().StringVar(&since, "since", "", "filter by created_at recency (RFC3339, Go duration such as 24h, or Nd)")
	cmd.Flags().StringVar(&minStep, "min-step", "", "require derived success to reach at least this max step")
	cmd.Flags().IntVar(&limit, "limit", 200, "maximum runs to return (max 1000)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json|jsonl|csv")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func newExpExperimentsCmd(storePath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "experiments",
		Short: "Search and tag experiment groupings",
	}
	cmd.AddCommand(
		newExpExperimentsSearchCmd(storePath),
		newExpExperimentsTagRunCmd(storePath),
	)
	return cmd
}

func newExpExperimentsSearchCmd(storePath *string) *cobra.Command {
	var query, project, lifecycle, since, output string
	var limit int
	var jsonOutput bool
	var tags, metricNames, metricFilters []string
	cmd := &cobra.Command{
		Use:     "search",
		Aliases: []string{"list"},
		Short:   "Search or list indexed experiments",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json", "jsonl", "csv")
			if err != nil {
				return err
			}
			tagFilters, err := parseExpTags(tags)
			if err != nil {
				return err
			}
			parsedFilters, err := parseExpMetricFilters(metricFilters)
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.SearchExperiments(cmd.Context(), expstore.ExperimentSearchOptions{
				Query:         query,
				Project:       project,
				Lifecycle:     lifecycle,
				Tags:          tagFilters,
				MetricNames:   metricNames,
				MetricFilters: parsedFilters,
				Since:         since,
				Limit:         limit,
			})
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeQueryResult(cmd.OutOrStdout(), experimentSearchQueryResult(result), out)
		},
	}
	cmd.Flags().StringVarP(&query, "query", "q", "", "free-text search over experiment id, name, description, tags, and metric names")
	cmd.Flags().StringVar(&project, "project", "", "filter by project")
	cmd.Flags().StringVar(&lifecycle, "lifecycle", "", "filter to experiments containing a run lifecycle: pending|running|failed|succeeded|incomplete")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "filter to experiments containing run tag key=value (repeatable)")
	cmd.Flags().StringArrayVar(&metricNames, "metric-name", nil, "filter to experiments containing metric name (repeatable)")
	cmd.Flags().StringArrayVar(&metricFilters, "metric-filter", nil, "filter by metric threshold, e.g. eval/score>=0.8 or loss@max<1")
	cmd.Flags().StringVar(&since, "since", "", "filter by experiment/update/run recency (RFC3339, Go duration such as 24h, or Nd)")
	cmd.Flags().IntVar(&limit, "limit", 200, "maximum experiments to return (max 1000)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json|jsonl|csv")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func newExpExperimentsTagRunCmd(storePath *string) *cobra.Command {
	var experimentID, name, description string
	cmd := &cobra.Command{
		Use:   "tag-run RUN_ID",
		Short: "Assign a run to an experiment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			experimentID = strings.TrimSpace(experimentID)
			if experimentID == "" {
				return fmt.Errorf("--experiment is required")
			}
			if strings.TrimSpace(name) == "" {
				name = experimentID
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.AssignRunToExperiment(cmd.Context(), expstore.ExperimentRecord{
				ExperimentID: experimentID,
				Name:         name,
				Description:  description,
				Source:       "explicit",
			}, args[0]); err != nil {
				return err
			}
			return writeExpJSON(cmd.OutOrStdout(), map[string]any{
				"run_id":        args[0],
				"experiment_id": experimentID,
				"name":          name,
			})
		},
	}
	cmd.Flags().StringVar(&experimentID, "experiment", "", "experiment id to assign")
	cmd.Flags().StringVar(&name, "name", "", "human-readable experiment name")
	cmd.Flags().StringVar(&description, "description", "", "experiment description")
	return cmd
}

func newExpStellarCmd(storePath *string) *cobra.Command {
	var output, outPath, metric string
	cmd := &cobra.Command{
		Use:     "stellar NAME",
		Aliases: []string{"dashboard"},
		Short:   "Render a local Stellar experiment dashboard",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := normalizeExpOutput(output, false, "html", "json", "tui")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			opts := expcockpit.Options{Target: args[0], Metric: metric}
			if out == "json" {
				snapshot, err := expcockpit.BuildSnapshot(cmd.Context(), store, opts)
				if err != nil {
					return err
				}
				if outPath != "" {
					raw, err := json.MarshalIndent(snapshot, "", "  ")
					if err != nil {
						return err
					}
					raw = append(raw, '\n')
					return os.WriteFile(outPath, raw, 0o644)
				}
				return writeExpJSON(cmd.OutOrStdout(), snapshot)
			}
			if out == "tui" {
				tui, err := expcockpit.RenderTUI(cmd.Context(), store, opts)
				if err != nil {
					return err
				}
				if outPath != "" {
					return os.WriteFile(outPath, tui, 0o644)
				}
				_, err = cmd.OutOrStdout().Write(tui)
				return err
			}
			html, err := expcockpit.RenderHTML(cmd.Context(), store, opts)
			if err != nil {
				return err
			}
			if outPath != "" {
				return os.WriteFile(outPath, html, 0o644)
			}
			_, err = cmd.OutOrStdout().Write(html)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "html", "html|json|tui")
	cmd.Flags().StringVar(&outPath, "out", "", "write Stellar output to a file instead of stdout")
	cmd.Flags().StringVar(&metric, "metric", "", "metric name to use for the primary dashboard chart")
	return cmd
}

func newExpCompareCmd(storePath *string) *cobra.Command {
	var metric, direction, output, format string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "compare TARGET",
		Short: "Compare local experiment run groups for a scalar metric",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := normalizeExpOutputAlias(output, format, jsonOutput, "table", "json", "jsonl", "csv")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			comparison, err := expcockpit.Compare(cmd.Context(), store, expcockpit.CompareOptions{
				Target:    args[0],
				Metric:    metric,
				Direction: direction,
			})
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), comparison)
			}
			return writeQueryResult(cmd.OutOrStdout(), comparisonGroupRows(comparison), out)
		},
	}
	cmd.Flags().StringVar(&metric, "metric", "", "metric name to compare (default: Stellar primary outcome metric)")
	cmd.Flags().StringVar(&direction, "direction", "max", "max|min winner direction")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json|jsonl|csv")
	cmd.Flags().StringVar(&format, "format", "", "table|json|jsonl|csv (overrides --output)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func newExpPlotCmd(storePath *string) *cobra.Command {
	var metric, outPath, output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "plot TARGET",
		Short: "Write a deterministic local SVG chart for a scalar metric",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			if strings.TrimSpace(outPath) == "" {
				return fmt.Errorf("--out is required")
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			svg, plot, err := expcockpit.RenderPlotSVG(cmd.Context(), store, expcockpit.PlotOptions{
				Target: args[0],
				Metric: metric,
			})
			if err != nil {
				return err
			}
			if err := os.WriteFile(outPath, svg, 0o644); err != nil {
				return err
			}
			result := expPlotResult{
				SchemaVersion: plot.SchemaVersion,
				Target:        plot.Target,
				TargetType:    plot.TargetType,
				MetricName:    plot.MetricName,
				Out:           outPath,
				Bytes:         len(svg),
				Series:        plot.Series,
				Warnings:      plot.Warnings,
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writePlotResultTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&metric, "metric", "", "metric name to plot (default: Stellar primary outcome metric)")
	cmd.Flags().StringVar(&outPath, "out", "", "write SVG chart artifact to PATH (required)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func newExpObserveCmd(storePath *string) *cobra.Command {
	var scope, typ, text, author, source, evidence, observationID, idempotencyKey, output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "observe",
		Short: "Append a structured observation to the experiment store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			scopeType, scopeID, err := parseObservationScope(scope)
			if err != nil {
				return err
			}
			if author == "" {
				author = defaultObservationAuthor()
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.RecordObservation(cmd.Context(), expstore.RecordObservationOptions{
				Observation: expstore.ObservationRecord{
					ObservationID: observationID,
					Author:        author,
					Source:        source,
					Type:          typ,
					ScopeType:     scopeType,
					ScopeID:       scopeID,
					Text:          text,
					Evidence:      evidence,
				},
				IdempotencyKey: idempotencyKey,
				Command:        "exp observe",
			})
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeObservationTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "observation scope as type:id (experiment|run_group|run|artifact|event|metric)")
	cmd.Flags().StringVar(&typ, "type", "observation", "observation|decision|blocker|next-experiment|exclusion")
	cmd.Flags().StringVar(&text, "text", "", "observation text")
	cmd.Flags().StringVar(&author, "author", "", "observation author (default: current user)")
	cmd.Flags().StringVar(&source, "source", "human", "observation source, such as human or agent")
	cmd.Flags().StringVar(&evidence, "evidence", "", "JSON or text evidence linking the observation to runs, metrics, artifacts, events, or queries")
	cmd.Flags().StringVar(&observationID, "id", "", "stable observation id (default: generated)")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key for agent-safe repeated writes")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	_ = cmd.MarkFlagRequired("scope")
	_ = cmd.MarkFlagRequired("text")
	return cmd
}

func newExpInitCmd(storePath *string) *cobra.Command {
	var project, description, group, idempotencyKey, output string
	cmd := &cobra.Command{
		Use:   "init NAME",
		Short: "Create or attach to a local experiment store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := normalizeExpOutput(output, false, "table", "json")
			if err != nil {
				return err
			}
			store, result, err := expstore.Init(cmd.Context(), storePathValue(storePath), expstore.InitOptions{
				Name:           args[0],
				Project:        project,
				Description:    description,
				Group:          group,
				IdempotencyKey: idempotencyKey,
			})
			if err != nil {
				return err
			}
			defer store.Close()
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeExpInitTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project identifier (default: NAME)")
	cmd.Flags().StringVar(&description, "description", "", "experiment description")
	cmd.Flags().StringVar(&group, "group", "", "initial run group id (default: default)")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key for agent-safe repeated writes")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	return cmd
}

func newExpListCmd(storePath *string) *cobra.Command {
	var kind, project, groupID, state, output string
	var jsonOutput bool
	var tags []string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List local experiment runs, groups, or experiments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			tagFilters, err := parseExpTags(tags)
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.List(cmd.Context(), expstore.ListOptions{
				Kind:       kind,
				Project:    project,
				RunGroupID: groupID,
				State:      state,
				Tags:       tagFilters,
			})
			if err != nil {
				return err
			}
			return writeQueryResult(cmd.OutOrStdout(), result, out)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "runs", "runs|groups|experiments")
	cmd.Flags().StringVar(&project, "project", "", "filter by project")
	cmd.Flags().StringVar(&groupID, "group", "", "filter by run group id")
	cmd.Flags().StringVar(&state, "state", "", "filter runs by state")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "filter runs by tag key=value (repeatable)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func newExpStatusCmd(storePath *string) *cobra.Command {
	var output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status NAME",
		Short: "Summarize a local experiment, group, or run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			status, err := store.Status(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), status)
			}
			return writeStatusTable(cmd.OutOrStdout(), status)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func newExpSQLCmd(storePath *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "sql QUERY",
		Short: "Run a read-only SQL query against the local experiment index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateExpFormat(format, "table", "json", "jsonl", "csv"); err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.Query(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeQueryResult(cmd.OutOrStdout(), result, format)
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "table|json|jsonl|csv")
	return cmd
}

func newExpExportCmd(storePath *string) *cobra.Command {
	var outDir, output string
	var force bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a portable local experiment packet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, false, "table", "json")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.Export(cmd.Context(), expstore.ExportOptions{Out: outDir, Force: force})
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeExportTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.AddCommand(newExpExportADXCmd(storePath))
	cmd.Flags().StringVar(&outDir, "out", "", "destination directory for the portable packet")
	cmd.Flags().BoolVar(&force, "force", false, "allow exporting into a non-empty directory by overwriting packet files")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	return cmd
}

func newExpExportADXCmd(storePath *string) *cobra.Command {
	var outDir, format, output string
	var force, dryRun bool
	cmd := &cobra.Command{
		Use:   "adx",
		Short: "Export ADX/Kusto projection files from the local experiment store",
		Long: `Export ADX/Kusto-shaped JSONL or CSV files from the resolved experiment store.

This is a downstream analytics projection only. The local expstore remains the
source of truth, and this command never requires live ADX credentials.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := normalizeExpOutput(output, false, "table", "json")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := store.ExportADX(cmd.Context(), expstore.ADXExportOptions{
				Out:    outDir,
				Format: format,
				Force:  force,
				DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeADXExportTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "", "destination directory for local ADX/Kusto projection files")
	cmd.Flags().StringVar(&format, "format", "jsonl", "row file format: jsonl|csv")
	cmd.Flags().BoolVar(&force, "force", false, "allow exporting into a non-empty directory by overwriting projection files")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count and describe projected tables without writing files")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	return cmd
}

func normalizeExpOutput(output string, jsonOutput bool, allowed ...string) (string, error) {
	if jsonOutput {
		output = "json"
	}
	if output == "" {
		output = allowed[0]
	}
	if err := validateExpFormat(output, allowed...); err != nil {
		return "", err
	}
	return output, nil
}

func normalizeExpOutputAlias(output, format string, jsonOutput bool, allowed ...string) (string, error) {
	if strings.TrimSpace(format) != "" {
		output = format
	}
	return normalizeExpOutput(output, jsonOutput, allowed...)
}

func storePathValue(storePath *string) string {
	if storePath == nil {
		return ""
	}
	return *storePath
}

func openExpStore(ctx context.Context, storePath *string) (*expstore.Store, error) {
	return expstore.Open(ctx, storePathValue(storePath))
}

func validateExpFormat(format string, allowed ...string) error {
	for _, a := range allowed {
		if format == a {
			return nil
		}
	}
	return fmt.Errorf("output must be one of: %s", strings.Join(allowed, ", "))
}

func parseExpTags(tags []string) (map[string]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, tag := range tags {
		i := strings.Index(tag, "=")
		if i <= 0 {
			return nil, fmt.Errorf("--tag: expected key=value, got %q", tag)
		}
		key, value := tag[:i], tag[i+1:]
		if key == "" {
			return nil, fmt.Errorf("--tag: key is required")
		}
		out[key] = value
	}
	return out, nil
}

func parseExpMetricFilters(filters []string) ([]expstore.MetricFilter, error) {
	out := make([]expstore.MetricFilter, 0, len(filters))
	for _, filter := range filters {
		parsed, err := expstore.ParseMetricFilter(filter)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

func runSearchQueryResult(result expstore.RunSearchResult) expstore.QueryResult {
	columns := []string{"run_id", "lifecycle_state", "successful", "state", "project", "run_group_id", "created_at", "completed_at", "metric_names", "tags", "success_reasons"}
	rows := make([]map[string]any, 0, len(result.Runs))
	for _, run := range result.Runs {
		rows = append(rows, map[string]any{
			"run_id":          run.RunID,
			"lifecycle_state": run.LifecycleState,
			"successful":      run.Successful,
			"state":           run.State,
			"project":         run.Project,
			"run_group_id":    run.RunGroupID,
			"created_at":      run.CreatedAt,
			"completed_at":    run.CompletedAt,
			"metric_names":    strings.Join(run.MetricNames, ","),
			"tags":            formatSearchTags(run.Tags),
			"success_reasons": strings.Join(run.SuccessReasons, "; "),
		})
	}
	return expstore.QueryResult{Columns: columns, Rows: rows}
}

func experimentSearchQueryResult(result expstore.ExperimentSearchResult) expstore.QueryResult {
	columns := []string{"experiment_id", "name", "project", "source", "runs", "run_groups", "lifecycle_counts", "latest_run_at", "metric_names"}
	rows := make([]map[string]any, 0, len(result.Experiments))
	for _, experiment := range result.Experiments {
		rows = append(rows, map[string]any{
			"experiment_id":    experiment.ExperimentID,
			"name":             experiment.Name,
			"project":          experiment.Project,
			"source":           experiment.Source,
			"runs":             experiment.RunCount,
			"run_groups":       experiment.RunGroupCount,
			"lifecycle_counts": formatIntCounts(experiment.LifecycleCounts),
			"latest_run_at":    experiment.LatestRunAt,
			"metric_names":     strings.Join(experiment.MetricNames, ","),
		})
	}
	return expstore.QueryResult{Columns: columns, Rows: rows}
}

func formatIntCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func formatSearchTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+tags[key])
	}
	return strings.Join(parts, ",")
}

func comparisonGroupRows(comparison expcockpit.Comparison) expstore.QueryResult {
	columns := []string{
		"metric_name",
		"direction",
		"run_group_id",
		"run_count",
		"latest_step",
		"min",
		"p25",
		"median",
		"p75",
		"max",
		"best_value",
		"best_run_id",
		"winner",
	}
	rows := make([]map[string]any, 0, len(comparison.Groups))
	for _, group := range comparison.Groups {
		rows = append(rows, map[string]any{
			"metric_name":  comparison.MetricName,
			"direction":    comparison.Direction,
			"run_group_id": group.RunGroupID,
			"run_count":    group.RunCount,
			"latest_step":  group.LatestStep,
			"min":          group.Min,
			"p25":          group.P25,
			"median":       group.Median,
			"p75":          group.P75,
			"max":          group.Max,
			"best_value":   group.BestValue,
			"best_run_id":  group.BestRunID,
			"winner":       group.RunGroupID == comparison.BestGroupID,
		})
	}
	return expstore.QueryResult{Columns: columns, Rows: rows}
}

type expPlotResult struct {
	SchemaVersion string   `json:"schema_version"`
	Target        string   `json:"target"`
	TargetType    string   `json:"target_type"`
	MetricName    string   `json:"metric_name"`
	Out           string   `json:"out"`
	Bytes         int      `json:"bytes"`
	Series        int      `json:"series"`
	Warnings      []string `json:"warnings,omitempty"`
}

func writePlotResultTable(w io.Writer, result expPlotResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "target\t%s\n", result.Target)
	fmt.Fprintf(tw, "metric\t%s\n", result.MetricName)
	fmt.Fprintf(tw, "out\t%s\n", result.Out)
	fmt.Fprintf(tw, "series\t%d\n", result.Series)
	fmt.Fprintf(tw, "bytes\t%d\n", result.Bytes)
	if len(result.Warnings) > 0 {
		fmt.Fprintf(tw, "warnings\t%s\n", strings.Join(result.Warnings, "; "))
	}
	return tw.Flush()
}

func parseObservationScope(scope string) (string, string, error) {
	i := strings.Index(scope, ":")
	if i <= 0 || i == len(scope)-1 {
		return "", "", fmt.Errorf("--scope: expected type:id")
	}
	return scope[:i], scope[i+1:], nil
}

func defaultObservationAuthor() string {
	for _, key := range []string{"GITHUB_ACTOR", "USER", "USERNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return "unknown"
}

func writeExpJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func writeExpInitTable(w io.Writer, result expstore.InitResult) error {
	action := "initialized"
	if result.Reused {
		action = "attached"
	}
	_, err := fmt.Fprintf(w, "%s experiment store %s\nexperiment: %s\ngroup:      %s\nstatus:     %s status %s --store %s -o json\n",
		action, result.StorePath, result.Experiment.ExperimentID, result.RunGroup.RunGroupID, portalbin.ExperimentCmd, result.Experiment.ExperimentID, result.StorePath)
	return err
}

func writeStatusTable(w io.Writer, status expstore.Status) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tTYPE\tRUNS\tGROUPS\tCONFIGS\tMETRIC_FILES\tARTIFACTS\tOBSERVATIONS\tLATEST_EVENT")
	fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
		status.Target,
		status.TargetType,
		status.Runs,
		status.RunGroups,
		status.Configs,
		status.MetricFiles,
		status.Artifacts,
		status.Observations,
		dash(status.LatestEventAt),
	)
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(status.StateCounts) == 0 {
		fmt.Fprintln(w, "states:  (none)")
		return nil
	}
	keys := make([]string, 0, len(status.StateCounts))
	for key := range status.StateCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, status.StateCounts[key]))
	}
	fmt.Fprintf(w, "states:  %s\n", strings.Join(parts, ", "))
	return nil
}

func writeExportTable(w io.Writer, result expstore.ExportResult) error {
	_, err := fmt.Fprintf(w, "exported experiment packet\nsource:      %s\ndestination: %s\nfiles:       %d\ndirs:        %d\n",
		result.Source, result.Destination, result.FilesCopied, result.DirsCreated)
	return err
}

func writeADXExportTable(w io.Writer, result expstore.ADXExportResult) error {
	if _, err := fmt.Fprintf(w, "exported ADX/Kusto projection\nmode:        %s\nsource:      %s\nstore_id:    %s\nformat:      %s\n",
		result.Mode, result.SourceStorePath, result.SourceStoreID, result.Format); err != nil {
		return err
	}
	if result.Destination != "" {
		if _, err := fmt.Fprintf(w, "destination: %s\nschema:      %s\n", result.Destination, result.SchemaFile); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "note:        downstream projection only; expstore remains authoritative")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tROWS\tFILE")
	for _, table := range result.Tables {
		file := table.File
		if file == "" {
			file = "(dry-run)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", table.Name, table.Rows, file)
	}
	return tw.Flush()
}

func writeObservationTable(w io.Writer, result expstore.RecordObservationResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "observation\t%s\n", result.ObservationID)
	fmt.Fprintf(tw, "scope\t%s:%s\n", result.ScopeType, result.ScopeID)
	fmt.Fprintf(tw, "type\t%s\n", result.Type)
	fmt.Fprintf(tw, "created\t%t\n", result.Created)
	fmt.Fprintf(tw, "reused\t%t\n", result.Reused)
	if result.IdempotencyKey != "" {
		fmt.Fprintf(tw, "idempotency_key\t%s\n", result.IdempotencyKey)
	}
	return tw.Flush()
}

func writeQueryResult(w io.Writer, result expstore.QueryResult, format string) error {
	switch format {
	case "json":
		return writeExpJSON(w, result.Rows)
	case "jsonl":
		enc := json.NewEncoder(w)
		for _, row := range result.Rows {
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	case "csv":
		cw := csv.NewWriter(w)
		if err := cw.Write(result.Columns); err != nil {
			return err
		}
		for _, row := range result.Rows {
			record := make([]string, 0, len(result.Columns))
			for _, col := range result.Columns {
				record = append(record, expCell(row[col]))
			}
			if err := cw.Write(record); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	default:
		return writeResultTable(w, result)
	}
}

func writeResultTable(w io.Writer, result expstore.QueryResult) error {
	if len(result.Columns) == 0 {
		_, err := fmt.Fprintln(w, "(no columns)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	heads := make([]string, 0, len(result.Columns))
	for _, col := range result.Columns {
		heads = append(heads, strings.ToUpper(col))
	}
	fmt.Fprintln(tw, strings.Join(heads, "\t"))
	for _, row := range result.Rows {
		values := make([]string, 0, len(result.Columns))
		for _, col := range result.Columns {
			values = append(values, expCell(row[col]))
		}
		fmt.Fprintln(tw, strings.Join(values, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(result.Rows) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	return nil
}

func expCell(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
