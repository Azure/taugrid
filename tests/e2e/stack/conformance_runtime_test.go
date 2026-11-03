// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	psa "k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/policy"
	"sigs.k8s.io/yaml"
)

func conformanceArtifactDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".conformance-artifacts-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	absolute, err := filepath.Abs(dir)
	require.NoError(t, err)
	return absolute
}

func conformanceFixture(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	for _, c := range payloadFixtureCases() {
		if c.fixture == name {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
		}
	}
	data, err := e2e.ReadFixtureWithSubstitutions(name)
	require.NoError(t, err)
	var obj map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &obj))
	return obj
}

func conformancePod(t *testing.T, obj map[string]interface{}, fields ...string) corev1.PodTemplateSpec {
	t.Helper()
	m, found, err := unstructured.NestedMap(obj, fields...)
	require.NoError(t, err)
	require.True(t, found)
	var pod corev1.PodTemplateSpec
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(m, &pod))
	return pod
}

func fineWebPodTemplates(t *testing.T) []corev1.PodTemplateSpec {
	t.Helper()
	t.Setenv("FINEWEB_TRAIN_WORKERS", "16")
	obj := conformanceFixture(t, "fineweb-rayjob-16xh200-ib.yaml")
	head := conformancePod(t, obj, "spec", "rayClusterSpec", "headGroupSpec", "template")
	workers, found, err := unstructured.NestedSlice(obj, "spec", "rayClusterSpec", "workerGroupSpecs")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, workers, 1)
	w := workers[0].(map[string]interface{})
	require.EqualValues(t, 16, w["replicas"])
	return []corev1.PodTemplateSpec{head, conformancePod(t, w, "template")}
}

func TestFineWebBaselinePods(t *testing.T) {
	evaluator, err := policy.NewEvaluator(policy.DefaultChecks(), nil)
	require.NoError(t, err)
	for i, pod := range fineWebPodTemplates(t) {
		result := policy.AggregateCheckResults(evaluator.EvaluatePod(
			psa.LevelVersion{Level: psa.LevelBaseline, Version: psa.LatestVersion()},
			&pod.ObjectMeta, &pod.Spec))
		require.True(t, result.Allowed, "pod template %d: %s", i, result.ForbiddenDetail())
	}
}

func TestFineWebScratchAndCheckpointContract(t *testing.T) {
	pods := fineWebPodTemplates(t)
	for _, pod := range pods {
		volumes := map[string]bool{}
		for _, v := range pod.Spec.Volumes {
			require.Nil(t, v.HostPath)
			if v.Name == "ray-tmp" || v.Name == "fineweb-ckpt" {
				volumes[v.Name] = true
				require.NotNil(t, v.EmptyDir)
				require.NotNil(t, v.EmptyDir.SizeLimit)
				require.Positive(t, v.EmptyDir.SizeLimit.Value())
			}
		}
		require.True(t, volumes["ray-tmp"])
		for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			for _, m := range c.VolumeMounts {
				if m.Name == "ray-tmp" {
					require.Empty(t, m.SubPathExpr)
				}
			}
		}
	}
	worker := pods[1].Spec.Containers[0]
	q := worker.Resources.Requests["nvidia.com/gpu"]
	require.EqualValues(t, 1, q.Value())
	q = worker.Resources.Limits["nvidia.com/gpu"]
	require.EqualValues(t, 1, q.Value())
	obj := conformanceFixture(t, "fineweb-rayjob-16xh200-ib.yaml")
	shutdown, _, err := unstructured.NestedBool(obj, "spec", "shutdownAfterJobFinishes")
	require.NoError(t, err)
	require.True(t, shutdown)
	ttl, _, err := unstructured.NestedFloat64(obj, "spec", "ttlSecondsAfterFinished")
	require.NoError(t, err)
	require.EqualValues(t, 7200, ttl)
}

func TestLargeGPURequestedWorldSize(t *testing.T) {
	t.Setenv("NANOGPT_TRAIN_WORKERS", "16")
	obj := conformanceFixture(t, "nanogpt-rayjob-large-gpu.yaml")
	workers, _, err := unstructured.NestedSlice(obj, "spec", "rayClusterSpec", "workerGroupSpecs")
	require.NoError(t, err)
	require.Len(t, workers, 1)
	w := workers[0].(map[string]interface{})
	for _, field := range []string{"replicas", "minReplicas", "maxReplicas"} {
		require.EqualValues(t, 16, w[field])
	}
	pod := conformancePod(t, w, "template")
	q := pod.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"]
	require.EqualValues(t, 1, q.Value())
	q = pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
	require.EqualValues(t, 1, q.Value())
}

