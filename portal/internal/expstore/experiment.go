// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const ExperimentSearchSchemaVersion = "tau.exp.experiment_search.v0"

const (
	experimentTagID   = "tau.experiment.id"
	experimentTagName = "tau.experiment.name"
)

type ExperimentIndexResult struct {
	Experiments int      `json:"experiments"`
	Assignments int      `json:"assignments"`
	Warnings    []string `json:"warnings,omitempty"`
}

func (s *Store) EnsureExperimentIndex(ctx context.Context) (ExperimentIndexResult, error) {
	var result ExperimentIndexResult
	lock, err := acquireStoreWriteLock(ctx, s.Root)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			result.Warnings = append(result.Warnings, "experiment index backfill skipped because the experiment store writer lock is busy")
			return result, nil
		}
		return result, err
	}
	defer lock.release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT g.run_group_id, g.project, g.name, g.created_at, g.updated_at
FROM run_groups g
WHERE EXISTS (
  SELECT 1 FROM runs r
  WHERE r.run_group_id = g.run_group_id AND r.experiment_id = ''
)
ORDER BY g.run_group_id`)
	if err != nil {
		return result, err
	}
	groups := []RunGroup{}
	for rows.Next() {
		var g RunGroup
		if err := rows.Scan(&g.RunGroupID, &g.Project, &g.Name, &g.CreatedAt, &g.UpdatedAt); err != nil {
			rows.Close()
			return result, err
		}
		groups = append(groups, g)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for _, g := range groups {
		created, err := ensureImplicitExperimentTx(ctx, tx, ExperimentRecord{
			ExperimentID: g.RunGroupID,
			Project:      g.Project,
			Name:         firstNonEmptyExperimentName(g.Name, g.RunGroupID),
			Source:       "run_group",
			CreatedAt:    g.CreatedAt,
			UpdatedAt:    g.UpdatedAt,
		})
		if err != nil {
			return result, err
		}
		if created {
			result.Experiments++
		}
	}
	assignments, err := ensureRunGroupExperimentAssignmentsTx(ctx, tx)
	if err != nil {
		return result, err
	}
	result.Assignments = assignments
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) AssignRunToExperiment(ctx context.Context, experiment ExperimentRecord, runID string) error {
	if err := validateID("run", runID); err != nil {
		return err
	}
	lock, err := acquireStoreWriteLock(ctx, s.Root)
	if err != nil {
		return err
	}
	defer lock.release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var runProject string
	err = tx.QueryRowContext(ctx, `SELECT project FROM runs WHERE run_id = ?`, runID).Scan(&runProject)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if experiment.Project == "" {
		experiment.Project = runProject
	}
	if experiment.CreatedAt == "" {
		experiment.CreatedAt = now
	}
	if experiment.UpdatedAt == "" {
		experiment.UpdatedAt = now
	}
	if experiment.Source == "" {
		experiment.Source = "explicit"
	}
	if err := upsertExperimentTx(ctx, tx, experiment); err != nil {
		return err
	}
	if err := setRunExperimentTx(ctx, tx, experiment.ExperimentID, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SearchExperiments(ctx context.Context, opts ExperimentSearchOptions) (ExperimentSearchResult, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	opts.Project = strings.TrimSpace(opts.Project)
	opts.CursorAt = strings.TrimSpace(opts.CursorAt)
	opts.CursorID = strings.TrimSpace(opts.CursorID)
	opts.Lifecycle = normalizeLifecycle(opts.Lifecycle)
	if opts.Limit < 0 {
		return ExperimentSearchResult{}, fmt.Errorf("limit must be non-negative")
	}
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	indexResult, err := s.EnsureExperimentIndex(ctx)
	if err != nil {
		return ExperimentSearchResult{}, err
	}
	metricBackfill, err := s.EnsureMetricSummaries(ctx)
	if err != nil {
		return ExperimentSearchResult{}, err
	}
	experiments, err := s.experimentCandidates(ctx, opts)
	if err != nil {
		return ExperimentSearchResult{}, err
	}
	summaries := make([]ExperimentSummary, 0, len(experiments))
	for _, experiment := range experiments {
		summary, err := s.experimentSummary(ctx, experiment, opts)
		if err != nil {
			return ExperimentSearchResult{}, err
		}
		if opts.Lifecycle != "" && summary.LifecycleCounts[opts.Lifecycle] == 0 {
			continue
		}
		summaries = append(summaries, summary)
		if len(summaries) > opts.Limit {
			break
		}
	}
	truncated := len(summaries) > opts.Limit
	if truncated {
		summaries = summaries[:opts.Limit]
	}
	warnings := append([]string{}, indexResult.Warnings...)
	warnings = append(warnings, metricBackfill.Warnings...)
	return ExperimentSearchResult{
		SchemaVersion: ExperimentSearchSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     s.Root,
		Total:         len(summaries),
		Truncated:     truncated,
		Experiments:   summaries,
		Warnings:      warnings,
	}, nil
}

func (s *Store) experimentCandidates(ctx context.Context, opts ExperimentSearchOptions) ([]ExperimentRecord, error) {
	with := `WITH workspace_run_experiments AS (
  SELECT re.experiment_id, re.run_id
  FROM runs re
  WHERE re.experiment_id != '' AND (? = '' OR EXISTS (
    SELECT 1 FROM tags workspace_tag
    WHERE workspace_tag.scope_type = 'run'
      AND workspace_tag.scope_id = re.run_id
      AND workspace_tag.key = ?
      AND workspace_tag.value = ?
  ) OR NOT EXISTS (
    SELECT 1 FROM tags workspace_tag
    WHERE workspace_tag.scope_type = 'run'
      AND workspace_tag.scope_id = re.run_id
      AND workspace_tag.key = ?
  ))
)
`
	clauses := []string{"(? = '' OR EXISTS (SELECT 1 FROM workspace_run_experiments scoped_re WHERE scoped_re.experiment_id = e.experiment_id))"}
	args := []any{opts.Workspace, exptelemetry.TauWorkspaceTag, opts.Workspace, exptelemetry.TauWorkspaceTag, opts.Workspace}
	if opts.Project != "" {
		clauses = append(clauses, "e.project = ?")
		args = append(args, opts.Project)
	}
	if opts.Query != "" {
		like := "%" + strings.ToLower(opts.Query) + "%"
		clauses = append(clauses, `(lower(e.experiment_id) LIKE ? OR lower(e.name) LIKE ? OR lower(coalesce(e.description, '')) LIKE ? OR lower(e.project) LIKE ? OR EXISTS (
  SELECT 1 FROM workspace_run_experiments re
  JOIN tags t ON t.scope_type = 'run' AND t.scope_id = re.run_id
  WHERE re.experiment_id = e.experiment_id AND (lower(t.key) LIKE ? OR lower(t.value) LIKE ?)
) OR EXISTS (
  SELECT 1 FROM workspace_run_experiments re
  JOIN metric_summaries ms ON ms.run_id = re.run_id
  WHERE re.experiment_id = e.experiment_id AND lower(ms.metric_name) LIKE ?
))`)
		args = append(args, like, like, like, like, like, like, like)
	}
	for _, key := range sortedKeys(opts.Tags) {
		clauses = append(clauses, `EXISTS (
  SELECT 1 FROM workspace_run_experiments re
  JOIN tags t ON t.scope_type = 'run' AND t.scope_id = re.run_id
  WHERE re.experiment_id = e.experiment_id AND t.key = ? AND t.value = ?
)`)
		args = append(args, key, opts.Tags[key])
	}
	for _, metricName := range opts.MetricNames {
		metricName = strings.TrimSpace(metricName)
		if metricName == "" {
			continue
		}
		clauses = append(clauses, `EXISTS (
  SELECT 1 FROM workspace_run_experiments re
  JOIN metric_summaries ms ON ms.run_id = re.run_id
  WHERE re.experiment_id = e.experiment_id AND ms.metric_name = ?
)`)
		args = append(args, metricName)
	}
	for _, filter := range opts.MetricFilters {
		column, ok := metricFilterFieldColumn(filter.Field)
		if !ok {
			return nil, fmt.Errorf("unsupported metric filter field %q", filter.Field)
		}
		if strings.TrimSpace(filter.MetricName) == "" {
			return nil, fmt.Errorf("metric filter metric name is required")
		}
		clauses = append(clauses, fmt.Sprintf(`EXISTS (
  SELECT 1 FROM workspace_run_experiments re
  JOIN metric_summaries ms ON ms.run_id = re.run_id
  WHERE re.experiment_id = e.experiment_id AND ms.metric_name = ? AND %s %s ?
)`, column, sqliteMetricOperator(filter.Op)))
		args = append(args, filter.MetricName, filter.Value)
	}
	if opts.Since != "" {
		since, err := normalizeSince(opts.Since, time.Now())
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, `(e.updated_at >= ? OR EXISTS (
  SELECT 1 FROM workspace_run_experiments re
  JOIN runs r ON r.run_id = re.run_id
  WHERE re.experiment_id = e.experiment_id AND coalesce(nullif(r.started_at, ''), nullif(r.completed_at, ''), r.created_at) >= ?
))`)
		args = append(args, since, since)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	limit := opts.Limit + 1
	if opts.Lifecycle != "" {
		limit = 1000
	}
	having := ""
	if opts.CursorAt != "" || opts.CursorID != "" {
		project, experimentID, err := searchCursorParts(opts.CursorAt, opts.CursorID)
		if err != nil {
			return nil, err
		}
		cursorAt := "coalesce(max(coalesce(nullif(r.started_at, ''), nullif(r.completed_at, ''), r.created_at) COLLATE tau_rfc3339_nano), e.updated_at) COLLATE tau_rfc3339_nano"
		having = fmt.Sprintf(`
