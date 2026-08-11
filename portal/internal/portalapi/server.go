// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package portalapi serves the unified observability portal: a single HTTP
// surface that aggregates and cross-links the runtime's existing dashboards.
//
// The portal owns the /portal frontend shell and the /api/portal/* board APIs.
// In legacy single-workspace mode it mounts the existing Stellar server
// (internal/expapi) unchanged. Managed workspace mode fails closed unless the
// workspace points to an explicit HTTPS experiment endpoint.
package portalapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/portal/cluster"
	"github.com/Azure/taugrid/portal/internal/portal/cost"
	"github.com/Azure/taugrid/portal/internal/portal/jobdetail"
	"github.com/Azure/taugrid/portal/internal/portal/jobs"
	"github.com/Azure/taugrid/portal/internal/portal/links"
	"github.com/Azure/taugrid/portal/internal/portal/nodes"
	"github.com/Azure/taugrid/portal/internal/portal/nodeutil"
	"github.com/Azure/taugrid/portal/internal/portal/ray"
)

// DefaultAddr is the portal's default listen address. It mirrors Stellar's
// default port so local workflows are familiar; in-cluster the Deployment binds
// 0.0.0.0 behind an internal LB.
const DefaultAddr = "127.0.0.1:8080"

type JobsScopeMode string

const (
	JobsScopeDisabled  JobsScopeMode = "disabled"
	JobsScopeWorkspace JobsScopeMode = "workspace"
	JobsScopeOperator  JobsScopeMode = "operator"
)

// Options configures a portal Server. The embedded Stellar server is built from
// StellarOptions so the portal reuses Stellar's full flag surface verbatim.
type Options struct {
	// Stellar carries the expapi options used to construct the mounted Stellar
	// server (store path, source, Kusto adapter, limits). Required.
	Stellar expapi.Options
	// Jobs configures the Jobs/Queue board. When Jobs.Reader is nil the board
	// is disabled and /api/portal/jobs returns 503, so the portal still runs
	// (shell + Stellar + Kusto boards) without Kubernetes access.
	Jobs JobsOptions
	// Cluster configures the Cluster Health board. When Cluster.Querier is nil
	// the board is disabled and /api/portal/cluster returns 503, so the portal
	// still runs without a --kusto-query-command.
	Cluster ClusterOptions
	// Cost configures the Cost board. When Cost.Querier is nil the board is
	// disabled and /api/portal/cost returns 503.
	Cost CostOptions
	// Ray configures the Ray board. When Ray.Reader is nil the board is disabled
	// and /api/portal/ray returns 503; it reuses the Jobs board's Kubernetes
	// reader, so both light up (or not) together.
	Ray RayOptions
	// Nodes configures the Cluster Nodes board. When Nodes.Reader is nil the
	// board is disabled and /api/portal/nodes returns 503; it reuses the same
	// Kubernetes reader as Jobs and Ray, so all three light up (or not) together.
	Nodes NodesOptions
	// Runs configures the Jobs board (RayJob/Job list). When Runs.Reader is nil
	// the board is disabled and /api/portal/runs returns 503; it reuses the same
	// Kubernetes reader as Jobs/Ray/Nodes, so they light up (or not) together.
	Runs RunsOptions
	// NodeUtil configures the node resource-utilization board. When
	// NodeUtil.Querier is nil the board is disabled and /api/portal/nodeutil
	// returns 503; it reuses the same Kusto querier as Cluster/Cost, so the
	// Kusto-backed boards light up (or not) together.
	NodeUtil NodeUtilOptions
	// WorkspaceDirectory enables authenticated, server-resolved workspace mode.
	// Nil preserves the legacy single/default-workspace behavior.
	WorkspaceDirectory WorkspaceDirectory
	// Identity names trusted Entra identity headers. It is used only when
	// WorkspaceDirectory is configured.
	Identity IdentityOptions
	// KueueViz configures the "Kueue (Live)" board that reverse-proxies the
	// KueueViz dashboard. When KueueViz.Enabled is false the board is disabled
	// and /api/portal/kueueviz* returns 503, so the portal still serves every
	// other board.
	KueueViz KueueVizOptions
}

// JobsOptions configures the Jobs/Queue board's data access. Reader is the
// client-go-backed Kueue reader (internal/portal/kubeclient); tests inject a
// fake. Namespace and PolicyPath mirror the `tau queue status` defaults.
type JobsOptions struct {
	Reader         jobs.Reader
	ScopeMode      JobsScopeMode
	OperatorScopes []jobs.Scope
	PolicyPath     string
}

// ClusterOptions configures the Cluster Health board's data access. Querier is
// the shell-out Kusto client (internal/portal/kustoquery); tests inject a fake.
// When nil the board is disabled. Cluster, when set, is the default cluster the
// board scopes to in a shared Metrics database (a per-request ?cluster= query
// param overrides it); empty means unscoped (every cluster's rows).
type ClusterOptions struct {
	Querier kustoquery.Querier
	Cluster string
}

// CostOptions configures the Cost board's data access. Querier is the shell-out
// Kusto client (internal/portal/kustoquery); tests inject a fake. When nil the
// board is disabled. Cluster, when set, is the default cluster the board scopes
// to in a shared Metrics database (a per-request ?cluster= query param overrides
// it); empty means unscoped.
type CostOptions struct {
	Querier kustoquery.Querier
	Cluster string
}

// RayOptions configures the Ray board's data access. Reader lists core Services
// so the board can discover <rc>-head-svc endpoints; kubeclient.Client satisfies
// it and tests inject a fake. When nil the board is disabled. Transport is used by
// the dashboard reverse proxy to dial each head Service's :8265 over in-cluster
// DNS; when nil it defaults to http.DefaultTransport.
type RayOptions struct {
	Reader    ray.Reader
	Namespace string
	Transport http.RoundTripper
}

