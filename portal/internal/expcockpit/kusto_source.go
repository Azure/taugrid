// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

const defaultKustoDiscoverySince = "90d"
const defaultKustoMaxDiscoverySince = "365d"
const defaultKustoTargetSince = "365d"
const noAllowedKustoProjectMatch = "__tau_no_allowed_project_match__"

type KustoMetricRow struct {
	CatalogVersion      string   `json:"catalog_version,omitempty"`
	WorkspaceID         string   `json:"workspace_id,omitempty"`
	Cluster             string   `json:"cluster,omitempty"`
	SourceStoreID       string   `json:"source_store_id"`
	Project             string   `json:"project"`
	ExperimentID        string   `json:"experiment_id"`
	RunGroupID          string   `json:"run_group_id"`
	RunID               string   `json:"run_id"`
	MetricName          string   `json:"metric_name"`
	Step                int64    `json:"step"`
	WallTime            string   `json:"wall_time"`
	Value               float64  `json:"value"`
	Unit                string   `json:"unit"`
	Source              string   `json:"source"`
	Split               string   `json:"split"`
	MetricFileID        string   `json:"metric_file_id"`
	MetricFilePath      string   `json:"metric_file_path"`
	SourcePointCount    int      `json:"source_point_count,omitempty"`
	ValidationMilestone bool     `json:"validation_milestone,omitempty"`
	Tags                string   `json:"tags"`
	FirstActivityAt     string   `json:"first_activity_at,omitempty"`
	LatestActivityAt    string   `json:"latest_activity_at,omitempty"`
	MinStep             *int64   `json:"min_step,omitempty"`
	MaxStep             *int64   `json:"max_step,omitempty"`
	LatestStep          *int64   `json:"latest_step,omitempty"`
	LatestValue         *float64 `json:"latest_value,omitempty"`
	State               string   `json:"state,omitempty"`
	Reason              string   `json:"reason,omitempty"`
	Message             string   `json:"message,omitempty"`
	SubmitTime          string   `json:"submit_time,omitempty"`
	CreatedTime         string   `json:"created_time,omitempty"`
	PodStartTime        string   `json:"pod_start_time,omitempty"`
	TerminalAt          string   `json:"terminal_at,omitempty"`
	CompletionTime      string   `json:"completion_time,omitempty"`
	ResultScope         string   `json:"result_scope,omitempty"`
	ImageDigest         string   `json:"image_digest,omitempty"`
	ConfigHash          string   `json:"config_hash,omitempty"`
	CodeSHA             string   `json:"code_sha,omitempty"`
	MetricSeriesCount   int64    `json:"metric_series_count,omitempty"`
	HasMetrics          bool     `json:"has_metrics,omitempty"`
	HasLifecycle        bool     `json:"has_lifecycle,omitempty"`
}

type KustoSource struct {
	WorkspaceID       string
	AllowedProjects   []string
	Endpoint          string
	Database          string
	DiscoverySince    string
	MaxDiscoverySince string
	TargetSince       string
	TargetPoints      int
	QueryCommand      string
	QueryArgs         []string
	// NativeQuery runs generated KQL against ADX through the azure-kusto-go SDK
	// and returns the raw JSON response. When set it replaces the
	// --kusto-query-command shell adapter, so deployments no longer need to stage
	// a shell plus a hand-rolled IMDS-token script into the distroless image.
	// QueryCommand still wins when both are configured.
	NativeQuery func(ctx context.Context, query string) (string, error)
	StaleAfter  time.Duration
	Now         func() time.Time
}

// hasRemoteQuery reports whether this source can reach ADX at all — through the
// shell adapter or the native SDK transport.
func (s KustoSource) hasRemoteQuery() bool {
	return strings.TrimSpace(s.QueryCommand) != "" || s.NativeQuery != nil
}

func (s KustoSource) BuildTypedSeries(ctx context.Context, opts SeriesOptions) (SeriesDetail, error) {
	opts.Target = strings.TrimSpace(opts.Target)
	opts.Metric = strings.TrimSpace(opts.Metric)
	opts.RunID = strings.TrimSpace(opts.RunID)
	if opts.Target == "" {
		return SeriesDetail{}, fmt.Errorf("dashboard target is required")
	}
	if opts.Metric == "" {
		return SeriesDetail{}, fmt.Errorf("metric query parameter is required")
	}
	if opts.MaxPoints <= 0 {
		opts.MaxPoints = chartMaxRenderedPoints
	}
	var err error
	s, err = s.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return SeriesDetail{}, err
	}
	if opts.RunID == "" {
		return SeriesDetail{}, fmt.Errorf("run_id query parameter is required")
	}
	rows, err := s.runKustoSeriesCommand(ctx, opts)
	if err != nil {
		return SeriesDetail{}, err
	}
	rows = filterKustoSeriesRows(rows, opts)
	if len(rows) == 0 {
		return SeriesDetail{}, expstore.ErrNotFound
	}
	if opts.MaxMetricRows > 0 && len(rows) > opts.MaxMetricRows {
		rows = rows[:opts.MaxMetricRows]
	}

	allowedRuns := map[string]bool{opts.RunID: true}
	points := kustoMetricPoints(rows, allowedRuns)
	chart := buildChartWithRunColorsBudgetAndInterval(points, opts.Metric, nil, runColorMapForRunIDs([]string{opts.RunID}), opts.MaxPoints, opts.StepInterval)
	chart.StepInterval = opts.StepInterval
	warnings := []string{"source=kusto series are extrema-preserving display projections; Kusto source rows remain authoritative"}
	detail := SeriesDetail{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Target:        opts.Target,
		Metric:        opts.Metric,
		RunID:         opts.RunID,
		StartStep:     opts.StartStep,
		EndStep:       opts.EndStep,
		StepInterval:  opts.StepInterval,
		MaxPoints:     opts.MaxPoints,
		Chart:         chart,
		Warnings:      warnings,
	}
	rawQuery, err := s.buildKustoSeriesQuery(ctx, opts, true)
	if err != nil {
		return SeriesDetail{}, err
	}
	detail.RawQuery = rawQuery
	detail.RawQuerySource = "kusto"
	return detail, nil
}

