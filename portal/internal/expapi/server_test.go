// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/blobstore"
	"github.com/Azure/taugrid/portal/internal/expcockpit"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestV2ExperimentSearchReturnsNarrowContract(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedMetricRichExpAPIStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?q=experiment-alpha&limit=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response v2ExperimentSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Experiments) != 1 || response.Experiments[0].ExperimentID != "experiment-alpha" {
		t.Fatalf("unexpected experiments: %+v", response.Experiments)
	}
	if response.Metadata.Provenance.RequestedSource != "local" ||
		response.Metadata.Freshness.State != "immediate" ||
		response.Metadata.Availability.State != "available" {
		t.Fatalf("unexpected metadata: %+v", response.Metadata)
	}
	for _, presentationField := range []string{`"store_path"`, `"cards"`, `"chart"`, `"sweep"`, `"actions"`} {
		if strings.Contains(rec.Body.String(), presentationField) {
			t.Fatalf("v2 narrow response exposed %s: %s", presentationField, rec.Body.String())
		}
	}
}

type stubV2CatalogSource struct {
	experimentCalls int
	experiments     []expstore.ExperimentSummary
	experimentErr   error
	runs            runSearchResponse
	runErr          error
	lastRunOpts     expstore.RunSearchOptions
}

func (s *stubV2CatalogSource) searchExperiments(_ context.Context, source string, opts expstore.ExperimentSearchOptions) (v2ExperimentCatalogResult, error) {
	s.experimentCalls++
	if s.experimentErr != nil {
		return v2ExperimentCatalogResult{}, s.experimentErr
	}
	if s.experiments != nil {
		return v2ExperimentCatalogResult{
			Result: expstore.ExperimentSearchResult{Experiments: s.experiments}, ServedSources: []string{source},
		}, nil
	}
	return v2ExperimentCatalogResult{
		Result: expstore.ExperimentSearchResult{
			Experiments: []expstore.ExperimentSummary{{
				ExperimentRecord: expstore.ExperimentRecord{
					ExperimentID: "adapter-experiment",
					Project:      opts.Project,
					Name:         "Adapter experiment",
					Source:       source,
				},
			}},
		},
		ServedSources: []string{source},
	}, nil
}

func (s *stubV2CatalogSource) searchRuns(_ context.Context, _ string, opts expstore.RunSearchOptions) (runSearchResponse, error) {
	s.lastRunOpts = opts
	if s.runErr != nil {
		return runSearchResponse{}, s.runErr
	}
	if s.runs.Runs == nil {
		return runSearchResponse{}, nil
	}
	filtered := s.runs
	filtered.Runs = nil
	for _, run := range s.runs.Runs {
		if opts.Project != "" && run.Project != opts.Project {
			continue
		}
		filtered.Runs = append(filtered.Runs, run)
	}
	return filtered, nil
}

func TestV2ExperimentSearchPaginatesWithValidatedOpaqueCursor(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}

	catalog := &stubV2CatalogSource{experiments: []expstore.ExperimentSummary{
		{ExperimentRecord: expstore.ExperimentRecord{ExperimentID: "experiment-a", UpdatedAt: "2026-09-18T12:00:00Z"}},
		{ExperimentRecord: expstore.ExperimentRecord{ExperimentID: "experiment-b", UpdatedAt: "2026-09-18T11:00:00Z"}},
		{ExperimentRecord: expstore.ExperimentRecord{ExperimentID: "experiment-c", UpdatedAt: "2026-09-18T10:00:00Z"}},
	}}
	server.v2Catalog = catalog

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?limit=1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
	}
	var firstPage v2ExperimentSearchResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Experiments) != 1 || firstPage.Experiments[0].ExperimentID != "experiment-a" ||
		firstPage.NextCursor == "" || firstPage.Metadata.Partial {
		t.Fatalf("unexpected first page: %+v", firstPage)
	}

	second := httptest.NewRecorder()
	path := "/api/v2/stellar/experiments/search?limit=1&cursor=" + url.QueryEscape(firstPage.NextCursor)
	server.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, path, nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body=%s", second.Code, second.Body.String())
	}
	var secondPage v2ExperimentSearchResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Experiments) != 1 || secondPage.Experiments[0].ExperimentID != "experiment-b" {
		t.Fatalf("cursor did not advance: %+v", secondPage)
	}

	tampered := httptest.NewRecorder()
	server.Handler().ServeHTTP(tampered, httptest.NewRequest(http.MethodGet, path+"x", nil))
	if tampered.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor status = %d, body=%s", tampered.Code, tampered.Body.String())
	}
}