// NodesOptions configures the Cluster Nodes board's data access. Reader lists
// core Nodes so the board can describe the fleet's hardware (SKU, agentpool,
// CPU/memory/GPU capacity); kubeclient.Client satisfies it and tests inject a
// fake. When nil the board is disabled.
type NodesOptions struct {
	Reader nodes.Reader
}

// RunsOptions configures the Jobs board's data access. Reader lists batch Jobs
// and ray.io RayJobs so the board can show Tau-managed workloads (name, kind,
// status, age); kubeclient.Client satisfies it and tests inject a fake.
// Namespace scopes both listings (defaults mirror `tau run list`). When Reader
// is nil the board is disabled.
type RunsOptions struct {
	Reader       runs.Reader
	Namespace    string
	History      runs.HistoryReader
	HistoryTable string
	HistoryLimit int
}

// NodeUtilOptions configures the node resource-utilization board's data access.
// Querier is the shell-out Kusto client (internal/portal/kustoquery); tests
// inject a fake. When nil the board is disabled. Cluster, when set, is the
// default cluster the board scopes to in a shared Metrics database (a
// per-request ?cluster= query param overrides it); empty means unscoped.
type NodeUtilOptions struct {
	Querier kustoquery.Querier
	Cluster string
}

// Server is the portal HTTP handler. It composes its own routes with a mounted
// Stellar handler.
type Server struct {
	mux                   *http.ServeMux
	stellar               *expapi.Server
	stellarKustoAvailable bool
	jobs                  JobsOptions
	cluster               ClusterOptions
	cost                  CostOptions
	ray                   RayOptions
	rayCache              rayTargetCache
	nodes                 NodesOptions
	runs                  RunsOptions
	nodeUtil              NodeUtilOptions
	workspaceDirectory    WorkspaceDirectory
	identity              IdentityOptions
	legacyScope           WorkspaceScope
	kueueViz              KueueVizOptions
}

func validateJobsOptions(opts JobsOptions, directory WorkspaceDirectory) error {
	mode := JobsScopeMode(strings.ToLower(strings.TrimSpace(string(opts.ScopeMode))))
	if mode == "" {
		mode = JobsScopeDisabled
	}
	switch mode {
	case JobsScopeDisabled:
		if len(opts.OperatorScopes) > 0 {
			return fmt.Errorf("jobs operator scopes require scope mode %q", JobsScopeOperator)
		}
	case JobsScopeWorkspace:
		if directory == nil {
			return fmt.Errorf("jobs scope mode %q requires a workspace directory", JobsScopeWorkspace)
		}
		if len(opts.OperatorScopes) > 0 {
			return fmt.Errorf("jobs scope mode %q does not accept operator scopes", JobsScopeWorkspace)
		}
	case JobsScopeOperator:
		if err := jobs.ValidateScopes(opts.OperatorScopes); err != nil {
			return fmt.Errorf("invalid jobs operator scopes: %w", err)
		}
	default:
		return fmt.Errorf("invalid jobs scope mode %q", opts.ScopeMode)
	}
	return nil
}

func jobsMode(opts JobsOptions) JobsScopeMode {
	mode := JobsScopeMode(strings.ToLower(strings.TrimSpace(string(opts.ScopeMode))))
	if mode == "" {
		return JobsScopeDisabled
	}
	return mode
}

// NewServer builds a portal Server, constructing and mounting the Stellar
// server from the provided options.
func NewServer(opts Options) (*Server, error) {
	if err := validateJobsOptions(opts.Jobs, opts.WorkspaceDirectory); err != nil {
		return nil, err
	}
	stellar, err := expapi.NewServer(opts.Stellar)
	if err != nil {
		return nil, err
	}
	legacyCluster := firstNonEmpty(opts.Cluster.Cluster, opts.Cost.Cluster, opts.NodeUtil.Cluster)
	if opts.Runs.History != nil && opts.WorkspaceDirectory == nil && legacyCluster == "" {
		return nil, fmt.Errorf("legacy durable run history requires an explicit cluster scope")
	}
	s := &Server{
		mux:                   http.NewServeMux(),
		stellar:               stellar,
		stellarKustoAvailable: kustoStellarAvailable(opts.Stellar),
		jobs:                  opts.Jobs,
		cluster:               opts.Cluster,
		cost:                  opts.Cost,
		ray:                   opts.Ray,
		nodes:                 opts.Nodes,
		runs:                  opts.Runs,
		nodeUtil:              opts.NodeUtil,
		workspaceDirectory:    opts.WorkspaceDirectory,
		identity:              normalizeIdentityOptions(opts.Identity),
		kueueViz:              opts.KueueViz,
		legacyScope: WorkspaceScope{
			WorkspaceID:       "default",
			Name:              "Default",
			Cluster:           legacyCluster,
			Namespace:         firstNonEmpty(opts.Runs.Namespace, opts.Ray.Namespace),
			Source:            opts.Stellar.Source,
			AuthorizationMode: workspaceAuthorizationClusterWide,
			ExperimentsURL:    "/stellar",
			Availability:      workspaceAvailabilityAvailable,
		},
	}
	s.routes()
	return s, nil
}

func kustoStellarAvailable(opts expapi.Options) bool {
	return strings.TrimSpace(opts.KustoMetricsFile) != "" ||
		strings.TrimSpace(opts.KustoQueryCommand) != "" ||
		opts.KustoNativeQuery != nil
}

func (s *Server) experimentSurface(scope WorkspaceScope) runs.ExperimentSurfaceState {
	experimentsURL := strings.TrimSpace(scope.ExperimentsURL)
	if experimentsURL == "" {
		return runs.ExperimentSurfaceUnconfigured
	}
	if scope.Availability != workspaceAvailabilityAvailable {
		return runs.ExperimentSurfaceUnavailable
	}
	if strings.HasPrefix(experimentsURL, "/") &&
		strings.Contains(strings.ToLower(scope.Source), "kusto") &&
		!s.stellarKustoAvailable {
		return runs.ExperimentSurfaceUnavailable
	}
	return runs.ExperimentSurfaceAvailable
}

