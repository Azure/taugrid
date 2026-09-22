// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestKustoCatalogSearchUsesStableFunctions(t *testing.T) {
	var queries []string
	source := KustoSource{
		WorkspaceID:     "workspace-a",
		AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, query string) (string, error) {
			queries = append(queries, query)
			if strings.Contains(query, exptelemetry.RunCatalogRowsFunction+"()") {
				return `[{
					"catalog_version":"v1",
					"workspace_id":"workspace-a",
					"project":"project-a",
					"experiment_id":"experiment-a",
					"run_group_id":"group-a",
					"run_id":"run-a",
					"first_activity_at":"2026-09-18T18:00:00Z",
					"latest_activity_at":"2026-09-18T18:10:00Z",
					"state":"succeeded",
					"completion_time":"2026-09-18T18:10:00Z",
					"has_metrics":true,
					"has_lifecycle":true
				},{
					"catalog_version":"v1",
					"workspace_id":"workspace-a",
					"project":"project-a",
					"experiment_id":"lifecycle-only",
					"run_group_id":"queued-group",
					"run_id":"queued-run",
					"first_activity_at":"2026-09-18T18:05:00Z",
					"latest_activity_at":"2026-09-18T18:05:00Z",
					"state":"queued",
					"has_metrics":false,
					"has_lifecycle":true
				}]`, nil
			}
			return `[{
				"catalog_version":"v1",
				"workspace_id":"workspace-a",
				"project":"project-a",
				"experiment_id":"experiment-a",
				"run_group_id":"group-a",
				"run_id":"run-a",
				"metric_name":"train/loss",
				"first_activity_at":"2026-09-18T18:00:00Z",
				"latest_activity_at":"2026-09-18T18:10:00Z",
				"min_step":1,
				"max_step":10,
				"latest_step":10,
				"latest_value":0.25,
				"step":10,
				"wall_time":"2026-09-18T18:10:00Z",
				"value":0.25
			}]`, nil
		},
	}

	experiments, err := source.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Workspace: "workspace-a",
		Project:   "project-a",
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(experiments.Experiments) != 2 ||
		experiments.Experiments[0].ExperimentID != "experiment-a" ||
		experiments.Experiments[1].ExperimentID != "lifecycle-only" {
		t.Fatalf("unexpected catalog experiments: %+v", experiments)
	}

	runs, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a",
		Project:   "project-a",
		Target:    "experiment-a",
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != "run-a" ||
		runs.Runs[0].LifecycleState != "succeeded" ||
		len(runs.Runs[0].Metrics) != 1 ||
		runs.Runs[0].Metrics[0].MetricName != "train/loss" {
		t.Fatalf("unexpected catalog runs: %+v", runs)
	}

	joined := strings.Join(queries, "\n")
	if !strings.Contains(joined, exptelemetry.SeriesCatalogRowsFunction+"()") ||
		!strings.Contains(joined, exptelemetry.RunCatalogRowsFunction+"()") {
		t.Fatalf("queries did not use both stable catalog functions:\n%s", joined)
	}
}

