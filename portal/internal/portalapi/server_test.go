// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

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

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/queue"
	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/portal/jobs"
)

// newTestServer builds a portal server backed by a Kusto-source Stellar so the
// test needs no seeded local store. It exercises portal routing and the Stellar
// mount, not Stellar's data path.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := NewServer(Options{Stellar: expapi.Options{Source: "kusto"}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	newTestServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

func TestRootRedirectsToPortal(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	newTestServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/portal" {
		t.Fatalf("Location = %q, want /portal", loc)
	}
}

func TestPortalShellServesIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/portal", nil)
	newTestServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if body := rec.Body.String(); len(body) == 0 {
		t.Fatal("portal shell body is empty")
	}
}

func TestPortalPathSPAFallbackAndMissingAsset(t *testing.T) {
	server := newTestServer(t)

	// Extensionless client-route → SPA fallback (200, index document).
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/portal/cluster", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("SPA fallback status = %d, want 200", rec.Code)
	}

	// Missing asset with an extension → 404, so broken references are visible.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/portal/does-not-exist.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", rec.Code)
	}
}

func TestOverviewListsBoards(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil)
	newTestServer(t).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v\n%s", err, rec.Body.String())
	}
	byID := map[string]boardLink{}
	for _, b := range got.Boards {
		byID[b.ID] = b
	}
	for _, id := range []string{"overview", "experiments", "jobs", "cluster", "nodes", "ray", "cost"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("overview missing board %q: %+v", id, got.Boards)
		}
	}
	if exp := byID["experiments"]; exp.Path != "/stellar" || !exp.External {
		t.Fatalf("experiments board = %+v, want path=/stellar external=true", exp)
	}
}

// runningJobsReader serves Kueue lists for the overview's running-now resolution.
// The Workloads list carries one admitted tau workload (run-id + job labels),
// one pending, and one finished; only the admitted one is "running".
type runningJobsReader struct{ stubJobsReader }

func (runningJobsReader) ListWorkloads(context.Context, string) ([]byte, error) {
	return []byte(`{"items":[
      {"metadata":{"name":"wl-run","namespace":"ray",
        "labels":{"` + workloadmeta.LabelRunID + `":"train-77","` + workloadmeta.LabelJob + `":"phi-finetune"}},
       "spec":{"queueName":"jobqueue"},
       "status":{"admission":{"clusterQueue":"taugrid-cq"},
         "conditions":[{"type":"Admitted","status":"True"}]}},
      {"metadata":{"name":"wl-pending","namespace":"ray"},
       "spec":{"queueName":"jobqueue"},
       "status":{"conditions":[{"type":"Admitted","status":"False"}]}},
      {"metadata":{"name":"wl-done","namespace":"ray",
        "labels":{"` + workloadmeta.LabelRunID + `":"train-01"}},
       "status":{"conditions":[{"type":"Admitted","status":"True"},{"type":"Finished","status":"True"}]}}
    ]}`), nil
}

func TestOverviewResolvesRunningCrossLinks(t *testing.T) {
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Jobs:    testOperatorJobs(t, runningJobsReader{}),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v\n%s", err, rec.Body.String())
	}
	if got.RunningUnavailable != "" {
		t.Fatalf("RunningUnavailable = %q, want empty (reader present)", got.RunningUnavailable)
	}
	if len(got.Running) != 1 {
		t.Fatalf("running = %+v, want exactly the 1 admitted+unfinished workload", got.Running)
	}
	run := got.Running[0]
	if run.Job != "phi-finetune" || run.RunID != "train-77" {
		t.Fatalf("running join keys = %q/%q, want phi-finetune/train-77", run.Job, run.RunID)
	}
	if run.ExperimentPath != "/stellar?target=train-77" {
		t.Fatalf("experimentPath = %q, want the Stellar deep-link", run.ExperimentPath)
	}
	if run.ClusterQueue != "taugrid-cq" {
		t.Fatalf("clusterQueue = %q, want taugrid-cq", run.ClusterQueue)
	}
}

func TestOverviewRunningUnavailableWithoutReader(t *testing.T) {
	// newTestServer has no Jobs.Reader → running-now section degrades softly:
	// 200 with a RunningUnavailable hint and no running rows.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (overview must not fail without cluster access)", rec.Code)
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if got.RunningUnavailable == "" {
		t.Fatal("RunningUnavailable should explain the disabled running-now section")
	}
	if len(got.Running) != 0 {
		t.Fatalf("running = %+v, want empty", got.Running)
	}
	// Boards must still be present so the shell renders every tab.
	if len(got.Boards) == 0 {
		t.Fatal("overview must still enumerate boards without cluster access")
	}
}

// The overview's Queue and Running hints must distinguish an unset scope mode
// from missing Kubernetes access the same way the Jobs board does, or a
// correctly-configured operator is sent to re-check a setting already right.
func TestOverviewJobsHintsNameTheirCause(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       JobsOptions
		wantReason string
	}{
		{
			name:       "disabled",
			opts:       JobsOptions{ScopeMode: JobsScopeDisabled},
			wantReason: "computed Jobs board disabled",
		},
		{
			name: "operator without reader",
			opts: JobsOptions{
				ScopeMode:      JobsScopeOperator,
				OperatorScopes: []jobs.Scope{{Team: "research", Namespace: "ray", Queue: "jobqueue"}},
			},
			wantReason: "portal started without Kubernetes access",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, err := NewServer(Options{Stellar: expapi.Options{Source: "kusto"}, Jobs: tc.opts})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
			var got overviewResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode overview: %v", err)
			}
			if got.Cards.QueueUnavailable != tc.wantReason {
				t.Fatalf("QueueUnavailable = %q, want %q", got.Cards.QueueUnavailable, tc.wantReason)
			}
			if got.RunningUnavailable != tc.wantReason {
				t.Fatalf("RunningUnavailable = %q, want %q", got.RunningUnavailable, tc.wantReason)
			}
		})
	}
}

// TestOverviewCardsAllAvailable wires every board's data source and asserts the
// overview handler summarizes each into a headline card (reusing the same stubs
// the per-board handler tests use), with no card marked unavailable.
func TestOverviewCardsAllAvailable(t *testing.T) {
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Jobs:    testOperatorJobs(t, stubJobsReader{}),
		Cluster: ClusterOptions{Querier: &stubClusterQuerier{}},
		Cost:    CostOptions{Querier: &stubCostQuerier{}},
		Ray:     RayOptions{Reader: &stubRayReader{}, Namespace: "ray"},
		Nodes:   NodesOptions{Reader: stubNodesReader{}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v\n%s", err, rec.Body.String())
	}
	c := got.Cards
	if c.FleetUnavailable != "" || c.HealthUnavailable != "" || c.QueueUnavailable != "" ||
		c.CostUnavailable != "" || c.RayUnavailable != "" {
		t.Fatalf("no card should be unavailable: %+v", c)
	}
	// Fleet: from stubNodesReader (2 nodes, 1 GPU node, H100 SKU leading).
	if c.Fleet == nil || c.Fleet.TotalNodes != 2 || c.Fleet.GPUNodes != 1 || c.Fleet.TotalGPUs != 1 {
		t.Fatalf("fleet card = %+v, want 2 nodes / 1 gpu-node / 1 gpu", c.Fleet)
	}
	if c.Fleet.TopSKU != "Standard_NC40ads_H100_v5" {
		t.Fatalf("fleet topSKU = %q, want the GPU SKU leading the rollup", c.Fleet.TopSKU)
	}
	// Health: from stubClusterQuerier (1 GPU row).
	if c.Health == nil || c.Health.TotalGPUs != 1 {
		t.Fatalf("health card = %+v, want 1 GPU", c.Health)
	}
	// Cost: from stubCostQuerier (48 GPU-hours, 1 idle GPU).
	if c.Cost == nil || c.Cost.TotalGPUHours != 48 || c.Cost.IdleGPUs != 1 {
		t.Fatalf("cost card = %+v, want 48 gpu-hours / 1 idle", c.Cost)
	}
	// Ray: from stubRayReader (1 discovered dashboard).
	if c.Ray == nil || c.Ray.Clusters != 1 {
		t.Fatalf("ray card = %+v, want 1 cluster", c.Ray)
	}
	// Queue: empty Kueue lists → a present-but-zero card (not unavailable).
	if c.Queue == nil {
		t.Fatalf("queue card should be present (empty queues → zeros), got nil")
	}
}