// Handler returns the portal's root http.Handler with security headers applied.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		s.mux.ServeHTTP(w, r)
	})
}

// Serve runs the portal HTTP server on the listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	err := httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ListenAndServe binds addr and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if strings.TrimSpace(addr) == "" {
		addr = DefaultAddr
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}

func (s *Server) routes() {
	// Portal-owned surface.
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/portal", s.handlePortalShell)
	s.mux.HandleFunc("/portal/", s.handlePortalPath)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/portal/workspaces", s.handleWorkspaces)
	s.mux.HandleFunc("/api/portal/overview", s.handleOverview)
	s.mux.HandleFunc("/api/portal/jobs", s.handleJobs)
	s.mux.HandleFunc("/api/portal/cluster", s.handleCluster)
	s.mux.HandleFunc("/api/portal/cost", s.handleCost)
	s.mux.HandleFunc("/api/portal/ray", s.handleRay)
	s.mux.HandleFunc("/api/portal/ray/history/", s.handleRayHistory)
	s.mux.HandleFunc("/api/portal/ray/proxy/", s.handleRayProxy)
	// Ray dashboard SPA root-absolute assets. The dashboard fetches these from the
	// origin root (/api, /static, ...), so they carry no proxy prefix; the asset
	// handler uses the ray_target cookie set by handleRayProxy to pick the upstream
	// head Service. Registered on the exact Ray dashboard prefixes to avoid
	// shadowing the portal's own routes.
	for _, p := range rayAssetPrefixes {
		s.mux.HandleFunc(p, s.handleRayAsset)
	}
	s.mux.HandleFunc("/api/portal/nodes", s.handleNodes)
	s.mux.HandleFunc("/api/portal/nodeutil", s.handleNodeUtil)
	s.mux.HandleFunc("/api/portal/runs", s.handleRuns)
	// Trailing slash keeps the per-job detail route
	// ("/api/portal/runs/{namespace}/{name}") distinct from the runs list above.
	s.mux.HandleFunc("/api/portal/runs/", s.handleJobDetail)

	// KueueViz "Kueue (Live)" board — reverse-proxied under
	// /api/portal/kueueviz/. The frontend/env.js/asset routes are embedded in a
	// same-origin iframe, so wrap them with framedSameOrigin to relax any DENY
	// X-Frame-Options. The /ws/ upgrade path is dialed directly (not framed).
	// Root-absolute SPA assets (/assets/...) route via the kueueviz_target cookie.
	kueueVizHandler := framedSameOrigin(http.HandlerFunc(s.handleKueueVizProxy))
	s.mux.Handle(kueueVizProxyPrefix, kueueVizHandler)
	kueueVizAssetHandler := framedSameOrigin(http.HandlerFunc(s.handleKueueVizAsset))
	for _, p := range kueueVizAssetPrefixes {
		s.mux.Handle(p, kueueVizAssetHandler)
	}

	// Mounted Stellar surface — reused unchanged for the Experiments board.
	// Stellar registers its own /stellar* and versioned/legacy API routes.
	// Managed workspace mode gates those prefixes before delegation.
	//
	// Stellar's handler hard-codes X-Frame-Options: DENY (internal/expapi), which
	// would overwrite the portal's SAMEORIGIN and block the /portal/experiments
	// iframe. Wrap the delegated handler so DENY is relaxed to SAMEORIGIN at the
	// mount boundary — Stellar itself stays untouched, and any stricter per-route
	// header it sets (e.g. a report-artifact CSP) is left alone.
	stellarHandler := framedSameOrigin(s.stellar.Handler())
	workspaceStellarHandler := s.workspaceAwareStellar(stellarHandler)
	s.mux.Handle("/stellar", workspaceStellarHandler)
	s.mux.Handle("/stellar/", workspaceStellarHandler)
	s.mux.Handle("/api/stellar/", workspaceStellarHandler)
	s.mux.Handle("/api/v1/stellar/", workspaceStellarHandler)
	s.mux.Handle("/api/v2/stellar/", workspaceStellarHandler)
}

// handleRoot redirects the bare origin to the portal shell.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		serveIndexHTML(w, http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/portal", http.StatusFound)
}

// handlePortalShell serves the SPA entry document at /portal.
func (s *Server) handlePortalShell(w http.ResponseWriter, _ *http.Request) {
	serveIndexHTML(w, http.StatusOK)
}

// handlePortalPath serves static frontend assets under /portal/, falling back to
// the SPA index for client-side routes (e.g. /portal/cluster).
func (s *Server) handlePortalPath(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/portal/")
	if serveAsset(w, name) {
		return
	}
	// Unmatched path under /portal/ → SPA fallback so the frontend router owns
	// it. Missing static assets (with a file extension) still 404 to avoid
	// masking broken asset references.
	if strings.Contains(name, ".") {
		http.NotFound(w, r)
		return
	}
	serveIndexHTML(w, http.StatusOK)
}

// handleHealth is the liveness/readiness probe target.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// boardLink describes one portal board for the frontend shell/navigation.
type boardLink struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	External bool   `json:"external,omitempty"`
}

// overviewResponse is the cross-source home payload. It always enumerates the
// boards and a per-board status card (Cards); when the portal has Kubernetes
// access it also pre-resolves the "running now" cross-links: admitted Kueue
// Workloads with their Experiment deep-link, so the home page answers "what is
// running, and where do I click to see its metrics" without visiting each board.
// RunningUnavailable explains a disabled running-now section (no cluster access)
// so the frontend can render a hint instead of an empty table.
type overviewResponse struct {
	Boards             []boardLink   `json:"boards"`
	Cards              overviewCards `json:"cards"`
	Running            []runningItem `json:"running"`
	RunningUnavailable string        `json:"runningUnavailable,omitempty"`
}

