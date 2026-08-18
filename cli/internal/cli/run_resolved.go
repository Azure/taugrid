// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"io"
	"slices"

	"github.com/Azure/taugrid/core/runconfig"
)

type resolvedDirectRunOptions struct {
	runRouting
	runPlacement
	runContainerRuntime
	runDirectStorage
	runResourceLimits
	runDirectMetrics

	script   string
	launcher string
	configs  map[string]any
}

type resolvedRunJobOptions struct {
	resolvedDirectRunOptions
	runProfile

	workingDir              string
	source                  *runconfig.Source
	volumeSpecs             []string
	mountSpecs              []string
	imageAssets             []runconfig.ImageAsset
	jobGPUs                 *int
	processesPerNode        int
	nodes                   int
	ttlSecondsAfterFinished int64
}

type resolvedRunRayJobOptions struct {
	resolvedDirectRunOptions
	runRayJobResources
	runRayJobTuning

	workingDir         string
	workingDirExcludes []string
	runtimePip         []string
}

type resolvedRunManagedWorkflowOptions struct {
	runRouting
	runPlacement
	runContainerRuntime
	runProfile

	file                    string
	mainScript              string
	dataPVC                 string
	workloadKind            string
	upstreamCheckpoint      string
	extraScripts            []string
	envKV                   []string
	keyVault                string
	kvTenantID              string
	kvClientID              string
	secretPayloadPath       string
	workers                 int
	cpuWorkers              int
	smokePairs              int
	ttlSecondsAfterFinished int64
}

func cloneRunPlacement(o runPlacement) runPlacement {
	o.nodeSelectors = slices.Clone(o.nodeSelectors)
	return o
}

func cloneRunContainerRuntime(o runContainerRuntime) runContainerRuntime {
	o.env = slices.Clone(o.env)
	o.envSecrets = slices.Clone(o.envSecrets)
	return o
}

func cloneRunDirectMetrics(o runDirectMetrics) runDirectMetrics {
	o.metricsHistory = slices.Clone(o.metricsHistory)
	return o
}

func resolveDirectRunOptions(o unresolvedRunOptions) resolvedDirectRunOptions {
	return resolvedDirectRunOptions{
		runRouting:          o.runRouting,
		runPlacement:        cloneRunPlacement(o.runPlacement),
		runContainerRuntime: cloneRunContainerRuntime(o.runContainerRuntime),
		runDirectStorage:    o.runDirectStorage,
		runResourceLimits:   o.runResourceLimits,
		runDirectMetrics:    cloneRunDirectMetrics(o.runDirectMetrics),
		script:              o.script,
		launcher:            o.launcher,
		configs:             o.configs,
	}
}

func resolveRunJobOptions(o unresolvedRunOptions) resolvedRunJobOptions {
	return resolvedRunJobOptions{
		resolvedDirectRunOptions: resolveDirectRunOptions(o),
		runProfile:               o.runProfile,
		workingDir:               o.workingDir.jobContainerPath(),
		source:                   o.source,
		volumeSpecs:              slices.Clone(o.volumeSpecs),
		mountSpecs:               slices.Clone(o.mountSpecs),
		imageAssets:              slices.Clone(o.imageAssets),
		jobGPUs:                  o.jobGPUs,
		processesPerNode:         o.processesPerNode,
		nodes:                    o.nodes,
		ttlSecondsAfterFinished:  o.ttlSecondsAfterFinished,
	}
}

func resolveRunRayJobOptions(o unresolvedRunOptions) resolvedRunRayJobOptions {
	return resolvedRunRayJobOptions{
		resolvedDirectRunOptions: resolveDirectRunOptions(o),
		runRayJobResources:       o.runRayJobResources,
		runRayJobTuning:          o.runRayJobTuning,
		workingDir:               o.workingDir.rayProjectPath(),
		workingDirExcludes:       slices.Clone(o.workingDir.excludes),
		runtimePip:               slices.Clone(o.runtimePip),
	}
}

func resolveRunManagedWorkflowOptions(o unresolvedRunOptions) resolvedRunManagedWorkflowOptions {
	return resolvedRunManagedWorkflowOptions{
		runRouting:              o.runRouting,
		runPlacement:            cloneRunPlacement(o.runPlacement),
		runContainerRuntime:     cloneRunContainerRuntime(o.runContainerRuntime),
		runProfile:              o.runProfile,
		file:                    o.file,
		mainScript:              o.mainScript,
		dataPVC:                 o.dataPVC,
		workloadKind:            o.workloadKind,
		upstreamCheckpoint:      o.upstreamCheckpoint,
		extraScripts:            slices.Clone(o.extraScripts),
		envKV:                   slices.Clone(o.envKV),
		keyVault:                o.keyVault,
		kvTenantID:              o.kvTenantID,
		kvClientID:              o.kvClientID,
		secretPayloadPath:       o.secretPayloadPath,
		workers:                 o.workers,
		cpuWorkers:              o.cpuWorkers,
		smokePairs:              o.smokePairs,
		ttlSecondsAfterFinished: o.ttlSecondsAfterFinished,
	}
}

type resolvedRunRequest interface {
	execute(context.Context, io.Writer, io.Writer, string) error
	namespace() string
}

type resolvedRunTarget = resolvedRunRequest

func (r *runJobRequest) execute(ctx context.Context, stdout, stderr io.Writer, captureCommand string) error {
	return executeRunJob(ctx, stdout, stderr, r, captureCommand)
}

func (r *runJobRequest) namespace() string {
	return r.Options.namespace
}

func (r *runRayJobRequest) execute(ctx context.Context, stdout, stderr io.Writer, captureCommand string) error {
	return executeRunRayJob(ctx, stdout, stderr, r, captureCommand)
}

func (r *runRayJobRequest) namespace() string {
	return r.Options.namespace
}

func (r *runManagedWorkflowRequest) execute(ctx context.Context, stdout, stderr io.Writer, captureCommand string) error {
	return executeRunManagedWorkflow(ctx, stdout, stderr, r, captureCommand)
}

func (r *runManagedWorkflowRequest) namespace() string {
	return r.Options.namespace
}
