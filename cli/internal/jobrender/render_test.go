package jobrender

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/artifactbundle"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// trainProfile is a representative resolved profile spec mirroring
// a representative multi-GPU training profile.
func trainProfile() profile.Profile {
	return profile.Profile{
		Name: "ai-train-gpu-l",
		Spec: map[string]any{
			"queue": map[string]any{
				"clusterQueue": "training-cq",
				"localQueue":   "training-queue",
			},
			"scheduling": map[string]any{
				"nodeSelector": map[string]any{workloadmeta.LabelLane: "train"},
				"tolerations": []any{
					map[string]any{"key": workloadmeta.LabelLane, "operator": "Equal", "value": "train", "effect": "NoSchedule"},
				},
				"priorityClassName": "taugrid-default",
			},
			"resources": map[string]any{
				"gpu":      map[string]any{"count": 1, "size": "l", "placement": "single-device"},
				"dra":      map[string]any{"claimTemplate": "full-gpu"},
				"requests": map[string]any{"cpu": "16", "memory": "64Gi"},
			},
			"runtime": map[string]any{
				"image":            "registry.example.com/research-pytorch:dev",
				"imagePullPolicy":  "IfNotPresent",
				"imagePullSecrets": []any{map[string]any{"name": "acr-secret"}},
			},
			"policy": map[string]any{
				"activeDeadlineSeconds": int64(3600),
			},
		},
	}
}

func parseYAML(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal rendered yaml: %v\n%s", err, string(b))
	}
	return m
}