// overviewCards carries the headline status of each board so the home page is a
// live dashboard, not just a link directory. Every card is independent: a nil
// data source or a query/list error sets that card's *Unavailable string and
// leaves the card nil, so the rest of the overview still renders (mirroring each
// board handler's own soft-degrade contract). The Kubernetes-backed cards
// (Fleet/Queue/Ray) light up together with the Jobs/Ray/Nodes reader; the
// Kusto-backed cards (Health/Cost) light up together with the query command.
type overviewCards struct {
	Fleet  *fleetCard  `json:"fleet,omitempty"`
	Health *healthCard `json:"health,omitempty"`
	Queue  *queueCard  `json:"queue,omitempty"`
	Cost   *costCard   `json:"cost,omitempty"`
	Ray    *rayCard    `json:"ray,omitempty"`

	FleetUnavailable  string `json:"fleetUnavailable,omitempty"`
	HealthUnavailable string `json:"healthUnavailable,omitempty"`
	QueueUnavailable  string `json:"queueUnavailable,omitempty"`
	CostUnavailable   string `json:"costUnavailable,omitempty"`
	RayUnavailable    string `json:"rayUnavailable,omitempty"`
}

// fleetCard is the Cluster Nodes headline: fleet size and capacity, plus the
// leading GPU SKU. Drills down to /portal/nodes.
type fleetCard struct {
	TotalNodes     int     `json:"totalNodes"`
	ReadyNodes     int     `json:"readyNodes"`
	GPUNodes       int     `json:"gpuNodes"`
	TotalGPUs      int64   `json:"totalGPUs"`
	TotalCPUCores  int64   `json:"totalCPUCores"`
	TotalMemoryGiB float64 `json:"totalMemoryGiB"`
	TopSKU         string  `json:"topSKU,omitempty"`
}

// healthCard is the Cluster Health headline: total GPUs seen in the window and
// how many are flagged unhealthy. Drills down to /portal/cluster.
type healthCard struct {
	TotalGPUs int `json:"totalGPUs"`
	ErrorGPUs int `json:"errorGPUs"`
}

// queueCard is the Jobs/Queue headline: summed pending/admitted counts and GPU
// pressure across all queue groups. Drills down to /portal/jobs.
type queueCard struct {
	Pending     int   `json:"pending"`
	Admitted    int   `json:"admitted"`
	GPUUsed     int64 `json:"gpuUsed"`
	GPUHeadroom int64 `json:"gpuHeadroom"`
}

// costCard is the Cost headline: total GPU-hours over the window and the idle
// GPU count. Drills down to /portal/cost.
type costCard struct {
	Window        string  `json:"window"`
	TotalGPUHours float64 `json:"totalGPUHours"`
	IdleGPUs      int     `json:"idleGPUs"`
}

// rayCard is the Ray headline: how many Ray dashboards were discovered. Drills
// down to /portal/ray.
type rayCard struct {
	Clusters int `json:"clusters"`
}

// runningItem is one admitted workload on the overview, resolved to its
// cross-board links. ExperimentPath is the Job→Experiment deep-link (empty when
// the workload carries no run-id, e.g. a non-tau Job).
type runningItem struct {
	Job          string `json:"job,omitempty"`
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	RunID        string `json:"runId,omitempty"`
	Queue        string `json:"queue,omitempty"`
	ClusterQueue string `json:"clusterQueue,omitempty"`
	// Project, Experiment, and Group are the Stellar identity `tau run` stamps
	// on the Job and Kueue copies onto the Workload, so a running row reads as
	// the experiment it belongs to rather than a bare run-id. They are
	// label-normalized (see links.Workload) and therefore display-only: they are
	// deliberately not used to build ExperimentPath, because Stellar matches
	// ?project= exactly and a folded value would deep-link to nothing.
	Project            string `json:"project,omitempty"`
	Experiment         string `json:"experiment,omitempty"`
	Group              string `json:"group,omitempty"`
	ExperimentPath     string `json:"experimentPath,omitempty"`
	ExperimentTracking string `json:"experimentTracking"`
}

// portalBoards is the canonical board list surfaced by the shell. Experiments
// links out to the mounted Stellar SPA for the MVP; the rest are portal-native
// routes filled in by later increments.
func portalBoards(scope WorkspaceScope) []boardLink {
	workspaceID := ""
	experimentsPath := "/stellar"
	experimentsExternal := true
	if scope.Managed {
		workspaceID = scope.WorkspaceID
		experimentsPath = "/portal/experiments"
		experimentsExternal = false
	}
	return []boardLink{
		{ID: "overview", Title: "Overview", Path: links.WorkspacePath("/portal", workspaceID)},
		{ID: "experiments", Title: "Experiments", Path: links.WorkspacePath(experimentsPath, workspaceID), External: experimentsExternal},
		{ID: "jobs", Title: "Jobs / Queue", Path: links.WorkspacePath("/portal/jobs", workspaceID)},
		{ID: "cluster", Title: "Cluster Health", Path: links.WorkspacePath("/portal/cluster", workspaceID)},
		{ID: "nodes", Title: "Cluster Nodes", Path: links.WorkspacePath("/portal/nodes", workspaceID)},
		{ID: "ray", Title: "Ray", Path: links.WorkspacePath("/portal/ray", workspaceID)},
		{ID: "cost", Title: "Cost", Path: links.WorkspacePath("/portal/cost", workspaceID)},
	}
}

// boardsForScope returns the board list surfaced by the shell for the given
// workspace scope, appending the optional "Kueue (Live)" board only when the
// KueueViz proxy is enabled so a portal without it does not advertise a 503
// route.
func (s *Server) boardsForScope(scope WorkspaceScope) []boardLink {
	boards := portalBoards(scope)
	if s.kueueViz.Enabled {
		workspaceID := ""
		if scope.Managed {
			workspaceID = scope.WorkspaceID
		}
		boards = append(boards, boardLink{ID: "kueueviz", Title: "Kueue (Live)", Path: links.WorkspacePath("/portal/kueueviz", workspaceID)})
	}
	return boards
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	resp := overviewResponse{Boards: s.boardsForScope(scope), Running: []runningItem{}}
	s.resolveCards(r.Context(), &resp, scope)
	s.resolveRunning(r.Context(), &resp, scope)
	writeScopedJSON(w, http.StatusOK, resp, scope, "ready")
}

