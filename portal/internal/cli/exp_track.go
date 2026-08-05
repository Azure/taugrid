package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

const expTrackVersion = "tau.exp.track.v0"

type expTrackOptions struct {
	Project        string
	RunGroupID     string
	ParentRunID    string
	State          string
	Owner          string
	CreatedAt      string
	StartedAt      string
	CompletedAt    string
	ConfigHash     string
	CodeSHA        string
	ImageDigest    string
	TauCommand     string
	ResultURI      string
	Configs        []string
	ConfigFormat   string
	Artifacts      []string
	Runtime        string
	Dependencies   string
	LogURI         string
	Tags           []string
	Metrics        []string
	MetricStep     int64
	MetricWallTime string
	MetricSource   string
	MetricSplit    string
	MetricUnit     string
	IdempotencyKey string
}

type expTrackResult struct {
	expstore.RecordRunDataResult
	MetricRows int                        `json:"metric_rows"`
	MetricFile *expstore.MetricFileRecord `json:"metric_file,omitempty"`
}

type trackArtifactSpec struct {
	Type                 string `json:"type,omitempty"`
	Name                 string `json:"name,omitempty"`
	URI                  string `json:"uri,omitempty"`
	Preview              string `json:"preview,omitempty"`
	ExternalRef          string `json:"external_ref,omitempty"`
	Caption              string `json:"caption,omitempty"`
	Direction            string `json:"direction,omitempty"`
	Alias                string `json:"alias,omitempty"`
	SourceArtifactID     string `json:"source_artifact_id,omitempty"`
	SourceRunID          string `json:"source_run_id,omitempty"`
	SourceDatasetName    string `json:"source_dataset_name,omitempty"`
	SourceDatasetVersion string `json:"source_dataset_version,omitempty"`
	SourceDatasetDigest  string `json:"source_dataset_digest,omitempty"`
}

