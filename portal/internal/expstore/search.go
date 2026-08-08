// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const RunSearchSchemaVersion = "tau.exp.run_search.v0"
const runSearchLifecycleBatchSize = 200

var failureMetricTokens = []string{
	"collapse_bad_steps",
	"bad_steps",
	"nan_steps",
	"inf_steps",
	"oom",
	"failed",
	"failure",
	"error_count",
	"errors",
}

func (s *Store) SearchRuns(ctx context.Context, opts RunSearchOptions) (RunSearchResult, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Project = strings.TrimSpace(opts.Project)
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	opts.State = normalizeRunState(opts.State)
	opts.Lifecycle = normalizeLifecycle(opts.Lifecycle)
	if opts.Limit < 0 {
		return RunSearchResult{}, fmt.Errorf("limit must be non-negative")
	}
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	backfill, err := s.EnsureMetricSummaries(ctx)
	if err != nil {
		return RunSearchResult{}, err
	}
	where, args, err := s.runSearchWhere(ctx, opts)
	if err != nil {
		return RunSearchResult{}, err
	}
	if opts.Lifecycle != "" {
		return s.searchRunsByLifecycle(ctx, opts, where, args, backfill)
	}
	sqlLimit := opts.Limit + 1
	runs, err := s.runRecordsWhere(ctx, where, args, sqlLimit)
	if err != nil {
		return RunSearchResult{}, err
	}
	searchRuns, err := s.searchRunRecords(ctx, runs, opts)
	if err != nil {
		return RunSearchResult{}, err
	}
	total := len(searchRuns)
	truncated := false
	if total > opts.Limit {
		searchRuns = searchRuns[:opts.Limit]
		truncated = true
	}
	if opts.Lifecycle == "" && len(runs) > opts.Limit {
		truncated = true
		total = len(runs)
	}
	return RunSearchResult{
		SchemaVersion: RunSearchSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     s.Root,
		Target:        strings.TrimSpace(opts.Target),
		Total:         total,
		Truncated:     truncated,
		Runs:          searchRuns,
		Warnings:      backfill.Warnings,
	}, nil
}

func (s *Store) searchRunsByLifecycle(ctx context.Context, opts RunSearchOptions, where string, args []any, backfill MetricSummaryBackfillResult) (RunSearchResult, error) {
	offset := 0
	searchRuns := []RunSearchRun{}
	truncated := false
	for {
		runs, err := s.runRecordsWherePage(ctx, where, args, runSearchLifecycleBatchSize, offset)
		if err != nil {
			return RunSearchResult{}, err
		}
		if len(runs) == 0 {
			break
		}
		batchRuns, err := s.searchRunRecords(ctx, runs, opts)
		if err != nil {
			return RunSearchResult{}, err
		}
		for _, run := range batchRuns {
			if run.LifecycleState != opts.Lifecycle {
				continue
			}
			searchRuns = append(searchRuns, run)
			if len(searchRuns) > opts.Limit {
				truncated = true
				break
			}
		}
		if truncated || len(runs) < runSearchLifecycleBatchSize {
			break
		}
		offset += len(runs)
	}
	total := len(searchRuns)
	if total > opts.Limit {
		searchRuns = searchRuns[:opts.Limit]
	}
	return RunSearchResult{
		SchemaVersion: RunSearchSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     s.Root,
		Target:        strings.TrimSpace(opts.Target),
		Total:         total,
		Truncated:     truncated,
		Runs:          searchRuns,
		Warnings:      backfill.Warnings,
	}, nil
}