func TestLargeGPUHeadCPURequests(t *testing.T) {
	for _, c := range []struct {
		fixture, key string
	}{
		{"fineweb-rayjob-16xh200-ib.yaml", "FINEWEB_HEAD_CPU_REQUEST"},
		{"nanogpt-rayjob-large-gpu.yaml", "NANOGPT_HEAD_CPU_REQUEST"},
	} {
		for _, raw := range []string{"", "100m", "1.5", "2", "0", "-1", "2001m", "invalid", "1\"\nother: value"} {
			t.Run(c.fixture+"/"+raw, func(t *testing.T) {
				for _, fixture := range payloadFixtureCases() {
					if fixture.fixture == c.fixture {
						for k, v := range fixture.env {
							t.Setenv(k, v)
						}
					}
				}
				t.Setenv(c.key, raw)
				data, err := e2e.ReadFixtureWithSubstitutions(c.fixture)
				if raw != "" && raw != "100m" && raw != "1.5" && raw != "2" {
					require.ErrorContains(t, err, "positive CPU quantity <= 2")
					return
				}
				require.NoError(t, err)
				var obj map[string]interface{}
				require.NoError(t, yaml.Unmarshal(data, &obj))
				pod := conformancePod(t, obj, "spec", "rayClusterSpec", "headGroupSpec", "template")
				want := int64(2000)
				if raw == "100m" {
					want = 100
				} else if raw == "1.5" {
					want = 1500
				}
				require.Equal(t, want, pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
				require.EqualValues(t, 2, pod.Spec.Containers[0].Resources.Limits.Cpu().Value())
			})
		}
	}
}

func TestNanoGPTModelSizing(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			for key, fallback := range map[string]string{"NANOGPT_N_LAYER": "4", "NANOGPT_N_HEAD": "4", "NANOGPT_N_EMBD": "256"} {
				value := ""
				if configured {
					value = map[string]string{"NANOGPT_N_LAYER": "8", "NANOGPT_N_HEAD": "8", "NANOGPT_N_EMBD": "512"}[key]
				}
				t.Setenv(key, value)
				obj := conformanceFixture(t, "nanogpt-rayjob-large-gpu.yaml")
				text, _, err := unstructured.NestedString(obj, "spec", "runtimeEnvYAML")
				require.NoError(t, err)
				var runtimeEnv struct {
					Env map[string]string `json:"env_vars"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(text), &runtimeEnv))
				if !configured {
					value = fallback
				}
				require.Equal(t, value, runtimeEnv.Env[key])
			}
		})
	}
}

func TestConformanceWorkloadPriority(t *testing.T) {
	for _, priority := range []string{"", "batch-low", "research.priority", "Invalid", "-invalid", "invalid\nlabel: injection", strings.Repeat("x", 64)} {
		t.Run(priority, func(t *testing.T) {
			for _, fixture := range payloadFixtureCases() {
				if fixture.fixture != "nanogpt-rayjob-large-gpu.yaml" && fixture.fixture != "inference-rayjob-gpu.yaml" && fixture.fixture != "training-rayjob-gpu.yaml" {
					continue
				}
				t.Run(fixture.fixture, func(t *testing.T) {
					for key, value := range fixture.env {
						t.Setenv(key, value)
					}
					t.Setenv("NANOGPT_WORKLOAD_PRIORITY_CLASS", priority)
					data, err := e2e.ReadFixtureWithSubstitutions(fixture.fixture)
					if priority != "" && priority != "batch-low" && priority != "research.priority" {
						require.ErrorContains(t, err, "invalid workload priority class")
						return
					}
					require.NoError(t, err)
					var obj map[string]interface{}
					require.NoError(t, yaml.Unmarshal(data, &obj))
					labels, _, err := unstructured.NestedStringMap(obj, "metadata", "labels")
					require.NoError(t, err)
					require.Equal(t, priority, labels["kueue.x-k8s.io/priority-class"])
					if priority == "" {
						require.NotContains(t, labels, "kueue.x-k8s.io/priority-class")
					}
				})
			}
		})
	}
}

func TestConformanceDeadlines(t *testing.T) {
	for _, raw := range []string{"0", "-1m", "91m", "nonsense"} {
		_, err := boundedDuration(raw, 30*time.Minute, 90*time.Minute)
		require.Error(t, err, raw)
	}
	d, err := boundedDuration("", 30*time.Minute, 90*time.Minute)
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, d)
	d, err = boundedDuration("45m", 30*time.Minute, 90*time.Minute)
	require.NoError(t, err)
	require.Equal(t, 45*time.Minute, d)
}

func TestConformanceGoTestTimeoutInterface(t *testing.T) {
	for _, tc := range []struct {
		name, completion, admission, capture, goTimeout string
		ok                                              bool
	}{
		{"default", "", "", "", "", true},
		{"longUnfunded", "180", "", "", "", false},
		{"longFunded", "180", "", "", "305m", true},
		{"shortFunded", "60", "", "", "185m", true},
		{"compound", "", "", "", "4h5m", true},
		{"fractional", "", "", "", "+245.5m", true},
		{"maximum", "180", "", "", "365m", true},
		{"aboveMaximum", "", "", "", "366m", false},
		{"zero", "", "", "", "0", false},
		{"negative", "", "", "", "-245m", false},
		{"missingUnit", "", "", "", "245", false},
		{"admissionUnfunded", "", "90m", "", "", false},
		{"admissionFunded", "", "1h30m", "", "305m", true},
		{"captureUnfunded", "", "", "30m", "", false},
		{"captureFunded", "", "", "30m", "260m", true},
		{"invalid", "", "0", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FINEWEB_RAYJOB_TIMEOUT_MINUTES", tc.completion)
			t.Setenv("CONFORMANCE_ADMISSION_TIMEOUT", tc.admission)
			t.Setenv("FINEWEB_CHECKPOINT_CAPTURE_TIMEOUT", tc.capture)
			t.Setenv("CONFORMANCE_GO_TEST_TIMEOUT", tc.goTimeout)
			budget, err := conformanceExecutionBudget(true)
			cmd := exec.Command("python3", "-B", "harness/conformance_contract.py", "budget", "fineweb")
			out, shellErr := cmd.CombinedOutput()
			if !tc.ok {
				require.Error(t, err)
				require.Error(t, shellErr, "%s", out)
				return
			}
			require.NoError(t, err)
			require.NoError(t, shellErr, "%s", out)
			expected := tc.goTimeout
			if expected == "" {
				expected = "245m"
			}
			timeout, err := time.ParseDuration(expected)
			require.NoError(t, err)
			require.Equal(t, timeout+5*time.Minute, budget)
			require.Equal(t, expected+"\n", string(out))
		})
	}
}

func TestFineWebCheckpointStreaming(t *testing.T) {
	payload := "small full-state stand-in"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	sentinel := fmt.Sprintf("FINEWEB_FIRST_CHECKPOINT step=50 path=/mnt/fineweb-ckpt/fineweb-step-50.pt bytes=%d params=1716000000 sha256=%s pod=owned-worker readback=verified", len(payload), digest)
	meta, err := parseVerifiedCheckpoint(sentinel)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, content     string
		copyErr, canceled bool
		ok                bool
	}{
		{"success", payload, false, false, true},
		{"truncated", payload[:5], false, false, false},
		{"hashMismatch", strings.Repeat("x", len(payload)), false, false, false},
		{"oversize", payload + "extra", false, false, false},
		{"copyFailed", payload, true, false, false},
		{"deadline", payload, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := conformanceArtifactDir(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			err := streamCheckpoint(ctx, dir, meta, func(ctx context.Context, out io.Writer) error {
				if tc.copyErr {
					return fmt.Errorf("mock exec failure")
				}
				_, err := io.Copy(out, strings.NewReader(tc.content))
				return err
			})
			if tc.ok {
				require.NoError(t, err)
				got, err := os.ReadFile(filepath.Join(dir, meta.File))
				require.NoError(t, err)
				require.Equal(t, payload, string(got))
				require.FileExists(t, filepath.Join(dir, "fineweb-checkpoint.json"))
				require.Error(t, streamCheckpoint(ctx, dir, meta, nil))
			} else {
				require.Error(t, err)
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Empty(t, entries)
			}
		})
	}
	require.Error(t, checkpointArtifactDir("relative"))
	for _, bad := range []string{
		strings.ReplaceAll(sentinel, "readback=verified", "readback=unchecked"),
		strings.ReplaceAll(sentinel, "/mnt/fineweb-ckpt/", "/etc/"),
		strings.ReplaceAll(sentinel, "step-50.pt", "step-51.pt"),
	} {
		_, err := parseVerifiedCheckpoint(bad)
		require.Error(t, err)
	}
}

func TestFineWebCleanupOwnership(t *testing.T) {
	for _, owner := range []string{"", "test", "caller"} {
		t.Run("owner="+owner, func(t *testing.T) {
			t.Setenv("FINEWEB_CLEANUP_OWNER", owner)
			for _, fixture := range []string{"fineweb-rayjob-16xh200-ib.yaml", "nanogpt-rayjob-large-gpu.yaml"} {
				cleaned := false
				t.Run(fixture, func(t *testing.T) {
					registerRayJobCleanup(t, fixture, func() { cleaned = true })
				})
				require.Equal(t, owner != "caller" || fixture != "fineweb-rayjob-16xh200-ib.yaml", cleaned)
			}
		})
	}
	t.Setenv("FINEWEB_CLEANUP_OWNER", "typo")
	_, err := fineWebCallerOwnsCleanup()
	require.Error(t, err)
}

func TestConformanceOwnedClusterAndDeadline(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "ray.io/v1", "kind": "RayJob",
		"metadata": map[string]interface{}{"name": "owned-job", "namespace": "shared"},
		"status":   map[string]interface{}{"rayClusterName": "owned-cluster"},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	name, err := conformanceRayCluster(context.Background(), client, "shared", "owned-job")
	require.NoError(t, err)
	require.Equal(t, "owned-cluster", name)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = conformanceRayCluster(ctx, client, "shared", "missing-job")
	require.ErrorIs(t, err, context.Canceled)
}

func TestFineWebCheckpointExecAndWriteErrors(t *testing.T) {
	bin := conformanceArtifactDir(t)
	argsFile := filepath.Join(bin, "args")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kubectl"), []byte(
		"#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FINEWEB_UNIT_ARGS\"\nprintf 'a\\000b'\n"), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FINEWEB_UNIT_ARGS", argsFile)
	t.Setenv("AI_RUNTIME_E2E_KUBE_CONTEXT", "unit-context")
	payload := "a\x00b"
	meta := fineWebCheckpoint{
		Bytes: 3, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(payload))),
		File: "fineweb-step-50.pt", Path: "/mnt/fineweb-ckpt/fineweb-step-50.pt",
		Pod: "owned-worker", Namespace: "unit-namespace",
	}
	dir := conformanceArtifactDir(t)
	require.NoError(t, streamCheckpoint(context.Background(), dir, meta, func(ctx context.Context, out io.Writer) error {
		return copyCheckpointFromPod(ctx, meta, out)
	}))
	data, err := os.ReadFile(filepath.Join(dir, meta.File))
	require.NoError(t, err)
	require.Equal(t, payload, string(data))
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Equal(t, "--context\nunit-context\n-n\nunit-namespace\nexec\nowned-worker\n-c\nray-worker\n--\ncat\n"+meta.Path+"\n", string(args))
	dir = conformanceArtifactDir(t)
	err = streamCheckpoint(context.Background(), dir, meta, func(_ context.Context, out io.Writer) error {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "fineweb-checkpoint.json"), []byte("other owner"), 0600))
		_, err := io.WriteString(out, payload)
		return err
	})
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(dir, meta.File))
	data, err = os.ReadFile(filepath.Join(dir, "fineweb-checkpoint.json"))
	require.NoError(t, err)
	require.Equal(t, "other owner", string(data))
	w := checkpointWriter{ctx: context.Background(), dst: failingCheckpointWriter{}, left: 10}
	_, err = w.Write([]byte("x"))
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

type failingCheckpointWriter struct{}

func (failingCheckpointWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestConformancePythonContracts(t *testing.T) {
	for _, path := range []string{"harness/conformance_contract_test.py", "fixtures/fineweb_runtime_test.py"} {
		t.Run(path, func(t *testing.T) {
			cmd := exec.Command("python3", "-B", path)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
		})
	}
}
