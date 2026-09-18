// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

const (
	v2DefaultLimit = 50
	v2MaxLimit     = 200
	v2ScanLimit    = 1000
)

type v2Metadata struct {
	GeneratedAt  string         `json:"generated_at"`
	Provenance   v2Provenance   `json:"provenance"`
	Freshness    v2Freshness    `json:"freshness"`
	Availability v2Availability `json:"availability"`
	Partial      bool           `json:"partial"`
	Warnings     []string       `json:"warnings,omitempty"`
}

type v2Provenance struct {
	RequestedSource string   `json:"requested_source"`
	ServedSources   []string `json:"served_sources"`
}

type v2Freshness struct {
	State string `json:"state"`
	AsOf  string `json:"as_of"`
}

type v2Availability struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons,omitempty"`
}

type v2Experiment struct {
	ExperimentID   string         `json:"experiment_id"`
	Project        string         `json:"project"`
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
	RunCount       int            `json:"run_count"`
	RunGroupCount  int            `json:"run_group_count"`
	StateCounts    map[string]int `json:"state_counts,omitempty"`
	LifecycleCount map[string]int `json:"lifecycle_counts,omitempty"`
	LatestRunAt    string         `json:"latest_run_at,omitempty"`
	MetricNames    []string       `json:"metric_names"`
}

type v2ExperimentSearchResponse struct {
	Metadata    v2Metadata     `json:"metadata"`
	Experiments []v2Experiment `json:"experiments"`
	NextCursor  string         `json:"next_cursor,omitempty"`
}

type v2Run struct {
	RunID          string            `json:"run_id"`
	ExperimentID   string            `json:"experiment_id,omitempty"`
	Project        string            `json:"project"`
	RunGroupID     string            `json:"run_group_id"`
	ParentRunID    string            `json:"parent_run_id,omitempty"`
	State          string            `json:"state"`
	LifecycleState string            `json:"lifecycle_state"`
	Successful     bool              `json:"successful"`
	SuccessReasons []string          `json:"success_reasons,omitempty"`
	Owner          string            `json:"owner,omitempty"`
	CreatedAt      string            `json:"created_at"`
	StartedAt      string            `json:"started_at,omitempty"`
	CompletedAt    string            `json:"completed_at,omitempty"`
	ConfigHash     string            `json:"config_hash,omitempty"`
	CodeSHA        string            `json:"code_sha,omitempty"`
	ImageDigest    string            `json:"image_digest,omitempty"`
	ResultURI      string            `json:"result_uri,omitempty"`
	Tags           map[string]string `json:"tags"`
	MetricNames    []string          `json:"metric_names"`
	Source         string            `json:"source"`
}

type v2RunListResponse struct {
	Metadata   v2Metadata `json:"metadata"`
	Target     string     `json:"target"`
	Runs       []v2Run    `json:"runs"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type v2RunDetailResponse struct {
	Metadata v2Metadata `json:"metadata"`
	Run      v2Run      `json:"run"`
}

type v2Metric struct {
	Name           string   `json:"name"`
	Count          int64    `json:"count"`
	FiniteCount    int64    `json:"finite_count"`
	NonFiniteCount int64    `json:"non_finite_count"`
	MinStep        *int64   `json:"min_step,omitempty"`
	MaxStep        *int64   `json:"max_step,omitempty"`
	LatestStep     *int64   `json:"latest_step,omitempty"`
	LatestWallTime *int64   `json:"latest_wall_time,omitempty"`
	LatestValue    *float64 `json:"latest_value,omitempty"`
	MinValue       *float64 `json:"min_value,omitempty"`
	MaxValue       *float64 `json:"max_value,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
}

type v2MetricCatalogResponse struct {
	Metadata v2Metadata `json:"metadata"`
	RunID    string     `json:"run_id"`
	Metrics  []v2Metric `json:"metrics"`
}

type v2SeriesPoint struct {
	Step  int64   `json:"step"`
	Value float64 `json:"value"`
}