func TestRender_HappyPath_Command(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "lora-7b-001",
		Namespace: "ray",
		Command:   []string{"python", "-m", "train", "--epochs", "3"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	m := parseYAML(t, out)

	if m["apiVersion"] != "batch/v1" || m["kind"] != "Job" {
		t.Fatalf("wrong gvk: %v / %v", m["apiVersion"], m["kind"])
	}
	meta := m["metadata"].(map[string]any)
	if meta["name"] != "lora-7b-001" || meta["namespace"] != "ray" {
		t.Errorf("metadata mismatch: %v", meta)
	}
	labels := meta["labels"].(map[string]any)
	if labels["kueue.x-k8s.io/queue-name"] != "training-queue" {
		t.Errorf("Kueue queue label missing/wrong: %v", labels)
	}
	if labels[workloadmeta.LabelManagedBy] != "tau" {
		t.Errorf("managed workload admission label missing/wrong: %v", labels)
	}
	for key := range labels {
		if strings.HasPrefix(key, workloadmeta.Domain) && key != workloadmeta.LabelManagedBy {
			t.Errorf("Tau metadata label should be omitted: %s", key)
		}
	}

	spec := m["spec"].(map[string]any)
	if spec["suspend"] != true {
		t.Errorf("suspend must be true so Kueue can admit; got %v", spec["suspend"])
	}
	if spec["activeDeadlineSeconds"] != 3600 {
		t.Errorf("activeDeadlineSeconds not propagated from policy: %v", spec["activeDeadlineSeconds"])
	}

	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	if pod["priorityClassName"] != "taugrid-default" {
		t.Errorf("priorityClassName not propagated: %v", pod["priorityClassName"])
	}
	if pod["affinity"] == nil {
		t.Error("single-device GPU jobs should prefer packing with existing GPU pods")
	}
	pullSecrets := pod["imagePullSecrets"].([]any)
	if len(pullSecrets) != 1 || pullSecrets[0].(map[string]any)["name"] != "acr-secret" {
		t.Errorf("imagePullSecrets not propagated: %v", pullSecrets)
	}
	claims := pod["resourceClaims"].([]any)
	if len(claims) != 1 || claims[0].(map[string]any)["resourceClaimTemplateName"] != "full-gpu" {
		t.Errorf("DRA claim wiring wrong: %v", claims)
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["image"] != "registry.example.com/research-pytorch:dev" {
		t.Errorf("image wrong: %v", c["image"])
	}
	cmd := c["command"].([]any)
	if len(cmd) != 5 || cmd[0] != "python" {
		t.Errorf("command not propagated: %v", cmd)
	}
	cres := c["resources"].(map[string]any)
	if cres["claims"] == nil {
		t.Errorf("container missing resources.claims for DRA: %v", cres)
	}
}

func TestRender_SuspendedJobRequiresQueue(t *testing.T) {
	p := trainProfile()
	delete(p.Spec, "queue")

	if _, err := Render(p, Options{
		Name:      "missing-queue",
		Namespace: "tau",
		Command:   []string{"true"},
	}); err == nil || !strings.Contains(err.Error(), "Kueue LocalQueue is required") {
		t.Fatalf("expected queue-less suspended Job rejection, got %v", err)
	}

	out, err := Render(p, Options{
		Name:      "explicit-queue",
		Namespace: "tau",
		Command:   []string{"true"},
		QueueName: "jobqueue",
	})
	if err != nil {
		t.Fatalf("render queue-bearing Job: %v", err)
	}
	labels := parseYAML(t, out)["metadata"].(map[string]any)["labels"].(map[string]any)
	if got := labels[runtopology.QueueLabel]; got != "jobqueue" {
		t.Fatalf("queue label = %#v, want jobqueue", got)
	}
}

func TestRender_ServiceAccountAndTTLOverrides(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:                    "onboarding-smoke",
		Namespace:               "sample",
		Command:                 []string{"true"},
		ServiceAccountName:      "tau-workload",
		AzureWorkloadIdentity:   true,
		TTLSecondsAfterFinished: 600,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	spec := m["spec"].(map[string]any)
	if got := spec["ttlSecondsAfterFinished"]; got != 600 {
		t.Fatalf("ttlSecondsAfterFinished = %#v, want 600", got)
	}
	podSpec := spec["template"].(map[string]any)["spec"].(map[string]any)
	if got := podSpec["serviceAccountName"]; got != "tau-workload" {
		t.Fatalf("serviceAccountName = %#v, want tau-workload", got)
	}
	podLabels := spec["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	if got := podLabels[workloadmeta.LabelAzureWorkloadIdentityUse]; got != "true" {
		t.Fatalf("%s = %#v, want true", workloadmeta.LabelAzureWorkloadIdentityUse, got)
	}
}

func TestRenderEnvSecretUsesSecretKeyRef(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:       "secret-env",
		Namespace:  "tau",
		Command:    []string{"python", "train.py"},
		EnvSecrets: []envspec.Var{envspec.Secret("HF_TOKEN", "hf-secret", "token-key")},
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
	out, err := Render(trainProfile(), Options{
		Name:          "secret-env",
		Namespace:     "tau",
		Command:       []string{"python", "train.py"},
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

func TestRender_RuntimeSecurityContextPropagatesToContainer(t *testing.T) {
	p := trainProfile()
	p.Spec["runtime"].(map[string]any)["securityContext"] = map[string]any{
		"capabilities": map[string]any{
			"add": []any{"SYS_ADMIN"},
		},
	}
	out, err := Render(p, Options{
		Name:      "a100-ncu",
		Namespace: "tau",
		Command:   []string{"python", "bench.py"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pod := parseYAML(t, out)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	securityContext := c["securityContext"].(map[string]any)
	capabilities := securityContext["capabilities"].(map[string]any)
	add := capabilities["add"].([]any)
	if len(add) != 1 || add[0] != "SYS_ADMIN" {
		t.Fatalf("securityContext capabilities not propagated: %v", securityContext)
	}
}

func TestRender_SmallSameNodeMultiGPUProfileUsesPacking(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["gpu"] = map[string]any{"count": 2, "size": "l", "placement": "same-node"}
	res["dra"] = map[string]any{"claimTemplate": "ds-2gpus"}

	out, err := Render(p, Options{
		Name:      "tp2",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if _, ok := pod["affinity"]; !ok {
		t.Fatal("same-node 2-4 GPU profiles should prefer packing onto already-used GPU nodes")
	}
	meta := m["metadata"].(map[string]any)
	if annotations, ok := meta["annotations"].(map[string]any); ok {
		if _, ok := annotations[workloadmeta.AnnotationGPUContract]; ok {
			t.Fatalf("Tau GPU contract annotation should be omitted: %v", annotations)
		}
	}
}

func TestRender_LargeSameNodeMultiGPUProfileDoesNotUsePacking(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["gpu"] = map[string]any{"count": 5, "size": "l", "placement": "same-node"}
	res["dra"] = map[string]any{"claimTemplate": "ds-5gpus"}

	out, err := Render(p, Options{
		Name:      "tp5",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if _, ok := pod["affinity"]; ok {
		t.Fatalf("same-node 5+ GPU profiles should not use bin-packing affinity: %v", pod["affinity"])
	}
}

// podTolerations returns the rendered pod tolerations as key|operator|value|effect
// strings, which keeps the assertions below readable and order-independent.
func podTolerations(t *testing.T, out []byte) []string {
	t.Helper()
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	raw, ok := pod["tolerations"].([]any)
	if !ok {
		return nil
	}
	var got []string
	for _, entry := range raw {
		e := entry.(map[string]any)
		got = append(got, fmt.Sprintf("%v|%v|%v|%v", e["key"], e["operator"], e["value"], e["effect"]))
	}
	sort.Strings(got)
	return got
}

func TestRender_GPUJobToleratesGPUNodeTaints(t *testing.T) {
	// trainProfile declares gpu.count 1 and a lane toleration, so this covers
	// injection and profile preservation in one render.
	out, err := Render(trainProfile(), Options{
		Name:      "gputol",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := podTolerations(t, out)
	want := []string{
		"nvidia.com/gpu|Exists|<nil>|NoSchedule",
		"sku|Equal|gpu|NoSchedule",
		workloadmeta.LabelLane + "|Equal|train|NoSchedule",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GPU job tolerations:\n got %v\nwant %v", got, want)
	}
}

func TestRender_CPUJobDoesNotTolerateGPUNodeTaints(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	delete(res, "gpu")
	delete(res, "dra")

	out, err := Render(p, Options{
		Name:      "cputol",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := podTolerations(t, out)
	want := []string{workloadmeta.LabelLane + "|Equal|train|NoSchedule"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CPU job must keep profile tolerations without GPU taints:\n got %v\nwant %v", got, want)
	}
}

func TestRender_GPUTolerationDeclaredByProfileIsNotDuplicated(t *testing.T) {
	p := trainProfile()
	sched := p.Spec["scheduling"].(map[string]any)
	sched["tolerations"] = []any{
		map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
	}

	out, err := Render(p, Options{
		Name:      "duptol",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := podTolerations(t, out)
	want := []string{
		"nvidia.com/gpu|Exists|<nil>|NoSchedule",
		"sku|Equal|gpu|NoSchedule",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profile-declared GPU toleration must not duplicate:\n got %v\nwant %v", got, want)
	}
}

func TestRender_GPUJobWithoutProfileSchedulingStillTolerates(t *testing.T) {
	// The synthesized profile built by `tau run --config` has no scheduling
	// block at all, which is the path BUG-23 hit.
	p := trainProfile()
	delete(p.Spec, "scheduling")

	out, err := Render(p, Options{
		Name:      "noschedtol",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := podTolerations(t, out)
	want := []string{
		"nvidia.com/gpu|Exists|<nil>|NoSchedule",
		"sku|Equal|gpu|NoSchedule",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GPU job without profile scheduling:\n got %v\nwant %v", got, want)
	}
}

func TestRender_DRAGPUJobToleratesGPUNodeTaints(t *testing.T) {
	// A DRA profile requests GPUs through dra.claimTemplate rather than a GPU
	// count, so gating tolerations on the count alone would miss it.
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	delete(res, "gpu")
	res["dra"] = map[string]any{"claimTemplate": "full-gpu"}
	delete(p.Spec, "scheduling")

	out, err := Render(p, Options{
		Name:      "dratol",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := podTolerations(t, out)
	want := []string{
		"nvidia.com/gpu|Exists|<nil>|NoSchedule",
		"sku|Equal|gpu|NoSchedule",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DRA GPU job tolerations:\n got %v\nwant %v", got, want)
	}
}

func TestRender_ProfileTolerationsDifferingOnlyInSecondsAreKept(t *testing.T) {
	// tolerationSeconds changes how long a pod tolerates a NoExecute taint, so
	// two entries differing only in it must both survive the dedupe.
	p := trainProfile()
	sched := p.Spec["scheduling"].(map[string]any)
	sched["tolerations"] = []any{
		map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 30},
		map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 600},
	}

	out, err := Render(p, Options{
		Name:      "tolsec",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	var seconds []any
	for _, entry := range pod["tolerations"].([]any) {
		e := entry.(map[string]any)
		if e["key"] == "node.kubernetes.io/unreachable" {
			seconds = append(seconds, e["tolerationSeconds"])
		}
	}
	if !reflect.DeepEqual(seconds, []any{30, 600}) {
		t.Fatalf("both tolerationSeconds variants must survive dedupe, got %v", seconds)
	}
}

func TestRender_GPUContractMismatchErrorsBeforeManifest(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["gpu"] = map[string]any{"count": 2, "placement": "same-node"}
	res["dra"] = map[string]any{"claimTemplate": "full-gpu"}

	_, err := Render(p, Options{
		Name:      "bad",
		Namespace: "tau",
		Command:   []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "GPU contract invalid") {
		t.Fatalf("expected GPU contract mismatch error, got %v", err)
	}
}

func TestRender_ScriptPath_Base64Encoded(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(trainProfile(), Options{
		Name:       "scripted",
		Namespace:  "tau",
		ScriptPath: script,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "TAU_SCRIPT_B64") {
		t.Errorf("missing TAU_SCRIPT_B64 env var:\n%s", s)
	}
	if !strings.Contains(s, "base64 -d") {
		t.Errorf("missing base64 decode entrypoint:\n%s", s)
	}
	if !strings.Contains(s, "exec /tmp/run.sh") {
		t.Errorf("shell shebang was not honored:\n%s", s)
	}
	// Sanity: the literal script content must NOT appear unencoded.
	if strings.Contains(s, "echo hello") {
		t.Errorf("script content leaked unencoded:\n%s", s)
	}
}

func TestRender_DetectsScriptInterpreter(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		body     string
		launcher string
		want     string
		wantErr  string
	}{
		{name: "python suffix", filename: "train.py", body: "print('ok')\n", want: "exec python3 /tmp/tau-entrypoint.py"},
		{name: "python shebang honored directly", filename: "train", body: "#!/usr/bin/env -S python3 -u\nprint('ok')\n", want: "exec /tmp/run.sh"},
		{name: "shell shebang wins over suffix", filename: "train.py", body: "#!/bin/sh\necho ok\n", want: "exec /tmp/run.sh"},
		{name: "explicit python", filename: "train", body: "print('ok')\n", launcher: "python", want: "exec python3 /tmp/tau-entrypoint.py"},
		{name: "explicit python with shebang", filename: "train", body: "#!/usr/bin/env -u PYTHONPATH python3\nprint('ok')\n", launcher: "python", want: "exec python3 /tmp/tau-entrypoint.py"},
		{name: "ambiguous", filename: "train", body: "echo ok\n", wantErr: "has no shebang and is not a .py file"},
		{name: "empty shebang", filename: "train", body: "#!\necho ok\n", wantErr: "has an empty shebang"},
		{name: "conflicting launcher", filename: "train.py", body: "#!/bin/sh\necho ok\n", launcher: "python", wantErr: "non-Python shebang"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), tt.filename)
			if err := os.WriteFile(script, []byte(tt.body), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := Render(trainProfile(), Options{
				Name:       "scripted",
				Namespace:  "tau",
				ScriptPath: script,
				Launcher:   tt.launcher,
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Fatalf("rendered command does not contain %q:\n%s", tt.want, out)
			}
		})
	}
}

func TestRender_ScriptPathDoesNotStageSourceTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\npython scripts/train.py\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := Render(trainProfile(), Options{
		Name:       "scripted",
		Namespace:  "tau",
		ScriptPath: script,
		PVCMount:   "blob-training",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["workingDir"] != "/data" {
		t.Fatalf("workingDir=%v, want /data", c["workingDir"])
	}
	foundScriptSrc := false
	for _, item := range c["env"].([]any) {
		env := item.(map[string]any)
		if env["name"] == "TAU_SCRIPT_SRC" {
			foundScriptSrc = true
			if env["value"] != "run.sh" {
				t.Fatalf("TAU_SCRIPT_SRC=%v, want script basename only", env["value"])
			}
		}
	}
	if !foundScriptSrc {
		t.Fatalf("TAU_SCRIPT_SRC missing from env: %+v", c["env"])
	}
	volumes := pod["volumes"].([]any)
	if len(volumes) != 2 {
		t.Fatalf("expected only data and hot scratch volumes, got %+v", volumes)
	}
	for _, item := range volumes {
		v := item.(map[string]any)
		if fmt.Sprint(v["name"]) == "scripts" || strings.Contains(fmt.Sprint(v), dir) {
			t.Fatalf("--script must not stage source directory %q: %+v", dir, volumes)
		}
	}
}

// TestRender_ScriptArgs_ForwardedToScript ensures positional args after `--`
// reach the in-pod script as $1, $2, ... — without this, dispatch SDKs that
// pass a script *plus* runtime arguments cannot use a direct tau run Job, because
// --script alone cannot communicate runtime args to the script body.
func TestRender_ScriptArgs_ForwardedToScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bench.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(trainProfile(), Options{
		Name:       "scripted",
		Namespace:  "tau",
		ScriptPath: script,
		ScriptArgs: []string{"liger-perkernel", "--kernel", "rmsnorm", "--out", "/data/x.json"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	// The args must appear (quoted) on the bash -c command line that exec's
	// the decoded script, so the in-pod bash sees them as $@.
	if !strings.Contains(s, "exec /tmp/run.sh liger-perkernel --kernel rmsnorm --out /data/x.json") {
		t.Errorf("script args not appended to exec line:\n%s", s)
	}
}

// TestRender_ScriptArgs_UnsafeCharsAreShellQuoted ensures args with spaces
// or special chars round-trip through bash without splitting.
func TestRender_ScriptArgs_UnsafeCharsAreShellQuoted(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bench.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(trainProfile(), Options{
		Name:       "scripted",
		Namespace:  "tau",
		ScriptPath: script,
		ScriptArgs: []string{"hello world", "it's me", "$EVIL"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"'hello world'",
		`'it'"'"'s me'`,
		"'$EVIL'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected quoted %q in rendered manifest:\n%s", want, s)
		}
	}
}

func TestRender_ImageOverride(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "j1",
		Namespace: "tau",
		Image:     "myregistry.io/my-recsys-train:v9",
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "myregistry.io/my-recsys-train:v9") {
		t.Errorf("--image override not honoured:\n%s", string(out))
	}
}

func TestRender_WorkloadMetadata(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "eval-1",
		Namespace: "tau",
		Command:   []string{"true"},
		Labels: map[string]string{
			workloadmeta.LabelJob: "eval-1",
			"run_id":              "eval-run-1",
		},
		Annotations: map[string]string{
			workloadmeta.LabelExperiment:               "vision",
			workloadmeta.LabelStellarProject:           "vit-enc",
			workloadmeta.AnnotationStellarExperimentID: "vision:baseline",
			workloadmeta.AnnotationWorkspaceID:         "sample",
			workloadmeta.AnnotationResultPath:          "/data/evals/eval-1.json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	meta := m["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	for key, want := range map[string]string{
		workloadmeta.LabelJob: "eval-1",
		"run_id":              "eval-run-1",
	} {
		if labels[key] != want {
			t.Errorf("metadata label %s=%v want %s", key, labels[key], want)
		}
	}
	annotations := meta["annotations"].(map[string]any)
	for key, want := range map[string]string{
		workloadmeta.LabelExperiment:               "vision",
		workloadmeta.LabelStellarProject:           "vit-enc",
		workloadmeta.AnnotationStellarExperimentID: "vision:baseline",
		workloadmeta.AnnotationWorkspaceID:         "sample",
		workloadmeta.AnnotationResultPath:          "/data/evals/eval-1.json",
	} {
		if annotations[key] != want {
			t.Errorf("metadata annotation %s=%v want %s", key, annotations[key], want)
		}
	}
	podMeta := m["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
	podAnnotations := podMeta["annotations"].(map[string]any)
	for key, want := range map[string]string{
		workloadmeta.LabelStellarProject:           "vit-enc",
		workloadmeta.AnnotationStellarExperimentID: "vision:baseline",
		workloadmeta.AnnotationWorkspaceID:         "sample",
	} {
		if podAnnotations[key] != want {
			t.Errorf("pod annotation %s=%v want %s", key, podAnnotations[key], want)
		}
	}
	if _, ok := podAnnotations[workloadmeta.AnnotationResultPath]; ok {
		t.Fatalf("result path should remain workload-only: %v", podAnnotations)
	}
}

func TestRender_PVCMountUsesDurableDataAndHotScratch(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "with-storage",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	volumes := pod["volumes"].([]any)
	if len(volumes) != 2 {
		t.Fatalf("expected data and hot volumes, got %+v", volumes)
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	mounts := c["volumeMounts"].([]any)
	gotMounts := map[string]string{}
	for _, item := range mounts {
		m := item.(map[string]any)
		gotMounts[m["name"].(string)] = m["mountPath"].(string)
	}
	if gotMounts["data"] != "/data" || gotMounts["tau-hot"] != "/mnt" {
		t.Fatalf("unexpected storage mounts: %+v", gotMounts)
	}
	if c["workingDir"] != "/data" {
		t.Fatalf("workingDir=%v, want /data", c["workingDir"])
	}
	cmd := c["command"].([]any)
	if len(cmd) < 6 || cmd[0] != "bash" || !strings.Contains(cmd[2].(string), "tau storage warning") {
		t.Fatalf("storage preflight did not wrap command: %+v", cmd)
	}
	env := c["env"].([]any)
	if !strings.Contains(fmt.Sprint(env), "TAU_DURABLE_CHECKPOINTS_DIR") {
		t.Fatalf("storage env missing: %+v", env)
	}
}

func TestRender_ProfilePersistenceMountsPVCs(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["persistence"] = []any{
		map[string]any{"pvcName": "blob-training", "mountPath": "/data"},
		map[string]any{"pvcName": "training-nfs", "mountPath": "/data-nfs", "readOnly": true},
	}

	out, err := Render(p, Options{
		Name:      "profile-storage",
		Namespace: "tau",
		Command:   []string{"python", "bench.py"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	volumes := pod["volumes"].([]any)
	claims := map[string]string{}
	emptyDirs := map[string]bool{}
	for _, item := range volumes {
		v := item.(map[string]any)
		name := v["name"].(string)
		if pvc, ok := v["persistentVolumeClaim"].(map[string]any); ok {
			claims[name] = pvc["claimName"].(string)
		}
		if _, ok := v["emptyDir"]; ok {
			emptyDirs[name] = true
		}
	}
	if claims["data"] != "blob-training" || claims["persistence-1"] != "training-nfs" {
		t.Fatalf("profile PVCs not mounted: claims=%+v volumes=%+v", claims, volumes)
	}
	if !emptyDirs["tau-hot"] {
		t.Fatalf("durable /data mount must add hot /mnt scratch: %+v", volumes)
	}

	c := pod["containers"].([]any)[0].(map[string]any)
	mounts := c["volumeMounts"].([]any)
	gotMounts := map[string]map[string]any{}
	for _, item := range mounts {
		m := item.(map[string]any)
		gotMounts[m["name"].(string)] = m
	}
	if gotMounts["data"]["mountPath"] != "/data" ||
		gotMounts["persistence-1"]["mountPath"] != "/data-nfs" ||
		gotMounts["tau-hot"]["mountPath"] != "/mnt" {
		t.Fatalf("profile mounts wrong: %+v", gotMounts)
	}
	if gotMounts["persistence-1"]["readOnly"] != true {
		t.Fatalf("readOnly not preserved: %+v", gotMounts["persistence-1"])
	}
	if c["workingDir"] != "/data" {
		t.Fatalf("workingDir=%v, want /data", c["workingDir"])
	}
	if !strings.Contains(fmt.Sprint(c["command"]), "tau storage warning") {
		t.Fatalf("durable profile storage did not wrap command: %+v", c["command"])
	}
}

func TestRender_ProfileStorageContractAddsMetadataAndEnv(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["persistence"] = []any{
		map[string]any{"pvcName": "blob-training", "mountPath": "/data"},
	}
	res["storage"] = map[string]any{
		"durable": map[string]any{"type": "blobfuse", "mountPath": "/data", "cache": "block"},
		"hot":     map[string]any{"type": "emptyDir", "mountPath": "/mnt", "fallback": "durable"},
		"modelCache": map[string]any{
			"type":      "local-nvme",
			"mountPath": "/models",
			"scope":     "node",
		},
		"checkpointing": map[string]any{"format": "sharded"},
	}

	out, err := Render(p, Options{
		Name:      "storage-contract",
		Namespace: "tau",
		Command:   []string{"python", "bench.py"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	meta := m["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels[workloadmeta.LabelManagedBy] != "tau" {
		t.Fatalf("managed workload marker = %v, want tau", labels[workloadmeta.LabelManagedBy])
	}
	for key := range labels {
		if strings.HasPrefix(key, workloadmeta.Domain) && key != workloadmeta.LabelManagedBy {
			t.Fatalf("Tau storage metadata label should be omitted: %s", key)
		}
	}
	env := envFromContainer(t, m)
	for key, want := range map[string]string{
		"TAU_STORAGE_DURABLE_TYPE":     "blobfuse",
		"TAU_STORAGE_DURABLE_CACHE":    "block",
		"TAU_STORAGE_HOT_TYPE":         "empty-dir",
		"TAU_STORAGE_HOT_FALLBACK":     "durable",
		"TAU_MODEL_CACHE_DIR":          "/models",
		"TAU_CHECKPOINT_FORMAT":        "sharded",
		"TAU_STORAGE_MODEL_CACHE_TYPE": "local-nvme",
	} {
		if env[key] != want {
			t.Fatalf("%s=%q want %q (env=%+v)", key, env[key], want, env)
		}
	}
}

func TestRender_RefusesToFakePlatformMountedHotStorage(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["persistence"] = []any{
		map[string]any{"pvcName": "blob-training", "mountPath": "/data"},
	}
	res["storage"] = map[string]any{
		"durable": map[string]any{"type": "blobfuse", "mountPath": "/data"},
		"hot":     map[string]any{"type": "local-nvme", "mountPath": "/mnt", "fallback": "durable"},
	}

	_, err := Render(p, Options{
		Name:      "storage-contract",
		Namespace: "tau",
		Command:   []string{"python", "bench.py"},
	})
	if err == nil || !strings.Contains(err.Error(), "only synthesizes empty-dir") {
		t.Fatalf("expected local-nvme hot storage to require explicit platform mount, got %v", err)
	}
}

func TestRender_ProfilePersistenceMapMountsNonDataPVC(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	res["persistence"] = map[string]any{"pvcName": "sandbox-home", "mountPath": "/home/researcher"}

	out, err := Render(p, Options{Name: "profile-home", Namespace: "tau", Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	volumes := pod["volumes"].([]any)
	if len(volumes) != 1 {
		t.Fatalf("non-/data persistence should not add hot scratch, got %+v", volumes)
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["workingDir"] == "/data" {
		t.Fatalf("non-/data persistence must not force durable workingDir")
	}
}

func TestRender_TopologyContractAddsKueueMetadata(t *testing.T) {
	p := trainProfile()
	p.Spec["policy"] = map[string]any{
		"preemptable":         true,
		"checkpointOnPreempt": true,
	}
	out, err := Render(p, Options{
		Name:      "train-a100",
		Namespace: "tau",
		Command:   []string{"true"},
		Team:      "research",
		Lane:      "training",
		Mode:      "fixed",
		Topology:  "single-node-nvlink",
		Shape:     "8xa100-80gb",
		GPUClass:  "a100-80gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	meta := m["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels["kueue.x-k8s.io/queue-name"] != runtopology.SharedGPUQueueName {
		t.Fatalf("queue label=%v want %s", labels["kueue.x-k8s.io/queue-name"], runtopology.SharedGPUQueueName)
	}
	if labels[workloadmeta.LabelManagedBy] != "tau" {
		t.Fatalf("managed workload marker = %v, want tau", labels[workloadmeta.LabelManagedBy])
	}
	if labels[workloadmeta.LabelGPUClass] != runtopology.GPUClassA10080GB {
		t.Fatalf("gpu class metadata label=%v want %s", labels[workloadmeta.LabelGPUClass], runtopology.GPUClassA10080GB)
	}
	for key := range labels {
		if strings.HasPrefix(key, workloadmeta.Domain) &&
			key != workloadmeta.LabelManagedBy &&
			key != workloadmeta.LabelGPUClass {
			t.Fatalf("Tau metadata label should be omitted: %s", key)
		}
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations["kueue.x-k8s.io/podset-required-topology"] != "kubernetes.io/hostname" {
		t.Fatalf("job topology annotation missing: %v", annotations)
	}
	templateMeta := m["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
	podAnnotations := templateMeta["annotations"].(map[string]any)
	if podAnnotations["kueue.x-k8s.io/podset-required-topology"] != "kubernetes.io/hostname" {
		t.Fatalf("pod topology annotation missing: %v", podAnnotations)
	}
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	nodeSelector := pod["nodeSelector"].(map[string]any)
	if nodeSelector[workloadmeta.NodeLabelGPUClass] != runtopology.GPUClassA10080GB {
		t.Fatalf("gpu class node selector=%v want %s", nodeSelector, runtopology.GPUClassA10080GB)
	}
}

func TestRender_LegacyGPUClassAliasRendersCanonicalContract(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "train-a100-legacy",
		Namespace: "tau",
		Command:   []string{"true"},
		QueueName: "jobqueue",
		GPUClass:  "a100-nvlink-80gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	labels := m["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[workloadmeta.LabelGPUClass] != runtopology.GPUClassA10080GB {
		t.Fatalf("gpu class label=%v want %s", labels, runtopology.GPUClassA10080GB)
	}
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	nodeSelector := pod["nodeSelector"].(map[string]any)
	if nodeSelector[workloadmeta.NodeLabelGPUClass] != runtopology.GPUClassA10080GB {
		t.Fatalf("gpu class selector=%v want %s", nodeSelector, runtopology.GPUClassA10080GB)
	}
}

func TestRender_RejectsConflictingGPUClassNodeSelector(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "train-conflict",
		Namespace: "tau",
		Command:   []string{"true"},
		QueueName: "jobqueue",
		GPUClass:  runtopology.GPUClassA10080GB,
		NodeSelector: map[string]string{
			workloadmeta.NodeLabelGPUClass: runtopology.GPUClassH10095GB,
		},
	})
	if err == nil || !strings.Contains(err.Error(), workloadmeta.NodeLabelGPUClass) {
		t.Fatalf("expected conflicting gpu class selector error, got %v", err)
	}
}

func TestRender_RejectsGPUClassSelectorForAny(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "train-any-conflict",
		Namespace: "tau",
		Command:   []string{"true"},
		QueueName: "jobqueue",
		GPUClass:  runtopology.GPUClassAny,
		NodeSelector: map[string]string{
			workloadmeta.NodeLabelGPUClass: runtopology.GPUClassA10080GB,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unconstrained") {
		t.Fatalf("expected gpu_class any selector error, got %v", err)
	}
}

func TestRender_RejectsSelectorConflictingWithProfileGPUClass(t *testing.T) {
	p := trainProfile()
	p.Spec["topology"] = map[string]any{"gpuClass": runtopology.GPUClassH10095GB}
	_, err := Render(p, Options{
		Name:      "train-profile-conflict",
		Namespace: "tau",
		Command:   []string{"true"},
		QueueName: "jobqueue",
		NodeSelector: map[string]string{
			workloadmeta.NodeLabelGPUClass: runtopology.GPUClassA10080GB,
		},
	})
	if err == nil || !strings.Contains(err.Error(), workloadmeta.NodeLabelGPUClass) {
		t.Fatalf("expected profile gpu class selector error, got %v", err)
	}
}

func TestRender_RejectsClassSelectorForProfileAny(t *testing.T) {
	p := trainProfile()
	p.Spec["topology"] = map[string]any{"gpuClass": runtopology.GPUClassAny}
	_, err := Render(p, Options{
		Name:      "train-profile-any-conflict",
		Namespace: "tau",
		Command:   []string{"true"},
		QueueName: "jobqueue",
		NodeSelector: map[string]string{
			workloadmeta.NodeLabelGPUClass: runtopology.GPUClassA10080GB,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unconstrained") {
		t.Fatalf("expected profile gpu_class any selector error, got %v", err)
	}
}

func TestRender_ClearNodeSelectorPreservesGPUClassContract(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:              "train-clear-selector",
		Namespace:         "tau",
		Command:           []string{"true"},
		QueueName:         "jobqueue",
		GPUClass:          runtopology.GPUClassA10080GB,
		ClearNodeSelector: true,
		NodeSelector:      map[string]string{"rack": "r1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	nodeSelector := pod["nodeSelector"].(map[string]any)
	if nodeSelector[workloadmeta.NodeLabelGPUClass] != runtopology.GPUClassA10080GB {
		t.Fatalf("gpu class selector=%v want %s", nodeSelector, runtopology.GPUClassA10080GB)
	}
	if nodeSelector["rack"] != "r1" {
		t.Fatalf("caller selector missing: %v", nodeSelector)
	}
	if _, ok := nodeSelector[workloadmeta.LabelLane]; ok {
		t.Fatalf("profile selector survived clear_node_selector: %v", nodeSelector)
	}
}

func TestRender_EmbeddedPresetTASContract(t *testing.T) {
	tests := []struct {
		name       string
		presetName string
		annotation string
		value      string
	}{
		{
			name:       "independent Job uses unconstrained TAS",
			presetName: "azure.research.training.l",
			annotation: "kueue.x-k8s.io/podset-unconstrained-topology",
			value:      "true",
		},
		{
			name:       "large-memory Job requires one hostname",
			presetName: "azure.research.large-memory.2x",
			annotation: "kueue.x-k8s.io/podset-required-topology",
			value:      "kubernetes.io/hostname",
		},
		{
			name:       "multi-node Job uses unconstrained TAS",
			presetName: "azure.research.large-memory.2node",
			annotation: "kueue.x-k8s.io/podset-unconstrained-topology",
			value:      "true",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := runtopology.ResolvePreset("", tc.presetName)
			if err != nil {
				t.Fatalf("resolve preset: %v", err)
			}
			opts := Options{
				Name:      "tas-contract",
				Namespace: "taugrid-default",
				Command:   []string{"true"},
			}
			ApplyTopologyOptions(&opts, resolved.Options)

			out, err := Render(trainProfile(), opts)
			if err != nil {
				t.Fatalf("render Job: %v", err)
			}
			job := parseYAML(t, out)
			jobAnnotations := job["metadata"].(map[string]any)["annotations"].(map[string]any)
			if got := jobAnnotations[tc.annotation]; got != tc.value {
				t.Fatalf("Job annotation %s=%v want %q; annotations=%v", tc.annotation, got, tc.value, jobAnnotations)
			}
			podAnnotations := job["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
			if got := podAnnotations[tc.annotation]; got != tc.value {
				t.Fatalf("Pod template annotation %s=%v want %q; annotations=%v", tc.annotation, got, tc.value, podAnnotations)
			}
		})
	}
}

func TestRender_DRAConvertedPresetOmitsTASAnnotations(t *testing.T) {
	resolved, err := runtopology.ResolvePreset("", "azure.research.training.l")
	if err != nil {
		t.Fatalf("resolve preset: %v", err)
	}
	resolved = runtopology.WithDRAQueue(resolved)
	opts := Options{
		Name:      "dra-contract",
		Namespace: "taugrid-default",
		Command:   []string{"true"},
	}
	ApplyTopologyOptions(&opts, resolved.Options)

	out, err := Render(trainProfile(), opts)
	if err != nil {
		t.Fatalf("render Job: %v", err)
	}
	job := parseYAML(t, out)
	jobAnnotations, _ := job["metadata"].(map[string]any)["annotations"].(map[string]any)
	podAnnotations, _ := job["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
	annotationSets := []map[string]any{
		jobAnnotations,
		podAnnotations,
	}
	for _, annotations := range annotationSets {
		for _, key := range []string{
			"kueue.x-k8s.io/podset-unconstrained-topology",
			"kueue.x-k8s.io/podset-required-topology",
			"kueue.x-k8s.io/podset-preferred-topology",
		} {
			if value, ok := annotations[key]; ok {
				t.Errorf("DRA Job retained TAS annotation %s=%v; annotations=%v", key, value, annotations)
			}
		}
	}
}

func TestRender_GPUClassAnyClearsProfileGPUSelectorAndDefaultPriorities(t *testing.T) {
	p := trainProfile()
	sched := p.Spec["scheduling"].(map[string]any)
	sched["nodeSelector"] = map[string]any{
		workloadmeta.LabelGPUClass: "h200-141gb",
		"agentpool":                "h200pool",
	}

	out, err := Render(p, Options{
		Name:                     "h200-smoke",
		Namespace:                "tau",
		Command:                  []string{"true"},
		QueueName:                "dev",
		GPUClass:                 runtopology.GPUClassAny,
		DisableDefaultPriorities: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	m := parseYAML(t, out)
	meta := m["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels["kueue.x-k8s.io/queue-name"] != "dev" {
		t.Fatalf("queue label=%v want dev", labels["kueue.x-k8s.io/queue-name"])
	}
	if labels[workloadmeta.LabelGPUClass] != runtopology.GPUClassAny {
		t.Fatalf("gpu class label=%v want %s", labels, runtopology.GPUClassAny)
	}
	if _, ok := labels["kueue.x-k8s.io/priority-class"]; ok {
		t.Fatalf("default workload priority should be omitted: %v", labels)
	}
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if _, ok := pod["priorityClassName"]; ok {
		t.Fatalf("default pod priority should be omitted: %v", pod["priorityClassName"])
	}
	nodeSelector := pod["nodeSelector"].(map[string]any)
	if nodeSelector["agentpool"] != "h200pool" {
		t.Fatalf("non-GPU selectors should remain: %v", nodeSelector)
	}
	if _, ok := nodeSelector[workloadmeta.LabelGPUClass]; ok {
		t.Fatalf("gpu_class any should remove the class selector: %v", nodeSelector)
	}
}

func TestRender_ElasticRequiresCheckpoint(t *testing.T) {
	p := trainProfile()
	p.Spec["policy"] = map[string]any{"preemptable": true}
	_, err := Render(p, Options{
		Name:      "elastic",
		Namespace: "tau",
		Command:   []string{"true"},
		Team:      "experimental",
		Lane:      "elastic",
		Mode:      "elastic",
		Topology:  "independent",
		GPUClass:  "h100-95gb",
	})
	if err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("expected checkpoint error, got %v", err)
	}
}

func TestRender_ElasticUsesLowPriorityAndSharedQueue(t *testing.T) {
	p := trainProfile()
	p.Spec["policy"] = map[string]any{
		"preemptable":         true,
		"checkpointOnPreempt": true,
	}
	out, err := Render(p, Options{
		Name:      "elastic",
		Namespace: "tau",
		Command:   []string{"true"},
		Team:      "experimental",
		Lane:      "elastic",
		Mode:      "elastic",
		Topology:  "independent",
		GPUClass:  "h100-95gb",
		Shape:     "1xh100-95gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	meta := m["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	if labels["kueue.x-k8s.io/queue-name"] != runtopology.SharedGPUQueueName {
		t.Fatalf("queue label=%v want %s", labels["kueue.x-k8s.io/queue-name"], runtopology.SharedGPUQueueName)
	}
	if labels["kueue.x-k8s.io/priority-class"] != "taugrid-default" {
		t.Fatalf("workload priority=%v", labels["kueue.x-k8s.io/priority-class"])
	}
	spec := m["spec"].(map[string]any)
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	nodeSelector := pod["nodeSelector"].(map[string]any)
	if nodeSelector[workloadmeta.NodeLabelGPUClass] != runtopology.GPUClassH10095GB {
		t.Fatalf("gpu class node selector=%v want %s", nodeSelector, runtopology.GPUClassH10095GB)
	}
	if pod["priorityClassName"] != "taugrid-default" {
		t.Fatalf("pod priority=%v", pod["priorityClassName"])
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations["kueue.x-k8s.io/podset-unconstrained-topology"] != "true" {
		t.Fatalf("elastic topology annotation missing: %v", annotations)
	}
}

func TestRender_NoImage_NoOverride_Errors(t *testing.T) {
	p := trainProfile()
	delete(p.Spec, "runtime")
	_, err := Render(p, Options{Name: "j1", Namespace: "tau", Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("expected no-image error, got: %v", err)
	}
}

func TestRender_NoCommand_NoScript_UsesImageEntrypoint(t *testing.T) {
	out, err := Render(trainProfile(), Options{Name: "j1", Namespace: "tau"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	c := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if _, ok := c["command"]; ok {
		t.Errorf("container.command must be omitted to honor image ENTRYPOINT/CMD; got %v", c["command"])
	}
	if c["image"] == nil || c["image"] == "" {
		t.Errorf("container.image must still be set: %v", c["image"])
	}
}

func TestRender_NoNamespace_Errors(t *testing.T) {
	_, err := Render(trainProfile(), Options{Name: "j1", Command: []string{"true"}})
	if err == nil {
		t.Errorf("expected namespace error, got nil")
	}
}

func TestRender_Retry_SetsBackoffLimit(t *testing.T) {
	out, err := Render(trainProfile(), Options{Name: "j", Namespace: "tau", Command: []string{"true"}, Retry: 3})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	spec := m["spec"].(map[string]any)
	bl, _ := spec["backoffLimit"].(int)
	bl64, _ := spec["backoffLimit"].(int64)
	if bl != 3 && bl64 != 3 {
		t.Errorf("backoffLimit=%v want 3", spec["backoffLimit"])
	}
}

func TestRender_Retry_Negative_Errors(t *testing.T) {
	_, err := Render(trainProfile(), Options{Name: "j", Namespace: "tau", Command: []string{"true"}, Retry: -1})
	if err == nil || !strings.Contains(err.Error(), "--retry") {
		t.Errorf("expected --retry negative error, got %v", err)
	}
}

func TestRender_GracePeriod_DefaultIs600(t *testing.T) {
	out, err := Render(trainProfile(), Options{Name: "j", Namespace: "tau", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	g := pod["terminationGracePeriodSeconds"]
	gi, _ := g.(int)
	gi64, _ := g.(int64)
	if gi != 600 && gi64 != 600 {
		t.Errorf("terminationGracePeriodSeconds=%v want 600", g)
	}
}

func TestRender_GracePeriod_OptionOverride(t *testing.T) {
	out, err := Render(trainProfile(), Options{Name: "j", Namespace: "tau", Command: []string{"true"}, TerminationGracePeriodSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	g := pod["terminationGracePeriodSeconds"]
	gi, _ := g.(int)
	gi64, _ := g.(int64)
	if gi != 300 && gi64 != 300 {
		t.Errorf("terminationGracePeriodSeconds=%v want 300", g)
	}
}

func TestRender_GracePeriod_ProfilePolicyOverride(t *testing.T) {
	p := trainProfile()
	pol, ok := p.Spec["policy"].(map[string]any)
	if !ok {
		pol = map[string]any{}
		p.Spec["policy"] = pol
	}
	pol["terminationGracePeriodSeconds"] = 120
	out, err := Render(p, Options{Name: "j", Namespace: "tau", Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	g := pod["terminationGracePeriodSeconds"]
	gi, _ := g.(int)
	gi64, _ := g.(int64)
	if gi != 120 && gi64 != 120 {
		t.Errorf("terminationGracePeriodSeconds=%v want 120 (profile policy)", g)
	}
}

func TestRender_NoDRA_NoClaimsAttached(t *testing.T) {
	p := trainProfile()
	res := p.Spec["resources"].(map[string]any)
	delete(res, "dra")
	res["gpu"].(map[string]any)["requestVia"] = "device-plugin"
	out, err := Render(p, Options{
		Name:          "j1",
		Namespace:     "tau",
		Command:       []string{"true"},
		CPURequest:    "4",
		MemoryRequest: "16Gi",
		CPULimit:      "8",
		MemoryLimit:   "32Gi",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if _, ok := pod["resourceClaims"]; ok {
		t.Errorf("pod must not have resourceClaims with device-plugin GPU: %v", pod["resourceClaims"])
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	cres := c["resources"].(map[string]any)
	if _, ok := cres["claims"]; ok {
		t.Errorf("container must not have resources.claims with device-plugin GPU: %v", cres)
	}
	reqs := cres["requests"].(map[string]any)
	lims := cres["limits"].(map[string]any)
	if fmt.Sprint(reqs["nvidia.com/gpu"]) != "1" || fmt.Sprint(lims["nvidia.com/gpu"]) != "1" {
		t.Errorf("device-plugin GPU must set nvidia.com/gpu=1 in requests+limits: reqs=%v lims=%v", reqs, lims)
	}
	for key, want := range map[string]string{"cpu": "4", "memory": "16Gi"} {
		if fmt.Sprint(reqs[key]) != want {
			t.Errorf("requests[%s]=%v want %s: %v", key, reqs[key], want, reqs)
		}
	}
	for key, want := range map[string]string{"cpu": "8", "memory": "32Gi"} {
		if fmt.Sprint(lims[key]) != want {
			t.Errorf("limits[%s]=%v want %s: %v", key, lims[key], want, lims)
		}
	}
}

// envFromContainer returns the rendered container's env as a flat map.
func envFromContainer(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	envAny, _ := c["env"].([]any)
	out := map[string]string{}
	for _, item := range envAny {
		e := item.(map[string]any)
		name, _ := e["name"].(string)
		val, _ := e["value"].(string)
		out[name] = val
	}
	return out
}

func TestRender_EnvFlagInjectsUserEnv(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "j1",
		Namespace: "tau",
		Command:   []string{"true"},
		Env: map[string]string{
			"FOO":         "bar",
			"WANDB_GROUP": "lora-sweep",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	env := envFromContainer(t, parseYAML(t, out))
	if env["FOO"] != "bar" || env["WANDB_GROUP"] != "lora-sweep" {
		t.Fatalf("user env not injected: %+v", env)
	}
}

func TestRender_EnvFlagWinsOverProfileEnv(t *testing.T) {
	p := trainProfile()
	rt := p.Spec["runtime"].(map[string]any)
	rt["env"] = map[string]any{"FOO": "from-profile"}
	out, err := Render(p, Options{
		Name:      "j1",
		Namespace: "tau",
		Command:   []string{"true"},
		Env:       map[string]string{"FOO": "from-cli"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	env := envFromContainer(t, parseYAML(t, out))
	if env["FOO"] != "from-cli" {
		t.Fatalf("user --env should win over profile env: got %q", env["FOO"])
	}
}

func TestRender_EnvFlagRejectsReservedPrefixes(t *testing.T) {
	cases := []string{"TAU_DATA_DIR", "TAU_HOT_DIR"}
	for _, k := range cases {
		_, err := Render(trainProfile(), Options{
			Name:      "j1",
			Namespace: "tau",
			Command:   []string{"true"},
			Env:       map[string]string{k: "v"},
		})
		if err == nil {
			t.Fatalf("--env %s: expected reserved-prefix error", k)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("--env %s: error %q should mention 'reserved'", k, err)
		}
	}
}

func TestRender_EnvFlagAcceptsRetryEnvVars(t *testing.T) {
	for _, k := range []string{"TAU_RESUME_FROM", "TAU_RETRY_ATTEMPT", "TAU_RETRY_MAX", "TAU_RETRY_REASON"} {
		_, err := Render(trainProfile(), Options{
			Name: "j1", Namespace: "tau", Command: []string{"true"},
			PVCMount: "blob-training",
			Env:      map[string]string{k: "test-value"},
		})
		if err != nil {
			t.Errorf("TAU env var %s should be allowed but got: %v", k, err)
		}
	}
}

// tauEnvContractCases spans both sides of the namespace rule. The permitted
// keys are derived from runconfig rather than retyped, so a key added there is
// covered here without an edit; the fixed extras cover keys the old denylist
// named, a key no list mentions, a lowercase spelling, and names that only look
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

// This renderer used to carry its own copy of the TAU_ allowlist, byte-identical
// to the RayJob renderer's and unrelated to the denylist that ran earlier in
// core/runconfig. Divergence between any two of the three presented as "this env
// var works for a Job but not a RayJob". Driving Render rather than calling the
// predicate directly is the point: this fails if the wiring is dropped, not just
// if the rule changes.
func TestRender_TauEnvGateAgreesWithRunconfig(t *testing.T) {
	for _, name := range tauEnvContractCases {
		t.Run(name, func(t *testing.T) {
			_, err := Render(trainProfile(), Options{
				Name: "j1", Namespace: "tau", Command: []string{"true"},
				PVCMount: "blob-training",
				Env:      map[string]string{name: "v"},
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

func TestRender_TauEnvSecretGateAgreesWithRunconfig(t *testing.T) {
	for _, name := range tauEnvContractCases {
		t.Run(name, func(t *testing.T) {
			_, err := Render(trainProfile(), Options{
				Name: "j1", Namespace: "tau", Command: []string{"true"},
				PVCMount:   "blob-training",
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

func TestRender_Profiler_NCU_WrapsCommandAndSetsEnv(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "bench-001",
		Namespace: "tau",
		Command:   []string{"python", "-m", "swordfish.runner"},
		PVCMount:  "blob-training",
		Profile:   ProfileOptions{Mode: "ncu"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	env := envFromContainer(t, m)
	if env["TAU_PROFILE_MODE"] != "ncu" {
		t.Errorf("TAU_PROFILE_MODE=%q want ncu", env["TAU_PROFILE_MODE"])
	}
	if env["TAU_PROFILE_OUT"] != "/data/bench-001/profile/rank-0.ncu-rep" {
		t.Errorf("TAU_PROFILE_OUT=%q", env["TAU_PROFILE_OUT"])
	}
	if env["TAU_PROFILE_OUT_DIR"] != "/data/bench-001/profile" {
		t.Errorf("TAU_PROFILE_OUT_DIR=%q", env["TAU_PROFILE_OUT_DIR"])
	}
	if env["TAU_PROFILE_RANK"] != "0" {
		t.Errorf("TAU_PROFILE_RANK=%q", env["TAU_PROFILE_RANK"])
	}
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	cmd := c["command"].([]any)
	script := cmd[2].(string)
	if !strings.Contains(script, "ncu --target-processes all") {
		t.Errorf("entrypoint script missing ncu invocation: %s", script)
	}
	if !strings.Contains(script, `command -v "$TAU_PROFILE_TOOL"`) {
		t.Errorf("entrypoint script missing ncu presence guard: %s", script)
	}
}

func TestRender_Profiler_NSYS_PicksNsysExtensionAndCaptureWindow(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "bench-002",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
		Profile: ProfileOptions{
			Mode:     "nsys",
			Rank:     "0",
			Warmup:   30 * time.Second,
			Duration: 2 * time.Minute,
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	env := envFromContainer(t, parseYAML(t, out))
	if env["TAU_PROFILE_OUT"] != "/data/bench-002/profile/rank-0.nsys-rep" {
		t.Errorf("TAU_PROFILE_OUT=%q", env["TAU_PROFILE_OUT"])
	}
	if env["TAU_PROFILE_WARMUP_SEC"] != "30" || env["TAU_PROFILE_ACTIVE_SEC"] != "120" {
		t.Errorf("profile capture env wrong: %+v", env)
	}
	pod := parseYAML(t, out)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	script := pod["containers"].([]any)[0].(map[string]any)["command"].([]any)[2].(string)
	if !strings.Contains(script, "nsys profile") {
		t.Errorf("entrypoint script missing nsys invocation: %s", script)
	}
	if !strings.Contains(script, "--duration") || !strings.Contains(script, "--delay") {
		t.Errorf("entrypoint script missing bounded nsys capture flags: %s", script)
	}
}

func TestProfileWrapperAvoidsOptionalImageUtilities(t *testing.T) {
	wrapper := profileModeWrapperScript()
	for _, unwanted := range []string{
		"awk ",
		"hostname 2>",
		"head -n",
		" tr ",
	} {
		if strings.Contains(wrapper, unwanted) {
			t.Fatalf("profile wrapper should not require optional utility %q:\n%s", unwanted, wrapper)
		}
	}
}

func TestProfileWrapperEscapesControlCharactersInMetadataJSON(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ncu := filepath.Join(binDir, "ncu")
	if err := os.WriteFile(ncu, []byte(`#!/usr/bin/env bash
case "${1:-}" in
  --version)
    printf 'NVIDIA Nsight Compute\001\013\037\n'
    exit 0
    ;;
esac
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; out="$1" ;;
    --) break ;;
  esac
  shift || true
done
if [ -z "$out" ]; then exit 2; fi
mkdir -p "$(dirname "$out")"
: > "$out.ncu-rep"
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}

	profileDir := filepath.Join(dir, "profile")
	cmd := exec.Command("bash", "-lc", profileModeWrapperScript(), "tau-profile-test", "python3", "train.py")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOSTNAME=host-\x1f",
		"TAU_PROFILE_MODE=ncu",
		"TAU_PROFILE_TOOL=ncu",
		"TAU_PROFILE_RANK=0",
		"TAU_PROFILE_OUT_DIR="+profileDir,
		"TAU_PROFILE_RUN_ID=control-\x01-run",
		"TAU_PROFILE_NAMESPACE=tau-\x02",
		"TAU_PROFILE_EXT=ncu-rep",
		"TAU_PROFILE_WARMUP_SEC=0",
		"TAU_PROFILE_ACTIVE_SEC=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper should write valid metadata with control chars; err=%v output=%s", err, out)
	}

	raw, err := os.ReadFile(filepath.Join(profileDir, "rank-0.metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("metadata json should escape control chars: %v\n%s", err, raw)
	}
	if got := meta["run_id"]; got != "control-\x01-run" {
		t.Fatalf("run_id=%q", got)
	}
	for _, want := range []string{`\u0001`, `\u0002`, `\u000b`, `\u001f`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("metadata missing escaped control %s:\n%s", want, raw)
		}
	}
}

func TestProfileWrapperTreatsNsysDuration143AsSuccessfulCapture(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nsys := filepath.Join(binDir, "nsys")
	if err := os.WriteFile(nsys, []byte(`#!/usr/bin/env bash
case "${1:-}" in
  --version)
    echo "NVIDIA Nsight Systems version 2025.5.2"
    exit 0
    ;;
  profile)
    out=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -o) shift; out="$1" ;;
        --) break ;;
      esac
      shift || true
    done
    if [ -z "$out" ]; then exit 2; fi
    mkdir -p "$(dirname "$out")"
    : > "$out.nsys-rep"
    exit 143
    ;;
  export)
    out=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --output) shift; out="$1" ;;
      esac
      shift || true
    done
    if [ -z "$out" ]; then exit 2; fi
    : > "$out"
    exit 0
    ;;
  *)
    exit 2
    ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}

	profileDir := filepath.Join(dir, "profile")
	cmd := exec.Command("bash", "-lc", profileModeWrapperScript(), "tau-profile-test", "python3", "train.py")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOSTNAME=test-host",
		"TAU_PROFILE_MODE=nsys",
		"TAU_PROFILE_TOOL=nsys",
		"TAU_PROFILE_RANK=0",
		"TAU_PROFILE_OUT_DIR="+profileDir,
		"TAU_PROFILE_RUN_ID=duration-smoke",
		"TAU_PROFILE_NAMESPACE=tau",
		"TAU_PROFILE_EXT=nsys-rep",
		"TAU_PROFILE_WARMUP_SEC=0",
		"TAU_PROFILE_ACTIVE_SEC=12",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper should exit successfully after duration-limited capture; err=%v output=%s", err, out)
	}

	raw, err := os.ReadFile(filepath.Join(profileDir, "rank-0.metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("metadata json: %v\n%s", err, raw)
	}
	if got := meta["exit_status"]; got != float64(0) {
		t.Fatalf("exit_status=%v want 0; metadata=%v", got, meta)
	}
	if got := meta["target_exit_status"]; got != float64(143) {
		t.Fatalf("target_exit_status=%v want 143; metadata=%v", got, meta)
	}
	if got := meta["completion_reason"]; got != "nsys-duration-capture-complete" {
		t.Fatalf("completion_reason=%v; metadata=%v", got, meta)
	}
	if got := meta["export_status"]; got != "ok" {
		t.Fatalf("export_status=%v; metadata=%v", got, meta)
	}
}

func TestRender_Profiler_AllRanksUsesOutputPattern(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "bench-all",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
		Profile: ProfileOptions{
			Mode: "nsys",
			Rank: "all",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	env := envFromContainer(t, parseYAML(t, out))
	if env["TAU_PROFILE_OUT"] != "" {
		t.Fatalf("TAU_PROFILE_OUT should be omitted for rank=all, got %q", env["TAU_PROFILE_OUT"])
	}
	if env["TAU_PROFILE_OUT_PATTERN"] != "/data/bench-all/profile/rank-<rank>.nsys-rep" {
		t.Fatalf("TAU_PROFILE_OUT_PATTERN=%q", env["TAU_PROFILE_OUT_PATTERN"])
	}
}

func TestRender_Profiler_RejectsUnknownMode(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "j1",
		Namespace: "tau",
		Command:   []string{"true"},
		Profile:   ProfileOptions{Mode: "vtune"},
	})
	if err == nil {
		t.Fatal("expected error for unknown profile mode")
	}
}

func TestRender_Profiler_RequiresCommand(t *testing.T) {
	// Image-only entrypoint (no Command, no Script) cannot be wrapped.
	_, err := Render(trainProfile(), Options{
		Name:      "j1",
		Namespace: "tau",
		Profile:   ProfileOptions{Mode: "ncu"},
	})
	if err == nil {
		t.Fatal("expected error: --profile-mode without command should fail")
	}
	if !strings.Contains(err.Error(), "image-only") && !strings.Contains(err.Error(), "wrap") {
		t.Errorf("error %q should mention image-only/wrap", err)
	}
}

func TestRender_Profiler_RequiresDurableOutput(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "j1",
		Namespace: "tau",
		Command:   []string{"true"},
		Profile:   ProfileOptions{Mode: "nsys"},
	})
	if err == nil {
		t.Fatal("expected error: --profiler without durable output should fail")
	}
	if !strings.Contains(err.Error(), "durable output") {
		t.Errorf("error %q should mention durable output", err)
	}
}

func TestRender_OutputAnnotationsRoundtripFromCLI(t *testing.T) {
	// Render-level surface only sees Annotations; the CLI builds them. This
	// asserts they survive the render unchanged so tau run get can read them.
	out, err := Render(trainProfile(), Options{
		Name:      "j1",
		Namespace: "tau",
		Command:   []string{"true"},
		PVCMount:  "blob-training",
		Annotations: map[string]string{
			workloadmeta.AnnotationResultPath: "/data/j1/results",
			workloadmeta.AnnotationResultPVC:  "blob-training",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	ann := m["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[workloadmeta.AnnotationResultPath] != "/data/j1/results" {
		t.Errorf("result-path annotation=%v", ann[workloadmeta.AnnotationResultPath])
	}
	if ann[workloadmeta.AnnotationResultPVC] != "blob-training" {
		t.Errorf("result-pvc annotation=%v", ann[workloadmeta.AnnotationResultPVC])
	}
}

func torchrunScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte("import torch\ntorch.distributed.init_process_group()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

func shellScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nset -euo pipefail\necho train\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func multiGPUProfile(gpuCount int) profile.Profile {
	return profile.Profile{
		Name: "ai-train-gpu-multi",
		Spec: map[string]any{
			"queue": map[string]any{
				"clusterQueue": "training-cq",
				"localQueue":   "training-queue",
			},
			"scheduling": map[string]any{
				"priorityClassName": "taugrid-default",
			},
			"resources": map[string]any{
				"gpu":      map[string]any{"count": gpuCount, "size": "l", "placement": "same-node", "requestVia": "device-plugin"},
				"requests": map[string]any{"cpu": "64", "memory": "256Gi"},
			},
			"runtime": map[string]any{
				"image": "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0",
			},
		},
	}
}

func noGPUCountProfile() profile.Profile {
	return profile.Profile{
		Name: "ai-train-dra-dynamic",
		Spec: map[string]any{
			"queue": map[string]any{
				"clusterQueue": "training-cq",
				"localQueue":   "training-queue",
			},
			"scheduling": map[string]any{
				"priorityClassName": "taugrid-default",
			},
			"resources": map[string]any{
				"gpu":      map[string]any{"size": "l"},
				"requests": map[string]any{"cpu": "16", "memory": "64Gi"},
			},
			"runtime": map[string]any{
				"image": "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0",
			},
		},
	}
}

func TestRender_Torchrun_DefaultPPN(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(trainProfile(), Options{
		Name:       "ddp-single",
		Namespace:  "tau",
		ScriptPath: script,
		Launcher:   "torchrun",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "nproc_per_node=1") {
		t.Errorf("torchrun default PPN should be 1:\n%s", s)
	}
	if !strings.Contains(s, "tau-entrypoint.py") {
		t.Errorf("torchrun should use tau-entrypoint.py:\n%s", s)
	}
	if !strings.Contains(s, "torch.distributed.run") {
		t.Errorf("torchrun should use python3 -m torch.distributed.run:\n%s", s)
	}
	env := envFromContainer(t, parseYAML(t, out))
	if env["MASTER_ADDR"] != "localhost" {
		t.Errorf("MASTER_ADDR=%q want localhost", env["MASTER_ADDR"])
	}
	if env["MASTER_PORT"] != "29500" {
		t.Errorf("MASTER_PORT=%q want 29500", env["MASTER_PORT"])
	}
	if _, ok := env["OMP_NUM_THREADS"]; ok {
		t.Errorf("OMP_NUM_THREADS should not be set for PPN=1")
	}
}

func TestRender_Torchrun_MultiProcess(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:             "ddp-8gpu",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 8,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "nproc_per_node=8") {
		t.Errorf("torchrun should set nproc_per_node=8:\n%s", s)
	}
	env := envFromContainer(t, parseYAML(t, out))
	if env["OMP_NUM_THREADS"] != "1" {
		t.Errorf("OMP_NUM_THREADS=%q want 1 (prevents oversubscription)", env["OMP_NUM_THREADS"])
	}
}

func TestRender_Torchrun_DevShm(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(4), Options{
		Name:             "ddp-shm",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 4,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	vols, _ := pod["volumes"].([]any)
	found := false
	for _, v := range vols {
		vol := v.(map[string]any)
		if vol["name"] == "dshm" {
			emptyDir, _ := vol["emptyDir"].(map[string]any)
			if emptyDir["medium"] != "Memory" {
				t.Errorf("dshm volume should be medium=Memory: %v", emptyDir)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("/dev/shm volume missing for PPN>1")
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	mounts, _ := c["volumeMounts"].([]any)
	shmMounted := false
	for _, vm := range mounts {
		mount := vm.(map[string]any)
		if mount["name"] == "dshm" && mount["mountPath"] == "/dev/shm" {
			shmMounted = true
		}
	}
	if !shmMounted {
		t.Errorf("/dev/shm mount missing for PPN>1")
	}
}

func TestRender_Torchrun_NoDevShm_WhenPPN1(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(trainProfile(), Options{
		Name:             "ddp-no-shm",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "dshm") {
		t.Errorf("/dev/shm volume should not be added for PPN=1:\n%s", s)
	}
}

func TestRender_Torchrun_ExecutionAnnotation(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(4), Options{
		Name:             "ddp-ann",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 4,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	m := parseYAML(t, out)
	ann := m["metadata"].(map[string]any)["annotations"].(map[string]any)
	execJSON, ok := ann[workloadmeta.AnnotationSpecExecution].(string)
	if !ok {
		t.Fatalf("execution annotation missing")
	}
	var exec struct {
		Launcher         string `json:"launcher"`
		ProcessesPerNode int    `json:"processes_per_node"`
	}
	if err := json.Unmarshal([]byte(execJSON), &exec); err != nil {
		t.Fatalf("parse execution annotation: %v", err)
	}
	if exec.Launcher != "torchrun" || exec.ProcessesPerNode != 4 {
		t.Errorf("execution annotation=%+v, want torchrun/4", exec)
	}
}

func TestRender_Torchrun_NoAnnotation_ForPython(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "plain-python",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "workloadspec-execution") {
		t.Errorf("execution annotation should not be set for plain python launcher:\n%s", s)
	}
}

func TestRender_Torchrun_PPNExceedsGPUCount(t *testing.T) {
	script := torchrunScript(t)
	_, err := Render(trainProfile(), Options{
		Name:             "ddp-over",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 4,
	})
	if err == nil {
		t.Fatal("expected error: PPN=4 exceeds profile GPU count=1")
	}
	if !strings.Contains(err.Error(), "exceeds profile GPU count") {
		t.Errorf("error %q should mention exceeds profile GPU count", err)
	}
}

func TestRender_Torchrun_PPNUnknownGPUCount(t *testing.T) {
	script := torchrunScript(t)
	_, err := Render(noGPUCountProfile(), Options{
		Name:             "ddp-dra",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 4,
	})
	if err == nil {
		t.Fatal("expected error: PPN>1 with unknown GPU count should be rejected")
	}
	if !strings.Contains(err.Error(), "known GPU count") {
		t.Errorf("error %q should mention known GPU count", err)
	}
}

func TestRender_PythonLauncher_PPNGreaterThan1_Rejected(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:             "bad-ppn",
		Namespace:        "tau",
		Command:          []string{"python", "train.py"},
		Launcher:         "python",
		ProcessesPerNode: 4,
	})
	if err == nil {
		t.Fatal("expected error: PPN>1 with python launcher")
	}
	if !strings.Contains(err.Error(), "requires launcher=torchrun") {
		t.Errorf("error %q should mention requires launcher=torchrun", err)
	}
}

func TestRender_PPN_Negative_Rejected(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:             "bad-ppn",
		Namespace:        "tau",
		Command:          []string{"python", "train.py"},
		ProcessesPerNode: -1,
	})
	if err == nil {
		t.Fatal("expected error: negative PPN")
	}
	if !strings.Contains(err.Error(), "must be >= 0") {
		t.Errorf("error %q should mention must be >= 0", err)
	}
}

func TestRender_Torchrun_RequiresScript(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "no-script",
		Namespace: "tau",
		Launcher:  "torchrun",
	})
	if err == nil {
		t.Fatal("expected error: torchrun without script")
	}
	if !strings.Contains(err.Error(), "requires --script") {
		t.Errorf("error %q should mention requires --script", err)
	}
}

func TestRender_Torchrun_ShellEntrypointRejected(t *testing.T) {
	script := shellScript(t)
	_, err := Render(trainProfile(), Options{
		Name:       "ddp-shell",
		Namespace:  "tau",
		ScriptPath: script,
		Launcher:   "torchrun",
	})
	if err == nil {
		t.Fatal("expected error: torchrun runs the entrypoint under python3, so a shell script cannot work")
	}
	if !strings.Contains(err.Error(), "must be a .py file") {
		t.Errorf("error %q should mention must be a .py file", err)
	}
}

// The default launcher decodes to /tmp/run.sh and execs it, so the shebang
// picks the interpreter. Shell entrypoints must keep working there.
func TestRender_PythonLauncher_ShellEntrypointAccepted(t *testing.T) {
	script := shellScript(t)
	out, err := Render(trainProfile(), Options{
		Name:       "shell-ok",
		Namespace:  "tau",
		ScriptPath: script,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "/tmp/run.sh") {
		t.Errorf("default launcher should exec /tmp/run.sh:\n%s", out)
	}
}

func TestRender_Torchrun_ProfilerRejected(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "bad-combo",
		Namespace: "tau",
		Command:   []string{"true"},
		Launcher:  "torchrun",
		Profile:   ProfileOptions{Mode: "nsys"},
	})
	if err == nil {
		t.Fatal("expected error: torchrun+profiler")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error %q should mention cannot be combined", err)
	}
}

func TestRender_DirectJobMetricsOffloadContract(t *testing.T) {
	script := torchrunScript(t)
	runtime := metricsoffload.Runtime{
		Image:               "registry.example.com/taugrid/tau:v0.6.0",
		RunID:               "modernbert-bounded",
		Project:             "pretraining",
		Experiment:          "modernbert-fineweb",
		Group:               "fwe100",
		Tags:                map[string]string{"tau_workspace": "research-workspace", "tau_namespace": "research-workspace", "tau_cluster": "sample-gpu-cluster"},
		Source:              "stellar-online",
		Store:               "/data/research-workspace/modernbert-bounded/.tau/metrics-expstore",
		Out:                 "/data/research-workspace/modernbert-bounded/.tau/metrics-offload",
		History:             []string{"/data/research-workspace/modernbert-bounded/metrics-history-attempt-*/*.jsonl"},
		CompletionFile:      "/var/run/tau/metrics-completion.json",
		RemoteWriteEndpoint: "http://${NODE_IP}:3100/receive",
		Interval:            10 * time.Second,
		ArtifactURI:         "/data/research-workspace/modernbert-bounded",
		CheckpointURI:       "/data/research-workspace/modernbert-bounded/checkpoints",
	}
	out, err := Render(trainProfile(), Options{
		Name:           "modernbert-bounded",
		Namespace:      "research-workspace",
		ScriptPath:     script,
		PVCMount:       "research-workspace",
		MetricsOffload: runtime,
		ArtifactBundle: artifactbundle.Runtime{
			BundleID:          "bundle-1",
			Run:               "modernbert-bounded",
			Namespace:         "research-workspace",
			ResultPVC:         "research-workspace",
			OutputDir:         "/data/research-workspace/modernbert-bounded",
			MetricsSessionID:  "metrics-1",
			MetricsHistory:    runtime.History,
			MetricsOffloadDir: runtime.Out,
			MetricsEnabled:    true,
		},
		Annotations: map[string]string{
			workloadmeta.AnnotationExperimentSource: "stellar",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	job := parseYAML(t, out)
	spec := job["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	podMeta := template["metadata"].(map[string]any)
	if got := podMeta["annotations"].(map[string]any)[workloadmeta.AnnotationExperimentSource]; got != "stellar" {
		t.Fatalf("pod experiment source = %v, want stellar", got)
	}
	pod := template["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	if len(containers) != 2 {
		t.Fatalf("containers = %d, want main + metrics-offload", len(containers))
	}
	main := containers[0].(map[string]any)
	sidecar := containers[1].(map[string]any)
	if sidecar["name"] != "metrics-offload" || sidecar["image"] != runtime.Image {
		t.Fatalf("unexpected sidecar: %+v", sidecar)
	}
	if got := sidecar["command"].([]any); len(got) != 1 || got[0] != metricsoffload.SidecarCommand {
		t.Fatalf("sidecar command = %v", got)
	}
	args := fmt.Sprint(sidecar["args"])
	for _, want := range []string{
		"experiment offload metrics --watch",
		"--history " + runtime.History[0],
		"--completion-file " + runtime.CompletionFile,
		"--remote-write-endpoint " + runtime.RemoteWriteEndpoint,
		"--tag tau_workspace=research-workspace",
		"--tag tau_namespace=research-workspace",
		"--tag tau_cluster=sample-gpu-cluster",
		"--status-artifact-uri " + runtime.ArtifactURI,
		"--status-checkpoint-uri " + runtime.CheckpointURI,
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("sidecar args missing %q: %s", want, args)
		}
	}
	mainCommand := fmt.Sprint(main["command"])
	for _, want := range []string{runtime.CompletionFile, `tau_metrics_state="succeeded"`, `tau_metrics_state="failed"`, `tau_metrics_state="cancelled"`, `mv -f "$tau_metrics_tmp"`} {
		if !strings.Contains(mainCommand, want) {
			t.Fatalf("main lifecycle wrapper missing %q:\n%s", want, mainCommand)
		}
	}
	command := main["command"].([]any)
	if len(command) < 5 || command[0] != "bash" || command[1] != "-c" ||
		command[3] != "tau-bundle-entrypoint" || command[4] != "bash" {
		t.Fatalf("artifact bundle must wrap metrics lifecycle command: %v", command)
	}
	bundleScript := command[2].(string)
	for _, want := range []string{"tau_bundle_child", ".tau/bundle.complete", "bundle-1"} {
		if !strings.Contains(bundleScript, want) {
			t.Fatalf("bundle lifecycle wrapper missing %q:\n%s", want, bundleScript)
		}
	}
	volumes := fmt.Sprint(pod["volumes"])
	if !strings.Contains(volumes, "tau-metrics-runtime") || !strings.Contains(volumes, "emptyDir") {
		t.Fatalf("pod-local metrics runtime volume missing: %s", volumes)
	}
	for name, container := range map[string]map[string]any{"main": main, "sidecar": sidecar} {
		mounts := fmt.Sprint(container["volumeMounts"])
		if !strings.Contains(mounts, "mountPath:/data") || !strings.Contains(mounts, "mountPath:/var/run/tau") {
			t.Fatalf("%s mounts do not share durable data and pod-local runtime: %s", name, mounts)
		}
	}
	rendered := string(out)
	for _, forbidden := range []string{"clientSecret", "accountKey", "sasToken", "AZURE_CLIENT_SECRET"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rendered telemetry contract must not contain credentials (%q):\n%s", forbidden, rendered)
		}
	}
}

func TestRender_ArtifactBundleRejectsMultiNodeIndexedJob(t *testing.T) {
	script := torchrunScript(t)
	_, err := Render(trainProfile(), Options{
		Name:       "multi-node",
		Namespace:  "research",
		ScriptPath: script,
		Launcher:   "torchrun",
		Nodes:      2,
		PVCMount:   "blob-training",
		OutputDir:  "/data/runs/multi-node",
		ArtifactBundle: artifactbundle.Runtime{
			BundleID:  "bundle-1",
			Run:       "multi-node",
			Namespace: "research",
			ResultPVC: "blob-training",
			OutputDir: "/data/runs/multi-node",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "single Job pod") {
		t.Fatalf("multi-node bundle error = %v", err)
	}
}

func TestRender_DirectJobMetricsOffloadSafety(t *testing.T) {
	script := torchrunScript(t)
	readOnlyProfile := trainProfile()
	readOnlyProfile.Spec["resources"].(map[string]any)["persistence"] = map[string]any{
		"pvcName":   "data",
		"mountPath": "/data",
		"readOnly":  true,
	}
	valid := metricsoffload.Runtime{
		Image:               "registry.example.com/taugrid/tau:v0.6.0",
		RunID:               "bounded",
		Project:             "pretraining",
		Experiment:          "bounded-experiment",
		Group:               "fwe100",
		Store:               "/data/bounded/.tau/store",
		Out:                 "/data/bounded/.tau/offload",
		History:             []string{"/data/bounded/metrics-history.jsonl"},
		CompletionFile:      "/var/run/tau/metrics-completion.json",
		RemoteWriteEndpoint: "http://${NODE_IP}:3100/receive",
		Interval:            10 * time.Second,
	}
	tests := []struct {
		name    string
		profile profile.Profile
		options Options
		want    string
	}{
		{
			name:    "durable PVC required",
			options: Options{Name: "bounded", Namespace: "tau", ScriptPath: script, MetricsOffload: valid},
			want:    "durable PVC storage",
		},
		{
			name:    "single pod required",
			options: Options{Name: "bounded", Namespace: "tau", ScriptPath: script, PVCMount: "data", Nodes: 2, Launcher: "torchrun", MetricsOffload: valid},
			want:    "single Job pod",
		},
		{
			name:    "wrappable command required",
			options: Options{Name: "bounded", Namespace: "tau", PVCMount: "data", MetricsOffload: valid},
			want:    "Tau-wrappable script or explicit command",
		},
		{
			name:    "writable durable PVC required",
			profile: readOnlyProfile,
			options: Options{Name: "bounded", Namespace: "tau", ScriptPath: script, MetricsOffload: valid},
			want:    "writable durable PVC storage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.profile
			if p.Name == "" {
				p = trainProfile()
			}
			_, err := Render(p, tt.options)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Render() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func testMetricsRuntime(completion, ready string, readyTimeout time.Duration) metricsoffload.Runtime {
	return metricsoffload.Runtime{
		Image:               "example.test/tau:v1",
		RunID:               "run-1",
		Project:             "project-1",
		Experiment:          "experiment-1",
		Group:               "group-1",
		Store:               "/data/store",
		Out:                 "/data/out",
		History:             []string{"/data/history.jsonl"},
		CompletionFile:      completion,
		RemoteWriteEndpoint: "http://127.0.0.1:3100/receive",
		Interval:            time.Second,
		ReadyFile:           ready,
		ReadyTimeout:        readyTimeout,
	}
}

func TestRenderRejectsStagedPublicationForMultiNodeJob(t *testing.T) {
	script := filepath.Join(t.TempDir(), "train.py")
	if err := os.WriteFile(script, []byte("print('train')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Render(trainProfile(), Options{
		Name:             "multi-node-publish",
		Namespace:        "ray",
		Image:            "example.test/trainer:v1",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 1,
		Nodes:            2,
		PVCMount:         "training-data",
		ArtifactPublish: artifactpublish.Runtime{
			Mode:          artifactpublish.ModeStaged,
			OutputDir:     "/data/runs/multi-node-publish",
			StagingDir:    "/mnt/tau-output/multi-node-publish",
			PublicationID: "publication-1",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires a single Job pod") {
		t.Fatalf("multi-node staged publication error = %v", err)
	}
}

func TestMetricsCompletionWrapperPreservesTerminalStateAndExitCode(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantCode   int
		wantState  string
		wantReason string
	}{
		{name: "succeeded", command: "exit 0", wantCode: 0, wantState: "succeeded", wantReason: "workload-entrypoint-exit"},
		{name: "failed", command: "exit 7", wantCode: 7, wantState: "failed", wantReason: "workload-entrypoint-exit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			completion := filepath.Join(dir, "metrics-completion.json")
			ready := filepath.Join(dir, "metrics-ready")
			if err := os.WriteFile(ready, []byte("ready\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			wrapped, err := metricsoffload.WrapCommand([]string{"bash", "-c", tt.command}, testMetricsRuntime(completion, ready, time.Second))
			if err != nil {
				t.Fatal(err)
			}
			err = exec.Command(wrapped[0], wrapped[1:]...).Run()
			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("wrapped command failed: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.wantCode {
					t.Fatalf("wrapped exit = %v, want %d", err, tt.wantCode)
				}
			}
			raw, err := os.ReadFile(completion)
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatalf("parse completion JSON: %v\n%s", err, raw)
			}
			if status.State != tt.wantState || status.Reason != tt.wantReason {
				t.Fatalf("completion = %+v, want state=%s reason=%s", status, tt.wantState, tt.wantReason)
			}
		})
	}
}

func TestMetricsCompletionWrapperWaitsForSidecarBaseline(t *testing.T) {
	dir := t.TempDir()
	completion := filepath.Join(dir, "metrics-completion.json")
	ready := filepath.Join(dir, "metrics-ready")
	started := filepath.Join(dir, "main-started")
	wrapped, err := metricsoffload.WrapCommand(
		[]string{"bash", "-c", "printf started > \"$1\"", "main", started},
		testMetricsRuntime(completion, ready, 5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Fatalf("main started before metrics history baseline was ready: %v", err)
	}
	if err := os.WriteFile(ready, []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wrapped command failed after readiness: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("wrapped command did not start after sidecar readiness")
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("main did not run after metrics baseline readiness: %v", err)
	}
}

func TestRender_DirectJobWithoutMetricsOffloadIsUnchanged(t *testing.T) {
	out, err := Render(trainProfile(), Options{
		Name:      "control",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		PVCMount:  "data",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	job := parseYAML(t, out)
	template := job["spec"].(map[string]any)["template"].(map[string]any)
	pod := template["spec"].(map[string]any)
	if got := len(pod["containers"].([]any)); got != 1 {
		t.Fatalf("non-opt-in container count = %d, want 1", got)
	}
	rendered := string(out)
	for _, forbidden := range []string{"metrics-offload", "tau-metrics-runtime", workloadmeta.AnnotationExperimentSource} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("non-opt-in render unexpectedly contains %q:\n%s", forbidden, rendered)
		}
	}
}

func parseMultiDocYAML(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	s := string(b)
	dec := yaml.NewDecoder(strings.NewReader(s))
	var docs []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		docs = append(docs, m)
	}
	if len(docs) == 0 {
		t.Fatalf("no YAML documents found in:\n%s", s)
	}
	return docs
}

func TestRender_MultiNode_IndexedJob(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:             "ddp-multi",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 8,
		Nodes:            2,
		Annotations: map[string]string{
			workloadmeta.AnnotationSubmissionID: "submission-ddp",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	docs := parseMultiDocYAML(t, out)
	if len(docs) != 2 {
		t.Fatalf("multi-node should produce 2 YAML documents (Service + Job), got %d", len(docs))
	}

	svc := docs[0]
	job := docs[1]

	if svc["kind"] != "Service" {
		t.Errorf("first document should be Service, got %v", svc["kind"])
	}
	if job["kind"] != "Job" {
		t.Errorf("second document should be Job, got %v", job["kind"])
	}

	// Verify headless Service.
	svcMeta := svc["metadata"].(map[string]any)
	if svcMeta["name"] != "ddp-multi-headless" {
		t.Errorf("service name=%v, want ddp-multi-headless", svcMeta["name"])
	}
	if svcMeta["namespace"] != "tau" {
		t.Errorf("service namespace=%v, want tau", svcMeta["namespace"])
	}
	if got := svcMeta["annotations"].(map[string]any)[workloadmeta.AnnotationSubmissionID]; got != "submission-ddp" {
		t.Errorf("service submission ID=%v, want submission-ddp", got)
	}
	svcSpec := svc["spec"].(map[string]any)
	if svcSpec["clusterIP"] != "None" {
		t.Errorf("headless service clusterIP=%v, want None", svcSpec["clusterIP"])
	}
	if svcSpec["publishNotReadyAddresses"] != true {
		t.Errorf("publishNotReadyAddresses=%v, want true", svcSpec["publishNotReadyAddresses"])
	}
	selector := svcSpec["selector"].(map[string]any)
	if selector["batch.kubernetes.io/job-name"] != "ddp-multi" {
		t.Errorf("selector job-name=%v, want ddp-multi", selector["batch.kubernetes.io/job-name"])
	}

	// Verify Indexed Job fields.
	jobSpec := job["spec"].(map[string]any)
	if jobSpec["completionMode"] != "Indexed" {
		t.Errorf("completionMode=%v, want Indexed", jobSpec["completionMode"])
	}
	completions, _ := toInt64(jobSpec["completions"])
	if completions != 2 {
		t.Errorf("completions=%v, want 2", jobSpec["completions"])
	}
	parallelism, _ := toInt64(jobSpec["parallelism"])
	if parallelism != 2 {
		t.Errorf("parallelism=%v, want 2", jobSpec["parallelism"])
	}

	// Verify pod subdomain.
	pod := jobSpec["template"].(map[string]any)["spec"].(map[string]any)
	if pod["subdomain"] != "ddp-multi-headless" {
		t.Errorf("subdomain=%v, want ddp-multi-headless", pod["subdomain"])
	}

	// Verify torchrun command args.
	s := string(out)
	if !strings.Contains(s, "--nnodes=2") {
		t.Errorf("missing --nnodes=2:\n%s", s)
	}
	if !strings.Contains(s, "--node_rank=$JOB_COMPLETION_INDEX") {
		t.Errorf("missing --node_rank=$JOB_COMPLETION_INDEX:\n%s", s)
	}
	if !strings.Contains(s, "--rdzv-backend=c10d") {
		t.Errorf("missing --rdzv-backend=c10d:\n%s", s)
	}
	if !strings.Contains(s, "--rdzv-endpoint=ddp-multi-0.ddp-multi-headless:29500") {
		t.Errorf("missing --rdzv-endpoint:\n%s", s)
	}
	if !strings.Contains(s, "--rdzv-id=ddp-multi") {
		t.Errorf("missing --rdzv-id:\n%s", s)
	}
	if !strings.Contains(s, "--nproc_per_node=8") {
		t.Errorf("missing --nproc_per_node=8:\n%s", s)
	}
	if strings.Contains(s, "--standalone") {
		t.Errorf("multi-node should NOT use --standalone:\n%s", s)
	}

	// Verify env vars.
	env := envFromContainer(t, job)
	if env["MASTER_ADDR"] != "ddp-multi-0.ddp-multi-headless" {
		t.Errorf("MASTER_ADDR=%q, want ddp-multi-0.ddp-multi-headless", env["MASTER_ADDR"])
	}
	if env["MASTER_PORT"] != "29500" {
		t.Errorf("MASTER_PORT=%q, want 29500", env["MASTER_PORT"])
	}
	if env["OMP_NUM_THREADS"] != "1" {
		t.Errorf("OMP_NUM_THREADS=%q, want 1", env["OMP_NUM_THREADS"])
	}
}

func TestRender_MultiNode_MultiKueueAnnotation(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:             "ddp-mkq",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 8,
		Nodes:            2,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := parseMultiDocYAML(t, out)
	job := docs[1]
	ann := job["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[workloadmeta.AnnotationMultiKueueIncompatible] != "indexed-job-headless-service" {
		t.Errorf("missing multikueue-incompatible annotation: %v", ann)
	}
}

func TestRender_MultiNode_ExecutionAnnotation(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:             "ddp-exec",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 8,
		Nodes:            4,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := parseMultiDocYAML(t, out)
	job := docs[1]
	ann := job["metadata"].(map[string]any)["annotations"].(map[string]any)
	execJSON, ok := ann[workloadmeta.AnnotationSpecExecution].(string)
	if !ok {
		t.Fatalf("execution annotation missing")
	}
	var exec struct {
		Launcher         string `json:"launcher"`
		ProcessesPerNode int    `json:"processes_per_node"`
		Nodes            int    `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(execJSON), &exec); err != nil {
		t.Fatalf("parse execution annotation: %v", err)
	}
	if exec.Launcher != "torchrun" || exec.ProcessesPerNode != 8 || exec.Nodes != 4 {
		t.Errorf("execution annotation=%+v, want torchrun/8/4", exec)
	}
}

func TestRender_MultiNode_DevShm(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:       "ddp-shm-multi",
		Namespace:  "tau",
		ScriptPath: script,
		Launcher:   "torchrun",
		Nodes:      2,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := parseMultiDocYAML(t, out)
	job := docs[1]
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	vols, _ := pod["volumes"].([]any)
	found := false
	for _, v := range vols {
		vol := v.(map[string]any)
		if vol["name"] == "dshm" {
			found = true
		}
	}
	if !found {
		t.Errorf("/dev/shm volume should be present for multi-node (even PPN=1)")
	}
}

func TestRender_MultiNode_SingleNode_NoIndexedJob(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:             "ddp-single-node",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 8,
		Nodes:            1,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := parseMultiDocYAML(t, out)
	if len(docs) != 1 {
		t.Fatalf("Nodes=1 should produce single document, got %d", len(docs))
	}
	job := docs[0]
	jobSpec := job["spec"].(map[string]any)
	if _, ok := jobSpec["completionMode"]; ok {
		t.Errorf("Nodes=1 should not set completionMode")
	}
	s := string(out)
	if !strings.Contains(s, "--standalone") {
		t.Errorf("Nodes=1 should use --standalone:\n%s", s)
	}
}

func TestRender_MultiNode_NameBoundary(t *testing.T) {
	script := torchrunScript(t)
	for _, tc := range []struct {
		name    string
		nameLen int
		wantErr string
	}{
		{"too-long", 55, "too long for multi-node"},
		{"max-length", 54, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobName := strings.Repeat("a", tc.nameLen)
			_, err := Render(multiGPUProfile(8), Options{
				Name:       jobName,
				Namespace:  "tau",
				ScriptPath: script,
				Launcher:   "torchrun",
				Nodes:      2,
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("name length %d should be accepted: %v", tc.nameLen, err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error for name too long")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q should contain %q", err, tc.wantErr)
				}
			}
		})
	}
}

func TestRender_MultiNode_WithoutTorchrun_Rejected(t *testing.T) {
	_, err := Render(multiGPUProfile(8), Options{
		Name:      "bad-multi",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		Nodes:     2,
	})
	if err == nil {
		t.Fatal("expected error: nodes > 1 without torchrun")
	}
	if !strings.Contains(err.Error(), "requires launcher=torchrun") {
		t.Errorf("error %q should mention requires launcher=torchrun", err)
	}
}

func TestRender_Nodes_Negative_Rejected(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:      "bad-nodes",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		Nodes:     -1,
	})
	if err == nil {
		t.Fatal("expected error: negative nodes")
	}
	if !strings.Contains(err.Error(), "nodes must be >= 0") {
		t.Errorf("error %q should mention nodes must be >= 0", err)
	}
}

func TestRender_MultiNode_ServicePorts(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(8), Options{
		Name:       "ddp-ports",
		Namespace:  "tau",
		ScriptPath: script,
		Launcher:   "torchrun",
		Nodes:      2,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := parseMultiDocYAML(t, out)
	svc := docs[0]
	svcSpec := svc["spec"].(map[string]any)
	ports := svcSpec["ports"].([]any)
	if len(ports) != 1 {
		t.Fatalf("service should have 1 port, got %d", len(ports))
	}
	port := ports[0].(map[string]any)
	if port["name"] != "c10d" {
		t.Errorf("port name=%v, want c10d", port["name"])
	}
	portNum, _ := toInt64(port["port"])
	if portNum != 29500 {
		t.Errorf("port=%v, want 29500", port["port"])
	}
}

func TestRender_Torchrun_ReservedEnvRejected(t *testing.T) {
	script := torchrunScript(t)
	for _, key := range []string{"MASTER_ADDR", "MASTER_PORT"} {
		_, err := Render(trainProfile(), Options{
			Name:       "reserved-env",
			Namespace:  "tau",
			ScriptPath: script,
			Launcher:   "torchrun",
			Env:        map[string]string{key: "bad-value"},
		})
		if err == nil {
			t.Fatalf("%s: expected error", key)
		}
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "managed by torchrun") {
			t.Errorf("%s: error %q should mention reserved var", key, err)
		}
	}
}

func TestRender_ExtraFlags_Torchrun(t *testing.T) {
	script := torchrunScript(t)
	out, err := Render(multiGPUProfile(4), Options{
		Name:             "ddp-extra",
		Namespace:        "tau",
		ScriptPath:       script,
		Launcher:         "torchrun",
		ProcessesPerNode: 4,
		ExtraFlags: map[string]string{
			"max-restarts": "3",
			"log-dir":      "/data/logs",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	// Extra flags must appear between managed torchrun args and the script path.
	// The command should be: ...--nproc_per_node=4 --log-dir=/data/logs --max-restarts=3 /tmp/tau-entrypoint.py
	if !strings.Contains(s, "--nproc_per_node=4 --log-dir=/data/logs --max-restarts=3 /tmp/tau-entrypoint.py") {
		t.Errorf("extra flags not in expected position:\n%s", s)
	}
}

func TestRender_ExtraFlags_Python(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(trainProfile(), Options{
		Name:       "run-extra",
		Namespace:  "tau",
		ScriptPath: script,
		ExtraFlags: map[string]string{
			"epochs": "10",
			"batch":  "32",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	// Extra flags must appear after the script path in the exec line.
	if !strings.Contains(s, "exec /tmp/run.sh --batch=32 --epochs=10") {
		t.Errorf("extra flags not appended after script:\n%s", s)
	}
}

func TestRender_ExtraFlags_EmptyValueRendersBareFlag(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := Render(trainProfile(), Options{
		Name:       "bare-flags",
		Namespace:  "tau",
		ScriptPath: script,
		ExtraFlags: map[string]string{
			"verbose": "",
			"debug":   "",
			"output":  "/tmp/out",
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	// --debug and --verbose have empty values → bare flags (no =).
	// --output has a value → --output=/tmp/out.
	// Sorted: debug, output, verbose.
	if !strings.Contains(s, "--debug --output=/tmp/out --verbose") {
		t.Errorf("bare flags not rendered correctly:\n%s", s)
	}
}
