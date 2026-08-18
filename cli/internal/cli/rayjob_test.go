// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
)

// RayJob has no command root of its own: `tau run --config` with
// `engine: rayjob` resolves a typed runRayJobRequest and calls executeRunRayJob
// (run.go, resolveRunTarget). These tests drive that same path directly.

func runRayJobDryRun(t *testing.T, name string, mutate func(*runDispatchOptions)) string {
	t.Helper()
	options := defaultRunDispatchOptions()
	options.engine = "rayjob"
	options.dryRun = "client"
	mutate(&options)

	request, err := newRunRayJobRequest(options, name)
	if err != nil {
		t.Fatalf("newRunRayJobRequest: %v", err)
	}
	var out, stderr bytes.Buffer
	if err := executeRunRayJob(context.Background(), &out, &stderr, &request, "tau run --config tau.yaml"); err != nil {
		t.Fatalf("ray dry-run failed: %v\nstderr:\n%s", err, stderr.String())
	}
	return out.String()
}

func writeRayScript(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("from ray.train.torch import TorchTrainer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestRayJobDispatchDryRunUsesPresetKueueLane(t *testing.T) {
	script := writeRayScript(t, t.TempDir())
	rendered := runRayJobDryRun(t, "ray-smoke", func(o *runDispatchOptions) {
		o.script = script
		o.preset = "azure.research.training.l"
		o.runtimePip = []string{"torch==2.4.0"}
		o.workspace = "sample"
		o.workspaceResultScope = "/data/projects/sample/runs"
	})
	for _, want := range []string{
		"kind: RayJob",
		"kueue.x-k8s.io/queue-name: jobqueue",
		workloadmeta.LabelManagedBy + ": tau",
		workloadmeta.LabelRunID + ": ray-smoke",
		workloadmeta.LabelWorkloadKind + ": rayjob",
		"suspend: true",
		"python3 /script/train.py",
		"torch==2.4.0",
		workloadmeta.LabelWorkspace + `: sample`,
		workloadmeta.AnnotationWorkspaceID + `: sample`,
		workloadmeta.AnnotationResultScope + `: /data/projects/sample/runs`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("dry-run missing %q:\n%s", want, rendered)
		}
	}
}

func TestRayJobDispatchMIGProfileForcesMIGResourceEvenWithConflictingMode(t *testing.T) {
	script := writeRayScript(t, t.TempDir())
	rendered := runRayJobDryRun(t, "ray-mig", func(o *runDispatchOptions) {
		o.script = script
		o.preset = "azure.research.training.l"
		o.gpusPerWorker = 1
		o.migProfile = "1g.18gb"
		o.gpuResourceMode = "device-plugin"
	})
	if !strings.Contains(rendered, "nvidia.com/mig-1g.18gb") {
		t.Fatalf("mig_profile must render the MIG resource even with a conflicting gpu_resource_mode:\n%s", rendered)
	}
	if strings.Contains(rendered, "nvidia.com/gpu:") {
		t.Fatalf("MIG request must not fall back to nvidia.com/gpu:\n%s", rendered)
	}
}