// TestOverviewCardsDegradeWithoutKusto gives only the Kubernetes reader: the
// Fleet/Queue/Ray cards light up while the Kusto-backed Health/Cost cards report
// unavailable — each card degrades independently.
func TestOverviewCardsDegradeWithoutKusto(t *testing.T) {
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Jobs:    testOperatorJobs(t, stubJobsReader{}),
		Ray:     RayOptions{Reader: &stubRayReader{}, Namespace: "ray"},
		Nodes:   NodesOptions{Reader: stubNodesReader{}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (overview must not fail without Kusto)", rec.Code)
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	c := got.Cards
	if c.Fleet == nil || c.Queue == nil || c.Ray == nil {
		t.Fatalf("kube-backed cards should be present: fleet=%+v queue=%+v ray=%+v", c.Fleet, c.Queue, c.Ray)
	}
	if c.Health != nil || c.HealthUnavailable == "" {
		t.Fatalf("health card should be unavailable without Kusto: card=%+v reason=%q", c.Health, c.HealthUnavailable)
	}
	if c.Cost != nil || c.CostUnavailable == "" {
		t.Fatalf("cost card should be unavailable without Kusto: card=%+v reason=%q", c.Cost, c.CostUnavailable)
	}
}

// TestOverviewCardsDegradeWithoutKube is the mirror: only the Kusto querier is
// wired, so Health/Cost light up while the Kubernetes-backed Fleet/Queue/Ray
// cards report unavailable.
func TestOverviewCardsDegradeWithoutKube(t *testing.T) {
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Cluster: ClusterOptions{Querier: &stubClusterQuerier{}},
		Cost:    CostOptions{Querier: &stubCostQuerier{}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (overview must not fail without Kubernetes)", rec.Code)
	}
	var got overviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	c := got.Cards
	if c.Health == nil || c.Cost == nil {
		t.Fatalf("kusto-backed cards should be present: health=%+v cost=%+v", c.Health, c.Cost)
	}
	if c.Fleet != nil || c.FleetUnavailable == "" {
		t.Fatalf("fleet card should be unavailable without Kubernetes: card=%+v reason=%q", c.Fleet, c.FleetUnavailable)
	}
	if c.Queue != nil || c.QueueUnavailable == "" {
		t.Fatalf("queue card should be unavailable without Kubernetes: card=%+v reason=%q", c.Queue, c.QueueUnavailable)
	}
	if c.Ray != nil || c.RayUnavailable == "" {
		t.Fatalf("ray card should be unavailable without Kubernetes: card=%+v reason=%q", c.Ray, c.RayUnavailable)
	}
}

func TestStellarMountedUnderPortal(t *testing.T) {
	server := newTestServer(t)

	// The Stellar shell route must resolve through the mount (not 404).
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stellar", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("/stellar not mounted: status = %d", rec.Code)
	}
	// Stellar defaults to X-Frame-Options: DENY; the portal mount relaxes it to
	// SAMEORIGIN so /portal/experiments can embed Stellar in a same-origin iframe.
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("/stellar X-Frame-Options = %q, want SAMEORIGIN (iframe embed would break)", got)
	}

	// A Stellar API route must also resolve through the mount.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stellar/capabilities", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("/api/stellar/capabilities not mounted: status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("/api/stellar/capabilities X-Frame-Options = %q, want SAMEORIGIN", got)
	}
}

// stubJobsReader returns canned Kueue list JSON so the jobs handler can be
// tested without a Kubernetes API.
type stubJobsReader struct{}

func (stubJobsReader) ListLocalQueues(_ context.Context, namespace string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"items":[
		{"metadata":{"name":"jobqueue","namespace":%q},"spec":{"clusterQueue":"taugrid-cq"}}
	]}`, namespace)), nil
}
func (stubJobsReader) ListClusterQueues(context.Context) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}
func (stubJobsReader) ListWorkloads(context.Context, string) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

// writeEmbeddedPolicy writes a minimal TopologyPolicy so the jobs handler does
// not depend on the repo's in-tree policy being reachable from the test CWD.
func writeEmbeddedPolicy(t *testing.T) string {
	t.Helper()
	const policy = `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata:
  name: test-portal-jobs
spec:
  description: "portalapi jobs handler test policy"
  presets:
    azure.research.training.l:
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: jobqueue
      clusterQueue: taugrid-cq
      namespace: ray
    alpha.training:
      team: alpha
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: alpha-queue
      clusterQueue: alpha-cq
      namespace: team-alpha
    beta.training:
      team: beta
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: beta-queue
      clusterQueue: beta-cq
      namespace: team-beta
    alpha.marker:
      team: alpha
      lane: eval
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: alpha-marker-queue
      clusterQueue: alpha-marker-cq
      namespace: team-alpha
    beta.marker:
      team: beta
      lane: eval
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: beta-marker-queue
      clusterQueue: beta-marker-cq
      namespace: team-beta
`
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	return path
}

func testOperatorJobs(t *testing.T, reader jobs.Reader) JobsOptions {
	t.Helper()
	return JobsOptions{
		Reader: reader, ScopeMode: JobsScopeOperator,
		OperatorScopes: []jobs.Scope{{Team: "research", Namespace: "ray", Queue: "jobqueue"}},
		PolicyPath:     writeEmbeddedPolicy(t),
	}
}

func TestJobsBoardServesSnapshot(t *testing.T) {
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Jobs:    testOperatorJobs(t, stubJobsReader{}),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var snap queue.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, rec.Body.String())
	}
	if snap.Namespace != "ray" {
		t.Fatalf("snapshot namespace = %q, want ray", snap.Namespace)
	}
}

func TestJobsBoardUnavailableWithoutReader(t *testing.T) {
	// newTestServer builds a server with no Jobs.Reader → board disabled.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/jobs", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestJobsScopeModeValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts JobsOptions
		dir  WorkspaceDirectory
	}{
		{name: "workspace without directory", opts: JobsOptions{Reader: stubJobsReader{}, ScopeMode: JobsScopeWorkspace}},
		{name: "operator without scopes", opts: JobsOptions{Reader: stubJobsReader{}, ScopeMode: JobsScopeOperator}},
		{name: "disabled with scopes", opts: JobsOptions{
			ScopeMode: JobsScopeDisabled, OperatorScopes: []jobs.Scope{{Team: "research", Namespace: "ray", Queue: "jobqueue"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewServer(Options{
				Stellar: expapi.Options{Source: "kusto"}, Jobs: tc.opts, WorkspaceDirectory: tc.dir,
			}); err == nil {
				t.Fatal("NewServer accepted invalid Jobs scope configuration")
			}
		})
	}
}

// An unset scope mode and a missing Kubernetes reader both yield 503, so the
// status code alone cannot tell an operator which gate they hit: each reason
// must name its own cause and not the other's.
func TestConfiguredJobsModesAllowMissingKubernetesReader(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       JobsOptions
		dir        WorkspaceDirectory
		request    *http.Request
		wantReason string
		notReason  string
	}{
		{
			name:       "disabled",
			opts:       JobsOptions{ScopeMode: JobsScopeDisabled},
			request:    httptest.NewRequest(http.MethodGet, "/api/portal/jobs", nil),
			wantReason: "portal.jobs.scopeMode",
			notReason:  "without Kubernetes access",
		},
		{
			name: "workspace",
			opts: JobsOptions{ScopeMode: JobsScopeWorkspace},
			dir:  testWorkspaceDirectory(t),
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/api/portal/jobs?workspace=alpha", nil)
				req.Header.Set(defaultViewerUserHeader, "user@example.com")
				req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
				return req
			}(),
			wantReason: "without Kubernetes access",
			notReason:  "portal.jobs.scopeMode",
		},
		{
			name: "operator",
			opts: JobsOptions{
				ScopeMode: JobsScopeOperator,
				OperatorScopes: []jobs.Scope{
					{Team: "research", Namespace: "ray", Queue: "jobqueue"},
				},
			},
			request:    httptest.NewRequest(http.MethodGet, "/api/portal/jobs", nil),
			wantReason: "without Kubernetes access",
			notReason:  "portal.jobs.scopeMode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, err := NewServer(Options{
				Stellar: expapi.Options{Source: "kusto"}, Jobs: tc.opts, WorkspaceDirectory: tc.dir,
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			health := httptest.NewRecorder()
			server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if health.Code != http.StatusOK {
				t.Fatalf("health status = %d, want 200", health.Code)
			}

			jobsResponse := httptest.NewRecorder()
			server.Handler().ServeHTTP(jobsResponse, tc.request)
			if jobsResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("jobs status = %d, body = %s; want 503", jobsResponse.Code, jobsResponse.Body.String())
			}
			body := jobsResponse.Body.String()
			if !strings.Contains(body, tc.wantReason) {
				t.Fatalf("jobs body = %s; want it to name %q", body, tc.wantReason)
			}
			if strings.Contains(body, tc.notReason) {
				t.Fatalf("jobs body = %s; must not name the other cause %q", body, tc.notReason)
			}
		})
	}
}

func TestDisabledJobsModePerformsNoQueueRead(t *testing.T) {
	reader := &scopedPortalReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Jobs:    JobsOptions{Reader: reader, ScopeMode: JobsScopeDisabled},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/portal/jobs", "/api/portal/overview"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}
	if len(reader.namespaces) != 0 {
		t.Fatalf("disabled Jobs mode read namespaces %v", reader.namespaces)
	}
}

// stubClusterQuerier returns one canned pivoted GpuHealth() row so the cluster
// handler can be tested without a live Kusto. It records the KQL it received.
type stubClusterQuerier struct{ lastKQL string }

func (s *stubClusterQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	s.lastKQL = kql
	return []kustoquery.Row{
		{"Cluster": "c1", "instance": "node-0", "gpu": "0", "modelName": "H100", "gpu_utilization": 88.0},
	}, nil
}

func TestClusterBoardServesSnapshot(t *testing.T) {
	q := &stubClusterQuerier{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Cluster: ClusterOptions{Querier: q},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/cluster?instance=node-0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		TotalGPUs int `json:"totalGPUs"`
		GPUs      []struct {
			Instance string `json:"instance"`
		} `json:"gpus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, rec.Body.String())
	}
	if got.TotalGPUs != 1 || len(got.GPUs) != 1 || got.GPUs[0].Instance != "node-0" {
		t.Fatalf("snapshot = %+v, want 1 GPU on node-0", got)
	}
	// The ?instance= filter must reach the KQL builder.
	if !strings.Contains(q.lastKQL, "instance == @'node-0'") {
		t.Fatalf("instance filter not applied to KQL:\n%s", q.lastKQL)
	}
}

