package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/kube"
)

const (
	defaultModelRegistryModel    = "unknown"
	maxHelperPodScriptArgPayload = 512 * 1024
)

type modelRegistryRecord struct {
	SchemaVersion  int                       `json:"schema_version"`
	Kind           string                    `json:"kind,omitempty"`
	Model          string                    `json:"model"`
	Run            string                    `json:"run"`
	Namespace      string                    `json:"namespace,omitempty"`
	ResourceName   string                    `json:"resource_name,omitempty"`
	CreatedAt      string                    `json:"created_at,omitempty"`
	CompletedAt    string                    `json:"completed_at,omitempty"`
	RecordPath     string                    `json:"record_path,omitempty"`
	RegistryPath   string                    `json:"registry_path,omitempty"`
	ArtifactsIndex string                    `json:"artifacts_index,omitempty"`
	ModelMetadata  map[string]any            `json:"model_metadata,omitempty"`
	Tags           map[string]string         `json:"tags,omitempty"`
	MetricsPath    string                    `json:"metrics_path,omitempty"`
	Metrics        map[string]float64        `json:"metrics,omitempty"`
	PrimaryMetric  modelRegistryMetric       `json:"primary_metric,omitempty"`
	Artifacts      []managedWorkflowArtifact `json:"artifacts,omitempty"`
	StorageProbe   map[string]interface{}    `json:"storage_probe,omitempty"`
}

type modelRegistryMetric struct {
	Name      string   `json:"name,omitempty"`
	Value     *float64 `json:"value,omitempty"`
	Direction string   `json:"direction,omitempty"`
}

type modelAliasRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Model         string `json:"model"`
	Alias         string `json:"alias"`
	Run           string `json:"run"`
	RecordPath    string `json:"record_path,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

type modelRef struct {
	Model string
	Run   string
	Alias string
}

func newModelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Discover durable model checkpoints and manage their aliases",
		Long: `Discover completed Tau train runs from the durable model registry.

