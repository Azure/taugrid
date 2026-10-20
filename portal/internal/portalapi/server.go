// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/queue"
	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/portal/internal/expapi"
	"github.com/Azure/taugrid/portal/internal/portal/cluster"
	"github.com/Azure/taugrid/portal/internal/portal/cost"
	"github.com/Azure/taugrid/portal/internal/portal/gpuinventory"
	"github.com/Azure/taugrid/portal/internal/portal/jobdetail"
	"github.com/Azure/taugrid/portal/internal/portal/jobs"
	"github.com/Azure/taugrid/portal/internal/portal/links"
	"github.com/Azure/taugrid/portal/internal/portal/nodes"
	"github.com/Azure/taugrid/portal/internal/portal/nodeutil"
	portalquota "github.com/Azure/taugrid/portal/internal/portal/quota"
	"github.com/Azure/taugrid/portal/internal/portal/ray"
	"github.com/Azure/taugrid/portal/internal/portal/workloadlogs"
)

const DefaultAddr = "127.0.0.1:8080"

type JobsScopeMode string

const (
	JobsScopeDisabled  JobsScopeMode = "disabled"
	JobsScopeWorkspace JobsScopeMode = "workspace"
	JobsScopeOperator  JobsScopeMode = "operator"
)

type Options struct {
	Stellar            expapi.Options
	Jobs               JobsOptions
	Cluster            ClusterOptions
	Cost               CostOptions
	Ray                RayOptions
	Nodes              NodesOptions
	Runs               RunsOptions
	NodeUtil           NodeUtilOptions
	Quota              QuotaOptions
	GPU                GPUOptions
	WorkspaceDirectory WorkspaceDirectory
	Identity           IdentityOptions
	KueueViz           KueueVizOptions
}

type JobsOptions struct {
	Reader         jobs.Reader
	Profiles       jobs.ProfileReader
	ScopeMode      JobsScopeMode
	OperatorScopes []jobs.Scope
}

type ClusterOptions struct {
	Querier kustoquery.Querier
	Cluster string
}

type CostOptions struct {
	Querier      kustoquery.Querier
	Cluster      string
	CostDatabase string
}

type RayOptions struct {
	Reader    ray.Reader
	Namespace string
	Transport http.RoundTripper
}

type NodesOptions struct {
	Reader nodes.Reader
}

type RunsOptions struct {
	Reader       runs.Reader
	Namespace    string
	LocalQueue   string
	History      runs.HistoryReader
	HistoryTable string
	HistoryLimit int
}

type NodeUtilOptions struct {
	Querier kustoquery.Querier
	Cluster string
}

type QuotaOptions struct {
	Reader portalquota.Reader
}

type GPUOptions struct {
	Reader         gpuinventory.Reader
	FleetEnabled   bool
	RequestTimeout time.Duration
	MaxWorkloads   int
}

