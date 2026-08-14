// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/portalbin"

	_ "modernc.org/sqlite"
)

type Store struct {
	Root     string
	manifest Manifest
	db       *sql.DB
}

func Open(ctx context.Context, root string) (*Store, error) {
	root, err := ResolveRoot(ResolveOptions{Explicit: root})
	if err != nil {
		return nil, err
	}
	lock, err := acquireExistingStoreWriteLock(ctx, root)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	manifest, err := readManifest(root)
	if err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		return nil, err
	}
	s := &Store{Root: root, manifest: manifest, db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func Init(ctx context.Context, root string, opts InitOptions) (*Store, InitResult, error) {
	root, err := ResolveRoot(ResolveOptions{Explicit: root})
	if err != nil {
		return nil, InitResult{}, err
	}
	lock, err := acquireStoreWriteLock(ctx, root)
	if err != nil {
		return nil, InitResult{}, err
	}
	defer lock.release()
	if err := validateID("name", opts.Name); err != nil {
		return nil, InitResult{}, err
	}
	if opts.Project == "" {
		opts.Project = opts.Name
	}
	if err := validateID("project", opts.Project); err != nil {
		return nil, InitResult{}, err
	}
	if opts.Group == "" {
		opts.Group = "default"
	}
	if err := validateID("group", opts.Group); err != nil {
		return nil, InitResult{}, err
	}
	manifest, err := ensureStoreFiles(root, opts)
	if err != nil {
		return nil, InitResult{}, err
	}
	db, err := openDB(filepath.Join(root, manifest.Index))
	if err != nil {
		return nil, InitResult{}, err
	}
	s := &Store{Root: root, manifest: manifest, db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, InitResult{}, err
	}
	res, err := s.initMetadata(ctx, opts)
	if err != nil {
		db.Close()
		return nil, InitResult{}, err
	}
	res.StorePath = root
	res.Manifest = s.manifest
	return s, res, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Manifest() Manifest {
	return s.manifest
}

func (s *Store) withWriteLock(ctx context.Context, fn func() error) error {
	lock, err := acquireStoreWriteLock(ctx, s.Root)
	if err != nil {
		return err
	}
	err = fn()
	if releaseErr := lock.release(); releaseErr != nil {
		if err != nil {
			return fmt.Errorf("%w; release experiment store writer lock: %v", err, releaseErr)
		}
		return releaseErr
	}
	return err
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("initialize experiment schema: %w", err)
	}
	for _, migration := range additiveColumnMigrations {
		if err := ensureColumn(ctx, s.db, migration.table, migration.column, migration.definition); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", migration.table, migration.column, err)
		}
	}
	// Created after the additive column pass rather than in schemaSQL, because
	// schemaSQL runs first and indexing a column an older store has not gained
	// yet fails the whole open.
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_runs_experiment ON runs(experiment_id)`); err != nil {
		return fmt.Errorf("create idx_runs_experiment: %w", err)
	}
	if err := s.migrateArtifactsSchema(ctx); err != nil {
		return err
	}
	// Walk an older store forward to the current schema, then persist the new
	// version so the upgrade is not repeated on every open.
	migrated, err := applySchemaMigrations(ctx, s.db, s.manifest.SchemaVersion)
	if err != nil {
		return err
	}
	if err := s.writeManifestVersion(migrated); err != nil {
		return fmt.Errorf("record migrated experiment store schema: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT OR REPLACE INTO schema_meta(key, value) VALUES
  ('schema_version', ?),
  ('metric_schema_version', ?),
  ('metric_schema_columns', ?)
`, SchemaVersion, MetricSchemaVersion, strings.Join(MetricSchemaColumns, ","))
	if err != nil {
		return fmt.Errorf("record experiment schema metadata: %w", err)
	}
	return nil
}

