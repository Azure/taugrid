// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serve

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func makeServeProfile() profile.Profile {
	return profile.Profile{
		Name:  "ai-serve-gpu-l",
		Queue: "serving-queue",
		Resources: profile.Resources{
			Requests: map[string]any{"cpu": "4", "memory": "16Gi"},
			GPU:      profile.GPUContract{Count: 1, Size: "l"},
		},
	}
}

func TestRender_HappyPath(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name: "my-7b", Namespace: "ray",
		Image: "vllm/vllm-openai:v0.6.3",
		Env:   map[string]string{"SAMPLE_SERVE_VARIANT": "SamplePretrained"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: RayService",
		"name: my-7b",
		"namespace: ray",
		"kueue.x-k8s.io/queue-name: serving-queue",
		workloadmeta.LabelService + ": my-7b",
		"rayVersion: 2.40.0",
		"image: vllm/vllm-openai:v0.6.3",
		"name: SAMPLE_SERVE_VARIANT",
		"value: SamplePretrained",
		workloadmeta.AnnotationGPUContract + ": count=1,size=l",
		workloadmeta.LabelGPUCount + ": \"1\"",
		"runtime_env:",
		"SAMPLE_SERVE_VARIANT: \"SamplePretrained\"",
		"nvidia.com/gpu: 1",
		"dashboard-host:",
		"containerPort: 8000",
		"import_path: serve:app",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered YAML missing %q\nYAML:\n%s", want, s)
		}
	}
}

func TestRender_CheckpointMountAndEnv(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name:      "model-api",
		Namespace: "tau",
		Image:     "acr.io/model-serve:v1",
		Env: map[string]string{
			"SAMPLE_MODEL_PATH": "/data/checkpoints/finetunes/run/checkpoints/best.safetensors",
		},
		Volumes: []Volume{
			{Name: "tau-data", PVC: "blob-training"},
		},
		VolumeMounts: []VolumeMount{
			{Name: "tau-data", MountPath: "/data"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"SAMPLE_MODEL_PATH: \"/data/checkpoints/finetunes/run/checkpoints/best.safetensors\"",
		"name: SAMPLE_MODEL_PATH",
		"value: /data/checkpoints/finetunes/run/checkpoints/best.safetensors",
		"mountPath: /data",
		"claimName: blob-training",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered YAML missing %q\nYAML:\n%s", want, s)
		}
	}
	assertRuntimeEnvVarsContains(t, s, "SAMPLE_MODEL_PATH")
}

func TestRender_RuntimePipAndSecretEnv(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name:      "captioner2",
		Namespace: "tau",
		Image:     "acr.io/captioner2-serve:v1",
		RuntimePip: []string{
			"transformers==4.45.0",
			"accelerate",
		},
		EnvVars: []envspec.Var{
			envspec.Secret("HF_TOKEN", "hf-token", "token"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"runtime_env:",
		"pip:",
		"- \"transformers==4.45.0\"",
		"- \"accelerate\"",
		"name: HF_TOKEN",
		"secretKeyRef:",
		"name: hf-token",
		"key: token",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered YAML missing %q\nYAML:\n%s", want, s)
		}
	}
	if strings.Contains(s, "HF_TOKEN:") {
		t.Fatalf("secret-backed HF_TOKEN must not be copied into Ray runtime_env env_vars:\n%s", s)
	}
}

func assertRuntimeEnvVarsContains(t *testing.T, rendered, envName string) {
	t.Helper()

	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "runtime_env:" {
			continue
		}
		runtimeIndent := leadingSpaces(line)
		if i+2 >= len(lines) {
			break
		}
		envVarsLine := lines[i+1]
		envLine := lines[i+2]
		if strings.TrimSpace(envVarsLine) == "env_vars:" &&
			leadingSpaces(envVarsLine) > runtimeIndent &&
			strings.HasPrefix(strings.TrimSpace(envLine), envName+":") &&
			leadingSpaces(envLine) > leadingSpaces(envVarsLine) {
			return
		}
	}

	t.Fatalf("rendered YAML missing %s under serve runtime_env.env_vars:\n%s", envName, rendered)
}

func leadingSpaces(line string) int {
	for i, r := range line {
		if r != ' ' {
			return i
		}
	}
	return len(line)
}

func TestRender_ReplicasAndImportPath(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name: "e", Namespace: "n",
		Image: "x", Replicas: 4, ReplicasSet: true, ImportPath: "my_pkg.api:entry", ServePort: 9000,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "num_replicas: 4") {
		t.Errorf("expected num_replicas: 4\n%s", s)
	}
	if !strings.Contains(s, "import_path: my_pkg.api:entry") {
		t.Errorf("expected import_path override\n%s", s)
	}
	if !strings.Contains(s, "containerPort: 9000") {
		t.Errorf("expected containerPort: 9000\n%s", s)
	}
}

func TestRender_NoImage_Errors(t *testing.T) {
	p := makeServeProfile() // no runtime.image in fixture
	_, err := Render(p, Options{Name: "x", Namespace: "n"})
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("expected no-image error, got: %v", err)
	}
}