type Server struct {
	mux                   *http.ServeMux
	stellar               *expapi.Server
	stellarKustoAvailable bool
	stellarBackendTimeout time.Duration
	jobs                  JobsOptions
	cluster               ClusterOptions
	cost                  CostOptions
	ray                   RayOptions
	rayCache              rayTargetCache
	nodes                 NodesOptions
	runs                  RunsOptions
	nodeUtil              NodeUtilOptions
	quota                 QuotaOptions
	gpu                   GPUOptions
	workspaceDirectory    WorkspaceDirectory
	identity              IdentityOptions
	singleWorkspaceScope  WorkspaceScope
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

func NewServer(opts Options) (*Server, error) {
	if err := validateJobsOptions(opts.Jobs, opts.WorkspaceDirectory); err != nil {
		return nil, err
	}
	stellar, err := expapi.NewServer(opts.Stellar)
	if err != nil {
		return nil, err
	}
	singleWorkspace := stellar.Workspace()
	singleWorkspaceCluster := firstNonEmpty(opts.Cluster.Cluster, opts.Cost.Cluster, opts.NodeUtil.Cluster)
	if opts.Runs.History != nil && opts.WorkspaceDirectory == nil && singleWorkspaceCluster == "" {
		return nil, fmt.Errorf("durable run history requires an explicit cluster scope when no workspace directory is configured")
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
		quota:                 opts.Quota,
		gpu:                   opts.GPU,
		workspaceDirectory:    opts.WorkspaceDirectory,
		identity:              normalizeIdentityOptions(opts.Identity),
		kueueViz:              opts.KueueViz,
		singleWorkspaceScope: WorkspaceScope{
			WorkspaceID:       singleWorkspace,
			Name:              singleWorkspace,
			Cluster:           singleWorkspaceCluster,
			Namespace:         firstNonEmpty(opts.Runs.Namespace, opts.Ray.Namespace),
			LocalQueue:        strings.TrimSpace(opts.Runs.LocalQueue),
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

func canonicalStellarSource(source string, kustoAvailable bool) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source != "" && source != "auto" {
		return source
	}
	if source == "auto" && kustoAvailable {
		return "kusto"
	}
	return "local"
}

func (s *Server) experimentSurface(scope WorkspaceScope) runs.ExperimentSurfaceState {
	if scope.experimentsBackend != nil {
		return runs.ExperimentSurfaceAvailable
	}
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

func (s *Server) Handler() http.Handler {
	return compressJSONHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		s.mux.ServeHTTP(w, r)
	}))
}

const minCompressedJSONBytes = 1024

type compressJSONResponseWriter struct {
	http.ResponseWriter
	header      http.Header
	body        bytes.Buffer
	status      int
	acceptsGzip bool
	committed   bool
	writer      io.Writer
	compressed  *gzip.Writer
}

func newCompressJSONResponseWriter(w http.ResponseWriter, acceptsGzip bool) *compressJSONResponseWriter {
	return &compressJSONResponseWriter{
		ResponseWriter: w,
		header:         make(http.Header),
		acceptsGzip:    acceptsGzip,
	}
}

func (w *compressJSONResponseWriter) Header() http.Header {
	return w.header
}

func (w *compressJSONResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *compressJSONResponseWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.committed {
		return w.writer.Write(payload)
	}
	contentType := strings.ToLower(w.header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "application/json") ||
		w.header.Get("Content-Encoding") != "" ||
		w.status == http.StatusNoContent ||
		w.status == http.StatusNotModified {
		if err := w.commit(false); err != nil {
			return 0, err
		}
		return w.writer.Write(payload)
	}
	if _, err := w.body.Write(payload); err != nil {
		return 0, err
	}
	if w.body.Len() < minCompressedJSONBytes {
		return len(payload), nil
	}
	if err := w.commit(w.acceptsGzip); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (w *compressJSONResponseWriter) finish() {
	if !w.committed {
		_ = w.commit(false)
	}
	if w.compressed != nil {
		_ = w.compressed.Close()
	}
}

func (w *compressJSONResponseWriter) Flush() {
	if !w.committed {
		_ = w.commit(false)
	}
	if w.compressed != nil {
		_ = w.compressed.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *compressJSONResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *compressJSONResponseWriter) commit(compress bool) error {
	if w.committed {
		return nil
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	headers := w.ResponseWriter.Header()
	for name, values := range w.header {
		headers[name] = append([]string(nil), values...)
	}
	if w.body.Len() >= minCompressedJSONBytes {
		addVary(headers, "Accept-Encoding")
	}
	if compress {
		headers.Set("Content-Encoding", "gzip")
		headers.Del("Content-Length")
		w.compressed = gzip.NewWriter(w.ResponseWriter)
		w.writer = w.compressed
	} else {
		w.writer = w.ResponseWriter
	}
	w.ResponseWriter.WriteHeader(status)
	w.committed = true
	if w.body.Len() == 0 {
		return nil
	}
	_, err := w.writer.Write(w.body.Bytes())
	w.body.Reset()
	return err
}

func compressJSONHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localJSONAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		compressed := newCompressJSONResponseWriter(w, acceptsGzip(r.Header.Get("Accept-Encoding")))
		next.ServeHTTP(compressed, r)
		compressed.finish()
	})
}

func localJSONAPIPath(path string) bool {
	if strings.HasPrefix(path, "/api/portal/") {
		return !strings.HasPrefix(path, rayProxyPrefix) &&
			!strings.HasPrefix(path, rayWorkspaceProxyPrefix) &&
			!strings.HasPrefix(path, kueueVizProxyPrefix)
	}
	for _, base := range []string{"/api/stellar", "/api/v1/stellar", "/api/v2/stellar"} {
		if path == base+"/artifact" || strings.HasPrefix(path, base+"/artifact/") {
			return false
		}
		if strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(value), ";")
		if !strings.EqualFold(parts[0], "gzip") {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(name, "q") {
				if parsed, err := strconv.ParseFloat(raw, 64); err == nil {
					quality = parsed
				}
			}
		}
		return quality > 0
	}
	return false
}

