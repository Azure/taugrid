package manifest

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/kvspec"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/payload"
	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestRenderJobScopedSecretRedactsClientDryRun(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: secret-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: tau-secret-job-secrets
          key: HF_TOKEN
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "secret-job.yaml",
		Namespace:        "e2e-stack",
		MainScript:       []byte("# stub SDK wrapper\n"),
		JobSecret: &JobSecret{
			Name:       "tau-secret-job-secrets",
			StringData: map[string]string{"HF_TOKEN": "fake-token-value"},
		},
		RedactSecrets: true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: Secret",
		"name: tau-secret-job-secrets",
		"namespace: e2e-stack",
		`HF_TOKEN: "<redacted>"`,
		"secretKeyRef:",
		`name: "<redacted>"`,
		`key: "<redacted>"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered secret output missing %q:\n%s", want, s)
		}
	}
	for _, leaked := range []string{"fake-token-value", "key: HF_TOKEN"} {
		if strings.Contains(s, leaked) {
			t.Fatalf("client dry-run leaked secret material %q:\n%s", leaked, s)
		}
	}
}

func TestRenderJobScopedSecretOwnerLabelsAndAnnotations(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: owner-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: tau-owner-job-secrets
          key: HF_TOKEN
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "owner-job.yaml",
		Namespace:        "ray",
		MainScript:       []byte("# stub\n"),
		JobSecret: &JobSecret{
			Name:       "tau-owner-job-secrets",
			StringData: map[string]string{"HF_TOKEN": "tok"},
			OwnerName:  "finetune-owner-job",
			OwnerKind:  "Job",
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		workloadmeta.LabelManagedBy + ": tau",
		workloadmeta.AnnotationOwnerName + ": finetune-owner-job",
		workloadmeta.AnnotationOwnerKind + ": Job",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered secret missing %q:\n%s", want, s)
		}
	}
}

func TestRenderJobScopedSecretOwnerAnnotationsOmittedWhenEmpty(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: noowner
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: tau-noowner-secrets
          key: HF_TOKEN
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "noowner.yaml",
		Namespace:        "ray",
		MainScript:       []byte("# stub\n"),
		JobSecret: &JobSecret{
			Name:       "tau-noowner-secrets",
			StringData: map[string]string{"HF_TOKEN": "tok"},
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, workloadmeta.LabelManagedBy+": tau") {
		t.Errorf("managed-by label missing:\n%s", s)
	}
	if strings.Contains(s, workloadmeta.AnnotationOwnerName) {
		t.Errorf("owner-name annotation should be absent when OwnerName is empty:\n%s", s)
	}
}

func TestBuildSchedulingMetadataForcesManagedWorkloadAdmissionMarker(t *testing.T) {
	got, err := buildSchedulingMetadata(RenderOptions{
		ProfileName: "test",
		Labels: map[string]string{
			workloadmeta.LabelManagedBy: "researcher-override",
		},
	})
	if err != nil {
		t.Fatalf("buildSchedulingMetadata: %v", err)
	}
	if got.Labels[workloadmeta.LabelManagedBy] != "tau" {
		t.Fatalf("managed workload marker = %q, want tau", got.Labels[workloadmeta.LabelManagedBy])
	}
	if got.PodLabels[workloadmeta.LabelManagedBy] != "tau" {
		t.Fatalf("pod managed workload marker = %q, want tau", got.PodLabels[workloadmeta.LabelManagedBy])
	}
}

func TestRenderRedactsManifestPayloadSecretRefs(t *testing.T) {
	// Design A (#869 PR2): the redacted manifest copy is no longer a
	// ConfigMap `data:` key a human can read directly off the rendered
	// YAML — it's base64-encoded inside the tau-manifest-payload
	// initContainer's env. Decode it via payload.Decode to assert
	// redaction, and separately confirm the workload's *own* env still
	// carries the real secretKeyRef (redaction only applies to the
	// mounted manifest copy the trainer reads for provenance, never to
	// the actual Kubernetes-native env wiring the pod runs with).
	raw := []byte(`
schema_version: 1
name: secret-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: raw-secret-name
          key: raw-secret-key
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "secret-job.yaml",
		Namespace:        "e2e-stack",
		MainScript:       []byte("# stub SDK wrapper\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	docs := splitDocs(t, out)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc (no JobSecret/SecretProviderClass in this case, and no ConfigMaps anymore), got %d:\n%s", len(docs), string(out))
	}
	workload := unmarshalLast(t, out)
	initContainers := podSpecInitContainers(t, dig(workload, "spec", "template", "spec"))
	manifestIC := containerByName(t, initContainers, manifestPayloadInitContainerName)
	files := decodePayloadFiles(t, manifestIC)
	mountedManifest, ok := files["secret-job.yaml"]
	if !ok {
		t.Fatalf("manifest payload missing embedded file %q; got files: %v", "secret-job.yaml", files)
	}
	mountedManifestStr := string(mountedManifest)
	for _, leaked := range []string{"raw-secret-name", "raw-secret-key"} {
		if strings.Contains(mountedManifestStr, leaked) {
			t.Fatalf("embedded manifest payload leaked secret ref %q:\n%s", leaked, mountedManifestStr)
		}
	}
	for _, want := range []string{"secretKeyRef:", "name: <redacted>", "key: <redacted>"} {
		if !strings.Contains(mountedManifestStr, want) {
			t.Fatalf("embedded manifest payload missing redacted dependency %q:\n%s", want, mountedManifestStr)
		}
	}
	if annDigest, _ := dig(workload, "metadata", "annotations", workloadmeta.AnnotationManifestPayloadDigest).(string); annDigest == "" {
		t.Fatalf("workload missing %s annotation", workloadmeta.AnnotationManifestPayloadDigest)
	}

	workloadDoc := docs[0]
	for _, want := range []string{`name: "raw-secret-name"`, `key: "raw-secret-key"`} {
		if !strings.Contains(workloadDoc, want) {
			t.Fatalf("workload env should keep real secret ref %q:\n%s", want, workloadDoc)
		}
	}
}

func TestRenderRayJobRuntimeSecretEnvAndStorageMount(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: secret-render
compute: { gpus: 1, workers: 2 }
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
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "secret-render.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"name: \"HF_TOKEN\"",
		"secretKeyRef:",
		"name: \"hf-token\"",
		"key: \"token\"",
		"name: \"WANDB_MODE\"",
		"value: \"offline\"",
		"claimName: project-training",
		"mountPath: \"/datasets/captioner\"",
		"claimName: \"captioner-dataset\"",
		"readOnly: true",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, s)
		}
	}
}

