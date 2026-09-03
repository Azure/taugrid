// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/payload"
	"github.com/Azure/taugrid/cli/internal/reposcaffold"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
)

func TestPortalRayStellarExampleUsesCanonicalExperimentIdentity(t *testing.T) {
	cfg, err := runconfig.Load(filepath.Join("..", "..", "..", "examples", "portal-ray-stellar", "tau.yaml"))
	if err != nil {
		t.Fatalf("parse portal-ray-stellar config: %v", err)
	}
	meta := runconfig.ExperimentRunMetadata(cfg.Experiment)
	if meta.ExperimentID != "ray-plus-stellar" || meta.RunGroupID != "default" {
		t.Fatalf("experiment identity = %#v", meta)
	}
	if cfg.Experiment.Title != "" {
		t.Fatalf("portal-ray-stellar still uses the retired experiment.title field")
	}
	if cfg.Entrypoint != "train.py" || cfg.Run.Script != "" {
		t.Fatalf("entrypoint = %q, run.script = %q; want canonical top-level entrypoint", cfg.Entrypoint, cfg.Run.Script)
	}
	if cfg.Storage.ResultPVC != "" {
		t.Fatalf("result_pvc = %q; data_pvc already defines the writable result claim", cfg.Storage.ResultPVC)
	}
	if got, want := strings.Join(cfg.Metrics.History, ","), "metrics-history-attempt-0/*.jsonl"; got != want {
		t.Fatalf("metrics.history = %q, want immutable chunk glob %q", got, want)
	}
	driver, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "portal-ray-stellar", "train.py"))
	if err != nil {
		t.Fatalf("read portal-ray-stellar driver: %v", err)
	}
	for _, unwanted := range []string{"ray.train.torch", "TorchTrainer", "import torch"} {
		if strings.Contains(string(driver), unwanted) {
			t.Fatalf("portal-ray-stellar carries undeclared framework dependency %q", unwanted)
		}
	}
	for _, want := range []string{"@ray.remote", `strategy="STRICT_SPREAD"`} {
		if !strings.Contains(string(driver), want) {
			t.Fatalf("portal-ray-stellar driver missing %q", want)
		}
	}
}

func TestPortalRayStellarExampleDryRun(t *testing.T) {
	// The example deliberately names policy.workspace. With no usable
	// kubeconfig in this test, success guards that client dry-run retains the
	// name for metrics metadata without fetching the live TauWorkspace.
	const offloadImage = "registry.example.com/taugrid-portal@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", offloadImage)
	config := filepath.Clean("../../../examples/portal-ray-stellar/tau.yaml")
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: RayJob",
		"name: portal-ray-stellar",
		"claimName: blob-training",
		"name: metrics-offload",
		offloadImage,
		workloadmeta.AnnotationExperimentSource + ": stellar",
		workloadmeta.AnnotationStellarExperimentID + ": ray-plus-stellar",
		"/data/projects/taugrid-default/runs/portal-ray-stellar/metrics-history-attempt-0/*.jsonl",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("portal Ray + Stellar dry-run missing %q:\n%s", want, rendered)
		}
	}
}

func TestMarketPolicyExampleResolvesCheckedInMetricsOffloadSettings(t *testing.T) {
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "")
	t.Setenv("TAU_METRICS_OFFLOAD_OUT", "")
	config := filepath.Clean("../../../examples/market-policy/tau.yaml")
	options, _, err := loadRunConfig(config)
	if err != nil {
		t.Fatalf("load market-policy config: %v", err)
	}
	options.workspace = "default"
	options.metricsSessionID = "market-policy-test"
	runtime, err := resolveMetricsOffload(
		options,
		"market-policy",
		"default",
		"test-context",
		options.output,
		true,
		map[string]string{workloadmeta.AnnotationResultPVC: options.dataPVC},
	)
	if err != nil {
		t.Fatalf("resolve market-policy metrics offload: %v", err)
	}
	if got, want := runtime.Image, "mcr.microsoft.com/aks/ai-runtime/taugrid-portal:0.4.0"; got != want {
		t.Fatalf("metrics offload image = %q, want %q", got, want)
	}
	if got, want := runtime.Out, "/var/run/tau/metrics-offload"; got != want {
		t.Fatalf("metrics offload out = %q, want %q", got, want)
	}
	if got, want := strings.Join(runtime.History, ","), "/data/market-policy/metrics-history-attempt-0/*.jsonl"; got != want {
		t.Fatalf("metrics history = %q, want %q", got, want)
	}
}