func newExpTrackCmd(storePath *string) *cobra.Command {
	var opts expTrackOptions
	var output string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "track RUN",
		Short: "Record explicit run metadata, configs, artifact pointers, tags, and scalar metrics",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyExperimentDefaults(cmd, nil, &opts.Project, nil, &opts.RunGroupID); err != nil {
				return err
			}
			out, err := normalizeExpOutput(output, jsonOutput, "table", "json")
			if err != nil {
				return err
			}
			store, err := openExpStore(cmd.Context(), storePath)
			if err != nil {
				return err
			}
			defer store.Close()

			run, err := buildTrackRunRecord(args[0], opts)
			if err != nil {
				return err
			}
			configs, derivedConfigHash, err := buildTrackConfigRecords(run.RunID, opts.Configs, opts.ConfigFormat)
			if err != nil {
				return err
			}
			if run.ConfigHash == "" {
				run.ConfigHash = derivedConfigHash
			}
			run = defaultTrackRunRecord(store.Manifest(), run)
			requestRun := run
			if existing, ok, err := existingRunRecord(cmd.Context(), store, run.RunID); err != nil {
				return err
			} else if ok && !trackRunMetadataChanged(cmd) {
				run = existing
			}
			tags, err := buildTrackTags(run.RunID, opts.Tags)
			if err != nil {
				return err
			}
			artifacts, err := buildTrackArtifacts(cmd.Context(), store, run.RunID, opts.Artifacts)
			if err != nil {
				return err
			}
			metricRows, minStep, maxStep, err := buildTrackMetricRows(cmd, run, opts, tags)
			if err != nil {
				return err
			}
			runContext := buildTrackRunContext(run.RunID, opts)
			requestHash, err := trackRequestHash(requestRun, runContext, configs, tags, artifacts, metricRows)
			if err != nil {
				return err
			}

			var metricFile *expstore.MetricFileRecord
			var metricPath string
			var wroteMetricFile bool
			if len(metricRows) > 0 {
				record, path, wrote, err := writeTrackMetricFile(cmd.Context(), store, run, metricRows, requestHash, minStep, maxStep)
				if err != nil {
					return err
				}
				metricFile = &record
				metricPath = path
				wroteMetricFile = wrote
			}

			recordOpts := expstore.RecordRunDataOptions{
				Run:            run,
				RunContext:     runContext,
				Configs:        configs,
				Tags:           tags,
				Artifacts:      artifacts,
				IdempotencyKey: opts.IdempotencyKey,
				Command:        "exp track",
				RequestHash:    requestHash,
			}
			if metricFile != nil {
				recordOpts.MetricFiles = []expstore.MetricFileRecord{*metricFile}
				recordOpts.MetricSummaries = expstore.SummarizeMetricRows(*metricFile, metricRows)
			}
			record, err := store.RecordRunData(cmd.Context(), recordOpts)
			if err != nil {
				if wroteMetricFile {
					if cleanupErr := os.Remove(metricPath); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
						return fmt.Errorf("%w; cleanup track metric file: %v", err, cleanupErr)
					}
				}
				return err
			}
			result := expTrackResult{
				RecordRunDataResult: record,
				MetricRows:          len(metricRows),
				MetricFile:          metricFile,
			}
			if out == "json" {
				return writeExpJSON(cmd.OutOrStdout(), result)
			}
			return writeTrackTable(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&opts.Project, "project", "", "project id for newly tracked runs (default: store manifest project)")
	cmd.Flags().StringVar(&opts.RunGroupID, "group", "", "run group id for newly tracked runs (default: default)")
	cmd.Flags().StringVar(&opts.ParentRunID, "parent-run", "", "parent run id for retry/lineage tracking")
	cmd.Flags().StringVar(&opts.State, "state", "", "run state (default: succeeded)")
	cmd.Flags().StringVar(&opts.Owner, "owner", "", "run owner")
	cmd.Flags().StringVar(&opts.CreatedAt, "created-at", "", "run creation time (RFC3339)")
	cmd.Flags().StringVar(&opts.StartedAt, "started-at", "", "run start time (RFC3339)")
	cmd.Flags().StringVar(&opts.CompletedAt, "completed-at", "", "run completion time (RFC3339)")
	cmd.Flags().StringVar(&opts.ConfigHash, "config-hash", "", "run config hash when no local --config file is available")
	cmd.Flags().StringVar(&opts.CodeSHA, "code-sha", "", "source commit SHA associated with the run")
	cmd.Flags().StringVar(&opts.ImageDigest, "image-digest", "", "container image digest associated with the run")
	cmd.Flags().StringVar(&opts.TauCommand, "command", "", "Tau or shell command used to produce the run")
	cmd.Flags().StringVar(&opts.ResultURI, "result-uri", "", "primary result URI or directory for the run")
	cmd.Flags().StringArrayVar(&opts.Configs, "config", nil, "local config file to hash and index (repeatable)")
	cmd.Flags().StringVar(&opts.ConfigFormat, "config-format", "", "config format override for --config files (default: from extension)")
	cmd.Flags().StringArrayVar(&opts.Artifacts, "artifact", nil, "artifact pointer as URI, type:name=URI, or JSON with type/name/uri/preview/external_ref/caption/lineage fields (repeatable)")
	cmd.Flags().StringVar(&opts.Runtime, "runtime", "", "runtime metadata JSON or string to attach to run context")
	cmd.Flags().StringVar(&opts.Dependencies, "dependencies", "", "dependency metadata JSON or string to attach to run context")
	cmd.Flags().StringVar(&opts.LogURI, "log-uri", "", "stdout/stderr/log pointer for reproducibility")
	cmd.Flags().StringArrayVar(&opts.Tags, "tag", nil, "run tag key=value (repeatable)")
	cmd.Flags().StringArrayVar(&opts.Metrics, "metric", nil, "scalar metric as name=value (repeatable)")
	cmd.Flags().Int64Var(&opts.MetricStep, "step", 0, "step to attach to all --metric rows")
	cmd.Flags().StringVar(&opts.MetricWallTime, "wall-time", "", "wall time for all --metric rows (RFC3339)")
	cmd.Flags().StringVar(&opts.MetricSource, "metric-source", "tau", "source label for scalar metric rows")
	cmd.Flags().StringVar(&opts.MetricSplit, "split", "", "split label for scalar metric rows")
	cmd.Flags().StringVar(&opts.MetricUnit, "unit", "", "unit label for scalar metric rows")
	cmd.Flags().StringVar(&opts.IdempotencyKey, "idempotency-key", "", "idempotency key for agent-safe repeated writes")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table|json")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON output")
	return cmd
}