func (s *Store) searchRunRecords(ctx context.Context, runs []RunRecord, opts RunSearchOptions) ([]RunSearchRun, error) {
	runIDs := runRecordIDs(runs)
	tags, err := s.RunTags(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	summaries, err := s.metricSummariesByRun(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	searchRuns := make([]RunSearchRun, 0, len(runs))
	for _, run := range runs {
		runTags := tags[run.RunID]
		runSummaries := summaries[run.RunID]
		classification := ClassifyRun(run, runTags, runSummaries, SuccessOptions{
			Tags:          runTags,
			MetricFilters: opts.MetricFilters,
			MinStep:       opts.MinStep,
		})
		searchRuns = append(searchRuns, RunSearchRun{
			RunRecord:      run,
			LifecycleState: classification.LifecycleState,
			Successful:     classification.Successful,
			SuccessReasons: classification.Reasons,
			Tags:           runTags,
			MetricNames:    metricSummaryNames(runSummaries),
			Metrics:        runSummaries,
		})
	}
	return searchRuns, nil
}

func ParseMetricFilter(spec string) (MetricFilter, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return MetricFilter{}, fmt.Errorf("metric filter is required")
	}
	for _, op := range []string{">=", "<=", "!=", ">", "<", "="} {
		if idx := strings.Index(spec, op); idx > 0 {
			left := strings.TrimSpace(spec[:idx])
			right := strings.TrimSpace(spec[idx+len(op):])
			if left == "" || right == "" {
				return MetricFilter{}, fmt.Errorf("metric filter %q must be metric%svalue", spec, op)
			}
			if strings.ContainsAny(left, "<>!=") {
				return MetricFilter{}, fmt.Errorf("metric filter %q metric name is required before operator", spec)
			}
			value, err := strconv.ParseFloat(right, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return MetricFilter{}, fmt.Errorf("metric filter %q value must be finite", spec)
			}
			metricName, field := splitMetricFilterField(left)
			if metricName == "" {
				return MetricFilter{}, fmt.Errorf("metric filter %q metric name is required", spec)
			}
			if field == "" {
				field = "latest"
			}
			if _, ok := metricFilterFieldColumn(field); !ok {
				return MetricFilter{}, fmt.Errorf("metric filter %q field must be latest, min, max, count, finite_count, non_finite_count, latest_step, min_step, or max_step", spec)
			}
			return MetricFilter{MetricName: metricName, Field: field, Op: op, Value: value}, nil
		}
	}
	return MetricFilter{}, fmt.Errorf("metric filter %q must contain one of >=, <=, !=, >, <, =", spec)
}

func ClassifyRun(run RunRecord, tags map[string]string, summaries []MetricSummaryRecord, opts SuccessOptions) RunClassification {
	state := normalizeRunState(run.State)
	switch state {
	case "pending", "running":
		return RunClassification{LifecycleState: state, Successful: false, Reasons: []string{"run is " + state}}
	case "failed":
		return RunClassification{LifecycleState: "failed", Successful: false, Reasons: []string{"run state is failed"}}
	case "succeeded", "completed":
		// Continue into success gates.
	default:
		if strings.TrimSpace(run.CompletedAt) == "" {
			return RunClassification{LifecycleState: "pending", Successful: false, Reasons: []string{"run has not reached a terminal state"}}
		}
		return RunClassification{LifecycleState: "incomplete", Successful: false, Reasons: []string{"run state is not recognized as succeeded"}}
	}
	reasons := []string{}
	maxStep := maxSummaryStep(summaries)
	minStep := opts.MinStep
	if minStep == nil {
		minStep = taggedMinStep(tags)
	}
	if minStep != nil && (maxStep == nil || *maxStep < *minStep) {
		got := "none"
		if maxStep != nil {
			got = strconv.FormatInt(*maxStep, 10)
		}
		reasons = append(reasons, fmt.Sprintf("max step %s is below required step %d", got, *minStep))
	}
	for _, summary := range summaries {
		if summary.NonFiniteCount > 0 {
			reasons = append(reasons, fmt.Sprintf("%s has %d non-finite values", summary.MetricName, summary.NonFiniteCount))
		}
		if isFailureMetric(summary.MetricName) && summary.FiniteCount > 0 && summary.MaxValue > 0 {
			reasons = append(reasons, fmt.Sprintf("%s indicates failure with max value %s", summary.MetricName, strconv.FormatFloat(summary.MaxValue, 'g', 6, 64)))
		}
	}
	for _, filter := range opts.MetricFilters {
		if !metricFilterMatches(summaries, filter) {
			reasons = append(reasons, fmt.Sprintf("%s does not satisfy %s%s", filter.MetricName, normalizedMetricFilterField(filter.Field), filter.Op))
		}
	}
	if len(reasons) > 0 {
		return RunClassification{LifecycleState: "incomplete", Successful: false, Reasons: reasons}
	}
	return RunClassification{LifecycleState: "succeeded", Successful: true, Reasons: []string{"run state succeeded and success gates passed"}}
}

func (s *Store) runSearchWhere(ctx context.Context, opts RunSearchOptions) (string, []any, error) {
	clauses := []string{}
	args := []any{}
	if target := strings.TrimSpace(opts.Target); target != "" {
		targetType, err := s.targetType(ctx, target)
		if err != nil {
			return "", nil, err
		}
		switch targetType {
		case "run_group":
			clauses = append(clauses, "r.run_group_id = ?")
			args = append(args, target)
		case "run":
			clauses = append(clauses, "r.run_id = ?")
			args = append(args, target)
		case "experiment":
			clauses = append(clauses, "r.experiment_id = ?")
			args = append(args, target)
		}
	}
	if opts.Project != "" {
		clauses = append(clauses, "r.project = ?")
		args = append(args, opts.Project)
	}
	if opts.RunGroupID != "" {
		clauses = append(clauses, "r.run_group_id = ?")
		args = append(args, opts.RunGroupID)
	}
	if opts.State != "" {
		clauses = append(clauses, "lower(r.state) = ?")
		args = append(args, opts.State)
	}
	if opts.Since != "" {
		since, err := normalizeSince(opts.Since, time.Now())
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "r.created_at >= ?")
		args = append(args, since)
	}
	if opts.Query != "" {
		like := "%" + strings.ToLower(opts.Query) + "%"
		clauses = append(clauses, `(lower(r.run_id) LIKE ? OR lower(r.project) LIKE ? OR lower(r.run_group_id) LIKE ? OR lower(coalesce(r.owner, '')) LIKE ? OR lower(coalesce(r.result_uri, '')) LIKE ? OR lower(coalesce(g.name, '')) LIKE ? OR EXISTS (
  SELECT 1 FROM tags t WHERE t.scope_type = 'run' AND t.scope_id = r.run_id AND (lower(t.key) LIKE ? OR lower(t.value) LIKE ?)
) OR EXISTS (
  SELECT 1 FROM metric_summaries ms WHERE ms.run_id = r.run_id AND lower(ms.metric_name) LIKE ?
))`)
		args = append(args, like, like, like, like, like, like, like, like, like)
	}
	if opts.Workspace != "" {
		clauses = append(clauses, workspaceRunClause)
		args = append(args, exptelemetry.TauWorkspaceTag, opts.Workspace, exptelemetry.TauWorkspaceTag)
	}
	for _, key := range sortedKeys(opts.Tags) {
		clauses = append(clauses, `EXISTS (
  SELECT 1 FROM tags t WHERE t.scope_type = 'run' AND t.scope_id = r.run_id AND t.key = ? AND t.value = ?
)`)
		args = append(args, key, opts.Tags[key])
	}
	for _, metricName := range opts.MetricNames {
		metricName = strings.TrimSpace(metricName)
		if metricName == "" {
			continue
		}
		clauses = append(clauses, `EXISTS (
  SELECT 1 FROM metric_summaries ms WHERE ms.run_id = r.run_id AND ms.metric_name = ?
)`)
		args = append(args, metricName)
	}
	for _, filter := range opts.MetricFilters {
		column, ok := metricFilterFieldColumn(filter.Field)
		if !ok {
			return "", nil, fmt.Errorf("unsupported metric filter field %q", filter.Field)
		}
		if filter.MetricName == "" {
			return "", nil, fmt.Errorf("metric filter metric name is required")
		}
		clauses = append(clauses, fmt.Sprintf(`EXISTS (
  SELECT 1 FROM metric_summaries ms WHERE ms.run_id = r.run_id AND ms.metric_name = ? AND %s %s ?
)`, column, sqliteMetricOperator(filter.Op)))
		args = append(args, filter.MetricName, filter.Value)
	}
	if len(clauses) == 0 {
		return "", args, nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args, nil
}

func (s *Store) runRecordsWhere(ctx context.Context, where string, args []any, limit int) ([]RunRecord, error) {
	return s.runRecordsWherePage(ctx, where, args, limit, 0)
}

func (s *Store) runRecordsWherePage(ctx context.Context, where string, args []any, limit, offset int) ([]RunRecord, error) {
	query := `
SELECT r.run_id, r.project, r.experiment_id, r.run_group_id,
       coalesce(r.parent_run_id, ''), r.state, coalesce(r.owner, ''), r.created_at, coalesce(r.started_at, ''),
       coalesce(r.completed_at, ''), coalesce(r.config_hash, ''), coalesce(r.code_sha, ''),
       coalesce(r.image_digest, ''), coalesce(r.tau_command, ''), coalesce(r.result_uri, ''), r.index_version
FROM runs r
LEFT JOIN run_groups g ON g.run_group_id = r.run_group_id
` + where + `
ORDER BY r.created_at DESC, r.run_id`
	if limit > 0 {
		query += " LIMIT " + strconv.Itoa(limit)
		if offset > 0 {
			query += " OFFSET " + strconv.Itoa(offset)
		}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunRecord{}
	for rows.Next() {
		var run RunRecord
		if err := rows.Scan(
			&run.RunID,
			&run.Project,
			&run.ExperimentID,
			&run.RunGroupID,
			&run.ParentRunID,
			&run.State,
			&run.Owner,
			&run.CreatedAt,
			&run.StartedAt,
			&run.CompletedAt,
			&run.ConfigHash,
			&run.CodeSHA,
			&run.ImageDigest,
			&run.TauCommand,
			&run.ResultURI,
			&run.IndexVersion,
		); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) RunTags(ctx context.Context, runIDs []string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope_id, key, value FROM tags WHERE scope_type = 'run' AND scope_id IN (`+sqlPlaceholders(len(runIDs))+`) ORDER BY scope_id, key`, anySlice(runIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID, key, value string
		if err := rows.Scan(&runID, &key, &value); err != nil {
			return nil, err
		}
		if out[runID] == nil {
			out[runID] = map[string]string{}
		}
		out[runID][key] = value
	}
	return out, rows.Err()
}

func (s *Store) metricSummariesByRun(ctx context.Context, runIDs []string) (map[string][]MetricSummaryRecord, error) {
	out := map[string][]MetricSummaryRecord{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, coalesce(project, ''), coalesce(run_group_id, ''), metric_name,
       count, finite_count, non_finite_count, min_step, max_step, latest_step, latest_wall_time,
       latest_value, min_value, max_value, updated_at, coalesce(latest_file_id, '')
FROM metric_summaries
WHERE run_id IN (`+sqlPlaceholders(len(runIDs))+`)
ORDER BY run_id, metric_name`, anySlice(runIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		summary, err := scanMetricSummaryRecord(rows)
		if err != nil {
			return nil, err
		}
		out[summary.RunID] = append(out[summary.RunID], summary)
	}
	return out, rows.Err()
}

func scanMetricSummaryRecord(scanner interface {
	Scan(dest ...any) error
}) (MetricSummaryRecord, error) {
	var summary MetricSummaryRecord
	var minStep, maxStep, latestStep, latestWallTime sql.NullInt64
	var latestValue, minValue, maxValue sql.NullFloat64
	err := scanner.Scan(
		&summary.RunID,
		&summary.Project,
		&summary.RunGroupID,
		&summary.MetricName,
		&summary.Count,
		&summary.FiniteCount,
		&summary.NonFiniteCount,
		&minStep,
		&maxStep,
		&latestStep,
		&latestWallTime,
		&latestValue,
		&minValue,
		&maxValue,
		&summary.UpdatedAt,
		&summary.LatestFileID,
	)
	if err != nil {
		return MetricSummaryRecord{}, err
	}
	applyMetricSummaryNulls(&summary, minStep, maxStep, latestStep, latestWallTime, latestValue, minValue, maxValue)
	return summary, nil
}

func applyMetricSummaryNulls(summary *MetricSummaryRecord, minStep, maxStep, latestStep, latestWallTime sql.NullInt64, latestValue, minValue, maxValue sql.NullFloat64) {
	if minStep.Valid {
		summary.MinStep = &minStep.Int64
	}
	if maxStep.Valid {
		summary.MaxStep = &maxStep.Int64
	}
	if latestStep.Valid {
		summary.LatestStep = &latestStep.Int64
	}
	if latestWallTime.Valid {
		summary.LatestWallTime = &latestWallTime.Int64
	}
	if latestValue.Valid {
		summary.LatestValue = latestValue.Float64
	}
	if minValue.Valid {
		summary.MinValue = minValue.Float64
	}
	if maxValue.Valid {
		summary.MaxValue = maxValue.Float64
	}
}

func normalizeRunState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func normalizeLifecycle(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "success", "successful":
		return "succeeded"
	default:
		return value
	}
}

func isFailureMetric(metricName string) bool {
	name := strings.ToLower(metricName)
	for _, token := range failureMetricTokens {
		if strings.Contains(name, token) {
			return true
		}
	}
	return false
}

func taggedMinStep(tags map[string]string) *int64 {
	for _, key := range []string{"tau.success.min_step", "success.min_step", "expected_max_step", "max_step"} {
		if value := strings.TrimSpace(tags[key]); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err == nil {
				return &parsed
			}
		}
	}
	return nil
}

func maxSummaryStep(summaries []MetricSummaryRecord) *int64 {
	var maxStep *int64
	for _, summary := range summaries {
		maxStep = maxInt64Ptr(maxStep, summary.MaxStep)
	}
	return maxStep
}

func metricFilterMatches(summaries []MetricSummaryRecord, filter MetricFilter) bool {
	for _, summary := range summaries {
		if summary.MetricName != filter.MetricName {
			continue
		}
		value, ok := metricFilterSummaryValue(summary, filter.Field)
		if !ok {
			continue
		}
		return compareMetricFilterValue(value, filter.Op, filter.Value)
	}
	return false
}

func metricFilterSummaryValue(summary MetricSummaryRecord, field string) (float64, bool) {
	switch normalizedMetricFilterField(field) {
	case "latest":
		return summary.LatestValue, summary.FiniteCount > 0
	case "min":
		return summary.MinValue, summary.FiniteCount > 0
	case "max":
		return summary.MaxValue, summary.FiniteCount > 0
	case "count":
		return float64(summary.Count), true
	case "finite_count":
		return float64(summary.FiniteCount), true
	case "non_finite_count":
		return float64(summary.NonFiniteCount), true
	case "latest_step":
		if summary.LatestStep == nil {
			return 0, false
		}
		return float64(*summary.LatestStep), true
	case "min_step":
		if summary.MinStep == nil {
			return 0, false
		}
		return float64(*summary.MinStep), true
	case "max_step":
		if summary.MaxStep == nil {
			return 0, false
		}
		return float64(*summary.MaxStep), true
	default:
		return 0, false
	}
}

func compareMetricFilterValue(left float64, op string, right float64) bool {
	switch op {
	case ">=":
		return left >= right
	case "<=":
		return left <= right
	case ">":
		return left > right
	case "<":
		return left < right
	case "=", "==":
		return left == right
	case "!=":
		return left != right
	default:
		return false
	}
}

func splitMetricFilterField(value string) (string, string) {
	if metric, field, ok := strings.Cut(value, "@"); ok {
		return strings.TrimSpace(metric), normalizedMetricFilterField(field)
	}
	for _, field := range []string{"latest_step", "min_step", "max_step", "finite_count", "non_finite_count", "latest", "min", "max", "count"} {
		suffix := ":" + field
		if strings.HasSuffix(value, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(value, suffix)), field
		}
	}
	return strings.TrimSpace(value), ""
}