func addVary(header http.Header, value string) {
	for _, current := range header.Values("Vary") {
		for _, item := range strings.Split(current, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{Handler: s.Handler()}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := httpServer.Shutdown(shutdownCtx)
		if err != nil {
			err = fmt.Errorf("shut down portal HTTP server: %w", errors.Join(err, httpServer.Close()))
		}
		if serveErr := <-serveErr; !errors.Is(serveErr, http.ErrServerClosed) {
			err = errors.Join(err, serveErr)
		}
		return err
	}
}

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
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/portal", s.handlePortalShell)
	s.mux.HandleFunc("/portal/", s.handlePortalPath)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/portal/workspaces", s.handleWorkspaces)
	s.mux.HandleFunc("/api/portal/overview", s.handleOverview)
	s.mux.HandleFunc("/api/portal/quota", s.handleQuota)
	s.mux.HandleFunc("/api/portal/workspace-gpu-workloads", s.handleWorkspaceGPUWorkloads)
	s.mux.HandleFunc("/api/portal/gpu-consumers", s.handleGPUConsumers)
	s.mux.HandleFunc("/api/portal/jobs", s.handleJobs)
	s.mux.HandleFunc("/api/portal/cluster", s.handleCluster)
	s.mux.HandleFunc("/api/portal/cost", s.handleCost)
	s.mux.HandleFunc("/api/portal/ray", s.handleRay)
	s.mux.HandleFunc("/api/portal/ray/history/", s.handleRayHistory)
	s.mux.HandleFunc("/api/portal/ray/proxy/", s.handleRayProxy)
	s.mux.HandleFunc(rayWorkspaceProxyPrefix, s.handleRayProxy)
	s.mux.HandleFunc("/api/profiling_enabled", handleRayProfilingDisabled)
	for _, p := range rayUnscopedPrefixes {
		s.mux.HandleFunc(p, handleRayUnscoped)
	}
	s.mux.HandleFunc("/api/portal/nodes", s.handleNodes)
	s.mux.HandleFunc("/api/portal/nodeutil", s.handleNodeUtil)
	s.mux.HandleFunc("/api/portal/runs", s.handleRuns)
	s.mux.HandleFunc("/api/portal/runs/", s.handleJobDetail)
	s.mux.HandleFunc("/api/portal/workloads/", s.handleWorkloadDetail)
	kueueVizHandler := framedSameOrigin(http.HandlerFunc(s.handleKueueVizProxy))
	s.mux.Handle(kueueVizProxyPrefix, kueueVizHandler)
	kueueVizAssetHandler := framedSameOrigin(http.HandlerFunc(s.handleKueueVizAsset))
	for _, p := range kueueVizAssetPrefixes {
		s.mux.Handle(p, kueueVizAssetHandler)
	}

	stellarHandler := framedSameOrigin(s.stellar.Handler())
	workspaceStellarHandler := s.workspaceAwareStellar(stellarHandler)
	s.mux.Handle("/stellar", workspaceStellarHandler)
	s.mux.Handle("/stellar/", workspaceStellarHandler)
	s.mux.Handle("/api/stellar/", workspaceStellarHandler)
	s.mux.Handle("/api/v1/stellar/", workspaceStellarHandler)
	s.mux.Handle("/api/v2/stellar/", workspaceStellarHandler)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		serveIndexHTML(w, http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/portal", http.StatusFound)
}

func (s *Server) handlePortalShell(w http.ResponseWriter, _ *http.Request) {
	serveIndexHTML(w, http.StatusOK)
}

func (s *Server) handlePortalPath(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/portal/")
	if serveAsset(w, name) {
		return
	}
	if strings.Contains(name, ".") {
		http.NotFound(w, r)
		return
	}
	serveIndexHTML(w, http.StatusOK)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

type boardLink struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Path     string `json:"path"`
	External bool   `json:"external,omitempty"`
}

type overviewResponse struct {
	Boards             []boardLink              `json:"boards"`
	Cards              overviewCards            `json:"cards"`
	Pending            []queue.PendingWorkload  `json:"pending"`
	Active             []runs.Run               `json:"active"`
	ActiveUnavailable  string                   `json:"activeUnavailable,omitempty"`
	Running            []runningItem            `json:"running"`
	Waiting            []runningItem            `json:"waiting"`
	RunningUnavailable string                   `json:"runningUnavailable,omitempty"`
	WorkloadProfiles   jobs.ProfileAvailability `json:"workloadProfiles"`
}

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

type fleetCard struct {
	TotalNodes     int     `json:"totalNodes"`
	ReadyNodes     int     `json:"readyNodes"`
	GPUNodes       int     `json:"gpuNodes"`
	TotalGPUs      int64   `json:"totalGPUs"`
	TotalCPUCores  int64   `json:"totalCPUCores"`
	TotalMemoryGiB float64 `json:"totalMemoryGiB"`
	TopSKU         string  `json:"topSKU,omitempty"`
}

type healthCard struct {
	TotalGPUs int `json:"totalGPUs"`
	ErrorGPUs int `json:"errorGPUs"`
}

type queueCard struct {
	Pending     int         `json:"pending"`
	Admitted    int         `json:"admitted"`
	GPUUsed     int64       `json:"gpuUsed"`
	GPUHeadroom int64       `json:"gpuHeadroom"`
	Queues      []queueLane `json:"queues"`
}

type queueLane struct {
	Namespace    string `json:"namespace"`
	Queue        string `json:"queue"`
	ClusterQueue string `json:"clusterQueue,omitempty"`
	Pending      int    `json:"pending"`
	Admitted     int    `json:"admitted"`
}

type costCard struct {
	Window        string  `json:"window"`
	TotalGPUHours float64 `json:"totalGPUHours"`
	IdleGPUs      int     `json:"idleGPUs"`
}

type rayCard struct {
	Clusters int `json:"clusters"`
}

type runningItem struct {
	Job                        string   `json:"job,omitempty"`
	Name                       string   `json:"name"`
	Namespace                  string   `json:"namespace"`
	ResourceUID                string   `json:"resourceUid,omitempty"`
	RunID                      string   `json:"runId,omitempty"`
	Queue                      string   `json:"queue,omitempty"`
	ClusterQueue               string   `json:"clusterQueue,omitempty"`
	Project                    string   `json:"project,omitempty"`
	Experiment                 string   `json:"experiment,omitempty"`
	Group                      string   `json:"group,omitempty"`
	ExperimentPath             string   `json:"experimentPath,omitempty"`
	ExperimentTracking         string   `json:"experimentTracking"`
	ExecutionTarget            string   `json:"executionTarget,omitempty"`
	AdmissionPriorityClass     string   `json:"admissionPriorityClass,omitempty"`
	AdmissionPriorityClassKind string   `json:"admissionPriorityClassKind,omitempty"`
	AdmissionPriority          *int32   `json:"admissionPriority,omitempty"`
	PodPriorityClasses         []string `json:"podPriorityClasses,omitempty"`
	PendingReason              string   `json:"pendingReason,omitempty"`
	PendingMessage             string   `json:"pendingMessage,omitempty"`
}

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
	view := r.URL.Query().Get("view")
	if view != "" && view != "workloads" {
		http.Error(w, "unsupported overview view: expected workloads or no view", http.StatusBadRequest)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	resp := overviewResponse{
		Boards: s.boardsForScope(scope), Pending: []queue.PendingWorkload{},
		Active: []runs.Run{}, Running: []runningItem{}, Waiting: []runningItem{},
	}
	resp.WorkloadProfiles = jobs.ReadProfiles(r.Context(), s.jobs.Profiles, s.profileScopes(scope), "")
	if view == "workloads" {
		s.resolveQueueCard(r.Context(), &resp, scope)
	} else {
		s.resolveCards(r.Context(), &resp, scope)
	}
	s.resolveRunning(r.Context(), &resp, scope)
	s.resolveActiveRuns(r.Context(), &resp, scope)
	writeScopedJSON(w, http.StatusOK, resp, scope, "ready")
}

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

	if s.cluster.Querier == nil {
		resp.Cards.HealthUnavailable = "portal started without a Kusto query command"
	} else if snap, err := cluster.Board(ctx, s.cluster.Querier, cluster.Options{
		Cluster: clusterScope, Namespace: workloadMetricsNamespace(scope),
	}); err != nil {
		resp.Cards.HealthUnavailable = err.Error()
	} else {
		resp.Cards.Health = &healthCard{TotalGPUs: snap.TotalGPUs, ErrorGPUs: snap.ErrorGPUs}
	}

	s.resolveQueueCard(ctx, resp, scope)

	if s.cost.Querier == nil {
		resp.Cards.CostUnavailable = "portal started without a Kusto query command"
	} else if snap, err := cost.Board(ctx, s.cost.Querier, cost.Options{
		Cluster: costClusterScope, Namespace: costNamespaceScope, CostDatabase: s.cost.CostDatabase,
	}); err != nil {
		resp.Cards.CostUnavailable = err.Error()
	} else {
		resp.Cards.Cost = &costCard{
			Window:        snap.Window,
			TotalGPUHours: snap.TotalGPUHours,
			IdleGPUs:      len(snap.IdleGPUs),
		}
	}

	if s.ray.Reader == nil {
		resp.Cards.RayUnavailable = "portal started without Kubernetes access"
	} else if snap, err := ray.Board(ctx, s.ray.Reader, ray.Options{Namespace: rayNamespaceScope}); err != nil {
		resp.Cards.RayUnavailable = err.Error()
	} else {
		resp.Cards.Ray = &rayCard{Clusters: snap.Total}
	}
}

