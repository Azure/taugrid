// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expimport

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

const JSONLImporterVersion = "tau.jsonl.import.v1"

var ErrNoJSONLScalarMetrics = errors.New("JSONL history files contain no scalar metrics")

type JSONLImportOptions struct {
	RunID          string
	Project        string
	ExperimentID   string
	RunGroupID     string
	Owner          string
	State          string
	History        []string
	MetricPrefix   string
	Source         string
	Tags           map[string]string
	StepField      string
	TimeField      string
	IdempotencyKey string
	SkipArtifacts  bool
	DryRun         bool
}

type JSONLImportResult struct {
	RunID          string                     `json:"run_id"`
	Project        string                     `json:"project"`
	ExperimentID   string                     `json:"experiment_id,omitempty"`
	RunGroupID     string                     `json:"run_group_id"`
	IdempotencyKey string                     `json:"idempotency_key"`
	RequestHash    string                     `json:"request_hash"`
	Reused         bool                       `json:"reused"`
	DryRun         bool                       `json:"dry_run"`
	HistoryFiles   []JSONLHistoryFile         `json:"history_files"`
	MetricFile     *expstore.MetricFileRecord `json:"metric_file,omitempty"`
	Artifacts      []expstore.ArtifactRecord  `json:"artifacts,omitempty"`
	Rows           int                        `json:"rows"`
	MinStep        *int64                     `json:"min_step,omitempty"`
	MaxStep        *int64                     `json:"max_step,omitempty"`
}

type JSONLHistoryFile struct {
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	ModTime    string `json:"mod_time"`
	SHA256     string `json:"sha256"`
	ScalarRows int    `json:"scalar_rows"`
}

type jsonlScalar struct {
	MetricName  string
	RawKey      string
	Step        *int64
	WallTime    *int64
	Value       float64
	HistoryFile string
	Line        int
}

func ImportJSONL(ctx context.Context, store *expstore.Store, opts JSONLImportOptions) (JSONLImportResult, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return JSONLImportResult{}, fmt.Errorf("--run is required")
	}
	historyPaths, err := expandJSONLHistoryInputs(opts.History)
	if err != nil {
		return JSONLImportResult{}, err
	}
	if len(historyPaths) == 0 {
		return JSONLImportResult{}, fmt.Errorf("at least one --history is required")
	}
	opts = defaultJSONLImportOptions(store.Manifest(), opts)

	var scalars []jsonlScalar
	historyFiles := make([]JSONLHistoryFile, 0, len(historyPaths))
	for _, path := range historyPaths {
		fileScalars, err := readJSONLScalars(path, opts)
		if err != nil {
			return JSONLImportResult{}, fmt.Errorf("read JSONL metrics from %s: %w", path, err)
		}
		info, err := jsonlHistoryFileIdentity(path)
		if err != nil {
			return JSONLImportResult{}, err
		}
		info.ScalarRows = len(fileScalars)
		historyFiles = append(historyFiles, info)
		scalars = append(scalars, fileScalars...)
	}
	if len(scalars) == 0 {
		return JSONLImportResult{}, ErrNoJSONLScalarMetrics
	}

	rows, minStep, maxStep, err := jsonlMetricRows(opts, scalars)
	if err != nil {
		return JSONLImportResult{}, err
	}
	requestHash, err := jsonlRequestHash(opts, historyFiles)
	if err != nil {
		return JSONLImportResult{}, err
	}
	if opts.IdempotencyKey == "" {
		opts.IdempotencyKey = "jsonl-" + opts.RunID + "-" + shortHash(requestHash)
	}

	result := JSONLImportResult{
		RunID:          opts.RunID,
		Project:        opts.Project,
		ExperimentID:   opts.ExperimentID,
		RunGroupID:     opts.RunGroupID,
		IdempotencyKey: opts.IdempotencyKey,
		RequestHash:    requestHash,
		DryRun:         opts.DryRun,
		HistoryFiles:   historyFiles,
		Rows:           len(rows),
		MinStep:        minStep,
		MaxStep:        maxStep,
	}
	if opts.DryRun {
		return result, nil
	}

	metricFile, err := writeJSONLMetricFile(store, opts, rows, requestHash, minStep, maxStep)
	if err != nil {
		return JSONLImportResult{}, err
	}
	writtenFiles := []string{filepath.Join(store.Root, filepath.FromSlash(metricFile.Path))}
	var artifacts []expstore.ArtifactRecord
	if !opts.SkipArtifacts {
		artifacts, err = copyJSONLArtifacts(store, opts.RunID, historyFiles)
		if err != nil {
			if cleanupErr := cleanupImportFiles(writtenFiles); cleanupErr != nil {
				return JSONLImportResult{}, fmt.Errorf("%w; cleanup JSONL import files: %v", err, cleanupErr)
			}
			return JSONLImportResult{}, err
		}
		for _, artifact := range artifacts {
			writtenFiles = append(writtenFiles, filepath.Join(store.Root, filepath.FromSlash(artifact.URI)))
		}
	}
	run := expstore.RunRecord{
		RunID:        opts.RunID,
		Project:      opts.Project,
		ExperimentID: opts.ExperimentID,
		RunGroupID:   opts.RunGroupID,
		State:        opts.State,
		Owner:        opts.Owner,
		ResultURI:    filepath.Dir(metricFile.Path),
	}
	if existing, ok, err := existingJSONLRun(ctx, store, opts.RunID); err != nil {
		return JSONLImportResult{}, err
	} else if ok {
		run = existing
		if run.ExperimentID == "" {
			run.ExperimentID = opts.ExperimentID
		}
	}
	record, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run:             run,
		Tags:            jsonlRunTags(opts),
		Artifacts:       artifacts,
		MetricFiles:     []expstore.MetricFileRecord{metricFile},
		MetricSummaries: expstore.SummarizeMetricRows(metricFile, rows),
		IdempotencyKey:  opts.IdempotencyKey,
		Command:         "exp import jsonl",
		RequestHash:     requestHash,
	})
	if err != nil {
		if cleanupErr := cleanupImportFiles(writtenFiles); cleanupErr != nil {
			return JSONLImportResult{}, fmt.Errorf("%w; cleanup JSONL import files: %v", err, cleanupErr)
		}
		return JSONLImportResult{}, err
	}
	result.Reused = record.Reused
	result.MetricFile = &metricFile
	result.Artifacts = artifacts
	return result, nil
}