func TestV2ExperimentCursorKeepsSameIDAcrossProjectsReachable(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server.v2Catalog = &stubV2CatalogSource{experiments: []expstore.ExperimentSummary{
		{ExperimentRecord: expstore.ExperimentRecord{Project: "project-a", ExperimentID: "shared", UpdatedAt: "2026-09-18T12:00:00Z"}},
		{ExperimentRecord: expstore.ExperimentRecord{Project: "project-b", ExperimentID: "shared", UpdatedAt: "2026-09-18T12:00:00Z"}},
	}}

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?limit=1", nil))
	var firstPage v2ExperimentSearchResponse
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &firstPage) != nil || firstPage.NextCursor == "" {
		t.Fatalf("unexpected first page: status=%d body=%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, httptest.NewRequest(
		http.MethodGet,
		"/api/v2/stellar/experiments/search?limit=1&cursor="+url.QueryEscape(firstPage.NextCursor),
		nil,
	))
	var secondPage v2ExperimentSearchResponse
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Experiments) != 1 ||
		secondPage.Experiments[0].Project == firstPage.Experiments[0].Project {
		t.Fatalf("project-scoped duplicate experiment was unreachable: first=%+v second=%+v", firstPage, secondPage)
	}
}

func TestV2ErrorsDistinguishClientDependencyConflictAndInternalFailures(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		err            error
		wantStatus     int
		wantCode       string
		classification string
	}{
		{name: "client", path: "/api/v2/stellar/experiments/search?limit=0", wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT", classification: "client"},
		{name: "conflict", err: expstore.ErrConflict, wantStatus: http.StatusConflict, wantCode: "CONFLICT", classification: "client"},
		{name: "unavailable", err: errors.New("Kusto endpoint is not configured"), wantStatus: http.StatusServiceUnavailable, wantCode: "SOURCE_UNAVAILABLE", classification: "dependency"},
		{name: "upstream", err: errors.New("Kusto query response reported errors"), wantStatus: http.StatusBadGateway, wantCode: "UPSTREAM_FAILURE", classification: "dependency"},
		{name: "internal", err: errors.New("sqlite scan failed"), wantStatus: http.StatusInternalServerError, wantCode: "INTERNAL", classification: "internal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
			if err != nil {
				t.Fatal(err)
			}
			if test.err != nil {
				server.v2Catalog = &stubV2CatalogSource{experimentErr: test.err}
			}
			path := test.path
			if path == "" {
				path = "/api/v2/stellar/experiments/search"
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, test.wantStatus, rec.Body.String())
			}
			var envelope v2ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.wantCode || envelope.Error.Classification != test.classification {
				t.Fatalf("unexpected error: %+v", envelope.Error)
			}
		})
	}
}

func TestV2CatalogSourceSeamIsModeIndependent(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &stubV2CatalogSource{}
	server.v2Catalog = catalog

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?project=adapter-project", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response v2ExperimentSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if catalog.experimentCalls != 1 || len(response.Experiments) != 1 ||
		response.Experiments[0].ExperimentID != "adapter-experiment" ||
		response.Experiments[0].Project != "adapter-project" {
		t.Fatalf("canonical search did not use catalog seam: calls=%d response=%+v", catalog.experimentCalls, response)
	}
}

func TestV2CatalogFunctionsSurfaceFailure(t *testing.T) {
	var query string
	server, err := NewServer(Options{
		Source:               "kusto",
		Workspace:            "workspace-a",
		KustoAllowedProjects: []string{"project-a"},
		KustoNativeQuery: func(_ context.Context, generated string) (string, error) {
			query = generated
			return "", errors.New("Kusto query response reported errors: catalog function unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		"/api/v2/stellar/experiments/search?project=project-a&limit=10",
		nil,
	))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(query, exptelemetry.RunCatalogRowsFunction+"()") {
		t.Fatalf("function-backed discovery did not query the stable run catalog:\n%s", query)
	}
}

func TestV2RunListPaginatesWithValidatedOpaqueCursor(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 3)})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &stubV2CatalogSource{runs: runSearchResponse{
		RunSearchResult: expstore.RunSearchResult{Total: 3},
		Runs: []sourcedRun{
			{RunSearchRun: expstore.RunSearchRun{RunRecord: expstore.RunRecord{RunID: "run-a", ExperimentID: "experiment-alpha", CreatedAt: "2026-09-18T12:00:00Z"}}, Source: "local"},
			{RunSearchRun: expstore.RunSearchRun{RunRecord: expstore.RunRecord{RunID: "run-b", ExperimentID: "experiment-alpha", CreatedAt: "2026-09-18T11:00:00Z"}}, Source: "local"},
			{RunSearchRun: expstore.RunSearchRun{RunRecord: expstore.RunRecord{RunID: "run-c", ExperimentID: "experiment-alpha", CreatedAt: "2026-09-18T10:00:00Z"}}, Source: "local"},
		},
	}}
	server.v2Catalog = catalog
	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/experiment-alpha/runs?limit=1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
	}
	var firstPage v2RunListResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Runs) != 1 || firstPage.NextCursor == "" || firstPage.Metadata.Partial {
		t.Fatalf("unexpected first page: %+v", firstPage)
	}

	second := httptest.NewRecorder()
	secondPath := "/api/v2/stellar/experiments/experiment-alpha/runs?limit=1&cursor=" + url.QueryEscape(firstPage.NextCursor)
	server.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, secondPath, nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body=%s", second.Code, second.Body.String())
	}
	var secondPage v2RunListResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Runs) != 1 || secondPage.Runs[0].RunID == firstPage.Runs[0].RunID {
		t.Fatalf("cursor did not advance: first=%+v second=%+v", firstPage.Runs, secondPage.Runs)
	}
	if catalog.lastRunOpts.CursorAt == "" || catalog.lastRunOpts.CursorID == "" {
		t.Fatalf("validated cursor was not passed to the catalog source: %+v", catalog.lastRunOpts)
	}

	tampered := httptest.NewRecorder()
	tamperedPath := "/api/v2/stellar/experiments/experiment-alpha/runs?limit=1&cursor=" + url.QueryEscape(firstPage.NextCursor+"x")
	server.Handler().ServeHTTP(tampered, httptest.NewRequest(http.MethodGet, tamperedPath, nil))
	if tampered.Code != http.StatusBadRequest {
		t.Fatalf("tampered cursor status = %d, body=%s", tampered.Code, tampered.Body.String())
	}
	var envelope v2ErrorEnvelope
	if err := json.Unmarshal(tampered.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "INVALID_CURSOR" || envelope.Error.Classification != "client" || envelope.Error.Retryable {
		t.Fatalf("unexpected cursor error: %+v", envelope.Error)
	}
}

