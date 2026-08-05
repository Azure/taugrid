package expstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeV0Store lays down a store whose manifest claims the pre-migration
// schema and whose SQLite index still carries the hypothesis axis.
func writeV0Store(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	manifest, err := ensureStoreFiles(root, InitOptions{
		Name: "old-question", Project: "old", Description: "old", Group: "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	// ensureStoreFiles stamps the current version; rewrite it to the old one so
	// Open has to migrate rather than no-op.
	manifest.SchemaVersion = "expstore.v0"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE hypotheses (
  hypothesis_id TEXT PRIMARY KEY, question_id TEXT NOT NULL, hypothesis_text TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE run_groups (
  run_group_id TEXT PRIMARY KEY, project TEXT NOT NULL, question_id TEXT NOT NULL,
  hypothesis_id TEXT, name TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE runs (
  run_id TEXT PRIMARY KEY, project TEXT NOT NULL, question_id TEXT, hypothesis_id TEXT,
  run_group_id TEXT NOT NULL, parent_run_id TEXT, state TEXT NOT NULL, owner TEXT,
  created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT, config_hash TEXT,
  code_sha TEXT, image_digest TEXT, tau_command TEXT, result_uri TEXT, index_version TEXT NOT NULL
);
CREATE TABLE metric_files (
  file_id TEXT PRIMARY KEY, path TEXT NOT NULL, format TEXT NOT NULL, schema_version TEXT NOT NULL,
  schema_hash TEXT, project TEXT, question_id TEXT, hypothesis_id TEXT, run_group_id TEXT,
  run_id TEXT, row_count INTEGER, digest TEXT, min_step INTEGER, max_step INTEGER,
  created_at TEXT NOT NULL
);
INSERT INTO hypotheses VALUES ('h1','old-question','a hypothesis','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
INSERT INTO runs(run_id, project, question_id, hypothesis_id, run_group_id, state, created_at, index_version)
VALUES ('old-run','old','old-question','h1','baseline','succeeded','2026-01-01T00:00:00Z','expstore.v0');
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// A v0 store must open. Before the migration chain existed, readManifest
// rejected any non-current schema outright, so a version bump bricked the store
// instead of upgrading it.
func TestOpenUpgradesV0StoreToCurrentSchema(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "v0-store")
	writeV0Store(t, root)

	store, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("open v0 store: %v", err)
	}
	defer store.Close()

	if got := store.Manifest().SchemaVersion; got != SchemaVersion {
		t.Fatalf("manifest schema version = %q, want %q", got, SchemaVersion)
	}

	// The upgrade must be durable, not just in-memory.
	var onDisk Manifest
	raw, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.SchemaVersion != SchemaVersion {
		t.Fatalf("manifest.json schema version = %q, want %q", onDisk.SchemaVersion, SchemaVersion)
	}
}

func TestV0MigrationRemovesHypothesisAxis(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "v0-store")
	writeV0Store(t, root)

	store, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, table := range []string{"runs", "run_groups", "metric_files"} {
		has, err := columnExists(ctx, store.db, table, "hypothesis_id")
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Errorf("%s.hypothesis_id still present after migration", table)
		}
	}
	present, err := tableExists(ctx, store.db, "hypotheses")
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("hypotheses table still present after migration")
	}

	// Rows unrelated to the dropped axis must survive.
	var runID string
	if err := store.db.QueryRowContext(ctx, `SELECT run_id FROM runs`).Scan(&runID); err != nil {
		t.Fatalf("pre-existing run lost during migration: %v", err)
	}
	if runID != "old-run" {
		t.Fatalf("run_id = %q, want old-run", runID)
	}
}

// Reopening must be a no-op, not a repeated migration.
func TestSchemaMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "v0-store")
	writeV0Store(t, root)

	for i := 0; i < 3; i++ {
		store, err := Open(ctx, root)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		if got := store.Manifest().SchemaVersion; got != SchemaVersion {
			t.Fatalf("open #%d schema = %q, want %q", i+1, got, SchemaVersion)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnknownSchemaVersionIsStillRejected(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "future-store")
	writeV0Store(t, root)

	raw, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = "expstore.v999"
	updated, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestFile), updated, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(ctx, root); err == nil {
		t.Fatal("expected unknown schema version to be rejected")
	}
}

