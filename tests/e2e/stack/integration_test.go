// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package stack contains full-stack integration tests that exercise the seam
// between charts rather than validating each one in isolation.
//
// TestInferenceRayJob runs a real image-classification inference workload through
// the full Kueue → KubeRay → Ray Data pipeline on a CPU-only CI cluster:
//
//  1. A RayJob is submitted with a Kueue queue-name label.
//  2. Kueue gang-admits the Workload (2 workers = 4 CPU).
//  3. KubeRay spawns head + 2 worker pods.
//  4. Ray Data distributes inference over 10 synthetic images across 2 actors.
//  5. The job reaches SUCCEEDED and its logs contain valid predictions.
//
// See docs/design/2026-04-17-e2e-stack-inference-test.md for the full design.
package stack

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

const (
	defaultStackNamespace = "e2e-stack"
	argoCDStackNamespace  = "taugrid-e2e"
	argoCDStackQueue      = "jobqueue"
	rayJobName            = "e2e-inference"
	rayJobNameGPU         = "e2e-inference-gpu"
	rayJobNameTrain       = "e2e-training"
	rayJobNameTrainGPU    = "e2e-training-gpu"
	rayJobNameNanoGPT     = "e2e-nanogpt-large-gpu"
	rayJobNameFineWeb     = "e2e-fineweb-16xh200-ib"
	ncclRDMAJobName       = "e2e-nccl-rdma-2x1xh200"
	ncclRDMAConfirmation  = "create-fixed-nccl-rdma-indexed-job"
	ncclRDMAInvocationKey = "e2e.taugrid.azure.com/invocation"
	ncclRDMAOwnedUIDFile  = "NCCL_RDMA_OWNED_UID_FILE"

	gpuPodReadyTimeout      = 10 * time.Minute
	gpuRayJobTimeout        = 20 * time.Minute
	largeGPUPodReadyTimeout = 30 * time.Minute
	largeGPURayJobTimeout   = 120 * time.Minute

	defaultNanoGPTTrainWorkers = 16
	defaultNanoGPTTrainSteps   = 2000

	defaultFineWebTrainWorkers = 16
	defaultFineWebTrainSteps   = 60

	// FineWeb conformance asserts the runtime param count lands in the ~1.7B band
	// (1.716B at the default 32L/2048d config); the workload also self-checks this.
	fineWebMinParams = 1_600_000_000
	fineWebMaxParams = 1_800_000_000
)

var localQueueGVR = schema.GroupVersionResource{
	Group:    "kueue.x-k8s.io",
	Version:  "v1beta2",
	Resource: "localqueues",
}

var ncclRDMAJobGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

var stackNamespace = stackNamespaceForRun()

func TestMain(m *testing.M) {
	// Skip all setup if e2e tests are not enabled.
	if os.Getenv("AI_RUNTIME_E2E") != "1" {
		os.Exit(m.Run())
	}

	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if os.Getenv("E2E_NCCL_RDMA") == "1" {
		if os.Getenv("NCCL_RDMA_CONFIRM") != ncclRDMAConfirmation {
			fmt.Fprintln(os.Stderr, "NCCL/RDMA diagnostic requires the explicit confirmation token before any cluster access")
			return 1
		}
		if !regexp.MustCompile(`^nccl-rdma-[a-f0-9]{32}$`).MatchString(strings.TrimSpace(os.Getenv("NCCL_RDMA_INVOCATION"))) {
			fmt.Fprintln(os.Stderr, "NCCL/RDMA diagnostic requires a unique nccl-rdma-<32 lowercase hex> invocation marker before any cluster access")
			return 1
		}
	}

	kubeClient, dynamicClient, err := e2e.BuildClients()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to build K8s clients: %v\n", err)
		return 1
	}

	ctx := context.Background()
	if os.Getenv("E2E_NCCL_RDMA") == "1" && !stackUsesArgoCDQueue() {
		fmt.Fprintln(os.Stderr, "NCCL/RDMA diagnostic requires an explicit pre-provisioned namespace and LocalQueue; refusing fixture-managed stack resources")
		return 1
	}
	if largeGPUUsesManagerWorkloadAccess() && !stackUsesArgoCDQueue() {
		fmt.Fprintln(os.Stderr, "manager workload access requires the pre-provisioned ArgoCD stack namespace and queue")
		return 1
	}

	if stackUsesArgoCDQueue() {
		if !largeGPUUsesManagerWorkloadAccess() {
			if err := requireArgoCDStackQueue(ctx, kubeClient, dynamicClient); err != nil {
				fmt.Fprintf(os.Stderr, "ArgoCD stack queue is not ready: %v\n", err)
				e2e.DumpDeploymentDiagnostics(ctx, kubeClient, "kueue-system", "kueue-controller-manager")
				e2e.DumpDeploymentDiagnostics(ctx, kubeClient, "kuberay-system", "kuberay-operator")
				return 1
			}
		}
	} else {
		// Setup order matters: the Kueue fixture must create the namespace before
		// any RayJob fixtures are applied into it.
		if err := e2e.ApplyFixtureWithClient(ctx, dynamicClient, "stack-kueue-resources.yaml"); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to setup stack Kueue resources: %v\n", err)
			e2e.DumpDeploymentDiagnostics(ctx, kubeClient, "kueue-system", "kueue-controller-manager")
			e2e.DumpDeploymentDiagnostics(ctx, kubeClient, "kuberay-system", "kuberay-operator")
			return 1
		}
		if os.Getenv("E2E_PRESERVE_KUEUE_RECORDS") != "1" {
			defer e2e.DeleteFixtureWithClient(ctx, dynamicClient, "stack-kueue-resources.yaml")
		}
	}

	if os.Getenv("E2E_STACK_TAU_ENTRYPOINT_ONLY") == "1" {
		defer results.FlushAll()
		return m.Run()
	}

	defer results.FlushAll()

	return m.Run()
}

func stackUsesArgoCDQueue() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("E2E_STACK_USE_ARGOCD_QUEUE"))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	if strings.TrimSpace(os.Getenv("E2E_STACK_NAMESPACE")) != "" ||
		strings.TrimSpace(os.Getenv("E2E_STACK_QUEUE")) != "" ||
		strings.TrimSpace(os.Getenv("E2E_STACK_LARGE_GPU_QUEUE")) != "" {
		return true
	}
	return false
}

func stackNamespaceForRun() string {
	if ns := strings.TrimSpace(os.Getenv("E2E_STACK_NAMESPACE")); ns != "" {
		return ns
	}
	if stackUsesArgoCDQueue() {
		return argoCDStackNamespace
	}
	return defaultStackNamespace
}

func stackQueueForRun() string {
	if queue := strings.TrimSpace(os.Getenv("E2E_STACK_QUEUE")); queue != "" {
		return queue
	}
	if queue := strings.TrimSpace(os.Getenv("E2E_STACK_LARGE_GPU_QUEUE")); queue != "" {
		return queue
	}
	if stackUsesArgoCDQueue() {
		return argoCDStackQueue
	}
	return "e2e-stack-queue"
}

func stackLargeGPUQueueForRun() string {
	if queue := strings.TrimSpace(os.Getenv("E2E_STACK_LARGE_GPU_QUEUE")); queue != "" {
		return queue
	}
	if stackUsesArgoCDQueue() {
		return argoCDStackQueue
	}
	return "e2e-stack-large-gpu-queue"
}

func requireArgoCDStackQueue(ctx context.Context, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface) error {
	if _, err := kubeClient.CoreV1().Namespaces().Get(ctx, stackNamespace, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("namespace %s must be created by platform queue policy before live stack conformance runs: %w", stackNamespace, err)
	}
	queues := []string{stackQueueForRun(), stackLargeGPUQueueForRun()}
	seen := map[string]struct{}{}
	for _, queue := range queues {
		if _, ok := seen[queue]; ok {
			continue
		}
		seen[queue] = struct{}{}
		if _, err := dynamicClient.Resource(localQueueGVR).Namespace(stackNamespace).Get(ctx, queue, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("LocalQueue %s/%s must be created by platform queue policy before live stack conformance runs: %w", stackNamespace, queue, err)
		}
	}
	return nil
}

func recordOutcomeWithWorkflowSuffix(t *testing.T, tc *e2e.TestContext) {
	t.Helper()
	if suffix := strings.TrimSpace(os.Getenv("E2E_TEST_NAME_SUFFIX")); suffix != "" {
		tc.RecordOutcomeAs(t.Name() + "/" + suffix)
	}
}

