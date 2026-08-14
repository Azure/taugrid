// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/portalbin"
)

// SchemaVersion is intentionally stable for existing machine consumers even
// though the researcher-facing surface is now branded Stellar.
const SchemaVersion = "tau.exp.cockpit.v0"

const (
	chartWidthPixels                  = 800
	chartSamplesPerPixel              = 1.5
	chartMaxRenderedPoints            = int(chartWidthPixels * chartSamplesPerPixel)
	chartSmoothingDensePointThreshold = chartWidthPixels
	chartSmoothingEMAAlpha            = 0.12
)

type SnapshotMode string

const (
	SnapshotModeFull    SnapshotMode = ""
	SnapshotModeSummary SnapshotMode = "summary"
	SnapshotModeMetric  SnapshotMode = "metric"
)

var runColorPalette = []string{
	"#2563eb",
	"#dc2626",
	"#16a34a",
	"#9333ea",
	"#ea580c",
	"#0891b2",
	"#be185d",
	"#ca8a04",
	"#4f46e5",
	"#0f766e",
	"#a16207",
	"#db2777",
}

var dashboardDecisionMetricPriority = []string{
	"final/macro_auprc",
	"detect/macro_auprc",
	"eval/macro_auprc",
	"final/mean_episode_return",
	"eval/mean_episode_return",
	"final/reward",
	"eval/reward",
	"final/return",
	"eval/return",
	"final/success_rate",
	"eval/success_rate",
	"final/win_rate",
	"eval/win_rate",
	"final/pass_rate",
	"eval/pass_rate",
	"final/exact_match",
	"eval/exact_match",
	"final/accuracy",
	"eval/accuracy",
	"final/score",
	"eval/score",
}

type Options struct {
	Target            string `json:"target"`
	Workspace         string `json:"workspace,omitempty"`
	Project           string `json:"project,omitempty"`
	Metric            string `json:"metric,omitempty"`
	MaxRuns           int    `json:"max_runs,omitempty"`
	MaxMetricRows     int    `json:"max_metric_rows,omitempty"`
	Mode              SnapshotMode
	SkipMetricCatalog bool `json:"-"`
}

type SeriesOptions struct {
	Target        string
	Workspace     string
	Metric        string
	RunID         string
	StartStep     *int64
	EndStep       *int64
	StepInterval  int
	MaxRuns       int
	MaxMetricRows int
	MaxPoints     int
}

type Source interface {
	BuildSnapshot(context.Context, Options) (Snapshot, error)
}

type LocalSource struct {
	Store *expstore.Store
}

func NewLocalSource(store *expstore.Store) LocalSource {
	return LocalSource{Store: store}
}

func (s LocalSource) BuildSnapshot(ctx context.Context, opts Options) (Snapshot, error) {
	if s.Store == nil {
		return Snapshot{}, fmt.Errorf("local expstore source is required")
	}
	return buildLocalSnapshot(ctx, s.Store, opts)
}

type MergedSource struct {
	Store *expstore.Store
	Kusto KustoSource
}

func (s MergedSource) BuildSnapshot(ctx context.Context, opts Options) (Snapshot, error) {
	if s.Store == nil {
		return s.Kusto.BuildSnapshot(ctx, opts)
	}
	snapshot, err := buildMergedSnapshot(ctx, s.Store, s.Kusto, opts)
	if err == nil {
		return snapshot, nil
	}
	if errors.Is(err, expstore.ErrNotFound) {
		return s.Kusto.BuildSnapshot(ctx, opts)
	}
	return Snapshot{}, err
}

func BuildSeries(ctx context.Context, store *expstore.Store, opts SeriesOptions) (SeriesDetail, error) {
	if store == nil {
		return SeriesDetail{}, fmt.Errorf("local expstore source is required")
	}
	opts.Target = strings.TrimSpace(opts.Target)
	if opts.Target == "" {
		return SeriesDetail{}, fmt.Errorf("dashboard target is required")
	}
	opts.Metric = strings.TrimSpace(opts.Metric)
	if opts.Metric == "" {
		return SeriesDetail{}, fmt.Errorf("metric query parameter is required")
	}
	if opts.MaxPoints <= 0 {
		opts.MaxPoints = chartMaxRenderedPoints
	}
	status, err := store.Status(ctx, opts.Target)
	if err != nil {
		return SeriesDetail{}, err
	}
	experiment, err := loadExperiment(ctx, store, status)
	if err != nil {
		return SeriesDetail{}, err
	}
	groups, err := loadRunGroups(ctx, store, status, experiment)
	if err != nil {
		return SeriesDetail{}, err
	}
	runs, runsTruncated, err := loadRuns(ctx, store, status, experiment, opts.MaxRuns, opts.Workspace, "")
	if err != nil {
		return SeriesDetail{}, err
	}
	if strings.TrimSpace(opts.Workspace) != "" && len(runs) == 0 {
		return SeriesDetail{}, expstore.ErrNotFound
	}
	runIDs := runIDs(runs)
	metricRunIDs := runIDs
	if opts.RunID != "" {
		matching, _, err := loadRuns(ctx, store, status, experiment, 1, opts.Workspace, opts.RunID)
		if err != nil {
			return SeriesDetail{}, err
		}
		if len(matching) == 0 {
			return SeriesDetail{}, expstore.ErrNotFound
		}
		metricRunIDs = []string{opts.RunID}
	}
	metricFiles, err := loadMetricFiles(ctx, store, status, experiment, metricRunIDs)
	if err != nil {
		return SeriesDetail{}, err
	}
	points, metricWarnings := readMetricPointsFiltered(store, metricFiles, opts.MaxMetricRows, metricPointFilter{
		MetricName: opts.Metric,
		RunID:      opts.RunID,
		StartStep:  opts.StartStep,
		EndStep:    opts.EndStep,
	})
	warnings := append([]string{}, metricWarnings...)
	if runsTruncated {
		warnings = append(warnings, fmt.Sprintf("runs truncated to %d of %d matching runs", len(runs), status.Runs))
	}
	if len(points) == 0 {
		warnings = append(warnings, "no scalar metric points matched the series query")
	}
	chart := buildChartWithRunColorsBudgetAndInterval(points, opts.Metric, groupClassMap(groups), runColorMapForRuns(runs), opts.MaxPoints, opts.StepInterval)
	chart.StepInterval = opts.StepInterval
	return SeriesDetail{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Target:        opts.Target,
		Metric:        opts.Metric,
		RunID:         opts.RunID,
		StartStep:     opts.StartStep,
		EndStep:       opts.EndStep,
		StepInterval:  opts.StepInterval,
		MaxPoints:     opts.MaxPoints,
		Chart:         chart,
		Warnings:      warnings,
	}, nil
}

type Snapshot struct {
	SchemaVersion string                     `json:"schema_version"`
	PayloadMode   string                     `json:"payload_mode,omitempty"`
	GeneratedAt   string                     `json:"generated_at"`
	StorePath     string                     `json:"store_path"`
	Target        string                     `json:"target"`
	TargetType    string                     `json:"target_type"`
	Manifest      expstore.Manifest          `json:"manifest"`
	Status        expstore.Status            `json:"status"`
	Summary       ExperimentSummary          `json:"summary"`
	Experiment    *expstore.ExperimentRecord `json:"experiment,omitempty"`
	RunGroups     []RunGroupView             `json:"run_groups"`
	Runs          []RunView                  `json:"runs"`
	Cards         []CardView                 `json:"cards"`
	Chart         ChartView                  `json:"chart"`
	MetricOptions []MetricOptionView         `json:"metric_options"`
	Sweep         SweepView                  `json:"sweep"`
	Compare       CompareInsights            `json:"compare"`
	Artifacts     []ArtifactView             `json:"artifacts"`
	Events        []EventView                `json:"events"`
	Observations  []ObservationView          `json:"observations"`
	Actions       ActionView                 `json:"actions"`
	BestGroupID   string                     `json:"best_group_id,omitempty"`
	SeedCoverage  string                     `json:"seed_coverage"`
	Warnings      []string                   `json:"warnings,omitempty"`
}

type SeriesDetail struct {
	SchemaVersion  string    `json:"schema_version"`
	GeneratedAt    string    `json:"generated_at"`
	Target         string    `json:"target"`
	Metric         string    `json:"metric"`
	RunID          string    `json:"run_id,omitempty"`
	StartStep      *int64    `json:"start_step,omitempty"`
	EndStep        *int64    `json:"end_step,omitempty"`
	StepInterval   int       `json:"step_interval,omitempty"`
	MaxPoints      int       `json:"max_points"`
	Chart          ChartView `json:"chart"`
	RawQuery       string    `json:"raw_query,omitempty"`
	RawQuerySource string    `json:"raw_query_source,omitempty"`
	Warnings       []string  `json:"warnings,omitempty"`
}

type ExperimentSummary struct {
	Status        string `json:"status"`
	CurrentAnswer string `json:"current_answer"`
	BestEvidence  string `json:"best_evidence"`
	Confidence    string `json:"confidence"`
	SeedCoverage  string `json:"seed_coverage"`
	Blockers      int    `json:"blockers"`
	Decisions     int    `json:"decisions"`
	NextAction    string `json:"next_action"`
	NextCommand   string `json:"next_command,omitempty"`
}

type RunGroupView struct {
	RunGroupID string `json:"run_group_id"`
	Project    string `json:"project"`
	Name       string `json:"name"`
	GroupClass string `json:"group_class"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type RunView struct {
	RunID                    string            `json:"run_id"`
	Source                   string            `json:"source,omitempty"`
	SourceStoreID            string            `json:"source_store_id,omitempty"`
	WorkspaceID              string            `json:"workspace_id,omitempty"`
	Cluster                  string            `json:"cluster,omitempty"`
	Project                  string            `json:"project"`
	RunGroupID               string            `json:"run_group_id"`
	State                    string            `json:"state"`
	LifecycleState           string            `json:"lifecycle_state,omitempty"`
	OutcomeState             string            `json:"outcome_state,omitempty"`
	LivenessState            string            `json:"liveness_state,omitempty"`
	LifecycleReason          string            `json:"lifecycle_reason,omitempty"`
	LifecycleSource          string            `json:"lifecycle_source,omitempty"`
	LastEvidenceAt           string            `json:"last_evidence_at,omitempty"`
	FreshnessSeconds         *int64            `json:"freshness_seconds,omitempty"`
	LifecycleExplicit        bool              `json:"lifecycle_explicit,omitempty"`
	WorkloadAbsenceConfirmed bool              `json:"workload_absence_confirmed,omitempty"`
	Successful               bool              `json:"successful,omitempty"`
	SuccessReasons           []string          `json:"success_reasons,omitempty"`
	Owner                    string            `json:"owner,omitempty"`
	CreatedAt                string            `json:"created_at"`
	UpdatedAt                string            `json:"updated_at,omitempty"`
	StartedAt                string            `json:"started_at,omitempty"`
	CompletedAt              string            `json:"completed_at,omitempty"`
	ConfigHash               string            `json:"config_hash,omitempty"`
	CodeSHA                  string            `json:"code_sha,omitempty"`
	ImageDigest              string            `json:"image_digest,omitempty"`
	TauCommand               string            `json:"tau_command,omitempty"`
	ResultURI                string            `json:"result_uri,omitempty"`
	Color                    string            `json:"color,omitempty"`
	Tags                     map[string]string `json:"tags,omitempty"`
	MetricNames              []string          `json:"metric_names,omitempty"`
	Systems                  []FieldView       `json:"systems"`
	Configs                  []ConfigView      `json:"configs,omitempty"`
	Artifacts                []ArtifactView    `json:"artifacts,omitempty"`
	Events                   []EventView       `json:"events,omitempty"`
	Observations             []ObservationView `json:"observations,omitempty"`
	ObserveCLI               string            `json:"observe_cli"`
}

type FieldView struct {
	Name            string `json:"name"`
	Value           string `json:"value"`
	CollectionState string `json:"collection_state"`
}

type CardView struct {
	Name    string       `json:"name"`
	Metrics []MetricView `json:"metrics"`
}

type MetricView struct {
	Name   string            `json:"name"`
	Unit   string            `json:"unit,omitempty"`
	Groups []MetricGroupView `json:"groups"`
}

type MetricGroupView struct {
	RunGroupID string  `json:"run_group_id"`
	GroupClass string  `json:"group_class"`
	RunCount   int     `json:"run_count"`
	LatestStep string  `json:"latest_step"`
	Min        string  `json:"min"`
	P25        string  `json:"p25"`
	Median     string  `json:"median"`
	P75        string  `json:"p75"`
	Max        string  `json:"max"`
	Best       string  `json:"best"`
	BestValue  float64 `json:"best_value"`
}

type MetricOptionView struct {
	Name     string `json:"name"`
	Card     string `json:"card"`
	Selected bool   `json:"selected"`
}

type CompareInsights struct {
	Summary      string        `json:"summary"`
	MetricName   string        `json:"metric_name,omitempty"`
	Outliers     []RunInsight  `json:"outliers,omitempty"`
	EventMarkers []EventMarker `json:"event_markers,omitempty"`
	RuntimeDiffs []RuntimeDiff `json:"runtime_diffs,omitempty"`
}

type RunInsight struct {
	RunID      string `json:"run_id"`
	RunGroupID string `json:"run_group_id"`
	Value      string `json:"value"`
	Reason     string `json:"reason"`
}

type decisionMetricContext struct {
	MetricName  string
	Points      []metricPoint
	Cards       []CardView
	BestGroupID string
}

type EventMarker struct {
	RunID      string `json:"run_id"`
	RunGroupID string `json:"run_group_id"`
	Time       string `json:"time"`
	Type       string `json:"type"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
}

type RuntimeDiff struct {
	Field  string             `json:"field"`
	Values []RuntimeDiffValue `json:"values"`
	Pinned bool               `json:"pinned,omitempty"`
}

type RuntimeDiffValue struct {
	RunGroupID string `json:"run_group_id"`
	Value      string `json:"value"`
}

type TablePreviewView struct {
	Columns []string         `json:"columns,omitempty"`
	Rows    []map[string]any `json:"rows,omitempty"`
	Caption string           `json:"caption,omitempty"`
	Step    string           `json:"step,omitempty"`
}

type ChartView struct {
	HasData      bool            `json:"has_data"`
	MetricName   string          `json:"metric_name,omitempty"`
	XMin         string          `json:"x_min,omitempty"`
	XMax         string          `json:"x_max,omitempty"`
	YMin         string          `json:"y_min,omitempty"`
	YMax         string          `json:"y_max,omitempty"`
	StepInterval int             `json:"step_interval,omitempty"`
	Series       []ChartSeries   `json:"series,omitempty"`
	Smoothing    *ChartSmoothing `json:"smoothing,omitempty"`
}

type ChartSeries struct {
	RunID          string           `json:"run_id"`
	RunGroupID     string           `json:"run_group_id"`
	GroupClass     string           `json:"group_class"`
	Color          string           `json:"color"`
	Points         string           `json:"points,omitempty"`
	Values         []ChartPoint     `json:"values,omitempty"`
	SmoothedValues []ChartPoint     `json:"smoothed_values,omitempty"`
	PointCount     int              `json:"point_count"`
	RenderedPoints int              `json:"rendered_points"`
	Decimated      bool             `json:"decimated,omitempty"`
	Overlay        OverlayMetadata  `json:"overlay,omitempty"`
	Sampling       SamplingMetadata `json:"sampling"`
}

type ChartSmoothing struct {
	Method              string  `json:"method"`
	Alpha               float64 `json:"alpha"`
	DensePointThreshold int     `json:"dense_point_threshold"`
	Reason              string  `json:"reason"`
	RawPreserved        bool    `json:"raw_preserved"`
}

type OverlayMetadata struct {
	Source      string `json:"source"`
	StartStep   int64  `json:"start_step"`
	EndStep     int64  `json:"end_step"`
	SampleCount int    `json:"sample_count"`
}

type ChartPoint struct {
	Step  int64   `json:"step"`
	Value float64 `json:"value"`
}

type SweepView struct {
	HasData     bool                      `json:"has_data"`
	MetricName  string                    `json:"metric_name,omitempty"`
	BestRun     *BestRunView              `json:"best_run,omitempty"`
	Runs        []SweepRunView            `json:"runs,omitempty"`
	Axes        []ParallelAxisView        `json:"axes,omitempty"`
	Series      []ParallelRunSeries       `json:"series,omitempty"`
	Importance  []ParameterImportanceView `json:"importance,omitempty"`
	ConfigCount int                       `json:"config_count"`
}

type BestRunView struct {
	RunID      string  `json:"run_id"`
	RunGroupID string  `json:"run_group_id"`
	GroupClass string  `json:"group_class"`
	MetricName string  `json:"metric_name"`
	Value      string  `json:"value"`
	RawValue   float64 `json:"raw_value"`
}

type SweepRunView struct {
	Rank        int    `json:"rank"`
	RunID       string `json:"run_id"`
	RunGroupID  string `json:"run_group_id"`
	GroupClass  string `json:"group_class"`
	State       string `json:"state"`
	Metric      string `json:"metric"`
	MetricWidth string `json:"metric_width"`
	Color       string `json:"color"`
}

type ParallelAxisView struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	X      string   `json:"x"`
	Min    string   `json:"min,omitempty"`
	Max    string   `json:"max,omitempty"`
	Values []string `json:"values,omitempty"`
}

type ParallelRunSeries struct {
	RunID      string  `json:"run_id"`
	RunGroupID string  `json:"run_group_id"`
	GroupClass string  `json:"group_class"`
	Color      string  `json:"color"`
	Points     string  `json:"points,omitempty"`
	Metric     string  `json:"metric"`
	RawMetric  float64 `json:"raw_metric"`
}

type ParameterImportanceView struct {
	Name             string  `json:"name"`
	Importance       float64 `json:"importance"`
	ImportanceLabel  string  `json:"importance_label"`
	ImportanceWidth  string  `json:"importance_width"`
	Correlation      float64 `json:"correlation"`
	CorrelationLabel string  `json:"correlation_label"`
	CorrelationWidth string  `json:"correlation_width"`
}

type ArtifactView struct {
	ArtifactID           string            `json:"artifact_id"`
	RunID                string            `json:"run_id"`
	Type                 string            `json:"type"`
	URI                  string            `json:"uri"`
	Name                 string            `json:"name"`
	DurableRef           string            `json:"durable_ref,omitempty"`
	ContentType          string            `json:"content_type,omitempty"`
	Digest               string            `json:"digest,omitempty"`
	SizeBytes            string            `json:"size_bytes,omitempty"`
	Step                 string            `json:"step,omitempty"`
	Tags                 string            `json:"tags,omitempty"`
	Rank                 string            `json:"rank,omitempty"`
	CreatedAt            string            `json:"created_at"`
	Preview              string            `json:"preview,omitempty"`
	ExternalRef          string            `json:"external_ref,omitempty"`
	Caption              string            `json:"caption,omitempty"`
	Direction            string            `json:"direction,omitempty"`
	Alias                string            `json:"alias,omitempty"`
	SourceArtifactID     string            `json:"source_artifact_id,omitempty"`
	SourceRunID          string            `json:"source_run_id,omitempty"`
	SourceDatasetName    string            `json:"source_dataset_name,omitempty"`
	SourceDatasetVersion string            `json:"source_dataset_version,omitempty"`
	SourceDatasetDigest  string            `json:"source_dataset_digest,omitempty"`
	Table                *TablePreviewView `json:"table,omitempty"`
}