func filterKustoSeriesRows(rows []KustoMetricRow, opts SeriesOptions) []KustoMetricRow {
	out := make([]KustoMetricRow, 0, len(rows))
	for _, row := range rows {
		if opts.RunID != "" && row.RunID != opts.RunID {
			continue
		}
		if row.MetricName != opts.Metric && !isValidationMilestoneMetric(row.MetricName) {
			continue
		}
		if opts.StartStep != nil && row.Step < *opts.StartStep {
			continue
		}
		if opts.EndStep != nil && row.Step > *opts.EndStep {
			continue
		}
		out = append(out, row)
	}
	return out
}

func normalizeKustoExperimentSearchOptions(opts expstore.ExperimentSearchOptions) expstore.ExperimentSearchOptions {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	opts.Project = strings.TrimSpace(opts.Project)
	opts.Lifecycle = strings.ToLower(strings.TrimSpace(opts.Lifecycle))
	switch opts.Lifecycle {
	case "success", "successful", "completed":
		opts.Lifecycle = "succeeded"
	}
	return opts
}

func normalizeKustoRunSearchOptions(opts expstore.RunSearchOptions) expstore.RunSearchOptions {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	opts.Project = strings.TrimSpace(opts.Project)
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.State = normalizeKustoLifecycle(opts.State)
	opts.Lifecycle = normalizeKustoLifecycle(opts.Lifecycle)
	return opts
}

func normalizeKustoSearchLimit(limit int) (int, error) {
	switch {
	case limit < 0:
		return 0, fmt.Errorf("limit must be non-negative")
	case limit == 0:
		return 200, nil
	default:
		return min(limit, 1000), nil
	}
}

// SearchCatalogExperiments reads run identities from TauExpRunCatalogRows() so
// lifecycle-only experiments remain discoverable, then enriches them from
// TauExpSeriesCatalogRows(). Helm selects the function implementations; Portal
// neither knows nor models alternate catalog implementations.
func (s KustoSource) SearchCatalogExperiments(ctx context.Context, opts expstore.ExperimentSearchOptions) (expstore.ExperimentSearchResult, error) {
	if !s.hasRemoteQuery() {
		return expstore.ExperimentSearchResult{}, fmt.Errorf("typed experiment catalog requires a live Kusto query transport")
	}
	opts = normalizeKustoExperimentSearchOptions(opts)
	var err error
	s, err = s.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	opts.Limit, err = normalizeKustoSearchLimit(opts.Limit)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	if strings.TrimSpace(opts.Since) == "" {
		opts.Since = s.effectiveDiscoverySince()
	}
	if err := s.validateDiscoverySince(opts.Since, opts.Project); err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	projects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	const sourceBatchSize = 1000
	afterAt := opts.CursorAt
	afterProject, afterExperiment := catalogCursorParts(opts.CursorID)
	summariesByKey := map[string]expstore.ExperimentSummary{}
	sourceExhausted := false
	for len(summariesByKey) <= opts.Limit && !sourceExhausted {
		query, err := expkusto.BuildExperimentCatalogQuery(expkusto.CatalogQueryOptions{
			WorkspaceID: s.WorkspaceID, Projects: projects, Target: opts.Target,
			MetricNames: opts.MetricNames, Since: opts.Since, Limit: sourceBatchSize,
			AfterAt: afterAt, AfterProject: afterProject, AfterExperimentID: afterExperiment,
		})
		if err != nil {
			return expstore.ExperimentSearchResult{}, err
		}
		runRows, err := s.executeKustoQueryCommand(ctx, query)
		if err != nil {
			return expstore.ExperimentSearchResult{}, err
		}
		if len(runRows) == 0 {
			sourceExhausted = true
			break
		}
		metricRows, err := s.catalogMetricRowsForRuns(ctx, projects, expstore.RunSearchOptions{
			Target: opts.Target, Project: opts.Project, Workspace: opts.Workspace,
			MetricNames: opts.MetricNames, Since: opts.Since, Limit: opts.Limit,
		}, runRows)
		if err != nil {
			return expstore.ExperimentSearchResult{}, err
		}
		runs := catalogRunSearchRuns(runRows, metricRows, expstore.RunSearchOptions{
			Target: opts.Target, Workspace: opts.Workspace, Query: opts.Query, Project: opts.Project,
			Lifecycle: opts.Lifecycle, Tags: opts.Tags, MetricNames: opts.MetricNames,
			MetricFilters: opts.MetricFilters, Since: opts.Since,
		})
		for _, summary := range catalogExperimentSummaries(runRows, runs, s.sourcePath()) {
			summariesByKey[summary.Project+"\x00"+summary.ExperimentID] = summary
		}
		pageCursor, pageCount := catalogExperimentPageCursor(runRows)
		if pageCount < sourceBatchSize {
			sourceExhausted = true
		} else if pageCursor.At == afterAt && pageCursor.Project == afterProject && pageCursor.ID == afterExperiment {
			return expstore.ExperimentSearchResult{}, fmt.Errorf("typed experiment catalog pagination did not advance")
		} else {
			afterAt, afterProject, afterExperiment = pageCursor.At, pageCursor.Project, pageCursor.ID
		}
	}
	summaries := make([]expstore.ExperimentSummary, 0, len(summariesByKey))
	for _, summary := range summariesByKey {
		summaries = append(summaries, summary)
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].LatestRunAt != summaries[j].LatestRunAt {
			return summaries[i].LatestRunAt > summaries[j].LatestRunAt
		}
		return summaries[i].ExperimentID < summaries[j].ExperimentID
	})
	truncated := len(summaries) > opts.Limit || !sourceExhausted
	if len(summaries) > opts.Limit {
		summaries = summaries[:opts.Limit]
	}
	return expstore.ExperimentSearchResult{
		SchemaVersion: expstore.ExperimentSearchSchemaVersion,
		GeneratedAt:   s.effectiveNow().Format(time.RFC3339),
		StorePath:     s.sourcePath(),
		Total:         len(summaries),
		Truncated:     truncated,
		Experiments:   summaries,
	}, nil
}