func TestClusterBoardUnavailableWithoutQuerier(t *testing.T) {
	// newTestServer builds a server with no Cluster.Querier → board disabled.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/cluster", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestClusterBoardDefaultClusterScope verifies the portal's configured default
// cluster scopes the KQL when no ?cluster= is given, and that a per-request
// ?cluster= overrides it (including an explicit empty value to go unscoped).
func TestClusterBoardDefaultClusterScope(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantScoped string // substring that must be present ("" = must be unscoped)
	}{
		{"default applied", "/api/portal/cluster", "Cluster == @'taugrid-flex'"},
		{"override", "/api/portal/cluster?cluster=other", "Cluster == @'other'"},
		{"override empty unscopes", "/api/portal/cluster?cluster=", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &stubClusterQuerier{}
			server, err := NewServer(Options{
				Stellar: expapi.Options{Source: "kusto"},
				Cluster: ClusterOptions{Querier: q, Cluster: "taugrid-flex"},
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if tc.wantScoped == "" {
				if strings.Contains(q.lastKQL, "Cluster ==") {
					t.Fatalf("want unscoped KQL, got cluster filter:\n%s", q.lastKQL)
				}
			} else if !strings.Contains(q.lastKQL, tc.wantScoped) {
				t.Fatalf("KQL missing %q:\n%s", tc.wantScoped, q.lastKQL)
			}
		})
	}
}

// stubCostQuerier answers the cost board's two queries in order: the first call
// (namespace chargeback) returns one namespace row, the second (idle GPUs)
// returns one idle row. It records the KQLs received.
type stubCostQuerier struct {
	calls int
	kqls  []string
}

func (s *stubCostQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	s.kqls = append(s.kqls, kql)
	s.calls++
	switch s.calls {
	case 1:
		return []kustoquery.Row{{"namespace": "research", "GpuHours": 48.0, "Gpus": 4.0, "AvgUtil": 66.0}}, nil
	default:
		return []kustoquery.Row{{"instance": "node-1", "gpu": "0", "namespace": "research", "AvgUtil": 3.0, "Samples": 99.0}}, nil
	}
}

func TestCostBoardServesSnapshot(t *testing.T) {
	q := &stubCostQuerier{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Cost:    CostOptions{Querier: q},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/cost?namespace=research", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		TotalGPUHours float64 `json:"totalGPUHours"`
		Namespaces    []struct {
			Namespace string  `json:"namespace"`
			GPUHours  float64 `json:"gpuHours"`
		} `json:"namespaces"`
		IdleGPUs []struct {
			Instance string `json:"instance"`
		} `json:"idleGPUs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, rec.Body.String())
	}
	if got.TotalGPUHours != 48 || len(got.Namespaces) != 1 || got.Namespaces[0].Namespace != "research" {
		t.Fatalf("snapshot namespaces = %+v, want research 48h", got.Namespaces)
	}
	if len(got.IdleGPUs) != 1 || got.IdleGPUs[0].Instance != "node-1" {
		t.Fatalf("idle gpus = %+v, want [node-1]", got.IdleGPUs)
	}
	// Two queries must have run and the ?namespace= filter must reach both.
	if q.calls != 2 {
		t.Fatalf("querier calls = %d, want 2", q.calls)
	}
	for i, kql := range q.kqls {
		if !strings.Contains(kql, "namespace == @'research'") {
			t.Fatalf("query %d missing namespace filter:\n%s", i, kql)
		}
	}
}

func TestCostBoardUnavailableWithoutQuerier(t *testing.T) {
	// newTestServer builds a server with no Cost.Querier → board disabled.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/cost", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestCostBoardDefaultClusterScope verifies the portal's default cluster scopes
// both cost queries when no ?cluster= is given, and that ?cluster= overrides it
// (empty value goes unscoped).
func TestCostBoardDefaultClusterScope(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantScoped string
	}{
		{"default applied", "/api/portal/cost", "Cluster == @'taugrid-flex'"},
		{"override", "/api/portal/cost?cluster=other", "Cluster == @'other'"},
		{"override empty unscopes", "/api/portal/cost?cluster=", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &stubCostQuerier{}
			server, err := NewServer(Options{
				Stellar: expapi.Options{Source: "kusto"},
				Cost:    CostOptions{Querier: q, Cluster: "taugrid-flex"},
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if len(q.kqls) != 2 {
				t.Fatalf("querier calls = %d, want 2", len(q.kqls))
			}
			for i, kql := range q.kqls {
				if tc.wantScoped == "" {
					if strings.Contains(kql, "Cluster ==") {
						t.Fatalf("query %d: want unscoped, got cluster filter:\n%s", i, kql)
					}
				} else if !strings.Contains(kql, tc.wantScoped) {
					t.Fatalf("query %d missing %q:\n%s", i, tc.wantScoped, kql)
				}
			}
		})
	}
}

// stubRayReader returns one canned <cluster>-head-svc head Service so the ray
// handler can be tested without a Kubernetes API. It records the namespace it
// was asked for.
type stubRayReader struct{ lastNS string }

func (s *stubRayReader) ListServices(_ context.Context, namespace string) ([]byte, error) {
	s.lastNS = namespace
	return []byte(`{"items":[
      {"metadata":{"name":"alpha-head-svc","namespace":"ray",
        "labels":{"ray.io/cluster":"alpha","ray.io/node-type":"head"}},
       "spec":{"type":"ClusterIP"}}
    ]}`), nil
}

func (s *stubRayReader) ListPods(_ context.Context, _ string) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func TestRayBoardServesSnapshot(t *testing.T) {
	reader := &stubRayReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Ray:     RayOptions{Reader: reader},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/ray?namespace=ray", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Total    int `json:"total"`
		Clusters []struct {
			Name      string `json:"name"`
			ProxyPath string `json:"proxyPath"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, rec.Body.String())
	}
	if got.Total != 1 || len(got.Clusters) != 1 || got.Clusters[0].Name != "alpha" {
		t.Fatalf("snapshot = %+v, want 1 cluster alpha", got)
	}
	if got.Clusters[0].ProxyPath != "/api/portal/ray/proxy/ray/alpha/" {
		t.Fatalf("cluster proxyPath = %q, want /api/portal/ray/proxy/ray/alpha/", got.Clusters[0].ProxyPath)
	}
	// The ?namespace= filter must reach the reader.
	if reader.lastNS != "ray" {
		t.Fatalf("reader namespace = %q, want ray", reader.lastNS)
	}
}