func TestRayJobDispatchUsesPresetNamespaceWhenNamespaceOmitted(t *testing.T) {
	dir := t.TempDir()
	script := writeRayScript(t, dir)
	policy := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policy, []byte(`apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-policy }
spec:
  presets:
    test.training:
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 4xgpu
      gpuClass: any
      queue: research-training
      clusterQueue: research-cq
      namespace: preset-ns
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := runRayJobDryRun(t, "ray-ns", func(o *runDispatchOptions) {
		o.script = script
		o.preset = "test.training"
		o.topologyPolicy = policy
	})
	if !strings.Contains(rendered, "namespace: preset-ns") {
		t.Fatalf("dry-run should render preset namespace when no namespace is set:\n%s", rendered)
	}
}

func TestExecuteRunTargetWritesBackResolvedRayJobNamespace(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policy, []byte(`apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata: { name: test-policy }
spec:
  presets:
    test.training:
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: research-training
      clusterQueue: research-cq
      namespace: ray-retry-ns
`), 0o644); err != nil {
		t.Fatal(err)
	}
	options := defaultRunDispatchOptions()
	options.engine = "rayjob"
	options.script = writeRayScript(t, dir)
	options.preset = "test.training"
	options.topologyPolicy = policy
	options.dryRun = "client"

	target, err := resolveRunTarget(options, "ray-retry")
	if err != nil {
		t.Fatal(err)
	}
	parent := &cobra.Command{Use: "run"}
	parent.SetContext(context.Background())
	parent.SetOut(&bytes.Buffer{})
	parent.SetErr(&bytes.Buffer{})
	if err := executeRunTarget(parent, target, "tau run", runExperimentMetadata{}); err != nil {
		t.Fatalf("executeRunTarget: %v", err)
	}
	if got := resolvedRayJobRequestForTest(target).Options.namespace; got != "ray-retry-ns" {
		t.Fatalf("resolved ray namespace = %q, want %q", got, "ray-retry-ns")
	}
}

func TestRayJobDispatchRejectsUnsupportedStorageAndMissingEntrypoint(t *testing.T) {
	base := func() runDispatchOptions {
		o := defaultRunDispatchOptions()
		o.engine = "rayjob"
		o.script = "train.py"
		return o
	}
	if _, err := newRunRayJobRequest(base(), ""); err == nil {
		t.Fatal("RayJob dispatch without NAME should fail")
	}
	missingScript := base()
	missingScript.script = ""
	if _, err := newRunRayJobRequest(missingScript, "ray-run"); err == nil {
		t.Fatal("RayJob dispatch without run.entrypoint should fail")
	}
	conflictingPVC := base()
	conflictingPVC.dataPVC = "a"
	conflictingPVC.resultPVC = "b"
	if _, err := newRunRayJobRequest(conflictingPVC, "ray-run"); err == nil {
		t.Fatal("RayJob dispatch with diverging data/result PVCs should fail")
	}
	withMounts := base()
	withMounts.mountSpecs = []string{"extra=pvc:other:/mnt/extra"}
	if _, err := newRunRayJobRequest(withMounts, "ray-run"); err == nil {
		t.Fatal("RayJob dispatch with storage.mounts should fail")
	}
}

func TestRayJobRequestedGPUCountIncludesWorkerReplicas(t *testing.T) {
	if got := rayJobRequestedGPUCount(10, 4); got != 40 {
		t.Fatalf("gpu demand = %d, want 40", got)
	}
	if got := rayJobRequestedGPUCount(10, 0); got != 0 {
		t.Fatalf("cpu-only demand = %d, want 0", got)
	}
}

// TestExecuteRunRayClientDryRunWithoutNamespace mirrors the engine=job guard:
// namespace and queue resolution both need a live cluster, so an offline
// client dry-run must still render rather than demand values it is forbidden
// to look up.
func TestExecuteRunRayJobClientDryRunWithoutNamespace(t *testing.T) {
	dir := t.TempDir()
	script := writeRayScript(t, dir)
	options := defaultRunDispatchOptions()
	options.engine = "rayjob"
	options.dryRun = "client"
	options.script = script
	options.workers = 2
	// options.namespace and options.queue deliberately left empty.

	request, err := newRunRayJobRequest(options, "offline-ray")
	if err != nil {
		t.Fatalf("newRunRayJobRequest: %v", err)
	}
	var out, stderr bytes.Buffer
	if err := executeRunRayJob(context.Background(), &out, &stderr, &request, "tau run --config tau.yaml"); err != nil {
		t.Fatalf("client dry-run must render offline without a namespace, got: %v\nstderr:\n%s", err, stderr.String())
	}
	rendered := out.String()
	if !strings.Contains(rendered, "kind: RayJob") {
		t.Fatalf("client dry-run did not render a RayJob:\n%s", rendered)
	}
	if !strings.Contains(rendered, clientDryRunNamespacePlaceholder) {
		t.Fatalf("client dry-run namespace is not marked as unresolved:\n%s", rendered)
	}
	if strings.Contains(rendered, "namespace: default") {
		t.Fatalf("client dry-run rendered a plausible-but-wrong namespace:\n%s", rendered)
	}
	if !strings.Contains(stderr.String(), "resolved at submit") {
		t.Fatalf("client dry-run must warn that namespace is unresolved, stderr:\n%s", stderr.String())
	}
}
