package expapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/core/version"
	"github.com/Azure/taugrid/portal/internal/blobstore"
	"github.com/Azure/taugrid/portal/internal/expcockpit"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

const (
	DefaultAddr            = "127.0.0.1:8080"
	DefaultMaxRuns         = 200
	DefaultMaxMetricRows   = 1000000
	DefaultRequestTimeout  = 30 * time.Second
	defaultSeriesMaxPoints = 8000
	maxSeriesMaxPoints     = 12000
)

const (
	stellarAPIVersion    = "v2"
	stellarAPILegacyBase = "/api/stellar"
	stellarAPIV1Base     = "/api/v1/stellar"
	stellarAPIV2Base     = "/api/v2/stellar"
)

var stellarAPIBasePaths = []string{stellarAPILegacyBase, stellarAPIV1Base, stellarAPIV2Base}
var stellarDeprecatedAPIBasePaths = []string{stellarAPILegacyBase, stellarAPIV1Base}

type Options struct {
	StorePath        string
	DefaultTarget    string
	DefaultMetric    string
	Source           string
	KustoMetricsFile string
	KustoProject     string
	// Workspace is the TauWorkspace this Stellar server serves. Stellar is
	// single-workspace: every read is scoped to it, and an unset workspace no
	// longer means "every workspace" -- it falls back to DefaultWorkspace.
	Workspace string
	// Deprecated: use Workspace. Retained because this used to be the
	// Kusto-only spelling of the same idea.
	KustoWorkspace         string
	KustoAllowedProjects   []string
	KustoFeaturedProjects  []string
	KustoEndpoint          string
	KustoDatabase          string
	KustoIngestion         string
	KustoSince             string
	KustoDiscoverySince    string
	KustoMaxDiscoverySince string
	KustoTargetSince       string
	KustoQueryCommand      string
	// KustoNativeQuery runs generated KQL against ADX through the azure-kusto-go
	// SDK. When set, Stellar reaches Kusto without an external query command.
	KustoNativeQuery  func(ctx context.Context, query string) (string, error)
	KustoQueryArgs    []string
	KustoTargetPoints int
	MaxRuns           int
	MaxMetricRows     int
	RequestTimeout    time.Duration
}

// DefaultWorkspace is the workspace Stellar serves when none is configured.
//
// Stellar is single-workspace for this release, so the server always has a
// workspace rather than treating "unset" as "every workspace". Deployments are
// expected to name theirs: the taugrid-core chart requires stellar.workspace so
// a real install never silently lands here.
const DefaultWorkspace = "default"

// defaultWorkspace resolves the server's workspace, preferring the current
// option over the Kusto-only spelling it replaced.
func defaultWorkspace(opts Options) string {
	if workspace := strings.TrimSpace(opts.Workspace); workspace != "" {
		return workspace
	}
	if workspace := strings.TrimSpace(opts.KustoWorkspace); workspace != "" {
		return workspace
	}
	return DefaultWorkspace
}

// ErrWorkspaceForbidden is returned when a request names a workspace other than
// the one this server serves.
var ErrWorkspaceForbidden = errors.New("this Stellar server serves a single workspace; ?workspace= must match it")

// resolveWorkspace returns the workspace a request is scoped to, which is
// always the workspace this server was started with. It is fail-closed: it
// never returns an empty workspace, because empty used to mean "every
// workspace".
//
// A request may still pass ?workspace=, but only to name the same workspace;
// anything else is refused rather than served. Stellar has no authenticating
// proxy in front of it, so a query parameter is not a credential and honouring
// an arbitrary one would be enforcement-shaped but spoofable. Scope therefore
// comes from server configuration, and multi-workspace Stellar needs an
// authenticated identity first -- see portalapi.WorkspaceDirectory, which the
// Portal already uses.
func (s *Server) resolveWorkspace(r *http.Request) (string, error) {
	workspace := strings.TrimSpace(s.workspace)
	if workspace == "" {
		// Unreachable via NewServer, which always resolves a workspace; kept so a
		// zero-value Server cannot fail open.
		return "", fmt.Errorf("Stellar has no workspace configured")
	}
	if requested := strings.TrimSpace(r.URL.Query().Get("workspace")); requested != "" && requested != workspace {
		return "", fmt.Errorf("%w: server serves %q", ErrWorkspaceForbidden, workspace)
	}
	return workspace, nil
}

type Server struct {
	storeRoot              string
	defaultTarget          string
	defaultMetric          string
	source                 string
	kustoMetricsFile       string
	kustoProject           string
	workspace              string
	kustoWorkspace         string
	kustoAllowedProjects   []string
	kustoFeaturedProjects  []string
	kustoEndpoint          string
	kustoDatabase          string
	kustoIngestion         string
	kustoSince             string
	kustoDiscoverySince    string
	kustoMaxDiscoverySince string
	kustoTargetSince       string
	kustoQueryCommand      string
	kustoQueryArgs         []string
	kustoNativeQuery       func(ctx context.Context, query string) (string, error)
	kustoTargetPoints      int
	maxRuns                int
	maxMetricRows          int
	requestTimeout         time.Duration
	mux                    *http.ServeMux
}

