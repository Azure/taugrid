// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import "time"

const (
	SchemaVersion             = "rdma-validation.v1"
	Kind                      = "tau.rdma_validation"
	ArtifactType              = "rdma-validation"
	ArtifactContentType       = "application/vnd.tau.rdma-validation.v1+json"
	RunKindTag                = "tau.validation.kind"
	MetricValidationIDTag     = "tau.rdma_validation.validation_id"
	MetricSchemaTag           = "tau.rdma_validation.schema"
	MetricKindTag             = "tau.rdma_validation.kind"
	MetricLifecycleStateTag   = "tau.rdma_validation.lifecycle_state"
	MetricValidationStatusTag = "tau.rdma_validation.status"
	MetricValidationReasonTag = "tau.rdma_validation.reason"
	MetricArtifactURITag      = "tau.rdma_validation.artifact_uri"
	MetricArtifactSHA256Tag   = "tau.rdma_validation.artifact_sha256"
	DefaultStaleAfterSeconds  = int64(24 * time.Hour / time.Second)
)

const (
	RunStatePending   = "pending"
	RunStateRunning   = "running"
	RunStateSucceeded = "succeeded"
	RunStateFailed    = "failed"
)

type Status string

const (
	StatusPass    Status = "pass"
	StatusFail    Status = "fail"
	StatusUnknown Status = "unknown"
)

type CleanupState string

const (
	CleanupComplete   CleanupState = "complete"
	CleanupIncomplete CleanupState = "incomplete"
	CleanupUnknown    CleanupState = "unknown"
)

type ReasonCode string

const (
	ReasonValidationPassed         ReasonCode = "validation_passed"
	ReasonSocketFallbackObserved   ReasonCode = "socket_fallback_observed"
	ReasonIBTransportNotProven     ReasonCode = "ib_transport_not_proven"
	ReasonTransportFailure         ReasonCode = "transport_failure"
	ReasonPeerAuthenticationFailed ReasonCode = "peer_authentication_failed"
	ReasonPlacementMismatch        ReasonCode = "placement_mismatch"
	ReasonTopologyMismatch         ReasonCode = "topology_mismatch"
	ReasonCorrectnessError         ReasonCode = "correctness_error"
	ReasonNonzeroExit              ReasonCode = "nonzero_exit"
	ReasonCleanupIncomplete        ReasonCode = "cleanup_incomplete"
	ReasonMissingRequiredEvidence  ReasonCode = "missing_required_evidence"
	ReasonInvalidMeasurement       ReasonCode = "invalid_measurement"
	ReasonEvidenceIntegrityMissing ReasonCode = "evidence_integrity_missing"
	ReasonParserRejected           ReasonCode = "parser_rejected"
	ReasonRuntimeError             ReasonCode = "runtime_error"
)

type Result struct {
	Schema string `json:"schema"`
	Kind   string `json:"kind"`

	ValidationID string `json:"validation_id"`
	RunID        string `json:"run_id"`
	Attempt      int    `json:"attempt"`
	WorkspaceID  string `json:"workspace_id"`
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	ProjectID    string `json:"project_id,omitempty"`
	ExperimentID string `json:"experiment_id,omitempty"`
	RunGroupID   string `json:"run_group_id,omitempty"`

	CreatedAt         time.Time `json:"created_at"`
	StartedAt         time.Time `json:"started_at"`
	AdmittedAt        time.Time `json:"admitted_at"`
	CompletedAt       time.Time `json:"completed_at"`
	ObservedAt        time.Time `json:"observed_at"`
	StaleAfterSeconds int64     `json:"stale_after_seconds"`
	ValidUntil        time.Time `json:"valid_until"`

	Source       Source            `json:"source"`
	Image        ImageProvenance   `json:"image"`
	Requested    Requested         `json:"requested"`
	Actual       Actual            `json:"actual"`
	Placement    Placement         `json:"placement"`
	Pods         []PodResult       `json:"pods"`
	Ranks        []RankResult      `json:"ranks"`
	NCCL         NCCL              `json:"nccl"`
	Transport    Transport         `json:"transport"`
	Measurements Measurements      `json:"measurements"`
	Correctness  Correctness       `json:"correctness"`
	Status       Status            `json:"status"`
	Reason       ReasonCode        `json:"reason"`
	Errors       []ValidationError `json:"errors"`
	JobExitCode  *int              `json:"job_exit_code"`
	RankExits    []RankExit        `json:"rank_exits"`
	Cleanup      Cleanup           `json:"cleanup"`
	Evidence     []EvidenceRef     `json:"evidence"`
	Producer     ComponentVersion  `json:"producer"`
	Parser       ComponentVersion  `json:"parser"`
}

type Source struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

type ImageProvenance struct {
	Repository              string `json:"repository"`
	IndexDigest             string `json:"index_digest"`
	PlatformDigest          string `json:"platform_digest"`
	ConfigDigest            string `json:"config_digest"`
	SBOMManifestDigest      string `json:"sbom_manifest_digest"`
	SBOMLayerDigest         string `json:"sbom_layer_digest"`
	VEXManifestDigest       string `json:"vex_manifest_digest"`
	VEXLayerDigest          string `json:"vex_layer_digest"`
	SignatureManifestDigest string `json:"signature_manifest_digest"`
	SignatureLayerDigest    string `json:"signature_layer_digest"`
	SignatureTrustVerified  *bool  `json:"signature_trust_verified"`
}