func (s *Server) resolveQueueCard(ctx context.Context, resp *overviewResponse, scope WorkspaceScope) {
	if jobsMode(s.jobs) == JobsScopeDisabled {
		resp.Cards.QueueUnavailable = "computed Jobs board disabled"
	} else if s.jobs.Reader == nil {
		resp.Cards.QueueUnavailable = "portal started without Kubernetes access"
	} else if jobScopes, err := s.resolvedJobScopes(scope); err != nil {
		resp.Cards.QueueUnavailable = err.Error()
	} else if snap, err := jobs.Board(ctx, s.jobs.Reader, jobs.Options{Scopes: jobScopes}); err != nil {
		resp.Cards.QueueUnavailable = err.Error()
	} else {
		summary := jobs.Summarize(snap.Snapshot)
		resp.Cards.Queue = &queueCard{
			Pending: summary.Pending, Admitted: summary.Admitted,
			GPUUsed: summary.GPUUsed, GPUHeadroom: summary.GPUHeadroom,
			Queues: queueLanes(snap.Groups),
		}
		resp.Pending = overviewPendingWorkloads(snap.Groups)
	}
}

func overviewPendingWorkloads(groups []queue.Group) []queue.PendingWorkload {
	seen := map[string]struct{}{}
	out := make([]queue.PendingWorkload, 0)
	for _, group := range groups {
		for _, workload := range group.PendingWorkloads {
			key := workload.Namespace + "\x00" + workload.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, workload)
		}
	}
	queue.SortPendingWorkloads(out)
	return out
}