func TestDirectConfigHashTracksConfigNotScriptAlone(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "train.py")
	if err := os.WriteFile(script, []byte("print('same script')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "tau.yaml")
	write := func(cpu string) {
		t.Helper()
		raw := fmt.Sprintf(`name: hash-test
engine: job
entrypoint: train.py
runtime:
  image: busybox:1.36
  env:
    EPOCHS: "1"
compute:
  gpus: 0
  cpu_request: %q
policy:
  queue: jobqueue
  namespace: tests
`, cpu)
		if err := os.WriteFile(config, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	render := func() map[string]any {
		t.Helper()
		text := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
		var manifest map[string]any
		if err := yaml.Unmarshal([]byte(text), &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest["metadata"].(map[string]any)["annotations"].(map[string]any)
	}

	write("1")
	first := render()
	write("2")
	second := render()
	wantScriptDigest, err := experiment.HashFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if first[workloadmeta.AnnotationScriptPayloadDigest] != wantScriptDigest {
		t.Fatalf("script payload digest = %v, want %s", first[workloadmeta.AnnotationScriptPayloadDigest], wantScriptDigest)
	}
	if first[workloadmeta.AnnotationConfigHash] == second[workloadmeta.AnnotationConfigHash] {
		t.Fatalf("config hash did not change: %v", first[workloadmeta.AnnotationConfigHash])
	}
	if first[workloadmeta.AnnotationScriptPayloadDigest] != second[workloadmeta.AnnotationScriptPayloadDigest] {
		t.Fatalf("script payload digest changed with only config: first=%v second=%v", first, second)
	}
}

func TestWorkflowFileConfigHashTracksWrapperAndManifest(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "tau.yaml")
	manifestPath := filepath.Join(root, "manifest.yaml")
	if err := os.WriteFile(filepath.Join(root, "train.py"), []byte("print('train')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeWrapper := func(image string) {
		t.Helper()
		raw := fmt.Sprintf(`schema_version: 1
name: workflow-hash
workflow:
  file: manifest.yaml
  main_script: train.py
  workload_kind: rayjob
runtime:
  image: %s
policy:
  namespace: tests
  queue: jobqueue
  disable_default_priorities: true
`, image)
		if err := os.WriteFile(config, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest := func(gpus int) {
		t.Helper()
		raw := fmt.Sprintf(`schema_version: 1
name: workflow-hash
run:
  entrypoint: train.py
runtime:
  pip:
    - numpy
compute:
  gpus: %d
`, gpus)
		if err := os.WriteFile(manifestPath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loadHash := func() string {
		t.Helper()
		_, options, _, _, err := readRunConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		return options.configHash
	}

	writeWrapper("example.com/train:v1")
	writeManifest(0)
	initial := loadHash()
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	var workload map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &workload); err != nil {
		t.Fatal(err)
	}
	annotations := workload["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got := fmt.Sprint(annotations[workloadmeta.AnnotationConfigHash]); got != initial {
		t.Fatalf("submitted config hash = %q, loaded hash = %q", got, initial)
	}

	writeWrapper("example.com/train:v2")
	if changed := loadHash(); changed == initial {
		t.Fatal("wrapper change did not change config hash")
	}
	writeWrapper("example.com/train:v1")
	writeManifest(1)
	if changed := loadHash(); changed == initial {
		t.Fatal("manifest change did not change config hash")
	}
}

func TestManagedManifestConfigHashUsesRawInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tau.yaml")
	raw := []byte("schema_version: 1\nname: managed-hash\nrun:\n  entrypoint: train.py\ncompute:\n  gpus: 0\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, options, _, _, err := readRunConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := experiment.HashBytes(raw); options.configHash != want {
		t.Fatalf("managed config hash = %q, want %q", options.configHash, want)
	}
}

func TestDiscoverRunInputResolvesNamedTrainConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "tau", "train.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("name: train\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := discoverRunInput(root, "", "train")
	if err != nil {
		t.Fatalf("discoverRunInput: %v", err)
	}
	if got.ConfigPath != configPath || got.ExplicitConfig {
		t.Fatalf("discovery = %#v", got)
	}
}

func TestDiscoverRunInputResolvesNamedSmokeConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "tau", "smoke.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("name: project-smoke\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := discoverRunInput(root, "", "smoke")
	if err != nil {
		t.Fatalf("discoverRunInput: %v", err)
	}
	if got.ConfigPath != configPath || got.ExplicitConfig {
		t.Fatalf("smoke discovery = %#v", got)
	}
}

func TestDiscoverRunInputUnknownNameIsActionable(t *testing.T) {
	_, err := discoverRunInput(t.TempDir(), "", "missing")
	if err == nil || !strings.Contains(err.Error(), "tau/missing.yaml") {
		t.Fatalf("expected named config guidance, got %v", err)
	}
}

func TestRunConfigJobDryRun(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho train\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profileDir := filepath.Join(dir, "profiles")
	if err := os.Mkdir(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "test-submit.yaml"), []byte(`apiVersion: tau.azure.com/v1alpha1
kind: Profile
metadata:
  name: test-submit
spec:
  queue: { localQueue: training-queue }
  resources:
    requests: { cpu: "1", memory: 1Gi }
  runtime:
    image: busybox:1.36
`), 0o644); err != nil {
		t.Fatal(err)
	}

	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: config-job
engine: job
entrypoint: train.sh
runtime:
  image: busybox:1.36
  env_secret:
    HF_TOKEN: hf-secret:token-key
compute:
  gpus: 0
  cpu_request: "1"
  memory_request: 4Gi
  cpu_limit: "2"
  memory_limit: 8Gi
policy:
  profile: test-submit
  queue: training-queue
  namespace: ray
  disable_default_priorities: true
storage:
  data_pvc: training-nfs
  output: /data/checkpoints/workflows/config-job
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client", "--service-account", "tau-workload"})
	for _, want := range []string{
		"kind: Job",
		"name: config-job",
		"claimName: training-nfs",
		"TAU_OUTPUT_DIR",
		"/data/checkpoints/workflows/config-job",
		"secretKeyRef:",
		"name: <redacted>",
		"key: <redacted>",
		"serviceAccountName: tau-workload",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("config job dry-run missing %q:\n%s", want, rendered)
		}
	}
	for _, leaked := range []string{"hf-secret", "token-key"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("config job dry-run leaked secret ref %q:\n%s", leaked, rendered)
		}
	}
	if strings.Contains(rendered, "azure.workload.identity/use") {
		t.Fatalf("non-workspace Job should not gain the Azure workload identity label:\n%s", rendered)
	}
	resources := renderedJobResources(t, rendered)
	assertResourceQuantities(t, resources, "requests", map[string]string{"cpu": "1", "memory": "4Gi"})
	assertResourceQuantities(t, resources, "limits", map[string]string{"cpu": "2", "memory": "8Gi"})
}

func TestRunConfigJobGPUIntentRendersDevicePluginResources(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`name: direct-job-gpu
engine: job
entrypoint: %q
compute:
  gpus: 1
  gpu_resource_mode: nvidia
runtime:
  image: python:3.12
policy:
  namespace: tau-default
  queue: jobqueue
  lane: training
  mode: fixed
`, script)), 0o600); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	if !strings.Contains(rendered, "kueue.x-k8s.io/queue-name: jobqueue") {
		t.Fatalf("workspace-compatible LocalQueue was not rendered:\n%s", rendered)
	}
	if got := strings.Count(rendered, "nvidia.com/gpu: 1"); got != 2 {
		t.Fatalf("expected one GPU in requests and limits, found %d occurrences:\n%s", got, rendered)
	}
}

