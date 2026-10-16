// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package hardwarematrix

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

const (
	serveRayJobName = "e2e-matrix-serve"
	trainRayJobName = "e2e-matrix-train"
)

//go:embed fixtures/serve_gpu.py
var serveScript string

//go:embed fixtures/train_gpu.py
var trainScript string

type matrixConfig struct {
	namespace        string
	queue            string
	target           string
	site             string
	selectorKey      string
	selectorValue    string
	image            string
	rayVersion       string
	torchSpec        string
	torchIndexURL    string
	workers          int
	expectedHosts    int
	expectedPerHost  int
	requiredTopology string
}

func TestMain(m *testing.M) {
	code := m.Run()
	results.FlushAll()
	os.Exit(code)
}

func TestRayServeGPU(t *testing.T) {
	if !matrixEnabled() {
		t.Skip("set AI_RUNTIME_E2E=1 and E2E_HARDWARE_MATRIX=1 to run hardware matrix tests")
	}
	runMatrixWorkload(t, "serve", serveRayJobName, serveScript)
}

func TestRayTrainGPU(t *testing.T) {
	if !matrixEnabled() {
		t.Skip("set AI_RUNTIME_E2E=1 and E2E_HARDWARE_MATRIX=1 to run hardware matrix tests")
	}
	runMatrixWorkload(t, "train", trainRayJobName, trainScript)
}

func runMatrixWorkload(t *testing.T, workload, rayJobName, script string) {
	t.Helper()
	cfg := configFromEnv(t)
	tc := e2e.NewTestContext(t, context.Background())
	tc.RecordOutcomeAs(fmt.Sprintf("%s/%s/%dgpu", workload, cfg.target, cfg.workers))

	tc.OnFailure(func() {
		tc.DumpCRState(cfg.namespace, e2e.RayJobGVR, rayJobName)
		tc.DumpCRList(cfg.namespace, e2e.WorkloadGVR)
		tc.DumpPods(cfg.namespace, "")
		tc.DumpEvents(cfg.namespace)
	})

	cleanupMatrixResources(t, tc, cfg.namespace, rayJobName)
	configMapName := rayJobName + "-script"
	_, err := tc.KubeClient().CoreV1().ConfigMaps(cfg.namespace).Create(tc.Ctx(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configMapName,
			Labels: map[string]string{
				"tau.azure.com/e2e-hardware-matrix": cfg.target,
			},
		},
		Data: map[string]string{workload + "_gpu.py": script},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "create matrix script ConfigMap")

	rayJob := buildRayJob(cfg, workload, rayJobName, configMapName)
	_, err = tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(cfg.namespace).Create(
		tc.Ctx(), rayJob, metav1.CreateOptions{})
	require.NoError(t, err, "create %s RayJob", workload)

	t.Cleanup(func() {
		cleanupMatrixResources(t, tc, cfg.namespace, rayJobName)
	})

	admission, err := tc.WaitForWorkloadAdmittedByRayJob(cfg.namespace, rayJobName, 5*time.Minute)
	require.NoError(t, err, "Kueue should admit %s matrix workload", workload)
	requireTopologyAssignment(t, admission.Workload, cfg.workers)

	if recoveryNode := strings.TrimSpace(os.Getenv("MATRIX_SCHEDULER_RECOVERY_NODE")); recoveryNode != "" {
		require.Equal(t, "h200", cfg.target, "scheduler recovery is restricted to the verified H200 target")
		require.Equal(t, 16, cfg.workers, "scheduler recovery is restricted to the verified 16-GPU shape")
		err = tc.RecoverSingleSchedulerBlockedGPUWorker(e2e.SchedulerRecoveryOptions{
			Namespace:       cfg.namespace,
			WorkerSelector:  matrixWorkerSelector(cfg, workload),
			TargetNode:      recoveryNode,
			ExpectedWorkers: cfg.workers,
			Timeout:         10 * time.Minute,
			Eligible: func(observation e2e.SchedulerRecoveryObservation) bool {
				return eligibleForSchedulerRecovery(cfg.workers, cfg.expectedPerHost, observation)
			},
			Description: "matrix worker",
		})
		require.NoError(t, err)
	}

	err = tc.WaitForRunningPodsByLabel(
		cfg.namespace,
		matrixWorkerSelector(cfg, workload),
		cfg.workers,
		20*time.Minute,
	)
	require.NoError(t, err, "all %s GPU workers should become ready", workload)
	requirePlacement(t, tc, cfg, workload)

	err = tc.WaitForRayJobStatus(cfg.namespace, rayJobName, "SUCCEEDED", 40*time.Minute)
	require.NoError(t, err, "%s RayJob should succeed", workload)

	success := "SUCCESS: Ray Serve GPU inference completed on every replica"
	logMarkers := []string{success}
	if workload == "train" {
		success = "SUCCESS: Ray Train completed on every GPU worker"
		logMarkers = []string{"MATRIX_TRAIN_RESULT=", success}
	}
	_, err = tc.WaitForFullPodLogsByLabelContaining(
		cfg.namespace,
		fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobName),
		logMarkers,
		2*time.Minute,
	)
	require.NoError(t, err, "%s submitter logs should contain the success sentinel", workload)
}