func queueLanes(groups []queue.Group) []queueLane {
	out := make([]queueLane, 0)
	seen := make(map[string]struct{})
	for _, group := range groups {
		key := group.Namespace + "\x00" + group.Queue
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, queueLane{
			Namespace: group.Namespace, Queue: group.Queue, ClusterQueue: group.ClusterQueue,
			Pending: group.Pending, Admitted: group.Admitted,
		})
	}
	return out
}

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
		if wl.Finished {
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
		resourceUID := ""
		if reader, ok := s.runs.Reader.(jobdetail.Reader); ok && jobdetail.SupportsKind(reader, wl.ResourceKind) {
			resourceUID = wl.ResourceUID
		}
		item := runningItem{
			Job:                        wl.Job,
			Name:                       wl.Name,
			Namespace:                  wl.Namespace,
			ResourceUID:                resourceUID,
			RunID:                      wl.RunID,
			Queue:                      wl.Queue,
			ClusterQueue:               wl.ClusterQueue,
			Project:                    wl.Project,
			Experiment:                 wl.Experiment,
			Group:                      wl.Group,
			ExperimentPath:             experimentPath,
			ExperimentTracking:         experimentTracking,
			ExecutionTarget:            wl.ExecutionTarget,
			AdmissionPriorityClass:     wl.AdmissionPriorityClass,
			AdmissionPriorityClassKind: wl.AdmissionPriorityClassKind,
			AdmissionPriority:          wl.AdmissionPriority,
			PodPriorityClasses:         wl.PodPriorityClasses,
			PendingReason:              wl.PendingReason,
			PendingMessage:             wl.PendingMessage,
		}
		if wl.Running() {
			resp.Running = append(resp.Running, item)
		} else {
			resp.Waiting = append(resp.Waiting, item)
		}
	}
}

func (s *Server) resolveActiveRuns(ctx context.Context, resp *overviewResponse, scope WorkspaceScope) {
	if s.runs.Reader == nil {
		resp.ActiveUnavailable = "portal started without Kubernetes runtime access"
		return
	}
	jobScopes, err := s.resolvedJobScopes(scope)
	if err != nil {
		resp.ActiveUnavailable = err.Error()
		return
	}
	seen := map[string]struct{}{}
	for _, jobScope := range jobScopes {
		snapshot, boardErr := runs.Board(ctx, s.runs.Reader, runs.Options{
			Namespace:         jobScope.Namespace,
			Queue:             jobScope.Queue,
			ExperimentSurface: s.experimentSurface(scope),
			HistoryScope: runs.HistoryScope{
				Cluster: scope.Cluster, Namespace: jobScope.Namespace, LocalQueue: jobScope.Queue,
			},
		})
		if boardErr != nil {
			resp.ActiveUnavailable = boardErr.Error()
			resp.Active = []runs.Run{}
			return
		}
		for _, run := range snapshot.Runs {
			if !strings.EqualFold(run.Status, "running") {
				continue
			}
			key := run.Namespace + "\x00" + run.Kind + "\x00" + run.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if !scope.Managed {
				run.ExperimentPath = links.ExperimentPath(run.RunID)
				if run.ExperimentPath != "" {
					run.ExperimentTracking = "legacy"
				}
			}
			resp.Active = append(resp.Active, run)
		}
	}
	sort.Slice(resp.Active, func(i, j int) bool {
		if !resp.Active[i].Created.Equal(resp.Active[j].Created) {
			return resp.Active[i].Created.After(resp.Active[j].Created)
		}
		if resp.Active[i].Namespace != resp.Active[j].Namespace {
			return resp.Active[i].Namespace < resp.Active[j].Namespace
		}
		if resp.Active[i].Kind != resp.Active[j].Kind {
			return resp.Active[i].Kind < resp.Active[j].Kind
		}
		return resp.Active[i].Name < resp.Active[j].Name
	})
}

func (s *Server) profileScopes(scope WorkspaceScope) []jobs.Scope {
	if scope.Managed && scope.Team != "" && scope.Namespace != "" {
		return []jobs.Scope{{Team: scope.Team, Namespace: scope.Namespace, Queue: scope.LocalQueue}}
	}
	if jobsMode(s.jobs) == JobsScopeOperator {
		return s.jobs.OperatorScopes
	}
	return nil
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

func (s *Server) workloadDetailScopes(scope WorkspaceScope) ([]jobs.Scope, error) {
	if !scope.Managed {
		return s.resolvedJobScopes(scope)
	}
	jobScopes := []jobs.Scope{{Team: scope.Team, Namespace: scope.Namespace, Queue: scope.LocalQueue}}
	if err := jobs.ValidateScopes(jobScopes); err != nil {
		return nil, fmt.Errorf("workspace %q has an invalid workload detail scope: %w", scope.WorkspaceID, err)
	}
	return jobScopes, nil
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	if jobsMode(s.jobs) == JobsScopeDisabled {
		writeScopedJSON(w, http.StatusServiceUnavailable, map[string]string{
			"reason": "jobs board setup is required; set --jobs-scope-mode (Helm: portal.jobs.scopeMode) to workspace or operator",
		}, scope, "setup_required")
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
		Scopes:   jobScopes,
		Profiles: s.jobs.Profiles,
		Team:     q.Get("team"),
		Lane:     q.Get("lane"),
		GPUClass: q.Get("gpu-class"),
	})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(len(snapshot.Groups) == 0))
}

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
	opts := cost.Options{
		Namespace:    namespaceScope,
		Cluster:      clusterScope,
		CostDatabase: s.cost.CostDatabase,
	}
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
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(len(snapshot.Workspaces) == 0 && len(snapshot.IdleGPUs) == 0))
}

