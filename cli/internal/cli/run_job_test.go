// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestResolveRunTargetUsesTypedJobExecutor(t *testing.T) {
	zeroGPUs := 0
	options := defaultRunDispatchOptions()
	options.engine = "job"
	options.profileName = "tau-cpu-smoke"
	options.script = "train.sh"
	options.image = "busybox:1.36"
	options.jobGPUs = &zeroGPUs
	options.env = []string{"FOO=bar"}
	options.serviceAccountName = "tau-workload"

	target, err := resolveRunTarget(options, "typed-job")
	if err != nil {
		t.Fatal(err)
	}

	if target.job == nil {
		t.Fatalf("Job run did not resolve to the typed Job executor: %#v", target)
	}
	if target.job.Name != "typed-job" ||
		target.job.Options.script != "train.sh" ||
		target.job.Options.image != "busybox:1.36" ||
		target.job.Options.serviceAccountName != "tau-workload" {
		t.Fatalf("typed Job request lost config fields: %#v", target.job)
	}
}

func TestResolveDirectJobGPUCountRejectsShapeConflict(t *testing.T) {
	one := 1
	if _, err := resolveDirectJobGPUCount(&one, "8x", nil); err == nil || !strings.Contains(err.Error(), "compute.gpus=1 conflicts") {
		t.Fatalf("expected explicit GPU and shape conflict, got %v", err)
	}
}

func TestResolveDirectJobGPUCountUsesPresetShape(t *testing.T) {
	got, err := resolveDirectJobGPUCount(nil, "1xgpu", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("preset/shape GPU count = %d, want 1", got)
	}
}

func TestWorkspaceQueueValidationKeepsOnlyPresetTopologyContract(t *testing.T) {
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{
			Namespace:      "preset-namespace",
			ClusterQueue:   "preset-cq",
			ResourceFlavor: "preset-flavor",
			TopologyName:   "default-node-topology",
		},
		Options: runtopology.Options{
			QueueName: "preset-queue",
			Team:      "preset-team",
			Lane:      "training",
			GPUClass:  "h200",
		},
	}
	got := queueValidationPolicyFor(preset, true)
	if got.Preset != nil {
		t.Fatal("workspace validation retained preset queue ownership")
	}
	if got.TopologyName != "default-node-topology" {
		t.Fatalf("workspace validation policy = %+v", got)
	}
	if !got.CatalogTopologyContract {
		t.Fatal("workspace validation did not retain the catalog topology contract")
	}
	if got := queueValidationPolicyFor(preset, false); got.Preset != preset {
		t.Fatal("non-workspace preset validation must be preserved")
	}
	preset.Preset.ResourceFlavor = ""
	if got := queueValidationPolicyFor(preset, true); !got.CatalogTopologyContract {
		t.Fatalf("flavorless workspace TAS policy = %+v", got)
	}
	preset.Preset.TopologyName = ""
	if got := queueValidationPolicyFor(preset, true); got.CatalogTopologyContract {
		t.Fatalf("topology-free workspace preset retained capability flavor: %+v", got)
	}
}

func TestNewRunJobRequestIsolatesFreshMetricsSessionsAndPreservesRetry(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.metricsOffloadEnabled = true

	first, err := newRunJobRequest(options, "reused-name")
	if err != nil {
		t.Fatal(err)
	}

	second, err := newRunJobRequest(options, "reused-name")
	if err != nil {
		t.Fatal(err)
	}
	if first.Options.metricsSessionID == "" || second.Options.metricsSessionID == "" {
		t.Fatalf("fresh requests did not get metrics session IDs: first=%q second=%q", first.Options.metricsSessionID, second.Options.metricsSessionID)
	}
	if first.Options.metricsSessionID == second.Options.metricsSessionID {
		t.Fatalf("fresh requests reused metrics session %q", first.Options.metricsSessionID)
	}

	retry, err := newRunJobRequest(first.Options, "reused-name")
	if err != nil {
		t.Fatal(err)
	}
	if retry.Options.metricsSessionID != first.Options.metricsSessionID {
		t.Fatalf("retry session = %q, want %q", retry.Options.metricsSessionID, first.Options.metricsSessionID)
	}
}