func NewServer(opts Options) (*Server, error) {
	source, err := normalizeStellarSource(opts.Source)
	if err != nil {
		return nil, err
	}
	if source == "" {
		source = "local"
	}
	root := strings.TrimSpace(opts.StorePath)
	if source == "kusto" && root == "" {
		root = expcockpit.KustoStorePathForIngestion(opts.KustoIngestion)
	} else {
		resolved, err := expstore.ResolveRoot(expstore.ResolveOptions{Explicit: opts.StorePath})
		if err != nil {
			return nil, err
		}
		root = resolved
	}
	if opts.MaxRuns < 0 {
		return nil, fmt.Errorf("--max-runs must be non-negative")
	}
	if opts.MaxMetricRows < 0 {
		return nil, fmt.Errorf("--max-metric-rows must be non-negative")
	}
	if opts.MaxRuns == 0 {
		opts.MaxRuns = DefaultMaxRuns
	}
	if opts.MaxMetricRows == 0 {
		opts.MaxMetricRows = DefaultMaxMetricRows
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	s := &Server{
		storeRoot:              root,
		defaultTarget:          strings.TrimSpace(opts.DefaultTarget),
		defaultMetric:          strings.TrimSpace(opts.DefaultMetric),
		source:                 source,
		kustoMetricsFile:       strings.TrimSpace(opts.KustoMetricsFile),
		kustoProject:           strings.TrimSpace(opts.KustoProject),
		workspace:              defaultWorkspace(opts),
		kustoWorkspace:         defaultWorkspace(opts),
		kustoAllowedProjects:   compactStrings(opts.KustoAllowedProjects),
		kustoFeaturedProjects:  compactStrings(opts.KustoFeaturedProjects),
		kustoEndpoint:          strings.TrimSpace(opts.KustoEndpoint),
		kustoDatabase:          strings.TrimSpace(opts.KustoDatabase),
		kustoIngestion:         strings.TrimSpace(opts.KustoIngestion),
		kustoSince:             strings.TrimSpace(opts.KustoSince),
		kustoDiscoverySince:    strings.TrimSpace(opts.KustoDiscoverySince),
		kustoMaxDiscoverySince: strings.TrimSpace(opts.KustoMaxDiscoverySince),
		kustoTargetSince:       strings.TrimSpace(opts.KustoTargetSince),
		kustoQueryCommand:      strings.TrimSpace(opts.KustoQueryCommand),
		kustoQueryArgs:         append([]string(nil), opts.KustoQueryArgs...),
		kustoNativeQuery:       opts.KustoNativeQuery,
		kustoTargetPoints:      opts.KustoTargetPoints,
		maxRuns:                opts.MaxRuns,
		maxMetricRows:          opts.MaxMetricRows,
		requestTimeout:         opts.RequestTimeout,
		mux:                    http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) StoreRoot() string {
	return s.storeRoot
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

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/stellar", s.handleStellarCockpit)
	s.mux.HandleFunc("/stellar/assets/", s.handleStellarAsset)
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.handleStellarAPI("/capabilities", s.handleCapabilities)
	s.handleStellarAPI("/experiments", s.handleExperiments)
	s.handleStellarAPI("/series", s.handleSeries)
	s.handleStellarAPI("/runs", s.handleRunSearch)
	s.handleStellarAPI("/snapshot", s.handleSnapshot)
	s.handleStellarAPI("/status", s.handleStatus)
	s.handleStellarAPI("/artifacts", s.handleArtifacts)
	s.handleStellarAPI("/artifact", s.handleArtifact)
	s.handleStellarAPI("/artifact/", s.handleArtifact)
	s.handleDeprecatedMutableStellarAPI("/labels")
	s.handleDeprecatedMutableStellarAPI("/dashboards")
	s.handleDeprecatedMutableStellarAPI("/workspaces")
}

func (s *Server) handleStellarAPI(route string, handler http.HandlerFunc) {
	for _, base := range stellarAPIBasePaths {
		s.mux.HandleFunc(base+route, handler)
	}
}

func (s *Server) handleDeprecatedMutableStellarAPI(route string) {
	for _, base := range stellarDeprecatedAPIBasePaths {
		s.mux.HandleFunc(base+route, s.handleUnsupportedMutableState)
		s.mux.HandleFunc(base+route+"/", s.handleUnsupportedMutableState)
	}
}

func (s *Server) handleUnsupportedMutableState(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "mutable Stellar UI state is unsupported; Kusto identity and run groups are read-only")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.defaultTarget != "" {
		http.Redirect(w, r, "/stellar?target="+url.QueryEscape(s.defaultTarget), http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":    "tau.exp.stellar",
		"store_path": s.storeRoot,
		"source":     s.source,
		"endpoints": []string{
			"/stellar?target=<experiment|run-group|run>",
			"/api/v2/stellar/capabilities",
			"/api/v2/stellar/snapshot?target=<experiment|run-group|run>",
			"/api/v2/stellar/experiments?q=<query>",
			"/api/v2/stellar/runs?q=<query>",
			"/api/v2/stellar/status?target=<experiment|run-group|run>",
			"/api/v1/stellar/capabilities",
			"/api/v1/stellar/snapshot?target=<experiment|run-group|run>",
			"/api/v1/stellar/experiments?q=<query>",
			"/api/v1/stellar/runs?q=<query>",
			"/api/v1/stellar/status?target=<experiment|run-group|run>",
			"/api/stellar/capabilities",
			"/api/stellar/snapshot?target=<experiment|run-group|run>",
			"/api/stellar/experiments?q=<query>",
			"/api/stellar/runs?q=<query>",
			"/api/stellar/status?target=<experiment|run-group|run>",
		},
	})
}

type capabilitiesResponse struct {
	APIVersion    string                          `json:"api_version"`
	SchemaVersion string                          `json:"schema_version"`
	Service       string                          `json:"service"`
	SourceMode    string                          `json:"source_mode"`
	Server        capabilitiesServer              `json:"server"`
	Paths         capabilitiesPaths               `json:"paths"`
	DataSources   map[string]capabilityDataSource `json:"data_sources"`
	Capabilities  map[string]map[string]any       `json:"capabilities"`
	Degradations  []capabilityDegradation         `json:"degradations,omitempty"`
}

type capabilitiesServer struct {
	TauVersion  string `json:"tau_version"`
	GeneratedAt string `json:"generated_at"`
}

type capabilitiesPaths struct {
	CanonicalBasePath string   `json:"canonical_base_path"`
	LegacyBasePath    string   `json:"legacy_base_path"`
	SupportedBases    []string `json:"supported_bases"`
}

type capabilityDataSource struct {
	Available      bool   `json:"available"`
	StorePath      string `json:"store_path,omitempty"`
	Freshness      string `json:"freshness,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	Database       string `json:"database,omitempty"`
	Ingestion      string `json:"ingestion,omitempty"`
	DiscoverySince string `json:"discovery_since,omitempty"`
	TargetSince    string `json:"target_since,omitempty"`
	QueryCommand   string `json:"query_command,omitempty"`
}

type capabilityDegradation struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.capabilities(capabilitiesDebugEnabled(r)))
}

func capabilitiesDebugEnabled(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("debug"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func (s *Server) capabilities(debug bool) capabilitiesResponse {
	localAvailable := s.source != "kusto"
	kustoAvailable := s.hasKustoSource()
	kustoSeriesDetail := s.hasKustoRemoteQuery()
	dataSources := map[string]capabilityDataSource{
		"local": {
			Available: localAvailable,
			Freshness: "immediate",
		},
		"kusto": {
			Available: kustoAvailable,
			Ingestion: expcockpit.KustoIngestionOrDefault(s.kustoIngestion),
		},
	}
	if debug {
		dataSources["local"] = capabilityDataSource{
			Available: localAvailable,
			StorePath: s.storeRoot,
			Freshness: "immediate",
		}
		dataSources["kusto"] = capabilityDataSource{
			Available:      kustoAvailable,
			Endpoint:       s.kustoEndpoint,
			Database:       s.kustoDatabase,
			Ingestion:      expcockpit.KustoIngestionOrDefault(s.kustoIngestion),
			DiscoverySince: s.kustoDiscoverySince,
			TargetSince:    s.kustoTargetSince,
			QueryCommand:   s.kustoQueryCommand,
		}
	}
	if !localAvailable {
		dataSources["local"] = capabilityDataSource{Available: false}
	}
	degradations := []capabilityDegradation{}
	if s.source == "kusto" && !kustoAvailable {
		degradations = append(degradations, capabilityDegradation{
			Code:   "SOURCE_UNAVAILABLE",
			Detail: "--kusto-metrics-file or --kusto-query-command is required when --source=kusto.",
		})
	}
	if s.source == "kusto" || kustoAvailable {
		if !kustoSeriesDetail {
			degradations = append(degradations, capabilityDegradation{
				Code:   "KUSTO_SERIES_DETAIL_UNAVAILABLE",
				Detail: "Kusto focused-series requery requires a configured query command.",
			})
		}
		degradations = append(degradations, capabilityDegradation{
			Code:   "KUSTO_ARTIFACT_INDEX_UNAVAILABLE",
			Detail: "Kusto-backed Stellar cannot list expstore artifact indexes.",
		})
	}
	return capabilitiesResponse{
		APIVersion:    stellarAPIVersion,
		SchemaVersion: expcockpit.SchemaVersion,
		Service:       "tau.exp.stellar",
		SourceMode:    s.source,
		Server: capabilitiesServer{
			TauVersion:  version.Version,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		},
		Paths: capabilitiesPaths{
			CanonicalBasePath: stellarAPIV2Base,
			LegacyBasePath:    stellarAPILegacyBase,
			SupportedBases:    []string{stellarAPIV2Base, stellarAPIV1Base, stellarAPILegacyBase},
		},
		DataSources: dataSources,
		Capabilities: map[string]map[string]any{
			"snapshot":            {"local": localAvailable, "kusto": kustoAvailable},
			"series_detail":       {"local": localAvailable, "kusto": kustoSeriesDetail},
			"run_search":          {"local": localAvailable, "kusto": kustoAvailable},
			"experiment_search":   {"local": localAvailable, "kusto": kustoAvailable},
			"experiment_mutation": {"local": localAvailable, "kusto": false},
			"artifact_index":      {"local": localAvailable, "kusto": false},
			"artifact_content":    {"local": localAvailable, "durable_ref": true},
			"status":              {"local": localAvailable, "kusto": kustoAvailable},
		},
		Degradations: degradations,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	if s.source == "kusto" {
		if s.kustoMetricsFile == "" && !s.hasKustoRemoteQuery() {
			writeError(w, http.StatusServiceUnavailable, "--kusto-metrics-file, --kusto-endpoint, or --kusto-query-command is required when --source=kusto")
			return
		}
		if s.kustoMetricsFile != "" {
			if _, err := expcockpit.LoadKustoMetricRows(s.kustoMetricsFile); err != nil {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
		}
		if s.kustoQueryCommand != "" {
			if _, err := execLookPath(s.kustoQueryCommand); err != nil {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                      true,
			"store_path":              s.storeRoot,
			"source":                  s.source,
			"kusto_metrics_file":      s.kustoMetricsFile,
			"kusto_query_command":     s.kustoQueryCommand,
			"kusto_allowed_projects":  s.kustoAllowedProjects,
			"kusto_featured_projects": s.kustoFeaturedProjects,
		})
		return
	}
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err := store.Close(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"store_path": s.storeRoot,
		"source":     s.source,
	})
}

func (s *Server) buildSnapshot(ctx context.Context, r *http.Request, target, metric string) (expcockpit.Snapshot, error) {
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		return expcockpit.Snapshot{}, err
	}
	mode, err := expcockpit.ParseSnapshotMode(r.URL.Query().Get("mode"))
	if err != nil {
		return expcockpit.Snapshot{}, err
	}
	if source == "" {
		source = s.source
	}
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		return expcockpit.Snapshot{}, err
	}
	return s.buildSnapshotWithMode(
		ctx,
		source,
		target,
		workspace,
		strings.TrimSpace(r.URL.Query().Get("project")),
		metric,
		mode,
		!includeStaticSnapshotFields(r),
	)
}

func (s *Server) buildSnapshotWithMode(ctx context.Context, source, target, workspace, project, metric string, mode expcockpit.SnapshotMode, omitStatic bool) (expcockpit.Snapshot, error) {
	opts := expcockpit.Options{
		Target:            target,
		Workspace:         workspace,
		Project:           project,
		Metric:            metric,
		MaxRuns:           s.maxRuns,
		MaxMetricRows:     s.maxMetricRows,
		Mode:              mode,
		SkipMetricCatalog: omitStatic && mode == expcockpit.SnapshotModeMetric,
	}
	switch source {
	case "local":
		return s.buildLocalSnapshot(ctx, opts)
	case "kusto":
		return s.buildKustoSnapshot(ctx, opts)
	case "auto":
		snapshot, err := s.buildMergedSnapshot(ctx, opts)
		if err == nil {
			return snapshot, nil
		}
		if !errors.Is(err, expstore.ErrNotFound) || (s.kustoMetricsFile == "" && !s.hasKustoRemoteQuery()) {
			return expcockpit.Snapshot{}, err
		}
		return s.buildKustoSnapshot(ctx, opts)
	default:
		return expcockpit.Snapshot{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func (s *Server) buildSeries(ctx context.Context, r *http.Request, opts expcockpit.SeriesOptions) (expcockpit.SeriesDetail, error) {
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		return expcockpit.SeriesDetail{}, err
	}
	if source == "" {
		source = s.source
	}
	if _, err := s.resolveWorkspace(r); err != nil {
		return expcockpit.SeriesDetail{}, err
	}
	switch source {
	case "local":
		store, err := expstore.Open(ctx, s.storeRoot)
		if err != nil {
			return expcockpit.SeriesDetail{}, err
		}
		defer store.Close()
		return expcockpit.BuildSeries(ctx, store, opts)
	case "kusto":
		return s.buildKustoSeries(ctx, opts)
	case "auto":
		store, err := expstore.Open(ctx, s.storeRoot)
		if err == nil {
			defer store.Close()
			series, localErr := expcockpit.BuildSeries(ctx, store, opts)
			if localErr == nil {
				return series, nil
			}
			err = localErr
		}
		if !errors.Is(err, expstore.ErrNotFound) || !s.hasKustoSource() {
			return expcockpit.SeriesDetail{}, err
		}
		return s.buildKustoSeries(ctx, opts)
	default:
		return expcockpit.SeriesDetail{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func (s *Server) buildKustoSeries(ctx context.Context, opts expcockpit.SeriesOptions) (expcockpit.SeriesDetail, error) {
	if !s.hasKustoSource() {
		return expcockpit.SeriesDetail{}, fmt.Errorf("source=kusto has no metrics file or query command configured")
	}
	return s.baseKustoSource().BuildSeries(ctx, opts)
}

func (s *Server) buildLocalSnapshot(ctx context.Context, opts expcockpit.Options) (expcockpit.Snapshot, error) {
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		return expcockpit.Snapshot{}, err
	}
	defer store.Close()
	return expcockpit.BuildSnapshot(ctx, store, opts)
}

func (s *Server) buildMergedSnapshot(ctx context.Context, opts expcockpit.Options) (expcockpit.Snapshot, error) {
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		return expcockpit.Snapshot{}, err
	}
	defer store.Close()
	return (expcockpit.MergedSource{
		Store: store,
		Kusto: s.baseKustoSource(),
	}).BuildSnapshot(ctx, opts)
}

func (s *Server) buildKustoSnapshot(ctx context.Context, opts expcockpit.Options) (expcockpit.Snapshot, error) {
	if s.kustoMetricsFile == "" && !s.hasKustoRemoteQuery() {
		return expcockpit.Snapshot{}, fmt.Errorf("--kusto-metrics-file, --kusto-endpoint, or --kusto-query-command is required when --source=kusto or Kusto fallback is selected")
	}
	return s.baseKustoSource().BuildSnapshot(ctx, opts)
}

func (s *Server) baseKustoSource() expcockpit.KustoSource {
	return expcockpit.KustoSource{
		MetricsFile:       s.kustoMetricsFile,
		StorePath:         expcockpit.KustoStorePathForIngestion(s.kustoIngestion),
		Project:           s.kustoProject,
		WorkspaceID:       s.kustoWorkspace,
		AllowedProjects:   append([]string(nil), s.kustoAllowedProjects...),
		FeaturedProjects:  append([]string(nil), s.kustoFeaturedProjects...),
		Endpoint:          s.kustoEndpoint,
		Database:          s.kustoDatabase,
		Ingestion:         s.kustoIngestion,
		Since:             s.kustoSince,
		DiscoverySince:    s.kustoDiscoverySince,
		MaxDiscoverySince: s.kustoMaxDiscoverySince,
		TargetSince:       s.kustoTargetSince,
		TargetPoints:      s.kustoTargetPoints,
		QueryCommand:      s.kustoQueryCommand,
		QueryArgs:         s.kustoQueryArgs,
		NativeQuery:       s.kustoNativeQuery,
	}
}

func (s *Server) searchExperiments(ctx context.Context, source string, opts expstore.ExperimentSearchOptions) (expstore.ExperimentSearchResult, error) {
	switch source {
	case "local":
		return s.searchLocalExperiments(ctx, opts)
	case "kusto":
		return s.baseKustoSource().SearchExperiments(ctx, opts)
	case "auto":
		local, err := s.searchLocalExperiments(ctx, opts)
		if err != nil {
			if !s.hasKustoSource() {
				return expstore.ExperimentSearchResult{}, err
			}
			kusto, kustoErr := s.baseKustoSource().SearchExperiments(ctx, opts)
			if kustoErr != nil {
				return expstore.ExperimentSearchResult{}, err
			}
			kusto.Warnings = append(kusto.Warnings, fmt.Sprintf("source=auto fell back to Kusto because local experiment search failed: %v", err))
			return kusto, nil
		}
		if !s.hasKustoSource() {
			return local, nil
		}
		kusto, err := s.baseKustoSource().SearchExperiments(ctx, opts)
		if err != nil {
			local.Warnings = append(local.Warnings, fmt.Sprintf("source=auto skipped Kusto experiment search because it failed: %v", err))
			return local, nil
		}

		return mergeExperimentSearchResults(local, kusto, opts.Limit), nil
	default:
		return expstore.ExperimentSearchResult{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func (s *Server) searchLocalExperiments(ctx context.Context, opts expstore.ExperimentSearchOptions) (expstore.ExperimentSearchResult, error) {
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		return expstore.ExperimentSearchResult{}, err
	}
	defer store.Close()
	return store.SearchExperiments(ctx, opts)
}

func (s *Server) hasKustoSource() bool {
	return strings.TrimSpace(s.kustoMetricsFile) != "" || s.hasKustoRemoteQuery()
}

// hasKustoRemoteQuery reports whether the server can issue live KQL — through
// the external query command or the native azure-kusto-go transport.
func (s *Server) hasKustoRemoteQuery() bool {
	return strings.TrimSpace(s.kustoQueryCommand) != "" || s.kustoNativeQuery != nil
}

func mergeExperimentSearchResults(local, kusto expstore.ExperimentSearchResult, limit int) expstore.ExperimentSearchResult {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	merged := append([]expstore.ExperimentSummary{}, local.Experiments...)
	seen := map[string]bool{}
	for _, experiment := range local.Experiments {
		seen[experiment.ExperimentID] = true
	}
	addedKusto := 0
	for _, experiment := range kusto.Experiments {
		if seen[experiment.ExperimentID] {
			continue
		}
		seen[experiment.ExperimentID] = true
		merged = append(merged, experiment)
		addedKusto++
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].LatestRunAt != merged[j].LatestRunAt {
			return merged[i].LatestRunAt > merged[j].LatestRunAt
		}
		return merged[i].ExperimentID < merged[j].ExperimentID
	})
	total := len(merged)
	truncated := local.Truncated || kusto.Truncated || total > limit
	if total > limit {
		merged = merged[:limit]
	}
	warnings := append([]string{}, local.Warnings...)
	warnings = append(warnings, kusto.Warnings...)
	if addedKusto > 0 {
		warnings = append(warnings, fmt.Sprintf("source=auto merged %d Kusto-backed experiments with local expstore experiments", addedKusto))
	}
	return expstore.ExperimentSearchResult{
		SchemaVersion: expstore.ExperimentSearchSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     local.StorePath,
		Total:         len(merged),
		Truncated:     truncated,
		Experiments:   merged,
		Warnings:      warnings,
	}
}

func (s *Server) handleStellarCockpit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.writeStellarCockpit(w, r)
}

// isIframeEmbed reports whether the request is a same-origin iframe navigation
// (the unified portal's Experiments board loads /stellar in an iframe). Browsers
// set Sec-Fetch-Dest: iframe on such subframe document requests; the portal
// serves X-Frame-Options: SAMEORIGIN to permit the embed. The frontend uses this
// to tighten the landing layout so it fills the narrower frame.
func isIframeEmbed(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Sec-Fetch-Dest"), "iframe")
}

func (s *Server) writeStellarCockpit(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if metric == "" {
		metric = s.defaultMetric
	}
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if source == "" {
		source = s.source
	}
	// This used to force source=kusto whenever a workspace was present, which
	// silently switched local deployments to a source they had not configured.
	// Workspace scoping is implemented for both sources, so the source stays as
	// requested.
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	html, err := expcockpit.RenderFrontendHTML(expcockpit.FrontendOptions{
		Target:       target,
		Workspace:    workspace,
		Project:      strings.TrimSpace(r.URL.Query().Get("project")),
		Metric:       metric,
		AssetBase:    "/stellar/assets",
		SnapshotPath: "/api/stellar/snapshot",
		SeriesPath:   "/api/stellar/series",
		Source:       source,
		Embedded:     isIframeEmbed(r),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(html)
}

func (s *Server) handleStellarAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/stellar/assets/")
	asset, ok, err := expcockpit.ReadFrontendAsset(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", asset.ContentType)
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(asset.Content)
	}
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	target, metric, err := s.targetAndMetric(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	snapshot, err := s.buildSnapshot(ctx, r, target, metric)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiSnapshot(snapshot, includeStaticSnapshotFields(r)))
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	target, metric, err := s.targetAndMetric(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := seriesOptionsFromRequest(r, workspace, target, metric, s.maxRuns, s.maxMetricRows)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	series, err := s.buildSeries(ctx, r, opts)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiSeries(series))
}

func (s *Server) handleRunSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if source == "" {
		source = s.source
	}
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := runSearchOptionsFromRequest(r, workspace)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	if source == "kusto" {
		result, err := s.baseKustoSource().SearchRuns(ctx, opts)
		if err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	store, err := expstore.Open(ctx, s.storeRoot)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	defer store.Close()
	result, err := store.SearchRuns(ctx, opts)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleExperiments(w http.ResponseWriter, r *http.Request) {
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if source == "" {
		source = s.source
	}
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		opts, err := experimentSearchOptionsFromRequest(r, workspace)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.searchExperiments(ctx, source, opts)
		if err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodPost:
		if source == "kusto" {
			writeError(w, http.StatusBadRequest, "Stellar experiment assignment is local-only; use source=local or source=auto")
			return
		}
		store, err := expstore.Open(ctx, s.storeRoot)
		if err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		defer store.Close()
		var req struct {
			RunID        string `json:"run_id"`
			ExperimentID string `json:"experiment_id"`
			Name         string `json:"name"`
			Description  string `json:"description"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid experiment assignment JSON")
			return
		}
		req.RunID = strings.TrimSpace(req.RunID)
		req.ExperimentID = strings.TrimSpace(req.ExperimentID)
		if req.RunID == "" || req.ExperimentID == "" {
			writeError(w, http.StatusBadRequest, "run_id and experiment_id are required")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			req.Name = req.ExperimentID
		}
		if err := store.AssignRunToExperiment(ctx, expstore.ExperimentRecord{
			ExperimentID: req.ExperimentID,
			Name:         req.Name,
			Description:  req.Description,
			Source:       "explicit",
		}, req.RunID); err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"run_id": req.RunID, "experiment_id": req.ExperimentID, "name": req.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		target = s.defaultTarget
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, "target query parameter is required")
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	snapshot, err := s.buildSnapshot(ctx, r, target, "")
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot.Status)
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	runID := strings.TrimSpace(r.URL.Query().Get("run"))
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if source == "" {
		source = s.source
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	var artifacts []expcockpit.ArtifactView
	if runID != "" {
		if source == "kusto" {
			writeError(w, http.StatusBadRequest, "run-scoped artifact listing requires the local expstore index")
			return
		}
		store, err := expstore.Open(ctx, s.storeRoot)
		if err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		defer store.Close()
		records, err := store.ArtifactsForRun(ctx, runID)
		if err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		artifacts = artifactRecordsToViews(records)
		if target == "" {
			target = runID
		}
	} else {
		if target == "" {
			target = s.defaultTarget
		}
		if target == "" {
			writeError(w, http.StatusBadRequest, "target or run query parameter is required")
			return
		}
		snapshot, err := s.buildSnapshot(ctx, r, target, "")
		if err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		artifacts = snapshot.Artifacts
	}
	artifacts = filterArtifacts(artifacts, r.URL.Query())
	writeJSON(w, http.StatusOK, map[string]any{
		"source":    "index",
		"target":    target,
		"run":       runID,
		"count":     len(artifacts),
		"artifacts": apiArtifacts(artifacts, target),
	})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	target, artifactID, assetPath, err := artifactRequestParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if target == "" {
		target = s.defaultTarget
	}
	if target == "" {
		writeError(w, http.StatusBadRequest, "target query parameter is required")
		return
	}
	if artifactID == "" {
		writeError(w, http.StatusBadRequest, "artifact query parameter is required")
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	snapshot, err := s.buildSnapshot(ctx, r, target, "")
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	for _, artifact := range snapshot.Artifacts {
		if artifact.ArtifactID != artifactID {
			continue
		}
		artifactPath, ok, err := s.localArtifactDocumentPath(artifact)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !ok {
			if assetPath != "" {
				writeError(w, http.StatusBadRequest, "artifact bundle assets require a local report artifact")
				return
			}
			durablePath, durableOK, cleanup, err := durableArtifactPath(ctx, artifact)
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			if !durableOK {
				writeError(w, http.StatusNotFound, "artifact does not reference a local or durable file")
				return
			}
			defer cleanup()
			if isReportArtifactDocument(artifact, durablePath) {
				setReportArtifactHeaders(w)
			}
			serveLocalArtifact(w, r, durablePath)
			return
		}
		reportDocument := isReportArtifactDocument(artifact, artifactPath)
		if assetPath != "" {
			if !reportDocument {
				writeError(w, http.StatusBadRequest, "artifact bundle assets are only supported for report artifacts")
				return
			}
			artifactPath, ok, err = localArtifactBundleAssetPath(artifactPath, assetPath)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "artifact bundle asset was not found")
				return
			}
		}
		if assetPath == "" && reportDocument {
			setReportArtifactHeaders(w)
		}
		serveLocalArtifact(w, r, artifactPath)
		return
	}
	writeError(w, http.StatusNotFound, "artifact was not found for target")
}