func buildTrackRunRecord(runID string, opts expTrackOptions) (expstore.RunRecord, error) {
	createdAt, err := parseOptionalTrackTime("--created-at", opts.CreatedAt)
	if err != nil {
		return expstore.RunRecord{}, err
	}
	startedAt, err := parseOptionalTrackTime("--started-at", opts.StartedAt)
	if err != nil {
		return expstore.RunRecord{}, err
	}
	completedAt, err := parseOptionalTrackTime("--completed-at", opts.CompletedAt)
	if err != nil {
		return expstore.RunRecord{}, err
	}
	return expstore.RunRecord{
		RunID:       runID,
		Project:     opts.Project,
		RunGroupID:  opts.RunGroupID,
		ParentRunID: opts.ParentRunID,
		State:       opts.State,
		Owner:       opts.Owner,
		CreatedAt:   createdAt,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		ConfigHash:  opts.ConfigHash,
		CodeSHA:     opts.CodeSHA,
		ImageDigest: opts.ImageDigest,
		TauCommand:  opts.TauCommand,
		ResultURI:   opts.ResultURI,
	}, nil
}

func defaultTrackRunRecord(manifest expstore.Manifest, run expstore.RunRecord) expstore.RunRecord {
	if run.Project == "" {
		run.Project = manifest.Project
	}
	if run.Project == "" {
		run.Project = "default"
	}
	if run.RunGroupID == "" {
		run.RunGroupID = "default"
	}
	if run.State == "" {
		run.State = "succeeded"
	}
	if run.IndexVersion == "" {
		run.IndexVersion = expstore.SchemaVersion
	}
	return run
}

func buildTrackConfigRecords(runID string, paths []string, formatOverride string) ([]expstore.ConfigRecord, string, error) {
	configs := make([]expstore.ConfigRecord, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("--config %s: %w", path, err)
		}
		format := strings.TrimSpace(formatOverride)
		if format == "" {
			format = detectConfigFormat(path)
		}
		normalized, err := normalizeConfigJSON(raw, format)
		if err != nil {
			return nil, "", fmt.Errorf("--config %s: %w", path, err)
		}
		hashInput := raw
		if normalized != "" {
			hashInput = []byte(normalized)
		}
		configs = append(configs, expstore.ConfigRecord{
			ConfigHash:     fileutil.SHA256Hex(hashInput),
			RunID:          runID,
			Format:         format,
			URI:            filepath.ToSlash(filepath.Clean(path)),
			NormalizedJSON: normalized,
			IndexedFields:  indexedConfigFields(normalized),
		})
	}
	if len(configs) == 0 {
		return configs, "", nil
	}

	hashes := make([]string, 0, len(configs))
	for _, config := range configs {
		hashes = append(hashes, config.ConfigHash)
	}
	sort.Strings(hashes)
	if len(hashes) == 1 {
		return configs, hashes[0], nil
	}
	return configs, fileutil.SHA256Hex([]byte(strings.Join(hashes, "\n"))), nil
}

func indexedConfigFields(normalized string) string {
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return ""
	}
	var payload any
	if err := json.Unmarshal([]byte(normalized), &payload); err != nil {
		return ""
	}
	fields := map[string]any{}
	flattenConfigFields("", payload, fields)
	if len(fields) == 0 {
		return ""
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return string(raw)
}

func flattenConfigFields(prefix string, value any, out map[string]any) {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v["value"]; ok && configScalar(raw) {
			if prefix != "" {
				out[prefix] = raw
			}
			return
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flattenConfigFields(next, v[key], out)
		}
	case []any:
		if prefix != "" && len(v) <= 8 {
			for _, item := range v {
				if !configScalar(item) {
					return
				}
			}
			out[prefix] = v
		}
	default:
		if prefix != "" && configScalar(v) {
			out[prefix] = v
		}
	}
}

func configScalar(value any) bool {
	switch value.(type) {
	case string, bool, float64, int, int64, json.Number:
		return true
	default:
		return false
	}
}

func buildTrackRunContext(runID string, opts expTrackOptions) *expstore.RunContextRecord {
	if strings.TrimSpace(opts.Runtime) == "" && strings.TrimSpace(opts.Dependencies) == "" && strings.TrimSpace(opts.LogURI) == "" {
		return nil
	}
	return &expstore.RunContextRecord{
		RunID:        runID,
		Runtime:      opts.Runtime,
		Dependencies: opts.Dependencies,
		LogURI:       opts.LogURI,
	}
}

func detectConfigFormat(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	default:
		return "raw"
	}
}