func normalizedMetricFilterField(field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "", "latest_value":
		return "latest"
	case "min_value":
		return "min"
	case "max_value":
		return "max"
	default:
		return field
	}
}

func metricFilterFieldColumn(field string) (string, bool) {
	switch normalizedMetricFilterField(field) {
	case "latest":
		return "ms.latest_value", true
	case "min":
		return "ms.min_value", true
	case "max":
		return "ms.max_value", true
	case "count":
		return "ms.count", true
	case "finite_count":
		return "ms.finite_count", true
	case "non_finite_count":
		return "ms.non_finite_count", true
	case "latest_step":
		return "ms.latest_step", true
	case "min_step":
		return "ms.min_step", true
	case "max_step":
		return "ms.max_step", true
	default:
		return "", false
	}
}

func sqliteMetricOperator(op string) string {
	if op == "==" {
		return "="
	}
	return op
}

func normalizeSince(value string, now time.Time) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		if err != nil {
			return "", fmt.Errorf("since must be RFC3339, Go duration, or Nd")
		}
		return now.UTC().Add(-time.Duration(days * float64(24*time.Hour))).Format(time.RFC3339), nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return now.UTC().Add(-duration).Format(time.RFC3339), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("since must be RFC3339, Go duration, or Nd")
	}
	return parsed.UTC().Format(time.RFC3339), nil
}

func runRecordIDs(runs []RunRecord) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.RunID)
	}
	return ids
}

func metricSummaryNames(summaries []MetricSummaryRecord) []string {
	names := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		names = append(names, summary.MetricName)
	}
	sort.Strings(names)
	return names
}

func anySlice(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func sqlPlaceholders(count int) string {
	placeholders := make([]string, count)
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return strings.Join(placeholders, ", ")
}