func TestNewRunJobRequestIsolatesArtifactPublicationGenerations(t *testing.T) {
	options := defaultRunDispatchOptions()
	options.outputPublish = "staged"
	first, err := newRunJobRequest(options, "reused-name")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newRunJobRequest(options, "reused-name")
	if err != nil {
		t.Fatal(err)
	}
	if first.Options.artifactPublicationID == "" || first.Options.artifactPublicationID == second.Options.artifactPublicationID {
		t.Fatalf("fresh artifact publication IDs = %q and %q", first.Options.artifactPublicationID, second.Options.artifactPublicationID)
	}
	retry, err := newRunJobRequest(first.Options, "reused-name")
	if err != nil {
		t.Fatal(err)
	}
	if retry.Options.artifactPublicationID != first.Options.artifactPublicationID {
		t.Fatalf("retry publication ID = %q, want %q", retry.Options.artifactPublicationID, first.Options.artifactPublicationID)
	}
}

func TestResolveDirectJobMetricsOffloadProtectsWorkspaceScope(t *testing.T) {
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "registry.example.com/taugrid/tau:v0.6.0")
	t.Setenv("TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT", "http://${NODE_IP}:3100/receive")
	o := defaultRunDispatchOptions()
	o.workspace = "research-workspace"
	o.metricsSessionID = "session-a"
	o.metricsHistory = []string{"metrics-history-attempt-*/*.jsonl", "/data/shared/eval.jsonl"}
	o.experiment = runExperimentMetadata{
		Project:      "pretraining",
		ExperimentID: "modernbert-bounded",
		RunGroupID:   "fwe100",
		Tags: map[string]string{
			"dataset":       "fineweb-edu",
			"tau_workspace": "researcher-override",
			"tau_namespace": "researcher-override",
			"tau_cluster":   "researcher-override",
		},
	}
	o.env = []string{"TAU_RETRY_ATTEMPT=2"}
	p := profile.Profile{
		Name: "direct",
		Spec: map[string]any{
			"metrics": map[string]any{
				"offload": map[string]any{
					"tags": "owner=platform,tau_workspace=profile-override",
				},
			},
		},
	}
	runtime, err := resolveMetricsOffload(
		o,
		p,
		"modernbert-bounded",
		"research-workspace",
		"sample-gpu-cluster",
		"/data/research-workspace/modernbert-bounded",
		true,
		map[string]string{workloadmeta.AnnotationResultPVC: "research-workspace"},
	)
	if err != nil {
		t.Fatalf("resolveMetricsOffload: %v", err)
	}
	if runtime.Experiment != "modernbert-bounded" {
		t.Fatalf("experiment = %q, want modernbert-bounded", runtime.Experiment)
	}
	if got, want := runtime.History[0], "/data/research-workspace/modernbert-bounded/metrics-history-attempt-*/*.jsonl"; got != want {
		t.Fatalf("relative history = %q, want %q", got, want)
	}
	for key, want := range map[string]string{
		"tau_workspace":     "research-workspace",
		"tau_namespace":     "research-workspace",
		"tau_cluster":       "sample-gpu-cluster",
		"tau_retry_attempt": "2",
		"dataset":           "fineweb-edu",
		"owner":             "platform",
	} {
		if got := runtime.Tags[key]; got != want {
			t.Fatalf("tag %s = %q, want %q; tags=%v", key, got, want, runtime.Tags)
		}
	}
	if runtime.ArtifactURI != "/data/research-workspace/modernbert-bounded" {
		t.Fatalf("artifact URI = %q", runtime.ArtifactURI)
	}
	if runtime.CompletionFile != "/var/run/tau/metrics-completion.json" {
		t.Fatalf("completion file = %q", runtime.CompletionFile)
	}
	wantStore := "/var/run/tau/metrics/session-a/expstore"
	wantOut := "/data/research-workspace/modernbert-bounded/.tau/metrics/session-a/offload"
	if runtime.Store != wantStore || runtime.Out != wantOut {
		t.Fatalf("session state paths = store %q out %q, want store %q out %q", runtime.Store, runtime.Out, wantStore, wantOut)
	}
	if !runtime.BaselineExistingHistory || runtime.ReadyFile != "/var/run/tau/metrics-ready" {
		t.Fatalf("fresh history gate = baseline %v ready %q", runtime.BaselineExistingHistory, runtime.ReadyFile)
	}
	if runtime.DoneFile != "/var/run/tau/metrics-done" || runtime.DoneTimeout <= 0 {
		t.Fatalf("terminal publication gate = done %q timeout %s", runtime.DoneFile, runtime.DoneTimeout)
	}
}