func workloadMetricsNamespace(scope WorkspaceScope) string {
	if !scope.Managed || scope.AuthorizationMode == workspaceAuthorizationClusterWide {
		return ""
	}
	return scope.Namespace
}

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
	historyWorkspaceID := scope.WorkspaceID
	historyCluster := scope.Cluster
	historyNamespace := namespace
	if !scope.Managed && s.runs.History != nil {
		historyCluster = s.singleWorkspaceScope.Cluster
		historyNamespace = s.singleWorkspaceScope.Namespace
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
		LocalQueue: scope.LocalQueue, WorkspaceID: scope.WorkspaceID, Kind: "RayJob", Limit: s.runs.HistoryLimit,
	}
	if !scope.Managed {
		historyScope.Cluster = s.singleWorkspaceScope.Cluster
		historyScope.Namespace = s.singleWorkspaceScope.Namespace
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
	includeClusterWide := !scope.Managed || scope.AuthorizationMode == workspaceAuthorizationClusterWide
	snapshot, err := nodes.Board(r.Context(), s.nodes.Reader, nodes.Options{
		IncludeDaemonSets:  includeClusterWide,
		IncludeAllocations: includeClusterWide,
		IncludeMetrics:     includeClusterWide,
	})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, err.Error())
		return
	}
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(snapshot.TotalNodes == 0))
}

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
	historyWorkspaceID := scope.WorkspaceID
	historyCluster := scope.Cluster
	historyNamespace := namespace
	if !scope.Managed && s.runs.History != nil {
		historyCluster = s.singleWorkspaceScope.Cluster
		historyNamespace = s.singleWorkspaceScope.Namespace
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
	s.annotateMultiKueueRuns(r.Context(), &snapshot, namespace)
	writeScopedJSON(w, http.StatusOK, snapshot, scope, dataState(snapshot.Total == 0))
}

func (s *Server) annotateMultiKueueRuns(ctx context.Context, snapshot *runs.Snapshot, namespace string) {
	if s.jobs.Reader == nil {
		return
	}
	workloads, err := links.ListWorkloads(ctx, s.jobs.Reader, namespace)
	if err != nil {
		return
	}
	multiKueueRunIDs := map[string]struct{}{}
	multiKueueNames := map[string]struct{}{}
	for _, workload := range workloads {
		if workload.ExecutionTarget != "multiKueue" {
			continue
		}
		if workload.RunID != "" {
			multiKueueRunIDs[workload.RunID] = struct{}{}
			continue
		}
		if workload.Job != "" {
			multiKueueNames[workload.Job] = struct{}{}
		}
		for _, owner := range workload.Owners {
			multiKueueNames[owner] = struct{}{}
		}
	}
	for i := range snapshot.Runs {
		_, matchedRunID := multiKueueRunIDs[snapshot.Runs[i].RunID]
		_, matchedName := multiKueueNames[snapshot.Runs[i].Name]
		if snapshot.Runs[i].RunID != "" && !matchedRunID || snapshot.Runs[i].RunID == "" && !matchedName {
			continue
		}
		snapshot.Runs[i].ExecutionTarget = "multiKueue"
	}
}

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
	opts := jobdetail.Options{Namespace: ns, Name: name}
	if scope.Managed {
		opts.WorkspaceID = scope.WorkspaceID
		opts.Cluster = scope.Cluster
	}
	snapshot, err := jobdetail.Detail(r.Context(), reader, s.cluster.Querier, opts)
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

func (s *Server) handleWorkloadDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/portal/workloads/")
	parts := strings.Split(tail, "/")
	if len(parts) == 2 && parts[1] == "logs" {
		s.handleWorkloadLogs(w, r, scope, parts[0])
		return
	}
	if len(parts) != 1 || parts[0] == "" {
		writeScopedError(w, http.StatusNotFound, scope, "not found: expected /api/portal/workloads/{resourceUID}")
		return
	}
	resourceUID := parts[0]

	namespace := scope.Namespace
	if !scope.Managed && namespace == "" {
		namespace = s.runs.Namespace
	}
	snapshot, found, liveErr := s.resolveLiveWorkload(r.Context(), scope, resourceUID)
	if found && liveErr == nil {
		writeScopedJSON(w, http.StatusOK, snapshot, scope, "ready")
		return
	}
	if liveErr != nil && s.runs.History == nil {
		writeScopedError(w, http.StatusBadGateway, scope, liveErr.Error())
		return
	}

	reader, ok := s.runs.History.(runs.HistoryDetailReader)
	if !ok || reader == nil {
		if s.runs.Reader == nil {
			writeScopedError(w, http.StatusServiceUnavailable, scope, "workload detail unavailable: Kubernetes and durable history are not configured")
			return
		}
		writeScopedError(w, http.StatusNotFound, scope, "workload not found")
		return
	}
	historyScope := runs.HistoryScope{
		Table: s.runs.HistoryTable, Cluster: scope.Cluster, Namespace: namespace,
		LocalQueue: scope.LocalQueue, WorkspaceID: scope.WorkspaceID, Limit: s.runs.HistoryLimit,
	}
	if !scope.Managed {
		historyScope.Cluster = s.singleWorkspaceScope.Cluster
		historyScope.Namespace = s.singleWorkspaceScope.Namespace
	}
	events, err := reader.GetHistoryTimeline(r.Context(), historyScope, resourceUID)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "durable workload history query failed")
		return
	}
	if len(events) == 0 || !historyEventInScope(events[0], historyScope, resourceUID) {
		writeScopedError(w, http.StatusNotFound, scope, "workload not found")
		return
	}
	writeScopedJSON(w, http.StatusOK, historicalWorkloadSnapshot(events), scope, "ready")
}