func TestRenderRayJobMetricsOffloadSidecar(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: vision-demo
research:
  experiment: demo-experiment
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  data_pvc: taugrid-datasets
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "vision-demo.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
		MetricsOffload: MetricsOffloadOptions{
			Image:               "registry.example.com/taugrid/tau:20260618.1",
			Project:             "vit-enc-vision",
			Tags:                map[string]string{"dataset": "vision", "recipe": "vit-enc"},
			RemoteWriteEndpoint: "http://${NODE_IP}:3100/receive",
			Interval:            5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	workload := unmarshalLast(t, out)
	headContainers := rayJobHeadContainers(t, workload)
	rayHead := containerByName(t, headContainers, "ray-head")
	sidecar := containerByName(t, headContainers, "metrics-offload")
	initContainers := podSpecInitContainers(t, dig(workload, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec"))
	prepare := containerByName(t, initContainers, "prepare-metrics-offload")
	if got := strings.Join(stringSliceValue(t, "metrics sentinel init command", dig(prepare, "command")), " "); !strings.Contains(got, "metrics-completion.json") || !strings.Contains(got, "metrics-done.json") {
		t.Fatalf("metrics sentinel init container does not clear stale files: %s", got)
	}
	if got := dig(prepare, "securityContext", "runAsUser"); got != 65532 {
		t.Fatalf("metrics sentinel init runAsUser=%v want 65532", got)
	}

	assertEnvVar(t, "ray-head", rayHead, "TAU_GROUP", "demo-experiment")
	assertEnvVar(t, "ray-head", rayHead, "TAU_EXPERIMENT", "demo-experiment")
	if got := sidecar["image"]; got != "registry.example.com/taugrid/tau:20260618.1" {
		t.Fatalf("sidecar image = %v", got)
	}
	for name, value := range map[string]string{
		"TAU_EXP_STORE":                             "/data/checkpoints/finetunes/vision-demo/metrics-expstore",
		"TAU_METRICS_HISTORY":                       "/data/checkpoints/finetunes/vision-demo/metrics-history.jsonl",
		"TAU_METRICS_OFFLOAD_RUN":                   "vision-demo",
		"TAU_METRICS_OFFLOAD_PROJECT":               "vit-enc-vision",
		"TAU_METRICS_OFFLOAD_EXPERIMENT":            "demo-experiment",
		"TAU_METRICS_OFFLOAD_GROUP":                 "demo-experiment",
		"TAU_METRICS_OFFLOAD_TAGS":                  "dataset=vision,recipe=vit-enc",
		"TAU_METRICS_OFFLOAD_SOURCE":                "stellar-online",
		"TAU_METRICS_OFFLOAD_OUT":                   "/data/checkpoints/finetunes/vision-demo/metrics-offload",
		"TAU_METRICS_OFFLOAD_COMPLETION_FILE":       "/data/checkpoints/finetunes/vision-demo/metrics-completion.json",
		"TAU_METRICS_OFFLOAD_INTERVAL":              "5s",
		"TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT": "http://${NODE_IP}:3100/receive",
		"TAU_GROUP":                                 "demo-experiment",
	} {
		assertEnvVar(t, "metrics-offload", sidecar, name, value)
	}
	assertStringSlice(t, "metrics-offload command", dig(sidecar, "command"), []string{metricsoffload.SidecarCommand})
	doneFile := "/data/checkpoints/finetunes/vision-demo/metrics-done.json"
	assertStringSlice(t, "metrics-offload args", dig(sidecar, "args"), []string{"experiment", "offload", "metrics", "--watch", "--done-file", doneFile})
	s := string(out)
	for _, want := range []string{
		"TAU_METRICS_COMPLETION='/data/checkpoints/finetunes/vision-demo/metrics-completion.json'",
		"TAU_METRICS_DONE='" + doneFile + "'",
		"TAU_METRICS_DONE_TIMEOUT=120",
		`while [ ! -f "$TAU_METRICS_DONE" ]`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("RayJob entrypoint missing terminal publication gate %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "tau experiment --store") {
		t.Fatalf("metrics offload sidecar should invoke tau directly via argv, not a shell script:\n%s", s)
	}
}

func TestRenderRayJobHeadDriverLogOffloadSidecar(t *testing.T) {
	cases := []struct {
		name string
		kind string
		raw  []byte
	}{
		{
			name: "gpu",
			kind: WorkloadKindRayJob,
			raw: []byte(`
schema_version: 1
name: gpu-driver-logs
compute:
  gpus: 1
  workers: 2
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - torch==2.4.0
`),
		},
		{
			name: "cpu",
			kind: WorkloadKindRayJob,
			raw: []byte(`
schema_version: 1
name: cpu-driver-logs
compute:
  gpus: 0
  workers: 2
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - torch==2.4.0
`),
		},
		{
			name: "eval",
			kind: WorkloadKindRayJobEval,
			raw: []byte(`
schema_version: 1
name: eval-driver-logs
compute:
  gpus: 1
eval:
  cpu_workers: 2
runtime:
  pip:
    - torch==2.4.0
`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: tc.name + ".yaml",
				WorkloadKind:     tc.kind,
				MainScript:       []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			workload := unmarshalLast(t, out)
			headContainers := rayJobHeadContainers(t, workload)
			head := containerByName(t, headContainers, "ray-head")
			sidecar := containerByName(t, headContainers, raylogoffload.SidecarContainerName)
			headAnnotations, ok := dig(workload, "spec", "rayClusterSpec", "headGroupSpec", "template", "metadata", "annotations").(map[string]any)
			if !ok {
				t.Fatalf("head annotations missing from workload: %v", workload)
			}
			if headAnnotations[raylogoffload.AnnotationKey] != raylogoffload.AnnotationValue {
				t.Fatalf("head log offload annotation missing: %v", headAnnotations)
			}
			if strings.Count(string(out), raylogoffload.AnnotationKey) != 1 {
				t.Fatalf("expected exactly one head log offload annotation:\n%s", out)
			}
			if sidecar["image"] != head["image"] {
				t.Fatalf("sidecar image=%v want head image %v", sidecar["image"], head["image"])
			}
			if got := asYAML(t, sidecar["args"]); !strings.Contains(got, "/tmp/ray/session_latest/logs/job-driver-*.log") {
				t.Fatalf("sidecar args missing ray driver log path contract:\n%s", got)
			}
			assertEnvVar(t, "driver-log-offload", sidecar, "TAU_RAY_LOG_COMPLETION_FILE", raylogoffload.CompletionFilePath)
			assertEnvVar(t, "driver-log-offload", sidecar, "TAU_RAY_LOG_DRAIN_SECONDS", raylogoffload.DefaultDrainSeconds)
			entrypoint, _ := dig(workload, "spec", "entrypoint").(string)
			for _, want := range []string{
				"TAU_RAY_LOG_COMPLETION_FILE=",
				"tau_write_driver_log_completion",
				"trap tau_complete_driver_logs EXIT",
			} {
				if !strings.Contains(entrypoint, want) {
					t.Fatalf("entrypoint missing driver completion contract %q:\n%s", want, entrypoint)
				}
			}
			if got := dig(workload, "spec", "ttlSecondsAfterFinished"); got != 15 {
				t.Fatalf("ttlSecondsAfterFinished=%v want 15-second bounded drain window", got)
			}
			if mounts := asYAML(t, sidecar["volumeMounts"]); !strings.Contains(mounts, "mountPath: /tmp/ray") || !strings.Contains(mounts, "readOnly: true") {
				t.Fatalf("sidecar volume mount missing readonly /tmp/ray contract:\n%s", mounts)
			}
			if mounts := asYAML(t, head["volumeMounts"]); !strings.Contains(mounts, "mountPath: /tmp/ray") {
				t.Fatalf("head container missing /tmp/ray mount:\n%s", mounts)
			}
			headInitContainers := podSpecInitContainers(t, dig(workload, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec"))
			prepare := containerByName(t, headInitContainers, raylogoffload.PrepareInitName)
			if got := asYAML(t, prepare["command"]); !strings.Contains(got, "chmod 1777 /tmp/ray") {
				t.Fatalf("prepare init container missing writable /tmp/ray contract:\n%s", got)
			}
			if volumes := asYAML(t, dig(workload, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "volumes")); !strings.Contains(volumes, "name: "+raylogoffload.VolumeName) {
				t.Fatalf("head pod missing shared /tmp/ray volume:\n%s", volumes)
			}
			if workerAnnotations, _ := dig(workload, "spec", "rayClusterSpec", "workerGroupSpecs", 0, "template", "metadata", "annotations").(map[string]any); workerAnnotations != nil {
				if _, ok := workerAnnotations[raylogoffload.AnnotationKey]; ok {
					t.Fatalf("worker should not carry head-only log offload annotation: %v", workerAnnotations)
				}
			}
		})
	}
}

func TestRenderJobDoesNotIncludeRayDriverLogOffload(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: batch-job
compute:
  gpus: 1
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "batch-job.yaml",
		WorkloadKind:     WorkloadKindJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, raylogoffload.AnnotationKey) {
		t.Fatalf("batch Job should not carry Ray driver log offload annotation:\n%s", s)
	}
	if strings.Contains(s, raylogoffload.SidecarContainerName) {
		t.Fatalf("batch Job should not render the Ray driver log offload sidecar:\n%s", s)
	}
	if strings.Contains(s, "mountPath: /tmp/ray") {
		t.Fatalf("batch Job should not mount the shared Ray tmp volume:\n%s", s)
	}
}

func TestRenderRayJobEvalMetricsOffloadSidecar(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: vision-eval
research:
  experiment: demo-experiment
eval:
  cpu_workers: 2
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "vision-workload.yaml",
		WorkloadKind:     WorkloadKindRayJobEval,
		MainScript:       []byte("# eval\n"),
		MetricsOffload: MetricsOffloadOptions{
			Image:   "registry.example.com/tau@sha256:0123456789abcdef",
			Project: "vit-enc-vision",
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	workload := unmarshalLast(t, out)
	headContainers := rayJobHeadContainers(t, workload)
	sidecar := containerByName(t, headContainers, "metrics-offload")
	initContainers := podSpecInitContainers(t, dig(workload, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec"))
	if prepare := containerByName(t, initContainers, "prepare-metrics-offload"); !strings.Contains(strings.Join(stringSliceValue(t, "eval metrics sentinel init command", dig(prepare, "command")), " "), "metrics-done.json") {
		t.Fatalf("eval metrics sentinel init container missing done-file cleanup: %v", prepare)
	}
	assertEnvVar(t, "metrics-offload", sidecar, "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/vision-eval/metrics-history.jsonl")
	assertEnvVar(t, "metrics-offload", sidecar, "TAU_METRICS_OFFLOAD_RUN", "vision-eval")
	assertEnvVar(t, "metrics-offload", sidecar, "TAU_METRICS_OFFLOAD_GROUP", "demo-experiment")
	assertEnvVar(t, "metrics-offload", sidecar, "TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT", "http://${NODE_IP}:3100/receive")
	assertStringSlice(t, "metrics-offload args", dig(sidecar, "args"), []string{
		"experiment", "offload", "metrics", "--watch", "--done-file",
		"/data/checkpoints/finetunes/vision-eval/metrics-done.json",
	})
}

func TestRenderJobAllowsStorageMountUsingRayTmpNameOrPath(t *testing.T) {
	cases := []struct {
		name      string
		raw       []byte
		wantMount string
	}{
		{
			name: "ray tmp name",
			raw: []byte(`
schema_version: 1
name: batch-job-ray-tmp-name
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: ray-tmp
      pvc: data
      mountPath: /datasets/data
`),
			wantMount: `name: "ray-tmp"`,
		},
		{
			name: "ray tmp path",
			raw: []byte(`
schema_version: 1
name: batch-job-ray-tmp-path
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray
`),
			wantMount: `mountPath: "/tmp/ray"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: tc.name + ".yaml",
				WorkloadKind:     WorkloadKindJob,
				MainScript:       []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			s := string(out)
			if !strings.Contains(s, tc.wantMount) {
				t.Fatalf("rendered batch Job should preserve custom storage mount %q:\n%s", tc.wantMount, s)
			}
			if strings.Contains(s, raylogoffload.SidecarContainerName) {
				t.Fatalf("batch Job should not render the Ray driver log offload sidecar:\n%s", s)
			}
		})
	}
}

func TestRenderRayKindsRejectRayTmpStorageCollisions(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		kind string
		want string
		main []byte
	}{
		{
			name: "gpu rayjob reserved name",
			raw: []byte(`
schema_version: 1
name: gpu-rayjob
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: ray-tmp
      pvc: data
      mountPath: /datasets/data
`),
			kind: WorkloadKindRayJob,
			want: `"ray-tmp" is reserved by Tau for Ray driver log offload on RayJob workloads`,
			main: []byte("# trainer\n"),
		},
		{
			name: "gpu rayjob descendant path",
			raw: []byte(`
schema_version: 1
name: gpu-rayjob-descendant
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray/session_latest/logs
`),
			kind: WorkloadKindRayJob,
			want: `"/tmp/ray/session_latest/logs" is reserved by Tau for Ray driver log offload on RayJob workloads`,
			main: []byte("# trainer\n"),
		},
		{
			name: "cpu rayjob exact path",
			raw: []byte(`
schema_version: 1
name: cpu-rayjob
compute:
  gpus: 0
  workers: 2
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - pyyaml
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray
`),
			kind: WorkloadKindRayJob,
			want: `"/tmp/ray" is reserved by Tau for Ray driver log offload on CPU RayJob workloads`,
			main: []byte("# trainer\n"),
		},
		{
			name: "cpu rayjob descendant path",
			raw: []byte(`
schema_version: 1
name: cpu-rayjob-descendant
compute:
  gpus: 0
  workers: 2
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - pyyaml
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray/worker-artifacts
`),
			kind: WorkloadKindRayJob,
			want: `"/tmp/ray/worker-artifacts" is reserved by Tau for Ray driver log offload on CPU RayJob workloads`,
			main: []byte("# trainer\n"),
		},
		{
			name: "rayjob eval exact path",
			raw: []byte(`
schema_version: 1
name: eval-rayjob
compute: { gpus: 1 }
eval:
  cpu_workers: 2
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray
`),
			kind: WorkloadKindRayJobEval,
			want: `"/tmp/ray" is reserved by Tau for Ray driver log offload on RayJobEval workloads`,
			main: []byte("# eval\n"),
		},
		{
			name: "rayjob eval descendant path",
			raw: []byte(`
schema_version: 1
name: eval-rayjob-descendant
compute: { gpus: 1 }
eval:
  cpu_workers: 2
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray/results
`),
			kind: WorkloadKindRayJobEval,
			want: `"/tmp/ray/results" is reserved by Tau for Ray driver log offload on RayJobEval workloads`,
			main: []byte("# eval\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: tc.name + ".yaml",
				WorkloadKind:     tc.kind,
				MainScript:       tc.main,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Render should reject ray tmp collision for %s, got %v", tc.name, err)
			}
		})
	}
}

func TestRenderRayKindsRejectNormalizedRayTmpStorageCollision(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: normalized-rayjob
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray/session_latest/../session_123
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "normalized-rayjob.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err == nil || !strings.Contains(err.Error(), `"/tmp/ray/session_latest/../session_123" is reserved by Tau for Ray driver log offload on RayJob workloads`) {
		t.Fatalf("Render should reject normalized ray tmp collision, got %v", err)
	}
}

func TestRenderRayKindsAllowRayTmpSiblingMount(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rayjob-sibling
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: sibling
      pvc: data
      mountPath: /tmp/ray-cache
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rayjob-sibling.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(out), `mountPath: "/tmp/ray-cache"`) {
		t.Fatalf("rendered RayJob should preserve sibling mount:\n%s", out)
	}
}

func TestRenderJobAllowsRayTmpDescendantStorageMount(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: batch-job-ray-tmp-descendant
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
storage:
  mounts:
    - name: dataset
      pvc: data
      mountPath: /tmp/ray/session_latest/logs
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "batch-job-ray-tmp-descendant.yaml",
		WorkloadKind:     WorkloadKindJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `mountPath: "/tmp/ray/session_latest/logs"`) {
		t.Fatalf("rendered batch Job should preserve descendant storage mount:\n%s", s)
	}
	if strings.Contains(s, raylogoffload.SidecarContainerName) {
		t.Fatalf("batch Job should not render the Ray driver log offload sidecar:\n%s", s)
	}
}

func TestRenderMetricsOffloadAllowsKnownShelllessTauImage(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: shellless-sidecar
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "shellless-sidecar.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
		MetricsOffload: MetricsOffloadOptions{
			Image:   "registry.example.com/taugrid/tau:701039b",
			Project: "vit-enc-vision",
		},
	})
	if err != nil {
		t.Fatalf("Render with shell-less Tau image: %v", err)
	}
	workload := unmarshalLast(t, out)
	sidecar := containerByName(t, rayJobHeadContainers(t, workload), "metrics-offload")
	assertStringSlice(t, "metrics-offload command", dig(sidecar, "command"), []string{metricsoffload.SidecarCommand})
	assertStringSlice(t, "metrics-offload args", dig(sidecar, "args"), []string{
		"experiment", "offload", "metrics", "--watch", "--done-file",
		"/data/checkpoints/finetunes/shellless-sidecar/metrics-done.json",
	})
	assertEnvVar(t, "metrics-offload", sidecar, "TAU_EXP_STORE", "/data/checkpoints/finetunes/shellless-sidecar/metrics-expstore")
	assertEnvVar(t, "metrics-offload", sidecar, "TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT", "http://${NODE_IP}:3100/receive")
}

func TestRenderMetricsOffloadRejectsUnsafeImages(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: bad-sidecar
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		image string
		want  string
	}{
		{"registry.example.com/tau:latest", "latest"},
		{"registry.example.com/tau", "explicit non-latest tag"},
		{"localhost:5000/tau", "explicit non-latest tag"},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			_, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      raw,
				ManifestFilename: "bad-sidecar.yaml",
				WorkloadKind:     WorkloadKindRayJob,
				MainScript:       []byte("# trainer\n"),
				MetricsOffload:   MetricsOffloadOptions{Image: tc.image},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q rejection for %q, got %v", tc.want, tc.image, err)
			}
		})
	}
}

func TestMetricsOffloadOptionsFromProfile(t *testing.T) {
	p := profile.Profile{
		Name: "demo-profile",
		Spec: map[string]any{
			"metrics": map[string]any{
				"offload": map[string]any{
					"image":               "registry.example.com/tau:v1",
					"project":             "vit-enc-vision",
					"group":               "demo-experiment",
					"remoteWriteEndpoint": "http://${NODE_IP}:3100/receive",
					"interval":            "15s",
				},
			},
		},
	}
	opts, err := MetricsOffloadOptionsFromProfile(p)
	if err != nil {
		t.Fatalf("MetricsOffloadOptionsFromProfile: %v", err)
	}
	if opts.Image != "registry.example.com/tau:v1" ||
		opts.Project != "vit-enc-vision" ||
		opts.Group != "demo-experiment" ||
		opts.RemoteWriteEndpoint != "http://${NODE_IP}:3100/receive" ||
		opts.Interval != 15*time.Second {
		t.Fatalf("profile metrics offload parsed incorrectly: %+v", opts)
	}
}

func TestRenderRayJobRejectsKubeRayNameTooLong(t *testing.T) {
	long := strings.Repeat("a", 44)
	src := `
schema_version: 1
name: ` + long + `
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse should allow Kubernetes-valid names before workload kind is known: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      []byte(src),
		ManifestFilename: "too-long.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err == nil {
		t.Fatal("expected RayJob render to reject names over KubeRay's 47 character limit")
	}
	for _, want := range []string{"KubeRay", "47", "tau-" + long, "manifest name must be at most 43 chars"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("RayJob name error missing %q: %v", want, err)
		}
	}
}

func TestRenderProducesWorkloadDoc(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rendertest
compute: { gpus: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rendertest.yaml",
		Namespace:        "ray",
		SmokePairs:       6,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	// No JobSecret/SecretProviderClass in this case, and no ConfigMaps
	// anymore (Design A embeds script+manifest as self-contained
	// payloads directly in the Job) — a single doc, no `---` separators.
	if c := strings.Count(s, "\n---\n"); c != 0 {
		t.Errorf("expected 0 doc separators (single self-contained workload doc), got %d", c)
	}
	// Substitutions applied
	for _, want := range []string{
		scriptPayloadInitContainerName,
		manifestPayloadInitContainerName,
		"tau-rendertest\n",                         // Job name
		"kueue.x-k8s.io/queue-name: jobqueue",      // Kueue admission path
		"suspend: true",                            // Kueue flips false on admission
		"backoffLimit: 0",                          // deterministic trainer failures should not retry
		`nvidia.com/gpu: "2"`,                      // default GPU mode uses device-plugin resources
		"--smoke-pairs 6",                          // smoke pairs threaded through
		"--manifest /manifest/rendertest.yaml",     // manifest path
		"torchrun --standalone --nproc_per_node=2", // multi-gpu launch
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
	// The script payload must actually embed train.py (the trainer
	// entrypoint every workload template invokes as /script/train.py).
	workload := unmarshalLast(t, out)
	initContainers := podSpecInitContainers(t, dig(workload, "spec", "template", "spec"))
	scriptIC := containerByName(t, initContainers, scriptPayloadInitContainerName)
	files := decodePayloadFiles(t, scriptIC)
	if _, ok := files["train.py"]; !ok {
		t.Errorf("script payload missing train.py; got files: %v", files)
	}
}

func TestRenderShellQuotesRuntimePipPackages(t *testing.T) {
	baseRaw := []byte(`
schema_version: 1
name: quote-pip
compute: { gpus: 1 }
runtime:
  pip:
    - numpy>=2.0,<3
    - skill-wm @ git+https://github.com/chokevin/skill-wm.git@main
    - weird's-pkg==1.0
`)
	evalRaw := []byte(`
schema_version: 1
name: quote-pip
eval:
  cpu_workers: 2
compute: { gpus: 1 }
runtime:
  pip:
    - numpy>=2.0,<3
    - skill-wm @ git+https://github.com/chokevin/skill-wm.git@main
    - weird's-pkg==1.0
`)
	cases := []struct {
		kind string
		raw  []byte
	}{
		{WorkloadKindJob, baseRaw},
		{WorkloadKindRayJob, baseRaw},
		{WorkloadKindRayJobEval, evalRaw},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: "quote-pip.yaml",
				WorkloadKind:     tc.kind,
				MainScript:       []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			s := string(out)
			want := `pip install --quiet --no-cache-dir 'numpy>=2.0,<3' 'skill-wm @ git+https://github.com/chokevin/skill-wm.git@main' 'weird'"'"'s-pkg==1.0'`
			if !strings.Contains(s, want) {
				t.Fatalf("runtime.pip specs should render as shell-quoted pip args; want %q in:\n%s", want, s)
			}
			if strings.Contains(s, "pip install --quiet --no-cache-dir numpy>=2.0,<3") {
				t.Fatalf("runtime.pip specs rendered unquoted:\n%s", s)
			}
		})
	}
}

func TestRenderPreservesWorkloadKindLabelForRayWorkloads(t *testing.T) {
	cases := []struct {
		kind string
		raw  []byte
	}{
		{
			kind: WorkloadKindRayJob,
			raw: []byte(`
schema_version: 1
name: workload-kind-rayjob
compute: { gpus: 1, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
`),
		},
		{
			kind: WorkloadKindRayJobEval,
			raw: []byte(`
schema_version: 1
name: workload-kind-rayjob-eval
eval:
  cpu_workers: 2
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: tc.kind + ".yaml",
				WorkloadKind:     tc.kind,
				Labels: map[string]string{
					workloadmeta.LabelWorkloadKind: tc.kind,
				},
				MainScript: []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			workload := unmarshalLast(t, out)
			got := dig(workload, "metadata", "labels", workloadmeta.LabelWorkloadKind)
			if got != tc.kind {
				t.Fatalf("metadata.labels[%q] = %v, want %q", workloadmeta.LabelWorkloadKind, got, tc.kind)
			}
		})
	}
}

func TestRenderOmitsMLflowWiring(t *testing.T) {
	jobRaw := []byte(`
schema_version: 1
name: trackingless-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	baseRaw := []byte(`
schema_version: 1
name: trackingless
compute: { gpus: 1, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	evalRaw := []byte(`
schema_version: 1
name: trackingless-eval
eval:
  cpu_workers: 2
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	cpuRaw := []byte(`
schema_version: 1
name: trackingless-cpu
compute: { gpus: 0, workers: 2 }
runtime:
  image: example.com/ray-cpu:stable
  pip:
    - torch==2.4.0
`)
	cases := []struct {
		name string
		raw  []byte
		kind string
	}{
		{name: "job", raw: jobRaw, kind: WorkloadKindJob},
		{name: "rayjob", raw: baseRaw, kind: WorkloadKindRayJob},
		{name: "rayjob-eval", raw: evalRaw, kind: WorkloadKindRayJobEval},
		{name: "rayjob-cpu", raw: cpuRaw, kind: WorkloadKindRayJob},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: tc.name + ".yaml",
				WorkloadKind:     tc.kind,
				MainScript:       []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if strings.Contains(strings.ToLower(string(out)), "mlflow") {
				t.Fatalf("rendered manifest should not contain MLflow wiring:\n%s", string(out))
			}
		})
	}
}

func TestRenderSingleGPUUsesPlainPython(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: solo
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{Manifest: m, ManifestRaw: raw, ManifestFilename: "solo.yaml", MainScript: []byte("# trainer\n")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `nvidia.com/gpu: "1"`) {
		t.Error("single-gpu run should request one device-plugin GPU by default")
	}
	if strings.Contains(s, "resourceClaimTemplateName: full-gpu") {
		t.Error("single-gpu default should not require the DRA full-gpu claim")
	}
	if !strings.Contains(s, `if [ "1" -le "1" ]; then`) {
		t.Errorf("expected GPUS=1 substitution into bash conditional; got snippet:\n%s", s)
	}
}

func TestRenderCPUOnlyJobOmitsDRAClaims(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: cpu-job
compute: { gpus: 0 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{Manifest: m, ManifestRaw: raw, ManifestFilename: "cpu-job.yaml", MainScript: []byte("# trainer\n")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, forbidden := range []string{
		"resourceClaimTemplateName:",
		"resourceClaims:",
		"claims:\n",
		"nvidia.com/gpu",
		"value: gpu",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("CPU-only Job should not render GPU/DRA field %q:\n%s", forbidden, s)
		}
	}
	if !strings.Contains(s, `if [ "0" -le "1" ]; then`) {
		t.Errorf("CPU-only Job should use plain python path; got:\n%s", s)
	}
	job := unmarshalLast(t, out)
	if got := dig(job, "kind"); got != "Job" {
		t.Errorf("kind = %v, want Job", got)
	}
	podSpec := dig(job, "spec", "template", "spec")
	if got := dig(podSpec, "resourceClaims"); got != nil {
		t.Errorf("CPU-only Job pod should omit resourceClaims, got %v", got)
	}
	if got := dig(podSpec, "containers", 0, "resources", "claims"); got != nil {
		t.Errorf("CPU-only Job container should omit resources.claims, got %v", got)
	}
}

func TestRenderCPUOnlyRayJobWorkersOmitDRAClaims(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: cpu-workers
compute:
  gpus: 0
  workers: 4
runtime:
  image: example.com/ray-cpu:stable
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "cpu-workers.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: RayJob",
		`num-gpus: "0"`,
		"workerGroupSpecs:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CPU-only RayJob missing %q:\n%s", want, s)
		}
	}
	for _, forbidden := range []string{
		"resourceClaimTemplateName:",
		"resourceClaims:",
		"claims:\n",
		"nvidia.com/gpu",
		"value: gpu",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("CPU-only RayJob should not render GPU/DRA field %q:\n%s", forbidden, s)
		}
	}
	rj := unmarshalLast(t, out)
	headSpec := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec")
	if got := dig(headSpec, "resourceClaims"); got != nil {
		t.Errorf("CPU-only RayJob head pod should omit resourceClaims, got %v", got)
	}
	if got := dig(headSpec, "containers", 0, "resources", "claims"); got != nil {
		t.Errorf("CPU-only RayJob head container should omit resources.claims, got %v", got)
	}
	workerSpec := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs", 0, "template", "spec")
	if got := dig(workerSpec, "resourceClaims"); got != nil {
		t.Errorf("CPU-only RayJob worker pod should omit resourceClaims, got %v", got)
	}
	if got := dig(workerSpec, "containers", 0, "resources", "claims"); got != nil {
		t.Errorf("CPU-only RayJob worker container should omit resources.claims, got %v", got)
	}
	// Worker pods get a script-only payload initContainer (never manifest):
	// the tau-py SDK wrapper's ray.train.torch.TorchTrainer path re-reads
	// /script/<trainer-filename> from each worker's own local disk whenever
	// ctx.workers > 1, regardless of GPU count, so CPU-only multi-worker
	// jobs need the script embedded on workers too. The manifest stays
	// head-only: it's parsed once, before any worker actor is spawned.
	workerInitContainers := podSpecInitContainers(t, workerSpec)
	if len(workerInitContainers) != 1 {
		t.Fatalf("CPU-only RayJob worker pod should have exactly 1 payload initContainer (script only), got %d: %v", len(workerInitContainers), workerInitContainers)
	}
	containerByName(t, workerInitContainers, scriptPayloadInitContainerName)
	for _, raw := range workerInitContainers {
		if c, ok := raw.(map[string]any); ok && c["name"] == manifestPayloadInitContainerName {
			t.Errorf("CPU-only RayJob worker pod must NOT have the manifest payload initContainer")
		}
	}
	headInitContainers := podSpecInitContainers(t, headSpec)
	containerByName(t, headInitContainers, scriptPayloadInitContainerName)
	containerByName(t, headInitContainers, manifestPayloadInitContainerName)
}

func TestRenderRayJobEvalRequiresGPUHead(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: cpu-eval
eval:
  upstream: gpu-train
compute: { gpus: 0 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), "compute.gpus=0") || !strings.Contains(err.Error(), "eval.upstream") {
		t.Fatalf("expected early CPU-only eval rejection, got manifest=%+v err=%v", m, err)
	}
}

func TestRenderWithTopologyPresetMetadata(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: presettrain
compute: { gpus: 8 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := profile.Profile{Name: "azure-gpu-8x"}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "presettrain.yaml",
		ProfileName:      "azure-gpu-8x",
		TopologyProfile:  &p,
		TopologyOptions: topology.Options{
			Team:      "research",
			Lane:      "training",
			Mode:      "fixed",
			Placement: "single-node-nvlink",
			Shape:     "8xa100-80gb",
			GPUClass:  "a100-80gb",
			QueueName: "research-training",
		},
		Labels: map[string]string{
			workloadmeta.LabelPreset: "azure.research.training.xl",
		},
		Annotations: map[string]string{
			workloadmeta.AnnotationPresetExplain:       "Pins training to one A100 NVLink island.",
			workloadmeta.AnnotationStellarExperimentID: "presettrain:exact",
			workloadmeta.AnnotationWorkspaceID:         "sample",
		},
		MainScript: []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: research-training",
		"kueue.x-k8s.io/podset-required-topology: \"kubernetes.io/hostname\"",
		`nvidia.com/gpu: "8"`,
		"torchrun --standalone --nproc_per_node=8",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered preset output missing %q", want)
		}
	}
	job := unmarshalLast(t, out)
	if got := dig(job, "metadata", "labels", workloadmeta.LabelGPUClass); got != topology.GPUClassA10080GB {
		t.Errorf("Job gpu class label=%v want %s", got, topology.GPUClassA10080GB)
	}
	if got := dig(job, "spec", "template", "spec", "nodeSelector", workloadmeta.NodeLabelGPUClass); got != topology.GPUClassA10080GB {
		t.Errorf("Job gpu class selector=%v want %s", got, topology.GPUClassA10080GB)
	}
	for key, want := range map[string]string{
		workloadmeta.AnnotationStellarExperimentID: "presettrain:exact",
		workloadmeta.AnnotationWorkspaceID:         "sample",
	} {
		if got := dig(job, "spec", "template", "metadata", "annotations", key); got != want {
			t.Errorf("Job pod annotation %s=%v want %s", key, got, want)
		}
	}
}

func TestRenderNamespaceOverride(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: nstest
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "nstest.yaml",
		Namespace:  "my-ns",
		MainScript: []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "namespace: my-ns") {
		t.Error("Job namespace should be overridden")
	}
	if strings.Contains(s, "namespace: tau") {
		t.Error("default namespace should be replaced, not duplicated")
	}
}

func TestRenderExtraScripts(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: extras
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "extras.yaml",
		ExtraScripts: []ExtraScript{
			{Name: "my_loss.py", Data: []byte("def loss():\n    return 1\n")},
			{Name: "storm_kernel.cu", Data: []byte("__global__ void k() {}\n")},
		},
		MainScript: []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Extra scripts are embedded in the self-contained script payload (no
	// ConfigMap `data:` key to grep) — decode it and assert on the bytes.
	workload := unmarshalLast(t, out)
	initContainers := podSpecInitContainers(t, dig(workload, "spec", "template", "spec"))
	scriptIC := containerByName(t, initContainers, scriptPayloadInitContainerName)
	files := decodePayloadFiles(t, scriptIC)
	wantFiles := map[string]string{
		"my_loss.py":      "def loss():\n    return 1\n",
		"storm_kernel.cu": "__global__ void k() {}\n",
	}
	for name, want := range wantFiles {
		got, ok := files[name]
		if !ok {
			t.Errorf("script payload missing extra script %q; got files: %v", name, files)
			continue
		}
		if string(got) != want {
			t.Errorf("script payload file %q = %q, want %q", name, got, want)
		}
	}
}

func TestRenderExtraScriptRejectsBadDestination(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: extras-bad
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "extras-bad.yaml",
		ExtraScripts:     []ExtraScript{{Name: "../my_loss.py", Data: []byte("x")}},
		MainScript:       []byte("# trainer\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "payload file name") {
		t.Fatalf("expected payload file name validation error, got %v", err)
	}
}

func TestRenderExtraScriptRejectsEmbeddedCollision(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: extras-collision
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "extras-collision.yaml",
		ExtraScripts:     []ExtraScript{{Name: "train.py", Data: []byte("print('override')")}},
		MainScript:       []byte("# trainer\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected collision error, got %v", err)
	}
}

func TestRenderRequiresMainScript(t *testing.T) {
	// The embedded fallback trainer is gone. Render() must hard-fail
	// when MainScript is empty so researchers see a clear migration
	// error instead of silently rendering a workload that crashes
	// in-pod with "no such file: /script/train.py".
	raw := []byte(`
schema_version: 1
name: needs-main-script
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "needs-main-script.yaml",
		// MainScript intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected Render to reject empty MainScript")
	}
	if !strings.Contains(err.Error(), "MainScript required") {
		t.Errorf("error should mention 'MainScript required'; got: %v", err)
	}
}

func TestRenderMainScriptDoesNotBypassExtrasCollision(t *testing.T) {
	// MainScript is the explicit override path. Extras still get the
	// collision check so a typo can't accidentally clobber the SDK
	// wrapper or the embedded trainer via --extra-script train.py.
	raw := []byte(`
schema_version: 1
name: belt-and-braces
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "belt.yaml",
		MainScript:       []byte("# wrapper\n"),
		ExtraScripts:     []ExtraScript{{Name: "train.py", Data: []byte("# typo")}},
	})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("extras must still collision-check against train.py even with MainScript present; got %v", err)
	}
}

// --- RayJob path -----------------------------------------------------------

// splitDocs separates a multi-doc YAML on `\n---\n` boundaries.
func splitDocs(t *testing.T, out []byte) []string {
	t.Helper()
	docs := strings.Split(string(out), "\n---\n")
	return docs
}

// unmarshalLast yaml-decodes the final doc (the workload object) into a
// generic map so tests can assert nested field paths.
func unmarshalLast(t *testing.T, out []byte) map[string]any {
	t.Helper()
	docs := splitDocs(t, out)
	if len(docs) < 1 {
		t.Fatalf("expected ≥1 docs, got %d:\n%s", len(docs), string(out))
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(docs[len(docs)-1]), &m); err != nil {
		t.Fatalf("yaml.Unmarshal workload doc: %v\n---\n%s", err, docs[len(docs)-1])
	}
	return m
}

func asYAML(t *testing.T, v any) string {
	t.Helper()
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	return string(data)
}

// dig walks nested map[string]any by string keys; numeric keys index slices.
// Returns nil if any segment is missing or has the wrong type.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch step := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[step]
		case int:
			s, ok := v.([]any)
			if !ok || step < 0 || step >= len(s) {
				return nil
			}
			v = s[step]
		default:
			return nil
		}
	}
	return v
}