func defaultJSONLImportOptions(manifest expstore.Manifest, opts JSONLImportOptions) JSONLImportOptions {
	opts.Project = cmp.Or(opts.Project, manifest.Project, "default")
	opts.RunGroupID = cmp.Or(opts.RunGroupID, "default")
	opts.State = cmp.Or(opts.State, "succeeded")
	opts.Owner = cmp.Or(opts.Owner, "jsonl-import")
	opts.Source = cmp.Or(opts.Source, "jsonl")
	opts.StepField = cmp.Or(opts.StepField, "_step")
	opts.TimeField = cmp.Or(opts.TimeField, "_timestamp")
	return opts
}

func existingJSONLRun(ctx context.Context, store *expstore.Store, runID string) (expstore.RunRecord, bool, error) {
	result, err := store.QueryArgs(ctx, `
SELECT run_id, project, experiment_id, run_group_id, state, owner,
       created_at, started_at, completed_at, config_hash, code_sha, image_digest,
       tau_command, result_uri, index_version
FROM runs WHERE run_id = ? LIMIT 1`, runID)
	if err != nil {
		return expstore.RunRecord{}, false, err
	}
	if len(result.Rows) == 0 {
		return expstore.RunRecord{}, false, nil
	}
	row := result.Rows[0]
	return expstore.RunRecord{
		RunID:        stringField(row, "run_id"),
		Project:      stringField(row, "project"),
		ExperimentID: stringField(row, "experiment_id"),
		RunGroupID:   stringField(row, "run_group_id"),
		State:        stringField(row, "state"),
		Owner:        stringField(row, "owner"),
		CreatedAt:    stringField(row, "created_at"),
		StartedAt:    stringField(row, "started_at"),
		CompletedAt:  stringField(row, "completed_at"),
		ConfigHash:   stringField(row, "config_hash"),
		CodeSHA:      stringField(row, "code_sha"),
		ImageDigest:  stringField(row, "image_digest"),
		TauCommand:   stringField(row, "tau_command"),
		ResultURI:    stringField(row, "result_uri"),
		IndexVersion: stringField(row, "index_version"),
	}, true, nil
}

func stringField(row map[string]any, key string) string {
	if value, ok := row[key]; ok && value != nil {
		return fmt.Sprint(value)
	}
	return ""
}

func expandJSONLHistoryInputs(patterns []string) ([]string, error) {
	paths, err := expandPathPatterns(patterns, "--history")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	sort.Strings(out)
	return out, nil
}