var additiveColumnMigrations = []struct {
	table      string
	column     string
	definition string
}{
	// runs.experiment_id is the run -> experiment link. It lives here as well as
	// in schemaSQL so that a store predating it gains the column before the
	// index below is created.
	{"runs", "experiment_id", "experiment_id TEXT NOT NULL DEFAULT ''"},
	{"artifacts", "preview", "preview TEXT"},
	{"artifacts", "external_ref", "external_ref TEXT"},
	{"artifacts", "caption", "caption TEXT"},
	{"artifacts", "direction", "direction TEXT"},
	{"artifacts", "alias", "alias TEXT"},
	{"artifacts", "source_artifact_id", "source_artifact_id TEXT"},
	{"artifacts", "source_run_id", "source_run_id TEXT"},
	{"artifacts", "source_dataset_name", "source_dataset_name TEXT"},
	{"artifacts", "source_dataset_version", "source_dataset_version TEXT"},
	{"artifacts", "source_dataset_digest", "source_dataset_digest TEXT"},
	{"configs", "indexed_fields", "indexed_fields TEXT"},
	{"run_context", "runtime", "runtime TEXT"},
	{"run_context", "dependencies", "dependencies TEXT"},
	{"run_context", "log_uri", "log_uri TEXT"},
}

func ensureColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+definition)
	return err
}

func (s *Store) migrateArtifactsSchema(ctx context.Context) error {
	columns, err := s.tableColumns(ctx, "artifacts")
	if err != nil {
		return fmt.Errorf("inspect artifacts schema: %w", err)
	}
	migrations := []struct {
		name string
		sql  string
	}{
		{"durable_ref", "ALTER TABLE artifacts ADD COLUMN durable_ref TEXT"},
		{"content_type", "ALTER TABLE artifacts ADD COLUMN content_type TEXT"},
		{"step", "ALTER TABLE artifacts ADD COLUMN step INTEGER"},
		{"tags", "ALTER TABLE artifacts ADD COLUMN tags TEXT"},
		{"rank", "ALTER TABLE artifacts ADD COLUMN rank INTEGER"},
	}
	for _, migration := range migrations {
		if columns[migration.name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("migrate artifacts schema add %s: %w", migration.name, err)
		}
	}
	return nil
}