func rayJobHeadContainer(t *testing.T, workload map[string]any) map[string]any {
	t.Helper()
	return containerByName(t, rayJobHeadContainers(t, workload), "ray-head")
}

func rayJobHeadContainers(t *testing.T, workload map[string]any) []any {
	t.Helper()
	containers, ok := dig(workload, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "containers").([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("RayJob head containers missing: %v", containers)
	}
	return containers
}

func containerByName(t *testing.T, containers []any, name string) map[string]any {
	t.Helper()
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("container has unexpected type: %T", raw)
		}
		if container["name"] == name {
			return container
		}
	}
	t.Fatalf("container %q not found in %+v", name, containers)
	return nil
}

// initContainerEnvValue extracts the string value of a plain (non-secretRef)
// env entry by name from a container/initContainer's env list, as rendered
// by payloadInitContainerYAML in render.go.
func initContainerEnvValue(t *testing.T, container map[string]any, name string) string {
	t.Helper()
	envList, ok := container["env"].([]any)
	if !ok {
		t.Fatalf("container %v has no env list", container["name"])
	}
	for _, raw := range envList {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == name {
			v, _ := entry["value"].(string)
			return v
		}
	}
	t.Fatalf("env var %q not found in container %v", name, container["name"])
	return ""
}

// decodePayloadFiles decodes the self-contained payload embedded in an
// initContainer rendered by payloadInitContainerYAML (see render.go),
// returning the map of embedded files by name. Used to assert on script/
// manifest payload contents without a ConfigMap to read `data:` from.
func decodePayloadFiles(t *testing.T, container map[string]any) map[string][]byte {
	t.Helper()
	b64 := initContainerEnvValue(t, container, payload.EnvB64)
	digest := initContainerEnvValue(t, container, payload.EnvDigest)
	files, err := payload.Decode(b64, digest)
	if err != nil {
		t.Fatalf("payload.Decode: %v", err)
	}
	return files
}

// podSpecInitContainers digs out a pod spec's initContainers list, failing
// the test with a helpful message if the path or type doesn't match.
func podSpecInitContainers(t *testing.T, podSpec any) []any {
	t.Helper()
	spec, ok := podSpec.(map[string]any)
	if !ok {
		t.Fatalf("pod spec has unexpected type: %T", podSpec)
	}
	initContainers, ok := spec["initContainers"].([]any)
	if !ok {
		t.Fatalf("pod spec missing initContainers: %v", spec)
	}
	return initContainers
}