func TestRunConfigJobRejectsRayGPUIntent(t *testing.T) {
	tests := []struct {
		name    string
		compute string
		profile string
		want    string
	}{
		{
			name:    "ray gpu field",
			compute: "compute:\n  gpus_per_worker: 1\n",
			profile: "rune-gpu-train",
			want:    "use compute.gpus for a direct Job",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "train.py")
			if err := os.WriteFile(script, []byte("print('ok')\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "tau.yaml")
			if err := os.WriteFile(config, []byte(fmt.Sprintf(`name: invalid-job-gpu
engine: job
entrypoint: %q
%sruntime:
  image: python:3.12
policy:
  profile: %s
  namespace: default
`, script, tt.compute, tt.profile)), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := newConnectedRunConfigTestCommand(t, []string{"run", "--config", config, "--dry-run=client"})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestGeneratedScaffoldJobResourcesDryRun(t *testing.T) {
	dir := t.TempDir()
	if _, err := reposcaffold.Render(reposcaffold.Options{
		Name:      "modernbert-baseline",
		OutputDir: dir,
		Image:     "example.azurecr.io/modernbert:test",
		Workspace: "modernbert-test",
	}); err != nil {
		t.Fatalf("render scaffold: %v", err)
	}

	for _, tc := range []struct {
		name     string
		config   string
		requests map[string]string
		limits   map[string]string
	}{
		{
			name:     "smoke",
			config:   "smoke.yaml",
			requests: map[string]string{"cpu": "1", "memory": "4Gi"},
			limits:   map[string]string{"cpu": "2", "memory": "8Gi"},
		},
		{
			name:     "train",
			config:   "train.yaml",
			requests: map[string]string{"cpu": "2", "memory": "4Gi"},
			limits:   map[string]string{"cpu": "4", "memory": "8Gi"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(dir, "tau", tc.config)
			raw, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), "policy:\n", "policy:\n  queue: jobqueue\n", 1))
			if err := os.WriteFile(configPath, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			rendered := executeTauConfigDryRun(t, []string{
				"run",
				"--config", configPath,
				"--dry-run=client",
			})
			resources := renderedJobResources(t, rendered)
			assertResourceQuantities(t, resources, "requests", tc.requests)
			assertResourceQuantities(t, resources, "limits", tc.limits)
		})
	}
}

func TestRunConfigKindSmokeExampleDryRun(t *testing.T) {
	config := filepath.Clean("../../../examples/kind-smoke/tau.yaml")
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: Job",
		"name: tau-kind-smoke",
		"namespace: ray",
		"image: bash:5.2",
		"kueue.x-k8s.io/queue-name: kind-cpu",
		"kueue.x-k8s.io/priority-class: taugrid-default",
		"priorityClassName: taugrid-default",
		"cpu: 10m",
		"memory: 32Mi",
		"TAU_SCRIPT_B64",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("kind smoke dry-run missing %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{
		"nvidia.com/gpu",
		workloadmeta.LabelGPUClass,
	} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("kind smoke dry-run should not request GPU field %q:\n%s", unwanted, rendered)
		}
	}
}

func TestRunConfigKindRayExampleDryRun(t *testing.T) {
	config := filepath.Clean("../../../examples/kind-smoke/tau-ray.yaml")
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: RayJob",
		"name: tau-kind-ray",
		"namespace: ray",
		"image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0",
		"kueue.x-k8s.io/queue-name: kind-cpu",
		"kueue.x-k8s.io/priority-class: taugrid-default",
		"priorityClassName: taugrid-default",
		`num-gpus: "0"`,
		"cpu: 5m",
		"cpu: 10m",
		"memory: 256Mi",
		"memory: 512Mi",
		"memory: 768Mi",
		"memory: 6Gi",
		workloadmeta.AnnotationPayloadDigest,
		"name: tau-payload",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("kind Ray smoke dry-run missing %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{"nvidia.com/gpu", "kind: ConfigMap"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("kind Ray smoke dry-run should not contain %q (RayJob is now self-contained):\n%s", unwanted, rendered)
		}
	}

	// The driver script is no longer visible as plaintext in the rendered
	// YAML (it's embedded as a base64 payload) — decode it the same way the
	// tau-payload initContainer would, and confirm the staged file still
	// contains the expected script content.
	files := decodeRenderedRayPayload(t, rendered)
	trainPy, ok := files["ray_train.py"]
	if !ok {
		t.Fatalf("decoded payload missing ray_train.py: keys=%v", filesKeys(files))
	}
	for _, want := range []string{"class WorkerProbe", "wait_for_ray_cpus(2)", "len(set(worker_nodes)) != 2", "tau kind ray smoke complete"} {
		if !strings.Contains(string(trainPy), want) {
			t.Fatalf("decoded ray_train.py missing %q:\n%s", want, trainPy)
		}
	}
}

func TestRunConfigMultiNodeJobDryRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('indexed job')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: config-indexed
engine: job
entrypoint: train.py
runtime:
  image: mcr.microsoft.com/azurelinux/base/python:3.12
compute:
  gpus: 0
  cpu_request: "1"
  memory_request: 512Mi
policy:
  namespace: ray
  queue: jobqueue
  disable_default_priorities: true
storage:
  data_pvc: shared-data
execution:
  launcher: torchrun
  nodes: 2
  processes_per_node: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: Service",
		"kind: Job",
		"completionMode: Indexed",
		"completions: 2",
		"parallelism: 2",
		"claimName: shared-data",
		"--nnodes=2",
		"--node_rank=$JOB_COMPLETION_INDEX",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("multi-node Job dry-run missing %q:\n%s", want, rendered)
		}
	}
}

// TestRunConfigAKSGPUQuickstartExampleDryRun guards the checked-in A100
// quickstart config. The GPU request, the CUDA-tagged MCR image, and the
// RAY_ACCEL_ENV_VAR_OVERRIDE_ON_ZERO opt-out are all load-bearing: without the
// last one Ray blanks CUDA_VISIBLE_DEVICES on the worker and the run fails on a
// healthy A100.
func TestRunConfigAKSGPUQuickstartExampleDryRun(t *testing.T) {
	config := filepath.Clean("../../../examples/aks-gpu-quickstart/tau.yaml")
	rendered := executeTauConfigDryRun(t, []string{
		"run", "--config", config, "--namespace", "default", "--dry-run=client",
	})
	for _, want := range []string{
		"kind: RayJob",
		"image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0",
		`nvidia.com/gpu: "1"`,
		"RAY_ACCEL_ENV_VAR_OVERRIDE_ON_ZERO",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("aks-gpu-quickstart dry-run missing %q:\n%s", want, rendered)
		}
	}
}