type apiArtifactRecord struct {
	ArtifactID  string `json:"artifact_id"`
	RunID       string `json:"run_id"`
	Type        string `json:"type"`
	URI         string `json:"uri"`
	Name        string `json:"name"`
	DurableRef  string `json:"durable_ref,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Digest      string `json:"digest,omitempty"`
	SizeBytes   string `json:"size_bytes,omitempty"`
	Step        string `json:"step,omitempty"`
	Tags        string `json:"tags,omitempty"`
	Rank        string `json:"rank,omitempty"`
	CreatedAt   string `json:"created_at"`
	Preview     string `json:"preview,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`
	FetchURL    string `json:"fetch_url"`
}

func apiArtifacts(artifacts []expcockpit.ArtifactView, target string) []apiArtifactRecord {
	out := make([]apiArtifactRecord, 0, len(artifacts))
	for _, artifact := range artifacts {
		fetchTarget := target
		if fetchTarget == "" {
			fetchTarget = artifact.RunID
		}
		out = append(out, apiArtifactRecord{
			ArtifactID:  artifact.ArtifactID,
			RunID:       artifact.RunID,
			Type:        artifact.Type,
			URI:         artifact.URI,
			Name:        artifact.Name,
			DurableRef:  artifact.DurableRef,
			ContentType: artifact.ContentType,
			Digest:      artifact.Digest,
			SizeBytes:   artifact.SizeBytes,
			Step:        artifact.Step,
			Tags:        artifact.Tags,
			Rank:        artifact.Rank,
			CreatedAt:   artifact.CreatedAt,
			Preview:     artifact.Preview,
			ExternalRef: artifact.ExternalRef,
			FetchURL:    "/api/stellar/artifact?target=" + url.QueryEscape(fetchTarget) + "&artifact=" + url.QueryEscape(artifact.ArtifactID),
		})
	}
	return out
}