HAVING %s < ? COLLATE tau_rfc3339_nano OR (%s = ? COLLATE tau_rfc3339_nano AND (e.project > ? OR (e.project = ? AND e.experiment_id > ?)))`,
			cursorAt, cursorAt)
		args = append(args, opts.CursorAt, opts.CursorAt, project, project, experimentID)
	}
	rows, err := s.db.QueryContext(ctx, with+`
SELECT e.experiment_id, e.project, e.name, coalesce(e.description, ''), e.source, e.created_at, e.updated_at
FROM experiments e
LEFT JOIN workspace_run_experiments re ON re.experiment_id = e.experiment_id
LEFT JOIN runs r ON r.run_id = re.run_id
`+where+`
GROUP BY e.experiment_id, e.project, e.name, e.description, e.source, e.created_at, e.updated_at
`+having+`
ORDER BY coalesce(max(coalesce(nullif(r.started_at, ''), nullif(r.completed_at, ''), r.created_at) COLLATE tau_rfc3339_nano), e.updated_at) COLLATE tau_rfc3339_nano DESC, e.project ASC, e.experiment_id ASC
LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExperimentRecord{}
	for rows.Next() {
		var exp ExperimentRecord
		if err := rows.Scan(&exp.ExperimentID, &exp.Project, &exp.Name, &exp.Description, &exp.Source, &exp.CreatedAt, &exp.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, exp)
	}
	return out, rows.Err()
}