// TestRunConfigAKSGPUQuickstartResourcePrefixIsLoadBearing binds the one
// empirical claim the example's comments make about resource_naming.
//
// The claim needs a test rather than prose because the default-valued render is
// not evidence either way: manifest.go's defaultResourcePrefix is itself "tau",
// so `prefix: tau` restates the default and renders identically whether the key
// is read or silently dropped. Reading that output actively manufactures
// confidence in whichever answer you already hold. Only perturbing the value
// discriminates, so that is what this does -- if resource_naming ever stops
// being read on the managed-workflow path, this fails by name instead of the
// comment going quietly false.
func TestRunConfigAKSGPUQuickstartResourcePrefixIsLoadBearing(t *testing.T) {
	const probePrefix = "zzprobe"

	source := filepath.Clean("../../../examples/aks-gpu-quickstart")
	original, err := os.ReadFile(filepath.Join(source, "tau.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	perturbed := strings.Replace(string(original), "prefix: tau", "prefix: "+probePrefix, 1)
	if perturbed == string(original) {
		t.Fatalf("example config no longer contains `prefix: tau`; update this probe")
	}

	// The entrypoint is a relative path, so it has to travel with the config.
	dir := t.TempDir()
	entrypoint, err := os.ReadFile(filepath.Join(source, "train.py"))
	if err != nil {
		t.Fatalf("read example entrypoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train.py"), entrypoint, 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(perturbed), 0o644); err != nil {
		t.Fatalf("write perturbed config: %v", err)
	}

	rendered := executeTauConfigDryRun(t, []string{
		"run", "--config", config, "--namespace", "default", "--dry-run=client",
	})
	if want := "name: " + probePrefix + "-aks-gpu-quickstart"; !strings.Contains(rendered, want) {
		t.Fatalf("resource_naming.prefix is not load-bearing: missing %q.\n"+
			"Either the managed-workflow path stopped reading resource_naming, or "+
			"routing changed and this config no longer reaches cli/internal/manifest.\n%s",
			want, rendered)
	}
}

// TestRunConfigAKSCPUQuickstartExampleDryRun guards the CPU quickstart config.
// The non-CUDA image tag and the absence of any GPU request are the properties
// that keep it runnable on a CPU-only cluster.
func TestRunConfigAKSCPUQuickstartExampleDryRun(t *testing.T) {
	config := filepath.Clean("../../../examples/aks-cpu-quickstart/tau.yaml")
	rendered := executeTauConfigDryRun(t, []string{
		"run", "--config", config, "--namespace", "default", "--dry-run=client",
	})
	for _, want := range []string{
		"kind: RayJob",
		"image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("aks-cpu-quickstart dry-run missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "nvidia.com/gpu") {
		t.Fatalf("aks-cpu-quickstart dry-run should not request GPUs:\n%s", rendered)
	}
}

// decodeRenderedRayPayload extracts the tau-payload initContainer's env
// vars from a rendered RayJob and decodes/verifies the embedded payload,
// mirroring what the initContainer does at pod startup.
func decodeRenderedRayPayload(t *testing.T, rendered string) map[string][]byte {
	t.Helper()
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &manifest); err != nil {
		t.Fatalf("parse rendered RayJob: %v\n%s", err, rendered)
	}
	spec := manifest["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	pod := head["template"].(map[string]any)["spec"].(map[string]any)
	initContainers := pod["initContainers"].([]any)
	var ic map[string]any
	for _, raw := range initContainers {
		candidate := raw.(map[string]any)
		envs, _ := candidate["env"].([]any)
		for _, e := range envs {
			v := e.(map[string]any)
			if v["name"] == payload.EnvTargetDir && v["value"] == "/script" {
				ic = candidate
				break
			}
		}
		if ic != nil {
			break
		}
	}
	if ic == nil {
		t.Fatalf("expected a script payload initContainer, got %d initContainers", len(initContainers))
	}
	var encoded, digest string
	for _, e := range ic["env"].([]any) {
		v := e.(map[string]any)
		switch v["name"] {
		case payload.EnvB64:
			encoded, _ = v["value"].(string)
		case payload.EnvDigest:
			digest, _ = v["value"].(string)
		}
	}
	if encoded == "" || digest == "" {
		t.Fatalf("initContainer env missing payload b64/digest: %v", ic["env"])
	}
	files, err := payload.Decode(encoded, digest)
	if err != nil {
		t.Fatalf("payload.Decode: %v", err)
	}
	return files
}

func filesKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestRunConfigRayJobDryRun(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: config-ray
engine: rayjob
entrypoint: train.py
runtime:
  image: example.com/research/ray:cuda13
  pip:
    - torch==2.4.0
  env:
    HF_HOME: /data/hf
  env_secret:
    HF_TOKEN: hf-secret:token-key
storage:
  data_pvc: vision-lustre
policy:
  namespace: ray
  queue: team-a
  disable_default_priorities: true
experiment:
  project: NanoGPT FineWeb
  title: NanoGPT API surface
  group: Safe Stack/H200
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client", "--service-account", "tau-workload"})
	for _, want := range []string{
		"kind: RayJob",
		"name: config-ray",
		"kueue.x-k8s.io/queue-name: team-a",
		"claimName: vision-lustre",
		"image: example.com/research/ray:cuda13",
		"torch==2.4.0",
		"name: HF_HOME",
		"value: /data/hf",
		"secretKeyRef:",
		"name: <redacted>",
		"key: <redacted>",
		"serviceAccountName: tau-workload",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("config RayJob dry-run missing %q:\n%s", want, rendered)
		}
	}
	for _, leaked := range []string{"hf-secret", "token-key"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("config RayJob dry-run leaked secret ref %q:\n%s", leaked, rendered)
		}
	}
	if got := strings.Count(rendered, "serviceAccountName: tau-workload"); got != 2 {
		t.Fatalf("GPU config RayJob dry-run serviceAccountName count=%d want head + worker (2):\n%s", got, rendered)
	}
	for _, want := range []string{
		"key: kubernetes.azure.com/mode",
		"operator: In",
		"- system",
		"operator: DoesNotExist",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("GPU config RayJob dry-run must give its CPU head portable system affinity; missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "azure.workload.identity/use") {
		t.Fatalf("non-workspace RayJob should not gain the Azure workload identity label:\n%s", rendered)
	}
}

func TestRunConfigLegacyRayEngineAliasDryRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: legacy-ray-alias
engine: ray
entrypoint: train.py
runtime:
  image: python:3.12
compute:
  gpus_per_worker: 0
policy:
  namespace: ray
  queue: team-a
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	if !strings.Contains(rendered, "kind: RayJob") {
		t.Fatalf("legacy engine: ray alias did not render a RayJob:\n%s", rendered)
	}
}

func TestRunConfigRunImageAliasDryRun(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`run:
  name: config-ray-run-image
  engine: rayjob
  entrypoint: train.py
  image: example.com/research/ray:run-block
runtime:
  pip:
    - torch==2.4.0
policy:
  namespace: ray
  queue: team-a
  disable_default_priorities: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"name: config-ray-run-image",
		"image: example.com/research/ray:run-block",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("run.image dry-run missing %q:\n%s", want, rendered)
		}
	}
}

func TestRunConfigWorkflowRuntimeImageOverride(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(`schema_version: 1
name: workflow-image
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: workflow-image
workflow:
  file: manifest.yaml
  main_script: train.py
  workload_kind: rayjob
runtime:
  image: example.com/research/ray:workflow
policy:
  namespace: ray
  queue: team-a
  disable_default_priorities: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: RayJob",
		"image: example.com/research/ray:workflow",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("workflow runtime.image dry-run missing %q:\n%s", want, rendered)
		}
	}
}