func artifactRecordsToViews(records []expstore.ArtifactRecord) []expcockpit.ArtifactView {
	out := make([]expcockpit.ArtifactView, 0, len(records))
	for _, record := range records {
		view := expcockpit.ArtifactView{
			ArtifactID:  record.ArtifactID,
			RunID:       record.RunID,
			Type:        record.Type,
			URI:         record.URI,
			Name:        record.Name,
			DurableRef:  record.DurableRef,
			ContentType: record.ContentType,
			Digest:      record.Digest,
			Tags:        record.Tags,
			CreatedAt:   record.CreatedAt,
			Preview:     record.Preview,
			ExternalRef: record.ExternalRef,
		}
		if record.SizeBytes != nil {
			view.SizeBytes = strconv.FormatInt(*record.SizeBytes, 10)
		}
		if record.Step != nil {
			view.Step = strconv.FormatInt(*record.Step, 10)
		}
		if record.Rank != nil {
			view.Rank = strconv.FormatInt(*record.Rank, 10)
		}
		out = append(out, view)
	}
	return out
}

func filterArtifacts(artifacts []expcockpit.ArtifactView, query url.Values) []expcockpit.ArtifactView {
	filterType := strings.TrimSpace(query.Get("type"))
	filterRank := strings.TrimSpace(query.Get("rank"))
	filterTag := query["tag"]
	if filterType == "" && filterRank == "" && len(filterTag) == 0 {
		return artifacts
	}
	out := artifacts[:0]
	for _, artifact := range artifacts {
		if filterType != "" && artifact.Type != filterType {
			continue
		}
		if filterRank != "" && artifact.Rank != filterRank {
			continue
		}
		matchesTags := true
		for _, tag := range filterTag {
			tag = strings.TrimSpace(tag)
			if tag != "" && !strings.Contains(artifact.Tags, tag) {
				matchesTags = false
				break
			}
		}
		if matchesTags {
			out = append(out, artifact)
		}
	}
	return out
}

