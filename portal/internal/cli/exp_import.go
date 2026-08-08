// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/portal/internal/expimport"
)

func newExpImportCmd(storePath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import metrics and artifacts into the local experiment store",
	}
	cmd.AddCommand(newExpImportJSONLCmd(storePath))
	return cmd
}

func newExpImportJSONLCmd(storePath *string) *cobra.Command {
	var opts expimport.JSONLImportOptions
	var tags []string
	var output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "jsonl",
		Short: "Import generic JSONL scalar history into the experiment store",
		Long: `Import generic JSONL scalar history into the experiment store.

Each input line must be a JSON object. Numeric fields become scalar metrics;
underscore-prefixed fields are metadata and are skipped. By default _step is
used as the metric step and _timestamp is interpreted as epoch seconds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := applyExperimentDefaultsToJSONLImport(cmd, &opts); err != nil {
				return err
			}
			parsedTags, err := parseExpTags(tags)
			if err != nil {
				return err
			}
			opts.Tags = parsedTags
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := expimport.ImportJSONL(cmd.Context(), store, opts)
			if err != nil {
				return err
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeJSONLImportTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&opts.RunID, "run", "", "run name to import JSONL scalars into (default: tau.yaml name/run.name)")
	cmd.Flags().StringArrayVar(&opts.History, "history", nil, "JSONL scalar history file path or glob (repeatable, required)")
	cmd.Flags().StringVar(&opts.Project, "project", "", "project id (default: store manifest project)")
	cmd.Flags().StringVar(&opts.ExperimentID, "experiment", "", "experiment id (default: tau.yaml experiment.name)")
	cmd.Flags().StringVar(&opts.RunGroupID, "group", "", "run group id (default: default)")
	cmd.Flags().StringVar(&opts.Owner, "owner", "", "run owner (default: jsonl-import)")
	cmd.Flags().StringVar(&opts.State, "state", "", "run state (default: succeeded)")
	cmd.Flags().StringVar(&opts.MetricPrefix, "metric-prefix", "", "prefix JSONL metric names")
	cmd.Flags().StringVar(&opts.Source, "source", "", "metric source label (default: jsonl)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "run discovery tag key=value stamped into imported metrics (repeatable)")
	cmd.Flags().StringVar(&opts.StepField, "step-field", "", "JSON field to use as metric step (default: _step)")
	cmd.Flags().StringVar(&opts.TimeField, "time-field", "", "JSON field to use as epoch-seconds wall time (default: _timestamp)")
	cmd.Flags().StringVar(&opts.IdempotencyKey, "idempotency-key", "", "idempotency key (default: derived from run and history checksums)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "parse and report the import plan without writing the store")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func writeJSONLImportTable(w io.Writer, result expimport.JSONLImportResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	action := "imported"
	if result.DryRun {
		action = "planned"
	} else if result.Reused {
		action = "reused"
	}
	fmt.Fprintf(tw, "ACTION\tRUN\tROWS\tHISTORY_FILES\tMETRIC_FILE\tIDEMPOTENCY_KEY\n")
	metricFile := ""
	if result.MetricFile != nil {
		metricFile = result.MetricFile.Path
	}
	fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", action, result.RunID, result.Rows, len(result.HistoryFiles), dash(metricFile), result.IdempotencyKey)
	return tw.Flush()
}
