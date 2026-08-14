// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/fileutil"

	"github.com/parquet-go/parquet-go"
)

const ADXProjectionVersion = "tau.exp.adx.v1"

type ADXExportOptions struct {
	Out        string
	Format     string
	Force      bool
	DryRun     bool
	ExportedAt time.Time
}

type ADXMetricsExportOptions struct {
	Out           string
	Format        string
	FileName      string
	ExportedAt    time.Time
	MetricFileIDs []string
}

type ADXExportResult struct {
	SourceStoreID       string                 `json:"source_store_id"`
	SourceStorePath     string                 `json:"source_store_path"`
	SourceSchemaVersion string                 `json:"source_schema_version"`
	ProjectionVersion   string                 `json:"projection_version"`
	ExportedAt          string                 `json:"exported_at"`
	Mode                string                 `json:"mode"`
	Format              string                 `json:"format"`
	Destination         string                 `json:"destination,omitempty"`
	SchemaFile          string                 `json:"schema_file,omitempty"`
	ManifestFile        string                 `json:"manifest_file,omitempty"`
	Tables              []ADXExportTableResult `json:"tables"`
}

type ADXMetricsExportResult struct {
	SourceStoreID       string `json:"source_store_id"`
	SourceStorePath     string `json:"source_store_path"`
	SourceSchemaVersion string `json:"source_schema_version"`
	ProjectionVersion   string `json:"projection_version"`
	ExportedAt          string `json:"exported_at"`
	Format              string `json:"format"`
	Destination         string `json:"destination"`
	MetricsFile         string `json:"metrics_file"`
	Rows                int    `json:"rows"`
}

type ADXExportTableResult struct {
	Name        string      `json:"name"`
	SourceTable string      `json:"source_table"`
	File        string      `json:"file,omitempty"`
	Rows        int         `json:"rows"`
	Columns     []ADXColumn `json:"columns"`
}

type ADXTableSchema struct {
	Name        string      `json:"name"`
	SourceTable string      `json:"source_table"`
	Columns     []ADXColumn `json:"columns"`
	selectSQL   string
}

type ADXColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

var adxMetadataColumns = []ADXColumn{
	{Name: "exported_at", Type: "datetime"},
	{Name: "source_store_id", Type: "string"},
	{Name: "source_store_path", Type: "string"},
	{Name: "source_schema_version", Type: "string"},
	{Name: "projection_version", Type: "string"},
}