func artifactRequestParams(r *http.Request) (string, string, string, error) {
	for _, base := range stellarAPIBasePaths {
		scopedPrefix := base + "/artifact/bundle/"
		if !strings.HasPrefix(r.URL.Path, scopedPrefix) {
			continue
		}
		rest := strings.TrimPrefix(r.URL.Path, scopedPrefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", fmt.Errorf("artifact bundle path requires target and artifact")
		}
		target, err := decodeArtifactPathKey(parts[0])
		if err != nil {
			return "", "", "", fmt.Errorf("invalid target path key: %w", err)
		}
		artifactID, err := decodeArtifactPathKey(parts[1])
		if err != nil {
			return "", "", "", fmt.Errorf("invalid artifact path key: %w", err)
		}
		assetPath := ""
		if len(parts) == 3 {
			assetPath = strings.TrimPrefix(parts[2], "/")
		}
		return strings.TrimSpace(target), strings.TrimSpace(artifactID), assetPath, nil
	}
	return strings.TrimSpace(r.URL.Query().Get("target")), strings.TrimSpace(r.URL.Query().Get("artifact")), "", nil
}

func decodeArtifactPathKey(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func (s *Server) localArtifactDocumentPath(artifact expcockpit.ArtifactView) (string, bool, error) {
	if isReportArtifactDocument(artifact, "") {
		for _, ref := range []string{artifact.URI, artifact.Preview} {
			artifactPath, ok, err := s.resolveStoreFile(ref)
			if err != nil || ok {
				return artifactPath, ok, err
			}
		}
		return "", false, nil
	}
	return s.localArtifactPath(artifact)
}

func (s *Server) localArtifactPath(artifact expcockpit.ArtifactView) (string, bool, error) {
	for _, ref := range []string{artifact.Preview, artifact.URI} {
		path, ok, err := s.resolveStoreFile(ref)
		if err != nil || ok {
			return path, ok, err
		}
	}
	return "", false, nil
}

func durableArtifactPath(ctx context.Context, artifact expcockpit.ArtifactView) (string, bool, func(), error) {
	if strings.TrimSpace(artifact.DurableRef) == "" {
		return "", false, func() {}, nil
	}
	ref, err := blobstore.ParseDurableRef(artifact.DurableRef)
	if err != nil {
		return "", false, func() {}, fmt.Errorf("parse durable artifact ref %q: %w", artifact.ArtifactID, err)
	}
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return "", false, func() {}, fmt.Errorf("parse durable artifact uri %q: %w", ref.URI, err)
	}
	if parsed.Scheme != "file" {
		return durableArtifactTempPath(ctx, artifact, ref, parsed.Scheme)
	}
	artifactPath := filepath.Clean(filepath.FromSlash(parsed.Path))
	info, err := os.Stat(artifactPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, func() {}, nil
		}
		return "", false, func() {}, err
	}
	if info.IsDir() {
		return "", false, func() {}, fmt.Errorf("durable artifact path is a directory")
	}
	digest, size, err := fileutil.FileSHA256(artifactPath)
	if err != nil {
		return "", false, func() {}, err
	}
	if blobstore.DigestWithAlgorithm(digest) != ref.Digest {
		return "", false, func() {}, fmt.Errorf("durable artifact digest %s does not match ref %s", blobstore.DigestWithAlgorithm(digest), ref.Digest)
	}
	if size != ref.SizeBytes {
		return "", false, func() {}, fmt.Errorf("durable artifact size %d does not match ref %d", size, ref.SizeBytes)
	}
	return artifactPath, true, func() {}, nil
}

