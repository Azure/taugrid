// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"io"

	"github.com/Azure/taugrid/core/runconfig"
)

// resolvedRunRouting contains invocation identity and cluster routing shared by
// every execution engine after target resolution.
type resolvedRunRouting struct {
	namespace              string
	workspace              string
	kubeContext            string
	dryRun                 string
	workspaceResultScope   string
	submissionID           string
	workspaceQueueResolved bool
}

// resolvedRunPlacement contains scheduling policy shared by all engines.
type resolvedRunPlacement struct {
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

// resolvedRunRuntime contains the container environment shared by all engines.
type resolvedRunRuntime struct {
	image                 string
	env                   []string
	envSecrets            []string
	serviceAccountName    string
	azureWorkloadIdentity bool
}

type resolvedDirectRunOptions struct {
	resolvedRunRouting
	resolvedRunPlacement
	resolvedRunRuntime

	script                string
	dataPVC               string
	resultPVC             string
	output                string
	outputPublish         string
	checkpointArtifact    string
	artifactPublicationID string
	cpuRequest            string
	memoryRequest         string
	cpuLimit              string
	memoryLimit           string
	launcher              string
	configs               map[string]any
	metricsHistory        []string
	metricsSessionID      string
	metricsOffloadEnabled bool
	checkpointPath        string
	experiment            runExperimentMetadata
}

type resolvedRunJobOptions struct {
	resolvedDirectRunOptions

	source                  *runconfig.Source
	volumeSpecs             []string
	mountSpecs              []string
	imageAssets             []runconfig.ImageAsset
	jobGPUs                 *int
	profiler                string
	profileRank             string
	profileWarmup           string
	profileDuration         string
	processesPerNode        int
	nodes                   int
	ttlSecondsAfterFinished int64
}

type resolvedRunRayOptions struct {
	resolvedDirectRunOptions

	workingDir              string
	workingDirExcludes      []string
	runtimePip              []string
	workers                 int
	gpusPerWorker           int
	headCPURequest          string
	headMemoryRequest       string
	headCPULimit            string
	headMemoryLimit         string
	workerCPURequest        string
	workerMemoryRequest     string
	workerCPULimit          string
	workerMemoryLimit       string
	tuneMetric              string
	tuneMode                string
	tuneParamSpace          string
	tuneNumSamples          int
	tuneMaxConcurrentTrials int
	allowNCCLOverride       bool
}

type resolvedRunManagedWorkflowOptions struct {
	resolvedRunRouting
	resolvedRunPlacement
	resolvedRunRuntime

	file               string
	mainScript         string
	dataPVC            string
	workloadKind       string
	upstreamCheckpoint string
	extraScripts       []string
	envKV              []string
	keyVault           string
	kvTenantID         string
	kvClientID         string
	secretPayloadPath  string
	workers            int
	cpuWorkers         int
	smokePairs         int
	profiler           string
	profileRank        string
	profileWarmup      string
	profileDuration    string
}

func resolveRunRouting(o unresolvedRunOptions) resolvedRunRouting {
	return resolvedRunRouting{
		namespace:              o.namespace,
		workspace:              o.workspace,
		kubeContext:            o.kubeContext,
		dryRun:                 o.dryRun,
		workspaceResultScope:   o.workspaceResultScope,
		submissionID:           o.submissionID,
		workspaceQueueResolved: o.workspaceQueueResolved,
	}
}

func resolveRunPlacement(o unresolvedRunOptions) resolvedRunPlacement {
	return resolvedRunPlacement{
		profileName:              o.profileName,
		queue:                    o.queue,
		preset:                   o.preset,
		team:                     o.team,
		lane:                     o.lane,
		gpuClass:                 o.gpuClass,
		mode:                     o.mode,
		topology:                 o.topology,
		shape:                    o.shape,
		topologyPolicy:           o.topologyPolicy,
		priorityTier:             o.priorityTier,
		workloadPriorityClass:    o.workloadPriorityClass,
		podPriorityClass:         o.podPriorityClass,
		nodeSelectors:            append([]string{}, o.nodeSelectors...),
		clearNodeSelector:        o.clearNodeSelector,
		disableDefaultPriorities: o.disableDefaultPriorities,
		gpuResourceMode:          o.gpuResourceMode,
		migProfile:               o.migProfile,
	}
}

func resolveRunRuntime(o unresolvedRunOptions) resolvedRunRuntime {
	return resolvedRunRuntime{
		image:                 o.image,
		env:                   append([]string{}, o.env...),
		envSecrets:            append([]string{}, o.envSecrets...),
		serviceAccountName:    o.serviceAccountName,
		azureWorkloadIdentity: o.azureWorkloadIdentity,
	}
}

func resolveDirectRunOptions(o unresolvedRunOptions) resolvedDirectRunOptions {
	return resolvedDirectRunOptions{
		resolvedRunRouting:    resolveRunRouting(o),
		resolvedRunPlacement:  resolveRunPlacement(o),
		resolvedRunRuntime:    resolveRunRuntime(o),
		script:                o.script,
		dataPVC:               o.dataPVC,
		resultPVC:             o.resultPVC,
		output:                o.output,
		outputPublish:         o.outputPublish,
		checkpointArtifact:    o.checkpointArtifact,
		artifactPublicationID: o.artifactPublicationID,
		cpuRequest:            o.cpuRequest,
		memoryRequest:         o.memoryRequest,
		cpuLimit:              o.cpuLimit,
		memoryLimit:           o.memoryLimit,
		launcher:              o.launcher,
		configs:               o.configs,
		metricsHistory:        append([]string{}, o.metricsHistory...),
		metricsSessionID:      o.metricsSessionID,
		metricsOffloadEnabled: o.metricsOffloadEnabled,
		checkpointPath:        o.checkpointPath,
		experiment:            o.experiment,
	}
}

func resolveRunJobOptions(o unresolvedRunOptions) resolvedRunJobOptions {
	return resolvedRunJobOptions{
		resolvedDirectRunOptions: resolveDirectRunOptions(o),
		source:                   o.source,
		volumeSpecs:              append([]string{}, o.volumeSpecs...),
		mountSpecs:               append([]string{}, o.mountSpecs...),
		imageAssets:              append([]runconfig.ImageAsset{}, o.imageAssets...),
		jobGPUs:                  o.jobGPUs,
		profiler:                 o.profiler,
		profileRank:              o.profileRank,
		profileWarmup:            o.profileWarmup,
		profileDuration:          o.profileDuration,
		processesPerNode:         o.processesPerNode,
		nodes:                    o.nodes,
		ttlSecondsAfterFinished:  o.ttlSecondsAfterFinished,
	}
}

func resolveRunRayOptions(o unresolvedRunOptions) resolvedRunRayOptions {
	return resolvedRunRayOptions{
		resolvedDirectRunOptions: resolveDirectRunOptions(o),
		workingDir:               o.workingDir,
		workingDirExcludes:       append([]string{}, o.workingDirExcludes...),
		runtimePip:               append([]string{}, o.runtimePip...),
		workers:                  o.workers,
		gpusPerWorker:            o.gpusPerWorker,
		headCPURequest:           o.headCPURequest,
		headMemoryRequest:        o.headMemoryRequest,
		headCPULimit:             o.headCPULimit,
		headMemoryLimit:          o.headMemoryLimit,
		workerCPURequest:         o.workerCPURequest,
		workerMemoryRequest:      o.workerMemoryRequest,
		workerCPULimit:           o.workerCPULimit,
		workerMemoryLimit:        o.workerMemoryLimit,
		tuneMetric:               o.tuneMetric,
		tuneMode:                 o.tuneMode,
		tuneParamSpace:           o.tuneParamSpace,
		tuneNumSamples:           o.tuneNumSamples,
		tuneMaxConcurrentTrials:  o.tuneMaxConcurrentTrials,
		allowNCCLOverride:        o.allowNCCLOverride,
	}
}

func resolveRunManagedWorkflowOptions(o unresolvedRunOptions) resolvedRunManagedWorkflowOptions {
	return resolvedRunManagedWorkflowOptions{
		resolvedRunRouting:   resolveRunRouting(o),
		resolvedRunPlacement: resolveRunPlacement(o),
		resolvedRunRuntime:   resolveRunRuntime(o),
		file:                 o.file,
		mainScript:           o.mainScript,
		dataPVC:              o.dataPVC,
		workloadKind:         o.workloadKind,
		upstreamCheckpoint:   o.upstreamCheckpoint,
		extraScripts:         append([]string{}, o.extraScripts...),
		envKV:                append([]string{}, o.envKV...),
		keyVault:             o.keyVault,
		kvTenantID:           o.kvTenantID,
		kvClientID:           o.kvClientID,
		secretPayloadPath:    o.secretPayloadPath,
		workers:              o.workers,
		cpuWorkers:           o.cpuWorkers,
		smokePairs:           o.smokePairs,
		profiler:             o.profiler,
		profileRank:          o.profileRank,
		profileWarmup:        o.profileWarmup,
		profileDuration:      o.profileDuration,
	}
}

type resolvedRunRequest interface {
	execute(context.Context, io.Writer, io.Writer, string) error
	namespace() string
}

type resolvedRunTarget struct {
	request resolvedRunRequest
}

func newResolvedRunTarget(request resolvedRunRequest) resolvedRunTarget {
	return resolvedRunTarget{request: request}
}

func (t resolvedRunTarget) namespace() string {
	if t.request == nil {
		return ""
	}
	return t.request.namespace()
}

func (r *runJobRequest) execute(ctx context.Context, stdout, stderr io.Writer, captureCommand string) error {
	return executeRunJob(ctx, stdout, stderr, r, captureCommand)
}

func (r *runJobRequest) namespace() string {
	return r.Options.namespace
}

func (r *runRayRequest) execute(ctx context.Context, stdout, stderr io.Writer, captureCommand string) error {
	return executeRunRay(ctx, stdout, stderr, r, captureCommand)
}

func (r *runRayRequest) namespace() string {
	return r.Options.namespace
}

func (r *runManagedWorkflowRequest) execute(ctx context.Context, stdout, stderr io.Writer, captureCommand string) error {
	return executeRunManagedWorkflow(ctx, stdout, stderr, r, captureCommand)
}

func (r *runManagedWorkflowRequest) namespace() string {
	return r.Options.namespace
}