func rayJobWorkerContainer(t *testing.T, workload map[string]any) map[string]any {
	t.Helper()
	workerGroups, ok := dig(workload, "spec", "rayClusterSpec", "workerGroupSpecs").([]any)
	if !ok || len(workerGroups) != 1 {
		t.Fatalf("RayJob workerGroupSpecs missing: %v", workerGroups)
	}
	workerGroup, ok := workerGroups[0].(map[string]any)
	if !ok {
		t.Fatalf("RayJob workerGroupSpec has unexpected type: %T", workerGroups[0])
	}
	containers, ok := dig(workerGroup, "template", "spec", "containers").([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("RayJob worker containers missing: %v", containers)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("RayJob worker container has unexpected type: %T", containers[0])
	}
	return container
}

func assertRDMAContainer(t *testing.T, label string, container map[string]any, resourceName string, count string) {
	t.Helper()
	securityContext, ok := container["securityContext"].(map[string]any)
	if !ok {
		t.Fatalf("%s: missing RDMA securityContext: %+v", label, container)
	}
	if securityContext["runAsUser"] != 0 || securityContext["runAsGroup"] != 0 {
		t.Fatalf("%s: RDMA securityContext should run as root, got %+v", label, securityContext)
	}
	if allow, ok := securityContext["allowPrivilegeEscalation"].(bool); !ok || allow {
		t.Fatalf("%s: RDMA securityContext should disable privilege escalation, got %+v", label, securityContext)
	}
	seccompProfile, ok := securityContext["seccompProfile"].(map[string]any)
	if !ok || seccompProfile["type"] != "RuntimeDefault" {
		t.Fatalf("%s: RDMA securityContext should use RuntimeDefault seccomp, got %+v", label, securityContext)
	}
	capabilityDrops, ok := dig(securityContext, "capabilities", "drop").([]any)
	if !ok {
		t.Fatalf("%s: missing RDMA capability drop list: %+v", label, securityContext)
	}
	if !stringListContains(capabilityDrops, "ALL") {
		t.Fatalf("%s: RDMA securityContext should drop ALL capabilities, got %+v", label, capabilityDrops)
	}
	capabilities, ok := dig(securityContext, "capabilities", "add").([]any)
	if !ok {
		t.Fatalf("%s: missing RDMA capabilities: %+v", label, securityContext)
	}
	for _, want := range []string{"IPC_LOCK", "SYS_RESOURCE", "DAC_OVERRIDE"} {
		if !stringListContains(capabilities, want) {
			t.Fatalf("%s: missing capability %s in %+v", label, want, capabilities)
		}
	}
	requests, ok := dig(container, "resources", "requests").(map[string]any)
	if !ok {
		t.Fatalf("%s: missing resource requests: %+v", label, container["resources"])
	}
	limits, ok := dig(container, "resources", "limits").(map[string]any)
	if !ok {
		t.Fatalf("%s: missing resource limits: %+v", label, container["resources"])
	}
	if requests[resourceName] != count {
		t.Fatalf("%s: RDMA request %s=%v, want %s", label, resourceName, requests[resourceName], count)
	}
	if limits[resourceName] != count {
		t.Fatalf("%s: RDMA limit %s=%v, want %s", label, resourceName, limits[resourceName], count)
	}
}

func stringListContains(values []any, want string) bool {
	for _, got := range values {
		if got == want {
			return true
		}
	}
	return false
}

func stringSliceValue(t *testing.T, label string, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("%s: got %T, want []any: %+v", label, value, value)
	}
	out := make([]string, 0, len(raw))
	for i, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s[%d]: got %T, want string: %+v", label, i, item, item)
		}
		out = append(out, s)
	}
	return out
}

func assertStringSlice(t *testing.T, label string, value any, want []string) {
	t.Helper()
	got := stringSliceValue(t, label, value)
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

func assertSocketProbe(t *testing.T, label string, container map[string]any, port string) {
	t.Helper()
	for _, probeName := range []string{"readinessProbe", "livenessProbe"} {
		command, ok := dig(container, probeName, "exec", "command").([]any)
		if !ok {
			t.Fatalf("%s: %s exec command missing: %+v", label, probeName, container[probeName])
		}
		got := fmt.Sprint(command)
		for _, want := range []string{"python3", "socket.create_connection", "127.0.0.1", port} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: %s command = %v, want %q", label, probeName, command, want)
			}
		}
	}
}

func assertEnvVar(t *testing.T, label string, container map[string]any, name, value string) {
	t.Helper()
	env, ok := container["env"].([]any)
	if !ok {
		t.Fatalf("%s: env missing: %+v", label, container)
	}
	for _, item := range env {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == name {
			if entry["value"] != value {
				t.Fatalf("%s: env %s=%v, want %s", label, name, entry["value"], value)
			}
			return
		}
	}
	t.Fatalf("%s: env %s missing in %+v", label, name, env)
}

func TestRenderedEntrypointsAreBashSyntaxValid(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		kind    string
		extract func(map[string]any) any
	}{
		{
			name: "job",
			raw: []byte(`
schema_version: 1
name: bash-job
compute: { gpus: 0 }
runtime:
  pip: [torch==2.4.0]
`),
			kind: WorkloadKindJob,
			extract: func(workload map[string]any) any {
				return dig(workload, "spec", "template", "spec", "containers", 0, "args", 0)
			},
		},
		{
			name: "rayjob",
			raw: []byte(`
schema_version: 1
name: bash-rayjob
compute: { gpus: 1 }
runtime:
  pip: [torch==2.4.0]
`),
			kind: WorkloadKindRayJob,
			extract: func(workload map[string]any) any {
				return dig(workload, "spec", "entrypoint")
			},
		},
		{
			name: "rayjob-eval",
			raw: []byte(`
schema_version: 1
name: bash-eval
compute: { gpus: 1 }
eval:
  cpu_workers: 2
runtime:
  pip: [torch==2.4.0]
`),
			kind: WorkloadKindRayJobEval,
			extract: func(workload map[string]any) any {
				return dig(workload, "spec", "entrypoint")
			},
		},
		{
			// GPU==0 selects the distinct managed-workflow-rayjob-cpu.yaml.tmpl
			// asset (see render.go's IsCPUOnly branch) — its entrypoint is
			// hand-written separately from the GPU rayjob template and
			// must be validated on its own.
			name: "rayjob-cpu",
			raw: []byte(`
schema_version: 1
name: bash-cpu
compute: { gpus: 0, workers: 2 }
runtime:
  image: example.com/ray-cpu:stable
  pip: [torch==2.4.0]
`),
			kind: WorkloadKindRayJob,
			extract: func(workload map[string]any) any {
				return dig(workload, "spec", "entrypoint")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      tc.raw,
				ManifestFilename: tc.name + ".yaml",
				WorkloadKind:     tc.kind,
				MainScript:       []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			script, ok := tc.extract(unmarshalLast(t, out)).(string)
			if !ok || script == "" {
				t.Fatalf("missing rendered entrypoint script")
			}
			path := t.TempDir() + "/entrypoint.sh"
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("write script: %v", err)
			}
			if output, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("bash -n: %v\n%s\nscript:\n%s", err, output, script)
			}
		})
	}
}

func TestRenderRayJobProducesWorkloadDoc(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjbasic
compute: { gpus: 2 }
research:
  experiment: demo-experiment
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjbasic.yaml",
		Namespace:        "ray",
		SmokePairs:       4,
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	// No JobSecret/SecretProviderClass in this case, and no ConfigMaps
	// anymore — a single self-contained RayJob doc, no `---` separators.
	if c := strings.Count(s, "\n---\n"); c != 0 {
		t.Errorf("expected 0 doc separators (single self-contained workload doc), got %d", c)
	}
	for _, want := range []string{
		scriptPayloadInitContainerName,
		manifestPayloadInitContainerName,
		"kind: RayJob",
		"apiVersion: ray.io/v1",
		"shutdownAfterJobFinishes: true",
		"workerGroupSpecs: []",
		`nvidia.com/gpu: "2"`,
		"--smoke-pairs 4",
		"--manifest /manifest/rjbasic.yaml",
		"torchrun --standalone --nproc_per_node=2",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered RayJob output missing %q", want)
		}
	}
	// Structural assertions: walk the parsed RayJob and confirm fields land
	// where KubeRay's CRD expects them. Substring tests would let an
	// indentation bug ship; the unmarshal catches that.
	rj := unmarshalLast(t, out)
	if got := dig(rj, "kind"); got != "RayJob" {
		t.Errorf("kind = %v, want RayJob", got)
	}
	if got := dig(rj, "spec", "suspend"); got != true {
		t.Errorf("spec.suspend = %v, want true (Kueue admission gate)", got)
	}
	if got := dig(rj, "spec", "shutdownAfterJobFinishes"); got != true {
		t.Errorf("spec.shutdownAfterJobFinishes = %v, want true", got)
	}
	// Until this TTL elapses a completed RayJob keeps its pods Running and holding
	// node capacity. It was 86400 (24h), so a job could report SUCCEEDED and still
	// consume the cluster all day; mirrors the eval template's assertion.
	if ttl, ok := dig(rj, "spec", "ttlSecondsAfterFinished").(int); !ok || ttl > 120 {
		t.Errorf("spec.ttlSecondsAfterFinished = %v; a completed training RayJob must not keep holding node capacity this long", dig(rj, "spec", "ttlSecondsAfterFinished"))
	}
	queueLabel := dig(rj, "metadata", "labels", "kueue.x-k8s.io/queue-name")
	if queueLabel == nil {
		t.Errorf("metadata.labels.kueue.x-k8s.io/queue-name missing — Kueue cannot admit")
	}
	headSpec := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec")
	if headSpec == nil {
		t.Fatalf("spec.rayClusterSpec.headGroupSpec.template.spec is missing or wrong type")
	}
	if claims := dig(headSpec, "resourceClaims"); claims != nil {
		t.Errorf("head pod should not use DRA resourceClaims under the device plugin, got %v", claims)
	}
	containers := dig(headSpec, "containers")
	cl, ok := containers.([]any)
	if !ok || len(cl) == 0 {
		t.Fatalf("headGroupSpec containers missing or empty: %T = %v", containers, containers)
	}
	if got := dig(cl[0], "resources", "limits", "nvidia.com/gpu"); got != "2" {
		t.Errorf("head container resources.limits[nvidia.com/gpu] = %v, want 2", got)
	}
	assertSocketProbe(t, "rayjob head", cl[0].(map[string]any), "8265")
	assertEnvVar(t, "rayjob head", cl[0].(map[string]any), "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/rjbasic/metrics-history.jsonl")
	assertEnvVar(t, "rayjob head", cl[0].(map[string]any), "TAU_GROUP", "demo-experiment")
	assertEnvVar(t, "rayjob head", cl[0].(map[string]any), "TAU_EXPERIMENT", "demo-experiment")
	// Entrypoint must be the bash launcher (not e.g. an empty string).
	entry, _ := dig(rj, "spec", "entrypoint").(string)
	if !strings.Contains(entry, "/script/train.py") {
		t.Errorf("spec.entrypoint missing trainer invocation: %q", entry)
	}
	// The head pod's script payload initContainer must actually embed
	// train.py; workers get neither initContainer.
	scriptIC := containerByName(t, podSpecInitContainers(t, headSpec), scriptPayloadInitContainerName)
	files := decodePayloadFiles(t, scriptIC)
	if _, ok := files["train.py"]; !ok {
		t.Errorf("head script payload missing train.py; got files: %v", files)
	}
}

func TestRenderRayJobProfileAddsWorkerEnvAndAnnotations(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjprofile
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
storage:
  data_pvc: taugrid-datasets
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjprofile.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
		Profile: ProfileOptions{
			Mode:     "nsys",
			Rank:     "0,8",
			Warmup:   15 * time.Second,
			Duration: 2 * time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"name: \"TAU_PROFILE_MODE\"",
		"value: \"nsys\"",
		"name: \"TAU_PROFILE_RANK\"",
		"value: \"0,8\"",
		"name: \"TAU_PROFILE_OUT_PATTERN\"",
		"value: \"/data/checkpoints/finetunes/rjprofile/profile/rank-<rank>.nsys-rep\"",
		"name: \"TAU_PROFILE_ACTIVE_SEC\"",
		"value: \"120\"",
		"name: \"TAU_PROFILE_WORLD_SIZE\"",
		"value: \"16\"",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("profiled RayJob output missing %q:\n%s", want, s)
		}
	}
	rj := unmarshalLast(t, out)
	workerEnv := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs").([]any)[0].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"]
	workerEnvYAML, err := yaml.Marshal(workerEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workerEnvYAML), "TAU_PROFILE_ACTIVE_SEC") {
		t.Fatalf("worker env missing profile contract: %v", workerEnv)
	}
}

func TestRenderRayJobProfileRequiresMultipleWorkers(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjprofileone
compute:
  gpus: 8
runtime:
  pip:
    - torch==2.4.0
storage:
  data_pvc: taugrid-datasets
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjprofileone.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
		Profile: ProfileOptions{
			Mode:     "nsys",
			Rank:     "0",
			Duration: time.Minute,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "compute.workers > 1") {
		t.Fatalf("expected multi-worker profile error, got %v", err)
	}
}