func TestRender_ProfileImageFallback(t *testing.T) {
	p := makeServeProfile()
	p.Runtime.Image = "acr.io/fallback:v1"
	out, err := Render(p, Options{Name: "x", Namespace: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "image: acr.io/fallback:v1") {
		t.Errorf("profile.runtime.image fallback not honoured:\n%s", string(out))
	}
}

func TestRender_DevicePluginHasNoClaimBlock(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{Name: "x", Namespace: "n", Image: "i"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "resourceClaimTemplateName") {
		t.Errorf("expected no claim block; got:\n%s", s)
	}
	if strings.Contains(s, "claims:") {
		t.Errorf("expected no container claims; got:\n%s", s)
	}
	if !strings.Contains(s, "nvidia.com/gpu") {
		t.Errorf("expected device-plugin nvidia.com/gpu request; got:\n%s", s)
	}
}

// TestRender_NoQueue_IsRejected pins the RayService half of the fail-closed
// contract. An unlabelled RayService is never admitted by Kueue, so rendering
// one produces a submit that silently never runs.
func TestRender_NoQueue_IsRejected(t *testing.T) {
	p := makeServeProfile()
	p.Queue = ""
	_, err := Render(p, Options{Name: "x", Namespace: "n", Image: "i"})
	if err == nil {
		t.Fatal("render without a LocalQueue must fail, not emit a RayService that is never admitted")
	}
	if !strings.Contains(err.Error(), "LocalQueue is required") {
		t.Fatalf("error should name the missing LocalQueue, got: %v", err)
	}
}

func TestRender_NameNamespaceRequired(t *testing.T) {
	p := makeServeProfile()
	_, err := Render(p, Options{Namespace: "n", Image: "i"})
	if err == nil || !strings.Contains(err.Error(), "Name") {
		t.Errorf("expected Name required, got: %v", err)
	}
	_, err = Render(p, Options{Name: "x", Image: "i"})
	if err == nil || !strings.Contains(err.Error(), "Namespace") {
		t.Errorf("expected Namespace required, got: %v", err)
	}
}

func TestRender_ArgsPropagated(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name: "x", Namespace: "n", Image: "i",
		Args: []string{"--model", "/ckpt/7b", "--quantize", "awq"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"--model", "/ckpt/7b", "--quantize", "awq"} {
		if !strings.Contains(s, want) {
			t.Errorf("arg %q not propagated\n%s", want, s)
		}
	}
}

func TestRender_OmitsReplicaOverrideByDefault(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{Name: "x", Namespace: "n", Image: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "deployments:") || strings.Contains(string(out), "num_replicas:") {
		t.Errorf("expected app decorator defaults without deployment override, got:\n%s", string(out))
	}
}

func TestRender_Replicas_ZeroOverrideAllowed(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{Name: "x", Namespace: "n", Image: "i", Replicas: 0, ReplicasSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "num_replicas: 0") {
		t.Errorf("expected explicit replicas=0 override, got:\n%s", string(out))
	}
}

func TestRender_ReplicasNegative_Rejected(t *testing.T) {
	p := makeServeProfile()
	_, err := Render(p, Options{Name: "x", Namespace: "n", Image: "i", Replicas: -1, ReplicasSet: true})
	if err == nil || !strings.Contains(err.Error(), "Replicas") {
		t.Errorf("expected Replicas >= 0 error, got: %v", err)
	}
}

func TestRender_Autoscaling(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name: "scaled", Namespace: "n", Image: "i",
		Autoscaling: &AutoscalingOptions{
			MinReplicas:    2,
			MaxReplicas:    8,
			TargetQPS:      100,
			ScaleDownDelay: 600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"autoscaling_config:",
		"min_replicas: 2",
		"max_replicas: 8",
		"target_num_ongoing_requests_per_replica: 100",
		"downscale_delay_s: 600",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered YAML missing %q\n%s", want, s)
		}
	}
	if strings.Contains(s, "num_replicas:") {
		t.Errorf("autoscaling_config and num_replicas must be mutually exclusive\n%s", s)
	}
}

func TestRender_Autoscaling_Defaults(t *testing.T) {
	p := makeServeProfile()
	out, err := Render(p, Options{
		Name: "x", Namespace: "n", Image: "i",
		Autoscaling: &AutoscalingOptions{MaxReplicas: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "min_replicas: 1") {
		t.Errorf("expected default min_replicas: 1\n%s", s)
	}
	if !strings.Contains(s, "target_num_ongoing_requests_per_replica: 5") {
		t.Errorf("expected default target_num_ongoing_requests_per_replica: 5\n%s", s)
	}
	if !strings.Contains(s, "downscale_delay_s: 300") {
		t.Errorf("expected default downscale_delay_s: 300\n%s", s)
	}
}

func TestRender_Autoscaling_MutualExclusion(t *testing.T) {
	p := makeServeProfile()
	_, err := Render(p, Options{
		Name: "x", Namespace: "n", Image: "i",
		Replicas: 3, ReplicasSet: true,
		Autoscaling: &AutoscalingOptions{MaxReplicas: 5},
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutual exclusion error, got: %v", err)
	}
}

func TestRender_Autoscaling_Validation(t *testing.T) {
	p := makeServeProfile()
	_, err := Render(p, Options{
		Name: "x", Namespace: "n", Image: "i",
		Autoscaling: &AutoscalingOptions{MinReplicas: 10, MaxReplicas: 5},
	})
	if err == nil || !strings.Contains(err.Error(), "MinReplicas must be <= MaxReplicas") {
		t.Errorf("expected validation error, got: %v", err)
	}

	_, err = Render(p, Options{
		Name: "x", Namespace: "n", Image: "i",
		Autoscaling: &AutoscalingOptions{MaxReplicas: 5, TargetQPS: -1},
	})
	if err == nil || !strings.Contains(err.Error(), "TargetQPS must be >= 0") {
		t.Errorf("expected TargetQPS validation error, got: %v", err)
	}

	_, err = Render(p, Options{
		Name: "x", Namespace: "n", Image: "i",
		Autoscaling: &AutoscalingOptions{MaxReplicas: 5, ScaleDownDelay: -1},
	})
	if err == nil || !strings.Contains(err.Error(), "ScaleDownDelay must be >= 0") {
		t.Errorf("expected ScaleDownDelay validation error, got: %v", err)
	}
}