func TestConfigMetricsOffloadUsesProfileGroupWhenConfigOmitsGroup(t *testing.T) {
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "registry.example.com/taugrid/tau:v0.6.0")
	o, err := configToDispatch(runconfig.Config{
		Engine: "job",
		Metrics: runconfig.Metrics{
			History: []string{"metrics-history.jsonl"},
			Offload: runconfig.MetricsOffload{Enabled: true},
		},
		Experiment: runconfig.Experiment{
			Project: "pretraining",
			Name:    "modernbert-bounded",
		},
	}, filepath.Join(t.TempDir(), "tau.yaml"))
	if err != nil {
		t.Fatalf("configToDispatch: %v", err)
	}
	o.workspace = "research-workspace"
	o.metricsSessionID = "session-profile-group"
	p := profile.Profile{
		Name: "direct",
		Spec: map[string]any{
			"metrics": map[string]any{
				"offload": map[string]any{
					"group": "profile-arm",
				},
			},
		},
	}

	runtime, err := resolveMetricsOffload(
		o,
		p,
		"modernbert-bounded",
		"research-workspace",
		"sample-gpu-cluster",
		"/data/research-workspace/modernbert-bounded",
		true,
		map[string]string{workloadmeta.AnnotationResultPVC: "research-workspace"},
	)
	if err != nil {
		t.Fatalf("resolveMetricsOffload: %v", err)
	}
	if runtime.Group != "profile-arm" {
		t.Fatalf("group = %q, want profile-arm", runtime.Group)
	}
}

func TestResolveDirectJobMetricsOffloadRejectsReadOnlyOutput(t *testing.T) {
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "registry.example.com/taugrid/tau:v0.6.0")
	o := defaultRunDispatchOptions()
	o.workspace = "research-workspace"
	o.metricsSessionID = "session-read-only"
	o.metricsHistory = []string{"metrics-history.jsonl"}
	o.experiment = runExperimentMetadata{
		Project:      "pretraining",
		ExperimentID: "modernbert-bounded",
	}

	_, err := resolveMetricsOffload(
		o,
		profile.Profile{},
		"modernbert-bounded",
		"research-workspace",
		"sample-gpu-cluster",
		"/data/research-workspace/modernbert-bounded",
		false,
		map[string]string{workloadmeta.AnnotationResultPVC: "research-workspace"},
	)
	if err == nil || !strings.Contains(err.Error(), "writable PVC") {
		t.Fatalf("expected read-only output rejection, got %v", err)
	}
}

