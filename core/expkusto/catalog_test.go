// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expkusto

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/exptelemetry"
)

func TestBuildSeriesCatalogQueryUsesStableFunctionAndFilters(t *testing.T) {
	query, err := BuildSeriesCatalogQuery(CatalogQueryOptions{
		WorkspaceID: "workspace-a",
		Projects:    []string{"project-a", "project-b"},
		Target:      "experiment-a",
		TargetType:  "experiment",
		MetricNames: []string{"train/loss"},
		Since:       "30d",
		Limit:       50,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		exptelemetry.SeriesCatalogRowsFunction + "()",
		"latest_activity_at > ago(30d)",
		"workspace_id == 'workspace-a'",
		"['project'] in ('project-a', 'project-b')",
		"experiment_id == 'experiment-a'",
		"metric_name in ('train/loss')",
		"| top 51 by latest_activity_at desc",
		"join kind=inner top_experiments",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, exptelemetry.RemoteWriteTable) {
		t.Fatalf("catalog query must not scan raw %s:\n%s", exptelemetry.RemoteWriteTable, query)
	}
	assertCatalogProjectColumnEscaped(t, query)
}

func TestBuildSeriesCatalogQueryScopesSelectedRunIdentitiesBeforeLimit(t *testing.T) {
	query, err := BuildSeriesCatalogQuery(CatalogQueryOptions{
		RunIdentities: []CatalogRunIdentity{
			{Project: "project-a", ExperimentID: "experiment-old", RunID: "shared"},
			{Project: "project-b", ExperimentID: "experiment-new", RunID: "shared"},
		},
		Limit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	identityFilter := strings.Index(query, "| where (['project'] == 'project-a' and experiment_id == 'experiment-old' and run_id == 'shared')")
	top := strings.Index(query, "| top 1001 by latest_activity_at desc")
	if identityFilter < 0 || top < 0 || identityFilter > top {
		t.Fatalf("selected run identities must be filtered before the experiment limit:\n%s", query)
	}
	if !strings.Contains(query, "(['project'] == 'project-b' and experiment_id == 'experiment-new' and run_id == 'shared')") {
		t.Fatalf("query omitted selected identity:\n%s", query)
	}
}

func TestBuildCatalogQueryRendersAbsoluteSinceAsDatetime(t *testing.T) {
	query, err := BuildExperimentCatalogQuery(CatalogQueryOptions{
		Since: "2026-09-01T00:00:00.1234-07:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "latest_activity_at > todatetime('2026-09-01T07:00:00.1234Z')") ||
		strings.Contains(query, "ago('2026-09-01") {
		t.Fatalf("absolute since was not rendered as a datetime:\n%s", query)
	}
}

func TestBuildCatalogQueryRejectsMalformedSince(t *testing.T) {
	if _, err := BuildRunCatalogQuery(CatalogQueryOptions{Since: "last Tuesday"}); err == nil ||
		!strings.Contains(err.Error(), "--since") {
		t.Fatalf("malformed since error = %v", err)
	}
}

func TestBuildRunCatalogQueryIncludesLifecycleContract(t *testing.T) {
	query, err := BuildRunCatalogQuery(CatalogQueryOptions{
		WorkspaceID: "workspace-a",
		RunGroupID:  "group-a",
		RunIDs:      []string{"run-a"},
		MetricNames: []string{"train/loss"},
		Limit:       5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		exptelemetry.RunCatalogRowsFunction + "()",
		exptelemetry.SeriesCatalogRowsFunction + "()",
		"metric_name in ('train/loss')",
		"join kind=inner matching_catalog_runs",
		"run_group_id == 'group-a'",
		"run_id in ('run-a')",
		"has_metrics",
		"has_lifecycle",
		"metric_series_count",
		"| take 1000",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, exptelemetry.RemoteWriteTable) {
		t.Fatalf("catalog query must not scan raw %s:\n%s", exptelemetry.RemoteWriteTable, query)
	}
	assertCatalogProjectColumnEscaped(t, query)
}

func TestBuildRunCatalogQueryAppliesCursorBeforeBoundedTake(t *testing.T) {
	query, err := BuildRunCatalogQuery(CatalogQueryOptions{
		AfterAt:      "2026-09-18T18:10:00Z",
		AfterProject: "project-a",
		AfterRunID:   "run-1000",
		Limit:        1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	cursor := strings.Index(query, "| where cursor_sort_at <")
	take := strings.Index(query, "| take 1000")
	if cursor < 0 || take < 0 || cursor > take {
		t.Fatalf("cursor filter must precede bounded take:\n%s", query)
	}
	for _, want := range []string{
		"cursor_sort_at=coalesce(created_time, submit_time, first_activity_at, latest_activity_at)",
		"['project'] > 'project-a'",
		"run_id > 'run-1000'",
		"order by cursor_sort_at desc, ['project'] asc, run_id asc",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildExperimentCatalogQueryPagesGroupedExperiments(t *testing.T) {
	query, err := BuildExperimentCatalogQuery(CatalogQueryOptions{
		AfterAt:           "2026-09-18T18:10:00Z",
		AfterProject:      "project-a",
		AfterExperimentID: "experiment-1000",
		Limit:             1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"summarize experiment_cursor_at=max(latest_activity_at) by workspace_id, ['project'], experiment_id",
		"['project'] > 'project-a'",
		"experiment_id > 'experiment-1000'",
		"| take 1000;",
		"join kind=inner page_experiments",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildCatalogQueryRejectsInvalidOptions(t *testing.T) {
	if _, err := BuildSeriesCatalogQuery(CatalogQueryOptions{TargetType: "invalid"}); err == nil {
		t.Fatal("expected invalid target type error")
	}
	if _, err := BuildRunCatalogQuery(CatalogQueryOptions{Limit: -1}); err == nil {
		t.Fatal("expected negative limit error")
	}
	if _, err := BuildRunCatalogQuery(CatalogQueryOptions{
		AfterAt: "not-a-time", AfterProject: "project-a", AfterRunID: "run-a",
	}); err == nil {
		t.Fatal("expected invalid cursor timestamp error")
	}
}

func assertCatalogProjectColumnEscaped(t *testing.T, query string) {
	t.Helper()
	for _, invalid := range []string{
		"['project']=project",
		" by workspace_id, project,",
		" on workspace_id, project,",
		" on workspace_id, cluster, project,",
		" project in (",
		", project asc",
		", project,",
	} {
		if strings.Contains(query, invalid) {
			t.Fatalf("catalog query contains unescaped reserved project identifier %q:\n%s", invalid, query)
		}
	}
}