func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func (s *Store) initMetadata(ctx context.Context, opts InitOptions) (InitResult, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	requestHash, err := initRequestHash(opts)
	if err != nil {
		return InitResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InitResult{}, err
	}
	defer tx.Rollback()

	reused := false
	if opts.IdempotencyKey != "" {
		existingHash, err := idempotencyHash(ctx, tx, opts.IdempotencyKey)
		if err != nil {
			return InitResult{}, err
		}
		if existingHash != "" {
			if existingHash != requestHash {
				return InitResult{}, fmt.Errorf("%w: idempotency key %q was used for a different request", ErrConflict, opts.IdempotencyKey)
			}
			reused = true
		}
	}

	group, groupCreated, err := ensureRunGroup(ctx, tx, RunGroup{
		RunGroupID: opts.Group,
		Project:    opts.Project,
		Name:       opts.Group,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return InitResult{}, err
	}

	// The experiment is the named unit the researcher compares runs within, so
	// it is keyed on the name they passed to `experiment init --name`. Run
	// groups are arms/variants inside it (baseline vs ablation), not separate
	// experiments -- keying on the group would split an experiment into pieces
	// that can no longer be compared.
	experiment := ExperimentRecord{
		ExperimentID: opts.Name,
		Project:      opts.Project,
		Name:         opts.Name,
		Description:  opts.Description,
		Source:       "explicit",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	experimentCreated, err := ensureImplicitExperimentTx(ctx, tx, experiment)
	if err != nil {
		return InitResult{}, err
	}

	if opts.IdempotencyKey != "" && !reused {
		_, err := tx.ExecContext(ctx, `
INSERT INTO idempotency_keys(key, command, target_type, target_id, request_hash, created_at)
VALUES (?, 'exp init', 'experiment', ?, ?, ?)
`, opts.IdempotencyKey, opts.Name, requestHash, now)
		if err != nil {
			return InitResult{}, fmt.Errorf("record idempotency key: %w", err)
		}
	}

	cleanupMirrors, err := s.appendInitMirrors(experiment, experimentCreated, group, groupCreated)
	if err != nil {
		return InitResult{}, err
	}

	if err := tx.Commit(); err != nil {
		if cleanupErr := cleanupMirrors(); cleanupErr != nil {
			return InitResult{}, fmt.Errorf("commit experiment metadata: %w; cleanup JSONL mirrors: %v", err, cleanupErr)
		}
		return InitResult{}, err
	}

	return InitResult{
		Experiment: experiment,
		RunGroup:   group,
		Created:    groupCreated,
		Reused:     reused || !groupCreated,
	}, nil
}

func (s *Store) appendInitMirrors(experiment ExperimentRecord, experimentCreated bool, group RunGroup, groupCreated bool) (func() error, error) {
	var cleanups []func() error
	add := func(name string, record any) error {
		cleanup, err := s.appendJSONLWithRollback(name, record)
		if err != nil {
			if cleanupErr := cleanupJSONL(cleanups); cleanupErr != nil {
				return fmt.Errorf("%w; cleanup JSONL mirrors: %v", err, cleanupErr)
			}
			return err
		}
		cleanups = append(cleanups, cleanup)
		return nil
	}

	if experimentCreated {
		if err := add("experiments.jsonl", experiment); err != nil {
			return nil, err
		}
	}
	if groupCreated {
		if err := add("run_groups.jsonl", group); err != nil {
			return nil, err
		}
	}
	return func() error {
		return cleanupJSONL(cleanups)
	}, nil
}

func (s *Store) List(ctx context.Context, opts ListOptions) (QueryResult, error) {
	kind := opts.Kind
	if kind == "" {
		kind = "runs"
	}
	switch kind {
	case "runs":
		return s.listRuns(ctx, opts)
	case "groups", "run-groups":
		return s.listRunGroups(ctx, opts)
	case "experiments":
		return s.listExperiments(ctx, opts)
	default:
		return QueryResult{}, fmt.Errorf("--kind must be one of: runs, groups, experiments")
	}
}

func (s *Store) Query(ctx context.Context, query string) (QueryResult, error) {
	return s.QueryArgs(ctx, query)
}

func (s *Store) QueryArgs(ctx context.Context, query string, args ...any) (QueryResult, error) {
	clean, err := readOnlyQuery(query)
	if err != nil {
		return QueryResult{}, err
	}
	return s.queryRows(ctx, clean, args)
}

func (s *Store) Status(ctx context.Context, target string) (Status, error) {
	if strings.TrimSpace(target) == "" {
		return Status{}, fmt.Errorf("status target is required")
	}
	status := Status{
		StorePath:   s.Root,
		Target:      target,
		StateCounts: map[string]int{},
	}

	group, err := s.runGroup(ctx, target)
	if err == nil {
		status.TargetType = "run_group"
		status.RunGroup = &group
		if err := s.fillGroupStatus(ctx, &status, target); err != nil {
			return Status{}, err
		}
		return status, nil
	}
	if !errorsIsNotFound(err) {
		return Status{}, err
	}

	run, err := s.run(ctx, target)
	if err == nil {
		status.TargetType = "run"
		status.Run = run
		if err := s.fillRunStatus(ctx, &status, target); err != nil {
			return Status{}, err
		}
		return status, nil
	}
	if !errorsIsNotFound(err) {
		return Status{}, err
	}
	experiment, err := s.experiment(ctx, target)
	if err == nil {
		status.TargetType = "experiment"
		status.Experiment = &experiment
		// Counts must key off the resolved experiment id, not the user-supplied
		// target, which may be the experiment's human name.
		if err := s.fillExperimentStatus(ctx, &status, experiment.ExperimentID); err != nil {
			return Status{}, err
		}
		return status, nil
	}
	if !errorsIsNotFound(err) {
		return Status{}, err
	}
	return Status{}, fmt.Errorf("%w: %s", ErrNotFound, target)
}

func (s *Store) targetType(ctx context.Context, target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("status target is required")
	}
	if _, err := s.runGroup(ctx, target); err == nil {
		return "run_group", nil
	} else if !errorsIsNotFound(err) {
		return "", err
	}
	if _, err := s.run(ctx, target); err == nil {
		return "run", nil
	} else if !errorsIsNotFound(err) {
		return "", err
	}
	if _, err := s.experiment(ctx, target); err == nil {
		return "experiment", nil
	} else if !errorsIsNotFound(err) {
		return "", err
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, target)
}

func (s *Store) listRuns(ctx context.Context, opts ListOptions) (QueryResult, error) {
	cols := []string{"run_id", "project", "run_group_id", "state", "owner", "created_at", "completed_at", "result_uri"}
	where, args, err := listWhere(opts, "run")
	if err != nil {
		return QueryResult{}, err
	}
	return s.queryRows(ctx, "SELECT "+strings.Join(cols, ", ")+" FROM runs"+where+" ORDER BY created_at DESC, run_id", args)
}

func (s *Store) listRunGroups(ctx context.Context, opts ListOptions) (QueryResult, error) {
	if len(opts.Tags) > 0 || opts.State != "" {
		return QueryResult{}, fmt.Errorf("--tag and --state are only supported with --kind runs")
	}
	cols := []string{"run_group_id", "project", "name", "created_at", "updated_at"}
	where, args := metadataWhere(opts)
	return s.queryRows(ctx, "SELECT "+strings.Join(cols, ", ")+" FROM run_groups"+where+" ORDER BY created_at DESC, run_group_id", args)
}

func (s *Store) listExperiments(ctx context.Context, opts ListOptions) (QueryResult, error) {
	if opts.State != "" || opts.RunGroupID != "" {
		return QueryResult{}, fmt.Errorf("--state and --group are not supported with --kind experiments")
	}
	result, err := s.SearchExperiments(ctx, ExperimentSearchOptions{
		Project: opts.Project,
		Tags:    opts.Tags,
		Limit:   1000,
	})
	if err != nil {
		return QueryResult{}, err
	}
	cols := []string{"experiment_id", "name", "project", "source", "runs", "run_groups", "state_counts", "lifecycle_counts", "latest_run_at", "metric_names"}
	rows := make([]map[string]any, 0, len(result.Experiments))
	for _, experiment := range result.Experiments {
		rows = append(rows, map[string]any{
			"experiment_id":    experiment.ExperimentID,
			"name":             experiment.Name,
			"project":          experiment.Project,
			"source":           experiment.Source,
			"runs":             experiment.RunCount,
			"run_groups":       experiment.RunGroupCount,
			"state_counts":     formatCountMap(experiment.StateCounts),
			"lifecycle_counts": formatCountMap(experiment.LifecycleCounts),
			"latest_run_at":    experiment.LatestRunAt,
			"metric_names":     strings.Join(experiment.MetricNames, ","),
		})
	}
	return QueryResult{Columns: cols, Rows: rows}, nil
}

func listWhere(opts ListOptions, tagScope string) (string, []any, error) {
	clauses := []string{}
	args := []any{}
	if opts.Project != "" {
		clauses = append(clauses, "project = ?")
		args = append(args, opts.Project)
	}
	if opts.RunGroupID != "" {
		clauses = append(clauses, "run_group_id = ?")
		args = append(args, opts.RunGroupID)
	}
	if opts.State != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, opts.State)
	}
	for _, key := range sortedKeys(opts.Tags) {
		value := opts.Tags[key]
		clauses = append(clauses, `EXISTS (
  SELECT 1 FROM tags
  WHERE tags.scope_type = ? AND tags.scope_id = runs.run_id AND tags.key = ? AND tags.value = ?
)`)
		args = append(args, tagScope, key, value)
	}
	if len(clauses) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func metadataWhere(opts ListOptions) (string, []any) {
	clauses := []string{}
	args := []any{}
	if opts.Project != "" {
		clauses = append(clauses, "project = ?")
		args = append(args, opts.Project)
	}
	if opts.RunGroupID != "" {
		clauses = append(clauses, "run_group_id = ?")
		args = append(args, opts.RunGroupID)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func formatCountMap(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func (s *Store) fillGroupStatus(ctx context.Context, status *Status, groupID string) error {
	var err error
	status.RunGroups = 1
	if status.Runs, err = s.count(ctx, "SELECT count(*) FROM runs WHERE run_group_id = ?", groupID); err != nil {
		return err
	}
	if status.Configs, err = s.count(ctx, "SELECT count(*) FROM configs WHERE run_id IN (SELECT run_id FROM runs WHERE run_group_id = ?)", groupID); err != nil {
		return err
	}
	if status.MetricFiles, err = s.count(ctx, "SELECT count(*) FROM metric_files WHERE run_group_id = ?", groupID); err != nil {
		return err
	}
	if status.Artifacts, err = s.count(ctx, "SELECT count(*) FROM artifacts WHERE run_id IN (SELECT run_id FROM runs WHERE run_group_id = ?)", groupID); err != nil {
		return err
	}
	if status.Observations, err = s.count(ctx, "SELECT count(*) FROM observations WHERE (scope_type = 'run_group' AND scope_id = ?) OR scope_id IN (SELECT run_id FROM runs WHERE run_group_id = ?)", groupID, groupID); err != nil {
		return err
	}
	if status.LatestEventAt, err = s.scalarString(ctx, "SELECT max(time) FROM events WHERE run_id IN (SELECT run_id FROM runs WHERE run_group_id = ?)", groupID); err != nil {
		return err
	}
	if err := s.fillStateCounts(ctx, status, "WHERE run_group_id = ?", groupID); err != nil {
		return err
	}
	return s.fillLifecycleCounts(ctx, status, "WHERE r.run_group_id = ?", groupID)
}

func (s *Store) fillRunStatus(ctx context.Context, status *Status, runID string) error {
	var err error
	status.Runs = 1
	if status.Configs, err = s.count(ctx, "SELECT count(*) FROM configs WHERE run_id = ?", runID); err != nil {
		return err
	}
	if status.MetricFiles, err = s.count(ctx, "SELECT count(*) FROM metric_files WHERE run_id = ?", runID); err != nil {
		return err
	}
	if status.Artifacts, err = s.count(ctx, "SELECT count(*) FROM artifacts WHERE run_id = ?", runID); err != nil {
		return err
	}
	if status.Observations, err = s.count(ctx, "SELECT count(*) FROM observations WHERE scope_id = ?", runID); err != nil {
		return err
	}
	if status.LatestEventAt, err = s.scalarString(ctx, "SELECT max(time) FROM events WHERE run_id = ?", runID); err != nil {
		return err
	}
	if err := s.fillStateCounts(ctx, status, "WHERE run_id = ?", runID); err != nil {
		return err
	}
	return s.fillLifecycleCounts(ctx, status, "WHERE r.run_id = ?", runID)
}

func (s *Store) fillExperimentStatus(ctx context.Context, status *Status, experimentID string) error {
	var err error
	runSubquery := "SELECT run_id FROM runs WHERE experiment_id = ?"
	if status.Runs, err = s.count(ctx, "SELECT count(*) FROM runs WHERE experiment_id = ?", experimentID); err != nil {
		return err
	}
	// An experiment's arms are the distinct groups its runs sit in. run_groups
	// no longer carries an experiment_id, so this is the only path.
	if status.RunGroups, err = s.count(ctx,
		"SELECT count(*) FROM (SELECT DISTINCT run_group_id FROM runs WHERE run_id IN ("+runSubquery+"))",
		experimentID); err != nil {
		return err
	}
	if status.Configs, err = s.count(ctx, "SELECT count(*) FROM configs WHERE run_id IN ("+runSubquery+")", experimentID); err != nil {
		return err
	}
	if status.MetricFiles, err = s.count(ctx, "SELECT count(*) FROM metric_files WHERE run_id IN ("+runSubquery+")", experimentID); err != nil {
		return err
	}
	if status.Artifacts, err = s.count(ctx, "SELECT count(*) FROM artifacts WHERE run_id IN ("+runSubquery+")", experimentID); err != nil {
		return err
	}
	if status.Observations, err = s.count(ctx, "SELECT count(*) FROM observations WHERE (scope_type = 'experiment' AND scope_id = ?) OR scope_id IN ("+runSubquery+")", experimentID, experimentID); err != nil {
		return err
	}
	if status.LatestEventAt, err = s.scalarString(ctx, "SELECT max(time) FROM events WHERE run_id IN ("+runSubquery+")", experimentID); err != nil {
		return err
	}
	if err := s.fillStateCounts(ctx, status, "WHERE run_id IN ("+runSubquery+")", experimentID); err != nil {
		return err
	}
	return s.fillLifecycleCounts(ctx, status, "WHERE r.run_id IN ("+runSubquery+")", experimentID)
}

func (s *Store) fillStateCounts(ctx context.Context, status *Status, where string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, "SELECT state, count(*) FROM runs "+where+" GROUP BY state ORDER BY state", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return err
		}
		status.StateCounts[state] = count
	}
	return rows.Err()
}

func (s *Store) fillLifecycleCounts(ctx context.Context, status *Status, where string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT lifecycle_state, count(*) FROM (
  SELECT CASE
    WHEN r.state_norm IN ('pending', 'running') THEN r.state_norm
    WHEN r.state_norm = 'failed' THEN 'failed'
    WHEN r.state_norm IN ('succeeded', 'completed') THEN
      CASE WHEN `+lifecycleFailureSQL()+` THEN 'incomplete' ELSE 'succeeded' END
    WHEN trim(coalesce(r.completed_at, '')) = '' THEN 'pending'
    ELSE 'incomplete'
  END AS lifecycle_state
  FROM (
    SELECT r.run_id, r.completed_at, lower(trim(coalesce(r.state, ''))) AS state_norm
    FROM runs r
    `+where+`
  ) r
)
GROUP BY lifecycle_state
ORDER BY lifecycle_state`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var lifecycle string
		var count int
		if err := rows.Scan(&lifecycle, &count); err != nil {
			return err
		}
		counts[lifecycle] = count
	}
	if err := rows.Err(); err != nil {
		return err
	}
	status.LifecycleCounts = counts
	return nil
}

func lifecycleFailureSQL() string {
	failureMetricClauses := make([]string, 0, len(failureMetricTokens))
	for _, token := range failureMetricTokens {
		failureMetricClauses = append(failureMetricClauses, "lower(ms.metric_name) LIKE '%"+token+"%'")
	}
	return `
EXISTS (
  SELECT 1 FROM metric_summaries ms
  WHERE ms.run_id = r.run_id AND ms.non_finite_count > 0
) OR EXISTS (
  SELECT 1 FROM metric_summaries ms
  WHERE ms.run_id = r.run_id AND ms.finite_count > 0 AND ms.max_value > 0
    AND (` + strings.Join(failureMetricClauses, " OR ") + `)
) OR EXISTS (
  SELECT 1 FROM tags t
  WHERE t.scope_type = 'run'
    AND t.scope_id = r.run_id
    AND t.key IN ('tau.success.min_step', 'success.min_step', 'expected_max_step', 'max_step')
    AND trim(t.value) GLOB '[0-9]*'
    AND trim(t.value) NOT GLOB '*[^0-9]*'
    AND CAST(trim(t.value) AS INTEGER) > coalesce((
      SELECT max(ms.max_step) FROM metric_summaries ms WHERE ms.run_id = r.run_id
    ), -9223372036854775808)
)`
}

func (s *Store) runGroup(ctx context.Context, id string) (RunGroup, error) {
	var g RunGroup
	err := s.db.QueryRowContext(ctx, `
SELECT run_group_id, project, name, created_at, updated_at
FROM run_groups WHERE run_group_id = ?`, id).Scan(&g.RunGroupID, &g.Project, &g.Name, &g.CreatedAt, &g.UpdatedAt)
	if err == sql.ErrNoRows {
		return RunGroup{}, ErrNotFound
	}
	return g, err
}

func (s *Store) run(ctx context.Context, id string) (map[string]any, error) {
	result, err := s.queryRows(ctx, `
SELECT run_id, project, run_group_id, parent_run_id, state, owner,
       created_at, started_at, completed_at, config_hash, code_sha, image_digest, tau_command,
       result_uri, index_version
FROM runs WHERE run_id = ?`, []any{id})
	if err != nil {
		return nil, err
	}
	if len(result.Rows) == 0 {
		return nil, ErrNotFound
	}
	return result.Rows[0], nil
}

// experiment resolves by experiment_id, falling back to name. Under the
// workspace -> project -> experiment -> run model the experiment id is the run
// group, but researchers routinely refer to an experiment by the human name
// they passed to `experiment init --name`, so both must resolve.
func (s *Store) experiment(ctx context.Context, id string) (ExperimentRecord, error) {
	var exp ExperimentRecord
	err := s.db.QueryRowContext(ctx, `
SELECT experiment_id, project, name, coalesce(description, ''), source, created_at, updated_at
FROM experiments WHERE experiment_id = ? OR name = ?
ORDER BY (experiment_id = ?) DESC LIMIT 1`, id, id, id).Scan(
		&exp.ExperimentID,
		&exp.Project,
		&exp.Name,
		&exp.Description,
		&exp.Source,
		&exp.CreatedAt,
		&exp.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return ExperimentRecord{}, ErrNotFound
	}
	return exp, err
}

func (s *Store) count(ctx context.Context, query string, args ...any) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) scalarString(ctx context.Context, query string, args ...any) (string, error) {
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
		return "", err
	}
	if !value.Valid {
		return "", nil
	}
	return value.String, nil
}

func (s *Store) queryRows(ctx context.Context, query string, args []any) (QueryResult, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}
	result := QueryResult{Columns: cols}
	for rows.Next() {
		values := make([]any, len(cols))
		scans := make([]any, len(cols))
		for i := range values {
			scans[i] = &values[i]
		}
		if err := rows.Scan(scans...); err != nil {
			return QueryResult{}, err
		}
		row := map[string]any{}
		for i, col := range cols {
			row[col] = normalizeDBValue(values[i])
		}
		result.Rows = append(result.Rows, row)
	}
	return result, rows.Err()
}

func normalizeDBValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return t
	}
}

func ensureRunGroup(ctx context.Context, tx *sql.Tx, g RunGroup) (RunGroup, bool, error) {
	var existing RunGroup
	err := tx.QueryRowContext(ctx, `
SELECT run_group_id, project, name, created_at, updated_at
FROM run_groups WHERE run_group_id = ?`, g.RunGroupID).Scan(&existing.RunGroupID, &existing.Project, &existing.Name, &existing.CreatedAt, &existing.UpdatedAt)
	if err == nil {
		if existing.Project != g.Project || existing.Name != g.Name {
			return RunGroup{}, false, fmt.Errorf("%w: run group %q already exists with different metadata", ErrConflict, g.RunGroupID)
		}
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return RunGroup{}, false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO run_groups(run_group_id, project, name, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`, g.RunGroupID, g.Project, g.Name, g.CreatedAt, g.UpdatedAt)
	return g, true, err
}