func TestRenderCPURayJobProducesCPUOnlyGang(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: cpu-interest
compute:
  gpus: 0
  cpus: 1
  worker_cpus: 2
  workers: 5
  memory: 2Gi
  worker_memory: 4Gi
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - pyyaml
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "cpu-interest.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		TopologyOptions: topology.Options{
			Lane:      "training",
			Placement: "independent",
			Shape:     "cpu-ray-5-pod",
			QueueName: "cpu-training-ray",
		},
		MainScript: []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: cpu-training-ray",
		"image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0",
		`num-cpus: "1"`,
		`num-cpus: "2"`,
		"workerGroupSpecs:",
		"replicas: 4",
		"--manifest /manifest/cpu-interest.yaml",
		`ray.io/overwrite-container-cmd: "true"`,
		`python3 -m pip install --quiet --no-cache-dir "ray[default]==2.40.0"`,
		`exec bash -lc "${KUBERAY_GEN_RAY_START_CMD}"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered CPU RayJob output missing %q", want)
		}
	}
	if strings.Contains(s, "resourceClaims") || strings.Contains(s, "resourceClaimTemplateName") || strings.Contains(s, "num-gpus: \"1\"") {
		t.Fatalf("CPU RayJob should not render GPU DRA or non-zero GPUs:\n%s", s)
	}
	if strings.Contains(s, "8080") {
		t.Fatalf("CPU RayJob workers should not render probes against a port Ray workers do not bind:\n%s", s)
	}
	if strings.Contains(s, "kueue.x-k8s.io/podset-unconstrained-topology") ||
		strings.Contains(s, "kueue.x-k8s.io/podset-required-topology") ||
		strings.Contains(s, "kueue.x-k8s.io/podset-preferred-topology") {
		t.Fatalf("CPU RayJob should not render Kueue TAS annotations; azure-cpu flavor does not support them:\n%s", s)
	}
	rj := unmarshalLast(t, out)
	if got := dig(rj, "kind"); got != "RayJob" {
		t.Errorf("kind = %v, want RayJob", got)
	}
	if got := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "rayStartParams", "num-gpus"); got != "0" {
		t.Errorf("head num-gpus = %v, want 0", got)
	}
	headClaims := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "resourceClaims")
	if headClaims != nil {
		t.Fatalf("CPU RayJob head must not request DRA claims, got %v", headClaims)
	}
	if got := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs", 0, "replicas"); got != 4 {
		t.Errorf("worker replicas = %v, want 4", got)
	}
	workerContainers := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs", 0, "template", "spec", "containers")
	wcl, ok := workerContainers.([]any)
	if !ok || len(wcl) == 0 {
		t.Fatalf("worker containers missing or empty: %T = %v", workerContainers, workerContainers)
	}
	if got := dig(wcl[0], "readinessProbe"); got != nil {
		t.Fatalf("worker readinessProbe = %v, want nil", got)
	}
	if got := dig(wcl[0], "livenessProbe"); got != nil {
		t.Fatalf("worker livenessProbe = %v, want nil", got)
	}
	// CPU-only RayJob workers get a script-only payload initContainer (never
	// manifest): the tau-py SDK wrapper's ray.train.torch.TorchTrainer path
	// re-reads /script/<trainer-filename> from each worker's own local disk
	// whenever ctx.workers > 1, regardless of GPU count. The manifest stays
	// head-only — it's parsed once, before any worker actor is spawned.
	workerSpec := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs", 0, "template", "spec")
	workerInitContainers := podSpecInitContainers(t, workerSpec)
	if len(workerInitContainers) != 1 {
		t.Fatalf("CPU RayJob worker pod should have exactly 1 payload initContainer (script only), got %d: %v", len(workerInitContainers), workerInitContainers)
	}
	containerByName(t, workerInitContainers, scriptPayloadInitContainerName)
	for _, raw := range workerInitContainers {
		if c, ok := raw.(map[string]any); ok && c["name"] == manifestPayloadInitContainerName {
			t.Errorf("CPU RayJob worker pod must NOT have the manifest payload initContainer")
		}
	}
}

func TestRenderCPURayJobUsesDataPVCWhenSet(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: cpu-pvc
compute:
  gpus: 0
  cpus: 1
  workers: 2
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - pyyaml
storage:
  data_pvc: lustre-research
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "cpu-pvc.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if got := strings.Count(s, "persistentVolumeClaim: { claimName: lustre-research }"); got != 2 {
		t.Fatalf("CPU RayJob should mount explicit data PVC on head and worker, got %d occurrences:\n%s", got, s)
	}
	if strings.Contains(s, "name: data\n              emptyDir: {}") || strings.Contains(s, "name: data\n                emptyDir: {}") {
		t.Fatalf("CPU RayJob should not use emptyDir for /data when storage.data_pvc is set:\n%s", s)
	}
}

func TestRenderGPURayJobUsesComputeResourceOverrides(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: gpu-small
compute:
  gpus: 1
  cpus: 4
  memory: 32Gi
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "gpu-small.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `requests: { cpu: "4", memory: "32Gi", nvidia.com/gpu: "1" }`) {
		t.Fatalf("GPU RayJob should use compute.cpus/memory for requests:\n%s", s)
	}
	if !strings.Contains(s, `limits:   { cpu: "4", memory: "32Gi", nvidia.com/gpu: "1" }`) {
		t.Fatalf("GPU RayJob should default limits to explicit requests:\n%s", s)
	}
	if strings.Contains(s, "128Gi") {
		t.Fatalf("GPU RayJob resource override must replace the default limit:\n%s", s)
	}
	if strings.Contains(s, "blob-training") || strings.Contains(s, "persistentVolumeClaim: { claimName:") {
		t.Fatalf("GPU RayJob without storage.data_pvc should not mount an implicit data PVC:\n%s", s)
	}
	if got := strings.Count(s, "emptyDir: {}"); got < 1 {
		t.Fatalf("GPU RayJob without storage.data_pvc should render emptyDir for /data:\n%s", s)
	}
}

func TestRenderGPUJobUsesDefaultResources(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: gpu-default
compute: {gpus: 1}
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "gpu-default.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `requests: { cpu: "8", memory: "64Gi", nvidia.com/gpu: "1" }`) ||
		!strings.Contains(s, `limits:   { cpu: "16", memory: "128Gi", nvidia.com/gpu: "1" }`) {
		t.Fatalf("GPU Job without overrides should use the default resources:\n%s", s)
	}
	if strings.Contains(s, "blob-training") || strings.Contains(s, "persistentVolumeClaim: { claimName:") {
		t.Fatalf("GPU Job without storage.data_pvc should not mount an implicit data PVC:\n%s", s)
	}
	if got := strings.Count(s, "emptyDir: {}"); got < 1 {
		t.Fatalf("GPU Job without storage.data_pvc should render emptyDir for /data:\n%s", s)
	}
}

func TestRenderEvalWorkerResourceOverridesRayNumCPUs(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: eval-workers
compute:
  gpus: 1
  cpus: 4
  memory: 32Gi
  worker_cpus: 3
  worker_cpu_limit: 4
  worker_memory: 12Gi
  worker_memory_limit: 16Gi
eval:
  cpu_workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:           m,
		ManifestRaw:        raw,
		ManifestFilename:   "eval-workers.yaml",
		Namespace:          "ray",
		WorkloadKind:       WorkloadKindRayJobEval,
		MainScript:         []byte("# eval\n"),
		UpstreamCheckpoint: "/data/checkpoints/train/artifacts/last.safetensors",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `num-cpus: "3"`) {
		t.Fatalf("eval CPU worker Ray num-cpus should follow compute.worker_cpus:\n%s", s)
	}
	if !strings.Contains(s, `requests: { cpu: "3", memory: "12Gi" }`) ||
		!strings.Contains(s, `limits:   { cpu: "4", memory: "16Gi" }`) {
		t.Fatalf("eval CPU worker resources should use worker overrides:\n%s", s)
	}
}

func TestRenderCPURayJobRequiresExplicitRuntimeImage(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: cpu-no-image
compute:
  gpus: 0
runtime:
  pip:
    - pyyaml
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse CPU manifest: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "cpu-no-image.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "runtime.image") {
		t.Fatalf("expected missing runtime.image error, got: %v", err)
	}
}

func TestRenderRayJobSingleGPUUsesPlainPython(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjsolo
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "rjsolo.yaml",
		WorkloadKind: WorkloadKindRayJob,
		MainScript:   []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `nvidia.com/gpu: "1"`) {
		t.Error("single-gpu RayJob should request one device-plugin GPU by default")
	}
	if strings.Contains(s, "resourceClaimTemplateName: full-gpu") {
		t.Error("single-gpu RayJob default should not require the DRA full-gpu claim")
	}
	if !strings.Contains(s, `if [ "1" -le "1" ]; then`) {
		t.Errorf("expected GPUS=1 substitution into bash conditional in RayJob entrypoint; got snippet:\n%s", s)
	}
	rj := unmarshalLast(t, out)
	if got, _ := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "rayStartParams", "num-gpus").(string); got != "1" {
		t.Errorf("headGroupSpec.rayStartParams.num-gpus = %q, want \"1\"", got)
	}
}

func TestRenderRayJobWithTopologyPresetMetadata(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjpreset
compute: { gpus: 8 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := profile.Profile{Name: "azure-gpu-8x"}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjpreset.yaml",
		ProfileName:      "azure-gpu-8x",
		TopologyProfile:  &p,
		TopologyOptions: topology.Options{
			Team:      "research",
			Lane:      "training",
			Mode:      "fixed",
			Placement: "single-node-nvlink",
			Shape:     "8xa100-80gb",
			GPUClass:  "a100-80gb",
			QueueName: "research-training",
		},
		Labels: map[string]string{
			workloadmeta.LabelPreset: "azure.research.training.xl",
		},
		Annotations: map[string]string{
			workloadmeta.AnnotationPresetExplain:       "Pins training to one A100 NVLink island.",
			workloadmeta.AnnotationStellarExperimentID: "rjpreset:exact",
			workloadmeta.AnnotationWorkspaceID:         "sample",
		},
		WorkloadKind: WorkloadKindRayJob,
		MainScript:   []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: research-training",
		`nvidia.com/gpu: "8"`,
		"torchrun --standalone --nproc_per_node=8",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered RayJob preset output missing %q", want)
		}
	}
	rj := unmarshalLast(t, out)
	if got := dig(rj, "metadata", "labels", workloadmeta.LabelGPUClass); got != topology.GPUClassA10080GB {
		t.Errorf("RayJob gpu class label=%v want %s", got, topology.GPUClassA10080GB)
	}
	if got := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "nodeSelector", workloadmeta.NodeLabelGPUClass); got != topology.GPUClassA10080GB {
		t.Errorf("Ray head gpu class selector=%v want %s", got, topology.GPUClassA10080GB)
	}
	// Topology placement is single-node-nvlink → kueue podset-required
	// annotation must reach the head pod template metadata (PodSet annotation
	// for Kueue's TAS).
	tas := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "metadata", "annotations", "kueue.x-k8s.io/podset-required-topology")
	if tas != "kubernetes.io/hostname" {
		t.Errorf("kueue podset-required-topology annotation missing on head pod template: %v", tas)
	}
	for key, want := range map[string]string{
		workloadmeta.AnnotationStellarExperimentID: "rjpreset:exact",
		workloadmeta.AnnotationWorkspaceID:         "sample",
	} {
		if got := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "metadata", "annotations", key); got != want {
			t.Errorf("Ray head pod annotation %s=%v want %s", key, got, want)
		}
	}
}

func TestRenderRayJobPropagatesCorrelationAnnotationsToHeadAndWorkerPods(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjmetadata
compute:
  gpus: 1
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjmetadata.yaml",
		Annotations: map[string]string{
			workloadmeta.AnnotationStellarExperimentID: "rjmetadata:exact",
			workloadmeta.AnnotationWorkspaceID:         "sample",
		},
		WorkloadKind: WorkloadKindRayJob,
		MainScript:   []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rj := unmarshalLast(t, out)
	for key, want := range map[string]string{
		workloadmeta.AnnotationStellarExperimentID: "rjmetadata:exact",
		workloadmeta.AnnotationWorkspaceID:         "sample",
	} {
		if got := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "metadata", "annotations", key); got != want {
			t.Errorf("Ray head pod annotation %s=%v want %s", key, got, want)
		}
		if got := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs", 0, "template", "metadata", "annotations", key); got != want {
			t.Errorf("Ray worker pod annotation %s=%v want %s", key, got, want)
		}
	}
}

func TestRenderRayJobNamespaceOverride(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjns
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "rjns.yaml",
		Namespace:    "my-ns",
		WorkloadKind: WorkloadKindRayJob,
		MainScript:   []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "namespace: my-ns") {
		t.Error("RayJob namespace should be overridden")
	}
	if strings.Contains(s, "namespace: tau\n") {
		t.Error("default namespace should be replaced, not duplicated")
	}
}

func TestRenderUnknownWorkloadKindRejected(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: bogus
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "bogus.yaml",
		WorkloadKind: "kustom",
		MainScript:   []byte("# trainer\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "workload kind") {
		t.Fatalf("expected unknown workload kind error, got %v", err)
	}
}

func TestRenderRayJobImageAndTorchPin(t *testing.T) {
	// The RayJob path deliberately uses the slim TauGrid MCR Ray
	// base (not Ray ML) and pins torch==2.4.0 in the entrypoint so trainer
	// behavior matches the single-pod Job path's canonical first-party MCR
	// image/runtime contract. If either drifts, the two workload kinds will
	// produce different results from the same manifest — guard against that
	// here.
	raw := []byte(`
schema_version: 1
name: rjimg
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "rjimg.yaml",
		WorkloadKind: WorkloadKindRayJob,
		MainScript:   []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "image: "+defaultRayJobImage) {
		t.Errorf("RayJob head image must be the TauGrid MCR Ray image (slim, no ML extras); got:\n%s", s)
	}
	if !strings.Contains(s, `rayVersion: "2.54.0"`) {
		t.Errorf("RayJob rayVersion must match the default MCR Ray image; got:\n%s", s)
	}
	if strings.Contains(s, "rayproject/ray-ml:") {
		t.Errorf("RayJob head image must NOT use ray-ml (its torch pin diverges from Job path); got ray-ml in:\n%s", s)
	}
	if !strings.Contains(s, "pip install --quiet --no-cache-dir 'torch==2.4.0'") {
		t.Errorf("RayJob entrypoint must pip-install torch==2.4.0 to match the Job path's canonical MCR runtime contract; got:\n%s", s)
	}
}

func TestRenderRayJobRuntimeImageOverride(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjimg-override
compute: { gpus: 1, workers: 2 }
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:custom-rdma
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjimg-override.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	override := "mcr.microsoft.com/aks/ai-runtime/ray:custom-rdma"
	rj := unmarshalLast(t, out)
	headContainers, ok := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "containers").([]any)
	if !ok || len(headContainers) == 0 {
		t.Fatalf("RayJob head containers missing: %v", headContainers)
	}
	head := containerByName(t, headContainers, "ray-head")
	if head["image"] != override {
		t.Fatalf("RayJob runtime.image override should apply to head image, got %v", head["image"])
	}
	workerGroups, ok := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs").([]any)
	if !ok || len(workerGroups) != 1 {
		t.Fatalf("RayJob workerGroupSpecs missing: %v", workerGroups)
	}
	workerContainers, ok := dig(workerGroups[0].(map[string]any), "template", "spec", "containers").([]any)
	if !ok || len(workerContainers) != 1 {
		t.Fatalf("RayJob worker containers missing: %v", workerContainers)
	}
	worker, ok := workerContainers[0].(map[string]any)
	if !ok {
		t.Fatalf("RayJob worker container has unexpected type: %T", workerContainers[0])
	}
	if worker["image"] != override {
		t.Fatalf("RayJob runtime.image override should apply to worker image, got %v", worker["image"])
	}
}