func TestKustoCatalogAndSeriesKeepDuplicateRunIDsProjectScoped(t *testing.T) {
	source := KustoSource{
		WorkspaceID:     "workspace-a",
		AllowedProjects: []string{"project-a", "project-b"},
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.RunCatalogRowsFunction+"()") {
				return `[
					{"workspace_id":"workspace-a","project":"project-a","experiment_id":"experiment-a","run_id":"shared-run","first_activity_at":"2026-09-18T18:00:00Z","state":"running","has_lifecycle":true},
					{"workspace_id":"workspace-a","project":"project-b","experiment_id":"experiment-b","run_id":"shared-run","first_activity_at":"2026-09-18T18:01:00Z","state":"queued","has_lifecycle":true}
				]`, nil
			}

			return `[
				{"workspace_id":"workspace-a","project":"project-a","experiment_id":"experiment-a","run_id":"shared-run","metric_name":"loss","latest_step":1,"latest_value":2},
				{"workspace_id":"workspace-a","project":"project-b","experiment_id":"experiment-b","run_id":"shared-run","metric_name":"accuracy","latest_step":1,"latest_value":3}
			]`, nil
		},
	}
	runs, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{Workspace: "workspace-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 2 || runs.Runs[0].Project == runs.Runs[1].Project {
		t.Fatalf("duplicate run IDs were collapsed across projects: %+v", runs.Runs)
	}
	for _, run := range runs.Runs {
		if len(run.MetricNames) != 1 {
			t.Fatalf("project metrics were combined for %+v", run)
		}
	}

	query, err := source.buildKustoSeriesQuery(context.Background(), SeriesOptions{
		Workspace: "workspace-a", Project: "project-b", Target: "experiment-b",
		RunID: "shared-run", Metric: "accuracy", MaxPoints: 100,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `'project-b'`) || strings.Contains(query, `'project-a'`) {
		t.Fatalf("series query was not project scoped:\n%s", query)
	}

	detail, err := (KustoSource{
		WorkspaceID: "workspace-a", Project: "project-a",
		AllowedProjects: []string{"project-a", "project-b"},
		NativeQuery: func(_ context.Context, _ string) (string, error) {
			return `[{"workspace_id":"workspace-a","project":"project-b","experiment_id":"experiment-b","run_id":"shared-run","metric_name":"accuracy","step":1,"wall_time":"2026-09-18T18:01:00Z","value":3}]`, nil
		},
	}).BuildTypedSeries(context.Background(), SeriesOptions{
		Workspace: "workspace-a", Project: "project-b", Target: "experiment-b",
		RunID: "shared-run", Metric: "accuracy", MaxPoints: 100,
	})
	if err != nil || !detail.Chart.HasData {
		t.Fatalf("request-scoped project series detail=%+v err=%v", detail, err)
	}
}

func TestKustoCatalogRejectsOutOfScopeResponseRows(t *testing.T) {
	source := KustoSource{
		WorkspaceID: "workspace-a", AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, _ string) (string, error) {
			return `[{"workspace_id":"workspace-b","project":"project-a","run_id":"run-a","latest_activity_at":"2026-09-18T12:00:00Z"}]`, nil
		},
	}
	if _, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Limit: 10,
	}); err == nil || !strings.Contains(err.Error(), "outside configured scope") {
		t.Fatalf("out-of-scope catalog row error = %v", err)
	}
}

func TestCatalogExperimentSummariesKeepProjectsDistinct(t *testing.T) {
	runs := []expstore.RunSearchRun{
		{RunRecord: expstore.RunRecord{Project: "project-a", ExperimentID: "shared", RunID: "run-a"}},
		{RunRecord: expstore.RunRecord{Project: "project-b", ExperimentID: "shared", RunID: "run-b"}},
	}
	summaries := catalogExperimentSummaries(nil, runs, "kusto")
	if len(summaries) != 2 {
		t.Fatalf("same experiment ID across projects was merged: %+v", summaries)
	}
	projects := map[string]bool{}
	for _, summary := range summaries {
		projects[summary.Project] = true
	}
	if !projects["project-a"] || !projects["project-b"] {
		t.Fatalf("project-scoped summaries=%+v", summaries)
	}
}

func TestCatalogRunQueryMatchesExperimentID(t *testing.T) {
	run := expstore.RunSearchRun{RunRecord: expstore.RunRecord{
		RunID: "run-a", Project: "project-a", ExperimentID: "experiment-search-target",
	}}
	if !kustoRunSearchMatches(run, expstore.RunSearchOptions{Query: "search-target"}) {
		t.Fatalf("experiment ID query did not match run: %+v", run)
	}
}

func TestCatalogLatestMetricFilterUsesKnownLatestValue(t *testing.T) {
	summary := expstore.MetricSummaryRecord{
		MetricName: "train/loss", LatestValue: 0.25, UpdatedAt: "2026-09-18T18:10:00Z",
	}
	if !kustoMetricFilterMatches([]expstore.MetricSummaryRecord{summary}, expstore.MetricFilter{
		MetricName: "train/loss", Field: "latest", Op: "<", Value: 0.5,
	}) {
		t.Fatalf("known catalog latest value was treated as unavailable: %+v", summary)
	}
}

