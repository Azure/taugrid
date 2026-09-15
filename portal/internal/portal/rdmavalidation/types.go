// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package rdmavalidation defines the Portal's read-only InfiniBand validation
// board contract. The producer-owned result schema lives in core/rdmavalidation.
package rdmavalidation

import (
	"context"
	"errors"
)

var (
	ErrNotFound          = errors.New("RDMA validation not found")
	ErrUnavailable       = errors.New("RDMA validation source unavailable")
	ErrInvalidCursor     = errors.New("invalid RDMA validation cursor")
	ErrMalformedArtifact = errors.New("malformed RDMA validation artifact")
	ErrUnsupportedSchema = errors.New("unsupported RDMA validation schema")
	ErrArtifactIntegrity = errors.New("RDMA validation artifact integrity check failed")
	ErrScopeMismatch     = errors.New("RDMA validation scope mismatch")
)

type Scope struct {
	WorkspaceID   string
	Cluster       string
	FetchArtifact ArtifactFetcher
}

type ArtifactFetcher func(context.Context, ArtifactMetadata) ([]byte, ArtifactMetadata, error)

type ListOptions struct {
	Limit  int
	Cursor string
}

type Reader interface {
	Summary(context.Context, Scope) (Summary, error)
	List(context.Context, Scope, ListOptions) (Page, error)
	Get(context.Context, Scope, string) (Detail, error)
}

type Summary struct {
	Latest      *Validation `json:"latest"`
	Total       int         `json:"total,omitempty"`
	GeneratedAt string      `json:"generatedAt,omitempty"`
}

type Page struct {
	Validations []Validation `json:"validations"`
	NextCursor  string       `json:"nextCursor,omitempty"`
	Truncated   bool         `json:"truncated"`
	GeneratedAt string       `json:"generatedAt,omitempty"`
}

type Validation struct {
	ValidationID         string                `json:"validationId"`
	RunID                string                `json:"runId,omitempty"`
	RunAttempt           *int                  `json:"runAttempt,omitempty"`
	State                string                `json:"state"`
	HistoricalStatus     *string               `json:"historicalStatus"`
	Freshness            string                `json:"freshness"`
	ReasonCode           string                `json:"reasonCode,omitempty"`
	Reason               string                `json:"reason,omitempty"`
	WorkspaceID          string                `json:"workspaceId,omitempty"`
	Cluster              string                `json:"cluster,omitempty"`
	Namespace            string                `json:"namespace,omitempty"`
	Project              string                `json:"project,omitempty"`
	ExperimentID         string                `json:"experimentId,omitempty"`
	RunGroupID           string                `json:"runGroupId,omitempty"`
	CreatedAt            string                `json:"createdAt,omitempty"`
	StartedAt            string                `json:"startedAt,omitempty"`
	AdmittedAt           string                `json:"admittedAt,omitempty"`
	CompletedAt          string                `json:"completedAt,omitempty"`
	ObservedAt           string                `json:"observedAt,omitempty"`
	ValidUntil           string                `json:"validUntil,omitempty"`
	StaleAfterSeconds    *int64                `json:"staleAfterSeconds,omitempty"`
	AgeSeconds           *int64                `json:"ageSeconds,omitempty"`
	Requested            *Requested            `json:"requested,omitempty"`
	Actual               *Actual               `json:"actual,omitempty"`
	Placement            *Placement            `json:"placement,omitempty"`
	Source               *Source               `json:"source,omitempty"`
	Collective           *Collective           `json:"collective,omitempty"`
	Transport            *Transport            `json:"transport,omitempty"`
	Parameters           *Parameters           `json:"parameters,omitempty"`
	Summary              *BandwidthSummary     `json:"summary,omitempty"`
	Correctness          *Correctness          `json:"correctness,omitempty"`
	DurationSeconds      *float64              `json:"durationSeconds,omitempty"`
	Cleanup              *Cleanup              `json:"cleanup,omitempty"`
	ArtifactVerification *ArtifactVerification `json:"artifactVerification,omitempty"`
}