func TestRunConfigPythonBuildArtifactsDryRun(t *testing.T) {
	tests := []struct {
		name  string
		wants []string
	}{
		{
			name: "train",
			wants: []string{
				"kind: RayJob",
				"train-model",
				`name: "MODE"`,
				`value: "train"`,
				`claimName: "model-cache"`,
				`mountPath: "/models"`,
			},
		},
		{
			name: "eval",
			wants: []string{
				"kind: RayJob",
				"eval-model",
				`name: "MODE"`,
				`value: "eval"`,
				"replicas: 7",
				"TAU_UPSTREAM_CHECKPOINT",
				"/data/checkpoints/finetunes/train-model/artifacts/best/model.safetensors",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"tau_py_wrapper.py", "tau_user_module.py"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("print('ok')\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(filepath.Join("testdata", "python-build", tc.name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "tau.yaml")
			if err := os.WriteFile(config, raw, 0o644); err != nil {
				t.Fatal(err)
			}

			rendered := executeTauConfigDryRun(t, []string{
				"run",
				"--config", config,
				"--dry-run=client",
			})
			for _, want := range tc.wants {
				if !strings.Contains(rendered, want) {
					t.Fatalf("Python build dry-run missing %q:\n%s", want, rendered)
				}
			}
		})
	}
}

func TestRunConfigRejectsMutableMetricsOffloadImage(t *testing.T) {
	err := executeTauConfigError(t, `name: mutable-metrics-image
engine: rayjob
entrypoint: train.py
metrics:
  offload:
    image: example.com/taugrid-portal:latest
`)
	if err == nil || !strings.Contains(err.Error(), "must not use the unpinned :latest tag") {
		t.Fatalf("expected mutable metrics offload image error, got %v", err)
	}
}

func TestRunConfigRejectsUnknownFields(t *testing.T) {
	err := executeTauConfigError(t, `name: unknown-field
engine: rayjob
entrypoint: train.py
compute:
  gpus_per_pod: 1
`)
	if err == nil || !strings.Contains(err.Error(), "field gpus_per_pod not found") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestRunConfigValidateOfflineDoesNotRequireClusterTopologyCapability(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: config-ray
engine: rayjob
entrypoint: train.py
runtime:
  pip:
    - torch==2.4.0
compute:
  workers: 2
  gpus_per_worker: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeTauCommand(t, []string{"run", "validate", "--config", config})
	if !strings.Contains(out, config+" is valid") {
		t.Fatalf("validate output = %q", out)
	}
}

func TestRunConfigValidateRejectsManagedManifestScope(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`schema_version: 1
name: managed
run:
  entrypoint: train.py
compute:
  gpus: 1
runtime:
  pip:
    - torch==2.4.0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "validate", "--config", config})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "managed workflow manifests are outside tau run validate") {
		t.Fatalf("expected managed manifest scope error, got %v", err)
	}
}

// torchrun runs the entrypoint under python3, so a shell entrypoint cannot
// work. Reject it while validating instead of at runtime, where it surfaces as
// a Python SyntaxError on the shell script.
func TestRunConfigValidateRejectsTorchrunShellEntrypoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.sh"), []byte("#!/usr/bin/env bash\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: ddp-shell
engine: job
entrypoint: train.sh
compute:
  gpus: 1
execution:
  launcher: torchrun
  processes_per_node: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "validate", "--config", config})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "must be a .py file") {
		t.Fatalf("expected .py entrypoint error, got %v", err)
	}
}

// runconfig validates a lowercased copy but leaves execution.launcher as
// written, and only jobrender.Render normalizes it. Validation runs before
// Render, so without folding the case here `validate` would report a config
// valid that `run` then rejects.
func TestRunConfigValidateRejectsTorchrunShellEntrypointMixedCase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.sh"), []byte("#!/usr/bin/env bash\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: ddp-shell-upper
engine: job
entrypoint: train.sh
compute:
  gpus: 1
execution:
  launcher: TORCHRUN
  processes_per_node: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "validate", "--config", config})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "must be a .py file") {
		t.Fatalf("expected .py entrypoint error, got %v", err)
	}
}

// A torchrun config with no entrypoint at all is a missing field, not a wrong
// file type. Validation must not answer it with the .py message.
func TestRunConfigValidateTorchrunWithoutEntrypointIsNotAFileTypeError(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: no-entry
engine: job
compute:
  gpus: 1
execution:
  launcher: torchrun
  processes_per_node: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "validate", "--config", config})
	if err := cmd.Execute(); err != nil && strings.Contains(err.Error(), "must be a .py file") {
		t.Fatalf("missing entrypoint should not report a file type error: %v", err)
	}
}

func TestRunConfigValidateAcceptsTorchrunPythonEntrypoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("import torch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: ddp-python
engine: job
entrypoint: train.py
compute:
  gpus: 1
execution:
  launcher: torchrun
  processes_per_node: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeTauCommand(t, []string{"run", "validate", "--config", config})
	if !strings.Contains(out, config+" is valid") {
		t.Fatalf("validate output = %q", out)
	}
}

// The default launcher execs the decoded script, so the shebang picks the
// interpreter and shell entrypoints stay valid.
func TestRunConfigValidateAcceptsShellEntrypointWithoutTorchrun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.sh"), []byte("#!/usr/bin/env bash\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: shell-ok
engine: job
entrypoint: train.sh
compute:
  gpus: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeTauCommand(t, []string{"run", "validate", "--config", config})
	if !strings.Contains(out, config+" is valid") {
		t.Fatalf("validate output = %q", out)
	}
}

// Managed workflows run run.main_script under python3 and ignore the launcher,
// so their run.entrypoint is not subject to the torchrun .py rule. `tau run
// validate` rejects managed manifests outright, so only the run path covers it.
func TestRunConfigManagedWorkflowTorchrunKeepsShellEntrypoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "launch.sh"), []byte("#!/usr/bin/env bash\necho launch\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`schema_version: 1
name: managed-torchrun
run:
  entrypoint: launch.sh
  main_script: train.py
policy:
  namespace: ray
  queue: jobqueue
compute:
  gpus: 1
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0
  pip:
    - torch==2.4.0
execution:
  launcher: torchrun
  processes_per_node: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
}

func TestRunConfigSchemaCommand(t *testing.T) {
	out := executeTauCommand(t, []string{"run", "schema", "-o", "json"})
	var schema map[string]any
	if err := json.Unmarshal([]byte(out), &schema); err != nil {
		t.Fatalf("schema output is not JSON: %v\n%s", err, out)
	}
	props := schema["properties"].(map[string]any)
	for _, section := range []string{"runtime", "compute", "policy", "storage", "profiler", "metrics", "experiment"} {
		if _, ok := props[section]; !ok {
			t.Fatalf("schema missing %s", section)
		}
	}
}

func TestRunConfigExplainConfigCommand(t *testing.T) {
	out := executeTauCommand(t, []string{"run", "explain-config"})
	for _, want := range []string{
		"direct `tau run --config` files",
		"`runtime.env_secret` | supported",
		"`metrics.offload` | supported",
		"`metrics.offload.enabled` | supported",
		"`metrics.offload.image` | supported",
		"`metrics.offload.out` | supported",
		"`run.ttl_seconds_after_finished` | direct-only",
		"`storage.image_assets.name` | direct-only",
		"`storage.image_assets.image` | direct-only",
		"`storage.image_assets.source_path` | direct-only",
		"`storage.image_assets.mount_path` | direct-only",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explain-config missing %q:\n%s", want, out)
		}
	}
}

func TestRunConfigAcceptsQueueAutoForLiveResolution(t *testing.T) {
	if err := validateRunDispatchOptions(runDispatchOptions{runPlacement: runPlacement{queue: "auto"}}); err != nil {
		t.Fatalf("policy.queue auto should reach live queue discovery: %v", err)
	}
}

func TestRunConfigRejectsMalformedEnvSecret(t *testing.T) {
	err := executeTauConfigError(t, `name: env-secret
engine: rayjob
entrypoint: train.py
runtime:
  env_secret:
    HF_TOKEN: hf-token
`)
	if err == nil || !strings.Contains(err.Error(), "runtime.env_secret.HF_TOKEN") || !strings.Contains(err.Error(), "expected SECRET:KEY") {
		t.Fatalf("expected explicit runtime.env_secret validation, got %v", err)
	}
}

func TestRunConfigExplicitJobRejectsRayOnlyShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{
			name: "workers",
			config: `name: explicit-job-workers
engine: job
entrypoint: train.sh
compute:
  workers: 2
`,
			want: "compute.workers=2",
		},
		{
			name: "runtime-pip",
			config: `name: explicit-job-pip
engine: job
entrypoint: train.sh
runtime:
  pip:
    - torch==2.4.0
`,
			want: "runtime.pip",
		},
		{
			name: "gpus-per-worker",
			config: `name: explicit-job-gpus-per-worker
engine: job
entrypoint: train.sh
compute:
  gpus_per_worker: 2
`,
			want: "compute.gpus_per_worker",
		},
		{
			name: "head-resource",
			config: `name: explicit-job-head-resource
engine: job
entrypoint: train.sh
compute:
  head_cpu_request: "2"
`,
			want: "compute.head_cpu_request",
		},
		{
			name: "worker-resource",
			config: `name: explicit-job-worker-resource
engine: job
entrypoint: train.sh
compute:
  worker_memory_limit: 8Gi
`,
			want: "compute.worker_memory_limit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := executeTauConfigError(t, tc.config)
			if err == nil || !strings.Contains(err.Error(), "engine=job") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected engine=job %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestRunConfigExplicitWorkloadKindJobRejectsRayInference(t *testing.T) {
	err := executeTauConfigError(t, `name: explicit-kind-job
run:
  workload_kind: job
entrypoint: train.sh
runtime:
  pip:
    - torch==2.4.0
`)
	if err == nil || !strings.Contains(err.Error(), "workload_kind=job") || !strings.Contains(err.Error(), "runtime.pip") {
		t.Fatalf("expected workload_kind=job runtime.pip error, got %v", err)
	}
}

func TestRunConfigRejectsMismatchedEngineAndWorkloadKind(t *testing.T) {
	err := executeTauConfigError(t, `name: mismatched-dispatch
engine: job
run:
  workload_kind: rayjob
entrypoint: train.py
`)
	if err == nil || !strings.Contains(err.Error(), "engine=job conflicts with workload_kind=rayjob") {
		t.Fatalf("expected mismatched dispatch error, got %v", err)
	}
}

func TestRunConfigRejectsUnsupportedWorkloadKind(t *testing.T) {
	err := executeTauConfigError(t, `name: unsupported-kind
run:
  workload_kind: rayjob-eval
entrypoint: train.py
`)
	if err == nil || !strings.Contains(err.Error(), "workload_kind must be one of") {
		t.Fatalf("expected unsupported workload_kind error, got %v", err)
	}
}

func TestRunConfigManagedWorkflowRejectsExtraPositionals(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`schema_version: 1
name: from-config
run:
  entrypoint: train.py
runtime:
  pip:
    - torch==2.4.0
compute:
  gpus: 1
storage:
  data_pvc: training-nfs
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "extra", "--config", config, "--dry-run=client"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("managed workflow configs should reject extra positional arguments")
	}
}

func TestRunConfigSchemaVersionImpliesManagedWorkflow(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`schema_version: 1
name: workflow-config
run:
  entrypoint: trainer.py
runtime:
  pip:
    - torch==2.4.0
compute:
  gpus: 1
storage:
  data_pvc: training-nfs
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, name, err := loadRunConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if name != "workflow-config" {
		t.Fatalf("name = %q, want workflow-config", name)
	}
	if got.file != config {
		t.Fatalf("schema_version config should dispatch as managed workflow manifest file %q, got %q", config, got.file)
	}
	if got.mainScript != filepath.Join(dir, "trainer.py") {
		t.Fatalf("mainScript = %q, want config-relative trainer.py", got.mainScript)
	}
}

func TestRunConfigRelativeEntrypointIsNotDoubleJoined(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "examples", "cpu")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(configDir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`schema_version: 1
name: relative-workflow
run:
  entrypoint: train.py
runtime:
  pip:
    - torch==2.4.0
compute:
  gpus: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	relConfig, err := filepath.Rel(dir, config)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()

	got, _, err := loadRunConfig(relConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("examples", "cpu", "train.py")
	if got.mainScript != want {
		t.Fatalf("mainScript = %q, want %q", got.mainScript, want)
	}
}

func executeTauConfigError(t *testing.T, configBody string) error {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newConnectedRunConfigTestCommand(t, []string{"run", "--config", config, "--dry-run=client"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

func executeTauConfigDryRun(t *testing.T, args []string) string {
	t.Helper()
	cmd := newConnectedRunConfigTestCommand(t, args)
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command %v failed: %v\nstderr:\n%s\nstdout:\n%s", args, err, stderr.String(), out.String())
	}
	return out.String()
}

func newConnectedRunConfigTestCommand(t *testing.T, args []string) *cobra.Command {
	t.Helper()
	runArgs := append([]string{}, args...)
	if len(runArgs) > 0 && runArgs[0] == "run" {
		runArgs = runArgs[1:]
	}
	configPath := ""
	for i, arg := range runArgs {
		switch {
		case arg == "--config" || arg == "-c":
			if i+1 < len(runArgs) {
				configPath = runArgs[i+1]
			}
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
		}
	}
	if configPath == "" {
		t.Fatal("connected run config test requires --config")
	}
	installClusterProfileClientForTest(t, runConfigProfileForTest(t, configPath))
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "test-context",
		Namespace:   "test-workspace",
	}}
	cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer {
		return ensurer
	})
	cmd.SetArgs(runArgs)
	return cmd
}

func renderedJobResources(t *testing.T, rendered string) map[string]any {
	t.Helper()
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &manifest); err != nil {
		t.Fatalf("parse rendered Job: %v\n%s", err, rendered)
	}
	spec := manifest["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	pod := template["spec"].(map[string]any)
	container := pod["containers"].([]any)[0].(map[string]any)
	resources, ok := container["resources"].(map[string]any)
	if !ok {
		t.Fatalf("rendered Job has no container resources:\n%s", rendered)
	}
	return resources
}

func assertResourceQuantities(t *testing.T, resources map[string]any, section string, want map[string]string) {
	t.Helper()
	values, ok := resources[section].(map[string]any)
	if !ok {
		t.Fatalf("resources.%s missing from %#v", section, resources)
	}
	for key, expected := range want {
		if actual := fmt.Sprint(values[key]); actual != expected {
			t.Errorf("resources.%s.%s=%q want %q", section, key, actual, expected)
		}
	}
}

func executeTauCommand(t *testing.T, args []string) string {
	t.Helper()
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command %v failed: %v\nstderr:\n%s\nstdout:\n%s", args, err, stderr.String(), out.String())
	}
	return out.String()
}

func TestResolveRunTargetRejectsDirectEnvKV(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.engine = "rayjob"
	o.script = "train.py"
	o.envKV = []string{"HF_TOKEN=hf-token"}
	_, err := resolveRunTarget(o, "kv-direct")
	if err == nil || !strings.Contains(err.Error(), "runtime.env_kv is only supported") {
		t.Fatalf("expected direct env_kv rejection, got %v", err)
	}
}

func TestLoadRunConfigMapsDirectMetricsOffload(t *testing.T) {
	config := filepath.Join(t.TempDir(), "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: tracked
engine: job
entrypoint: train.sh
metrics:
  history: [metrics-history-attempt-*/*.jsonl]
  offload:
    enabled: true
    image: registry.example.com/taugrid-portal:20260903.1
    out: /var/run/tau/metrics-offload
experiment:
  project: pretraining
  title: bounded run
`), 0o644); err != nil {
		t.Fatal(err)
	}

	options, _, err := loadRunConfig(config)
	if err != nil {
		t.Fatalf("loadRunConfig: %v", err)
	}
	if !options.metricsOffloadEnabled ||
		options.metricsOffloadImage != "registry.example.com/taugrid-portal:20260903.1" ||
		options.metricsOffloadOut != "/var/run/tau/metrics-offload" ||
		len(options.metricsHistory) != 1 ||
		options.metricsHistory[0] != "metrics-history-attempt-*/*.jsonl" {
		t.Fatalf("unexpected direct metrics dispatch options: %+v", options)
	}
}