func readJSONLScalars(path string, opts JSONLImportOptions) ([]jsonlScalar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var scalars []jsonlScalar
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var payload map[string]any
		if err := dec.Decode(&payload); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		step := jsonlStep(payload, opts.StepField)
		wallTime := jsonlWallTime(payload, opts.TimeField)
		for key, value := range payload {
			if skipJSONLMetricKey(key, opts.StepField, opts.TimeField) {
				continue
			}
			numeric, ok := jsonNumber(value)
			if !ok {
				continue
			}
			metricName := strings.TrimSpace(key)
			if metricName == "" {
				continue
			}
			scalars = append(scalars, jsonlScalar{
				MetricName:  metricName,
				RawKey:      key,
				Step:        cloneInt64(step),
				WallTime:    cloneInt64(wallTime),
				Value:       numeric,
				HistoryFile: path,
				Line:        line,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return scalars, nil
}

func jsonlStep(payload map[string]any, field string) *int64 {
	if number, ok := payload[field].(json.Number); ok {
		if exact, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
			return &exact
		}
	}
	value, ok := jsonNumber(payload[field])
	if !ok {
		return nil
	}
	step := int64(value)
	return &step
}

func jsonlWallTime(payload map[string]any, field string) *int64 {
	value, ok := jsonNumber(payload[field])
	if !ok {
		return nil
	}
	micros := int64(value * 1_000_000)
	return &micros
}

func skipJSONLMetricKey(key, stepField, timeField string) bool {
	return key == stepField || key == timeField || strings.HasPrefix(key, "_")
}

func jsonlMetricRows(opts JSONLImportOptions, scalars []jsonlScalar) ([]expstore.MetricRow, *int64, *int64, error) {
	rows := make([]expstore.MetricRow, 0, len(scalars))
	var minStep, maxStep *int64
	for _, scalar := range scalars {
		metricName := scalar.MetricName
		if opts.MetricPrefix != "" {
			metricName = strings.TrimSuffix(opts.MetricPrefix, "/") + "/" + metricName
		}
		if scalar.Step != nil {
			step := *scalar.Step
			if minStep == nil || step < *minStep {
				v := step
				minStep = &v
			}
			if maxStep == nil || step > *maxStep {
				v := step
				maxStep = &v
			}
		}
		tags, err := jsonlMetricTagsJSON(metricName, scalar, opts.Tags)
		if err != nil {
			return nil, nil, nil, err
		}
		rows = append(rows, expstore.MetricRow{
			Project:    opts.Project,
			RunGroupID: opts.RunGroupID,
			RunID:      opts.RunID,
			MetricName: metricName,
			Step:       cloneInt64(scalar.Step),
			WallTime:   cloneInt64(scalar.WallTime),
			Value:      scalar.Value,
			Source:     opts.Source,
			Tags:       tags,
		})
	}
	return rows, minStep, maxStep, nil
}

func jsonlMetricTagsJSON(metricName string, scalar jsonlScalar, userTags map[string]string) (string, error) {
	tags := compactTagMap(userTags)
	for key, value := range map[string]string{
		"jsonl.raw_key":       scalar.RawKey,
		"jsonl.history_file":  filepath.Base(scalar.HistoryFile),
		"jsonl.history_path":  scalar.HistoryFile,
		"jsonl.history_line":  fmt.Sprint(scalar.Line),
		"jsonl.importer":      JSONLImporterVersion,
		"tau.metric.card":     ResearchMetricCard(metricName),
		"tau.metric.standard": fmt.Sprint(IsStandardResearchMetric(metricName)),
	} {
		tags[key] = value
	}
	raw, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func jsonlRunTags(opts JSONLImportOptions) []expstore.TagRecord {
	tags := []expstore.TagRecord{
		{ScopeType: "run", ScopeID: opts.RunID, Key: "source", Value: opts.Source},
		{ScopeType: "run", ScopeID: opts.RunID, Key: "jsonl.importer", Value: JSONLImporterVersion},
	}
	userTags := compactTagMap(opts.Tags)
	keys := make([]string, 0, len(userTags))
	for key := range userTags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tags = append(tags, expstore.TagRecord{ScopeType: "run", ScopeID: opts.RunID, Key: key, Value: userTags[key]})
	}
	return tags
}

func compactTagMap(tags map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range tags {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}

func writeJSONLMetricFile(store *expstore.Store, opts JSONLImportOptions, rows []expstore.MetricRow, requestHash string, minStep, maxStep *int64) (expstore.MetricFileRecord, error) {
	rel := filepath.Join(
		expstore.MetricsDir,
		"project="+opts.Project,
		"experiment="+emptyPartition(opts.ExperimentID),
		"group="+opts.RunGroupID,
		"run="+opts.RunID,
		"jsonl-"+shortHash(requestHash)+".parquet",
	)
	path := filepath.Join(store.Root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return expstore.MetricFileRecord{}, err
	}
	if err := parquet.WriteFile(path, rows); err != nil {
		return expstore.MetricFileRecord{}, fmt.Errorf("write JSONL metric parquet: %w", err)
	}
	digest, _, err := fileDigest(path)
	if err != nil {
		return expstore.MetricFileRecord{}, err
	}
	return expstore.MetricFileRecord{
		FileID:        "jsonl-" + opts.RunID + "-" + shortHash(requestHash),
		Path:          filepath.ToSlash(rel),
		Format:        "parquet",
		SchemaVersion: expstore.MetricSchemaVersion,
		SchemaHash:    metricSchemaHash(),
		Project:       opts.Project,
		RunGroupID:    opts.RunGroupID,
		RunID:         opts.RunID,
		RowCount:      int64(len(rows)),
		Digest:        digest,
		MinStep:       minStep,
		MaxStep:       maxStep,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func copyJSONLArtifacts(store *expstore.Store, runID string, historyFiles []JSONLHistoryFile) (records []expstore.ArtifactRecord, err error) {
	records = make([]expstore.ArtifactRecord, 0, len(historyFiles))
	var copiedFiles []string
	defer func() {
		if err != nil {
			if cleanupErr := cleanupImportFiles(copiedFiles); cleanupErr != nil {
				err = fmt.Errorf("%w; cleanup JSONL artifacts: %v", err, cleanupErr)
			}
		}
	}()
	for _, historyFile := range historyFiles {
		destRel := filepath.Join(expstore.ArtifactsDir, runID, "jsonl", shortHash(historyFile.SHA256)+"-"+filepath.Base(historyFile.Path))
		dest := filepath.Join(store.Root, destRel)
		if err := copyFile(historyFile.Path, dest); err != nil {
			return nil, err
		}
		copiedFiles = append(copiedFiles, dest)
		size := historyFile.SizeBytes
		preview, err := json.Marshal(map[string]string{
			"kind":          "jsonl_history",
			"original_path": historyFile.Path,
		})
		if err != nil {
			return nil, err
		}
		records = append(records, expstore.ArtifactRecord{
			ArtifactID: "jsonl-history-" + runID + "-" + shortHash(historyFile.SHA256),
			RunID:      runID,
			Type:       "metrics-jsonl",
			URI:        filepath.ToSlash(destRel),
			Name:       filepath.Base(historyFile.Path),
			Digest:     historyFile.SHA256,
			SizeBytes:  &size,
			CreatedAt:  historyFile.ModTime,
			Preview:    string(preview),
		})
	}
	return records, nil
}

func jsonlHistoryFileIdentity(path string) (JSONLHistoryFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return JSONLHistoryFile{}, err
	}
	if info.IsDir() {
		return JSONLHistoryFile{}, fmt.Errorf("JSONL history path %s is a directory", path)
	}
	digest, _, err := fileDigest(path)
	if err != nil {
		return JSONLHistoryFile{}, err
	}
	return JSONLHistoryFile{
		Path:      path,
		SizeBytes: info.Size(),
		ModTime:   info.ModTime().UTC().Format(time.RFC3339),
		SHA256:    digest,
	}, nil
}

func jsonlRequestHash(opts JSONLImportOptions, historyFiles []JSONLHistoryFile) (string, error) {
	payload := map[string]any{
		"importer":      JSONLImporterVersion,
		"metric_schema": expstore.MetricSchemaVersion,
		"run_id":        opts.RunID,
		"project":       opts.Project,
		"experiment_id": opts.ExperimentID,
		"run_group_id":  opts.RunGroupID,
		"owner":         opts.Owner,
		"state":         opts.State,
		"metric_prefix": opts.MetricPrefix,
		"source":        opts.Source,
		"tags":          compactTagMap(opts.Tags),
		"step_field":    opts.StepField,
		"time_field":    opts.TimeField,
		"history_files": jsonlRequestHistoryFiles(historyFiles),
	}
	if opts.SkipArtifacts {
		payload["skip_artifacts"] = true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type jsonlRequestHistoryFile struct {
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	ScalarRows int    `json:"scalar_rows"`
}

func jsonlRequestHistoryFiles(historyFiles []JSONLHistoryFile) []jsonlRequestHistoryFile {
	out := make([]jsonlRequestHistoryFile, 0, len(historyFiles))
	for _, historyFile := range historyFiles {
		out = append(out, jsonlRequestHistoryFile{
			SizeBytes:  historyFile.SizeBytes,
			SHA256:     historyFile.SHA256,
			ScalarRows: historyFile.ScalarRows,
		})
	}
	return out
}