type Detail struct {
	Validation
	SchemaVersion string        `json:"schemaVersion,omitempty"`
	Kind          string        `json:"kind,omitempty"`
	Pods          []Pod         `json:"pods,omitempty"`
	Ranks         []Rank        `json:"ranks,omitempty"`
	Measurements  []Measurement `json:"measurements,omitempty"`
	Errors        []ResultError `json:"errors,omitempty"`
	JobExitCode   *int          `json:"jobExitCode,omitempty"`
	RankExitCodes []RankExit    `json:"rankExitCodes,omitempty"`
	Evidence      []Evidence    `json:"evidence,omitempty"`
	Producer      *Component    `json:"producer,omitempty"`
	Parser        *Component    `json:"parser,omitempty"`
}

type Source struct {
	Repository              string `json:"repository,omitempty"`
	Revision                string `json:"revision,omitempty"`
	ImageRepository         string `json:"imageRepository,omitempty"`
	ImageIndexDigest        string `json:"imageIndexDigest,omitempty"`
	ImagePlatformDigest     string `json:"imagePlatformDigest,omitempty"`
	ImageConfigDigest       string `json:"imageConfigDigest,omitempty"`
	SBOMManifestDigest      string `json:"sbomManifestDigest,omitempty"`
	SBOMLayerDigest         string `json:"sbomLayerDigest,omitempty"`
	VEXManifestDigest       string `json:"vexManifestDigest,omitempty"`
	VEXLayerDigest          string `json:"vexLayerDigest,omitempty"`
	SignatureManifestDigest string `json:"signatureManifestDigest,omitempty"`
	SignatureLayerDigest    string `json:"signatureLayerDigest,omitempty"`
	SignatureTrustVerified  *bool  `json:"signatureTrustVerified,omitempty"`
}

type SiteTopologyMode string

const (
	SiteTopologyComplete      SiteTopologyMode = "complete"
	SiteTopologyNotApplicable SiteTopologyMode = "not_applicable"
	SiteTopologyIncomplete    SiteTopologyMode = "incomplete"
)

type Requested struct {
	NodeCount         *int             `json:"nodeCount,omitempty"`
	PodCount          *int             `json:"podCount,omitempty"`
	RankCount         *int             `json:"rankCount,omitempty"`
	RanksPerNode      *int             `json:"ranksPerNode,omitempty"`
	GPUsPerRank       *int             `json:"gpusPerRank,omitempty"`
	DistinctHostname  *bool            `json:"distinctHostname,omitempty"`
	Site              string           `json:"site,omitempty"`
	SiteProvider      string           `json:"siteProvider,omitempty"`
	SiteMode          SiteTopologyMode `json:"siteMode,omitempty"`
	Region            string           `json:"region,omitempty"`
	Pool              string           `json:"pool,omitempty"`
	GPUModel          string           `json:"gpuModel,omitempty"`
	GPUResource       string           `json:"gpuResource,omitempty"`
	RDMAResource      string           `json:"rdmaResource,omitempty"`
	RDMAPerPod        *int             `json:"rdmaPerPod,omitempty"`
	MessageSizesBytes []int64          `json:"messageSizesBytes,omitempty"`
	WarmupIterations  *int             `json:"warmupIterations,omitempty"`
	Iterations        *int             `json:"iterations,omitempty"`
}

type Actual struct {
	Site         string           `json:"site,omitempty"`
	SiteProvider string           `json:"siteProvider,omitempty"`
	SiteMode     SiteTopologyMode `json:"siteMode,omitempty"`
	Region       string           `json:"region,omitempty"`
	Pool         string           `json:"pool,omitempty"`
	Nodes        []Node           `json:"nodes,omitempty"`
}

type Node struct {
	Name              string       `json:"name,omitempty"`
	UID               string       `json:"uid,omitempty"`
	Site              string       `json:"site,omitempty"`
	SiteSourceKey     string       `json:"siteSourceKey,omitempty"`
	SiteLabelConflict bool         `json:"siteLabelConflict,omitempty"`
	Region            string       `json:"region,omitempty"`
	Pool              string       `json:"pool,omitempty"`
	GPUModel          string       `json:"gpuModel,omitempty"`
	GPUUUIDs          []string     `json:"gpuUuids,omitempty"`
	RDMADevices       []RDMADevice `json:"rdmaDevices,omitempty"`
}

type RDMADevice struct {
	ResourceName string `json:"resourceName,omitempty"`
	Device       string `json:"device,omitempty"`
	Port         *int   `json:"port,omitempty"`
	Interface    string `json:"interface,omitempty"`
	LinkLayer    string `json:"linkLayer,omitempty"`
	State        string `json:"state,omitempty"`
}