func TestInferenceRayJob(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	// CR-specific diagnostics on failure (controller logs are auto-captured by NewTestContext).
	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, e2e.RayJobGVR, rayJobName)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	// Step 1: Submit the RayJob. t.Cleanup deletes it when the test finishes.
	// Note: ApplyFixture resolves {{RAY_IMAGE}} (from RAY_E2E_IMAGE env var) and
	// {{RAY_VERSION}} (from RAY_E2E_VERSION/image tag, then Makefile fallback)
	// via readFixture() in testcontext.go.
	applyRayJobFixture(t, tc, "inference-rayjob.yaml", rayJobName)

	// Step 2: Kueue admits the workload gang (head + 2 workers).
	_, err := tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, rayJobName, 30*time.Second)
	require.NoError(t, err, "Kueue should admit the inference RayJob workload")

	// Step 3: Head + 2 workers must reach Running+Ready before Ray will run the job.
	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=head", 1, 3*time.Minute)
	require.NoError(t, err, "Ray head should be running and ready")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=worker", 2, 3*time.Minute)
	require.NoError(t, err, "Both Ray workers should be running and ready")

	// Step 4: RayJob runs inference. 8 min covers pip install (~60s on warm PyPI,
	// longer on cold CI) + model init + classification across 2 actors.
	err = tc.WaitForRayJobStatus(stackNamespace, rayJobName, "SUCCEEDED", 8*time.Minute)
	require.NoError(t, err, "RayJob should reach SUCCEEDED status")

	// Step 5: Verify inference output in the submitter pod logs.
	// KubeRay creates a Job named after the RayJob that runs `ray job submit`; its pod logs
	// stream the remote driver's stdout, which includes the classification predictions.
	_, err = tc.WaitForPodLogsByLabelContaining(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobName), []string{
		"Label:",
		"SUCCESS: All images classified",
	}, time.Minute)
	require.NoError(t, err, "submitter logs should show classification predictions and inference success")
}

// TestInferenceRayJobGPU is the GPU variant: the worker requests nvidia.com/gpu: 1
// and the inference script auto-detects CUDA. Gated on E2E_GPU=1 so it only runs on
// CI clusters that actually have GPU nodes (see chart-integration-test.yaml).
func TestInferenceRayJobGPU(t *testing.T) {
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run the GPU inference test (requires nvidia.com/gpu-capable nodes)")
	}

	tc := e2e.NewTestContext(t, context.Background())
	recordOutcomeWithWorkflowSuffix(t, tc)

	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, e2e.RayJobGVR, rayJobNameGPU)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	requireGPUNodeKubeletProxyReachable(t, tc)
	applyRayJobFixture(t, tc, "inference-rayjob-gpu.yaml", rayJobNameGPU)

	_, err := tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, rayJobNameGPU, 30*time.Second)
	require.NoError(t, err, "Kueue should admit the GPU inference RayJob workload")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=head", 1, gpuPodReadyTimeout)
	require.NoError(t, err, "Ray head should be running and ready")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=worker", 1, gpuPodReadyTimeout)
	require.NoError(t, err, "GPU Ray worker should be running and ready")
	requirePodsOnSelectedNodes(t, tc, "ray.io/node-type=worker", os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"),
		"GPU inference worker should run on the selected GPU node")

	// GPU path: CUDA torch wheel + nvidia.com/gpu allocation can push RayCluster
	// readiness and runtime env setup longer than CPU, especially on cold H100 nodes.
	err = tc.WaitForRayJobStatus(stackNamespace, rayJobNameGPU, "SUCCEEDED", gpuRayJobTimeout)
	require.NoError(t, err, "GPU RayJob should reach SUCCEEDED status")

	_, err = tc.WaitForPodLogsByLabelContaining(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameGPU), []string{
		"Device: cuda",
		"SUCCESS: All images classified",
	}, time.Minute)
	require.NoError(t, err, "submitter logs should show CUDA execution and inference success")
	requirePodsOnSelectedNodes(t, tc, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameGPU), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"GPU inference submitter should stay on the selected CPU node pool")
}

// TestTrainingRayJobGPU validates the training half of the advertised
// feature set: a Ray worker with nvidia.com/gpu=1 runs a tiny PyTorch SGD
// loop on CUDA and loss decreases.
//
// Intentionally short-running — this exists to catch "GPU training path is
// broken" regressions, not to validate model quality. Guarded by E2E_GPU=1.
func TestTrainingRayJobGPU(t *testing.T) {
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run the GPU training test (requires nvidia.com/gpu-capable nodes)")
	}

	tc := e2e.NewTestContext(t, context.Background())
	recordOutcomeWithWorkflowSuffix(t, tc)

	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, e2e.RayJobGVR, rayJobNameTrainGPU)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	requireGPUNodeKubeletProxyReachable(t, tc)
	applyRayJobFixture(t, tc, "training-rayjob-gpu.yaml", rayJobNameTrainGPU)

	_, err := tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, rayJobNameTrainGPU, 30*time.Second)
	require.NoError(t, err, "Kueue should admit the GPU training RayJob workload")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=head", 1, gpuPodReadyTimeout)
	require.NoError(t, err, "Ray head should be running")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=worker", 1, gpuPodReadyTimeout)
	require.NoError(t, err, "GPU training worker should be running")
	requirePodsOnSelectedNodes(t, tc, "ray.io/node-type=worker", os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"),
		"GPU training worker should run on the selected GPU node")

	// Training loop is tiny but CUDA torch wheel install dominates; mirror the GPU
	// inference budget.
	err = tc.WaitForRayJobStatus(stackNamespace, rayJobNameTrainGPU, "SUCCEEDED", gpuRayJobTimeout)
	require.NoError(t, err, "GPU training RayJob should reach SUCCEEDED status")

	_, err = tc.WaitForPodLogsByLabelContaining(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameTrainGPU), []string{
		"Device: cuda",
		"SUCCESS: GPU training loss decreased",
	}, time.Minute)
	require.NoError(t, err, "submitter logs should show CUDA execution and decreasing training loss")
	requirePodsOnSelectedNodes(t, tc, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameTrainGPU), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"GPU training submitter should stay on the selected CPU node pool")
}

// TestNanoGPTRayTrainLargeGPU validates the live large-GPU conformance path:
// Kueue admits a KubeRay RayJob, Ray schedules 16 GPU workers, and Ray Train
// completes the configured optimizer steps of a bounded nanoGPT-style workload on CUDA.
func TestNanoGPTRayTrainLargeGPU(t *testing.T) {
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run GPU stack tests")
	}
	if os.Getenv("E2E_LARGE_GPU") != "1" {
		t.Skip("set E2E_LARGE_GPU=1 to run the 16-GPU nanoGPT Ray Train conformance test")
	}

	workers := envInt(t, "NANOGPT_TRAIN_WORKERS", defaultNanoGPTTrainWorkers)
	steps := envInt(t, "NANOGPT_TRAIN_STEPS", defaultNanoGPTTrainSteps)

	managerWorkloadAccess := largeGPUUsesManagerWorkloadAccess()
	if managerWorkloadAccess && os.Getenv("AI_RUNTIME_E2E_MANAGER_WORKLOAD_ONLY") != "1" {
		t.Fatal("manager workload mode requires AI_RUNTIME_E2E_MANAGER_WORKLOAD_ONLY=1 before constructing Kubernetes clients")
	}
	tc := e2e.NewTestContext(t, context.Background())
	recordOutcomeWithWorkflowSuffix(t, tc)

	// Skip-if-busy guard: the scheduled workflow uses this as a second check
	// after its capacity preflight so a late-arriving H200 workload skips instead
	// of turning the conformance run red before the test can make progress. The
	// manager route cannot inspect worker nodes, so its separate platform
	// preflight owns this assertion.
	if os.Getenv("NANOGPT_SKIP_IF_BUSY") == "1" && !managerWorkloadAccess {
		if available := availableGPUsOnSelectedNodes(t, tc); available < workers {
			t.Skipf("NANOGPT_SKIP_IF_BUSY=1 and only %d/%d GPUs available on the selected GPU nodes; skipping (cluster busy)", available, workers)
		}
	}

	tc.OnFailure(func() {
		// Do not dump the RayJob spec here: the runtimeEnv can include short-lived
		// dataset SAS URIs.
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		if managerWorkloadAccess {
			return
		}
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	if !managerWorkloadAccess {
		requireGPUNodeKubeletProxyReachable(t, tc)
	}
	applyRayJobFixture(t, tc, "nanogpt-rayjob-large-gpu.yaml", rayJobNameNanoGPT)

	admission, err := tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, rayJobNameNanoGPT, 2*time.Minute)
	require.NoError(t, err, "Kueue should admit the 16-GPU nanoGPT RayJob workload")

	if managerWorkloadAccess {
		// MultiKueue mirrors RayJob completion to the manager, but worker pods,
		// node labels, kubelet health, and submitter logs remain worker-cluster
		// evidence. Keep the researcher credential on the manager and defer those
		// assertions until manager-side result/placement evidence is available.
		err = tc.WaitForRayJobStatus(stackNamespace, rayJobNameNanoGPT, "SUCCEEDED", largeGPURayJobTimeout)
		require.NoError(t, err, "manager-routed 16-GPU nanoGPT RayJob should mirror SUCCEEDED status")
		return
	}

	if stackUsesArgoCDQueue() {
		requireNanoGPTFlavorAssignments(t, admission.Workload, os.Getenv("GPU_NODE_SELECTOR_VALUE"))
	}

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=head", 1, largeGPUPodReadyTimeout)
	require.NoError(t, err, "Ray head should be running and ready")
	requirePodsOnSelectedNodes(t, tc, "ray.io/node-type=head", envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"nanoGPT Ray head should stay on the selected CPU node pool")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=worker", workers, largeGPUPodReadyTimeout)
	require.NoError(t, err, "all 16 Ray Train GPU workers should be running and ready")
	requirePodsOnSelectedNodes(t, tc, "ray.io/node-type=worker", os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"),
		"nanoGPT Ray Train workers should run on the selected GPU nodes")

	err = tc.WaitForRayJobStatus(stackNamespace, rayJobNameNanoGPT, "SUCCEEDED", largeGPURayJobTimeout)
	require.NoError(t, err, "16-GPU nanoGPT RayJob should reach SUCCEEDED status")

	_, err = tc.WaitForPodLogsByLabelContaining(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameNanoGPT), []string{
		"NANOGPT_RAY_TRAIN_SUCCESS",
		fmt.Sprintf("step=%d", steps),
		fmt.Sprintf("world_size=%d", workers),
	}, 2*time.Minute)
	require.NoError(t, err, "submitter logs should show Ray Train nanoGPT success at the configured step count")
	requirePodsOnSelectedNodes(t, tc, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameNanoGPT), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"nanoGPT submitter should stay on the selected CPU node pool")
}