type ConfigView struct {
	ConfigHash     string `json:"config_hash"`
	RunID          string `json:"run_id"`
	Format         string `json:"format"`
	URI            string `json:"uri"`
	NormalizedJSON string `json:"normalized_json,omitempty"`
	IndexedFields  string `json:"indexed_fields,omitempty"`
}

type EventView struct {
	EventID  string `json:"event_id"`
	RunID    string `json:"run_id"`
	Time     string `json:"time"`
	Type     string `json:"type"`
	Source   string `json:"source"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Payload  string `json:"payload,omitempty"`
}

type ObservationView struct {
	ObservationID  string `json:"observation_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Author         string `json:"author"`
	Source         string `json:"source"`
	Type           string `json:"type"`
	ScopeType      string `json:"scope_type"`
	ScopeID        string `json:"scope_id"`
	Text           string `json:"text"`
	Evidence       string `json:"evidence,omitempty"`
	CreatedAt      string `json:"created_at"`
}

type ActionView struct {
	CopyCLI      string `json:"copy_cli"`
	OpenCLI      string `json:"open_cli,omitempty"`
	CopySQL      string `json:"copy_sql"`
	ExportPacket string `json:"export_packet"`
	ObserveCLI   string `json:"observe_cli"`
	NextCommand  string `json:"next_command,omitempty"`
	StorePath    string `json:"store_path"`
}

type metricPoint struct {
	RunID            string
	RunGroupID       string
	MetricName       string
	Card             string
	Step             int64
	Value            float64
	Unit             string
	Source           string
	SourcePointCount int
	Milestone        bool
}

func BuildSnapshot(ctx context.Context, store *expstore.Store, opts Options) (Snapshot, error) {
	return NewLocalSource(store).BuildSnapshot(ctx, opts)
}

func ParseSnapshotMode(value string) (SnapshotMode, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "full":
		return SnapshotModeFull, nil
	case string(SnapshotModeSummary):
		return SnapshotModeSummary, nil
	case string(SnapshotModeMetric):
		return SnapshotModeMetric, nil
	default:
		return SnapshotModeFull, fmt.Errorf("unsupported Stellar snapshot mode %q", value)
	}
}