func TestRenderRayJobRuntimeRDMA(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rj-rdma
compute: { gpus: 8, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
  rdma:
    enabled: true
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rj-rdma.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rj := unmarshalLast(t, out)
	if image := rayJobHeadContainer(t, rj)["image"]; image != defaultRayJobImage {
		t.Fatalf("RayJob runtime.rdma should keep the canonical MCR Ray image, got %v", image)
	}
	assertRDMAContainer(t, "rayjob head", rayJobHeadContainer(t, rj), "rdma/rdma_shared_device_a", "1")
	assertRDMAContainer(t, "rayjob worker", rayJobWorkerContainer(t, rj), "rdma/rdma_shared_device_a", "1")
	assertSocketProbe(t, "rayjob head", rayJobHeadContainer(t, rj), "8265")
	assertSocketProbe(t, "rayjob worker", rayJobWorkerContainer(t, rj), "52365")
	assertEnvVar(t, "rayjob head", rayJobHeadContainer(t, rj), "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/rj-rdma/metrics-history.jsonl")
	assertEnvVar(t, "rayjob worker", rayJobWorkerContainer(t, rj), "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/rj-rdma/metrics-history.jsonl")
}

func TestRenderRayJobRuntimeRDMADefaultsOff(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rj-no-rdma
compute: { gpus: 8, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rj-no-rdma.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	rj := unmarshalLast(t, out)
	for label, container := range map[string]map[string]any{
		"head":   rayJobHeadContainer(t, rj),
		"worker": rayJobWorkerContainer(t, rj),
	} {
		if _, ok := container["securityContext"]; ok {
			t.Fatalf("%s: RDMA securityContext should be opt-in only: %+v", label, container["securityContext"])
		}
		if request := dig(container, "resources", "requests", "rdma/rdma_shared_device_a"); request != nil {
			t.Fatalf("%s: RDMA resource request should be opt-in only, got %v", label, request)
		}
		if limit := dig(container, "resources", "limits", "rdma/rdma_shared_device_a"); limit != nil {
			t.Fatalf("%s: RDMA resource limit should be opt-in only, got %v", label, limit)
		}
	}
}

func TestRenderJobPathUsesCanonicalMCRImageForTrainerAndPayloads(t *testing.T) {
	// Image policy: the shipped batch Job and both payload init containers
	// must stay on the canonical first-party MCR Ray image.
	raw := []byte(`
schema_version: 1
name: jobimg
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "jobimg.yaml",
		// WorkloadKind unset → defaults to job.
		MainScript: []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	workload := unmarshalLast(t, out)
	podSpec := dig(workload, "spec", "template", "spec")
	containers, ok := dig(workload, "spec", "template", "spec", "containers").([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("Job containers missing: %v", containers)
	}
	trainer := containerByName(t, containers, "trainer")
	if image := trainer["image"]; image != defaultRayJobImage {
		t.Fatalf("Job trainer image = %v, want %q", image, defaultRayJobImage)
	}
	initContainers := podSpecInitContainers(t, podSpec)
	for _, name := range []string{scriptPayloadInitContainerName, manifestPayloadInitContainerName} {
		container := containerByName(t, initContainers, name)
		if image := container["image"]; image != defaultRayJobImage {
			t.Fatalf("%s image = %v, want %q", name, image, defaultRayJobImage)
		}
	}
}

func TestRenderJobPathUnchangedWhenWorkloadKindEmpty(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: defaultkind
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	out, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "defaultkind.yaml",
		// WorkloadKind unset → defaults to job
		MainScript: []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "kind: Job") {
		t.Errorf("default workload kind should produce Job, got:\n%s", s)
	}
	if strings.Contains(s, "kind: RayJob") {
		t.Errorf("default workload kind should not emit RayJob")
	}
}

// --- compute.workers (multi-node) tests ---

func TestRenderRayJobMultiNodeProducesWorkerGroupSpecs(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjmulti
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
    - microsoft-sample
    - peft
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjmulti.yaml",
		Namespace:        "ray",
		WorkloadKind:     WorkloadKindRayJob,
		// MainScript required for multi-node — use a stub.
		MainScript: []byte("# stub SDK wrapper\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: RayJob",
		// workerGroupSpecs is no longer the empty-list literal.
		"groupName: tau-rjmulti-w",
		// Worker pod count = workers - 1 (head is rank 0 trainer).
		"replicas: 1",
		"minReplicas: 1",
		"maxReplicas: 1",
		// Worker rayStartParams.num-gpus must equal per-pod GPU count for
		// Ray's resource view to match the device-plugin allocation.
		`num-gpus: "8"`,
		// Worker pod must request its own nvidia.com/gpu, identical to head.
		`nvidia.com/gpu: "8"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("multi-node RayJob output missing %q", want)
		}
	}
	// torchrun --standalone is single-node only; must NOT appear in the
	// multi-node entrypoint (would deadlock — it doesn't talk to other pods).
	// Check the parsed entrypoint specifically — the string can legitimately
	// appear inside the base64-encoded script payload (it embeds template
	// assets), which is not what this assertion is checking.
	rj := unmarshalLast(t, out)
	entry, _ := dig(rj, "spec", "entrypoint").(string)
	if strings.Contains(entry, "torchrun --standalone") {
		t.Errorf("multi-node spec.entrypoint must not use torchrun --standalone (single-node only); got:\n%s", entry)
	}
	if !strings.Contains(entry, "python3 /script/train.py") {
		t.Errorf("multi-node spec.entrypoint must invoke python3 /script/train.py; got:\n%s", entry)
	}
	// Both pod sets (head + worker) must request and limit the same nvidia.com/gpu count.
	if c := strings.Count(s, `nvidia.com/gpu: "8"`); c != 4 {
		t.Errorf("expected 4 nvidia.com/gpu entries (request+limit for head+worker), got %d", c)
	}

	// Structural assertions on the parsed RayJob.
	wgs := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs")
	wgsList, ok := wgs.([]any)
	if !ok || len(wgsList) != 1 {
		t.Fatalf("workerGroupSpecs must be a 1-element list (1 worker group for 2 pods total), got %T = %v", wgs, wgs)
	}
	wg := wgsList[0]
	if got := dig(wg, "replicas"); got != 1 {
		t.Errorf("workerGroup.replicas = %v, want 1", got)
	}
	if got := dig(wg, "rayStartParams", "num-gpus"); got != "8" {
		t.Errorf("workerGroup.rayStartParams.num-gpus = %v, want \"8\" (must match GPUS for DRA)", got)
	}
	workerSpec := dig(wg, "template", "spec")
	if workerSpec == nil {
		t.Fatalf("workerGroup.template.spec missing")
	}
	if claims := dig(workerSpec, "resourceClaims"); claims != nil {
		t.Errorf("worker pod should not use DRA resourceClaims under the device plugin, got %v", claims)
	}
	workerContainers := dig(workerSpec, "containers")
	wcl, ok := workerContainers.([]any)
	if !ok || len(wcl) == 0 {
		t.Fatalf("worker containers missing or empty: %T", workerContainers)
	}
	if got := dig(wcl[0], "resources", "limits", "nvidia.com/gpu"); got != "8" {
		t.Errorf("worker container resources.limits[nvidia.com/gpu] = %v, want 8 (matches head)", got)
	}
	// GPU multi-node workers pull python deps via spec.runtimeEnvYAML
	// (checked below), but the trainer script itself is re-read from local
	// disk on every worker by the tau-py SDK wrapper's
	// ray.train.torch.TorchTrainer path — so workers must carry the
	// script-only payload initContainer (never the manifest payload, which
	// stays head-only).
	workerInitContainers := podSpecInitContainers(t, workerSpec)
	if len(workerInitContainers) != 1 {
		t.Fatalf("multi-node RayJob worker pod should have exactly 1 payload initContainer (script only), got %d: %v", len(workerInitContainers), workerInitContainers)
	}
	containerByName(t, workerInitContainers, scriptPayloadInitContainerName)
	for _, raw := range workerInitContainers {
		if c, ok := raw.(map[string]any); ok && c["name"] == manifestPayloadInitContainerName {
			t.Errorf("multi-node RayJob worker pod must NOT have the manifest payload initContainer")
		}
	}
	head := rayJobHeadContainer(t, rj)
	worker := rayJobWorkerContainer(t, rj)
	assertSocketProbe(t, "multi-node rayjob head", head, "8265")
	assertSocketProbe(t, "multi-node rayjob worker", worker, "52365")
	assertEnvVar(t, "multi-node rayjob head", head, "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/rjmulti/metrics-history.jsonl")
	assertEnvVar(t, "multi-node rayjob worker", worker, "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/rjmulti/metrics-history.jsonl")

	// runtimeEnvYAML must live at spec.runtimeEnvYAML (RayJob CRD
	// level), NOT at ray.init's runtime_env, so KubeRay propagates the
	// pip deps to ALL Train worker actors at job submission time. Without
	// this, workers come up without torch/reference deps and trainer.fit() fails
	// at first import.
	rtEnv, _ := dig(rj, "spec", "runtimeEnvYAML").(string)
	if rtEnv == "" {
		t.Fatalf("multi-node RayJob must render spec.runtimeEnvYAML so worker pods get torch + reference trainer deps; got empty")
	}
	for _, want := range []string{"torch==2.4.0", "microsoft-sample", "peft"} {
		if !strings.Contains(rtEnv, want) {
			t.Errorf("spec.runtimeEnvYAML missing required pip dep %q; got:\n%s", want, rtEnv)
		}
	}
}

func TestRenderRayJobMultiNodeDevicePluginWorkersRequestGPUs(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: rjmulti-dp
compute:
  gpus: 2
  workers: 3
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "rjmulti-dp.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# stub SDK wrapper\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "resourceClaims") || strings.Contains(s, "resourceClaimTemplateName") || strings.Contains(s, "claims:\n") {
		t.Fatalf("device-plugin multi-node RayJob must not render DRA claims:\n%s", s)
	}
	rj := unmarshalLast(t, out)
	headResources := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "containers", 0, "resources")
	if got := dig(headResources, "requests", "nvidia.com/gpu"); got != "2" {
		t.Errorf("head requests nvidia.com/gpu = %v, want \"2\"", got)
	}
	if got := dig(headResources, "limits", "nvidia.com/gpu"); got != "2" {
		t.Errorf("head limits nvidia.com/gpu = %v, want \"2\"", got)
	}

	wgs, ok := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs").([]any)
	if !ok || len(wgs) != 1 {
		t.Fatalf("workerGroupSpecs must be a 1-element list, got %T = %v", wgs, wgs)
	}
	if got := dig(wgs[0], "replicas"); got != 2 {
		t.Errorf("workerGroup.replicas = %v, want 2", got)
	}
	if got := dig(wgs[0], "rayStartParams", "num-gpus"); got != "2" {
		t.Errorf("workerGroup.rayStartParams.num-gpus = %v, want \"2\"", got)
	}
	workerSpec := dig(wgs[0], "template", "spec")
	if got := dig(workerSpec, "resourceClaims"); got != nil {
		t.Errorf("worker pod resourceClaims = %v, want nil", got)
	}
	workerResources := dig(workerSpec, "containers", 0, "resources")
	if got := dig(workerResources, "requests", "nvidia.com/gpu"); got != "2" {
		t.Errorf("worker requests nvidia.com/gpu = %v, want \"2\"", got)
	}
	if got := dig(workerResources, "limits", "nvidia.com/gpu"); got != "2" {
		t.Errorf("worker limits nvidia.com/gpu = %v, want \"2\"", got)
	}
}

func TestRenderRayJobMultiNodeRequiresMainScript(t *testing.T) {
	// Multi-node requires the tau-py SDK (--main-script). Without it,
	// the embedded Sample trainer would run, which is single-node only
	// and would silently produce wrong results. Render must hard-fail.
	raw := []byte(`
schema_version: 1
name: rjbad
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "rjbad.yaml",
		WorkloadKind: WorkloadKindRayJob,
		// MainScript intentionally omitted.
	})
	if err == nil {
		t.Fatal("expected Render to reject multi-node without MainScript")
	}
	if !strings.Contains(err.Error(), "tau-py SDK") {
		t.Errorf("error should point researcher at the SDK; got: %v", err)
	}
}

func TestRenderJobKindRejectsMultiNode(t *testing.T) {
	// The Job path is single-pod only — k8s batch/v1 can't gang-admit
	// multiple pods. Multi-node + Job must hard-fail with a clear error.
	raw := []byte(`
schema_version: 1
name: jbad
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "jbad.yaml",
		WorkloadKind: WorkloadKindJob,
		MainScript:   []byte("# trainer\n"),
	})
	if err == nil {
		t.Fatal("expected Render to reject multi-node with workload kind=job")
	}
	if !strings.Contains(err.Error(), "rayjob") {
		t.Errorf("error should suggest rayjob; got: %v", err)
	}
}

func TestRenderRayJobEvalProducesGPUHeadAndCPUWorkers(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: eval-smoke
eval:
  cpu_workers: 4
  upstream: train-fullft
compute:
  gpus: 1
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:           m,
		ManifestRaw:        raw,
		ManifestFilename:   "eval-smoke.yaml",
		WorkloadKind:       WorkloadKindRayJobEval,
		MainScript:         []byte("# stub wrapper\n"),
		UpstreamCheckpoint: "/data/checkpoints/train-fullft/last.safetensors",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		scriptPayloadInitContainerName,
		manifestPayloadInitContainerName,
		"name: tau-eval-smoke",
		"ttlSecondsAfterFinished: 15",
		// Head pod pins one GPU via DRA — the actor lives here.
		`num-gpus: "1"`,
		`nvidia.com/gpu: "1"`,
		// CPU worker group pinned to 4 replicas via static gang admission.
		"groupName: tau-eval-smoke-cpu",
		"replicas: 4",
		"minReplicas: 4",
		"maxReplicas: 4",
		`num-cpus: "1"`,
		`num-gpus: "0"`,
		// TAU_UPSTREAM_CHECKPOINT propagated to the head AND the CPU workers.
		// (We can grep both occurrences below; this just checks at least one.)
		`name: TAU_UPSTREAM_CHECKPOINT, value: "/data/checkpoints/train-fullft/last.safetensors"`,
		// runtimeEnvYAML must propagate Sample deps to CPU worker pods
		// — they don't run the head's pip install.
		"runtimeEnvYAML",
		"microsoft-sample",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("eval RayJob output missing %q", want)
		}
	}

	// Both head and worker pods should expose TAU_UPSTREAM_CHECKPOINT
	// as an env var (matched specifically — the var name also appears in
	// template comments). Either pod could read it (e.g. a CPU worker
	// loading per-IC metadata from the checkpoint dir).
	envVarPattern := `name: TAU_UPSTREAM_CHECKPOINT, value:`
	if c := strings.Count(s, envVarPattern); c != 2 {
		t.Errorf("expected TAU_UPSTREAM_CHECKPOINT env on both head and worker (2 occurrences of %q), got %d", envVarPattern, c)
	}

	// CPU workers MUST NOT carry GPU plumbing. The whole point of
	// rayjob-eval is decoupled GPU+CPU — if a CPU worker pod claims a
	// DRA GPU, it'll be unschedulable and stall the entire workload.
	rj := unmarshalLast(t, out)
	wgs := dig(rj, "spec", "rayClusterSpec", "workerGroupSpecs")
	wgsList, ok := wgs.([]any)
	if !ok || len(wgsList) != 1 {
		t.Fatalf("workerGroupSpecs must be a single CPU group, got %T = %v", wgs, wgs)
	}
	wg := wgsList[0]
	workerSpec := dig(wg, "template", "spec")
	if workerSpec == nil {
		t.Fatalf("workerGroup.template.spec missing")
	}
	if claims := dig(workerSpec, "resourceClaims"); claims != nil {
		t.Errorf("CPU worker pod must NOT have resourceClaims (no GPU); got: %v", claims)
	}
	// CPU workers run plain ray.remote score tasks against the head's
	// actor; Ray's own runtime_env working_dir mechanism ships them the
	// user module but never any --extra-script file, so they get a
	// script-only payload initContainer (never manifest) — the same
	// mechanism the two Ray Train templates' workers use. /manifest stays
	// head-only: it's parsed once, before any worker actor is spawned.
	workerInitContainers := podSpecInitContainers(t, workerSpec)
	if len(workerInitContainers) != 1 {
		t.Fatalf("eval CPU worker pod should have exactly 1 payload initContainer (script only), got %d: %v", len(workerInitContainers), workerInitContainers)
	}
	containerByName(t, workerInitContainers, scriptPayloadInitContainerName)
	for _, raw := range workerInitContainers {
		if c, ok := raw.(map[string]any); ok && c["name"] == manifestPayloadInitContainerName {
			t.Errorf("eval CPU worker pod must NOT have the manifest payload initContainer")
		}
	}
	headSpec := dig(rj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec")
	headInitContainers := podSpecInitContainers(t, headSpec)
	scriptIC := containerByName(t, headInitContainers, scriptPayloadInitContainerName)
	if _, ok := decodePayloadFiles(t, scriptIC)["train.py"]; !ok {
		t.Errorf("head script payload missing train.py")
	}
	containerByName(t, headInitContainers, manifestPayloadInitContainerName)
	head := rayJobHeadContainer(t, rj)
	worker := rayJobWorkerContainer(t, rj)
	assertSocketProbe(t, "eval rayjob head", head, "8265")
	assertSocketProbe(t, "eval rayjob worker", worker, "52365")
	assertEnvVar(t, "eval rayjob head", head, "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/eval-smoke/metrics-history.jsonl")
	assertEnvVar(t, "eval rayjob worker", worker, "TAU_METRICS_HISTORY", "/data/checkpoints/finetunes/eval-smoke/metrics-history.jsonl")
	// Worker container resources must not carry a 'claims' entry pointing at a GPU.
	containers, ok := dig(workerSpec, "containers").([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("expected exactly 1 worker container, got %T = %v", containers, containers)
	}
	if cclaims := dig(containers[0], "resources", "claims"); cclaims != nil {
		t.Errorf("CPU worker container must NOT request a DRA claim (no GPU); got: %v", cclaims)
	}

	// Head pod's spec.entrypoint runs the tau-py wrapper directly (no torchrun
	// — eval is single-pod-driven, not distributed training).
	entry, _ := dig(rj, "spec", "entrypoint").(string)
	if !strings.Contains(entry, "python3 /script/train.py") {
		t.Errorf("eval entrypoint must invoke python3 /script/train.py; got:\n%s", entry)
	}
	if strings.Contains(entry, "torchrun") {
		t.Errorf("eval entrypoint must NOT invoke torchrun (eval isn't distributed training); got:\n%s", entry)
	}
	if got := dig(rj, "spec", "ttlSecondsAfterFinished"); got != 15 {
		t.Errorf("eval RayJob should retain only the bounded final-log drain window; ttlSecondsAfterFinished=%v", got)
	}
}

func TestRenderRayJobEvalRequiresCPUWorkers(t *testing.T) {
	// rayjob-eval without cpu_workers is incoherent — the whole shape
	// is "1 GPU head + N CPU workers". If you don't want CPU workers,
	// just submit as a normal rayjob (which the manifest already
	// supports for single-GPU work).
	raw := []byte(`
schema_version: 1
name: bad-eval
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "bad-workload.yaml",
		WorkloadKind: WorkloadKindRayJobEval,
		MainScript:   []byte("# stub\n"),
	})
	if err == nil {
		t.Fatal("expected Render to reject rayjob-eval without cpu_workers")
	}
	if !strings.Contains(err.Error(), "cpu_workers") {
		t.Errorf("error should mention cpu_workers; got: %v", err)
	}
}

func TestRenderRayJobEvalRequiresMainScript(t *testing.T) {
	// Like rayjob multi-node, the embedded Sample trainer can't drive an
	// eval — the user fn lives in their @tau.eval handle, only reachable
	// via the tau-py wrapper.
	raw := []byte(`
schema_version: 1
name: bad-eval
eval:
  cpu_workers: 4
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "bad-workload.yaml",
		WorkloadKind: WorkloadKindRayJobEval,
	})
	if err == nil {
		t.Fatal("expected Render to reject rayjob-eval without MainScript")
	}
	if !strings.Contains(err.Error(), "tau-py SDK") {
		t.Errorf("error should point researcher at the SDK; got: %v", err)
	}
}