func TestV2ExactRunDetailAndMetricCatalogSupportLifecycleOnlyRuns(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	detail := httptest.NewRecorder()
	server.Handler().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/runs/seed-1", nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body=%s", detail.Code, detail.Body.String())
	}
	var runResponse v2RunDetailResponse
	if err := json.Unmarshal(detail.Body.Bytes(), &runResponse); err != nil {
		t.Fatal(err)
	}
	if runResponse.Run.RunID != "seed-1" || runResponse.Run.LifecycleState != "succeeded" ||
		runResponse.Run.Tags == nil || runResponse.Run.MetricNames == nil {
		t.Fatalf("unexpected exact run detail: %+v", runResponse)
	}

	catalog := httptest.NewRecorder()
	server.Handler().ServeHTTP(catalog, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/runs/seed-1/metrics", nil))
	if catalog.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, body=%s", catalog.Code, catalog.Body.String())
	}
	var metricResponse v2MetricCatalogResponse
	if err := json.Unmarshal(catalog.Body.Bytes(), &metricResponse); err != nil {
		t.Fatal(err)
	}
	if metricResponse.RunID != "seed-1" || len(metricResponse.Metrics) != 0 ||
		metricResponse.Metadata.Availability.State != "unavailable" {
		t.Fatalf("unexpected lifecycle-only metric catalog: %+v", metricResponse)
	}

	mismatched := httptest.NewRecorder()
	server.Handler().ServeHTTP(mismatched, httptest.NewRequest(
		http.MethodGet, "/api/v2/stellar/runs/seed-1?target=another-experiment", nil))
	if mismatched.Code != http.StatusNotFound {
		t.Fatalf("mismatched exact target status = %d, body=%s", mismatched.Code, mismatched.Body.String())
	}
}