type v2SeriesResponse struct {
	Metadata       v2Metadata      `json:"metadata"`
	Target         string          `json:"target"`
	RunID          string          `json:"run_id"`
	Metric         string          `json:"metric"`
	StartStep      *int64          `json:"start_step,omitempty"`
	EndStep        *int64          `json:"end_step,omitempty"`
	StepInterval   int             `json:"step_interval,omitempty"`
	MaxPoints      int             `json:"max_points"`
	SourcePoints   int             `json:"source_points"`
	ReturnedPoints int             `json:"returned_points"`
	Points         []v2SeriesPoint `json:"points"`
}

type v2CursorPayload struct {
	Version    int    `json:"v"`
	FilterHash string `json:"f"`
	SortAt     string `json:"t"`
	ItemID     string `json:"i"`
}

type v2ErrorEnvelope struct {
	Error v2Error `json:"error"`
}

type v2Error struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Classification string `json:"classification"`
	Retryable      bool   `json:"retryable"`
}

type v2CatalogSource interface {
	searchExperiments(context.Context, string, expstore.ExperimentSearchOptions) (v2ExperimentCatalogResult, error)
	searchRuns(context.Context, string, expstore.RunSearchOptions) (runSearchResponse, error)
}

type v2ExperimentCatalogResult struct {
	Result        expstore.ExperimentSearchResult
	ServedSources []string
}

// legacyCatalogSource preserves the bounded raw ExperimentMetrics discovery
// path while stable catalog functions are being deployed and verified.
type legacyCatalogSource struct {
	server *Server
}