func normalizeConfigJSON(raw []byte, format string) (string, error) {
	switch strings.ToLower(format) {
	case "json":
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("parse JSON config: %w", err)
		}
		normalized, err := json.Marshal(normalizeYAMLValue(value))
		if err != nil {
			return "", err
		}
		return string(normalized), nil
	case "yaml", "yml":
		var value any
		if err := yaml.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("parse YAML config: %w", err)
		}
		normalized, err := json.Marshal(normalizeYAMLValue(value))
		if err != nil {
			return "", err
		}
		return string(normalized), nil
	default:
		return "", nil
	}
}

func normalizeYAMLValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			out[key] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for key, item := range v {
			out[fmt.Sprint(key)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, normalizeYAMLValue(item))
		}
		return out
	default:
		return value
	}
}

func buildTrackTags(runID string, specs []string) ([]expstore.TagRecord, error) {
	tagMap, err := parseExpTags(specs)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(tagMap))
	for key := range tagMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tags := make([]expstore.TagRecord, 0, len(keys))
	for _, key := range keys {
		tags = append(tags, expstore.TagRecord{ScopeType: "run", ScopeID: runID, Key: key, Value: tagMap[key]})
	}
	return tags, nil
}

func buildTrackArtifacts(ctx context.Context, store *expstore.Store, runID string, specs []string) ([]expstore.ArtifactRecord, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	artifacts := make([]expstore.ArtifactRecord, 0, len(specs))
	for _, spec := range specs {
		parsed, err := parseTrackArtifactSpec(spec)
		if err != nil {
			return nil, err
		}
		identityHash := fileutil.SHA256Hex([]byte(strings.Join([]string{
			parsed.Type,
			parsed.Name,
			parsed.URI,
			parsed.Preview,
			parsed.ExternalRef,
			parsed.Caption,
			parsed.Direction,
			parsed.Alias,
			parsed.SourceArtifactID,
			parsed.SourceRunID,
			parsed.SourceDatasetName,
			parsed.SourceDatasetVersion,
			parsed.SourceDatasetDigest,
		}, "\x00")))
		artifact := expstore.ArtifactRecord{
			ArtifactID:           "artifact-" + runID + "-" + fileutil.ShortDigest(identityHash, 12),
			RunID:                runID,
			Type:                 parsed.Type,
			URI:                  filepath.ToSlash(parsed.URI),
			Name:                 parsed.Name,
			CreatedAt:            now,
			Preview:              filepath.ToSlash(parsed.Preview),
			ExternalRef:          parsed.ExternalRef,
			Caption:              parsed.Caption,
			Direction:            parsed.Direction,
			Alias:                parsed.Alias,
			SourceArtifactID:     parsed.SourceArtifactID,
			SourceRunID:          parsed.SourceRunID,
			SourceDatasetName:    parsed.SourceDatasetName,
			SourceDatasetVersion: parsed.SourceDatasetVersion,
			SourceDatasetDigest:  parsed.SourceDatasetDigest,
		}
		if digest, size, modTime, ok, err := localArtifactIdentity(parsed.URI); err != nil {
			return nil, err
		} else if ok {
			artifact.Digest = digest
			if size >= 0 {
				artifact.SizeBytes = &size
			}
			if modTime != "" {
				artifact.CreatedAt = modTime
			}
		} else if existingCreatedAt, err := existingArtifactCreatedAt(ctx, store, artifact.ArtifactID); err != nil {
			return nil, err
		} else if existingCreatedAt != "" {
			artifact.CreatedAt = existingCreatedAt
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func parseTrackArtifactSpec(spec string) (trackArtifactSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return trackArtifactSpec{}, fmt.Errorf("--artifact cannot be empty")
	}
	if strings.HasPrefix(spec, "{") {
		var artifact trackArtifactSpec
		dec := json.NewDecoder(strings.NewReader(spec))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&artifact); err != nil {
			return trackArtifactSpec{}, fmt.Errorf("--artifact JSON: %w", err)
		}
		return normalizeTrackArtifactSpec(spec, artifact)
	}
	artifact := trackArtifactSpec{URI: spec}
	if left, right, ok := strings.Cut(spec, "="); ok {
		artifact.URI = strings.TrimSpace(right)
		left = strings.TrimSpace(left)
		if leftType, leftName, hasType := strings.Cut(left, ":"); hasType {
			artifact.Type = strings.TrimSpace(leftType)
			artifact.Name = strings.TrimSpace(leftName)
		} else {
			artifact.Name = left
		}
	}
	return normalizeTrackArtifactSpec(spec, artifact)
}

func normalizeTrackArtifactSpec(raw string, artifact trackArtifactSpec) (trackArtifactSpec, error) {
	artifact.Type = strings.TrimSpace(artifact.Type)
	artifact.Name = strings.TrimSpace(artifact.Name)
	artifact.URI = strings.TrimSpace(artifact.URI)
	artifact.Preview = strings.TrimSpace(artifact.Preview)
	artifact.ExternalRef = strings.TrimSpace(artifact.ExternalRef)
	artifact.Caption = strings.TrimSpace(artifact.Caption)
	artifact.Direction = strings.ToLower(strings.TrimSpace(artifact.Direction))
	artifact.Alias = strings.TrimSpace(artifact.Alias)
	artifact.SourceArtifactID = strings.TrimSpace(artifact.SourceArtifactID)
	artifact.SourceRunID = strings.TrimSpace(artifact.SourceRunID)
	artifact.SourceDatasetName = strings.TrimSpace(artifact.SourceDatasetName)
	artifact.SourceDatasetVersion = strings.TrimSpace(artifact.SourceDatasetVersion)
	artifact.SourceDatasetDigest = strings.TrimSpace(artifact.SourceDatasetDigest)
	if artifact.URI == "" {
		return trackArtifactSpec{}, fmt.Errorf("--artifact %q has an empty URI", raw)
	}
	if artifact.Name == "" {
		artifact.Name = artifactNameFromURI(artifact.URI)
	}
	if artifact.Type == "" {
		artifact.Type = guessArtifactType(artifact.URI)
	}
	if artifact.Type == "" || artifact.Name == "" {
		return trackArtifactSpec{}, fmt.Errorf("--artifact %q must provide a type/name or a path with a basename", raw)
	}
	switch artifact.Direction {
	case "", "input", "output":
	default:
		return trackArtifactSpec{}, fmt.Errorf("--artifact %q direction must be input or output", raw)
	}
	artifact.Type = strings.ToLower(artifact.Type)
	return artifact, nil
}

func artifactNameFromURI(uri string) string {
	clean := strings.TrimRight(uri, "/\\")
	name := filepath.Base(clean)
	if name == "." || name == string(filepath.Separator) {
		return "artifact"
	}
	return name
}

func guessArtifactType(uri string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimRight(uri, "/\\")))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".svg", ".gif":
		return "plot"
	case ".mp4", ".mov", ".webm":
		return "video"
	case ".pt", ".pth", ".ckpt", ".safetensors":
		return "checkpoint"
	case ".log", ".txt":
		return "log"
	case ".json", ".yaml", ".yml":
		return "manifest"
	case ".csv", ".tsv", ".parquet":
		return "table"
	default:
		return "other"
	}
}

