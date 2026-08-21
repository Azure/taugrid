// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package workloadmeta defines the shared Kubernetes metadata contract for Tau.
// Keys are centralized here so producers, readers, manifests, and docs cannot
// silently drift.
package workloadmeta

const (
	// APIGroup is the Kubernetes API group for Tau custom resources.
	APIGroup = "tau.azure.com"

	// Domain is the metadata namespace owned by Tau.
	Domain = APIGroup + "/"
)

// Ownership and workspace identity.
const (
	// LabelManagedBy marks an object as Tau-managed.
	LabelManagedBy = "tau.azure.com/managed-by"
	// ManagedByValue identifies researcher workloads rendered by Tau.
	ManagedByValue = "tau"

	LabelWorkspace             = "tau.azure.com/workspace"
	AnnotationWorkspaceID      = "tau.azure.com/workspace-id"
	AnnotationResultScope      = "tau.azure.com/result-scope"
	AnnotationClusterName      = "tau.azure.com/cluster-name"
	AnnotationControllerVerion = "tau.azure.com/controller-version"

	// AnnotationV0PrimaryWorkspace marks the active v0 workspace.
	AnnotationV0PrimaryWorkspace = "tau.azure.com/v0-primary-workspace"

	// LabelAzureWorkloadIdentityUse is the Azure identity label stamped by Tau.
	LabelAzureWorkloadIdentityUse = "azure.workload.identity/use"
)

// Workload identity and kind.
const (
	LabelJob          = "tau.azure.com/job"
	LabelRun          = "tau.azure.com/run"
	LabelRunID        = "tau.azure.com/run-id"
	LabelWorkloadKind = "tau.azure.com/workload-kind"
	LabelService      = "tau.azure.com/service"
	LabelDataset      = "tau.azure.com/dataset"

	AnnotationNamespace = "tau.azure.com/namespace"

	// These identify the workload that owns a generated Secret.
	AnnotationOwnerName = "tau.azure.com/owner-name"
	AnnotationOwnerKind = "tau.azure.com/owner-kind"

	LabelOnboardingSmoke = "tau.azure.com/onboarding-smoke"

	// Both spellings are read so workloads stamped by older CLI builds remain
	// discoverable. New writers use AnnotationDurableID.
	AnnotationDurableID           = "tau.azure.com/durable-id"
	AnnotationDurableIDUnderscore = "tau.azure.com/durable_id"
)

// Scheduling, queueing, and topology placement.
const (
	LabelTeam      = "tau.azure.com/team"
	LabelLane      = "tau.azure.com/lane"
	LabelShape     = "tau.azure.com/shape"
	LabelTopology  = "tau.azure.com/topology"
	LabelPreset    = "tau.azure.com/preset"
	LabelGPUClass  = "tau.azure.com/gpu-class"
	LabelQueueRole = "tau.azure.com/queue-role"

	AnnotationClusterQueue   = "tau.azure.com/cluster-queue"
	AnnotationResourceFlavor = "tau.azure.com/resource-flavor"

	AnnotationTauClusterGeneration   = "tau.azure.com/tau-cluster-generation"
	AnnotationWorkloadProfileSetHash = "tau.azure.com/workload-profile-set-hash"
	AnnotationWorkloadProfileName    = "tau.azure.com/workload-profile"

	AnnotationPresetExplain      = "tau.azure.com/preset-explain"
	AnnotationTopologyQueue      = "tau.azure.com/topology-local-queue"
	AnnotationTopologyGPUFamily  = "tau.azure.com/topology-gpu-family"
	AnnotationTopologyGPUProfile = "tau.azure.com/topology-gpu-profile"
)

// Node-scoped topology labels. NodeLabelGPUClass intentionally shares its key
// with LabelGPUClass.
const (
	NodeLabelGPUClass = "tau.azure.com/gpu-class"
)

// GPU resource contract.
const (
	LabelGPUCount = "tau.azure.com/gpu-count"
	LabelGPUSize  = "tau.azure.com/gpu-size"

	AnnotationGPUContract = "tau.azure.com/gpu-contract"
	AnnotationGPUCount    = "tau.azure.com/gpu-count"
	AnnotationDRAClaim    = "tau.azure.com/dra-claim-template"
)