func buildMergedSnapshot(ctx context.Context, store *expstore.Store, kusto KustoSource, opts Options) (Snapshot, error) {
	opts.Target = strings.TrimSpace(opts.Target)
	var err error
	kusto, err = kusto.scopedToWorkspace(opts.Workspace)
	if err != nil {
		return Snapshot{}, err
	}
	if opts.Target == "" {
		return Snapshot{}, fmt.Errorf("dashboard target is required")
	}
	status, err := store.Status(ctx, opts.Target)
	if err != nil {
		return Snapshot{}, err
	}
	experiment, err := loadExperiment(ctx, store, status)
	if err != nil {
		return Snapshot{}, err
	}
	groups, err := loadRunGroups(ctx, store, status, experiment)
	if err != nil {
		return Snapshot{}, err
	}
	runs, runsTruncated, err := loadRuns(ctx, store, status, experiment, opts.MaxRuns, opts.Workspace, "")
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(opts.Workspace) != "" && len(runs) == 0 {
		return kusto.BuildSnapshot(ctx, opts)
	}
	localRunIDs := runIDSet(runs)
	// scopeSnapshotStatus rewrites status.Runs to the scoped count, so the
	// denominator has to be read before it runs or the warning degenerates to
	// "N of N".
	totalRuns := status.Runs
	groups = scopeSnapshotStatus(&status, groups, runs, opts.Workspace)
	warnings := []string{}
	if runsTruncated {
		warnings = append(warnings, workspaceTruncationWarning(opts.Workspace, len(runs), totalRuns))
	}
	runIDs := runIDs(runs)
	artifacts, err := loadArtifacts(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	configs, err := loadConfigs(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	events, err := loadEvents(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	contexts, err := loadRunContexts(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	var metricFiles []map[string]any
	if strings.TrimSpace(opts.Workspace) == "" || len(runIDs) > 0 {
		metricFiles, err = loadMetricFiles(ctx, store, status, experiment, runIDs)
		if err != nil {
			return Snapshot{}, err
		}
	}
	points, metricWarnings := readMetricPoints(store, metricFiles, opts.MaxMetricRows)
	warnings = append(warnings, metricWarnings...)
	kustoRows, err := kusto.loadRows(ctx, opts)
	if err != nil {
		return Snapshot{}, err
	}
	kustoRows = filterKustoRowsByWorkspace(kustoRows, kusto.WorkspaceID)
	if len(kustoRows) > 0 {
		var remoteWarnings []string
		groups, runs, points, remoteWarnings = mergeKustoRows(opts, status, kustoRows, groups, runs, points)
		warnings = append(warnings, remoteWarnings...)
		status.Runs = len(runs)
		status.RunGroups = len(groups)
		if len(remoteWarnings) > 0 {
			status.MetricFiles++
		}
		status.StateCounts = stateCountsWithKusto(status.StateCounts, runs)
	}
	if strings.TrimSpace(opts.Workspace) != "" && len(runs) == 0 {
		return Snapshot{}, expstore.ErrNotFound
	}
	runs, metadataWarnings, err := attachRunSearchMetadata(ctx, store, runs, localRunIDs)
	if err != nil {
		return Snapshot{}, err
	}
	groups = scopeSnapshotStatus(&status, groups, runs, opts.Workspace)
	warnings = append(warnings, metadataWarnings...)
	runColors := runColorMapForRuns(runs)
	applyRunColors(runs, runColors)
	groupClasses := groupClassMap(groups)
	metricOptions := buildMetricOptionsFromPoints(points, "")
	if opts.Mode == SnapshotModeSummary {
		actions := buildActions(store.Root, opts.Target, status.TargetType, experiment, "")
		summary := buildExperimentSummary(opts.Target, status.TargetType, experiment, groups, runs, nil, ChartView{}, nil, "", actions.NextCommand)
		snapshot := Snapshot{
			SchemaVersion: SchemaVersion,
			PayloadMode:   string(SnapshotModeSummary),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     store.Root,
			Target:        opts.Target,
			TargetType:    status.TargetType,
			Manifest:      store.Manifest(),
			Status:        status,
			Summary:       summary,
			Experiment:    experiment,
			RunGroups:     groups,
			Runs:          runs,
			MetricOptions: metricOptions,
			Actions:       actions,
			SeedCoverage:  summary.SeedCoverage,
			Warnings:      warnings,
		}
		if len(points) == 0 {
			snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
		}
		return snapshot, nil
	}
	if opts.Mode == SnapshotModeMetric {
		metric := requestedMetric(points, opts.Metric)
		metricPoints := filterMetricPoints(points, metric)
		cards := summarizeCards(metricPoints, groupClasses)
		chart := buildChartWithRunColors(metricPoints, metric, groupClasses, runColors)
		metricOptions = markMetricOptionSelected(metricOptions, chart.MetricName)
		sweep := buildSweepWithRunColors(metricPoints, chart.MetricName, runs, configs, groupClasses, runColors)
		decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
		bestGroup := decision.BestGroupID
		actions := buildActions(store.Root, opts.Target, status.TargetType, experiment, actionMetric(opts.Metric, chart.MetricName, decision.MetricName))
		summary := buildExperimentSummary(opts.Target, status.TargetType, experiment, groups, runs, decision.Cards, decisionChart(chart, decision.MetricName), nil, bestGroup, defaultNextCommand(store.Root, opts.Target, decision.MetricName))
		if summary.NextCommand != "" {
			actions.NextCommand = summary.NextCommand
		}
		compare := buildCompareInsights(decision.Points, decision.MetricName, runs, contexts, configs, events, bestGroup)
		snapshot := Snapshot{
			SchemaVersion: SchemaVersion,
			PayloadMode:   string(SnapshotModeMetric),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     store.Root,
			Target:        opts.Target,
			TargetType:    status.TargetType,
			Manifest:      store.Manifest(),
			Status:        status,
			Summary:       summary,
			Experiment:    experiment,
			RunGroups:     groups,
			Runs:          runs,
			Cards:         cards,
			Chart:         chart,
			MetricOptions: metricOptions,
			Sweep:         sweep,
			Compare:       compare,
			Actions:       actions,
			BestGroupID:   bestGroup,
			SeedCoverage:  summary.SeedCoverage,
			Warnings:      warnings,
		}
		if len(points) == 0 {
			snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
		}
		return snapshot, nil
	}
	observationExperiment, observationGroups := workspaceObservationScope(opts.Workspace, experiment, groups)
	observations, err := loadObservations(ctx, store, observationExperiment, observationGroups, runs, artifacts, events, metricNames(points))
	if err != nil {
		return Snapshot{}, err
	}
	cards := summarizeCards(points, groupClasses)
	chart := buildChartWithRunColors(points, opts.Metric, groupClasses, runColors)
	metricOptions = buildMetricOptions(cards, chart.MetricName)
	sweep := buildSweepWithRunColors(points, chart.MetricName, runs, configs, groupClasses, runColors)
	decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
	bestGroup := decision.BestGroupID
	actions := buildActions(store.Root, opts.Target, status.TargetType, experiment, actionMetric(opts.Metric, chart.MetricName, decision.MetricName))
	summary := buildExperimentSummary(opts.Target, status.TargetType, experiment, groups, runs, decision.Cards, decisionChart(chart, decision.MetricName), observations, bestGroup, defaultNextCommand(store.Root, opts.Target, decision.MetricName))
	if summary.NextCommand != "" {
		actions.NextCommand = summary.NextCommand
	}
	compare := buildCompareInsights(decision.Points, decision.MetricName, runs, contexts, configs, events, bestGroup)
	runs = attachRunDetails(store.Root, runs, contexts, configs, artifacts, events, observations)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     store.Root,
		Target:        opts.Target,
		TargetType:    status.TargetType,
		Manifest:      store.Manifest(),
		Status:        status,
		Summary:       summary,
		Experiment:    experiment,
		RunGroups:     groups,
		Runs:          runs,
		Cards:         cards,
		Chart:         chart,
		MetricOptions: metricOptions,
		Sweep:         sweep,
		Compare:       compare,
		Artifacts:     artifacts,
		Events:        events,
		Observations:  observations,
		Actions:       actions,
		BestGroupID:   bestGroup,
		SeedCoverage:  summary.SeedCoverage,
		Warnings:      warnings,
	}
	if len(points) == 0 {
		snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
	}
	return snapshot, nil
}

func buildLocalSnapshot(ctx context.Context, store *expstore.Store, opts Options) (Snapshot, error) {
	opts.Target = strings.TrimSpace(opts.Target)
	if opts.Target == "" {
		return Snapshot{}, fmt.Errorf("dashboard target is required")
	}
	status, err := store.Status(ctx, opts.Target)
	if err != nil {
		return Snapshot{}, err
	}
	experiment, err := loadExperiment(ctx, store, status)
	if err != nil {
		return Snapshot{}, err
	}
	groups, err := loadRunGroups(ctx, store, status, experiment)
	if err != nil {
		return Snapshot{}, err
	}
	runs, runsTruncated, err := loadRuns(ctx, store, status, experiment, opts.MaxRuns, opts.Workspace, "")
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(opts.Workspace) != "" && len(runs) == 0 {
		return Snapshot{}, expstore.ErrNotFound
	}
	// scopeSnapshotStatus rewrites status.Runs to the scoped count, so the
	// denominator has to be read before it runs or the warning degenerates to
	// "N of N".
	totalRuns := status.Runs
	groups = scopeSnapshotStatus(&status, groups, runs, opts.Workspace)
	warnings := []string{}
	if runsTruncated {
		warnings = append(warnings, workspaceTruncationWarning(opts.Workspace, len(runs), totalRuns))
	}
	groupClasses := groupClassMap(groups)
	runIDs := runIDs(runs)
	runColors := runColorMapForRuns(runs)
	applyRunColors(runs, runColors)
	metricFiles, err := loadMetricFiles(ctx, store, status, experiment, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	points, metricWarnings := readMetricPoints(store, metricFiles, opts.MaxMetricRows)
	warnings = append(warnings, metricWarnings...)
	runs, metadataWarnings, err := attachRunSearchMetadata(ctx, store, runs, nil)
	if err != nil {
		return Snapshot{}, err
	}
	groups = scopeSnapshotStatus(&status, groups, runs, opts.Workspace)
	warnings = append(warnings, metadataWarnings...)
	metricOptions := buildMetricOptionsFromPoints(points, "")
	if opts.Mode == SnapshotModeSummary {
		actions := buildActions(store.Root, opts.Target, status.TargetType, experiment, "")
		summary := buildExperimentSummary(opts.Target, status.TargetType, experiment, groups, runs, nil, ChartView{}, nil, "", actions.NextCommand)
		snapshot := Snapshot{
			SchemaVersion: SchemaVersion,
			PayloadMode:   string(SnapshotModeSummary),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     store.Root,
			Target:        opts.Target,
			TargetType:    status.TargetType,
			Manifest:      store.Manifest(),
			Status:        status,
			Summary:       summary,
			Experiment:    experiment,
			RunGroups:     groups,
			Runs:          runs,
			MetricOptions: metricOptions,
			Actions:       actions,
			SeedCoverage:  summary.SeedCoverage,
			Warnings:      warnings,
		}
		if len(points) == 0 {
			snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
		}
		return snapshot, nil
	}
	if opts.Mode == SnapshotModeMetric {
		metric := requestedMetric(points, opts.Metric)
		metricPoints := filterMetricPoints(points, metric)
		cards := summarizeCards(metricPoints, groupClasses)
		chart := buildChartWithRunColors(metricPoints, metric, groupClasses, runColors)
		metricOptions = markMetricOptionSelected(metricOptions, chart.MetricName)
		sweep := buildSweepWithRunColors(metricPoints, chart.MetricName, runs, nil, groupClasses, runColors)
		decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
		bestGroup := decision.BestGroupID
		actions := buildActions(store.Root, opts.Target, status.TargetType, experiment, actionMetric(opts.Metric, chart.MetricName, decision.MetricName))
		summary := buildExperimentSummary(opts.Target, status.TargetType, experiment, groups, runs, decision.Cards, decisionChart(chart, decision.MetricName), nil, bestGroup, defaultNextCommand(store.Root, opts.Target, decision.MetricName))
		if summary.NextCommand != "" {
			actions.NextCommand = summary.NextCommand
		}
		snapshot := Snapshot{
			SchemaVersion: SchemaVersion,
			PayloadMode:   string(SnapshotModeMetric),
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			StorePath:     store.Root,
			Target:        opts.Target,
			TargetType:    status.TargetType,
			Manifest:      store.Manifest(),
			Status:        status,
			Summary:       summary,
			Experiment:    experiment,
			RunGroups:     groups,
			Runs:          runs,
			Cards:         cards,
			Chart:         chart,
			MetricOptions: metricOptions,
			Sweep:         sweep,
			Actions:       actions,
			BestGroupID:   bestGroup,
			SeedCoverage:  summary.SeedCoverage,
			Warnings:      warnings,
		}
		if len(points) == 0 {
			snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
		}
		return snapshot, nil
	}
	artifacts, err := loadArtifacts(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	configs, err := loadConfigs(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	events, err := loadEvents(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	contexts, err := loadRunContexts(ctx, store, runIDs)
	if err != nil {
		return Snapshot{}, err
	}
	observationExperiment, observationGroups := workspaceObservationScope(opts.Workspace, experiment, groups)
	observations, err := loadObservations(ctx, store, observationExperiment, observationGroups, runs, artifacts, events, metricNames(points))
	if err != nil {
		return Snapshot{}, err
	}
	cards := summarizeCards(points, groupClasses)
	chart := buildChartWithRunColors(points, opts.Metric, groupClasses, runColors)
	metricOptions = buildMetricOptions(cards, chart.MetricName)
	sweep := buildSweepWithRunColors(points, chart.MetricName, runs, configs, groupClasses, runColors)
	decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
	bestGroup := decision.BestGroupID
	actions := buildActions(store.Root, opts.Target, status.TargetType, experiment, actionMetric(opts.Metric, chart.MetricName, decision.MetricName))
	summary := buildExperimentSummary(opts.Target, status.TargetType, experiment, groups, runs, decision.Cards, decisionChart(chart, decision.MetricName), observations, bestGroup, defaultNextCommand(store.Root, opts.Target, decision.MetricName))
	if summary.NextCommand != "" {
		actions.NextCommand = summary.NextCommand
	}
	compare := buildCompareInsights(decision.Points, decision.MetricName, runs, contexts, configs, events, bestGroup)
	runs = attachRunDetails(store.Root, runs, contexts, configs, artifacts, events, observations)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		StorePath:     store.Root,
		Target:        opts.Target,
		TargetType:    status.TargetType,
		Manifest:      store.Manifest(),
		Status:        status,
		Summary:       summary,
		Experiment:    experiment,
		RunGroups:     groups,
		Runs:          runs,
		Cards:         cards,
		Chart:         chart,
		MetricOptions: metricOptions,
		Sweep:         sweep,
		Compare:       compare,
		Artifacts:     artifacts,
		Events:        events,
		Observations:  observations,
		Actions:       actions,
		BestGroupID:   bestGroup,
		SeedCoverage:  summary.SeedCoverage,
		Warnings:      warnings,
	}
	if len(points) == 0 {
		snapshot.Warnings = append(snapshot.Warnings, "no scalar metrics were found for this target")
	}
	return snapshot, nil
}

func RenderHTML(ctx context.Context, store *expstore.Store, opts Options) ([]byte, error) {
	snapshot, err := BuildSnapshot(ctx, store, opts)
	if err != nil {
		return nil, err
	}
	return RenderSnapshotHTML(snapshot)
}

func RenderSourceHTML(ctx context.Context, source Source, opts Options) ([]byte, error) {
	if source == nil {
		return nil, fmt.Errorf("Stellar source is required")
	}
	snapshot, err := source.BuildSnapshot(ctx, opts)
	if err != nil {
		return nil, err
	}
	return RenderSnapshotHTML(snapshot)
}

func RenderSnapshotHTML(snapshot Snapshot) ([]byte, error) {
	tmpl, err := template.New("cockpit").Parse(htmlTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, snapshot); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// loadExperiment resolves the active experiment. Under the
// workspace -> project -> experiment -> run model, run groups are arms inside
// an experiment rather than experiments themselves, so a run or group target
// resolves upward through the run_experiments join.
func loadExperiment(ctx context.Context, store *expstore.Store, status expstore.Status) (*expstore.ExperimentRecord, error) {
	if status.Experiment != nil {
		return experimentFromRecord(*status.Experiment), nil
	}
	scope := ""
	switch {
	case status.Run != nil:
		scope = "re.run_id = " + sqlQuote(stringValue(status.Run, "run_id"))
	case status.RunGroup != nil:
		scope = "r.run_group_id = " + sqlQuote(status.RunGroup.RunGroupID)
	}
	if scope == "" {
		return nil, nil
	}
	rows, err := queryRows(ctx, store, `
SELECT e.experiment_id, e.project, e.name, coalesce(e.description, '') AS description,
       e.source, e.created_at, e.updated_at
FROM experiments e
WHERE e.experiment_id IN (
  SELECT re.experiment_id FROM runs re
  JOIN runs r ON r.run_id = re.run_id
  WHERE `+scope+`
)
ORDER BY e.created_at, e.experiment_id
LIMIT 1`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	q := experimentFromRow(rows[0])
	return &q, nil
}

func loadRunGroups(ctx context.Context, store *expstore.Store, status expstore.Status, experiment *expstore.ExperimentRecord) ([]RunGroupView, error) {
	query := "SELECT run_group_id, project, name, created_at, updated_at FROM run_groups"
	// An experiment's arms are the groups its runs sit in. run_groups carries no
	// experiment_id of its own: the same arm label may be reused by more than
	// one experiment, so ownership lives on the run.
	if status.Experiment != nil {
		id := sqlQuote(status.Experiment.ExperimentID)
		query += " WHERE run_group_id IN (SELECT DISTINCT r.run_group_id FROM runs r WHERE r.experiment_id = " + id + ")"
	} else if experiment != nil {
		id := sqlQuote(experiment.ExperimentID)
		query += " WHERE run_group_id IN (SELECT DISTINCT r.run_group_id FROM runs r WHERE r.experiment_id = " + id + ")"
	} else if status.RunGroup != nil {
		query += " WHERE run_group_id = " + sqlQuote(status.RunGroup.RunGroupID)
	} else if status.Run != nil {
		query += " WHERE run_group_id = " + sqlQuote(stringValue(status.Run, "run_group_id"))
	}
	query += " ORDER BY run_group_id"
	rows, err := queryRows(ctx, store, query)
	if err != nil {
		return nil, err
	}
	out := make([]RunGroupView, 0, len(rows))
	for _, row := range rows {
		out = append(out, RunGroupView{
			RunGroupID: stringValue(row, "run_group_id"),
			Project:    stringValue(row, "project"),
			Name:       stringValue(row, "name"),
			CreatedAt:  stringValue(row, "created_at"),
			UpdatedAt:  stringValue(row, "updated_at"),
		})
	}
	for i := range out {
		out[i].GroupClass = cssClass("group", out[i].RunGroupID)
	}
	return out, nil
}

func loadRuns(ctx context.Context, store *expstore.Store, status expstore.Status, experiment *expstore.ExperimentRecord, maxRuns int, workspace, exactRunID string) ([]RunView, bool, error) {
	query := `SELECT run_id, project, run_group_id, state, owner, created_at, started_at,
       completed_at, config_hash, code_sha, image_digest, tau_command, result_uri
FROM runs`
	args := []any{}
	switch {
	case status.Experiment != nil:
		query += " WHERE run_id IN (SELECT run_id FROM runs WHERE experiment_id = ?)"
		args = append(args, status.Experiment.ExperimentID)
	case experiment != nil:
		query += " WHERE run_id IN (SELECT run_id FROM runs WHERE experiment_id = ?)"
		args = append(args, experiment.ExperimentID)
	case status.RunGroup != nil:
		query += " WHERE run_group_id = ?"
		args = append(args, status.RunGroup.RunGroupID)
	case status.Run != nil:
		query += " WHERE run_id = ?"
		args = append(args, stringValue(status.Run, "run_id"))
	}
	if workspace = strings.TrimSpace(workspace); workspace != "" {
		joiner := " WHERE "
		if strings.Contains(query, " WHERE ") {
			joiner = " AND "
		}
		// Untagged runs belong to the workspace this server serves; Stellar is
		// single-workspace, so excluding them would hide data rather than deny
		// access. Placeholder order: tag key, workspace, tag key.
		query += joiner + `(EXISTS (
  SELECT 1 FROM tags workspace_tag
  WHERE workspace_tag.scope_type = 'run'
    AND workspace_tag.scope_id = runs.run_id
    AND workspace_tag.key = ?
    AND workspace_tag.value = ?
) OR NOT EXISTS (
  SELECT 1 FROM tags workspace_tag
  WHERE workspace_tag.scope_type = 'run'
    AND workspace_tag.scope_id = runs.run_id
    AND workspace_tag.key = ?
))`
		args = append(args, exptelemetry.TauWorkspaceTag, workspace, exptelemetry.TauWorkspaceTag)
	}
	if exactRunID = strings.TrimSpace(exactRunID); exactRunID != "" {
		joiner := " WHERE "
		if strings.Contains(query, " WHERE ") {
			joiner = " AND "
		}
		query += joiner + "runs.run_id = ?"
		args = append(args, exactRunID)
	}
	query += " ORDER BY run_group_id, run_id"
	if maxRuns > 0 {
		query += " LIMIT " + strconv.Itoa(maxRuns+1)
	}
	rows, err := queryRowsArgs(ctx, store, query, args...)
	if err != nil {
		return nil, false, err
	}
	truncated := maxRuns > 0 && len(rows) > maxRuns
	if truncated {
		rows = rows[:maxRuns]
	}
	out := make([]RunView, 0, len(rows))
	for _, row := range rows {
		out = append(out, RunView{
			RunID:      stringValue(row, "run_id"),
			Source:     "local",
			Project:    stringValue(row, "project"),
			RunGroupID: stringValue(row, "run_group_id"),
			State:      stringValue(row, "state"),
			Owner:      stringValue(row, "owner"),
			CreatedAt:  stringValue(row, "created_at"),
			// Metric summaries are loaded later; use created_at until run search metadata can enrich this.
			UpdatedAt:   stringValue(row, "created_at"),
			StartedAt:   stringValue(row, "started_at"),
			CompletedAt: stringValue(row, "completed_at"),
			ConfigHash:  stringValue(row, "config_hash"),
			CodeSHA:     stringValue(row, "code_sha"),
			ImageDigest: stringValue(row, "image_digest"),
			TauCommand:  stringValue(row, "tau_command"),
			ResultURI:   stringValue(row, "result_uri"),
		})
	}
	return out, truncated, nil
}

func scopeSnapshotStatus(status *expstore.Status, groups []RunGroupView, runs []RunView, workspace string) []RunGroupView {
	if strings.TrimSpace(workspace) == "" {
		return groups
	}
	groupIDs := map[string]bool{}
	lifecycleCounts := map[string]int{}
	for _, run := range runs {
		groupIDs[run.RunGroupID] = true
		if run.LifecycleState != "" {
			lifecycleCounts[run.LifecycleState]++
		}
	}
	scopedGroups := make([]RunGroupView, 0, len(groupIDs))
	for _, group := range groups {
		if groupIDs[group.RunGroupID] {
			scopedGroups = append(scopedGroups, group)
		}
	}
	status.Runs = len(runs)
	status.RunGroups = len(scopedGroups)
	status.StateCounts = stateCountsWithKusto(nil, runs)
	status.LifecycleCounts = lifecycleCounts
	return scopedGroups
}

// workspaceTruncationWarning reports how many runs a snapshot dropped.
//
// This used to report a vaguer "workspace runs truncated to N" whenever a
// workspace was set, because a workspace filter meant the total was computed
// over a wider set than was shown. Stellar now always has a workspace, so that
// branch would fire on every request and permanently hide the denominator.
func workspaceTruncationWarning(workspace string, returned, total int) string {
	return fmt.Sprintf("runs truncated to %d of %d matching runs", returned, total)
}

// workspaceObservationScope returns the experiment and groups whose
// observations a snapshot may show.
//
// This used to drop both whenever a workspace was set, because experiment- and
// group-scoped observations carry no workspace tag of their own. That guard is
// now counterproductive: a workspace is always set, so it would silently hide
// every observation. The experiment and groups passed in are already
// workspace-scoped by scopeSnapshotStatus, so their observations are in scope.
func workspaceObservationScope(workspace string, experiment *expstore.ExperimentRecord, groups []RunGroupView) (*expstore.ExperimentRecord, []RunGroupView) {
	return experiment, groups
}

func loadArtifacts(ctx context.Context, store *expstore.Store, runIDs []string) ([]ArtifactView, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := queryRows(ctx, store, `SELECT artifact_id, run_id, type, uri, name, durable_ref, content_type,
       digest, size_bytes, step, tags, rank, created_at, preview, external_ref,
       caption, direction, alias, source_artifact_id, source_run_id,
       source_dataset_name, source_dataset_version, source_dataset_digest
FROM artifacts WHERE run_id IN (`+sqlIn(runIDs)+`) ORDER BY created_at, artifact_id`)
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactView, 0, len(rows))
	for _, row := range rows {
		artifact := ArtifactView{
			ArtifactID:           stringValue(row, "artifact_id"),
			RunID:                stringValue(row, "run_id"),
			Type:                 stringValue(row, "type"),
			URI:                  stringValue(row, "uri"),
			Name:                 stringValue(row, "name"),
			DurableRef:           stringValue(row, "durable_ref"),
			ContentType:          stringValue(row, "content_type"),
			Digest:               stringValue(row, "digest"),
			SizeBytes:            formatAny(row["size_bytes"]),
			Step:                 formatAny(row["step"]),
			Tags:                 stringValue(row, "tags"),
			Rank:                 formatAny(row["rank"]),
			CreatedAt:            stringValue(row, "created_at"),
			Preview:              stringValue(row, "preview"),
			ExternalRef:          stringValue(row, "external_ref"),
			Caption:              stringValue(row, "caption"),
			Direction:            stringValue(row, "direction"),
			Alias:                stringValue(row, "alias"),
			SourceArtifactID:     stringValue(row, "source_artifact_id"),
			SourceRunID:          stringValue(row, "source_run_id"),
			SourceDatasetName:    stringValue(row, "source_dataset_name"),
			SourceDatasetVersion: stringValue(row, "source_dataset_version"),
			SourceDatasetDigest:  stringValue(row, "source_dataset_digest"),
		}
		artifact.Table = loadTablePreview(store.Root, artifact)
		out = append(out, artifact)
	}
	return out, nil
}

func loadTablePreview(storeRoot string, artifact ArtifactView) *TablePreviewView {
	if strings.ToLower(artifact.Type) != "table" || artifact.URI == "" {
		return nil
	}
	path := filepath.Join(storeRoot, filepath.FromSlash(artifact.URI))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var payload struct {
		Columns []string         `json:"columns"`
		Rows    []map[string]any `json:"rows"`
		Caption string           `json:"caption"`
		Step    any              `json:"step"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if len(payload.Columns) == 0 && len(payload.Rows) == 0 {
		return nil
	}
	rows := payload.Rows
	if len(rows) > 25 {
		rows = rows[:25]
	}
	caption := firstNonEmptyString(payload.Caption, artifact.Caption)
	return &TablePreviewView{
		Columns: payload.Columns,
		Rows:    rows,
		Caption: caption,
		Step:    formatTableStep(payload.Step),
	}
}

func formatTableStep(value any) string {
	switch v := value.(type) {
	case float64:
		if math.Trunc(v) == v {
			return strconv.FormatInt(int64(v), 10)
		}
		return formatFloat(v)
	default:
		return formatAny(value)
	}
}

func loadConfigs(ctx context.Context, store *expstore.Store, runIDs []string) ([]ConfigView, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := queryRows(ctx, store, `SELECT config_hash, run_id, format, uri, normalized_json, indexed_fields
FROM configs WHERE run_id IN (`+sqlIn(runIDs)+`) ORDER BY run_id, config_hash`)
	if err != nil {
		return nil, err
	}
	out := make([]ConfigView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ConfigView{
			ConfigHash:     stringValue(row, "config_hash"),
			RunID:          stringValue(row, "run_id"),
			Format:         stringValue(row, "format"),
			URI:            stringValue(row, "uri"),
			NormalizedJSON: stringValue(row, "normalized_json"),
			IndexedFields:  stringValue(row, "indexed_fields"),
		})
	}
	return out, nil
}

func loadEvents(ctx context.Context, store *expstore.Store, runIDs []string) ([]EventView, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := queryRows(ctx, store, `SELECT event_id, run_id, time, type, source, severity, message, payload
FROM events WHERE run_id IN (`+sqlIn(runIDs)+`) ORDER BY time, event_id`)
	if err != nil {
		return nil, err
	}
	out := make([]EventView, 0, len(rows))
	for _, row := range rows {
		out = append(out, EventView{
			EventID:  stringValue(row, "event_id"),
			RunID:    stringValue(row, "run_id"),
			Time:     stringValue(row, "time"),
			Type:     stringValue(row, "type"),
			Source:   stringValue(row, "source"),
			Severity: stringValue(row, "severity"),
			Message:  stringValue(row, "message"),
			Payload:  stringValue(row, "payload"),
		})
	}
	return out, nil
}

func loadObservations(ctx context.Context, store *expstore.Store, experiment *expstore.ExperimentRecord, groups []RunGroupView, runs []RunView, artifacts []ArtifactView, events []EventView, metricNames []string) ([]ObservationView, error) {
	clauses := []string{}
	if experiment != nil {
		clauses = append(clauses, "(scope_type = 'experiment' AND scope_id = "+sqlQuote(experiment.ExperimentID)+")")
	}
	groupIDs := make([]string, 0, len(groups))
	for _, group := range groups {
		groupIDs = append(groupIDs, group.RunGroupID)
	}
	if len(groupIDs) > 0 {
		clauses = append(clauses, "(scope_type = 'run_group' AND scope_id IN ("+sqlIn(groupIDs)+"))")
	}
	runIDs := runIDs(runs)
	if len(runIDs) > 0 {
		clauses = append(clauses, "(scope_type = 'run' AND scope_id IN ("+sqlIn(runIDs)+"))")
	}
	artifactIDs := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifactIDs = append(artifactIDs, artifact.ArtifactID)
	}
	if len(artifactIDs) > 0 {
		clauses = append(clauses, "(scope_type = 'artifact' AND scope_id IN ("+sqlIn(artifactIDs)+"))")
	}
	eventIDs := make([]string, 0, len(events))
	for _, event := range events {
		eventIDs = append(eventIDs, event.EventID)
	}
	if len(eventIDs) > 0 {
		clauses = append(clauses, "(scope_type = 'event' AND scope_id IN ("+sqlIn(eventIDs)+"))")
	}
	if len(metricNames) > 0 {
		clauses = append(clauses, "(scope_type = 'metric' AND scope_id IN ("+sqlIn(metricNames)+"))")
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	rows, err := queryRows(ctx, store, `SELECT observation_id, idempotency_key, author, source, type, scope_type, scope_id, text, evidence, created_at
FROM observations WHERE `+strings.Join(clauses, " OR ")+` ORDER BY created_at, observation_id`)
	if err != nil {
		return nil, err
	}
	out := make([]ObservationView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ObservationView{
			ObservationID:  stringValue(row, "observation_id"),
			IdempotencyKey: stringValue(row, "idempotency_key"),
			Author:         stringValue(row, "author"),
			Source:         stringValue(row, "source"),
			Type:           stringValue(row, "type"),
			ScopeType:      stringValue(row, "scope_type"),
			ScopeID:        stringValue(row, "scope_id"),
			Text:           stringValue(row, "text"),
			Evidence:       stringValue(row, "evidence"),
			CreatedAt:      stringValue(row, "created_at"),
		})
	}
	return out, nil
}

func loadRunContexts(ctx context.Context, store *expstore.Store, runIDs []string) (map[string]map[string]any, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := queryRows(ctx, store, `SELECT run_id, cluster, namespace, team, profile, lane, local_queue, cluster_queue,
       kueue_workload, pod_uid, ray_job, resource_claims, gpu_class, gpu_count, node_names, mounts,
       queue_wait_seconds, gpu_hours, estimated_cost, runtime, dependencies, log_uri
FROM run_context WHERE run_id IN (`+sqlIn(runIDs)+`)`)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		out[stringValue(row, "run_id")] = row
	}
	return out, nil
}

func loadMetricFiles(ctx context.Context, store *expstore.Store, status expstore.Status, experiment *expstore.ExperimentRecord, runIDs []string) ([]map[string]any, error) {
	query := `SELECT file_id, path, run_id, run_group_id, row_count, min_step, max_step FROM metric_files`
	args := []any{}
	switch {
	case len(runIDs) > 0:
		query += " WHERE run_id IN (" + sqlPlaceholders(len(runIDs)) + ")"
		for _, runID := range runIDs {
			args = append(args, runID)
		}
	case experiment != nil:
		query += " WHERE run_id IN (SELECT run_id FROM runs WHERE experiment_id = ?)"
		args = append(args, experiment.ExperimentID)
	case status.RunGroup != nil:
		query += " WHERE run_group_id = ?"
		args = append(args, status.RunGroup.RunGroupID)
	}
	query += " ORDER BY created_at, file_id"
	return queryRowsArgs(ctx, store, query, args...)
}

func readMetricPoints(store *expstore.Store, metricFiles []map[string]any, maxRows int) ([]metricPoint, []string) {
	return readMetricPointsFiltered(store, metricFiles, maxRows, metricPointFilter{})
}

type metricPointFilter struct {
	MetricName string
	RunID      string
	StartStep  *int64
	EndStep    *int64
}

func readMetricPointsFiltered(store *expstore.Store, metricFiles []map[string]any, maxRows int, filter metricPointFilter) ([]metricPoint, []string) {
	points := []metricPoint{}
	warnings := []string{}
	loadedRows := 0
	skippedFiles := 0
	skippedRows := int64(0)
	for _, file := range metricFiles {
		rel := stringValue(file, "path")
		if rel == "" {
			continue
		}
		if filter.RunID != "" && stringValue(file, "run_id") != filter.RunID {
			continue
		}
		if filter.StartStep != nil {
			fileMax := int64Value(file, "max_step")
			if fileMax != 0 && fileMax < *filter.StartStep {
				continue
			}
		}
		if filter.EndStep != nil {
			fileMin := int64Value(file, "min_step")
			if fileMin != 0 && fileMin > *filter.EndStep {
				continue
			}
		}
		rowCount := intValue(file, "row_count")
		if maxRows > 0 {
			switch {
			case rowCount <= 0:
				skippedFiles++
				continue
			case loadedRows+rowCount > maxRows:
				skippedFiles++
				skippedRows += int64(rowCount)
				continue
			}
		}
		path := filepath.Join(store.Root, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			warnings = append(warnings, fmt.Sprintf("metric file %s was not readable: %v", rel, err))
			continue
		}
		rows, err := parquet.ReadFile[expstore.MetricRow](path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("metric file %s could not be parsed: %v", rel, err))
			continue
		}
		for _, row := range rows {
			if filter.MetricName != "" && row.MetricName != filter.MetricName {
				continue
			}
			if filter.RunID != "" && row.RunID != filter.RunID {
				continue
			}
			if maxRows > 0 && loadedRows >= maxRows {
				skippedFiles++
				break
			}
			step := int64(0)
			if row.Step != nil {
				step = *row.Step
			}
			if filter.StartStep != nil && step < *filter.StartStep {
				continue
			}
			if filter.EndStep != nil && step > *filter.EndStep {
				continue
			}
			unit := ""
			if row.Unit != nil {
				unit = *row.Unit
			}
			points = append(points, metricPoint{
				RunID:      row.RunID,
				RunGroupID: row.RunGroupID,
				MetricName: row.MetricName,
				Card:       metricCard(row),
				Step:       step,
				Value:      row.Value,
				Unit:       unit,
				Source:     "local",
			})
			loadedRows++
		}
	}
	if skippedFiles > 0 {
		detail := fmt.Sprintf("metric points truncated: skipped %d metric files because max_metric_rows=%d", skippedFiles, maxRows)
		if skippedRows > 0 {
			detail = fmt.Sprintf("metric points truncated: skipped %d metric files (%d declared rows) because max_metric_rows=%d", skippedFiles, skippedRows, maxRows)
		}
		warnings = append(warnings, detail)
	}
	return points, warnings
}

func summarizeCards(points []metricPoint, groupClasses map[string]string) []CardView {
	cardMetrics := map[string]map[string][]metricPoint{}
	for _, point := range points {
		if cardMetrics[point.Card] == nil {
			cardMetrics[point.Card] = map[string][]metricPoint{}
		}
		cardMetrics[point.Card][point.MetricName] = append(cardMetrics[point.Card][point.MetricName], point)
	}
	order := []string{"Outcome", "Optimization", "Throughput", "Systems", "Checkpoint", "Model diagnostics", "Behavior", "World model", "Other metrics"}
	for card := range cardMetrics {
		if !contains(order, card) {
			order = append(order, card)
		}
	}
	out := []CardView{}
	for _, cardName := range order {
		metrics := cardMetrics[cardName]
		if len(metrics) == 0 {
			continue
		}
		names := make([]string, 0, len(metrics))
		for name := range metrics {
			names = append(names, name)
		}
		sort.Strings(names)
		card := CardView{Name: cardName}
		for _, name := range names {
			card.Metrics = append(card.Metrics, summarizeMetric(name, metrics[name], groupClasses))
		}
		out = append(out, card)
	}
	return out
}

func buildMetricOptions(cards []CardView, selected string) []MetricOptionView {
	out := []MetricOptionView{}
	for _, card := range cards {
		for _, metric := range card.Metrics {
			out = append(out, MetricOptionView{
				Name:     metric.Name,
				Card:     card.Name,
				Selected: metric.Name == selected,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Card == out[j].Card {
			return out[i].Name < out[j].Name
		}
		return out[i].Card < out[j].Card
	})
	return out
}

func buildMetricOptionsFromPoints(points []metricPoint, selected string) []MetricOptionView {
	cardByMetric := map[string]string{}
	for _, point := range points {
		name := strings.TrimSpace(point.MetricName)
		if name == "" {
			continue
		}
		card := strings.TrimSpace(point.Card)
		if card == "" {
			card = "Other metrics"
		}
		if _, exists := cardByMetric[name]; !exists {
			cardByMetric[name] = card
		}
	}
	out := make([]MetricOptionView, 0, len(cardByMetric))
	for name, card := range cardByMetric {
		out = append(out, MetricOptionView{
			Name:     name,
			Card:     card,
			Selected: name == selected,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Card == out[j].Card {
			return out[i].Name < out[j].Name
		}
		return out[i].Card < out[j].Card
	})
	return out
}

func markMetricOptionSelected(options []MetricOptionView, selected string) []MetricOptionView {
	out := append([]MetricOptionView(nil), options...)
	for i := range out {
		out[i].Selected = out[i].Name == selected
	}
	return out
}

func filterMetricPoints(points []metricPoint, metric string) []metricPoint {
	if metric == "" {
		return nil
	}
	out := make([]metricPoint, 0)
	for _, point := range points {
		if point.MetricName == metric {
			out = append(out, point)
		}
	}
	return out
}

func summarizeMetric(name string, points []metricPoint, groupClasses map[string]string) MetricView {
	unit := ""
	latestByRun := map[string]metricPoint{}
	for _, point := range points {
		if unit == "" {
			unit = point.Unit
		}
		prev, ok := latestByRun[point.RunID]
		if !ok || point.Step >= prev.Step {
			latestByRun[point.RunID] = point
		}
	}
	valuesByGroup := map[string][]metricPoint{}
	for _, point := range latestByRun {
		valuesByGroup[point.RunGroupID] = append(valuesByGroup[point.RunGroupID], point)
	}
	groupIDs := make([]string, 0, len(valuesByGroup))
	for groupID := range valuesByGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	out := MetricView{Name: name, Unit: unit}
	for _, groupID := range groupIDs {
		points := valuesByGroup[groupID]
		values := make([]float64, 0, len(points))
		latestStep := int64(0)
		for _, point := range points {
			values = append(values, point.Value)
			if point.Step > latestStep {
				latestStep = point.Step
			}
		}
		sort.Float64s(values)
		best := values[len(values)-1]
		out.Groups = append(out.Groups, MetricGroupView{
			RunGroupID: groupID,
			GroupClass: groupClasses[groupID],
			RunCount:   len(values),
			LatestStep: strconv.FormatInt(latestStep, 10),
			Min:        formatFloat(values[0]),
			P25:        formatFloat(percentile(values, 0.25)),
			Median:     formatFloat(percentile(values, 0.5)),
			P75:        formatFloat(percentile(values, 0.75)),
			Max:        formatFloat(values[len(values)-1]),
			Best:       formatFloat(best),
			BestValue:  best,
		})
	}
	return out
}

func buildChart(points []metricPoint, requested string, groupClasses map[string]string) ChartView {
	return buildChartWithRunColors(points, requested, groupClasses, nil)
}

func buildChartWithRunColors(points []metricPoint, requested string, groupClasses map[string]string, runColors map[string]string) ChartView {
	return buildChartWithRunColorsBudgetAndInterval(points, requested, groupClasses, runColors, chartMaxRenderedPoints, 0)
}

func buildChartWithRunColorsBudgetAndInterval(points []metricPoint, requested string, groupClasses map[string]string, runColors map[string]string, maxRenderedPoints int, stepInterval int) ChartView {
	if maxRenderedPoints <= 0 {
		maxRenderedPoints = chartMaxRenderedPoints
	}
	metric := requestedMetric(points, requested)
	if metric == "" {
		return ChartView{}
	}
	selected := []metricPoint{}
	milestoneSteps := validationMilestoneSteps(points, metric)
	for _, point := range points {
		if point.MetricName == metric {
			selected = append(selected, point)
		}
	}
	if len(selected) == 0 {
		return ChartView{}
	}
	xMin, xMax := selected[0].Step, selected[0].Step
	yMin, yMax := selected[0].Value, selected[0].Value
	byRun := map[string][]metricPoint{}
	for _, point := range selected {
		byRun[point.RunID] = append(byRun[point.RunID], point)
		if point.Step < xMin {
			xMin = point.Step
		}
		if point.Step > xMax {
			xMax = point.Step
		}
		if point.Value < yMin {
			yMin = point.Value
		}
		if point.Value > yMax {
			yMax = point.Value
		}
	}
	if xMin == xMax {
		xMin--
		xMax++
	}
	if yMin == yMax {
		yMin -= 1
		yMax += 1
	}
	runIDs := make([]string, 0, len(byRun))
	for runID := range byRun {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	series := make([]ChartSeries, 0, len(runIDs))
	if len(runColors) == 0 {
		runColors = runColorMap(points)
	}
	targetPerSeries := maxRenderedPoints
	smoothing := chartSmoothingFor(metric, byRun)
	for _, runID := range runIDs {
		runPoints := byRun[runID]
		sort.Slice(runPoints, func(i, j int) bool {
			return runPoints[i].Step < runPoints[j].Step
		})
		if len(runPoints) == 0 {
			continue
		}
		groupID := runPoints[0].RunGroupID
		source := runPoints[0].Source
		if source == "" {
			source = "local"
		}
		renderPoints := runPoints
		if stepInterval > 0 {
			renderPoints = sampleMetricPointsByStepInterval(runPoints, stepInterval)
		}
		runMilestones := milestoneSteps[runID]
		renderPoints = includeMilestonePoints(renderPoints, runPoints, runMilestones)
		renderPoints, sampling := sampleMetricPoints(renderPoints, targetPerSeries, runMilestones)
		sampling.ServerPreselectedPoints = len(runPoints)
		sourcePointCount := len(runPoints)
		if reportedSourceCount := maxMetricPointSourceCount(runPoints); reportedSourceCount > sourcePointCount {
			sourcePointCount = reportedSourceCount
		}
		sampling.SourcePoints = sourcePointCount
		sampling.Truncated = len(renderPoints) < sourcePointCount
		smoothedValues := []ChartPoint{}
		if smoothing != nil {
			smoothedValues = chartPoints(smoothedMetricPointsForRendered(runPoints, renderPoints, smoothing.Alpha))
		}

		color := runColors[runID]
		if color == "" {
			color = colorForRunID(runID, nil)
		}
		series = append(series, ChartSeries{
			RunID:          runID,
			RunGroupID:     groupID,
			GroupClass:     groupClasses[groupID],
			Color:          color,
			Points:         svgPoints(renderPoints, xMin, xMax, yMin, yMax),
			Values:         chartPoints(renderPoints),
			SmoothedValues: smoothedValues,
			PointCount:     sourcePointCount,
			RenderedPoints: len(renderPoints),
			Decimated:      len(renderPoints) < sourcePointCount,
			Sampling:       sampling,
			Overlay: OverlayMetadata{
				Source:      source,
				StartStep:   runPoints[0].Step,
				EndStep:     runPoints[len(runPoints)-1].Step,
				SampleCount: len(renderPoints),
			},
		})
	}
	return ChartView{
		HasData:    len(series) > 0,
		MetricName: metric,
		XMin:       strconv.FormatInt(xMin, 10),
		XMax:       strconv.FormatInt(xMax, 10),
		YMin:       formatFloat(yMin),
		YMax:       formatFloat(yMax),
		Series:     series,
		Smoothing:  smoothing,
	}
}

func maxMetricPointSourceCount(points []metricPoint) int {
	maxCount := 0
	for _, point := range points {
		if point.SourcePointCount > maxCount {
			maxCount = point.SourcePointCount
		}
	}
	return maxCount
}

type sweepConfigValue struct {
	Label   string
	Number  float64
	Numeric bool
}

type parallelAxis struct {
	ParallelAxisView
	min        float64
	max        float64
	categories map[string]int
}

func buildSweepWithRunColors(points []metricPoint, metric string, runs []RunView, configs []ConfigView, groupClasses map[string]string, runColors map[string]string) SweepView {
	latestByRun := latestMetricByRun(points, metric)
	if metric == "" || len(latestByRun) == 0 {
		return SweepView{}
	}
	runByID := map[string]RunView{}
	for _, run := range runs {
		runByID[run.RunID] = run
	}
	configByRun := sweepConfigsByRun(configs)
	if len(runColors) == 0 {
		runColors = runColorMap(points)
	}
	importance := parameterImportance(configByRun, latestByRun)
	params := topSweepParameters(importance, configByRun, latestByRun, 5)
	axes := buildParallelAxes(params, metric, configByRun, latestByRun)

	runIDs := make([]string, 0, len(latestByRun))
	for runID := range latestByRun {
		if _, ok := runByID[runID]; ok {
			runIDs = append(runIDs, runID)
		}
	}
	sort.Slice(runIDs, func(i, j int) bool {
		left := latestByRun[runIDs[i]]
		right := latestByRun[runIDs[j]]
		if left.Value == right.Value {
			return runIDs[i] < runIDs[j]
		}
		return left.Value > right.Value
	})
	metricMin, metricMax := 0.0, 0.0
	for i, runID := range runIDs {
		value := latestByRun[runID].Value
		if i == 0 {
			metricMin, metricMax = value, value
			continue
		}
		if value < metricMin {
			metricMin = value
		}
		if value > metricMax {
			metricMax = value
		}
	}

	sweepRuns := make([]SweepRunView, 0, len(runIDs))
	series := make([]ParallelRunSeries, 0, len(runIDs))
	var best *BestRunView
	for i, runID := range runIDs {
		run := runByID[runID]
		point := latestByRun[runID]
		color := runColors[runID]
		if color == "" {
			color = colorForRunID(runID, nil)
		}
		if i == 0 {
			best = &BestRunView{
				RunID:      runID,
				RunGroupID: point.RunGroupID,
				GroupClass: groupClasses[point.RunGroupID],
				MetricName: metric,
				Value:      formatFloat(point.Value),
				RawValue:   point.Value,
			}
		}
		sweepRuns = append(sweepRuns, SweepRunView{
			Rank:        i + 1,
			RunID:       runID,
			RunGroupID:  point.RunGroupID,
			GroupClass:  groupClasses[point.RunGroupID],
			State:       run.State,
			Metric:      formatFloat(point.Value),
			MetricWidth: metricValueWidth(point.Value, metricMin, metricMax),
			Color:       color,
		})
		series = append(series, ParallelRunSeries{
			RunID:      runID,
			RunGroupID: point.RunGroupID,
			GroupClass: groupClasses[point.RunGroupID],
			Color:      color,
			Points:     parallelPoints(params, axes, configByRun[runID], point.Value),
			Metric:     formatFloat(point.Value),
			RawMetric:  point.Value,
		})
	}

	return SweepView{
		HasData:     len(sweepRuns) > 0,
		MetricName:  metric,
		BestRun:     best,
		Runs:        sweepRuns,
		Axes:        axisViews(axes),
		Series:      series,
		Importance:  importance,
		ConfigCount: countConfiguredRuns(configByRun, latestByRun),
	}
}

func latestMetricByRun(points []metricPoint, metric string) map[string]metricPoint {
	latest := map[string]metricPoint{}
	for _, point := range points {
		if point.MetricName != metric {
			continue
		}
		prev, ok := latest[point.RunID]
		if !ok || point.Step >= prev.Step {
			latest[point.RunID] = point
		}
	}
	return latest
}

func sweepConfigsByRun(configs []ConfigView) map[string]map[string]sweepConfigValue {
	out := map[string]map[string]sweepConfigValue{}
	for _, config := range configs {
		if strings.TrimSpace(config.NormalizedJSON) == "" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(config.NormalizedJSON), &payload); err != nil {
			continue
		}
		fields := map[string]sweepConfigValue{}
		flattenSweepConfig("", payload, fields)
		if len(fields) == 0 {
			continue
		}
		if out[config.RunID] == nil {
			out[config.RunID] = map[string]sweepConfigValue{}
		}
		for key, value := range fields {
			out[config.RunID][key] = value
		}
	}
	return out
}

func flattenSweepConfig(prefix string, value any, out map[string]sweepConfigValue) {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v["value"]; ok && isSweepScalar(raw) {
			if prefix != "" {
				out[prefix] = newSweepConfigValue(raw)
			}
			return
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flattenSweepConfig(next, v[key], out)
		}
	case []any:
		if prefix != "" && len(v) <= 4 {
			labels := make([]string, 0, len(v))
			for _, item := range v {
				if !isSweepScalar(item) {
					return
				}
				labels = append(labels, newSweepConfigValue(item).Label)
			}
			out[prefix] = sweepConfigValue{Label: strings.Join(labels, ","), Numeric: false}
		}
	default:
		if prefix != "" && isSweepScalar(v) {
			out[prefix] = newSweepConfigValue(v)
		}
	}
}

func isSweepScalar(value any) bool {
	switch value.(type) {
	case nil, bool, string, float64, int, int64, json.Number:
		return true
	default:
		return false
	}
}

func newSweepConfigValue(value any) sweepConfigValue {
	switch v := value.(type) {
	case nil:
		return sweepConfigValue{Label: "null"}
	case bool:
		return sweepConfigValue{Label: strconv.FormatBool(v)}
	case string:
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			return sweepConfigValue{Label: v, Number: parsed, Numeric: true}
		}
		return sweepConfigValue{Label: v}
	case float64:
		return sweepConfigValue{Label: formatFloat(v), Number: v, Numeric: true}
	case int:
		return sweepConfigValue{Label: strconv.Itoa(v), Number: float64(v), Numeric: true}
	case int64:
		return sweepConfigValue{Label: strconv.FormatInt(v, 10), Number: float64(v), Numeric: true}
	case json.Number:
		if parsed, err := v.Float64(); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			return sweepConfigValue{Label: v.String(), Number: parsed, Numeric: true}
		}
		return sweepConfigValue{Label: v.String()}
	default:
		return sweepConfigValue{Label: fmt.Sprint(v)}
	}
}

func parameterImportance(configs map[string]map[string]sweepConfigValue, metrics map[string]metricPoint) []ParameterImportanceView {
	names := map[string]bool{}
	for runID := range metrics {
		for name := range configs[runID] {
			names[name] = true
		}
	}
	metricValues := make([]float64, 0, len(metrics))
	for _, point := range metrics {
		metricValues = append(metricValues, point.Value)
	}
	metricRange := valueRange(metricValues)
	if metricRange == 0 {
		metricRange = 1
	}
	out := []ParameterImportanceView{}
	for name := range names {
		xs := []float64{}
		ys := []float64{}
		categories := map[string][]float64{}
		allNumeric := true
		for runID, metric := range metrics {
			value, ok := configs[runID][name]
			if !ok {
				continue
			}
			ys = append(ys, metric.Value)
			if value.Numeric {
				xs = append(xs, value.Number)
			} else {
				allNumeric = false
			}
			categories[value.Label] = append(categories[value.Label], metric.Value)
		}
		if len(ys) < 2 || len(categories) < 2 {
			continue
		}
		importance := 0.0
		correlation := 0.0
		correlationLabel := "n/a"
		if allNumeric && len(xs) == len(ys) {
			correlation = pearson(xs, ys)
			importance = math.Abs(correlation)
			correlationLabel = formatSignedFloat(correlation)
		} else {
			means := make([]float64, 0, len(categories))
			for _, values := range categories {
				means = append(means, mean(values))
			}
			importance = valueRange(means) / metricRange
			if importance > 1 {
				importance = 1
			}
		}
		out = append(out, ParameterImportanceView{
			Name:             name,
			Importance:       importance,
			ImportanceLabel:  formatFloat(importance),
			ImportanceWidth:  percentWidth(importance),
			Correlation:      correlation,
			CorrelationLabel: correlationLabel,
			CorrelationWidth: percentWidth(math.Abs(correlation)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Importance == out[j].Importance {
			return out[i].Name < out[j].Name
		}
		return out[i].Importance > out[j].Importance
	})
	return out
}

func topSweepParameters(importance []ParameterImportanceView, configs map[string]map[string]sweepConfigValue, metrics map[string]metricPoint, limit int) []string {
	names := []string{}
	seen := map[string]bool{}
	for _, item := range importance {
		if len(names) >= limit {
			break
		}
		names = append(names, item.Name)
		seen[item.Name] = true
	}
	if len(names) >= limit {
		return names
	}
	fallback := map[string]map[string]bool{}
	for runID := range metrics {
		for name, value := range configs[runID] {
			if seen[name] {
				continue
			}
			if fallback[name] == nil {
				fallback[name] = map[string]bool{}
			}
			fallback[name][value.Label] = true
		}
	}
	candidates := make([]string, 0, len(fallback))
	for name, values := range fallback {
		if len(values) > 1 {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	for _, name := range candidates {
		if len(names) >= limit {
			break
		}
		names = append(names, name)
	}
	return names
}

func buildParallelAxes(params []string, metric string, configs map[string]map[string]sweepConfigValue, metrics map[string]metricPoint) []parallelAxis {
	axes := make([]parallelAxis, 0, len(params)+1)
	for _, name := range params {
		values := []sweepConfigValue{}
		allNumeric := true
		for runID := range metrics {
			value, ok := configs[runID][name]
			if !ok {
				continue
			}
			values = append(values, value)
			if !value.Numeric {
				allNumeric = false
			}
		}
		if len(values) == 0 {
			continue
		}
		axis := parallelAxis{ParallelAxisView: ParallelAxisView{Name: name}}
		if allNumeric {
			axis.Kind = "number"
			axis.min, axis.max = values[0].Number, values[0].Number
			for _, value := range values {
				if value.Number < axis.min {
					axis.min = value.Number
				}
				if value.Number > axis.max {
					axis.max = value.Number
				}
			}
			if axis.min == axis.max {
				axis.min--
				axis.max++
			}
			axis.Min = formatFloat(axis.min)
			axis.Max = formatFloat(axis.max)
		} else {
			axis.Kind = "category"
			axis.categories = map[string]int{}
			for _, value := range values {
				axis.categories[value.Label] = 0
			}
			axis.Values = sortedKeys(axis.categories)
			for i, label := range axis.Values {
				axis.categories[label] = i
			}
		}
		axes = append(axes, axis)
	}
	metricAxis := parallelAxis{ParallelAxisView: ParallelAxisView{Name: metric, Kind: "number"}}
	first := true
	for _, point := range metrics {
		if first {
			metricAxis.min, metricAxis.max = point.Value, point.Value
			first = false
			continue
		}
		if point.Value < metricAxis.min {
			metricAxis.min = point.Value
		}
		if point.Value > metricAxis.max {
			metricAxis.max = point.Value
		}
	}
	if metricAxis.min == metricAxis.max {
		metricAxis.min--
		metricAxis.max++
	}
	metricAxis.Min = formatFloat(metricAxis.min)
	metricAxis.Max = formatFloat(metricAxis.max)
	axes = append(axes, metricAxis)
	return axes
}

func axisViews(axes []parallelAxis) []ParallelAxisView {
	out := make([]ParallelAxisView, 0, len(axes))
	for i, axis := range axes {
		view := axis.ParallelAxisView
		x := 38.0
		if len(axes) > 1 {
			x = 38.0 + (float64(i)/float64(len(axes)-1))*(760.0-2*38.0)
		}
		view.X = fmt.Sprintf("%.1f", x)
		out = append(out, view)
	}
	return out
}

func parallelPoints(params []string, axes []parallelAxis, config map[string]sweepConfigValue, metricValue float64) string {
	const (
		width   = 760.0
		height  = 300.0
		padding = 38.0
	)
	if len(axes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(axes))
	for i, axis := range axes {
		x := padding
		if len(axes) > 1 {
			x = padding + (float64(i)/float64(len(axes)-1))*(width-2*padding)
		}
		var y float64
		if i < len(params) {
			value, ok := config[params[i]]
			if ok {
				y = parallelAxisY(axis, value)
			} else {
				y = height / 2
			}
		} else {
			y = numberAxisY(metricValue, axis.min, axis.max, height, padding)
		}
		parts = append(parts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return strings.Join(parts, " ")
}

func parallelAxisY(axis parallelAxis, value sweepConfigValue) float64 {
	const (
		height  = 300.0
		padding = 38.0
	)
	if axis.Kind == "number" && value.Numeric {
		return numberAxisY(value.Number, axis.min, axis.max, height, padding)
	}
	if len(axis.Values) <= 1 {
		return height / 2
	}
	index := axis.categories[value.Label]
	return padding + (float64(index)/float64(len(axis.Values)-1))*(height-2*padding)
}

func numberAxisY(value, min, max, height, padding float64) float64 {
	if max == min {
		return height / 2
	}
	return height - padding - ((value-min)/(max-min))*(height-2*padding)
}

func countConfiguredRuns(configs map[string]map[string]sweepConfigValue, metrics map[string]metricPoint) int {
	count := 0
	for runID := range metrics {
		if len(configs[runID]) > 0 {
			count++
		}
	}
	return count
}

func sortedKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func valueRange(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	minValue, maxValue := values[0], values[0]
	for _, value := range values[1:] {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	return maxValue - minValue
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func pearson(xs, ys []float64) float64 {
	if len(xs) != len(ys) || len(xs) < 2 {
		return 0
	}
	xMean := mean(xs)
	yMean := mean(ys)
	numerator := 0.0
	xDenominator := 0.0
	yDenominator := 0.0
	for i := range xs {
		xDelta := xs[i] - xMean
		yDelta := ys[i] - yMean
		numerator += xDelta * yDelta
		xDenominator += xDelta * xDelta
		yDenominator += yDelta * yDelta
	}
	denominator := math.Sqrt(xDenominator * yDenominator)
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}

func formatSignedFloat(value float64) string {
	if value > 0 {
		return "+" + formatFloat(value)
	}
	return formatFloat(value)
}

func percentWidth(value float64) string {
	if value < 0 {
		value = -value
	}
	if value > 1 {
		value = 1
	}
	return strconv.FormatFloat(value*100, 'f', 1, 64) + "%"
}

func metricValueWidth(value, min, max float64) string {
	if max == min {
		return "100%"
	}
	normalized := (value - min) / (max - min)
	if normalized < 0 {
		normalized = 0
	}
	if normalized > 1 {
		normalized = 1
	}
	return strconv.FormatFloat(12+normalized*88, 'f', 1, 64) + "%"
}

func requestedMetric(points []metricPoint, requested string) string {
	if requested != "" {
		for _, point := range points {
			if point.MetricName == requested {
				return requested
			}
		}
	}
	for _, candidate := range []string{"train/return", "eval/mean_episode_return", "eval/score"} {
		for _, point := range points {
			if point.MetricName == candidate {
				return candidate
			}
		}
	}
	if metric := preferredOutcomeMetricName(points); metric != "" {
		return metric
	}
	for _, point := range points {
		if point.Card == "Outcome" {
			return point.MetricName
		}
	}
	if len(points) == 0 {
		return ""
	}
	return points[0].MetricName
}

func buildDecisionMetricContext(points []metricPoint, fallbackMetric string, groupClasses map[string]string) decisionMetricContext {
	metric := dashboardDecisionMetricName(points, fallbackMetric)
	if metric == "" {
		return decisionMetricContext{}
	}
	decisionPoints := filterMetricPoints(points, metric)
	cards := summarizeCards(decisionPoints, groupClasses)
	return decisionMetricContext{
		MetricName:  metric,
		Points:      decisionPoints,
		Cards:       cards,
		BestGroupID: bestGroupID(cards, metric),
	}
}

func dashboardDecisionMetricName(points []metricPoint, fallbackMetric string) string {
	if metric := preferredOutcomeMetricName(points); metric != "" {
		return metric
	}
	if fallbackMetric != "" {
		for _, point := range points {
			if point.MetricName == fallbackMetric {
				return fallbackMetric
			}
		}
	}
	return requestedMetric(points, fallbackMetric)
}

func preferredOutcomeMetricName(points []metricPoint) string {
	available := map[string]bool{}
	for _, point := range points {
		available[point.MetricName] = true
	}
	bestMetric := ""
	bestRank := maxInt()
	for metric := range available {
		rank := dashboardDecisionMetricRank(metric)
		if rank >= maxInt() {
			continue
		}
		if rank < bestRank || (rank == bestRank && metric < bestMetric) {
			bestMetric = metric
			bestRank = rank
		}
	}
	return bestMetric
}

func dashboardDecisionMetricRank(metric string) int {
	name := normalizedMetricName(metric)
	for index, preferred := range dashboardDecisionMetricPriority {
		if name == normalizedMetricName(preferred) {
			return index * 10
		}
	}
	if isModelLossMetric(name) || !isGenericOutcomeMetric(name) {
		return maxInt()
	}
	prefixRank := 700
	switch {
	case strings.HasPrefix(name, "final/"):
		prefixRank = 200
	case strings.HasPrefix(name, "eval/"):
		prefixRank = 300
	case strings.HasPrefix(name, "test/"), strings.HasPrefix(name, "val/"), strings.HasPrefix(name, "valid/"):
		prefixRank = 320
	case strings.HasPrefix(name, "detect/"):
		prefixRank = 400
	}
	return prefixRank + outcomeMeasureRank(name)
}

func isGenericOutcomeMetric(name string) bool {
	if !strings.Contains(name, "/") {
		return false
	}
	if !strings.HasPrefix(name, "final/") &&
		!strings.HasPrefix(name, "eval/") &&
		!strings.HasPrefix(name, "test/") &&
		!strings.HasPrefix(name, "val/") &&
		!strings.HasPrefix(name, "valid/") &&
		!strings.HasPrefix(name, "detect/") {
		return false
	}
	return outcomeMeasureRank(name) < 900
}

func outcomeMeasureRank(name string) int {
	switch {
	case strings.Contains(name, "auprc"):
		return 1
	case strings.Contains(name, "mean_episode_return"), strings.Contains(name, "episode_return"):
		return 2
	case strings.Contains(name, "return"):
		return 3
	case strings.Contains(name, "reward"):
		return 4
	case strings.Contains(name, "success_rate"), strings.Contains(name, "success/rate"):
		return 5
	case strings.Contains(name, "win_rate"), strings.Contains(name, "win/rate"):
		return 6
	case strings.Contains(name, "pass@1"), strings.Contains(name, "pass_rate"), strings.Contains(name, "pass/rate"):
		return 7
	case strings.Contains(name, "exact_match"):
		return 8
	case strings.Contains(name, "accuracy"), strings.Contains(name, "acc@"):
		return 9
	case strings.Contains(name, "score"):
		return 10
	case strings.Contains(name, "macro_f1"), strings.Contains(name, "/f1"), strings.HasSuffix(name, "_f1"):
		return 11
	case strings.Contains(name, "auroc"), strings.Contains(name, "auc"):
		return 12
	default:
		return 900
	}
}

func normalizedMetricName(metric string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(metric), "-", "_"))
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func decisionChart(chart ChartView, metric string) ChartView {
	chart.MetricName = metric
	return chart
}

func svgPoints(points []metricPoint, xMin, xMax int64, yMin, yMax float64) string {
	const (
		width   = 760.0
		height  = 220.0
		padding = 28.0
	)
	parts := make([]string, 0, len(points))
	for _, point := range points {
		x := padding + (float64(point.Step-xMin)/float64(xMax-xMin))*(width-2*padding)
		y := height - padding - ((point.Value-yMin)/(yMax-yMin))*(height-2*padding)
		parts = append(parts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return strings.Join(parts, " ")
}

func sampleMetricPointsByStepInterval(points []metricPoint, interval int) []metricPoint {
	if interval <= 1 || len(points) <= 2 {
		return points
	}
	out := make([]metricPoint, 0, len(points)/interval+2)
	first := points[0]
	last := points[len(points)-1]
	out = append(out, first)
	lastStep := first.Step
	for _, point := range points[1 : len(points)-1] {
		if point.Step == first.Step || point.Step == last.Step {
			continue
		}
		if (point.Step-first.Step)%int64(interval) == 0 {
			out = append(out, point)
			lastStep = point.Step
		}
	}
	if last.Step != lastStep {
		out = append(out, last)
	}
	return out
}

func chartSmoothingFor(metric string, byRun map[string][]metricPoint) *ChartSmoothing {
	if !defaultSmoothMetricName(metric) {
		return nil
	}
	maxRunPoints := 0
	for _, points := range byRun {
		if len(points) > maxRunPoints {
			maxRunPoints = len(points)
		}
	}
	if maxRunPoints < chartSmoothingDensePointThreshold {
		return nil
	}
	return &ChartSmoothing{
		Method:              "ema",
		Alpha:               chartSmoothingEMAAlpha,
		DensePointThreshold: chartSmoothingDensePointThreshold,
		Reason:              "dense training scalar",
		RawPreserved:        true,
	}
}

func defaultSmoothMetricName(metric string) bool {
	name := strings.ToLower(strings.TrimSpace(metric))
	if name == "" {
		return false
	}
	for _, prefix := range []string{"eval/", "validation/", "val/", "test/", "final/"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return strings.HasPrefix(name, "train/") || strings.Contains(name, "/loss") || name == "loss"
}

func smoothedMetricPointsForRendered(points, rendered []metricPoint, alpha float64) []metricPoint {
	if len(points) == 0 || len(rendered) == 0 {
		return nil
	}
	if alpha <= 0 || alpha > 1 {
		alpha = chartSmoothingEMAAlpha
	}
	smoothedByStep := make(map[int64]float64, len(points))
	ema := points[0].Value
	for index, point := range points {
		if index == 0 {
			ema = point.Value
		} else {
			ema = alpha*point.Value + (1-alpha)*ema
		}
		smoothedByStep[point.Step] = ema
	}
	out := make([]metricPoint, 0, len(rendered))
	for _, point := range rendered {
		smoothed := point
		if value, ok := smoothedByStep[point.Step]; ok {
			smoothed.Value = value
		}
		out = append(out, smoothed)
	}
	return out
}

func chartPoints(points []metricPoint) []ChartPoint {
	values := make([]ChartPoint, 0, len(points))
	for _, point := range points {
		values = append(values, ChartPoint{
			Step:  point.Step,
			Value: point.Value,
		})
	}
	return values
}

func attachRunDetails(storeRoot string, runs []RunView, contexts map[string]map[string]any, configs []ConfigView, artifacts []ArtifactView, events []EventView, observations []ObservationView) []RunView {
	configsByRun := map[string][]ConfigView{}
	for _, config := range configs {
		configsByRun[config.RunID] = append(configsByRun[config.RunID], config)
	}
	artifactsByRun := map[string][]ArtifactView{}
	for _, artifact := range artifacts {
		artifactsByRun[artifact.RunID] = append(artifactsByRun[artifact.RunID], artifact)
	}
	eventsByRun := map[string][]EventView{}
	for _, event := range events {
		eventsByRun[event.RunID] = append(eventsByRun[event.RunID], event)
	}
	observationsByScope := map[string][]ObservationView{}
	for _, obs := range observations {
		observationsByScope[obs.ScopeType+":"+obs.ScopeID] = append(observationsByScope[obs.ScopeType+":"+obs.ScopeID], obs)
	}
	for i := range runs {
		run := &runs[i]
		run.Configs = configsByRun[run.RunID]
		run.Artifacts = artifactsByRun[run.RunID]
		run.Events = eventsByRun[run.RunID]
		run.Observations = observationsByScope["run:"+run.RunID]
		run.Systems = systemFields(*run, contexts[run.RunID])
		run.ObserveCLI = portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " observe --scope run:" + shellQuote(run.RunID) + " --text " + shellQuote("note") + " --idempotency-key " + shellQuote("obs-"+run.RunID)
	}
	return runs
}

func systemFields(run RunView, context map[string]any) []FieldView {
	fields := []struct {
		label string
		key   string
		row   bool
	}{
		{"Queue wait", "queue_wait_seconds", false},
		{"GPU class", "gpu_class", false},
		{"GPU count", "gpu_count", false},
		{"Cluster queue", "cluster_queue", false},
		{"Local queue", "local_queue", false},
		{"Kueue workload", "kueue_workload", false},
		{"Pod UID", "pod_uid", false},
		{"Nodes", "node_names", false},
		{"Resource claims", "resource_claims", false},
		{"GPU hours", "gpu_hours", false},
		{"Estimated cost", "estimated_cost", false},
		{"Cluster", "cluster", false},
		{"Namespace", "namespace", false},
		{"Team", "team", false},
		{"Profile", "profile", false},
		{"Lane", "lane", false},
		{"Runtime", "runtime", false},
		{"Dependencies", "dependencies", false},
		{"Log URI", "log_uri", false},
		{"Config hash", "config_hash", true},
		{"Code SHA", "code_sha", true},
		{"Image digest", "image_digest", true},
		{"Tau command", "tau_command", true},
	}
	out := make([]FieldView, 0, len(fields))
	for _, field := range fields {
		value := ""
		if field.row {
			value = runValue(run, field.key)
		} else {
			value = formatAny(context[field.key])
		}
		state := "collected"
		if value == "" {
			value = "not collected"
			state = "not_collected"
		}
		out = append(out, FieldView{Name: field.label, Value: value, CollectionState: state})
	}
	return out
}

func buildExperimentSummary(target, targetType string, experiment *expstore.ExperimentRecord, groups []RunGroupView, runs []RunView, cards []CardView, chart ChartView, observations []ObservationView, bestGroup, fallbackCommand string) ExperimentSummary {
	blockers := countObservations(observations, "blocker")
	decisions := countObservations(observations, "decision")
	latestDecision := latestObservation(observations, "decision")
	latestNext := latestObservation(observations, "next-experiment")
	seedCoverage := seedCoverageText(groups, runs)
	currentAnswer := "No current answer yet."
	if latestDecision.Text != "" {
		currentAnswer = latestDecision.Text
	} else if bestGroup != "" && chart.MetricName != "" {
		currentAnswer = fmt.Sprintf("Current best evidence favors %s on %s.", bestGroup, chart.MetricName)
	}
	bestEvidence := bestEvidenceText(cards, chart.MetricName, bestGroup)
	status := "active"
	switch {
	case len(runs) == 0:
		status = "empty"
	case blockers > 0:
		status = "blocked"
	case decisions > 0:
		status = "answered"
	}
	confidence := confidenceText(groups, runs, bestGroup, blockers)
	nextAction := "Review the comparison and record the next experiment as an evidence-linked observation."
	nextCommand := fallbackCommand
	if latestNext.Text != "" {
		nextAction = latestNext.Text
		if command := commandFromEvidence(latestNext.Evidence); command != "" {
			nextCommand = command
		}
	} else if blockers > 0 {
		nextAction = "Resolve blockers before adding more seeds."
	} else if bestGroup != "" && chart.MetricName != "" {
		nextAction = fmt.Sprintf("Use %s as the current baseline, then compare the next ablation on %s.", bestGroup, chart.MetricName)
	}
	if experiment == nil && targetType == "experiment" {
		currentAnswer = fmt.Sprintf("Experiment %s has no metadata yet.", target)
	}
	return ExperimentSummary{
		Status:        status,
		CurrentAnswer: currentAnswer,
		BestEvidence:  bestEvidence,
		Confidence:    confidence,
		SeedCoverage:  seedCoverage,
		Blockers:      blockers,
		Decisions:     decisions,
		NextAction:    nextAction,
		NextCommand:   nextCommand,
	}
}

func countObservations(observations []ObservationView, typ string) int {
	count := 0
	for _, obs := range observations {
		if obs.Type == typ {
			count++
		}
	}
	return count
}

func latestObservation(observations []ObservationView, typ string) ObservationView {
	var latest ObservationView
	for _, obs := range observations {
		if obs.Type == typ {
			latest = obs
		}
	}
	return latest
}

func seedCoverageText(groups []RunGroupView, runs []RunView) string {
	if len(runs) == 0 {
		return "0 runs"
	}
	counts := map[string]int{}
	for _, run := range runs {
		counts[run.RunGroupID]++
	}
	groupIDs := make([]string, 0, len(counts))
	seen := map[string]bool{}
	for _, group := range groups {
		if _, ok := counts[group.RunGroupID]; ok {
			groupIDs = append(groupIDs, group.RunGroupID)
			seen[group.RunGroupID] = true
		}
	}
	for groupID := range counts {
		if !seen[groupID] {
			groupIDs = append(groupIDs, groupID)
		}
	}
	sort.Strings(groupIDs)
	parts := make([]string, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		parts = append(parts, fmt.Sprintf("%s=%d", groupID, counts[groupID]))
	}
	return fmt.Sprintf("%d runs across %d run groups (%s)", len(runs), len(groupIDs), strings.Join(parts, ", "))
}

func confidenceText(groups []RunGroupView, runs []RunView, bestGroup string, blockers int) string {
	if blockers > 0 {
		return "blocked - unresolved blockers"
	}
	if len(runs) == 0 || bestGroup == "" {
		return "low - no outcome evidence yet"
	}
	counts := map[string]int{}
	for _, run := range runs {
		counts[run.RunGroupID]++
	}
	minSeeds := len(runs) + 1
	comparedGroups := 0
	for _, group := range groups {
		count := counts[group.RunGroupID]
		if count == 0 {
			continue
		}
		comparedGroups++
		if count < minSeeds {
			minSeeds = count
		}
	}
	if comparedGroups >= 2 && minSeeds >= 3 {
		return "high - at least 3 seeds per compared group"
	}
	if comparedGroups >= 2 && minSeeds >= 2 {
		return "medium - multiple groups with repeated seeds"
	}
	return "low - needs another group or more seeds"
}

func bestEvidenceText(cards []CardView, metric, bestGroup string) string {
	if metric == "" || bestGroup == "" {
		return "No scalar evidence collected yet."
	}
	for _, card := range cards {
		for _, m := range card.Metrics {
			if m.Name != metric {
				continue
			}
			for _, group := range m.Groups {
				if group.RunGroupID == bestGroup {
					return fmt.Sprintf("%s: %s best=%s, median=%s across %d runs at latest step %s.", card.Name, metric, group.Best, group.Median, group.RunCount, group.LatestStep)
				}
			}
		}
	}
	return fmt.Sprintf("Best current group is %s on %s.", bestGroup, metric)
}

func commandFromEvidence(evidence string) string {
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(evidence), &payload); err != nil {
		return ""
	}
	for _, key := range []string{"command", "next_command", "tau_command"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func buildCompareInsights(points []metricPoint, metric string, runs []RunView, contexts map[string]map[string]any, configs []ConfigView, events []EventView, bestGroup string) CompareInsights {
	insights := CompareInsights{
		MetricName:   metric,
		Outliers:     metricOutliers(points, metric),
		EventMarkers: eventMarkers(runs, events),
		RuntimeDiffs: append(runtimeDiffs(runs, contexts), configDiffs(runs, configs)...),
	}
	if metric == "" {
		insights.Summary = "No scalar metric is available yet; import metrics before comparing run groups."
		return insights
	}
	if bestGroup == "" {
		insights.Summary = fmt.Sprintf("No winning group has been identified for %s yet.", metric)
		return insights
	}
	insights.Summary = fmt.Sprintf("Best current group is %s on %s; inspect outliers, event markers, and runtime diffs before recording a decision.", bestGroup, metric)
	return insights
}

func metricOutliers(points []metricPoint, metric string) []RunInsight {
	if metric == "" {
		return nil
	}
	latestByRun := map[string]metricPoint{}
	for _, point := range points {
		if point.MetricName != metric {
			continue
		}
		prev, ok := latestByRun[point.RunID]
		if !ok || point.Step >= prev.Step {
			latestByRun[point.RunID] = point
		}
	}
	byGroup := map[string][]metricPoint{}
	for _, point := range latestByRun {
		byGroup[point.RunGroupID] = append(byGroup[point.RunGroupID], point)
	}
	groupIDs := make([]string, 0, len(byGroup))
	for groupID := range byGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	out := []RunInsight{}
	for _, groupID := range groupIDs {
		groupPoints := byGroup[groupID]
		if len(groupPoints) < 3 {
			continue
		}
		values := make([]float64, 0, len(groupPoints))
		for _, point := range groupPoints {
			values = append(values, point.Value)
		}
		sort.Float64s(values)
		q1 := percentile(values, 0.25)
		q3 := percentile(values, 0.75)
		iqr := q3 - q1
		if iqr == 0 {
			continue
		}
		low := q1 - 1.5*iqr
		high := q3 + 1.5*iqr
		sort.Slice(groupPoints, func(i, j int) bool { return groupPoints[i].RunID < groupPoints[j].RunID })
		for _, point := range groupPoints {
			switch {
			case point.Value < low:
				out = append(out, RunInsight{RunID: point.RunID, RunGroupID: point.RunGroupID, Value: formatFloat(point.Value), Reason: "below group envelope"})
			case point.Value > high:
				out = append(out, RunInsight{RunID: point.RunID, RunGroupID: point.RunGroupID, Value: formatFloat(point.Value), Reason: "above group envelope"})
			}
		}
	}
	return out
}

func eventMarkers(runs []RunView, events []EventView) []EventMarker {
	if len(events) == 0 {
		return nil
	}
	groupByRun := map[string]string{}
	for _, run := range runs {
		groupByRun[run.RunID] = run.RunGroupID
	}
	out := []EventMarker{}
	for _, event := range events {
		if !isExperimentEventMarker(event) {
			continue
		}
		out = append(out, EventMarker{
			RunID:      event.RunID,
			RunGroupID: groupByRun[event.RunID],
			Time:       event.Time,
			Type:       event.Type,
			Severity:   event.Severity,
			Message:    event.Message,
		})
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func isExperimentEventMarker(event EventView) bool {
	if event.Severity == "warning" || event.Severity == "error" {
		return true
	}
	switch event.Type {
	case "restart", "preempted", "oom", "failed", "checkpoint", "artifact":
		return true
	default:
		return false
	}
}

func runtimeDiffs(runs []RunView, contexts map[string]map[string]any) []RuntimeDiff {
	fields := []struct {
		label  string
		key    string
		source string
	}{
		{"State", "state", "run"},
		{"Config hash", "config_hash", "run"},
		{"Code SHA", "code_sha", "run"},
		{"Image digest", "image_digest", "run"},
		{"Profile", "profile", "context"},
		{"Cluster queue", "cluster_queue", "context"},
		{"GPU class", "gpu_class", "context"},
		{"GPU count", "gpu_count", "context"},
		{"Lane", "lane", "context"},
		{"Runtime", "runtime", "context"},
		{"Dependencies", "dependencies", "context"},
		{"Log URI", "log_uri", "context"},
	}
	out := []RuntimeDiff{}
	for _, field := range fields {
		byGroup := map[string]map[string]bool{}
		unique := map[string]bool{}
		for _, run := range runs {
			value := runtimeFieldValue(run, contexts[run.RunID], field.key, field.source)
			if value == "" {
				continue
			}
			if byGroup[run.RunGroupID] == nil {
				byGroup[run.RunGroupID] = map[string]bool{}
			}
			byGroup[run.RunGroupID][value] = true
			unique[value] = true
		}
		if len(unique) <= 1 {
			continue
		}
		groupIDs := make([]string, 0, len(byGroup))
		for groupID := range byGroup {
			groupIDs = append(groupIDs, groupID)
		}
		sort.Strings(groupIDs)
		diff := RuntimeDiff{Field: field.label}
		for _, groupID := range groupIDs {
			diff.Values = append(diff.Values, RuntimeDiffValue{RunGroupID: groupID, Value: joinedSet(byGroup[groupID])})
		}
		out = append(out, diff)
	}
	return out
}

func configDiffs(runs []RunView, configs []ConfigView) []RuntimeDiff {
	if len(runs) == 0 || len(configs) == 0 {
		return nil
	}
	runGroups := map[string]string{}
	for _, run := range runs {
		runGroups[run.RunID] = run.RunGroupID
	}
	fieldsByRun := configFieldsByRun(configs)
	keys := map[string]bool{}
	for runID := range runGroups {
		for key := range fieldsByRun[runID] {
			keys[key] = true
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		leftPinned := pinnedConfigKey(ordered[i])
		rightPinned := pinnedConfigKey(ordered[j])
		if leftPinned != rightPinned {
			return leftPinned
		}
		return ordered[i] < ordered[j]
	})
	out := []RuntimeDiff{}
	for _, key := range ordered {
		byGroup := map[string]map[string]bool{}
		unique := map[string]bool{}
		for _, run := range runs {
			value := fieldsByRun[run.RunID][key]
			if value == "" {
				continue
			}
			if byGroup[run.RunGroupID] == nil {
				byGroup[run.RunGroupID] = map[string]bool{}
			}
			byGroup[run.RunGroupID][value] = true
			unique[value] = true
		}
		if len(unique) <= 1 {
			continue
		}
		diff := RuntimeDiff{Field: "Config: " + key, Pinned: pinnedConfigKey(key)}
		groupIDs := make([]string, 0, len(byGroup))
		for groupID := range byGroup {
			groupIDs = append(groupIDs, groupID)
		}
		sort.Strings(groupIDs)
		for _, groupID := range groupIDs {
			diff.Values = append(diff.Values, RuntimeDiffValue{RunGroupID: groupID, Value: joinedSet(byGroup[groupID])})
		}
		out = append(out, diff)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

func configFieldsByRun(configs []ConfigView) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, config := range configs {
		fields := map[string]any{}
		raw := strings.TrimSpace(config.IndexedFields)
		if raw == "" {
			raw = strings.TrimSpace(config.NormalizedJSON)
		}
		if raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			var payload any
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				continue
			}
			flattenConfigDiffFields("", payload, fields)
		}
		if out[config.RunID] == nil {
			out[config.RunID] = map[string]string{}
		}
		for key, value := range fields {
			out[config.RunID][key] = formatConfigDiffValue(value)
		}
	}
	return out
}

func flattenConfigDiffFields(prefix string, value any, out map[string]any) {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v["value"]; ok && prefix != "" {
			out[prefix] = raw
			return
		}
		for key, item := range v {
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			flattenConfigDiffFields(next, item, out)
		}
	case []any:
		if prefix != "" && len(v) <= 8 {
			out[prefix] = v
		}
	default:
		if prefix != "" {
			out[prefix] = v
		}
	}
}

func formatConfigDiffValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return formatFloat(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		raw, err := json.Marshal(v)
		if err == nil {
			return string(raw)
		}
		return fmt.Sprint(v)
	}
}

func pinnedConfigKey(key string) bool {
	switch strings.ToLower(key) {
	case "lr", "learning_rate", "batch_size", "model", "model_name", "rank", "seed":
		return true
	default:
		return false
	}
}

func runtimeFieldValue(run RunView, context map[string]any, key, source string) string {
	switch source {
	case "run":
		switch key {
		case "state":
			return run.State
		default:
			return runValue(run, key)
		}
	case "context":
		return formatAny(context[key])
	default:
		return ""
	}
}

func joinedSet(values map[string]bool) string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func buildActions(storeRoot, target, targetType string, experiment *expstore.ExperimentRecord, metric string) ActionView {
	query := "select run_id, run_group_id, state, owner, created_at, completed_at from runs"
	if experiment != nil {
		query += " where run_group_id = " + sqlQuote(experiment.ExperimentID)
	} else if targetType == "run_group" {
		query += " where run_group_id = " + sqlQuote(target)
	} else if targetType == "run" {
		query += " where run_id = " + sqlQuote(target)
	}
	query += " order by run_group_id, run_id"
	packetName := safeFilename(target) + "-packet"
	openCLI := portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " open " + shellQuote(target)
	if metric != "" {
		openCLI += " --metric " + shellQuote(metric)
	}
	return ActionView{
		CopyCLI:      portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " stellar " + shellQuote(target) + " -o html > " + shellQuote(safeFilename(target)+"-stellar.html"),
		OpenCLI:      openCLI,
		CopySQL:      query,
		ExportPacket: portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " export --out " + shellQuote(packetName),
		ObserveCLI:   portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " observe --scope " + shellQuote(targetType+":"+target) + " --text " + shellQuote("note") + " --idempotency-key " + shellQuote("obs-"+safeFilename(target)),
		NextCommand:  defaultNextCommand(storeRoot, target, metric),
		StorePath:    storeRoot,
	}
}

func actionMetric(requested, chartMetric, decisionMetric string) string {
	if metric := strings.TrimSpace(requested); metric != "" {
		return metric
	}
	if metric := strings.TrimSpace(decisionMetric); metric != "" {
		return metric
	}
	return strings.TrimSpace(chartMetric)
}

func defaultNextCommand(storeRoot, target, metric string) string {
	if metric == "" {
		return portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " status " + shellQuote(target) + " --json"
	}
	return portalbin.ExperimentCmd + " --store " + shellQuote(storeRoot) + " compare " + shellQuote(target) + " --metric " + shellQuote(metric) + " --format jsonl"
}

func bestGroupID(cards []CardView, metric string) string {
	bestGroup := ""
	bestValue := math.Inf(-1)
	for _, card := range cards {
		for _, m := range card.Metrics {
			if metric != "" && m.Name != metric {
				continue
			}
			for _, group := range m.Groups {
				if group.BestValue > bestValue {
					bestValue = group.BestValue
					bestGroup = group.RunGroupID
				}
			}
		}
	}
	return bestGroup
}

func metricCard(row expstore.MetricRow) string {
	var tags map[string]string
	if row.Tags != "" && json.Unmarshal([]byte(row.Tags), &tags) == nil {
		if card := tags["tau.metric.card"]; card != "" {
			return card
		}
	}
	name := strings.ToLower(row.MetricName)
	switch {
	case isGenericOutcomeMetric(normalizedMetricName(name)),
		name == "train/return":
		return "Outcome"
	case strings.HasPrefix(name, "gpu/"),
		strings.HasPrefix(name, "system/"):
		return "Systems"
	case strings.HasPrefix(name, "checkpoint/"):
		return "Checkpoint"
	case strings.HasPrefix(name, "feature/"),
		strings.HasPrefix(name, "inference/"):
		return "Model diagnostics"
	case strings.HasPrefix(name, "train/examples"),
		strings.Contains(name, "tokens"),
		strings.Contains(name, "throughput"):
		return "Throughput"
	case strings.HasPrefix(name, "train/lr"),
		strings.HasPrefix(name, "train/learning_rate"),
		strings.HasPrefix(name, "train/grad_norm"),
		strings.HasPrefix(name, "train/gradient_norm"),
		strings.HasPrefix(name, "train/step_time"):
		return "Optimization"
	case strings.HasPrefix(name, "wm/"), strings.HasPrefix(name, "world_model/"), strings.HasPrefix(name, "model/"):
		return "World model"
	case isModelLossMetric(name):
		return "World model"
	case strings.HasPrefix(name, "policy/"):
		return "Behavior"
	case strings.HasPrefix(name, "train/"), strings.HasPrefix(name, "eval/"):
		return "Model diagnostics"
	default:
		return "Other metrics"
	}
}

func isModelLossMetric(name string) bool {
	name = strings.ReplaceAll(name, "-", "_")
	switch {
	case strings.Contains(name, "perplexity"),
		strings.Contains(name, "cross_entropy"),
		strings.Contains(name, "nll"),
		strings.Contains(name, "negative_log_likelihood"),
		strings.HasSuffix(name, "/loss"),
		strings.HasSuffix(name, ".loss"),
		name == "loss":
		return true
	default:
		return false
	}
}

func queryRows(ctx context.Context, store *expstore.Store, query string) ([]map[string]any, error) {
	result, err := store.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return result.Rows, nil
}

func queryRowsArgs(ctx context.Context, store *expstore.Store, query string, args ...any) ([]map[string]any, error) {
	result, err := store.QueryArgs(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result.Rows, nil
}

func experimentFromRecord(q expstore.ExperimentRecord) *expstore.ExperimentRecord {
	return &q
}

func experimentFromRow(row map[string]any) expstore.ExperimentRecord {
	return expstore.ExperimentRecord{
		ExperimentID: stringValue(row, "experiment_id"),
		Project:      stringValue(row, "project"),
		Name:         stringValue(row, "name"),
		Description:  stringValue(row, "description"),
		Source:       stringValue(row, "source"),
		CreatedAt:    stringValue(row, "created_at"),
		UpdatedAt:    stringValue(row, "updated_at"),
	}
}

func groupClassMap(groups []RunGroupView) map[string]string {
	out := map[string]string{}
	for _, group := range groups {
		out[group.RunGroupID] = group.GroupClass
	}
	return out
}

func runIDs(runs []RunView) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.RunID)
	}
	return ids
}

func runIDSet(runs []RunView) map[string]bool {
	out := make(map[string]bool, len(runs))
	for _, run := range runs {
		out[run.RunID] = true
	}
	return out
}

func attachRunSearchMetadata(ctx context.Context, store *expstore.Store, runs []RunView, allowedRunIDs map[string]bool) ([]RunView, []string, error) {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		if allowedRunIDs == nil || allowedRunIDs[run.RunID] {
			ids = append(ids, run.RunID)
		}
	}
	if len(ids) == 0 {
		return runs, nil, nil
	}
	backfill, err := store.EnsureMetricSummaries(ctx)
	if err != nil {
		return nil, nil, err
	}
	tags, err := loadRunTags(ctx, store, ids)
	if err != nil {
		return nil, nil, err
	}
	summaries, err := loadRunMetricSummaries(ctx, store, ids)
	if err != nil {
		return nil, nil, err
	}
	for i := range runs {
		run := &runs[i]
		if allowedRunIDs != nil && !allowedRunIDs[run.RunID] {
			continue
		}
		runTags := tags[run.RunID]
		runSummaries := summaries[run.RunID]
		classification := expstore.ClassifyRun(runRecordFromView(*run), runTags, runSummaries, expstore.SuccessOptions{Tags: runTags})
		run.Successful = classification.Successful
		run.SuccessReasons = classification.Reasons
		run.Tags = runTags
		run.MetricNames = metricSummaryNames(runSummaries)
		latestMetricAt := time.Time{}
		if updatedAt := latestMetricSummaryUpdatedAt(runSummaries); updatedAt != "" {
			run.UpdatedAt = updatedAt
			latestMetricAt = parseLifecycleTime(updatedAt)
		}
		if run.UpdatedAt == "" {
			run.UpdatedAt = firstNonEmptyString(run.CompletedAt, run.StartedAt, run.CreatedAt)
		}
		explicitOutcome := terminalOutcome(run.State)
		explicitReason := ""
		if explicitOutcome != "" {
			explicitReason = "local run record has explicit terminal state " + explicitOutcome
		}
		applyLifecycleTruth(run, ResolveLifecycle(LifecycleEvidence{
			ExplicitOutcome:      explicitOutcome,
			ExplicitReason:       explicitReason,
			ExplicitSource:       "local_run_record",
			TerminalAt:           parseLifecycleTime(run.CompletedAt),
			LatestMetricAt:       latestMetricAt,
			LatestControlPlaneAt: parseLifecycleTime(run.StartedAt),
		}))
	}
	return runs, backfill.Warnings, nil
}

func loadRunTags(ctx context.Context, store *expstore.Store, runIDs []string) (map[string]map[string]string, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	return store.RunTags(ctx, runIDs)
}

func loadRunMetricSummaries(ctx context.Context, store *expstore.Store, runIDs []string) (map[string][]expstore.MetricSummaryRecord, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := queryRowsArgs(ctx, store, `
SELECT run_id, project, run_group_id, metric_name, count, finite_count, non_finite_count,
       min_step, max_step, latest_step, latest_wall_time, latest_value, min_value, max_value,
       updated_at, latest_file_id
FROM metric_summaries WHERE run_id IN (`+sqlPlaceholders(len(runIDs))+`) ORDER BY run_id, metric_name`, stringArgs(runIDs)...)
	if err != nil {
		return nil, err
	}
	out := map[string][]expstore.MetricSummaryRecord{}
	for _, row := range rows {
		summary := expstore.MetricSummaryRecord{
			RunID:          stringValue(row, "run_id"),
			Project:        stringValue(row, "project"),
			RunGroupID:     stringValue(row, "run_group_id"),
			MetricName:     stringValue(row, "metric_name"),
			Count:          int64Value(row, "count"),
			FiniteCount:    int64Value(row, "finite_count"),
			NonFiniteCount: int64Value(row, "non_finite_count"),
			MinStep:        optionalInt64Value(row, "min_step"),
			MaxStep:        optionalInt64Value(row, "max_step"),
			LatestStep:     optionalInt64Value(row, "latest_step"),
			LatestWallTime: optionalInt64Value(row, "latest_wall_time"),
			LatestValue:    float64Value(row, "latest_value"),
			MinValue:       float64Value(row, "min_value"),
			MaxValue:       float64Value(row, "max_value"),
			UpdatedAt:      stringValue(row, "updated_at"),
			LatestFileID:   stringValue(row, "latest_file_id"),
		}
		out[summary.RunID] = append(out[summary.RunID], summary)
	}
	return out, nil
}

func runRecordFromView(run RunView) expstore.RunRecord {
	return expstore.RunRecord{
		RunID:        run.RunID,
		Project:      run.Project,
		RunGroupID:   run.RunGroupID,
		State:        run.State,
		Owner:        run.Owner,
		CreatedAt:    run.CreatedAt,
		StartedAt:    run.StartedAt,
		CompletedAt:  run.CompletedAt,
		ConfigHash:   run.ConfigHash,
		CodeSHA:      run.CodeSHA,
		ImageDigest:  run.ImageDigest,
		TauCommand:   run.TauCommand,
		ResultURI:    run.ResultURI,
		IndexVersion: expstore.SchemaVersion,
	}
}

func metricSummaryNames(summaries []expstore.MetricSummaryRecord) []string {
	names := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		names = append(names, summary.MetricName)
	}
	sort.Strings(names)
	return names
}

func latestMetricSummaryUpdatedAt(summaries []expstore.MetricSummaryRecord) string {
	latest := ""
	for _, summary := range summaries {
		updatedAt := strings.TrimSpace(summary.UpdatedAt)
		if updatedAt != "" && (latest == "" || updatedAt > latest) {
			latest = updatedAt
		}
	}
	return latest
}

func stringArgs(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func mergeKustoRows(opts Options, status expstore.Status, rows []KustoMetricRow, groups []RunGroupView, runs []RunView, points []metricPoint) ([]RunGroupView, []RunView, []metricPoint, []string) {
	filtered, targetType := filterKustoRows(rows, opts.Target)
	if len(filtered) == 0 {
		return groups, runs, points, nil
	}
	if targetType != "" && status.TargetType != "" && targetType != status.TargetType {
		return groups, runs, points, []string{fmt.Sprintf("source=auto skipped Kusto rows for target type %s because local target type is %s", targetType, status.TargetType)}
	}
	existingGroups := map[string]bool{}
	for _, group := range groups {
		existingGroups[group.RunGroupID] = true
	}
	for _, group := range kustoRunGroups(filtered) {
		if existingGroups[group.RunGroupID] {
			continue
		}
		existingGroups[group.RunGroupID] = true
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].RunGroupID < groups[j].RunGroupID
	})

	existingRuns := map[string]bool{}
	for _, run := range runs {
		existingRuns[run.RunID] = true
	}
	remainingRuns := 0
	if opts.MaxRuns > 0 {
		remainingRuns = opts.MaxRuns - len(runs)
		if remainingRuns < 0 {
			remainingRuns = 0
		}
	}
	remoteRuns, truncated := kustoRuns(filtered, 0, time.Now().UTC(), defaultKustoRunStaleAfter)
	addedRuns := map[string]bool{}
	duplicates := 0
	for _, run := range remoteRuns {
		if existingRuns[run.RunID] {
			duplicates++
			continue
		}
		if opts.MaxRuns > 0 && remainingRuns == 0 {
			truncated = true
			continue
		}
		existingRuns[run.RunID] = true
		addedRuns[run.RunID] = true
		runs = append(runs, run)
		if opts.MaxRuns > 0 {
			remainingRuns--
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].RunID < runs[j].RunID
	})
	if len(addedRuns) > 0 {
		points = append(points, kustoMetricPoints(filtered, addedRuns)...)
	}
	warnings := []string{}
	if len(addedRuns) > 0 {
		warnings = append(warnings, fmt.Sprintf("source=auto merged %d Kusto-backed runs with local/PVC runs", len(addedRuns)))
	}
	if duplicates > 0 {
		warnings = append(warnings, fmt.Sprintf("source=auto kept local metrics for %d duplicate Kusto run IDs", duplicates))
	}
	if truncated {
		warnings = append(warnings, fmt.Sprintf("source=auto Kusto runs truncated by max_runs=%d", opts.MaxRuns))
	}
	return groups, runs, points, warnings
}

func stateCountsWithKusto(_ map[string]int, runs []RunView) map[string]int {
	out := map[string]int{}
	for _, run := range runs {
		state := strings.TrimSpace(run.State)
		if state == "" {
			state = "completed"
		}
		out[state]++
	}
	return out
}

func metricNames(points []metricPoint) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, point := range points {
		if !seen[point.MetricName] {
			seen[point.MetricName] = true
			names = append(names, point.MetricName)
		}
	}
	sort.Strings(names)
	return names
}

func runColorMap(points []metricPoint) map[string]string {
	runIDs := []string{}
	for _, point := range points {
		runIDs = append(runIDs, point.RunID)
	}
	return runColorMapForRunIDs(runIDs)
}

func runColorMapForRuns(runs []RunView) map[string]string {
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.RunID)
	}
	return runColorMapForRunIDs(runIDs)
}

func runColorMapForRunIDs(runIDs []string) map[string]string {
	seen := map[string]bool{}
	uniqueRunIDs := make([]string, 0, len(runIDs))
	for _, value := range runIDs {
		runID := strings.TrimSpace(value)
		if runID == "" || seen[runID] {
			continue
		}
		seen[runID] = true
		uniqueRunIDs = append(uniqueRunIDs, runID)
	}
	runIDs = uniqueRunIDs
	sort.Strings(runIDs)
	out := map[string]string{}
	used := map[string]bool{}
	for _, runID := range runIDs {
		color := colorForRunID(runID, used)
		out[runID] = color
		used[color] = true
	}
	return out
}

func applyRunColors(runs []RunView, runColors map[string]string) {
	for i := range runs {
		if color := runColors[runs[i].RunID]; color != "" {
			runs[i].Color = color
		}
	}
}

func colorForRunID(runID string, used map[string]bool) string {
	token := strings.TrimSpace(runID)
	if token == "" {
		token = "run"
	}
	hash := stableHash(token)
	if len(runColorPalette) > 0 {
		start := int(hash % uint32(len(runColorPalette)))
		for offset := 0; offset < len(runColorPalette); offset++ {
			color := runColorPalette[(start+offset)%len(runColorPalette)]
			if used == nil || !used[color] {
				return color
			}
		}
	}
	for salt := uint32(0); ; salt++ {
		color := fallbackRunColor(stableHash(fmt.Sprintf("%s:%d", token, salt)))
		if used == nil || !used[color] {
			return color
		}
	}
}

func stableHash(token string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(token))
	return hash.Sum32()
}

func fallbackRunColor(hash uint32) string {
	hue := float64(hash % 360)
	saturation := 0.68
	lightness := 0.42
	chroma := (1 - math.Abs(2*lightness-1)) * saturation
	x := chroma * (1 - math.Abs(math.Mod(hue/60, 2)-1))
	m := lightness - chroma/2

	var red, green, blue float64
	switch {
	case hue < 60:
		red, green, blue = chroma, x, 0
	case hue < 120:
		red, green, blue = x, chroma, 0
	case hue < 180:
		red, green, blue = 0, chroma, x
	case hue < 240:
		red, green, blue = 0, x, chroma
	case hue < 300:
		red, green, blue = x, 0, chroma
	default:
		red, green, blue = chroma, 0, x
	}
	return fmt.Sprintf("#%02x%02x%02x", colorChannel(red+m), colorChannel(green+m), colorChannel(blue+m))
}

func colorChannel(value float64) int {
	return int(math.Round(math.Max(0, math.Min(1, value)) * 255))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(math.Round(p * float64(len(sorted)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func runValue(run RunView, key string) string {
	switch key {
	case "code_sha":
		return run.CodeSHA
	case "config_hash":
		return run.ConfigHash
	case "image_digest":
		return run.ImageDigest
	case "tau_command":
		return run.TauCommand
	default:
		return ""
	}
}

func stringValue(row map[string]any, key string) string {
	value, ok := row[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func intValue(row map[string]any, key string) int {
	value, ok := row[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	case []byte:
		parsed, _ := strconv.Atoi(string(v))
		return parsed
	case string:
		parsed, _ := strconv.Atoi(v)
		return parsed
	default:
		parsed, _ := strconv.Atoi(fmt.Sprint(v))
		return parsed
	}
}

func int64Value(row map[string]any, key string) int64 {
	value, ok := row[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case []byte:
		parsed, _ := strconv.ParseInt(string(v), 10, 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(v, 10, 64)
		return parsed
	default:
		parsed, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		return parsed
	}
}

func optionalInt64Value(row map[string]any, key string) *int64 {
	value, ok := row[key]
	if !ok || value == nil {
		return nil
	}
	parsed := int64Value(row, key)
	return &parsed
}

func float64Value(row map[string]any, key string) float64 {
	value, ok := row[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case []byte:
		parsed, _ := strconv.ParseFloat(string(v), 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseFloat(v, 64)
		return parsed
	default:
		parsed, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
		return parsed
	}
}

func formatAny(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case float64:
		return formatFloat(v)
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func formatFloat(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "not collected"
	}
	return strconv.FormatFloat(value, 'f', 4, 64)
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func sqlIn(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, sqlQuote(value))
	}
	return strings.Join(quoted, ", ")
}

func sqlPlaceholders(count int) string {
	placeholders := make([]string, count)
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return strings.Join(placeholders, ", ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func cssClass(prefix, value string) string {
	safe := strings.Builder{}
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			safe.WriteRune(r)
		default:
			safe.WriteRune('-')
		}
	}
	return prefix + "-" + strings.Trim(safe.String(), "-")
}

func safeFilename(value string) string {
	name := strings.Trim(cssClass("", value), "-")
	if name == "" {
		return "cockpit"
	}
	return name
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

const htmlTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Tau experiment Stellar - {{.Target}}</title>
  <style>
    :root { color-scheme: light dark; --bg: #f8fafc; --panel: #ffffff; --ink: #0f172a; --muted: #64748b; --line: #cbd5e1; --accent: #2563eb; --warn: #b45309; }
    @media (prefers-color-scheme: dark) { :root { --bg: #020617; --panel: #0f172a; --ink: #e2e8f0; --muted: #94a3b8; --line: #334155; --accent: #60a5fa; --warn: #fbbf24; } }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--ink); font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    header { padding: 28px 32px 18px; border-bottom: 1px solid var(--line); background: var(--panel); }
    main { padding: 24px 32px 48px; max-width: 1280px; margin: 0 auto; }
    h1, h2, h3 { margin: 0 0 12px; line-height: 1.2; }
    h1 { font-size: 28px; }
    h2 { font-size: 20px; margin-top: 28px; }
    h3 { font-size: 16px; }
    .muted { color: var(--muted); }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 14px; }
    .sweep-layout { display: grid; grid-template-columns: minmax(220px, 280px) 1fr; gap: 16px; align-items: start; }
    @media (max-width: 880px) { .sweep-layout { grid-template-columns: 1fr; } }
    .toolbar { display: flex; flex-wrap: wrap; gap: 10px; align-items: end; margin: 10px 0 16px; }
    .toolbar label { display: grid; gap: 4px; color: var(--muted); font-size: 12px; font-weight: 650; }
    select, input[type="search"] { min-width: 180px; border: 1px solid var(--line); border-radius: 10px; padding: 8px 10px; background: var(--panel); color: var(--ink); font: inherit; }
    input[type="search"] { min-width: 240px; }
    .sweep-layout aside { position: sticky; top: 16px; }
    .run-list { display: grid; gap: 8px; max-height: 420px; overflow: auto; padding-right: 4px; }
    .run-chip { width: 100%; text-align: left; border-radius: 12px; background: color-mix(in oklab, var(--panel) 92%, var(--bg)); }
    .run-chip.selected { border-color: var(--accent); box-shadow: 0 0 0 2px color-mix(in oklab, var(--accent) 20%, transparent); }
    .dot { display: inline-block; width: 9px; height: 9px; border-radius: 50%; margin-right: 8px; vertical-align: middle; }
    .rank { display: inline-flex; min-width: 28px; color: var(--muted); }
    .mini-bar { height: 5px; margin-top: 8px; border-radius: 999px; background: color-mix(in oklab, var(--panel) 78%, var(--line)); overflow: hidden; }
    .mini-bar span { display: block; height: 100%; border-radius: inherit; }
    .best-value { font-size: 54px; line-height: 1; font-weight: 750; letter-spacing: -0.04em; }
    .parallel { min-height: 340px; }
    .parallel-line { fill: none; stroke-width: 1.7; opacity: 0.55; cursor: pointer; }
    .parallel-line:hover, .parallel-line.selected { stroke-width: 3.5; opacity: 1; }
    .axis-label { font-size: 11px; }
    .bar { height: 9px; border-radius: 999px; background: color-mix(in oklab, var(--panel) 78%, var(--line)); overflow: hidden; min-width: 80px; }
    .bar span { display: block; height: 100%; background: var(--accent); border-radius: inherit; }
    .panel { background: var(--panel); border: 1px solid var(--line); border-radius: 14px; padding: 16px; box-shadow: 0 1px 2px rgb(15 23 42 / 0.06); }
    .stat { font-size: 24px; font-weight: 700; }
    .summary-text { font-size: 16px; max-width: 860px; }
    .actions { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 16px; }
    button, .pill { border: 1px solid var(--line); background: color-mix(in oklab, var(--panel) 90%, var(--accent)); color: var(--ink); border-radius: 999px; padding: 7px 11px; cursor: pointer; font: inherit; }
    button:hover { border-color: var(--accent); }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; }
    pre { white-space: pre-wrap; word-break: break-word; background: color-mix(in oklab, var(--panel) 86%, var(--bg)); border: 1px solid var(--line); border-radius: 10px; padding: 12px; }
    table { width: 100%; border-collapse: collapse; font-size: 13px; }
    th, td { border-bottom: 1px solid var(--line); padding: 8px 6px; text-align: left; vertical-align: top; }
    th { color: var(--muted); font-weight: 650; }
    tr.selected { background: color-mix(in oklab, var(--accent) 12%, transparent); }
    .warning { border-color: var(--warn); color: var(--warn); }
    .chart { width: 100%; overflow-x: auto; }
    svg { max-width: 100%; background: color-mix(in oklab, var(--panel) 92%, var(--bg)); border: 1px solid var(--line); border-radius: 12px; }
    .series { fill: none; stroke-width: 2.5; cursor: pointer; opacity: 0.86; }
    .series:hover, .series.selected { stroke-width: 4; opacity: 1; }
    details { border: 1px solid var(--line); border-radius: 12px; padding: 12px 14px; background: var(--panel); margin: 10px 0; }
    summary { cursor: pointer; font-weight: 650; }
    .not_collected { color: var(--muted); font-style: italic; }
    .status { text-transform: uppercase; letter-spacing: 0.08em; font-size: 12px; font-weight: 800; color: var(--accent); }
    .hidden-by-filter { display: none; }
  </style>
</head>
<body>
<header>
  <h1>Tau experiment Stellar</h1>
  <div class="muted">schema {{.SchemaVersion}} - generated {{.GeneratedAt}} - store <code>{{.StorePath}}</code></div>
  <div class="actions">
    <button data-copy="{{.Actions.CopyCLI}}" onclick="copyText(this)">Copy CLI</button>
    <button data-copy="{{.Actions.CopySQL}}" onclick="copyText(this)">Copy SQL</button>
    <button onclick="downloadPNG()">Export PNG</button>
    <button data-copy="{{.Actions.ExportPacket}}" onclick="copyText(this)">Export Parquet packet</button>
    <button data-copy="{{.Actions.ObserveCLI}}" onclick="copyText(this)">Copy observation command</button>
    {{if .Actions.NextCommand}}<button data-copy="{{.Actions.NextCommand}}" onclick="copyText(this)">Copy next command</button>{{end}}
  </div>
</header>
<main>
  {{if .Warnings}}
  <section class="panel warning">
    <h2>Warnings</h2>
    <ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul>
  </section>
  {{end}}

  <section class="panel">
    <h2>Research loop summary</h2>
    <div class="grid">
      <div><div class="muted">Status</div><div class="status">{{.Summary.Status}}</div></div>
      <div><div class="muted">Confidence</div><div>{{.Summary.Confidence}}</div></div>
      <div><div class="muted">Seed coverage</div><div>{{.Summary.SeedCoverage}}</div></div>
      <div><div class="muted">Observations</div><div>{{.Summary.Decisions}} decisions, {{.Summary.Blockers}} blockers</div></div>
    </div>
    <h3>Current answer</h3>
    <p class="summary-text">{{.Summary.CurrentAnswer}}</p>
    <h3>Best evidence</h3>
    <p class="summary-text">{{.Summary.BestEvidence}}</p>
    <h3>Next action</h3>
    <p class="summary-text">{{.Summary.NextAction}}</p>
    {{if .Summary.NextCommand}}<pre>{{.Summary.NextCommand}}</pre>{{end}}
  </section>

  <section class="grid">
    <div class="panel"><div class="muted">Target</div><div class="stat">{{.Target}}</div><div class="muted">{{.TargetType}}</div></div>
    <div class="panel"><div class="muted">Best group</div><div class="stat">{{if .BestGroupID}}{{.BestGroupID}}{{else}}not collected{{end}}</div><div class="muted">from selected outcome metric</div></div>
    <div class="panel"><div class="muted">Seed coverage</div><div class="stat">{{.SeedCoverage}}</div><div class="muted">{{.Status.MetricFiles}} metric files, {{.Status.Observations}} observations</div></div>
    <div class="panel"><div class="muted">State counts</div><pre>{{range $state, $count := .Status.StateCounts}}{{$state}}: {{$count}}
{{end}}</pre></div>
  </section>

  <section class="panel">
    <h2>Active experiment</h2>
    {{if .Experiment}}
      <h3>{{.Experiment.Name}}</h3>
      <div class="muted">experiment_id <code>{{.Experiment.ExperimentID}}</code> - project <code>{{.Experiment.Project}}</code></div>
    {{else}}
      <div class="muted">not collected</div>
    {{end}}
  </section>

  <section class="panel">
    <h2>Hyperparameter sweep</h2>
    {{if .Sweep.HasData}}
    <div class="toolbar" aria-label="Sweep controls">
      <label>Metric selector
        <select id="metric-selector" onchange="changeMetric(this.value)">
          {{range .MetricOptions}}<option value="{{.Name}}" {{if .Selected}}selected{{end}}>{{.Name}} - {{.Card}}</option>{{end}}
        </select>
      </label>
      <label>Search runs
        <input id="run-search" type="search" placeholder="run, group, state, metric value">
      </label>
      <label>Filter group
        <select id="group-filter">
          <option value="">All groups</option>
          {{range .RunGroups}}<option value="{{.RunGroupID}}">{{.RunGroupID}}</option>{{end}}
        </select>
      </label>
    </div>
    <div class="sweep-layout">
      <aside>
        <h3>Runs ({{len .Sweep.Runs}} visualized)</h3>
        <div class="run-list">
          {{range .Sweep.Runs}}
          <button class="run-chip" data-run-chip="{{.RunID}}" data-group="{{.RunGroupID}}" onclick="selectRun('{{.RunID}}')">
            <span class="rank">#{{.Rank}}</span><span class="dot" style="background: {{.Color}}"></span><code>{{.RunID}}</code>
            <div class="muted">{{.RunGroupID}} - {{$.Sweep.MetricName}}={{.Metric}}</div>
            <div class="mini-bar" aria-hidden="true"><span style="width: {{.MetricWidth}}; background: {{.Color}}"></span></div>
          </button>
          {{end}}
        </div>
      </aside>
      <div>
        <div class="grid">
          <div class="panel">
            <div class="muted">Best run by {{.Sweep.MetricName}}</div>
            {{with .Sweep.BestRun}}
            <div class="best-value">{{.Value}}</div>
            <div><code>{{.RunID}}</code></div>
            <div class="muted">{{.RunGroupID}}</div>
            {{end}}
          </div>
          <div class="panel">
            <div class="muted">Config coverage</div>
            <div class="stat">{{.Sweep.ConfigCount}}/{{len .Sweep.Runs}}</div>
            <div class="muted">runs with indexed config values for sweep coordinates</div>
          </div>
        </div>
        {{if .Sweep.Series}}
        <h3>Parallel coordinates</h3>
        <div class="chart parallel">
          <svg id="sweep-parallel" width="800" height="340" viewBox="0 0 800 340" role="img" aria-label="Parallel coordinates for {{.Sweep.MetricName}}">
            <text x="28" y="20" fill="currentColor">Results of hyperparameter sweep - {{.Sweep.MetricName}}</text>
            {{range .Sweep.Axes}}
              <line x1="{{.X}}" y1="38" x2="{{.X}}" y2="300" stroke="currentColor" opacity="0.25"></line>
              <text class="axis-label" x="{{.X}}" y="32" text-anchor="middle" fill="currentColor">{{.Name}}</text>
              {{if eq .Kind "number"}}
                <text class="axis-label" x="{{.X}}" y="52" text-anchor="middle" fill="currentColor" opacity="0.65">{{.Max}}</text>
                <text class="axis-label" x="{{.X}}" y="316" text-anchor="middle" fill="currentColor" opacity="0.65">{{.Min}}</text>
              {{else}}
                <text class="axis-label" x="{{.X}}" y="52" text-anchor="middle" fill="currentColor" opacity="0.65">{{len .Values}} values</text>
              {{end}}
            {{end}}
            {{range .Sweep.Series}}<polyline class="parallel-line {{.GroupClass}}" data-group="{{.RunGroupID}}" data-run="{{.RunID}}" points="{{.Points}}" stroke="{{.Color}}"><title>{{.RunID}} / {{$.Sweep.MetricName}}={{.Metric}}</title></polyline>{{end}}
          </svg>
        </div>
        {{end}}
        {{if .Sweep.Importance}}
        <h3>Parameter importance with respect to {{.Sweep.MetricName}}</h3>
        <table>
          <thead><tr><th>Config parameter</th><th>Importance</th><th>Correlation</th></tr></thead>
          <tbody>{{range .Sweep.Importance}}<tr><td><code>{{.Name}}</code></td><td><div class="bar" title="{{.ImportanceLabel}}"><span style="width: {{.ImportanceWidth}}"></span></div></td><td><div>{{.CorrelationLabel}}</div><div class="bar" title="{{.CorrelationLabel}}"><span style="width: {{.CorrelationWidth}}"></span></div></td></tr>{{end}}</tbody>
        </table>
        {{end}}
      </div>
    </div>
    {{else}}
      <p class="muted">No sweep data was collected for this target yet. Import run configs and scalar metrics to enable parallel coordinates and best-run ranking.</p>
    {{end}}
  </section>

  <section class="panel">
    <h2>Run-group compare</h2>
    <p class="summary-text">{{.Compare.Summary}}</p>
    {{if .Chart.HasData}}
      <div class="chart">
        <svg id="seed-envelope-chart" width="800" height="260" viewBox="0 0 800 260" role="img" aria-label="Seed envelope chart for {{.Chart.MetricName}}">
          <text x="28" y="20" fill="currentColor">{{.Chart.MetricName}}</text>
          <line x1="28" y1="220" x2="760" y2="220" stroke="currentColor" opacity="0.35"></line>
          <line x1="28" y1="28" x2="28" y2="220" stroke="currentColor" opacity="0.35"></line>
          <text x="28" y="246" fill="currentColor" opacity="0.7">step {{.Chart.XMin}}</text>
          <text x="690" y="246" fill="currentColor" opacity="0.7">step {{.Chart.XMax}}</text>
          <text x="34" y="42" fill="currentColor" opacity="0.7">{{.Chart.YMax}}</text>
          <text x="34" y="214" fill="currentColor" opacity="0.7">{{.Chart.YMin}}</text>
          {{range .Chart.Series}}<polyline class="series {{.GroupClass}}" data-group="{{.RunGroupID}}" data-run="{{.RunID}}" points="{{.Points}}" stroke="{{.Color}}"><title>{{.RunGroupID}} / {{.RunID}}</title></polyline>{{end}}
        </svg>
      </div>
    {{else}}
      <p class="muted">No scalar metric chart data was collected for this target.</p>
    {{end}}
    {{if .Compare.Outliers}}
      <h3>Outlier seeds</h3>
      <table>
        <thead><tr><th>Run</th><th>Group</th><th>Value</th><th>Reason</th></tr></thead>
        <tbody>{{range .Compare.Outliers}}<tr data-group-row="{{.RunGroupID}}" onclick="selectGroup('{{.RunGroupID}}')"><td><code>{{.RunID}}</code></td><td><code>{{.RunGroupID}}</code></td><td>{{.Value}}</td><td>{{.Reason}}</td></tr>{{end}}</tbody>
      </table>
    {{end}}
    {{if .Compare.EventMarkers}}
      <h3>Event markers</h3>
      <table>
        <thead><tr><th>Time</th><th>Run</th><th>Group</th><th>Type</th><th>Severity</th><th>Message</th></tr></thead>
        <tbody>{{range .Compare.EventMarkers}}<tr data-group-row="{{.RunGroupID}}" onclick="selectGroup('{{.RunGroupID}}')"><td>{{.Time}}</td><td><code>{{.RunID}}</code></td><td><code>{{.RunGroupID}}</code></td><td>{{.Type}}</td><td>{{.Severity}}</td><td>{{.Message}}</td></tr>{{end}}</tbody>
      </table>
    {{end}}
    {{if .Compare.RuntimeDiffs}}
      <h3>Runtime/config diffs</h3>
      <table>
        <thead><tr><th>Field</th><th>Run group values</th></tr></thead>
        <tbody>{{range .Compare.RuntimeDiffs}}<tr><td>{{.Field}}</td><td>{{range .Values}}<div><code>{{.RunGroupID}}</code>: {{.Value}}</div>{{end}}</td></tr>{{end}}</tbody>
      </table>
    {{end}}
    {{range .Cards}}
      {{range .Metrics}}
        {{if eq $.Chart.MetricName .Name}}
        <h3>Seed envelope: {{.Name}}</h3>
        <table>
          <thead><tr><th>Run group</th><th>Runs</th><th>Latest step</th><th>Min</th><th>P25</th><th>Median</th><th>P75</th><th>Max</th><th>Best</th></tr></thead>
          <tbody>{{range .Groups}}<tr data-group-row="{{.RunGroupID}}" onclick="selectGroup('{{.RunGroupID}}')"><td><code>{{.RunGroupID}}</code></td><td>{{.RunCount}}</td><td>{{.LatestStep}}</td><td>{{.Min}}</td><td>{{.P25}}</td><td>{{.Median}}</td><td>{{.P75}}</td><td>{{.Max}}</td><td>{{.Best}}</td></tr>{{end}}</tbody>
        </table>
        {{end}}
      {{end}}
    {{end}}
  </section>

  <section class="panel">
    <h2>Canonical evidence cards</h2>
    {{if .Cards}}
      {{range .Cards}}
      <details open>
        <summary>{{.Name}}</summary>
        {{range .Metrics}}
          <h3>{{.Name}}{{if .Unit}} <span class="muted">({{.Unit}})</span>{{end}}</h3>
          <table>
            <thead><tr><th>Run group</th><th>Runs</th><th>Median</th><th>Best</th><th>Min</th><th>Max</th></tr></thead>
            <tbody>{{range .Groups}}<tr data-group-row="{{.RunGroupID}}" onclick="selectGroup('{{.RunGroupID}}')"><td><code>{{.RunGroupID}}</code></td><td>{{.RunCount}}</td><td>{{.Median}}</td><td>{{.Best}}</td><td>{{.Min}}</td><td>{{.Max}}</td></tr>{{end}}</tbody>
          </table>
        {{end}}
      </details>
      {{end}}
    {{else}}
      <p class="muted">No canonical card metrics were collected.</p>
    {{end}}
  </section>

  <section class="panel">
    <h2>Run detail cards</h2>
    <table id="run-table">
      <thead><tr><th>Run</th><th>Group</th><th>State</th><th>Owner</th><th>Created</th><th>Completed</th></tr></thead>
      <tbody>{{range .Runs}}<tr data-run-row="{{.RunID}}" data-group="{{.RunGroupID}}"><td><code>{{.RunID}}</code></td><td><code>{{.RunGroupID}}</code></td><td>{{.State}}</td><td>{{.Owner}}</td><td>{{.CreatedAt}}</td><td>{{.CompletedAt}}</td></tr>{{end}}</tbody>
    </table>
    {{range .Runs}}
    <details data-run-detail="{{.RunID}}" data-group="{{.RunGroupID}}">
      <summary>{{.RunID}} - {{.RunGroupID}} - {{.State}}</summary>
      <h3>Systems</h3>
      <table>
        <tbody>{{range .Systems}}<tr><th>{{.Name}}</th><td class="{{.CollectionState}}">{{.Value}}</td></tr>{{end}}</tbody>
      </table>
      <h3>Configs</h3>
      {{if .Configs}}<ul>{{range .Configs}}<li><code>{{.Format}}</code> {{.URI}} - <code>{{.ConfigHash}}</code></li>{{end}}</ul>{{else}}<p class="muted">not collected</p>{{end}}
      <h3>Artifacts</h3>
      {{if .Artifacts}}<ul>{{range .Artifacts}}<li><code>{{.Type}}</code> {{.Name}} - <code>{{.URI}}</code></li>{{end}}</ul>{{else}}<p class="muted">not collected</p>{{end}}
      <h3>Events</h3>
      {{if .Events}}<ul>{{range .Events}}<li>{{.Time}} <code>{{.Type}}</code> {{.Severity}} - {{.Message}}</li>{{end}}</ul>{{else}}<p class="muted">not collected</p>{{end}}
      <h3>Observations</h3>
      {{if .Observations}}<ul>{{range .Observations}}<li><strong>{{.Type}}</strong> {{.Text}} <span class="muted">({{.Author}}, {{.CreatedAt}})</span></li>{{end}}</ul>{{else}}<p class="muted">not collected</p>{{end}}
      <button data-copy="{{.ObserveCLI}}" onclick="copyText(this)">Copy run observation command</button>
    </details>
    {{end}}
  </section>

  <section class="panel">
    <h2>Observation notebook</h2>
    {{if .Observations}}
      {{range .Observations}}
      <article class="panel">
        <div><strong>{{.Type}}</strong> on <code>{{.ScopeType}}:{{.ScopeID}}</code></div>
        <p>{{.Text}}</p>
        <div class="muted">{{.Author}} / {{.Source}} / {{.CreatedAt}}{{if .IdempotencyKey}} / idempotency <code>{{.IdempotencyKey}}</code>{{end}}</div>
        {{if .Evidence}}<pre>{{.Evidence}}</pre>{{end}}
      </article>
      {{end}}
    {{else}}
      <p class="muted">No observations yet. Use the copied observation command so notes persist in the experiment store instead of browser state.</p>
    {{end}}
  </section>

  <section class="panel">
    <h2>Local/offline access</h2>
    <p>All data displayed above is read from the local packet/index and metric Parquet files.</p>
    <pre>{{.Actions.CopySQL}}</pre>
  </section>
</main>
<script>
function copyText(button) {
  const text = button.dataset.copy || "";
  if (!text) return;
  navigator.clipboard?.writeText(text).then(() => {
    const old = button.textContent;
    button.textContent = "Copied";
    setTimeout(() => button.textContent = old, 1000);
  });
}
function changeMetric(metric) {
  const url = new URL(window.location.href);
  url.searchParams.set("target", "{{.Target}}");
  url.searchParams.set("metric", metric);
  window.location.href = url.toString();
}
function runTextMatches(element, text, group) {
  const elementGroup = element.dataset.group || "";
  const run = element.dataset.run || element.dataset.runRow || element.dataset.runDetail || element.dataset.runChip || "";
  if (group && elementGroup !== group) return false;
  if (!text) return true;
  return run.toLowerCase().includes(text) || element.textContent.toLowerCase().includes(text);
}
function lineMatches(line, text, group) {
  const lineGroup = line.dataset.group || "";
  const run = line.dataset.run || "";
  if (group && lineGroup !== group) return false;
  if (!text) return true;
  return run.toLowerCase().includes(text);
}
function applyRunFilters() {
  const text = (document.getElementById("run-search")?.value || "").trim().toLowerCase();
  const group = document.getElementById("group-filter")?.value || "";
  document.querySelectorAll("[data-run-row], [data-run-detail], [data-run-chip]").forEach(row => {
    row.classList.toggle("hidden-by-filter", !runTextMatches(row, text, group));
  });
  document.querySelectorAll(".series, .parallel-line").forEach(line => {
    line.classList.toggle("hidden-by-filter", !lineMatches(line, text, group));
  });
}
function selectGroup(group) {
  const groupFilter = document.getElementById("group-filter");
  if (groupFilter) groupFilter.value = group;
  applyRunFilters();
  document.querySelectorAll("[data-group-row]").forEach(row => row.classList.toggle("selected", row.dataset.groupRow === group));
  document.querySelectorAll(".series").forEach(line => line.classList.toggle("selected", line.dataset.group === group));
  document.querySelectorAll(".parallel-line").forEach(line => line.classList.toggle("selected", line.dataset.group === group));
}
document.querySelectorAll(".series").forEach(line => line.addEventListener("click", () => selectGroup(line.dataset.group)));
function selectRun(run) {
  document.querySelectorAll("[data-run-row], [data-run-detail], [data-run-chip]").forEach(row => row.classList.toggle("selected", row.dataset.runRow === run || row.dataset.runDetail === run || row.dataset.runChip === run));
  document.querySelectorAll(".series, .parallel-line").forEach(line => line.classList.toggle("selected", line.dataset.run === run));
}
document.querySelectorAll(".parallel-line").forEach(line => line.addEventListener("click", () => selectRun(line.dataset.run)));
document.getElementById("run-search")?.addEventListener("input", applyRunFilters);
document.getElementById("group-filter")?.addEventListener("change", applyRunFilters);
function downloadPNG() {
  const svg = document.getElementById("seed-envelope-chart");
  if (!svg) return;
  const xml = new XMLSerializer().serializeToString(svg);
  const image = new Image();
  const blob = new Blob([xml], { type: "image/svg+xml;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  image.onload = () => {
    const canvas = document.createElement("canvas");
    canvas.width = svg.viewBox.baseVal.width || 800;
    canvas.height = svg.viewBox.baseVal.height || 260;
    const ctx = canvas.getContext("2d");
    ctx.fillStyle = getComputedStyle(document.body).backgroundColor;
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(image, 0, 0);
    URL.revokeObjectURL(url);
    const link = document.createElement("a");
    link.download = "{{.Target}}-seed-envelope.png";
    link.href = canvas.toDataURL("image/png");
    link.click();
  };
  image.src = url;
}
</script>
</body>
</html>
`
