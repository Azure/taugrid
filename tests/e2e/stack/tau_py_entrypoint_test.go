// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	e2e "github.com/Azure/taugrid/tests/e2e"
)

const (
	tauPyEntrypointRunName    = "py-entrypoint-e2e"
	tauPyEntrypointRayJob     = "tau-" + tauPyEntrypointRunName
	tauPyGPUEntrypointRunName = "py-entrypoint-gpu-e2e"
	tauPyGPUEntrypointRayJob  = "tau-" + tauPyGPUEntrypointRunName
)

// TestTauPyEntrypointRayJob validates the Tau Python SDK's "pure PyTorch
// file" contract on a live Kueue/KubeRay stack. The user entrypoint imports no
// Tau APIs: tau.train(entrypoint=...) stages it and sibling helpers, submits a
// RayJob, and the cluster wrapper loads the function by path.
func TestTauPyEntrypointRayJob(t *testing.T) {
	if os.Getenv("E2E_TAU_PY_ENTRYPOINT") != "1" {
		t.Skip("set E2E_TAU_PY_ENTRYPOINT=1 to run the tau-py entrypoint RayJob smoke test")
	}
	rayImage := os.Getenv("RAY_E2E_IMAGE")
	require.NotEmpty(t, rayImage, "RAY_E2E_IMAGE must point at a Ray image for the tau-py entrypoint e2e")

	tc := e2e.NewTestContext(t, context.Background())
	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, e2e.RayJobGVR, tauPyEntrypointRayJob)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	cleanupTauPyEntrypointResources(t, tc, tauPyEntrypointRayJob)
	waitForNoTauPyEntrypointPods(t, tc, tauPyEntrypointRayJob)
	t.Cleanup(func() {
		cleanupTauPyEntrypointResources(t, tc, tauPyEntrypointRayJob)
		waitForNoTauPyEntrypointPods(t, tc, tauPyEntrypointRayJob)
	})

	projectDir := writeTauPyEntrypointProject(t)
	tauBinary := tauBinaryForE2E(t)
	python := tauPythonForE2E(t)

	output, err := runTauPyEntrypointSubmit(t, projectDir, python, tauBinary, rayImage, tauPyEntrypointRunName)
	require.NoError(t, err, output)

	_, err = tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, tauPyEntrypointRayJob, 30*time.Second)
	require.NoError(t, err, "Kueue should admit the tau-py entrypoint RayJob workload")

	rayClusterName := waitForTauPyRayClusterName(t, tc, tauPyEntrypointRayJob)
	headSelector := "ray.io/node-type=head,ray.io/cluster=" + rayClusterName
	err = tc.WaitForRunningPodsByLabel(stackNamespace, headSelector, 1, 3*time.Minute)
	require.NoError(t, err, "Tau Python entrypoint Ray head should be running and ready")

	workers := tauPyEntrypointWorkers(t)
	if workers > 0 {
		workerSelector := "ray.io/node-type=worker,ray.io/cluster=" + rayClusterName
		err = tc.WaitForRunningPodsByLabel(stackNamespace, workerSelector, workers, 3*time.Minute)
		require.NoError(t, err, "Tau Python entrypoint Ray workers should be running and ready")
	}

	err = tc.WaitForRayJobStatus(stackNamespace, tauPyEntrypointRayJob, "SUCCEEDED", 10*time.Minute)
	require.NoError(t, err, "Tau Python entrypoint RayJob should reach SUCCEEDED status")
}

