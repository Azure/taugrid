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
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/portalbin"
)

const defaultKustoRunStaleAfter = 15 * time.Minute
const defaultKustoDiscoverySince = "90d"
const defaultKustoMaxDiscoverySince = "365d"
const defaultKustoTargetSince = "365d"
const noAllowedKustoProjectMatch = "__tau_no_allowed_project_match__"

func KustoStorePathForIngestion(ingestion string) string {
	switch KustoIngestionOrDefault(ingestion) {
	case "remote-write":
		return "kusto://" + expkusto.DefaultRemoteWriteTable
	default:
		return "kusto://" + expkusto.DefaultProjectionTable
	}
}

func KustoIngestionOrDefault(ingestion string) string {
	switch strings.ToLower(strings.TrimSpace(ingestion)) {
	case "remote-write":
		return "remote-write"
	default:
		return "projection"
	}
}

type KustoMetricRow struct {
	WorkspaceID         string  `json:"workspace_id,omitempty"`
	Cluster             string  `json:"cluster,omitempty"`
	SourceStoreID       string  `json:"source_store_id"`
	Project             string  `json:"project"`
	ExperimentID        string  `json:"experiment_id"`
	RunGroupID          string  `json:"run_group_id"`
	RunID               string  `json:"run_id"`
	MetricName          string  `json:"metric_name"`
	Step                int64   `json:"step"`
	WallTime            string  `json:"wall_time"`
	Value               float64 `json:"value"`
	Unit                string  `json:"unit"`
	Source              string  `json:"source"`
	Split               string  `json:"split"`
	MetricFileID        string  `json:"metric_file_id"`
	MetricFilePath      string  `json:"metric_file_path"`
	SourcePointCount    int     `json:"source_point_count,omitempty"`
	ValidationMilestone bool    `json:"validation_milestone,omitempty"`
	Tags                string  `json:"tags"`
}