func TestRayBoardUnavailableWithoutReader(t *testing.T) {
	// newTestServer builds a server with no Ray.Reader → board disabled.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/ray", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// stubNodesReader serves a canned Nodes list so the board can be tested without
// a Kubernetes API: one H100 GPU node and one non-GPU system node.
type stubNodesReader struct{}

func (stubNodesReader) ListNodes(_ context.Context) ([]byte, error) {
	return []byte(`{"items":[
      {"metadata":{"name":"aks-h100pool-1","labels":{
          "node.kubernetes.io/instance-type":"Standard_NC40ads_H100_v5",
          "kubernetes.azure.com/agentpool":"h100pool"}},
       "status":{"capacity":{"cpu":"40","memory":"329974272Ki","nvidia.com/gpu":"1"},
         "allocatable":{"nvidia.com/gpu":"1"},
         "conditions":[{"type":"Ready","status":"True"}]}},
      {"metadata":{"name":"aks-nodepool1-1","labels":{
          "node.kubernetes.io/instance-type":"Standard_D8s_v3",
          "kubernetes.azure.com/agentpool":"nodepool1"}},
       "status":{"capacity":{"cpu":"8","memory":"32868176Ki"},
         "conditions":[{"type":"Ready","status":"True"}]}}
    ]}`), nil
}
func (stubNodesReader) ListDaemonSets(_ context.Context) ([]byte, error) {
	return []byte(`{"items":[{"metadata":{"namespace":"gpu-monitoring","name":"dcgm-exporter"},"status":{"desiredNumberScheduled":2,"numberReady":1,"numberAvailable":1}}]}`), nil
}

func TestNodesBoardServesSnapshot(t *testing.T) {
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Nodes:   NodesOptions{Reader: stubNodesReader{}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		TotalNodes int `json:"totalNodes"`
		GPUNodes   int `json:"gpuNodes"`
		TotalGPUs  int `json:"totalGPUs"`
		SKUs       []struct {
			SKU  string `json:"sku"`
			GPUs int    `json:"gpus"`
		} `json:"skus"`
		DaemonSets []struct {
			Name    string `json:"name"`
			Desired int    `json:"desired"`
			Ready   int    `json:"ready"`
			Healthy bool   `json:"healthy"`
		} `json:"daemonSets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, rec.Body.String())
	}
	if got.TotalNodes != 2 || got.GPUNodes != 1 || got.TotalGPUs != 1 {
		t.Fatalf("snapshot = %+v, want 2 nodes / 1 gpu-node / 1 gpu", got)
	}
	// GPU SKU sorts first in the rollup.
	if len(got.SKUs) != 2 || got.SKUs[0].SKU != "Standard_NC40ads_H100_v5" || got.SKUs[0].GPUs != 1 {
		t.Fatalf("skus = %+v, want NC40ads_H100 leading with 1 gpu", got.SKUs)
	}
	if len(got.DaemonSets) != 1 || got.DaemonSets[0].Name != "dcgm-exporter" || got.DaemonSets[0].Healthy || got.DaemonSets[0].Ready != 1 || got.DaemonSets[0].Desired != 2 {
		t.Fatalf("daemonSets = %+v, want one unhealthy dcgm-exporter", got.DaemonSets)
	}
}

func TestNodesBoardUnavailableWithoutReader(t *testing.T) {
	// newTestServer builds a server with no Nodes.Reader → board disabled.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/nodes", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// stubRunsReader serves canned Jobs and RayJobs lists so the runs board can be
// tested without a Kubernetes API: one Tau-managed Job, one non-Tau Job (must
// be filtered), and one Tau-managed RayJob. It records the namespace it was
// asked for so the ?namespace= plumbing can be asserted.
type stubRunsReader struct{ lastNS string }

func (s *stubRunsReader) ListJobs(_ context.Context, namespace string) ([]byte, error) {
	s.lastNS = namespace
	return []byte(`{"items":[
      {"metadata":{"name":"train-job","creationTimestamp":"2026-07-02T10:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"train"}},
       "status":{"active":1}},
      {"metadata":{"name":"other-job","creationTimestamp":"2026-07-02T11:00:00Z","labels":{"app":"foo"}},
       "status":{"active":1}}
    ]}`), nil
}

func (s *stubRunsReader) ListRayJobs(_ context.Context, _ string) ([]byte, error) {
	return []byte(`{"items":[
      {"metadata":{"name":"train-rayjob","creationTimestamp":"2026-07-02T11:45:00Z","labels":{"` + workloadmeta.LabelRun + `":"r1"}},
       "status":{"jobDeploymentStatus":"Running"}}
    ]}`), nil
}

func TestRunsBoardServesSnapshot(t *testing.T) {
	reader := &stubRunsReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto", KustoQueryCommand: "query-kusto"},
		Runs:    RunsOptions{Reader: reader, Namespace: "ray"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Namespace string `json:"namespace"`
		Total     int    `json:"total"`
		Runs      []struct {
			Name               string `json:"name"`
			Kind               string `json:"kind"`
			Status             string `json:"status"`
			ExperimentTracking string `json:"experimentTracking"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, rec.Body.String())
	}
	// other-job is filtered (no tau label); the RayJob sorts newest-first.
	if got.Total != 2 {
		t.Fatalf("total = %d, want 2 (train-rayjob + train-job)", got.Total)
	}
	if got.Runs[0].Name != "train-rayjob" || got.Runs[0].Kind != "RayJob" || got.Runs[0].Status != "Running" {
		t.Fatalf("runs[0] = %+v, want train-rayjob/RayJob/Running", got.Runs[0])
	}
	if got.Runs[1].Name != "train-job" || got.Runs[1].Kind != "Job" {
		t.Fatalf("runs[1] = %+v, want train-job/Job", got.Runs[1])
	}
	for _, run := range got.Runs {
		if run.ExperimentTracking != "available" {
			t.Fatalf("%s experimentTracking = %q, want available for the configured Stellar surface", run.Name, run.ExperimentTracking)
		}
	}
	// The configured namespace must reach the reader.
	if reader.lastNS != "ray" {
		t.Fatalf("reader namespace = %q, want ray", reader.lastNS)
	}
}

func TestRunsBoardUnavailableWithoutReader(t *testing.T) {
	// newTestServer builds a server with no Runs.Reader → board disabled.
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/runs", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestManagedRunsReportsConfiguredStellarWithoutFalseUntracked(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "sample-gpu-cluster",
		Workspaces: []WorkspaceRecord{{
			ID:             "research-workspace",
			Cluster:        "sample-gpu-cluster",
			Namespace:      "research-workspace",
			Source:         "kubernetes+kusto",
			ExperimentsURL: "/stellar",
			Default:        true,
			Authorization:  WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &stubRunsReader{}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto", KustoQueryCommand: "query-kusto"},
		Runs:               RunsOptions{Reader: reader},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := managedRequest(t, server, "/api/portal/runs?workspace=research-workspace")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"experimentTracking":"available"`) {
		t.Fatalf("runs response = %d %s", rec.Code, rec.Body.String())
	}
	if reader.lastNS != "research-workspace" {
		t.Fatalf("reader namespace = %q, want resolved workspace namespace", reader.lastNS)
	}
}

func TestRunsBoardDistinguishesUnconfiguredAndUnavailableStellar(t *testing.T) {
	t.Run("configured backend unavailable", func(t *testing.T) {
		server, err := NewServer(Options{
			Stellar: expapi.Options{Source: "kusto"},
			Runs:    RunsOptions{Reader: &stubRunsReader{}, Namespace: "ray"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/runs", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"experimentTracking":"unavailable"`) {
			t.Fatalf("runs response = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("managed Kusto scope overrides local default source", func(t *testing.T) {
		directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
			LocalCluster: "cluster-a",
			Workspaces: []WorkspaceRecord{{
				ID: "alpha", Cluster: "cluster-a", Namespace: "ray", Source: "kubernetes+kusto", ExperimentsURL: "/stellar",
				Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(Options{
			Stellar:            expapi.Options{Source: "auto", StorePath: t.TempDir()},
			Runs:               RunsOptions{Reader: &stubRunsReader{}},
			WorkspaceDirectory: directory,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := managedRequest(t, server, "/api/portal/runs?workspace=alpha")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"experimentTracking":"unavailable"`) {
			t.Fatalf("runs response = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("workspace without experiment surface", func(t *testing.T) {
		directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
			LocalCluster: "cluster-a",
			Workspaces: []WorkspaceRecord{{
				ID: "alpha", Cluster: "cluster-a", Namespace: "ray", Source: "kubernetes+kusto",
				Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(Options{
			Stellar:            expapi.Options{Source: "kusto", KustoQueryCommand: "query-kusto"},
			Runs:               RunsOptions{Reader: &stubRunsReader{}},
			WorkspaceDirectory: directory,
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := managedRequest(t, server, "/api/portal/runs?workspace=alpha")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"experimentTracking":"untracked"`) {
			t.Fatalf("runs response = %d %s", rec.Code, rec.Body.String())
		}
	})
}

type scopedHistoryReader struct {
	rows  []runs.Run
	calls int
	scope runs.HistoryScope
}

func (r *scopedHistoryReader) ListHistory(_ context.Context, scope runs.HistoryScope) ([]runs.Run, error) {
	r.calls++
	r.scope = scope
	return r.rows, nil
}

func TestManagedRunsPassesResolvedScopeToDurableHistory(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Endpoints: []WorkspacePortalEndpoint{
			{Cluster: "cluster-b", Endpoint: "https://cluster-b.portal.example"},
			{Cluster: "cluster-c", Availability: workspaceAvailabilityUnreachable},
		},
		Workspaces: []WorkspaceRecord{
			{ID: "alpha", Cluster: "cluster-a", Namespace: "team-alpha", LocalQueue: "alpha-queue", Source: "kubernetes+kusto", Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}}},
			{ID: "beta", Cluster: "cluster-a", Namespace: "team-beta", LocalQueue: "beta-queue", Source: "kubernetes+kusto", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}}},
			{ID: "remote", Cluster: "cluster-b", Namespace: "team-remote", LocalQueue: "remote-queue", Source: "kubernetes+kusto", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}}},
			{ID: "offline", Cluster: "cluster-c", Namespace: "team-offline", LocalQueue: "offline-queue", Source: "kubernetes+kusto", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}}},
			{ID: "secret", Cluster: "cluster-a", Namespace: "team-secret", LocalQueue: "secret-queue", Source: "kubernetes+kusto", Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"admins"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	history := &scopedHistoryReader{rows: []runs.Run{
		{Name: "alpha-terminal", Kind: "Job", Status: "succeeded", Namespace: "team-alpha", Cluster: "cluster-a", Queue: "alpha-queue"},
		{Name: "beta-terminal", Kind: "Job", Status: "failed", Namespace: "team-beta", Cluster: "cluster-a", Queue: "beta-queue"},
		{Name: "secret-terminal", Kind: "Job", Status: "failed", Namespace: "team-secret", Cluster: "cluster-a", Queue: "secret-queue"},
	}}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Runs: RunsOptions{
			Reader:       &scopedPortalReader{},
			History:      history,
			HistoryTable: "TauExpRunLifecycle",
			HistoryLimit: 25,
		},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := managedRequest(t, server, "/api/portal/runs?workspace=alpha")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"historyState":"available"`) ||
		!strings.Contains(rec.Body.String(), "alpha-terminal") || strings.Contains(rec.Body.String(), "secret-terminal") {
		t.Fatalf("alpha runs response = %d %s", rec.Code, rec.Body.String())
	}
	if history.calls != 1 || history.scope.Cluster != "cluster-a" || history.scope.Namespace != "team-alpha" ||
		history.scope.LocalQueue != "alpha-queue" || history.scope.WorkspaceID != "alpha" ||
		history.scope.Table != "TauExpRunLifecycle" || history.scope.Limit != 25 {
		t.Fatalf("history scope = %+v (calls=%d)", history.scope, history.calls)
	}

	rec = managedRequest(t, server, "/api/portal/runs?workspace=beta")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "beta-terminal") || strings.Contains(rec.Body.String(), "alpha-terminal") {
		t.Fatalf("beta runs response = %d %s", rec.Code, rec.Body.String())
	}
	if history.calls != 2 || history.scope.Namespace != "team-beta" || history.scope.LocalQueue != "beta-queue" || history.scope.WorkspaceID != "beta" {
		t.Fatalf("beta history scope = %+v (calls=%d)", history.scope, history.calls)
	}

	rec = managedRequest(t, server, "/api/portal/runs?workspace=secret")
	if rec.Code != http.StatusNotFound || history.calls != 2 {
		t.Fatalf("unauthorized runs response = %d calls=%d", rec.Code, history.calls)
	}
	rec = managedRequest(t, server, "/api/portal/runs?workspace=alpha&namespace=team-secret")
	if rec.Code != http.StatusBadRequest || history.calls != 2 {
		t.Fatalf("conflicting scope response = %d calls=%d", rec.Code, history.calls)
	}
	rec = managedRequest(t, server, "/api/portal/runs?workspace=remote")
	if rec.Code != http.StatusOK || history.calls != 2 {
		t.Fatalf("remote runs response = %d calls=%d", rec.Code, history.calls)
	}
	rec = managedRequest(t, server, "/api/portal/runs?workspace=offline")
	if rec.Code != http.StatusOK || history.calls != 2 {
		t.Fatalf("offline runs response = %d calls=%d", rec.Code, history.calls)
	}
}

func TestLegacyRunsDoNotApplySyntheticWorkspaceHistoryFilter(t *testing.T) {
	history := &scopedHistoryReader{rows: []runs.Run{{
		Name: "legacy-terminal", Kind: "Job", Status: "succeeded",
		Namespace: "ray", Cluster: "cluster-a",
	}}}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Cluster: ClusterOptions{Cluster: "cluster-a"},
		Runs: RunsOptions{
			Namespace: "ray", Reader: &scopedPortalReader{}, History: history,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/runs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "legacy-terminal") {
		t.Fatalf("legacy runs response = %d %s", rec.Code, rec.Body.String())
	}
	if history.scope.WorkspaceID != "" || history.scope.Namespace != "ray" || history.scope.Cluster != "cluster-a" {
		t.Fatalf("legacy history scope = %+v", history.scope)
	}
	for _, path := range []string{
		"/api/portal/runs?cluster=",
		"/api/portal/runs?cluster=other-cluster",
	} {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s response = %d %s", path, rec.Code, rec.Body.String())
		}
		if history.scope.Cluster != "cluster-a" {
			t.Fatalf("%s durable history cluster = %q, want cluster-a", path, history.scope.Cluster)
		}
	}
}

func TestLegacyDurableHistoryRequiresClusterScope(t *testing.T) {
	_, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Runs: RunsOptions{
			Namespace: "ray", Reader: &scopedPortalReader{}, History: &scopedHistoryReader{},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "explicit cluster scope") {
		t.Fatalf("NewServer error = %v", err)
	}
}

func TestLegacyWorkspacePreservesStellarAndCrossNamespaceBoards(t *testing.T) {
	costQuerier := &stubCostQuerier{}
	rayReader := &stubRayReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Cost:    CostOptions{Querier: costQuerier},
		Ray:     RayOptions{Reader: rayReader},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/workspaces", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"experimentsUrl":"/stellar"`) {
		t.Fatalf("legacy workspace directory = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/cost", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy cost status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, kql := range costQuerier.kqls {
		if strings.Contains(kql, "namespace ==") {
			t.Fatalf("legacy cost was narrowed to Jobs namespace:\n%s", kql)
		}
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/ray", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy ray status = %d: %s", rec.Code, rec.Body.String())
	}
	if rayReader.lastNS != "" {
		t.Fatalf("legacy Ray namespace = %q, want cluster-wide empty namespace", rayReader.lastNS)
	}

	costQuerier.calls = 0
	costQuerier.kqls = nil
	rayReader.lastNS = "not-called"
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy overview status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, kql := range costQuerier.kqls {
		if strings.Contains(kql, "namespace ==") {
			t.Fatalf("legacy overview cost was narrowed to Jobs namespace:\n%s", kql)
		}
	}
	if rayReader.lastNS != "" {
		t.Fatalf("legacy overview Ray namespace = %q, want cluster-wide empty namespace", rayReader.lastNS)
	}
}

