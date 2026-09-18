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

func TestBuildCatalogQueryRejectsInvalidOptions(t *testing.T) {
	if _, err := BuildSeriesCatalogQuery(CatalogQueryOptions{TargetType: "invalid"}); err == nil {
		t.Fatal("expected invalid target type error")
	}
	if _, err := BuildRunCatalogQuery(CatalogQueryOptions{Limit: -1}); err == nil {
		t.Fatal("expected negative limit error")
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
