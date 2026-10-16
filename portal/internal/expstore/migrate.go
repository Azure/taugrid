// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

type schemaMigration struct {
	from  string
	to    string
	apply func(ctx context.Context, db *sql.DB) error
}

var schemaMigrations = []schemaMigration{
	{from: "expstore.v0", to: "expstore.v1", apply: migrateV0ToV1},
	{from: "expstore.v1", to: "expstore.v2", apply: migrateV1ToV2},
	{from: "expstore.v2", to: "expstore.v3", apply: migrateV2ToV3},
	{from: "expstore.v3", to: "expstore.v4", apply: migrateV3ToV4},
	{from: "expstore.v4", to: "expstore.v5", apply: migrateV4ToV5},
}

func knownSchemaVersions() []string {
	versions := make([]string, 0, len(schemaMigrations)+1)
	for _, migration := range schemaMigrations {
		versions = append(versions, migration.from)
	}
	return append(versions, SchemaVersion)
}

func isKnownSchemaVersion(version string) bool {
	for _, known := range knownSchemaVersions() {
		if known == version {
			return true
		}
	}
	return false
}

func applySchemaMigrations(ctx context.Context, db *sql.DB, current string) (string, error) {
	if current == "" {
		current = knownSchemaVersions()[0]
	}
	if current == SchemaVersion {
		return current, nil
	}
	if !isKnownSchemaVersion(current) {
		return current, fmt.Errorf("unsupported experiment store schema %q", current)
	}
	for _, migration := range schemaMigrations {
		if migration.from != current {
			continue
		}
		if err := migration.apply(ctx, db); err != nil {
			return current, fmt.Errorf("migrate experiment store %s -> %s: %w", migration.from, migration.to, err)
		}
		current = migration.to
		if current == SchemaVersion {
			break
		}
	}
	if current != SchemaVersion {
		return current, fmt.Errorf("experiment store schema %q has no migration path to %q", current, SchemaVersion)
	}
	return current, nil
}

func (s *Store) writeManifestVersion(version string) error {
	if s.manifest.SchemaVersion == version {
		return nil
	}
	s.manifest.SchemaVersion = version
	s.manifest.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return fileutil.WriteJSONFileAtomic(filepath.Join(s.Root, ManifestFile), s.manifest)
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func dropColumnIfExists(ctx context.Context, db *sql.DB, table, column string) error {
	present, err := tableExists(ctx, db, table)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	has, err := columnExists(ctx, db, table, column)
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" DROP COLUMN "+column)
	return err
}

func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, columnType string) error {
	present, err := tableExists(ctx, db, table)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	has, err := columnExists(ctx, db, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+columnType)
	return err
}

func dropIndexIfExists(ctx context.Context, db *sql.DB, index string) error {
	_, err := db.ExecContext(ctx, "DROP INDEX IF EXISTS "+index)
	return err
}

func migrateV0ToV1(ctx context.Context, db *sql.DB) error {
	for _, index := range []string{
		"idx_runs_hypothesis",
		"idx_run_groups_hypothesis",
		"idx_metric_files_hypothesis",
	} {
		if err := dropIndexIfExists(ctx, db, index); err != nil {
			return err
		}
	}
	for _, target := range []struct{ table, column string }{
		{"runs", "hypothesis_id"},
		{"run_groups", "hypothesis_id"},
		{"metric_files", "hypothesis_id"},
	} {
		if err := dropColumnIfExists(ctx, db, target.table, target.column); err != nil {
			return fmt.Errorf("drop %s.%s: %w", target.table, target.column, err)
		}
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS hypotheses"); err != nil {
		return fmt.Errorf("drop hypotheses table: %w", err)
	}
	return nil
}

