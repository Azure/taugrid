package rayjobrender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/payload"
	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func decodeDocs(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode yaml: %v\n%s", err, string(data))
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

func TestRenderRayTrainScriptAsKueueRayJob(t *testing.T) {
	out, err := Render(Options{
		Name:               "ray-smoke",
		Namespace:          "ray",
		ServiceAccountName: "tau-workload",
		ScriptName:         "train.py",
		Script:             []byte("from ray.train.torch import TorchTrainer\n"),
		Workers:            2,
		GPUsPerWorker:      1,
		RuntimePip:         []string{"torch==2.4.0", "transformers"},
		Env:                map[string]string{"WANDB_MODE": "offline"},
		DataPVC:            "training-data",
		Labels: map[string]string{
			workloadmeta.LabelJob: "ray-smoke",
			"run_id":              "ray-run-1",
		},
		Annotations: map[string]string{
			workloadmeta.LabelExperiment:               "ray-train",
			workloadmeta.LabelStellarProject:           "smoke",
			workloadmeta.AnnotationStellarExperimentID: "ray-train:exact",
			workloadmeta.AnnotationWorkspaceID:         "sample",
		},
		TopologyOptions: topology.Options{
			Team:      "research",
			Lane:      "training",
			QueueName: "team-a",
			Placement: "independent",
			GPUClass:  "any",
		},

		AzureWorkloadIdentity: true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	docs := decodeDocs(t, out)
	if len(docs) != 1 {
		t.Fatalf("doc count=%d want 1 (self-contained RayJob, no per-run ConfigMap)\n%s", len(docs), string(out))
	}

	rayjob := docs[0]
	if rayjob["kind"] != "RayJob" {
		t.Fatalf("first doc kind=%v", rayjob["kind"])
	}
	meta := rayjob["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels[topology.QueueLabel] != "team-a" {
		t.Fatalf("queue label missing: %v", labels)
	}
	if labels[workloadmeta.LabelManagedBy] != "tau" {
		t.Fatalf("managed workload admission label=%v, want tau", labels[workloadmeta.LabelManagedBy])
	}
	if labels[workloadmeta.LabelJob] != "ray-smoke" || labels["run_id"] != "ray-run-1" {
		t.Fatalf("caller metadata labels missing: %v", labels)
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations[workloadmeta.LabelExperiment] != "ray-train" || annotations[workloadmeta.LabelStellarProject] != "smoke" {
		t.Fatalf("caller metadata annotations missing: %v", annotations)
	}
	if digest, _ := annotations[payload.AnnotationDigest].(string); digest == "" {
		t.Fatalf("payload digest annotation missing: %v", annotations)
	}
	spec := rayjob["spec"].(map[string]any)
	if spec["suspend"] != true {
		t.Fatalf("RayJob must be suspended for Kueue admission: %v", spec["suspend"])
	}
	if !strings.Contains(spec["entrypoint"].(string), "python3 /script/train.py") {
		t.Fatalf("entrypoint does not run staged driver:\n%s", spec["entrypoint"])
	}
	if !strings.Contains(spec["runtimeEnvYAML"].(string), "torch==2.4.0") {
		t.Fatalf("runtimeEnvYAML missing pip deps:\n%s", spec["runtimeEnvYAML"])
	}
	cluster := spec["rayClusterSpec"].(map[string]any)
	if cluster["rayVersion"] != RayVersion {
		t.Fatalf("rayVersion=%v want %s", cluster["rayVersion"], RayVersion)
	}
	workers := cluster["workerGroupSpecs"].([]any)
	if len(workers) != 1 {
		t.Fatalf("workerGroupSpecs=%v", workers)
	}
	if got := workers[0].(map[string]any)["replicas"]; got != 1 {
		t.Fatalf("worker replicas=%v want 1 for two total execution pods", got)
	}
	head := cluster["headGroupSpec"].(map[string]any)
	tpl := head["template"].(map[string]any)
	pod := tpl["spec"].(map[string]any)
	if pod["serviceAccountName"] != "tau-workload" {
		t.Fatalf("head serviceAccountName=%v want tau-workload", pod["serviceAccountName"])
	}
	containers := pod["containers"].([]any)
	headContainer := containers[0].(map[string]any)
	if headContainer["image"] != DefaultGPUImage {
		t.Fatalf("default GPU image=%v want %s", headContainer["image"], DefaultGPUImage)
	}
	if _, hasStartup := headContainer["startupProbe"]; hasStartup {
		t.Fatalf("head should not have explicit startupProbe (let KubeRay inject): %v", headContainer["startupProbe"])
	}
	if _, hasReadiness := headContainer["readinessProbe"]; hasReadiness {
		t.Fatalf("head should not have explicit readinessProbe (let KubeRay inject): %v", headContainer["readinessProbe"])
	}
	if _, hasLiveness := headContainer["livenessProbe"]; hasLiveness {
		t.Fatalf("head should not have explicit livenessProbe (let KubeRay inject): %v", headContainer["livenessProbe"])
	}
	ports := headContainer["ports"].([]any)
	portYAML := asYAML(t, ports)
	for _, want := range []string{"name: metrics", "containerPort: 8080", "name: dashboard", "containerPort: 8265", "name: gcs", "containerPort: 6379", "name: client", "containerPort: 10001"} {
		if !strings.Contains(portYAML, want) {
			t.Fatalf("head ports missing %q:\n%s", want, portYAML)
		}
	}

	podMeta := tpl["metadata"].(map[string]any)
	podLabels := podMeta["labels"].(map[string]any)
	if podLabels[workloadmeta.LabelAzureWorkloadIdentityUse] != "true" {
		t.Fatalf("head workload identity label missing: %v", podLabels)
	}
	podAnns := podMeta["annotations"].(map[string]any)
	if podAnns["adx-mon/scrape"] != "true" || podAnns["adx-mon/port"] != fmt.Sprintf("%d", metricsPort) {
		t.Fatalf("adx-mon scrape annotations missing: %v", podAnns)
	}
	if podAnns[raylogoffload.AnnotationKey] != raylogoffload.AnnotationValue {
		t.Fatalf("head log offload annotation missing: %v", podAnns)
	}
	for key, want := range map[string]string{
		workloadmeta.LabelStellarProject:           "smoke",
		workloadmeta.AnnotationStellarExperimentID: "ray-train:exact",
		workloadmeta.AnnotationWorkspaceID:         "sample",
	} {
		if podAnns[key] != want {
			t.Fatalf("head pod annotation %s=%v want %s", key, podAnns[key], want)
		}
		workerMeta := workers[0].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
		workerAnnotations := workerMeta["annotations"].(map[string]any)
		if workerAnnotations[key] != want {
			t.Fatalf("worker pod annotation %s=%v want %s", key, workerAnnotations[key], want)
		}
	}
	if strings.Count(string(out), raylogoffload.AnnotationKey) != 1 {
		t.Fatalf("expected exactly one head log offload annotation:\n%s", string(out))
	}
	sidecar := containerByName(t, containers, raylogoffload.SidecarContainerName)
	if sidecar["image"] != DefaultGPUImage {
		t.Fatalf("sidecar image=%v want %s", sidecar["image"], DefaultGPUImage)
	}
	if got := asYAML(t, sidecar["args"]); !strings.Contains(got, "/tmp/ray/session_latest/logs/job-driver-*.log") {
		t.Fatalf("sidecar script missing ray driver log contract:\n%s", got)
	}
	if got := asYAML(t, sidecar["env"]); !strings.Contains(got, "TAU_RAY_LOG_COMPLETION_FILE") || !strings.Contains(got, raylogoffload.CompletionFilePath) {
		t.Fatalf("sidecar env missing completion contract:\n%s", got)
	}
	entrypoint := spec["entrypoint"].(string)
	for _, want := range []string{"tau_write_driver_log_completion", "trap tau_complete_driver_logs EXIT"} {
		if !strings.Contains(entrypoint, want) {
			t.Fatalf("entrypoint missing driver completion contract %q:\n%s", want, entrypoint)
		}
	}
	if mounts := asYAML(t, sidecar["volumeMounts"]); !strings.Contains(mounts, "mountPath: /tmp/ray") || !strings.Contains(mounts, "readOnly: true") {
		t.Fatalf("sidecar volume mount missing /tmp/ray readonly contract:\n%s", mounts)
	}
	if mounts := asYAML(t, headContainer["volumeMounts"]); !strings.Contains(mounts, "mountPath: /tmp/ray") {
		t.Fatalf("head container missing /tmp/ray mount:\n%s", mounts)
	}
	initContainers := pod["initContainers"].([]any)
	prepare := containerByName(t, initContainers, raylogoffload.PrepareInitName)
	if got := asYAML(t, prepare["command"]); !strings.Contains(got, "chmod 1777 /tmp/ray") {
		t.Fatalf("prepare init container missing writable /tmp/ray contract:\n%s", got)
	}
	volumes := pod["volumes"].([]any)
	if !strings.Contains(asYAML(t, volumes), "training-data") {
		t.Fatalf("data PVC not mounted: %v", volumes)
	}
	if !strings.Contains(asYAML(t, volumes), "name: "+raylogoffload.VolumeName) {
		t.Fatalf("head pod missing shared /tmp/ray volume: %v", volumes)
	}
	worker := workers[0].(map[string]any)
	workerMeta := worker["template"].(map[string]any)["metadata"].(map[string]any)
	workerLabels := workerMeta["labels"].(map[string]any)
	if workerLabels[workloadmeta.LabelAzureWorkloadIdentityUse] != "true" {
		t.Fatalf("worker workload identity label missing: %v", workerLabels)
	}
	if anns, _ := workerMeta["annotations"].(map[string]any); anns != nil {
		if _, ok := anns[raylogoffload.AnnotationKey]; ok {
			t.Fatalf("worker should not carry head-only log offload annotation: %v", anns)
		}
	}
	workerPod := worker["template"].(map[string]any)["spec"].(map[string]any)
	if workerPod["serviceAccountName"] != "tau-workload" {
		t.Fatalf("worker serviceAccountName=%v want tau-workload", workerPod["serviceAccountName"])
	}
	if containerNames(t, workerPod["containers"].([]any)) != "ray-worker" {
		t.Fatalf("worker should not include head-only log sidecar: %s", containerNames(t, workerPod["containers"].([]any)))
	}
	workerContainer := workerPod["containers"].([]any)[0].(map[string]any)
	workerPorts := asYAML(t, workerContainer["ports"])
	if !strings.Contains(workerPorts, "name: metrics") || strings.Contains(workerPorts, "name: dashboard") {
		t.Fatalf("worker should expose metrics but not dashboard ports:\n%s", workerPorts)
	}
	if probe := workerContainer["startupProbe"].(map[string]any); probe["httpGet"].(map[string]any)["path"] != "/api/healthz" {
		t.Fatalf("worker startupProbe should use /api/healthz for Ray >=2.53: %v", probe)
	}
	if probe := workerContainer["readinessProbe"].(map[string]any); fmt.Sprint(probe["tcpSocket"].(map[string]any)["port"]) != "52365" {
		t.Fatalf("worker readinessProbe should be TCP socket on agent port: %v", probe)
	}
	if probe := workerContainer["livenessProbe"].(map[string]any); fmt.Sprint(probe["tcpSocket"].(map[string]any)["port"]) != "52365" {
		t.Fatalf("worker livenessProbe should be TCP socket on agent port: %v", probe)
	}
}

func TestRenderSingleRayExecutionPodUsesHeadOnly(t *testing.T) {
	out, err := Render(Options{
		Name:          "ray-single",
		Namespace:     "tau",
		ScriptName:    "train.py",
		Script:        []byte("print('train')\n"),
		Workers:       1,
		GPUsPerWorker: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	rayjob := decodeDocs(t, out)[0]
	cluster := rayjob["spec"].(map[string]any)["rayClusterSpec"].(map[string]any)
	if groups := cluster["workerGroupSpecs"].([]any); len(groups) != 0 {
		t.Fatalf("workerGroupSpecs=%v want none for one total execution pod", groups)
	}
	head := cluster["headGroupSpec"].(map[string]any)
	if got := head["rayStartParams"].(map[string]any)["num-gpus"]; got != "1" {
		t.Fatalf("head num-gpus=%v want 1 because the head participates in execution", got)
	}
}

func TestRenderRayJobWithManagedMetricsAndStagedArtifacts(t *testing.T) {
	runtime := metricsoffload.Runtime{
		Image:                   "registry.example.com/taugrid/tau@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RunID:                   "modernbert-ray",
		Project:                 "pretraining",
		Experiment:              "modernbert-fineweb",
		Group:                   "fwe100",
		Tags:                    map[string]string{"tau_workspace": "research-workspace", "tau_namespace": "research-workspace", "tau_cluster": "sample-gpu-cluster"},
		Source:                  "stellar-online",
		Store:                   "/data/research-workspace/runs/modernbert-ray/.tau/metrics/session/expstore",
		Out:                     "/data/research-workspace/runs/modernbert-ray/.tau/metrics/session/offload",
		History:                 []string{"/data/research-workspace/runs/modernbert-ray/metrics-history-attempt-*/*.jsonl"},
		CompletionFile:          "/var/run/tau/metrics-completion.json",
		RemoteWriteEndpoint:     "http://${NODE_IP}:3100/receive",
		Interval:                10 * time.Second,
		ArtifactURI:             "/data/research-workspace/runs/modernbert-ray",
		BaselineExistingHistory: true,
		ReadyFile:               "/var/run/tau/metrics-ready",
		ReadyTimeout:            2 * time.Minute,
		DoneFile:                "/var/run/tau/metrics-done",
		DoneTimeout:             2 * time.Minute,
	}
	out, err := Render(Options{
		Name:          "modernbert-ray",
		Namespace:     "research-workspace",
		ScriptName:    "train.py",
		Script:        []byte("print('train')\n"),
		Workers:       1,
		GPUsPerWorker: 1,
		DataPVC:       "research-workspace",
		OutputDir:     "/data/research-workspace/runs/modernbert-ray",
		ArtifactPublish: artifactpublish.Runtime{
			Mode:          artifactpublish.ModeStaged,
			OutputDir:     "/data/research-workspace/runs/modernbert-ray",
			StagingDir:    "/mnt/tau-output/modernbert-ray",
			PublicationID: "publication-1",
		},
		MetricsOffload: runtime,
		Annotations: map[string]string{
			workloadmeta.AnnotationResultPath:            "/data/research-workspace/runs/modernbert-ray",
			workloadmeta.AnnotationResultPVC:             "research-workspace",
			workloadmeta.AnnotationExperimentSource:      "stellar",
			workloadmeta.AnnotationMetricsSession:        "session",
			workloadmeta.AnnotationArtifactPublication:   "staged",
			workloadmeta.AnnotationArtifactPublicationID: "publication-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(out)
	for _, want := range []string{
		"name: metrics-offload",
		"--baseline-existing-history",
		"--done-file",
		"/var/run/tau/metrics-done",
		"TAU_OUTPUT_STAGING_DIR",
		"/mnt/tau-output/modernbert-ray",
		workloadmeta.AnnotationExperimentSource + ": stellar",
		workloadmeta.AnnotationMetricsSession + ": session",
		workloadmeta.AnnotationResultPath + ": /data/research-workspace/runs/modernbert-ray",
		workloadmeta.AnnotationArtifactPublication + ": staged",
		workloadmeta.AnnotationArtifactPublicationID + ": publication-1",
		"tau_workspace=research-workspace",
		"tau_namespace=research-workspace",
		"tau_cluster=sample-gpu-cluster",
		"metrics-entrypoint.sh",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("managed RayJob missing %q:\n%s", want, rendered)
		}
	}
	docs := decodeDocs(t, out)
	spec := docs[0]["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	pod := head["template"].(map[string]any)["spec"].(map[string]any)
	if got := containerNames(t, pod["containers"].([]any)); !strings.Contains(got, "metrics-offload") {
		t.Fatalf("head containers = %s", got)
	}
}

func TestRenderCustomOlderRayImageUsesOldProbeEndpoint(t *testing.T) {
	out, err := Render(Options{
		Name:          "ray-old",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("print('train')\n"),
		Image:         "registry.example.com/taugrid/ray:py3.10-ray2.39.0-gpu-rdma-jammy-custom",
		Workers:       2,
		GPUsPerWorker: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	workers := cluster["workerGroupSpecs"].([]any)
	worker := workers[0].(map[string]any)
	workerPod := worker["template"].(map[string]any)["spec"].(map[string]any)
	workerContainer := workerPod["containers"].([]any)[0].(map[string]any)
	probe := workerContainer["startupProbe"].(map[string]any)
	path := probe["httpGet"].(map[string]any)["path"]
	if path != "/api/local_raylet_healthz" {
		t.Fatalf("worker startupProbe with Ray 2.39 image should use /api/local_raylet_healthz, got %v", path)
	}
	if cluster["rayVersion"] != "2.39.0" {
		t.Fatalf("rayVersion should match image tag, got %v", cluster["rayVersion"])
	}
}

func TestRenderReleasesCapacityShortlyAfterCompletion(t *testing.T) {
	// Until this TTL elapses a completed RayJob keeps its pods Running and holding
	// node capacity. It was 86400 (24h), so a job could report SUCCEEDED and still
	// consume the cluster all day. Keep it short so finished runs cannot block
	// follow-on jobs.
	out, err := Render(Options{
		Name:       "ray-ttl",
		Namespace:  "ray",
		ScriptName: "train.py",
		Script:     []byte("print('train')\n"),
		Workers:    1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	spec := decodeDocs(t, out)[0]["spec"].(map[string]any)
	if got := spec["shutdownAfterJobFinishes"]; got != true {
		t.Errorf("shutdownAfterJobFinishes = %v, want true (otherwise the RayCluster is never reclaimed)", got)
	}
	ttl, ok := spec["ttlSecondsAfterFinished"].(int)
	if !ok {
		if v, isInt64 := spec["ttlSecondsAfterFinished"].(int64); isInt64 {
			ttl, ok = int(v), true
		}
	}
	if !ok {
		t.Fatalf("ttlSecondsAfterFinished missing or not numeric: %#v", spec["ttlSecondsAfterFinished"])
	}
	if ttl > 120 {
		t.Errorf("ttlSecondsAfterFinished = %d; a completed RayJob must not keep holding node capacity this long", ttl)
	}
}

func TestRenderRayTuneSetsDistributedExecutionContract(t *testing.T) {
	out, err := Render(Options{
		Name:          "ray-tune",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("def train_func(config):\n    pass\n"),
		Launcher:      "ray-tune",
		TuneMetric:    "loss",
		Workers:       3,
		GPUsPerWorker: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := string(out)
	for _, want := range []string{"name: TAU_NUM_WORKERS\n", "value: \"3\"", "name: TAU_DIST_BACKEND\n", "value: nccl"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered Ray Tune contract missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderEnvSecretUsesSecretKeyRef(t *testing.T) {
	out, err := Render(Options{
		Name:          "ray-secret",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("print('train')\n"),
		Workers:       2,
		GPUsPerWorker: 1,
		EnvSecrets:    []envspec.Var{envspec.Secret("HF_TOKEN", "hf-secret", "token-key")},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"name: HF_TOKEN",
		"secretKeyRef:",
		"name: hf-secret",
		"key: token-key",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("rendered secret env missing %q:\n%s", want, s)
		}
	}
}

func TestRenderRedactsEnvSecretRefs(t *testing.T) {
	out, err := Render(Options{
		Name:          "ray-secret",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("print('train')\n"),
		Workers:       1,
		GPUsPerWorker: 0,
		EnvSecrets:    []envspec.Var{envspec.Secret("HF_TOKEN", "hf-secret", "token-key")},
		RedactSecrets: true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, want := range []string{"secretKeyRef:", "name: <redacted>", "key: <redacted>"} {
		if !strings.Contains(s, want) {
			t.Fatalf("redacted secret env missing %q:\n%s", want, s)
		}
	}
	for _, leaked := range []string{"hf-secret", "token-key"} {
		if strings.Contains(s, leaked) {
			t.Fatalf("redacted secret env leaked %q:\n%s", leaked, s)
		}
	}
}

func TestRenderRejectsReservedEnv(t *testing.T) {
	_, err := Render(Options{
		Name:          "bad-env",
		Namespace:     "tau",
		ScriptName:    "train.py",
		Script:        []byte("print('x')\n"),
		Workers:       1,
		GPUsPerWorker: 1,
		Env:           map[string]string{"TAU_DATA_DIR": "/tmp"},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved env error, got %v", err)
	}
}

func TestRenderAcceptsRetryEnvVars(t *testing.T) {
	for _, k := range []string{"TAU_RESUME_FROM", "TAU_RETRY_ATTEMPT", "TAU_RETRY_MAX", "TAU_RETRY_REASON"} {
		_, err := Render(Options{
			Name: "r1", Namespace: "tau", ScriptName: "train.py",
			Script: []byte("print('x')\n"), Workers: 1, GPUsPerWorker: 1,
			Env: map[string]string{k: "test-value"},
		})
		if err != nil {
			t.Errorf("TAU env var %s should be allowed but got: %v", k, err)
		}
	}
}

// tauEnvContractCases mirrors the Job renderer's list. The permitted keys are
// derived from runconfig rather than retyped, so a key added there is covered
// here without an edit; the fixed extras cover keys the old denylist named, a
// key no list mentions, a lowercase spelling, and names that only look
// Tau-prefixed. No expected verdict is written down -- it is read from
// runconfig, so this list cannot encode a second opinion.
var tauEnvContractCases = append(slices.Clone(runconfig.TauEnvAllowed),
	"TAU_DIST_BACKEND",
	"TAU_WORLD_SIZE",
	"TAU_EXPERIMENT",
	"TAU_SOME_FUTURE_KEY",
	"tau_resume_from",
	"tau_experiment",
	"MY_TAU_VAR",
	"TAUX_THING",
	"MY_VAR",
)

// The Job renderer runs the same assertions over the same cases. Both read the
// verdict from runconfig rather than restating it, so the two renderers can no
// longer drift into "this env var works for a Job but not a RayJob" -- which
// this renderer already did for lowercase spellings, before the gates were
// unified.
func TestRenderTauEnvGateAgreesWithRunconfig(t *testing.T) {
	for _, name := range tauEnvContractCases {
		t.Run(name, func(t *testing.T) {
			_, err := Render(Options{
				Name: "r1", Namespace: "tau", ScriptName: "train.py",
				Script: []byte("print('x')\n"), Workers: 1, GPUsPerWorker: 1,
				Env: map[string]string{name: "v"},
			})
			wantReserved := runconfig.ReservedTauEnvKey(name)
			if wantReserved && err == nil {
				t.Fatalf("Env %q: runconfig reserves it but Render accepted it", name)
			}
			if !wantReserved && err != nil {
				t.Fatalf("Env %q: runconfig permits it but Render rejected it: %v", name, err)
			}
		})
	}
}

func TestRenderTauEnvSecretGateAgreesWithRunconfig(t *testing.T) {
	for _, name := range tauEnvContractCases {
		t.Run(name, func(t *testing.T) {
			_, err := Render(Options{
				Name: "r1", Namespace: "tau", ScriptName: "train.py",
				Script: []byte("print('x')\n"), Workers: 1, GPUsPerWorker: 1,
				EnvSecrets: []envspec.Var{envspec.Secret(name, "s", "k")},
			})
			wantReserved := runconfig.ReservedTauEnvKey(name)
			if wantReserved && err == nil {
				t.Fatalf("EnvSecrets %q: runconfig reserves it but Render accepted it", name)
			}
			if !wantReserved && err != nil {
				t.Fatalf("EnvSecrets %q: runconfig permits it but Render rejected it: %v", name, err)
			}
		})
	}
}

func TestRenderRejectsKubeRayNameTooLong(t *testing.T) {
	_, err := Render(Options{
		Name:          strings.Repeat("a", maxRayJobResourceNameLen+1),
		Namespace:     "tau",
		ScriptName:    "train.py",
		Script:        []byte("print('x')\n"),
		Workers:       1,
		GPUsPerWorker: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "KubeRay limit is 47") {
		t.Fatalf("expected KubeRay name length error, got %v", err)
	}
}

func TestRenderUsesCPUDefaultImageForCPUOnlyJobs(t *testing.T) {
	out, err := Render(Options{
		Name:          "cpu-prep",
		Namespace:     "tau",
		ScriptName:    "prepare.py",
		Script:        []byte("print('prep')\n"),
		Workers:       1,
		GPUsPerWorker: 0,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	pod := head["template"].(map[string]any)["spec"].(map[string]any)
	container := pod["containers"].([]any)[0].(map[string]any)
	if container["image"] != DefaultCPUImage {
		t.Fatalf("default CPU image=%v want %s", container["image"], DefaultCPUImage)
	}
	sidecar := containerByName(t, pod["containers"].([]any), raylogoffload.SidecarContainerName)
	if sidecar["image"] != DefaultCPUImage {
		t.Fatalf("cpu sidecar image=%v want %s", sidecar["image"], DefaultCPUImage)
	}
	annotations := head["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations[raylogoffload.AnnotationKey] != raylogoffload.AnnotationValue {
		t.Fatalf("cpu head log offload annotation missing: %v", annotations)
	}
}

func TestRenderUsesExplicitCPUResourceOverrides(t *testing.T) {
	out, err := Render(Options{
		Name:          "cpu-kind",
		Namespace:     "tau",
		ScriptName:    "prepare.py",
		Script:        []byte("print('prep')\n"),
		Workers:       2,
		GPUsPerWorker: 0,
		Resources: Resources{
			CPURequest:    "500m",
			MemoryRequest: "1Gi",
			CPULimit:      "1",
			MemoryLimit:   "2Gi",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	headPod := head["template"].(map[string]any)["spec"].(map[string]any)
	headContainer := headPod["containers"].([]any)[0].(map[string]any)
	if got := asYAML(t, headContainer["resources"]); !strings.Contains(got, "cpu: 500m") || !strings.Contains(got, "memory: 1Gi") || !strings.Contains(got, "cpu: \"1\"") || !strings.Contains(got, "memory: 2Gi") {
		t.Fatalf("head resources missing overrides:\n%s", got)
	}
	worker := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
	workerPod := worker["template"].(map[string]any)["spec"].(map[string]any)
	workerContainer := workerPod["containers"].([]any)[0].(map[string]any)
	if got := asYAML(t, workerContainer["resources"]); !strings.Contains(got, "cpu: 500m") || !strings.Contains(got, "memory: 1Gi") || !strings.Contains(got, "cpu: \"1\"") || !strings.Contains(got, "memory: 2Gi") {
		t.Fatalf("worker resources missing overrides:\n%s", got)
	}
}

func TestRenderRequestOnlyResourceOverridesSetMatchingLimits(t *testing.T) {
	out, err := Render(Options{
		Name:          "cpu-kind",
		Namespace:     "tau",
		ScriptName:    "prepare.py",
		Script:        []byte("print('prep')\n"),
		Workers:       1,
		GPUsPerWorker: 0,
		Resources: Resources{
			MemoryRequest: "32Gi",
			Head: ResourceOverrides{
				CPURequest: "6",
			},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	headPod := head["template"].(map[string]any)["spec"].(map[string]any)
	headContainer := headPod["containers"].([]any)[0].(map[string]any)
	resources := headContainer["resources"].(map[string]any)
	requests := resources["requests"].(map[string]any)
	limits := resources["limits"].(map[string]any)
	if requests["memory"] != "32Gi" || limits["memory"] != "32Gi" {
		t.Fatalf("request-only memory override should set matching limit: requests=%v limits=%v", requests, limits)
	}
	if requests["cpu"] != "6" || limits["cpu"] != "6" {
		t.Fatalf("request-only head CPU override should set matching limit: requests=%v limits=%v", requests, limits)
	}
}

func TestRenderUsesRoleSpecificResourceOverrides(t *testing.T) {
	out, err := Render(Options{
		Name:          "cpu-kind",
		Namespace:     "tau",
		ScriptName:    "prepare.py",
		Script:        []byte("print('prep')\n"),
		Workers:       2,
		GPUsPerWorker: 0,
		Resources: Resources{
			Head: ResourceOverrides{
				CPURequest:    "1",
				MemoryRequest: "4Gi",
				CPULimit:      "1",
				MemoryLimit:   "4Gi",
			},
			Worker: ResourceOverrides{
				CPURequest:    "500m",
				MemoryRequest: "512Mi",
				CPULimit:      "1",
				MemoryLimit:   "1Gi",
			},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	headPod := head["template"].(map[string]any)["spec"].(map[string]any)
	headContainer := headPod["containers"].([]any)[0].(map[string]any)
	if got := asYAML(t, headContainer["resources"]); !strings.Contains(got, "memory: 4Gi") || strings.Contains(got, "512Mi") {
		t.Fatalf("head resources should use head overrides only:\n%s", got)
	}
	worker := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
	workerPod := worker["template"].(map[string]any)["spec"].(map[string]any)
	workerContainer := workerPod["containers"].([]any)[0].(map[string]any)
	if got := asYAML(t, workerContainer["resources"]); !strings.Contains(got, "memory: 512Mi") || !strings.Contains(got, "memory: 1Gi") || strings.Contains(got, "4Gi") {
		t.Fatalf("worker resources should use worker overrides only:\n%s", got)
	}
}

func TestParseRayVersionFromImage(t *testing.T) {
	tests := []struct {
		image string
		want  [2]int
	}{
		{"mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0", [2]int{2, 54}},
		{"mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0", [2]int{2, 54}},
		{"registry.example.com/taugrid/ray:py3.10-ray2.39.0-gpu-rdma-jammy-custom", [2]int{2, 39}},
		{"custom-registry.io/ray:py3.11-ray2.40.1-custom", [2]int{2, 40}},
		{"no-version-tag:latest", [2]int{2, 54}}, // falls back to RayVersion constant
		{"", [2]int{2, 54}},                      // empty falls back
	}
	for _, tt := range tests {
		got := parseRayVersion(tt.image)
		if got != tt.want {
			t.Errorf("parseRayVersion(%q) = %v, want %v", tt.image, got, tt.want)
		}
	}
}

func TestWorkerStartupProbeVersionConditional(t *testing.T) {
	tests := []struct {
		ver  [2]int
		want string
	}{
		{[2]int{2, 39}, "/api/local_raylet_healthz"},
		{[2]int{2, 40}, "/api/local_raylet_healthz"},
		{[2]int{2, 51}, "/api/local_raylet_healthz"},
		{[2]int{2, 52}, "/api/local_raylet_healthz"},
		{[2]int{2, 53}, "/api/healthz"},
		{[2]int{2, 54}, "/api/healthz"},
		{[2]int{3, 0}, "/api/healthz"},
	}
	for _, tt := range tests {
		probe := workerStartupProbe(tt.ver)
		got := probe["httpGet"].(map[string]any)["path"]
		if got != tt.want {
			t.Errorf("Ray %d.%d: got %v, want %v", tt.ver[0], tt.ver[1], got, tt.want)
		}
	}
}

func TestRenderGracePeriodDefaultIs600(t *testing.T) {
	out, err := Render(Options{
		Name:          "gp-test",
		Namespace:     "tau",
		ScriptName:    "train.py",
		Script:        []byte("print('train')\n"),
		Workers:       2,
		GPUsPerWorker: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)

	headPod := cluster["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	g := headPod["terminationGracePeriodSeconds"]
	gi, _ := g.(int)
	gi64, _ := g.(int64)
	if gi != 600 && gi64 != 600 {
		t.Errorf("head terminationGracePeriodSeconds=%v want 600", g)
	}

	workers := cluster["workerGroupSpecs"].([]any)
	workerPod := workers[0].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	g = workerPod["terminationGracePeriodSeconds"]
	gi, _ = g.(int)
	gi64, _ = g.(int64)
	if gi != 600 && gi64 != 600 {
		t.Errorf("worker terminationGracePeriodSeconds=%v want 600", g)
	}
}

func asYAML(t *testing.T, v any) string {
	t.Helper()
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func containerByName(t *testing.T, containers []any, name string) map[string]any {
	t.Helper()
	for _, container := range containers {
		m, ok := container.(map[string]any)
		if ok && m["name"] == name {
			return m
		}
	}
	t.Fatalf("container %q not found in %v", name, containers)
	return nil
}

func containerNames(t *testing.T, containers []any) string {
	t.Helper()
	var names []string
	for _, container := range containers {
		m, ok := container.(map[string]any)
		if !ok {
			t.Fatalf("container has unexpected type %T", container)
		}
		if name, _ := m["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}

// TestRenderIsSelfContained locks in the PR1 conversion: the rendered RayJob
// must not depend on any external ConfigMap for its driver script. Instead
// the payload is embedded and decoded at pod startup by a tau-payload
// initContainer into an emptyDir mounted at /script. Only the head template
// carries this wiring: the RayJob's entrypoint is submitted via the Ray Job
// Submission API and runs as the driver process on the head node, so the
// payload never needs to be duplicated onto worker templates.
func TestRenderIsSelfContained(t *testing.T) {
	script := []byte("print('hello from tau')\n")
	out, err := Render(Options{
		Name:          "self-contained",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        script,
		Workers:       2,
		GPUsPerWorker: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	if len(docs) != 1 {
		t.Fatalf("doc count=%d want 1; rendered output must not include a ConfigMap doc\n%s", len(docs), string(out))
	}
	rendered := string(out)
	if strings.Contains(rendered, "kind: ConfigMap") {
		t.Fatalf("rendered output must not contain a ConfigMap:\n%s", rendered)
	}

	rayjob := docs[0]
	meta := rayjob["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	digest, _ := annotations[payload.AnnotationDigest].(string)
	if digest == "" {
		t.Fatalf("expected %s annotation on RayJob metadata: %v", payload.AnnotationDigest, annotations)
	}
	if len(digest) != 64 { // hex-encoded sha256
		t.Fatalf("payload digest annotation does not look like a sha256 hex digest: %q", digest)
	}

	spec := rayjob["spec"].(map[string]any)
	cluster := spec["rayClusterSpec"].(map[string]any)

	if !strings.Contains(spec["entrypoint"].(string), "python3 /script/") {
		t.Fatalf("RayJob entrypoint must run the staged driver script (executed as the driver on the head node):\n%v", spec["entrypoint"])
	}

	checkHeadPod := func(tpl map[string]any) {
		t.Helper()
		pod := tpl["spec"].(map[string]any)

		volumes := pod["volumes"].([]any)
		var scriptVol map[string]any
		for _, v := range volumes {
			vm := v.(map[string]any)
			if vm["name"] == "script" {
				scriptVol = vm
			}
		}
		if scriptVol == nil {
			t.Fatalf("head: script volume missing: %v", volumes)
		}
		if _, hasConfigMap := scriptVol["configMap"]; hasConfigMap {
			t.Fatalf("head: script volume must not be backed by a ConfigMap: %v", scriptVol)
		}
		if _, hasEmptyDir := scriptVol["emptyDir"]; !hasEmptyDir {
			t.Fatalf("head: script volume must be an emptyDir: %v", scriptVol)
		}

		initContainers, _ := pod["initContainers"].([]any)
		if len(initContainers) != 2 {
			t.Fatalf("head: expected exactly two initContainers, got %d: %v", len(initContainers), initContainers)
		}
		prepare := containerByName(t, initContainers, raylogoffload.PrepareInitName)
		if got := asYAML(t, prepare["command"]); !strings.Contains(got, "chmod 1777 /tmp/ray") {
			t.Fatalf("head: prepare initContainer missing writable /tmp/ray contract:\n%s", got)
		}
		ic := containerByName(t, initContainers, payload.InitContainerName)
		if ic["name"] != payload.InitContainerName {
			t.Fatalf("head: initContainer name=%v want %s", ic["name"], payload.InitContainerName)
		}
		icEnv := asYAML(t, ic["env"])
		for _, want := range []string{payload.EnvB64, payload.EnvDigest, payload.EnvTargetDir} {
			if !strings.Contains(icEnv, want) {
				t.Fatalf("head: initContainer env missing %q:\n%s", want, icEnv)
			}
		}
		if !strings.Contains(icEnv, digest) {
			t.Fatalf("head: initContainer env does not carry the same digest as the metadata annotation:\n%s", icEnv)
		}
		icMounts := asYAML(t, ic["volumeMounts"])
		if !strings.Contains(icMounts, "name: script") || !strings.Contains(icMounts, "mountPath: /script") {
			t.Fatalf("head: initContainer must mount the script emptyDir at /script:\n%s", icMounts)
		}

		containers := pod["containers"].([]any)
		mainMounts := asYAML(t, containers[0].(map[string]any)["volumeMounts"])
		if !strings.Contains(mainMounts, "name: script") || !strings.Contains(mainMounts, "mountPath: /script") {
			t.Fatalf("head: main container must mount script at /script:\n%s", mainMounts)
		}
	}

	// checkWorkerPodHasNoPayload is the regression guard for review blocker
	// #2: the payload/initContainer/emptyDir/mount must exist on the head
	// template only. Workers never read the driver script from local disk
	// (they receive work from the driver over Ray's own RPC/object store),
	// so duplicating the payload there would only inflate the rendered
	// object's size for no benefit.
	checkWorkerPodHasNoPayload := func(tpl map[string]any) {
		t.Helper()
		pod := tpl["spec"].(map[string]any)

		if initContainers, _ := pod["initContainers"].([]any); len(initContainers) != 0 {
			t.Fatalf("worker: must not have any initContainers (payload is head-only): %v", initContainers)
		}

		volumes, _ := pod["volumes"].([]any)
		for _, v := range volumes {
			vm := v.(map[string]any)
			if vm["name"] == "script" {
				t.Fatalf("worker: must not have a script volume (payload is head-only): %v", vm)
			}
		}

		containers := pod["containers"].([]any)
		mainMounts, _ := containers[0].(map[string]any)["volumeMounts"].([]any)
		for _, m := range mainMounts {
			mm := m.(map[string]any)
			if mm["name"] == "script" || mm["mountPath"] == "/script" {
				t.Fatalf("worker: main container must not mount script (payload is head-only): %v", mm)
			}
		}
	}

	head := cluster["headGroupSpec"].(map[string]any)
	checkHeadPod(head["template"].(map[string]any))
	worker := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
	checkWorkerPodHasNoPayload(worker["template"].(map[string]any))
}

// TestRenderNeverSetsManagedBy is a regression guard: Kueue v0.18.1 owns
// manager-side defaulting of spec.managedBy for MultiKueue dispatch. Tau
// must never stamp this field itself, or it could conflict with (or bypass)
// Kueue's own admission/dispatch logic.
func TestRenderNeverSetsManagedBy(t *testing.T) {
	out, err := Render(Options{
		Name:          "managed-by-check",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("print('x')\n"),
		Workers:       1,
		GPUsPerWorker: 0,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	rayjob := docs[0]
	spec := rayjob["spec"].(map[string]any)
	if _, ok := spec["managedBy"]; ok {
		t.Fatalf("rendered RayJob must never set spec.managedBy (Kueue owns this field): %v", spec["managedBy"])
	}
	if strings.Contains(string(out), "managedBy") {
		t.Fatalf("rendered output must never mention managedBy:\n%s", string(out))
	}
}

// TestRenderRejectsScriptOverPayloadCap ensures an oversized driver script
// surfaces an actionable error instead of silently truncating or rendering
// an object that will fail admission later.
func TestRenderRejectsScriptOverPayloadCap(t *testing.T) {
	_, err := Render(Options{
		Name:          "too-big",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        bytes.Repeat([]byte("a"), payload.MaxDecodedBytes+1),
		Workers:       1,
		GPUsPerWorker: 0,
	})
	if err == nil {
		t.Fatal("expected error for script exceeding payload cap, got nil")
	}
	for _, want := range []string{"1024 KiB", "custom", "PVC"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should be actionable and mention %q, got: %v", want, err)
		}
	}
}

// TestRenderNearCapPayloadStaysUnderLastAppliedConfigLimit exercises the
// worst case for the embedded-payload design: a script whose encoded payload
// sits at the binding environment-entry budget, embedded once on the head
// template (workers carry no payload; see
// TestRenderIsSelfContained). The relevant constraint for PR1's client-side
// `kubectl apply` workflow is not raw etcd object size but the
// kubectl.kubernetes.io/last-applied-configuration annotation, which stores
// the fully-applied object as JSON. This test marshals the rendered RayJob
// to JSON (matching what client-side apply stores in that annotation, not
// the YAML wire format) and asserts it stays comfortably under Kubernetes'
// ~256 KiB annotation size ceiling, leaving headroom for the rest of the
// object's metadata/spec fields. Server-side apply (which doesn't have this
// annotation constraint) is out of scope for PR1.
func TestRenderNearCapPayloadStaysUnderLastAppliedConfigLimit(t *testing.T) {
	script := incompressibleScript(t, payload.MaxEnvEntryBytes)
	out, err := Render(Options{
		Name:          "near-cap",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        script,
		Workers:       2,
		GPUsPerWorker: 1,
		RuntimePip:    []string{"torch==2.4.0"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	if len(docs) != 1 {
		t.Fatalf("doc count=%d want 1", len(docs))
	}
	jsonBytes, err := json.Marshal(docs[0])
	if err != nil {
		t.Fatalf("marshal rendered RayJob as JSON: %v", err)
	}
	const lastAppliedConfigHeadroom = 200 * 1024 // leave margin under k8s' ~256 KiB last-applied-configuration annotation ceiling.
	if len(jsonBytes) > lastAppliedConfigHeadroom {
		t.Fatalf("rendered RayJob at payload cap serializes to %d JSON bytes, want <= %d (last-applied-configuration headroom)",
			len(jsonBytes), lastAppliedConfigHeadroom)
	}
	t.Logf("rendered RayJob JSON size for a %d-byte script at the env-entry budget (head-only): %d bytes (limit=%d, headroom=%d bytes)",
		len(script), len(jsonBytes), lastAppliedConfigHeadroom, lastAppliedConfigHeadroom-len(jsonBytes))
}

// incompressibleScript returns the largest deterministic, effectively
// incompressible script that still encodes within payload.MaxEnvEntryBytes.
// Payload envelopes are gzip-compressed, so a run of repeated bytes would
// shrink to nothing and could not exercise the worst case this test is about.
func incompressibleScript(t *testing.T, ceiling int) []byte {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	r := rand.New(rand.NewSource(1))
	blob := make([]byte, ceiling)
	for i := range blob {
		blob[i] = alphabet[r.Intn(len(alphabet))]
	}
	lo, hi := 1, len(blob)
	fits := func(n int) bool {
		_, _, err := payload.Encode(payload.New(map[string][]byte{"train.py": blob[:n]}))
		return err == nil
	}
	for lo < hi-1 {
		mid := (lo + hi) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return blob[:lo]
}

func TestRenderMIGUsesCorrectResourceName(t *testing.T) {
	out, err := Render(Options{
		Name:            "ray-mig",
		Namespace:       "ray",
		ScriptName:      "train.py",
		Script:          []byte("print('mig')\n"),
		Workers:         2,
		GPUsPerWorker:   1,
		GPUResourceMode: "mig",
		MIGProfile:      "1g.18gb",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if c := strings.Count(s, "nvidia.com/mig-1g.18gb"); c < 2 {
		t.Errorf("expected nvidia.com/mig-1g.18gb on both head and worker (got %d occurrences):\n%s", c, s)
	}
	if strings.Contains(s, `nvidia.com/gpu: `) {
		t.Errorf("rendered output should not contain nvidia.com/gpu resource request in MIG mode:\n%s", s)
	}
}

func TestRenderMIGRequiresExplicitMode(t *testing.T) {
	out, err := Render(Options{
		Name:          "ray-mig-no-mode",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("print('no-mode')\n"),
		Workers:       1,
		GPUsPerWorker: 1,
		MIGProfile:    "2g.35gb",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "nvidia.com/mig-") {
		t.Errorf("MIGProfile without explicit GPUResourceMode=mig should not produce MIG resource:\n%s", s)
	}
	if !strings.Contains(s, `nvidia.com/gpu`) {
		t.Errorf("expected nvidia.com/gpu when GPUResourceMode is not set:\n%s", s)
	}
}

func TestRenderMIGModeWithoutProfileErrors(t *testing.T) {
	_, err := Render(Options{
		Name:            "ray-mig-no-profile",
		Namespace:       "ray",
		ScriptName:      "train.py",
		Script:          []byte("print('fail')\n"),
		Workers:         1,
		GPUsPerWorker:   1,
		GPUResourceMode: "mig",
	})
	if err == nil {
		t.Fatal("expected error when GPUResourceMode=mig without MIGProfile")
	}
	if !strings.Contains(err.Error(), "mig-profile") {
		t.Errorf("error should mention --mig-profile, got: %v", err)
	}
}

func TestRenderMIGInvalidProfileFormat(t *testing.T) {
	_, err := Render(Options{
		Name:            "ray-mig-bad",
		Namespace:       "ray",
		ScriptName:      "train.py",
		Script:          []byte("print('bad')\n"),
		Workers:         1,
		GPUsPerWorker:   1,
		GPUResourceMode: "mig",
		MIGProfile:      "invalid-profile",
	})
	if err == nil {
		t.Fatal("expected error for invalid MIG profile format")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Errorf("error should mention format requirement, got: %v", err)
	}
}

func TestExecutionContractEnvVarsGPUMode(t *testing.T) {
	out, err := Render(Options{
		Name:          "exec-gpu",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("import ray\n"),
		Workers:       2,
		GPUsPerWorker: 4,
		DataPVC:       "data",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "TAU_DIST_BACKEND") || !strings.Contains(rendered, "nccl") {
		t.Fatalf("GPU mode should set TAU_DIST_BACKEND=nccl:\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_NUM_WORKERS") || !strings.Contains(rendered, `"8"`) {
		t.Fatalf("GPU mode should set TAU_NUM_WORKERS=8 (2*4):\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_WORLD_SIZE") {
		t.Fatalf("GPU mode should set TAU_WORLD_SIZE:\n%s", rendered)
	}
}

func TestExecutionContractEnvVarsCPUMode(t *testing.T) {
	out, err := Render(Options{
		Name:          "exec-cpu",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("import ray\n"),
		Workers:       3,
		GPUsPerWorker: 0,
		DataPVC:       "data",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "TAU_DIST_BACKEND") || !strings.Contains(rendered, "gloo") {
		t.Fatalf("CPU mode should set TAU_DIST_BACKEND=gloo:\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_NUM_WORKERS") || !strings.Contains(rendered, `"3"`) {
		t.Fatalf("CPU mode should set TAU_NUM_WORKERS=3:\n%s", rendered)
	}
}

func TestExecutionContractEnvVarsTuneMultiGPU(t *testing.T) {
	out, err := Render(Options{
		Name:           "tune-multigpu",
		Namespace:      "ray",
		ScriptName:     "train.py",
		Script:         []byte("import ray\n"),
		Workers:        2,
		GPUsPerWorker:  4,
		Launcher:       "ray-tune",
		TuneMetric:     "val_loss",
		TuneParamSpace: `{"lr":[0.001,0.01]}`,
		DataPVC:        "data",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "TAU_NUM_WORKERS") || !strings.Contains(rendered, `"8"`) {
		t.Fatalf("Tune multi-GPU should set TAU_NUM_WORKERS=8 (2*4), consistent with WORLD_SIZE:\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_WORLD_SIZE") || !strings.Contains(rendered, `"8"`) {
		t.Fatalf("Tune multi-GPU should set TAU_WORLD_SIZE=8 (2*4):\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_DIST_BACKEND") || !strings.Contains(rendered, "nccl") {
		t.Fatalf("Tune multi-GPU should set TAU_DIST_BACKEND=nccl:\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_TUNE_TRAIN_MODULE") {
		t.Fatalf("Tune should set TAU_TUNE_TRAIN_MODULE:\n%s", rendered)
	}
}

func TestEnvValidationRejectsNCCLKeys(t *testing.T) {
	_, err := Render(Options{
		Name:          "exec-nccl",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("import ray\n"),
		Workers:       1,
		GPUsPerWorker: 1,
		Env:           map[string]string{"NCCL_DEBUG": "INFO"},
	})
	if err == nil {
		t.Fatal("expected error for NCCL_ env key")
	}
	if !strings.Contains(err.Error(), "NCCL_DEBUG") {
		t.Fatalf("error should mention NCCL_DEBUG, got: %v", err)
	}
	if !strings.Contains(err.Error(), "allow_nccl_override") {
		t.Fatalf("error should mention escape hatch, got: %v", err)
	}
}

func TestAllowNCCLOverridePassesValidation(t *testing.T) {
	out, err := Render(Options{
		Name:              "nccl-override",
		Namespace:         "ray",
		ScriptName:        "train.py",
		Script:            []byte("import ray\n"),
		Workers:           2,
		GPUsPerWorker:     1,
		AllowNCCLOverride: true,
		Env:               map[string]string{"NCCL_DEBUG": "INFO", "NCCL_SOCKET_IFNAME": "ib0"},
	})
	if err != nil {
		t.Fatalf("AllowNCCLOverride=true should allow NCCL_ keys, got: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "NCCL_DEBUG") || !strings.Contains(rendered, "INFO") {
		t.Fatalf("rendered output should contain user-supplied NCCL_DEBUG:\n%s", rendered)
	}
}

func TestAllowNCCLOverrideStillRejectsTauKeys(t *testing.T) {
	_, err := Render(Options{
		Name:              "nccl-tau",
		Namespace:         "ray",
		ScriptName:        "train.py",
		Script:            []byte("import ray\n"),
		Workers:           1,
		GPUsPerWorker:     1,
		AllowNCCLOverride: true,
		Env:               map[string]string{"TAU_DIST_BACKEND": "gloo"},
	})
	if err == nil {
		t.Fatal("AllowNCCLOverride should not bypass TAU_* reserved keys")
	}
	if !strings.Contains(err.Error(), "TAU_DIST_BACKEND") {
		t.Fatalf("error should mention TAU_DIST_BACKEND, got: %v", err)
	}
}

func TestAllowNCCLOverrideEnvSecretsValidation(t *testing.T) {
	_, err := Render(Options{
		Name:              "nccl-secret",
		Namespace:         "ray",
		ScriptName:        "train.py",
		Script:            []byte("import ray\n"),
		Workers:           1,
		GPUsPerWorker:     1,
		AllowNCCLOverride: false,
		EnvSecrets:        []envspec.Var{envspec.Secret("NCCL_P2P_DISABLE", "nccl-secret", "key")},
	})
	if err == nil {
		t.Fatal("expected error for NCCL_ env secret without override")
	}
	if !strings.Contains(err.Error(), "NCCL_P2P_DISABLE") {
		t.Fatalf("error should mention NCCL_P2P_DISABLE, got: %v", err)
	}

	// With override enabled, should pass.
	_, err = Render(Options{
		Name:              "nccl-secret-ok",
		Namespace:         "ray",
		ScriptName:        "train.py",
		Script:            []byte("import ray\n"),
		Workers:           1,
		GPUsPerWorker:     1,
		AllowNCCLOverride: true,
		EnvSecrets:        []envspec.Var{envspec.Secret("NCCL_P2P_DISABLE", "nccl-secret", "key")},
	})
	if err != nil {
		t.Fatalf("AllowNCCLOverride=true should allow NCCL_ env secrets, got: %v", err)
	}
}

func TestRayTrainConfigRendersEnvJSON(t *testing.T) {
	out, err := Render(Options{
		Name:          "config-test",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("import ray\n"),
		Workers:       2,
		GPUsPerWorker: 1,
		RayTrainConfig: map[string]any{
			"failure_config": map[string]any{
				"max_failures": 3,
			},
			"torch_config": map[string]any{
				"timeout_s": 1800,
			},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "TAU_RAY_TRAIN_CONFIG_JSON") {
		t.Fatalf("rendered output should contain TAU_RAY_TRAIN_CONFIG_JSON:\n%s", rendered)
	}
	// Verify sorted keys: failure_config comes before torch_config.
	idx1 := strings.Index(rendered, "failure_config")
	idx2 := strings.Index(rendered, "torch_config")
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("rendered output should contain both config keys:\n%s", rendered)
	}
	if idx1 > idx2 {
		t.Fatalf("JSON keys should be sorted (failure_config before torch_config):\n%s", rendered)
	}
	// Verify max_failures value is present.
	if !strings.Contains(rendered, `"max_failures":3`) {
		t.Fatalf("rendered output should contain max_failures:3:\n%s", rendered)
	}
}

func TestRayTrainConfigSkippedForTuneLauncher(t *testing.T) {
	out, err := Render(Options{
		Name:          "tune-no-config",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("def train_func(config):\n    pass\n"),
		Launcher:      "ray-tune",
		TuneMetric:    "loss",
		Workers:       2,
		GPUsPerWorker: 1,
		RayTrainConfig: map[string]any{
			"failure_config": map[string]any{"max_failures": 3},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "TAU_RAY_TRAIN_CONFIG_JSON") {
		t.Fatalf("TAU_RAY_TRAIN_CONFIG_JSON should not be set for ray-tune launcher:\n%s", string(out))
	}
}

func TestRayTrainConfigEmptyMapOmitsEnv(t *testing.T) {
	out, err := Render(Options{
		Name:           "config-empty",
		Namespace:      "ray",
		ScriptName:     "train.py",
		Script:         []byte("import ray\n"),
		Workers:        1,
		GPUsPerWorker:  0,
		RayTrainConfig: map[string]any{},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "TAU_RAY_TRAIN_CONFIG_JSON") {
		t.Fatalf("empty RayTrainConfig should not emit TAU_RAY_TRAIN_CONFIG_JSON:\n%s", string(out))
	}
}

func TestEnvValidationRejectsMASTERKeys(t *testing.T) {
	_, err := Render(Options{
		Name:          "exec-master",
		Namespace:     "ray",
		ScriptName:    "train.py",
		Script:        []byte("import ray\n"),
		Workers:       1,
		GPUsPerWorker: 1,
		Env:           map[string]string{"MASTER_ADDR": "localhost"},
	})
	if err == nil {
		t.Fatal("expected error for MASTER_ADDR env key")
	}
	if !strings.Contains(err.Error(), "MASTER_ADDR") {
		t.Fatalf("error should mention MASTER_ADDR, got: %v", err)
	}
}

// --- working_dir project archive -------------------------------------------
//
// Ray resolves runtime_env working_dir independently on every node and
// prepends the unpacked tree to PYTHONPATH. These tests pin the three things
// that have to line up for that to work: a scheme-bearing URI (Ray's parse_uri
// rejects bare paths), the archive present on head AND workers, and an
// entrypoint invocation that does not depend on cwd.

func renderWithArchive(t *testing.T, o Options) map[string]any {
	t.Helper()
	raw, err := Render(o)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

func archiveOptions() Options {
	return Options{
		Name:           "wd",
		Namespace:      "ray",
		ScriptName:     "train.py",
		Script:         []byte("print('hi')\n"),
		ProjectArchive: []byte("PK\x03\x04 pretend zip"),
		Workers:        2,
		GPUsPerWorker:  0,
	}
}

func TestRenderProjectArchiveEmitsFileURIWorkingDir(t *testing.T) {
	doc := renderWithArchive(t, archiveOptions())
	spec := doc["spec"].(map[string]any)
	runtimeEnv, ok := spec["runtimeEnvYAML"].(string)
	if !ok {
		t.Fatalf("runtimeEnvYAML missing: %v", spec)
	}
	// A bare path fails server-side in Ray's parse_uri with "Invalid
	// protocol"; the file:// scheme is what makes it resolvable per node.
	if !strings.Contains(runtimeEnv, `working_dir: "file:///script/_tau_project.zip"`) {
		t.Fatalf("working_dir must be a file:// URI, got: %q", runtimeEnv)
	}
}

func TestRenderProjectArchiveShipsPayloadToWorkersToo(t *testing.T) {
	doc := renderWithArchive(t, archiveOptions())
	cluster := doc["spec"].(map[string]any)["rayClusterSpec"].(map[string]any)

	worker := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
	pod := worker["template"].(map[string]any)["spec"].(map[string]any)

	inits, ok := pod["initContainers"].([]any)
	if !ok || len(inits) == 0 {
		t.Fatal("workers need the payload initContainer: Ray resolves file:// working_dir on each node separately")
	}
	if name := inits[0].(map[string]any)["name"].(string); name != payload.InitContainerName {
		t.Fatalf("unexpected worker initContainer %q", name)
	}

	// The runtime_env agent runs inside the ray container, so the archive has
	// to be mounted there, not only in the initContainer.
	container := pod["containers"].([]any)[0].(map[string]any)
	if !hasMount(container, "script", "/script") {
		t.Fatalf("worker ray container must mount the archive: %v", container["volumeMounts"])
	}
	if !hasVolume(pod, "script") {
		t.Fatalf("worker pod missing script volume: %v", pod["volumes"])
	}
}

func TestRenderWithoutProjectArchiveLeavesWorkersUntouched(t *testing.T) {
	o := archiveOptions()
	o.ProjectArchive = nil
	doc := renderWithArchive(t, o)
	cluster := doc["spec"].(map[string]any)["rayClusterSpec"].(map[string]any)
	worker := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
	pod := worker["template"].(map[string]any)["spec"].(map[string]any)

	if _, ok := pod["initContainers"]; ok {
		t.Fatal("single-file runs must not gain worker initContainers")
	}
	if hasVolume(pod, "script") {
		t.Fatal("single-file runs must not gain a worker script volume")
	}
	spec := doc["spec"].(map[string]any)
	if env, ok := spec["runtimeEnvYAML"].(string); ok && strings.Contains(env, "working_dir") {
		t.Fatalf("single-file runs must not set working_dir, got %q", env)
	}
}

func TestRenderProjectArchiveRunsEntrypointAsModule(t *testing.T) {
	o := archiveOptions()
	o.ScriptName = "pkg/train.py"
	doc := renderWithArchive(t, o)
	entry := doc["spec"].(map[string]any)["entrypoint"].(string)
	// -m resolves through PYTHONPATH, which Ray sets to the unpacked
	// working_dir, so it survives the entrypoint's own `cd /data`.
	if !strings.Contains(entry, "python3 -m pkg.train") {
		t.Fatalf("entrypoint should run the archived module, got: %q", entry)
	}
	if strings.Contains(entry, "python3 /script/pkg/train.py") {
		t.Fatal("entrypoint must not reference a loose script path in working_dir mode")
	}
}

func TestRenderProjectArchiveRejectsEntrypointOutsideProject(t *testing.T) {
	o := archiveOptions()
	o.ScriptName = "../outside.py"
	if _, err := Render(o); err == nil {
		t.Fatal("want error for an entrypoint outside the project archive")
	}
}

func TestRenderProjectArchiveDoesNotShipLooseScriptCopy(t *testing.T) {
	o := archiveOptions()
	// Incompressible, so the embedded payload dominates the rendered size and
	// the comparison actually measures the duplicate copy.
	o.Script = incompressibleScript(t, 40<<10)
	withArchive, err := Render(o)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	o.ProjectArchive = nil
	withoutArchive, err := Render(o)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The archive already contains the entrypoint; embedding the loose copy
	// as well would double the payload for nothing.
	if len(withArchive) >= len(withoutArchive) {
		t.Fatalf("working_dir render (%d bytes) should not carry a duplicate script copy (%d bytes)",
			len(withArchive), len(withoutArchive))
	}
}

func hasMount(container map[string]any, name, path string) bool {
	mounts, _ := container["volumeMounts"].([]any)
	for _, m := range mounts {
		mm := m.(map[string]any)
		if mm["name"] == name && mm["mountPath"] == path {
			return true
		}
	}
	return false
}

func hasVolume(pod map[string]any, name string) bool {
	vols, _ := pod["volumes"].([]any)
	for _, v := range vols {
		if v.(map[string]any)["name"] == name {
			return true
		}
	}
	return false
}

// TestRenderMaxProjectArchiveStaysUnderLastAppliedConfigLimit pins the
// constraint that actually bounds working_dir. Because Ray resolves a file://
// working_dir on every node, the archive is embedded in the head template and
// in each worker template, so it counts more than once against the
// kubectl.kubernetes.io/last-applied-configuration annotation (~256 KiB).
// Without MaxProjectArchiveBytes a project sized to the env-entry budget
// rendered to ~215 KiB of JSON and blew the 200 KiB working budget.
func TestRenderMaxProjectArchiveStaysUnderLastAppliedConfigLimit(t *testing.T) {
	archive := incompressibleScript(t, MaxProjectArchiveBytes)
	out, err := Render(Options{
		Name:           "max-archive",
		Namespace:      "ray",
		ScriptName:     "train.py",
		Script:         []byte("print('hi')\n"),
		ProjectArchive: archive,
		Workers:        2,
		GPUsPerWorker:  1,
		RuntimePip:     []string{"torch==2.4.0"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeDocs(t, out)
	jsonBytes, err := json.Marshal(docs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const lastAppliedConfigHeadroom = 200 * 1024
	if len(jsonBytes) > lastAppliedConfigHeadroom {
		t.Fatalf("rendered RayJob with a max-size project archive serializes to %d JSON bytes, want <= %d",
			len(jsonBytes), lastAppliedConfigHeadroom)
	}
	t.Logf("max project archive (%d bytes) renders to %d JSON bytes (limit=%d, headroom=%d)",
		len(archive), len(jsonBytes), lastAppliedConfigHeadroom, lastAppliedConfigHeadroom-len(jsonBytes))
}

// On the canonical Ray CUDA image a plain `pip install` fails with EACCES (pip
// cannot write to the root-owned site-packages/nvidia) and runtime.pip is
// silently unusable. The entrypoint must fall back to the user site so
// researchers do not have to discover PIP_USER=1 for themselves. On the plain
// non-CUDA image the install succeeds and the fallback never fires.
func TestEntrypointFallsBackToUserSiteWhenPipInstallIsNotPermitted(t *testing.T) {
	got, err := entrypoint(Options{
		ScriptName: "train.py",
		RuntimePip: []string{"torch>=2.4.0"},
	})
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}
	want := "python3 -m pip install --quiet --no-cache-dir 'torch>=2.4.0'" +
		" || python3 -m pip install --quiet --no-cache-dir --user 'torch>=2.4.0'"
	if !strings.Contains(got, want) {
		t.Fatalf("runtime.pip install has no user-site fallback.\nwant substring: %s\ngot:\n%s", want, got)
	}
	if !strings.Contains(got, "-r /script/requirements.txt || python3 -m pip install --quiet --no-cache-dir --user -r /script/requirements.txt") {
		t.Fatalf("requirements.txt install has no user-site fallback.\ngot:\n%s", got)
	}
}

// Same contract as the managed-workflow templates: the --user fallback must be
// followed by a PATH export, or console scripts installed by pip are unusable.
func TestEntrypointPutsUserSiteBinOnPath(t *testing.T) {
	got, err := entrypoint(Options{
		ScriptName: "train.py",
		RuntimePip: []string{"torch>=2.4.0"},
	})
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}
	if !strings.Contains(got, "--user") {
		t.Fatalf("expected a --user pip fallback in the entrypoint")
	}
	if !strings.Contains(got, `python3 -m site --user-base`) {
		t.Errorf("entrypoint does not put the user base bin dir on PATH; "+
			"console scripts from a --user install would not be runnable.\ngot:\n%s", got)
	}
}

// A checkpoint written to an emptyDir-backed /data is deleted with the pod, so
// `tau serve deploy --from-finetune` would resolve a model that no longer
// exists. Refuse the render instead of producing that silent data loss.
func TestRenderRejectsCheckpointArtifactWithoutDurablePVC(t *testing.T) {
	base := func() Options {
		return Options{
			Name:               "ray-ckpt",
			Namespace:          "ray",
			ServiceAccountName: "tau-workload",
			ScriptName:         "train.py",
			Script:             []byte("from ray.train.torch import TorchTrainer\n"),
			Workers:            1,
			GPUsPerWorker:      1,
			CheckpointArtifact: "last.safetensors",
			TopologyOptions: topology.Options{
				Team:      "research",
				Lane:      "training",
				QueueName: "team-a",
				Placement: "independent",
				GPUClass:  "any",
			},
		}
	}

	t.Run("without a data PVC the render is refused", func(t *testing.T) {
		if _, err := Render(base()); err == nil {
			t.Fatal("expected an error when storage.checkpoint is set without a durable PVC, got nil")
		} else if !strings.Contains(err.Error(), "storage.checkpoint requires") {
			t.Fatalf("expected a checkpoint/PVC diagnostic, got: %v", err)
		}
	})

	t.Run("with a data PVC the render succeeds", func(t *testing.T) {
		o := base()
		o.DataPVC = "blob-training"
		if _, err := Render(o); err != nil {
			t.Fatalf("render with a durable PVC should succeed, got: %v", err)
		}
	})
}
