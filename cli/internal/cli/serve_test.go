package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestApplyCheckpointMount(t *testing.T) {
	env, volumes, mounts, err := applyCheckpointMount(nil, nil, nil, "finetunes/run/checkpoints/best.safetensors", "blob-training")
	if err != nil {
		t.Fatal(err)
	}
	if env["TAU_MODEL_PATH"] != "/data/checkpoints/finetunes/run/checkpoints/best.safetensors" {
		t.Fatalf("TAU_MODEL_PATH not normalized: %v", env)
	}
	if env["TAU_DATA_DIR"] != "/data" {
		t.Fatalf("TAU_DATA_DIR not set: %v", env)
	}
	if len(volumes) != 1 || volumes[0].Name != "tau-data" || volumes[0].PVC != "blob-training" {
		t.Fatalf("checkpoint volume not added: %#v", volumes)
	}
	if len(mounts) != 1 || mounts[0].Name != "tau-data" || mounts[0].MountPath != "/data" || mounts[0].ReadOnly {
		t.Fatalf("checkpoint mount not added: %#v", mounts)
	}
}

func TestApplyCheckpointMountRejectsConflicts(t *testing.T) {
	_, _, _, err := applyCheckpointMount(map[string]string{"TAU_MODEL_PATH": "/other"}, nil, nil, "/checkpoints/model", "blob-training")
	if err == nil {
		t.Fatal("expected conflicting TAU_MODEL_PATH to be rejected")
	}
}

func TestParseServeCheckpointRef(t *testing.T) {
	ref, ok, err := parseServeCheckpointRef("train-run", "")
	if err != nil || !ok {
		t.Fatalf("parse --from-finetune: ok=%v err=%v", ok, err)
	}
	if ref.Run != "train-run" || ref.Artifact != "checkpoint" {
		t.Fatalf("ref=%+v", ref)
	}

	ref, ok, err = parseServeCheckpointRef("", "finetune/train-run:rank0/final.safetensors")
	if err != nil || !ok {
		t.Fatalf("parse --checkpoint-ref: ok=%v err=%v", ok, err)
	}
	if ref.Run != "train-run" || ref.Artifact != "rank0/final.safetensors" {
		t.Fatalf("ref=%+v", ref)
	}

	if _, _, err := parseServeCheckpointRef("a", "finetune/b"); err == nil {
		t.Fatal("expected conflicting refs to fail")
	}
	if _, _, err := parseServeCheckpointRef("", "other/train-run"); err == nil {
		t.Fatal("expected unsupported checkpoint-ref to fail")
	}
}

func TestParseServeModelRef(t *testing.T) {
	ref, ok, err := parseServeModelRef("sample:best-loss", "")
	if err != nil || !ok {
		t.Fatalf("parse --from-model: ok=%v err=%v", ok, err)
	}
	if ref != "sample:best-loss" {
		t.Fatalf("ref=%q", ref)
	}
	ref, ok, err = parseServeModelRef("", "sample@run-1")
	if err != nil || !ok {
		t.Fatalf("parse --model-ref: ok=%v err=%v", ok, err)
	}
	if ref != "sample@run-1" {
		t.Fatalf("ref=%q", ref)
	}
	if _, _, err := parseServeModelRef("a", "b"); err == nil {
		t.Fatal("expected conflicting model refs to fail")
	}
	if _, _, err := parseServeModelRef("", "bad:"); err == nil {
		t.Fatal("expected invalid model ref to fail")
	}
}