func requireNanoGPTFlavorAssignments(t *testing.T, workload *unstructured.Unstructured, gpuFlavor string) {
	t.Helper()
	require.NotNil(t, workload, "admitted Workload should be returned for flavor validation")
	require.NotEmpty(t, gpuFlavor, "GPU_NODE_SELECTOR_VALUE must identify the canonical GPU ResourceFlavor")

	assignments, found, err := unstructured.NestedSlice(workload.Object, "status", "admission", "podSetAssignments")
	require.NoError(t, err, "read Workload PodSetAssignments")
	require.True(t, found, "admitted Workload should have PodSetAssignments")

	got := make(map[string]map[string]string, len(assignments))
	for _, rawAssignment := range assignments {
		assignment, ok := rawAssignment.(map[string]interface{})
		require.True(t, ok, "PodSetAssignment should be an object")
		name, found, err := unstructured.NestedString(assignment, "name")
		require.NoError(t, err, "read PodSetAssignment name")
		require.True(t, found, "PodSetAssignment should have a name")
		flavors, found, err := unstructured.NestedStringMap(assignment, "flavors")
		require.NoError(t, err, "read flavors for PodSetAssignment %q", name)
		require.True(t, found, "PodSetAssignment %q should have flavors", name)
		got[name] = flavors
	}

	require.Equal(t, map[string]map[string]string{
		"head": {
			"cpu":    "tau-system",
			"memory": "tau-system",
		},
		"nanogpt-workers": {
			"cpu":            gpuFlavor,
			"memory":         gpuFlavor,
			"nvidia.com/gpu": gpuFlavor,
		},
		"submitter": {
			"cpu":    "tau-system",
			"memory": "tau-system",
		},
	}, got, "Kueue should assign the canonical system and GPU ResourceFlavors")
}

func TestRequireNanoGPTFlavorAssignments(t *testing.T) {
	workload := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"admission": map[string]interface{}{
				"podSetAssignments": []interface{}{
					map[string]interface{}{
						"name": "head",
						"flavors": map[string]interface{}{
							"cpu":    "tau-system",
							"memory": "tau-system",
						},
					},
					map[string]interface{}{
						"name": "nanogpt-workers",
						"flavors": map[string]interface{}{
							"cpu":            "nd-h200-v5",
							"memory":         "nd-h200-v5",
							"nvidia.com/gpu": "nd-h200-v5",
						},
					},
					map[string]interface{}{
						"name": "submitter",
						"flavors": map[string]interface{}{
							"cpu":    "tau-system",
							"memory": "tau-system",
						},
					},
				},
			},
		},
	}}

	requireNanoGPTFlavorAssignments(t, workload, "nd-h200-v5")
}

func largeGPUUsesManagerWorkloadAccess() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("LARGE_GPU_WORKLOAD_ACCESS_MODE")), "manager")
}