func (s *Server) resolveLiveWorkload(ctx context.Context, scope WorkspaceScope, resourceUID string) (jobdetail.Snapshot, bool, error) {
	if s.runs.Reader == nil {
		return jobdetail.Snapshot{}, false, nil
	}
	namespace := scope.Namespace
	if !scope.Managed && namespace == "" {
		namespace = s.runs.Namespace
	}
	live, err := runs.Board(ctx, s.runs.Reader, runs.Options{Namespace: namespace, Queue: scope.LocalQueue})
	if err != nil {
		return jobdetail.Snapshot{}, false, err
	}
	for _, run := range live.Runs {
		if run.ResourceUID != resourceUID {
			continue
		}
		reader, ok := s.runs.Reader.(jobdetail.Reader)
		if !ok {
			return jobdetail.Snapshot{}, false, errors.New("workload detail reader does not support single-object reads")
		}
		snapshot, detailErr := jobdetail.Detail(ctx, reader, s.cluster.Querier, jobdetail.Options{
			Namespace: run.Namespace, Name: run.Name, WorkspaceID: scope.WorkspaceID, Cluster: scope.Cluster,
		})
		if detailErr != nil {
			if errors.Is(detailErr, jobdetail.ErrNotFound) {
				return jobdetail.Snapshot{}, false, nil
			}
			return jobdetail.Snapshot{}, false, detailErr
		}
		if snapshot.ResourceUID != resourceUID {
			return jobdetail.Snapshot{}, false, nil
		}
		return snapshot, true, nil
	}

	reader, ok := s.runs.Reader.(jobdetail.Reader)
	if !ok || s.jobs.Reader == nil {
		return jobdetail.Snapshot{}, false, nil
	}
	jobScopes, err := s.workloadDetailScopes(scope)
	if err != nil {
		return jobdetail.Snapshot{}, false, err
	}
	for _, jobScope := range jobScopes {
		workloads, listErr := links.ListWorkloads(ctx, s.jobs.Reader, jobScope.Namespace)
		if listErr != nil {
			return jobdetail.Snapshot{}, false, listErr
		}
		for _, workload := range workloads {
			if workload.Queue != jobScope.Queue || workload.ResourceUID != resourceUID ||
				!jobdetail.SupportsKind(reader, workload.ResourceKind) {
				continue
			}
			snapshot, detailErr := jobdetail.DetailReference(ctx, reader, s.cluster.Querier, jobdetail.Options{
				Namespace: workload.Namespace, Name: workload.ResourceName,
				WorkspaceID: scope.WorkspaceID, Cluster: scope.Cluster,
			}, jobdetail.WorkloadReference{
				Kind: workload.ResourceKind, Name: workload.ResourceName, UID: workload.ResourceUID,
			})
			if detailErr != nil {
				if errors.Is(detailErr, jobdetail.ErrNotFound) {
					return jobdetail.Snapshot{}, false, nil
				}
				return jobdetail.Snapshot{}, false, detailErr
			}
			return snapshot, true, nil
		}
	}
	return jobdetail.Snapshot{}, false, nil
}

type workloadLogReader interface {
	GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool, tailLines, limitBytes int64) ([]byte, error)
}