func TestKustoCatalogOrderingMatchesCursorOrdering(t *testing.T) {
	source := KustoSource{
		WorkspaceID: "workspace-a",
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.SeriesCatalogRowsFunction+"()") {
				return `[]`, nil
			}
			return `[
				{"workspace_id":"workspace-a","project":"project-b","experiment_id":"experiment-a","run_id":"run-a","created_time":"2026-09-18T12:00:00Z","latest_activity_at":"2026-09-18T12:00:00Z","state":"succeeded","has_lifecycle":true},
				{"workspace_id":"workspace-a","project":"project-a","experiment_id":"experiment-z","run_id":"run-z","created_time":"2026-09-18T12:00:00Z","latest_activity_at":"2026-09-18T12:00:00Z","state":"succeeded","has_lifecycle":true}
			]`, nil
		},
	}
	runs, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].Project != "project-a" {
		t.Fatalf("run truncation order diverged from cursor order: %+v", runs.Runs)
	}
	experiments, err := source.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Workspace: "workspace-a", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(experiments.Experiments) != 1 || experiments.Experiments[0].Project != "project-a" {
		t.Fatalf("experiment truncation order diverged from cursor order: %+v", experiments.Experiments)
	}
}

func TestKustoCatalogOrderingUsesChronologicalTimestamps(t *testing.T) {
	source := KustoSource{
		WorkspaceID: "workspace-a",
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.SeriesCatalogRowsFunction+"()") {
				return `[]`, nil
			}
			return `[
				{"workspace_id":"workspace-a","project":"project-a","experiment_id":"whole","run_id":"whole","created_time":"2026-09-18T12:00:00Z","latest_activity_at":"2026-09-18T12:00:00Z","state":"succeeded","has_lifecycle":true},
				{"workspace_id":"workspace-a","project":"project-a","experiment_id":"fractional","run_id":"fractional","created_time":"2026-09-18T12:00:00.5Z","latest_activity_at":"2026-09-18T12:00:00.5Z","state":"succeeded","has_lifecycle":true}
			]`, nil
		},
	}
	runs, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].RunID != "fractional" {
		t.Fatalf("run truncation was not chronological: %+v", runs.Runs)
	}
	experiments, err := source.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Workspace: "workspace-a", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(experiments.Experiments) != 1 || experiments.Experiments[0].ExperimentID != "fractional" {
		t.Fatalf("experiment truncation was not chronological: %+v", experiments.Experiments)
	}
}

func TestKustoCatalogRunListingUsesExactExperimentPredicate(t *testing.T) {
	var runQuery string
	source := KustoSource{
		WorkspaceID: "workspace-a",
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.SeriesCatalogRowsFunction+"()") {
				return `[]`, nil
			}
			runQuery = query
			return `[]`, nil
		},
	}
	_, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Target: "experiment-a", ExactExperimentID: "experiment-a", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runQuery, "| where experiment_id == 'experiment-a'") ||
		strings.Contains(runQuery, "run_group_id == 'experiment-a'") {
		t.Fatalf("run listing did not use an exact experiment predicate:\n%s", runQuery)
	}
}

func TestKustoCatalogRunIdentityIncludesExperiment(t *testing.T) {
	runRows := []KustoMetricRow{
		{Project: "project-a", ExperimentID: "experiment-a", RunID: "shared", State: "succeeded"},
		{Project: "project-a", ExperimentID: "experiment-b", RunID: "shared", State: "succeeded"},
	}
	metricRows := []KustoMetricRow{
		{Project: "project-a", ExperimentID: "experiment-a", RunID: "shared", MetricName: "loss-a", LatestValue: catalogFloat64Pointer(1)},
		{Project: "project-a", ExperimentID: "experiment-b", RunID: "shared", MetricName: "loss-b", LatestValue: catalogFloat64Pointer(2)},
	}
	runs := catalogRunSearchRuns(runRows, metricRows, expstore.RunSearchOptions{})
	if len(runs) != 2 {
		t.Fatalf("same project/run ID across experiments was collapsed: %+v", runs)
	}
	for _, run := range runs {
		if len(run.MetricNames) != 1 || run.MetricNames[0] != "loss-"+strings.TrimPrefix(run.ExperimentID, "experiment-") {
			t.Fatalf("metrics crossed experiment identity: %+v", run)
		}
	}
}

func TestKustoLegacyMetricEvaluatorSupportsAllStatistics(t *testing.T) {
	step1, step9 := int64(1), int64(9)
	summary := expstore.MetricSummaryRecord{
		MetricName: "loss", Count: 10, FiniteCount: 8, NonFiniteCount: 2,
		MinValue: 0.25, MaxValue: 4, LatestValue: 1,
		MinStep: &step1, MaxStep: &step9, LatestStep: &step9,
	}
	tests := []struct {
		field string
		want  float64
	}{
		{"min", 0.25}, {"max", 4}, {"count", 10}, {"finite_count", 8},
		{"non_finite_count", 2}, {"latest", 1}, {"min_step", 1}, {"max_step", 9}, {"latest_step", 9},
	}
	for _, test := range tests {
		got, ok := kustoMetricFilterSummaryValue(summary, test.field)
		if !ok || got != test.want {
			t.Fatalf("%s = %v, %v; want %v, true", test.field, got, ok, test.want)
		}
	}
}