// TestTauPyEntrypointRayJobGPU is the golden Tau live e2e for the "submit a
// pure PyTorch file" contract on GPU nodes. Unlike the CPU smoke above, this
// uses real torch, requests nvidia.com/gpu on a dedicated worker, asserts the
// control head stays on the system pool, and the user code fails unless CUDA
// is available.
func TestTauPyEntrypointRayJobGPU(t *testing.T) {
	if os.Getenv("E2E_TAU_PY_ENTRYPOINT_GPU") != "1" {
		t.Skip("set E2E_TAU_PY_ENTRYPOINT_GPU=1 to run the tau-py GPU entrypoint RayJob e2e")
	}
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run the tau-py GPU entrypoint RayJob e2e")
	}
	rayImage := os.Getenv("RAY_E2E_IMAGE")
	require.NotEmpty(t, rayImage, "RAY_E2E_IMAGE must point at a Ray image for the tau-py GPU entrypoint e2e")
	require.NotEmpty(t, os.Getenv("TORCH_SPEC"), "TORCH_SPEC must pin the CUDA torch wheel for the tau-py GPU entrypoint e2e")
	require.NotEmpty(t, os.Getenv("TORCH_INDEX_URL"), "TORCH_INDEX_URL must point at the CUDA torch wheel index for the tau-py GPU entrypoint e2e")
	gpuSelectorKey := os.Getenv("GPU_NODE_SELECTOR_KEY")
	gpuSelectorValue := os.Getenv("GPU_NODE_SELECTOR_VALUE")
	require.NotEmpty(t, gpuSelectorKey, "GPU_NODE_SELECTOR_KEY must select the target GPU node")
	require.NotEmpty(t, gpuSelectorValue, "GPU_NODE_SELECTOR_VALUE must select the target GPU node")

	tc := e2e.NewTestContext(t, context.Background())
	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, e2e.RayJobGVR, tauPyGPUEntrypointRayJob)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	requireGPUNodeKubeletProxyReachable(t, tc)
	cleanupTauPyEntrypointResources(t, tc, tauPyGPUEntrypointRayJob)
	waitForNoTauPyEntrypointPods(t, tc, tauPyGPUEntrypointRayJob)
	t.Cleanup(func() {
		cleanupTauPyEntrypointResources(t, tc, tauPyGPUEntrypointRayJob)
		waitForNoTauPyEntrypointPods(t, tc, tauPyGPUEntrypointRayJob)
	})

	projectDir := writeTauPyGPUEntrypointProject(t)
	tauBinary := tauBinaryForE2E(t)
	python := tauPythonForE2E(t)

	output, err := runTauPyEntrypointSubmit(t, projectDir, python, tauBinary, rayImage, tauPyGPUEntrypointRunName)
	require.NoError(t, err, output)

	_, err = tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, tauPyGPUEntrypointRayJob, 30*time.Second)
	require.NoError(t, err, "Kueue should admit the tau-py GPU entrypoint RayJob workload")

	rayClusterName := waitForTauPyRayClusterName(t, tc, tauPyGPUEntrypointRayJob)
	headSelector := "ray.io/node-type=head,ray.io/cluster=" + rayClusterName
	err = tc.WaitForRunningPodsByLabel(stackNamespace, headSelector, 1, gpuPodReadyTimeout)
	require.NoError(t, err, "Tau Python GPU entrypoint Ray head should be running and ready")
	requirePodsOnSelectedNodes(t, tc, headSelector, envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"Tau Python GPU entrypoint head should run on the selected system node")

	workerSelector := "ray.io/node-type=worker,ray.io/cluster=" + rayClusterName
	err = tc.WaitForRunningPodsByLabel(stackNamespace, workerSelector, 1, gpuPodReadyTimeout)
	require.NoError(t, err, "Tau Python GPU entrypoint Ray worker should be running and ready")
	requirePodsOnSelectedNodes(t, tc, workerSelector, gpuSelectorKey, gpuSelectorValue,
		"Tau Python GPU entrypoint worker should run on the selected GPU node")

	err = tc.WaitForRayJobStatus(stackNamespace, tauPyGPUEntrypointRayJob, "SUCCEEDED", gpuRayJobTimeout)
	require.NoError(t, err, "Tau Python GPU entrypoint RayJob should reach SUCCEEDED status")
}

func cleanupTauPyEntrypointResources(t *testing.T, tc *e2e.TestContext, rayJobName string) {
	t.Helper()
	propagation := metav1.DeletePropagationBackground
	err := tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(stackNamespace).Delete(tc.Ctx(), rayJobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	require.True(t, err == nil || apierrors.IsNotFound(err), "delete stale RayJob: %v", err)
	for _, name := range []string{
		rayJobName + "-script",
		rayJobName + "-manifest",
	} {
		err := tc.KubeClient().CoreV1().ConfigMaps(stackNamespace).Delete(tc.Ctx(), name, metav1.DeleteOptions{})
		require.True(t, err == nil || apierrors.IsNotFound(err), "delete stale ConfigMap %s: %v", name, err)
	}
	waitForTauPyRayJobDeleted(t, tc, rayJobName)
}

func waitForTauPyRayJobDeleted(t *testing.T, tc *e2e.TestContext, rayJobName string) {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for stale RayJob %s/%s to be deleted", stackNamespace, rayJobName)
		case <-tc.Ctx().Done():
			t.Fatalf("context canceled while waiting for stale RayJob deletion: %v", tc.Ctx().Err())
		case <-ticker.C:
			_, err := tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(stackNamespace).Get(tc.Ctx(), rayJobName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return
			}
			require.NoError(t, err, "checking stale RayJob deletion")
		}
	}
}