func TestV2ExactRunRequiresProjectWhenRunIDsCollide(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server.v2Catalog = &stubV2CatalogSource{runs: runSearchResponse{Runs: []sourcedRun{
		{RunSearchRun: expstore.RunSearchRun{RunRecord: expstore.RunRecord{
			RunID: "shared-run", Project: "project-a", ExperimentID: "experiment-a",
		}}, Source: "kusto"},
		{RunSearchRun: expstore.RunSearchRun{RunRecord: expstore.RunRecord{
			RunID: "shared-run", Project: "project-b", ExperimentID: "experiment-b",
		}}, Source: "kusto"},
	}}}

	ambiguous := httptest.NewRecorder()
	server.Handler().ServeHTTP(ambiguous, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/runs/shared-run", nil))
	if ambiguous.Code != http.StatusConflict {
		t.Fatalf("ambiguous status = %d, body=%s", ambiguous.Code, ambiguous.Body.String())
	}

	exact := httptest.NewRecorder()
	server.Handler().ServeHTTP(exact, httptest.NewRequest(
		http.MethodGet, "/api/v2/stellar/runs/shared-run?project=project-b&target=experiment-b", nil))
	if exact.Code != http.StatusOK {
		t.Fatalf("exact status = %d, body=%s", exact.Code, exact.Body.String())
	}
	var response v2RunDetailResponse
	if err := json.Unmarshal(exact.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Run.Project != "project-b" || response.Run.ExperimentID != "experiment-b" {
		t.Fatalf("wrong exact run: %+v", response.Run)
	}
}

func TestV2ExactSeriesRequiresScopeAndHonorsPointBounds(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedMetricRichExpAPIStore(t), Workspace: "sample"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		code int
	}{
		{"missing target", "/api/v2/stellar/runs/ablation-seed-2/series?metric=train%2Freturn", http.StatusBadRequest},
		{"wrong target", "/api/v2/stellar/runs/ablation-seed-2/series?target=other&metric=train%2Freturn", http.StatusNotFound},
		{"wrong workspace", "/api/v2/stellar/runs/ablation-seed-2/series?workspace=other&target=experiment-alpha&metric=train%2Freturn", http.StatusForbidden},
		{"excessive points", "/api/v2/stellar/runs/ablation-seed-2/series?workspace=sample&target=experiment-alpha&metric=train%2Freturn&max_points=12001", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.path, nil))
			if rec.Code != test.code {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, test.code, rec.Body.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	path := "/api/v2/stellar/runs/ablation-seed-2/series?workspace=sample&target=experiment-alpha&metric=train%2Freturn&max_points=1"
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response v2SeriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RunID != "ablation-seed-2" || response.Metric != "train/return" ||
		response.ReturnedPoints > 1 || response.MaxPoints != 1 {
		t.Fatalf("series bounds not enforced: %+v", response)
	}
	if strings.Contains(rec.Body.String(), `"color"`) || strings.Contains(rec.Body.String(), `"smoothing"`) {
		t.Fatalf("series exposed presentation metadata: %s", rec.Body.String())
	}
}

func TestWorkspaceRoutePolicyAllowsOnlyConcreteV2NarrowReads(t *testing.T) {
	allowed := []string{
		"/api/v2/stellar/experiments/search",
		"/api/v2/stellar/experiments/experiment-alpha/runs",
		"/api/v2/stellar/runs/run-1",
		"/api/v2/stellar/runs/run-1/metrics",
		"/api/v2/stellar/runs/run-1/series",
	}
	for _, path := range allowed {
		if !WorkspaceRouteAllowed(http.MethodGet, path) {
			t.Fatalf("expected route to be workspace scoped: %s", path)
		}
	}
	for _, path := range []string{
		"/api/v2/stellar/experiments/experiment-alpha",
		"/api/v2/stellar/runs/run-1/delete",
		"/api/v2/stellar/runs/run-1/metrics/extra",
	} {
		if WorkspaceRouteAllowed(http.MethodGet, path) {
			t.Fatalf("unexpected workspace route allowed: %s", path)
		}
	}
}

// TestWorkspaceScopeFlowsFromServerConfig pins where scope comes from. The
// server's configured workspace is authoritative; ?workspace= may echo it but
// cannot widen or change it, because nothing authenticates the caller.
func TestWorkspaceScopeIsParsedForExperimentAndRunSearch(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1), Workspace: "sample"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments?workspace=sample", nil)
	workspace, err := server.resolveWorkspace(req)
	if err != nil {
		t.Fatal(err)
	}
	if workspace != "sample" {
		t.Fatalf("explicit workspace = %q, want sample", workspace)
	}
	experimentOpts, err := experimentSearchOptionsFromRequest(req, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if experimentOpts.Workspace != "sample" {
		t.Fatalf("experiment workspace = %q, want sample", experimentOpts.Workspace)
	}

	// Omitting ?workspace= still yields the server's scope rather than "all".
	bare := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/runs", nil)
	if workspace, err = server.resolveWorkspace(bare); err != nil || workspace != "sample" {
		t.Fatalf("bare request workspace = %q, err = %v; want sample", workspace, err)
	}
	runOpts, err := runSearchOptionsFromRequest(bare, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if runOpts.Workspace != "sample" {
		t.Fatalf("run workspace = %q, want sample", runOpts.Workspace)
	}

	seriesReq := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/series?metric=train/loss", nil)
	seriesOpts, err := seriesOptionsFromRequest(seriesReq, workspace, "experiment", "train/loss", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if seriesOpts.Workspace != "sample" {
		t.Fatalf("series workspace = %q, want sample", seriesOpts.Workspace)
	}
}

// TestStellarIsNeverUnscoped pins the fail-closed behaviour. An empty workspace
// used to mean "every workspace"; a server with none configured now serves the
// default workspace instead, and reads still succeed.
func TestStellarIsNeverUnscoped(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := server.resolveWorkspace(httptest.NewRequest(http.MethodGet, "/api/v2/stellar/runs", nil))
	if err != nil {
		t.Fatal(err)
	}
	if workspace != DefaultWorkspace {
		t.Fatalf("workspace = %q, want %q", workspace, DefaultWorkspace)
	}
	// A request naming a different workspace is refused rather than silently
	// served from this one.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?workspace=other", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched workspace status = %d, want 403", rec.Code)
	}
}

// TestStellarPageCarriesWorkspaceScope pins that the rendered shell tells the
// frontend which workspace it is looking at. Workspace no longer implies
// source=kusto: the local store is workspace-scoped too.
func TestStellarPageCarriesWorkspaceScope(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root, Workspace: "sample"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stellar", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-workspace="sample"`) {
		t.Fatalf("Stellar page dropped workspace scope:\n%s", rec.Body.String())
	}

	// A local-source read is now legitimate while workspace-scoped.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?source=local", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace-scoped local source status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Naming a different workspace is refused rather than served.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v2/stellar/experiments/search?workspace=other", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace read status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestStellarCapabilitiesEndpointReportsLocalAndKustoModes(t *testing.T) {
	localServer, err := NewServer(Options{StorePath: t.TempDir(), Source: "local"})
	if err != nil {
		t.Fatal(err)
	}
	assertCapabilities := func(server *Server, wantSource string, wantLocal, wantKusto bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/stellar/capabilities", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("capabilities status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var caps capabilitiesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatalf("parse capabilities: %v", err)
		}
		if caps.SourceMode != wantSource || caps.Paths.CanonicalBasePath != "/api/v2/stellar" || len(caps.Paths.SupportedBases) != 1 {
			t.Fatalf("unexpected capabilities identity: %+v", caps)
		}
		if caps.DataSources["local"].Available != wantLocal || caps.DataSources["kusto"].Available != wantKusto {
			t.Fatalf("unexpected data sources: %+v", caps.DataSources)
		}
	}
	assertCapabilities(localServer, "local", true, false)

	kustoServer, err := NewServer(Options{
		Source:            "kusto",
		Workspace:         "sample",
		KustoEndpoint:     "https://example.kusto.windows.net",
		KustoDatabase:     "Metrics",
		KustoQueryCommand: "/bin/true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if kustoServer.StoreRoot() != "kusto://typed" {
		t.Fatalf("Kusto store root = %q", kustoServer.StoreRoot())
	}
	assertCapabilities(kustoServer, "kusto", false, true)
}

func TestStellarFrontendShellServed(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stellar?target=experiment-alpha", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content-type = %q", got)
	}
	html := rec.Body.String()
	for _, want := range []string{
		"Stellar sweep",
		`id="stellar-root"`,
		`data-target="experiment-alpha"`,
		`/stellar/assets/app.css`,
		`/stellar/assets/app.js`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("frontend shell missing %q\n%s", want, html)
		}
	}
}