func TestKustoCatalogMinStepAppliesBeforeLifecycleFilter(t *testing.T) {
	maxStep := int64(5)
	runRows := []KustoMetricRow{{
		Project: "project-a", ExperimentID: "experiment-a", RunID: "run-a", State: "succeeded",
	}}
	metricRows := []KustoMetricRow{{
		Project: "project-a", ExperimentID: "experiment-a", RunID: "run-a",
		MetricName: "loss", MaxStep: &maxStep, LatestStep: &maxStep, LatestValue: catalogFloat64Pointer(1),
	}}
	required := int64(10)
	runs := catalogRunSearchRuns(runRows, metricRows, expstore.RunSearchOptions{
		Lifecycle: "succeeded", MinStep: &required,
	})
	if len(runs) != 0 {
		t.Fatalf("run below MinStep passed succeeded lifecycle filter: %+v", runs)
	}
	allRuns := catalogRunSearchRuns(runRows, metricRows, expstore.RunSearchOptions{MinStep: &required})
	if len(allRuns) != 1 || allRuns[0].LifecycleState != "incomplete" || allRuns[0].Successful {
		t.Fatalf("MinStep did not reclassify typed run before filtering: %+v", allRuns)
	}
}

func TestKustoCatalogLatestFilterKeepsSucceededRun(t *testing.T) {
	latestStep := int64(10)
	runRows := []KustoMetricRow{{
		Project: "project-a", ExperimentID: "experiment-a", RunID: "run-a", State: "succeeded",
	}}
	metricRows := []KustoMetricRow{{
		Project: "project-a", ExperimentID: "experiment-a", RunID: "run-a",
		MetricName: "loss", LatestStep: &latestStep, LatestValue: catalogFloat64Pointer(0.25),
	}}
	runs := catalogRunSearchRuns(runRows, metricRows, expstore.RunSearchOptions{
		Lifecycle: "succeeded",
		MetricFilters: []expstore.MetricFilter{{
			MetricName: "loss", Field: "latest", Op: "<", Value: 0.5,
		}},
	})
	if len(runs) != 1 || runs[0].LifecycleState != "succeeded" || !runs[0].Successful {
		t.Fatalf("authoritative catalog latest filter removed succeeded run: %+v", runs)
	}
}