var adxProjectionSchemas = []ADXTableSchema{
	newADXTableSchema("TauExpRuns", "runs", `
SELECT run_id, project, experiment_id, run_group_id, parent_run_id, state, owner,
       created_at, started_at, completed_at, config_hash, code_sha, image_digest, tau_command,
       result_uri, index_version
FROM runs
ORDER BY created_at, run_id`,
		ADXColumn{Name: "run_id", Type: "string"},
		ADXColumn{Name: "project", Type: "string"},
		ADXColumn{Name: "experiment_id", Type: "string"},
		ADXColumn{Name: "run_group_id", Type: "string"},
		ADXColumn{Name: "parent_run_id", Type: "string"},
		ADXColumn{Name: "state", Type: "string"},
		ADXColumn{Name: "owner", Type: "string"},
		ADXColumn{Name: "created_at", Type: "datetime"},
		ADXColumn{Name: "started_at", Type: "datetime"},
		ADXColumn{Name: "completed_at", Type: "datetime"},
		ADXColumn{Name: "config_hash", Type: "string"},
		ADXColumn{Name: "code_sha", Type: "string"},
		ADXColumn{Name: "image_digest", Type: "string"},
		ADXColumn{Name: "tau_command", Type: "string"},
		ADXColumn{Name: "result_uri", Type: "string"},
		ADXColumn{Name: "index_version", Type: "string"},
	),
	newADXTableSchema("TauExpRunContext", "run_context", `
SELECT run_id, cluster, namespace, team, profile, lane, local_queue, cluster_queue,
       kueue_workload, pod_uid, ray_job, resource_claims, gpu_class, gpu_count,
       node_names, mounts, queue_wait_seconds, gpu_hours, estimated_cost,
       runtime, dependencies, log_uri
FROM run_context
ORDER BY run_id`,
		ADXColumn{Name: "run_id", Type: "string"},
		ADXColumn{Name: "cluster", Type: "string"},
		ADXColumn{Name: "namespace", Type: "string"},
		ADXColumn{Name: "team", Type: "string"},
		ADXColumn{Name: "profile", Type: "string"},
		ADXColumn{Name: "lane", Type: "string"},
		ADXColumn{Name: "local_queue", Type: "string"},
		ADXColumn{Name: "cluster_queue", Type: "string"},
		ADXColumn{Name: "kueue_workload", Type: "string"},
		ADXColumn{Name: "pod_uid", Type: "string"},
		ADXColumn{Name: "ray_job", Type: "string"},
		ADXColumn{Name: "resource_claims", Type: "string"},
		ADXColumn{Name: "gpu_class", Type: "string"},
		ADXColumn{Name: "gpu_count", Type: "long"},
		ADXColumn{Name: "node_names", Type: "string"},
		ADXColumn{Name: "mounts", Type: "string"},
		ADXColumn{Name: "queue_wait_seconds", Type: "real"},
		ADXColumn{Name: "gpu_hours", Type: "real"},
		ADXColumn{Name: "estimated_cost", Type: "real"},
		ADXColumn{Name: "runtime", Type: "string"},
		ADXColumn{Name: "dependencies", Type: "string"},
		ADXColumn{Name: "log_uri", Type: "string"},
	),
	newADXTableSchema("TauExpMetricFiles", "metric_files", `
SELECT file_id, path, format, schema_version, schema_hash, project,
       run_group_id, run_id, row_count, digest, min_step, max_step, created_at
FROM metric_files
ORDER BY created_at, file_id`,
		ADXColumn{Name: "file_id", Type: "string"},
		ADXColumn{Name: "path", Type: "string"},
		ADXColumn{Name: "format", Type: "string"},
		ADXColumn{Name: "schema_version", Type: "string"},
		ADXColumn{Name: "schema_hash", Type: "string"},
		ADXColumn{Name: "project", Type: "string"},
		ADXColumn{Name: "run_group_id", Type: "string"},
		ADXColumn{Name: "run_id", Type: "string"},
		ADXColumn{Name: "row_count", Type: "long"},
		ADXColumn{Name: "digest", Type: "string"},
		ADXColumn{Name: "min_step", Type: "long"},
		ADXColumn{Name: "max_step", Type: "long"},
		ADXColumn{Name: "created_at", Type: "datetime"},
	),
	newADXTableSchema("TauExpArtifacts", "artifacts", `
SELECT artifact_id, run_id, type, uri, name, digest, size_bytes, created_at, preview, external_ref,
       caption, direction, alias, source_artifact_id, source_run_id,
       source_dataset_name, source_dataset_version, source_dataset_digest
FROM artifacts
ORDER BY created_at, artifact_id`,
		ADXColumn{Name: "artifact_id", Type: "string"},
		ADXColumn{Name: "run_id", Type: "string"},
		ADXColumn{Name: "type", Type: "string"},
		ADXColumn{Name: "uri", Type: "string"},
		ADXColumn{Name: "name", Type: "string"},
		ADXColumn{Name: "digest", Type: "string"},
		ADXColumn{Name: "size_bytes", Type: "long"},
		ADXColumn{Name: "created_at", Type: "datetime"},
		ADXColumn{Name: "preview", Type: "string"},
		ADXColumn{Name: "external_ref", Type: "string"},
		ADXColumn{Name: "caption", Type: "string"},
		ADXColumn{Name: "direction", Type: "string"},
		ADXColumn{Name: "alias", Type: "string"},
		ADXColumn{Name: "source_artifact_id", Type: "string"},
		ADXColumn{Name: "source_run_id", Type: "string"},
		ADXColumn{Name: "source_dataset_name", Type: "string"},
		ADXColumn{Name: "source_dataset_version", Type: "string"},
		ADXColumn{Name: "source_dataset_digest", Type: "string"},
	),
	newADXTableSchema("TauExpEvents", "events", `
SELECT event_id, run_id, time, type, source, severity, message, payload
FROM events
ORDER BY time, event_id`,
		ADXColumn{Name: "event_id", Type: "string"},
		ADXColumn{Name: "run_id", Type: "string"},
		ADXColumn{Name: "time", Type: "datetime"},
		ADXColumn{Name: "type", Type: "string"},
		ADXColumn{Name: "source", Type: "string"},
		ADXColumn{Name: "severity", Type: "string"},
		ADXColumn{Name: "message", Type: "string"},
		ADXColumn{Name: "payload", Type: "string"},
	),
	newADXTableSchema("TauExpObservations", "observations", `
SELECT observation_id, idempotency_key, author, source, type, scope_type, scope_id,
       text, evidence, created_at
FROM observations
ORDER BY created_at, observation_id`,
		ADXColumn{Name: "observation_id", Type: "string"},
		ADXColumn{Name: "idempotency_key", Type: "string"},
		ADXColumn{Name: "author", Type: "string"},
		ADXColumn{Name: "source", Type: "string"},
		ADXColumn{Name: "type", Type: "string"},
		ADXColumn{Name: "scope_type", Type: "string"},
		ADXColumn{Name: "scope_id", Type: "string"},
		ADXColumn{Name: "text", Type: "string"},
		ADXColumn{Name: "evidence", Type: "string"},
		ADXColumn{Name: "created_at", Type: "datetime"},
	),
	newADXTableSchema("TauExpConfigs", "configs", `
SELECT config_hash, run_id, format, uri, normalized_json, indexed_fields
FROM configs
ORDER BY config_hash, run_id`,
		ADXColumn{Name: "config_hash", Type: "string"},
		ADXColumn{Name: "run_id", Type: "string"},
		ADXColumn{Name: "format", Type: "string"},
		ADXColumn{Name: "uri", Type: "string"},
		ADXColumn{Name: "normalized_json", Type: "string"},
		ADXColumn{Name: "indexed_fields", Type: "string"},
	),
}