// UnmarshalJSON normalizes rows written before the experiment-axis rename.
func (r *KustoMetricRow) UnmarshalJSON(data []byte) error {
	type alias KustoMetricRow
	aux := struct {
		*alias
		LegacyExperimentID string `json:"question_id"`
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ExperimentID == "" {
		r.ExperimentID = aux.LegacyExperimentID
	}
	return nil
}

type KustoSource struct {
	MetricsFile       string
	Metrics           []KustoMetricRow
	StorePath         string
	Project           string
	WorkspaceID       string
	AllowedProjects   []string
	FeaturedProjects  []string
	Endpoint          string
	Database          string
	Ingestion         string
	Since             string
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

func (s KustoSource) BuildSeries(ctx context.Context, opts SeriesOptions) (SeriesDetail, error) {
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
	rows, err := s.loadSeriesRows(ctx, opts)
	if err != nil {
		return SeriesDetail{}, err
	}
	rows = filterKustoRowsByWorkspace(rows, s.WorkspaceID)
	projects, err := s.rawProjectScope(ctx, s.Project)
	if err != nil {
		return SeriesDetail{}, err
	}
	rows = filterKustoRowsByProjects(rows, projects)
	rows, _ = filterKustoRows(rows, opts.Target)
	rows = filterKustoSeriesRows(rows, opts)
	if len(rows) == 0 {
		return SeriesDetail{}, expstore.ErrNotFound
	}
	if opts.MaxMetricRows > 0 && len(rows) > opts.MaxMetricRows {
		rows = rows[:opts.MaxMetricRows]
	}

	runRows := rows
	if opts.RunID != "" {
		runRows = filterKustoRowsByRunID(rows, opts.RunID)
		if len(runRows) == 0 {
			return SeriesDetail{}, expstore.ErrNotFound
		}
	}
	groups := kustoRunGroups(runRows)
	runs, truncated := kustoRuns(runRows, opts.MaxRuns, s.effectiveNow(), s.effectiveStaleAfter())
	allowedRuns := map[string]bool{}
	for _, run := range runs {
		allowedRuns[run.RunID] = true
	}
	points := kustoMetricPoints(runRows, allowedRuns)
	chart := buildChartWithRunColorsBudgetAndInterval(points, opts.Metric, groupClassMap(groups), runColorMapForRuns(runs), opts.MaxPoints, opts.StepInterval)
	chart.StepInterval = opts.StepInterval
	warnings := []string{"source=kusto series are extrema-preserving display projections; Kusto source rows remain authoritative"}
	if truncated {
		warnings = append(warnings, fmt.Sprintf("runs truncated to %d matching runs", len(runs)))
	}
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
	if s.hasRemoteQuery() {
		rawQuery, err := s.buildKustoSeriesQuery(ctx, opts, true)
		if err != nil {
			return SeriesDetail{}, err
		}
		detail.RawQuery = rawQuery
		detail.RawQuerySource = "kusto"
	}
	return detail, nil
}

func (s KustoSource) loadSeriesRows(ctx context.Context, opts SeriesOptions) ([]KustoMetricRow, error) {
	if len(s.Metrics) > 0 {
		return s.Metrics, nil
	}
	if s.hasRemoteQuery() {
		return s.runKustoSeriesCommand(ctx, opts)
	}
	if strings.TrimSpace(s.MetricsFile) != "" {
		return LoadKustoMetricRows(s.MetricsFile)
	}
	return nil, fmt.Errorf("source=kusto has no --kusto-metrics-file, --kusto-endpoint, or --kusto-query-command configured")
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

func filterKustoRowsByRunID(rows []KustoMetricRow, runID string) []KustoMetricRow {
	out := make([]KustoMetricRow, 0, len(rows))
	for _, row := range rows {
		if row.RunID == runID {
			out = append(out, row)
		}
	}
	return out
}

func (s KustoSource) BuildSnapshot(ctx context.Context, opts Options) (Snapshot, error) {
	opts.Target = strings.TrimSpace(opts.Target)
	var err error
	s, err = s.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return Snapshot{}, err
	}
	if opts.Target == "" {
		return Snapshot{}, fmt.Errorf("dashboard target is required")
	}

	rows, err := s.loadSnapshotRows(ctx, opts)
	if err != nil {
		return Snapshot{}, err
	}
	rows = filterKustoRowsByWorkspace(rows, s.WorkspaceID)
	rawProjects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return Snapshot{}, err
	}
	rows = filterKustoRowsByProjects(rows, rawProjects)
	rows = filterKustoRowsByProjects(rows, s.effectiveProjectScope(opts.Project))
	if len(rows) == 0 {
		return Snapshot{}, expstore.ErrNotFound
	}
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	default:
	}

	filtered, targetType := filterKustoRows(rows, opts.Target)
	if len(filtered) == 0 {
		return Snapshot{}, expstore.ErrNotFound
	}
	if strings.TrimSpace(opts.Project) == "" {
		projects := kustoRowsProjects(filtered)
		if len(projects) > 1 {
			return Snapshot{}, fmt.Errorf("ambiguous Kusto target %q matched multiple projects (%s); add project= to the request", opts.Target, strings.Join(projects, ", "))
		}
	}
	metricOptionRows := filtered
	warnings := []string{fmt.Sprintf("source=kusto uses %s scalar rows; local-only artifacts, observations, and run context are unavailable unless exported separately", strings.TrimPrefix(KustoStorePathForIngestion(s.Ingestion), "kusto://"))}
	if !opts.SkipMetricCatalog && opts.Mode != SnapshotModeSummary {
		discoveryRows, err := s.loadRowsForMetricOptions(ctx, opts)
		if err != nil {
			return Snapshot{}, err
		}
		if len(discoveryRows) > 0 {
			discoveryRows = filterKustoRowsByWorkspace(discoveryRows, s.WorkspaceID)
			discoveryRows = filterKustoRowsByProjects(discoveryRows, rawProjects)
			discoveryRows = filterKustoRowsByProjects(discoveryRows, s.effectiveProjectScope(opts.Project))
			if scopedRows, _ := filterKustoRows(discoveryRows, opts.Target); len(scopedRows) > 0 {
				metricOptionRows = scopedRows
			}
		}
	}
	if opts.MaxMetricRows > 0 && len(filtered) > opts.MaxMetricRows {
		warnings = append(warnings, fmt.Sprintf("metric points truncated to %d of %d Kusto rows", opts.MaxMetricRows, len(filtered)))
		filtered = filtered[:opts.MaxMetricRows]
	}

	catalogRows := filtered
	if len(metricOptionRows) > 0 {
		catalogRows = metricOptionRows
	}
	now := s.effectiveNow()
	staleAfter := s.effectiveStaleAfter()
	groups := kustoRunGroups(catalogRows)
	runs, runsTruncated := kustoRuns(catalogRows, opts.MaxRuns, now, staleAfter)
	if runsTruncated {
		warnings = append(warnings, fmt.Sprintf("runs truncated to %d matching runs", len(runs)))
	}
	runColors := runColorMapForRuns(runs)
	applyRunColors(runs, runColors)
	allowedRuns := map[string]bool{}
	for _, run := range runs {
		allowedRuns[run.RunID] = true
	}
	points := kustoMetricPoints(filtered, allowedRuns)
	catalogPoints := kustoMetricPoints(catalogRows, allowedRuns)
	groupClasses := groupClassMap(groups)
	metricOptions := buildMetricOptionsFromPoints(catalogPoints, opts.Metric)
	experiment := kustoExperiment(filtered, opts.Target, targetType)
	if catalogExperiment := kustoExperiment(catalogRows, opts.Target, targetType); catalogExperiment != nil {
		experiment = catalogExperiment
	}
	status := kustoStatus(s.sourcePath(), opts.Target, targetType, experiment, groups, runs, catalogRows)
	if opts.Mode == SnapshotModeSummary {
		actions := kustoActions(s.sourcePath(), opts.Target, targetType, experiment, "")
		summary := buildExperimentSummary(opts.Target, targetType, experiment, groups, runs, nil, ChartView{}, nil, "", actions.NextCommand)
		snapshot := Snapshot{
			SchemaVersion: SchemaVersion,
			PayloadMode:   string(SnapshotModeSummary),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     s.sourcePath(),
			Target:        opts.Target,
			TargetType:    targetType,
			Manifest:      kustoManifest(experiment, filtered),
			Status:        status,
			Summary:       summary,
			Experiment:    experiment,
			RunGroups:     groups,
			Runs:          runs,
			MetricOptions: metricOptions,
			Actions:       actions,
			SeedCoverage:  summary.SeedCoverage,
			Warnings:      warnings,
		}
		if len(points) == 0 {
			snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
		}
		return snapshot, nil
	}

	if opts.Mode == SnapshotModeMetric {
		metric := requestedMetric(points, opts.Metric)
		metricPoints := filterMetricPoints(points, metric)
		cards := summarizeCards(metricPoints, groupClasses)
		chart := buildChartWithRunColors(metricPoints, metric, groupClasses, runColors)
		metricOptions = markMetricOptionSelected(metricOptions, chart.MetricName)
		sweep := buildSweepWithRunColors(metricPoints, chart.MetricName, runs, nil, groupClasses, runColors)
		decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
		bestGroup := decision.BestGroupID
		actions := kustoActions(s.sourcePath(), opts.Target, targetType, experiment, chart.MetricName)
		summary := buildExperimentSummary(opts.Target, targetType, experiment, groups, runs, decision.Cards, decisionChart(chart, decision.MetricName), nil, bestGroup, kustoActions(s.sourcePath(), opts.Target, targetType, experiment, decision.MetricName).NextCommand)
		if summary.NextCommand != "" {
			actions.NextCommand = summary.NextCommand
		}
		compare := buildCompareInsights(decision.Points, decision.MetricName, runs, nil, nil, nil, bestGroup)
		snapshot := Snapshot{
			SchemaVersion: SchemaVersion,
			PayloadMode:   string(SnapshotModeMetric),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     s.sourcePath(),
			Target:        opts.Target,
			TargetType:    targetType,
			Manifest:      kustoManifest(experiment, filtered),
			Status:        status,
			Summary:       summary,
			Experiment:    experiment,
			RunGroups:     groups,
			Runs:          runs,
			Cards:         cards,
			Chart:         chart,
			MetricOptions: metricOptions,
			Sweep:         sweep,
			Compare:       compare,
			Actions:       actions,
			BestGroupID:   bestGroup,
			SeedCoverage:  summary.SeedCoverage,
			Warnings:      warnings,
		}
		if len(points) == 0 {
			snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
		}
		return snapshot, nil
	}
	cards := summarizeCards(points, groupClasses)
	chart := buildChartWithRunColors(points, opts.Metric, groupClasses, runColors)
	metricOptions = markMetricOptionSelected(metricOptions, chart.MetricName)
	sweep := buildSweepWithRunColors(points, chart.MetricName, runs, nil, groupClasses, runColors)
	decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
	bestGroup := decision.BestGroupID
	actions := kustoActions(s.sourcePath(), opts.Target, targetType, experiment, chart.MetricName)
	summary := buildExperimentSummary(opts.Target, targetType, experiment, groups, runs, decision.Cards, decisionChart(chart, decision.MetricName), nil, bestGroup, kustoActions(s.sourcePath(), opts.Target, targetType, experiment, decision.MetricName).NextCommand)
	if summary.NextCommand != "" {
		actions.NextCommand = summary.NextCommand
	}
	compare := buildCompareInsights(decision.Points, decision.MetricName, runs, nil, nil, nil, bestGroup)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     s.sourcePath(),
		Target:        opts.Target,
		TargetType:    targetType,
		Manifest:      kustoManifest(experiment, filtered),
		Status:        status,
		Summary:       summary,
		Experiment:    experiment,
		RunGroups:     groups,
		Runs:          runs,
		Cards:         cards,
		Chart:         chart,
		MetricOptions: metricOptions,
		Sweep:         sweep,
		Compare:       compare,
		Actions:       actions,
		BestGroupID:   bestGroup,
		SeedCoverage:  summary.SeedCoverage,
		Warnings:      warnings,
	}
	if len(points) == 0 {
		snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
	}
	return snapshot, nil
}