The registry is stored as small JSON metadata records on the blob-training PVC.
It points at existing completed-run artifacts; it does not move checkpoint
payloads or replace managed workflow artifacts.`,
	}
	cmd.AddCommand(
		newModelListCmd(),
		newModelShowCmd(),
		newModelBestCmd(),
		newModelAliasCmd(),
		newModelIndexCmd(),
	)
	return cmd
}

func newModelListCmd() *cobra.Command {
	var namespace, kubeContext, modelName, metricName, sortDir, output string
	var tags []string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List durable model runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedContext, ns, restore, err := resolveWorkloadDataConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			if output != "table" && output != "json" {
				return fmt.Errorf("--output must be one of: table, json")
			}
			if sortDir != "" && sortDir != "asc" && sortDir != "desc" {
				return fmt.Errorf("--sort must be one of: asc, desc")
			}
			tagFilters, err := parseModelTagFilters(tags)
			if err != nil {
				return err
			}
			records, warnings, err := fetchModelRegistryRecords(cmd.Context(), resolvedContext, ns, defaultTauPVCName, modelName)
			if err != nil {
				return err
			}
			for _, warning := range warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", warning)
			}
			records = filterModelRecords(records, tagFilters, metricName)
			sortModelRecords(records, metricName, sortDir)
			if output == "json" {
				raw, err := json.MarshalIndent(records, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return err
			}
			return printModelRecordsTable(cmd.OutOrStdout(), records, metricName)
		},
	}
	cmd.Flags().StringVar(&modelName, "model", "", "model name to list")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag filter key=value (repeatable)")
	cmd.Flags().StringVar(&metricName, "metric", "", "metric to display/sort by")
	cmd.Flags().StringVar(&sortDir, "sort", "", "metric sort direction: asc|desc (default: primary direction)")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newModelShowCmd() *cobra.Command {
	var namespace, kubeContext, output string
	cmd := &cobra.Command{
		Use:   "show MODEL/RUN|RUN",
		Short: "Show a durable model registry record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "json" && output != "summary" {
				return fmt.Errorf("--output must be one of: json, summary")
			}
			resolvedContext, ns, restore, err := resolveWorkloadDataConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			record, err := fetchModelRegistryRecordByArg(cmd.Context(), resolvedContext, ns, defaultTauPVCName, args[0])
			if err != nil {
				return err
			}
			if output == "summary" {
				return printModelRecordsTable(cmd.OutOrStdout(), []modelRegistryRecord{record}, "")
			}
			raw, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "json", "json|summary")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newModelBestCmd() *cobra.Command {
	var namespace, kubeContext, metricName, direction, output string
	cmd := &cobra.Command{
		Use:   "best MODEL",
		Short: "Select the best run for a model by metric",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modelName := args[0]
			resolvedContext, ns, restore, err := resolveWorkloadDataConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			records, warnings, err := fetchModelRegistryRecords(cmd.Context(), resolvedContext, ns, defaultTauPVCName, modelName)
			if err != nil {
				return err
			}
			for _, warning := range warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", warning)
			}
			best, err := selectBestModelRecord(records, metricName, direction)
			if err != nil {
				return err
			}
			switch output {
			case "ref":
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s@%s\n", best.Model, best.Run)
			case "json":
				var raw []byte
				raw, err = json.MarshalIndent(best, "", "  ")
				if err == nil {
					_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				}
			default:
				err = fmt.Errorf("--output must be one of: ref, json")
			}
			return err
		},
	}
	cmd.Flags().StringVar(&metricName, "metric", "", "metric to optimize (default: each record's primary_metric.name)")
	cmd.Flags().StringVar(&direction, "direction", "", "lower|higher (default: primary metric direction, or lower for loss-like metrics)")
	cmd.Flags().StringVarP(&output, "output", "o", "ref", "ref|json")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newModelAliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Manage model alias pointers",
	}
	cmd.AddCommand(newModelAliasSetCmd(), newModelAliasGetCmd())
	return cmd
}

func newModelAliasSetCmd() *cobra.Command {
	var namespace, kubeContext, output string
	cmd := &cobra.Command{
		Use:   "set MODEL ALIAS RUN",
		Short: "Point a model alias at an explicit run",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			modelName, alias, run := args[0], args[1], args[2]
			if err := validateRegistrySegment("model", modelName); err != nil {
				return err
			}
			if err := validateRegistrySegment("alias", alias); err != nil {
				return err
			}
			if err := validateRegistrySegment("run", run); err != nil {
				return err
			}
			resolvedContext, ns, restore, err := resolveWorkloadDataConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			record, err := fetchModelRegistryRecord(cmd.Context(), resolvedContext, ns, defaultTauPVCName, modelName, run)
			if err != nil {
				return err
			}
			aliasRecord := modelAliasRecord{
				SchemaVersion: 1,
				Model:         modelName,
				Alias:         alias,
				Run:           run,
				RecordPath:    record.RecordPath,
				UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
			}
			raw, err := json.MarshalIndent(aliasRecord, "", "  ")
			if err != nil {
				return err
			}
			raw = append(raw, '\n')
			if err := writePVCFile(cmd.Context(), kubeContext, ns, "model-"+modelName+"-"+alias, defaultTauPVCName, storage.ModelRegistryAliasFile(modelName, alias), raw); err != nil {
				return err
			}
			if output == "json" {
				_, err = cmd.OutOrStdout().Write(raw)
			} else if output == "ref" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s@%s\n", modelName, run)
			} else {
				err = fmt.Errorf("--output must be one of: ref, json")
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "ref", "ref|json")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newModelAliasGetCmd() *cobra.Command {
	var namespace, kubeContext, output string
	cmd := &cobra.Command{
		Use:   "get MODEL ALIAS",
		Short: "Read a model alias pointer",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedContext, ns, restore, err := resolveWorkloadDataConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			aliasRecord, err := fetchModelAlias(cmd.Context(), resolvedContext, ns, defaultTauPVCName, args[0], args[1])
			if err != nil {
				return err
			}
			if output == "json" {
				raw, err := json.MarshalIndent(aliasRecord, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return err
			}
			if output != "ref" {
				return fmt.Errorf("--output must be one of: ref, json")
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s@%s\n", aliasRecord.Model, aliasRecord.Run)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "ref", "ref|json")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func newModelIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Maintain durable model registry indexes",
	}
	cmd.AddCommand(newModelRebuildCmd())
	return cmd
}

func newModelRebuildCmd() *cobra.Command {
	var namespace, kubeContext, modelName, runName string
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Backfill registry rows from existing durable finetune artifacts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedContext, ns, restore, err := resolveWorkloadDataConnection(cmd, kubeContext, namespace)
			if err != nil {
				return err
			}
			defer restore()
			runs := []string{runName}
			if runName == "" {
				var err error
				runs, err = fetchPVCList(cmd.Context(), resolvedContext, ns, "model-rebuild", defaultTauPVCName, storage.DurableCheckpointsDir+"/finetunes")
				if err != nil {
					return err
				}
			}
			count := 0
			for _, run := range runs {
				if strings.TrimSpace(run) == "" {
					continue
				}
				record, err := rebuildModelRegistryRecord(cmd.Context(), resolvedContext, ns, defaultTauPVCName, run, modelName)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipping %s: %v\n", run, err)
					continue
				}
				if err := writeModelRegistryRecord(cmd.Context(), resolvedContext, ns, defaultTauPVCName, record); err != nil {
					return err
				}
				count++
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "indexed %d model run(s)\n", count)
			return err
		},
	}
	cmd.Flags().StringVar(&modelName, "model", "", "model name to assign when older runs have no model.json")
	cmd.Flags().StringVar(&runName, "run", "", "single finetune run to rebuild")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", workloadNamespaceHelp)
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	return cmd
}

func validateRegistrySegment(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s: required", kind)
	}
	if strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return fmt.Errorf("%s %q is invalid (use lowercase alphanumerics with internal hyphens)", kind, value)
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return fmt.Errorf("%s %q is invalid (use lowercase alphanumerics with internal hyphens)", kind, value)
	}
	return nil
}

func fetchModelRegistryRecords(ctx context.Context, kubeContext, namespace, pvcName, modelName string) ([]modelRegistryRecord, []error, error) {
	if modelName != "" {
		if err := validateRegistrySegment("model", modelName); err != nil {
			return nil, nil, err
		}
		return fetchModelRunsForModel(ctx, kubeContext, namespace, pvcName, modelName)
	}
	models, err := fetchPVCList(ctx, kubeContext, namespace, "model-registry", pvcName, storage.ModelRegistryModelsDir())
	if err != nil {
		if isPVCNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var all []modelRegistryRecord
	var warnings []error
	for _, model := range models {
		records, modelWarnings, err := fetchModelRunsForModel(ctx, kubeContext, namespace, pvcName, model)
		if err != nil {
			warnings = append(warnings, err)
			continue
		}
		all = append(all, records...)
		warnings = append(warnings, modelWarnings...)
	}
	return all, warnings, nil
}

func fetchModelRunsForModel(ctx context.Context, kubeContext, namespace, pvcName, modelName string) ([]modelRegistryRecord, []error, error) {
	files, err := fetchPVCList(ctx, kubeContext, namespace, "model-"+modelName, pvcName, storage.ModelRegistryModelRunsDir(modelName))
	if err != nil {
		if isPVCNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var records []modelRegistryRecord
	var warnings []error
	for _, file := range files {
		if !strings.HasSuffix(file, ".json") {
			continue
		}
		run := strings.TrimSuffix(file, ".json")
		record, err := fetchModelRegistryRecord(ctx, kubeContext, namespace, pvcName, modelName, run)
		if err != nil {
			warnings = append(warnings, err)
			continue
		}
		records = append(records, record)
	}
	return records, warnings, nil
}

func fetchModelRegistryRecordByArg(ctx context.Context, kubeContext, namespace, pvcName, ref string) (modelRegistryRecord, error) {
	if strings.Contains(ref, "/") {
		parts := strings.Split(ref, "/")
		if len(parts) != 2 {
			return modelRegistryRecord{}, fmt.Errorf("model ref %q must be MODEL/RUN or RUN", ref)
		}
		return fetchModelRegistryRecord(ctx, kubeContext, namespace, pvcName, parts[0], parts[1])
	}
	raw, err := fetchPVCFile(ctx, kubeContext, namespace, ref, pvcName, storage.DurableFinetuneModelFile(ref))
	if err != nil {
		return modelRegistryRecord{}, err
	}
	return parseModelRegistryRecord(raw)
}

func fetchModelRegistryRecord(ctx context.Context, kubeContext, namespace, pvcName, modelName, run string) (modelRegistryRecord, error) {
	if err := validateRegistrySegment("model", modelName); err != nil {
		return modelRegistryRecord{}, err
	}
	if err := validateRegistrySegment("run", run); err != nil {
		return modelRegistryRecord{}, err
	}
	raw, err := fetchPVCFile(ctx, kubeContext, namespace, run, pvcName, storage.ModelRegistryRunFile(modelName, run))
	if err != nil {
		return modelRegistryRecord{}, err
	}
	return parseModelRegistryRecord(raw)
}

func parseModelRegistryRecord(raw []byte) (modelRegistryRecord, error) {
	var record modelRegistryRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return modelRegistryRecord{}, fmt.Errorf("parse model registry record: %w", err)
	}
	if record.Model == "" || record.Run == "" {
		return modelRegistryRecord{}, fmt.Errorf("model registry record missing model/run")
	}
	return record, nil
}

func fetchModelAlias(ctx context.Context, kubeContext, namespace, pvcName, modelName, alias string) (modelAliasRecord, error) {
	if err := validateRegistrySegment("model", modelName); err != nil {
		return modelAliasRecord{}, err
	}
	if err := validateRegistrySegment("alias", alias); err != nil {
		return modelAliasRecord{}, err
	}
	raw, err := fetchPVCFile(ctx, kubeContext, namespace, modelName+"-"+alias, pvcName, storage.ModelRegistryAliasFile(modelName, alias))
	if err != nil {
		return modelAliasRecord{}, err
	}
	var record modelAliasRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return modelAliasRecord{}, fmt.Errorf("parse model alias: %w", err)
	}
	if record.Model == "" || record.Alias == "" || record.Run == "" {
		return modelAliasRecord{}, fmt.Errorf("model alias record missing model/alias/run")
	}
	return record, nil
}

func parseModelTagFilters(tags []string) (map[string]string, error) {
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

func filterModelRecords(records []modelRegistryRecord, tags map[string]string, metricName string) []modelRegistryRecord {
	var out []modelRegistryRecord
	for _, record := range records {
		matches := true
		for key, value := range tags {
			if record.Tags[key] != value {
				matches = false
				break
			}
		}
		if metricName != "" {
			if _, ok := modelMetricValue(record, metricName); !ok {
				matches = false
			}
		}
		if matches {
			out = append(out, record)
		}
	}
	return out
}

func modelMetricValue(record modelRegistryRecord, metricName string) (float64, bool) {
	if metricName == "" {
		if record.PrimaryMetric.Value != nil {
			return *record.PrimaryMetric.Value, true
		}
		metricName = record.PrimaryMetric.Name
	}
	if metricName == "" || record.Metrics == nil {
		return 0, false
	}
	value, ok := record.Metrics[metricName]
	return value, ok
}

func sortModelRecords(records []modelRegistryRecord, metricName, sortDir string) {
	desc := sortDir == "desc"
	sort.SliceStable(records, func(i, j int) bool {
		iv, iok := modelMetricValue(records[i], metricName)
		jv, jok := modelMetricValue(records[j], metricName)
		if iok && jok && iv != jv {
			if desc {
				return iv > jv
			}
			return iv < jv
		}
		return records[i].Run < records[j].Run
	})
}

func printModelRecordsTable(w io.Writer, records []modelRegistryRecord, metricName string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if metricName == "" {
		metricName = "primary"
	}
	fmt.Fprintf(tw, "MODEL\tRUN\tMETRIC(%s)\tDIRECTION\tTAGS\tARTIFACT\tSTATUS\n", metricName)
	for _, record := range records {
		value := "-"
		if v, ok := modelMetricValue(record, metricName); ok {
			value = strconv.FormatFloat(v, 'f', -1, 64)
		}
		direction := record.PrimaryMetric.Direction
		if direction == "" {
			direction = "-"
		}
		artifact, status := "-", "-"
		if len(record.Artifacts) > 0 {
			artifact = record.Artifacts[0].ManifestPath
			status = record.Artifacts[0].Status
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", record.Model, record.Run, value, direction, formatTags(record.Tags), artifact, status)
	}
	if len(records) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\t-\t-")
	}
	return tw.Flush()
}

func formatTags(tags map[string]string) string {
	if len(tags) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		parts = append(parts, key+"="+tags[key])
	}
	return strings.Join(parts, ",")
}

func selectBestModelRecord(records []modelRegistryRecord, metricName, direction string) (modelRegistryRecord, error) {
	if len(records) == 0 {
		return modelRegistryRecord{}, fmt.Errorf("no model registry records found")
	}
	if direction != "" && direction != "lower" && direction != "higher" {
		return modelRegistryRecord{}, fmt.Errorf("--direction must be lower|higher")
	}
	var best modelRegistryRecord
	var bestValue float64
	var compareMetricName string
	var compareDirection string
	found := false
	for _, record := range records {
		value, ok := modelMetricValue(record, metricName)
		if !ok {
			continue
		}
		name := metricName
		if name == "" {
			name = record.PrimaryMetric.Name
		}
		recordDirection := direction
		if recordDirection == "" && metricName == "" {
			recordDirection = record.PrimaryMetric.Direction
		}
		if recordDirection == "" {
			if strings.Contains(strings.ToLower(name), "loss") {
				recordDirection = "lower"
			} else {
				recordDirection = "higher"
			}
		}
		if metricName == "" {
			if compareMetricName == "" && compareDirection == "" {
				compareMetricName = name
				compareDirection = recordDirection
			} else if name != compareMetricName || recordDirection != compareDirection {
				return modelRegistryRecord{}, fmt.Errorf("records use mixed primary metrics (%q/%s and %q/%s); pass --metric and --direction to compare explicitly", compareMetricName, compareDirection, name, recordDirection)
			}
		}
		if !found || (recordDirection == "lower" && value < bestValue) || (recordDirection == "higher" && value > bestValue) {
			best = record
			bestValue = value
			found = true
		}
	}
	if !found {
		return modelRegistryRecord{}, fmt.Errorf("no records have metric %q", metricName)
	}
	return best, nil
}

func parseModelRef(ref string) (modelRef, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return modelRef{}, fmt.Errorf("model ref is required")
	}
	if strings.Contains(ref, "@") {
		parts := strings.SplitN(ref, "@", 2)
		if parts[0] == "" || parts[1] == "" {
			return modelRef{}, fmt.Errorf("model ref %q must be MODEL@RUN", ref)
		}
		return modelRef{Model: parts[0], Run: parts[1]}, nil
	}
	if strings.Contains(ref, ":") {
		parts := strings.SplitN(ref, ":", 2)
		if parts[0] == "" || parts[1] == "" {
			return modelRef{}, fmt.Errorf("model ref %q must be MODEL:ALIAS", ref)
		}
		return modelRef{Model: parts[0], Alias: parts[1]}, nil
	}
	return modelRef{Model: ref, Alias: "default"}, nil
}

func resolveModelRef(ctx context.Context, kubeContext, namespace, pvcName, ref string) (modelRegistryRecord, error) {
	parsed, err := parseModelRef(ref)
	if err != nil {
		return modelRegistryRecord{}, err
	}
	if parsed.Run != "" {
		return fetchModelRegistryRecord(ctx, kubeContext, namespace, pvcName, parsed.Model, parsed.Run)
	}
	alias, err := fetchModelAlias(ctx, kubeContext, namespace, pvcName, parsed.Model, parsed.Alias)
	if err != nil {
		return modelRegistryRecord{}, err
	}
	return fetchModelRegistryRecord(ctx, kubeContext, namespace, pvcName, alias.Model, alias.Run)
}

func rebuildModelRegistryRecord(ctx context.Context, kubeContext, namespace, pvcName, run, modelOverride string) (modelRegistryRecord, error) {
	if modelOverride != "" {
		if err := validateRegistrySegment("model", modelOverride); err != nil {
			return modelRegistryRecord{}, err
		}
	}
	if modelOverride == "" {
		raw, err := fetchPVCFile(ctx, kubeContext, namespace, run, pvcName, storage.DurableFinetuneModelFile(run))
		if err == nil {
			record, parseErr := parseModelRegistryRecord(raw)
			if parseErr != nil {
				return modelRegistryRecord{}, fmt.Errorf("existing model.json for %s is corrupt: %w", run, parseErr)
			}
			return record, nil
		}
		if !isPVCNotFound(err) {
			return modelRegistryRecord{}, err
		}
	}
	artifactsRaw, artifactsPath, err := fetchManagedWorkflowArtifacts(ctx, kubeContext, namespace, run, pvcName)
	if err != nil {
		return modelRegistryRecord{}, err
	}
	var artifacts managedWorkflowArtifactIndex
	if err := json.Unmarshal(artifactsRaw, &artifacts); err != nil {
		return modelRegistryRecord{}, fmt.Errorf("parse artifacts.json for %s: %w", run, err)
	}
	modelName := modelOverride
	if modelName == "" {
		modelName = defaultModelRegistryModel
	}
	metricsPath := ""
	metrics := map[string]float64{}
	if metricsRaw, path, err := fetchFirstPVCFile(ctx, kubeContext, namespace, run, pvcName, managedWorkflowResultCandidatePaths(run, "")); err == nil {
		metricsPath = path
		metrics = numericMetricsFromJSON(metricsRaw)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	record := modelRegistryRecord{
		SchemaVersion:  1,
		Kind:           "tau.model",
		Model:          modelName,
		Run:            run,
		Namespace:      artifacts.Namespace,
		ResourceName:   artifacts.ResourceName,
		CreatedAt:      artifacts.CreatedAt,
		CompletedAt:    now,
		RecordPath:     storage.DurableFinetuneModelFile(run),
		RegistryPath:   storage.ModelRegistryRunFile(modelName, run),
		ArtifactsIndex: artifactsPath,
		ModelMetadata: map[string]any{
			"name": modelName,
			"tags": map[string]string{},
		},
		Tags:          map[string]string{},
		MetricsPath:   metricsPath,
		Metrics:       metrics,
		PrimaryMetric: inferPrimaryMetric(metrics, "", ""),
		Artifacts:     artifacts.Artifacts,
		StorageProbe:  artifacts.StorageProbe,
	}
	if record.CreatedAt == "" {
		record.CreatedAt = now
	}
	return record, nil
}

func writeModelRegistryRecord(ctx context.Context, kubeContext, namespace, pvcName string, record modelRegistryRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := writePVCFile(ctx, kubeContext, namespace, record.Run, pvcName, storage.DurableFinetuneModelFile(record.Run), raw); err != nil {
		return err
	}
	return writePVCFile(ctx, kubeContext, namespace, record.Model+"-"+record.Run, pvcName, storage.ModelRegistryRunFile(record.Model, record.Run), raw)
}

func numericMetricsFromJSON(raw []byte) map[string]float64 {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]float64{}
	}
	out := map[string]float64{}
	collectNumericMetrics(out, "", decoded)
	return out
}

func collectNumericMetrics(out map[string]float64, prefix string, value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			collectNumericMetrics(out, next, child)
		}
	case float64:
		if prefix != "" {
			out[prefix] = v
		}
	}
}

func inferPrimaryMetric(metrics map[string]float64, requested, direction string) modelRegistryMetric {
	name := requested
	if name == "" {
		for _, candidate := range []string{"loss", "eval.loss", "workload.loss", "train.loss", "score", "accuracy"} {
			if _, ok := metrics[candidate]; ok {
				name = candidate
				break
			}
		}
	}
	if name == "" {
		return modelRegistryMetric{}
	}
	value, ok := metrics[name]
	metric := modelRegistryMetric{Name: name}
	if ok {
		metric.Value = &value
	}
	if direction == "" {
		if strings.Contains(strings.ToLower(name), "loss") {
			direction = "lower"
		} else {
			direction = "higher"
		}
	}
	metric.Direction = direction
	return metric
}

func selectModelArtifact(record modelRegistryRecord, artifactName string) (managedWorkflowArtifact, error) {
	if artifactName == "" {
		artifactName = "checkpoint"
	}
	var names []string
	for _, artifact := range record.Artifacts {
		names = append(names, artifact.Name)
		if artifact.Name != artifactName && artifact.ManifestPath != artifactName {
			continue
		}
		if artifact.Status != "ready" {
			return managedWorkflowArtifact{}, fmt.Errorf("artifact %q for model %q run %q is not ready (status=%q)", artifactName, record.Model, record.Run, artifact.Status)
		}
		if artifact.DurablePath == "" {
			return managedWorkflowArtifact{}, fmt.Errorf("artifact %q for model %q run %q has no durable_path", artifactName, record.Model, record.Run)
		}
		return artifact, nil
	}
	return managedWorkflowArtifact{}, fmt.Errorf("artifact %q not found for model %q run %q (available: %s)", artifactName, record.Model, record.Run, strings.Join(names, ", "))
}

func isPVCNotFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "result artifact not found at ") || strings.Contains(err.Error(), "result directory not found at "))
}

func writePVCFile(ctx context.Context, kubeContext, namespace, runName, pvcName, path string, data []byte) error {
	if pvcName == "" {
		pvcName = defaultTauPVCName
	}
	podName := fmt.Sprintf("tau-write-%s-%d", sanitizePodName(runName), time.Now().Unix())
	if len(podName) > 60 {
		podName = podName[:60]
	}
	encoded, err := helperPodPayloadArg(data)
	if err != nil {
		return err
	}
	script := `set -eu