// resolveCards fills the overview's per-board headline cards by reusing each
// board's Board() aggregator and keeping only the summary fields. Every card
// degrades on its own: a nil data source or a Board() error records the reason
// in the matching *Unavailable field and leaves the card nil, so the home page
// still renders the boards that are up. This mirrors each board handler's own
// 503/502 soft-degrade — the overview just surfaces it inline instead of a
// dead tile.
func (s *Server) resolveCards(ctx context.Context, resp *overviewResponse, scope WorkspaceScope) {
	clusterScope := s.cluster.Cluster
	costClusterScope := s.cost.Cluster
	costNamespaceScope := ""
	rayNamespaceScope := s.ray.Namespace
	if scope.Managed {
		clusterScope = scope.Cluster
		costClusterScope = scope.Cluster
		costNamespaceScope = workloadMetricsNamespace(scope)
		rayNamespaceScope = scope.Namespace
	}
	// Fleet (Cluster Nodes) — Kubernetes-backed.
	if s.nodes.Reader == nil {
		resp.Cards.FleetUnavailable = "portal started without Kubernetes access"
	} else if snap, err := nodes.Board(ctx, s.nodes.Reader, nodes.Options{}); err != nil {
		resp.Cards.FleetUnavailable = err.Error()
	} else {
		card := &fleetCard{
			TotalNodes:     snap.TotalNodes,
			ReadyNodes:     snap.ReadyNodes,
			GPUNodes:       snap.GPUNodes,
			TotalGPUs:      snap.TotalGPUs,
			TotalCPUCores:  snap.TotalCPUCores,
			TotalMemoryGiB: snap.TotalMemoryGiB,
		}
		if len(snap.SKUs) > 0 {
			card.TopSKU = snap.SKUs[0].SKU
		}
		resp.Cards.Fleet = card
	}

	// Health (Cluster Health) — Kusto-backed.
	if s.cluster.Querier == nil {
		resp.Cards.HealthUnavailable = "portal started without a Kusto query command"
	} else if snap, err := cluster.Board(ctx, s.cluster.Querier, cluster.Options{
		Cluster: clusterScope, Namespace: workloadMetricsNamespace(scope),
	}); err != nil {
		resp.Cards.HealthUnavailable = err.Error()
	} else {
		resp.Cards.Health = &healthCard{TotalGPUs: snap.TotalGPUs, ErrorGPUs: snap.ErrorGPUs}
	}

	// Queue (Jobs) — Kubernetes-backed. Sum the per-group counters into one
	// fleet-wide headline (the same rollup the Jobs page renders per row).
	if jobsMode(s.jobs) == JobsScopeDisabled {
		resp.Cards.QueueUnavailable = "computed Jobs board disabled"
	} else if s.jobs.Reader == nil {
		resp.Cards.QueueUnavailable = "portal started without Kubernetes access"
	} else if jobScopes, err := s.resolvedJobScopes(scope); err != nil {
		resp.Cards.QueueUnavailable = err.Error()
	} else if snap, err := jobs.Board(ctx, s.jobs.Reader, jobs.Options{Scopes: jobScopes, PolicyPath: s.jobs.PolicyPath}); err != nil {
		resp.Cards.QueueUnavailable = err.Error()
	} else {
		summary := jobs.Summarize(snap)
		resp.Cards.Queue = &queueCard{
			Pending: summary.Pending, Admitted: summary.Admitted,
			GPUUsed: summary.GPUUsed, GPUHeadroom: summary.GPUHeadroom,
		}
	}

	// Cost — Kusto-backed.
	if s.cost.Querier == nil {
		resp.Cards.CostUnavailable = "portal started without a Kusto query command"
	} else if snap, err := cost.Board(ctx, s.cost.Querier, cost.Options{Cluster: costClusterScope, Namespace: costNamespaceScope}); err != nil {
		resp.Cards.CostUnavailable = err.Error()
	} else {
		resp.Cards.Cost = &costCard{
			Window:        snap.Window,
			TotalGPUHours: snap.TotalGPUHours,
			IdleGPUs:      len(snap.IdleGPUs),
		}
	}

	// Ray — Kubernetes-backed.
	if s.ray.Reader == nil {
		resp.Cards.RayUnavailable = "portal started without Kubernetes access"
	} else if snap, err := ray.Board(ctx, s.ray.Reader, ray.Options{Namespace: rayNamespaceScope}); err != nil {
		resp.Cards.RayUnavailable = err.Error()
	} else {
		resp.Cards.Ray = &rayCard{Clusters: snap.Total}
	}
}

// resolveRunning pre-resolves the overview's "running now" cross-links from the
// Jobs board's Kueue reader: admitted, unfinished Workloads projected to their
// Experiment deep-link. The whole section degrades softly — without a
// Kubernetes reader it sets RunningUnavailable and leaves Running empty; a list
// or parse error is reported the same way rather than failing the overview, so
// the home page (boards + the Kusto-backed sections) still renders.
func (s *Server) resolveRunning(ctx context.Context, resp *overviewResponse, scope WorkspaceScope) {
	if jobsMode(s.jobs) == JobsScopeDisabled {
		resp.RunningUnavailable = "computed Jobs board disabled"
		return
	}
	if s.jobs.Reader == nil {
		resp.RunningUnavailable = "portal started without Kubernetes access"
		return
	}
	jobScopes, err := s.resolvedJobScopes(scope)
	if err != nil {
		resp.RunningUnavailable = err.Error()
		return
	}
	workloads := make([]links.Workload, 0)
	seen := map[string]struct{}{}
	for _, jobScope := range jobScopes {
		scopeWorkloads, listErr := links.ListWorkloads(ctx, s.jobs.Reader, jobScope.Namespace)
		if listErr != nil {
			resp.RunningUnavailable = listErr.Error()
			return
		}
		for _, workload := range scopeWorkloads {
			if workload.Queue != jobScope.Queue {
				continue
			}
			key := workload.Namespace + "\x00" + workload.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			workloads = append(workloads, workload)
		}
	}
	for _, wl := range workloads {
		if !wl.Running() {
			continue
		}
		experimentPath := ""
		experimentTracking := string(s.experimentSurface(scope))
		if experimentTracking == "" {
			experimentTracking = "untracked"
		}
		if !scope.Managed {
			experimentPath = links.ExperimentPath(wl.RunID)
			if experimentPath != "" {
				experimentTracking = "legacy"
			}
		}
		resp.Running = append(resp.Running, runningItem{
			Job:                wl.Job,
			Name:               wl.Name,
			Namespace:          wl.Namespace,
			RunID:              wl.RunID,
			Queue:              wl.Queue,
			ClusterQueue:       wl.ClusterQueue,
			Project:            wl.Project,
			Experiment:         wl.Experiment,
			Group:              wl.Group,
			ExperimentPath:     experimentPath,
			ExperimentTracking: experimentTracking,
		})
	}
}