func TestStellarLandingShellServedWithoutTarget(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root, DefaultTarget: "experiment-alpha", MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stellar", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	html := rec.Body.String()
	for _, want := range []string{
		"Stellar sweep",
		`id="stellar-root"`,
		`data-target=""`,
		`/stellar/assets/app.css`,
		`/stellar/assets/app.js`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("landing shell missing %q\n%s", want, html)
		}
	}
	if strings.Contains(html, `data-target="experiment-alpha"`) {
		t.Fatalf("/stellar should not apply default target; root redirect keeps that compatibility:\n%s", html)
	}
}

func TestStellarPreviewRoutesAreRemoved(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	for _, path := range []string{
		"/stellar/preview?target=experiment-alpha",
		"/api/v2/stellar/preview?target=experiment-alpha",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "/stellar/preview") || strings.Contains(body, "/api/v2/stellar/preview") {
		t.Fatalf("root endpoint should not advertise preview routes: %s", body)
	}
}

func TestStellarShellDeniesFraming(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		iframe bool
	}{
		{name: "top-level navigation"},
		{name: "subframe navigation", iframe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/stellar?target=experiment-alpha", nil)
			if tc.iframe {
				req.Header.Set("Sec-Fetch-Dest", "iframe")
			}

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("X-Frame-Options = %q, want DENY", got)
			}
		})
	}
}

func TestStellarFrontendAssetsServed(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path        string
		contentType string
		body        string
	}{
		{path: "/stellar/assets/app.css", contentType: "text/css", body: ".stellar-app"},
		{path: "/stellar/assets/app.js", contentType: "text/javascript", body: "fetchSnapshot"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
				t.Fatalf("content-type = %q", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("cache-control = %q", got)
			}
			if !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("asset body missing %q", tc.body)
			}
		})
	}

	if _, ok, err := expcockpit.ReadFrontendAsset("../server.go"); err != nil || ok {
		t.Fatalf("traversal asset lookup ok=%v err=%v, want not found without error", ok, err)
	}
}

func TestVersionedStellarFrontendAssetsAreImmutable(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stellar/assets/app.css?v=test-version", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("cache-control = %q", got)
	}
}

func TestStellarArtifactEndpointServesStoreLocalArtifact(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	artifactPath := filepath.Join(root, "artifacts", "baseline-seed-1", "rollout.mp4")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("mp4 smoke artifact")
	if err := os.WriteFile(artifactPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/artifact?target=experiment-alpha&artifact=artifact-baseline-seed-1-rollout", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "video/mp4") {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Fatalf("body = %q, want %q", got, string(body))
	}
}