type Requested struct {
	Topology   RequestedTopology   `json:"topology"`
	Resources  RequestedResources  `json:"resources"`
	Parameters BenchmarkParameters `json:"parameters"`
}

type RequestedTopology struct {
	NodeCount        int    `json:"node_count"`
	PodCount         int    `json:"pod_count"`
	RankCount        int    `json:"rank_count"`
	DistinctHostname bool   `json:"distinct_hostname"`
	Site             string `json:"site"`
	Pool             string `json:"pool"`
	GPUModel         string `json:"gpu_model"`
}

type RequestedResources struct {
	CPURequestMilli    int64  `json:"cpu_request_milli"`
	CPULimitMilli      int64  `json:"cpu_limit_milli"`
	MemoryRequestBytes int64  `json:"memory_request_bytes"`
	MemoryLimitBytes   int64  `json:"memory_limit_bytes"`
	GPUResource        string `json:"gpu_resource"`
	GPUCount           int64  `json:"gpu_count"`
	RDMAResource       string `json:"rdma_resource"`
	RDMACount          int64  `json:"rdma_count"`
	SharedMemoryBytes  int64  `json:"shared_memory_bytes"`
}

type BenchmarkParameters struct {
	WorldSize       int    `json:"world_size"`
	ProcessesPerPod int    `json:"processes_per_pod"`
	Elements        int64  `json:"elements"`
	Warmup          int    `json:"warmup"`
	Iterations      int    `json:"iterations"`
	Operation       string `json:"operation"`
	DataType        string `json:"data_type"`
}

type Actual struct {
	Site  string       `json:"site"`
	Pool  string       `json:"pool"`
	Nodes []NodeResult `json:"nodes"`
}

type NodeResult struct {
	Name          string `json:"name"`
	UID           string `json:"uid"`
	GPUModel      string `json:"gpu_model"`
	GPUUUID       string `json:"gpu_uuid"`
	RDMADevice    string `json:"rdma_device"`
	RDMAInterface string `json:"rdma_interface"`
	RDMALinkState string `json:"rdma_link_state"`
}

type Placement struct {
	MatchesRequest *bool `json:"matches_request"`
	DistinctNodes  *bool `json:"distinct_nodes"`
}

type PodResult struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	NodeName string `json:"node_name"`
	Rank     int    `json:"rank"`
	ExitCode *int   `json:"exit_code"`
}

type RankResult struct {
	Rank             int    `json:"rank"`
	PodUID           string `json:"pod_uid"`
	NodeName         string `json:"node_name"`
	NodeUID          string `json:"node_uid"`
	PeerAuthVerified *bool  `json:"peer_auth_verified"`
	MemlockSoftBytes *int64 `json:"memlock_soft_bytes"`
	MemlockHardBytes *int64 `json:"memlock_hard_bytes"`
}

type NCCL struct {
	Version   string `json:"version"`
	Operation string `json:"operation"`
}

type Transport struct {
	Backend                string            `json:"backend"`
	NCCLNet                string            `json:"nccl_net"`
	Interfaces             []string          `json:"interfaces"`
	SocketFallbackObserved *bool             `json:"socket_fallback_observed"`
	PositiveIBEvidence     *bool             `json:"positive_ib_evidence"`
	Evidence               []string          `json:"evidence"`
	Environment            map[string]string `json:"environment"`
}

type Measurements struct {
	Samples            []BandwidthMeasurement `json:"samples"`
	AlgBWGbps          SummaryStats           `json:"algbw_gbps"`
	BusBWGbps          SummaryStats           `json:"busbw_gbps"`
	MaxRankTimeSeconds *float64               `json:"max_rank_time_seconds"`
}

type BandwidthMeasurement struct {
	Rank           int     `json:"rank"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	AlgBWGbps      float64 `json:"algbw_gbps"`
	BusBWGbps      float64 `json:"busbw_gbps"`
}

type SummaryStats struct {
	Count  int      `json:"count"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Mean   *float64 `json:"mean"`
	Median *float64 `json:"median"`
}

type Correctness struct {
	MaxError   *float64 `json:"max_error"`
	ErrorCount *int     `json:"error_count"`
}

type ValidationError struct {
	Code    ReasonCode `json:"code"`
	Field   string     `json:"field,omitempty"`
	Message string     `json:"message,omitempty"`
}

type RankExit struct {
	Rank     int  `json:"rank"`
	ExitCode *int `json:"exit_code"`
}

type Cleanup struct {
	State              CleanupState  `json:"state"`
	StartedAt          time.Time     `json:"started_at"`
	CompletedAt        time.Time     `json:"completed_at"`
	OwnedResources     []ResourceRef `json:"owned_resources"`
	RemainingResources []ResourceRef `json:"remaining_resources"`
}

type ResourceRef struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type EvidenceRef struct {
	Name       string    `json:"name"`
	URI        string    `json:"uri"`
	SHA256     string    `json:"sha256"`
	SizeBytes  int64     `json:"size_bytes"`
	CapturedAt time.Time `json:"captured_at"`
}

type ComponentVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Evaluation struct {
	Status Status
	Reason ReasonCode
	Errors []ValidationError
}

type ArtifactInfo struct {
	Path      string
	SHA256    string
	SizeBytes int64
	WrittenAt time.Time
}

type ArtifactLink struct {
	URI         string    `json:"uri"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	FinalizedAt time.Time `json:"finalized_at"`
}

type RunLifecycle struct {
	State       string
	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}