func localArtifactIdentity(uri string) (string, int64, string, bool, error) {
	if looksLikeRemoteURI(uri) {
		return "", 0, "", false, nil
	}
	info, err := os.Stat(uri)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, "", false, nil
		}
		return "", 0, "", false, err
	}
	modTime := info.ModTime().UTC().Format(time.RFC3339)
	if info.IsDir() {
		return "", -1, modTime, true, nil
	}
	digest, size, err := fileutil.FileSHA256(uri)
	if err != nil {
		return "", 0, "", false, err
	}
	return digest, size, modTime, true, nil
}

func looksLikeRemoteURI(uri string) bool {
	return strings.Contains(uri, "://")
}

func existingArtifactCreatedAt(ctx context.Context, store *expstore.Store, artifactID string) (string, error) {
	rows, err := store.Query(ctx, "select created_at from artifacts where artifact_id = "+sqlStringLiteral(artifactID))
	if err != nil {
		return "", err
	}
	if len(rows.Rows) == 0 {
		return "", nil
	}
	return expCell(rows.Rows[0]["created_at"]), nil
}

func buildTrackMetricRows(cmd *cobra.Command, run expstore.RunRecord, opts expTrackOptions, runTags []expstore.TagRecord) ([]expstore.MetricRow, *int64, *int64, error) {
	if len(opts.Metrics) == 0 {
		return nil, nil, nil, nil
	}
	source := strings.TrimSpace(opts.MetricSource)
	if source == "" {
		source = "tau"
	}
	var step *int64
	if cmd.Flags().Changed("step") {
		v := opts.MetricStep
		step = &v
	}
	var wallTime *int64
	if opts.MetricWallTime != "" {
		parsed, err := time.Parse(time.RFC3339, opts.MetricWallTime)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("--wall-time must be RFC3339: %w", err)
		}
		v := parsed.UTC().UnixMicro()
		wallTime = &v
	}
	var minStep, maxStep *int64
	metricTags := map[string]string{}
	for _, tag := range runTags {
		if tag.ScopeType == "run" && tag.ScopeID == run.RunID {
			metricTags[tag.Key] = tag.Value
		}
	}
	metricTags["tau.metric.source"] = "track"
	metricTags["tau.metric.write_version"] = expTrackVersion
	encodedTags, err := json.Marshal(metricTags)
	if err != nil {
		return nil, nil, nil, err
	}
	rows := make([]expstore.MetricRow, 0, len(opts.Metrics))
	for _, spec := range opts.Metrics {
		name, value, err := parseTrackMetricSpec(spec)
		if err != nil {
			return nil, nil, nil, err
		}
		if step != nil {
			if minStep == nil || *step < *minStep {
				v := *step
				minStep = &v
			}
			if maxStep == nil || *step > *maxStep {
				v := *step
				maxStep = &v
			}
		}
		rows = append(rows, expstore.MetricRow{
			Project:    run.Project,
			RunGroupID: run.RunGroupID,
			RunID:      run.RunID,
			MetricName: name,
			Step:       step,
			WallTime:   wallTime,
			Value:      value,
			Unit:       optionalTrackString(opts.MetricUnit),
			Source:     source,
			Split:      optionalTrackString(opts.MetricSplit),
			Tags:       string(encodedTags),
		})
	}
	return rows, minStep, maxStep, nil
}

