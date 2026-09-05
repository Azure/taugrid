// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestAutoRunSearchMergesSources(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	metricsFile := filepath.Join(t.TempDir(), "metrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(
		`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"seed-1","metric_name":"loss","step":1,"wall_time":"2026-05-21T00:00:00Z","value":999}`+"\n"+
			`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"adx-only","metric_name":"loss","step":1,"wall_time":"2026-05-21T00:00:00Z","value":1}`+"\n"+
			`{"workspace_id":"other","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"forbidden","metric_name":"loss","step":1,"wall_time":"2026-05-21T00:00:00Z","value":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Options{StorePath: root, Source: "auto", Workspace: "sample", KustoMetricsFile: metricsFile})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/runs?target=experiment-alpha", nil))
	var result runSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(result.Runs) != 2 {
		t.Fatalf("auto search must include local and Kusto runs: %d %s", rec.Code, rec.Body.String())
	}
	for _, run := range result.Runs {
		if run.RunID == "seed-1" && (run.Owner != "expapi-test" || len(run.Metrics) != 0) {
			t.Fatalf("duplicate did not keep local record: %+v", run)
		}
		if run.RunID == "seed-1" && run.Source != "local" || run.RunID == "adx-only" && run.Source != "kusto" {
			t.Fatalf("incorrect source attribution: %+v", run)
		}
	}
	if !containsWarning(result.Warnings, "duplicate Kusto run IDs") || !containsWarning(result.Warnings, "merged 1 Kusto-backed runs") {
		t.Fatalf("missing merge provenance: %+v", result.Warnings)
	}
	for _, base := range stellarAPIBasePaths {
		for _, query := range []string{"q=adx-only", "group=reference-group&q=adx-only", "metric_name=loss&q=adx-only"} {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/runs?"+query, nil))
			var filtered runSearchResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || len(filtered.Runs) != 1 || filtered.Runs[0].RunID != "adx-only" {
				t.Fatalf("filtered auto source %s: %d %s", query, rec.Code, rec.Body.String())
			}
		}
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/runs?workspace=other", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("workspace override accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAutoRunSearchPartialFailures(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	metricsFile := filepath.Join(t.TempDir(), "metrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"adx-only","metric_name":"loss","step":1,"wall_time":"2026-05-21T00:00:00Z","value":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name, store, metrics, warning, source string
		wantError                             bool
	}{
		{"local failure", missing, metricsFile, "fell back to Kusto", "kusto", false},
		{"kusto failure", root, missing, "skipped Kusto run search", "local", false},
		{"local only", root, "", "", "local", false},
		{"both failed", missing, missing, "", "", true},
		{"no fallback", missing, "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(Options{StorePath: tc.store, KustoMetricsFile: tc.metrics, Workspace: "sample", Source: "auto"})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/runs", nil))
			if tc.wantError {
				if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), `"error"`) {
					t.Fatalf("source failure looked successful: %d %s", rec.Code, rec.Body.String())
				}
				if tc.name == "both failed" && (!strings.Contains(rec.Body.String(), "local run search") || !strings.Contains(rec.Body.String(), "Kusto run search")) {
					t.Fatalf("lost source error: %s", rec.Body.String())
				}
				return
			}
			var result runSearchResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || len(result.Runs) != 1 || result.Runs[0].Source != tc.source {
				t.Fatalf("wrong fallback: %d %s", rec.Code, rec.Body.String())
			}
			if tc.warning != "" && !containsWarning(result.Warnings, tc.warning) {
				t.Fatalf("missing partial failure warning: %s", rec.Body.String())
			}
		})
	}
}

func TestMergeRunSearchResultsBoundsAndDedup(t *testing.T) {
	local := withRunSource(expstore.RunSearchResult{Target: "target", Warnings: []string{"local warning"}}, "local")
	kusto := withRunSource(expstore.RunSearchResult{Warnings: []string{"kusto warning"}}, "kusto")
	for i := 0; i < 1000; i++ {
		local.Runs = append(local.Runs, sourcedRun{RunSearchRun: expstore.RunSearchRun{
			RunRecord: expstore.RunRecord{RunID: fmt.Sprintf("local-%04d", i), CreatedAt: "2026-05-01"},
		}, Source: "local"})
		kusto.Runs = append(kusto.Runs, sourcedRun{RunSearchRun: expstore.RunSearchRun{
			RunRecord: expstore.RunRecord{RunID: fmt.Sprintf("kusto-%04d", i), CreatedAt: "2026-05-02"},
		}, Source: "kusto"})
	}
	kusto.Runs = append(kusto.Runs, sourcedRun{RunSearchRun: expstore.RunSearchRun{
		RunRecord: expstore.RunRecord{RunID: "local-0000", CreatedAt: "2099-01-01"},
	}, Source: "kusto"})
	for _, limit := range []int{0, 1, 1000, 1000000} {
		result := mergeRunSearchResults(local, kusto, limit)
		want := limit
		if want == 0 {
			want = 200
		}
		if want > 1000 {
			want = 1000
		}
		if len(result.Runs) != want || !result.Truncated || result.Total != 2000 || result.Target != "target" {
			t.Fatalf("limit %d => %d runs, total=%d truncated=%v", limit, len(result.Runs), result.Total, result.Truncated)
		}
		if result.Runs[0].RunID != "kusto-0000" {
			t.Fatalf("duplicate or unstable ordering: %+v", result.Runs[0])
		}
		if !containsWarning(result.Warnings, "local warning") || !containsWarning(result.Warnings, "kusto warning") {
			t.Fatalf("source warnings dropped: %+v", result.Warnings)
		}
	}
	local.Truncated = true
	result := mergeRunSearchResults(local, runSearchResponse{}, 1000)
	if !result.Truncated || !containsWarning(result.Warnings, "source windows") {
		t.Fatalf("source truncation lost: %+v", result)
	}
}

func TestAutoSearchCancellationDoesNotFallBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := searchAutoSources(ctx, true, "run",
		func() (int, error) { return 0, ctx.Err() },
		func() (int, error) { t.Fatal("cancelled request queried fallback"); return 0, nil },
		func(a, b int) int { return a + b })
	if err != context.Canceled {
		t.Fatalf("cancellation = %v", err)
	}
}
