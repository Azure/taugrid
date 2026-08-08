// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/blobstore"
	"github.com/Azure/taugrid/portal/internal/expcockpit"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestSnapshotEndpointReturnsBoundedJSON(t *testing.T) {
	root := seedExpAPIStore(t, 3)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 2, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=experiment-alpha", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, rec.Body.String())
	}
	if len(snapshot.Runs) != 2 {
		t.Fatalf("runs = %d, want 2: %+v", len(snapshot.Runs), snapshot.Runs)
	}
	if snapshot.Runs[0].RunID != "seed-1" || snapshot.Runs[1].RunID != "seed-2" {
		t.Fatalf("runs were not stable ordered: %+v", snapshot.Runs)
	}
	if !containsWarning(snapshot.Warnings, "runs truncated to 2 of 3 matching runs") {
		t.Fatalf("missing truncation warning: %+v", snapshot.Warnings)
	}
}

func TestRunSearchEndpointUsesIndexedMetricSummaries(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/runs?target=experiment-alpha&q=ablation-seed-2&metric_filter=train/return%3E60&lifecycle=succeeded", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.RunSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse run search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Runs) != 1 || result.Runs[0].RunID != "ablation-seed-2" || !result.Runs[0].Successful {
		t.Fatalf("unexpected run search result: %+v", result)
	}
	if len(result.Runs[0].MetricNames) == 0 {
		t.Fatalf("run search should include metric names after summary backfill: %+v", result.Runs[0])
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

	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?workspace=sample", nil)
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
	bare := httptest.NewRequest(http.MethodGet, "/api/stellar/runs", nil)
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

	seriesReq := httptest.NewRequest(http.MethodGet, "/api/stellar/series?metric=train/loss", nil)
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
	workspace, err := server.resolveWorkspace(httptest.NewRequest(http.MethodGet, "/api/stellar/runs", nil))
	if err != nil {
		t.Fatal(err)
	}
	if workspace != DefaultWorkspace {
		t.Fatalf("workspace = %q, want %q", workspace, DefaultWorkspace)
	}
	// A request naming a different workspace is refused rather than silently
	// served from this one.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/runs?workspace=other", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched workspace status = %d, want 400", rec.Code)
	}
}

// TestDeprecatedKustoWorkspaceStillScopes covers the old flag name, which used
// to be the only way to pin a workspace and is still set by existing installs.
func TestDeprecatedKustoWorkspaceStillScopes(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1), KustoWorkspace: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := server.resolveWorkspace(httptest.NewRequest(http.MethodGet, "/api/stellar/runs", nil))
	if err != nil {
		t.Fatal(err)
	}
	if workspace != "legacy" {
		t.Fatalf("workspace = %q, want legacy", workspace)
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
	req = httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?source=local", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace-scoped local source status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Naming a different workspace is refused rather than served.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?workspace=other", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-workspace read status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestExperimentsEndpointSearchesAndAssignsRuns(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?q=project-alpha&lifecycle=succeeded&metric_filter=train/return%3E60", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.ExperimentSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "experiment-alpha" || result.Experiments[0].RunCount != 4 {
		t.Fatalf("unexpected experiment search result: %+v", result.Experiments)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/stellar/experiments", strings.NewReader(`{"run_id":"ablation-seed-2","experiment_id":"api-comparison","name":"API comparison"}`))
	req.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tag-run status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?q=api-comparison", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-search status = %d, body=%s", rec.Code, rec.Body.String())
	}
	result = expstore.ExperimentSearchResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse post-search experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "api-comparison" || result.Experiments[0].RunCount != 1 {
		t.Fatalf("unexpected tagged experiment search result: %+v", result.Experiments)
	}
}

func TestStellarV1AliasesMatchLegacyRoutes(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	for _, tc := range []struct {
		name  string
		route string
	}{
		{name: "snapshot", route: "/snapshot?target=experiment-alpha&metric=train/return&include_static=false"},
		{name: "series", route: "/series?target=experiment-alpha&metric=train/loss&max_points=12"},
		{name: "runs", route: "/runs?target=experiment-alpha&q=baseline"},
		{name: "experiments", route: "/experiments?q=project-alpha"},
		{name: "status", route: "/status?target=experiment-alpha"},
		{name: "artifacts", route: "/artifacts?target=experiment-alpha&type=video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyStatus, legacyBody := responseJSON(t, handler, "/api/stellar"+tc.route)
			v1Status, v1Body := responseJSON(t, handler, "/api/v1/stellar"+tc.route)
			if legacyStatus != v1Status {
				t.Fatalf("status mismatch: legacy=%d v1=%d", legacyStatus, v1Status)
			}
			if !reflect.DeepEqual(normalizeVolatileFields(legacyBody), normalizeVolatileFields(v1Body)) {
				t.Fatalf("v1 payload differed from legacy for %s\nlegacy=%#v\nv1=%#v", tc.route, legacyBody, v1Body)
			}
		})
	}
}