func (s *Store) experimentSummary(ctx context.Context, experiment ExperimentRecord, opts ExperimentSearchOptions) (ExperimentSummary, error) {
	runs, err := s.runRecordsWhere(ctx, "WHERE r.experiment_id = ?", []any{experiment.ExperimentID}, 0)
	if err != nil {
		return ExperimentSummary{}, err
	}
	runIDs := runRecordIDs(runs)
	tags, err := s.RunTags(ctx, runIDs)
	if err != nil {
		return ExperimentSummary{}, err
	}
	if opts.Workspace != "" {
		scoped := runs[:0]
		for _, run := range runs {
			if runWorkspace := tags[run.RunID][exptelemetry.TauWorkspaceTag]; runWorkspace == opts.Workspace || runWorkspace == "" {
				scoped = append(scoped, run)
			}
		}
		runs = scoped
		runIDs = runRecordIDs(runs)
	}
	metricSummaries, err := s.metricSummariesByRun(ctx, runIDs)
	if err != nil {
		return ExperimentSummary{}, err
	}
	stateCounts := map[string]int{}
	lifecycleCounts := map[string]int{}
	metricNames := map[string]bool{}
	groupIDs := map[string]bool{}
	latestRunAt := ""
	for _, run := range runs {
		stateCounts[normalizeRunState(run.State)]++
		groupIDs[run.RunGroupID] = true
		if candidate := latestRunTimestamp(run); timestampAfter(candidate, latestRunAt) {
			latestRunAt = candidate
		}
		for _, summary := range metricSummaries[run.RunID] {
			metricNames[summary.MetricName] = true
		}
		classification := ClassifyRun(run, tags[run.RunID], metricSummaries[run.RunID], SuccessOptions{
			Tags:          tags[run.RunID],
			MetricFilters: opts.MetricFilters,
		})
		lifecycleCounts[classification.LifecycleState]++
	}
	return ExperimentSummary{
		ExperimentRecord: experiment,
		RunCount:         len(runs),
		RunGroupCount:    len(groupIDs),
		StateCounts:      stateCounts,
		LifecycleCounts:  lifecycleCounts,
		LatestRunAt:      latestRunAt,
		MetricNames:      sortedBoolKeys(metricNames),
	}, nil
}

func timestampAfter(candidate, current string) bool {
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(candidate))
	currentTime, currentErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(current))
	if candidateErr != nil {
		return false
	}
	return currentErr != nil || candidateTime.After(currentTime)
}