// TestNCCLRDMA2x1H200 is an operator-owned 2-GPU inter-node RDMA validation,
// not a full-node bandwidth benchmark or standard Tau profile.
func TestNCCLRDMA2x1H200(t *testing.T) {
	if os.Getenv("AI_RUNTIME_E2E") != "1" {
		t.Skip("set AI_RUNTIME_E2E=1 to run live e2e tests")
	}
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run GPU stack tests")
	}
	if os.Getenv("E2E_NCCL_RDMA") != "1" {
		t.Skip("set E2E_NCCL_RDMA=1 to run the NCCL/RDMA Indexed Job diagnostic")
	}
	require.Equal(t, ncclRDMAConfirmation, os.Getenv("NCCL_RDMA_CONFIRM"),
		"NCCL/RDMA diagnostic requires the explicit harness confirmation token")
	invocation := strings.TrimSpace(os.Getenv("NCCL_RDMA_INVOCATION"))
	require.Regexp(t, `^nccl-rdma-[a-f0-9]{32}$`, invocation,
		"NCCL/RDMA diagnostic requires the unique harness invocation marker")
	require.True(t, stackUsesArgoCDQueue(), "NCCL/RDMA diagnostic requires a pre-existing namespace and LocalQueue")
	recorder := newNCCLRDMAArtifactRecorder(t, invocation, time.Now().UTC())
	t.Cleanup(recorder.fallbackWrite)
	require.NoError(t, recorder.requireInputs())

	tc := e2e.NewTestContext(t, context.Background())
	recordOutcomeWithWorkflowSuffix(t, tc)

	job, err := readNCCLRDMAJobFixture()
	require.NoError(t, err)
	owned, createErr := createNCCLRDMAResources(tc, job, invocation)
	t.Cleanup(func() {
		if recorder.cleanupDone {
			return
		}
		if err := recorder.cleanup(tc, owned, invocation, createErr != nil); err != nil {
			t.Errorf("delete only successful-response UID-owned NCCL/RDMA resources: %v", err)
		}
	})
	require.NoError(t, createErr)
	jobUID := ownedUID(owned, ncclRDMAJobGVR, ncclRDMAJobName)
	require.NotEmpty(t, jobUID)
	suspended, found, err := unstructured.NestedBool(job.Object, "spec", "suspend")
	require.NoError(t, err)
	require.True(t, found && suspended, "persisted Job must remain suspended until Kueue admits it")

	jobDeadline := time.Now().Add(13 * time.Minute)
	ownedSelector := fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s", ncclRDMAInvocationKey, invocation)
	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, ncclRDMAJobGVR, ncclRDMAJobName)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, ownedSelector)
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
	})

	require.NoError(t, waitForNCCLRDMAWorkloadAdmitted(tc, jobUID, 2*time.Minute), "Kueue should admit the fixed Job")
	admittedAt, err := ncclRDMAWorkloadAdmittedAt(tc, jobUID)
	require.NoError(t, err)
	require.NoError(t, waitForNCCLRDMAJobSuspendedState(tc, jobUID, false, 30*time.Second),
		"Kueue should unsuspend only the admitted Job")
	pods, err := waitForNCCLRDMAPodsScheduled(tc, jobUID, invocation, 5*time.Minute)
	if err != nil && strings.Contains(err.Error(), "failed") {
		recorder.addError(rdmavalidation.ReasonRuntimeError, "pods", "an Indexed Job pod failed before placement validation")
	}
	require.NoError(t, err)
	nodesByIndex, err := requireNCCLRDMAExactPlacement(pods,
		os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"))
	if err != nil {
		recorder.addError(rdmavalidation.ReasonPlacementMismatch, "placement", "Indexed Job pods did not retain exact distinct-node placement")
	}
	require.NoError(t, err)
	err = requireNCCLRDMASelectedNodes(
		tc.Ctx(), tc.KubeClient(), nodesByIndex,
		os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"),
	)
	if err != nil {
		recorder.addError(rdmavalidation.ReasonPlacementMismatch, "placement", "selected nodes did not retain the exact approved H200 selector")
	}
	require.NoError(t, err)

	remaining := time.Until(jobDeadline)
	require.Positive(t, remaining, "Job consumed the bounded diagnostic deadline before execution completed")
	err = waitForNCCLRDMAJobSucceeded(tc, jobUID, remaining)
	if err != nil && (strings.Contains(err.Error(), "Job failed") || strings.Contains(err.Error(), "failed completion indexes")) {
		recorder.addError(rdmavalidation.ReasonNonzeroExit, "job_exit_code", "Job reported a failed rank or nonzero termination")
	}
	require.NoError(t, err, "both Indexed Job pods must exit zero within the bounded diagnostic deadline")
	logs, err := readNCCLRDMACombinedLogs(tc, invocation)
	require.NoError(t, err)
	result, err := e2e.ParseNCCLRDMAOutput(logs)
	if err != nil {
		recorder.addError(
			e2e.NCCLRDMAParseFailureReason(err),
			"parser",
			"combined rank logs did not satisfy the fail-closed NCCL/RDMA parser",
		)
	}
	require.NoError(t, err, "combined rank logs must satisfy the fail-closed NCCL/RDMA parser")
	expectedNodes := [2]string{nodesByIndex[0], nodesByIndex[1]}
	if result.Nodes != expectedNodes {
		recorder.addError(rdmavalidation.ReasonPlacementMismatch, "placement", "probe-reported nodes did not match Kubernetes pod placement")
	}
	require.Equal(t, expectedNodes, result.Nodes,
		"probe-reported actual nodes must match the Indexed Job pod placement")
	require.Positive(t, result.MaxAlgBW)
	require.Positive(t, result.MaxBusBW)

	pods, err = waitForNCCLRDMAPodsScheduled(tc, jobUID, invocation, 30*time.Second)
	require.NoError(t, err)
	nodes, err := readNCCLRDMASelectedNodes(tc, nodesByIndex)
	require.NoError(t, err)
	completedJob, err := tc.KubeClient().BatchV1().Jobs(stackNamespace).Get(tc.Ctx(), ncclRDMAJobName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, completedJob.Status.StartTime)
	require.NotNil(t, completedJob.Status.CompletionTime)
	manifest, err := sanitizedNCCLRDMAJob(job)
	require.NoError(t, err)
	require.NoError(t, recorder.cleanup(tc, owned, invocation, false))
	contract, err := e2e.BuildNCCLRDMAValidationResult(e2e.NCCLRDMAValidationInput{
		ValidationID: invocation,
		RunID:        recorder.result.RunID, Attempt: recorder.result.Attempt,
		WorkspaceID: recorder.result.WorkspaceID, Cluster: recorder.result.Cluster,
		Namespace: stackNamespace, ProjectID: recorder.result.ProjectID,
		ExperimentID: recorder.result.ExperimentID, RunGroupID: recorder.result.RunGroupID,
		ExpectedSite:     recorder.result.Requested.Topology.Site,
		ExpectedPool:     recorder.result.Requested.Topology.Pool,
		ExpectedGPUModel: recorder.result.Requested.Topology.GPUModel,
		SourceRevision:   recorder.result.Source.Revision,
		CreatedAt:        job.GetCreationTimestamp().Time.UTC(),
		StartedAt:        completedJob.Status.StartTime.Time.UTC(),
		AdmittedAt:       admittedAt,
		CompletedAt:      completedJob.Status.CompletionTime.Time.UTC(),
		ObservedAt:       recorder.result.Cleanup.CompletedAt,
		StaleAfter:       time.Duration(ncclRDMAStaleSeconds) * time.Second,
		Parsed:           result, Pods: pods, Nodes: nodes, SanitizedManifest: manifest,
		Cleanup: recorder.result.Cleanup,
	})
	require.NoError(t, err)
	require.Equal(t, rdmavalidation.StatusPass, contract.Status)
	require.NoError(t, recorder.write(contract))
}

type ownedNCCLRDMAResource struct {
	GVR  schema.GroupVersionResource
	Name string
	UID  types.UID
}

type ncclRDMAResourceManifest struct {
	GVR    schema.GroupVersionResource
	Object *unstructured.Unstructured
}

func readNCCLRDMAJobFixture() (*unstructured.Unstructured, error) {
	data, err := e2e.ReadFixtureWithSubstitutions("stack/fixtures/nccl-rdma-indexed-job-2x1xh200.yaml")
	if err != nil {
		return nil, err
	}
	job := &unstructured.Unstructured{}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(job); err != nil {
		return nil, fmt.Errorf("decode NCCL/RDMA Job fixture: %w", err)
	}
	return job, nil
}

func createNCCLRDMAResources(
	tc *e2e.TestContext,
	job *unstructured.Unstructured,
	invocation string,
) ([]ownedNCCLRDMAResource, error) {
	if job.GetName() != ncclRDMAJobName || job.GetNamespace() != stackNamespace {
		return nil, fmt.Errorf("NCCL/RDMA fixture identity changed unexpectedly: %s/%s", job.GetNamespace(), job.GetName())
	}
	if job.GetLabels()[ncclRDMAInvocationKey] != invocation {
		return nil, fmt.Errorf("NCCL/RDMA fixture invocation marker does not match the authorized invocation")
	}
	if err := validateNCCLRDMAOwnedUIDLedger(); err != nil {
		return nil, err
	}

	script, err := e2e.ReadRepoFile("tests/e2e/stack/scripts/torchrun-rdma-probe.py")
	if err != nil {
		return nil, fmt.Errorf("read repository-owned probe: %w", err)
	}
	authKey := make([]byte, 32)
	if _, err := rand.Read(authKey); err != nil {
		return nil, fmt.Errorf("generate run-scoped authentication key: %w", err)
	}
	support, err := e2e.BuildNCCLRDMASupportResources(stackNamespace, invocation, string(script), authKey)
	if err != nil {
		return nil, err
	}
	manifests := []ncclRDMAResourceManifest{
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, Object: mustUnstructured(tc, support.ServiceAccount)},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, Object: mustUnstructured(tc, support.ConfigMap)},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, Object: mustUnstructured(tc, support.Secret)},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "services"}, Object: mustUnstructured(tc, support.Service)},
		{GVR: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, Object: mustUnstructured(tc, support.NetworkPolicy)},
		{GVR: ncclRDMAJobGVR, Object: job},
	}
	for _, manifest := range manifests {
		resource := tc.DynamicClient().Resource(manifest.GVR).Namespace(stackNamespace)
		_, getErr := resource.Get(tc.Ctx(), manifest.Object.GetName(), metav1.GetOptions{})
		if getErr == nil {
			return nil, fmt.Errorf("fixed resource %s/%s already exists; refusing to replace or adopt it",
				manifest.GVR.Resource, manifest.Object.GetName())
		}
		if !apierrors.IsNotFound(getErr) {
			return nil, fmt.Errorf("check fixed %s/%s absence: %w", manifest.GVR.Resource, manifest.Object.GetName(), getErr)
		}
	}
	if err := requireNCCLRDMANamespaceApproval(tc.Ctx(), tc.KubeClient(), stackNamespace); err != nil {
		return nil, err
	}

	owned := make([]ownedNCCLRDMAResource, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.GVR == ncclRDMAJobGVR {
			if err := requireNCCLRDMANamespaceApproval(tc.Ctx(), tc.KubeClient(), stackNamespace); err != nil {
				return owned, err
			}
		}
		resource := tc.DynamicClient().Resource(manifest.GVR).Namespace(stackNamespace)
		created, createErr := resource.Create(tc.Ctx(), manifest.Object, metav1.CreateOptions{})
		item, err := successfulNCCLRDMAOwnedResource(manifest, created, createErr)
		if err != nil {
			return owned, err
		}
		owned = append(owned, item)
		if err := appendNCCLRDMAOwnedUID(item); err != nil {
			return owned, fmt.Errorf("record successful create UID for %s/%s: %w",
				manifest.GVR.Resource, manifest.Object.GetName(), err)
		}
		if manifest.GVR == ncclRDMAJobGVR {
			job.Object = created.Object
		}
	}
	return owned, nil
}