func TestStellarCapabilitiesEndpointReportsLocalAndKustoModes(t *testing.T) {
	localRoot := seedExpAPIStore(t, 1)
	localServer, err := NewServer(Options{StorePath: localRoot})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/stellar/capabilities", "/api/v1/stellar/capabilities", "/api/stellar/capabilities"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		localServer.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body=%s", path, rec.Code, rec.Body.String())
		}
		var caps capabilitiesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatalf("parse %s capabilities: %v\n%s", path, err, rec.Body.String())
		}
		if caps.APIVersion != "v2" || caps.SchemaVersion != expcockpit.SchemaVersion || caps.Service != "tau.exp.stellar" || caps.SourceMode != "local" {
			t.Fatalf("unexpected local capabilities identity: %+v", caps)
		}
		if caps.Paths.CanonicalBasePath != "/api/v2/stellar" || caps.Paths.LegacyBasePath != "/api/stellar" {
			t.Fatalf("unexpected capabilities paths: %+v", caps.Paths)
		}
		if !caps.DataSources["local"].Available || caps.DataSources["kusto"].Available {
			t.Fatalf("unexpected local data sources: %+v", caps.DataSources)
		}
		if caps.DataSources["local"].StorePath != "" || strings.Contains(rec.Body.String(), localRoot) {
			t.Fatalf("capabilities should redact local store paths by default: %s", rec.Body.String())
		}
		if caps.Capabilities["snapshot"]["local"] != true || caps.Capabilities["series_detail"]["kusto"] != false || caps.Capabilities["artifact_content"]["durable_ref"] != true {
			t.Fatalf("unexpected local capability map: %+v", caps.Capabilities)
		}
	}

	tempDir := t.TempDir()
	metricsFile := filepath.Join(tempDir, "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":44}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	autoServer, err := NewServer(Options{
		StorePath:        localRoot,
		Source:           "auto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stellar/capabilities", nil)
	autoServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto capabilities status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var caps capabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("parse auto capabilities: %v\n%s", err, rec.Body.String())
	}
	if caps.SourceMode != "auto" || !caps.DataSources["local"].Available || !caps.DataSources["kusto"].Available {
		t.Fatalf("unexpected auto data sources: %+v", caps)
	}
	if caps.DataSources["local"].StorePath != "" || strings.Contains(rec.Body.String(), localRoot) || strings.Contains(rec.Body.String(), metricsFile) {
		t.Fatalf("auto capabilities should redact local paths by default: %s", rec.Body.String())
	}
	if caps.Capabilities["snapshot"]["kusto"] != true {
		t.Fatalf("unexpected auto capability map: %+v", caps.Capabilities)
	}
	if _, ok := caps.Capabilities["labels"]; ok {
		t.Fatalf("mutable label capability must not be advertised: %+v", caps.Capabilities)
	}
	if _, ok := caps.Capabilities["dashboards"]; ok {
		t.Fatalf("mutable dashboard capability must not be advertised: %+v", caps.Capabilities)
	}

	kustoProjectionServer, err := NewServer(Options{
		Source:           "kusto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stellar/capabilities", nil)
	kustoProjectionServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("kusto projection capabilities status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("parse kusto projection capabilities: %v\n%s", err, rec.Body.String())
	}
	if kustoProjectionServer.StoreRoot() != "kusto://TauExpMetrics" || caps.DataSources["kusto"].Ingestion != "projection" {
		t.Fatalf("projection Kusto defaults should report TauExpMetrics/projection, store=%q caps=%+v", kustoProjectionServer.StoreRoot(), caps.DataSources["kusto"])
	}

	debugCommand := filepath.Join(tempDir, "kusto-query-command-with-sensitive-token")
	kustoServer, err := NewServer(Options{
		Source:            "kusto",
		KustoMetricsFile:  metricsFile,
		Workspace:         "sample",
		KustoEndpoint:     "https://example.kusto.windows.net",
		KustoDatabase:     "Metrics",
		KustoIngestion:    "remote-write",
		KustoQueryCommand: debugCommand,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stellar/capabilities", nil)
	kustoServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("kusto capabilities status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("parse kusto capabilities: %v\n%s", err, rec.Body.String())
	}
	if caps.SourceMode != "kusto" || caps.DataSources["local"].Available || !caps.DataSources["kusto"].Available {
		t.Fatalf("unexpected kusto data sources: %+v", caps)
	}
	if kustoServer.StoreRoot() != "kusto://ExperimentMetrics" || caps.DataSources["kusto"].Ingestion != "remote-write" {
		t.Fatalf("remote-write Kusto defaults should report ExperimentMetrics/remote-write, store=%q caps=%+v", kustoServer.StoreRoot(), caps.DataSources["kusto"])
	}
	if caps.DataSources["kusto"].Endpoint != "" || caps.DataSources["kusto"].Database != "" || caps.DataSources["kusto"].QueryCommand != "" ||
		strings.Contains(rec.Body.String(), tempDir) || strings.Contains(rec.Body.String(), debugCommand) || strings.Contains(rec.Body.String(), "sensitive-host") {
		t.Fatalf("kusto capabilities should redact debug metadata by default: %s", rec.Body.String())
	}
	if caps.Capabilities["artifact_index"]["kusto"] != false {
		t.Fatalf("unexpected kusto capability map: %+v", caps.Capabilities)
	}
	if caps.Capabilities["series_detail"]["kusto"] != true {
		t.Fatalf("configured Kusto query command should advertise focused series support: %+v", caps.Capabilities)
	}
	if !hasCapabilityDegradation(caps.Degradations, "KUSTO_ARTIFACT_INDEX_UNAVAILABLE") || hasCapabilityDegradation(caps.Degradations, "KUSTO_SERIES_DETAIL_UNAVAILABLE") {
		t.Fatalf("unexpected kusto degradations: %+v", caps.Degradations)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stellar/capabilities?debug=1", nil)
	kustoServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("kusto debug capabilities status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("parse kusto debug capabilities: %v\n%s", err, rec.Body.String())
	}
	if caps.DataSources["kusto"].Endpoint != "https://example.kusto.windows.net" || caps.DataSources["kusto"].Database != "Metrics" || caps.DataSources["kusto"].QueryCommand != debugCommand {
		t.Fatalf("debug capabilities should include explicit metadata: %+v", caps.DataSources["kusto"])
	}
}

func TestStellarStructuredErrorCodesAndKustoSeriesAliases(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stellar/snapshot", nil)
	server.Handler().ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT", "Bad Request", "target query parameter is required")

	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(
		`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":44}`+"\n"+
			`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":2,"wall_time":"2026-05-21T00:01:00Z","value":45}`+"\n"+
			`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":3,"wall_time":"2026-05-21T00:02:00Z","value":46}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kustoServer, err := NewServer(Options{Source: "kusto", KustoMetricsFile: metricsFile, Workspace: "sample"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/stellar/series", "/api/stellar/series"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, path+"?source=kusto&target=experiment-alpha&metric=train/return&run_id=seed-1&start_step=2&end_step=3&max_points=600", nil)
		kustoServer.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body=%s", path, rec.Code, rec.Body.String())
		}
		var detail expcockpit.SeriesDetail
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
			t.Fatalf("parse %s Kusto series: %v\n%s", path, err, rec.Body.String())
		}
		if detail.RunID != "seed-1" || detail.MaxPoints != 600 || detail.StartStep == nil || *detail.StartStep != 2 || detail.EndStep == nil || *detail.EndStep != 3 {
			t.Fatalf("%s did not preserve Kusto series scope: %+v", path, detail)
		}
		if len(detail.Chart.Series) != 1 || len(detail.Chart.Series[0].Values) != 2 || detail.Chart.Series[0].Values[0].Step != 2 || detail.Chart.Series[0].Values[1].Step != 3 {
			t.Fatalf("%s returned unexpected Kusto range: %+v", path, detail.Chart)
		}
	}
}

func TestDeprecatedMutableStellarRoutesReportUnsupported(t *testing.T) {
	server, err := NewServer(Options{StorePath: seedExpAPIStore(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/stellar/labels",
		"/api/v1/stellar/labels/run-1",
		"/api/v1/stellar/dashboards",
		"/api/v1/stellar/dashboards/view-1",
		"/api/stellar/labels",
		"/api/stellar/dashboards",
		"/api/stellar/workspaces",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		server.Handler().ServeHTTP(rec, req)
		assertAPIError(t, rec, http.StatusNotImplemented, "UNSUPPORTED_CAPABILITY", "Not Implemented", "mutable Stellar UI state is unsupported")
	}
	for _, path := range []string{
		"/api/v2/stellar/labels",
		"/api/v2/stellar/dashboards",
		"/api/v2/stellar/workspaces",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestStellarV1ArtifactBundleAliasServesReportAssets(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	seedLocalReportArtifact(t, root)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 100})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, versionedArtifactBundlePath("experiment-alpha", "artifact-baseline-seed-1-report", "thumb.png"), nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "png smoke artifact" {
		t.Fatalf("asset body = %q", got)
	}
}

func TestStellarShellPropagatesSourceParam(t *testing.T) {
	server, err := NewServer(Options{
		StorePath: filepath.Join(t.TempDir(), "empty-store"),
		Source:    "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stellar?target=experiment-a&source=auto", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-source="auto"`) {
		t.Fatalf("shell did not propagate source param:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "overlay") || strings.Contains(rec.Body.String(), "/api/stellar/dashboards") || strings.Contains(rec.Body.String(), "/api/stellar/labels") {
		t.Fatalf("shell must not advertise mutable Stellar state:\n%s", rec.Body.String())
	}
}

func TestSnapshotEndpointSupportsKustoSource(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":10,"source":"wandb"}`,
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-2","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:01:00Z","value":20,"source":"wandb"}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Source:           "kusto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
		MaxRuns:          10,
		MaxMetricRows:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=sample-project-wandb-migration&metric=train/return", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, rec.Body.String())
	}
	if snapshot.StorePath != "kusto://TauExpMetrics" || snapshot.Status.Runs != 2 || !snapshot.Chart.HasData {
		t.Fatalf("unexpected Kusto snapshot: %+v", snapshot)
	}
	if snapshot.Chart.Series[0].Overlay.Source != "kusto" {
		t.Fatalf("unexpected overlay source: %+v", snapshot.Chart.Series[0].Overlay)
	}
	if containsWarning(snapshot.Warnings, "overlay") {
		t.Fatalf("Kusto snapshot must not advertise overlays: %+v", snapshot.Warnings)
	}
}

func TestRunSearchEndpointSupportsKustoSource(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":10,"tags":"{\"suite\":\"migration\"}"}`,
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"` + expkusto.RunStatusMetricName + `","step":0,"wall_time":"2026-05-21T00:01:00Z","value":-1,"tags":"{\"tau.status.state\":\"failed\",\"tau.status.reason\":\"OOMKilled\"}"}`,
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-2","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:02:00Z","value":20}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Source:           "kusto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/runs?source=kusto&lifecycle=failed&tag=suite%3Dmigration", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.RunSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse run search: %v\n%s", err, rec.Body.String())
	}
	if result.StorePath != "kusto://TauExpMetrics" || len(result.Runs) != 1 || result.Runs[0].RunID != "seed-1" || result.Runs[0].LifecycleState != "failed" {
		t.Fatalf("unexpected Kusto run search result: %+v", result)
	}
	if result.Runs[0].Tags[expkusto.RunStatusReasonTag] != "OOMKilled" {
		t.Fatalf("missing status reason tag: %+v", result.Runs[0].Tags)
	}
}

func TestExperimentsEndpointSupportsKustoSource(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":10,"tags":"{\"suite\":\"migration\"}"}`,
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"` + expkusto.RunStatusMetricName + `","step":0,"wall_time":"2026-05-21T00:00:30Z","value":1,"tags":"{\"tau.status.state\":\"succeeded\"}"}`,
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-2","metric_name":"eval/score","step":1,"wall_time":"2026-05-21T00:01:00Z","value":20,"tags":"{\"suite\":\"migration\"}"}`,
		`{"workspace_id":"sample","project":"sample-project","experiment_id":"older-control","run_group_id":"control","run_id":"seed-3","metric_name":"train/return","step":1,"wall_time":"2026-05-20T00:00:00Z","value":5}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Source:           "kusto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?q=wandb&tag=suite%3Dmigration&metric_filter=train%2Freturn%3E5&lifecycle=succeeded", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.ExperimentSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse experiment search: %v\n%s", err, rec.Body.String())
	}
	if result.StorePath != "kusto://TauExpMetrics" || len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "sample-project-wandb-migration" {
		t.Fatalf("unexpected Kusto experiment search result: %+v", result)
	}
	if result.Experiments[0].RunCount != 2 || result.Experiments[0].Source != "kusto" {
		t.Fatalf("unexpected Kusto experiment summary: %+v", result.Experiments[0])
	}
	if !containsWarning(result.Warnings, "source=kusto derives lifecycle") {
		t.Fatalf("missing lifecycle approximation warning: %+v", result.Warnings)
	}
}