func catalogExperimentSummaries(rows []KustoMetricRow, runs []expstore.RunSearchRun, source string) []expstore.ExperimentSummary {
	type accumulator struct {
		summary expstore.ExperimentSummary
		groups  map[string]bool
		metrics map[string]bool
	}
	latestActivity := map[string]string{}
	for _, row := range rows {
		key := catalogRunKey(row.Project, row.ExperimentID)
		if row.LatestActivityAt > latestActivity[key] {
			latestActivity[key] = row.LatestActivityAt
		}
	}
	byExperiment := map[string]*accumulator{}
	for _, run := range runs {
		experimentID := firstNonEmptyString(run.ExperimentID, run.RunGroupID, run.RunID)
		key := strings.TrimSpace(run.Project) + "\x00" + experimentID
		item := byExperiment[key]
		if item == nil {
			item = &accumulator{
				summary: expstore.ExperimentSummary{
					ExperimentRecord: expstore.ExperimentRecord{
						ExperimentID: experimentID, Project: run.Project, Name: experimentID, Source: source,
						CreatedAt: run.CreatedAt, UpdatedAt: firstNonEmptyString(run.CompletedAt, run.StartedAt, run.CreatedAt),
					},
					StateCounts: map[string]int{}, LifecycleCounts: map[string]int{},
				},
				groups: map[string]bool{}, metrics: map[string]bool{},
			}
			byExperiment[key] = item
		}
		item.summary.RunCount++
		if run.RunGroupID != "" {
			item.groups[run.RunGroupID] = true
		}
		item.summary.StateCounts[firstNonEmptyString(run.State, "unknown")]++
		item.summary.LifecycleCounts[firstNonEmptyString(run.LifecycleState, "unknown")]++
		latest := firstNonEmptyString(latestActivity[catalogRunKey(run.Project, experimentID)], run.CompletedAt, run.StartedAt, run.CreatedAt)
		if latest > item.summary.LatestRunAt {
			item.summary.LatestRunAt = latest
			item.summary.UpdatedAt = latest
		}
		if run.CreatedAt != "" && (item.summary.CreatedAt == "" || run.CreatedAt < item.summary.CreatedAt) {
			item.summary.CreatedAt = run.CreatedAt
		}
		for _, metric := range run.MetricNames {
			if metric != "" {
				item.metrics[metric] = true
			}
		}
	}
	out := make([]expstore.ExperimentSummary, 0, len(byExperiment))
	for _, item := range byExperiment {
		item.summary.RunGroupCount = len(item.groups)
		item.summary.MetricNames = make([]string, 0, len(item.metrics))
		for metric := range item.metrics {
			item.summary.MetricNames = append(item.summary.MetricNames, metric)
		}
		sort.Strings(item.summary.MetricNames)
		out = append(out, item.summary)
	}
	return out
}