func TestRenderRayJobEvalRejectsMultiNode(t *testing.T) {
	// Eval is always 1 GPU pod + N CPU pods — multi-node training (>1 GPU
	// pod) is conceptually wrong here. The user is probably trying to do
	// distributed eval, which isn't supported in v1.
	raw := []byte(`
schema_version: 1
name: bad-eval
eval:
  cpu_workers: 4
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	_, err := Render(RenderOptions{
		Manifest: m, ManifestRaw: raw, ManifestFilename: "bad-workload.yaml",
		WorkloadKind: WorkloadKindRayJobEval,
		MainScript:   []byte("# stub\n"),
	})
	if err == nil {
		t.Fatal("expected Render to reject rayjob-eval with workers>1")
	}
}

func TestRenderJobAndRayJobRejectEvalManifest(t *testing.T) {
	// If a researcher's manifest has eval.cpu_workers / eval.upstream set
	// but they pass --workload-kind=rayjob (or omit it, defaulting to job),
	// dispatch is wrong. Hard-fail with a pointer at the right kind.
	raw := []byte(`
schema_version: 1
name: misdispatched
eval:
  cpu_workers: 4
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, _ := Parse(raw)
	for _, kind := range []string{WorkloadKindJob, WorkloadKindRayJob} {
		_, err := Render(RenderOptions{
			Manifest: m, ManifestRaw: raw, ManifestFilename: "misdispatched.yaml",
			WorkloadKind: kind,
			MainScript:   []byte("# stub\n"),
		})
		if err == nil {
			t.Errorf("kind=%s: expected Render to reject eval manifest with non-eval workload kind", kind)
			continue
		}
		if !strings.Contains(err.Error(), "rayjob-eval") {
			t.Errorf("kind=%s: error should suggest rayjob-eval; got: %v", kind, err)
		}
	}
}

func testKVSpec() *kvspec.Spec {
	return &kvspec.Spec{
		Entries: []kvspec.Entry{
			{EnvVar: "HF_TOKEN", VaultName: "my-vault", SecretName: "hf-token"},
			{EnvVar: "WANDB_KEY", VaultName: "my-vault", SecretName: "wandb-api-key"},
		},
		Vault:    "my-vault",
		TenantID: "tenant-abc",
		ClientID: "client-xyz",
	}
}

func TestRenderJobWithKVSpec(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: kv-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env_kv:
    HF_TOKEN: my-vault/hf-token
    WANDB_KEY: my-vault/wandb-api-key
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "kv-job.yaml",
		WorkloadKind:     WorkloadKindJob,
		MainScript:       []byte("# train\n"),
		KVSpec:           testKVSpec(),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		"secrets-store.csi.k8s.io",
		"secretProviderClass: tau-kv-job-kv",
		"/mnt/secrets-store",
		"readOnly: true",
		"azure.workload.identity/use: \"true\"",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Job with KVSpec missing %q", want)
		}
	}

	// SPC document should be emitted
	docs := splitDocs(t, out)
	foundSPC := false
	for _, doc := range docs {
		if strings.Contains(doc, "kind: SecretProviderClass") {
			foundSPC = true
			for _, want := range []string{
				"keyvaultName: my-vault",
				"tenantId: tenant-abc",
				"clientID: client-xyz",
				"objectName: hf-token",
				"objectName: wandb-api-key",
			} {
				if !strings.Contains(doc, want) {
					t.Errorf("SPC doc missing %q", want)
				}
			}
		}
	}
	if !foundSPC {
		t.Error("no SecretProviderClass document in output")
	}

	// secretKeyRef env vars
	for _, want := range []string{
		`name: "HF_TOKEN"`,
		`name: "WANDB_KEY"`,
		"secretKeyRef:",
		"tau-kv-job-kv-sync",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Job KV env missing %q", want)
		}
	}
}

func TestRenderRayJobWithKVSpec(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: kv-ray
compute: { gpus: 1, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
  env_kv:
    HF_TOKEN: my-vault/hf-token
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	spec := &kvspec.Spec{
		Entries: []kvspec.Entry{
			{EnvVar: "HF_TOKEN", VaultName: "my-vault", SecretName: "hf-token"},
		},
		Vault:    "my-vault",
		TenantID: "tenant-abc",
		ClientID: "client-xyz",
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "kv-ray.yaml",
		WorkloadKind:     WorkloadKindRayJob,
		MainScript:       []byte("# train\n"),
		KVSpec:           spec,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)

	// WI label on pod
	if !strings.Contains(s, "azure.workload.identity/use: \"true\"") {
		t.Error("RayJob with KVSpec missing WI pod label")
	}

	// CSI volume and mount on head and worker (string-based — appendYAMLBlock
	// injects raw YAML that template interpolation may render at varying indent)
	for _, want := range []string{
		"name: kv-secrets",
		"secrets-store.csi.k8s.io",
		"secretProviderClass: tau-kv-ray-kv",
		"/mnt/secrets-store",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("RayJob KV missing %q", want)
		}
	}

	// Verify head and worker both have CSI volumes by counting occurrences
	csiCount := strings.Count(s, "driver: secrets-store.csi.k8s.io")
	if csiCount < 2 {
		t.Errorf("expected CSI driver in head+worker, got %d occurrences", csiCount)
	}

	mountCount := strings.Count(s, "mountPath: /mnt/secrets-store")
	if mountCount < 2 {
		t.Errorf("expected CSI mount in head+worker, got %d occurrences", mountCount)
	}

	// SPC doc
	if !strings.Contains(s, "kind: SecretProviderClass") {
		t.Error("RayJob KV missing SecretProviderClass")
	}
}

func TestRenderWithoutKVSpecOmitsCSI(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: no-kv
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "no-kv.yaml",
		WorkloadKind:     WorkloadKindJob,
		MainScript:       []byte("# train\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "secrets-store.csi.k8s.io") {
		t.Error("CSI driver should not appear without KVSpec")
	}
	if strings.Contains(s, "SecretProviderClass") {
		t.Error("SPC should not appear without KVSpec")
	}
}

func TestRenderServiceAccountNameJob(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: sa-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:           m,
		ManifestRaw:        raw,
		ManifestFilename:   "sa-job.yaml",
		WorkloadKind:       WorkloadKindJob,
		MainScript:         []byte("# train\n"),
		ServiceAccountName: "tau-wi-sa",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(out), "serviceAccountName: tau-wi-sa") {
		t.Errorf("Job should contain serviceAccountName:\n%s", string(out))
	}
}

func TestRenderServiceAccountNameRayJob(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: sa-ray
compute: { gpus: 2, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:           m,
		ManifestRaw:        raw,
		ManifestFilename:   "sa-ray.yaml",
		WorkloadKind:       WorkloadKindRayJob,
		MainScript:         []byte("# train\n"),
		ServiceAccountName: "tau-wi-sa",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	count := strings.Count(s, "serviceAccountName: tau-wi-sa")
	if count < 2 {
		t.Errorf("RayJob should contain serviceAccountName in both head and worker specs, found %d occurrences:\n%s", count, s)
	}
}

func TestRenderServiceAccountNameOmittedWhenEmpty(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: no-sa
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "no-sa.yaml",
		WorkloadKind:     WorkloadKindJob,
		MainScript:       []byte("# train\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(out), "serviceAccountName:") {
		t.Error("Job without ServiceAccountName should not render serviceAccountName field")
	}
}

// --- managedBy, size-limit, and JobSecret-independence coverage ---
//
// These tests lock in three PR2 guarantees that are easy to silently
// regress: (1) Tau never stamps spec.managedBy (KubeRay/Kueue own that
// field), (2) the two independent embedded payloads (script, manifest) are
// each capped at payload.MaxDecodedBytes and the fully-rendered workload
// JSON is additionally capped at maxRenderedWorkloadBytes, and (3) an
// oversized JobSecret can never hide an oversized workload (the size guard
// only ever measures the workload document, not the Secret).

// TestRenderNeverSetsManagedBy mirrors internal/rayjobrender's
// TestRenderNeverSetsManagedBy: Tau must never stamp spec.managedBy on any
// of the four workload kinds, since KubeRay/Kueue/the job controller decide
// that field themselves; Tau stamping it would break their ownership model.
func TestRenderNeverSetsManagedBy(t *testing.T) {
	cases := []struct {
		kind string
		raw  string
	}{
		{
			kind: WorkloadKindJob,
			raw: `
schema_version: 1
name: managedby-check-job
compute:
  gpus: 1
runtime:
  pip:
    - torch==2.4.0
`,
		},
		{
			kind: WorkloadKindRayJob,
			raw: `
schema_version: 1
name: managedby-check-rayjob
compute:
  gpus: 1
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`,
		},
		{
			kind: WorkloadKindRayJobEval,
			raw: `
schema_version: 1
name: managedby-check-eval
compute:
  gpus: 1
eval:
  cpu_workers: 2
runtime:
  pip:
    - torch==2.4.0
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			raw := []byte(tc.raw)
			m, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := Render(RenderOptions{
				Manifest:         m,
				ManifestRaw:      raw,
				ManifestFilename: "managedby-check.yaml",
				WorkloadKind:     tc.kind,
				MainScript:       []byte("# trainer\n"),
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if strings.Contains(string(out), "managedBy") {
				t.Fatalf("%s output must never mention managedBy in any form; got:\n%s", tc.kind, out)
			}
			doc := unmarshalLast(t, out)
			if got := dig(doc, "spec", "managedBy"); got != nil {
				t.Fatalf("%s spec.managedBy = %v, want absent", tc.kind, got)
			}
		})
	}
}

// TestRenderScriptAndManifestPayloadsAreDistinct proves the two payloads
// embedded in every workload (script, manifest) never collide: they use
// different init-container names, different volumes/mount targets, and
// (since their content always differs) different digests. This is the
// Design A guarantee that the two payload artifacts are fully independent,
// not a single shared payload wearing two names.
func TestRenderScriptAndManifestPayloadsAreDistinct(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: distinct-payloads
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "distinct-payloads.yaml",
		MainScript:       []byte("# trainer\n"),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if scriptPayloadInitContainerName == manifestPayloadInitContainerName {
		t.Fatalf("script and manifest init container names must differ, both are %q", scriptPayloadInitContainerName)
	}
	if workloadmeta.AnnotationScriptPayloadDigest == workloadmeta.AnnotationManifestPayloadDigest {
		t.Fatalf("script and manifest digest annotation keys must differ, both are %q", workloadmeta.AnnotationScriptPayloadDigest)
	}
	if scriptPayloadTargetDir == manifestPayloadTargetDir {
		t.Fatalf("script and manifest target dirs must differ, both are %q", scriptPayloadTargetDir)
	}

	doc := unmarshalLast(t, out)
	spec := dig(doc, "spec", "template", "spec")
	initContainers := podSpecInitContainers(t, spec)
	scriptIC := containerByName(t, initContainers, scriptPayloadInitContainerName)
	manifestIC := containerByName(t, initContainers, manifestPayloadInitContainerName)
	if scriptIC["name"] == manifestIC["name"] {
		t.Fatalf("rendered script and manifest init containers must have distinct names, both are %v", scriptIC["name"])
	}

	scriptDigest, _ := dig(doc, "metadata", "annotations", workloadmeta.AnnotationScriptPayloadDigest).(string)
	manifestDigest, _ := dig(doc, "metadata", "annotations", workloadmeta.AnnotationManifestPayloadDigest).(string)
	if scriptDigest == "" || manifestDigest == "" {
		t.Fatalf("both payload digest annotations must be present; script=%q manifest=%q", scriptDigest, manifestDigest)
	}
	if scriptDigest == manifestDigest {
		t.Fatalf("script and manifest payloads must have distinct digests (they carry different content), both are %q", scriptDigest)
	}

	// Volumes and mounts must also target distinct emptyDirs — same
	// safety property as the init container/digest checks, but at the
	// pod-spec/container-mount level.
	volumes, _ := dig(spec, "volumes").([]any)
	seenVolumeNames := map[string]bool{}
	for _, v := range volumes {
		name, _ := dig(v, "name").(string)
		if name == "" {
			continue
		}
		if seenVolumeNames[name] {
			t.Fatalf("duplicate volume name %q in pod spec", name)
		}
		seenVolumeNames[name] = true
	}
	if len(seenVolumeNames) < 2 {
		t.Fatalf("expected at least 2 distinct volumes (script + manifest emptyDirs), got %v", seenVolumeNames)
	}
}

// comment of padBytes incompressible ASCII characters appended, used to
// precisely inflate the manifest payload's decoded size without touching its
// parsed semantics. The padding is deliberately incompressible: payload
// envelopes are gzip-compressed, so a run of repeated characters would shrink
// to almost nothing and could never exercise the size guards these probes
// exist to pin.
func sizeProbeManifest(padBytes int) []byte {
	raw := []byte(`
schema_version: 1
name: sizeprobe
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	pad := "# " + string(incompressibleASCII(1, padBytes)) + "\n"
	return append(raw, []byte(pad)...)
}

// incompressibleASCII returns n deterministic, effectively incompressible
// characters drawn from [A-Za-z0-9]. Tests that need to push a payload past a
// size guard must use this rather than strings.Repeat, because the payload
// envelope is gzip-compressed before it is embedded. seed varies the output so
// callers generating many strings (e.g. unique file names) do not hand gzip a
// set of identical runs to collapse.
func incompressibleASCII(seed int64, n int) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	r := rand.New(rand.NewSource(seed))
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[r.Intn(len(alphabet))]
	}
	return out
}

// TestRenderRejectsScriptPayloadOverCap proves the script payload is
// independently subject to payload.MaxDecodedBytes, and that Render()
// surfaces the failure with a "script payload:" prefix so the two payloads'
// errors are distinguishable.
func TestRenderRejectsScriptPayloadOverCap(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: script-over-cap
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// train.py ("# trainer\n", 10 bytes) + this extra file must together
	// decode to just over payload.MaxDecodedBytes.
	over := make([]byte, payload.MaxDecodedBytes)
	for i := range over {
		over[i] = 'a'
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "script-over-cap.yaml",
		MainScript:       []byte("# trainer\n"),
		ExtraScripts:     []ExtraScript{{Name: "over.py", Data: over}},
	})
	if err == nil {
		t.Fatal("Render: want error for script payload over payload.MaxDecodedBytes, got nil")
	}
	if !strings.Contains(err.Error(), "script payload:") {
		t.Errorf("error must be prefixed \"script payload:\" so it's distinguishable from the manifest payload cap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "exceeds the limit of") {
		t.Errorf("error must explain the payload limit was exceeded; got: %v", err)
	}
}

// manyLongNamedExtraScripts builds n ExtraScript entries, each with a
// nameLen-character destination name and 1 byte of content. It exists to
// prove that payload.MaxDecodedBytes (which sums only file *content* bytes)
// is insufficient on its own: many small files with long names can stay
// trivially under that decoded ceiling while their JSON envelope's per-file
// name/field overhead — after compression and base64 encoding the whole
// envelope — blows past the limit on the final TAU_PAYLOAD_B64=<encoded>
// environment entry. Names are drawn from incompressible ASCII so the guard
// is exercised against real encoded size rather than against a run of
// characters gzip would collapse.
func manyLongNamedExtraScripts(n, nameLen int) []ExtraScript {
	scripts := make([]ExtraScript, 0, n)
	for i := 0; i < n; i++ {
		// Destination names must match payloadFileNamePattern
		// ([A-Za-z0-9._-]+); a numeric prefix keeps them unique and the
		// incompressible remainder pads each to exactly nameLen characters.
		prefix := fmt.Sprintf("f%04d", i)
		name := prefix + string(incompressibleASCII(int64(i), nameLen-len(prefix))) + ".py"
		scripts = append(scripts, ExtraScript{Name: name, Data: []byte("x")})
	}
	return scripts
}

// TestRenderRejectsManyFilesOverPayloadEnvArgLimit proves blocker #2 from the
// autoreview of PR #894: a script payload can be trivially under
// payload.MaxDecodedBytes (64 KiB of file *content*) while still rendering an
// encoded TAU_PAYLOAD_B64 environment entry over the 131072 byte Linux
// MAX_ARG_STRLEN limit, because that cap does not account for per-file name
// and JSON envelope overhead. 500 files x 200-char names x 1 content byte
// each decode to only 500 bytes (0.76% of MaxDecodedBytes) but encode to a
// ~152 KB env value — over the limit by a wide margin, not a boundary case,
// because base64's 3-byte-to-4-char block quantization makes an exact
// single-byte crossover impractical to construct deterministically across
// Go versions/JSON encoders.
func TestRenderRejectsManyFilesOverPayloadEnvArgLimit(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: many-small-files
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	scripts := manyLongNamedExtraScripts(800, 200)
	decoded := len(scripts) // 1 content byte per file
	if decoded >= payload.MaxDecodedBytes {
		t.Fatalf("test setup bug: decoded size %d must stay under payload.MaxDecodedBytes (%d) to prove this is NOT a MaxDecodedBytes violation", decoded, payload.MaxDecodedBytes)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "many-small-files.yaml",
		MainScript:       []byte("# trainer\n"),
		ExtraScripts:     scripts,
	})
	if err == nil {
		t.Fatal("Render: want error for script payload whose encoded env value exceeds the kernel argument limit, got nil")
	}
	if !strings.Contains(err.Error(), "script payload") {
		t.Errorf("error must be prefixed to identify the script payload; got: %v", err)
	}
	if !strings.Contains(err.Error(), "environment entry") {
		t.Errorf("error must identify the environment entry limit, not the decoded ceiling (decoded size is only %d bytes) — got: %v", decoded, err)
	}
	if !strings.Contains(err.Error(), "MAX_ARG_STRLEN") && !strings.Contains(err.Error(), "kernel argument limit") {
		t.Errorf("error must explain the kernel argument (MAX_ARG_STRLEN) limit is what bounds this; got: %v", err)
	}
}

