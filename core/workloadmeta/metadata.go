// Package workloadmeta is the single authoritative source of truth for every
// Kubernetes label, annotation, and finalizer key in the "tau.azure.com/"
// namespace.
//
// # Why this package exists
//
// These keys are a live wire contract. They are read by Kueue ResourceFlavors,
// Kusto queries, GitOps manifests, and the Tau CLI itself. When a key is spelled
// as an inline string literal in several places, renaming one occurrence and
// missing another is not a compile error -- it is a silent empty-result failure
// that ships.
//
// That has happened. A README documented a retired job selector long after the
// code emitted a different key, so every documented kubectl command matched zero
// pods. Test fixtures embedded stale keys while the code read renamed ones, so a
// merge produced a silently passing test instead of a build failure.
//
// Declaring each key exactly once turns all of those into compile errors.
// TestNoInlineTauKeyLiterals enforces that no other file in this module
// reintroduces a raw literal.
//
// # Naming convention
//
// Keys use prefix naming: Label*, Annotation*, NodeLabel*. This groups the whole
// contract together in godoc and in editor autocomplete, which is what stops
// somebody re-inventing a literal because they could not find the existing
// constant. Packages that previously used suffix naming (ManagedByLabel)
// re-export from here.
//
// # Collision hazard
//
// LabelProfile ("tau.azure.com/profile") and the seven Nsight profiler
// annotations ("tau.azure.com/profile-mode" and friends) share a key prefix by
// historical accident. They are unrelated contracts. Never derive one from the
// other, and never use prefix matching to map a key back to its constant --
// match keys exactly.
package workloadmeta

import "strings"

const (
	// APIGroup is the Kubernetes API group for Tau custom resources
	// (TauWorkspace, TauQuotaRequest). It is not a label key, but it shares
	// the "tau.azure.com" string, so the guard test needs to know about it
	// to avoid flagging apiVersion literals.
	APIGroup = "tau.azure.com"

	// Domain is the key namespace owned by Tau. Declared so the guard test
	// can find the contract without hardcoding the domain a second time.
	Domain = APIGroup + "/"
)

// Ownership and workspace identity.
const (
	// LabelManagedBy marks an object as Tau-managed.
	LabelManagedBy = "tau.azure.com/managed-by"
	// ManagedByValue is the value LabelManagedBy carries on researcher
	// workloads rendered by Tau.
	ManagedByValue = "tau"

	LabelWorkspace             = "tau.azure.com/workspace"
	AnnotationWorkspaceID      = "tau.azure.com/workspace-id"
	AnnotationResultScope      = "tau.azure.com/result-scope"
	AnnotationClusterName      = "tau.azure.com/cluster-name"
	AnnotationControllerVerion = "tau.azure.com/controller-version"

	// AnnotationV0PrimaryWorkspace marks the one workspace a v0 cluster
	// activates. The tau-core workspace controller is authoritative for its
	// meaning; it is declared here because the CLI must read it too, and the
	// controller package is internal/ in a separate module.
	AnnotationV0PrimaryWorkspace = "tau.azure.com/v0-primary-workspace"

	// LabelAzureWorkloadIdentityUse is not a Tau key; it is Azure's. It
	// lives here because Tau stamps it, and one obvious declaration is
	// better than several literals.
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

	// AnnotationOwnerName and AnnotationOwnerKind record the Job/RayJob that
	// owns a generated Secret so the ownerReference can be patched in after
	// the workload's UID is known. These are written through a format string
	// rather than a map literal, which is exactly why the guard test scans
	// all string literals rather than only map keys.
	AnnotationOwnerName = "tau.azure.com/owner-name"
	AnnotationOwnerKind = "tau.azure.com/owner-kind"

	LabelOnboardingSmoke = "tau.azure.com/onboarding-smoke"

	// AnnotationDurableID and AnnotationDurableIDUnderscore are an
	// intentional alias pair, not a typo. Both spellings are read so that
	// runs stamped by older CLI builds stay discoverable. Do not "fix" one
	// into the other.
	AnnotationDurableID           = "tau.azure.com/durable-id"
	AnnotationDurableIDUnderscore = "tau.azure.com/durable_id"
)

// Scheduling, queueing, and topology placement.
const (
	LabelTeam        = "tau.azure.com/team"
	LabelLane        = "tau.azure.com/lane"
	LabelShape       = "tau.azure.com/shape"
	LabelTopology    = "tau.azure.com/topology"
	LabelPreset      = "tau.azure.com/preset"
	LabelGPUClass    = "tau.azure.com/gpu-class"
	LabelPreemptible = "tau.azure.com/preemptible"
	LabelReclaimable = "tau.azure.com/reclaimable"
	LabelQueueRole   = "tau.azure.com/queue-role"

	AnnotationClusterQueue           = "tau.azure.com/cluster-queue"
	AnnotationResourceFlavor         = "tau.azure.com/resource-flavor"
	AnnotationPodPriorityClass       = "tau.azure.com/pod-priority-class"
	AnnotationWorkloadPriorityClass  = "tau.azure.com/workload-priority-class"
	AnnotationKueueTopology          = "tau.azure.com/kueue-topology"
	AnnotationMultiKueueIncompatible = "tau.azure.com/multikueue-incompatible"

	AnnotationPresetDesc         = "tau.azure.com/preset-description"
	AnnotationPresetExplain      = "tau.azure.com/preset-explain"
	AnnotationPolicySource       = "tau.azure.com/topology-policy-source"
	AnnotationTopologyQueue      = "tau.azure.com/topology-local-queue"
	AnnotationTopologyGPUFamily  = "tau.azure.com/topology-gpu-family"
	AnnotationTopologyGPUProfile = "tau.azure.com/topology-gpu-profile"
)

