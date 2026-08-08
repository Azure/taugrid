// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package expstore implements Tau's local experiment-tracking store.
package expstore

import "errors"

const (
	SchemaVersion = "expstore.v5"
	StoreKind     = "tau.exp.store"

	ManifestFile = "manifest.json"
	IndexFile    = "index.sqlite"
	AppendLogDir = "append-log"
	MetricsDir   = "metrics"
	ArtifactsDir = "artifacts"
)

var (
	ErrConflict = errors.New("experiment store write conflict")
	ErrNotFound = errors.New("experiment store record not found")
)

type Manifest struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
	Project       string `json:"project,omitempty"`
	// ExperimentID is the store's default experiment -- the name passed to
	// `experiment init --name`. Runs tracked into this store join it unless they
	// name a different experiment.
	ExperimentID string `json:"experiment_id,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Index        string `json:"index"`
	AppendLogDir string `json:"append_log_dir"`
	MetricsDir   string `json:"metrics_dir"`
	ArtifactsDir string `json:"artifacts_dir"`
}

type InitOptions struct {
	Name           string
	Project        string
	Description    string
	Group          string
	IdempotencyKey string
}

type InitResult struct {
	StorePath  string           `json:"store_path"`
	Manifest   Manifest         `json:"manifest"`
	Experiment ExperimentRecord `json:"experiment"`
	RunGroup   RunGroup         `json:"run_group"`
	Created    bool             `json:"created"`
	Reused     bool             `json:"reused"`
}

type RunGroup struct {
	RunGroupID string `json:"run_group_id"`
	Project    string `json:"project"`
	// A run group is an arm inside an experiment (baseline vs ablation), not a
	// level of its own. It deliberately carries no experiment_id: the same arm
	// label is reusable across experiments, so ownership lives on the run.
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ExperimentRecord struct {
	ExperimentID string `json:"experiment_id"`
	Project      string `json:"project"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Source       string `json:"source"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type RunRecord struct {
	RunID        string `json:"run_id"`
	Project      string `json:"project"`
	ExperimentID string `json:"experiment_id,omitempty"`
	RunGroupID   string `json:"run_group_id"`
	ParentRunID  string `json:"parent_run_id,omitempty"`
	State        string `json:"state"`
	Owner        string `json:"owner,omitempty"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
	ConfigHash   string `json:"config_hash,omitempty"`
	CodeSHA      string `json:"code_sha,omitempty"`
	ImageDigest  string `json:"image_digest,omitempty"`
	TauCommand   string `json:"tau_command,omitempty"`
	ResultURI    string `json:"result_uri,omitempty"`
	IndexVersion string `json:"index_version"`
}

type ArtifactRecord struct {
	ArtifactID           string `json:"artifact_id"`
	RunID                string `json:"run_id"`
	Type                 string `json:"type"`
	URI                  string `json:"uri"`
	Name                 string `json:"name"`
	DurableRef           string `json:"durable_ref,omitempty"`
	ContentType          string `json:"content_type,omitempty"`
	Digest               string `json:"digest,omitempty"`
	SizeBytes            *int64 `json:"size_bytes,omitempty"`
	Step                 *int64 `json:"step,omitempty"`
	Tags                 string `json:"tags,omitempty"`
	Rank                 *int64 `json:"rank,omitempty"`
	CreatedAt            string `json:"created_at"`
	Preview              string `json:"preview,omitempty"`
	ExternalRef          string `json:"external_ref,omitempty"`
	Caption              string `json:"caption,omitempty"`
	Direction            string `json:"direction,omitempty"`
	Alias                string `json:"alias,omitempty"`
	SourceArtifactID     string `json:"source_artifact_id,omitempty"`
	SourceRunID          string `json:"source_run_id,omitempty"`
	SourceDatasetName    string `json:"source_dataset_name,omitempty"`
	SourceDatasetVersion string `json:"source_dataset_version,omitempty"`
	SourceDatasetDigest  string `json:"source_dataset_digest,omitempty"`
}

type RunContextRecord struct {
	RunID            string   `json:"run_id"`
	Cluster          string   `json:"cluster,omitempty"`
	Namespace        string   `json:"namespace,omitempty"`
	Team             string   `json:"team,omitempty"`
	Profile          string   `json:"profile,omitempty"`
	Lane             string   `json:"lane,omitempty"`
	LocalQueue       string   `json:"local_queue,omitempty"`
	ClusterQueue     string   `json:"cluster_queue,omitempty"`
	KueueWorkload    string   `json:"kueue_workload,omitempty"`
	PodUID           string   `json:"pod_uid,omitempty"`
	RayJob           string   `json:"ray_job,omitempty"`
	ResourceClaims   string   `json:"resource_claims,omitempty"`
	GPUClass         string   `json:"gpu_class,omitempty"`
	GPUCount         *int64   `json:"gpu_count,omitempty"`
	NodeNames        string   `json:"node_names,omitempty"`
	Mounts           string   `json:"mounts,omitempty"`
	QueueWaitSeconds *float64 `json:"queue_wait_seconds,omitempty"`
	GPUHours         *float64 `json:"gpu_hours,omitempty"`
	EstimatedCost    *float64 `json:"estimated_cost,omitempty"`
	Runtime          string   `json:"runtime,omitempty"`
	Dependencies     string   `json:"dependencies,omitempty"`
	LogURI           string   `json:"log_uri,omitempty"`
}

