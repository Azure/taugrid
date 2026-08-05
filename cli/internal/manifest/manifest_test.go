package manifest

import (
	"strings"
	"testing"
)

func TestValidateRejectsBadSchemaVersion(t *testing.T) {
	src := `
schema_version: 2
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("expected schema_version error, got: %v", err)
	}
}

func TestValidateRejectsBadGPUCount(t *testing.T) {
	src := `
schema_version: 1
name: x
compute: { gpus: 16 }
runtime:
  pip:
    - torch==2.4.0
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "0..8") {
		t.Fatalf("expected gpus range error, got: %v", err)
	}
}

func TestValidateAllowsModelRegistryMetadata(t *testing.T) {
	src := `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
model:
  name: sample-lora
  base: microsoft/sample
  task: weather
  tags:
    dataset: era5
    owner: kevin
  primary_metric: loss
  metric_direction: lower
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Model.Name != "sample-lora" || m.Model.Tags["dataset"] != "era5" {
		t.Fatalf("model metadata not parsed: %+v", m.Model)
	}
}

func TestValidateRejectsUnsafeModelRegistryMetadata(t *testing.T) {
	src := `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
model:
  name: Sample/LoRA
  metric_direction: sideways
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "model.name") {
		t.Fatalf("expected model.name error, got: %v", err)
	}
}

func TestValidateAllowsCPUOnlyGPUCount(t *testing.T) {
	src := `
schema_version: 1
name: cpu-only
compute:
  gpus: 0
  workers: 4
runtime:
  pip:
    - torch==2.4.0
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse should allow CPU-only gpus=0: %v", err)
	}
	if m.Compute.GPUs != 0 || m.Compute.Workers != 4 {
		t.Fatalf("CPU-only compute parsed incorrectly: %+v", m.Compute)
	}
	if m.Compute.CPUs != 1 || m.Compute.WorkerCPUs != 1 || m.Compute.Memory != "2Gi" || m.Compute.WorkerMemory != "4Gi" {
		t.Fatalf("CPU-only defaults parsed incorrectly: %+v", m.Compute)
	}
}

func TestValidateRejectsCPUOnlyEval(t *testing.T) {
	src := `
schema_version: 1
name: cpu-eval
compute:
  gpus: 0
eval:
  cpu_workers: 1
runtime:
  pip:
    - torch==2.4.0
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "compute.gpus=0") || !strings.Contains(err.Error(), "eval.cpu_workers") {
		t.Fatalf("expected CPU-only eval rejection, got: %v", err)
	}
}

func TestValidateAllowsCheckpointArtifact(t *testing.T) {
	src := `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
artifacts:
  checkpoint: rank0/final.safetensors
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse should allow a relative checkpoint artifact: %v", err)
	}
	if m.Artifacts.Checkpoint != "rank0/final.safetensors" {
		t.Fatalf("checkpoint artifact parsed as %q", m.Artifacts.Checkpoint)
	}
}

func TestValidateAllowsRuntimeEnvAndStorageMounts(t *testing.T) {
	src := `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: hf-token
          key: token
    - name: WANDB_MODE
      value: offline
storage:
  data_pvc: project-training
  mounts:
    - name: dataset
      pvc: captioner-dataset
      mountPath: /datasets/captioner
      readOnly: true
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse should allow runtime.env/storage mounts: %v", err)
	}

	if m.DataPVC() != "project-training" {
		t.Fatalf("DataPVC=%q", m.DataPVC())
	}
	if len(m.RuntimeEnv()) != 2 || len(m.StorageMounts()) != 1 {
		t.Fatalf("runtime/storage not parsed: env=%+v mounts=%+v", m.RuntimeEnv(), m.StorageMounts())
	}
}

func TestValidateAllowsRuntimeRDMA(t *testing.T) {
	src := `
schema_version: 1
name: rdma
compute: { gpus: 8, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
  rdma:
    enabled: true
    resource_name: rdma/rdma_shared_device_a
    count: 1
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse should allow runtime.rdma: %v", err)
	}
	rdma := m.RuntimeRDMA()
	if !rdma.Enabled || rdma.ResourceName != "rdma/rdma_shared_device_a" || rdma.Count != 1 {
		t.Fatalf("RuntimeRDMA()=%+v", rdma)
	}
}

func TestValidateRejectsUnsafeRuntimeRDMA(t *testing.T) {
	cases := []struct {
		name string
		rdma string
		want string
	}{
		{
			name: "resource name has surrounding whitespace",
			rdma: `
    enabled: true
    resource_name: " rdma/rdma_shared_device_a"
`,
			want: "resource_name",
		},
		{
			name: "uppercase resource prefix",
			rdma: `
    enabled: true
    resource_name: RDMA/rdma_shared_device_a
`,
			want: "DNS-1123",
		},
		{
			name: "underscore in resource prefix",
			rdma: `
    enabled: true
    resource_name: rdma_devices/rdma_shared_device_a
`,
			want: "DNS-1123",
		},
		{
			name: "resource name missing prefix",
			rdma: `
    enabled: true
    resource_name: rdma_shared_device_a
`,
			want: "extended resource name",
		},
		{
			name: "resource name trailing punctuation",
			rdma: `
    enabled: true
    resource_name: rdma/rdma_shared_device_a-
`,
			want: "resource name segment",
		},
		{
			name: "reserved resource prefix",
			rdma: `
    enabled: true
    resource_name: kubernetes.io/rdma_shared_device_a
`,
			want: "reserved Kubernetes resource prefix",
		},
		{
			name: "overlong resource name segment",
			rdma: `
    enabled: true
    resource_name: rdma/` + strings.Repeat("a", 64) + `
`,
			want: "resource name segment",
		},
		{
			name: "bad count",
			rdma: `
    enabled: true
    count: 0
`,
			want: "count",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
schema_version: 1
name: rdma-bad
compute: { gpus: 8, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
  rdma:
` + tc.rdma
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q validation error, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateRejectsUnsafeCheckpointArtifact(t *testing.T) {
	for _, checkpoint := range []string{"/abs.safetensors", "../final.safetensors", "rank0/../final.safetensors", "rank0//final.safetensors", "."} {
		t.Run(checkpoint, func(t *testing.T) {
			src := `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
artifacts:
  checkpoint: ` + checkpoint + `
`
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), "artifacts.checkpoint") {
				t.Fatalf("expected artifacts.checkpoint error, got: %v", err)
			}
		})
	}
}