func (s KustoSource) SearchExperiments(ctx context.Context, opts expstore.ExperimentSearchOptions) (expstore.ExperimentSearchResult, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	var err error
	s, err = s.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	opts.Project = strings.TrimSpace(opts.Project)
	opts.Lifecycle = strings.ToLower(strings.TrimSpace(opts.Lifecycle))
	if opts.Lifecycle == "success" || opts.Lifecycle == "successful" || opts.Lifecycle == "completed" {
		opts.Lifecycle = "succeeded"
	}
	if opts.Limit < 0 {
		return expstore.ExperimentSearchResult{}, fmt.Errorf("limit must be non-negative")
	}
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	if strings.TrimSpace(opts.Since) == "" && s.hasRemoteQuery() {
		opts.Since = s.effectiveDiscoverySince()
	}
	if s.hasRemoteQuery() {
		if err := s.validateDiscoverySince(opts.Since, opts.Project); err != nil {
			return expstore.ExperimentSearchResult{}, err
		}
	}
	rows, warnings, err := s.loadRowsForExperimentSearch(ctx, opts)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	rows = filterKustoRowsByWorkspace(rows, s.WorkspaceID)
	rawProjects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	rows = filterKustoRowsByProjects(rows, rawProjects)
	if opts.Since != "" {
		var sinceWarnings []string
		rows, sinceWarnings, err = filterKustoRowsSince(rows, opts.Since)
		if err != nil {
			return expstore.ExperimentSearchResult{}, err
		}
		warnings = append(warnings, sinceWarnings...)
	}
	rows = filterKustoRowsByProjects(rows, s.effectiveProjectScope(opts.Project))
	summaries := kustoExperimentSummaries(rows, opts, s.sourcePath(), s.effectiveNow(), s.effectiveStaleAfter())
	if opts.Lifecycle != "" {
		warnings = append(warnings, "source=kusto derives lifecycle from tau/run_status markers plus metric freshness; queued runs without metrics are not visible")
		summaries = filterKustoExperimentSummariesByLifecycle(summaries, opts.Lifecycle)
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if summaries[i].LatestRunAt != summaries[j].LatestRunAt {
			return summaries[i].LatestRunAt > summaries[j].LatestRunAt
		}
		return summaries[i].ExperimentID < summaries[j].ExperimentID
	})
	truncated := len(summaries) > opts.Limit
	if truncated {
		summaries = summaries[:opts.Limit]
	}
	return expstore.ExperimentSearchResult{
		SchemaVersion: expstore.ExperimentSearchSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     s.sourcePath(),
		Total:         len(summaries),
		Truncated:     truncated,
		Experiments:   summaries,
		Warnings:      warnings,
	}, nil
}

func (s KustoSource) SearchRuns(ctx context.Context, opts expstore.RunSearchOptions) (expstore.RunSearchResult, error) {
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	var err error
	s, err = s.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return expstore.RunSearchResult{}, err
	}
	opts.Project = strings.TrimSpace(opts.Project)
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.State = normalizeKustoLifecycle(opts.State)
	opts.Lifecycle = normalizeKustoLifecycle(opts.Lifecycle)
	if opts.Limit < 0 {
		return expstore.RunSearchResult{}, fmt.Errorf("limit must be non-negative")
	}
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	if strings.TrimSpace(opts.Since) == "" && s.hasRemoteQuery() {
		opts.Since = s.effectiveTargetSince()
	}
	rows, warnings, err := s.loadRowsForRunSearch(ctx, opts)
	if err != nil {
		return expstore.RunSearchResult{}, err
	}
	rows = filterKustoRowsByWorkspace(rows, s.WorkspaceID)
	rawProjects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return expstore.RunSearchResult{}, err
	}
	rows = filterKustoRowsByProjects(rows, rawProjects)
	if opts.Since != "" {
		var sinceWarnings []string
		rows, sinceWarnings, err = filterKustoRowsSince(rows, opts.Since)
		if err != nil {
			return expstore.RunSearchResult{}, err
		}
		warnings = append(warnings, sinceWarnings...)
	}
	rows = filterKustoRowsByProjects(rows, s.effectiveProjectScope(opts.Project))
	runs := kustoRunSearchRuns(rows, opts, s.sourcePath(), s.effectiveNow(), s.effectiveStaleAfter())
	sort.SliceStable(runs, func(i, j int) bool {
		left := firstNonEmptyString(runs[i].CompletedAt, runs[i].StartedAt, runs[i].CreatedAt)
		right := firstNonEmptyString(runs[j].CompletedAt, runs[j].StartedAt, runs[j].CreatedAt)
		if left != right {
			return left > right
		}
		return runs[i].RunID < runs[j].RunID
	})
	truncated := len(runs) > opts.Limit
	total := len(runs)
	if truncated {
		runs = runs[:opts.Limit]
	}
	return expstore.RunSearchResult{
		SchemaVersion: expstore.RunSearchSchemaVersion,
		GeneratedAt:   s.effectiveNow().UTC().Format(time.RFC3339),
		StorePath:     s.sourcePath(),
		Target:        strings.TrimSpace(opts.Target),
		Total:         total,
		Truncated:     truncated,
		Runs:          runs,
		Warnings:      warnings,
	}, nil
}