func migrateV1ToV2(ctx context.Context, db *sql.DB) error {
	questionsPresent, err := tableExists(ctx, db, "questions")
	if err != nil {
		return err
	}
	experimentsHaveQuestion, err := columnExists(ctx, db, "experiments", "question_id")
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if questionsPresent {
		if _, err := db.ExecContext(ctx, `
INSERT OR IGNORE INTO experiments(experiment_id, project, name, description, source, created_at, updated_at)
SELECT q.question_id, q.project, q.question_id, nullif(q.question_text, ''), 'question',
       coalesce(q.created_at, ?), coalesce(q.updated_at, ?)
FROM questions q`, now, now); err != nil {
			return fmt.Errorf("promote questions to experiments: %w", err)
		}
	}

	if questionsPresent && experimentsHaveQuestion {
		if _, err := db.ExecContext(ctx, `
UPDATE experiments
   SET description = (SELECT q.question_text FROM questions q WHERE q.question_id = experiments.question_id)
 WHERE coalesce(description, '') = ''
   AND question_id IS NOT NULL
   AND EXISTS (SELECT 1 FROM questions q WHERE q.question_id = experiments.question_id)`); err != nil {
			return fmt.Errorf("fold question text into experiment descriptions: %w", err)
		}
	}

	if err := addColumnIfMissing(ctx, db, "run_groups", "experiment_id", "TEXT"); err != nil {
		return err
	}

	runsHaveQuestion, err := columnExists(ctx, db, "runs", "question_id")
	if err != nil {
		return err
	}
	if runsHaveQuestion {
		if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS run_experiments (
  experiment_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  role TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (experiment_id, run_id)
)`); err != nil {
			return fmt.Errorf("recreate run_experiments for replay: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
INSERT OR IGNORE INTO run_experiments(experiment_id, run_id, role, created_at, updated_at)
SELECT r.question_id, r.run_id, NULL, ?, ?
FROM runs r
WHERE coalesce(r.question_id, '') != ''
  AND EXISTS (SELECT 1 FROM experiments e WHERE e.experiment_id = r.question_id)`, now, now); err != nil {
			return fmt.Errorf("replay run question links into run_experiments: %w", err)
		}
		if _, err := db.ExecContext(ctx, `
UPDATE run_groups
   SET experiment_id = (
     SELECT min(r.question_id) FROM runs r
     WHERE r.run_group_id = run_groups.run_group_id
       AND coalesce(r.question_id, '') != ''
   )
 WHERE coalesce(experiment_id, '') = ''
   AND EXISTS (
     SELECT 1 FROM runs r
     WHERE r.run_group_id = run_groups.run_group_id
       AND coalesce(r.question_id, '') != ''
   )`); err != nil {
			return fmt.Errorf("backfill run group experiment ids: %w", err)
		}
	}

	for _, index := range []string{
		"idx_runs_question",
		"idx_run_groups_question",
		"idx_experiments_question",
		"idx_metric_summaries_question",
		"idx_stellar_workspaces_question",
	} {
		if err := dropIndexIfExists(ctx, db, index); err != nil {
			return err
		}
	}

	if err := rebuildStellarWorkspacesWithoutQuestion(ctx, db); err != nil {
		return err
	}

	for _, target := range []struct{ table, column string }{
		{"runs", "question_id"},
		{"run_groups", "question_id"},
		{"experiments", "question_id"},
		{"metric_files", "question_id"},
		{"metric_summaries", "question_id"},
	} {
		if err := dropColumnIfExists(ctx, db, target.table, target.column); err != nil {
			return fmt.Errorf("drop %s.%s: %w", target.table, target.column, err)
		}
	}

	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS questions"); err != nil {
		return fmt.Errorf("drop questions table: %w", err)
	}
	return nil
}