var adxMetricProjectionSchema = newADXTableSchema(exptelemetry.ProjectionTable, "metric_files", "",
	ADXColumn{Name: "metric_file_id", Type: "string"},
	ADXColumn{Name: "metric_file_path", Type: "string"},
	ADXColumn{Name: "project", Type: "string"},
	ADXColumn{Name: "experiment_id", Type: "string"},
	ADXColumn{Name: "run_group_id", Type: "string"},
	ADXColumn{Name: "run_id", Type: "string"},
	ADXColumn{Name: "metric_name", Type: "string"},
	ADXColumn{Name: "step", Type: "long"},
	ADXColumn{Name: "wall_time", Type: "datetime"},
	ADXColumn{Name: "value", Type: "real"},
	ADXColumn{Name: "unit", Type: "string"},
	ADXColumn{Name: "source", Type: "string"},
	ADXColumn{Name: "split", Type: "string"},
	ADXColumn{Name: "tags", Type: "string"},
)

func newADXTableSchema(name, sourceTable, selectSQL string, columns ...ADXColumn) ADXTableSchema {
	allColumns := make([]ADXColumn, 0, len(adxMetadataColumns)+len(columns))
	allColumns = append(allColumns, adxMetadataColumns...)
	allColumns = append(allColumns, columns...)
	return ADXTableSchema{
		Name:        name,
		SourceTable: sourceTable,
		Columns:     allColumns,
		selectSQL:   strings.TrimSpace(selectSQL),
	}
}

func ADXProjectionSchemas() []ADXTableSchema {
	source := append([]ADXTableSchema(nil), adxProjectionSchemas...)
	source = append(source, adxMetricProjectionSchema)
	schemas := make([]ADXTableSchema, len(source))
	for i, schema := range source {
		schemas[i] = schema
		schemas[i].Columns = append([]ADXColumn(nil), schema.Columns...)
	}
	return schemas
}

func ADXProjectionKQL() string {
	var b strings.Builder
	b.WriteString("// Tau exp ADX/Kusto downstream projection.\n")
	b.WriteString("// The local expstore/index append log remains authoritative; these tables are analytics mirrors only.\n\n")
	for _, schema := range ADXProjectionSchemas() {
		fmt.Fprintf(&b, ".create-merge table %s (\n", schema.Name)
		for i, col := range schema.Columns {
			comma := ","
			if i == len(schema.Columns)-1 {
				comma = ""
			}
			fmt.Fprintf(&b, "    %s: %s%s\n", adxKustoColumnName(col.Name), col.Type, comma)
		}
		b.WriteString(")\n\n")
	}
	return b.String()
}

func adxKustoColumnName(name string) string {
	switch name {
	case "project", "time", "type":
		return fmt.Sprintf("['%s']", name)
	}
	return name
}