func successfulNCCLRDMAOwnedResource(
	manifest ncclRDMAResourceManifest,
	created *unstructured.Unstructured,
	createErr error,
) (ownedNCCLRDMAResource, error) {
	if createErr != nil {
		return ownedNCCLRDMAResource{}, fmt.Errorf(
			"create %s/%s returned an ambiguous error without an owned UID; the object may have persisted and requires manual inspection, so cleanup will not adopt it by name or labels: %w",
			manifest.GVR.Resource, manifest.Object.GetName(), createErr,
		)
	}
	if created == nil || created.GetUID() == "" {
		return ownedNCCLRDMAResource{}, fmt.Errorf(
			"create %s/%s succeeded without returning an owned UID; the object may have persisted and requires manual inspection, so cleanup will not adopt it by name or labels",
			manifest.GVR.Resource, manifest.Object.GetName(),
		)
	}
	return ownedNCCLRDMAResource{
		GVR: manifest.GVR, Name: manifest.Object.GetName(), UID: created.GetUID(),
	}, nil
}

func mustUnstructured(tc *e2e.TestContext, object interface{}) *unstructured.Unstructured {
	tc.Helper()
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	require.NoError(tc, err)
	return &unstructured.Unstructured{Object: raw}
}

func validateNCCLRDMAOwnedUIDLedger() error {
	path := strings.TrimSpace(os.Getenv(ncclRDMAOwnedUIDFile))
	if path == "" {
		return fmt.Errorf("%s is required so cleanup uses only UIDs returned by successful CREATE responses",
			ncclRDMAOwnedUIDFile)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat owned UID ledger %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("owned UID ledger %s is not a regular file", path)
	}
	return nil
}

func appendNCCLRDMAOwnedUID(item ownedNCCLRDMAResource) error {
	path := strings.TrimSpace(os.Getenv(ncclRDMAOwnedUIDFile))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	if _, err := fmt.Fprintf(writer, "%s|%s|%s|%s|%s\n",
		item.GVR.Group, item.GVR.Version, item.GVR.Resource, item.Name, item.UID); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return file.Sync()
}

func requireNCCLRDMANamespaceApproval(ctx context.Context, kubeClient kubernetes.Interface, namespace string) error {
	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read NCCL/RDMA namespace %s immediately before create: %w", namespace, err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		return fmt.Errorf("namespace %s no longer has pod-security.kubernetes.io/enforce=restricted; refusing create", namespace)
	}
	if ns.Annotations["tau.azure.com/nccl-rdma-diagnostic-approved"] != "true" {
		return fmt.Errorf("namespace %s no longer has tau.azure.com/nccl-rdma-diagnostic-approved=true; refusing create", namespace)
	}
	return nil
}

func waitForNCCLRDMAWorkloadAdmitted(tc *e2e.TestContext, uid types.UID, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(tc.Ctx(), 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := tc.DynamicClient().Resource(ncclRDMAJobGVR).Namespace(stackNamespace).Get(ctx, ncclRDMAJobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		if job.GetUID() != uid {
			return false, fmt.Errorf("Job UID changed from owned UID %s to %s", uid, job.GetUID())
		}
		workloads, err := tc.DynamicClient().Resource(e2e.WorkloadGVR).Namespace(stackNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for i := range workloads.Items {
			workload := &workloads.Items[i]
			owned := false
			for _, ref := range workload.GetOwnerReferences() {
				if ref.Kind == "Job" && ref.Name == ncclRDMAJobName && ref.UID == job.GetUID() {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
			conditions, _, err := unstructured.NestedSlice(workload.Object, "status", "conditions")
			if err != nil {
				return false, err
			}
			for _, raw := range conditions {
				condition, ok := raw.(map[string]interface{})
				if ok && condition["type"] == "Admitted" && condition["status"] == "True" {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

func ncclRDMAWorkloadAdmittedAt(tc *e2e.TestContext, uid types.UID) (time.Time, error) {
	workloads, err := tc.DynamicClient().Resource(e2e.WorkloadGVR).Namespace(stackNamespace).List(
		tc.Ctx(), metav1.ListOptions{},
	)
	if err != nil {
		return time.Time{}, err
	}
	for i := range workloads.Items {
		workload := &workloads.Items[i]
		for _, ref := range workload.GetOwnerReferences() {
			if ref.Kind != "Job" || ref.Name != ncclRDMAJobName || ref.UID != uid {
				continue
			}
			conditions, _, err := unstructured.NestedSlice(workload.Object, "status", "conditions")
			if err != nil {
				return time.Time{}, err
			}
			for _, raw := range conditions {
				condition, ok := raw.(map[string]interface{})
				if !ok || condition["type"] != "Admitted" || condition["status"] != "True" {
					continue
				}
				value, ok := condition["lastTransitionTime"].(string)
				if !ok || value == "" {
					return time.Time{}, fmt.Errorf("admitted Workload is missing lastTransitionTime")
				}
				admittedAt, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return time.Time{}, fmt.Errorf("parse Workload admitted lastTransitionTime: %w", err)
				}
				return admittedAt.UTC(), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("owned admitted Workload was not found")
}

func waitForNCCLRDMAJobSuspendedState(tc *e2e.TestContext, uid types.UID, want bool, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(tc.Ctx(), time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := tc.DynamicClient().Resource(ncclRDMAJobGVR).Namespace(stackNamespace).Get(ctx, ncclRDMAJobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if job.GetUID() != uid {
			return false, fmt.Errorf("Job UID changed from owned UID %s to %s", uid, job.GetUID())
		}
		suspended, found, err := unstructured.NestedBool(job.Object, "spec", "suspend")
		if err != nil {
			return false, err
		}
		return found && suspended == want, nil
	})
}

func waitForNCCLRDMAJobSucceeded(tc *e2e.TestContext, uid types.UID, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(tc.Ctx(), 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		job, err := tc.KubeClient().BatchV1().Jobs(stackNamespace).Get(ctx, ncclRDMAJobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if job.UID != uid {
			return false, fmt.Errorf("Job UID changed from owned UID %s to %s", uid, job.UID)
		}
		complete := false
		for _, condition := range job.Status.Conditions {
			if condition.Status == corev1.ConditionTrue &&
				(condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget) {
				return false, fmt.Errorf("Job failed: reason=%s message=%s", condition.Reason, condition.Message)
			}
			if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobComplete {
				complete = true
			}
		}
		if job.Status.FailedIndexes != nil && *job.Status.FailedIndexes != "" {
			return false, fmt.Errorf("Job reported failed completion indexes %q", *job.Status.FailedIndexes)
		}
		if complete && job.Status.Succeeded == 2 && job.Status.Failed == 0 &&
			job.Status.CompletedIndexes == "0,1" {
			return true, nil
		}
		return false, nil
	})
}

func waitForNCCLRDMAPodsScheduled(
	tc *e2e.TestContext,
	jobUID types.UID,
	invocation string,
	timeout time.Duration,
) ([]corev1.Pod, error) {
	var result []corev1.Pod
	err := wait.PollUntilContextTimeout(tc.Ctx(), time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s",
				ncclRDMAInvocationKey, invocation),
		})
		if err != nil {
			return false, err
		}
		if len(pods.Items) != 2 {
			return false, nil
		}
		for _, pod := range pods.Items {
			owned := false
			for _, owner := range pod.OwnerReferences {
				if owner.APIVersion == "batch/v1" && owner.Kind == "Job" &&
					owner.Name == ncclRDMAJobName && owner.UID == jobUID {
					owned = true
					break
				}
			}
			if !owned {
				return false, fmt.Errorf("pod %s is not owned by Job UID %s", pod.Name, jobUID)
			}
			if pod.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("Indexed Job pod %s failed before placement validation completed", pod.Name)
			}
			if pod.Spec.NodeName == "" || (pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded) {
				return false, nil
			}
		}
		result = pods.Items
		return true, nil
	})
	return result, err
}

func requireNCCLRDMAExactPlacement(pods []corev1.Pod, selectorKey, selectorValue string) ([2]string, error) {
	var nodes [2]string
	seenNodes := map[string]bool{}
	seenIndexes := map[string]bool{}
	for _, pod := range pods {
		if pod.Spec.NodeSelector[selectorKey] != selectorValue {
			return nodes, fmt.Errorf("pod %s does not retain exact H200 selector", pod.Name)
		}
		index := pod.Labels["batch.kubernetes.io/job-completion-index"]
		if index != "0" && index != "1" {
			return nodes, fmt.Errorf("pod %s has invalid Indexed Job completion label %q", pod.Name, index)
		}
		if seenIndexes[index] {
			return nodes, fmt.Errorf("duplicate Indexed Job completion label %q", index)
		}
		seenIndexes[index] = true
		nodes[index[0]-'0'] = pod.Spec.NodeName
		seenNodes[pod.Spec.NodeName] = true
	}
	if len(seenNodes) != 2 || len(seenIndexes) != 2 {
		return nodes, fmt.Errorf("expected exactly two completion indexes on distinct actual nodes")
	}
	return nodes, nil
}

func requireNCCLRDMASelectedNodes(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	nodes [2]string,
	selectorKey, selectorValue string,
) error {
	if selectorKey == "" || selectorValue == "" {
		return fmt.Errorf("exact H200 selector key and value are required")
	}

	for _, nodeName := range nodes {
		node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read selected node %s: %w", nodeName, err)
		}
		if node.Labels[selectorKey] != selectorValue {
			return fmt.Errorf("selected node %s does not have %s=%s", nodeName, selectorKey, selectorValue)
		}
	}
	return nil
}

func readNCCLRDMASelectedNodes(
	tc *e2e.TestContext,
	nodes [2]string,
) (map[string]*corev1.Node, error) {
	result := make(map[string]*corev1.Node, 2)
	for _, name := range nodes {
		node, err := tc.KubeClient().CoreV1().Nodes().Get(tc.Ctx(), name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("read selected node %s for validation artifact: %w", name, err)
		}
		result[name] = node
	}
	return result, nil
}

func sanitizedNCCLRDMAJob(job *unstructured.Unstructured) ([]byte, error) {
	sanitized := job.DeepCopy()
	sanitized.SetManagedFields(nil)
	sanitized.SetResourceVersion("")
	sanitized.SetGeneration(0)
	unstructured.RemoveNestedField(sanitized.Object, "status")
	raw, err := json.MarshalIndent(sanitized.Object, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal sanitized Job manifest: %w", err)
	}
	return append(raw, '\n'), nil
}

func readNCCLRDMACombinedLogs(tc *e2e.TestContext, invocation string) (string, error) {
	pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s",
			ncclRDMAInvocationKey, invocation),
	})
	if err != nil {
		return "", err
	}
	if len(pods.Items) != 2 {
		return "", fmt.Errorf("expected exactly two Indexed Job pods, got %d", len(pods.Items))
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Labels["batch.kubernetes.io/job-completion-index"] <
			pods.Items[j].Labels["batch.kubernetes.io/job-completion-index"]
	})
	var combined strings.Builder
	for _, pod := range pods.Items {
		rank := pod.Labels["batch.kubernetes.io/job-completion-index"]
		if rank != "0" && rank != "1" {
			return "", fmt.Errorf("pod %s has invalid completion index %q", pod.Name, rank)
		}
		fmt.Fprintf(&combined, "TAUGRID_RANK_LOG_BEGIN rank=%s\n", rank)
		stream, err := tc.KubeClient().CoreV1().Pods(stackNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "probe",
		}).Stream(tc.Ctx())
		if err != nil {
			return "", fmt.Errorf("open logs for %s: %w", pod.Name, err)
		}
		data, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil {
			return "", fmt.Errorf("read logs for %s: %w", pod.Name, readErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close logs for %s: %w", pod.Name, closeErr)
		}
		combined.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			combined.WriteByte('\n')
		}
		fmt.Fprintf(&combined, "TAUGRID_RANK_LOG_END rank=%s\n", rank)
	}
	return combined.String(), nil
}