func TestExperimentsEndpointKustoDiscoverySpansProjectsByDefault(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"pretrain/loss","step":1,"wall_time":"2026-06-16T21:04:00Z","value":1.2}`,
		`{"workspace_id":"sample","project":"vit-enc-vision","experiment_id":"vision-vitenc-public-recipe","run_group_id":"vision-vitenc-paper-param-pilot","run_id":"rad-seed-1","metric_name":"pretrain/loss","step":1,"wall_time":"2026-06-16T17:46:59Z","value":0.8}`,
		`{"workspace_id":"sample","project":"other-project","experiment_id":"older-control","run_group_id":"control","run_id":"control-seed","metric_name":"pretrain/loss","step":1,"wall_time":"2026-06-10T00:00:00Z","value":2.0}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Source:           "kusto",
		KustoProject:     "tau-submit",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.ExperimentSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) != 3 {
		t.Fatalf("legacy KustoProject should not filter discovery, got %+v", result.Experiments)
	}
	if !apiExperimentProjects(result.Experiments)["vit-enc-vision"] {
		t.Fatalf("default Kusto discovery omitted ViT-Enc project: %+v", result.Experiments)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?project=vit-enc-vision", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("project-filtered status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse filtered experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) != 1 || result.Experiments[0].Project != "vit-enc-vision" {
		t.Fatalf("request project filter did not isolate ViT-Enc: %+v", result.Experiments)
	}
}

func TestSnapshotEndpointKustoAmbiguousTargetRequiresProject(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"tau-submit","experiment_id":"shared-target","run_group_id":"default","run_id":"seed-1","metric_name":"pretrain/loss","step":1,"wall_time":"2026-06-16T21:04:00Z","value":1.2}`,
		`{"workspace_id":"sample","project":"vit-enc-vision","experiment_id":"shared-target","run_group_id":"paper-param-pilot","run_id":"seed-2","metric_name":"pretrain/loss","step":1,"wall_time":"2026-06-16T17:46:59Z","value":0.8}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		Source:           "kusto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=shared-target&metric=pretrain%2Floss", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ambiguous Kusto target") || !strings.Contains(rec.Body.String(), "project=") {
		t.Fatalf("expected ambiguous target error, status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=shared-target&project=vit-enc-vision&metric=pretrain%2Floss", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("project-filtered snapshot status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse project-filtered snapshot: %v\n%s", err, rec.Body.String())
	}
	if snapshot.Experiment == nil || snapshot.Experiment.Project != "vit-enc-vision" || len(snapshot.Runs) != 1 || snapshot.Runs[0].RunID != "seed-2" {
		t.Fatalf("project-filtered snapshot returned wrong target: %+v", snapshot)
	}
}