func (a legacyCatalogSource) searchExperiments(ctx context.Context, source string, opts expstore.ExperimentSearchOptions) (v2ExperimentCatalogResult, error) {
	localSearch := func() (expstore.ExperimentSearchResult, error) {
		return a.server.searchLocalExperiments(ctx, opts)
	}
	kustoSearch := func() (expstore.ExperimentSearchResult, error) {
		return a.server.baseKustoSource().SearchExperiments(ctx, opts)
	}
	switch source {
	case "local":
		result, err := localSearch()
		return v2ExperimentCatalogResult{Result: result, ServedSources: []string{"local"}}, err
	case "kusto":
		result, err := kustoSearch()
		return v2ExperimentCatalogResult{Result: result, ServedSources: []string{"kusto"}}, err
	case "auto":
		return searchAutoExperimentCatalogs(ctx, a.server.hasKustoSource(), opts.Limit, localSearch, kustoSearch)
	default:
		return v2ExperimentCatalogResult{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func (a legacyCatalogSource) searchRuns(ctx context.Context, source string, opts expstore.RunSearchOptions) (runSearchResponse, error) {
	localSearch := func() (runSearchResponse, error) {
		return a.server.searchRuns(ctx, "local", opts)
	}
	kustoSearch := func() (runSearchResponse, error) {
		result, err := a.server.baseKustoSource().SearchRuns(ctx, opts)
		return withRunSource(result, "kusto"), err
	}
	switch source {
	case "local":
		return localSearch()
	case "kusto":
		return kustoSearch()
	case "auto":
		result, warnings, err := searchAutoSources(ctx, a.server.hasKustoSource(), "run", localSearch, kustoSearch,
			func(local, kusto runSearchResponse) runSearchResponse {
				return mergeRunSearchResults(local, kusto, opts.Limit)
			})
		result.Warnings = append(result.Warnings, warnings...)
		return result, err
	default:
		return runSearchResponse{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

// stableFunctionCatalogSource sends Kusto discovery through
// TauExpSeriesCatalogRows()/TauExpRunCatalogRows(). CatalogShadowRead remains a
// separate diagnostic on the legacy layer and is not part of this adapter.
type stableFunctionCatalogSource struct {
	server *Server
}

func (a stableFunctionCatalogSource) searchExperiments(ctx context.Context, source string, opts expstore.ExperimentSearchOptions) (v2ExperimentCatalogResult, error) {
	localSearch := func() (expstore.ExperimentSearchResult, error) {
		return a.server.searchLocalExperiments(ctx, opts)
	}
	kustoSearch := func() (expstore.ExperimentSearchResult, error) {
		return a.server.baseKustoSource().SearchCatalogExperiments(ctx, opts)
	}
	switch source {
	case "local":
		result, err := localSearch()
		return v2ExperimentCatalogResult{Result: result, ServedSources: []string{"local"}}, err
	case "kusto":
		result, err := kustoSearch()
		return v2ExperimentCatalogResult{Result: result, ServedSources: []string{"kusto"}}, err
	case "auto":
		return searchAutoExperimentCatalogs(ctx, a.server.hasKustoSource(), opts.Limit, localSearch, kustoSearch)
	default:
		return v2ExperimentCatalogResult{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func searchAutoExperimentCatalogs(
	ctx context.Context,
	hasKusto bool,
	limit int,
	localSearch, kustoSearch func() (expstore.ExperimentSearchResult, error),
) (v2ExperimentCatalogResult, error) {
	local, localErr := localSearch()
	if err := ctx.Err(); err != nil {
		return v2ExperimentCatalogResult{Result: local}, err
	}
	if !hasKusto {
		return v2ExperimentCatalogResult{Result: local, ServedSources: []string{"local"}}, localErr
	}
	kusto, kustoErr := kustoSearch()
	if err := ctx.Err(); err != nil {
		return v2ExperimentCatalogResult{Result: local}, err
	}
	if localErr != nil {
		if kustoErr != nil {
			return v2ExperimentCatalogResult{}, errors.Join(
				fmt.Errorf("local experiment search: %w", localErr),
				fmt.Errorf("Kusto experiment search: %w", kustoErr))
		}
		kusto.Warnings = append(kusto.Warnings,
			fmt.Sprintf("source=auto fell back to Kusto because local experiment search failed: %v", localErr))
		return v2ExperimentCatalogResult{Result: kusto, ServedSources: []string{"kusto"}}, nil
	}
	if kustoErr != nil {
		local.Warnings = append(local.Warnings,
			fmt.Sprintf("source=auto skipped Kusto experiment search because it failed: %v", kustoErr))
		return v2ExperimentCatalogResult{Result: local, ServedSources: []string{"local"}}, nil
	}
	merged := mergeExperimentSearchResults(local, kusto, limit)
	sources := experimentContributingSources(local, kusto)
	return v2ExperimentCatalogResult{Result: merged, ServedSources: sources}, nil
}

func experimentContributingSources(local, kusto expstore.ExperimentSearchResult) []string {
	sources := []string{}
	if len(local.Experiments) > 0 {
		sources = append(sources, "local")
	}
	localIDs := make(map[string]bool, len(local.Experiments))
	for _, experiment := range local.Experiments {
		localIDs[experiment.ExperimentID] = true
	}
	for _, experiment := range kusto.Experiments {
		if !localIDs[experiment.ExperimentID] {
			sources = append(sources, "kusto")
			break
		}
	}
	if len(sources) == 0 {
		return []string{"local", "kusto"}
	}
	return sources
}

func (a stableFunctionCatalogSource) searchRuns(ctx context.Context, source string, opts expstore.RunSearchOptions) (runSearchResponse, error) {
	localSearch := func() (runSearchResponse, error) {
		return a.server.searchRuns(ctx, "local", opts)
	}
	kustoSearch := func() (runSearchResponse, error) {
		result, err := a.server.baseKustoSource().SearchCatalogRuns(ctx, opts)
		return withRunSource(result, "kusto"), err
	}
	switch source {
	case "local":
		return localSearch()
	case "kusto":
		return kustoSearch()
	case "auto":
		result, warnings, err := searchAutoSources(ctx, a.server.hasKustoSource(), "run", localSearch, kustoSearch,
			func(local, kusto runSearchResponse) runSearchResponse {
				return mergeRunSearchResults(local, kusto, opts.Limit)
			})
		result.Warnings = append(result.Warnings, warnings...)
		return result, err
	default:
		return runSearchResponse{}, fmt.Errorf("unsupported Stellar source %q", source)
	}
}

func (s *Server) v2CatalogSource() v2CatalogSource {
	if s.v2Catalog != nil {
		return s.v2Catalog
	}
	return s.configuredV2CatalogSource()
}

func (s *Server) configuredV2CatalogSource() v2CatalogSource {
	if s.kustoExperimentCatalogReadSource == "functions" {
		return stableFunctionCatalogSource{server: s}
	}
	return legacyCatalogSource{server: s}
}

func newV2CursorKey(workspace, source string) []byte {
	sum := sha256.Sum256([]byte("tau.stellar.v2.cursor\x00" + workspace + "\x00" + source))
	return sum[:]
}

func (s *Server) handleV2ExperimentSearch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != stellarAPIV2Base+"/experiments/search" {
		s.writeV2Error(w, http.StatusNotFound, "NOT_FOUND", "route was not found")
		return
	}
	if r.Method != http.MethodGet {
		s.writeV2Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	source, workspace, ok := s.v2SourceAndWorkspace(w, r)
	if !ok {
		return
	}
	opts, err := experimentSearchOptionsFromRequest(r, workspace)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	limit, err := v2Limit(r)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	opts.Limit = v2ScanLimit
	filterHash := v2ExperimentFilterHash(source, workspace, opts)
	cursor, err := s.decodeV2Cursor(r.URL.Query().Get("cursor"), filterHash)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_CURSOR", err.Error())
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	catalogResult, err := s.v2CatalogSource().searchExperiments(ctx, source, opts)
	if err != nil {
		s.writeV2ClassifiedError(w, err)
		return
	}
	result := catalogResult.Result
	experiments := append([]expstore.ExperimentSummary(nil), result.Experiments...)
	sort.SliceStable(experiments, func(i, j int) bool {
		left, right := v2ExperimentSortAt(experiments[i]), v2ExperimentSortAt(experiments[j])
		if left != right {
			return left > right
		}
		return experiments[i].ExperimentID < experiments[j].ExperimentID
	})
	filtered := experiments[:0]
	for _, item := range experiments {
		if cursor != nil && !v2ExperimentAfterCursor(item, *cursor) {
			continue
		}
		filtered = append(filtered, item)
	}
	hasMore := len(filtered) > limit
	if hasMore {
		filtered = filtered[:limit]
	}
	nextCursor := ""
	if hasMore && len(filtered) > 0 {
		last := filtered[len(filtered)-1]
		nextCursor, err = s.encodeV2Cursor(v2CursorPayload{
			Version: 1, FilterHash: filterHash, SortAt: v2ExperimentSortAt(last), ItemID: last.ExperimentID,
		})
		if err != nil {
			s.writeV2ClassifiedError(w, err)
			return
		}
	}
	items := make([]v2Experiment, 0, len(filtered))
	for _, item := range filtered {
		items = append(items, v2Experiment{
			ExperimentID: item.ExperimentID, Project: item.Project, Name: item.Name,
			Description: item.Description, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
			RunCount: item.RunCount, RunGroupCount: item.RunGroupCount, StateCounts: item.StateCounts,
			LifecycleCount: item.LifecycleCounts, LatestRunAt: item.LatestRunAt,
			MetricNames: nonNilStrings(item.MetricNames),
		})
	}
	writeJSON(w, http.StatusOK, v2ExperimentSearchResponse{
		Metadata:    s.v2Metadata(source, catalogResult.ServedSources, result.Truncated, result.Warnings),
		Experiments: items,
		NextCursor:  nextCursor,
	})
}

func (s *Server) handleV2ExperimentRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, stellarAPIV2Base+"/experiments/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "runs" {
		s.writeV2Error(w, http.StatusNotFound, "NOT_FOUND", "route was not found")
		return
	}
	if r.Method != http.MethodGet {
		s.writeV2Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	target, err := decodeV2PathID(parts[0])
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_PATH", err.Error())
		return
	}
	s.handleV2RunList(w, r, target)
}

func (s *Server) handleV2RunRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, stellarAPIV2Base+"/runs/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || len(parts) > 2 {
		s.writeV2Error(w, http.StatusNotFound, "NOT_FOUND", "route was not found")
		return
	}
	runID, err := decodeV2PathID(parts[0])
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_PATH", err.Error())
		return
	}
	if r.Method != http.MethodGet {
		s.writeV2Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if len(parts) == 1 {
		s.handleV2RunDetail(w, r, runID)
		return
	}
	switch parts[1] {
	case "metrics":
		s.handleV2MetricCatalog(w, r, runID)
	case "series":
		s.handleV2Series(w, r, runID)
	default:
		s.writeV2Error(w, http.StatusNotFound, "NOT_FOUND", "route was not found")
	}
}

func (s *Server) handleV2RunList(w http.ResponseWriter, r *http.Request, target string) {
	source, workspace, ok := s.v2SourceAndWorkspace(w, r)
	if !ok {
		return
	}
	opts, err := runSearchOptionsFromRequest(r, workspace)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if queryTarget := strings.TrimSpace(opts.Target); queryTarget != "" && queryTarget != target {
		s.writeV2Error(w, http.StatusBadRequest, "TARGET_MISMATCH", "target query parameter must match the experiment path")
		return
	}
	opts.Target = target
	limit, err := v2Limit(r)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	opts.Limit = v2ScanLimit
	filterHash := v2RunFilterHash(source, workspace, opts)
	cursor, err := s.decodeV2Cursor(r.URL.Query().Get("cursor"), filterHash)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_CURSOR", err.Error())
		return
	}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	result, err := s.v2CatalogSource().searchRuns(ctx, source, opts)
	if err != nil {
		s.writeV2ClassifiedError(w, err)
		return
	}
	runs := append([]sourcedRun(nil), result.Runs...)
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].CreatedAt != runs[j].CreatedAt {
			return runs[i].CreatedAt > runs[j].CreatedAt
		}
		return v2RunCursorID(runs[i]) < v2RunCursorID(runs[j])
	})
	filtered := runs[:0]
	for _, run := range runs {
		if run.ExperimentID != target {
			continue
		}
		if cursor != nil && !v2RunAfterCursor(run, *cursor) {
			continue
		}
		filtered = append(filtered, run)
	}
	hasMore := len(filtered) > limit
	if hasMore {
		filtered = filtered[:limit]
	}
	nextCursor := ""
	if hasMore && len(filtered) > 0 {
		last := filtered[len(filtered)-1]
		nextCursor, err = s.encodeV2Cursor(v2CursorPayload{Version: 1, FilterHash: filterHash, SortAt: last.CreatedAt, ItemID: v2RunCursorID(last)})
		if err != nil {
			s.writeV2ClassifiedError(w, err)
			return
		}
	}
	items := make([]v2Run, 0, len(filtered))
	for _, run := range filtered {
		items = append(items, v2RunFromSearch(run))
	}
	writeJSON(w, http.StatusOK, v2RunListResponse{
		Metadata: s.v2Metadata(source, sourceListForRuns(source, filtered), result.Truncated, result.Warnings),
		Target:   target, Runs: items, NextCursor: nextCursor,
	})
}

func (s *Server) handleV2RunDetail(w http.ResponseWriter, r *http.Request, runID string) {
	source, workspace, ok := s.v2SourceAndWorkspace(w, r)
	if !ok {
		return
	}
	run, warnings, err := s.v2ExactRun(r, source, workspace, runID)
	if err != nil {
		s.writeV2ClassifiedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v2RunDetailResponse{
		Metadata: s.v2Metadata(source, []string{run.Source}, false, warnings),
		Run:      v2RunFromSearch(run),
	})
}

func (s *Server) handleV2MetricCatalog(w http.ResponseWriter, r *http.Request, runID string) {
	source, workspace, ok := s.v2SourceAndWorkspace(w, r)
	if !ok {
		return
	}
	run, warnings, err := s.v2ExactRun(r, source, workspace, runID)
	if err != nil {
		s.writeV2ClassifiedError(w, err)
		return
	}
	metrics := make([]v2Metric, 0, len(run.Metrics))
	for _, metric := range run.Metrics {
		item := v2Metric{
			Name: metric.MetricName, Count: metric.Count, FiniteCount: metric.FiniteCount,
			NonFiniteCount: metric.NonFiniteCount, MinStep: metric.MinStep, MaxStep: metric.MaxStep,
			LatestStep: metric.LatestStep, LatestWallTime: metric.LatestWallTime, UpdatedAt: metric.UpdatedAt,
		}
		if metric.FiniteCount > 0 {
			item.LatestValue = float64Pointer(metric.LatestValue)
			item.MinValue = float64Pointer(metric.MinValue)
			item.MaxValue = float64Pointer(metric.MaxValue)
		}
		metrics = append(metrics, item)
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	meta := s.v2Metadata(source, []string{run.Source}, false, warnings)
	if len(metrics) == 0 {
		meta.Availability = v2Availability{State: "unavailable", Reasons: []string{"no indexed scalar metrics are available for this run"}}
	}
	writeJSON(w, http.StatusOK, v2MetricCatalogResponse{Metadata: meta, RunID: runID, Metrics: metrics})
}

func (s *Server) handleV2Series(w http.ResponseWriter, r *http.Request, runID string) {
	source, workspace, ok := s.v2SourceAndWorkspace(w, r)
	if !ok {
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if target == "" || metric == "" {
		s.writeV2Error(w, http.StatusBadRequest, "EXACT_SERIES_REQUIRED", "target and metric query parameters are required")
		return
	}
	if queryRun := strings.TrimSpace(r.URL.Query().Get("run_id")); queryRun != "" && queryRun != runID {
		s.writeV2Error(w, http.StatusBadRequest, "RUN_MISMATCH", "run_id query parameter must match the run path")
		return
	}
	run, runWarnings, err := s.v2ExactRun(r, source, workspace, runID)
	if err != nil {
		s.writeV2ClassifiedError(w, err)
		return
	}
	if target != runID && target != run.ExperimentID && target != run.RunGroupID {
		s.writeV2Error(w, http.StatusNotFound, "TARGET_RUN_NOT_FOUND", "run was not found for target")
		return
	}
	opts, err := seriesOptionsFromRequest(r, workspace, target, metric, 1, s.maxMetricRows)
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	opts.RunID = runID
	ctx, cancel := s.requestContext(r)
	defer cancel()
	series, err := s.buildSeries(ctx, r, opts)
	if err != nil {
		s.writeV2ClassifiedError(w, err)
		return
	}
	points := []v2SeriesPoint{}
	sourcePoints := 0
	decimated := false
	for _, chartSeries := range series.Chart.Series {
		if chartSeries.RunID != runID {
			continue
		}
		sourcePoints += chartSeries.PointCount
		decimated = decimated || chartSeries.Decimated
		for _, point := range chartSeries.Values {
			points = append(points, v2SeriesPoint{Step: point.Step, Value: point.Value})
		}
	}
	warnings := append(runWarnings, series.Warnings...)
	writeJSON(w, http.StatusOK, v2SeriesResponse{
		Metadata: s.v2Metadata(source, []string{run.Source}, decimated, warnings),
		Target:   target, RunID: runID, Metric: metric, StartStep: opts.StartStep, EndStep: opts.EndStep,
		StepInterval: opts.StepInterval, MaxPoints: opts.MaxPoints, SourcePoints: sourcePoints,
		ReturnedPoints: len(points), Points: points,
	})
}

func (s *Server) v2ExactRun(r *http.Request, source, workspace, runID string) (sourcedRun, []string, error) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	opts := expstore.RunSearchOptions{Target: runID, Workspace: workspace, Project: project, Limit: 3}
	ctx, cancel := s.requestContext(r)
	defer cancel()
	result, err := s.v2CatalogSource().searchRuns(ctx, source, opts)
	if err != nil {
		return sourcedRun{}, nil, err
	}
	matches := make([]sourcedRun, 0, len(result.Runs))
	for _, run := range result.Runs {
		if run.RunID != runID || (project != "" && run.Project != project) {
			continue
		}
		target := strings.TrimSpace(r.URL.Query().Get("target"))
		if target != "" && target != run.RunID && target != run.ExperimentID && target != run.RunGroupID {
			continue
		}
		matches = append(matches, run)
	}
	if len(matches) > 1 {
		return sourcedRun{}, result.Warnings, fmt.Errorf("%w: run ID %q exists in multiple projects; specify project", expstore.ErrConflict, runID)
	}
	if len(matches) == 1 {
		return matches[0], result.Warnings, nil
	}
	return sourcedRun{}, result.Warnings, expstore.ErrNotFound
}

func (s *Server) v2SourceAndWorkspace(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	source, err := normalizeStellarSource(r.URL.Query().Get("source"))
	if err != nil {
		s.writeV2Error(w, http.StatusBadRequest, "INVALID_SOURCE", err.Error())
		return "", "", false
	}
	if source == "" {
		source = s.source
	}
	workspace, err := s.resolveWorkspace(r)
	if err != nil {
		s.writeV2Error(w, http.StatusForbidden, "WORKSPACE_FORBIDDEN", err.Error())
		return "", "", false
	}
	return source, workspace, true
}

func (s *Server) v2Metadata(requestedSource string, servedSources []string, partial bool, warnings []string) v2Metadata {
	now := time.Now().UTC().Format(time.RFC3339)
	servedSources = uniqueSortedStrings(servedSources)
	if len(servedSources) == 0 {
		servedSources = []string{requestedSource}
	}
	availability := v2Availability{State: "available"}
	if partial || len(warnings) > 0 {
		availability.State = "degraded"
		availability.Reasons = append([]string(nil), warnings...)
	}
	freshness := "unknown"
	if len(servedSources) == 1 && servedSources[0] == "local" {
		freshness = "immediate"
	}
	return v2Metadata{
		GeneratedAt:  now,
		Provenance:   v2Provenance{RequestedSource: requestedSource, ServedSources: servedSources},
		Freshness:    v2Freshness{State: freshness, AsOf: now},
		Availability: availability,
		Partial:      partial,
		Warnings:     append([]string(nil), warnings...),
	}
}

func v2RunFromSearch(run sourcedRun) v2Run {
	return v2Run{
		RunID: run.RunID, ExperimentID: run.ExperimentID, Project: run.Project,
		RunGroupID: run.RunGroupID, ParentRunID: run.ParentRunID, State: run.State,
		LifecycleState: run.LifecycleState, Successful: run.Successful,
		SuccessReasons: nonNilStrings(run.SuccessReasons), Owner: run.Owner,
		CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		ConfigHash: run.ConfigHash, CodeSHA: run.CodeSHA, ImageDigest: run.ImageDigest,
		ResultURI: run.ResultURI, Tags: nonNilMap(run.Tags),
		MetricNames: nonNilStrings(run.MetricNames), Source: run.Source,
	}
}

func v2Limit(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return v2DefaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > v2MaxLimit {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", v2MaxLimit)
	}
	return limit, nil
}

func decodeV2PathID(raw string) (string, error) {
	value, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("path identifier is invalid")
	}
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") || len(value) > 512 {
		return "", fmt.Errorf("path identifier is invalid")
	}
	return value, nil
}

