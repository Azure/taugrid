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
	attachAuthoritativeProfileForTest(&options)

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
		o.profileName = "azure.research.training.l"
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
		o.profileName = "azure.research.training.l"
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

func TestRayJobDispatchUsesProfileNamespace(t *testing.T) {
	dir := t.TempDir()
	script := writeRayScript(t, dir)
	rendered := runRayJobDryRun(t, "ray-ns", func(o *runDispatchOptions) {
		o.script = script
		o.profileName = "test.training"
		o.namespace = "profile-ns"
	})
	if !strings.Contains(rendered, "namespace: profile-ns") {
		t.Fatalf("dry-run should render the authorized profile namespace:\n%s", rendered)
	}
}

func TestExecuteRunTargetWritesBackResolvedRayJobNamespace(t *testing.T) {
	dir := t.TempDir()
	options := defaultRunDispatchOptions()
	options.engine = "rayjob"
	options.script = writeRayScript(t, dir)
	options.profileName = "test.training"
	options.namespace = "ray-retry-ns"
	options.dryRun = "client"
	attachAuthoritativeProfileForTest(&options)

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

func TestExecuteRunRayJobClientDryRunUsesAuthoritativeProfileRouting(t *testing.T) {
	dir := t.TempDir()
	script := writeRayScript(t, dir)
	options := defaultRunDispatchOptions()
	options.engine = "rayjob"
	options.dryRun = "client"
	options.script = script
	options.workers = 2
	attachAuthoritativeProfileForTest(&options)
	setAuthoritativeProfileCardinalityForTest(&options, 1, 2)

	request, err := newRunRayJobRequest(options, "offline-ray")
	if err != nil {
		t.Fatalf("newRunRayJobRequest: %v", err)
	}
	var out, stderr bytes.Buffer
	if err := executeRunRayJob(context.Background(), &out, &stderr, &request, "tau run --config tau.yaml"); err != nil {
		t.Fatalf("client dry-run with an authoritative profile: %v\nstderr:\n%s", err, stderr.String())
	}
	rendered := out.String()
	if !strings.Contains(rendered, "kind: RayJob") {
		t.Fatalf("client dry-run did not render a RayJob:\n%s", rendered)
	}
	if !strings.Contains(rendered, "namespace: test-workspace") ||
		!strings.Contains(rendered, "kueue.x-k8s.io/queue-name: jobqueue") {
		t.Fatalf("client dry-run did not use authoritative profile routing:\n%s", rendered)
	}
	if strings.Contains(rendered, "unresolved") {
		t.Fatalf("authoritative profile routing was replaced by a placeholder:\n%s", rendered)
	}
	if strings.Contains(stderr.String(), "resolved at submit") {
		t.Fatalf("authoritative profile routing was reported as unresolved:\n%s", stderr.String())
	}
}