func TestStellarArtifactsEndpointListsIndexedArtifacts(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/artifacts?target=experiment-alpha&type=video", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result struct {
		Source    string `json:"source"`
		Target    string `json:"target"`
		Count     int    `json:"count"`
		Artifacts []struct {
			ArtifactID string `json:"artifact_id"`
			RunID      string `json:"run_id"`
			Type       string `json:"type"`
			FetchURL   string `json:"fetch_url"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse artifacts response: %v\n%s", err, rec.Body.String())
	}
	if result.Source != "index" || result.Target != "experiment-alpha" || result.Count == 0 {
		t.Fatalf("unexpected artifacts response: %+v", result)
	}
	for _, artifact := range result.Artifacts {
		if artifact.Type != "video" || artifact.FetchURL == "" || artifact.RunID == "" || artifact.ArtifactID == "" {
			t.Fatalf("artifact response missing indexed fields: %+v", artifact)
		}
	}
}

func TestStellarArtifactEndpointServesDurableArtifactWithoutLocalCopy(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	body := []byte("durable mp4 smoke artifact")
	sourcePath := filepath.Join(t.TempDir(), "rollout.mp4")
	if err := os.WriteFile(sourcePath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	objectStore, err := blobstore.NewFileStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := fileutil.FileSHA256(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	ref := blobstore.NewDurableRef(blobstore.Partition{
		BaseURI:      objectStore.BaseURI,
		Project:      "project-alpha",
		ExperimentID: "experiment-alpha",
		RunGroupID:   "reference-group",
		RunID:        "baseline-seed-1",
		ArtifactType: "video",
		ArtifactName: "rollout.mp4",
		Digest:       digest,
	}, size, "video/mp4", time.Now())
	if _, err := objectStore.UploadFile(context.Background(), ref, sourcePath); err != nil {
		t.Fatal(err)
	}
	refString, err := ref.String()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := expstore.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.ArtifactsForRun(ctx, "baseline-seed-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) == 0 {
		t.Fatal("expected seeded rollout artifact")
	}
	artifact := artifacts[0]
	artifact.DurableRef = refString
	artifact.ContentType = "video/mp4"
	artifact.Digest = digest
	artifact.SizeBytes = &size
	if err := store.UpdateArtifactDurableRefs(ctx, []expstore.ArtifactRecord{artifact}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(root, "artifacts", "baseline-seed-1", "rollout.mp4")
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatalf("local artifact copy should be absent for durable-only test, stat err=%v", err)
	}
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/stellar/artifact?target=experiment-alpha&artifact=artifact-baseline-seed-1-rollout", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "video/mp4") {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Fatalf("body = %q, want %q", got, string(body))
	}
}

func TestStellarArtifactEndpointServesReportBundle(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	seedLocalReportArtifact(t, root)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, artifactBundlePath("experiment-alpha", "artifact-baseline-seed-1-report", ""), nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"sandbox", "frame-ancestors 'self'", "script-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("Content-Security-Policy = %q, missing %q", csp, want)
		}
	}
	if !strings.Contains(rec.Body.String(), `src="thumb.png"`) {
		t.Fatalf("report body did not include relative image reference: %s", rec.Body.String())
	}

	assetRec := httptest.NewRecorder()
	assetReq := httptest.NewRequest(http.MethodGet, artifactBundlePath("experiment-alpha", "artifact-baseline-seed-1-report", "thumb.png"), nil)
	server.Handler().ServeHTTP(assetRec, assetReq)

	if assetRec.Code != http.StatusOK {
		t.Fatalf("asset status = %d, body=%s", assetRec.Code, assetRec.Body.String())
	}
	if got := assetRec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/png") {
		t.Fatalf("asset content-type = %q", got)
	}
	if got := assetRec.Body.String(); got != "png smoke artifact" {
		t.Fatalf("asset body = %q", got)
	}
}

func TestStellarArtifactEndpointRejectsReportBundleTraversal(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	seedLocalReportArtifact(t, root)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}

	for _, assetPath := range []string{
		"..%5crollout.mp4",
		"%2e%2e/rollout.mp4",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, artifactBundlePath("experiment-alpha", "artifact-baseline-seed-1-report", assetPath), nil)
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("asset path %q status = %d, want %d, body=%s", assetPath, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}
}

func TestStellarArtifactEndpointRejectsNonReportBundleAssets(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	artifactPath := filepath.Join(root, "artifacts", "baseline-seed-1", "rollout.mp4")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("mp4 smoke artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, artifactBundlePath("experiment-alpha", "artifact-baseline-seed-1-rollout", "thumb.png"), nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestStellarFrontendExplainsMetricsOnlyStores(t *testing.T) {
	asset, ok, err := expcockpit.ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset not found")
	}
	body := string(asset.Content)
	for _, want := range []string{
		"Metrics-only store",
		"metrics-only import",
		"Those panels stay hidden until ingestion records that evidence.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("frontend asset missing metrics-only empty-state text %q", want)
		}
	}
}

func TestStellarFrontendIncludesReportArtifactActions(t *testing.T) {
	asset, ok, err := expcockpit.ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset not found")
	}
	body := string(asset.Content)
	for _, want := range []string{
		"Open report",
		"renderArtifactReportPreview",
		"artifactScopedSource",
		"base64URLSegment",
		`sandbox: ""`,
		`h("video"`,
		`h("img"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("frontend asset missing report artifact support %q", want)
		}
	}
}

func seedExpAPIStore(t *testing.T, runs int) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(context.Background(), root, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce candidate model sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 1; i <= runs; i++ {
		runID := fmt.Sprintf("seed-%d", i)
		if _, err := store.RecordRunData(context.Background(), expstore.RecordRunDataOptions{
			Run: expstore.RunRecord{
				RunID:      runID,
				Project:    "project-alpha",
				RunGroupID: "reference-group",
				State:      "succeeded",
				Owner:      "expapi-test",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func seedMetricRichExpAPIStore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(ctx, root, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can Stellar replace the experiment loop we use W&B for?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, _, err = expstore.Init(ctx, root, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can Stellar replace the experiment loop we use W&B for?",
		Group:       "candidate-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	recordMetricRichRun(t, ctx, store, "baseline-seed-1", "reference-group", 42, 39, 1.20, 0.82, "2026-05-20T10:00:00Z")
	recordMetricRichRun(t, ctx, store, "baseline-seed-2", "reference-group", 45, 41, 1.16, 0.79, "2026-05-20T10:10:00Z")
	recordMetricRichRun(t, ctx, store, "ablation-seed-1", "candidate-group", 58, 53, 1.05, 0.86, "2026-05-20T10:20:00Z")
	recordMetricRichRun(t, ctx, store, "ablation-seed-2", "candidate-group", 61, 56, 1.00, 0.88, "2026-05-20T10:30:00Z")

	if _, err := store.EnrichRunData(ctx, expstore.EnrichRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "ablation-seed-2",
			Project:    "project-alpha",
			RunGroupID: "candidate-group",
			State:      "succeeded",
			Owner:      "agent",
			CreatedAt:  "2026-05-20T10:30:00Z",
		},
		Events: []expstore.EventRecord{{
			EventID:  "event-ablation-seed-2-checkpoint",
			RunID:    "ablation-seed-2",
			Time:     "2026-05-20T10:42:00Z",
			Type:     "checkpoint",
			Source:   "tau",
			Severity: "info",
			Message:  "Best checkpoint promoted after eval/score improved.",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordObservation(ctx, expstore.RecordObservationOptions{
		Observation: expstore.ObservationRecord{
			ObservationID: "obs-ablation-decision",
			Author:        "agent",
			Source:        "test",
			Type:          "decision",
			ScopeType:     "experiment",
			ScopeID:       "experiment-alpha",
			Text:          "Promote the ablation group as the current winner.",
			Evidence:      `{"metric":"train/return","run_group_id":"candidate-group"}`,
			CreatedAt:     "2026-05-20T10:45:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordObservation(ctx, expstore.RecordObservationOptions{
		Observation: expstore.ObservationRecord{
			ObservationID: "obs-next-ablation-seed",
			Author:        "agent",
			Source:        "test",
			Type:          "next-experiment",
			ScopeType:     "experiment",
			ScopeID:       "experiment-alpha",
			Text:          "Run one more ablation seed before closing the experiment.",
			Evidence:      `{"command":"tau run candidate-training-seed-5 --config experiments/candidate-training/candidate-group-seed-5.yaml"}`,
			CreatedAt:     "2026-05-20T10:46:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func recordMetricRichRun(t *testing.T, ctx context.Context, store *expstore.Store, runID, groupID string, trainReturn, evalScore, worldModelLoss, gpuUtil float64, createdAt string) {
	t.Helper()
	step1 := int64(1)
	step2 := int64(2)
	unitPercent := "percent"
	rel := filepath.ToSlash(filepath.Join(expstore.MetricsDir, "project=project-alpha", "group="+groupID, "run="+runID, "part.parquet"))
	rows := []expstore.MetricRow{
		metricRow(runID, groupID, "train/return", step1, trainReturn*0.80, nil),
		metricRow(runID, groupID, "train/return", step2, trainReturn, nil),
		metricRow(runID, groupID, "eval/score", step2, evalScore, nil),
		metricRow(runID, groupID, "world_model/loss", step2, worldModelLoss, nil),
		metricRow(runID, groupID, "policy/entropy", step2, 0.71, nil),
		metricRow(runID, groupID, "system/gpu_utilization", step2, gpuUtil*100, &unitPercent),
	}
	writeMetricParquet(t, store.Root, rel, rows)
	gpuCount := int64(8)
	gpuHours := 2.5
	estimatedCost := 12.75
	queueWait := 90.0
	size := int64(2048)
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:       runID,
			Project:     "project-alpha",
			RunGroupID:  groupID,
			State:       "succeeded",
			Owner:       "agent",
			CreatedAt:   createdAt,
			CompletedAt: createdAt,
			ConfigHash:  "config-" + runID,
			CodeSHA:     "abc123",
			ImageDigest: "sha256:" + runID,
			TauCommand:  "tau run candidate-training-" + groupID + " --config experiments/candidate-training/" + groupID + ".yaml",
		},
		RunContext: &expstore.RunContextRecord{
			RunID:            runID,
			Cluster:          "kind-taugrid",
			Namespace:        "ray",
			Team:             "research",
			Profile:          "research-train-gpu",
			Lane:             "training",
			LocalQueue:       "training-queue",
			ClusterQueue:     "gpu-a100",
			KueueWorkload:    runID + "-workload",
			GPUClass:         "a100-80gb",
			GPUCount:         &gpuCount,
			NodeNames:        "node-a",
			QueueWaitSeconds: &queueWait,
			GPUHours:         &gpuHours,
			EstimatedCost:    &estimatedCost,
		},
		Configs: []expstore.ConfigRecord{{
			ConfigHash:     "config-" + runID,
			RunID:          runID,
			Format:         "json",
			URI:            "configs/" + runID + ".json",
			NormalizedJSON: sweepConfigJSON(runID, groupID),
		}},
		Artifacts: []expstore.ArtifactRecord{{
			ArtifactID: "artifact-" + runID + "-rollout",
			RunID:      runID,
			Type:       "video",
			URI:        "artifacts/" + runID + "/rollout.mp4",
			Name:       "rollout.mp4",
			SizeBytes:  &size,
			CreatedAt:  createdAt,
			Preview:    "rollout preview",
		}},
		MetricFiles: []expstore.MetricFileRecord{{
			FileID:        "metrics-" + runID,
			Path:          rel,
			Format:        "parquet",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    groupID,
			RunID:         runID,
			RowCount:      int64(len(rows)),
			MinStep:       &step1,
			MaxStep:       &step2,
			CreatedAt:     createdAt,
		}},
		Tags: []expstore.TagRecord{{ScopeType: "run", ScopeID: runID, Key: "seed", Value: strings.TrimPrefix(runID, groupID+"-seed-")}},
	}); err != nil {
		t.Fatal(err)
	}
}

func sweepConfigJSON(runID, groupID string) string {
	shape := "narrow"
	activation := "relu"
	batchSize := 64
	nParams := "3m"
	lr := 0.0003
	if groupID == "candidate-group" {
		shape = "wide"
		activation = "gelu"
		batchSize = 128
		nParams = "5m"
		lr = 0.0005
	}
	if strings.HasSuffix(runID, "2") {
		batchSize += 16
	}
	return fmt.Sprintf(`{"n_params":"%s","activation":"%s","batch_size":%d,"shape":"%s","lr":%g}`, nParams, activation, batchSize, shape, lr)
}

func metricRow(runID, groupID, name string, step int64, value float64, unit *string) expstore.MetricRow {
	return expstore.MetricRow{
		Project:    "project-alpha",
		RunGroupID: groupID,
		RunID:      runID,
		MetricName: name,
		Step:       &step,
		Value:      value,
		Unit:       unit,
		Source:     "e2e-fixture",
		Tags:       "{}",
	}
}

func writeMetricParquet(t *testing.T, root, rel string, rows []expstore.MetricRow) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(path, rows); err != nil {
		t.Fatalf("write metric parquet: %v", err)
	}
}

func seedLocalReportArtifact(t *testing.T, root string) {
	t.Helper()
	reportDir := filepath.Join(root, "artifacts", "baseline-seed-1", "report")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportBody := []byte(`<!doctype html><html><body><h1>Qualitative gallery</h1><img src="thumb.png" alt="sample"></body></html>`)
	if err := os.WriteFile(filepath.Join(reportDir, "index.html"), reportBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reportDir, "thumb.png"), []byte("png smoke artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := expstore.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	size := int64(len(reportBody))
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:       "baseline-seed-1",
			Project:     "project-alpha",
			RunGroupID:  "reference-group",
			State:       "succeeded",
			Owner:       "agent",
			CreatedAt:   "2026-05-20T10:00:00Z",
			CompletedAt: "2026-05-20T10:00:00Z",
			ConfigHash:  "config-baseline-seed-1",
			CodeSHA:     "abc123",
			ImageDigest: "sha256:baseline-seed-1",
			TauCommand:  "tau run candidate-training-reference-group --config experiments/candidate-training/reference-group.yaml",
		},
		Artifacts: []expstore.ArtifactRecord{{
			ArtifactID: "artifact-baseline-seed-1-report",
			RunID:      "baseline-seed-1",
			Type:       "report",
			URI:        "artifacts/baseline-seed-1/report/index.html",
			Name:       "Qualitative gallery.html",
			SizeBytes:  &size,
			CreatedAt:  "2026-05-20T10:05:00Z",
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func artifactBundlePath(target, artifactID, assetPath string) string {
	return artifactBundlePathWithBase("/api/v2/stellar", target, artifactID, assetPath)
}

func artifactBundlePathWithBase(basePath, target, artifactID, assetPath string) string {
	targetKey := base64.RawURLEncoding.EncodeToString([]byte(target))
	artifactKey := base64.RawURLEncoding.EncodeToString([]byte(artifactID))
	base := basePath + "/artifact/bundle/" + targetKey + "/" + artifactKey + "/"
	return base + assetPath
}