path="$1"
payload="$2"
tmp="${path}.tmp.$$"
mkdir -p "$(dirname "$path")"
printf '%s' "$payload" | base64 -d > "$tmp"
mv "$tmp" "$path"
`
	podYAML, err := helperPodYAML(helperPodSpec{
		Name:       podName,
		Namespace:  namespace,
		LabelApp:   "tau-pvc-write",
		Image:      pvcHelperImage,
		PVCName:    pvcName,
		TTLSec:     int(pvcHelperPodTTL.Seconds()),
		Script:     script,
		ScriptArgs: []string{path, encoded},
	})
	if err != nil {
		return fmt.Errorf("render helper pod: %w", err)
	}
	r := kube.New(kubeContext)
	if _, err := r.Raw(ctx, []string{"apply", "-n", namespace, "-f", "-"}, podYAML); err != nil {
		return fmt.Errorf("create helper pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = r.Raw(delCtx, []string{"delete", "pod", "-n", namespace, podName, "--wait=false", "--ignore-not-found"}, nil)
	}()
	phase, err := waitForHelperPodTerminal(ctx, r, namespace, podName, 90*time.Second)
	if err != nil {
		logs, _ := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		return fmt.Errorf("write helper pod did not finish: %w (logs: %s)", err, strings.TrimSpace(logs))
	}
	if phase != "Succeeded" {
		logs, _ := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		return fmt.Errorf("write helper pod did not succeed: phase=%s (logs: %s)", phase, strings.TrimSpace(logs))
	}
	return nil
}

func helperPodPayloadArg(data []byte) (string, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	if len(encoded) > maxHelperPodScriptArgPayload {
		return "", fmt.Errorf("registry payload is too large for helper pod args (%d bytes encoded > %d bytes)", len(encoded), maxHelperPodScriptArgPayload)
	}
	return encoded, nil
}