func TestParseAllowsStorageMountUsingRayTmpNameOrPath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "ray tmp name",
			raw: `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: ray-tmp
      pvc: data
      mountPath: /datasets/data
`,
		},
		{
			name: "ray tmp path",
			raw: `
schema_version: 1
name: x
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.raw)); err != nil {
				t.Fatalf("Parse should allow batch-only ray tmp storage mounts, got %v", err)
			}
		})
	}
}

func TestParseAllowsCPUOnlyManifest(t *testing.T) {
	src := `
schema_version: 1
name: cpu-ray
compute:
  gpus: 0
  cpus: 1
  worker_cpus: 2
  workers: 5
  memory: 2Gi
  worker_memory: 4Gi
runtime:
  pip:
    - pyyaml
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse CPU manifest: %v", err)
	}
	if !m.IsCPUOnly() {
		t.Fatal("expected CPU-only manifest")
	}
	if got := m.Compute.Memory; got != "2Gi" {
		t.Fatalf("default head memory = %q, want 2Gi", got)
	}
	if got := m.Compute.WorkerMemory; got != "4Gi" {
		t.Fatalf("default worker memory = %q, want 4Gi", got)
	}
}

func TestValidateNameLength(t *testing.T) {
	long := strings.Repeat("a", 60)
	src := `
schema_version: 1
name: ` + long + `
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected name length error, got: %v", err)
	}
}

func TestResourceNamingDefaultAndOverride(t *testing.T) {
	defaultManifest, err := Parse([]byte(`
schema_version: 1
name: train-smoke
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`))
	if err != nil {
		t.Fatalf("Parse default: %v", err)
	}
	if got, want := defaultManifest.ResourceName(), "tau-train-smoke"; got != want {
		t.Fatalf("default ResourceName() = %q, want %q", got, want)
	}

	overrideManifest, err := Parse([]byte(`
schema_version: 1
name: train-smoke
resource_naming:
  prefix: diffusion
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`))
	if err != nil {
		t.Fatalf("Parse override: %v", err)
	}
	if got, want := overrideManifest.ResourceName(), "diffusion-train-smoke"; got != want {
		t.Fatalf("override ResourceName() = %q, want %q", got, want)
	}
}

func TestResourceNamingRejectsInvalidPrefix(t *testing.T) {
	src := `
schema_version: 1
name: train-smoke
resource_naming:
  prefix: Bad_Prefix
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "resource_naming.prefix") {
		t.Fatalf("expected resource_naming.prefix error, got: %v", err)
	}
}

func TestClaim(t *testing.T) {
	cases := map[int]string{0: "", 1: "full-gpu", 2: "ds-2gpus", 4: "ds-4gpus", 8: "ds-8gpus"}
	for g, want := range cases {
		if got := Claim(g); got != want {
			t.Errorf("Claim(%d) = %q, want %q", g, got, want)
		}
	}
}

func TestValidateRejectsResourceLimitBelowRequest(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: bad-resources
compute:
  gpus: 1
  cpus: 4
  cpu_limit: 2
runtime:
  pip:
    - torch==2.4.0
`)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), "cpu_limit") {
		t.Fatalf("expected cpu limit validation error, got: %v", err)
	}
}

func TestParseWorkersDefaultsToOne(t *testing.T) {
	src := `
schema_version: 1
name: x
compute: { gpus: 2 }
runtime:
  pip:
    - torch==2.4.0
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Compute.Workers != 1 {
		t.Fatalf("workers should default to 1, got %d", m.Compute.Workers)
	}
	if m.IsMultiNode() {
		t.Fatal("workers=1 should not be multi-node")
	}
}

func TestParseWorkersExplicitTwo(t *testing.T) {
	src := `
schema_version: 1
name: x
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Compute.Workers != 2 {
		t.Fatalf("workers: want 2, got %d", m.Compute.Workers)
	}
	if !m.IsMultiNode() {
		t.Fatal("workers=2 should be multi-node")
	}
}

func TestValidateRejectsNegativeWorkers(t *testing.T) {
	src := `
schema_version: 1
name: x
compute:
  gpus: 1
  workers: -1
runtime:
  pip:
    - torch==2.4.0
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "compute.workers") {
		t.Fatalf("expected compute.workers error, got: %v", err)
	}
}

// --- multi-node RayJob render tests ---

func TestManifestIsEval(t *testing.T) {
	// IsEval drives auto-defaulting in the CLI when --workload-kind is omitted.
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{
			name: "no eval fields",
			raw: []byte(`schema_version: 1
name: t
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`),
			want: false,
		},
		{
			name: "cpu_workers set",
			raw: []byte(`schema_version: 1
name: t
eval: { harness: tc_score, cpu_workers: 4 }
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`),
			want: true,
		},
		{
			name: "upstream set",
			raw: []byte(`schema_version: 1
name: t
eval: { harness: tc_score, upstream: prev-train }
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := m.IsEval(); got != tc.want {
				t.Errorf("IsEval = %v, want %v", got, tc.want)
			}
		})
	}
}