func durableArtifactTempPath(ctx context.Context, artifact expcockpit.ArtifactView, ref blobstore.DurableRef, scheme string) (string, bool, func(), error) {
	if scheme != "azblob" && scheme != "https" {
		return "", false, func() {}, fmt.Errorf("durable artifact uri scheme %q is not supported by this server", scheme)
	}
	ext := filepath.Ext(artifact.Name)
	if ext == "" {
		ext = filepath.Ext(artifact.URI)
	}
	tmp, err := os.CreateTemp("", "tau-durable-artifact-*"+ext)
	if err != nil {
		return "", false, func() {}, err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := blobstore.DownloadAndVerify(ctx, ref, tmp); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", false, func() {}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", false, func() {}, err
	}
	return tmpPath, true, cleanup, nil
}

func (s *Server) resolveStoreFile(ref string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "data:") {
		return "", false, nil
	}
	if parsed, err := url.Parse(ref); err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "file" {
			return "", false, nil
		}
		ref = parsed.Path
	}
	root, err := filepath.Abs(s.storeRoot)
	if err != nil {
		return "", false, err
	}
	var candidate string
	if filepath.IsAbs(ref) {
		candidate = filepath.Clean(ref)
	} else {
		candidate = filepath.Join(root, filepath.Clean(ref))
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", false, err
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("artifact path escapes the Stellar store")
	}
	_, err = existingContainedFileInfo(root, candidate, "artifact path escapes the Stellar store")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return candidate, true, nil
}