func apiExperimentProjects(experiments []expstore.ExperimentSummary) map[string]bool {
	out := map[string]bool{}
	for _, experiment := range experiments {
		out[experiment.Project] = true
	}
	return out
}

func TestAutoExperimentsEndpointMergesKustoOnlyExperiments(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"project-alpha","experiment_id":"adx-only-experiment","run_group_id":"adx-group","run_id":"adx-seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":44}`,
		`{"workspace_id":"sample","project":"project-alpha","experiment_id":"adx-only-experiment","run_group_id":"adx-group","run_id":"adx-seed-2","metric_name":"eval/score","step":1,"wall_time":"2026-05-21T00:01:00Z","value":55}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		StorePath:        root,
		Source:           "auto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?q=adx-only", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.ExperimentSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "adx-only-experiment" || result.Experiments[0].Source != "kusto" {
		t.Fatalf("auto source did not include Kusto-only experiment: %+v", result)
	}
	if !containsWarning(result.Warnings, "source=auto merged 1 Kusto-backed experiments") {
		t.Fatalf("missing Kusto experiment merge warning: %+v", result.Warnings)
	}
}

func TestAutoExperimentsEndpointFallsBackToKustoWhenLocalStoreMissing(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(`{"workspace_id":"sample","project":"project-alpha","experiment_id":"adx-only-experiment","run_group_id":"adx-group","run_id":"adx-seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":44}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		StorePath:        filepath.Join(t.TempDir(), "missing-store"),
		Source:           "auto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?q=adx-only", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.ExperimentSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "adx-only-experiment" {
		t.Fatalf("auto source did not fall back to Kusto experiment search: %+v", result)
	}
	if !containsWarning(result.Warnings, "source=auto fell back to Kusto because local experiment search failed") {
		t.Fatalf("missing local fallback warning: %+v", result.Warnings)
	}
}

func TestAutoExperimentsEndpointKeepsLocalResultsWhenKustoFails(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{
		StorePath:         root,
		Source:            "auto",
		KustoQueryCommand: filepath.Join(t.TempDir(), "missing-kusto-query"),
		KustoProject:      "project-alpha",
		KustoTargetPoints: 100,
		RequestTimeout:    time.Second,
		MaxRuns:           10,
		MaxMetricRows:     100,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/experiments?q=project-alpha", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result expstore.ExperimentSearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse experiment search: %v\n%s", err, rec.Body.String())
	}
	if len(result.Experiments) == 0 {
		t.Fatalf("auto source discarded local experiments after Kusto failure: %+v", result)
	}
	if !containsWarning(result.Warnings, "source=auto skipped Kusto experiment search because it failed") {
		t.Fatalf("missing Kusto failure warning: %+v", result.Warnings)
	}
}

func TestAutoSourceMergesLocalAndKustoRuns(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"offloaded-seed-2","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":44,"source":"wandb"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		StorePath:        root,
		Source:           "auto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
		MaxRuns:          10,
		MaxMetricRows:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=experiment-alpha&metric=train/return", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, rec.Body.String())
	}
	if snapshot.Status.Runs != 2 || !snapshot.Chart.HasData || snapshot.Chart.Series[0].Overlay.Source != "kusto" {
		t.Fatalf("auto source did not merge Kusto run: %+v", snapshot)
	}
	sourceByRun := map[string]string{}
	for _, run := range snapshot.Runs {
		sourceByRun[run.RunID] = run.Source
	}
	if sourceByRun["seed-1"] != "local" || sourceByRun["offloaded-seed-2"] != "kusto" {
		t.Fatalf("auto source should mark local and Kusto run origins: %+v", sourceByRun)
	}
	if !containsWarning(snapshot.Warnings, "source=auto merged 1 Kusto-backed runs") {
		t.Fatalf("missing auto merge warning: %+v", snapshot.Warnings)
	}
}

func TestAutoSourceKeepsLocalMetricsForDuplicateKustoRuns(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(strings.Join([]string{
		`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"baseline-seed-1","metric_name":"train/return","step":2,"wall_time":"2026-05-21T00:00:00Z","value":999,"source":"adx"}`,
		`{"workspace_id":"sample","project":"project-alpha","experiment_id":"experiment-alpha","run_group_id":"reference-group","run_id":"adx-only-seed-9","metric_name":"train/return","step":2,"wall_time":"2026-05-21T00:01:00Z","value":44,"source":"adx"}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{
		StorePath:        root,
		Source:           "auto",
		KustoMetricsFile: metricsFile,
		Workspace:        "sample",
		MaxRuns:          10,
		MaxMetricRows:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=experiment-alpha&metric=train/return", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, rec.Body.String())
	}
	if snapshot.Status.Runs != 5 || !containsWarning(snapshot.Warnings, "source=auto merged 1 Kusto-backed runs") ||
		!containsWarning(snapshot.Warnings, "source=auto kept local metrics for 1 duplicate Kusto run IDs") {
		t.Fatalf("auto source did not report expected merge/dedupe behavior: %+v", snapshot)
	}
	var localSeries, adxSeries *expcockpit.ChartSeries
	for i := range snapshot.Chart.Series {
		switch snapshot.Chart.Series[i].RunID {
		case "baseline-seed-1":
			localSeries = &snapshot.Chart.Series[i]
		case "adx-only-seed-9":
			adxSeries = &snapshot.Chart.Series[i]
		}
	}
	if localSeries == nil || adxSeries == nil {
		t.Fatalf("missing local or ADX chart series: %+v", snapshot.Chart.Series)
	}
	if localSeries.Overlay.Source == "kusto" || adxSeries.Overlay.Source != "kusto" {
		t.Fatalf("unexpected overlay sources: local=%+v adx=%+v", localSeries.Overlay, adxSeries.Overlay)
	}
	for _, point := range localSeries.Values {
		if point.Value == 999 {
			t.Fatalf("duplicate ADX value leaked into local-winning series: %+v", localSeries.Values)
		}
	}
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
		`data-snapshot-path="/api/stellar/snapshot"`,
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
		`data-snapshot-path="/api/stellar/snapshot"`,
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
		"/api/stellar/preview?target=experiment-alpha",
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
	if body := rec.Body.String(); strings.Contains(body, "/stellar/preview") || strings.Contains(body, "/api/stellar/preview") {
		t.Fatalf("root endpoint should not advertise preview routes: %s", body)
	}
}