func TestLegacyNamespaceOverridesPreserveOriginalSemantics(t *testing.T) {
	rayReader := &stubRayReader{}
	runsReader := &stubRunsReader{}
	server, err := NewServer(Options{
		Stellar: expapi.Options{Source: "kusto"},
		Ray:     RayOptions{Reader: rayReader, Namespace: "configured-ray"},
		Runs:    RunsOptions{Reader: runsReader, Namespace: "configured-runs"},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/ray?namespace=", nil))
	if rec.Code != http.StatusOK || rayReader.lastNS != "configured-ray" {
		t.Fatalf("legacy empty Ray override = status %d namespace %q", rec.Code, rayReader.lastNS)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/portal/runs?namespace=requested-runs", nil))
	if rec.Code != http.StatusOK || runsReader.lastNS != "requested-runs" {
		t.Fatalf("legacy Runs override = status %d namespace %q", rec.Code, runsReader.lastNS)
	}
}

type scopedPortalReader struct {
	namespaces     []string
	daemonSetCalls int
}

func (r *scopedPortalReader) record(namespace string) {
	r.namespaces = append(r.namespaces, namespace)
}

func (r *scopedPortalReader) ListLocalQueues(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	queueName := strings.TrimPrefix(namespace, "team-") + "-queue"
	clusterQueue := strings.TrimPrefix(namespace, "team-") + "-cq"
	return []byte(fmt.Sprintf(`{"items":[
		{"metadata":{"name":%q,"namespace":%q},"spec":{"clusterQueue":%q}}
	]}`, queueName, namespace, clusterQueue)), nil
}

func (r *scopedPortalReader) ListClusterQueues(context.Context) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func (r *scopedPortalReader) ListWorkloads(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"items":[]}`), nil
}

func (r *scopedPortalReader) ListServices(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"items":[]}`), nil
}

func (r *scopedPortalReader) ListPods(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"items":[]}`), nil
}

func (r *scopedPortalReader) ListNodes(context.Context) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}
func (r *scopedPortalReader) ListDaemonSets(context.Context) ([]byte, error) {
	r.daemonSetCalls++
	return []byte(`{"items":[]}`), nil
}

func (r *scopedPortalReader) ListJobs(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"items":[]}`), nil
}

