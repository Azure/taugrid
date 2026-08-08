// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func TestExportADXProjectionRowsAndSchemas(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := Init(ctx, root, InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce Candidate training sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	recordADXFixture(t, ctx, store)

	exportedAt := time.Date(2026, 5, 19, 23, 0, 0, 0, time.UTC)
	dest := filepath.Join(t.TempDir(), "adx")
	result, err := store.ExportADX(ctx, ADXExportOptions{Out: dest, Format: "jsonl", ExportedAt: exportedAt})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectionVersion != ADXProjectionVersion || result.ExportedAt != exportedAt.Format(time.RFC3339) {
		t.Fatalf("unexpected export metadata: %+v", result)
	}
	if !strings.HasPrefix(result.SourceStoreID, "tau-exp-") || result.SourceStorePath != root || result.SourceSchemaVersion != SchemaVersion {
		t.Fatalf("unexpected source metadata: %+v", result)
	}
	if _, err := os.Stat(result.SchemaFile); err != nil {
		t.Fatalf("expected schema file: %v", err)
	}
	if _, err := os.Stat(result.ManifestFile); err != nil {
		t.Fatalf("expected projection manifest: %v", err)
	}

	counts := map[string]int{}
	for _, table := range result.Tables {
		counts[table.Name] = table.Rows
		if len(table.Columns) == 0 || table.Columns[0].Name != "exported_at" {
			t.Fatalf("table %s missing metadata columns: %+v", table.Name, table.Columns)
		}
	}
	for table, want := range map[string]int{
		"TauExpRuns":         1,
		"TauExpRunContext":   1,
		"TauExpMetricFiles":  1,
		"TauExpMetrics":      2,
		"TauExpArtifacts":    1,
		"TauExpEvents":       1,
		"TauExpObservations": 1,
		"TauExpConfigs":      1,
	} {
		if counts[table] != want {
			t.Fatalf("%s rows=%d, want %d; counts=%+v", table, counts[table], want, counts)
		}
	}

	runs := readJSONLRows(t, filepath.Join(dest, "TauExpRuns.jsonl"))
	if len(runs) != 1 {
		t.Fatalf("runs rows=%d", len(runs))
	}
	if runs[0]["run_id"] != "seed-1" || runs[0]["project"] != "project-alpha" || runs[0]["source_store_id"] != result.SourceStoreID {
		t.Fatalf("unexpected run projection row: %+v", runs[0])
	}
	if runs[0]["exported_at"] != exportedAt.Format(time.RFC3339) || runs[0]["projection_version"] != ADXProjectionVersion {
		t.Fatalf("missing projection metadata: %+v", runs[0])
	}

	contextRows := readJSONLRows(t, filepath.Join(dest, "TauExpRunContext.jsonl"))
	if contextRows[0]["cluster"] != "kind-taugrid" || contextRows[0]["gpu_count"] != float64(8) {
		t.Fatalf("unexpected run_context projection row: %+v", contextRows[0])
	}
	eventRows := readJSONLRows(t, filepath.Join(dest, "TauExpEvents.jsonl"))
	if eventRows[0]["event_id"] != "event-seed-1-started" || eventRows[0]["payload"] != `{"pod":"seed-1"}` {
		t.Fatalf("unexpected event projection row: %+v", eventRows[0])
	}
	configRows := readJSONLRows(t, filepath.Join(dest, "TauExpConfigs.jsonl"))
	if configRows[0]["config_hash"] != "cfg-seed-1" || configRows[0]["normalized_json"] != `{"lr":0.001}` {
		t.Fatalf("unexpected config projection row: %+v", configRows[0])
	}
	metricRows := readJSONLRows(t, filepath.Join(dest, "TauExpMetrics.jsonl"))
	if len(metricRows) != 2 {
		t.Fatalf("metric rows=%d", len(metricRows))
	}
	if metricRows[0]["metric_file_id"] != "tb-metrics-seed-1" || metricRows[0]["metric_name"] != "train/return" || metricRows[0]["value"] != float64(42) {
		t.Fatalf("unexpected metric projection row: %+v", metricRows[0])
	}
	if metricRows[0]["wall_time"] != "2026-05-16T00:02:00Z" {
		t.Fatalf("unexpected metric wall_time projection: %+v", metricRows[0])
	}

	kql := ADXProjectionKQL()
	for _, want := range []string{
		".create-merge table TauExpRuns",
		".create-merge table TauExpMetrics",
		"exported_at: datetime",
		"source_store_id: string",
		"['project']: string",
		"['time']: datetime",
		"['type']: string",
		".create-merge table TauExpObservations",
		"analytics mirrors only",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("KQL schema missing %q:\n%s", want, kql)
		}
	}
}