func TestStellarSeriesEndpointReturnsBoundedFocusedPoints(t *testing.T) {
	root := seedDenseSeriesExpAPIStore(t, 100)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&start_step=10&end_step=49&max_points=12", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var series expcockpit.SeriesDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &series); err != nil {
		t.Fatalf("parse series: %v\n%s", err, rec.Body.String())
	}
	if series.Target != "experiment-alpha" || series.Metric != "train/loss" || series.MaxPoints != 12 {
		t.Fatalf("unexpected series metadata: %+v", series)
	}
	if !series.Chart.HasData || series.Chart.MetricName != "train/loss" || len(series.Chart.Series) != 1 {
		t.Fatalf("unexpected chart: %+v", series.Chart)
	}
	got := series.Chart.Series[0]
	if got.PointCount != 40 || got.RenderedPoints > 12 || !got.Decimated {
		t.Fatalf("series point budget/count = %+v, want raw window 40 and rendered <= 12", got)
	}
	if got.Points != "" {
		t.Fatalf("series endpoint should omit server SVG point string, got %q", got.Points)
	}
	for _, point := range got.Values {
		if point.Step < 10 || point.Step > 49 {
			t.Fatalf("point outside requested window: %+v", point)
		}
	}
}

func TestStellarSeriesEndpointSupportsStepInterval(t *testing.T) {
	root := seedDenseSeriesExpAPIStore(t, 2000)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&step_interval=20&max_points=8000", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var series expcockpit.SeriesDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &series); err != nil {
		t.Fatalf("parse series: %v\n%s", err, rec.Body.String())
	}
	if series.StepInterval != 20 || series.Chart.StepInterval != 20 {
		t.Fatalf("step interval metadata missing: series=%+v chart=%+v", series, series.Chart)
	}
	if !series.Chart.HasData || len(series.Chart.Series) != 1 {
		t.Fatalf("unexpected chart: %+v", series.Chart)
	}
	got := series.Chart.Series[0]
	if got.PointCount != 2000 || got.RenderedPoints != 101 || len(got.Values) != 101 {
		t.Fatalf("series interval count = %+v values=%d, want raw 2000 and 101 rendered", got, len(got.Values))
	}
	if got.Values[0].Step != 0 || got.Values[len(got.Values)-1].Step != 1999 {
		t.Fatalf("step interval should preserve endpoints: first=%+v last=%+v", got.Values[0], got.Values[len(got.Values)-1])
	}
	for _, point := range got.Values[1 : len(got.Values)-1] {
		if point.Step%20 != 0 {
			t.Fatalf("interior point should be every 20 steps: %+v", point)
		}
	}
}

func TestStellarSeriesEndpointTreatsRunIDAsData(t *testing.T) {
	root := seedDenseSeriesExpAPIStore(t, 10)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&run_id=vit-enc-run-01%27%20OR%20%271%27%3D%271&max_points=10", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 without treating run_id as SQL: %s", rec.Code, rec.Body.String())
	}
}

func TestStellarSeriesEndpointSkipsOutOfWindowMetricFiles(t *testing.T) {
	root := seedWindowedSeriesExpAPIStore(t)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&start_step=10&end_step=19&max_points=20", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var series expcockpit.SeriesDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &series); err != nil {
		t.Fatalf("parse series: %v\n%s", err, rec.Body.String())
	}
	if len(series.Warnings) != 0 {
		t.Fatalf("out-of-window unreadable metric files should be skipped without warnings: %+v", series.Warnings)
	}
	if !series.Chart.HasData || len(series.Chart.Series) != 1 {
		t.Fatalf("unexpected chart: %+v", series.Chart)
	}
	got := series.Chart.Series[0]
	if got.PointCount != 10 || got.RenderedPoints != 10 {
		t.Fatalf("series count = %+v, want only the in-window metric file", got)
	}
	for _, point := range got.Values {
		if point.Step < 10 || point.Step > 19 {
			t.Fatalf("point outside requested window: %+v", point)
		}
	}
}

func TestStellarSeriesEndpointRejectsUnboundedPointRequest(t *testing.T) {
	root := seedDenseSeriesExpAPIStore(t, 10)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&max_points=999999", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "max_points must be at most") {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestStellarSeriesEndpointRejectsInvertedStepWindow(t *testing.T) {
	root := seedDenseSeriesExpAPIStore(t, 10)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&start_step=20&end_step=10", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "start_step must be <= end_step") {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestStellarSeriesEndpointRejectsInvalidStepInterval(t *testing.T) {
	root := seedDenseSeriesExpAPIStore(t, 10)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/series?target=experiment-alpha&metric=train/loss&step_interval=0", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "step_interval must be at least 1") {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
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
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/artifact?target=experiment-alpha&artifact=artifact-baseline-seed-1-rollout", nil)

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
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/artifacts?target=experiment-alpha&type=video", nil)

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
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/artifact?target=experiment-alpha&artifact=artifact-baseline-seed-1-rollout", nil)

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

func TestStellarFrontendUsesBoundedChartHover(t *testing.T) {
	asset, ok, err := expcockpit.ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset not found")
	}
	body := string(asset.Content)
	for _, want := range []string{
		"requestAnimationFrame",
		"nearestPointByX",
		"maxStaticChartPointMarkers",
		"focusedSeriesMaxPoints",
		"/api/stellar/series",
		"loadFocusedSeriesDetail",
		`initialURL.searchParams.get("detail") === "series"`,
		`url.searchParams.set("include_static", "false")`,
		"snapshotWithSummaryDefaults",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("frontend asset missing bounded hover guard %q", want)
		}
	}
	if strings.Contains(body, "for (const point of series.points) {\n      const distance = Math.abs(point.x - x)") {
		t.Fatal("chart hover regressed to scanning every point in every series on each mousemove")
	}
}