// TestRenderAcceptsManyFilesUnderPayloadEnvArgLimit is the contrasting case:
// a similarly file-count-heavy payload that stays under both
// payload.MaxDecodedBytes AND the encoded env arg limit must render
// successfully — proving the env entry guard doesn't false-positive on
// ordinary multi-file payloads.
func TestRenderAcceptsManyFilesUnderPayloadEnvArgLimit(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: many-small-files-ok
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	scripts := manyLongNamedExtraScripts(200, 100)
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "many-small-files-ok.yaml",
		MainScript:       []byte("# trainer\n"),
		ExtraScripts:     scripts,
	})
	if err != nil {
		t.Fatalf("Render: 200 small named files should stay under both payload caps, got error: %v", err)
	}
	doc := unmarshalLast(t, out)
	spec := dig(doc, "spec", "template", "spec")
	initContainers := podSpecInitContainers(t, spec)
	scriptInit := containerByName(t, initContainers, scriptPayloadInitContainerName)
	files := decodePayloadFiles(t, scriptInit)
	if len(files) != len(scripts)+1 { // +1 for MainScript's train.py
		t.Errorf("decoded script payload file count = %d, want %d", len(files), len(scripts)+1)
	}
}

// TestRenderRejectsWorkloadOverRenderedSizeLimit and
// TestRenderAcceptsWorkloadUnderRenderedSizeLimit together pin the exact
// crossover point of the additional maxRenderedWorkloadBytes (200 KiB) hard
// guard on the fully-rendered workload JSON. Neither payload alone can
// reach 200 KiB (each is separately capped at 64 KiB decoded by
// payload.MaxDecodedBytes), so both a near-cap script payload AND a
// near-cap manifest payload must be combined to cross this limit — proving
// the guard genuinely measures the *combined* rendered object, not just one
// payload. Byte counts below were bisected empirically against the current
// render.go template overhead; if the templates change materially, these
// two tests will need to be re-bisected (they currently sit just under and
// just over the limit respectively with the current canonical MCR workload
// image contract).
const sizeLimitScriptExtraBytes = 88000

func renderSizeLimitProbe(t *testing.T, manifestPadBytes int) ([]byte, error) {
	t.Helper()
	raw := sizeProbeManifest(manifestPadBytes)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	extra := incompressibleASCII(2, sizeLimitScriptExtraBytes)
	return Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "sizelimit.yaml",
		MainScript:       []byte("# trainer\n"),
		ExtraScripts:     []ExtraScript{{Name: "pad.py", Data: extra}},
	})
}

func TestRenderAcceptsWorkloadUnderRenderedSizeLimit(t *testing.T) {
	// manifestPadBytes=66000 lands the rendered workload JSON just below
	// 204800 bytes — under the rendered-workload limit with the batch Job's
	// canonical MCR image, and with both payloads' encoded environment
	// entries still inside payload.MaxEnvEntryBytes.
	out, err := renderSizeLimitProbe(t, 66000)
	if err != nil {
		t.Fatalf("Render: want success at (not over) the %d byte rendered-workload limit, got error: %v", maxRenderedWorkloadBytes, err)
	}
	doc := unmarshalLast(t, out)
	jsonBytes, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(jsonBytes) > maxRenderedWorkloadBytes {
		t.Fatalf("test fixture drifted: rendered workload JSON is %d bytes, want <= %d (re-bisect manifestPadBytes)", len(jsonBytes), maxRenderedWorkloadBytes)
	}
	t.Logf("just-under case: rendered workload JSON is %d bytes (limit %d)", len(jsonBytes), maxRenderedWorkloadBytes)
}

func TestRenderRejectsWorkloadOverRenderedSizeLimit(t *testing.T) {
	// Keep enough margin that public identifier length changes cannot move this
	// fixture back below the rendered-workload limit.
	_, err := renderSizeLimitProbe(t, 70000)
	if err == nil {
		t.Fatalf("Render: want error just over the %d byte rendered-workload limit, got nil", maxRenderedWorkloadBytes)
	}
	if !strings.Contains(err.Error(), "exceeds the") || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error must explain the rendered-workload byte limit was exceeded; got: %v", err)
	}
	t.Logf("just-over case error: %v", err)
}

func TestJobSecretDoesNotCountTowardRenderedSizeLimit(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: secret-size-check
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HUGE_SECRET
      valueFrom:
        secretKeyRef:
          name: tau-secret-size-check-secrets
          key: HUGE_SECRET
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 300 KiB of secret string data — bigger than maxRenderedWorkloadBytes
	// on its own. If the guard ever mistakenly measured the Secret, or
	// measured Secret+workload combined, this render would fail.
	hugeSecretValue := strings.Repeat("s", 300*1024)
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "secret-size-check.yaml",
		MainScript:       []byte("# trainer\n"),
		JobSecret: &JobSecret{
			Name:       "tau-secret-size-check-secrets",
			StringData: map[string]string{"HUGE_SECRET": hugeSecretValue},
		},
	})
	if err != nil {
		t.Fatalf("Render: a large JobSecret must never count toward the workload size limit, got error: %v", err)
	}
	docs := splitDocs(t, out)
	if len(docs) != 2 {
		t.Fatalf("expected exactly 2 documents (Secret + workload), got %d", len(docs))
	}
	if !strings.Contains(docs[0], "kind: Secret") {
		t.Fatalf("first document must be the Secret; got:\n%s", docs[0])
	}
	if len(docs[0]) < 300*1024 {
		t.Errorf("Secret document should carry the full 300 KiB secret value, got only %d bytes", len(docs[0]))
	}
}

func TestNormalizeGPUResourceModeMIG(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"mig", GPUResourceModeMIG},
		{"MIG", GPUResourceModeMIG},
		{"mig-slice", GPUResourceModeMIG},
		{"  mig  ", GPUResourceModeMIG},
		{"device-plugin", GPUResourceModeDevicePlugin},
		{"dra", GPUResourceModeDRA},
	}
	for _, tc := range cases {
		got, err := NormalizeGPUResourceMode(tc.input)
		if err != nil {
			t.Errorf("NormalizeGPUResourceMode(%q) error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeGPUResourceMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMIGResourceName(t *testing.T) {
	cases := []struct {
		profile string
		want    string
	}{
		{"1g.18gb", "nvidia.com/mig-1g.18gb"},
		{"3g.71gb", "nvidia.com/mig-3g.71gb"},
		{"7g.141gb", "nvidia.com/mig-7g.141gb"},
	}
	for _, tc := range cases {
		got := MIGResourceName(tc.profile)
		if got != tc.want {
			t.Errorf("MIGResourceName(%q) = %q, want %q", tc.profile, got, tc.want)
		}
	}
}

func TestRenderMIGUsesCorrectResourceName(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: mig-test
compute:
  gpus: 1
  gpu_resource_mode: mig
  mig_profile: 1g.18gb
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "mig-test.yaml",
		Namespace:        "ray",
		SmokePairs:       1,
		MainScript:       []byte("# trainer\n"),
		GPUResourceMode:  "mig",
		MIGProfile:       "1g.18gb",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `nvidia.com/mig-1g.18gb: "1"`) {
		t.Errorf("rendered output missing nvidia.com/mig-1g.18gb resource request:\n%s", s)
	}
	if strings.Contains(s, `nvidia.com/gpu: "1"`) {
		t.Errorf("rendered output should NOT contain nvidia.com/gpu when in MIG mode:\n%s", s)
	}
}

func TestRenderMIGRejectsEmptyProfile(t *testing.T) {
	raw := []byte(`
schema_version: 1
name: mig-noprofile
compute:
  gpus: 1
  gpu_resource_mode: mig
runtime:
  pip:
    - torch==2.4.0
`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = Render(RenderOptions{
		Manifest:         m,
		ManifestRaw:      raw,
		ManifestFilename: "mig-noprofile.yaml",
		Namespace:        "ray",
		SmokePairs:       1,
		MainScript:       []byte("# trainer\n"),
		GPUResourceMode:  "mig",
	})
	if err == nil {
		t.Fatal("expected error when MIG mode has no profile, got nil")
	}
	if !strings.Contains(err.Error(), "mig_profile") {
		t.Fatalf("error should mention mig_profile, got: %v", err)
	}
}

// Every workload template installs runtime.pip into the pod. On the canonical
// Ray CUDA image a plain install fails with EACCES (pip cannot write to the
// root-owned site-packages/nvidia) and the workload dies before the trainer
// starts. All four templates must fall back to the user site; on images that
// install cleanly, such as the plain non-CUDA ray tags, the fallback no-ops.
func TestWorkloadTemplatesFallBackToUserSitePip(t *testing.T) {
	for _, name := range []string{
		"managed-workflow-job.yaml.tmpl",
		"managed-workflow-rayjob.yaml.tmpl",
		"managed-workflow-rayjob-cpu.yaml.tmpl",
		"managed-workflow-rayjob-eval.yaml.tmpl",
	} {
		b, err := Asset(name)
		if err != nil {
			t.Fatalf("Asset(%s): %v", name, err)
		}
		if !strings.Contains(string(b), "|| pip install --quiet --no-cache-dir --user ${PIP_PACKAGES}") {
			t.Errorf("%s installs runtime.pip with no user-site fallback; it will fail with EACCES on the nonroot canonical images", name)
		}
	}
}

// A --user install puts console scripts (torchrun, deepspeed) under the user
// base bin dir, which is not on PATH in the canonical images. The rayjob
// template invokes `torchrun` as a bare command for single-pod multi-GPU runs,
// so without this export the pip fallback produces importable libraries and an
// immediate exit 127. Observed live on an 8-GPU A100 RayJob.
func TestWorkloadTemplatesPutUserSiteBinOnPath(t *testing.T) {
	for _, name := range []string{
		"managed-workflow-job.yaml.tmpl",
		"managed-workflow-rayjob.yaml.tmpl",
		"managed-workflow-rayjob-cpu.yaml.tmpl",
		"managed-workflow-rayjob-eval.yaml.tmpl",
	} {
		body, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		got := string(body)
		if !strings.Contains(got, `--user ${PIP_PACKAGES}`) {
			t.Fatalf("%s: expected a --user pip fallback", name)
		}
		if !strings.Contains(got, `python3 -m site --user-base`) {
			t.Errorf("%s: --user fallback does not add the user base bin dir to PATH, "+
				"so console scripts such as torchrun are installed but not runnable", name)
		}
	}
}