func idempotencyHash(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var hash string
	err := tx.QueryRowContext(ctx, "SELECT request_hash FROM idempotency_keys WHERE key = ?", key).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

func initRequestHash(opts InitOptions) (string, error) {
	payload := map[string]string{
		"name":        opts.Name,
		"project":     opts.Project,
		"description": opts.Description,
		"group":       opts.Group,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func readOnlyQuery(query string) (string, error) {
	clean := strings.TrimSpace(query)
	if clean == "" {
		return "", fmt.Errorf("query is required")
	}
	clean = strings.TrimRight(clean, " \t\r\n;")
	if strings.Contains(clean, ";") {
		return "", fmt.Errorf("%s sql accepts exactly one read-only statement", portalbin.ExperimentCmd)
	}
	lower := strings.ToLower(strings.TrimSpace(clean))
	if !strings.HasPrefix(lower, "select ") && !strings.HasPrefix(lower, "with ") {
		return "", fmt.Errorf("%s sql only accepts read-only SELECT/WITH queries", portalbin.ExperimentCmd)
	}
	for _, denied := range []string{"insert ", "update ", "delete ", "drop ", "alter ", "create ", "replace ", "attach ", "detach ", "pragma ", "vacuum"} {
		if strings.Contains(lower, denied) {
			return "", fmt.Errorf("%s sql rejected mutating or unsafe token %q", portalbin.ExperimentCmd, strings.TrimSpace(denied))
		}
	}
	return clean, nil
}

func ensureStoreFiles(root string, opts InitOptions) (Manifest, error) {
	if err := os.MkdirAll(filepath.Join(root, AppendLogDir), 0o755); err != nil {
		return Manifest{}, err
	}
	for _, dir := range []string{MetricsDir, ArtifactsDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return Manifest{}, err
		}
	}
	manifestPath := filepath.Join(root, ManifestFile)
	if _, err := os.Stat(manifestPath); err == nil {
		return readManifest(root)
	} else if err != nil && !os.IsNotExist(err) {
		return Manifest{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Kind:          StoreKind,
		Project:       opts.Project,
		ExperimentID:  opts.Name,
		CreatedAt:     now,
		UpdatedAt:     now,
		Index:         IndexFile,
		AppendLogDir:  AppendLogDir,
		MetricsDir:    MetricsDir,
		ArtifactsDir:  ArtifactsDir,
	}
	if err := fileutil.WriteJSONFileAtomic(manifestPath, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func readManifest(root string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(root, ManifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, fmt.Errorf("experiment store %s has no %s", root, ManifestFile)
		}
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", ManifestFile, err)
	}
	if !isKnownSchemaVersion(manifest.SchemaVersion) {
		return Manifest{}, fmt.Errorf("unsupported experiment store schema %q", manifest.SchemaVersion)
	}
	if manifest.Kind != StoreKind {
		return Manifest{}, fmt.Errorf("unsupported experiment store kind %q", manifest.Kind)
	}
	return defaultManifestPaths(manifest), nil
}

func defaultManifestPaths(manifest Manifest) Manifest {
	manifest.Index = cmp.Or(manifest.Index, IndexFile)
	manifest.AppendLogDir = cmp.Or(manifest.AppendLogDir, AppendLogDir)
	manifest.MetricsDir = cmp.Or(manifest.MetricsDir, MetricsDir)
	manifest.ArtifactsDir = cmp.Or(manifest.ArtifactsDir, ArtifactsDir)
	return manifest
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

const (
	storeLockFile    = ".tau-exp.lock"
	storeLockPoll    = 250 * time.Millisecond
	storeLockTimeout = 10 * time.Second
)

type storeWriteLock struct {
	file *os.File
}

func acquireExistingStoreWriteLock(ctx context.Context, root string) (*storeWriteLock, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("experiment store root %s is not a directory", root)
	}
	return acquireStoreWriteLock(ctx, root)
}

func acquireStoreWriteLock(ctx context.Context, root string) (*storeWriteLock, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, storeLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	timeout := time.NewTimer(storeLockTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(storeLockPoll)
	defer ticker.Stop()
	for {
		err := tryFileLock(f)
		if err == nil {
			return &storeWriteLock{file: f}, nil
		}
		if !isLockBusy(err) {
			return nil, errors.Join(fmt.Errorf("lock experiment store %s: %w", root, err), f.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("lock experiment store %s: %w", root, ctx.Err()), f.Close())
		case <-timeout.C:
			return nil, errors.Join(fmt.Errorf("%w: timed out waiting for experiment store writer lock %s", ErrConflict, path), f.Close())
		case <-ticker.C:
		}
	}
}

func (l *storeWriteLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := releaseFileLock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

func (s *Store) appendJSONLWithRollback(name string, record any) (func() error, error) {
	path := filepath.Join(s.Root, s.manifest.AppendLogDir, name)
	existed := true
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		existed = false
	} else if info.IsDir() {
		return nil, fmt.Errorf("append JSONL mirror %s: path is a directory", path)
	}
	var offset int64
	if existed {
		offset = info.Size()
	}

	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		if !existed {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return os.Truncate(path, offset)
	}
	if _, err := f.Write(raw); err != nil {
		err = errors.Join(err, f.Close())
		_ = cleanup()
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = cleanup()
		return nil, err
	}
	return cleanup, nil
}

func cleanupJSONL(cleanups []func() error) error {
	var first error
	for i := len(cleanups) - 1; i >= 0; i-- {
		if err := cleanups[i](); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func cleanRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("--store is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func validateID(kind, value string) error {
	return exptelemetry.ValidateID(kind, value)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func errorsIsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