func TestHostedStellarLoadsMetricRichStoreOverLocalPort(t *testing.T) {
	root := seedMetricRichExpAPIStore(t)
	server, err := NewServer(Options{
		StorePath:      root,
		DefaultTarget:  "experiment-alpha",
		DefaultMetric:  "train/return",
		MaxRuns:        10,
		MaxMetricRows:  100,
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(ctx, listener)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("serve shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("serve did not shut down")
		}
	})

	client := &http.Client{Timeout: 5 * time.Second}
	baseURL := "http://" + listener.Addr().String()
	status, body := httpGet(t, client, baseURL+"/healthz")
	if status != http.StatusOK {
		t.Fatalf("health status = %d, body=%s", status, body)
	}

	status, body = httpGet(t, client, baseURL+"/api/stellar/snapshot")
	if status != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", status, body)
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, body)
	}
	if snapshot.Target != "experiment-alpha" || snapshot.TargetType != "experiment" {
		t.Fatalf("snapshot target = %s/%s", snapshot.Target, snapshot.TargetType)
	}
	if snapshot.Status.Runs != 4 || snapshot.Status.MetricFiles != 4 {
		t.Fatalf("status did not load all runs/metrics: %+v", snapshot.Status)
	}
	if !snapshot.Chart.HasData || snapshot.Chart.MetricName != "train/return" || len(snapshot.Chart.Series) != 4 {
		t.Fatalf("chart did not load train/return series: %+v", snapshot.Chart)
	}
	if len(snapshot.Chart.Series[0].Values) == 0 {
		t.Fatalf("chart series did not include raw values for frontend charting: %+v", snapshot.Chart.Series[0])
	}
	if !metricOptionSelected(snapshot, "train/return") || !metricOptionExists(snapshot, "eval/score", "Outcome") {
		t.Fatalf("metric options did not include selected chart metric and alternatives: %+v", snapshot.MetricOptions)
	}
	if !snapshot.Sweep.HasData || snapshot.Sweep.MetricName != "train/return" {
		t.Fatalf("sweep did not load train/return data: %+v", snapshot.Sweep)
	}
	if snapshot.Sweep.BestRun == nil || snapshot.Sweep.BestRun.RunID != "ablation-seed-2" {
		t.Fatalf("sweep best run = %+v, want ablation-seed-2", snapshot.Sweep.BestRun)
	}
	if !sweepHasAxis(snapshot, "n_params") || !sweepHasAxis(snapshot, "shape") {
		t.Fatalf("sweep axes missing config params: %+v", snapshot.Sweep.Axes)
	}
	if !sweepHasImportance(snapshot, "shape") {
		t.Fatalf("sweep importance missing shape: %+v", snapshot.Sweep.Importance)
	}
	if snapshot.BestGroupID != "candidate-group" {
		t.Fatalf("best group = %q, want candidate-group", snapshot.BestGroupID)
	}
	for _, tc := range []struct {
		card   string
		metric string
	}{
		{card: "Outcome", metric: "train/return"},
		{card: "Behavior", metric: "policy/entropy"},
		{card: "World model", metric: "world_model/loss"},
		{card: "Systems", metric: "system/gpu_utilization"},
	} {
		if !snapshotHasCardMetric(snapshot, tc.card, tc.metric) {
			t.Fatalf("snapshot missing %s metric %s: %+v", tc.card, tc.metric, snapshot.Cards)
		}
	}
	if !runSystemValue(snapshot, "ablation-seed-1", "GPU class", "a100-80gb") {
		t.Fatalf("run systems did not include GPU class: %+v", snapshot.Runs)
	}
	if len(snapshot.Compare.EventMarkers) == 0 {
		t.Fatalf("expected event markers in compare insights: %+v", snapshot.Compare)
	}
	if snapshot.Summary.NextCommand != "tau run candidate-training-seed-5 --config experiments/candidate-training/candidate-group-seed-5.yaml" {
		t.Fatalf("next command = %q", snapshot.Summary.NextCommand)
	}
	if len(snapshot.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", snapshot.Warnings)
	}

	status, body = httpGet(t, client, baseURL+"/stellar")
	if status != http.StatusOK {
		t.Fatalf("stellar status = %d, body=%s", status, body)
	}
	html := string(body)
	for _, want := range []string{
		"Stellar sweep",
		`id="stellar-root"`,
		`data-target=""`,
		`data-metric="train/return"`,
		`/stellar/assets/app.css`,
		`/stellar/assets/app.js`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stellar frontend HTML missing %q", want)
		}
	}
}

func TestSnapshotEndpointSummaryModeAvoidsHeavyMetricPayload(t *testing.T) {
	root := seedLargeMetricCatalogExpAPIStore(t, 4, 96)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=experiment-alpha&mode=summary", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var summary expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("parse summary: %v\n%s", err, rec.Body.String())
	}
	if summary.PayloadMode != "summary" {
		t.Fatalf("payload mode = %q, want summary", summary.PayloadMode)
	}
	if len(summary.MetricOptions) != 96 {
		t.Fatalf("metric options = %d, want 96", len(summary.MetricOptions))
	}
	if summary.Chart.HasData || summary.Sweep.HasData || len(summary.Cards) != 0 || len(summary.Artifacts) != 0 || len(summary.Events) != 0 || len(summary.Observations) != 0 {
		t.Fatalf("summary included heavy payloads: chart=%+v sweep=%+v cards=%d artifacts=%d events=%d observations=%d", summary.Chart, summary.Sweep, len(summary.Cards), len(summary.Artifacts), len(summary.Events), len(summary.Observations))
	}
	if len(summary.Runs) != 4 || summary.Runs[0].Color == "" {
		t.Fatalf("summary runs missing stable colors: %+v", summary.Runs)
	}
	if !metricOptionExists(summary, "eval/metric_096", "Model diagnostics") {
		t.Fatalf("summary catalog omitted late metric: %+v", summary.MetricOptions)
	}
}

func TestSnapshotEndpointMetricModeKeepsRunColorsStable(t *testing.T) {
	root := seedLargeMetricCatalogExpAPIStore(t, 4, 12)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	first := requestSnapshot(t, server, "/api/stellar/snapshot?target=experiment-alpha&metric=eval/metric_001&mode=metric")
	second := requestSnapshot(t, server, "/api/stellar/snapshot?target=experiment-alpha&metric=eval/metric_002&mode=metric")

	if first.PayloadMode != "metric" || second.PayloadMode != "metric" {
		t.Fatalf("payload modes = %q/%q, want metric", first.PayloadMode, second.PayloadMode)
	}
	if !first.Chart.HasData || !second.Chart.HasData || len(first.MetricOptions) != 12 || len(second.MetricOptions) != 12 {
		t.Fatalf("metric snapshots missing chart/catalog: first=%+v second=%+v", first.Chart, second.Chart)
	}
	if len(first.Cards) != 1 || len(second.Cards) != 1 {
		t.Fatalf("metric mode should summarize only focused metric: cards=%d/%d", len(first.Cards), len(second.Cards))
	}
	firstColors := seriesColorsByRun(first.Chart.Series)
	secondColors := seriesColorsByRun(second.Chart.Series)
	if len(firstColors) != 4 || len(secondColors) != 4 {
		t.Fatalf("unexpected chart series colors: first=%+v second=%+v", first.Chart.Series, second.Chart.Series)
	}
	seen := map[string]string{}
	for runID, color := range firstColors {
		if color == "" {
			t.Fatalf("run %s missing color", runID)
		}
		if otherRun, exists := seen[color]; exists {
			t.Fatalf("runs %s and %s share color %s", otherRun, runID, color)
		}
		seen[color] = runID
		if secondColors[runID] != color {
			t.Fatalf("run %s color changed across metrics: %s vs %s", runID, color, secondColors[runID])
		}
	}
}

