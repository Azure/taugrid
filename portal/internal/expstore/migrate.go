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

// Schema evolution for the local experiment store.
//
// Before this existed, `migrate` could only ever *add* columns
// (see additiveColumnMigrations) and `readManifest` rejected any store whose
// schema_version differed from the current constant. That combination meant a
// version bump did not upgrade an existing store -- it made the store
// unopenable. Anything that needed to remove a column or a table therefore had
// no safe path at all.
//
// schemaMigrations closes that gap: it is an ordered chain of steps, each
// moving a store from one schema version to the next. `readManifest` now
// accepts any version in the chain, and `migrate` walks the store forward to
// the current version and rewrites manifest.json when it lands.
//
// Rules for adding a migration:
//   - Append to schemaMigrations; never reorder or edit a released entry.
//   - `to` must equal the `from` of the next entry, and the final `to` must
//     equal SchemaVersion.
//   - `apply` must be idempotent. Fresh stores are created directly at
//     SchemaVersion by schemaSQL and skip the chain entirely, so a step can be
//     re-entered after a partial failure. Use dropColumnIfExists rather than a
//     bare ALTER ... DROP COLUMN, which errors when the column is already gone.
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

// knownSchemaVersions returns every schema version this binary can open: the
// origin of each migration plus the current version.
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

// applySchemaMigrations walks a store from `current` up to SchemaVersion.
// It returns the version the store ended on.
func applySchemaMigrations(ctx context.Context, db *sql.DB, current string) (string, error) {
	if current == "" {
		// A store with no recorded version predates versioning; treat it as the
		// oldest schema we know how to migrate.
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

// writeManifestVersion persists a migrated schema version to manifest.json so
// the upgrade survives the process that performed it.
func (s *Store) writeManifestVersion(version string) error {
	if s.manifest.SchemaVersion == version {
		return nil
	}
	s.manifest.SchemaVersion = version
	s.manifest.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return fileutil.WriteJSONFileAtomic(filepath.Join(s.Root, ManifestFile), s.manifest)
}

// columnExists reports whether table.column is present. Used to keep
// destructive migrations idempotent.
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

// dropColumnIfExists removes a column when present. A bare
// "ALTER TABLE ... DROP COLUMN" errors if the column is already gone, which
// would make re-running a migration fail.
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

// migrateV0ToV1 removes the `hypothesis` axis.
//
// Nothing on any `tau run` submit path ever set a hypothesis: it was reachable
// only through `taugrid-portal experiment init --hypothesis`. Every
// Tau-submitted run therefore carried an empty hypothesis_id, while the column
// still participated in metric grouping keys and the ADX projection. The axis
// is removed rather than left dormant so aggregations stop carrying a column
// that is empty by construction.
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

// migrateV1ToV2 removes the question axis by promoting it to the experiment.
//
// `question` was not a distinct level: core/experiment/runmetadata.go set
// ExperimentID directly from QuestionID, and EnsureExperimentIndex backfilled
// one experiment per question. A question *was* an experiment under another
// name. Keeping both meant the Kusto listing key was
// (project_id, question_id, workspace_id) when (project_id, workspace_id) says
// the same thing. The identity model is now workspace -> project ->
// experiment -> run, with run_group demoted to an arm/variant label on a run
// rather than a level of its own.
//
// Two things are preserved rather than dropped:
//   - Question rows become experiment rows, so the named unit researchers
//     compare runs within survives the rename.
//   - runs.question_id was the run -> named-experiment link. It is replayed
//     into run_experiments *before* the column is dropped, so the association
//     survives as the single M:N link instead of being lost.
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

	// Promote every question to an experiment. INSERT OR IGNORE so a question
	// that already backfilled into experiments (the pre-migration
	// EnsureExperimentIndex did exactly this) is not duplicated or clobbered.
	if questionsPresent {
		if _, err := db.ExecContext(ctx, `
INSERT OR IGNORE INTO experiments(experiment_id, project, name, description, source, created_at, updated_at)
SELECT q.question_id, q.project, q.question_id, nullif(q.question_text, ''), 'question',
       coalesce(q.created_at, ?), coalesce(q.updated_at, ?)
FROM questions q`, now, now); err != nil {
			return fmt.Errorf("promote questions to experiments: %w", err)
		}
	}

	// Preserve the prose for experiments that backfilled before this migration
	// and therefore have no description. Only fill empty ones so a hand-written
	// description is never clobbered on re-entry.
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

	// run_groups gains experiment_id: an arm belongs to exactly one experiment.
	// Backfill it from the runs in the group so existing arms keep their parent.
	if err := addColumnIfMissing(ctx, db, "run_groups", "experiment_id", "TEXT"); err != nil {
		return err
	}

	// Replay the run -> named-experiment link into run_experiments before the
	// column carrying it is dropped. Without this the association is lost and
	// every run silently falls back to its run_group, which would reintroduce
	// the extra level this migration exists to remove.
	runsHaveQuestion, err := columnExists(ctx, db, "runs", "question_id")
	if err != nil {
		return err
	}
	if runsHaveQuestion {
		// v2 stored this link in run_experiments. schemaSQL no longer creates
		// that table (v3 replaced it with runs.experiment_id), so this step has
		// to create what it writes into: a migration must not depend on the
		// current schema, only on the schema its `from` version had.
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
		// Derive each arm's parent experiment from the runs it holds. A group
		// whose runs disagree is not expected; pick the lowest id so the result
		// is deterministic rather than dependent on scan order.
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

	// stellar_workspaces carries UNIQUE (target, project, question_id, name).
	// SQLite refuses ALTER TABLE ... DROP COLUMN on a column bound by a table
	// constraint, so this one needs a rebuild rather than a drop.
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

// rebuildStellarWorkspacesWithoutQuestion recreates stellar_workspaces without
// question_id. Collapsing UNIQUE (target, project, question_id, name) to
// UNIQUE (target, project, name) can merge rows that previously differed only
// by question, so the copy keeps the most recently updated row per surviving
// key instead of failing the migration on a constraint violation.
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

// migrateV2ToV3 makes runs.experiment_id the run -> experiment link.
//
// v2 modelled the link as a run_experiments join table, which allowed a run to
// sit under several experiments at once. The identity hierarchy is
// workspace -> project -> experiment -> run, so a run has exactly one
// experiment; this collapses the join table into a column on runs.
//
// Where a run does have several experiments, the surviving one is the most
// specific: an explicitly named experiment beats one synthesized from a run
// group. Ties break on the lowest id so the outcome does not depend on scan
// order.
//
// run_groups.experiment_id also goes, because with runs pointing at their
// experiment directly it is a second, redundant path to the same answer, and
// two paths can disagree.
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

	// Runs that never got an experiment fall back to their arm, so that "every
	// run belongs to an experiment" holds after the migration too.
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

// migrateV3ToV4 moves saved dashboard layouts off the stellar_workspaces name.
// "Workspace" now means only the tenancy boundary (the tau_workspace tag); a
// saved set of pinned metrics and section state is a dashboard.
//
// This compatibility step copies rather than renames. A store may have already
// created the v4 destination table before the migration runs.
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

// migrateV4ToV5 retires mutable Stellar UI state without deleting it. New code
// no longer reads or writes these tables, but preserving existing rows keeps an
// ordinary Open reversible during the API deprecation window.
func migrateV4ToV5(ctx context.Context, db *sql.DB) error {
	return nil
}