func matrixEnabled() bool {
	return os.Getenv("AI_RUNTIME_E2E") == "1" && os.Getenv("E2E_HARDWARE_MATRIX") == "1"
}

func configFromEnv(t *testing.T) matrixConfig {
	t.Helper()

	workers := requirePositiveEnvInt(t, "MATRIX_WORKERS")
	expectedHosts := requirePositiveEnvInt(t, "MATRIX_EXPECTED_HOSTS")
	expectedPerHost := requirePositiveEnvInt(t, "MATRIX_EXPECTED_GPUS_PER_HOST")
	require.Equal(t, workers, expectedHosts*expectedPerHost,
		"MATRIX_WORKERS must equal MATRIX_EXPECTED_HOSTS * MATRIX_EXPECTED_GPUS_PER_HOST")

	selectorKey, selectorValue := parseSelector(t, requireEnv(t, "MATRIX_GPU_SELECTOR"))
	requiredTopology := requireEnv(t, "MATRIX_REQUIRED_TOPOLOGY")
	require.Contains(t, []string{"kubernetes.io/hostname", "tau.azure.com/site"}, requiredTopology)

	image := requireEnv(t, "RAY_E2E_IMAGE")
	return matrixConfig{
		namespace:        requireEnv(t, "MATRIX_NAMESPACE"),
		queue:            requireEnv(t, "MATRIX_QUEUE"),
		target:           requireEnv(t, "MATRIX_TARGET"),
		site:             requireEnv(t, "MATRIX_SITE"),
		selectorKey:      selectorKey,
		selectorValue:    selectorValue,
		image:            image,
		rayVersion:       rayVersion(t, image),
		torchSpec:        envOrDefault("MATRIX_TORCH_SPEC", "torch==2.7.1"),
		torchIndexURL:    envOrDefault("MATRIX_TORCH_INDEX_URL", "https://download.pytorch.org/whl/cu128"),
		workers:          workers,
		expectedHosts:    expectedHosts,
		expectedPerHost:  expectedPerHost,
		requiredTopology: requiredTopology,
	}
}

