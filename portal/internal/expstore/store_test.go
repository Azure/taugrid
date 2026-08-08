// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestInitCreatesStoreAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project-alpha")
	opts := InitOptions{
		Name:           "experiment-alpha",
		Project:        "project-alpha",
		Description:    "Can we reproduce Candidate training sample benchmark on A100?",
		Group:          "reference-group",
		IdempotencyKey: "init-project-alpha",
	}

	store, result, err := Init(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Reused {
		t.Fatalf("first init created=%v reused=%v", result.Created, result.Reused)
	}
	if result.RunGroup.RunGroupID != "reference-group" {
		t.Fatalf("unexpected init result: %+v", result)
	}
	for _, want := range []string{ManifestFile, IndexFile, AppendLogDir, MetricsDir, ArtifactsDir} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Fatalf("expected %s: %v", want, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, result, err = Init(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if result.Created || !result.Reused {
		t.Fatalf("second init created=%v reused=%v", result.Created, result.Reused)
	}
	raw, err := os.ReadFile(filepath.Join(root, AppendLogDir, "experiments.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "\n"); got != 1 {
		t.Fatalf("experiments append log has %d rows, want 1:\n%s", got, raw)
	}

	list, err := store.List(ctx, ListOptions{Kind: "groups"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Rows) != 1 || list.Rows[0]["run_group_id"] != "reference-group" {
		t.Fatalf("groups=%+v", list.Rows)
	}
}

func TestOpenMigratesOldStoreColumns(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "old-store")
	manifest, err := ensureStoreFiles(root, InitOptions{Name: "old-question", Project: "old", Description: "old", Group: "baseline"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE runs (
  run_id TEXT PRIMARY KEY, project TEXT NOT NULL, question_id TEXT, hypothesis_id TEXT,
  run_group_id TEXT NOT NULL, parent_run_id TEXT, state TEXT NOT NULL, owner TEXT,
  created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT, config_hash TEXT,
  code_sha TEXT, image_digest TEXT, tau_command TEXT, result_uri TEXT, index_version TEXT NOT NULL
);
CREATE TABLE artifacts (
  artifact_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, type TEXT NOT NULL, uri TEXT NOT NULL,
  name TEXT NOT NULL, digest TEXT, size_bytes INTEGER, created_at TEXT NOT NULL, preview TEXT, external_ref TEXT
);
CREATE TABLE configs (
  config_hash TEXT NOT NULL, run_id TEXT NOT NULL, format TEXT NOT NULL, uri TEXT NOT NULL,
  normalized_json TEXT, PRIMARY KEY (config_hash, run_id)
);
CREATE TABLE run_context (
  run_id TEXT PRIMARY KEY, cluster TEXT, namespace TEXT, team TEXT, profile TEXT, lane TEXT,
  local_queue TEXT, cluster_queue TEXT, kueue_workload TEXT, pod_uid TEXT, ray_job TEXT,
  resource_claims TEXT, gpu_class TEXT, gpu_count INTEGER, node_names TEXT, mounts TEXT,
  queue_wait_seconds REAL, gpu_hours REAL, estimated_cost REAL
);
INSERT INTO runs(run_id, project, question_id, run_group_id, state, created_at, index_version)
VALUES ('old-run', 'old', 'old-question', 'baseline', 'succeeded', '2026-01-01T00:00:00Z', 'expstore.v0');
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for table, columns := range map[string][]string{
		"artifacts":   {"caption", "direction", "alias", "source_artifact_id", "source_run_id", "source_dataset_name", "source_dataset_version", "source_dataset_digest"},
		"configs":     {"indexed_fields"},
		"run_context": {"runtime", "dependencies", "log_uri"},
	} {
		for _, column := range columns {
			if !sqliteColumnExists(t, store, table, column) {
				t.Fatalf("expected migrated column %s.%s", table, column)
			}
		}
	}
	rows, err := store.Query(ctx, "select run_id from runs where run_id = 'old-run'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("old run was not preserved: %+v", rows.Rows)
	}
}

func sqliteColumnExists(t *testing.T, store *Store, table, column string) bool {
	t.Helper()
	rows, err := store.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

func TestConcurrentInitUsesStoreWriterLock(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project-alpha")
	opts := InitOptions{
		Name:           "experiment-alpha",
		Project:        "project-alpha",
		Description:    "Can we reproduce Candidate training sample benchmark on A100?",
		Group:          "reference-group",
		IdempotencyKey: "init-project-alpha",
	}
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, _, err := Init(ctx, root, opts)
			if store != nil {
				_ = store.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent init failed: %v", err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, AppendLogDir, "experiments.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "\n"); got != 1 {
		t.Fatalf("experiments append log has %d rows, want 1:\n%s", got, raw)
	}
}

func TestInitRejectsIdempotencyConflict(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project-alpha")
	opts := InitOptions{
		Name:           "experiment-alpha",
		Project:        "project-alpha",
		Description:    "Can we reproduce Candidate training sample benchmark on A100?",
		Group:          "reference-group",
		IdempotencyKey: "init-project-alpha",
	}
	store, _, err := Init(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	opts.Description = "A different description"
	store, _, err = Init(ctx, root, opts)
	if store != nil {
		store.Close()
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestInitRollsBackSQLiteWhenJSONLMirrorFails(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project-alpha")
	if err := os.MkdirAll(filepath.Join(root, AppendLogDir, "experiments.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := InitOptions{
		Name:           "experiment-alpha",
		Project:        "project-alpha",
		Description:    "Can we reproduce Candidate training sample benchmark on A100?",
		Group:          "reference-group",
		IdempotencyKey: "init-project-alpha",
	}

	store, _, err := Init(ctx, root, opts)
	if store != nil {
		store.Close()
	}
	if err == nil {
		t.Fatal("expected JSONL mirror failure")
	}
	if !strings.Contains(err.Error(), "experiments.jsonl") {
		t.Fatalf("error should mention failed mirror, got %v", err)
	}

	if err := os.RemoveAll(filepath.Join(root, AppendLogDir, "experiments.jsonl")); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, query := range []string{
		"select count(*) as count from experiments",
		"select count(*) as count from run_groups",
		"select count(*) as count from idempotency_keys",
	} {
		result, err := store.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(result.Rows[0]["count"]); got != "0" {
			t.Fatalf("%s = %s, want 0", query, got)
		}
	}
}

func TestQueryIsReadOnly(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce Candidate training sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	result, err := store.Query(ctx, "select experiment_id, project from experiments")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["project"] != "project-alpha" {
		t.Fatalf("query result=%+v", result)
	}
	if _, err := store.Query(ctx, "delete from experiments"); err == nil {
		t.Fatal("expected mutating query to fail")
	}
}

func TestListAndStatusIncludeRuns(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce Candidate training sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO runs(run_id, project, run_group_id, state, owner, created_at, completed_at, result_uri, index_version)
VALUES ('seed-1', 'project-alpha', 'reference-group', 'succeeded', 'agent', '2026-05-16T00:00:00Z', '2026-05-16T01:00:00Z', '/data/runs/seed-1', ?)`, SchemaVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO tags(scope_type, scope_id, key, value)
VALUES ('run', 'seed-1', 'seed', '1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO metric_files(file_id, path, format, schema_version, project, run_group_id, run_id, row_count, created_at)
VALUES ('metrics-seed-1', 'metrics/project=project-alpha/run_group=reference-group/run=seed-1/part-000.parquet', 'parquet', ?, 'project-alpha', 'reference-group', 'seed-1', 10, '2026-05-16T01:00:00Z')`, MetricSchemaVersion); err != nil {
		t.Fatal(err)
	}

	list, err := store.List(ctx, ListOptions{
		Kind:       "runs",
		RunGroupID: "reference-group",
		State:      "succeeded",
		Tags:       map[string]string{"seed": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Rows) != 1 || list.Rows[0]["run_id"] != "seed-1" {
		t.Fatalf("runs=%+v", list.Rows)
	}

	status, err := store.Status(ctx, "reference-group")
	if err != nil {
		t.Fatal(err)
	}
	if status.Runs != 1 || status.MetricFiles != 1 || status.StateCounts["succeeded"] != 1 {
		t.Fatalf("status=%+v", status)
	}
}

func TestSearchRunsClassifiesLifecycleAndMetricFilters(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "vision-vitenc-pretrain-dino-sweep",
		Project:     "vision",
		Description: "Does DINO pretraining stay healthy?",
		Group:       "arm-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	recordSearchRun := func(run RunRecord, rows []MetricRow, tags []TagRecord) {
		t.Helper()
		opts := RecordRunDataOptions{Run: run, Tags: tags}
		if len(rows) > 0 {
			minStep := int64(0)
			maxStep := int64(0)
			for i, row := range rows {
				if row.Step == nil {
					continue
				}
				if i == 0 || *row.Step < minStep {
					minStep = *row.Step
				}
				if i == 0 || *row.Step > maxStep {
					maxStep = *row.Step
				}
			}
			metricFile := MetricFileRecord{
				FileID:        "metrics-" + run.RunID,
				Path:          "metrics/" + run.RunID + ".parquet",
				Format:        "parquet",
				SchemaVersion: MetricSchemaVersion,
				Project:       run.Project,
				RunGroupID:    run.RunGroupID,
				RunID:         run.RunID,
				RowCount:      int64(len(rows)),
				MinStep:       &minStep,
				MaxStep:       &maxStep,
				CreatedAt:     run.CreatedAt,
			}
			opts.MetricFiles = []MetricFileRecord{metricFile}
			opts.MetricSummaries = SummarizeMetricRows(metricFile, rows)
		}
		if _, err := store.RecordRunData(ctx, opts); err != nil {
			t.Fatal(err)
		}
	}
	question := "vision-vitenc-pretrain-dino-sweep"
	runMetric := func(runID, metric string, step int64, value float64) MetricRow {
		return MetricRow{Project: "vision", RunGroupID: "arm-a", RunID: runID, MetricName: metric, Step: &step, Value: value, Source: "jsonl", Tags: "{}"}
	}
	baseRun := func(runID, state string) RunRecord {
		return RunRecord{RunID: runID, Project: "vision", RunGroupID: "arm-a", State: state, CreatedAt: "2026-06-10T17:00:00Z"}
	}
	recordSearchRun(baseRun("queued", "pending"), nil, nil)
	recordSearchRun(baseRun("active", "running"), nil, nil)
	recordSearchRun(baseRun("crashed", "failed"), nil, nil)
	recordSearchRun(baseRun("healthy", "succeeded"), []MetricRow{
		runMetric("healthy", "pretrain/loss_minus_random_baseline", 100, -0.2),
		runMetric("healthy", "pretrain/collapse_bad_steps", 100, 0),
	}, []TagRecord{{ScopeType: "run", ScopeID: "healthy", Key: "sweep", Value: "dino"}})
	recordSearchRun(baseRun("collapsed", "succeeded"), []MetricRow{
		runMetric("collapsed", "pretrain/loss_minus_random_baseline", 100, -0.1),
		runMetric("collapsed", "pretrain/collapse_bad_steps", 100, 2),
	}, nil)
	recordSearchRun(baseRun("short", "succeeded"), []MetricRow{
		runMetric("short", "pretrain/loss_minus_random_baseline", 50, -0.3),
	}, []TagRecord{{ScopeType: "run", ScopeID: "short", Key: "success.min_step", Value: "100"}})

	status, err := store.Status(ctx, question)
	if err != nil {
		t.Fatal(err)
	}
	if status.StateCounts["pending"] != 1 || status.StateCounts["running"] != 1 || status.StateCounts["failed"] != 1 || status.StateCounts["succeeded"] != 3 {
		t.Fatalf("raw state counts = %+v", status.StateCounts)
	}
	if status.LifecycleCounts["pending"] != 1 || status.LifecycleCounts["running"] != 1 || status.LifecycleCounts["failed"] != 1 || status.LifecycleCounts["succeeded"] != 1 || status.LifecycleCounts["incomplete"] != 2 {
		t.Fatalf("lifecycle counts = %+v", status.LifecycleCounts)
	}

	result, err := store.SearchRuns(ctx, RunSearchOptions{
		Target:        question,
		Lifecycle:     "succeeded",
		Tags:          map[string]string{"sweep": "dino"},
		MetricFilters: []MetricFilter{{MetricName: "pretrain/loss_minus_random_baseline", Op: "<", Value: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].RunID != "healthy" || !result.Runs[0].Successful {
		t.Fatalf("successful search result = %+v", result.Runs)
	}

	result, err = store.SearchRuns(ctx, RunSearchOptions{Target: question, Query: "collapse_bad_steps"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("metric-name search should find both runs with collapse metric: %+v", result.Runs)
	}
}

func TestSearchExperimentsAndExplicitRunAssignment(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "question-backed-experiment",
		Project:     "tau",
		Description: "Can experiments be searched?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, runID := range []string{"seed-1", "seed-2"} {
		if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
			Run: RunRecord{
				RunID:      runID,
				Project:    "tau",
				RunGroupID: "baseline",
				State:      "running",
				CreatedAt:  "2026-06-10T17:00:00Z",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AssignRunToExperiment(ctx, ExperimentRecord{
		ExperimentID: "manual-comparison",
		Project:      "tau",
		Name:         "Manual comparison",
		Description:  "Post-hoc selected runs",
		Source:       "explicit",
		CreatedAt:    "2026-06-10T17:05:00Z",
		UpdatedAt:    "2026-06-10T17:05:00Z",
	}, "seed-2"); err != nil {
		t.Fatal(err)
	}

	result, err := store.SearchExperiments(ctx, ExperimentSearchOptions{Query: "manual", Lifecycle: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "manual-comparison" || result.Experiments[0].RunCount != 1 {
		t.Fatalf("unexpected explicit experiment search: %+v", result.Experiments)
	}

	status, err := store.Status(ctx, "manual-comparison")
	if err != nil {
		t.Fatal(err)
	}
	if status.TargetType != "experiment" || status.Runs != 1 || status.LifecycleCounts["running"] != 1 {
		t.Fatalf("unexpected experiment status: %+v", status)
	}

	runs, err := store.SearchRuns(ctx, RunSearchOptions{Target: "manual-comparison"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != "seed-2" {
		t.Fatalf("experiment-targeted run search returned %+v", runs.Runs)
	}
}

func TestSearchExperimentsAndRunsByWorkspace(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "workspace-experiment",
		Project:     "tau",
		Description: "Can workspaces isolate experiment discovery?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, tc := range []struct {
		runID     string
		workspace string
	}{
		{runID: "sample-seed", workspace: "sample"},
		{runID: "research-seed", workspace: "research"},
	} {
		if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
			Run: RunRecord{
				RunID:      tc.runID,
				Project:    "tau",
				RunGroupID: "baseline",
				State:      "running",
				CreatedAt:  "2026-06-10T17:00:00Z",
			},
			Tags: []TagRecord{
				{
					ScopeType: "run",
					ScopeID:   tc.runID,
					Key:       "tau_workspace",
					Value:     tc.workspace,
				},
				{
					ScopeType: "run",
					ScopeID:   tc.runID,
					Key:       "suite",
					Value:     tc.workspace,
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	experiments, err := store.SearchExperiments(ctx, ExperimentSearchOptions{Workspace: "sample"})
	if err != nil {
		t.Fatal(err)
	}
	if len(experiments.Experiments) != 1 || experiments.Experiments[0].ExperimentID != "workspace-experiment" || experiments.Experiments[0].RunCount != 1 {
		t.Fatalf("workspace-scoped experiment search = %+v", experiments.Experiments)
	}
	crossWorkspace, err := store.SearchExperiments(ctx, ExperimentSearchOptions{
		Workspace: "sample",
		Tags:      map[string]string{"suite": "research"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(crossWorkspace.Experiments) != 0 {
		t.Fatalf("workspace filter matched another workspace's tagged run: %+v", crossWorkspace.Experiments)
	}
	injectedWorkspace, err := store.SearchExperiments(ctx, ExperimentSearchOptions{
		Workspace: `" OR 1=1 --`,
	})
	if err != nil {
		t.Fatalf("workspace value was treated as SQL instead of data: %v", err)
	}
	if len(injectedWorkspace.Experiments) != 0 {
		t.Fatalf("injected workspace escaped its filter: %+v", injectedWorkspace.Experiments)
	}

	runs, err := store.SearchRuns(ctx, RunSearchOptions{Workspace: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != "research-seed" {
		t.Fatalf("workspace-scoped run search = %+v", runs.Runs)
	}
}

func TestSearchRunsLifecycleFilterScansPastFirstPage(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "paged-lifecycle-search",
		Project:     "tau",
		Description: "Can lifecycle search find old successful runs?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := "paged-lifecycle-search"
	for i := 0; i < runSearchLifecycleBatchSize+5; i++ {
		if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
			Run: RunRecord{
				RunID:      fmt.Sprintf("failed-%03d", i),
				Project:    "tau",
				RunGroupID: "baseline",
				State:      "failed",
				CreatedAt:  fmt.Sprintf("2026-06-10T17:%02d:%02dZ", i/60, i%60),
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: RunRecord{
			RunID:      "older-success",
			Project:    "tau",
			RunGroupID: "baseline",
			State:      "succeeded",
			CreatedAt:  "2026-06-09T17:00:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := store.SearchRuns(ctx, RunSearchOptions{Target: target, Lifecycle: "succeeded", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].RunID != "older-success" {
		t.Fatalf("lifecycle search should scan beyond first page: %+v", result.Runs)
	}
}

func TestParseMetricFilterRejectsMissingMetricName(t *testing.T) {
	for _, spec := range []string{">=0.5", "<=1", "!=0"} {
		if _, err := ParseMetricFilter(spec); err == nil {
			t.Fatalf("ParseMetricFilter(%q) succeeded, want missing metric error", spec)
		}
	}
}

func TestRecordRunDataWritesSQLiteAndJSONLMirrors(t *testing.T) {
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
	size := int64(128)
	minStep := int64(1)
	maxStep := int64(2)
	gpuCount := int64(8)
	queueWait := 120.0
	gpuHours := 4.0
	estimatedCost := 3.49
	result, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: RunRecord{
			RunID:      "seed-1",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			Owner:      "agent",
		},
		RunContext: &RunContextRecord{
			RunID:            "seed-1",
			Cluster:          "kind-taugrid",
			Namespace:        "ray",
			Team:             "research",
			Profile:          "research-train-gpu",
			Lane:             "training",
			LocalQueue:       "training-queue",
			KueueWorkload:    "seed-1-workload",
			PodUID:           "pod-uid",
			ResourceClaims:   "claim-a",
			GPUClass:         "a100-80gb",
			GPUCount:         &gpuCount,
			NodeNames:        "node-a",
			Mounts:           `[{"source":"pvc","path":"/data"}]`,
			QueueWaitSeconds: &queueWait,
			GPUHours:         &gpuHours,
			EstimatedCost:    &estimatedCost,
			Runtime:          `{"python":"3.13"}`,
			Dependencies:     `{"packages":[{"name":"torch","version":"2"}]}`,
			LogURI:           "logs/seed-1.txt",
		},
		Configs: []ConfigRecord{{
			ConfigHash:     "config-seed-1",
			RunID:          "seed-1",
			Format:         "yaml",
			URI:            "configs/seed-1.yaml",
			NormalizedJSON: `{"seed":1}`,
		}},
		Tags: []TagRecord{{ScopeType: "run", ScopeID: "seed-1", Key: "source", Value: "tensorboard"}},
		Artifacts: []ArtifactRecord{{
			ArtifactID: "tb-event-seed-1",
			RunID:      "seed-1",
			Type:       "tensorboard",
			URI:        "artifacts/seed-1/events.out.tfevents.test",
			Name:       "events.out.tfevents.test",
			Digest:     "sha256:test",
			SizeBytes:  &size,
			CreatedAt:  "2026-05-16T00:00:00Z",
			Caption:    "tensorboard event file",
			Direction:  "output",
			Alias:      "events-latest",
		}},
		MetricFiles: []MetricFileRecord{{
			FileID:        "tb-metrics-seed-1",
			Path:          "metrics/project=project-alpha/run=seed-1/part.parquet",
			Format:        "parquet",
			SchemaVersion: MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    "reference-group",
			RunID:         "seed-1",
			RowCount:      2,
			MinStep:       &minStep,
			MaxStep:       &maxStep,
			CreatedAt:     "2026-05-16T00:00:01Z",
		}},
		IdempotencyKey: "tb-seed-1",
		Command:        "exp import jsonl",
		RequestHash:    "hash-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CreatedRun || !result.RunContext || result.Configs != 1 || result.MetricFiles != 1 || result.Artifacts != 1 || result.Tags != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	for _, mirror := range []string{"runs.jsonl", "run_context.jsonl", "configs.jsonl", "tags.jsonl", "artifacts.jsonl", "metric_files.jsonl", "idempotency_keys.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, AppendLogDir, mirror)); err != nil {
			t.Fatalf("expected mirror %s: %v", mirror, err)
		}
	}
	contextRows, err := store.Query(ctx, "select run_id, cluster, queue_wait_seconds, gpu_hours, estimated_cost, runtime, dependencies, log_uri from run_context where run_id = 'seed-1'")
	if err != nil {
		t.Fatal(err)
	}
	if len(contextRows.Rows) != 1 || contextRows.Rows[0]["cluster"] != "kind-taugrid" || fmt.Sprint(contextRows.Rows[0]["queue_wait_seconds"]) != "120" {
		t.Fatalf("unexpected run context rows: %+v", contextRows.Rows)
	}
	if contextRows.Rows[0]["runtime"] != `{"python":"3.13"}` || contextRows.Rows[0]["log_uri"] != "logs/seed-1.txt" {
		t.Fatalf("unexpected repro context rows: %+v", contextRows.Rows)
	}
	configRows, err := store.Query(ctx, "select config_hash, normalized_json from configs where run_id = 'seed-1'")
	if err != nil {
		t.Fatal(err)
	}
	if len(configRows.Rows) != 1 || configRows.Rows[0]["config_hash"] != "config-seed-1" || configRows.Rows[0]["normalized_json"] != `{"seed":1}` {
		t.Fatalf("unexpected config rows: %+v", configRows.Rows)
	}
	again, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run:            RunRecord{RunID: "seed-1", Project: "project-alpha", RunGroupID: "reference-group", State: "succeeded", Owner: "agent"},
		IdempotencyKey: "tb-seed-1",
		Command:        "exp import jsonl",
		RequestHash:    "hash-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Reused {
		t.Fatalf("expected reused idempotency result: %+v", again)
	}
}

func TestRecordRunDataIsIdempotentAndRejectsConflicts(t *testing.T) {
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
	size := int64(64)
	base := RecordRunDataOptions{
		Run: RunRecord{
			RunID:      "manual-seed-1",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			Owner:      "agent",
			ConfigHash: "manual-config",
		},
		Configs: []ConfigRecord{{
			ConfigHash:     "manual-config",
			RunID:          "manual-seed-1",
			Format:         "json",
			URI:            "configs/manual-seed-1.json",
			NormalizedJSON: `{"lr":0.001}`,
		}},
		Artifacts: []ArtifactRecord{{
			ArtifactID: "artifact-manual-seed-1-rollout",
			RunID:      "manual-seed-1",
			Type:       "video",
			URI:        "runs/manual-seed-1/rollout.mp4",
			Name:       "rollout.mp4",
			Digest:     "video-digest",
			SizeBytes:  &size,
			CreatedAt:  "2026-05-16T00:00:00Z",
		}},
		Tags: []TagRecord{{ScopeType: "run", ScopeID: "manual-seed-1", Key: "seed", Value: "1"}},
	}
	if result, err := store.RecordRunData(ctx, base); err != nil {
		t.Fatal(err)
	} else if !result.CreatedRun || result.Configs != 1 || result.Artifacts != 1 || result.Tags != 1 {
		t.Fatalf("unexpected first result: %+v", result)
	}
	if result, err := store.RecordRunData(ctx, base); err != nil {
		t.Fatal(err)
	} else if !result.Reused || result.Configs != 0 || result.Artifacts != 0 || result.Tags != 0 {
		t.Fatalf("expected second write to reuse existing rows: %+v", result)
	}

	configConflict := base
	configConflict.Configs = []ConfigRecord{{
		ConfigHash:     "manual-config",
		RunID:          "manual-seed-1",
		Format:         "json",
		URI:            "configs/changed.json",
		NormalizedJSON: `{"lr":0.001}`,
	}}
	if _, err := store.RecordRunData(ctx, configConflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected config conflict, got %v", err)
	}
	artifactConflict := base
	artifactConflict.Artifacts = []ArtifactRecord{{
		ArtifactID: "artifact-manual-seed-1-rollout",
		RunID:      "manual-seed-1",
		Type:       "video",
		URI:        "runs/manual-seed-1/changed.mp4",
		Name:       "rollout.mp4",
		Digest:     "video-digest",
		SizeBytes:  &size,
		CreatedAt:  "2026-05-16T00:00:00Z",
	}}
	if _, err := store.RecordRunData(ctx, artifactConflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected artifact conflict, got %v", err)
	}
	runConflict := base
	runConflict.Run.Owner = "different-agent"
	if _, err := store.RecordRunData(ctx, runConflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected run conflict, got %v", err)
	}
	for _, query := range []string{
		"select count(*) as count from configs where run_id = 'manual-seed-1'",
		"select count(*) as count from artifacts where run_id = 'manual-seed-1'",
		"select count(*) as count from tags where scope_type = 'run' and scope_id = 'manual-seed-1'",
	} {
		got, err := store.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if got.Rows[0]["count"] != int64(1) {
			t.Fatalf("%s => %+v", query, got.Rows)
		}
	}
}

func TestOpenMigratesLegacyArtifactColumns(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(filepath.Join(root, IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
DROP TABLE artifacts;
CREATE TABLE artifacts (
  artifact_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  type TEXT NOT NULL,
  uri TEXT NOT NULL,
  name TEXT NOT NULL,
  digest TEXT,
  size_bytes INTEGER,
  created_at TEXT NOT NULL,
  preview TEXT,
  external_ref TEXT
);
CREATE INDEX idx_artifacts_run ON artifacts(run_id);
`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	size := int64(4096)
	step := int64(12)
	rank := int64(0)
	artifact := ArtifactRecord{
		ArtifactID:  "artifact-manual-seed-1-confusion",
		RunID:       "manual-seed-1",
		Type:        "image",
		URI:         "artifacts/manual-seed-1/confusion.svg",
		Name:        "confusion-matrix",
		DurableRef:  `{"schema_version":"tau.blobref.v1","uri":"azblob://acct.blob.core.windows.net/tau-artifacts/v1/project=project-alpha/run=manual-seed-1/image/ab/confusion.svg","digest":"sha256:abc","size_bytes":4096}`,
		ContentType: "image/svg+xml",
		Digest:      "sha256:abc",
		SizeBytes:   &size,
		Step:        &step,
		Tags:        `{"artifact_tag":"confusion"}`,
		Rank:        &rank,
		CreatedAt:   "2026-06-16T00:00:00Z",
	}
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: RunRecord{
			RunID:      "manual-seed-1",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			CreatedAt:  "2026-06-16T00:00:00Z",
		},
		Artifacts: []ArtifactRecord{artifact},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.Query(ctx, `select durable_ref, content_type, step, tags, rank from artifacts where artifact_id = 'artifact-manual-seed-1-confusion'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("artifact rows = %d, want 1", len(rows.Rows))
	}
	row := rows.Rows[0]
	if row["durable_ref"] != artifact.DurableRef || row["content_type"] != artifact.ContentType || row["step"] != step || row["tags"] != artifact.Tags || row["rank"] != rank {
		t.Fatalf("migrated artifact columns not persisted: %+v", row)
	}
}

func TestQueryArgsBindsParameters(t *testing.T) {
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
	runID := "seed-1"
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: RunRecord{
			RunID:      runID,
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			Owner:      "agent",
		},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := store.QueryArgs(ctx, "select run_id from runs where run_id = ?", "seed-1' OR 1=1 --")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 0 {
		t.Fatalf("injection-like parameter returned %+v, want no rows", result.Rows)
	}

	result, err = store.QueryArgs(ctx, "select run_id from runs where run_id = ?", runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["run_id"] != runID {
		t.Fatalf("parameterized query returned %+v, want exact run id", result.Rows)
	}
}

func TestEnrichRunDataUpdatesLifecycleAndDedupesEvents(t *testing.T) {
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
	created := "2026-05-19T10:00:00Z"
	started := "2026-05-19T10:05:00Z"
	queueWait := 300.0
	gpuCount := int64(8)
	first, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: RunRecord{
			RunID:      "train-001",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "pending",
			Owner:      "tau-controller",
			CreatedAt:  created,
		},
		RunContext: &RunContextRecord{
			RunID:      "train-001",
			Cluster:    "kind-taugrid",
			Namespace:  "ray",
			LocalQueue: "training-queue",
			GPUCount:   &gpuCount,
		},
		Tags: []TagRecord{{ScopeType: "run", ScopeID: "train-001", Key: "tau.capture.source", Value: "controller-autocapture"}},
		Events: []EventRecord{{
			EventID:  "event-submitted-train-001",
			RunID:    "train-001",
			Time:     created,
			Type:     "submitted",
			Source:   "kubernetes/job",
			Severity: "info",
			Message:  "Job ray/train-001 was submitted",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.CreatedRun || !first.CreatedRunContext || first.Events != 1 {
		t.Fatalf("unexpected first enrichment: %+v", first)
	}
	second, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: RunRecord{
			RunID:      "train-001",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "running",
			StartedAt:  started,
		},
		RunContext: &RunContextRecord{
			RunID:            "train-001",
			PodUID:           "pod-uid",
			ResourceClaims:   "claim-a",
			NodeNames:        "node-a",
			QueueWaitSeconds: &queueWait,
		},
		Tags: []TagRecord{{ScopeType: "run", ScopeID: "train-001", Key: "tau.capture.source", Value: "controller-autocapture"}},
		Events: []EventRecord{{
			EventID:  "event-running-train-001",
			RunID:    "train-001",
			Time:     started,
			Type:     "running",
			Source:   "kubernetes/job",
			Severity: "info",
			Message:  "Job ray/train-001 started running",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.UpdatedRun || !second.UpdatedRunContext || second.Events != 1 {
		t.Fatalf("unexpected second enrichment: %+v", second)
	}
	again, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: RunRecord{
			RunID:      "train-001",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "running",
			StartedAt:  started,
		},
		RunContext: &RunContextRecord{
			RunID:            "train-001",
			PodUID:           "pod-uid",
			ResourceClaims:   "claim-a",
			NodeNames:        "node-a",
			QueueWaitSeconds: &queueWait,
		},
		Tags: []TagRecord{{ScopeType: "run", ScopeID: "train-001", Key: "tau.capture.source", Value: "controller-autocapture"}},
		Events: []EventRecord{{
			EventID:  "event-running-train-001",
			RunID:    "train-001",
			Time:     started,
			Type:     "running",
			Source:   "kubernetes/job",
			Severity: "info",
			Message:  "Job ray/train-001 started running",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Reused || again.Events != 0 || again.Tags != 0 {
		t.Fatalf("expected idempotent reuse, got %+v", again)
	}
	rows, err := store.Query(ctx, "select state, started_at from runs where run_id = 'train-001'")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Rows[0]["state"] != "running" || rows.Rows[0]["started_at"] != started {
		t.Fatalf("run was not updated: %+v", rows.Rows)
	}
	eventRows, err := store.Query(ctx, "select count(*) as count from events where run_id = 'train-001'")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(eventRows.Rows[0]["count"]) != "2" {
		t.Fatalf("events were duplicated: %+v", eventRows.Rows)
	}
}

func TestRecordRunContextMergesWithAutocaptureEnrichment(t *testing.T) {
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name:        "merge-context",
		Project:     "tau",
		Description: "Can record and autocapture context merge?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := RunRecord{RunID: "run-x", Project: "tau", RunGroupID: "baseline", State: "running", CreatedAt: "2026-06-17T00:00:00Z"}
	if _, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: run,
		RunContext: &RunContextRecord{
			RunID:   "run-x",
			Cluster: "taugrid",
			Team:    "research",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: run,
		RunContext: &RunContextRecord{
			RunID:        "run-x",
			Runtime:      `{"python":"3.13"}`,
			Dependencies: `{"packages":[{"name":"lightning","version":"2"}]}`,
			LogURI:       "logs/run-x.txt",
		},
		IdempotencyKey: "record-runtime",
	}); err != nil {
		t.Fatalf("record after enrichment should merge run_context: %v", err)
	}
	rows, err := store.Query(ctx, "select cluster, team, runtime, dependencies, log_uri from run_context where run_id = 'run-x'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0]["cluster"] != "taugrid" || rows.Rows[0]["runtime"] != `{"python":"3.13"}` || rows.Rows[0]["log_uri"] != "logs/run-x.txt" {
		t.Fatalf("merged context missing fields: %+v", rows.Rows)
	}

	run2 := RunRecord{RunID: "run-y", Project: "tau", RunGroupID: "baseline", State: "running", CreatedAt: "2026-06-17T00:00:00Z"}
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run: run2,
		RunContext: &RunContextRecord{
			RunID:   "run-y",
			Runtime: `{"python":"3.13"}`,
			LogURI:  "logs/run-y.txt",
		},
		IdempotencyKey: "record-runtime-first",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: run2,
		RunContext: &RunContextRecord{
			RunID:   "run-y",
			Cluster: "taugrid",
			Team:    "research",
		},
	}); err != nil {
		t.Fatalf("enrichment after record should merge run_context: %v", err)
	}
	rows, err = store.Query(ctx, "select cluster, team, runtime, log_uri from run_context where run_id = 'run-y'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0]["cluster"] != "taugrid" || rows.Rows[0]["runtime"] != `{"python":"3.13"}` || rows.Rows[0]["log_uri"] != "logs/run-y.txt" {
		t.Fatalf("merged reverse-order context missing fields: %+v", rows.Rows)
	}
}

func TestExportCopiesPacket(t *testing.T) {
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

	dest := filepath.Join(t.TempDir(), "packet")
	result, err := store.Export(ctx, ExportOptions{Out: dest})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesCopied == 0 {
		t.Fatalf("expected copied files: %+v", result)
	}
	for _, want := range []string{ManifestFile, IndexFile, AppendLogDir, MetricsDir, ArtifactsDir, "README.json"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Fatalf("expected export %s: %v", want, err)
		}
	}
	if _, err := store.Export(ctx, ExportOptions{Out: dest}); err == nil {
		t.Fatal("expected non-empty export destination to require --force")
	}
}