func LoadKustoMetricRows(path string) ([]KustoMetricRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKustoMetricRows(raw)
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

func parseKustoFrameArray(raw []byte) ([]KustoMetricRow, bool, error) {
	var frames []struct {
		FrameType string            `json:"FrameType"`
		TableKind string            `json:"TableKind"`
		TableName string            `json:"TableName"`
		TableId   *int              `json:"TableId"`
		Columns   []kustoColumnSpec `json:"Columns"`
		Rows      []json.RawMessage `json:"Rows"`
	}
	if err := json.Unmarshal(raw, &frames); err != nil {
		return nil, false, nil
	}
	if len(frames) == 0 || frames[0].FrameType == "" {
		return nil, false, nil
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
	return nil, true, nil
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
		names = append(names, column.name())
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

func (s KustoSource) loadRows(ctx context.Context, opts Options) ([]KustoMetricRow, error) {
	if len(s.Metrics) > 0 {
		return s.Metrics, nil
	}
	if s.hasRemoteQuery() {
		return s.runKustoQueryCommand(ctx, opts)
	}
	if strings.TrimSpace(s.MetricsFile) != "" {
		return LoadKustoMetricRows(s.MetricsFile)
	}
	return nil, nil
}

func (s KustoSource) loadSnapshotRows(ctx context.Context, opts Options) ([]KustoMetricRow, error) {
	if opts.Mode == SnapshotModeSummary && s.shouldQueryMetricCatalog(opts) {
		return s.runKustoMetricOptionsCommand(ctx, opts)
	}
	return s.loadRows(ctx, opts)
}

func (s KustoSource) loadRowsForMetricOptions(ctx context.Context, opts Options) ([]KustoMetricRow, error) {
	if len(s.Metrics) > 0 || !s.hasRemoteQuery() {
		return nil, nil
	}
	if strings.TrimSpace(opts.Metric) == "" {
		return nil, nil
	}
	return s.runKustoMetricOptionsCommand(ctx, opts)
}

func (s KustoSource) loadRowsForExperimentSearch(ctx context.Context, opts expstore.ExperimentSearchOptions) ([]KustoMetricRow, []string, error) {
	if len(s.Metrics) > 0 {
		return s.Metrics, nil, nil
	}
	if s.hasRemoteQuery() {
		rows, err := s.runKustoExperimentSearchCommand(ctx, opts)
		return rows, nil, err
	}
	if strings.TrimSpace(s.MetricsFile) != "" {
		rows, err := LoadKustoMetricRows(s.MetricsFile)
		return rows, nil, err
	}
	return nil, []string{"source=kusto has no --kusto-metrics-file, --kusto-endpoint, or --kusto-query-command configured"}, nil
}

func (s KustoSource) shouldQueryMetricCatalog(opts Options) bool {
	return strings.TrimSpace(opts.Metric) != "" &&
		len(s.Metrics) == 0 &&
		s.hasRemoteQuery()
}

func (s KustoSource) loadRowsForRunSearch(ctx context.Context, opts expstore.RunSearchOptions) ([]KustoMetricRow, []string, error) {
	if len(s.Metrics) > 0 {
		return s.Metrics, nil, nil
	}
	if s.hasRemoteQuery() {
		searchOpts := expstore.ExperimentSearchOptions{
			Project:     opts.Project,
			MetricNames: opts.MetricNames,
			Since:       opts.Since,
		}
		rows, err := s.runKustoExperimentSearchCommand(ctx, searchOpts)
		return rows, nil, err
	}
	if strings.TrimSpace(s.MetricsFile) != "" {
		rows, err := LoadKustoMetricRows(s.MetricsFile)
		return rows, nil, err
	}
	return nil, []string{"source=kusto has no --kusto-metrics-file, --kusto-endpoint, or --kusto-query-command configured"}, nil
}

func (s KustoSource) runKustoQueryCommand(ctx context.Context, opts Options) ([]KustoMetricRow, error) {
	projects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return nil, err
	}
	targetPoints := s.TargetPoints
	if targetPoints == 0 {
		targetPoints = expkusto.DefaultTargetPoints
	}
	query, err := expkusto.BuildMetricsQuery(expkusto.MetricsQueryOptions{
		WorkspaceID:  s.WorkspaceID,
		Projects:     projects,
		Target:       opts.Target,
		TargetType:   "auto",
		MetricNames:  metricNamesWithKustoRunStatus(metricNameFilter(opts.Metric)),
		Since:        s.effectiveTargetSince(),
		Ingestion:    s.Ingestion,
		TargetPoints: targetPoints,
	})
	if err != nil {
		return nil, err
	}
	return s.executeKustoQueryCommand(ctx, query)
}

func (s KustoSource) runKustoSeriesCommand(ctx context.Context, opts SeriesOptions) ([]KustoMetricRow, error) {
	query, err := s.buildKustoSeriesQuery(ctx, opts, false)
	if err != nil {
		return nil, err
	}
	return s.executeKustoQueryCommand(ctx, query)
}

func (s KustoSource) buildKustoSeriesQuery(ctx context.Context, opts SeriesOptions, raw bool) (string, error) {
	projects, err := s.rawProjectScope(ctx, s.Project)
	if err != nil {
		return "", err
	}
	runIDs := []string{}
	if opts.RunID != "" {
		runIDs = append(runIDs, opts.RunID)
	}
	targetPoints := max(opts.MaxPoints, expkusto.MinTargetPoints)
	query, err := expkusto.BuildMetricsQuery(expkusto.MetricsQueryOptions{
		WorkspaceID:                 s.WorkspaceID,
		Projects:                    projects,
		Target:                      opts.Target,
		TargetType:                  "auto",
		RunIDs:                      runIDs,
		MetricNames:                 []string{opts.Metric},
		StartStep:                   opts.StartStep,
		EndStep:                     opts.EndStep,
		Since:                       s.effectiveTargetSince(),
		Ingestion:                   s.Ingestion,
		TargetPoints:                targetPoints,
		Raw:                         raw,
		IncludeValidationMilestones: !raw,
	})
	if err != nil {
		return "", err
	}
	return query, nil
}

func (s KustoSource) runKustoMetricOptionsCommand(ctx context.Context, opts Options) ([]KustoMetricRow, error) {
	projects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return nil, err
	}
	targetPoints := s.TargetPoints
	if targetPoints == 0 {
		targetPoints = expkusto.DefaultTargetPoints
	}
	query, err := expkusto.BuildExperimentSearchQuery(expkusto.MetricsQueryOptions{
		WorkspaceID:  s.WorkspaceID,
		Projects:     projects,
		Target:       opts.Target,
		TargetType:   "auto",
		Since:        s.effectiveTargetSince(),
		Ingestion:    s.Ingestion,
		TargetPoints: targetPoints,
	})
	if err != nil {
		return nil, err
	}
	return s.executeKustoQueryCommand(ctx, query)
}

func (s KustoSource) runKustoExperimentSearchCommand(ctx context.Context, opts expstore.ExperimentSearchOptions) ([]KustoMetricRow, error) {
	projects, err := s.rawProjectScope(ctx, opts.Project)
	if err != nil {
		return nil, err
	}
	targetPoints := s.TargetPoints
	if targetPoints == 0 {
		targetPoints = expkusto.DefaultTargetPoints
	}
	query, err := expkusto.BuildExperimentSearchQuery(expkusto.MetricsQueryOptions{
		WorkspaceID:  s.WorkspaceID,
		Projects:     projects,
		MetricNames:  metricNamesWithKustoRunStatus(opts.MetricNames),
		Since:        firstNonEmptyString(strings.TrimSpace(opts.Since), s.effectiveDiscoverySince()),
		Ingestion:    s.Ingestion,
		TargetPoints: targetPoints,
		Limit:        opts.Limit,
	})
	if err != nil {
		return nil, err
	}
	return s.executeKustoQueryCommand(ctx, query)
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

func metricNameFilter(metric string) []string {
	metric = strings.TrimSpace(metric)
	if metric == "" {
		return nil
	}
	return []string{metric}
}

func metricNamesWithKustoRunStatus(metricNames []string) []string {
	if len(metricNames) == 0 {
		return nil
	}
	out := make([]string, 0, len(metricNames)+1)
	seen := map[string]bool{}
	for _, metricName := range metricNames {
		metricName = strings.TrimSpace(metricName)
		if metricName == "" || seen[metricName] {
			continue
		}
		seen[metricName] = true
		out = append(out, metricName)
	}
	if !seen[expkusto.RunStatusMetricName] {
		out = append(out, expkusto.RunStatusMetricName)
	}
	return out
}

func (s KustoSource) effectiveProjectScope(requestProject string) []string {
	requestProject = strings.TrimSpace(requestProject)
	allowed := normalizeProjectList(s.AllowedProjects)
	if len(allowed) == 0 {
		if requestProject == "" {
			return nil
		}
		return []string{requestProject}
	}
	if requestProject == "" {
		return allowed
	}
	for _, project := range allowed {
		if project == requestProject {
			return []string{requestProject}
		}
	}
	return []string{noAllowedKustoProjectMatch}
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

func filterKustoRowsByProjects(rows []KustoMetricRow, projects []string) []KustoMetricRow {
	projects = normalizeProjectList(projects)
	if len(projects) == 0 {
		return rows
	}
	allowed := map[string]bool{}
	for _, project := range projects {
		allowed[project] = true
	}
	out := make([]KustoMetricRow, 0, len(rows))
	for _, row := range rows {
		if allowed[strings.TrimSpace(row.Project)] {
			out = append(out, row)
		}
	}
	return out
}

func filterKustoRowsByWorkspace(rows []KustoMetricRow, workspaceID string) []KustoMetricRow {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return rows
	}
	out := make([]KustoMetricRow, 0, len(rows))
	for _, row := range rows {
		rowWorkspace := strings.TrimSpace(row.WorkspaceID)
		if rowWorkspace == "" {
			var tags map[string]string
			if json.Unmarshal([]byte(row.Tags), &tags) == nil {
				rowWorkspace = strings.TrimSpace(tags[exptelemetry.TauWorkspaceTag])
			}
		}
		// Untagged rows are NOT admitted here, unlike the local store. ADX
		// TauExpMetrics is a fleet-wide table shared by every workspace, so an
		// untagged row is of unknown ownership rather than implicitly ours.
		if rowWorkspace == workspaceID {
			out = append(out, row)
		}
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
	return firstNonEmptyString(strings.TrimSpace(s.DiscoverySince), strings.TrimSpace(s.Since), defaultKustoDiscoverySince)
}

func (s KustoSource) effectiveMaxDiscoverySince() string {
	return firstNonEmptyString(strings.TrimSpace(s.MaxDiscoverySince), defaultKustoMaxDiscoverySince)
}

func (s KustoSource) effectiveTargetSince() string {
	return firstNonEmptyString(strings.TrimSpace(s.TargetSince), strings.TrimSpace(s.Since), defaultKustoTargetSince)
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

func (s KustoSource) effectiveStaleAfter() time.Duration {
	if s.StaleAfter > 0 {
		return s.StaleAfter
	}
	return defaultKustoRunStaleAfter
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

func kustoExperimentSummaries(rows []KustoMetricRow, opts expstore.ExperimentSearchOptions, sourcePath string, now time.Time, staleAfter time.Duration) []expstore.ExperimentSummary {
	byExperiment := map[string][]KustoMetricRow{}
	for _, row := range rows {
		experimentID := firstNonEmptyString(strings.TrimSpace(row.ExperimentID), strings.TrimSpace(row.RunGroupID))
		if experimentID == "" {
			continue
		}
		key := strings.TrimSpace(row.Project) + "\x00" + experimentID
		byExperiment[key] = append(byExperiment[key], row)
	}
	ids := make([]string, 0, len(byExperiment))
	for id := range byExperiment {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]expstore.ExperimentSummary, 0, len(ids))
	for _, id := range ids {
		experimentRows := byExperiment[id]
		if !kustoExperimentMatches(experimentRows, opts, sourcePath) {
			continue
		}
		out = append(out, kustoExperimentSummary(firstKustoExperimentID(experimentRows), experimentRows, now, staleAfter))
	}
	return out
}

func firstKustoExperimentID(rows []KustoMetricRow) string {
	for _, row := range rows {
		if value := strings.TrimSpace(row.ExperimentID); value != "" {
			return value
		}
	}
	for _, row := range rows {
		if value := strings.TrimSpace(row.RunGroupID); value != "" {
			return value
		}
	}
	return ""
}

func kustoExperimentMatches(rows []KustoMetricRow, opts expstore.ExperimentSearchOptions, sourcePath string) bool {
	if len(rows) == 0 {
		return false
	}
	if opts.Project != "" && !kustoRowsContain(rows, func(row KustoMetricRow) bool { return row.Project == opts.Project }) {
		return false
	}
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	if query != "" && !kustoRowsContain(rows, func(row KustoMetricRow) bool { return kustoRowMatchesQuery(row, query) }) {
		return false
	}
	for _, metricName := range opts.MetricNames {
		metricName = strings.TrimSpace(metricName)
		if metricName == "" {
			continue
		}
		if !kustoRowsContain(rows, func(row KustoMetricRow) bool { return row.MetricName == metricName }) {
			return false
		}
	}
	for _, key := range sortedKustoTagKeys(opts.Tags) {
		expected := opts.Tags[key]
		if !kustoRowsContain(rows, func(row KustoMetricRow) bool { return kustoRowTags(row)[key] == expected }) {
			return false
		}
	}
	if len(opts.MetricFilters) > 0 && !kustoExperimentMatchesMetricFilters(rows, opts.MetricFilters, sourcePath) {
		return false
	}
	return true
}

func kustoRowsContain(rows []KustoMetricRow, fn func(KustoMetricRow) bool) bool {
	for _, row := range rows {
		if fn(row) {
			return true
		}
	}
	return false
}

func kustoRowMatchesQuery(row KustoMetricRow, query string) bool {
	for _, value := range []string{row.Project, row.ExperimentID, row.RunGroupID, row.RunID, row.MetricName} {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	for key, value := range kustoRowTags(row) {
		if strings.Contains(strings.ToLower(key), query) || strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func kustoExperimentSummary(experimentID string, rows []KustoMetricRow, now time.Time, staleAfter time.Duration) expstore.ExperimentSummary {
	runIDs := map[string]bool{}
	groupIDs := map[string]bool{}
	metricNames := map[string]bool{}
	stateCounts := map[string]int{}
	lifecycleCounts := map[string]int{}
	latest := ""
	project := ""
	runViews, _ := kustoRuns(rows, 0, now, staleAfter)
	for _, run := range runViews {
		if run.RunID != "" && !runIDs[run.RunID] {
			runIDs[run.RunID] = true
			stateCounts[run.State]++
			lifecycleState := run.LifecycleState
			if lifecycleState == "" {
				lifecycleState = run.State
			}
			lifecycleCounts[lifecycleState]++
		}
	}
	for _, row := range rows {
		if project == "" {
			project = row.Project
		}
		if row.RunGroupID != "" {
			groupIDs[row.RunGroupID] = true
		}
		if row.MetricName != "" && !isKustoRunStatusMetric(row) {
			metricNames[row.MetricName] = true
		}
		if row.WallTime > latest {
			latest = row.WallTime
		}
	}
	if latest == "" {
		latest = time.Now().UTC().Format(time.RFC3339)
	}
	return expstore.ExperimentSummary{
		ExperimentRecord: expstore.ExperimentRecord{
			ExperimentID: experimentID,
			Project:      project,
			Name:         experimentID,
			Source:       "kusto",
			CreatedAt:    firstKustoTime(rows),
			UpdatedAt:    latest,
		},
		RunCount:        len(runIDs),
		RunGroupCount:   len(groupIDs),
		StateCounts:     stateCounts,
		LifecycleCounts: lifecycleCounts,
		LatestRunAt:     latest,
		MetricNames:     sortedKustoBoolKeys(metricNames),
	}
}

func kustoExperimentMatchesMetricFilters(rows []KustoMetricRow, filters []expstore.MetricFilter, sourcePath string) bool {
	summariesByRun := kustoMetricSummariesByRun(rows, sourcePath)
	for _, filter := range filters {
		matchedFilter := false
		for _, summaries := range summariesByRun {
			if kustoMetricFilterMatches(summaries, filter) {
				matchedFilter = true
				break
			}
		}
		if !matchedFilter {
			return false
		}
	}
	return true
}

func kustoMetricSummariesByRun(rows []KustoMetricRow, sourcePath string) map[string][]expstore.MetricSummaryRecord {
	sourcePath = firstNonEmptyString(sourcePath, KustoStorePathForIngestion(""))
	rowsByRun := map[string][]expstore.MetricRow{}
	for _, row := range rows {
		if strings.TrimSpace(row.RunID) == "" || strings.TrimSpace(row.MetricName) == "" {
			continue
		}
		if isKustoRunStatusMetric(row) {
			continue
		}
		rowsByRun[row.RunID] = append(rowsByRun[row.RunID], kustoMetricRow(row))
	}
	out := map[string][]expstore.MetricSummaryRecord{}
	now := time.Now().UTC().Format(time.RFC3339)
	for runID, metricRows := range rowsByRun {
		file := expstore.MetricFileRecord{
			FileID:        "kusto:" + runID,
			Path:          sourcePath,
			Format:        "kusto",
			SchemaVersion: expstore.MetricSchemaVersion,
			RunID:         runID,
			CreatedAt:     now,
		}
		out[runID] = expstore.SummarizeMetricRows(file, metricRows)
	}
	return out
}

func kustoMetricRow(row KustoMetricRow) expstore.MetricRow {
	step := row.Step
	unit := row.Unit
	split := row.Split
	metricRow := expstore.MetricRow{
		Project:    row.Project,
		RunGroupID: row.RunGroupID,
		RunID:      row.RunID,
		MetricName: row.MetricName,
		Step:       &step,
		Value:      row.Value,
		Unit:       &unit,
		Source:     "kusto",
		Split:      &split,
		Tags:       row.Tags,
	}
	if wallTime := kustoWallTimeMicros(row.WallTime); wallTime != nil {
		metricRow.WallTime = wallTime
	}
	return metricRow
}

func kustoRunSearchRuns(rows []KustoMetricRow, opts expstore.RunSearchOptions, sourcePath string, now time.Time, staleAfter time.Duration) []expstore.RunSearchRun {
	byID := map[string][]KustoMetricRow{}
	for _, row := range rows {
		if strings.TrimSpace(row.RunID) == "" {
			continue
		}
		byID[row.RunID] = append(byID[row.RunID], row)
	}
	summariesByRun := kustoMetricSummariesByRun(rows, sourcePath)
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]expstore.RunSearchRun, 0, len(ids))
	for _, id := range ids {
		view := kustoRunView(id, byID[id], now, staleAfter)
		tags := view.Tags
		summaries := summariesByRun[id]
		run := expstore.RunRecord{
			RunID:        view.RunID,
			Project:      view.Project,
			ExperimentID: firstKustoExperimentID(byID[id]),
			RunGroupID:   view.RunGroupID,
			State:        view.State,
			CreatedAt:    view.CreatedAt,
			StartedAt:    view.StartedAt,
			CompletedAt:  view.CompletedAt,
			ResultURI:    view.ResultURI,
		}
		lifecycleState := view.LifecycleState
		successful := view.Successful
		reasons := view.SuccessReasons
		if lifecycleState == "succeeded" {
			classification := expstore.ClassifyRun(run, tags, summaries, expstore.SuccessOptions{
				Tags:          tags,
				MetricFilters: opts.MetricFilters,
				MinStep:       opts.MinStep,
			})
			lifecycleState = classification.LifecycleState
			successful = classification.Successful
			reasons = classification.Reasons
		}
		item := expstore.RunSearchRun{
			RunRecord:      run,
			LifecycleState: lifecycleState,
			Successful:     successful,
			SuccessReasons: reasons,
			Tags:           tags,
			MetricNames:    view.MetricNames,
			Metrics:        summaries,
		}
		if !kustoRunSearchMatches(item, opts) {
			continue
		}
		out = append(out, item)
	}
	return out
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
	for _, key := range sortedKustoTagKeys(opts.Tags) {
		if run.Tags[key] != opts.Tags[key] {
			return false
		}
	}
	for _, metricName := range opts.MetricNames {
		if !kustoStringSliceContains(run.MetricNames, strings.TrimSpace(metricName)) {
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
	values := []string{run.RunID, run.Project, run.RunGroupID, run.State, run.LifecycleState, run.Owner, run.ResultURI}
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

func filterKustoExperimentSummariesByLifecycle(summaries []expstore.ExperimentSummary, lifecycle string) []expstore.ExperimentSummary {
	lifecycle = normalizeKustoLifecycle(lifecycle)
	if lifecycle == "" {
		return summaries
	}
	out := make([]expstore.ExperimentSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.LifecycleCounts[lifecycle] > 0 {
			out = append(out, summary)
		}
	}
	return out
}

func isKustoRunStatusMetric(row KustoMetricRow) bool {
	return strings.TrimSpace(row.MetricName) == expkusto.RunStatusMetricName
}

func firstKustoDataRow(rows []KustoMetricRow) KustoMetricRow {
	for _, row := range rows {
		if !isKustoRunStatusMetric(row) {
			return row
		}
	}
	return KustoMetricRow{}
}

func latestKustoStatusRow(rows []KustoMetricRow) (KustoMetricRow, bool) {
	var latest KustoMetricRow
	ok := false
	for _, row := range rows {
		if !isKustoRunStatusMetric(row) {
			continue
		}
		if !ok || row.WallTime > latest.WallTime {
			latest = row
			ok = true
		}
	}
	return latest, ok
}

func kustoRunStatusState(row KustoMetricRow) string {
	tags := kustoRowTags(row)
	if state := normalizeKustoLifecycle(tags[expkusto.RunStatusStateTag]); state != "" {
		switch state {
		case "succeeded", "failed", "cancelled":
			return state
		}
	}
	switch {
	case row.Value > 0:
		return "succeeded"
	case row.Value < -1:
		return "cancelled"
	case row.Value < 0:
		return "failed"
	default:
		return "running"
	}
}

func kustoRunStatusReasons(row KustoMetricRow) []string {
	tags := kustoRowTags(row)
	reasons := []string{"run emitted terminal status marker"}
	if reason := strings.TrimSpace(tags[expkusto.RunStatusReasonTag]); reason != "" {
		reasons = append(reasons, reason)
	}
	if message := strings.TrimSpace(tags[expkusto.RunStatusMessageTag]); message != "" {
		reasons = append(reasons, message)
	}
	return reasons
}

func kustoMergedTags(rows []KustoMetricRow) map[string]string {
	tags := map[string]string{}
	for _, row := range rows {
		for key, value := range kustoRowTags(row) {
			tags[key] = value
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

func kustoMetricNames(rows []KustoMetricRow) []string {
	names := map[string]bool{}
	for _, row := range rows {
		metricName := strings.TrimSpace(row.MetricName)
		if metricName == "" || isKustoRunStatusMetric(row) {
			continue
		}
		names[metricName] = true
	}
	return sortedKustoBoolKeys(names)
}

func firstKustoMetricTime(rows []KustoMetricRow) string {
	return boundKustoMetricTime(rows, true)
}

func lastKustoMetricTime(rows []KustoMetricRow) string {
	return boundKustoMetricTime(rows, false)
}

func boundKustoMetricTime(rows []KustoMetricRow, min bool) string {
	out := ""
	for _, row := range rows {
		if isKustoRunStatusMetric(row) || row.WallTime == "" {
			continue
		}
		if out == "" || (min && row.WallTime < out) || (!min && row.WallTime > out) {
			out = row.WallTime
		}
	}
	return out
}

func kustoStringSliceContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
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

func filterKustoRowsSince(rows []KustoMetricRow, since string) ([]KustoMetricRow, []string, error) {
	cutoff, err := parseKustoSince(since, time.Now())
	if err != nil {
		return nil, nil, err
	}
	if cutoff.IsZero() {
		return rows, nil, nil
	}
	out := make([]KustoMetricRow, 0, len(rows))
	skippedInvalidTime := 0
	for _, row := range rows {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(row.WallTime))
		if err != nil {
			skippedInvalidTime++
			continue
		}
		if !t.Before(cutoff) {
			out = append(out, row)
		}
	}
	warnings := []string{}
	if skippedInvalidTime > 0 {
		warnings = append(warnings, fmt.Sprintf("source=kusto skipped %d rows with unparsable wall_time while applying since", skippedInvalidTime))
	}
	return out, warnings, nil
}

func parseKustoSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("since must be RFC3339, Go duration, or Nd")
		}
		return now.UTC().Add(-time.Duration(days * float64(24*time.Hour))), nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return now.UTC().Add(-duration), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("since must be RFC3339, Go duration, or Nd")
	}
	return parsed.UTC(), nil
}

func kustoWallTimeMicros(value string) *int64 {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	micros := t.UnixMicro()
	return &micros
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

func sortedKustoTagKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedKustoBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s KustoSource) sourcePath() string {
	if strings.TrimSpace(s.StorePath) != "" {
		return s.StorePath
	}
	if strings.TrimSpace(s.MetricsFile) != "" {
		return s.MetricsFile
	}
	return KustoStorePathForIngestion(s.Ingestion)
}

func filterKustoRows(rows []KustoMetricRow, target string) ([]KustoMetricRow, string) {
	for _, match := range []struct {
		typ string
		fn  func(KustoMetricRow) bool
	}{
		{typ: "run", fn: func(row KustoMetricRow) bool { return row.RunID == target }},
		{typ: "run_group", fn: func(row KustoMetricRow) bool { return row.RunGroupID == target }},
		{typ: "experiment", fn: func(row KustoMetricRow) bool { return row.ExperimentID == target }},
	} {
		out := []KustoMetricRow{}
		for _, row := range rows {
			if match.fn(row) {
				out = append(out, row)
			}
		}
		if len(out) > 0 {
			return out, match.typ
		}
	}
	return nil, ""
}

func kustoRowsProjects(rows []KustoMetricRow) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, row := range rows {
		project := strings.TrimSpace(row.Project)
		if project == "" || seen[project] {
			continue
		}
		seen[project] = true
		out = append(out, project)
	}
	sort.Strings(out)
	return out
}

func kustoExperiment(rows []KustoMetricRow, target, targetType string) *expstore.ExperimentRecord {
	if len(rows) == 0 {
		return nil
	}
	experimentID := firstNonEmptyString(rows[0].ExperimentID, rows[0].RunGroupID)
	if experimentID == "" && targetType == "experiment" {
		experimentID = target
	}
	if experimentID == "" {
		return nil
	}
	return &expstore.ExperimentRecord{
		ExperimentID: experimentID,
		Project:      rows[0].Project,
		Name:         experimentID,
		CreatedAt:    firstKustoTime(rows),
		UpdatedAt:    lastKustoTime(rows),
	}
}

func kustoRunGroups(rows []KustoMetricRow) []RunGroupView {
	byID := map[string]KustoMetricRow{}
	for _, row := range rows {
		if row.RunGroupID == "" {
			continue
		}
		if _, ok := byID[row.RunGroupID]; !ok {
			byID[row.RunGroupID] = row
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]RunGroupView, 0, len(ids))
	for _, id := range ids {
		row := byID[id]
		out = append(out, RunGroupView{
			RunGroupID: id,
			Project:    row.Project,
			Name:       id,
			GroupClass: cssClass("group", id),
			CreatedAt:  firstKustoTime(rows),
			UpdatedAt:  lastKustoTime(rows),
		})
	}
	return out
}

func kustoRuns(rows []KustoMetricRow, maxRuns int, now time.Time, staleAfter time.Duration) ([]RunView, bool) {
	byID := map[string][]KustoMetricRow{}
	for _, row := range rows {
		if row.RunID == "" {
			continue
		}
		byID[row.RunID] = append(byID[row.RunID], row)
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	truncated := false
	if maxRuns > 0 && len(ids) > maxRuns {
		ids = ids[:maxRuns]
		truncated = true
	}
	out := make([]RunView, 0, len(ids))
	for _, id := range ids {
		out = append(out, kustoRunView(id, byID[id], now, staleAfter))
	}
	return out, truncated
}

func kustoRunView(runID string, rows []KustoMetricRow, now time.Time, staleAfter time.Duration) RunView {
	row := firstKustoDataRow(rows)
	if row.RunID == "" {
		row = rows[0]
	}
	for _, candidate := range rows {
		row.SourceStoreID = firstNonEmptyString(row.SourceStoreID, candidate.SourceStoreID)
		row.WorkspaceID = firstNonEmptyString(row.WorkspaceID, candidate.WorkspaceID)
		row.Cluster = firstNonEmptyString(row.Cluster, candidate.Cluster)
		row.Project = firstNonEmptyString(row.Project, candidate.Project)
		row.RunGroupID = firstNonEmptyString(row.RunGroupID, candidate.RunGroupID)
	}
	classification := classifyKustoRun(rows, now, staleAfter)
	tags := kustoMergedTags(rows)
	view := RunView{
		RunID:          runID,
		Source:         "kusto",
		SourceStoreID:  row.SourceStoreID,
		WorkspaceID:    row.WorkspaceID,
		Cluster:        row.Cluster,
		Project:        row.Project,
		RunGroupID:     row.RunGroupID,
		State:          classification.Truth.legacyState(),
		Successful:     classification.Successful,
		SuccessReasons: classification.Reasons,
		CreatedAt:      firstKustoTime(rows),
		UpdatedAt:      firstNonEmptyString(lastKustoMetricTime(rows), classification.CompletedAt, lastKustoTime(rows)),
		StartedAt:      classification.StartedAt,
		CompletedAt:    classification.CompletedAt,
		ResultURI:      firstNonEmptyString(tags[expkusto.RunStatusArtifactURITag], tags[expkusto.RunStatusCheckpointURITag]),
		Tags:           tags,
		MetricNames:    kustoMetricNames(rows),
		ObserveCLI:     "",
	}
	applyLifecycleTruth(&view, classification.Truth)
	return view
}

type kustoRunClassification struct {
	Truth       LifecycleTruth
	Successful  bool
	Reasons     []string
	StartedAt   string
	CompletedAt string
}

func classifyKustoRun(rows []KustoMetricRow, now time.Time, staleAfter time.Duration) kustoRunClassification {
	statusRow, hasStatus := latestKustoStatusRow(rows)
	firstMetricAt := firstKustoMetricTime(rows)
	latestMetricAt := lastKustoMetricTime(rows)
	outcome := ""
	explicitReason := ""
	explicitSource := ""
	terminalAt := time.Time{}
	controlPlaneAt := time.Time{}
	reasons := []string{}
	if hasStatus {
		statusState := kustoRunStatusState(statusRow)
		if terminalOutcome(statusState) != "" {
			outcome = statusState
			reasons = kustoRunStatusReasons(statusRow)
			explicitReason = strings.Join(reasons, ": ")
			explicitSource = "tau_terminal_marker"
			terminalAt = parseLifecycleTime(statusRow.WallTime)
		} else {
			controlPlaneAt = parseLifecycleTime(statusRow.WallTime)
		}
	}
	truth := ResolveLifecycle(LifecycleEvidence{
		ExplicitOutcome:      outcome,
		ExplicitReason:       explicitReason,
		ExplicitSource:       explicitSource,
		TerminalAt:           terminalAt,
		LatestMetricAt:       parseLifecycleTime(latestMetricAt),
		LatestControlPlaneAt: controlPlaneAt,
		Now:                  now,
		NotRespondingAfter:   staleAfter,
	})
	if len(reasons) == 0 {
		reasons = []string{truth.Reason}
	}
	return kustoRunClassification{
		Truth:      truth,
		Successful: truth.OutcomeState == "succeeded",
		Reasons:    reasons,
		StartedAt:  firstNonEmptyString(firstMetricAt, firstKustoTime(rows)),
		CompletedAt: func() string {
			if truth.OutcomeState == "" {
				return ""
			}
			return firstNonEmptyString(statusRow.WallTime, lastKustoTime(rows))
		}(),
	}
}

func kustoMetricPoints(rows []KustoMetricRow, allowedRuns map[string]bool) []metricPoint {
	out := make([]metricPoint, 0, len(rows))
	for _, row := range rows {
		if row.RunID == "" || row.MetricName == "" || !allowedRuns[row.RunID] {
			continue
		}
		if isKustoRunStatusMetric(row) {
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

func kustoStatus(sourcePath, target, targetType string, experiment *expstore.ExperimentRecord, groups []RunGroupView, runs []RunView, rows []KustoMetricRow) expstore.Status {
	stateCounts := map[string]int{}
	lifecycleCounts := map[string]int{}
	for _, run := range runs {
		stateCounts[run.State]++
		lifecycle := run.LifecycleState
		if lifecycle == "" {
			lifecycle = run.State
		}
		lifecycleCounts[lifecycle]++
	}
	status := expstore.Status{
		StorePath:       sourcePath,
		Target:          target,
		TargetType:      targetType,
		Runs:            len(runs),
		RunGroups:       len(groups),
		StateCounts:     stateCounts,
		LifecycleCounts: lifecycleCounts,
		MetricFiles:     1,
		LatestEventAt:   lastKustoTime(rows),
	}
	if experiment != nil {
		status.Experiment = experiment
	}
	if targetType == "run_group" {
		for _, group := range groups {
			if group.RunGroupID == target {
				status.RunGroup = &expstore.RunGroup{
					RunGroupID: group.RunGroupID,
					Project:    group.Project,
					Name:       group.Name,
					CreatedAt:  group.CreatedAt,
					UpdatedAt:  group.UpdatedAt,
				}
				break
			}
		}
	}
	if targetType == "run" {
		for _, run := range runs {
			if run.RunID == target {
				status.Run = map[string]any{
					"run_id":       run.RunID,
					"project":      run.Project,
					"run_group_id": run.RunGroupID,
					"state":        run.State,
					"created_at":   run.CreatedAt,
				}
				break
			}
		}
	}
	return status
}

func kustoManifest(experiment *expstore.ExperimentRecord, rows []KustoMetricRow) expstore.Manifest {
	project := ""
	if experiment != nil {
		project = experiment.Project
	} else if len(rows) > 0 {
		project = rows[0].Project
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return expstore.Manifest{
		SchemaVersion: expstore.SchemaVersion,
		Kind:          "tau.exp.kusto",
		Project:       project,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func kustoActions(sourcePath, target, targetType string, experiment *expstore.ExperimentRecord, metric string) ActionView {
	queryTarget := target
	queryFlag := "--experiment"
	if targetType == "run_group" {
		queryFlag = "--group"
	} else if targetType == "run" {
		queryFlag = "--run"
	} else if experiment != nil {
		queryTarget = experiment.ExperimentID
	}
	next := portalbin.ExperimentCmd + " kusto metrics-query " + queryFlag + " " + shellQuote(queryTarget)
	if metric != "" {
		next += " --metric " + shellQuote(metric)
	}
	return ActionView{
		CopyCLI:     next,
		CopySQL:     next,
		ObserveCLI:  "",
		NextCommand: next,
		StorePath:   sourcePath,
	}
}

func firstKustoTime(rows []KustoMetricRow) string {
	return boundKustoTime(rows, true)
}

func lastKustoTime(rows []KustoMetricRow) string {
	return boundKustoTime(rows, false)
}

func boundKustoTime(rows []KustoMetricRow, first bool) string {
	var best time.Time
	for _, row := range rows {
		t, err := time.Parse(time.RFC3339Nano, row.WallTime)
		if err != nil {
			continue
		}
		if best.IsZero() || (first && t.Before(best)) || (!first && t.After(best)) {
			best = t
		}
	}
	if best.IsZero() {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return best.UTC().Format(time.RFC3339)
}