func (s *Server) handleWorkloadLogs(w http.ResponseWriter, r *http.Request, scope WorkspaceScope, resourceUID string) {
	if resourceUID == "" || strings.Contains(resourceUID, "/") {
		writeScopedError(w, http.StatusNotFound, scope, "not found: expected /api/portal/workloads/{resourceUID}/logs")
		return
	}
	reader, ok := s.runs.Reader.(workloadLogReader)
	if !ok || reader == nil {
		writeScopedError(w, http.StatusServiceUnavailable, scope, "workload logs unavailable: Kubernetes log access is not configured")
		return
	}
	snapshot, found, err := s.resolveLiveWorkload(r.Context(), scope, resourceUID)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "workload logs unavailable")
		return
	}
	if !found {
		writeScopedError(w, http.StatusNotFound, scope, "live workload not found; logs may have expired")
		return
	}
	podName := strings.TrimSpace(r.URL.Query().Get("pod"))
	containerName := strings.TrimSpace(r.URL.Query().Get("container"))
	previous, err := strconv.ParseBool(firstNonEmpty(r.URL.Query().Get("previous"), "false"))
	if err != nil {
		writeScopedError(w, http.StatusBadRequest, scope, "previous must be true or false")
		return
	}
	tailLines, err := boundedPositiveQuery(r, "tailLines", workloadlogs.DefaultTailLines, workloadlogs.MaxTailLines)
	if err != nil {
		writeScopedError(w, http.StatusBadRequest, scope, err.Error())
		return
	}
	limitBytes, err := boundedPositiveQuery(r, "limitBytes", workloadlogs.DefaultLimitBytes, workloadlogs.MaxLimitBytes)
	if err != nil {
		writeScopedError(w, http.StatusBadRequest, scope, err.Error())
		return
	}
	container, ok := workloadContainer(snapshot.Pods, podName, containerName)
	if !ok {
		writeScopedError(w, http.StatusNotFound, scope, "pod or container does not belong to this workload")
		return
	}
	if previous && !container.PreviousAvailable {
		writeScopedError(w, http.StatusNotFound, scope, "previous container logs are not available")
		return
	}
	data, err := reader.GetPodLogs(r.Context(), snapshot.Namespace, podName, containerName, previous, tailLines, limitBytes+1)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "container logs could not be read")
		return
	}
	content, truncated, redacted := workloadlogs.Sanitize(data, limitBytes)
	w.Header().Set("Cache-Control", "no-store")
	writeScopedJSON(w, http.StatusOK, workloadlogs.Snapshot{
		Pod: podName, Container: containerName, Previous: previous, Content: content,
		TailLines: tailLines, LimitBytes: limitBytes, Truncated: truncated, RedactionApplied: redacted,
	}, scope, "ready")
}

func boundedPositiveQuery(r *http.Request, name string, fallback, maximum int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || value > maximum {
		return 0, fmt.Errorf("%s must be between 1 and %d", name, maximum)
	}
	return value, nil
}

func workloadContainer(pods []jobdetail.PodDetail, podName, containerName string) (jobdetail.ContainerDetail, bool) {
	for _, pod := range pods {
		if pod.Name != podName {
			continue
		}
		for _, container := range pod.Containers {
			if container.Name == containerName {
				return container, true
			}
		}
	}
	return jobdetail.ContainerDetail{}, false
}

func historyEventInScope(event runs.LifecycleEvent, scope runs.HistoryScope, resourceUID string) bool {
	return event.ResourceUID == resourceUID &&
		(scope.Namespace == "" || strings.EqualFold(event.Namespace, scope.Namespace)) &&
		(scope.Cluster == "" || strings.EqualFold(event.Cluster, scope.Cluster))
}

func historicalWorkloadSnapshot(events []runs.LifecycleEvent) jobdetail.Snapshot {
	first := events[0]
	last := events[len(events)-1]
	created := firstNonEmptyTime(first.SubmitTime, first.ObservedAt)
	status := historicalWorkloadStatus(last.State)
	snapshot := jobdetail.Snapshot{
		Namespace: first.Namespace, Name: first.Name, Kind: first.Kind,
		ResourceUID: first.ResourceUID, ObjectState: "deleted", Status: status,
		RunID: first.RunID, History: events,
		Diagnostics: jobdetail.Diagnostics{
			Workloads: jobdetail.SourceDiagnostic{State: "unavailable", Message: "The Kubernetes object was deleted; live Kueue evidence is no longer retained."},
			Pods:      jobdetail.SourceDiagnostic{State: "unavailable", Message: "The Kubernetes object was deleted; pod state and logs are no longer retained."},
			Events:    jobdetail.SourceDiagnostic{State: "unavailable", Message: "The Kubernetes object was deleted; Kubernetes events may have expired."},
			Tracking:  jobdetail.SourceDiagnostic{State: "empty", Message: "No experiment identity is available in durable history."},
			Telemetry: jobdetail.SourceDiagnostic{State: "unavailable", Message: "Historical GPU attribution is unavailable after pod placement data is deleted."},
		},
		Lifecycle: &jobdetail.LifecycleRow{
			State: last.State, EffectiveState: last.State, Reason: last.Reason, Message: last.Message,
			CompletionTime: last.CompletionTime, ArtifactURI: last.ArtifactURI, CheckpointURI: last.CheckpointURI,
		},
	}
	if first.RunID != "" {
		snapshot.Diagnostics.Tracking = jobdetail.SourceDiagnostic{State: "ready"}
	}
	if !created.IsZero() {
		snapshot.Object.Created = &created
		snapshot.Object.Age = runs.FormatAge(time.Now(), created)
	}
	snapshot.Stages = jobdetail.LifecycleStages{
		Object: "deleted", Admission: "unavailable", Scheduling: "unavailable",
		Application: strings.ToLower(status), Tracking: map[bool]string{true: "linked", false: "unlinked"}[first.RunID != ""],
	}
	return snapshot
}

func firstNonEmptyTime(values ...string) time.Time {
	for _, value := range values {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func historicalWorkloadStatus(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "submitted", "queued", "admitted", "pending":
		return "Pending"
	case "running":
		return "Running"
	case "succeeded", "complete", "completed":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "cancelled", "canceled":
		return "Cancelled"
	default:
		return state
	}
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

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

func framedSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&sameOriginFrameWriter{ResponseWriter: w}, r)
	})
}

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

func (w *sameOriginFrameWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
}