// Storage mounts.
const (
	AnnotationStorageMounts = "tau.azure.com/storage-mounts"
)

// Durable results and artifacts.
const (
	AnnotationResultPath            = "tau.azure.com/result-path"
	AnnotationResultPVC             = "tau.azure.com/result-pvc"
	AnnotationResultArtifacts       = "tau.azure.com/result-artifacts"
	AnnotationResultNote            = "tau.azure.com/result-note"
	AnnotationArtifactURI           = "tau.azure.com/artifact-uri"
	AnnotationCheckpointURI         = "tau.azure.com/checkpoint-uri"
	AnnotationArtifactPublication   = "tau.azure.com/artifact-publication"
	AnnotationArtifactPublicationID = "tau.azure.com/artifact-publication-id"

	// AnnotationCheckpointArtifact records a declared checkpoint name;
	// AnnotationCheckpointURI records its resolved location.
	AnnotationCheckpointArtifact = "tau.azure.com/checkpoint-artifact"
)

// Experiment tracking and Stellar.
const (
	LabelExperiment        = "tau.azure.com/experiment"
	LabelStellarExperiment = "tau.azure.com/stellar-experiment"
	LabelStellarGroup      = "tau.azure.com/stellar-group"
	LabelStellarProject    = "tau.azure.com/stellar-project"

	AnnotationExperimentSource       = "tau.azure.com/experiment-source"
	AnnotationMetricsSession         = "tau.azure.com/metrics-session-id"
	AnnotationCaptureVersion         = "tau.azure.com/capture-version"
	AnnotationStellarExperimentID    = "tau.azure.com/stellar-experiment-id"
	AnnotationStellarExperimentTitle = "tau.azure.com/stellar-experiment-title"
	AnnotationStellarGroup           = "tau.azure.com/stellar-group-value"
	AnnotationStellarProject         = "tau.azure.com/stellar-project-value"
	AnnotationStellarQuestion        = "tau.azure.com/stellar-question"
	AnnotationStellarTags            = "tau.azure.com/stellar-tags"

	// StellarAnnotationPrefix matches Stellar annotations for bulk copying.
	StellarAnnotationPrefix = "tau.azure.com/stellar-"
)

// Provenance.
const (
	AnnotationCodeSHA               = "tau.azure.com/code-sha"
	AnnotationConfigHash            = "tau.azure.com/config-hash"
	AnnotationImage                 = "tau.azure.com/image"
	AnnotationImageDigest           = "tau.azure.com/image-digest"
	AnnotationTauCommand            = "tau.azure.com/tau-command"
	AnnotationSubmissionID          = "tau.azure.com/submission-id"
	AnnotationPayloadDigest         = "tau.azure.com/payload-digest"
	AnnotationManifestPayloadDigest = "tau.azure.com/manifest-payload-digest"
	AnnotationScriptPayloadDigest   = "tau.azure.com/script-payload-digest"
)

// LabelProfile names the resolved compute profile. It is unrelated to the
// profiler annotations below despite their shared prefix.
const (
	LabelProfile = "tau.azure.com/profile"
)

// Nsight Systems profiler annotations.
const (
	AnnotationProfilerMode      = "tau.azure.com/profile-mode"
	AnnotationProfilerPath      = "tau.azure.com/profile-path"
	AnnotationProfilerPVC       = "tau.azure.com/profile-pvc"
	AnnotationProfilerRank      = "tau.azure.com/profile-rank"
	AnnotationProfilerWorldSize = "tau.azure.com/profile-world-size"
	AnnotationProfilerWarmup    = "tau.azure.com/profile-warmup"
	AnnotationProfilerDuration  = "tau.azure.com/profile-duration"
)

// Rendered workload spec provenance.
const (
	AnnotationSpecExecution = "tau.azure.com/workloadspec-execution"
)

// DefaultWorkspaceName is the default v0 workspace and workload namespace.
// Operators may override it; callers should resolve the active workspace.
const DefaultWorkspaceName = "taugrid-default"