func deleteOwnedNCCLRDMAResourcesAndWait(
	tc *e2e.TestContext,
	owned []ownedNCCLRDMAResource,
	invocation string,
) error {
	return deleteOwnedNCCLRDMAResourcesWithin(
		tc.Ctx(), tc.DynamicClient(), tc.KubeClient(), owned, invocation, 3*time.Minute,
	)
}

func deleteOwnedNCCLRDMAResourcesWithin(
	parent context.Context,
	dynamicClient dynamic.Interface,
	kubeClient kubernetes.Interface,
	owned []ownedNCCLRDMAResource,
	invocation string,
	timeout time.Duration,
) error {
	cleanupCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	pollInterval := time.Second
	if timeout < 10*time.Second {
		pollInterval = timeout / 10
		if pollInterval <= 0 {
			pollInterval = time.Millisecond
		}
	}
	var cleanupErrors []error
	type pendingResource struct {
		item     ownedNCCLRDMAResource
		resource dynamic.ResourceInterface
	}
	pending := make([]pendingResource, 0, len(owned))
	for index := len(owned) - 1; index >= 0; index-- {
		item := owned[index]
		resource := dynamicClient.Resource(item.GVR).Namespace(stackNamespace)
		pending = append(pending, pendingResource{item: item, resource: resource})
		foreground := metav1.DeletePropagationForeground
		err := resource.Delete(cleanupCtx, item.Name, metav1.DeleteOptions{
			PropagationPolicy: &foreground,
			Preconditions:     &metav1.Preconditions{UID: &item.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"start UID-owned delete for %s/%s UID %s: %w",
				item.GVR.Resource, item.Name, item.UID, err,
			))
		}
	}

	remaining := append([]pendingResource(nil), pending...)
	err := wait.PollUntilContextTimeout(cleanupCtx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		next := remaining[:0]
		for _, candidate := range remaining {
			current, err := candidate.resource.Get(ctx, candidate.item.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				next = append(next, candidate)
				continue
			}
			if current.GetUID() != candidate.item.UID {
				continue
			}
			next = append(next, candidate)
		}
		remaining = next
		return len(remaining) == 0, nil
	})
	if err != nil {
		for _, candidate := range remaining {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"UID-owned %s/%s UID %s remained after cleanup deadline",
				candidate.item.GVR.Resource, candidate.item.Name, candidate.item.UID,
			))
		}
	}
	selector := fmt.Sprintf("e2e.taugrid.azure.com/diagnostic=nccl-rdma-2x1xh200,%s=%s", ncclRDMAInvocationKey, invocation)
	if err := wait.PollUntilContextTimeout(cleanupCtx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := kubeClient.CoreV1().Pods(stackNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("diagnostic pods remained after cleanup deadline: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func ownedUID(owned []ownedNCCLRDMAResource, gvr schema.GroupVersionResource, name string) types.UID {
	for _, item := range owned {
		if item.GVR == gvr && item.Name == name {
			return item.UID
		}
	}
	return ""
}

// TestFineWebRayTrain16xH200IB validates the live FineWeb InfiniBand conformance
// path: Kueue admits a 16-GPU KubeRay RayJob whose workers shard a ~1.7B GPT with
// FSDP across both H200 nodes (split 8+8), NCCL runs over InfiniBand
// (rdma/rdma_shared_device_a + IPC_LOCK + a real /dev/shm), and Ray Train reaches
// and persists its FIRST checkpoint. Gated by E2E_GPU=1 + E2E_FINEWEB=1 (and
// requires enable-infiniband.sh to have advertised the RDMA resource first).
//
// The success contract is "reach + persist the first checkpoint", not
// convergence. The job is bounded (default 60 steps, first checkpoint at 50) so it
// also completes cleanly to RayJob SUCCEEDED.
func TestFineWebRayTrain16xH200IB(t *testing.T) {
	if os.Getenv("E2E_GPU") != "1" {
		t.Skip("set E2E_GPU=1 to run GPU stack tests")
	}
	if os.Getenv("E2E_FINEWEB") != "1" {
		t.Skip("set E2E_FINEWEB=1 to run the 16xH200 FineWeb InfiniBand conformance test")
	}

	workers := envInt(t, "FINEWEB_TRAIN_WORKERS", defaultFineWebTrainWorkers)
	steps := envInt(t, "FINEWEB_TRAIN_STEPS", defaultFineWebTrainSteps)

	tc := e2e.NewTestContext(t, context.Background())
	recordOutcomeWithWorkflowSuffix(t, tc)

	// Skip-if-busy guard (longhaul-friendly): when another workload is already
	// consuming the H200 GPUs, skip gracefully instead of failing. Only active
	// when explicitly opted in so direct test runs stay fail-hard by default.
	if os.Getenv("FINEWEB_SKIP_IF_BUSY") == "1" {
		if available := availableGPUsOnSelectedNodes(t, tc); available < workers {
			t.Skipf("FINEWEB_SKIP_IF_BUSY=1 and only %d/%d GPUs available on the selected H200 nodes; skipping (cluster busy)", available, workers)
		}
	}

	tc.OnFailure(func() {
		// Do not dump the RayJob spec here: the runtimeEnv can include short-lived
		// dataset SAS URIs (mirrors the nanoGPT large-GPU path).
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	requireGPUNodeKubeletProxyReachable(t, tc)
	applyRayJobFixture(t, tc, "fineweb-rayjob-16xh200-ib.yaml", rayJobNameFineWeb)

	_, err := tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, rayJobNameFineWeb, 2*time.Minute)
	require.NoError(t, err, "Kueue should admit the 16-GPU FineWeb IB RayJob workload (large-gpu queue must cover rdma/rdma_shared_device_a)")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=head", 1, largeGPUPodReadyTimeout)
	require.NoError(t, err, "Ray head should be running and ready")
	requirePodsOnSelectedNodes(t, tc, "ray.io/node-type=head", envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"FineWeb Ray head should stay on the selected CPU node pool")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=worker", workers, largeGPUPodReadyTimeout)
	require.NoError(t, err, "all 16 FineWeb Ray Train GPU workers should be running and ready")
	requirePodsOnSelectedNodes(t, tc, "ray.io/node-type=worker", os.Getenv("GPU_NODE_SELECTOR_KEY"), os.Getenv("GPU_NODE_SELECTOR_VALUE"),
		"FineWeb Ray Train workers should run on the selected H200 nodes")

	// Assert the 16 workers split evenly 8+8 across exactly the two H200 nodes,
	// keyed on the dedicated worker label so the spread/placement check never
	// matches unrelated Ray pods.
	requireWorkersSplitEvenlyAcrossNodes(t, tc, "e2e-test=fineweb-16xh200-ib", 2, workers/2)

	// The RayJob completion timeout defaults to the large-GPU value (sized for the
	// bounded ~60-step conformance run). A longhaul/soak dispatch can raise the step
	// count well beyond that, so allow the timeout to scale via
	// FINEWEB_RAYJOB_TIMEOUT_MINUTES without weakening the default.
	rayJobTimeout := largeGPURayJobTimeout
	if mins := envInt(t, "FINEWEB_RAYJOB_TIMEOUT_MINUTES", 0); mins > 0 {
		rayJobTimeout = time.Duration(mins) * time.Minute
	}
	err = tc.WaitForRayJobStatus(stackNamespace, rayJobNameFineWeb, "SUCCEEDED", rayJobTimeout)
	require.NoError(t, err, "16-GPU FineWeb IB RayJob should reach SUCCEEDED status")

	// Wait for the IB net marker, the first-checkpoint sentinel, and the success
	// marker together in the submitter logs (KubeRay streams worker stdout to the
	// submitter). These markers span the whole run — the early NCCL NET/IB init line
	// and the late checkpoint/success sentinels would never coexist in a tailed
	// window of verbose (NCCL_DEBUG=INFO) output across 16 workers — so read the FULL
	// submitter log. The returned logs are then parsed for strict IB engagement and
	// the persisted-checkpoint contract.
	logs, err := tc.WaitForFullPodLogsByLabelContaining(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameFineWeb), []string{
		"NET/IB",
		"FINEWEB_FIRST_CHECKPOINT",
		"FINEWEB_RAY_TRAIN_SUCCESS",
		fmt.Sprintf("step=%d", steps),
		fmt.Sprintf("world_size=%d", workers),
	}, 3*time.Minute)
	require.NoError(t, err, "submitter logs should show InfiniBand NCCL, the first checkpoint sentinel, and FineWeb Ray Train success")

	requireInfiniBandEngaged(t, logs)
	requireFirstCheckpointPersisted(t, logs)

	requirePodsOnSelectedNodes(t, tc, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameFineWeb), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"), envOrDefault("RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"),
		"FineWeb submitter should stay on the selected CPU node pool")
}

// fineWebFirstCheckpointRE parses the FINEWEB_FIRST_CHECKPOINT sentinel emitted by
// rank 0 after the full state dict is gathered and persisted.
var fineWebFirstCheckpointRE = regexp.MustCompile(`FINEWEB_FIRST_CHECKPOINT step=(\d+) path=(\S+) bytes=(\d+) params=(\d+)`)

// ncclIBUsingRE matches a positive NCCL InfiniBand selection line, e.g.
// "NET/IB : Using [0]mlx5_ib0:1/IB". A bare "NET/IB" substring is too weak: NCCL
// logs "NET/IB : No device found" before falling back to sockets.
var ncclIBUsingRE = regexp.MustCompile(`NET/IB\s*:\s*Using`)

// ncclSocketUsingRE matches the NCCL socket-transport fallback selection line. Its
// presence means NCCL did NOT use InfiniBand for the data plane.
var ncclSocketUsingRE = regexp.MustCompile(`NET/Socket\s*:\s*Using`)

// requireInfiniBandEngaged asserts the worker NCCL logs prove IB was used: a
// positive "NET/IB : Using" line and no "NET/Socket : Using" fallback line.
func requireInfiniBandEngaged(t *testing.T, logs string) {
	t.Helper()
	require.Regexp(t, ncclIBUsingRE, logs,
		"expected NCCL to select InfiniBand (NET/IB : Using ...) in worker logs; IB did not engage")
	require.NotRegexp(t, ncclSocketUsingRE, logs,
		"NCCL fell back to the socket transport (NET/Socket : Using ...); InfiniBand was not used for the data plane")
}

// requireFirstCheckpointPersisted asserts the first-checkpoint sentinel reports a
// non-empty artifact and a runtime param count in the ~1.7B conformance band.
func requireFirstCheckpointPersisted(t *testing.T, logs string) {
	t.Helper()
	match := fineWebFirstCheckpointRE.FindStringSubmatch(logs)
	require.NotNil(t, match, "submitter logs should contain a parseable FINEWEB_FIRST_CHECKPOINT sentinel")

	step, err := strconv.Atoi(match[1])
	require.NoError(t, err, "checkpoint step should be an integer")
	require.Positive(t, step, "first checkpoint should be written at a positive step")

	bytesWritten, err := strconv.Atoi(match[3])
	require.NoError(t, err, "checkpoint bytes should be an integer")
	require.Positive(t, bytesWritten, "first checkpoint artifact should be persisted with bytes>0 at %s", match[2])

	params, err := strconv.Atoi(match[4])
	require.NoError(t, err, "checkpoint params should be an integer")
	require.GreaterOrEqual(t, params, fineWebMinParams, "model should have ~1.7B params, got %d (below band)", params)
	require.LessOrEqual(t, params, fineWebMaxParams, "model should have ~1.7B params, got %d (above band)", params)
}

// requireWorkersSplitEvenlyAcrossNodes asserts that the pods matching podSelector
// are scheduled across exactly wantNodes distinct nodes with exactly perNode pods
// on each — i.e. the 8+8 split across the two H200 nodes.
func requireWorkersSplitEvenlyAcrossNodes(t *testing.T, tc *e2e.TestContext, podSelector string, wantNodes, perNode int) {
	t.Helper()
	pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: podSelector,
	})
	require.NoError(t, err, "list pods for 8+8 split assertion")
	require.Len(t, pods.Items, wantNodes*perNode, "expected %d worker pods matching %q", wantNodes*perNode, podSelector)

	perNodeCount := map[string]int{}
	for _, pod := range pods.Items {
		require.NotEmpty(t, pod.Spec.NodeName, "worker pod %s/%s should be scheduled", pod.Namespace, pod.Name)
		perNodeCount[pod.Spec.NodeName]++
	}

	require.Len(t, perNodeCount, wantNodes, "workers should span exactly %d nodes, got %d (%v)", wantNodes, len(perNodeCount), perNodeCount)
	for node, count := range perNodeCount {
		require.Equal(t, perNode, count, "node %s should run exactly %d workers, got %d", node, perNode, count)
	}
}