func v2RunFilterHash(source, workspace string, opts expstore.RunSearchOptions) string {
	payload, _ := json.Marshal(struct {
		Source, Workspace, Target, Query, Project, Group, State, Lifecycle, Since string
		Tags                                                                      map[string]string
		Metrics                                                                   []string
		Filters                                                                   []expstore.MetricFilter
		MinStep                                                                   *int64
	}{
		source, workspace, opts.Target, opts.Query, opts.Project, opts.RunGroupID,
		opts.State, opts.Lifecycle, opts.Since, opts.Tags, opts.MetricNames, opts.MetricFilters, opts.MinStep,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func v2ExperimentFilterHash(source, workspace string, opts expstore.ExperimentSearchOptions) string {
	payload, _ := json.Marshal(struct {
		Source, Workspace, Target, Query, Project, Lifecycle, Since string
		Tags                                                        map[string]string
		Metrics                                                     []string
		Filters                                                     []expstore.MetricFilter
	}{
		source, workspace, opts.Target, opts.Query, opts.Project, opts.Lifecycle, opts.Since,
		opts.Tags, opts.MetricNames, opts.MetricFilters,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (s *Server) encodeV2Cursor(payload v2CursorPayload) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.cursorKey)
	_, _ = mac.Write(body)
	token := append(body, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func (s *Server) decodeV2Cursor(raw, filterHash string) (*v2CursorPayload, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	token, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(token) <= sha256.Size {
		return nil, fmt.Errorf("cursor is malformed")
	}
	body, signature := token[:len(token)-sha256.Size], token[len(token)-sha256.Size:]
	mac := hmac.New(sha256.New, s.cursorKey)
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, fmt.Errorf("cursor signature is invalid")
	}
	var payload v2CursorPayload
	if err := json.Unmarshal(body, &payload); err != nil || payload.Version != 1 ||
		payload.FilterHash != filterHash || payload.SortAt == "" || payload.ItemID == "" {
		return nil, fmt.Errorf("cursor does not match this query")
	}
	return &payload, nil
}

func v2RunAfterCursor(run sourcedRun, cursor v2CursorPayload) bool {
	return run.CreatedAt < cursor.SortAt || (run.CreatedAt == cursor.SortAt && v2RunCursorID(run) > cursor.ItemID)
}

func v2RunCursorID(run sourcedRun) string {
	return strings.TrimSpace(run.Project) + "\x00" + strings.TrimSpace(run.RunID)
}

func v2ExperimentAfterCursor(experiment expstore.ExperimentSummary, cursor v2CursorPayload) bool {
	sortAt := v2ExperimentSortAt(experiment)
	return sortAt < cursor.SortAt || (sortAt == cursor.SortAt && experiment.ExperimentID > cursor.ItemID)
}

func v2ExperimentSortAt(experiment expstore.ExperimentSummary) string {
	for _, value := range []string{experiment.LatestRunAt, experiment.UpdatedAt, experiment.CreatedAt} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "0001-01-01T00:00:00Z"
}

func sourceListForRuns(requested string, runs []sourcedRun) []string {
	sources := make([]string, 0, len(runs))
	for _, run := range runs {
		sources = append(sources, run.Source)
	}
	if len(sources) == 0 && requested != "auto" {
		sources = append(sources, requested)
	}
	return sources
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		if value != "" {
			seen[value] = true
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilMap(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}

func float64Pointer(value float64) *float64 {
	return &value
}

func (s *Server) writeV2ClassifiedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, expstore.ErrNotFound):
		s.writeV2Error(w, http.StatusNotFound, "NOT_FOUND", "requested experiment data was not found")
	case errors.Is(err, expstore.ErrConflict):
		s.writeV2Error(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, ErrWorkspaceForbidden):
		s.writeV2Error(w, http.StatusForbidden, "WORKSPACE_FORBIDDEN", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		s.writeV2Error(w, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "experiment data request timed out")
	default:
		message := strings.ToLower(err.Error())
		switch {
		case strings.Contains(message, "no --kusto"),
			strings.Contains(message, "no metrics file"),
			strings.Contains(message, "not configured"):
			s.writeV2Error(w, http.StatusServiceUnavailable, "SOURCE_UNAVAILABLE", err.Error())
		case strings.Contains(message, "kusto"),
			strings.Contains(message, "upstream"),
			strings.Contains(message, "proxy"),
			strings.Contains(message, "query response"):
			s.writeV2Error(w, http.StatusBadGateway, "UPSTREAM_FAILURE", err.Error())
		default:
			s.writeV2Error(w, http.StatusInternalServerError, "INTERNAL", "experiment data request failed")
		}
	}
}

func (s *Server) writeV2Error(w http.ResponseWriter, status int, code, message string) {
	WriteV2Error(w, status, code, message)
}

// IsCanonicalV2Read reports whether path is one of the narrow, workspace-safe
// v2 reads. Portal uses it to retain the typed error contract at its auth
// boundary before a request reaches this server.
func IsCanonicalV2Read(path string) bool {
	return workspaceScopedV2Route(path)
}

// WriteV2Error writes the canonical v2 typed error envelope.
func WriteV2Error(w http.ResponseWriter, status int, code, message string) {
	classification := "client"
	retryable := false
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		classification = "authorization"
	case http.StatusNotFound:
		classification = "not_found"
	case http.StatusGatewayTimeout, http.StatusBadGateway, http.StatusServiceUnavailable:
		classification = "dependency"
		retryable = true
	default:
		if status >= 500 {
			classification = "internal"
			retryable = true
		}
	}
	writeJSON(w, status, v2ErrorEnvelope{Error: v2Error{
		Code: code, Message: message, Classification: classification, Retryable: retryable,
	}})
}