func buildRayJob(cfg matrixConfig, workload, name, configMapName string) *unstructured.Unstructured {
	scriptName := workload + "_gpu.py"
	workerLabels := map[string]any{
		"tau.azure.com/e2e-hardware-matrix": cfg.target,
		"tau.azure.com/e2e-matrix-workload": workload,
	}
	workerMetadata := map[string]any{
		"labels": workerLabels,
		"annotations": map[string]any{
			"kueue.x-k8s.io/podset-required-topology": cfg.requiredTopology,
		},
	}
	workerSpec := map[string]any{
		"nodeSelector": map[string]any{
			cfg.selectorKey:      cfg.selectorValue,
			"tau.azure.com/site": cfg.site,
		},
		"tolerations": []any{
			map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
			map[string]any{"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
		},
		"containers": []any{
			map[string]any{
				"name":  "ray-worker",
				"image": cfg.image,
				"resources": map[string]any{
					"requests": map[string]any{"cpu": "1", "memory": "4Gi", "nvidia.com/gpu": "1"},
					"limits":   map[string]any{"cpu": "2", "memory": "8Gi", "nvidia.com/gpu": "1"},
				},
				"volumeMounts": []any{
					map[string]any{"name": "matrix-script", "mountPath": "/matrix", "readOnly": true},
				},
			},
		},
		"volumes": []any{
			map[string]any{"name": "matrix-script", "configMap": map[string]any{"name": configMapName}},
		},
	}
	if cfg.expectedHosts > 1 {
		workerSpec["topologySpreadConstraints"] = []any{
			map[string]any{
				"maxSkew":           int64(1),
				"topologyKey":       "kubernetes.io/hostname",
				"whenUnsatisfiable": "DoNotSchedule",
				"labelSelector": map[string]any{
					"matchLabels": workerLabels,
				},
			},
		}
	}

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ray.io/v1",
		"kind":       "RayJob",
		"metadata": map[string]any{
			"name":      name,
			"namespace": cfg.namespace,
			"labels": map[string]any{
				"kueue.x-k8s.io/queue-name":         cfg.queue,
				"tau.azure.com/e2e-hardware-matrix": cfg.target,
			},
		},
		"spec": map[string]any{
			"entrypoint":               fmt.Sprintf("MATRIX_WORKERS=%d python3 /matrix/%s", cfg.workers, scriptName),
			"shutdownAfterJobFinishes": true,
			"ttlSecondsAfterFinished":  int64(600),
			"activeDeadlineSeconds":    int64(2100),
			"runtimeEnvYAML": fmt.Sprintf(
				"pip:\n  packages:\n    - %s\n  pip_install_options:\n    - --no-cache-dir\n    - --index-url\n    - %s\n    - --extra-index-url\n    - https://pypi.org/simple\n  pip_check: false\nenv_vars:\n  MATRIX_TORCH_SPEC: %q\n  MATRIX_TORCH_INDEX_URL: %q\n",
				cfg.torchSpec,
				cfg.torchIndexURL,
				cfg.torchSpec,
				cfg.torchIndexURL,
			),
			"submitterPodTemplate": map[string]any{
				"spec": map[string]any{
					"restartPolicy": "Never",
					"nodeSelector":  map[string]any{"kubernetes.azure.com/mode": "system"},
					"tolerations": []any{
						map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists", "effect": "NoSchedule"},
					},
					"containers": []any{
						map[string]any{
							"name":  "ray-job-submitter",
							"image": cfg.image,
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "250m", "memory": "256Mi"},
								"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
							},
						},
					},
				},
			},
			"rayClusterSpec": map[string]any{
				"rayVersion":              cfg.rayVersion,
				"enableInTreeAutoscaling": false,
				"headGroupSpec": map[string]any{
					"rayStartParams": map[string]any{"num-cpus": "0", "dashboard-host": "0.0.0.0"},
					"template": map[string]any{
						"metadata": map[string]any{
							"labels": map[string]any{"tau.azure.com/e2e-hardware-matrix": cfg.target},
						},
						"spec": map[string]any{
							"nodeSelector": map[string]any{"kubernetes.azure.com/mode": "system"},
							"tolerations": []any{
								map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists", "effect": "NoSchedule"},
							},
							"containers": []any{
								map[string]any{
									"name":  "ray-head",
									"image": cfg.image,
									"resources": map[string]any{
										"requests": map[string]any{"cpu": "1", "memory": "4Gi"},
										"limits":   map[string]any{"cpu": "2", "memory": "8Gi"},
									},
									"ports": []any{
										map[string]any{"name": "gcs-server", "containerPort": int64(6379)},
										map[string]any{"name": "dashboard", "containerPort": int64(8265)},
										map[string]any{"name": "client", "containerPort": int64(10001)},
										map[string]any{"name": "serve", "containerPort": int64(8000)},
									},
									"volumeMounts": []any{
										map[string]any{"name": "matrix-script", "mountPath": "/matrix", "readOnly": true},
									},
								},
							},
							"volumes": []any{
								map[string]any{"name": "matrix-script", "configMap": map[string]any{"name": configMapName}},
							},
						},
					},
				},
				"workerGroupSpecs": []any{
					map[string]any{
						"groupName":      "matrix-workers",
						"replicas":       int64(cfg.workers),
						"minReplicas":    int64(cfg.workers),
						"maxReplicas":    int64(cfg.workers),
						"rayStartParams": map[string]any{"num-gpus": "1"},
						"template": map[string]any{
							"metadata": workerMetadata,
							"spec":     workerSpec,
						},
					},
				},
			},
		},
	}}
	return object
}