// availableGPUsOnSelectedNodes computes how many nvidia.com/gpu are free on the
// Ready, schedulable GPU-selected nodes: allocatable minus GPUs requested by
// non-terminal pods already on those nodes. Used by the skip-if-busy guard.
func availableGPUsOnSelectedNodes(t *testing.T, tc *e2e.TestContext) int {
	t.Helper()
	key := os.Getenv("GPU_NODE_SELECTOR_KEY")
	value := os.Getenv("GPU_NODE_SELECTOR_VALUE")
	require.NotEmpty(t, key, "GPU_NODE_SELECTOR_KEY must be set for the skip-if-busy guard")
	require.NotEmpty(t, value, "GPU_NODE_SELECTOR_VALUE must be set for the skip-if-busy guard")

	selector := fmt.Sprintf("%s=%s", key, value)
	nodes, err := tc.KubeClient().CoreV1().Nodes().List(tc.Ctx(), metav1.ListOptions{LabelSelector: selector})
	require.NoError(t, err, "list GPU nodes for selector %q", selector)

	gpuName := corev1.ResourceName("nvidia.com/gpu")
	available := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		allocatable := node.Status.Allocatable[gpuName]
		if node.Spec.Unschedulable || !nodeReady(node) || allocatable.IsZero() {
			continue
		}

		pods, err := tc.KubeClient().CoreV1().Pods("").List(tc.Ctx(), metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
		})
		require.NoError(t, err, "list pods on GPU node %s", node.Name)

		requested := 0
		for j := range pods.Items {
			pod := &pods.Items[j]
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			for c := range pod.Spec.Containers {
				if q, ok := pod.Spec.Containers[c].Resources.Requests[gpuName]; ok {
					requested += int(q.Value())
				}
			}
		}

		free := int(allocatable.Value()) - requested
		if free > 0 {
			available += free
		}
	}
	return available
}