func (s *Store) ExportADX(ctx context.Context, opts ADXExportOptions) (ADXExportResult, error) {
	if err := ctx.Err(); err != nil {
		return ADXExportResult{}, err
	}
	format := opts.Format
	if format == "" {
		format = "jsonl"
	}
	if err := validateADXExportFormat(format); err != nil {
		return ADXExportResult{}, err
	}
	exportedAt := opts.ExportedAt
	if exportedAt.IsZero() {
		exportedAt = time.Now().UTC()
	} else {
		exportedAt = exportedAt.UTC()
	}
	sourceStoreID, err := s.adxSourceStoreID()
	if err != nil {
		return ADXExportResult{}, err
	}
	mode := "local-files"
	if opts.DryRun {
		mode = "dry-run"
	}
	result := ADXExportResult{
		SourceStoreID:       sourceStoreID,
		SourceStorePath:     s.Root,
		SourceSchemaVersion: s.manifest.SchemaVersion,
		ProjectionVersion:   ADXProjectionVersion,
		ExportedAt:          exportedAt.Format(time.RFC3339),
		Mode:                mode,
		Format:              format,
	}

	dest := ""
	if !opts.DryRun {
		dest, err = s.prepareADXDestination(opts)
		if err != nil {
			return ADXExportResult{}, err
		}
		result.Destination = dest
	}
	meta := map[string]any{
		"exported_at":           result.ExportedAt,
		"source_store_id":       result.SourceStoreID,
		"source_store_path":     result.SourceStorePath,
		"source_schema_version": result.SourceSchemaVersion,
		"projection_version":    result.ProjectionVersion,
	}

	for _, schema := range adxProjectionSchemas {
		rows, err := s.queryRows(ctx, schema.selectSQL, nil)
		if err != nil {
			return ADXExportResult{}, fmt.Errorf("project %s: %w", schema.SourceTable, err)
		}
		table := ADXExportTableResult{
			Name:        schema.Name,
			SourceTable: schema.SourceTable,
			Rows:        len(rows.Rows),
			Columns:     append([]ADXColumn(nil), schema.Columns...),
		}
		if !opts.DryRun {
			file := filepath.Join(dest, schema.Name+"."+format)
			if err := writeADXProjectionFile(file, format, schema, rows.Rows, meta); err != nil {
				return ADXExportResult{}, err
			}
			table.File = file
		}
		result.Tables = append(result.Tables, table)
	}
	metricRows, err := s.adxMetricRowsForFiles(ctx, nil)
	if err != nil {
		return ADXExportResult{}, err
	}
	metricTable := ADXExportTableResult{
		Name:        adxMetricProjectionSchema.Name,
		SourceTable: adxMetricProjectionSchema.SourceTable,
		Rows:        len(metricRows),
		Columns:     append([]ADXColumn(nil), adxMetricProjectionSchema.Columns...),
	}
	if !opts.DryRun {
		file := filepath.Join(dest, adxMetricProjectionSchema.Name+"."+format)
		if err := writeADXProjectionFile(file, format, adxMetricProjectionSchema, metricRows, meta); err != nil {
			return ADXExportResult{}, err
		}
		metricTable.File = file
	}
	result.Tables = append(result.Tables, metricTable)

	if !opts.DryRun {
		schemaFile := filepath.Join(dest, "schema.kql")
		if err := os.WriteFile(schemaFile, []byte(ADXProjectionKQL()), 0o644); err != nil {
			return ADXExportResult{}, err
		}
		result.SchemaFile = schemaFile
		manifestFile := filepath.Join(dest, "projection_manifest.json")
		if err := fileutil.WriteJSONFileAtomic(manifestFile, result); err != nil {
			return ADXExportResult{}, err
		}
		result.ManifestFile = manifestFile
	}
	return result, nil
}