func TestResolveRunTargetCarriesManagedRayMetricsAndOutput(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.engine = "rayjob"
	o.script = "train.py"
	o.dataPVC = "research-workspace"
	o.output = "/data/research-workspace/runs/modernbert-ray"
	o.outputPublish = "staged"
	o.workspace = "research-workspace"
	o.metricsOffloadEnabled = true
	o.metricsSessionID = "session-ray"
	o.metricsHistory = []string{"metrics-history-attempt-*/*.jsonl"}

	target, err := resolveRunTarget(o, "modernbert-ray")
	if err != nil {
		t.Fatal(err)
	}
	rayjob := resolvedRayJobRequestForTest(target)
	if rayjob == nil {
		t.Fatalf("expected typed RayJob dispatch, got %+v", target)
	}
	opts := rayjob.Options
	if opts.dataPVC != "research-workspace" {
		t.Fatalf("dataPVC = %q", opts.dataPVC)
	}
	if opts.output != "/data/research-workspace/runs/modernbert-ray" {
		t.Fatalf("output = %q", opts.output)
	}
	if opts.outputPublish != "staged" {
		t.Fatalf("outputPublish = %q", opts.outputPublish)
	}
	if strings.TrimSpace(opts.artifactPublicationID) == "" {
		t.Fatal("artifactPublicationID not assigned")
	}
	if !opts.metricsOffloadEnabled || opts.metricsSessionID != "session-ray" {
		t.Fatalf("metrics offload = %v/%q", opts.metricsOffloadEnabled, opts.metricsSessionID)
	}
	if len(opts.metricsHistory) != 1 || opts.metricsHistory[0] != "metrics-history-attempt-*/*.jsonl" {
		t.Fatalf("metricsHistory = %v", opts.metricsHistory)
	}
}