type ConfigRecord struct {
	ConfigHash     string `json:"config_hash"`
	RunID          string `json:"run_id"`
	Format         string `json:"format"`
	URI            string `json:"uri"`
	NormalizedJSON string `json:"normalized_json,omitempty"`
	IndexedFields  string `json:"indexed_fields,omitempty"`
}

type MetricFileRecord struct {
	FileID        string `json:"file_id"`
	Path          string `json:"path"`
	Format        string `json:"format"`
	SchemaVersion string `json:"schema_version"`
	SchemaHash    string `json:"schema_hash,omitempty"`
	Project       string `json:"project,omitempty"`
	RunGroupID    string `json:"run_group_id,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	RowCount      int64  `json:"row_count"`
	Digest        string `json:"digest,omitempty"`
	MinStep       *int64 `json:"min_step,omitempty"`
	MaxStep       *int64 `json:"max_step,omitempty"`
	CreatedAt     string `json:"created_at"`
}

type MetricSummaryRecord struct {
	FileID         string  `json:"file_id,omitempty"`
	RunID          string  `json:"run_id"`
	Project        string  `json:"project,omitempty"`
	RunGroupID     string  `json:"run_group_id,omitempty"`
	MetricName     string  `json:"metric_name"`
	Count          int64   `json:"count"`
	FiniteCount    int64   `json:"finite_count"`
	NonFiniteCount int64   `json:"non_finite_count,omitempty"`
	MinStep        *int64  `json:"min_step,omitempty"`
	MaxStep        *int64  `json:"max_step,omitempty"`
	LatestStep     *int64  `json:"latest_step,omitempty"`
	LatestWallTime *int64  `json:"latest_wall_time,omitempty"`
	LatestValue    float64 `json:"latest_value,omitempty"`
	MinValue       float64 `json:"min_value,omitempty"`
	MaxValue       float64 `json:"max_value,omitempty"`
	UpdatedAt      string  `json:"updated_at"`
	LatestFileID   string  `json:"latest_file_id,omitempty"`
}

type TagRecord struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

type EventRecord struct {
	EventID  string `json:"event_id"`
	RunID    string `json:"run_id"`
	Time     string `json:"time"`
	Type     string `json:"type"`
	Source   string `json:"source"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Payload  string `json:"payload,omitempty"`
}

type RecordRunDataOptions struct {
	Run             RunRecord
	RunContext      *RunContextRecord
	Configs         []ConfigRecord
	Tags            []TagRecord
	Artifacts       []ArtifactRecord
	MetricFiles     []MetricFileRecord
	MetricSummaries []MetricSummaryRecord
	IdempotencyKey  string
	Command         string
	RequestHash     string
}