func TestSnapshotEndpointMetricModeCanOmitRepeatedStaticFields(t *testing.T) {
	root := seedLargeMetricCatalogExpAPIStore(t, 4, 96)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	full := requestSnapshot(t, server, "/api/stellar/snapshot?target=experiment-alpha&metric=eval/metric_001&mode=metric")
	compact := requestSnapshot(t, server, "/api/stellar/snapshot?target=experiment-alpha&metric=eval/metric_001&mode=metric&include_static=false")

	if len(full.MetricOptions) != 96 || len(full.Runs) != 4 || len(full.RunGroups) == 0 || full.Experiment == nil {
		t.Fatalf("default metric snapshot should retain static fields: options=%d runs=%d groups=%d experiment=%v", len(full.MetricOptions), len(full.Runs), len(full.RunGroups), full.Experiment)
	}
	if len(compact.MetricOptions) != 0 || len(compact.Runs) != 0 || len(compact.RunGroups) != 0 || compact.Experiment != nil {
		t.Fatalf("compact metric snapshot retained repeated static fields: options=%d runs=%d groups=%d experiment=%v", len(compact.MetricOptions), len(compact.Runs), len(compact.RunGroups), compact.Experiment)
	}
	if !compact.Chart.HasData || len(compact.Chart.Series) == 0 || len(compact.Chart.Series[0].Values) == 0 || !compact.Sweep.HasData || len(compact.Cards) != 1 {
		t.Fatalf("compact metric snapshot lost metric-specific render data: chart=%+v sweep=%+v cards=%d", compact.Chart, compact.Sweep, len(compact.Cards))
	}
}

func TestSnapshotEndpointOmitsServerSVGPointStrings(t *testing.T) {
	root := seedLargeMetricCatalogExpAPIStore(t, 1, 1)
	server, err := NewServer(Options{StorePath: root, MaxRuns: 10, MaxMetricRows: 10000})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=experiment-alpha&metric=eval/metric_001&mode=metric", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, rec.Body.String())
	}
	if !snapshot.Chart.HasData || len(snapshot.Chart.Series) != 1 || len(snapshot.Chart.Series[0].Values) == 0 {
		t.Fatalf("API snapshot still needs chart values for browser rendering: %+v", snapshot.Chart)
	}
	if snapshot.Chart.Series[0].Points != "" {
		t.Fatalf("API snapshot retained SVG points: %+v", snapshot.Chart.Series[0])
	}
	if len(snapshot.Sweep.Series) > 0 && snapshot.Sweep.Series[0].Points != "" {
		t.Fatalf("API snapshot retained sweep SVG points: %+v", snapshot.Sweep.Series[0])
	}
}