func TestResolveRunTargetRejectsUnsupportedStagedPublicationDispatch(t *testing.T) {
	tests := []runDispatchOptions{
		{
			runDispatchInput: runDispatchInput{file: "managed.yaml"},
			runStorageInput:  runStorageInput{runDirectStorage: runDirectStorage{outputPublish: "staged"}},
		},
	}
	for _, options := range tests {
		if _, err := resolveRunTarget(options, "unsupported"); err == nil || !strings.Contains(err.Error(), "storage.publish is not supported") {
			t.Fatalf("dispatch %+v error = %v", options, err)
		}
	}
}

func TestResolveRunTargetRejectsUnsafeMetricsOffloadDispatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runDispatchOptions)
		want   string
	}{
		{
			name: "multi node",
			mutate: func(o *runDispatchOptions) {
				o.engine = "job"
				o.nodes = 2
				o.launcher = "torchrun"
			},
			want: "requires a single Job pod",
		},
		{
			name: "image entrypoint",
			mutate: func(o *runDispatchOptions) {
				o.engine = "job"
				o.script = ""
			},
			want: "requires run.entrypoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := defaultRunDispatchOptions()
			o.engine = "job"
			o.script = "train.sh"
			o.metricsOffloadEnabled = true
			tt.mutate(&o)
			_, err := resolveRunTarget(o, "tracked")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
		})
	}
}

func TestRunConfigRejectsCrossEngineLauncher(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "ray+torchrun",
			config: `name: bad-combo
engine: rayjob
entrypoint: train.py
execution:
  launcher: torchrun
`,
			wantErr: "torchrun is for engine: job",
		},
		{
			name: "ray+python",
			config: `name: bad-combo
engine: rayjob
entrypoint: train.py
execution:
  launcher: python
`,
			wantErr: "python is for engine: job",
		},
		{
			name: "job+ray-train",
			config: `name: bad-combo
engine: job
entrypoint: train.py
execution:
  launcher: ray-train
`,
			wantErr: "requires engine: rayjob",
		},
		{
			name: "job+ray-tune",
			config: `name: bad-combo
engine: job
entrypoint: train.py
execution:
  launcher: ray-tune
`,
			wantErr: "requires engine: rayjob",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := executeTauConfigError(t, tt.config)
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestRunConfigAcceptsRayTrainLauncher(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: ray-train-explicit
engine: rayjob
entrypoint: train.py
runtime:
  image: example.com/research/ray:cuda13
policy:
  namespace: ray
  queue: team-a
execution:
  launcher: ray-train
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	if !strings.Contains(rendered, "kind: RayJob") {
		t.Fatalf("expected RayJob output, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "name: ray-train-explicit") {
		t.Fatalf("expected name ray-train-explicit, got:\n%s", rendered)
	}
}

func TestRunConfigDryRunStagesDigestPinnedImageAsset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "generate.py"), []byte("print('generate')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: external-batch-job
engine: job
entrypoint: generate.py
runtime:
  image: example.azurecr.io/workload:latest
compute:
  gpus: 0
policy:
  namespace: tau-default
  queue: research-training
storage:
  image_assets:
    - name: pinned-reference-assets
      image: example.azurecr.io/reference-assets@sha256:`+digest+`
      source_path: /opt/source-assets
      mount_path: /opt/reference
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: Job",
		"suspend: true",
		"kueue.x-k8s.io/queue-name: research-training",
		"name: tau-asset-pinned-reference-assets",
		"example.azurecr.io/reference-assets@sha256:" + digest,
		"- /opt/source-assets/.",
		"mountPath: /opt/reference",
		"readOnly: true",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("image asset dry-run missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "kind: ConfigMap") {
		t.Fatalf("image asset dry-run must remain self-contained:\n%s", rendered)
	}
}

func TestRunConfigDryRunSetsDirectJobContainerWorkingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: container-cwd
engine: job
entrypoint: train.py
runtime:
  image: example.azurecr.io/workload:latest
  working_dir: /workspace/slime
compute:
  gpus: 0
policy:
  namespace: tau-default
  queue: research-training
storage:
  data_pvc: training-data
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	if !strings.Contains(rendered, "workingDir: /workspace/slime") {
		t.Fatalf("direct Job working directory was not rendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "runtimeEnvYAML") || strings.Contains(rendered, "_tau_project.zip") {
		t.Fatalf("container working directory must not use Ray project shipping:\n%s", rendered)
	}
}

func TestRunConfigDryRunStagesDigestPinnedSourceWithoutLocalEntrypoint(t *testing.T) {
	dir := t.TempDir()
	digest := strings.Repeat("d", 64)
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: source-backed-job
engine: job
entrypoint: experiments/train.py
run:
  ttl_seconds_after_finished: 3600
  source:
    image: example.azurecr.io/research-source@sha256:`+digest+`
    path: /workspace
runtime:
  image: example.azurecr.io/workload:latest
compute:
  gpus: 0
policy:
  namespace: tau-default
  queue: research-training
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: Job",
		"ttlSecondsAfterFinished: 3600",
		"name: tau-source",
		"example.azurecr.io/research-source@sha256:" + digest,
		"- /workspace",
		`chmod a+x "/tau-source/$2"`,
		"mountPath: /tau/source",
		"workingDir: /tau/source",
		"exec python3 /tau/source/experiments/train.py",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("source dry-run missing %q:\n%s", want, rendered)
		}
	}
	for _, forbidden := range []string{"TAU_SCRIPT_B64", "TAU_PAYLOAD_B64", "kind: ConfigMap"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("source dry-run contains forbidden transport %q:\n%s", forbidden, rendered)
		}
	}
}