// The chain must be well-formed: contiguous, and ending at SchemaVersion.
func TestSchemaMigrationChainIsContiguous(t *testing.T) {
	if len(schemaMigrations) == 0 {
		if SchemaVersion == "" {
			t.Fatal("SchemaVersion must not be empty")
		}
		return
	}
	for i := 1; i < len(schemaMigrations); i++ {
		if schemaMigrations[i].from != schemaMigrations[i-1].to {
			t.Fatalf("migration %d starts at %q but previous ends at %q",
				i, schemaMigrations[i].from, schemaMigrations[i-1].to)
		}
	}
	if last := schemaMigrations[len(schemaMigrations)-1].to; last != SchemaVersion {
		t.Fatalf("final migration targets %q, want SchemaVersion %q", last, SchemaVersion)
	}
}

// writeV1StoreWithQuestions lays down a store at the schema version that still
// had a questions table plus runs.question_id, so the v1 -> v2 promotion has
// something real to migrate.
func writeV1StoreWithQuestions(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	manifest, err := ensureStoreFiles(root, InitOptions{
		Name: "experiment-alpha", Project: "project-alpha", Description: "old", Group: "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = "expstore.v1"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE questions (
  question_id TEXT PRIMARY KEY, project TEXT NOT NULL, question_text TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE experiments (
  experiment_id TEXT PRIMARY KEY, project TEXT NOT NULL, name TEXT NOT NULL,
  description TEXT, question_id TEXT, source TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE run_experiments (
  experiment_id TEXT NOT NULL, run_id TEXT NOT NULL, role TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  PRIMARY KEY (experiment_id, run_id)
);
CREATE TABLE run_groups (
  run_group_id TEXT PRIMARY KEY, project TEXT NOT NULL, question_id TEXT NOT NULL,
  name TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE runs (
  run_id TEXT PRIMARY KEY, project TEXT NOT NULL, question_id TEXT,
  run_group_id TEXT NOT NULL, parent_run_id TEXT, state TEXT NOT NULL, owner TEXT,
  created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT, config_hash TEXT,
  code_sha TEXT, image_digest TEXT, tau_command TEXT, result_uri TEXT, index_version TEXT NOT NULL
);
CREATE TABLE metric_files (
  file_id TEXT PRIMARY KEY, path TEXT NOT NULL, format TEXT NOT NULL, schema_version TEXT NOT NULL,
  schema_hash TEXT, project TEXT, question_id TEXT, run_group_id TEXT,
  run_id TEXT, row_count INTEGER, digest TEXT, min_step INTEGER, max_step INTEGER,
  created_at TEXT NOT NULL
);
INSERT INTO questions VALUES
  ('experiment-alpha','project-alpha','Can Stellar replace W&B?','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
INSERT INTO run_groups VALUES
  ('reference-group','project-alpha','experiment-alpha','baseline','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('candidate-group','project-alpha','experiment-alpha','ablation','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
INSERT INTO runs(run_id, project, question_id, run_group_id, state, created_at, index_version)
VALUES ('seed-1','project-alpha','experiment-alpha','reference-group','succeeded','2026-01-01T00:00:00Z','expstore.v1'),
       ('seed-2','project-alpha','experiment-alpha','candidate-group','succeeded','2026-01-01T00:00:00Z','expstore.v1');
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// The question axis was not deleted, it was promoted. A question *was* the
// named experiment, so after migrating there must be exactly one experiment
// spanning both arms -- not one experiment per run group, which would make the
// baseline and the ablation impossible to compare.
func TestV1MigrationPromotesQuestionsToExperiments(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "v1-store")
	writeV1StoreWithQuestions(t, root)

	store, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("open v1 store: %v", err)
	}
	defer store.Close()

	var id, name, description string
	if err := store.db.QueryRowContext(ctx,
		`SELECT experiment_id, name, coalesce(description, '') FROM experiments`).
		Scan(&id, &name, &description); err != nil {
		t.Fatalf("expected exactly one promoted experiment: %v", err)
	}
	if id != "experiment-alpha" {
		t.Fatalf("experiment_id = %q, want the question id", id)
	}
	if description != "Can Stellar replace W&B?" {
		t.Fatalf("question text was not folded into the description: %q", description)
	}

	// runs.question_id carried the run -> named-experiment link. Losing it
	// would silently drop every run back to its run_group. v3 moved that link
	// from the run_experiments join table onto runs.experiment_id, so the
	// end-to-end v1 -> v3 walk must land it there.
	var linked int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM runs WHERE experiment_id = 'experiment-alpha'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 2 {
		t.Fatalf("runs linked to the experiment = %d, want both runs replayed", linked)
	}

	// Both arms must resolve to the one experiment they belong to, while
	// remaining distinct groups -- comparing baseline against ablation is the
	// point of the experiment.
	rows, err := store.db.QueryContext(ctx,
		`SELECT run_group_id, experiment_id FROM runs ORDER BY run_group_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var group, experiment string
		if err := rows.Scan(&group, &experiment); err != nil {
			t.Fatal(err)
		}
		got[group] = experiment
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"candidate-group": "experiment-alpha", "reference-group": "experiment-alpha"}
	for group, experiment := range want {
		if got[group] != experiment {
			t.Fatalf("run experiment_id by group = %+v, want %+v", got, want)
		}
	}

	// The join table and the group-level experiment column are the M:N model
	// v3 narrowed to 1:N; both must be gone from the schema, not merely unread.
	joinPresent, err := tableExists(ctx, store.db, "run_experiments")
	if err != nil {
		t.Fatal(err)
	}
	if joinPresent {
		t.Fatal("run_experiments survived the v2 -> v3 migration")
	}
	groupExperiment, err := columnExists(ctx, store.db, "run_groups", "experiment_id")
	if err != nil {
		t.Fatal(err)
	}
	if groupExperiment {
		t.Fatal("run_groups.experiment_id survived the v2 -> v3 migration")
	}

	// The dead axis must be gone from the schema, not merely unread.
	for _, target := range []struct{ table, column string }{
		{"runs", "question_id"},
		{"run_groups", "question_id"},
		{"metric_files", "question_id"},
		{"experiments", "question_id"},
	} {
		present, err := columnExists(ctx, store.db, target.table, target.column)
		if err != nil {
			t.Fatal(err)
		}
		if present {
			t.Fatalf("%s.%s survived the migration", target.table, target.column)
		}
	}
	present, err := tableExists(ctx, store.db, "questions")
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("questions table survived the migration")
	}
}

// v2 linked runs to experiments through the run_experiments join table, which
// allowed a run to belong to many experiments. v3 narrows that to 1:N by moving
// the link onto runs.experiment_id, so the migration has to pick one winner per
// run. It prefers an explicitly recorded experiment over the run_group fallback
// the store auto-synthesizes, and breaks a remaining tie on the lowest id so the
// outcome does not depend on row order.
func TestV2MigrationCollapsesRunExperimentsIntoRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeV2StoreWithRunExperiments(t, root)

	store, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("open v2 store: %v", err)
	}
	defer store.Close()

	rows, err := store.db.QueryContext(ctx, `SELECT run_id, experiment_id FROM runs ORDER BY run_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var run, experiment string
		if err := rows.Scan(&run, &experiment); err != nil {
			t.Fatal(err)
		}
		got[run] = experiment
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		// Only ever had the auto-synthesized run_group link.
		"only-group": "reference-group",
		// An explicit experiment must beat the run_group fallback.
		"explicit-wins": "experiment-alpha",
		// Two explicit experiments, no fallback: lowest id wins deterministically.
		"tie": "aardvark-sweep",
		// Never linked at all: falls back to the group it ran in.
		"unlinked": "candidate-group",
	}
	for run, experiment := range want {
		if got[run] != experiment {
			t.Fatalf("runs.experiment_id = %+v, want %+v", got, want)
		}
	}
}

func writeV2StoreWithRunExperiments(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	manifest, err := ensureStoreFiles(root, InitOptions{
		Name: "experiment-alpha", Project: "project-alpha", Group: "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = "expstore.v2"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE experiments (
  experiment_id TEXT PRIMARY KEY, project TEXT NOT NULL, name TEXT NOT NULL,
  description TEXT, source TEXT NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE run_experiments (
  experiment_id TEXT NOT NULL, run_id TEXT NOT NULL, role TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  PRIMARY KEY (experiment_id, run_id)
);
CREATE TABLE run_groups (
  run_group_id TEXT PRIMARY KEY, project TEXT NOT NULL, experiment_id TEXT,
  name TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE runs (
  run_id TEXT PRIMARY KEY, project TEXT NOT NULL,
  run_group_id TEXT NOT NULL, parent_run_id TEXT, state TEXT NOT NULL, owner TEXT,
  created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT, config_hash TEXT,
  code_sha TEXT, image_digest TEXT, tau_command TEXT, result_uri TEXT, index_version TEXT NOT NULL
);
CREATE TABLE metric_files (
  file_id TEXT PRIMARY KEY, path TEXT NOT NULL, format TEXT NOT NULL, schema_version TEXT NOT NULL,
  schema_hash TEXT, project TEXT, run_group_id TEXT,
  run_id TEXT, row_count INTEGER, digest TEXT, min_step INTEGER, max_step INTEGER,
  created_at TEXT NOT NULL
);
INSERT INTO experiments VALUES
  ('experiment-alpha','project-alpha','experiment-alpha',NULL,'explicit','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('zebra-sweep','project-alpha','zebra-sweep',NULL,'explicit','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('aardvark-sweep','project-alpha','aardvark-sweep',NULL,'explicit','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('reference-group','project-alpha','reference-group',NULL,'run_group','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
INSERT INTO run_groups VALUES
  ('reference-group','project-alpha','experiment-alpha','baseline','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('candidate-group','project-alpha',NULL,'ablation','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
INSERT INTO runs(run_id, project, run_group_id, state, created_at, index_version)
VALUES ('only-group','project-alpha','reference-group','succeeded','2026-01-01T00:00:00Z','expstore.v2'),
       ('explicit-wins','project-alpha','reference-group','succeeded','2026-01-01T00:00:00Z','expstore.v2'),
       ('tie','project-alpha','reference-group','succeeded','2026-01-01T00:00:00Z','expstore.v2'),
       ('unlinked','project-alpha','candidate-group','succeeded','2026-01-01T00:00:00Z','expstore.v2');
INSERT INTO run_experiments(experiment_id, run_id, created_at, updated_at) VALUES
  ('reference-group','only-group','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('reference-group','explicit-wins','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('experiment-alpha','explicit-wins','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('zebra-sweep','tie','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
  ('aardvark-sweep','tie','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestV3MigrationPreservesRetiredMutableUIState proves the full migration chain
// stops exposing mutable state without destroying data needed for rollback.
func TestV3MigrationPreservesRetiredMutableUIState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	manifest, err := ensureStoreFiles(root, InitOptions{Name: "run-1", Project: "project-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = "expstore.v3"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE stellar_workspaces (
  workspace_id TEXT PRIMARY KEY, target TEXT NOT NULL, target_type TEXT NOT NULL,
  project TEXT NOT NULL, name TEXT NOT NULL, description TEXT,
  pinned_metrics TEXT NOT NULL, sections TEXT NOT NULL, tags TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL, created_by TEXT, UNIQUE (target, project, name)
);
CREATE INDEX idx_stellar_workspaces_target ON stellar_workspaces(target);
CREATE TABLE label_overlays (
  overlay_id TEXT PRIMARY KEY,
  scope_type TEXT NOT NULL,
  scope_id TEXT NOT NULL
);
INSERT INTO stellar_workspaces VALUES
  ('ws-1','run-1','run','project-alpha','Loss view','',
   '["pretrain/loss"]','{"order":["charts"]}','{}',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','owner-a');
INSERT INTO label_overlays VALUES ('overlay-1','run','run-1');
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, root)
	if err != nil {
		t.Fatalf("open v3 store: %v", err)
	}
	defer store.Close()

	workspacePresent, err := tableExists(ctx, store.db, "stellar_workspaces")
	if err != nil {
		t.Fatal(err)
	}
	if workspacePresent {
		t.Fatal("legacy workspace table should be renamed to avoid tenancy ambiguity")
	}
	for table, wantRows := range map[string]int{"stellar_dashboards": 1, "label_overlays": 1} {
		present, err := tableExists(ctx, store.db, table)
		if err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Fatalf("%s should be retained for rollback", table)
		}
		var rows int
		if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != wantRows {
			t.Fatalf("%s row count = %d, want %d", table, rows, wantRows)
		}
	}
}