// Node-scoped topology labels.
//
// These describe observed node hardware rather than workload intent. Where a key
// string is shared with a scheduling label (NodeLabelGPUClass and LabelGPUClass)
// that is deliberate: it is the same key viewed from the two sides of the
// contract.
const (
	NodeLabelGPUClass = "tau.azure.com/gpu-class"
)

// GPU resource contract.
const (
	LabelGPUCount     = "tau.azure.com/gpu-count"
	LabelGPUSize      = "tau.azure.com/gpu-size"
	LabelGPUPlacement = "tau.azure.com/gpu-placement"

	AnnotationGPUContract     = "tau.azure.com/gpu-contract"
	AnnotationGPUMemoryGiBMin = "tau.azure.com/gpu-memory-gib-min"
	AnnotationGPUCount        = "tau.azure.com/gpu-count"
	AnnotationDRAClaim        = "tau.azure.com/dra-claim-template"
)

// Storage contract.
const (
	LabelStorageDurableType      = "tau.azure.com/storage-durable"
	LabelStorageHotType          = "tau.azure.com/storage-hot"
	LabelStorageModelCacheType   = "tau.azure.com/storage-model-cache"
	LabelStorageCheckpointFormat = "tau.azure.com/storage-checkpoint-format"

	AnnotationStorageContract = "tau.azure.com/storage-contract"
	AnnotationStorageMounts   = "tau.azure.com/storage-mounts"
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

	// AnnotationCheckpointArtifact records the storage.checkpoint value a run
	// declared, so a later command can tell "this run produced no artifacts"
	// apart from "this run promised one and did not deliver it". It is set
	// only when a checkpoint is declared; readers treat presence as the
	// declaration. Distinct from AnnotationCheckpointURI, which carries a
	// resolved URI rather than a manifest-relative name.
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

	// StellarAnnotationPrefix matches the Stellar annotation family for bulk
	// copying. It is the one legitimate prefix use in this package; see the
	// collision-hazard note in the package doc before adding another.
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
	AnnotationSourceBundleDigest    = "tau.azure.com/source-bundle-digest"
	AnnotationSourceBundlePVC       = "tau.azure.com/source-bundle-pvc"
	AnnotationSourceBundlePath      = "tau.azure.com/source-bundle-path"
)

// LabelProfile records the name of the resolved compute profile a workload was
// rendered from. Writers (serve, storage, manifest) and readers (status,
// autocapture) must agree on this key.
//
// Note the unrelated "tau.azure.com/profile-*" annotations below: those carry
// Nsight Systems profiler settings and share a key prefix with this label by
// historical accident. They are distinct contracts -- do not derive one from the
// other.
const (
	LabelProfile = "tau.azure.com/profile"
)

// Nsight Systems profiler annotations. Both the config-first runner
// (internal/cli) and the manifest renderer (internal/manifest) stamp these, so
// the keys are defined once to keep the two paths from drifting.
const (
	AnnotationProfilerMode      = "tau.azure.com/profile-mode"
	AnnotationProfilerPath      = "tau.azure.com/profile-path"
	AnnotationProfilerPVC       = "tau.azure.com/profile-pvc"
	AnnotationProfilerRank      = "tau.azure.com/profile-rank"
	AnnotationProfilerWorldSize = "tau.azure.com/profile-world-size"
	AnnotationProfilerWarmup    = "tau.azure.com/profile-warmup"
	AnnotationProfilerDuration  = "tau.azure.com/profile-duration"
)

// Rendered workload spec provenance ("workloadspec-*").
//
// Only the execution key survives. The rest described a parallel spec-resolution
// layer that the eval engine alone populated; eval now renders through the
// ordinary job path, so nothing emitted them any more.
const (
	AnnotationSpecExecution = "tau.azure.com/workloadspec-execution"
)

func StampWorkspace(labels map[string]string, workspace string) map[string]string {
	if workspace == "" {
		return labels
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelWorkspace] = workspace
	return labels
}

// PodCorrelationAnnotations returns the full-fidelity workload annotations
// needed to correlate pod logs with a TauWorkspace and Stellar experiment.
func PodCorrelationAnnotations(annotations map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range annotations {
		if value == "" {
			continue
		}
		if key == AnnotationWorkspaceID ||
			key == AnnotationResultScope ||
			key == AnnotationExperimentSource ||
			key == AnnotationMetricsSession ||
			strings.HasPrefix(key, StellarAnnotationPrefix) {
			out[key] = value
		}
	}
	return out
}

// DefaultWorkspaceName is the TauWorkspace every TauGrid v0 cluster gets unless
// an operator deliberately picks another name.
//
// TauGrid v0 admits exactly one workspace per cluster, so leaving its name
// unset made every install differ in the one place researchers can actually
// see: the workload namespace, which is derived from this name. Pinning a
// default is what lets docs, examples, and tooling assume a shape rather than
// parameterising over an operator's choice.
//
// It lives here, rather than next to either consumer, because both the CLI
// (workspace creation) and core topology (preset LocalQueue lookup) have to
// agree on it. When they disagreed, preset-based submission looked for queues
// in a namespace no install created.
//
// The name remains customisable; nothing may depend on this value being
// literal. `tau` resolves the active workspace from the cluster instead of
// assuming this constant.
const DefaultWorkspaceName = "taugrid-default"