func requireTopologyAssignment(t *testing.T, workload *unstructured.Unstructured, workers int) {
	t.Helper()
	require.NotNil(t, workload)
	assignments, found, err := unstructured.NestedSlice(
		workload.Object, "status", "admission", "podSetAssignments")
	require.NoError(t, err)
	require.True(t, found, "admitted Workload should contain podSetAssignments")

	for _, raw := range assignments {
		assignment, ok := raw.(map[string]any)
		if !ok || !strings.Contains(fmt.Sprint(assignment["name"]), "worker") {
			continue
		}
		topology, ok := assignment["topologyAssignment"].(map[string]any)
		require.True(t, ok, "worker PodSetAssignment should contain topologyAssignment")
		domains, ok := topology["domains"].([]any)
		require.True(t, ok, "topologyAssignment should contain domains")
		total := int64(0)
		for _, rawDomain := range domains {
			domain, ok := rawDomain.(map[string]any)
			require.True(t, ok)
			count, ok := domain["count"].(int64)
			require.True(t, ok)
			total += count
		}
		require.Equal(t, int64(workers), total, "topology assignment should cover every worker")
		return
	}
	t.Fatal("worker PodSetAssignment was not found")
}

func requirePlacement(t *testing.T, tc *e2e.TestContext, cfg matrixConfig, workload string) {
	t.Helper()
	pods, err := tc.KubeClient().CoreV1().Pods(cfg.namespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: matrixWorkerSelector(cfg, workload),
	})
	require.NoError(t, err)
	require.Len(t, pods.Items, cfg.workers)

	perHost := make(map[string]int)
	for i := range pods.Items {
		pod := &pods.Items[i]
		require.NotEmpty(t, pod.Spec.NodeName)
		node, err := tc.KubeClient().CoreV1().Nodes().Get(tc.Ctx(), pod.Spec.NodeName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, cfg.site, node.Labels["tau.azure.com/site"])
		require.Equal(t, cfg.selectorValue, node.Labels[cfg.selectorKey])
		perHost[pod.Spec.NodeName]++
		require.Equal(t, resource.MustParse("1"),
			pod.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")])
	}
	require.Len(t, perHost, cfg.expectedHosts)
	for host, count := range perHost {
		require.Equal(t, cfg.expectedPerHost, count, "unexpected worker count on %s", host)
	}
}

func eligibleForSchedulerRecovery(workers, expectedPerHost int, state e2e.SchedulerRecoveryObservation) bool {
	return workers == 16 &&
		expectedPerHost == 8 &&
		state.TotalWorkers == workers &&
		state.RunningWorkers == workers-1 &&
		state.PendingWorkers == 1 &&
		state.RunningOnTarget == expectedPerHost-1 &&
		state.RunningOnOtherNodes == workers-expectedPerHost &&
		state.RequestedGPUsOnTarget == int64(expectedPerHost-1) &&
		state.PendingGPURequest == 1 &&
		state.TargetAllocatableGPUs == int64(expectedPerHost) &&
		state.HasInsufficientEvent
}

func cleanupMatrixResources(t *testing.T, tc *e2e.TestContext, namespace, rayJobName string) {
	t.Helper()
	propagation := metav1.DeletePropagationForeground
	err := tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(namespace).Delete(
		tc.Ctx(), rayJobName, metav1.DeleteOptions{PropagationPolicy: &propagation})
	require.True(t, err == nil || apierrors.IsNotFound(err), "delete RayJob: %v", err)
	if err == nil {
		require.NoError(t, tc.WaitForRayJobDeleted(namespace, rayJobName, 3*time.Minute))
	}
	err = tc.KubeClient().CoreV1().ConfigMaps(namespace).Delete(
		tc.Ctx(), rayJobName+"-script", metav1.DeleteOptions{})
	require.True(t, err == nil || apierrors.IsNotFound(err), "delete script ConfigMap: %v", err)
}