func TestRunConfigAcceptsRayTuneLauncher(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tune.py")
	if err := os.WriteFile(script, []byte("print('tune')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "tau.yaml")
	if err := os.WriteFile(config, []byte(`name: ray-tune-explicit
engine: rayjob
entrypoint: tune.py
runtime:
  image: example.com/research/ray:cuda13
policy:
  namespace: ray
  queue: team-a
execution:
  launcher: ray-tune
  metric: val_loss
  mode: min
  num_samples: 5
  max_concurrent_trials: 2
  configs:
    lr: [0.001, 0.003, 0.01]
    batch_size: [32, 64]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered := executeTauConfigDryRun(t, []string{"run", "--config", config, "--dry-run=client"})
	for _, want := range []string{
		"kind: RayJob",
		"name: ray-tune-explicit",
		"_tau_tune_driver.py",
		"TAU_TUNE_METRIC",
		"TAU_TUNE_PARAM_SPACE",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected %q in output, got:\n%s", want, rendered)
		}
	}
}

// TestQuickstartScriptsPassWorkspaceCreateName guards the quickstarts' explicit
// workspace selection.
//
// This asserts on the scripts rather than the command so the guard fails in CI,
// where the scripts are never executed.
//
// NAME is optional at the command (cobra.MaximumNArgs(1), `create [NAME]`), but
// the scripts must still pass it: omitting it would ignore a
// TAU_QUICKSTART_WORKSPACE override and create a workspace other than the one
// the status checks below poll — a mismatch that only surfaces after the
// billable cluster exists.
func TestQuickstartScriptsPassWorkspaceCreateName(t *testing.T) {
	// Fail closed on the command no longer ACCEPTING a NAME at all (e.g.
	// cobra.NoArgs), which would make the scripts below fail at runtime.
	// Whether NAME is required or optional is deliberately not asserted.
	//
	// This reports with Errorf rather than Fatalf: a fatal precondition here
	// silently skips the script assertions that are the point of the test.
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "cli", "workspace_create.go"))
	if err != nil {
		t.Fatalf("read workspace_create.go: %v", err)
	}
	if !strings.Contains(string(src), "cobra.ExactArgs(1)") &&
		!strings.Contains(string(src), "cobra.MaximumNArgs(1)") {
		t.Errorf("workspace_create.go no longer accepts a NAME argument " +
			"(expected cobra.ExactArgs(1) or cobra.MaximumNArgs(1)); " +
			"the quickstart scripts' NAME argument contract has changed")
	}

	for _, script := range []string{
		filepath.Join("..", "..", "..", "examples", "aks-cpu-quickstart", "run.sh"),
		filepath.Join("..", "..", "..", "examples", "aks-gpu-quickstart", "run.sh"),
	} {
		body, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		text := string(body)
		if !strings.Contains(text, "tau workspace create \"$WORKSPACE\"") {
			t.Errorf("%s: `tau workspace create` must pass the configured NAME "+
				"argument (expected `tau workspace create \"$WORKSPACE\"`)", script)
		}
	}
}

// TestQuickstartCleanupGuardsResourceGroupDeletion guards the destructive path:
// run.sh reuses a pre-existing resource group, so cleanup.sh must not delete the
// group unless this quickstart created (and therefore tagged) it.
func TestQuickstartCleanupGuardsResourceGroupDeletion(t *testing.T) {
	for _, tc := range []struct{ dir, owner string }{
		{"aks-cpu-quickstart", "aks-cpu-quickstart"},
		{"aks-gpu-quickstart", "aks-gpu-quickstart"},
	} {
		runSrc, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", tc.dir, "run.sh"))
		if err != nil {
			t.Fatalf("read %s/run.sh: %v", tc.dir, err)
		}
		wantTag := "tau-quickstart-owned=" + tc.owner
		if !strings.Contains(string(runSrc), wantTag) {
			t.Errorf("%s/run.sh: must tag groups it creates with %q so cleanup.sh "+
				"can tell them apart from pre-existing groups", tc.dir, wantTag)
		}

		cleanSrc, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", tc.dir, "cleanup.sh"))
		if err != nil {
			t.Fatalf("read %s/cleanup.sh: %v", tc.dir, err)
		}
		clean := string(cleanSrc)
		if !strings.Contains(clean, "tau-quickstart-owned") {
			t.Errorf("%s/cleanup.sh: must check the ownership tag before deleting "+
				"the resource group", tc.dir)
		}
		// The delete must be inside a conditional, never at top level.
		for _, line := range strings.Split(clean, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "run az group delete") {
				continue
			}
			if line == trimmed {
				t.Errorf("%s/cleanup.sh: `az group delete` is unindented and so "+
					"appears unconditional: %q", tc.dir, line)
			}
		}
	}
}

// TestQuickstartPrivilegedPodsUseFixedDigestPinnedImage enforces AGENTS.md rule 18
// for the GPU quickstart's MIG probe/repair pods, which run privileged as root in
// the node's host namespaces. A user-overridable image there would be arbitrary
// root code execution on the node.
func TestQuickstartPrivilegedPodsUseFixedDigestPinnedImage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "aks-gpu-quickstart", "run.sh")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)

	if !strings.Contains(text, "DIAG_IMAGE=\"mcr.microsoft.com/azurelinux/base/core@sha256:") {
		t.Errorf("%s: privileged host pods must use a fixed, digest-pinned MCR "+
			"image (AGENTS.md rule 18)", path)
	}
	// TAU_QUICKSTART_RAY_IMAGE was the override that made these pods unsafe.
	if strings.Contains(text, "TAU_QUICKSTART_RAY_IMAGE") {
		t.Errorf("%s: the privileged MIG pods must not use a user-overridable image", path)
	}
	// The probe must not request a GPU: in the MIG-enabled state it exists to
	// repair, the device plugin advertises zero GPUs, so the pod would hang
	// Pending and its empty logs would be misread as "MIG disabled".
	if strings.Contains(text, "nvidia.com/gpu\":1") {
		t.Errorf("%s: the MIG probe must not request nvidia.com/gpu", path)
	}
}