// SearchCatalogRuns reads TauExpRunCatalogRows() for lifecycle/detail and
// TauExpSeriesCatalogRows() for metric names and summaries. The stable
// functions encapsulate Helm-time implementation selection.
func (s KustoSource) SearchCatalogRuns(ctx context.Context, opts expstore.RunSearchOptions) (expstore.RunSearchResult, error) {
	if !s.hasRemoteQuery() {
		return expstore.RunSearchResult{}, fmt.Errorf("typed run catalog requires a live Kusto query transport")
	}
	opts = normalizeKustoRunSearchOptions(opts)
	var err error
	s, err = s.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return expstore.RunSearchResult{}, err
	}
	opts.Limit, err = normalizeKustoSearchLimit(opts.Limit)
	if err != nil {
		return expstore.RunSearchResult{}, err
	}
	if strings.TrimSpace(opts.Since) == "" {
		opts.Since = s.effectiveTargetSince()
	}
	projects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return expstore.RunSearchResult{}, err
	}
	const sourceBatchSize = 1000
	afterAt := opts.CursorAt
	afterProject, afterRun := catalogCursorParts(opts.CursorID)
	runsByKey := map[string]expstore.RunSearchRun{}
	sourceExhausted := false
	for len(runsByKey) <= opts.Limit && !sourceExhausted {
		query, err := expkusto.BuildRunCatalogQuery(expkusto.CatalogQueryOptions{
			WorkspaceID: s.WorkspaceID, Projects: projects, Target: opts.Target,
			RunGroupID: opts.RunGroupID, MetricNames: opts.MetricNames, Since: opts.Since,
			Limit: sourceBatchSize, AfterAt: afterAt, AfterProject: afterProject, AfterRunID: afterRun,
		})
		if err != nil {
			return expstore.RunSearchResult{}, err
		}
		rows, err := s.executeKustoQueryCommand(ctx, query)
		if err != nil {
			return expstore.RunSearchResult{}, err
		}
		if len(rows) == 0 {
			sourceExhausted = true
			break
		}
		metricRows, err := s.catalogMetricRowsForRuns(ctx, projects, opts, rows)
		if err != nil {
			return expstore.RunSearchResult{}, err
		}
		for _, run := range catalogRunSearchRuns(rows, metricRows, opts) {
			runsByKey[catalogRunKey(run.Project, run.RunID)] = run
		}
		pageCursor := catalogRunPageCursor(rows)
		if len(rows) < sourceBatchSize {
			sourceExhausted = true
		} else if pageCursor.At == afterAt && pageCursor.Project == afterProject && pageCursor.ID == afterRun {
			return expstore.RunSearchResult{}, fmt.Errorf("typed run catalog pagination did not advance")
		} else {
			afterAt, afterProject, afterRun = pageCursor.At, pageCursor.Project, pageCursor.ID
		}
	}
	runs := make([]expstore.RunSearchRun, 0, len(runsByKey))
	for _, run := range runsByKey {
		runs = append(runs, run)
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].CreatedAt != runs[j].CreatedAt {
			return runs[i].CreatedAt > runs[j].CreatedAt
		}
		return runs[i].RunID < runs[j].RunID
	})
	total := len(runs)
	truncated := total > opts.Limit || !sourceExhausted
	if total > opts.Limit {
		runs = runs[:opts.Limit]
	}
	return expstore.RunSearchResult{
		SchemaVersion: expstore.RunSearchSchemaVersion,
		GeneratedAt:   s.effectiveNow().Format(time.RFC3339),
		StorePath:     s.sourcePath(),
		Target:        strings.TrimSpace(opts.Target),
		Total:         total,
		Truncated:     truncated,
		Runs:          runs,
	}, nil
}

func (s KustoSource) catalogMetricRowsForRuns(ctx context.Context, projects []string, opts expstore.RunSearchOptions, runs []KustoMetricRow) ([]KustoMetricRow, error) {
	runIDs := make([]string, 0, len(runs))
	for _, row := range runs {
		if strings.TrimSpace(row.RunID) != "" {
			runIDs = append(runIDs, row.RunID)
		}
	}
	runIDs = catalogUniqueSortedStrings(runIDs)
	if len(runIDs) == 0 {
		return nil, nil
	}
	query, err := expkusto.BuildSeriesCatalogQuery(expkusto.CatalogQueryOptions{
		WorkspaceID: s.WorkspaceID,
		Projects:    projects,
		RunIDs:      runIDs,
		MetricNames: opts.MetricNames,
		Since:       opts.Since,
		Limit:       1000,
	})
	if err != nil {
		return nil, err
	}
	return s.executeKustoQueryCommand(ctx, query)
}

func catalogRunSearchRuns(runRows, metricRows []KustoMetricRow, opts expstore.RunSearchOptions) []expstore.RunSearchRun {
	metricsByRun := catalogMetricSummariesByRun(metricRows)
	out := make([]expstore.RunSearchRun, 0, len(runRows))
	seen := map[string]bool{}
	for _, row := range runRows {
		key := catalogRunKey(row.Project, row.RunID)
		if row.RunID == "" || seen[key] {
			continue
		}
		seen[key] = true
		state := normalizeKustoLifecycle(row.State)
		lifecycle := state
		if lifecycle == "" {
			if row.HasLifecycle {
				lifecycle = "pending"
			} else if row.HasMetrics {
				lifecycle = "running"
			} else {
				lifecycle = "incomplete"
			}
		}
		reasons := catalogCompactStrings([]string{row.Reason, row.Message})
		successful := lifecycle == "succeeded"
		if len(reasons) == 0 && successful {
			reasons = []string{"run catalog state succeeded"}
		}
		run := expstore.RunSearchRun{
			RunRecord: expstore.RunRecord{
				RunID: row.RunID, Project: row.Project, ExperimentID: row.ExperimentID,
				RunGroupID: row.RunGroupID, State: firstNonEmptyString(state, lifecycle),
				CreatedAt: firstNonEmptyString(row.CreatedTime, row.SubmitTime, row.FirstActivityAt, row.LatestActivityAt),
				StartedAt: row.PodStartTime, CompletedAt: firstNonEmptyString(row.CompletionTime, row.TerminalAt),
				ConfigHash: row.ConfigHash, CodeSHA: row.CodeSHA, ImageDigest: row.ImageDigest,
				ResultURI: row.ResultScope,
			},
			LifecycleState: lifecycle,
			Successful:     successful,
			SuccessReasons: reasons,
			Tags:           kustoRowTags(row),
			MetricNames:    metricSummaryNames(metricsByRun[key]),
			Metrics:        metricsByRun[key],
		}
		if kustoRunSearchMatches(run, opts) {
			out = append(out, run)
		}
	}
	return out
}