func TestParseEnvSecretKV(t *testing.T) {
	vars, err := parseEnvSecretKV([]string{"HF_TOKEN=hf-token:token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 1 || vars[0].Name != "HF_TOKEN" || vars[0].ValueFrom.SecretKeyRef.Name != "hf-token" || vars[0].ValueFrom.SecretKeyRef.Key != "token" {
		t.Fatalf("bad env secret parse: %#v", vars)
	}
	if _, err := parseEnvSecretKV([]string{"HF_TOKEN=bad"}); err == nil {
		t.Fatal("expected malformed env secret to fail")
	}
}

func TestSelectFinetuneArtifact(t *testing.T) {
	raw := []byte(`{
	  "schema_version": 1,
	  "run": "train-run",
	  "artifacts": [
	    {
	      "name": "checkpoint",
	      "manifest_path": "rank0/final.safetensors",
	      "durable_path": "/data/checkpoints/finetunes/train-run/artifacts/rank0/final.safetensors",
	      "status": "ready",
	      "file_count": 1
	    }
	  ]
	}`)
	artifact, err := selectManagedWorkflowArtifact(raw, "rank0/final.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.DurablePath != "/data/checkpoints/finetunes/train-run/artifacts/rank0/final.safetensors" || artifact.FileCount != 1 {
		t.Fatalf("artifact=%+v", artifact)
	}

	notReady := []byte(`{"run":"train-run","artifacts":[{"name":"checkpoint","durable_path":"/data/x","status":"uploading"}]}`)
	if _, err := selectManagedWorkflowArtifact(notReady, "checkpoint"); err == nil {
		t.Fatal("expected non-ready artifact to fail")
	}
}

func TestServeDeployDeploymentServiceAndProbes(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "stt-serving",
		"--kind=deployment",
		"--profile", "sample-project-stt-a100",
		"--image", "sampleprojectcr.azurecr.io/stt:v1",
		"--deployment-port", "8000",
		"--readiness-path", "/health",
		"--startup-path", "/health",
		"--startup-failure-threshold", "30",
		"--liveness-path", "/health",
		"--service-port", "8000",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve deploy failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rendered := out.String()
	for _, want := range []string{
		"kind: Deployment",
		"readinessProbe:",
		"startupProbe:",
		"failureThreshold: 30",
		"livenessProbe:",
		"path: /health",
		"port: 8000",
		"kind: Service",
		"type: ClusterIP",
		"targetPort: 8000",
		"app: stt-serving",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestServeDeployGPUsFlag(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "multi-gpu-svc",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "test:v1",
		"--gpus", "4",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve deploy --gpus 4 failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rendered := out.String()
	if !strings.Contains(rendered, "nvidia.com/gpu: 4") {
		t.Fatalf("expected nvidia.com/gpu: 4 in rendered manifest:\n%s", rendered)
	}
}

func TestServeDeployGPUsRejectsNegative(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "bad-svc",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "test:v1",
		"--gpus", "-1",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected negative --gpus to fail; stdout=%s stderr=%s", out.String(), stderr.String())
	} else if !strings.Contains(err.Error(), "--gpus must be >= 0") {
		t.Fatalf("serve deploy --gpus -1 error = %v", err)
	}
}

func TestServeDeployGPUsZero(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "cpu-svc",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "test:v1",
		"--gpus", "0",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve deploy --gpus 0 failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rendered := out.String()
	if strings.Contains(rendered, "nvidia.com/gpu") {
		t.Fatalf("expected no nvidia.com/gpu for --gpus 0:\n%s", rendered)
	}
}

func TestServeDeployMaxReplicas_Deployment(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "scale-svc",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "test:v1",
		"--deployment-port", "8080",
		"--service-port", "8080",
		"--max-replicas", "10",
		"--min-replicas", "2",
		"--target-qps", "50",
		"--scale-down-delay", "600",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve deploy --max-replicas failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rendered := out.String()
	for _, want := range []string{
		"kind: HorizontalPodAutoscaler",
		"apiVersion: autoscaling/v2",
		"name: scale-svc",
		"maxReplicas: 10",
		"minReplicas: 2",
		"http_requests_per_second",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestServeDeployMaxReplicas_RayService(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "ray-scale",
		"--kind=rayservice",
		"--profile", "model-serve",
		"--image", "test:v1",
		"--max-replicas", "8",
		"--target-qps", "100",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve deploy --max-replicas rayservice failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rendered := out.String()
	for _, want := range []string{
		"autoscaling_config:",
		"max_replicas: 8",
		"target_num_ongoing_requests_per_replica: 100",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestServeDeploySubFlagsWithoutMaxReplicas(t *testing.T) {
	for _, flag := range []string{"--min-replicas=2", "--target-qps=50", "--scale-down-delay=600"} {
		t.Run(flag, func(t *testing.T) {
			cmd := NewRoot()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{
				"serve", "deploy", "bad-svc",
				"--kind=deployment",
				"--profile", "model-serve",
				"--image", "test:v1",
				flag,
				"--dry-run=client",
				"-n", "tau",
			})
			if err := cmd.Execute(); err == nil {
				t.Fatalf("expected %s without --max-replicas to fail", flag)
			} else if !strings.Contains(err.Error(), "requires --max-replicas") {
				t.Fatalf("%s error = %v", flag, err)
			}
		})
	}
}

func TestServeDeployMaxReplicasAndReplicasMutuallyExclusive(t *testing.T) {
	cmd := NewRoot()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"serve", "deploy", "bad-svc",
		"--kind=deployment",
		"--profile", "model-serve",
		"--image", "test:v1",
		"--max-replicas", "10",
		"--replicas", "3",
		"--dry-run=client",
		"-n", "tau",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected --max-replicas + --replicas to fail; stdout=%s stderr=%s", out.String(), stderr.String())
	} else if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("serve deploy --max-replicas --replicas error = %v", err)
	}
}

func TestServeDeployAutoscalingRejectsNegativeValues(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"max replicas", []string{"--max-replicas=-1"}, "--max-replicas must be >= 0"},
		{"target qps", []string{"--target-qps=-1"}, "requires --max-replicas"},
		{"target qps with autoscaling", []string{"--max-replicas=3", "--target-qps=-1"}, "TargetQPS must be >= 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := NewRoot()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			args := []string{
				"serve", "deploy", "bad-svc",
				"--kind=deployment",
				"--profile", "model-serve",
				"--image", "test:v1",
				"--dry-run=client",
				"-n", "tau",
			}
			args = append(args, c.args...)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("expected %v to fail; stdout=%s stderr=%s", c.args, out.String(), stderr.String())
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v error = %v, want substring %q", c.args, err, c.want)
			}
		})
	}
}