func (r *scopedPortalReader) ListRayJobs(_ context.Context, namespace string) ([]byte, error) {
	r.record(namespace)
	return []byte(`{"items":[]}`), nil
}

type scopedPortalQuerier struct {
	kqls []string
}

func (q *scopedPortalQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	q.kqls = append(q.kqls, kql)
	return nil, nil
}

func managedPortalServer(t *testing.T, cfg WorkspaceDirectoryConfig) (*Server, *scopedPortalReader, *scopedPortalQuerier) {
	t.Helper()
	directory, err := NewWorkspaceDirectory(cfg)
	if err != nil {
		t.Fatalf("NewWorkspaceDirectory: %v", err)
	}
	reader := &scopedPortalReader{}
	querier := &scopedPortalQuerier{}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		Jobs:               JobsOptions{Reader: reader, ScopeMode: JobsScopeWorkspace, PolicyPath: writeEmbeddedPolicy(t)},
		Cluster:            ClusterOptions{Querier: querier},
		Cost:               CostOptions{Querier: querier},
		Ray:                RayOptions{Reader: reader},
		Nodes:              NodesOptions{Reader: reader},
		Runs:               RunsOptions{Reader: reader},
		NodeUtil:           NodeUtilOptions{Querier: querier},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server, reader, querier
}

func managedRequest(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(defaultViewerUserHeader, "viewer@example.com")
	req.Header.Set(defaultViewerGroupsHeader, "researchers")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestManagedWorkspaceSwitchScopesEveryBoard(t *testing.T) {
	cfg := WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{
			{
				ID: "alpha", Name: "Alpha", Cluster: "cluster-a", Team: "alpha", Namespace: "team-alpha",
				LocalQueue: "alpha-queue", ResultScope: "az://results/alpha", Source: "kubernetes+kusto",
				Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
			{
				ID: "beta", Name: "Beta", Cluster: "cluster-a", Team: "beta", Namespace: "team-beta",
				LocalQueue: "beta-queue", ResultScope: "az://results/beta", Source: "kubernetes+kusto",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
		},
	}
	server, reader, querier := managedPortalServer(t, cfg)
	boards := []string{"overview", "jobs", "cluster", "cost", "ray", "nodes", "nodeutil", "runs"}
	for _, workspace := range []struct {
		id        string
		namespace string
		queue     string
	}{
		{id: "alpha", namespace: "team-alpha", queue: "alpha-queue"},
		{id: "beta", namespace: "team-beta", queue: "beta-queue"},
	} {
		reader.namespaces = nil
		querier.kqls = nil
		for _, board := range boards {
			kqlStart := len(querier.kqls)
			rec := managedRequest(t, server, "/api/portal/"+board+"?workspace="+workspace.id)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s/%s status = %d, body = %s", workspace.id, board, rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s/%s decode: %v", workspace.id, board, err)
			}
			scope := body["scope"].(map[string]any)
			if scope["workspace"] != workspace.id || scope["namespace"] != workspace.namespace || scope["localQueue"] != workspace.queue {
				t.Fatalf("%s/%s scope = %+v", workspace.id, board, scope)
			}
			if board == "overview" {
				for _, raw := range body["boards"].([]any) {
					path := raw.(map[string]any)["path"].(string)
					if !strings.Contains(path, "workspace="+workspace.id) {
						t.Fatalf("overview link %q does not preserve workspace %q", path, workspace.id)
					}
				}
			}
			for _, kql := range querier.kqls[kqlStart:] {
				if !strings.Contains(kql, "Cluster == @'cluster-a'") {
					t.Fatalf("%s/%s KQL missing resolved cluster filter:\n%s", workspace.id, board, kql)
				}
				if board != "nodeutil" && !strings.Contains(kql, "namespace == @'"+workspace.namespace+"'") {
					t.Fatalf("%s/%s KQL missing resolved namespace filter:\n%s", workspace.id, board, kql)
				}
			}
		}
		for _, namespace := range reader.namespaces {
			if namespace != workspace.namespace {
				t.Fatalf("%s reader namespace = %q, want %q; all calls = %v", workspace.id, namespace, workspace.namespace, reader.namespaces)
			}
		}
		if len(reader.namespaces) == 0 {
			t.Fatalf("%s made no namespaced Kubernetes calls", workspace.id)
		}
		for _, kql := range querier.kqls {
			if !strings.Contains(kql, "Cluster == @'cluster-a'") {
				t.Fatalf("%s KQL missing resolved cluster filter:\n%s", workspace.id, kql)
			}
		}
	}
	if reader.daemonSetCalls != 0 {
		t.Fatalf("workspace-rbac requests read cluster-wide DaemonSets %d times", reader.daemonSetCalls)
	}
}