func TestExecuteRunJobRendersOptInMetricsProducer(t *testing.T) {
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "registry.example.com/taugrid/tau:v0.6.0")
	script := filepath.Join(t.TempDir(), "train.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nset -eu\nchunk_dir=\"$TAU_OUTPUT_DIR/metrics-history-attempt-0\"\nmkdir -p \"$chunk_dir\"\nprintf '{\"step\":1,\"loss\":1.0}\\n' > \"$chunk_dir/chunk-000001.jsonl.tmp\"\nmv \"$chunk_dir/chunk-000001.jsonl.tmp\" \"$chunk_dir/chunk-000001.jsonl\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.engine = "job"
	o.profileName = "tau-cpu-smoke"
	o.namespace = "research-workspace"
	o.queue = "jobqueue"
	o.image = "mcr.microsoft.com/azurelinux/base/python:3.12"
	o.script = script
	o.dataPVC = "research-workspace"
	o.output = "/data/research-workspace/modernbert-bounded"
	o.outputPublish = "staged"
	o.artifactPublicationID = "publication-render"
	o.workspace = "research-workspace"
	o.metricsSessionID = "session-render"
	o.metricsOffloadEnabled = true
	o.metricsHistory = []string{"metrics-history-attempt-*/*.jsonl"}
	o.experiment = runExperimentMetadata{
		Workspace:    "research-workspace",
		Project:      "pretraining",
		ExperimentID: "modernbert-bounded",
		RunGroupID:   "fwe100",
	}
	o.dryRun = "client"
	var stdout, stderr bytes.Buffer
	ctx := withRunExperimentMetadata(context.Background(), o.experiment)
	err := executeRunJob(ctx, &stdout, &stderr, &runJobRequest{
		Name:    "modernbert-bounded",
		Options: o,
	}, "tau run --config tau.yaml")
	if err != nil {
		t.Fatalf("executeRunJob: %v\nstderr:\n%s", err, stderr.String())
	}
	rendered := stdout.String()
	for _, want := range []string{
		"name: metrics-offload",
		"registry.example.com/taugrid/tau:v0.6.0",
		workloadmeta.AnnotationExperimentSource + ": stellar",
		workloadmeta.AnnotationStellarExperimentID + ": modernbert-bounded",
		"--experiment",
		"modernbert-bounded",
		"tau_workspace=research-workspace",
		"tau_namespace=research-workspace",
		"/data/research-workspace/modernbert-bounded/metrics-history-attempt-*/*.jsonl",
		"/var/run/tau/metrics/session-render/expstore",
		workloadmeta.AnnotationMetricsSession + ": session-render",
		"baseline-existing-history",
		"/var/run/tau/metrics-ready",
		"/var/run/tau/metrics-completion.json",
		"/var/run/tau/metrics-done",
		"--done-file",
		"status-artifact-uri",
		"TAU_OUTPUT_STAGING_DIR",
		"/mnt/tau-output/modernbert-bounded",
		workloadmeta.AnnotationArtifactPublication + ": staged",
		workloadmeta.AnnotationArtifactPublicationID + ": publication-render",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered direct Job missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, workloadmeta.AnnotationStellarExperimentTitle) {
		t.Fatalf("rendered direct Job contains retired title annotation:\n%s", rendered)
	}
}

func TestRunJobDryRunPreservesTypedConfig(t *testing.T) {
	zeroGPUs := 0
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.Mkdir(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "typed-job.yaml"), []byte(`apiVersion: tau.azure.com/v1alpha1
kind: Profile
metadata:
  name: typed-job
spec:
  queue: { localQueue: training-queue }
  resources:
    requests: { cpu: "1", memory: 1Gi }
  runtime:
    image: busybox:1.36
`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	options := defaultRunDispatchOptions()
	options.engine = "job"
	options.profileName = "typed-job"
	options.queue = "training-queue"
	options.jobGPUs = &zeroGPUs
	options.script = script
	options.dryRun = "client"
	options.namespace = "ray"
	options.volumeSpecs = []string{"data=pvc:training-nfs"}
	options.mountSpecs = []string{"data:/workspace"}
	options.output = "/workspace/results"
	options.env = []string{"FOO=bar"}
	options.envSecrets = []string{"HF_TOKEN=hf-secret:token"}
	options.nodeSelectors = []string{"agentpool=cpu"}
	options.cpuRequest = "500m"
	options.memoryRequest = "2Gi"
	options.cpuLimit = "2"
	options.memoryLimit = "4Gi"
	options.profiler = "nsys"
	options.profileWarmup = "30s"
	options.profileDuration = "90s"
	options.serviceAccountName = "tau-workload"
	options.azureWorkloadIdentity = true
	options.disableDefaultPriorities = true

	target, err := resolveRunTarget(options, "typed-job")
	if err != nil {
		t.Fatal(err)
	}
	parent := &cobra.Command{Use: "run"}
	parent.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	parent.SetOut(&stdout)
	parent.SetErr(&stderr)
	if err := executeRunTarget(
		parent,
		target,
		"tau run --config tau.yaml --dry-run client",
		runExperimentMetadata{Project: "typed-project", RunGroupID: "typed-experiment"},
	); err != nil {
		t.Fatalf("execute typed Job: %v\nstderr:\n%s", err, stderr.String())
	}
	rendered := stdout.String()
	for _, want := range []string{
		"kind: Job",
		"name: typed-job",
		"namespace: ray",
		"claimName: training-nfs",
		"mountPath: /workspace",
		"agentpool: cpu",
		"name: FOO",
		"value: bar",
		"secretKeyRef:",
		"name: <redacted>",
		"key: <redacted>",
		"serviceAccountName: tau-workload",
		`azure.workload.identity/use: "true"`,
		"name: TAU_OUTPUT_DIR",
		"value: /workspace/results",
		"name: TAU_PROFILE_MODE",
		"value: nsys",
		workloadmeta.AnnotationProfilerWarmup + ": 30s",
		workloadmeta.AnnotationProfilerDuration + ": 1m30s",
		workloadmeta.AnnotationTauCommand + ": tau run --config tau.yaml --dry-run client",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("typed Job dry-run missing %q:\n%s", want, rendered)
		}
	}
	for _, leaked := range []string{"hf-secret", "token"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("typed Job dry-run leaked secret ref %q:\n%s", leaked, rendered)
		}
	}
	resources := renderedJobResources(t, rendered)
	assertResourceQuantities(t, resources, "requests", map[string]string{"cpu": "500m", "memory": "2Gi"})
	assertResourceQuantities(t, resources, "limits", map[string]string{"cpu": "2", "memory": "4Gi"})
}