func (s *Server) resolvedJobScopes(scope WorkspaceScope) ([]jobs.Scope, error) {
	switch jobsMode(s.jobs) {
	case JobsScopeWorkspace:
		if !scope.Managed {
			return nil, fmt.Errorf("workspace-scoped Jobs board requires an authenticated workspace")
		}
		jobScopes := []jobs.Scope{{Team: scope.Team, Namespace: scope.Namespace, Queue: scope.LocalQueue}}
		if err := jobs.ValidateScopes(jobScopes); err != nil {
			return nil, fmt.Errorf("workspace %q has an invalid Jobs scope: %w", scope.WorkspaceID, err)
		}
		return jobScopes, nil
	case JobsScopeOperator:
		return s.jobs.OperatorScopes, nil
	default:
		return nil, fmt.Errorf("computed Jobs board disabled")
	}
}

// handleJobs serves the Jobs/Queue board: the same queue.Snapshot schema as
// `tau queue status --output json`, fetched via client-go and aggregated by
// queue.BuildSnapshot(). Optional ?team=&lane=&gpu-class= filters mirror the
// CLI. Two gates answer 503 before any cluster read so the frontend can render
// a disabled state: an unset scope mode, and a missing Kubernetes reader.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	// Each gate names only its own remedy: one shared message would send an
	// operator whose scope mode is already correct off to re-check it.
	if jobsMode(s.jobs) == JobsScopeDisabled {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "jobs board unavailable: no Jobs scope mode is configured; set --jobs-scope-mode (Helm: portal.jobs.scopeMode) to workspace or operator")
		return
	}
	if s.jobs.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "jobs board unavailable: portal started without Kubernetes access")
		return
	}
	q := r.URL.Query()
	if jobsMode(s.jobs) == JobsScopeOperator {
		for _, key := range []string{"namespace", "queue"} {
			if q.Has(key) {
				writeScopedError(w, http.StatusBadRequest, scope, key+" query is not allowed in operator Jobs mode")
				return
			}
		}
	}
	jobScopes, err := s.resolvedJobScopes(scope)
	if err != nil {
		writeScopedError(w, http.StatusBadRequest, scope, err.Error())
		return
	}
	snapshot, err := jobs.Board(r.Context(), s.jobs.Reader, jobs.Options{
		Scopes:     jobScopes,
		PolicyPath: s.jobs.PolicyPath,
		Team:       q.Get("team"),
		Lane:       q.Get("lane"),
		GPUClass:   q.Get("gpu-class"),
	})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(len(snapshot.Groups) == 0))
}

// handleCluster serves the Cluster Health board: latest-per-GPU health samples
// (utilization/temp/power/memory + remapped-row error flags) from the Metrics
// ADX database via GpuHealth(). Optional ?window=&cluster=&instance=&model=
// scope the query. When the board has no Kusto querier (portal started without
// --kusto-query-command) it returns 503 so the frontend can render a disabled
// state; a query failure returns 502.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.cluster.Querier == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "cluster board unavailable: portal started without a Kusto query command")
		return
	}
	q := r.URL.Query()
	clusterScope := legacyQueryScope(r, "cluster", s.cluster.Cluster)
	if scope.Managed {
		clusterScope = scope.Cluster
	}
	opts := cluster.Options{
		Cluster:  clusterScope,
		Instance: q.Get("instance"),
		Model:    q.Get("model"),
	}
	opts.Namespace = workloadMetricsNamespace(scope)
	if window := q.Get("window"); window != "" {
		if d, err := time.ParseDuration(window); err == nil {
			opts.Window = d
		}
	}
	snapshot, err := cluster.Board(r.Context(), s.cluster.Querier, opts)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(snapshot.TotalGPUs == 0))
}

// handleCost serves the Cost board: GPU-hours by namespace plus the
// idle/underutilized GPU list, derived from GpuHealth() gpu_utilization samples
// in the Metrics ADX database. Optional ?window=&namespace=&idle-threshold=
// scope the query. When the board has no Kusto querier it returns 503; a query
// failure returns 502.
func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.cost.Querier == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "cost board unavailable: portal started without a Kusto query command")
		return
	}
	q := r.URL.Query()
	namespaceScope := q.Get("namespace")
	clusterScope := legacyQueryScope(r, "cluster", s.cost.Cluster)
	if scope.Managed {
		namespaceScope = workloadMetricsNamespace(scope)
		clusterScope = scope.Cluster
	}
	opts := cost.Options{Namespace: namespaceScope, Cluster: clusterScope}
	if window := q.Get("window"); window != "" {
		if d, err := time.ParseDuration(window); err == nil {
			opts.Window = d
		}
	}
	if threshold := q.Get("idle-threshold"); threshold != "" {
		if v, err := strconv.ParseFloat(threshold, 64); err == nil {
			opts.IdleThresholdPct = v
		}
	}
	snapshot, err := cost.Board(r.Context(), s.cost.Querier, opts)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(len(snapshot.Namespaces) == 0 && len(snapshot.IdleGPUs) == 0))
}

