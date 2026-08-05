package cli

import (
	"time"

	"github.com/Azure/taugrid/portal/internal/expimport"
	"github.com/Azure/taugrid/portal/internal/jsonlutil"
)

type metricsOffloadOptions struct {
	Out                     string
	History                 []string
	RunID                   string
	Project                 string
	ExperimentID            string
	RunGroupID              string
	Source                  string
	Tags                    []string
	CompletionFile          string
	ArtifactURI             string
	CheckpointURI           string
	BaselineExistingHistory bool
	ReadyFile               string
	DoneFile                string
	ShutdownCompletionWait  time.Duration
	FinalizeHistory         bool
	RemoteWrite             remoteWriteConfig
}

type metricsOffloadResult struct {
	Mode                 string                       `json:"mode"`
	Rows                 int                          `json:"rows"`
	ImportRows           int                          `json:"import_rows,omitempty"`
	Imports              int                          `json:"imports,omitempty"`
	MetricsFile          string                       `json:"metrics_file,omitempty"`
	ImportedMetricFiles  []string                     `json:"imported_metric_files,omitempty"`
	CheckpointFile       string                       `json:"checkpoint_file,omitempty"`
	OnlineCheckpointFile string                       `json:"online_checkpoint_file,omitempty"`
	SourceStoreID        string                       `json:"source_store_id,omitempty"`
	ExportedAt           string                       `json:"exported_at,omitempty"`
	RemoteWrite          *expimport.RemoteWriteResult `json:"remote_write,omitempty"`
	RemoteWriteSamples   int                          `json:"remote_write_samples,omitempty"`
	Completed            bool                         `json:"completed,omitempty"`
	StatusState          string                       `json:"status_state,omitempty"`
	StatusMetricsFile    string                       `json:"status_metrics_file,omitempty"`
}

func (result *metricsOffloadResult) addOnlineChunk(chunk metricsOnlineChunkResult) {
	if chunk.Imported {
		result.Imports++
	}
	result.ImportRows += chunk.ImportRows
	if chunk.ImportedMetricFileID != "" {
		result.ImportedMetricFiles = append(result.ImportedMetricFiles, chunk.ImportedMetricFileID)
	}
	result.Rows += chunk.ExportRows
	if chunk.MetricsFile != "" {
		result.MetricsFile = chunk.MetricsFile
	}
	if chunk.SourceStoreID != "" {
		result.SourceStoreID = chunk.SourceStoreID
	}
	if chunk.ExportedAt != "" {
		result.ExportedAt = chunk.ExportedAt
	}
	if chunk.RemoteWrite != nil {
		result.RemoteWriteSamples += chunk.RemoteWrite.Samples
		result.RemoteWrite = chunk.RemoteWrite
	}
}

func (result *metricsOffloadResult) addStatusMarker(status metricsStatusMarkerResult) {
	if status.State != "" {
		result.StatusState = status.State
	}
	if status.MetricsFile != "" {
		result.StatusMetricsFile = status.MetricsFile
	}
	if status.RemoteWrite != nil {
		result.RemoteWriteSamples += status.RemoteWrite.Samples
		result.RemoteWrite = status.RemoteWrite
	}
	if status.Samples > 0 {
		result.Rows += status.Samples
	}
}

type metricsOnlineChunkResult struct {
	Imported             bool
	ImportRows           int
	ImportedMetricFileID string
	ExportRows           int
	MetricsFile          string
	SourceStoreID        string
	ExportedAt           string
	RemoteWrite          *expimport.RemoteWriteResult
}

type metricsOffloadCheckpoint struct {
	SchemaVersion string `json:"schema_version"`
	SourceStoreID string `json:"source_store_id"`
	ExportedAt    string `json:"exported_at"`
	MetricsFile   string `json:"metrics_file"`
	Rows          int    `json:"rows"`
	UpdatedAt     string `json:"updated_at"`
}

const metricsJSONLCheckpointSchemaVersion = "tau.metrics.jsonl_checkpoint.v1"

type metricsHistoryChunk = jsonlutil.HistoryChunk

type metricsOffloadAgentManifestOptions struct {
	Name                    string
	Namespace               string
	Image                   string
	PVC                     string
	MountPath               string
	Store                   string
	History                 string
	Run                     string
	Out                     string
	Project                 string
	Experiment              string
	Group                   string
	Source                  string
	Tags                    []string
	Interval                string
	MaxIterations           int
	CompletionFile          string
	RemoteWriteEndpoint     string
	RemoteWriteMaxAttempts  int
	RemoteWriteRetryBackoff string
	ServiceAccount          string
	CPURequest              string
	MemoryRequest           string
	CPULimit                string
	MemoryLimit             string
	NodeSelectors           []string
}