func TestRunJobProfilerRequiresDurablePVC(t *testing.T) {
	zeroGPUs := 0
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.Mkdir(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "typed-job.yaml"), []byte(`apiVersion: tau.azure.com/v1alpha1
kind: Profile
metadata:
  name: typed-job
spec:
  queue: { localQueue: training-queue }
  resources:
    requests: { cpu: "1", memory: 1Gi }
  runtime:
    image: busybox:1.36
`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	options := defaultRunDispatchOptions()
	options.engine = "job"
	options.profileName = "typed-job"
	options.jobGPUs = &zeroGPUs
	options.script = script
	options.dryRun = "client"
	options.profiler = "nsys"
	target, err := resolveRunTarget(options, "profile-no-pvc")
	if err != nil {
		t.Fatal(err)
	}
	parent := &cobra.Command{Use: "run"}
	parent.SetContext(context.Background())
	parent.SetOut(&bytes.Buffer{})
	parent.SetErr(&bytes.Buffer{})
	err = executeRunTarget(parent, target, "tau run --config tau.yaml", runExperimentMetadata{})
	if err == nil || !strings.Contains(err.Error(), "durable PVC output") {
		t.Fatalf("expected durable PVC error, got %v", err)
	}
}

func TestExecuteRunTargetWritesBackResolvedNamespace(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	zeroGPUs := 0
	options := defaultRunDispatchOptions()
	options.engine = "job"
	options.profileName = "direct-job"
	options.jobGPUs = &zeroGPUs
	options.queue = "training-queue"
	options.script = script
	options.image = "busybox:1.36"
	options.dryRun = "client"
	options.namespace = ""
	options.disableDefaultPriorities = true

	target, err := resolveRunTarget(options, "ns-writeback")
	if err != nil {
		t.Fatal(err)
	}
	if target.job == nil {
		t.Fatal("expected job target")
	}
	if target.job.Options.namespace != "" {
		t.Fatalf("precondition: namespace should start empty, got %q", target.job.Options.namespace)
	}
	parent := &cobra.Command{Use: "run"}
	parent.SetContext(context.Background())
	parent.SetOut(&bytes.Buffer{})
	parent.SetErr(&bytes.Buffer{})
	if err := executeRunTarget(parent, target, "tau run", runExperimentMetadata{}); err != nil {
		t.Fatalf("executeRunTarget: %v", err)
	}
	if target.job.Options.namespace == "" {
		t.Fatal("namespace not written back after executeRunTarget: still empty")
	}
}

func TestExecuteRunTargetWritesBackResolvedNamespaceRay(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.py")
	if err := os.WriteFile(script, []byte("import ray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := defaultRunDispatchOptions()
	options.engine = "ray"
	options.script = script
	options.image = "busybox:1.36"
	options.dryRun = "client"
	options.namespace = "demo"
	options.disableDefaultPriorities = true

	target, err := resolveRunTarget(options, "ray-ns-writeback")
	if err != nil {
		t.Fatal(err)
	}
	if target.ray == nil {
		t.Fatal("expected ray target")
	}
	parent := &cobra.Command{Use: "run"}
	parent.SetContext(context.Background())
	parent.SetOut(&bytes.Buffer{})
	parent.SetErr(&bytes.Buffer{})
	if err := executeRunTarget(parent, target, "tau run", runExperimentMetadata{}); err != nil {
		t.Fatalf("executeRunTarget: %v", err)
	}
	// Verify the retry dispatch switch reads the resolved namespace from
	// the ray target (not from stale targetOptions).
	retryDispatch, ok := target.dispatchOptions()
	if !ok {
		t.Fatal("dispatchOptions returned false")
	}
	if retryDispatch.namespace != "demo" {
		t.Fatalf("ray target namespace not propagated: got %q, want %q", retryDispatch.namespace, "demo")
	}
}

func TestFormatRunJobSubmission(t *testing.T) {
	out := formatRunJobSubmission("train-001", "ai-train-gpu-m", "ray", "research-admin")
	for _, want := range []string{
		"submitted train-001 (profile=ai-train-gpu-m, ns=ray)",
		"status:  tau run status train-001 -n ray --context research-admin",
		"logs:    tau run logs train-001 -n ray -f --context research-admin",
		"profile: tau run status train-001 -n ray --run-profile --context research-admin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run output missing %q:\n%s", want, out)
		}
	}
}

func TestResolvePVCMountsWithExplicitMount(t *testing.T) {
	pvc, volumes, mounts, err := resolvePVCMounts(
		[]string{"nfs=pvc:training-nfs"},
		[]string{"nfs:/data-nfs:ro"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pvc != "" {
		t.Fatalf("explicit mount should not use durable shortcut, got pvc=%q", pvc)
	}
	if len(volumes) != 1 || volumes[0].Name != "nfs" || volumes[0].PVC != "training-nfs" {
		t.Fatalf("bad volumes: %+v", volumes)
	}
	if len(mounts) != 1 || mounts[0].Name != "nfs" || mounts[0].MountPath != "/data-nfs" || !mounts[0].ReadOnly {
		t.Fatalf("bad mounts: %+v", mounts)
	}
}

type fakePriorityChecker map[string]bool

func (f fakePriorityChecker) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	if len(args) >= 3 && args[0] == "get" {
		key := args[1] + "/" + args[2]
		if f[key] {
			return key + "\n", nil
		}
	}
	return "", fmt.Errorf("not found")
}

func TestAutoDisableMissingDefaultPriorities(t *testing.T) {
	disabled, warning := autoDisableMissingDefaultPriorities(context.Background(), fakePriorityChecker{})
	if !disabled {
		t.Fatal("missing priority classes should auto-disable defaults")
	}
	if !strings.Contains(warning, "taugrid-default") || !strings.Contains(warning, "disable_default_priorities") {
		t.Fatalf("warning should be actionable, got %q", warning)
	}
}

func TestAutoDisableDefaultPrioritiesKeepsExistingClasses(t *testing.T) {
	disabled, warning := autoDisableMissingDefaultPriorities(context.Background(), fakePriorityChecker{
		"workloadpriorityclass.kueue.x-k8s.io/taugrid-default": true,
		"priorityclass/taugrid-default":                        true,
	})
	if disabled || warning != "" {
		t.Fatalf("existing classes should keep default priorities, disabled=%v warning=%q", disabled, warning)
	}
}

func TestBuildRunJobOutputAnnotationsAcceptsExplicitMountPath(t *testing.T) {
	annotations, outputDir, writable, err := buildRunJobOutputAnnotations(
		"j1",
		"/data-nfs/runs/j1",
		"",
		[]jobrender.Volume{{Name: "nfs", PVC: "training-nfs"}},
		[]jobrender.VolumeMount{{Name: "nfs", MountPath: "/data-nfs"}},
		profile.Profile{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !writable {
		t.Fatal("expected writable output mount")
	}
	if annotations[workloadmeta.AnnotationResultPath] != "/data-nfs/runs/j1" ||
		annotations[workloadmeta.AnnotationResultPVC] != "training-nfs" ||
		outputDir != "/data-nfs/runs/j1" {
		t.Fatalf("output metadata = %#v, outputDir=%q", annotations, outputDir)
	}
}

func TestBuildRunJobOutputAnnotationsRejectsOutsidePVC(t *testing.T) {
	_, _, _, err := buildRunJobOutputAnnotations(
		"j1",
		"/elsewhere/results",
		"training-nfs",
		nil,
		nil,
		profile.Profile{},
	)
	if err == nil || !strings.Contains(err.Error(), "not under any mounted PVC path") {
		t.Fatalf("expected mounted PVC validation error, got %v", err)
	}
}

func TestBuildRunJobOutputAnnotationsReportsReadOnlyProfilePersistence(t *testing.T) {
	p := profile.Profile{
		Spec: map[string]any{
			"resources": map[string]any{
				"persistence": map[string]any{
					"pvcName":   "data",
					"mountPath": "/data",
					"readOnly":  true,
				},
			},
		},
	}
	annotations, outputDir, writable, err := buildRunJobOutputAnnotations(
		"j1",
		"/data/runs/j1",
		"",
		nil,
		nil,
		p,
	)
	if err != nil {
		t.Fatal(err)
	}
	if writable {
		t.Fatal("read-only profile persistence must not be reported as writable")
	}
	if annotations[workloadmeta.AnnotationResultPVC] != "data" || outputDir != "/data/runs/j1" {
		t.Fatalf("output metadata = %#v, outputDir=%q", annotations, outputDir)
	}
}

func TestBuildRunJobProfileAnnotationsUsesOutputDir(t *testing.T) {
	annotations := buildRunJobProfileAnnotations("/data-nfs/runs/vision-001", "training-nfs", jobrender.ProfileOptions{
		Mode: "nsys",
		Rank: "0",
	})
	if annotations[workloadmeta.AnnotationProfilerPath] != "/data-nfs/runs/vision-001/profile" ||
		annotations[workloadmeta.AnnotationProfilerPVC] != "training-nfs" ||
		annotations[workloadmeta.AnnotationProfilerMode] != "nsys" {
		t.Fatalf("profile annotations = %#v", annotations)
	}
}

// TestExecuteRunJobClientDryRunWithoutQueue asserts that offline client
// dry-run renders a complete Job even when no queue was supplied. Queue
// resolution deliberately requires a live cluster (topology_flags.go), so a
// client dry-run can never carry a resolved LocalQueue; failing the render
// instead would make the documented "always follow validate with a client
// dry-run" step impossible for engine=job.
func TestExecuteRunJobClientDryRunWithoutQueue(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.engine = "job"
	o.profileName = "tau-cpu-smoke"
	o.image = "mcr.microsoft.com/azurelinux/base/python:3.12"
	o.script = script
	o.dryRun = "client"
	// o.queue and o.namespace deliberately left empty: this is the
	// config-only path, where both are resolved server-side at submit.

	var stdout, stderr bytes.Buffer
	err := executeRunJob(context.Background(), &stdout, &stderr, &runJobRequest{
		Name:    "offline-render",
		Options: o,
	}, "tau run --config tau.yaml --dry-run client")
	if err != nil {
		t.Fatalf("client dry-run must render offline without a queue, got: %v\nstderr:\n%s", err, stderr.String())
	}
	rendered := stdout.String()
	if !strings.Contains(rendered, "kind: Job") {
		t.Fatalf("client dry-run did not render a Job:\n%s", rendered)
	}
	if !strings.Contains(rendered, "suspend: true") {
		t.Fatalf("client dry-run lost the Kueue suspend contract:\n%s", rendered)
	}
	// The placeholder must be visibly a placeholder, not a plausible real
	// queue name that a reader would mistake for the resolved value.
	if !strings.Contains(rendered, clientDryRunQueuePlaceholder) {
		t.Fatalf("client dry-run queue label is not marked as unresolved:\n%s", rendered)
	}
	if !strings.Contains(rendered, clientDryRunNamespacePlaceholder) {
		t.Fatalf("client dry-run namespace is not marked as unresolved:\n%s", rendered)
	}
	// "default" is a real namespace on every cluster. Rendering it here would
	// read as the resolved answer while being unrelated to where the workload
	// actually lands.
	if strings.Contains(rendered, "namespace: default") {
		t.Fatalf("client dry-run rendered a plausible-but-wrong namespace:\n%s", rendered)
	}
	if !strings.Contains(stderr.String(), "queue and namespace") || !strings.Contains(stderr.String(), "resolved at submit") {
		t.Fatalf("client dry-run must warn that queue and namespace are unresolved, stderr:\n%s", stderr.String())
	}
}

// TestExecuteRunJobServerDryRunStillRequiresQueue is the mutation guard for the
// test above: the #1263 fail-closed protection against submitting a
// permanently-suspended Job must survive on every non-client path. Only the
// offline client path substitutes a visible placeholder.
//
// An explicit namespace is what keeps this offline — it makes
// resolveAccessibleQueueNamespace return before any cluster call, so the render
// is reached with the queue still empty.
func TestExecuteRunJobServerDryRunStillRequiresQueue(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.engine = "job"
	o.profileName = "tau-cpu-smoke"
	o.image = "mcr.microsoft.com/azurelinux/base/python:3.12"
	o.script = script
	o.dryRun = "server"
	o.namespace = "team-ns"
	// o.queue deliberately left empty.

	var stdout, stderr bytes.Buffer
	err := executeRunJob(context.Background(), &stdout, &stderr, &runJobRequest{
		Name:    "server-render",
		Options: o,
	}, "tau run --config tau.yaml --dry-run server")
	if err == nil {
		t.Fatalf("server dry-run must refuse to render a suspended Job with no queue; got:\n%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "Kueue LocalQueue is required") {
		t.Fatalf("server dry-run failed for the wrong reason: %v", err)
	}
	if strings.Contains(stdout.String(), clientDryRunQueuePlaceholder) || strings.Contains(stdout.String(), clientDryRunNamespacePlaceholder) {
		t.Fatalf("client dry-run placeholders leaked onto the server path:\n%s", stdout.String())
	}
}

// TestExecuteRunJobClientDryRunWithExplicitValuesDoesNotWarn is the end-to-end
// counterpart: when the researcher supplied both values, nothing is substituted
// and the render must not claim their inputs will be re-resolved.
func TestExecuteRunJobClientDryRunWithExplicitValuesDoesNotWarn(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.engine = "job"
	o.profileName = "tau-cpu-smoke"
	o.image = "mcr.microsoft.com/azurelinux/base/python:3.12"
	o.script = script
	o.dryRun = "client"
	o.namespace = "team-ns"
	o.queue = "team-queue"

	var stdout, stderr bytes.Buffer
	if err := executeRunJob(context.Background(), &stdout, &stderr, &runJobRequest{
		Name:    "explicit-render",
		Options: o,
	}, "tau run --config tau.yaml --dry-run client"); err != nil {
		t.Fatalf("client dry-run with explicit values: %v\nstderr:\n%s", err, stderr.String())
	}
	rendered := stdout.String()
	if !strings.Contains(rendered, "namespace: team-ns") || !strings.Contains(rendered, "team-queue") {
		t.Fatalf("explicit queue/namespace were not honored:\n%s", rendered)
	}
	if strings.Contains(rendered, "unresolved") {
		t.Fatalf("explicit values were replaced by a placeholder:\n%s", rendered)
	}
	if strings.Contains(stderr.String(), "resolved at submit") {
		t.Fatalf("warned that the researcher's own explicit values are unresolved:\n%s", stderr.String())
	}
}

// TestClientDryRunPlaceholdersReadAsUnresolved pins the one property the whole
// approach rests on: a reader must not mistake a placeholder for a resolved
// value. Angle brackets are illegal in RFC 1123 names, so the value is also
// rejected server-side if it ever escapes.
func TestClientDryRunPlaceholdersReadAsUnresolved(t *testing.T) {
	for _, placeholder := range []string{clientDryRunQueuePlaceholder, clientDryRunNamespacePlaceholder} {
		if !strings.Contains(placeholder, "unresolved") {
			t.Errorf("placeholder %q must read as unresolved, not as a plausible name", placeholder)
		}
		if !strings.HasPrefix(placeholder, "<") || !strings.HasSuffix(placeholder, ">") {
			t.Errorf("placeholder %q must be angle-bracketed so it cannot be a valid RFC 1123 name", placeholder)
		}
	}
}

// TestClientDryRunPlaceholderWarningNamesOnlySubstitutedFields guards against
// telling a researcher their own explicit --namespace "is resolved at submit",
// which reads as "your flag was ignored". The suppressed-entirely case is
// covered end to end by TestExecuteRunJobClientDryRunWithExplicitValuesDoesNotWarn.
func TestClientDryRunPlaceholderWarningNamesOnlySubstitutedFields(t *testing.T) {
	got := clientDryRunPlaceholderWarning("namespace")
	if strings.Contains(got, "queue") {
		t.Fatalf("warning names a field that was not substituted: %q", got)
	}
	if !strings.Contains(got, "namespace") || !strings.Contains(got, "resolved at submit") {
		t.Fatalf("warning does not explain the unresolved namespace: %q", got)
	}
}