func matrixWorkerSelector(cfg matrixConfig, workload string) string {
	return fmt.Sprintf(
		"ray.io/node-type=worker,tau.azure.com/e2e-hardware-matrix=%s,tau.azure.com/e2e-matrix-workload=%s",
		cfg.target,
		workload,
	)
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmpty(t, value, "%s is required", name)
	return value
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func requirePositiveEnvInt(t *testing.T, name string) int {
	t.Helper()
	value, err := strconv.Atoi(requireEnv(t, name))
	require.NoError(t, err, "%s must be an integer", name)
	require.Positive(t, value, "%s must be positive", name)
	return value
}

func parseSelector(t *testing.T, selector string) (string, string) {
	t.Helper()
	key, value, found := strings.Cut(selector, "=")
	require.True(t, found)
	require.NotEmpty(t, key)
	require.NotEmpty(t, value)
	require.NotContains(t, key, ",")
	require.NotContains(t, value, ",")
	return key, value
}

var rayVersionPattern = regexp.MustCompile(`ray([0-9]+\.[0-9]+\.[0-9]+)`)

func rayVersion(t *testing.T, image string) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv("RAY_E2E_VERSION")); value != "" {
		return value
	}
	match := rayVersionPattern.FindStringSubmatch(image)
	require.Len(t, match, 2, "RAY_E2E_IMAGE must include a rayX.Y.Z version or RAY_E2E_VERSION must be set")
	return match[1]
}

func TestBuildRayJobContract(t *testing.T) {
	cfg := matrixConfig{
		namespace:        "nightly",
		queue:            "h200",
		target:           "h200",
		site:             "site-h200",
		selectorKey:      "kueue.azure.com/gpu-series",
		selectorValue:    "nd-h200-v5",
		image:            "ray:test",
		rayVersion:       "2.55.1",
		torchSpec:        "torch==2.7.1",
		torchIndexURL:    "https://download.pytorch.org/whl/cu128",
		workers:          16,
		expectedHosts:    2,
		expectedPerHost:  8,
		requiredTopology: "tau.azure.com/site",
	}
	job := buildRayJob(cfg, "train", trainRayJobName, trainRayJobName+"-script")

	workers, found, err := unstructured.NestedSlice(
		job.Object, "spec", "rayClusterSpec", "workerGroupSpecs")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, workers, 1)
	group := workers[0].(map[string]any)
	require.Equal(t, int64(16), group["replicas"])
	annotations, found, err := unstructured.NestedStringMap(
		group, "template", "metadata", "annotations")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "tau.azure.com/site",
		annotations["kueue.x-k8s.io/podset-required-topology"])
	spread, found, err := unstructured.NestedSlice(group, "template", "spec", "topologySpreadConstraints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, spread, 1)
}

func TestParseSelector(t *testing.T) {
	key, value := parseSelector(t, "kueue.azure.com/gpu-series=ndm-a100-v4")
	require.Equal(t, "kueue.azure.com/gpu-series", key)
	require.Equal(t, "ndm-a100-v4", value)
}

func TestRayVersion(t *testing.T) {
	t.Setenv("RAY_E2E_VERSION", "")
	require.Equal(t, "2.55.1", rayVersion(t,
		"mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.55.1-cuda13.0"))
}

func TestEligibleForSchedulerRecovery(t *testing.T) {
	eligible := e2e.SchedulerRecoveryObservation{
		TotalWorkers:          16,
		RunningWorkers:        15,
		PendingWorkers:        1,
		RunningOnTarget:       7,
		RunningOnOtherNodes:   8,
		RequestedGPUsOnTarget: 7,
		PendingGPURequest:     1,
		TargetAllocatableGPUs: 8,
		HasInsufficientEvent:  true,
	}
	require.True(t, eligibleForSchedulerRecovery(16, 8, eligible))

	eligible.PendingGPURequest = 2
	require.False(t, eligibleForSchedulerRecovery(16, 8, eligible))
}

func TestPlacementShapeOrdering(t *testing.T) {
	shapes := []matrixConfig{
		{target: "h200", workers: 16},
		{target: "dgx-spark", workers: 2},
		{target: "a100", workers: 8},
	}
	sort.Slice(shapes, func(i, j int) bool {
		if shapes[i].target == shapes[j].target {
			return shapes[i].workers < shapes[j].workers
		}
		return shapes[i].target < shapes[j].target
	})
	require.Equal(t, []string{"a100", "dgx-spark", "h200"},
		[]string{shapes[0].target, shapes[1].target, shapes[2].target})
}