func rebuildStellarWorkspacesWithoutQuestion(ctx context.Context, db *sql.DB) error {
	present, err := tableExists(ctx, db, "stellar_workspaces")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	hasQuestion, err := columnExists(ctx, db, "stellar_workspaces", "question_id")
	if err != nil {
		return err
	}
	if !hasQuestion {
		return nil
	}
	stmts := []string{
		`CREATE TABLE stellar_workspaces_v2 (
  workspace_id TEXT PRIMARY KEY,
  target TEXT NOT NULL,
  target_type TEXT NOT NULL,
  project TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT,
  pinned_metrics TEXT NOT NULL,
  sections TEXT NOT NULL,
  tags TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  created_by TEXT,
  UNIQUE (target, project, name)
)`,
		`INSERT INTO stellar_workspaces_v2
SELECT workspace_id, target, target_type, project, name, description, pinned_metrics,
       sections, tags, is_default, created_at, updated_at, created_by
  FROM stellar_workspaces
 WHERE workspace_id IN (
   SELECT workspace_id FROM (
     SELECT workspace_id,
            row_number() OVER (PARTITION BY target, project, name ORDER BY updated_at DESC, workspace_id ASC) AS rn
       FROM stellar_workspaces
   ) WHERE rn = 1
 )`,
		`DROP TABLE stellar_workspaces`,
		`ALTER TABLE stellar_workspaces_v2 RENAME TO stellar_workspaces`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rebuild stellar_workspaces: %w", err)
		}
	}
	return nil
}

func migrateV2ToV3(ctx context.Context, db *sql.DB) error {
	if err := addColumnIfMissing(ctx, db, "runs", "experiment_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	joinExists, err := tableExists(ctx, db, "run_experiments")
	if err != nil {
		return err
	}
	if joinExists {
		if _, err := db.ExecContext(ctx, `
UPDATE runs SET experiment_id = coalesce((
  SELECT re.experiment_id
  FROM run_experiments re
  LEFT JOIN experiments e ON e.experiment_id = re.experiment_id
  WHERE re.run_id = runs.run_id
  ORDER BY (CASE WHEN coalesce(e.source, '') = 'run_group' THEN 1 ELSE 0 END), re.experiment_id
  LIMIT 1
), '')
WHERE coalesce(experiment_id, '') = ''`); err != nil {
			return fmt.Errorf("collapse run_experiments into runs.experiment_id: %w", err)
		}
		if _, err := db.ExecContext(ctx, `DROP TABLE run_experiments`); err != nil {
			return fmt.Errorf("drop run_experiments: %w", err)
		}
	}

	if _, err := db.ExecContext(ctx, `
UPDATE runs SET experiment_id = run_group_id
WHERE coalesce(experiment_id, '') = '' AND coalesce(run_group_id, '') != ''`); err != nil {
		return fmt.Errorf("backfill runs.experiment_id from run group: %w", err)
	}

	if err := dropIndexIfExists(ctx, db, "idx_run_experiments_run"); err != nil {
		return err
	}
	if err := dropColumnIfExists(ctx, db, "run_groups", "experiment_id"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_runs_experiment ON runs(experiment_id)`); err != nil {
		return fmt.Errorf("create idx_runs_experiment: %w", err)
	}
	return nil
}

func migrateV3ToV4(ctx context.Context, db *sql.DB) error {
	present, err := tableExists(ctx, db, "stellar_workspaces")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS stellar_dashboards (
  dashboard_id TEXT PRIMARY KEY,
  target TEXT NOT NULL,
  target_type TEXT NOT NULL,
  project TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT,
  pinned_metrics TEXT NOT NULL,
  sections TEXT NOT NULL,
  tags TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  created_by TEXT,
  UNIQUE (target, project, name)
)`,
		`INSERT OR IGNORE INTO stellar_dashboards(dashboard_id, target, target_type, project, name,
       description, pinned_metrics, sections, tags, is_default, created_at, updated_at, created_by)
SELECT workspace_id, target, target_type, project, name, description, pinned_metrics,
       sections, tags, is_default, created_at, updated_at, created_by
  FROM stellar_workspaces`,
		`DROP INDEX IF EXISTS idx_stellar_workspaces_target`,
		`DROP TABLE stellar_workspaces`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rename stellar_workspaces to stellar_dashboards: %w", err)
		}
	}
	return nil
}

func migrateV4ToV5(ctx context.Context, db *sql.DB) error {
	return nil
}
