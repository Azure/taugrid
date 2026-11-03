// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"fmt"
	"strings"
	"testing"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

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
