// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func submitFineWebTauRun(t *testing.T, tc *e2e.TestContext, name string, workers, steps int) error {
	t.Helper()
	pvc := os.Getenv("FINEWEB_CHECKPOINT_PVC")
	if pvc == "" || strings.HasPrefix(pvc, "$(") {
		return fmt.Errorf("Tau FineWeb requires an existing FINEWEB_CHECKPOINT_PVC in %s", stackNamespace)
	}
	script, err := os.ReadFile(filepath.Join("fixtures", "fineweb_ray_train.py"))
	if err != nil {
		return err
	}
	config := fineWebTauConfig(name, workers, steps, pvc)
	return e2e.SubmitTauRun(t, tc.Ctx(), config, map[string][]byte{"fineweb_ray_train.py": script})
}

func fineWebTauConfig(name string, workers, steps int, pvc string) map[string]any {
	checkpoint := "/data/checkpoints/nightly/" + name
	interval := os.Getenv("FINEWEB_CHECKPOINT_INTERVAL")
	if interval == "" {
		interval = "1"
	}
	return map[string]any{
		"name": name, "engine": "rayjob",
		"run": map[string]any{"entrypoint": "fineweb_ray_train.py", "ttl_seconds_after_finished": 600},
		"runtime": map[string]any{
			"image": os.Getenv("RAY_E2E_IMAGE"),
			"rdma":  map[string]any{"enabled": true},
			"pip":   []string{"torch==2.7.1", "numpy==2.1.3"},
			"env": map[string]string{
				"FINEWEB_DATA_MODE":               "synthetic",
				"FINEWEB_TRAIN_WORKERS":           strconv.Itoa(workers),
				"FINEWEB_TRAIN_STEPS":             strconv.Itoa(steps),
				"FINEWEB_CHECKPOINT_INTERVAL":     interval,
				"FINEWEB_CHECKPOINT_DIR":          checkpoint,
				"NCCL_DEBUG":                      "INFO",
				"NCCL_IB_HCA":                     "mlx5_ib0,mlx5_ib1,mlx5_ib2,mlx5_ib3,mlx5_ib4,mlx5_ib5,mlx5_ib6,mlx5_ib7",
				"NCCL_SOCKET_IFNAME":              "eth0",
				"TORCH_NCCL_ASYNC_ERROR_HANDLING": "1",
				"PIP_INDEX_URL":                   "https://download.pytorch.org/whl/cu128",
				"PIP_EXTRA_INDEX_URL":             "https://pypi.org/simple",
			},
		},
		"compute": map[string]any{
			"workers": workers, "gpus_per_worker": 1,
			"head_cpu_request": "2", "head_memory_request": "8Gi",
			"worker_cpu_request": "4", "worker_memory_request": "48Gi",
		},
		"policy":    e2e.NightlyTauPolicy(stackNamespace, stackLargeGPUQueueForRun(), "nightly.h200.ib"),
		"storage":   map[string]any{"data_pvc": pvc, "output": checkpoint},
		"execution": map[string]any{"launcher": "ray-train", "allow_nccl_override": true},
	}
}

func TestFineWebTauConfig(t *testing.T) {
	t.Setenv("RAY_E2E_IMAGE", "registry.example.com/ray:py3.12-ray2.58.0")
	t.Setenv("FINEWEB_CHECKPOINT_INTERVAL", "")
	cfg := fineWebTauConfig("nightly-ib-42", 16, 2, "checkpoint")
	require.Equal(t, true, cfg["runtime"].(map[string]any)["rdma"].(map[string]any)["enabled"])
	require.Equal(t, 16, cfg["compute"].(map[string]any)["workers"])
	require.Equal(t, 1, cfg["compute"].(map[string]any)["gpus_per_worker"])
	require.Equal(t, "Never", cfg["policy"].(map[string]any)["pod_preemption_policy"])
	env := cfg["runtime"].(map[string]any)["env"].(map[string]string)
	require.Equal(t, "1", env["FINEWEB_CHECKPOINT_INTERVAL"])
	require.Equal(t, "/data/checkpoints/nightly/nightly-ib-42", env["FINEWEB_CHECKPOINT_DIR"])
	require.NotContains(t, env, "NCCL_IB_DISABLE")
	require.Equal(t, "checkpoint", cfg["storage"].(map[string]any)["data_pvc"])
	t.Setenv("FINEWEB_CHECKPOINT_INTERVAL", "2")
	configured := fineWebTauConfig("nightly-ib-43", 16, 4, "checkpoint")
	require.Equal(t, "2", configured["runtime"].(map[string]any)["env"].(map[string]string)["FINEWEB_CHECKPOINT_INTERVAL"])
}

func TestNativeTauFineWebRendering(t *testing.T) {
	t.Setenv("RAY_E2E_IMAGE", "registry.example.com/ray:py3.12-ray2.58.0")
	binary := e2e.BuildTauForTest(t)
	script, err := os.ReadFile("fixtures/fineweb_ray_train.py")
	require.NoError(t, err)
	platform := profile.WorkloadProfile{
		Name: "nightly.h200.ib", WorkerCount: 16, GPUsPerWorker: 1,
		Mode: "fixed", Placement: profile.PlacementSameNetworkDomain, DefaultLocalQueue: stackLargeGPUQueueForRun(),
		NodeSelector: map[string]string{"kueue.azure.com/gpu-series": "nd-h200-v5", "tau.azure.com/site": "test-site"},
		Priorities:   profile.ProfilePriorities{WorkloadPriorityClassName: "aks-low-priority", PodPriorityClassName: "tau-nightly-low"},
	}
	job := e2e.RenderTauRunOffline(t, binary, fineWebTauConfig("nightly-ib", 16, 2, "checkpoint"),
		map[string][]byte{"fineweb_ray_train.py": script}, platform)
	require.Equal(t, "RayJob", job.GetKind())
	groups, _, err := unstructured.NestedSlice(job.Object, "spec", "rayClusterSpec", "workerGroupSpecs")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	group := groups[0].(map[string]any)
	require.Equal(t, int64(16), group["replicas"])
	spec, _, err := unstructured.NestedMap(group, "template", "spec")
	require.NoError(t, err)
	require.Equal(t, "Never", spec["preemptionPolicy"])
	annotations, _, err := unstructured.NestedStringMap(group, "template", "metadata", "annotations")
	require.NoError(t, err)
	require.Equal(t, "tau.azure.com/network-domain", annotations["kueue.x-k8s.io/podset-required-topology"])
	rendered, err := yaml.Marshal(job.Object)
	require.NoError(t, err)
	require.Contains(t, string(rendered), "inject-ib-libs")
	require.Contains(t, string(rendered), "claimName: checkpoint")
	require.Contains(t, string(rendered), "/data/checkpoints/nightly/nightly-ib")
	require.Contains(t, string(rendered), "IPC_LOCK")
	require.NotContains(t, string(rendered), "NCCL_IB_DISABLE")
}