type Placement struct {
	DistinctNodes  *bool  `json:"distinctNodes,omitempty"`
	MatchesRequest *bool  `json:"matchesRequest,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type Pod struct {
	Name        string `json:"name,omitempty"`
	UID         string `json:"uid,omitempty"`
	Node        string `json:"node,omitempty"`
	Phase       string `json:"phase,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type Rank struct {
	Rank              *int   `json:"rank,omitempty"`
	PodUID            string `json:"podUid,omitempty"`
	Node              string `json:"node,omitempty"`
	NodeUID           string `json:"nodeUid,omitempty"`
	PeerAuthenticated *bool  `json:"peerAuthenticated,omitempty"`
	MemlockSoftBytes  *int64 `json:"memlockSoftBytes,omitempty"`
	MemlockHardBytes  *int64 `json:"memlockHardBytes,omitempty"`
	ExitCode          *int   `json:"exitCode,omitempty"`
}

type Collective struct {
	Library   string `json:"library,omitempty"`
	Version   string `json:"version,omitempty"`
	Operation string `json:"operation,omitempty"`
}

type Transport struct {
	Backend                string            `json:"backend,omitempty"`
	NCCLNet                string            `json:"ncclNet,omitempty"`
	Interfaces             []string          `json:"interfaces,omitempty"`
	RDMADevices            []string          `json:"rdmaDevices,omitempty"`
	SocketFallbackDetected *bool             `json:"socketFallbackDetected,omitempty"`
	IBPositiveEvidence     *bool             `json:"ibPositiveEvidence,omitempty"`
	Evidence               []string          `json:"evidence,omitempty"`
	Environment            map[string]string `json:"environment,omitempty"`
}

type Parameters struct {
	WorldSize         *int    `json:"worldSize,omitempty"`
	ProcessesPerPod   *int    `json:"processesPerPod,omitempty"`
	Elements          *int64  `json:"elements,omitempty"`
	DataType          string  `json:"dataType,omitempty"`
	Operation         string  `json:"operation,omitempty"`
	MessageSizesBytes []int64 `json:"messageSizesBytes,omitempty"`
	WarmupIterations  *int    `json:"warmupIterations,omitempty"`
	Iterations        *int    `json:"iterations,omitempty"`
}

type Distribution struct {
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Mean   *float64 `json:"mean,omitempty"`
	Median *float64 `json:"median,omitempty"`
}

type BandwidthSummary struct {
	AlgBWGbps *Distribution `json:"algbwGbps,omitempty"`
	BusBWGbps *Distribution `json:"busbwGbps,omitempty"`
}

type Measurement struct {
	Rank             *int     `json:"rank,omitempty"`
	MessageSizeBytes *int64   `json:"messageSizeBytes,omitempty"`
	Iterations       *int     `json:"iterations,omitempty"`
	ElapsedSeconds   *float64 `json:"elapsedSeconds,omitempty"`
	AlgBWGbps        *float64 `json:"algbwGbps,omitempty"`
	BusBWGbps        *float64 `json:"busbwGbps,omitempty"`
}

type Correctness struct {
	Passed     *bool    `json:"passed,omitempty"`
	MaxError   *float64 `json:"maxError,omitempty"`
	ErrorCount *int64   `json:"errorCount,omitempty"`
}

type ResultError struct {
	Stage   string `json:"stage,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type RankExit struct {
	Rank *int `json:"rank,omitempty"`
	Code *int `json:"code,omitempty"`
}

type Cleanup struct {
	Status             string   `json:"status,omitempty"`
	StartedAt          string   `json:"startedAt,omitempty"`
	CompletedAt        string   `json:"completedAt,omitempty"`
	OwnedResources     []string `json:"ownedResources,omitempty"`
	RemainingResources []string `json:"remainingResources,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

type Evidence struct {
	Name       string `json:"name,omitempty"`
	URI        string `json:"uri,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	SizeBytes  *int64 `json:"sizeBytes,omitempty"`
	CapturedAt string `json:"capturedAt,omitempty"`
}

type Component struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type ArtifactVerification struct {
	State       string `json:"state,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	URI         string `json:"uri,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	SizeBytes   *int64 `json:"sizeBytes,omitempty"`
	VerifiedAt  string `json:"verifiedAt,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type ArtifactMetadata struct {
	ValidationID string
	RunID        string
	WorkspaceID  string
	URI          string
	ContentType  string
	SHA256       string
	SizeBytes    int64
}