func (s *Store) ExportADXMetrics(ctx context.Context, opts ADXMetricsExportOptions) (ADXMetricsExportResult, error) {
	if err := ctx.Err(); err != nil {
		return ADXMetricsExportResult{}, err
	}
	format := opts.Format
	if format == "" {
		format = "jsonl"
	}
	if err := validateADXExportFormat(format); err != nil {
		return ADXMetricsExportResult{}, err
	}
	if strings.TrimSpace(opts.Out) == "" {
		return ADXMetricsExportResult{}, fmt.Errorf("--out is required")
	}
	dest, err := filepath.Abs(opts.Out)
	if err != nil {
		return ADXMetricsExportResult{}, err
	}
	dest = filepath.Clean(dest)
	if dest == s.Root {
		return ADXMetricsExportResult{}, fmt.Errorf("--out must differ from --store")
	}
	if isSubpath(s.Root, dest) {
		return ADXMetricsExportResult{}, fmt.Errorf("--out must not be inside --store")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return ADXMetricsExportResult{}, err
	}
	fileName := strings.TrimSpace(opts.FileName)
	if fileName == "" {
		fileName = adxMetricProjectionSchema.Name + "." + format
	}
	if filepath.Base(fileName) != fileName || fileName == "." || fileName == string(filepath.Separator) {
		return ADXMetricsExportResult{}, fmt.Errorf("metrics export file name must not contain path separators")
	}
	exportedAt := opts.ExportedAt
	if exportedAt.IsZero() {
		exportedAt = time.Now().UTC()
	} else {
		exportedAt = exportedAt.UTC()
	}
	sourceStoreID, err := s.adxSourceStoreID()
	if err != nil {
		return ADXMetricsExportResult{}, err
	}
	rows, err := s.adxMetricRowsForFiles(ctx, opts.MetricFileIDs)
	if err != nil {
		return ADXMetricsExportResult{}, err
	}
	result := ADXMetricsExportResult{
		SourceStoreID:       sourceStoreID,
		SourceStorePath:     s.Root,
		SourceSchemaVersion: s.manifest.SchemaVersion,
		ProjectionVersion:   ADXProjectionVersion,
		ExportedAt:          exportedAt.Format(time.RFC3339),
		Format:              format,
		Destination:         dest,
		MetricsFile:         filepath.Join(dest, fileName),
		Rows:                len(rows),
	}
	meta := map[string]any{
		"exported_at":           result.ExportedAt,
		"source_store_id":       result.SourceStoreID,
		"source_store_path":     result.SourceStorePath,
		"source_schema_version": result.SourceSchemaVersion,
		"projection_version":    result.ProjectionVersion,
	}
	if err := writeADXProjectionFile(result.MetricsFile, format, adxMetricProjectionSchema, rows, meta); err != nil {
		return ADXMetricsExportResult{}, err
	}
	return result, nil
}

func (s *Store) adxMetricRowsForFiles(ctx context.Context, metricFileIDs []string) ([]map[string]any, error) {
	args := []any{MetricSchemaVersion}
	filter := ""
	if len(metricFileIDs) > 0 {
		placeholders := make([]string, 0, len(metricFileIDs))
		for _, id := range metricFileIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		if len(placeholders) == 0 {
			return nil, nil
		}
		filter = " AND file_id IN (" + strings.Join(placeholders, ", ") + ")"
	}
	records, err := s.queryRows(ctx, `
SELECT file_id, path
FROM metric_files
WHERE format = 'parquet' AND schema_version = ?
`+filter+`
ORDER BY created_at, file_id`, args)
	if err != nil {
		return nil, fmt.Errorf("query metric files for scalar projection: %w", err)
	}
	experimentByRun, err := s.experimentIDsByRun(ctx)
	if err != nil {
		return nil, err
	}
	var projected []map[string]any
	for _, record := range records.Rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fileID, _ := record["file_id"].(string)
		relPath, _ := record["path"].(string)
		if fileID == "" || relPath == "" {
			return nil, fmt.Errorf("metric file record missing file_id or path: %+v", record)
		}
		path, err := s.metricFileProjectionPath(relPath)
		if err != nil {
			return nil, err
		}
		rows, err := parquet.ReadFile[MetricRow](path)
		if err != nil {
			return nil, fmt.Errorf("read metric parquet %s: %w", relPath, err)
		}
		for _, row := range rows {
			projected = append(projected, projectMetricRow(fileID, relPath, experimentByRun[row.RunID], row))
		}
	}
	return projected, nil
}

func (s *Store) metricFileProjectionPath(relPath string) (string, error) {
	if strings.TrimSpace(relPath) == "" {
		return "", fmt.Errorf("metric file path is required")
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." {
		return "", fmt.Errorf("metric file path %s must point to a file", relPath)
	}
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("metric file path %s must be relative to the store", relPath)
	}
	path := filepath.Join(s.Root, clean)
	if path != s.Root && !isSubpath(s.Root, path) {
		return "", fmt.Errorf("metric file path %s escapes the store", relPath)
	}
	return path, nil
}