func TestSnapshotEndpointErrors(t *testing.T) {
	root := seedExpAPIStore(t, 1)
	server, err := NewServer(Options{StorePath: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		code int
	}{
		{name: "missing target", path: "/api/stellar/snapshot", code: http.StatusBadRequest},
		{name: "unknown target", path: "/api/stellar/snapshot?target=missing-target", code: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
}

func TestHealthEndpointReportsMissingStore(t *testing.T) {
	server, err := NewServer(Options{StorePath: filepath.Join(t.TempDir(), "missing-store")})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

// TestKustoNativeQueryServesSnapshotWithoutQueryCommand pins the transport
// contract that removed the shell adapter: a bare native transport must be
// enough for source=kusto to be considered configured, so deployments no longer
// need to stage a shell plus an IMDS-token script into the distroless image.
func TestKustoNativeQueryServesSnapshotWithoutQueryCommand(t *testing.T) {
	server, err := NewServer(Options{
		Source:            "kusto",
		Workspace:         "sample",
		KustoProject:      "crafter",
		KustoTargetPoints: 100,
		RequestTimeout:    time.Second,
		MaxRuns:           10,
		MaxMetricRows:     100,
		KustoNativeQuery: func(context.Context, string) (string, error) {
			return `{"workspace_id":"sample","project":"crafter","question_id":"crafter-dreamerv3","run_group_id":"baseline-a100","run_id":"native-seed","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":44}`, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("native transport should satisfy the Kusto health gate: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/snapshot?target=crafter-dreamerv3&metric=train/return", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "native-seed") {
		t.Fatalf("snapshot did not come from the native transport: %s", rec.Body.String())
	}
}

func responseJSON(t *testing.T, handler http.Handler, path string) (int, any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	handler.ServeHTTP(rec, req)
	var body any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse %s JSON: %v\n%s", path, err, rec.Body.String())
	}
	return rec.Code, body
}

// normalizeVolatileFields zeroes wall-clock-derived fields so the v1/legacy
// route-parity comparison only asserts on payload shape and content. Both
// generated_at and freshness_seconds are recomputed per request, so two
// sequential requests that straddle a second boundary would otherwise fail
// the comparison spuriously.
func normalizeVolatileFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "generated_at" || key == "freshness_seconds" {
				out[key] = ""
				continue
			}
			out[key] = normalizeVolatileFields(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = normalizeVolatileFields(child)
		}
		return out
	default:
		return value
	}
}

func hasCapabilityDegradation(degradations []capabilityDegradation, code string) bool {
	for _, degradation := range degradations {
		if degradation.Code == code {
			return true
		}
	}
	return false
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, errorText, detailSubstring string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, status, rec.Body.String())
	}
	var body struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse error response: %v\n%s", err, rec.Body.String())
	}
	if body.Error != errorText || body.Code != code || body.Status != status || !strings.Contains(body.Detail, detailSubstring) {
		t.Fatalf("unexpected error response: %+v", body)
	}
}

func artifactBundlePath(target, artifactID, assetPath string) string {
	return artifactBundlePathWithBase("/api/stellar", target, artifactID, assetPath)
}

func versionedArtifactBundlePath(target, artifactID, assetPath string) string {
	return artifactBundlePathWithBase("/api/v1/stellar", target, artifactID, assetPath)
}

func artifactBundlePathWithBase(basePath, target, artifactID, assetPath string) string {
	targetKey := base64.RawURLEncoding.EncodeToString([]byte(target))
	artifactKey := base64.RawURLEncoding.EncodeToString([]byte(artifactID))
	base := basePath + "/artifact/bundle/" + targetKey + "/" + artifactKey + "/"
	return base + assetPath
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

func seedLargeMetricCatalogExpAPIStore(t *testing.T, runs, metrics int) string {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(ctx, root, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can Stellar keep large metric catalogs responsive?",
		Group:       "vit-enc-sweep",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for runIndex := 1; runIndex <= runs; runIndex++ {
		runID := fmt.Sprintf("vit-enc-run-%02d", runIndex)
		groupID := "vit-enc-sweep"
		rel := filepath.ToSlash(filepath.Join(expstore.MetricsDir, "project=project-alpha", "group="+groupID, "run="+runID, "large.parquet"))
		rows := make([]expstore.MetricRow, 0, metrics*2)
		for metricIndex := 1; metricIndex <= metrics; metricIndex++ {
			name := fmt.Sprintf("eval/metric_%03d", metricIndex)
			rows = append(rows,
				metricRow(runID, groupID, name, 1, float64(runIndex*metricIndex), nil),
				metricRow(runID, groupID, name, 2, float64(runIndex*metricIndex)+0.5, nil),
			)
		}
		writeMetricParquet(t, store.Root, rel, rows)
		step1 := int64(1)
		step2 := int64(2)
		if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
			Run: expstore.RunRecord{
				RunID:      runID,
				Project:    "project-alpha",
				RunGroupID: groupID,
				State:      "succeeded",
				Owner:      "expapi-test",
				CreatedAt:  "2026-06-03T12:00:00Z",
			},
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
				CreatedAt:     "2026-06-03T12:00:00Z",
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func seedDenseSeriesExpAPIStore(t *testing.T, steps int) string {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(ctx, root, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can Stellar keep dense metric series inspectable?",
		Group:       "vit-enc-sweep",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID := "vit-enc-run-01"
	groupID := "vit-enc-sweep"
	rel := filepath.ToSlash(filepath.Join(expstore.MetricsDir, "project=project-alpha", "group="+groupID, "run="+runID, "dense.parquet"))
	rows := make([]expstore.MetricRow, 0, steps*2)
	for step := 0; step < steps; step++ {
		rows = append(rows,
			metricRow(runID, groupID, "train/loss", int64(step), 10.0/(float64(step)+1), nil),
			metricRow(runID, groupID, "eval/score", int64(step), float64(step)/float64(steps), nil),
		)
	}
	writeMetricParquet(t, store.Root, rel, rows)
	minStep := int64(0)
	maxStep := int64(steps - 1)
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      runID,
			Project:    "project-alpha",
			RunGroupID: groupID,
			State:      "succeeded",
			Owner:      "expapi-test",
			CreatedAt:  "2026-06-03T12:00:00Z",
		},
		MetricFiles: []expstore.MetricFileRecord{{
			FileID:        "metrics-" + runID,
			Path:          rel,
			Format:        "parquet",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    groupID,
			RunID:         runID,
			RowCount:      int64(len(rows)),
			MinStep:       &minStep,
			MaxStep:       &maxStep,
			CreatedAt:     "2026-06-03T12:00:00Z",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func seedWindowedSeriesExpAPIStore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(ctx, root, expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can Stellar skip out-of-window metric files?",
		Group:       "vit-enc-sweep",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID := "vit-enc-run-01"
	groupID := "vit-enc-sweep"
	validRel := filepath.ToSlash(filepath.Join(expstore.MetricsDir, "project=project-alpha", "group="+groupID, "run="+runID, "window.parquet"))
	rows := make([]expstore.MetricRow, 0, 10)
	for step := int64(10); step <= 19; step++ {
		rows = append(rows, metricRow(runID, groupID, "train/loss", step, float64(step), nil))
	}
	writeMetricParquet(t, store.Root, validRel, rows)

	beforeMin, beforeMax := int64(0), int64(4)
	windowMin, windowMax := int64(10), int64(19)
	afterMin, afterMax := int64(50), int64(59)
	metricFiles := []expstore.MetricFileRecord{
		{
			FileID:        "metrics-before-window",
			Path:          filepath.ToSlash(filepath.Join(expstore.MetricsDir, "project=project-alpha", "group="+groupID, "run="+runID, "missing-before.parquet")),
			Format:        "parquet",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    groupID,
			RunID:         runID,
			RowCount:      5,
			MinStep:       &beforeMin,
			MaxStep:       &beforeMax,
			CreatedAt:     "2026-06-03T12:00:00Z",
		},
		{
			FileID:        "metrics-window",
			Path:          validRel,
			Format:        "parquet",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    groupID,
			RunID:         runID,
			RowCount:      int64(len(rows)),
			MinStep:       &windowMin,
			MaxStep:       &windowMax,
			CreatedAt:     "2026-06-03T12:00:01Z",
		},
		{
			FileID:        "metrics-after-window",
			Path:          filepath.ToSlash(filepath.Join(expstore.MetricsDir, "project=project-alpha", "group="+groupID, "run="+runID, "missing-after.parquet")),
			Format:        "parquet",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "project-alpha",
			RunGroupID:    groupID,
			RunID:         runID,
			RowCount:      10,
			MinStep:       &afterMin,
			MaxStep:       &afterMax,
			CreatedAt:     "2026-06-03T12:00:02Z",
		},
	}
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      runID,
			Project:    "project-alpha",
			RunGroupID: groupID,
			State:      "succeeded",
			Owner:      "expapi-test",
			CreatedAt:  "2026-06-03T12:00:00Z",
		},
		MetricFiles: metricFiles,
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

func httpGet(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func requestSnapshot(t *testing.T, server *Server, path string) expcockpit.Snapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot expcockpit.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("parse snapshot: %v\n%s", err, rec.Body.String())
	}
	return snapshot
}

func seriesColorsByRun(series []expcockpit.ChartSeries) map[string]string {
	out := map[string]string{}
	for _, item := range series {
		out[item.RunID] = item.Color
	}
	return out
}

func snapshotHasCardMetric(snapshot expcockpit.Snapshot, cardName, metricName string) bool {
	for _, card := range snapshot.Cards {
		if card.Name != cardName {
			continue
		}
		for _, metric := range card.Metrics {
			if metric.Name == metricName && len(metric.Groups) > 0 {
				return true
			}
		}
	}
	return false
}

func metricOptionSelected(snapshot expcockpit.Snapshot, metricName string) bool {
	for _, option := range snapshot.MetricOptions {
		if option.Name == metricName && option.Selected {
			return true
		}
	}
	return false
}

func metricOptionExists(snapshot expcockpit.Snapshot, metricName, cardName string) bool {
	for _, option := range snapshot.MetricOptions {
		if option.Name == metricName && option.Card == cardName {
			return true
		}
	}
	return false
}

func sweepHasAxis(snapshot expcockpit.Snapshot, axisName string) bool {
	for _, axis := range snapshot.Sweep.Axes {
		if axis.Name == axisName {
			return true
		}
	}
	return false
}

func sweepHasImportance(snapshot expcockpit.Snapshot, parameter string) bool {
	for _, item := range snapshot.Sweep.Importance {
		if item.Name == parameter {
			return true
		}
	}
	return false
}

func runSystemValue(snapshot expcockpit.Snapshot, runID, fieldName, fieldValue string) bool {
	for _, run := range snapshot.Runs {
		if run.RunID != runID {
			continue
		}
		for _, field := range run.Systems {
			if field.Name == fieldName && field.Value == fieldValue {
				return true
			}
		}
	}
	return false
}

func containsWarning(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}