func TestExportADXMetricsReadsIndexedLegacyAndCurrentPartitions(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:    "mixed-experiment",
		Project: "mixed-project",
		Group:   "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fixtures := []struct {
		runID string
		path  string
	}{
		{
			runID: "legacy-run",
			path:  "metrics/project=mixed-project/question=mixed-experiment/group=baseline/run=legacy-run/part.parquet",
		},
		{
			runID: "current-run",
			path:  "metrics/project=mixed-project/experiment=mixed-experiment/group=baseline/run=current-run/part.parquet",
		},
	}
	for i, fixture := range fixtures {
		step := int64(i + 1)
		writeADXMetricParquet(t, store.Root, fixture.path, []MetricRow{{
			Project:    "mixed-project",
			RunGroupID: "baseline",
			RunID:      fixture.runID,
			MetricName: "train/loss",
			Step:       &step,
			Value:      float64(i + 1),
			Source:     "jsonl",
		}})
		if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
			Run: RunRecord{
				RunID:        fixture.runID,
				Project:      "mixed-project",
				ExperimentID: "mixed-experiment",
				RunGroupID:   "baseline",
				State:        "succeeded",
			},
			MetricFiles: []MetricFileRecord{{
				FileID:        "metrics-" + fixture.runID,
				Path:          fixture.path,
				Format:        "parquet",
				SchemaVersion: MetricSchemaVersion,
				Project:       "mixed-project",
				RunGroupID:    "baseline",
				RunID:         fixture.runID,
				RowCount:      1,
				MinStep:       &step,
				MaxStep:       &step,
				CreatedAt:     "2026-05-16T00:00:00Z",
			}},
			IdempotencyKey: "record-" + fixture.runID,
			Command:        "test fixture",
			RequestHash:    "hash-" + fixture.runID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := store.ExportADXMetrics(ctx, ADXMetricsExportOptions{
		Out:    filepath.Join(t.TempDir(), "export"),
		Format: "jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result.MetricsFile)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows != 2 ||
		!strings.Contains(string(raw), `"run_id":"legacy-run"`) ||
		!strings.Contains(string(raw), `"run_id":"current-run"`) {
		t.Fatalf("mixed partition export did not read both indexed files: result=%+v\n%s", result, raw)
	}
}

func TestExportADXCSVAndDryRun(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := Init(ctx, root, InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce Candidate training sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recordADXFixture(t, ctx, store)

	dest := filepath.Join(t.TempDir(), "adx-csv")
	result, err := store.ExportADX(ctx, ADXExportOptions{Out: dest, Format: "csv"})
	if err != nil {
		t.Fatal(err)
	}
	headerRaw, err := os.ReadFile(filepath.Join(dest, "TauExpRuns.csv"))
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(string(headerRaw), "\n", 2)[0]
	if !strings.HasPrefix(header, "exported_at,source_store_id,source_store_path,source_schema_version,projection_version,run_id") {
		t.Fatalf("unexpected CSV header: %s", header)
	}
	if result.Tables[0].File == "" {
		t.Fatalf("expected table file paths: %+v", result.Tables[0])
	}

	dryRun, err := store.ExportADX(ctx, ADXExportOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.Mode != "dry-run" || dryRun.Destination != "" || dryRun.Format != "jsonl" {
		t.Fatalf("unexpected dry-run result: %+v", dryRun)
	}
	for _, table := range dryRun.Tables {
		if table.File != "" {
			t.Fatalf("dry-run should not write table files: %+v", table)
		}
	}
}

func TestExpADXDocsDescribeProjectionOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"ADX/Kusto is a downstream analytics projection only",
		"not the source of truth",
		"expstore remains authoritative",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("README missing projection-only contract %q", want)
		}
	}
}

func recordADXFixture(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	size := int64(128)
	minStep := int64(1)
	maxStep := int64(2)
	gpuCount := int64(8)
	queueWait := 120.0
	gpuHours := 4.0
	estimatedCost := 3.49
	metricRel := "metrics/project=project-alpha/experiment=experiment-alpha/group=reference-group/run=seed-1/part.parquet"
	wallTime := time.Date(2026, 5, 16, 0, 2, 0, 0, time.UTC).UnixMicro()
	writeADXMetricParquet(t, store.Root, metricRel, []MetricRow{
		{
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			RunID:      "seed-1",
			MetricName: "train/return",
			Step:       int64Ptr(1),
			WallTime:   &wallTime,
			Value:      42,
			Source:     "tensorboard",
			Tags:       `{"tensorboard.raw_tag":"train/return"}`,
		},
		{
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			RunID:      "seed-1",
			MetricName: "eval/score",
			Step:       int64Ptr(2),
			WallTime:   &wallTime,
			Value:      7,
			Source:     "tensorboard",
			Tags:       `{"tensorboard.raw_tag":"eval/score"}`,
		},
	})
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: RunRecord{
			RunID:        "seed-1",
			Project:      "project-alpha",
			ExperimentID: "experiment-alpha",
			RunGroupID:   "reference-group",
			State:        "succeeded",
			Owner:        "agent",
			CreatedAt:    "2026-05-16T00:00:00Z",
			StartedAt:    "2026-05-16T00:01:00Z",
			CompletedAt:  "2026-05-16T01:00:00Z",
			ConfigHash:   "cfg-seed-1",
			CodeSHA:      "abc123",
			ImageDigest:  "sha256:image",
			TauCommand:   "tau run train-001 --config tau.yaml",
			ResultURI:    "artifacts/seed-1",
		},
		RunContext: &RunContextRecord{
			RunID:            "seed-1",
			Cluster:          "kind-taugrid",
			Namespace:        "ray",
			Team:             "research",
			Profile:          "research-train-gpu",
			Lane:             "training",
			LocalQueue:       "training-queue",
			ClusterQueue:     "taugrid-training",
			KueueWorkload:    "seed-1-workload",
			PodUID:           "pod-uid",
			RayJob:           "ray-seed-1",
			ResourceClaims:   `[{"name":"claim-a"}]`,
			GPUClass:         "a100-80gb",
			GPUCount:         &gpuCount,
			NodeNames:        `["node-a"]`,
			Mounts:           `[{"source":"pvc","path":"/data"}]`,
			QueueWaitSeconds: &queueWait,
			GPUHours:         &gpuHours,
			EstimatedCost:    &estimatedCost,
		},
		Artifacts: []ArtifactRecord{{
			ArtifactID:  "tb-event-seed-1",
			RunID:       "seed-1",
			Type:        "tensorboard",
			URI:         "artifacts/seed-1/events.out.tfevents.test",
			Name:        "events.out.tfevents.test",
			Digest:      "sha256:test",
			SizeBytes:   &size,
			CreatedAt:   "2026-05-16T00:00:00Z",
			Preview:     `{"kind":"scalar-summary"}`,
			ExternalRef: `{"external_run":"seed-1"}`,
		}},
		MetricFiles: []MetricFileRecord{{
			FileID:        "tb-metrics-seed-1",
			Path:          metricRel,
			Format:        "parquet",
			SchemaVersion: MetricSchemaVersion,
			SchemaHash:    "schema-hash",
			Project:       "project-alpha",
			RunGroupID:    "reference-group",
			RunID:         "seed-1",
			RowCount:      2,
			Digest:        "sha256:metrics",
			MinStep:       &minStep,
			MaxStep:       &maxStep,
			CreatedAt:     "2026-05-16T00:00:01Z",
		}},
		IdempotencyKey: "tb-seed-1",
		Command:        "exp import jsonl",
		RequestHash:    "hash-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO events(event_id, run_id, time, type, source, severity, message, payload)
VALUES ('event-seed-1-started', 'seed-1', '2026-05-16T00:01:00Z', 'started', 'tau', 'info', 'pod started', '{"pod":"seed-1"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO configs(config_hash, run_id, format, uri, normalized_json, indexed_fields)
VALUES ('cfg-seed-1', 'seed-1', 'json', 'artifacts/seed-1/config.json', '{"lr":0.001}', '{"optimizer":"adam"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordObservation(ctx, RecordObservationOptions{
		Observation: ObservationRecord{
			ObservationID:  "obs-seed-1",
			IdempotencyKey: "obs-seed-1",
			Author:         "researcher",
			Source:         "human",
			Type:           "decision",
			ScopeType:      "run",
			ScopeID:        "seed-1",
			Text:           "Keep the run.",
			Evidence:       `{"metric":"train/return"}`,
			CreatedAt:      "2026-05-16T01:05:00Z",
		},
		IdempotencyKey: "obs-seed-1",
		Command:        "exp observe",
		RequestHash:    "obs-hash-1",
	}); err != nil {
		t.Fatal(err)
	}
}

func readJSONLRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		row := map[string]any{}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func writeADXMetricParquet(t *testing.T, root, rel string, rows []MetricRow) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(path, rows); err != nil {
		t.Fatal(err)
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}
