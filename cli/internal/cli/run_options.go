// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"time"

	"github.com/Azure/taugrid/core/runconfig"
)

type runDispatchInput struct {
	engine         string
	file           string
	workloadKind   string
	nameFromConfig bool
}

type runRouting struct {
	namespace              string
	workspace              string
	kubeContext            string
	dryRun                 string
	workspaceResultScope   string
	submissionID           string
	workspaceQueueResolved bool
}

type runRoutingInput struct {
	workspaceExplicit   bool
	kubeContextExplicit bool
	// kubeContextFromFlag distinguishes an explicit --context from one inherited
	// through TAU_CONTEXT when reconciling a workspace connection descriptor.
	kubeContextFromFlag bool
}

type runPlacement struct {
	profileName              string
	queue                    string
	preset                   string
	team                     string
	lane                     string
	gpuClass                 string
	mode                     string
	topology                 string
	shape                    string
	topologyPolicy           string
	priorityTier             string
	workloadPriorityClass    string
	podPriorityClass         string
	nodeSelectors            []string
	clearNodeSelector        bool
	disableDefaultPriorities bool
	gpuResourceMode          string
	migProfile               string
}

type runPayloadInput struct {
	script             string
	mainScript         string
	configDir          string
	workingDir         string
	workingDirExcludes []string
	source             *runconfig.Source
	extraScripts       []string
}

type runContainerRuntime struct {
	image                 string
	env                   []string
	envSecrets            []string
	serviceAccountName    string
	azureWorkloadIdentity bool
}

type runRuntimeInput struct {
	runContainerRuntime

	runtimePip        []string
	envKV             []string
	keyVault          string
	kvTenantID        string
	kvClientID        string
	secretPayloadPath string
}

type runDirectStorage struct {
	dataPVC               string
	resultPVC             string
	output                string
	outputPublish         string
	checkpointArtifact    string
	artifactPublicationID string
}

type runStorageInput struct {
	runDirectStorage

	volumeSpecs []string
	mountSpecs  []string
	imageAssets []runconfig.ImageAsset
}

type runResourceLimits struct {
	cpuRequest    string
	memoryRequest string
	cpuLimit      string
	memoryLimit   string
}

type runRayResources struct {
	headCPURequest      string
	headMemoryRequest   string
	headCPULimit        string
	headMemoryLimit     string
	workerCPURequest    string
	workerMemoryRequest string
	workerCPULimit      string
	workerMemoryLimit   string
	workers             int
	gpusPerWorker       int
}

type runComputeInput struct {
	runResourceLimits
	runRayResources

	cpuWorkers            int
	jobGPUs               *int
	gpusPerWorkerExplicit bool
}

type runProfile struct {
	profiler        string
	profileRank     string
	profileWarmup   string
	profileDuration string
}

type runDirectMetrics struct {
	metricsHistory        []string
	metricsSessionID      string
	metricsOffloadEnabled bool
	checkpointPath        string
	experiment            runExperimentMetadata
}

type runObservabilityInput struct {
	runProfile
	runDirectMetrics
}

type runRayTuning struct {
	tuneMetric              string
	tuneMode                string
	tuneParamSpace          string
	tuneNumSamples          int
	tuneMaxConcurrentTrials int
	allowNCCLOverride       bool
}

type runExecutionInput struct {
	runRayTuning

	launcher                string
	processesPerNode        int
	nodes                   int
	ttlSecondsAfterFinished int64
	configs                 map[string]any
	smokePairs              int
	upstreamCheckpoint      string
}

type runResilienceInput struct {
	maxRetries     int
	retryOn        []string
	backoffInitial time.Duration
	backoffMax     time.Duration
}

// unresolvedRunOptions is the mutable input model used while config, flags,
// connection metadata, workspace defaults, retry/resume overrides, and engine
// inference are still being combined. resolveRunTarget is its terminal boundary.
type unresolvedRunOptions struct {
	runDispatchInput
	runRouting
	runRoutingInput
	runPlacement
	runPayloadInput
	runRuntimeInput
	runStorageInput
	runComputeInput
	runObservabilityInput
	runExecutionInput
	runResilienceInput
}

func defaultRunDispatchOptions() unresolvedRunOptions {
	o := unresolvedRunOptions{}
	o.workers = 1
	o.gpusPerWorker = 1
	return o
}