func TestManagedWorkspaceAuthorizationAndRemoteStates(t *testing.T) {
	cfg := WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Endpoints: []WorkspacePortalEndpoint{
			{Cluster: "cluster-b", Endpoint: "https://cluster-b.portal.example"},
			{Cluster: "cluster-c", Availability: workspaceAvailabilityUnreachable},
		},
		Workspaces: []WorkspaceRecord{
			{
				ID: "local", Cluster: "cluster-a", Namespace: "local-ns", Source: "kubernetes",
				Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
			{
				ID: "remote", Cluster: "cluster-b", Namespace: "remote-ns", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
			{
				ID: "offline", Cluster: "cluster-c", Namespace: "offline-ns", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationClusterWide, Groups: []string{"researchers"}},
			},
			{
				ID: "secret", Cluster: "cluster-a", Namespace: "secret-ns", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"admins"}},
			},
		},
	}
	server, _, _ := managedPortalServer(t, cfg)

	rec := managedRequest(t, server, "/api/portal/workspaces")
	if rec.Code != http.StatusOK {
		t.Fatalf("directory status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("unauthorized workspace leaked in directory: %s", rec.Body.String())
	}

	rec = managedRequest(t, server, "/api/portal/runs?workspace=secret")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unauthorized direct workspace status = %d, want 404", rec.Code)
	}

	rec = managedRequest(t, server, "/api/portal/runs?workspace=remote")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"redirect"`) ||
		!strings.Contains(rec.Body.String(), `https://cluster-b.portal.example/api/portal/runs?workspace=remote`) {
		t.Fatalf("remote redirect response = %d %s", rec.Code, rec.Body.String())
	}

	rec = managedRequest(t, server, "/api/portal/runs?workspace=offline")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"unreachable"`) {
		t.Fatalf("offline response = %d %s", rec.Code, rec.Body.String())
	}

	rec = managedRequest(t, server, "/api/portal/runs?workspace=local&namespace=secret-ns")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("conflicting namespace status = %d, want 400", rec.Code)
	}
}

func TestManagedWorkspaceIdentityClaimEdgeCases(t *testing.T) {
	cfg := WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{
			{
				ID: "alpha", Cluster: "cluster-a", Namespace: "team-alpha", Source: "kubernetes",
				Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"group-alpha"}},
			},
			{
				ID: "beta", Cluster: "cluster-a", Namespace: "team-beta", Source: "kubernetes",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"group-beta"}},
			},
		},
	}
	server, _, _ := managedPortalServer(t, cfg)

	request := func(user, groups, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if user != "" {
			req.Header.Set(defaultViewerUserHeader, user)
		}
		if groups != "" {
			req.Header.Set(defaultViewerGroupsHeader, groups)
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := request("", "group-alpha", "/api/portal/workspaces"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing user status = %d, want 401", rec.Code)
	}
	if rec := request("viewer@example.com", " , ; ", "/api/portal/runs?workspace=alpha"); rec.Code != http.StatusNotFound {
		t.Fatalf("empty group claim status = %d, want 404", rec.Code)
	}
	for name, claims := range map[string]string{
		"oversized header": strings.Repeat("x", maxViewerGroupsHeaderBytes+1),
		"too many groups":  strings.Repeat("g,", maxViewerGroups+1),
	} {
		t.Run(name, func(t *testing.T) {
			rec := request("viewer@example.com", claims, "/api/portal/workspaces")
			if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
				t.Fatalf("status = %d, want 431: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "team-alpha") || strings.Contains(rec.Body.String(), "team-beta") {
				t.Fatalf("oversized claims leaked workspace metadata: %s", rec.Body.String())
			}
		})
	}

	unauthorized := request("viewer@example.com", "group-alpha", "/api/portal/runs?workspace=beta")
	unknown := request("viewer@example.com", "group-alpha", "/api/portal/runs?workspace=does-not-exist")
	if unauthorized.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound ||
		unauthorized.Body.String() != unknown.Body.String() {
		t.Fatalf("unauthorized and unknown workspaces must be indistinguishable: unauthorized=%d %q unknown=%d %q",
			unauthorized.Code, unauthorized.Body.String(), unknown.Code, unknown.Body.String())
	}
}

type markerPortalReader struct {
	namespaces []string
}

func (r *markerPortalReader) marker(namespace string) string {
	r.namespaces = append(r.namespaces, namespace)
	if namespace == "team-beta" {
		return "beta-marker"
	}
	return "alpha-marker"
}

func (r *markerPortalReader) ListLocalQueues(_ context.Context, namespace string) ([]byte, error) {
	marker := r.marker(namespace)
	return []byte(fmt.Sprintf(`{"items":[
		{"metadata":{"name":"%[1]s-queue","namespace":"%[2]s"},"spec":{"clusterQueue":"%[1]s-cq"}}
	]}`, marker, namespace)), nil
}

func (r *markerPortalReader) ListClusterQueues(context.Context) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func (r *markerPortalReader) ListWorkloads(_ context.Context, namespace string) ([]byte, error) {
	marker := r.marker(namespace)
	return []byte(fmt.Sprintf(`{"items":[{
		"metadata":{"name":"%[1]s-workload","namespace":"%[2]s",
			"labels":{"`+workloadmeta.LabelRunID+`":"%[1]s-run","`+workloadmeta.LabelJob+`":"%[1]s-job"}},
		"spec":{"queueName":"%[1]s-queue"},
		"status":{"admission":{"clusterQueue":"%[1]s-cq"},"conditions":[{"type":"Admitted","status":"True"}]}
	}]}`, marker, namespace)), nil
}

func (r *markerPortalReader) ListServices(_ context.Context, namespace string) ([]byte, error) {
	marker := r.marker(namespace)
	return []byte(fmt.Sprintf(`{"items":[{
		"metadata":{"name":"%[1]s-ray-head-svc","namespace":"%[2]s",
			"labels":{"ray.io/cluster":"%[1]s-ray","ray.io/node-type":"head"}},
		"spec":{"type":"ClusterIP"}
	}]}`, marker, namespace)), nil
}

func (r *markerPortalReader) ListPods(_ context.Context, _ string) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func (r *markerPortalReader) ListNodes(context.Context) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}
func (r *markerPortalReader) ListDaemonSets(context.Context) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func (r *markerPortalReader) ListJobs(_ context.Context, namespace string) ([]byte, error) {
	marker := r.marker(namespace)
	return []byte(fmt.Sprintf(`{"items":[{
		"metadata":{"name":"%[1]s-job","creationTimestamp":"2026-07-02T10:00:00Z",
			"labels":{"`+workloadmeta.LabelRunID+`":"%[1]s-run","kueue.x-k8s.io/queue-name":"%[1]s-queue"}},
		"status":{"active":1}
	}]}`, marker)), nil
}

func (r *markerPortalReader) ListRayJobs(_ context.Context, namespace string) ([]byte, error) {
	r.marker(namespace)
	return []byte(`{"items":[]}`), nil
}

type markerPortalQuerier struct {
	kqls []string
}

func (q *markerPortalQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	q.kqls = append(q.kqls, kql)
	switch {
	case strings.Contains(kql, "evaluate pivot(metric"):
		return []kustoquery.Row{
			{"Cluster": "cluster-a", "instance": "alpha-marker-node", "gpu": "0", "namespace": "team-alpha", "pod": "alpha-marker-pod"},
			{"Cluster": "cluster-a", "instance": "beta-marker-node", "gpu": "0", "namespace": "team-beta", "pod": "beta-marker-pod"},
		}, nil
	case strings.Contains(kql, "GpuHours="):
		return []kustoquery.Row{
			{"namespace": "team-alpha", "GpuHours": 10.0, "Gpus": 1.0, "AvgUtil": 50.0},
			{"namespace": "team-beta", "GpuHours": 90.0, "Gpus": 8.0, "AvgUtil": 95.0},
		}, nil
	case strings.Contains(kql, "Samples=count()"):
		return []kustoquery.Row{
			{"instance": "alpha-marker-node", "gpu": "0", "namespace": "team-alpha", "pod": "alpha-marker-pod", "AvgUtil": 4.0, "Samples": 20.0},
			{"instance": "beta-marker-node", "gpu": "0", "namespace": "team-beta", "pod": "beta-marker-pod", "AvgUtil": 1.0, "Samples": 20.0},
		}, nil
	default:
		return nil, nil
	}
}

func adversarialPortalServer(t *testing.T) (*Server, *markerPortalReader, *markerPortalQuerier) {
	t.Helper()
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{
			{
				ID: "alpha", Name: "Alpha", Cluster: "cluster-a", Team: "alpha", Namespace: "team-alpha",
				LocalQueue: "alpha-marker-queue", ResultScope: "az://alpha-marker-results", Source: "kubernetes+kusto",
				Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"group-alpha"}},
			},
			{
				ID: "beta", Name: "Beta", Cluster: "cluster-a", Team: "beta", Namespace: "team-beta",
				LocalQueue: "beta-marker-queue", ResultScope: "az://beta-marker-results", Source: "kubernetes+kusto",
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"group-beta"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceDirectory: %v", err)
	}
	reader := &markerPortalReader{}
	querier := &markerPortalQuerier{}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		Jobs:               JobsOptions{Reader: reader, ScopeMode: JobsScopeWorkspace, PolicyPath: writeEmbeddedPolicy(t)},
		Cluster:            ClusterOptions{Querier: querier},
		Cost:               CostOptions{Querier: querier},
		Ray:                RayOptions{Reader: reader},
		Nodes:              NodesOptions{Reader: reader},
		Runs:               RunsOptions{Reader: reader},
		NodeUtil:           NodeUtilOptions{Querier: querier},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server, reader, querier
}

func alphaRequest(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(defaultViewerUserHeader, "alpha@example.com")
	req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestManagedWorkspaceAdversarialIsolationMatrix(t *testing.T) {
	server, reader, querier := adversarialPortalServer(t)
	substitutions := "workspace=alpha" +
		"&localQueue=beta-marker-queue" +
		"&resultScope=az%3A%2F%2Fbeta-marker-results&project=beta-marker-project" +
		"&target=beta-marker-run&run=beta-marker-run&name=beta-marker-run" +
		"&detail=beta-marker-detail&log=beta-marker-log&instance=beta-marker-node" +
		"&model=beta-marker-model" +
		"&limit=999999&offset=-1&cursor=beta-marker-cursor&page=999999"

	for _, board := range []string{"overview", "jobs", "cluster", "cost", "ray", "nodes", "nodeutil", "runs"} {
		t.Run(board, func(t *testing.T) {
			rec := alphaRequest(t, server, "/api/portal/"+board+"?"+substitutions)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "beta-marker") || strings.Contains(rec.Body.String(), "team-beta") ||
				strings.Contains(rec.Body.String(), `"workspace":"beta"`) {
				t.Fatalf("Beta data leaked through %s: %s", board, rec.Body.String())
			}
			if board == "overview" || board == "cluster" || board == "cost" || board == "ray" || board == "runs" {
				if !strings.Contains(rec.Body.String(), "alpha-marker") {
					t.Fatalf("%s response did not carry Alpha marker data: %s", board, rec.Body.String())
				}
			}
			if (board == "overview" || board == "runs") && strings.Contains(rec.Body.String(), "/stellar?target=") {
				t.Fatalf("%s advertised an unscoped managed experiment link: %s", board, rec.Body.String())
			}
		})
	}

	for _, board := range []string{"overview", "jobs", "cluster", "cost", "ray", "nodes", "nodeutil", "runs"} {
		for _, conflict := range []string{"namespace=team-beta", "cluster=cluster-beta", "team=beta", "queue=beta-marker-queue"} {
			rec := alphaRequest(t, server, "/api/portal/"+board+"?workspace=alpha&"+conflict)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s accepted conflicting %s: status=%d body=%s", board, conflict, rec.Code, rec.Body.String())
			}
		}
	}

	for _, path := range []string{
		"/api/portal/runs/beta-marker-run?workspace=alpha",
		"/api/portal/runs/beta-marker-run/logs?workspace=alpha",
		"/api/portal/export?workspace=alpha&run=beta-marker-run",
	} {
		rec := alphaRequest(t, server, path)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "beta-marker") {
			t.Fatalf("unsupported secondary route leaked data: %s => %d %s", path, rec.Code, rec.Body.String())
		}
	}

	for _, path := range []string{
		"/api/stellar/experiments?workspace=alpha&project=beta-marker-project&target=beta-marker-run&limit=999999",
		"/api/v1/stellar/runs?workspace=alpha&project=beta-marker-project&target=beta-marker-run",
		"/api/v2/stellar/runs?workspace=alpha&project=beta-marker-project&target=beta-marker-run",
	} {
		rec := alphaRequest(t, server, path)
		if rec.Code == http.StatusTemporaryRedirect || strings.Contains(rec.Body.String(), "beta-marker") {
			t.Fatalf("managed Stellar route did not fail closed: %s => %d %s", path, rec.Code, rec.Body.String())
		}
	}

	rec := alphaRequest(t, server, "/api/portal/ray/proxy/team-beta/beta-marker-ray/?workspace=alpha")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-namespace Ray proxy status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	for _, cookie := range []string{
		"beta|team-beta|beta-marker-ray",
		"alpha|team-beta|beta-marker-ray",
	} {
		req := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)
		req.Header.Set(defaultViewerUserHeader, "alpha@example.com")
		req.Header.Set(defaultViewerGroupsHeader, "group-alpha")
		req.AddCookie(&http.Cookie{Name: rayTargetCookie, Value: cookie})
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("forged Ray cookie %q status = %d, want 404: %s", cookie, rec.Code, rec.Body.String())
		}
	}

	for _, namespace := range reader.namespaces {
		if namespace != "team-alpha" {
			t.Fatalf("reader escaped Alpha namespace: calls=%v", reader.namespaces)
		}
	}
	for _, kql := range querier.kqls {
		if !strings.Contains(kql, "Cluster == @'cluster-a'") {
			t.Fatalf("KQL missing Alpha cluster scope:\n%s", kql)
		}
		if strings.Contains(kql, "GpuHealth()") && !strings.Contains(kql, "namespace == @'team-alpha'") {
			t.Fatalf("workload-attributed KQL missing Alpha namespace scope:\n%s", kql)
		}
	}
}

func TestClusterWideWorkspaceUsesClusterWideInfrastructureMetrics(t *testing.T) {
	if got := workloadMetricsNamespace(WorkspaceScope{Managed: true, Namespace: "team-alpha"}); got != "team-alpha" {
		t.Fatalf("managed scope without an authorization mode used namespace %q, want fail-closed team-alpha", got)
	}

	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "alpha", Cluster: "cluster-a", Namespace: "team-alpha", Source: "kusto",
			Default: true,
			Authorization: WorkspaceAuthorization{
				Mode:  workspaceAuthorizationClusterWide,
				Users: []string{"alpha@example.com"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	clusterQuerier := &stubClusterQuerier{}
	costQuerier := &stubCostQuerier{}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		Cluster:            ClusterOptions{Querier: clusterQuerier},
		Cost:               CostOptions{Querier: costQuerier},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(defaultViewerUserHeader, "alpha@example.com")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := request("/api/portal/cluster?workspace=alpha")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authorizationMode":"cluster-wide"`) {
		t.Fatalf("cluster response = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(clusterQuerier.lastKQL, "Cluster == @'cluster-a'") ||
		strings.Contains(clusterQuerier.lastKQL, "namespace ==") {
		t.Fatalf("cluster-wide workspace KQL =\n%s", clusterQuerier.lastKQL)
	}

	rec = request("/api/portal/cost?workspace=alpha")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authorizationMode":"cluster-wide"`) {
		t.Fatalf("cost response = %d %s", rec.Code, rec.Body.String())
	}
	for _, kql := range costQuerier.kqls {
		if !strings.Contains(kql, "Cluster == @'cluster-a'") || strings.Contains(kql, "namespace ==") {
			t.Fatalf("cluster-wide cost KQL =\n%s", kql)
		}
	}
}