func TestKustoCatalogMetricNameDoesNotHideFilterOrMinStepMetrics(t *testing.T) {
	maxLossStep, maxAccuracyStep := int64(1), int64(20)
	source := KustoSource{
		WorkspaceID: "workspace-a", AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.RunCatalogRowsFunction+"()") {
				return `[{"workspace_id":"workspace-a","project":"project-a","experiment_id":"experiment-a","run_id":"run-a","created_time":"2026-09-18T18:00:00Z","latest_activity_at":"2026-09-18T18:10:00Z","state":"succeeded","has_metrics":true,"has_lifecycle":true}]`, nil
			}
			rows := []KustoMetricRow{{
				WorkspaceID: "workspace-a", Project: "project-a", ExperimentID: "experiment-a",
				RunID: "run-a", MetricName: "loss", MaxStep: &maxLossStep, LatestStep: &maxLossStep,
				LatestValue: catalogFloat64Pointer(1),
			}}
			if !strings.Contains(query, "metric_name in ('loss')") {
				rows = append(rows, KustoMetricRow{
					WorkspaceID: "workspace-a", Project: "project-a", ExperimentID: "experiment-a",
					RunID: "run-a", MetricName: "accuracy", MaxStep: &maxAccuracyStep, LatestStep: &maxAccuracyStep,
					LatestValue: catalogFloat64Pointer(0.9),
				})
			}
			raw, err := json.Marshal(rows)
			return string(raw), err
		},
	}
	minStep := int64(10)
	result, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Project: "project-a", Limit: 10,
		MetricNames: []string{"loss"}, MinStep: &minStep,
		MetricFilters: []expstore.MetricFilter{{
			MetricName: "accuracy", Field: "latest", Op: ">", Value: 0.8,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || len(result.Runs[0].MetricNames) != 2 {
		t.Fatalf("combined metric filters lost enrichment: %+v", result.Runs)
	}
}

func TestKustoFileCatalogPaginationAppliesCursorBeforeLimit(t *testing.T) {
	base := time.Date(2026, 9, 18, 18, 0, 0, 0, time.UTC)
	runRows := make([]KustoMetricRow, 0, 1001)
	experimentRows := make([]KustoMetricRow, 0, 1001)
	for i := range 1001 {
		at := base.Add(-time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		runRows = append(runRows, KustoMetricRow{
			Project: "project-a", ExperimentID: "experiment-a", RunID: fmt.Sprintf("run-%04d", i),
			MetricName: "loss", Step: int64(i), WallTime: at, Value: float64(i),
		})
		experimentRows = append(experimentRows, KustoMetricRow{
			Project: "project-a", ExperimentID: fmt.Sprintf("experiment-%04d", i), RunID: fmt.Sprintf("run-%04d", i),
			MetricName: "loss", Step: int64(i), WallTime: at, Value: float64(i),
		})
	}

	runSource := KustoSource{MetricsFile: "configured.json", Metrics: runRows}
	firstRuns, err := runSource.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Project: "project-a", Target: "experiment-a", ExactExperimentID: "experiment-a", Limit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRuns.Runs) != 1000 || !firstRuns.Truncated {
		t.Fatalf("first run page count=%d truncated=%v", len(firstRuns.Runs), firstRuns.Truncated)
	}
	lastRun := firstRuns.Runs[len(firstRuns.Runs)-1]
	secondRuns, err := runSource.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Project: "project-a", Target: "experiment-a", ExactExperimentID: "experiment-a", Limit: 1000,
		CursorAt: lastRun.CreatedAt, CursorID: lastRun.Project + "\x00" + lastRun.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondRuns.Runs) != 1 || secondRuns.Runs[0].RunID != "run-1000" {
		t.Fatalf("second run page=%+v", secondRuns.Runs)
	}

	experimentSource := KustoSource{MetricsFile: "configured.json", Metrics: experimentRows}
	firstExperiments, err := experimentSource.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Project: "project-a", Limit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstExperiments.Experiments) != 1000 || !firstExperiments.Truncated {
		t.Fatalf("first experiment page count=%d truncated=%v", len(firstExperiments.Experiments), firstExperiments.Truncated)
	}
	lastExperiment := firstExperiments.Experiments[len(firstExperiments.Experiments)-1]
	secondExperiments, err := experimentSource.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Project: "project-a", Limit: 1000, CursorAt: lastExperiment.LatestRunAt,
		CursorID: lastExperiment.Project + "\x00" + lastExperiment.ExperimentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondExperiments.Experiments) != 1 ||
		secondExperiments.Experiments[0].ExperimentID != "experiment-1000" {
		t.Fatalf("second experiment page=%+v", secondExperiments.Experiments)
	}
}

func catalogFloat64Pointer(value float64) *float64 {
	return &value
}

func TestKustoCatalogExactRunAndUnsupportedStatistics(t *testing.T) {
	var query string
	calls := 0
	source := KustoSource{
		WorkspaceID: "workspace-a",
		NativeQuery: func(_ context.Context, generated string) (string, error) {
			calls++
			query = generated
			return `[]`, nil
		},
	}
	if _, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", ExactRunID: "needle", Limit: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "run_id in ('needle')") ||
		strings.Contains(query, "experiment_id == 'needle' or") {
		t.Fatalf("exact run query was not source-filtered:\n%s", query)
	}
	calls = 0
	_, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Limit: 10,
		MetricFilters: []expstore.MetricFilter{{
			MetricName: "train/loss", Field: "count", Op: ">", Value: 0,
		}},
	})
	if !errors.Is(err, expstore.ErrInvalidArgument) || calls != 0 {
		t.Fatalf("unsupported statistic err=%v calls=%d", err, calls)
	}
}

func TestKustoCatalogRejectsMalformedSinceBeforeQuery(t *testing.T) {
	calls := 0
	source := KustoSource{
		AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, _ string) (string, error) {
			calls++
			return `[]`, nil
		},
	}
	_, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Project: "project-a", Since: "last Tuesday", Limit: 10,
	})
	if !errors.Is(err, expstore.ErrInvalidArgument) || calls != 0 {
		t.Fatalf("malformed since err=%v calls=%d", err, calls)
	}
}