func waitForTauPyRayClusterName(t *testing.T, tc *e2e.TestContext, rayJobName string) string {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for RayJob %s/%s to report status.rayClusterName", stackNamespace, rayJobName)
		case <-tc.Ctx().Done():
			t.Fatalf("context canceled while waiting for RayJob rayClusterName: %v", tc.Ctx().Err())
		case <-ticker.C:
			rayJob, err := tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(stackNamespace).Get(tc.Ctx(), rayJobName, metav1.GetOptions{})
			require.NoError(t, err, "get RayJob %s/%s", stackNamespace, rayJobName)
			name, _, _ := unstructured.NestedString(rayJob.Object, "status", "rayClusterName")
			if name != "" {
				return name
			}
		}
	}
}

func waitForNoTauPyEntrypointPods(t *testing.T, tc *e2e.TestContext, rayJobName string) {
	t.Helper()
	deadline := time.After(3 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var remaining []string
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s pods to be deleted; remaining: %v", rayJobName, remaining)
		case <-tc.Ctx().Done():
			t.Fatalf("context canceled while waiting for %s pod deletion: %v", rayJobName, tc.Ctx().Err())
		case <-ticker.C:
			pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(tc.Ctx(), metav1.ListOptions{})
			require.NoError(t, err, "list pods while waiting for tau-py entrypoint cleanup")
			remaining = remaining[:0]
			for _, pod := range pods.Items {
				if strings.HasPrefix(pod.Name, rayJobName+"-") {
					remaining = append(remaining, pod.Name)
				}
			}
			if len(remaining) == 0 {
				return
			}
		}
	}
}

func runTauPyEntrypointSubmit(t *testing.T, projectDir, python, tauBinary, rayImage, runName string) (string, error) {
	t.Helper()
	cmd := exec.Command(python, "submit_tau.py")
	cmd.Dir = projectDir
	pythonPath := filepath.Join(repoRoot(t), "applications", "tau-py")
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pythonPath += string(os.PathListSeparator) + existing
	}
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+pythonPath,
		"TAU_BINARY="+tauBinary,
		"TAU_E2E_JOB_NAME="+runName,
		"TAU_E2E_NAMESPACE="+stackNamespace,
		"TAU_E2E_QUEUE="+stackQueueForRun(),
		"TAU_E2E_IMAGE="+rayImage,
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func tauBinaryForE2E(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("TAU_BINARY"); override != "" {
		return override
	}
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "tau")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/tau")
	cmd.Dir = filepath.Join(root, "applications", "tau")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return out
}

func tauPythonForE2E(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("TAU_PY_E2E_PYTHON"); override != "" {
		return override
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err, "python3 is required to prepare the tau-py e2e environment")

	venv := filepath.Join(t.TempDir(), "tau-py-venv")
	output, err := runLocalCommand("", python, "-m", "venv", venv)
	require.NoError(t, err, output)

	venvPython := filepath.Join(venv, "bin", "python")
	output, err = runLocalCommand("", venvPython, "-m", "pip", "install", "--quiet", "-e", filepath.Join(repoRoot(t), "applications", "tau-py"))
	require.NoError(t, err, output)
	return venvPython
}

func writeTauPyEntrypointProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "submit_tau.py"), `import json
import os
import pathlib

import tau


root = pathlib.Path(__file__).parent
runtime_pip = json.loads(os.environ.get("TAU_PY_ENTRYPOINT_RUNTIME_PIP", '["setuptools"]'))
if not isinstance(runtime_pip, list) or not runtime_pip:
    raise ValueError("TAU_PY_ENTRYPOINT_RUNTIME_PIP must be a non-empty JSON list")

job = tau.train(
    name=os.environ["TAU_E2E_JOB_NAME"],
    gpus=0,
    workers=int(os.environ.get("TAU_PY_ENTRYPOINT_WORKERS", "1")),
    cpu_request=1,
    memory_request="2Gi",
    worker_cpu_request=1,
    worker_memory_request="2Gi",
    entrypoint=root / "pure_pytorch_job.py:main",
    entrypoint_kwargs={"checkpoint_dir": "/data/checkpoints/" + os.environ["TAU_E2E_JOB_NAME"]},
    extra_scripts=[root / "torch.py", root / "tensor_helpers.py"],
    runtime_pip=runtime_pip,
    extra_manifest={
        "runtime": {"image": os.environ["TAU_E2E_IMAGE"]},
    },
)

job.submit(
    namespace=os.environ["TAU_E2E_NAMESPACE"],
    tau_binary=os.environ["TAU_BINARY"],
    gpu_resource_mode="device-plugin",
    extra_args=["--queue", os.environ["TAU_E2E_QUEUE"]],
)
`)
	writeFile(t, filepath.Join(dir, "pure_pytorch_job.py"), `import pathlib

import torch

from tensor_helpers import fit_batch, make_batches, rounded_state


class TinyRegressor(torch.nn.Module):
    def __init__(self):
        self.weight = 0.0
        self.bias = 0.0

    def __call__(self, xs):
        return torch.tensor([self.weight * value + self.bias for value in xs.tolist()])

    def parameters(self):
        return [self]


def main(checkpoint_dir):
    torch.manual_seed(7)
    model = TinyRegressor()
    loss_fn = torch.nn.MSELoss()
    optimizer = torch.optim.SGD(model.parameters(), lr=0.1)
    losses = []
    for xs_raw, ys_raw in make_batches():
        xs = torch.tensor(xs_raw)
        ys = torch.tensor(ys_raw)
        losses.append(round(loss_fn(model(xs), ys).item(), 4))
        optimizer.zero_grad()
        fit_batch(model, xs, ys, lr=optimizer.lr)
        optimizer.step()

    state = rounded_state(model, losses)
    expected = {"weight": 2.81, "bias": 1.37, "losses": [5.0, 24.245]}
    if state != expected:
        raise RuntimeError(f"unexpected model state: {state}")
    checkpoint_dir = pathlib.Path(checkpoint_dir)
    checkpoint_dir.mkdir(parents=True, exist_ok=True)
    torch.save(state, checkpoint_dir / "last.safetensors")
    print(
        f"TAU_PY_ENTRYPOINT_E2E_SUCCESS weight={state['weight']} bias={state['bias']} losses={state['losses']}",
        flush=True,
    )
`)
	writeFile(t, filepath.Join(dir, "tensor_helpers.py"), `def make_batches():
    return [
        ([0.0, 1.0], [1.0, 3.0]),
        ([2.0, 3.0], [5.0, 7.0]),
    ]


def fit_batch(model, xs, ys, *, lr):
    errors = model(xs) - ys
    grad_w = 2.0 * (errors * xs).mean().item()
    grad_b = 2.0 * errors.mean().item()
    model.weight -= lr * grad_w
    model.bias -= lr * grad_b


def rounded_state(model, losses):
    return {
        "weight": round(model.weight, 2),
        "bias": round(model.bias, 2),
        "losses": losses,
    }
`)
	writeFile(t, filepath.Join(dir, "torch.py"), `import json
import pathlib

_seed = None


class _Tensor:
    def __init__(self, values):
        if isinstance(values, _Tensor):
            values = values._values
        if isinstance(values, (int, float)):
            values = [values]
        self._values = [float(v) for v in values]

    def __iter__(self):
        return iter(self._values)

    def tolist(self):
        return list(self._values)

    def sum(self):
        return _Tensor([sum(self._values)])

    def mean(self):
        return _Tensor([sum(self._values) / len(self._values)])

    def item(self):
        return self._values[0]

    def _coerce(self, other):
        if isinstance(other, _Tensor):
            values = other._values
        else:
            values = [float(other)]
        if len(values) == 1 and len(self._values) != 1:
            values = values * len(self._values)
        if len(values) != len(self._values):
            raise ValueError("tensor shapes do not match")
        return values

    def _binary(self, other, op):
        return _Tensor(op(left, right) for left, right in zip(self._values, self._coerce(other)))

    def __sub__(self, other):
        return self._binary(other, lambda left, right: left - right)

    def __mul__(self, other):
        return self._binary(other, lambda left, right: left * right)


def tensor(values):
    return _Tensor(values)


def manual_seed(seed):
    global _seed
    _seed = int(seed)


def save(payload, path):
    path = pathlib.Path(path)
    path.write_text(json.dumps(payload, sort_keys=True))


class nn:
    class Module:
        def parameters(self):
            return []

    class MSELoss:
        def __call__(self, prediction, target):
            error = prediction - target
            return (error * error).mean()


class optim:
    class SGD:
        def __init__(self, params, *, lr):
            self.params = list(params)
            self.lr = float(lr)

        def zero_grad(self):
            return None

        def step(self):
            return None
`)
	return dir
}

func writeTauPyGPUEntrypointProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "submit_tau.py"), `import json
import os
import pathlib

import tau


root = pathlib.Path(__file__).parent
runtime_pip = json.loads(os.environ.get("TAU_PY_GPU_ENTRYPOINT_RUNTIME_PIP", "[]"))
if not isinstance(runtime_pip, list):
    raise ValueError("TAU_PY_GPU_ENTRYPOINT_RUNTIME_PIP must be a JSON list")
if not runtime_pip:
    runtime_pip = [
        os.environ["TORCH_SPEC"],
        "pyyaml",
        "setuptools",
        "--index-url",
        os.environ["TORCH_INDEX_URL"],
        "--extra-index-url",
        "https://pypi.org/simple",
    ]

job = tau.train(
    name=os.environ["TAU_E2E_JOB_NAME"],
    gpus=1,
    workers=1,
    node_selector={os.environ["GPU_NODE_SELECTOR_KEY"]: os.environ["GPU_NODE_SELECTOR_VALUE"]},
    cpu_request=int(os.environ.get("TAU_PY_GPU_CPU_REQUEST", "2")),
    memory_request=os.environ.get("TAU_PY_GPU_MEMORY_REQUEST", "8Gi"),
    cpu_limit=int(os.environ.get("TAU_PY_GPU_CPU_LIMIT", "4")),
    memory_limit=os.environ.get("TAU_PY_GPU_MEMORY_LIMIT", "16Gi"),
    checkpoint_artifact="last.pt",
    entrypoint=root / "pure_pytorch_gpu_job.py:main",
    entrypoint_kwargs={"checkpoint_dir": "/data/checkpoints/" + os.environ["TAU_E2E_JOB_NAME"]},
    extra_scripts=[root / "gpu_helpers.py"],
    runtime_pip=runtime_pip,
    extra_manifest={
        "runtime": {"image": os.environ["TAU_E2E_IMAGE"]},
    },
)

job.submit(
    namespace=os.environ["TAU_E2E_NAMESPACE"],
    tau_binary=os.environ["TAU_BINARY"],
    gpu_resource_mode="device-plugin",
    extra_args=["--queue", os.environ["TAU_E2E_QUEUE"]],
)
`)
	writeFile(t, filepath.Join(dir, "pure_pytorch_gpu_job.py"), `import pathlib

import torch

from gpu_helpers import make_batch


class TinyCudaRegressor(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.linear = torch.nn.Linear(2, 1)

    def forward(self, features):
        return self.linear(features)


def main(checkpoint_dir):
    if not torch.cuda.is_available():
        raise RuntimeError("CUDA is not available inside the Tau GPU entrypoint pod")

    device = torch.device("cuda")
    torch.manual_seed(11)
    features, labels = make_batch(device)
    model = TinyCudaRegressor().to(device)
    optimizer = torch.optim.SGD(model.parameters(), lr=0.05)
    loss_fn = torch.nn.MSELoss()

    losses = []
    for _ in range(6):
        optimizer.zero_grad(set_to_none=True)
        loss = loss_fn(model(features), labels)
        losses.append(float(loss.detach().cpu()))
        loss.backward()
        optimizer.step()

    if not losses[-1] < losses[0]:
        raise RuntimeError(f"GPU training loss did not decrease: {losses}")

    checkpoint_dir = pathlib.Path(checkpoint_dir)
    checkpoint_dir.mkdir(parents=True, exist_ok=True)
    torch.save(
        {
            "device": str(device),
            "initial_loss": losses[0],
            "final_loss": losses[-1],
            "cuda_device_name": torch.cuda.get_device_name(0),
        },
        checkpoint_dir / "last.pt",
    )
    print(
        f"TAU_PY_ENTRYPOINT_GPU_E2E_SUCCESS Device: {device} initial_loss={losses[0]:.6f} final_loss={losses[-1]:.6f}",
        flush=True,
    )
`)
	writeFile(t, filepath.Join(dir, "gpu_helpers.py"), `import torch


def make_batch(device):
    features = torch.tensor(
        [
            [0.0, 1.0],
            [1.0, 0.0],
            [1.0, 1.0],
            [2.0, 1.0],
        ],
        device=device,
    )
    labels = torch.tensor([[1.0], [2.0], [3.0], [5.0]], device=device)
    return features, labels
`)
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func tauPyEntrypointWorkers(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("TAU_PY_ENTRYPOINT_WORKERS")
	if raw == "" {
		return 1
	}
	workers, err := strconv.Atoi(raw)
	require.NoError(t, err, "TAU_PY_ENTRYPOINT_WORKERS must be an integer")
	require.GreaterOrEqual(t, workers, 1, "TAU_PY_ENTRYPOINT_WORKERS must be >= 1")
	return workers
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	return root
}

func runLocalCommand(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	return string(output), err
}
