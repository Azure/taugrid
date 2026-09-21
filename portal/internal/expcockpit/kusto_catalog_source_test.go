// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
}

func TestKustoCatalogRequiresLiveQueryTransport(t *testing.T) {
	source := KustoSource{MetricsFile: filepath.Join(t.TempDir(), "legacy-metrics.jsonl")}
	if _, err := source.SearchCatalogExperiments(context.Background(), expstore.ExperimentSearchOptions{Limit: 10}); err == nil ||
		!strings.Contains(err.Error(), "live Kusto query transport") {
		t.Fatalf("SearchCatalogExperiments error = %v", err)
	}
	if _, err := source.SearchCatalogRuns(context.Background(), expstore.RunSearchOptions{Limit: 10}); err == nil ||
		!strings.Contains(err.Error(), "live Kusto query transport") {
		t.Fatalf("SearchCatalogRuns error = %v", err)
	}
}