// experimentIDsByRun maps run_id -> experiment_id so the metrics projection can
// carry the experiment axis without duplicating it into the Parquet schema.
func (s *Store) experimentIDsByRun(ctx context.Context) (map[string]string, error) {
	records, err := s.queryRows(ctx, `SELECT run_id, experiment_id FROM runs`, nil)
	if err != nil {
		return nil, fmt.Errorf("query run experiment links for scalar projection: %w", err)
	}
	out := make(map[string]string, len(records.Rows))
	for _, record := range records.Rows {
		runID, _ := record["run_id"].(string)
		experimentID, _ := record["experiment_id"].(string)
		if runID != "" && experimentID != "" {
			out[runID] = experimentID
		}
	}
	return out, nil
}

func projectMetricRow(fileID, relPath, experimentID string, row MetricRow) map[string]any {
	return map[string]any{
		"metric_file_id":   fileID,
		"metric_file_path": relPath,
		"project":          row.Project,
		"experiment_id":    experimentID,
		"run_group_id":     row.RunGroupID,
		"run_id":           row.RunID,
		"metric_name":      row.MetricName,
		"step":             int64PtrValue(row.Step),
		"wall_time":        metricWallTimeValue(row.WallTime),
		"value":            row.Value,
		"unit":             stringPtrValue(row.Unit),
		"source":           row.Source,
		"split":            stringPtrValue(row.Split),
		"tags":             row.Tags,
	}
}

func stringPtrValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func int64PtrValue(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func metricWallTimeValue(value *int64) any {
	if value == nil {
		return nil
	}
	return time.UnixMicro(*value).UTC().Format(time.RFC3339Nano)
}

func validateADXExportFormat(format string) error {
	switch format {
	case "jsonl", "csv":
		return nil
	default:
		return fmt.Errorf("ADX projection format must be one of: jsonl, csv")
	}
}

func (s *Store) prepareADXDestination(opts ADXExportOptions) (string, error) {
	if strings.TrimSpace(opts.Out) == "" {
		return "", fmt.Errorf("--out is required unless --dry-run is set")
	}
	dest, err := filepath.Abs(opts.Out)
	if err != nil {
		return "", err
	}
	dest = filepath.Clean(dest)
	if dest == s.Root {
		return "", fmt.Errorf("--out must differ from --store")
	}
	if isSubpath(s.Root, dest) {
		return "", fmt.Errorf("--out must not be inside --store")
	}
	if err := prepareExportDestination(dest, opts.Force); err != nil {
		return "", err
	}
	return dest, nil
}

func (s *Store) adxSourceStoreID() (string, error) {
	payload := struct {
		SchemaVersion string `json:"schema_version"`
		Kind          string `json:"kind"`
		Project       string `json:"project"`
		CreatedAt     string `json:"created_at"`
	}{
		SchemaVersion: s.manifest.SchemaVersion,
		Kind:          s.manifest.Kind,
		Project:       s.manifest.Project,
		CreatedAt:     s.manifest.CreatedAt,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "tau-exp-" + hex.EncodeToString(sum[:])[:16], nil
}

func writeADXProjectionFile(path, format string, schema ADXTableSchema, rows []map[string]any, meta map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := validateADXExportFormat(format); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	switch format {
	case "jsonl":
		if err := writeADXJSONL(tmpPath, schema, rows, meta); err != nil {
			return err
		}
	case "csv":
		if err := writeADXCSV(tmpPath, schema, rows, meta); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil && !fileutil.ChmodUnsupported(err) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func writeADXJSONL(path string, schema ADXTableSchema, rows []map[string]any, meta map[string]any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(projectADXRow(schema, row, meta)); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return f.Close()
}

func writeADXCSV(path string, schema ADXTableSchema, rows []map[string]any, meta map[string]any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(f)
	header := make([]string, 0, len(schema.Columns))
	for _, col := range schema.Columns {
		header = append(header, col.Name)
	}
	if err := cw.Write(header); err != nil {
		return errors.Join(err, f.Close())
	}
	for _, row := range rows {
		projected := projectADXRow(schema, row, meta)
		record := make([]string, 0, len(schema.Columns))
		for _, col := range schema.Columns {
			record = append(record, adxCSVCell(projected[col.Name]))
		}
		if err := cw.Write(record); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

func projectADXRow(schema ADXTableSchema, row map[string]any, meta map[string]any) map[string]any {
	projected := make(map[string]any, len(schema.Columns))
	for _, col := range schema.Columns {
		if value, ok := meta[col.Name]; ok {
			projected[col.Name] = value
			continue
		}
		projected[col.Name] = row[col.Name]
	}
	return projected
}

func adxCSVCell(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