func latestRunTimestamp(run RunRecord) string {
	for _, value := range []string{run.StartedAt, run.CompletedAt, run.CreatedAt} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sortedBoolKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ensureRunExperimentIndexesTx(ctx context.Context, tx *sql.Tx, run RunRecord, tags []TagRecord, now string) (int, error) {
	experimentID, experimentName := experimentTags(run, tags)
	if experimentID == "" {
		experimentID = strings.TrimSpace(run.ExperimentID)
	}

	if experimentID == "" {
		if run.RunGroupID == "" {
			return 0, nil
		}
		if _, err := ensureImplicitExperimentTx(ctx, tx, ExperimentRecord{
			ExperimentID: run.RunGroupID,
			Project:      run.Project,
			Name:         run.RunGroupID,
			Source:       "run_group",
			CreatedAt:    now,
			UpdatedAt:    now,
		}); err != nil {
			return 0, err
		}
		assigned, err := assignRunExperimentIfUnsetTx(ctx, tx, run.RunGroupID, run.RunID)
		if err != nil {
			return 0, err
		}
		return boolToInt(assigned), nil
	}

	exp := ExperimentRecord{
		ExperimentID: experimentID,
		Project:      run.Project,
		Name:         experimentName,
		Source:       "explicit",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if exp.Name == "" {
		exp.Name = experimentID
	}
	if err := upsertExperimentTx(ctx, tx, exp); err != nil {
		return 0, err
	}
	if err := setRunExperimentTx(ctx, tx, experimentID, run.RunID); err != nil {
		return 0, err
	}
	return 1, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func experimentTags(run RunRecord, tags []TagRecord) (string, string) {
	var experimentID, experimentName string
	for _, tag := range tags {
		if tag.ScopeType != "run" || tag.ScopeID != run.RunID {
			continue
		}
		switch tag.Key {
		case experimentTagID, "experiment_id", "experiment":
			experimentID = strings.TrimSpace(tag.Value)
		case experimentTagName, "experiment_name":
			experimentName = strings.TrimSpace(tag.Value)
		}
	}
	return experimentID, experimentName
}

func ensureImplicitExperimentTx(ctx context.Context, tx *sql.Tx, exp ExperimentRecord) (bool, error) {
	if err := validateExperiment(exp); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO experiments(experiment_id, project, name, description, source, created_at, updated_at)
VALUES (?, ?, ?, nullif(?, ''), ?, ?, ?)`,
		exp.ExperimentID, exp.Project, exp.Name, exp.Description, exp.Source, exp.CreatedAt, exp.UpdatedAt)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func upsertExperimentTx(ctx context.Context, tx *sql.Tx, exp ExperimentRecord) error {
	if err := validateExperiment(exp); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO experiments(experiment_id, project, name, description, source, created_at, updated_at)
VALUES (?, ?, ?, nullif(?, ''), ?, ?, ?)
ON CONFLICT(experiment_id) DO UPDATE SET
  project = excluded.project,
  name = excluded.name,
  description = coalesce(excluded.description, experiments.description),
  source = excluded.source,
  updated_at = excluded.updated_at`,
		exp.ExperimentID, exp.Project, exp.Name, exp.Description, exp.Source, exp.CreatedAt, exp.UpdatedAt)
	return err
}

func ensureRunGroupExperimentAssignmentsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	result, err := tx.ExecContext(ctx, `
UPDATE runs SET experiment_id = run_group_id
WHERE experiment_id = '' AND coalesce(run_group_id, '') != ''`)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	return int(rows), err
}

func setRunExperimentTx(ctx context.Context, tx *sql.Tx, experimentID, runID string) error {
	if err := validateID("experiment", experimentID); err != nil {
		return err
	}
	if err := validateID("run", runID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE runs SET experiment_id = ? WHERE run_id = ?`, experimentID, runID)
	return err
}

func assignRunExperimentIfUnsetTx(ctx context.Context, tx *sql.Tx, experimentID, runID string) (bool, error) {
	if err := validateID("experiment", experimentID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE runs SET experiment_id = ? WHERE run_id = ? AND experiment_id = ''`, experimentID, runID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func validateExperiment(exp ExperimentRecord) error {
	if err := validateID("experiment", exp.ExperimentID); err != nil {
		return err
	}
	if err := validateID("project", exp.Project); err != nil {
		return err
	}
	if strings.TrimSpace(exp.Name) == "" {
		return fmt.Errorf("experiment name is required")
	}
	if exp.Source == "" {
		return fmt.Errorf("experiment source is required")
	}
	if exp.CreatedAt == "" || exp.UpdatedAt == "" {
		return fmt.Errorf("experiment created_at and updated_at are required")
	}
	return nil
}

func firstNonEmptyExperimentName(name, id string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return id
}