func TestClusterWideWorkspaceCanReadRuntimeDaemonSets(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "platform", Cluster: "cluster-a", Namespace: "tau", Source: "kubernetes",
			Default: true,
			Authorization: WorkspaceAuthorization{
				Mode:  workspaceAuthorizationClusterWide,
				Users: []string{"operator@example.com"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &scopedPortalReader{}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		Nodes:              NodesOptions{Reader: reader},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/portal/nodes?workspace=platform", nil)
	req.Header.Set(defaultViewerUserHeader, "operator@example.com")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if reader.daemonSetCalls != 1 {
		t.Fatalf("cluster-wide workspace read DaemonSets %d times, want 1", reader.daemonSetCalls)
	}
}

func TestManagedWorkspaceStellarCannotCrossWorkspaceScopes(t *testing.T) {
	cfg := WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{
			{
				ID: "alpha", Cluster: "cluster-a", Namespace: "team-alpha", ResultScope: "alpha", Source: "kusto",
				ExperimentsURL: "/stellar", Default: true,
				Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
			},
			{
				ID: "beta", Cluster: "cluster-a", Namespace: "team-beta", ResultScope: "beta", Source: "kusto",
				ExperimentsURL: "/stellar",
				Authorization:  WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"admins"}},
			},
		},
	}
	server, _, _ := managedPortalServer(t, cfg)

	rec := managedRequest(t, server, "/api/portal/workspaces?workspace=alpha")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"experimentsUrl":"/stellar"`) {
		t.Fatalf("managed directory omitted scoped local Stellar = %d %s", rec.Code, rec.Body.String())
	}

	rec = managedRequest(t, server, "/api/stellar/experiments?workspace=alpha&project=beta&target=beta-run")
	if rec.Code == http.StatusTemporaryRedirect || strings.Contains(rec.Body.String(), `"workspace":"beta"`) {
		t.Fatalf("alpha cross-workspace Stellar response = %d %s", rec.Code, rec.Body.String())
	}

	rec = managedRequest(t, server, "/api/stellar/experiments?workspace=beta")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unauthorized beta Stellar response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestManagedOverviewFiltersRunningByResolvedQueue(t *testing.T) {
	directory, err := NewWorkspaceDirectory(WorkspaceDirectoryConfig{
		LocalCluster: "cluster-a",
		Workspaces: []WorkspaceRecord{{
			ID: "local", Cluster: "cluster-a", Namespace: "ray", LocalQueue: "other-queue", Source: "kubernetes",
			Default: true, Authorization: WorkspaceAuthorization{Mode: workspaceAuthorizationRBAC, Groups: []string{"researchers"}},
		}},
	})
	if err != nil {
		t.Fatalf("NewWorkspaceDirectory: %v", err)
	}
	server, err := NewServer(Options{
		Stellar:            expapi.Options{Source: "kusto"},
		Jobs:               JobsOptions{Reader: runningJobsReader{}, PolicyPath: writeEmbeddedPolicy(t)},
		WorkspaceDirectory: directory,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := managedRequest(t, server, "/api/portal/overview?workspace=local")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Running []runningItem `json:"running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if len(body.Running) != 0 {
		t.Fatalf("running = %+v, want no workloads from another LocalQueue", body.Running)
	}
}

func TestPortalShellContainsWorkspaceScopeContract(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/portal", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`id="workspace-select"`,
		`function currentWorkspace()`,
		`fetch(withWorkspace(path)`,
		`field("cluster", activeScope.cluster)`,
		`field("namespace", activeScope.namespace)`,
		`field("queue", activeScope.localQueue)`,
		`field("result scope", activeScope.resultScope)`,
		`if (requested !== currentWorkspace()) return false;`,
		`if (data.scope && requested === currentWorkspace())`,
		`fetchJSON(withWorkspace("/api/stellar/experiments"))`,
		`const view = el("div");`,
		`host.replaceChildren(view);`,
		`No local fallback was used.`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("portal shell missing workspace UI contract %q", want)
		}
	}
}

// TestKustoStellarAvailableForEndpointOnlyPortal covers `portal serve
// --source=kusto --kusto-endpoint=...`: Stellar reaches ADX through the native
// azure-kusto-go transport, so the portal must not report experiment tracking
// as unavailable just because no query command is configured.
func TestKustoStellarAvailableForEndpointOnlyPortal(t *testing.T) {
	native := func(ctx context.Context, query string) (string, error) { return "[]", nil }
	for _, tc := range []struct {
		name string
		opts expapi.Options
		want bool
	}{
		{"native only", expapi.Options{Source: "kusto", KustoEndpoint: "https://adx.example.com", KustoNativeQuery: native}, true},
		{"query command only", expapi.Options{Source: "kusto", KustoQueryCommand: "/bin/true"}, true},
		{"metrics file only", expapi.Options{Source: "kusto", KustoMetricsFile: "rows.jsonl"}, true},
		{"no transport", expapi.Options{Source: "kusto"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kustoStellarAvailable(tc.opts); got != tc.want {
				t.Fatalf("kustoStellarAvailable = %v, want %v", got, tc.want)
			}
			server, err := NewServer(Options{Stellar: tc.opts})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			state := server.experimentSurface(WorkspaceScope{
				Source:         tc.opts.Source,
				ExperimentsURL: "/stellar",
				Availability:   workspaceAvailabilityAvailable,
			})
			want := runs.ExperimentSurfaceUnavailable
			if tc.want {
				want = runs.ExperimentSurfaceAvailable
			}
			if state != want {
				t.Fatalf("experimentSurface = %v, want %v", state, want)
			}
		})
	}
}
