package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// runManagedWorkflowDispatch executes the managed workflow dispatch the way
// `tau run --config` does: typed options in, rendered output out. There is no
// intermediate cobra command and no argv.
func runManagedWorkflowDispatch(t *testing.T, o runDispatchOptions) (string, string, error) {
	t.Helper()
	// In production the namespace arrives already resolved from the connected
	// workspace (applyWorkspaceDefaults / the workspace connection descriptor).
	// These tests call the executor directly, so stand in for that here rather
	// than making every case repeat it. Cases that exercise namespace
	// resolution itself set o.namespace or a preset explicitly.
	if strings.TrimSpace(o.namespace) == "" && o.preset == "" {
		o.namespace = "test-workspace"
	}
	request, err := newRunManagedWorkflowRequest(o)
	if err != nil {
		return "", "", err
	}
	var out, stderr bytes.Buffer
	err = executeRunManagedWorkflow(context.Background(), &out, &stderr, &request, "tau run --config "+o.file)
	return out.String(), stderr.String(), err
}

func TestLoadExtraScripts(t *testing.T) {
	dir := t.TempDir()
	lossPath := filepath.Join(dir, "my_loss.py")
	kernelPath := filepath.Join(dir, "storm_kernel.cu")
	if err := os.WriteFile(lossPath, []byte("def loss():\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(kernelPath, []byte("__global__ void k() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extras, err := loadExtraScripts([]string{
		lossPath,
		kernelPath + ":kernel.cu",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(extras) != 2 {
		t.Fatalf("len(extras)=%d want 2", len(extras))
	}
	if extras[0].Name != "my_loss.py" || !strings.Contains(string(extras[0].Data), "def loss") {
		t.Fatalf("unexpected first extra: %+v", extras[0])
	}
	if extras[1].Name != "kernel.cu" || !strings.Contains(string(extras[1].Data), "__global__") {
		t.Fatalf("unexpected second extra: %+v", extras[1])
	}
}

func TestManagedWorkflowSubmitSecretPayloadDryRunRedactsValue(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
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
	mainScript := writeMainScript(t)
	payloadPath := filepath.Join(t.TempDir(), "secret.json")
	payload, err := json.Marshal(map[string]any{
		"name":       "tau-secret-job-secrets",
		"stringData": map[string]string{"HF_TOKEN": "fake-token-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.secretPayloadPath = payloadPath
	o.workspace = "sample"
	o.workspaceResultScope = "/data/projects/sample/runs"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kind: Secret",
		"name: tau-secret-job-secrets",
		`HF_TOKEN: "<redacted>"`,
		"secretKeyRef:",
		`key: "<redacted>"`,
		`kueue.x-k8s.io/queue-name: jobqueue`,
		workloadmeta.LabelManagedBy + `: "tau"`,
		workloadmeta.LabelRunID + `: "tau-secret-job"`,
		workloadmeta.LabelWorkloadKind + `: "job"`,
		workloadmeta.LabelWorkspace + `: "sample"`,
		workloadmeta.AnnotationWorkspaceID + `: "sample"`,
		workloadmeta.AnnotationResultScope + `: "/data/projects/sample/runs"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("dry-run manifest missing %q:\n%s", want, rendered)
		}
	}
	for _, leaked := range []string{"fake-token-value", "key: HF_TOKEN"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("dry-run leaked secret material %q:\n%s", leaked, rendered)
		}
	}
}

func TestExecuteRunTargetWritesBackResolvedManagedWorkflowNamespace(t *testing.T) {
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: managed-retry
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
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
      namespace: managed-retry-ns
`), 0o644); err != nil {
		t.Fatal(err)
	}
	options := defaultRunDispatchOptions()
	options.file = manifestPath
	options.mainScript = writeMainScript(t)
	options.workloadKind = "job"
	options.preset = "test.training"
	options.topologyPolicy = policy
	options.dryRun = "client"

	target, err := resolveRunTarget(options, "")
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
	if got := target.managedWorkflow.Options.namespace; got != "managed-retry-ns" {
		t.Fatalf("resolved managed workflow namespace = %q, want %q", got, "managed-retry-ns")
	}
}

func TestManagedWorkflowRenderedLabelsSatisfyWorkloadIdentityContract(t *testing.T) {
	cases := []struct {
		workloadKind string
		resourceKind string
		raw          string
	}{
		{
			workloadKind: "job",
			resourceKind: "Job",
			raw: `
schema_version: 1
name: vap-job
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`,
		},
		{
			workloadKind: "rayjob",
			resourceKind: "RayJob",
			raw: `
schema_version: 1
name: vap-rayjob
compute: { gpus: 1, workers: 2 }
runtime:
  pip:
    - torch==2.4.0
`,
		},
		{
			workloadKind: "rayjob-eval",
			resourceKind: "RayJob",
			raw: `
schema_version: 1
name: vap-rayjob-eval
eval:
  cpu_workers: 2
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.workloadKind, func(t *testing.T) {
			manifestPath := writeFinetuneManifest(t, tc.raw)
			o := defaultRunDispatchOptions()
			o.file = manifestPath
			o.mainScript = writeMainScript(t)
			o.workloadKind = tc.workloadKind
			o.workspace = "vap-workspace"
			o.workspaceResultScope = "/data/projects/vap-workspace/runs"
			o.dryRun = "client"
			out, stderr, err := runManagedWorkflowDispatch(t, o)
			if err != nil {
				t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
			}

			var workload struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(out), &workload); err != nil {
				t.Fatalf("yaml.Unmarshal rendered workload: %v\n%s", err, out)
			}
			if workload.Kind != tc.resourceKind {
				t.Fatalf("rendered kind = %q, want %q", workload.Kind, tc.resourceKind)
			}
			if !satisfiesWorkloadIdentityContract(workload.Kind, workload.Metadata.Labels) {
				t.Fatalf("rendered %s labels do not satisfy the Tau workload identity contract: %#v",
					workload.Kind, workload.Metadata.Labels)
			}
			if got := workload.Metadata.Labels[workloadmeta.LabelWorkloadKind]; got != tc.workloadKind {
				t.Fatalf("metadata.labels[%q] = %q, want %q",
					workloadmeta.LabelWorkloadKind, got, tc.workloadKind)
			}

			for _, requiredKey := range []string{
				workloadmeta.LabelManagedBy,
				workloadmeta.LabelRunID,
				workloadmeta.LabelWorkloadKind,
			} {
				missing := make(map[string]string, len(workload.Metadata.Labels))
				for key, value := range workload.Metadata.Labels {
					missing[key] = value
				}
				delete(missing, requiredKey)
				if satisfiesWorkloadIdentityContract(workload.Kind, missing) {
					t.Fatalf("identity contract unexpectedly accepted %s without %q", workload.Kind, requiredKey)
				}
			}
		})
	}
}

// satisfiesWorkloadIdentityContract checks the labels shared by Tau renderers
// and lifecycle consumers.
func satisfiesWorkloadIdentityContract(resourceKind string, labels map[string]string) bool {
	if labels[workloadmeta.LabelManagedBy] != workloadmeta.ManagedByValue ||
		labels[workloadmeta.LabelRunID] == "" {
		return false
	}
	switch resourceKind {
	case "Job":
		return labels[workloadmeta.LabelWorkloadKind] == "job"
	case "RayJob":
		kind := labels[workloadmeta.LabelWorkloadKind]
		return kind == "rayjob" || kind == "rayjob-eval"
	default:
		return false
	}
}

// TestLoadExtraScriptsAcceptsWindowsDriveLetterPath verifies that an SRC
// path containing a Windows drive-letter colon (e.g. `C:\...\foo.py`) is
// preserved when followed by `:DEST`. The old strings.Cut split on the
// drive-letter colon and treated the rest of the path as DEST, which broke
// every tau-py submit on Windows because the SDK passes
// `str(staged_user) + ":" + user_filename`. On Linux this test still exercises
// the LastIndex split path: the spec has a single colon, so old and new code
// behave identically.
func TestLoadExtraScriptsAcceptsWindowsDriveLetterPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "user_module.py")
	if err := os.WriteFile(src, []byte("# user module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := src + ":user_module.py"
	extras, err := loadExtraScripts([]string{spec})
	if err != nil {
		t.Fatalf("loadExtraScripts(%q): %v", spec, err)
	}
	if len(extras) != 1 {
		t.Fatalf("len(extras)=%d want 1", len(extras))
	}
	if extras[0].Name != "user_module.py" {
		t.Fatalf("extras[0].Name=%q want %q", extras[0].Name, "user_module.py")
	}
	if !strings.Contains(string(extras[0].Data), "user module") {
		t.Fatalf("extras[0].Data=%q missing payload", string(extras[0].Data))
	}
}

// TestSplitExtraScriptSpecLastColon directly verifies the helper splits on
// the LAST ':' AFTER stripping any drive letter so Windows paths survive
// in both `SRC` and `SRC:DEST` forms.
func TestSplitExtraScriptSpecLastColon(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		wantSrc  string
		wantDest string
	}{
		{"posix-no-dest", "/tmp/foo.py", "/tmp/foo.py", ""},
		{"posix-with-dest", "/tmp/foo.py:bar.py", "/tmp/foo.py", "bar.py"},
		{"windows-drive-with-dest", `C:\Users\foo\bar.py:bar.py`, `C:\Users\foo\bar.py`, "bar.py"},
		{"empty-spec", "", "", ""},
	}
	// Windows-only cases — drive-letter survival when DEST is absent and
	// when an extended-length prefix is used. On POSIX, filepath.VolumeName
	// returns "" and the spec gets parsed as SRC:DEST, which is the wrong
	// expected value, so gate these.
	if runtime.GOOS == "windows" {
		cases = append(cases, struct {
			name     string
			spec     string
			wantSrc  string
			wantDest string
		}{"windows-drive-no-dest", `C:\Users\foo\bar.py`, `C:\Users\foo\bar.py`, ""})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dest := splitExtraScriptSpec(tc.spec)
			if src != tc.wantSrc || dest != tc.wantDest {
				t.Fatalf("splitExtraScriptSpec(%q) = (%q, %q); want (%q, %q)", tc.spec, src, dest, tc.wantSrc, tc.wantDest)
			}
		})
	}
}

func TestLoadExtraScriptsRejectsBadSpecs(t *testing.T) {
	dir := t.TempDir()
	lossPath := filepath.Join(dir, "my_loss.py")
	if err := os.WriteFile(lossPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		spec string
		want string
	}{
		{"", "source path"},
		{lossPath + ":subdir/my_loss.py", "single filename"},
		{lossPath + ":" + string(filepath.Separator), "single filename"},
		{dir, "directories are not supported"},
	}
	for _, tc := range cases {
		_, err := loadExtraScripts([]string{tc.spec})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("loadExtraScripts(%q) error=%v, want %q", tc.spec, err, tc.want)
		}
	}
}

func TestLoadExtraScriptsRejectsDuplicateDest(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.py")
	b := filepath.Join(dir, "b.py")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadExtraScripts([]string{a + ":x.py", b + ":x.py"})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestManagedWorkflowSubmitPresetDryRunRendersTopology(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: preset-smoke
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.training.l"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: jobqueue",
		`kueue.x-k8s.io/podset-unconstrained-topology: "true"`,
		`nvidia.com/gpu: "1"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("dry-run manifest missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"kueue.x-k8s.io/podset-required-topology",
		"kueue.x-k8s.io/podset-preferred-topology",
		workloadmeta.AnnotationResourceFlavor + ":",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("device-plugin preset should not render Kueue TAS annotation %q", forbidden)
		}
	}
}

func TestManagedWorkflowSubmitDRAPresetUsesDRAQueueAndManagedSeries(t *testing.T) {
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: dra-h200
compute: { gpus: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifestPath
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.preset = "azure.research.large-memory.2x"
	o.gpuResourceMode = "dra"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow DRA submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: jobqueue-dra",
		"resourceClaimTemplateName: ds-2gpus",
		`kueue.azure.com/gpu-series: "nd-h200-v5"`,
		workloadmeta.AnnotationResourceFlavor + `: "nd-h200-v5-dra"`,
		workloadmeta.AnnotationClusterQueue + `: "tau-dra-cq"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("DRA dry-run manifest missing %q:\n%s", want, rendered)
		}
	}
	for _, forbidden := range []string{
		`nvidia.com/gpu: "2"`,
		"kueue.x-k8s.io/podset-unconstrained-topology",
		"kueue.x-k8s.io/podset-required-topology",
		"kueue.x-k8s.io/podset-preferred-topology",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("DRA dry-run manifest should not render %q", forbidden)
		}
	}
}

func TestManagedWorkflowSubmitDevicePluginQueueNodeSelectorDryRun(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: flex-a100
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.gpuResourceMode = "device-plugin"
	o.namespace = "e2e-stack"
	o.queue = "e2e-stack-large-gpu-queue"
	o.nodeSelectors = append(o.nodeSelectors, "gpu=a100")
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kind: RayJob",
		"namespace: e2e-stack",
		"kueue.x-k8s.io/queue-name: e2e-stack-large-gpu-queue",
		`nvidia.com/gpu: "1"`,
		"nodeSelector:",
		"gpu: \"a100\"",
		"suspend: true",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("device-plugin dry-run manifest missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"resourceClaimTemplateName:",
		"resourceClaims:",
		"claims:\n",
		"priorityClassName: \"taugrid-default\"",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("device-plugin dry-run manifest should not render %q:\n%s", forbidden, rendered)
		}
	}
	if strings.Contains(stderr, "inferred preset") {
		t.Errorf("explicit queue device-plugin submit should not require/infer a topology preset; stderr:\n%s", stderr)
	}
}

func TestManagedWorkflowSubmitDataPVCOverrideDryRun(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: vision-probe
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.namespace = "ray"
	o.queue = "research-queue"
	o.nodeSelectors = append(o.nodeSelectors, "kubernetes.azure.com/agentpool=a10")
	o.dataPVC = "lustre-research"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kind: RayJob",
		"namespace: ray",
		"kueue.x-k8s.io/queue-name: research-queue",
		"kubernetes.azure.com/agentpool: \"a10\"",
		"persistentVolumeClaim: { claimName: lustre-research }",
		// The manifest's storage.data_pvc override is no longer visible as
		// plaintext (the embedded manifest copy is base64-encoded inside
		// the manifest payload initContainer's env, per #869 PR2). The real
		// volume claim asserted above is the evidence the override
		// propagated end-to-end.
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("data PVC dry-run manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestManagedWorkflowSubmitRayJobProfilerDryRun(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: vision-profile
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
storage:
  data_pvc: taugrid-datasets
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.namespace = "ray"
	o.queue = "dev"
	o.profiler = "nsys"
	o.profileRank = "0,8"
	o.profileWarmup = "30s"
	o.profileDuration = "2m"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit profile dry-run: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kind: RayJob",
		"namespace: ray",
		"name: \"TAU_PROFILE_OUT_PATTERN\"",
		"value: \"/data/checkpoints/finetunes/vision-profile/profile/rank-<rank>.nsys-rep\"",
		"name: \"TAU_PROFILE_ACTIVE_SEC\"",
		"value: \"120\"",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("profile dry-run missing %q:\n%s", want, rendered)
		}
	}
}

func TestManagedWorkflowSubmitMetricsOffloadSidecarDryRun(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
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
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.namespace = "ray"
	o.queue = "dev"
	o.dryRun = "client"
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "registry.example.com/taugrid/tau:20260618.1")
	t.Setenv("TAU_METRICS_OFFLOAD_PROJECT", "vit-enc-vision")
	t.Setenv("TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT", "http://${NODE_IP}:3100/receive")
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit metrics offload dry-run: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"name: metrics-offload",
		"image: \"registry.example.com/taugrid/tau:20260618.1\"",
		"name: TAU_METRICS_HISTORY",
		"value: \"/data/checkpoints/finetunes/vision-demo/metrics-history.jsonl\"",
		`command: ["/usr/local/bin/taugrid-portal"]`,
		`args: ["experiment", "offload", "metrics", "--watch", "--done-file", "/data/checkpoints/finetunes/vision-demo/metrics-done.json"]`,
		"name: TAU_EXP_STORE",
		"value: \"/data/checkpoints/finetunes/vision-demo/metrics-expstore\"",
		"name: TAU_METRICS_OFFLOAD_PROJECT",
		"value: \"vit-enc-vision\"",
		"name: TAU_METRICS_OFFLOAD_GROUP",
		"name: TAU_METRICS_OFFLOAD_COMPLETION_FILE",
		"value: \"/data/checkpoints/finetunes/vision-demo/metrics-completion.json\"",
		"name: TAU_METRICS_OFFLOAD_REMOTE_WRITE_ENDPOINT",
		"value: \"http://${NODE_IP}:3100/receive\"",
		"name: \"TAU_GROUP\"",
		"value: \"demo-experiment\"",
		"name: \"TAU_EXPERIMENT\"",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("metrics offload dry-run missing %q:\n%s", want, rendered)
		}
	}
	var workload map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &workload); err != nil {
		t.Fatalf("yaml.Unmarshal rendered workload: %v\n%s", err, rendered)
	}
	containers := workload["spec"].(map[string]any)["rayClusterSpec"].(map[string]any)["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	stringSlice := func(value any) []string {
		items, ok := value.([]any)
		if !ok {
			t.Fatalf("expected []any, got %T", value)
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				t.Fatalf("expected string entry, got %T", item)
			}
			out = append(out, s)
		}
		return out
	}
	var metricsContainer map[string]any
	for _, container := range containers {
		if m, ok := container.(map[string]any); ok && m["name"] == "metrics-offload" {
			metricsContainer = m
			break
		}
	}
	if metricsContainer == nil {
		t.Fatalf("metrics offload sidecar missing from rendered output:\n%s", rendered)
	}
	commandText := strings.Join(stringSlice(metricsContainer["command"]), " ")
	argsText := strings.Join(stringSlice(metricsContainer["args"]), " ")
	if strings.Contains(commandText, "/bin/sh") || strings.Contains(commandText, "-lc") ||
		strings.Contains(argsText, "/bin/sh") || strings.Contains(argsText, "-lc") {
		t.Fatalf("metrics offload sidecar dry-run must not require a shell:\n%v", metricsContainer)
	}
}

func TestManagedWorkflowSubmitRejectsLatestMetricsOffloadImage(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: latest-sidecar
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.dryRun = "client"
	t.Setenv("TAU_METRICS_OFFLOAD_IMAGE", "registry.example.com/tau:latest")
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err == nil || !strings.Contains(err.Error(), "latest") {
		t.Fatalf("expected latest sidecar image rejection, got %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
}

func TestManagedWorkflowSubmitRejectsEvalPreset(t *testing.T) {
	// A normal finetune (no eval.cpu_workers set → workload-kind defaults
	// to job) using the eval-lane preset is misdispatched. Reject with a
	// clear error pointing at the right shape (rayjob-eval).
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: eval-preset
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.eval.gpu"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err == nil || !strings.Contains(err.Error(), "lane=eval") || !strings.Contains(err.Error(), "rayjob-eval") {
		t.Fatalf("expected eval-lane rejection pointing at rayjob-eval, got %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr)
	}
}

// TestManagedWorkflowSubmitRejectsTrainLaneOnEvalWorkload is the symmetric guard to
// TestManagedWorkflowSubmitRejectsEvalPreset: a train-lane preset (or --lane override)
// applied to a rayjob-eval workload would silently land the eval on the
// training Kueue queue with the wrong priority class. The validator must
// hard-fail before the manifest is rendered.
func TestManagedWorkflowSubmitRejectsTrainLaneOnEvalWorkload(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: misrouted-eval
eval:
  cpu_workers: 4
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	cases := []struct {
		name    string
		policy  func(*runDispatchOptions)
		wantSub string
	}{
		{
			name:    "preset-train-lane",
			policy:  func(o *runDispatchOptions) { o.preset = "azure.research.training.l" },
			wantSub: "lane=\"training\"",
		},
		{
			name:    "lane-flag-large-memory",
			policy:  func(o *runDispatchOptions) { o.team, o.lane = "research", "large-memory" },
			wantSub: "lane=\"large-memory\"",
		},
		{
			name:    "lane-flag-elastic",
			policy:  func(o *runDispatchOptions) { o.team, o.lane = "research", "elastic" },
			wantSub: "lane=\"elastic\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mainScript := writeMainScript(t)
			o := defaultRunDispatchOptions()
			o.file = manifest
			o.mainScript = mainScript
			o.workloadKind = "rayjob-eval"
			o.dryRun = "client"
			tc.policy(&o)
			out, stderr, err := runManagedWorkflowDispatch(t, o)
			if err == nil {
				t.Fatalf("expected train-lane on rayjob-eval to fail; stdout:\n%s\nstderr:\n%s", out, stderr)
			}
			if !strings.Contains(err.Error(), "rayjob-eval") || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error should mention rayjob-eval and %q; got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestManagedWorkflowSubmitRejectsPresetShapeMismatch(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: mismatch
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.training.xl"
	o.dryRun = "client"
	_, stderr, err := runManagedWorkflowDispatch(t, o)
	if err == nil || !strings.Contains(err.Error(), "shape \"8xgpu\" requests 8 GPU(s), but manifest compute.gpus=1") {
		t.Fatalf("expected shape mismatch rejection, got %v\nstderr:\n%s", err, stderr)
	}
}

func TestManagedWorkflowSubmitPresetQueueOverrideInfersTeam(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: queue-team-infer
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.training.l"
	o.queue = "sample-training"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit should infer team from queue override: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: sample-training",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("inferred queue/team render missing %q:\n%s", want, rendered)
		}
	}
	for _, want := range []string{"inferred --team=sample", "--queue=\"sample-training\""} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("queue/team inference warning missing %q: %s", want, stderr)
		}
	}
}

func TestManagedWorkflowSubmitRejectsExplicitQueueTeamConflict(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: queue-team-conflict
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.training.l"
	o.queue = "sample-training"
	o.team = "research"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err == nil {
		t.Fatalf("expected explicit queue/team conflict to fail; stdout:\n%s\nstderr:\n%s", out, stderr)
	}
	for _, want := range []string{"--queue=\"sample-training\"", "--team=\"research\"", "team intent consistent"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("queue/team conflict error missing %q: %v", want, err)
		}
	}
}

func TestManagedWorkflowSubmitPresetQueueOverrideWithTeamRendersConsistently(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: queue-team-ok
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.training.l"
	o.queue = "sample-training"
	o.team = "sample"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit should accept matching queue/team override: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: sample-training",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("consistent queue/team render missing %q:\n%s", want, rendered)
		}
	}
}

func TestManagedWorkflowSubmitLargeMemoryPresetDryRun(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: h200-large
compute: { gpus: 8 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.large-memory.xl"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: jobqueue",
		"priorityClassName: \"taugrid-default\"",
		`nvidia.com/gpu: "8"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("dry-run manifest missing %q", want)
		}
	}
}

// TestManagedWorkflowSubmitInfersPresetFromManifestGPUs verifies the one-line
// gap-closer: with no --preset flag, --team supplies the team and the
// preset is derived from the manifest's compute.gpus. The rendered Job
// must stamp the inferred preset annotation, and stderr must surface the
// inference message so researchers can see what was picked.
func TestManagedWorkflowSubmitInfersPresetFromManifestGPUs(t *testing.T) {
	t.Setenv("TAU_TEAM", "")
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: infer-2gpu
compute: { gpus: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.team = "research"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: jobqueue",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inferred render missing %q", want)
		}
	}
	if !strings.Contains(stderr, "inferred preset: azure.research.training.2x") {
		t.Errorf("expected stderr to surface inference, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "team=research from --team") {
		t.Errorf("expected stderr to name team source, got:\n%s", stderr)
	}
}

// TestManagedWorkflowSubmitInferenceUsesTeamFlag verifies that --team selects the
// correct team in the inference path, picking experimental.training.2x.
func TestManagedWorkflowSubmitInferenceUsesTeamFlag(t *testing.T) {
	t.Setenv("TAU_TEAM", "")
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: infer-team-flag
compute: { gpus: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.team = "experimental"
	o.dryRun = "client"
	_, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "team=experimental from --team") {
		t.Errorf("expected stderr to name --team source, got:\n%s", stderr)
	}
}

// TestManagedWorkflowSubmitInferenceUsesEnvVar verifies that TAU_TEAM takes effect
// when --team is not passed.
func TestManagedWorkflowSubmitInferenceUsesEnvVar(t *testing.T) {
	t.Setenv("TAU_TEAM", "research")
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: infer-team-env
compute: { gpus: 4 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.dryRun = "client"
	_, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "team=research from TAU_TEAM env") {
		t.Errorf("expected stderr to name env source, got:\n%s", stderr)
	}
}

// TestManagedWorkflowSubmitInferenceFallsThroughOnNoMatch verifies that if no preset
// matches the (team, lane, gpus) intent, the submit still proceeds via the
// no-preset path. We don't want inference to fail-closed when researchers
// want a custom shape outside the catalog.
func TestManagedWorkflowSubmitInferenceFallsThroughOnNoMatch(t *testing.T) {
	t.Setenv("TAU_TEAM", "")
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: infer-no-match
compute: { gpus: 3 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.team = "research"
	o.dryRun = "client"
	_, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit (no-match should fall through, not fail): %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stderr, "inferred preset:") {
		t.Errorf("no-match path should not announce an inferred preset; stderr:\n%s", stderr)
	}
}

// TestManagedWorkflowSubmitExplicitPresetSkipsInference verifies that --preset
// disables the inference path entirely (researcher choice wins).
func TestManagedWorkflowSubmitExplicitPresetSkipsInference(t *testing.T) {
	t.Setenv("TAU_TEAM", "experimental")
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: explicit-preset
compute: { gpus: 2 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.preset = "azure.research.training.2x"
	o.dryRun = "client"
	_, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	if strings.Contains(stderr, "inferred preset:") {
		t.Errorf("explicit --preset should suppress inference message; stderr:\n%s", stderr)
	}
}

func writeFinetuneManifest(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManagedWorkflowSubmitRayJobHandoffUsesResourceNameWithoutUnsupportedResults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl uses a POSIX shell script")
	}
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: vit-enc-vision-smoke
compute:
  gpus: 0
  workers: 1
runtime:
  image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	binDir := t.TempDir()
	kubectlPath := filepath.Join(binDir, "kubectl")
	fakeKubectl := `#!/bin/sh
case "$*" in
  *localqueue.kueue.x-k8s.io*)
    printf '%s\n' '{"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"tau-cq"}}'
    ;;
  *clusterqueue.kueue.x-k8s.io*)
    printf '%s\n' '{"metadata":{"name":"tau-cq"},"spec":{"resourceGroups":[]}}'
    ;;
  *"get rayjobs.ray.io"*)
    printf '%s\n' '{"metadata":{"uid":"uid-submit","annotations":{"` + workloadmeta.AnnotationSubmissionID + `":"submission-test"},"labels":{}}}'
    ;;
  *)
    printf '%s\n' 'rayjob.ray.io/tau-vit-enc-vision-smoke created'
    ;;
esac
`
	if err := os.WriteFile(kubectlPath, []byte(fakeKubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	o := defaultRunDispatchOptions()
	o.file = manifestPath
	o.mainScript = mainScript
	o.workloadKind = "rayjob"
	o.namespace = "taugrid-e2e"
	o.kubeContext = "taugrid-flex"
	o.submissionID = "submission-test"
	o.runID = "run-0001"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	resourceName := physicalRunName("tau-vit-enc-vision-smoke", "run-0001")
	for _, want := range []string{
		"submitted " + resourceName + " (kind=rayjob",
		"status:  tau run status " + resourceName,
		"logs:    tau run logs " + resourceName,
		"artifacts: /data is ephemeral (emptyDir); set storage.data_pvc to persist files beyond RayJob teardown",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("RayJob handoff missing %q:\n%s", want, rendered)
		}
	}
	for _, forbidden := range []string{
		"logs:    tau run logs vit-enc-vision-smoke",
		"results: tau run get",
		"html:    tau run get",
		"durable copy on  PVC",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("RayJob handoff unexpectedly contains %q:\n%s", forbidden, rendered)
		}
	}
}

// writeMainScript drops a stub trainer .py into a temp file and returns
// its absolute path so tests can supply --main-script (now required).
func writeMainScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trainer.py")
	if err := os.WriteFile(path, []byte("# trainer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- multi-node CLI dispatch tests ---

func TestManagedWorkflowSubmitMultiNodeWorkersFlagOverridesManifest(t *testing.T) {
	// --workers overrides compute.workers in the manifest. With workers>1
	// and --workload-kind omitted, dispatch infers rayjob and emits a
	// stderr warning so the researcher knows the dispatch was implicit.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: mn-cli
compute: { gpus: 8 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := filepath.Join(t.TempDir(), "wrapper.py")
	if err := os.WriteFile(mainScript, []byte("# stub SDK wrapper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.workers = 2
	o.mainScript = mainScript
	o.preset = "azure.research.large-memory.2node"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	if !strings.Contains(rendered, "kind: RayJob") {
		t.Errorf("dispatch should infer RayJob; got:\n%s", rendered)
	}
	if !strings.Contains(stderr, "inferred workload_kind=rayjob") {
		t.Errorf("stderr should warn about inferred workload kind; got:\n%s", stderr)
	}
	var rayJob struct {
		Spec struct {
			RayClusterSpec struct {
				HeadGroupSpec struct {
					Template struct {
						Metadata struct {
							Annotations map[string]string `yaml:"annotations"`
						} `yaml:"metadata"`
					} `yaml:"template"`
				} `yaml:"headGroupSpec"`
				WorkerGroupSpecs []struct {
					Template struct {
						Metadata struct {
							Annotations map[string]string `yaml:"annotations"`
						} `yaml:"metadata"`
					} `yaml:"template"`
				} `yaml:"workerGroupSpecs"`
			} `yaml:"rayClusterSpec"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &rayJob); err != nil {
		t.Fatalf("decode rendered RayJob: %v\n%s", err, rendered)
	}
	headAnnotations := rayJob.Spec.RayClusterSpec.HeadGroupSpec.Template.Metadata.Annotations
	for _, key := range []string{
		"kueue.x-k8s.io/podset-unconstrained-topology",
		"kueue.x-k8s.io/podset-preferred-topology",
		"kueue.x-k8s.io/podset-required-topology",
	} {
		if _, ok := headAnnotations[key]; ok {
			t.Errorf("control head retained workload topology annotation %q: %v", key, headAnnotations)
		}
	}
	if len(rayJob.Spec.RayClusterSpec.WorkerGroupSpecs) != 1 {
		t.Fatalf("rendered RayJob worker groups=%d want 1", len(rayJob.Spec.RayClusterSpec.WorkerGroupSpecs))
	}
	workerAnnotations := rayJob.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template.Metadata.Annotations
	if got := workerAnnotations["kueue.x-k8s.io/podset-unconstrained-topology"]; got != "true" {
		t.Errorf("worker unconstrained TAS annotation=%q want true; annotations=%v", got, workerAnnotations)
	}
	if value, ok := workerAnnotations["kueue.x-k8s.io/podset-preferred-topology"]; ok {
		t.Errorf("worker requests undeclared preferred topology %q", value)
	}
}

func TestManagedWorkflowWorkspaceQueuePreservesPresetTASContract(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: workspace-tas
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = writeMainScript(t)
	o.preset = "azure.research.training.l"
	o.queue = "workspace-jobqueue"
	o.workspaceQueueResolved = true
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		"kueue.x-k8s.io/queue-name: workspace-jobqueue",
		`kueue.x-k8s.io/podset-unconstrained-topology: "true"`,
		workloadmeta.AnnotationTopologyQueue + `: "workspace-jobqueue"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("workspace managed workflow missing %q:\n%s", want, out)
		}
	}
	for _, stale := range []string{
		workloadmeta.AnnotationClusterQueue + ":",
		workloadmeta.AnnotationResourceFlavor + ":",
		workloadmeta.LabelTeam + ":",
	} {
		if strings.Contains(out, stale) {
			t.Fatalf("workspace managed workflow retained stale preset metadata %q:\n%s", stale, out)
		}
	}
}

func TestManagedWorkflowSubmitMultiNodeWithExplicitJobKindFails(t *testing.T) {
	// Explicit --workload-kind=job + workers>1 must hard-fail with a
	// clear error pointing at the fix (omit the flag or switch to
	// rayjob). The Job CRD is single-pod; can't gang-admit.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: mn-bad
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "job"
	o.dryRun = "client"
	_, _, err := runManagedWorkflowDispatch(t, o)
	if err == nil {
		t.Fatalf("expected explicit --workload-kind=job + workers>1 to fail")
	}
	if !strings.Contains(err.Error(), "rayjob") {
		t.Errorf("error should suggest rayjob; got: %v", err)
	}
}

func TestManagedWorkflowSubmitMultiNodePresetWorkersMustMatchManifest(t *testing.T) {
	// A 2-node preset cannot host a manifest claiming workers=4.
	// The cross-check matches the existing gpus/shape consistency check.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: mn-mismatch
compute:
  gpus: 8
  workers: 4
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := filepath.Join(t.TempDir(), "wrapper.py")
	if err := os.WriteFile(mainScript, []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.preset = "azure.research.large-memory.2node"
	o.mainScript = mainScript
	o.dryRun = "client"
	_, _, err := runManagedWorkflowDispatch(t, o)
	if err == nil {
		t.Fatalf("expected preset/manifest workers mismatch to fail")
	}
	if !strings.Contains(err.Error(), "compute.workers=2") || !strings.Contains(err.Error(), "compute.workers=4") {
		t.Errorf("error should call out the mismatch with both values; got: %v", err)
	}
}

func TestManagedWorkflowSubmitMultiNodeRequiresMainScript(t *testing.T) {
	// Multi-node without --main-script must fail at render time
	// (Render owns the message — we just verify the path bubbles up).
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: mn-nosdk
compute:
  gpus: 8
  workers: 2
runtime:
  pip:
    - torch==2.4.0
`)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.preset = "azure.research.large-memory.2node"
	o.dryRun = "client"
	_, _, err := runManagedWorkflowDispatch(t, o)
	if err == nil {
		t.Fatalf("expected workers>1 without --main-script to fail")
	}
	if !strings.Contains(err.Error(), "run.entrypoint") {
		t.Errorf("error should point at the missing entrypoint; got: %v", err)
	}
}

func TestManagedWorkflowSubmitInfersRayJobEvalFromManifest(t *testing.T) {
	// eval.cpu_workers > 0 in the manifest with --workload-kind omitted
	// must infer rayjob-eval (not rayjob) and emit a stderr warning.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: eval-smoke
eval:
  cpu_workers: 4
  upstream: train-fullft
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := filepath.Join(t.TempDir(), "wrapper.py")
	if err := os.WriteFile(mainScript, []byte("# stub eval wrapper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.team = "research"
	o.mainScript = mainScript
	o.upstreamCheckpoint = "/data/checkpoints/train-fullft/last.safetensors"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	if !strings.Contains(rendered, "kind: RayJob") {
		t.Errorf("dispatch should infer RayJob; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "TAU_UPSTREAM_CHECKPOINT") || !strings.Contains(rendered, "/data/checkpoints/train-fullft/last.safetensors") {
		t.Errorf("rendered output should inject TAU_UPSTREAM_CHECKPOINT env; got:\n%s", rendered)
	}
	if !strings.Contains(stderr, "inferred workload_kind=rayjob-eval") {
		t.Errorf("stderr should warn about inferred eval kind; got:\n%s", stderr)
	}
	if !strings.Contains(rendered, "name: tau-eval-smoke\n") {
		t.Errorf("rendered output should use default tau- resource prefix; got:\n%s", rendered)
	}
}

func TestManagedWorkflowSubmitCPUWorkersFlagOverridesManifest(t *testing.T) {
	// --cpu-workers, like --workers, overrides eval.cpu_workers from the
	// manifest. With it set and --workload-kind omitted, dispatch infers
	// rayjob-eval.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: eval-smoke
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := filepath.Join(t.TempDir(), "wrapper.py")
	if err := os.WriteFile(mainScript, []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.cpuWorkers = 8
	o.mainScript = mainScript
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("managed workflow submit: %v\nstderr:\n%s", err, stderr)
	}
	rendered := out
	if !strings.Contains(rendered, "replicas: 8") {
		t.Errorf("rendered output should set worker replicas to 8; got:\n%s", rendered)
	}
}

func TestManagedWorkflowSubmitExplicitJobKindRejectsEvalManifest(t *testing.T) {
	// eval.cpu_workers set + explicit --workload-kind=job (or rayjob)
	// must hard-fail. The user is misdispatching — point them at the
	// right kind.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: misdispatched
eval:
  cpu_workers: 4
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	for _, kind := range []string{"job", "rayjob"} {
		t.Run(kind, func(t *testing.T) {
			mainScript := writeMainScript(t)
			o := defaultRunDispatchOptions()
			o.file = manifest
			o.mainScript = mainScript
			o.workloadKind = kind
			o.dryRun = "client"
			_, _, err := runManagedWorkflowDispatch(t, o)
			if err == nil {
				t.Fatalf("expected explicit --workload-kind=%s + eval manifest to fail", kind)
			}
			if !strings.Contains(err.Error(), "rayjob-eval") {
				t.Errorf("error should suggest rayjob-eval; got: %v", err)
			}
		})
	}
}

func TestManagedWorkflowSubmitRayJobEvalRequiresCPUWorkers(t *testing.T) {
	// Explicit --workload-kind=rayjob-eval without cpu_workers must fail
	// — the eval template needs at least one CPU worker pod to do useful
	// fanout work.
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: empty-eval
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := filepath.Join(t.TempDir(), "wrapper.py")
	if err := os.WriteFile(mainScript, []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.workloadKind = "rayjob-eval"
	o.mainScript = mainScript
	o.dryRun = "client"
	_, _, err := runManagedWorkflowDispatch(t, o)
	if err == nil {
		t.Fatalf("expected --workload-kind=rayjob-eval without cpu_workers to fail")
	}
	if !strings.Contains(err.Error(), "cpu_workers") && !strings.Contains(err.Error(), "cpu-workers") {
		t.Errorf("error should mention cpu_workers; got: %v", err)
	}
}

func TestManagedWorkflowSubmitInvalidWorkloadKind(t *testing.T) {
	manifest := writeFinetuneManifest(t, `
schema_version: 1
name: t
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifest
	o.mainScript = mainScript
	o.workloadKind = "deployment"
	o.dryRun = "client"
	_, _, err := runManagedWorkflowDispatch(t, o)
	if err == nil {
		t.Fatal("expected --workload-kind=deployment to fail")
	}
	if !strings.Contains(err.Error(), "rayjob-eval") {
		t.Errorf("error should list rayjob-eval among valid kinds; got: %v", err)
	}
}

func TestWorkloadKindToK8sKind(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"job", "Job"},
		{"rayjob", "RayJob"},
		{"rayjob-eval", "RayJob"},
		{"", "Job"},
	}
	for _, tc := range cases {
		got := workloadKindToK8sKind(tc.input)
		if got != tc.want {
			t.Errorf("workloadKindToK8sKind(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFinetuneSecretOwnerMetadataInDryRun(t *testing.T) {
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: sec-test
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: tau-sec-test-secrets
          key: HF_TOKEN
`)
	mainScript := writeMainScript(t)
	secretPayload := filepath.Join(t.TempDir(), "secret.json")
	payload, _ := json.Marshal(map[string]any{
		"name":       "tau-sec-test-secrets",
		"stringData": map[string]string{"HF_TOKEN": "test-token"},
	})
	os.WriteFile(secretPayload, payload, 0o644)

	o := defaultRunDispatchOptions()
	o.file = manifestPath
	o.mainScript = mainScript
	o.secretPayloadPath = secretPayload
	o.dryRun = "client"
	out, _, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	s := out
	if !strings.Contains(s, workloadmeta.LabelManagedBy+": tau") {
		t.Errorf("dry-run output missing managed-by label:\n%s", s)
	}
	if !strings.Contains(s, workloadmeta.AnnotationOwnerName+": tau-sec-test") {
		t.Errorf("dry-run output missing owner-name annotation:\n%s", s)
	}
	if !strings.Contains(s, workloadmeta.AnnotationOwnerKind+": Job") {
		t.Errorf("dry-run output missing owner-kind annotation:\n%s", s)
	}
}

func TestManagedWorkflowSubmitKVSpecWithPresetDryRun(t *testing.T) {
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: kv-preset
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env_kv:
    HF_TOKEN: my-vault/hf-token
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifestPath
	o.mainScript = mainScript
	o.preset = "azure.research.training.l"
	o.keyVault = "my-vault"
	o.kvTenantID = "tenant-abc"
	o.kvClientID = "client-xyz"
	o.serviceAccountName = "tau-workload"
	o.dryRun = "client"
	out, stderr, err := runManagedWorkflowDispatch(t, o)
	if err != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", err, stderr)
	}
	s := out
	for _, want := range []string{
		"SecretProviderClass",
		"keyvaultName: my-vault",
		"azure.workload.identity/use",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("KV+preset dry-run missing %q", want)
		}
	}
}

func TestManagedWorkflowSubmitKVRequiresServiceAccount(t *testing.T) {
	manifestPath := writeFinetuneManifest(t, `
schema_version: 1
name: kv-no-sa
compute: { gpus: 1 }
runtime:
  pip:
    - torch==2.4.0
  env_kv:
    HF_TOKEN: my-vault/hf-token
`)
	mainScript := writeMainScript(t)
	o := defaultRunDispatchOptions()
	o.file = manifestPath
	o.mainScript = mainScript
	o.keyVault = "my-vault"
	o.kvTenantID = "tenant-abc"
	o.kvClientID = "client-xyz"
	o.dryRun = "client"
	_, stderr, err := runManagedWorkflowDispatch(t, o)
	if err == nil || !strings.Contains(err.Error(), "a ServiceAccount is required") {
		t.Fatalf("expected service-account error, got %v\nstderr:\n%s", err, stderr)
	}
}