func TestKustoCatalogRunCursorFiltersAtSource(t *testing.T) {
	var query string
	source := KustoSource{
		WorkspaceID:     "workspace-a",
		AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, generated string) (string, error) {
			query = generated
			return `[]`, nil
		},
	}

	_, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Target: "experiment-a", Limit: 1000,
		CursorAt: "2026-09-18T18:10:00Z", CursorID: "project-a\x00run-1000",
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor := strings.Index(query, "| where cursor_sort_at <")
	take := strings.Index(query, "| take 1000")
	if cursor < 0 || take < 0 || cursor > take {
		t.Fatalf("cursor was not applied before the source limit:\n%s", query)
	}
}

func TestKustoCatalogRunSearchScansPastFilteredSourcePage(t *testing.T) {
	firstPage := make([]map[string]any, 0, 1000)
	for i := range 1000 {
		firstPage = append(firstPage, map[string]any{
			"workspace_id": "workspace-a", "project": "project-a", "experiment_id": "experiment-a",
			"run_id": fmt.Sprintf("run-%04d", i), "created_time": fmt.Sprintf("2026-09-18T%02d:%02d:00Z", 23-(i/60)%24, i%60),
			"latest_activity_at": "2026-09-18T23:00:00Z", "state": "queued", "has_lifecycle": true,
		})
	}
	firstRaw, err := json.Marshal(firstPage)
	if err != nil {
		t.Fatal(err)
	}
	runCalls := 0
	source := KustoSource{
		WorkspaceID: "workspace-a", AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.SeriesCatalogRowsFunction+"()") {
				return `[]`, nil
			}
			runCalls++
			if runCalls == 1 {
				return string(firstRaw), nil
			}
			if !strings.Contains(query, "| where cursor_sort_at <") {
				t.Fatalf("second source page omitted cursor:\n%s", query)
			}
			return `[{"workspace_id":"workspace-a","project":"project-a","experiment_id":"experiment-a","run_id":"older-match","created_time":"2026-09-17T00:00:00Z","latest_activity_at":"2026-09-17T00:00:00Z","state":"succeeded","has_lifecycle":true}]`, nil
		},
	}
	result, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "workspace-a", Target: "experiment-a", Lifecycle: "succeeded", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runCalls != 2 || len(result.Runs) != 1 || result.Runs[0].RunID != "older-match" {
		t.Fatalf("runCalls=%d result=%+v", runCalls, result)
	}
}

func TestKustoCatalogExperimentSearchScansPastFilteredSourcePage(t *testing.T) {
	firstPage := make([]map[string]any, 0, 1000)
	for i := range 1000 {
		firstPage = append(firstPage, map[string]any{
			"workspace_id": "workspace-a", "project": "project-a",
			"experiment_id": fmt.Sprintf("experiment-%04d", i), "run_id": fmt.Sprintf("run-%04d", i),
			"latest_activity_at": "2026-09-18T23:00:00Z", "state": "succeeded", "has_lifecycle": true,
		})
	}
	firstRaw, err := json.Marshal(firstPage)
	if err != nil {
		t.Fatal(err)
	}
	runCalls := 0
	source := KustoSource{
		WorkspaceID: "workspace-a", AllowedProjects: []string{"project-a"},
		NativeQuery: func(_ context.Context, query string) (string, error) {
			if strings.Contains(query, exptelemetry.SeriesCatalogRowsFunction+"()") {
				return `[]`, nil
			}
			runCalls++
			if runCalls == 1 {
				return string(firstRaw), nil
			}
			if !strings.Contains(query, "| where experiment_cursor_at <") {
				t.Fatalf("second experiment page omitted cursor:\n%s", query)
			}
			return `[{"workspace_id":"workspace-a","project":"project-a","experiment_id":"needle-experiment","run_id":"older-match","latest_activity_at":"2026-09-17T00:00:00Z","state":"succeeded","has_lifecycle":true}]`, nil
		},
	}
	result, err := source.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Workspace: "workspace-a", Query: "needle", Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runCalls != 2 || len(result.Experiments) != 1 ||
		result.Experiments[0].ExperimentID != "needle-experiment" {
		t.Fatalf("runCalls=%d result=%+v", runCalls, result)
	}
}