// workloadMetricsNamespace preserves namespace isolation for workspaces that
// have real workspace RBAC. Cluster-wide TauWorkspaces have no such boundary,
// so their infrastructure boards intentionally use the whole resolved cluster
// and expose that authorization mode in the response scope.
func workloadMetricsNamespace(scope WorkspaceScope) string {
	if !scope.Managed || scope.AuthorizationMode == workspaceAuthorizationClusterWide {
		return ""
	}
	return scope.Namespace
}

// handleRay serves the Ray board: the list of <cluster>-head-svc head Services
// KubeRay auto-creates for every RayCluster, plus each cluster's portal proxy
// path. Optional ?namespace= scopes discovery; empty lists cluster-wide. When
// the board has no Kubernetes reader (portal started without cluster access) it
// returns 503; a list failure returns 502. An empty result is a normal 200 (no
// RayClusters running), so the frontend renders an empty board, not an error.
func (s *Server) handleRay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.ray.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "ray board unavailable: portal started without Kubernetes access")
		return
	}
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = s.ray.Namespace
	}
	if scope.Managed {
		namespace = scope.Namespace
	}
	historyWorkspaceID := ""
	historyCluster := scope.Cluster
	historyNamespace := namespace
	if scope.Managed {
		historyWorkspaceID = scope.WorkspaceID
	} else if s.runs.History != nil {
		// Durable history is always bound to the cluster validated at startup,
		// never to request-level overrides. Legacy live boards keep accepting a
		// namespace filter, but HistoryScope must remain server-resolved.
		historyCluster = s.legacyScope.Cluster
		historyNamespace = s.legacyScope.Namespace
	}
	snapshot, err := ray.Board(r.Context(), s.ray.Reader, ray.Options{
		Namespace: namespace,
		History:   s.runs.History,
		HistoryScope: runs.HistoryScope{
			Table:       s.runs.HistoryTable,
			Cluster:     historyCluster,
			Namespace:   historyNamespace,
			WorkspaceID: historyWorkspaceID,
			Limit:       s.runs.HistoryLimit,
		},
	})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(snapshot.Total == 0 && len(snapshot.History) == 0))
}

// handleRayHistory serves an ADX-only RayJob detail page. Unlike
// /api/portal/runs/{namespace}/{name}, it never reads a Kubernetes object and
// remains useful after KubeRay has garbage-collected the RayCluster and Pods.
// GET /api/portal/ray/history/{resourceUID}
func (s *Server) handleRayHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	resourceUID := strings.TrimPrefix(r.URL.Path, "/api/portal/ray/history/")
	if resourceUID == "" || strings.Contains(resourceUID, "/") {
		writeScopedError(w, http.StatusNotFound, scope, "not found: expected /api/portal/ray/history/{resourceUID}")
		return
	}
	reader, ok := s.runs.History.(runs.HistoryDetailReader)
	if !ok || reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "durable RayJob history is not configured")
		return
	}
	historyScope := runs.HistoryScope{
		Table: s.runs.HistoryTable, Cluster: scope.Cluster, Namespace: scope.Namespace,
		LocalQueue: scope.LocalQueue, Kind: "RayJob", Limit: s.runs.HistoryLimit,
	}
	if scope.Managed {
		historyScope.WorkspaceID = scope.WorkspaceID
	} else {
		historyScope.Cluster = s.legacyScope.Cluster
		historyScope.Namespace = s.legacyScope.Namespace
	}
	events, err := reader.GetHistoryTimeline(r.Context(), historyScope, resourceUID)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "durable RayJob history query failed")
		return
	}
	if len(events) == 0 {
		writeScopedError(w, http.StatusNotFound, scope, "durable RayJob history not found")
		return
	}
	if !strings.EqualFold(events[0].Kind, "RayJob") ||
		(historyScope.Namespace != "" && !strings.EqualFold(events[0].Namespace, historyScope.Namespace)) ||
		(historyScope.Cluster != "" && !strings.EqualFold(events[0].Cluster, historyScope.Cluster)) {
		writeScopedError(w, http.StatusNotFound, scope, "durable RayJob history not found")
		return
	}
	writeScopedJSON(w, http.StatusOK, map[string]any{"events": events}, scope, "ready")
}

// handleNodes serves the Cluster Nodes board: the fleet's static hardware
// inventory — per-node SKU, agentpool, region/zone, and CPU/memory/GPU capacity,
// plus fleet totals and a per-SKU rollup. It reads core Node objects via the
// same Kubernetes reader as Jobs and Ray. When the board has no reader (portal
// started without cluster access) it returns 503; a list failure returns 502.
// An empty cluster is a normal 200, so the frontend renders an empty board.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.nodes.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "nodes board unavailable: portal started without Kubernetes access")
		return
	}
	includeDaemonSets := !scope.Managed || scope.AuthorizationMode == workspaceAuthorizationClusterWide
	snapshot, err := nodes.Board(r.Context(), s.nodes.Reader, nodes.Options{IncludeDaemonSets: includeDaemonSets})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(snapshot.TotalNodes == 0))
}

// handleNodeUtil serves the node resource-utilization board: per-node CPU
// utilization (from the node_cpu_idle_seconds counter delta) and memory-used
// percentage (from node_memory_total/available), from the Metrics ADX database
// via NodeHealth(). It is the CPU/memory sibling of the Cluster Health board,
// rendered beneath the per-GPU table on the Utilization page. Optional
// ?window=&cluster=&instance= scope the query. When the board has no Kusto
// querier (portal started without --kusto-query-command) it returns 503; a
// query failure returns 502.
func (s *Server) handleNodeUtil(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.nodeUtil.Querier == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "node utilization board unavailable: portal started without a Kusto query command")
		return
	}
	q := r.URL.Query()
	clusterScope := legacyQueryScope(r, "cluster", s.nodeUtil.Cluster)
	if scope.Managed {
		clusterScope = scope.Cluster
	}
	opts := nodeutil.Options{
		Cluster:  clusterScope,
		Instance: q.Get("instance"),
	}
	if window := q.Get("window"); window != "" {
		if d, err := time.ParseDuration(window); err == nil {
			opts.Window = d
		}
	}
	snapshot, err := nodeutil.Board(r.Context(), s.nodeUtil.Querier, opts)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(len(snapshot.Nodes) == 0))
}