func localArtifactBundleAssetPath(artifactPath, assetPath string) (string, bool, error) {
	clean, err := cleanArtifactAssetPath(assetPath)
	if err != nil {
		return "", false, err
	}
	if clean == "" {
		return artifactPath, true, nil
	}
	artifactDir := filepath.Dir(artifactPath)
	candidate := filepath.Join(artifactDir, filepath.FromSlash(clean))
	_, err = existingContainedFileInfo(artifactDir, candidate, "artifact bundle asset escapes the artifact directory")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return candidate, true, nil
}

func cleanArtifactAssetPath(assetPath string) (string, error) {
	assetPath = strings.TrimSpace(strings.TrimPrefix(assetPath, "/"))
	if assetPath == "" {
		return "", nil
	}
	if strings.Contains(assetPath, `\`) {
		return "", fmt.Errorf("artifact bundle asset path contains a path separator")
	}
	for _, segment := range strings.Split(assetPath, "/") {
		if segment == ".." {
			return "", fmt.Errorf("artifact bundle asset path escapes the artifact directory")
		}
	}
	clean := path.Clean(assetPath)
	if clean == "." {
		return "", nil
	}
	if strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
		return "", fmt.Errorf("artifact bundle asset path escapes the artifact directory")
	}
	return clean, nil
}

func existingContainedFileInfo(root, candidate, escapeMessage string) (os.FileInfo, error) {
	if err := ensureContainedPath(root, candidate, escapeMessage); err != nil {
		return nil, err
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("artifact path is a directory")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return nil, err
	}
	if err := ensureContainedPath(realRoot, realCandidate, escapeMessage); err != nil {
		return nil, err
	}
	return info, nil
}

func ensureContainedPath(root, candidate, escapeMessage string) error {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s", escapeMessage)
	}
	return nil
}

func isReportArtifactDocument(artifact expcockpit.ArtifactView, artifactPath string) bool {
	value := strings.ToLower(strings.Join([]string{
		artifact.Type,
		artifact.Name,
		artifact.URI,
		artifact.Preview,
		artifact.ExternalRef,
		filepath.Ext(artifactPath),
	}, " "))
	return strings.Contains(value, "report") || strings.Contains(value, "html") || strings.Contains(value, ".htm")
}

func setReportArtifactHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Content-Security-Policy", "sandbox; frame-ancestors 'self'; script-src 'none'; object-src 'none'; base-uri 'none'")
}

func serveLocalArtifact(w http.ResponseWriter, r *http.Request, path string) {
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(path))
	if contentType == "" {
		buffer := make([]byte, 512)
		n, _ := file.Read(buffer)
		contentType = http.DetectContentType(buffer[:n])
		_, _ = file.Seek(0, 0)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) requestContext(r *http.Request) (context.Context, context.CancelFunc) {
	if s.requestTimeout <= 0 {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeout(r.Context(), s.requestTimeout)
}

func (s *Server) targetAndMetric(r *http.Request) (string, string, error) {
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		target = s.defaultTarget
	}
	if target == "" {
		return "", "", fmt.Errorf("target query parameter is required")
	}
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if metric == "" {
		metric = s.defaultMetric
	}
	return target, metric, nil
}

func seriesOptionsFromRequest(r *http.Request, workspace, target, metric string, maxRuns, maxMetricRows int) (expcockpit.SeriesOptions, error) {
	if strings.TrimSpace(metric) == "" {
		return expcockpit.SeriesOptions{}, fmt.Errorf("metric query parameter is required")
	}
	maxPoints := defaultSeriesMaxPoints
	if raw := strings.TrimSpace(r.URL.Query().Get("max_points")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return expcockpit.SeriesOptions{}, fmt.Errorf("max_points must be an integer")
		}
		if parsed < 1 {
			return expcockpit.SeriesOptions{}, fmt.Errorf("max_points must be at least 1")
		}
		if parsed > maxSeriesMaxPoints {
			return expcockpit.SeriesOptions{}, fmt.Errorf("max_points must be at most %d", maxSeriesMaxPoints)
		}
		maxPoints = parsed
	}
	startStep, err := optionalInt64Query(r, "start_step")
	if err != nil {
		return expcockpit.SeriesOptions{}, err
	}
	endStep, err := optionalInt64Query(r, "end_step")
	if err != nil {
		return expcockpit.SeriesOptions{}, err
	}
	if startStep != nil && endStep != nil && *startStep > *endStep {
		return expcockpit.SeriesOptions{}, fmt.Errorf("start_step must be <= end_step")
	}
	stepInterval := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("step_interval")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return expcockpit.SeriesOptions{}, fmt.Errorf("step_interval must be an integer")
		}
		if parsed < 1 {
			return expcockpit.SeriesOptions{}, fmt.Errorf("step_interval must be at least 1")
		}
		stepInterval = parsed
	}
	return expcockpit.SeriesOptions{
		Target:        target,
		Workspace:     workspace,
		Metric:        metric,
		RunID:         strings.TrimSpace(r.URL.Query().Get("run_id")),
		StartStep:     startStep,
		EndStep:       endStep,
		StepInterval:  stepInterval,
		MaxRuns:       maxRuns,
		MaxMetricRows: maxMetricRows,
		MaxPoints:     maxPoints,
	}, nil
}

func runSearchOptionsFromRequest(r *http.Request, workspace string) (expstore.RunSearchOptions, error) {
	q := r.URL.Query()
	limit := 200
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return expstore.RunSearchOptions{}, fmt.Errorf("limit must be an integer")
		}
		limit = parsed
	}
	minStep, err := optionalInt64Query(r, "min_step")
	if err != nil {
		return expstore.RunSearchOptions{}, err
	}
	metricNames := append([]string{}, q["metric_name"]...)
	if len(metricNames) == 0 {
		metricNames = append(metricNames, q["metric"]...)
	}
	metricFilters, err := parseRunSearchMetricFilters(q["metric_filter"])
	if err != nil {
		return expstore.RunSearchOptions{}, err
	}
	tags, err := parseRunSearchTags(q["tag"])
	if err != nil {
		return expstore.RunSearchOptions{}, err
	}
	return expstore.RunSearchOptions{
		Target:        strings.TrimSpace(q.Get("target")),
		Workspace:     workspace,
		Query:         strings.TrimSpace(q.Get("q")),
		Project:       strings.TrimSpace(q.Get("project")),
		RunGroupID:    strings.TrimSpace(q.Get("group")),
		State:         strings.TrimSpace(q.Get("state")),
		Lifecycle:     strings.TrimSpace(q.Get("lifecycle")),
		Tags:          tags,
		MetricNames:   compactStrings(metricNames),
		MetricFilters: metricFilters,
		Since:         strings.TrimSpace(q.Get("since")),
		Limit:         limit,
		MinStep:       minStep,
	}, nil
}

func experimentSearchOptionsFromRequest(r *http.Request, workspace string) (expstore.ExperimentSearchOptions, error) {
	q := r.URL.Query()
	limit := 200
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return expstore.ExperimentSearchOptions{}, fmt.Errorf("limit must be an integer")
		}
		limit = parsed
	}
	metricNames := append([]string{}, q["metric_name"]...)
	if len(metricNames) == 0 {
		metricNames = append(metricNames, q["metric"]...)
	}
	metricFilters, err := parseRunSearchMetricFilters(q["metric_filter"])
	if err != nil {
		return expstore.ExperimentSearchOptions{}, err
	}
	tags, err := parseRunSearchTags(q["tag"])
	if err != nil {
		return expstore.ExperimentSearchOptions{}, err
	}
	return expstore.ExperimentSearchOptions{
		Query:         strings.TrimSpace(q.Get("q")),
		Workspace:     workspace,
		Project:       strings.TrimSpace(q.Get("project")),
		Lifecycle:     strings.TrimSpace(q.Get("lifecycle")),
		Tags:          tags,
		MetricNames:   compactStrings(metricNames),
		MetricFilters: metricFilters,
		Since:         strings.TrimSpace(q.Get("since")),
		Limit:         limit,
	}, nil
}

func parseRunSearchMetricFilters(filters []string) ([]expstore.MetricFilter, error) {
	out := make([]expstore.MetricFilter, 0, len(filters))
	for _, filter := range filters {
		parsed, err := expstore.ParseMetricFilter(filter)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

func parseRunSearchTags(tags []string) (map[string]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, tag := range tags {
		key, value, ok := strings.Cut(tag, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("tag filters must be key=value")
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out, nil
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func optionalInt64Query(r *http.Request, name string) (*int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer", name)
	}
	return &parsed, nil
}

func statusCode(err error) int {
	if errors.Is(err, expstore.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadRequest
}

func normalizeStellarSource(source string) (string, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return "", nil
	}
	switch source {
	case "local", "kusto", "auto":
		return source, nil
	default:
		return "", fmt.Errorf("source must be local, kusto, or auto")
	}
}

func execLookPath(command string) (string, error) {
	if strings.Contains(command, "/") {
		return command, nil
	}
	return exec.LookPath(command)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func apiSnapshot(snapshot expcockpit.Snapshot, includeStatic bool) expcockpit.Snapshot {
	clearChartSVGPoints(&snapshot.Chart)
	for i := range snapshot.Sweep.Series {
		snapshot.Sweep.Series[i].Points = ""
	}
	if !includeStatic && snapshot.PayloadMode == string(expcockpit.SnapshotModeMetric) {
		snapshot.Experiment = nil
		snapshot.RunGroups = nil
		snapshot.Runs = nil
		snapshot.MetricOptions = nil
	}
	return snapshot
}

func apiSeries(series expcockpit.SeriesDetail) expcockpit.SeriesDetail {
	clearChartSVGPoints(&series.Chart)
	return series
}

func includeStaticSnapshotFields(r *http.Request) bool {
	return r.URL.Query().Get("include_static") != "false"
}

func clearChartSVGPoints(chart *expcockpit.ChartView) {
	for i := range chart.Series {
		chart.Series[i].Points = ""
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error":  http.StatusText(status),
		"code":   errorCode(status, message),
		"detail": message,
		"status": status,
	})
}

func errorCode(status int, message string) string {
	detail := strings.ToLower(message)
	if strings.Contains(detail, "local-only") ||
		strings.Contains(detail, "currently supports local") ||
		strings.Contains(detail, "requires the local expstore") ||
		strings.Contains(detail, "artifact bundle assets require a local report artifact") {
		return "UNSUPPORTED_CAPABILITY"
	}
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusMethodNotAllowed:
		return "METHOD_NOT_ALLOWED"
	case http.StatusNotImplemented:
		return "UNSUPPORTED_CAPABILITY"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusServiceUnavailable:
		return "SOURCE_UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "UPSTREAM_TIMEOUT"
	case http.StatusBadGateway:
		return "ARTIFACT_FETCH_FAILED"
	default:
		if status >= 500 {
			return "SOURCE_UNAVAILABLE"
		}
		return "INVALID_ARGUMENT"
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