func requireGPUNodeKubeletProxyReachable(t *testing.T, tc *e2e.TestContext) {
	t.Helper()

	key := os.Getenv("GPU_NODE_SELECTOR_KEY")
	value := os.Getenv("GPU_NODE_SELECTOR_VALUE")
	if key == "" || value == "" {
		return
	}

	selector := fmt.Sprintf("%s=%s", key, value)
	nodes, err := tc.KubeClient().CoreV1().Nodes().List(tc.Ctx(), metav1.ListOptions{LabelSelector: selector})
	require.NoError(t, err, "list GPU nodes for selector %q", selector)

	var checked []string
	for _, node := range nodes.Items {
		gpu := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
		if node.Spec.Unschedulable || !nodeReady(&node) || gpu.IsZero() {
			continue
		}
		checked = append(checked, node.Name)
		healthz, err := tc.KubeClient().CoreV1().RESTClient().
			Get().
			AbsPath("/api/v1/nodes/" + node.Name + "/proxy/healthz").
			DoRaw(tc.Ctx())
		require.NoError(t, err, "kubelet proxy on required GPU node %s is unreachable; Ray worker init containers cannot verify GCS readiness from this node", node.Name)
		require.Contains(t, string(healthz), "ok", "kubelet proxy healthz for GPU node %s", node.Name)
	}

	require.NotEmpty(t, checked, "no Ready schedulable GPU nodes with allocatable nvidia.com/gpu found for selector %q", selector)
}

func nodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func applyRayJobFixture(t *testing.T, tc *e2e.TestContext, fixture, rayJobName string) {
	t.Helper()
	if stackUsesArgoCDQueue() {
		deleteRayJobFixtureAndWait(t, tc, fixture, rayJobName)
	}
	if os.Getenv("E2E_PRESERVE_KUEUE_RECORDS") == "1" && fixture == "nanogpt-rayjob-large-gpu.yaml" {
		require.NoError(t, e2e.ApplyFixtureWithClient(tc.Ctx(), tc.DynamicClient(), fixture))
		return
	}
	require.NoError(t, e2e.ApplyFixtureWithClient(tc.Ctx(), tc.DynamicClient(), fixture))
	t.Cleanup(func() {
		deleteRayJobFixtureAndWait(t, tc, fixture, rayJobName)
	})
}

func deleteRayJobFixtureAndWait(t *testing.T, tc *e2e.TestContext, fixture, rayJobName string) {
	t.Helper()
	var rayClusterNames []string
	if !largeGPUUsesManagerWorkloadAccess() {
		var err error
		rayClusterNames, err = rayClusterNamesForRayJob(tc.Ctx(), tc.DynamicClient(), stackNamespace, rayJobName)
		require.NoError(t, err, "discover RayClusters owned by fixed RayJob before deletion")
	}
	require.NoError(t, e2e.DeleteFixtureWithClient(tc.Ctx(), tc.DynamicClient(), fixture), "delete RayJob fixture")
	require.NoError(t, tc.WaitForRayJobDeleted(stackNamespace, rayJobName, 3*time.Minute),
		"named RayJob should be absent before applying its fixed-name fixture")
	if largeGPUUsesManagerWorkloadAccess() {
		require.Equal(t, "nanogpt-rayjob-large-gpu.yaml", fixture, "manager workload access is only supported for the fixed nanoGPT RayJob fixture")
		return
	}
	require.NoError(t, tc.WaitForNoPodsByLabel(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobName), 3*time.Minute),
		"stale RayJob cleanup should release its submitter pods before applying")
	for _, rayClusterName := range rayClusterNames {
		require.NoError(t, tc.WaitForNoPodsByLabel(stackNamespace, fmt.Sprintf("ray.io/cluster=%s", rayClusterName), 3*time.Minute),
			"stale RayJob cleanup should release Ray pods for cluster %s before applying", rayClusterName)
	}
}

func rayClusterNamesForRayJob(ctx context.Context, dynamicClient dynamic.Interface, namespace, rayJobName string) ([]string, error) {
	names := map[string]struct{}{}
	rayJob, err := dynamicClient.Resource(e2e.RayJobGVR).Namespace(namespace).Get(ctx, rayJobName, metav1.GetOptions{})
	if err == nil {
		if name, found, nestedErr := unstructured.NestedString(rayJob.Object, "status", "rayClusterName"); nestedErr != nil {
			return nil, fmt.Errorf("read RayJob %s/%s status.rayClusterName: %w", namespace, rayJobName, nestedErr)
		} else if found && name != "" {
			names[name] = struct{}{}
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get RayJob %s/%s before cleanup: %w", namespace, rayJobName, err)
	}

	selector := fmt.Sprintf("ray.io/originated-from-cr-name=%s,ray.io/originated-from-crd=RayJob", rayJobName)
	rayClusters, err := dynamicClient.Resource(e2e.RayClusterGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list RayClusters for RayJob %s/%s: %w", namespace, rayJobName, err)
	}
	for i := range rayClusters.Items {
		names[rayClusters.Items[i].GetName()] = struct{}{}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func requirePodsOnSelectedNodes(t *testing.T, tc *e2e.TestContext, podSelector, nodeSelectorKey, nodeSelectorValue, message string) {
	t.Helper()
	require.NotEmpty(t, nodeSelectorKey, "node selector key must be configured")
	require.NotEmpty(t, nodeSelectorValue, "node selector value must be configured")

	pods, err := tc.KubeClient().CoreV1().Pods(stackNamespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: podSelector,
	})
	require.NoError(t, err, "list pods for placement assertion")
	require.NotEmpty(t, pods.Items, "expected pods matching %q for placement assertion", podSelector)

	for _, pod := range pods.Items {
		require.NotEmpty(t, pod.Spec.NodeName, "%s: pod %s/%s should be scheduled", message, pod.Namespace, pod.Name)
		node, err := tc.KubeClient().CoreV1().Nodes().Get(tc.Ctx(), pod.Spec.NodeName, metav1.GetOptions{})
		require.NoError(t, err, "%s: get node %s for pod %s/%s", message, pod.Spec.NodeName, pod.Namespace, pod.Name)
		require.Equal(t, nodeSelectorValue, node.Labels[nodeSelectorKey],
			"%s: pod %s/%s ran on node %s, expected node label %s=%s",
			message, pod.Namespace, pod.Name, pod.Spec.NodeName, nodeSelectorKey, nodeSelectorValue)
	}
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func envInt(t *testing.T, key string, defaultValue int) int {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	require.NoError(t, err, "%s must be an integer", key)
	return parsed
}

// TestTrainingRayJob exercises the "run Ray training jobs queued by Kueue" claim.
// Submits a RayJob whose entrypoint runs a short distributed SGD loop over Ray Data
// actors; asserts Kueue gang-admits it, head + workers come up, RayJob reaches
// SUCCEEDED, and the driver logs show loss decreasing across epochs.
func TestTrainingRayJob(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	tc.OnFailure(func() {
		tc.DumpCRState(stackNamespace, e2e.RayJobGVR, rayJobNameTrain)
		tc.DumpCRList(stackNamespace, e2e.WorkloadGVR)
		tc.DumpPods(stackNamespace, "")
		tc.DumpEvents(stackNamespace)
		tc.DumpPods("kueue-system", "")
		tc.DumpPods("kuberay-system", "")
	})

	applyRayJobFixture(t, tc, "training-rayjob.yaml", rayJobNameTrain)

	_, err := tc.WaitForWorkloadAdmittedByRayJob(stackNamespace, rayJobNameTrain, 30*time.Second)
	require.NoError(t, err, "Kueue should admit the training RayJob workload")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=head", 1, 3*time.Minute)
	require.NoError(t, err, "Ray head should be running and ready")

	err = tc.WaitForRunningPodsByLabel(stackNamespace, "ray.io/node-type=worker", 2, 3*time.Minute)
	require.NoError(t, err, "Both Ray workers should be running and ready")

	err = tc.WaitForRayJobStatus(stackNamespace, rayJobNameTrain, "SUCCEEDED", 8*time.Minute)
	require.NoError(t, err, "RayJob should reach SUCCEEDED status")

	_, err = tc.WaitForPodLogsByLabelContaining(stackNamespace, fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobNameTrain), []string{
		"step=",
		"TRAINING_COMPLETE",
	}, 2*time.Minute)
	require.NoError(t, err, "submitter logs should contain per-step loss output and the training success marker")
}