type RecordRunDataResult struct {
	RunID           string `json:"run_id"`
	CreatedRun      bool   `json:"created_run"`
	Reused          bool   `json:"reused"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
	RunContext      bool   `json:"run_context"`
	Configs         int    `json:"configs"`
	MetricFiles     int    `json:"metric_files"`
	MetricSummaries int    `json:"metric_summaries"`
	Artifacts       int    `json:"artifacts"`
	Tags            int    `json:"tags"`
}

type EnrichRunDataOptions struct {
	Run        RunRecord
	RunContext *RunContextRecord
	Tags       []TagRecord
	Events     []EventRecord
	Command    string
}

type EnrichRunDataResult struct {
	RunID             string `json:"run_id"`
	CreatedRun        bool   `json:"created_run"`
	UpdatedRun        bool   `json:"updated_run"`
	CreatedRunContext bool   `json:"created_run_context"`
	UpdatedRunContext bool   `json:"updated_run_context"`
	Events            int    `json:"events"`
	Tags              int    `json:"tags"`
	Reused            bool   `json:"reused"`
}

type ObservationRecord struct {
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

type RecordObservationOptions struct {
	Observation    ObservationRecord
	IdempotencyKey string
	Command        string
	RequestHash    string
}

type RecordObservationResult struct {
	ObservationID  string `json:"observation_id"`
	ScopeType      string `json:"scope_type"`
	ScopeID        string `json:"scope_id"`
	Type           string `json:"type"`
	Created        bool   `json:"created"`
	Reused         bool   `json:"reused"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type MetricRow struct {
	Project    string  `parquet:"project" json:"project"`
	RunGroupID string  `parquet:"run_group_id" json:"run_group_id"`
	RunID      string  `parquet:"run_id" json:"run_id"`
	MetricName string  `parquet:"metric_name" json:"metric_name"`
	Step       *int64  `parquet:"step,optional" json:"step,omitempty"`
	WallTime   *int64  `parquet:"wall_time,timestamp(microsecond),optional" json:"wall_time,omitempty"`
	Value      float64 `parquet:"value" json:"value"`
	Unit       *string `parquet:"unit,optional" json:"unit,omitempty"`
	Source     string  `parquet:"source" json:"source"`
	Split      *string `parquet:"split,optional" json:"split,omitempty"`
	Tags       string  `parquet:"tags,json" json:"tags"`
}

type ListOptions struct {
	Kind       string
	Project    string
	RunGroupID string
	State      string
	Tags       map[string]string
}

type MetricFilter struct {
	MetricName string  `json:"metric_name"`
	Field      string  `json:"field,omitempty"`
	Op         string  `json:"op"`
	Value      float64 `json:"value"`
}

type RunSearchOptions struct {
	Target        string
	Workspace     string
	Query         string
	Project       string
	RunGroupID    string
	State         string
	Lifecycle     string
	Tags          map[string]string
	MetricNames   []string
	MetricFilters []MetricFilter
	Since         string
	Limit         int
	MinStep       *int64
}

type RunClassification struct {
	LifecycleState string   `json:"lifecycle_state"`
	Successful     bool     `json:"successful"`
	Reasons        []string `json:"reasons,omitempty"`
}

type SuccessOptions struct {
	Tags          map[string]string
	MetricFilters []MetricFilter
	MinStep       *int64
}

type RunSearchRun struct {
	RunRecord
	LifecycleState string                `json:"lifecycle_state"`
	Successful     bool                  `json:"successful"`
	SuccessReasons []string              `json:"success_reasons,omitempty"`
	Tags           map[string]string     `json:"tags,omitempty"`
	MetricNames    []string              `json:"metric_names,omitempty"`
	Metrics        []MetricSummaryRecord `json:"metrics,omitempty"`
}

type RunSearchResult struct {
	SchemaVersion string         `json:"schema_version"`
	GeneratedAt   string         `json:"generated_at"`
	StorePath     string         `json:"store_path"`
	Target        string         `json:"target,omitempty"`
	Total         int            `json:"total"`
	Truncated     bool           `json:"truncated,omitempty"`
	Runs          []RunSearchRun `json:"runs"`
	Warnings      []string       `json:"warnings,omitempty"`
}

type ExperimentSearchOptions struct {
	Query         string
	Workspace     string
	Project       string
	Lifecycle     string
	Tags          map[string]string
	MetricNames   []string
	MetricFilters []MetricFilter
	Since         string
	Limit         int
}

type ExperimentSummary struct {
	ExperimentRecord
	RunCount        int            `json:"run_count"`
	RunGroupCount   int            `json:"run_group_count"`
	StateCounts     map[string]int `json:"state_counts,omitempty"`
	LifecycleCounts map[string]int `json:"lifecycle_counts,omitempty"`
	LatestRunAt     string         `json:"latest_run_at,omitempty"`
	MetricNames     []string       `json:"metric_names,omitempty"`
}

type ExperimentSearchResult struct {
	SchemaVersion string              `json:"schema_version"`
	GeneratedAt   string              `json:"generated_at"`
	StorePath     string              `json:"store_path"`
	Total         int                 `json:"total"`
	Truncated     bool                `json:"truncated,omitempty"`
	Experiments   []ExperimentSummary `json:"experiments"`
	Warnings      []string            `json:"warnings,omitempty"`
}

type QueryResult struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

type Status struct {
	StorePath       string            `json:"store_path"`
	Target          string            `json:"target"`
	TargetType      string            `json:"target_type"`
	RunGroup        *RunGroup         `json:"run_group,omitempty"`
	Experiment      *ExperimentRecord `json:"experiment,omitempty"`
	Run             map[string]any    `json:"run,omitempty"`
	Runs            int               `json:"runs"`
	RunGroups       int               `json:"run_groups"`
	StateCounts     map[string]int    `json:"state_counts"`
	LifecycleCounts map[string]int    `json:"lifecycle_counts,omitempty"`
	Configs         int               `json:"configs"`
	MetricFiles     int               `json:"metric_files"`
	Artifacts       int               `json:"artifacts"`
	Observations    int               `json:"observations"`
	LatestEventAt   string            `json:"latest_event_at,omitempty"`
}

type ExportOptions struct {
	Out   string
	Force bool
}

type ExportResult struct {
	Source        string   `json:"source"`
	Destination   string   `json:"destination"`
	FilesCopied   int      `json:"files_copied"`
	DirsCreated   int      `json:"dirs_created"`
	PacketFiles   []string `json:"packet_files"`
	SchemaVersion string   `json:"schema_version"`
}