// batch/v1 Jobs and ray.io RayJobs — with name, kind, status, and age. It reads
// via the same Kubernetes reader as Jobs/Ray/Nodes. When the board has no reader
// (portal started without cluster access) it returns 503; only when *both*
// listings fail does runs.Board return an error, surfaced as 502. An empty
// result (or a missing RayJob CRD dropping just those rows) is a normal 200, so
// the frontend renders an empty board, not an error.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if s.runs.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "runs board unavailable: portal started without Kubernetes access")
		return
	}
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = s.runs.Namespace
	}
	if scope.Managed {
		namespace = scope.Namespace
	}
	historyWorkspaceID := ""
	historyCluster := scope.Cluster
	historyNamespace := namespace
	if scope.Managed {
		historyWorkspaceID = scope.WorkspaceID
	} else if s.runs.History != nil {
		// Legacy live boards retain their request-level cluster override for
		// compatibility, but durable history is always bound to the explicit
		// cluster and namespace validated at startup.
		historyCluster = s.legacyScope.Cluster
		historyNamespace = s.legacyScope.Namespace
	}
	snapshot, err := runs.Board(r.Context(), s.runs.Reader, runs.Options{
		Namespace:         namespace,
		Queue:             scope.LocalQueue,
		ExperimentSurface: s.experimentSurface(scope),
		History:           s.runs.History,
		HistoryScope: runs.HistoryScope{
			Table:       s.runs.HistoryTable,
			Cluster:     historyCluster,
			Namespace:   historyNamespace,
			LocalQueue:  scope.LocalQueue,
			WorkspaceID: historyWorkspaceID,
			Limit:       s.runs.HistoryLimit,
		},
	})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(snapshot.Total == 0))
}

// handleJobDetail serves the per-job detail page at
// GET /api/portal/runs/{ns}/{name}. It reuses the Runs board's Kubernetes
// reader (which the concrete *kubeclient.Client widens to jobdetail.Reader) and
// the Cluster board's Kusto querier for the durable lifecycle tier. Missing
// job → 404; reader unavailable → 503; aggregation failure → 502.
func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/portal/runs/")
	ns, name, ok := strings.Cut(tail, "/")
	// Kubernetes object names cannot contain "/", so any extra segment means a
	// malformed URL — reject it rather than folding it into name.
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		writeScopedError(w, http.StatusNotFound, scope, "not found: expected /api/portal/runs/{namespace}/{name}")
		return
	}
	if scope.Managed {
		ns = scope.Namespace
	}
	if s.runs.Reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "runs board unavailable: portal started without Kubernetes access")
		return
	}
	reader, ok := s.runs.Reader.(jobdetail.Reader)
	if !ok {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "job detail unavailable: reader does not support single-object reads")
		return
	}
	snapshot, err := jobdetail.Detail(r.Context(), reader, s.cluster.Querier, jobdetail.Options{Namespace: ns, Name: name})
	if err != nil {
		if errors.Is(err, jobdetail.ErrNotFound) {
			writeScopedError(w, http.StatusNotFound, scope, err.Error())
			return
		}
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, "ready")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func legacyQueryScope(r *http.Request, key, fallback string) string {
	if values, ok := r.URL.Query()[key]; ok && len(values) > 0 {
		return values[0]
	}
	return fallback
}

// setSecurityHeaders applies the portal's baseline headers. Unlike Stellar
// (which sends X-Frame-Options: DENY), the portal uses SAMEORIGIN so a future
// Experiments tab can embed the same-origin Stellar SPA in an iframe.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

// framedSameOrigin wraps the mounted Stellar handler so its hard-coded
// X-Frame-Options: DENY is relaxed to SAMEORIGIN, letting /portal/experiments
// embed Stellar in a same-origin iframe. Stellar is same-origin (served under
// the portal's own listener), so SAMEORIGIN keeps the clickjacking protection
// while permitting the embed. A stricter value Stellar sets on a specific route
// (e.g. the report-artifact CSP sandbox) is left untouched; only the blanket
// DENY is rewritten. The rewrite happens just before headers are committed, so
// it wins over whatever Stellar wrote into the header map.
func framedSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&sameOriginFrameWriter{ResponseWriter: w}, r)
	})
}

// sameOriginFrameWriter rewrites a DENY X-Frame-Options to SAMEORIGIN at the
// moment the status is committed (WriteHeader), covering the common path where a
// handler writes the body without an explicit WriteHeader call.
type sameOriginFrameWriter struct {
	http.ResponseWriter
	rewritten bool
}

func (w *sameOriginFrameWriter) relaxFrameOptions() {
	if w.rewritten {
		return
	}
	w.rewritten = true
	if strings.EqualFold(w.Header().Get("X-Frame-Options"), "DENY") {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	}
}

func (w *sameOriginFrameWriter) WriteHeader(status int) {
	w.relaxFrameOptions()
	w.ResponseWriter.WriteHeader(status)
}

func (w *sameOriginFrameWriter) Write(b []byte) (int, error) {
	w.relaxFrameOptions()
	return w.ResponseWriter.Write(b)
}

// Hijack passes through to the underlying ResponseWriter so reverse-proxied
// WebSocket upgrades (e.g. the KueueViz /ws/ endpoints) can take over the
// connection. httputil.ReverseProxy requires the writer to implement
// http.Hijacker for protocol switching.
func (w *sameOriginFrameWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
}