func catalogMetricSummariesByRun(rows []KustoMetricRow) map[string][]expstore.MetricSummaryRecord {
	out := map[string][]expstore.MetricSummaryRecord{}
	for _, row := range rows {
		if row.RunID == "" || row.MetricName == "" {
			continue
		}
		latestStep := row.LatestStep
		if latestStep == nil {
			step := row.Step
			latestStep = &step
		}
		summary := expstore.MetricSummaryRecord{
			FileID:       firstNonEmptyString(row.MetricFileID, "kusto:"+row.RunID),
			RunID:        row.RunID,
			Project:      row.Project,
			RunGroupID:   row.RunGroupID,
			MetricName:   row.MetricName,
			MinStep:      row.MinStep,
			MaxStep:      row.MaxStep,
			LatestStep:   latestStep,
			UpdatedAt:    firstNonEmptyString(row.LatestActivityAt, row.WallTime),
			LatestFileID: row.MetricFileID,
		}
		if row.LatestValue != nil {
			summary.LatestValue = *row.LatestValue
		} else {
			summary.LatestValue = row.Value
		}
		key := catalogRunKey(row.Project, row.RunID)
		out[key] = append(out[key], summary)
	}
	for key := range out {
		sort.Slice(out[key], func(i, j int) bool { return out[key][i].MetricName < out[key][j].MetricName })
	}
	return out
}

func catalogRunKey(project, runID string) string {
	return strings.TrimSpace(project) + "\x00" + strings.TrimSpace(runID)
}

func catalogCursorParts(cursorID string) (string, string) {
	project, id, _ := strings.Cut(cursorID, "\x00")
	return strings.TrimSpace(project), strings.TrimSpace(id)
}

type catalogPageCursor struct {
	At      string
	Project string
	ID      string
}

func catalogRunPageCursor(rows []KustoMetricRow) catalogPageCursor {
	cursors := make([]catalogPageCursor, 0, len(rows))
	for _, row := range rows {
		cursors = append(cursors, catalogPageCursor{
			At:      firstNonEmptyString(row.CreatedTime, row.SubmitTime, row.FirstActivityAt, row.LatestActivityAt),
			Project: strings.TrimSpace(row.Project), ID: strings.TrimSpace(row.RunID),
		})
	}
	sort.Slice(cursors, func(i, j int) bool {
		if cursors[i].At != cursors[j].At {
			return cursors[i].At > cursors[j].At
		}
		if cursors[i].Project != cursors[j].Project {
			return cursors[i].Project < cursors[j].Project
		}
		return cursors[i].ID < cursors[j].ID
	})
	return cursors[len(cursors)-1]
}

func catalogExperimentPageCursor(rows []KustoMetricRow) (catalogPageCursor, int) {
	byExperiment := map[string]catalogPageCursor{}
	for _, row := range rows {
		key := strings.TrimSpace(row.Project) + "\x00" + strings.TrimSpace(row.ExperimentID)
		cursor, exists := byExperiment[key]
		if !exists || row.LatestActivityAt > cursor.At {
			cursor = catalogPageCursor{
				At: row.LatestActivityAt, Project: strings.TrimSpace(row.Project), ID: strings.TrimSpace(row.ExperimentID),
			}
			byExperiment[key] = cursor
		}
	}
	cursors := make([]catalogPageCursor, 0, len(byExperiment))
	for _, cursor := range byExperiment {
		cursors = append(cursors, cursor)
	}
	sort.Slice(cursors, func(i, j int) bool {
		if cursors[i].At != cursors[j].At {
			return cursors[i].At > cursors[j].At
		}
		if cursors[i].Project != cursors[j].Project {
			return cursors[i].Project < cursors[j].Project
		}
		return cursors[i].ID < cursors[j].ID
	})
	return cursors[len(cursors)-1], len(cursors)
}

func catalogCompactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func catalogUniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = true
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ParseKustoMetricRows(raw []byte) ([]KustoMetricRow, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '[' {
		if rows, ok, err := parseKustoFrameArray(raw); ok || err != nil {
			return rows, err
		}
		var rows []KustoMetricRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	if raw[0] == '{' && json.Valid(raw) {
		rows, err := parseKustoMetricRowsObject(raw)
		if err != nil {
			return nil, err
		}
		return rows, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	rows := []KustoMetricRow{}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row KustoMetricRow
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}

func parseKustoMetricRowsObject(raw []byte) ([]KustoMetricRow, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	for _, pair := range []struct {
		rowsKey    string
		columnsKey string
	}{
		{rowsKey: "Rows", columnsKey: "Columns"},
		{rowsKey: "rows", columnsKey: "columns"},
	} {
		if value, ok := obj[pair.rowsKey]; ok {
			if columnRaw, hasColumns := obj[pair.columnsKey]; hasColumns {
				columns, err := parseKustoColumns(columnRaw)
				if err != nil {
					return nil, err
				}
				var rawRows []json.RawMessage
				if err := json.Unmarshal(value, &rawRows); err != nil {
					return nil, err
				}
				return parseKustoTableRows(columns, rawRows)
			}
			rows, err := parseKustoObjectRows(value)
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
	}
	for _, key := range []string{"Tables", "tables"} {
		if value, ok := obj[key]; ok {
			rows, err := parseKustoTables(value)
			if err != nil {
				return nil, err
			}
			return rows, nil
		}
	}
	var row KustoMetricRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	if row.RunID == "" && row.MetricName == "" {
		return nil, fmt.Errorf("Kusto JSON object did not contain rows, Tables, or a metric row")
	}
	return []KustoMetricRow{row}, nil
}

func parseKustoObjectRows(raw json.RawMessage) ([]KustoMetricRow, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var rows []KustoMetricRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func parseKustoTables(raw json.RawMessage) ([]KustoMetricRow, error) {
	var tables []struct {
		TableName string            `json:"TableName"`
		Name      string            `json:"name"`
		Columns   []kustoColumnSpec `json:"Columns"`
		Rows      []json.RawMessage `json:"Rows"`
	}
	if err := json.Unmarshal(raw, &tables); err != nil {
		return nil, err
	}
	for _, table := range tables {
		if table.TableName != "" && table.TableName != "PrimaryResult" {
			continue
		}
		return parseKustoTableRows(table.Columns, table.Rows)
	}
	if len(tables) == 0 {
		return nil, nil
	}
	return parseKustoTableRows(tables[0].Columns, tables[0].Rows)
}

type kustoResponseFrame struct {
	FrameType string            `json:"FrameType"`
	TableKind string            `json:"TableKind"`
	TableName string            `json:"TableName"`
	TableId   *int              `json:"TableId"`
	Columns   []kustoColumnSpec `json:"Columns"`
	Rows      []json.RawMessage `json:"Rows"`
	HasErrors bool              `json:"HasErrors"`
	Cancelled bool              `json:"Cancelled"`
}

func parseKustoFrameArray(raw []byte) ([]KustoMetricRow, bool, error) {
	var frames []kustoResponseFrame
	if err := json.Unmarshal(raw, &frames); err != nil {
		return nil, false, nil
	}
	if len(frames) == 0 || frames[0].FrameType == "" {
		return nil, false, nil
	}
	if err := kustoFrameCompletionError(frames); err != nil {
		return nil, true, err
	}
	for _, frame := range frames {
		if frame.FrameType != "DataTable" {
			continue
		}
		if frame.TableKind != "" && frame.TableKind != "PrimaryResult" {
			continue
		}
		if frame.TableName != "" && frame.TableName != "PrimaryResult" && frame.TableKind == "" {
			continue
		}
		rows, err := parseKustoTableRows(frame.Columns, frame.Rows)
		return rows, true, err
	}
	// Fragmented v2 REST stream: the PrimaryResult table is split across a
	// TableHeader (Columns + TableId) and one or more TableFragment frames
	// (Rows for the matching TableId). This is what azure-kusto-go QueryToJson
	// emits when the response is progressive; the DataTable branch above then
	// only ever sees QueryProperties/QueryCompletionInformation.
	for i, frame := range frames {
		if frame.FrameType != "TableHeader" {
			continue
		}
		if frame.TableKind != "" && frame.TableKind != "PrimaryResult" {
			continue
		}
		var rawRows []json.RawMessage
		for _, next := range frames[i+1:] {
			if next.FrameType == "TableFragment" && sameKustoTableID(next.TableId, frame.TableId) {
				rawRows = append(rawRows, next.Rows...)
				continue
			}
			if next.FrameType == "TableCompletion" && sameKustoTableID(next.TableId, frame.TableId) {
				break
			}
		}
		rows, err := parseKustoTableRows(frame.Columns, rawRows)
		return rows, true, err
	}
	return nil, true, fmt.Errorf("Kusto query response did not contain a PrimaryResult table")
}

func kustoFrameCompletionError(frames []kustoResponseFrame) error {
	for _, frame := range frames {
		if frame.FrameType != "DataSetCompletion" {
			continue
		}
		if frame.Cancelled {
			return fmt.Errorf("Kusto query was cancelled")
		}
		if frame.HasErrors {
			return fmt.Errorf("Kusto query response reported errors")
		}
	}
	return nil
}

func sameKustoTableID(a, b *int) bool {
	if a == nil || b == nil {
		return true
	}
	return *a == *b
}

type kustoColumnSpec struct {
	ColumnName string `json:"ColumnName"`
	Name       string `json:"name"`
}

func parseKustoColumns(raw json.RawMessage) ([]kustoColumnSpec, error) {
	var specs []kustoColumnSpec
	if err := json.Unmarshal(raw, &specs); err == nil && len(specs) > 0 {
		return specs, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	specs = make([]kustoColumnSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, kustoColumnSpec{ColumnName: name})
	}
	return specs, nil
}

func (c kustoColumnSpec) name() string {
	if c.ColumnName != "" {
		return c.ColumnName
	}
	return c.Name
}

func parseKustoTableRows(columns []kustoColumnSpec, rawRows []json.RawMessage) ([]KustoMetricRow, error) {
	rows := make([]KustoMetricRow, 0, len(rawRows))
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		name := column.name()
		if strings.EqualFold(name, "OneApiErrors") {
			return nil, fmt.Errorf("Kusto query returned a partial failure")
		}
		names = append(names, name)
	}
	for _, raw := range rawRows {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		if raw[0] == '{' {
			var row KustoMetricRow
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, err
			}
			rows = append(rows, row)
			continue
		}
		var values []any
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		if len(values) > len(names) {
			return nil, fmt.Errorf("Kusto row has %d values but only %d columns", len(values), len(names))
		}
		rowMap := map[string]any{}
		for i, value := range values {
			if i >= len(names) || names[i] == "" {
				continue
			}
			rowMap[names[i]] = value
		}
		row, err := decodeKustoMetricRowMap(rowMap)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func decodeKustoMetricRowMap(rowMap map[string]any) (KustoMetricRow, error) {
	raw, err := json.Marshal(rowMap)
	if err != nil {
		return KustoMetricRow{}, err
	}
	var row KustoMetricRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return KustoMetricRow{}, err
	}
	return row, nil
}

func (s KustoSource) runKustoSeriesCommand(ctx context.Context, opts SeriesOptions) ([]KustoMetricRow, error) {
	query, err := s.buildKustoSeriesQuery(ctx, opts, false)
	if err != nil {
		return nil, err
	}
	return s.executeKustoQueryCommand(ctx, query)
}

func (s KustoSource) buildKustoSeriesQuery(ctx context.Context, opts SeriesOptions, raw bool) (string, error) {
	projects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return "", err
	}
	targetPoints := max(opts.MaxPoints, expkusto.MinTargetPoints)
	return expkusto.BuildTypedMetricsQuery(expkusto.MetricsQueryOptions{
		WorkspaceID:                 s.WorkspaceID,
		Projects:                    projects,
		Target:                      opts.Target,
		TargetType:                  "auto",
		RunIDs:                      []string{opts.RunID},
		MetricNames:                 []string{opts.Metric},
		StartStep:                   opts.StartStep,
		EndStep:                     opts.EndStep,
		Since:                       s.effectiveTargetSince(),
		TargetPoints:                targetPoints,
		Raw:                         raw,
		IncludeValidationMilestones: !raw,
	})
}

func (s KustoSource) executeKustoQueryCommand(ctx context.Context, query string) ([]KustoMetricRow, error) {
	if strings.TrimSpace(s.QueryCommand) == "" && s.NativeQuery != nil {
		raw, err := s.NativeQuery(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("execute Kusto query: %w", err)
		}

		rows, err := ParseKustoMetricRows([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("parse Kusto query output: %w", err)
		}
		return rows, nil
	}
	endpoint := firstNonEmptyString(s.Endpoint, expkusto.DefaultEndpoint)
	database := firstNonEmptyString(s.Database, expkusto.DefaultDatabase)
	args, queryInArgs := expandKustoCommandArgs(s.QueryArgs, endpoint, database, query)
	var stdin io.Reader
	if !queryInArgs {
		stdin = strings.NewReader(query)
	}
	out, stderr, err := kustoquery.RunCommand(ctx, s.QueryCommand, args, stdin)
	if err != nil {
		if stderr != "" {
			return nil, fmt.Errorf("execute Kusto query command: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("execute Kusto query command: %w", err)
	}
	rows, err := ParseKustoMetricRows(out)
	if err != nil {
		return nil, fmt.Errorf("parse Kusto query command output: %w", err)
	}
	return rows, nil
}

func expandKustoCommandArgs(args []string, endpoint, database, query string) ([]string, bool) {
	out := make([]string, 0, len(args))
	queryInArgs := false
	for _, arg := range args {
		replaced := strings.ReplaceAll(arg, "{endpoint}", endpoint)
		replaced = strings.ReplaceAll(replaced, "{database}", database)
		if strings.Contains(replaced, "{query}") {
			queryInArgs = true
			replaced = strings.ReplaceAll(replaced, "{query}", query)
		}
		out = append(out, replaced)
	}
	return out, queryInArgs
}

func (s KustoSource) rawProjectScope(_ context.Context, requestProject string) ([]string, error) {
	requestProject = strings.TrimSpace(requestProject)
	allowed := normalizeProjectList(s.AllowedProjects)
	if requestProject == "" {
		if len(allowed) > 0 {
			return allowed, nil
		}
		return nil, nil
	}
	if !kustoProjectAllowed(requestProject, allowed) {
		return []string{noAllowedKustoProjectMatch}, nil
	}
	return []string{requestProject}, nil
}

func kustoProjectAllowed(project string, allowed []string) bool {
	project = strings.TrimSpace(project)
	if project == "" {
		return false
	}
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == project {
			return true
		}
	}
	return false
}

func normalizeProjectList(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (s KustoSource) scopedToWorkspace(workspace string) (KustoSource, error) {
	workspace = strings.TrimSpace(workspace)
	configured := strings.TrimSpace(s.WorkspaceID)
	if configured != "" && workspace != "" && configured != workspace {
		return KustoSource{}, fmt.Errorf("workspace %q conflicts with configured Kusto workspace %q", workspace, configured)
	}
	if configured == "" && workspace != "" {
		s.WorkspaceID = workspace
	}
	return s, nil
}

func (s KustoSource) effectiveDiscoverySince() string {
	return firstNonEmptyString(strings.TrimSpace(s.DiscoverySince), defaultKustoDiscoverySince)
}

func (s KustoSource) effectiveMaxDiscoverySince() string {
	return firstNonEmptyString(strings.TrimSpace(s.MaxDiscoverySince), defaultKustoMaxDiscoverySince)
}

func (s KustoSource) effectiveTargetSince() string {
	return firstNonEmptyString(strings.TrimSpace(s.TargetSince), defaultKustoTargetSince)
}

func (s KustoSource) validateDiscoverySince(since, requestProject string) error {
	since = strings.TrimSpace(since)
	if since == "" {
		return nil
	}
	if strings.TrimSpace(requestProject) != "" || len(normalizeProjectList(s.AllowedProjects)) > 0 {
		return nil
	}
	duration, err := parseKustoLookbackDuration(since, s.effectiveNow())
	if err != nil {
		return err
	}
	maxDuration, err := parseKustoLookbackDuration(s.effectiveMaxDiscoverySince(), s.effectiveNow())
	if err != nil {
		return fmt.Errorf("max discovery since: %w", err)
	}
	if duration > maxDuration {
		return fmt.Errorf("unscoped Kusto discovery since %q exceeds max discovery since %q", since, s.effectiveMaxDiscoverySince())
	}
	return nil
}

func parseKustoLookbackDuration(value string, now time.Time) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if strings.HasSuffix(value, "d") || strings.HasSuffix(value, "w") {
		unit := value[len(value)-1:]
		amount, err := strconv.ParseFloat(strings.TrimSuffix(value, unit), 64)
		if err != nil {
			return 0, fmt.Errorf("since must be a Go duration, Nd, or Nw")
		}
		if unit == "w" {
			amount *= 7
		}
		return time.Duration(amount * float64(24*time.Hour)), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		parsed, parseErr := time.Parse(time.RFC3339, value)
		if parseErr != nil {
			return 0, fmt.Errorf("since must be RFC3339, Go duration, Nd, or Nw")
		}
		duration = now.UTC().Sub(parsed.UTC())
		if duration < 0 {
			duration = 0
		}
	}
	return duration, nil
}

func (s KustoSource) effectiveNow() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func normalizeKustoLifecycle(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "success", "successful", "completed", "complete", "done":
		return "succeeded"
	case "cancel", "canceled":
		return "cancelled"
	default:
		return value
	}
}

func kustoRunSearchMatches(run expstore.RunSearchRun, opts expstore.RunSearchOptions) bool {
	if target := strings.TrimSpace(opts.Target); target != "" && run.RunID != target && run.RunGroupID != target && run.ExperimentID != target {
		return false
	}
	if opts.Project != "" && run.Project != opts.Project {
		return false
	}
	if opts.RunGroupID != "" && run.RunGroupID != opts.RunGroupID {
		return false
	}
	if opts.State != "" && normalizeKustoLifecycle(run.State) != opts.State {
		return false
	}
	if opts.Lifecycle != "" && normalizeKustoLifecycle(run.LifecycleState) != opts.Lifecycle {
		return false
	}
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	if query != "" && !kustoRunSearchMatchesQuery(run, query) {
		return false
	}
	for key, value := range opts.Tags {
		if run.Tags[key] != value {
			return false
		}
	}
	for _, metricName := range opts.MetricNames {
		if !stringSliceContains(run.MetricNames, strings.TrimSpace(metricName)) {
			return false
		}
	}
	for _, filter := range opts.MetricFilters {
		if !kustoMetricFilterMatches(run.Metrics, filter) {
			return false
		}
	}
	return true
}

func kustoRunSearchMatchesQuery(run expstore.RunSearchRun, query string) bool {
	values := []string{run.RunID, run.Project, run.ExperimentID, run.RunGroupID, run.State, run.LifecycleState, run.Owner, run.ResultURI}
	values = append(values, run.MetricNames...)
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	for key, value := range run.Tags {
		if strings.Contains(strings.ToLower(key), query) || strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func kustoMetricFilterMatches(summaries []expstore.MetricSummaryRecord, filter expstore.MetricFilter) bool {
	for _, summary := range summaries {
		if summary.MetricName != filter.MetricName {
			continue
		}
		value, ok := kustoMetricFilterSummaryValue(summary, filter.Field)
		if !ok {
			continue
		}
		return compareKustoMetricFilterValue(value, filter.Op, filter.Value)
	}
	return false
}

func kustoMetricFilterSummaryValue(summary expstore.MetricSummaryRecord, field string) (float64, bool) {
	switch normalizedKustoMetricFilterField(field) {
	case "latest":
		return summary.LatestValue, summary.FiniteCount > 0 || summary.Count > 0 ||
			summary.LatestStep != nil || summary.UpdatedAt != ""
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

func normalizedKustoMetricFilterField(field string) string {
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

func compareKustoMetricFilterValue(left float64, op string, right float64) bool {
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

func kustoRowTags(row KustoMetricRow) map[string]string {
	raw := strings.TrimSpace(row.Tags)
	if raw == "" || raw == "{}" {
		return nil
	}
	values := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	out := map[string]string{}
	for key, value := range values {
		out[key] = fmt.Sprint(value)
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringSliceContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func (s KustoSource) sourcePath() string {
	return "kusto://typed"
}

func kustoMetricPoints(rows []KustoMetricRow, allowedRuns map[string]bool) []metricPoint {
	out := make([]metricPoint, 0, len(rows))
	for _, row := range rows {
		if row.RunID == "" || row.MetricName == "" || !allowedRuns[row.RunID] {
			continue
		}
		out = append(out, metricPoint{
			RunID:            row.RunID,
			RunGroupID:       row.RunGroupID,
			MetricName:       row.MetricName,
			Card:             metricCard(expstore.MetricRow{MetricName: row.MetricName}),
			Step:             row.Step,
			Value:            row.Value,
			Unit:             row.Unit,
			Source:           "kusto",
			SourcePointCount: row.SourcePointCount,
			Milestone:        row.ValidationMilestone,
		})
	}
	return out
}