func parseTrackMetricSpec(spec string) (string, float64, error) {
	name, rawValue, ok := strings.Cut(strings.TrimSpace(spec), "=")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(rawValue) == "" {
		return "", 0, fmt.Errorf("--metric: expected name=value, got %q", spec)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
	if err != nil {
		return "", 0, fmt.Errorf("--metric %q: parse value: %w", spec, err)
	}
	return strings.TrimSpace(name), value, nil
}

func writeTrackMetricFile(ctx context.Context, store *expstore.Store, run expstore.RunRecord, rows []expstore.MetricRow, requestHash string, minStep, maxStep *int64) (expstore.MetricFileRecord, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return expstore.MetricFileRecord{}, "", false, err
	}
	fileID := "track-" + run.RunID + "-" + fileutil.ShortDigest(requestHash, 12)
	if existing, ok, err := existingMetricFileRecord(ctx, store, fileID); err != nil {
		return expstore.MetricFileRecord{}, "", false, err
	} else if ok {
		path := filepath.Join(store.Root, filepath.FromSlash(existing.Path))
		if _, err := os.Stat(path); err == nil {
			return existing, path, false, nil
		} else if err != nil && !os.IsNotExist(err) {
			return expstore.MetricFileRecord{}, "", false, err
		}
	}
	rel := filepath.Join(
		expstore.MetricsDir,
		"project="+run.Project,
		"group="+run.RunGroupID,
		"run="+run.RunID,
		"track-"+fileutil.ShortDigest(requestHash, 12)+".parquet",
	)
	path := filepath.Join(store.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return expstore.MetricFileRecord{}, "", false, err
	}
	if err := parquet.WriteFile(path, rows); err != nil {
		return expstore.MetricFileRecord{}, "", false, fmt.Errorf("write track metric parquet: %w", err)
	}
	digest, _, err := fileutil.FileSHA256(path)
	if err != nil {
		return expstore.MetricFileRecord{}, "", true, err
	}
	record := expstore.MetricFileRecord{
		FileID:        fileID,
		Path:          filepath.ToSlash(rel),
		Format:        "parquet",
		SchemaVersion: expstore.MetricSchemaVersion,
		SchemaHash:    metricTrackSchemaHash(),
		Project:       run.Project,
		RunGroupID:    run.RunGroupID,
		RunID:         run.RunID,
		RowCount:      int64(len(rows)),
		Digest:        digest,
		MinStep:       minStep,
		MaxStep:       maxStep,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	return record, path, true, nil
}

func existingMetricFileRecord(ctx context.Context, store *expstore.Store, fileID string) (expstore.MetricFileRecord, bool, error) {
	result, err := store.Query(ctx, `select file_id, path, format, schema_version, schema_hash, project,
       run_group_id, run_id, row_count, digest, min_step, max_step, created_at
from metric_files where file_id = `+sqlStringLiteral(fileID))
	if err != nil {
		return expstore.MetricFileRecord{}, false, err
	}
	if len(result.Rows) == 0 {
		return expstore.MetricFileRecord{}, false, nil
	}
	row := result.Rows[0]
	record := expstore.MetricFileRecord{
		FileID:        expCell(row["file_id"]),
		Path:          expCell(row["path"]),
		Format:        expCell(row["format"]),
		SchemaVersion: expCell(row["schema_version"]),
		SchemaHash:    expCell(row["schema_hash"]),
		Project:       expCell(row["project"]),
		RunGroupID:    expCell(row["run_group_id"]),
		RunID:         expCell(row["run_id"]),
		RowCount:      int64Value(row["row_count"]),
		Digest:        expCell(row["digest"]),
		MinStep:       optionalInt64Value(row["min_step"]),
		MaxStep:       optionalInt64Value(row["max_step"]),
		CreatedAt:     expCell(row["created_at"]),
	}
	return record, true, nil
}

func trackRequestHash(run expstore.RunRecord, runContext *expstore.RunContextRecord, configs []expstore.ConfigRecord, tags []expstore.TagRecord, artifacts []expstore.ArtifactRecord, metricRows []expstore.MetricRow) (string, error) {
	stableArtifacts := make([]expstore.ArtifactRecord, len(artifacts))
	copy(stableArtifacts, artifacts)
	for i := range stableArtifacts {
		stableArtifacts[i].CreatedAt = ""
	}
	payload := struct {
		Version   string                     `json:"version"`
		Run       expstore.RunRecord         `json:"run"`
		Context   *expstore.RunContextRecord `json:"context,omitempty"`
		Configs   []expstore.ConfigRecord    `json:"configs,omitempty"`
		Tags      []expstore.TagRecord       `json:"tags,omitempty"`
		Artifacts []expstore.ArtifactRecord  `json:"artifacts,omitempty"`
		Metrics   []expstore.MetricRow       `json:"metrics,omitempty"`
	}{
		Version:   expTrackVersion,
		Run:       run,
		Context:   runContext,
		Configs:   configs,
		Tags:      tags,
		Artifacts: stableArtifacts,
		Metrics:   metricRows,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return fileutil.SHA256Hex(raw), nil
}

func trackRunMetadataChanged(cmd *cobra.Command) bool {
	for _, flag := range []string{
		"project", "group", "parent-run", "state", "owner",
		"created-at", "started-at", "completed-at", "config-hash", "code-sha",
		"image-digest", "command", "result-uri",
	} {
		if cmd.Flags().Changed(flag) {
			return true
		}
	}
	return false
}

func parseOptionalTrackTime(flag, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("%s must be RFC3339: %w", flag, err)
	}
	return parsed.UTC().Format(time.RFC3339), nil
}

func optionalTrackString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func metricTrackSchemaHash() string {
	return fileutil.SHA256Hex([]byte(strings.Join(expstore.MetricSchemaColumns, ",")))
}

func sqlStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func int64Value(value any) int64 {
	if value == nil {
		return 0
	}
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		parsed, _ := strconv.ParseInt(v, 10, 64)
		return parsed
	default:
		parsed, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		return parsed
	}
}

func optionalInt64Value(value any) *int64 {
	if value == nil {
		return nil
	}
	parsed := int64Value(value)
	return &parsed
}

func writeTrackTable(w io.Writer, result expTrackResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "run\t%s\n", result.RunID)
	fmt.Fprintf(tw, "created_run\t%t\n", result.CreatedRun)
	fmt.Fprintf(tw, "configs\t%d\n", result.Configs)
	fmt.Fprintf(tw, "artifacts\t%d\n", result.Artifacts)
	fmt.Fprintf(tw, "tags\t%d\n", result.Tags)
	fmt.Fprintf(tw, "metric_files\t%d\n", result.MetricFiles)
	fmt.Fprintf(tw, "metric_rows\t%d\n", result.MetricRows)
	fmt.Fprintf(tw, "reused\t%t\n", result.Reused)
	if result.IdempotencyKey != "" {
		fmt.Fprintf(tw, "idempotency_key\t%s\n", result.IdempotencyKey)
	}
	return tw.Flush()
}
