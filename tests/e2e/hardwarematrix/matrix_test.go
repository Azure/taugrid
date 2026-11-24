// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package hardwarematrix

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	"sigs.k8s.io/yaml"

	profile "github.com/Azure/taugrid/core/resourceprofile"
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

//go:embed fixtures/runtime_contract.py
var runtimeContractScript string

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
	profile          matrixProfile
	expectedHosts    int
	expectedPerHost  int
	requiredTopology string
	clusterName      string
}

type matrixProfile struct {
	name          string
	lane          string
	workerPods    int
	gpusPerWorker int
	placement     string
}

func authoritativeMatrixProfile(totalGPUs int) (matrixProfile, bool) {
	switch totalGPUs {
	case 1:
		return matrixProfile{"azure.research.training.l", "training", 1, 1, profile.PlacementUnconstrained}, true
	case 2:
		return matrixProfile{"azure.research.training.2x", "training", 1, 2, profile.PlacementUnconstrained}, true
	case 8:
		return matrixProfile{"azure.research.large-memory.xl", "large-memory", 1, 8, profile.PlacementSameHost}, true
	case 16:
		return matrixProfile{"azure.research.large-memory.2node", "large-memory", 2, 8, profile.PlacementSameNetworkDomain}, true
	default:
		return matrixProfile{}, false
	}
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
	workerPods := cfg.workers
	gpusPerWorker := 1
	if e2e.TauRunEnabled() {
		rayJobName = e2e.TauRunName(rayJobName)
		workerPods = cfg.profile.workerPods
		gpusPerWorker = cfg.profile.gpusPerWorker
	}
	tc := e2e.NewTestContext(t, context.Background())
	tc.RecordOutcomeAs(fmt.Sprintf("%s/%s/%dgpu", workload, cfg.target, cfg.workers))

	tc.OnFailure(func() {
		tc.DumpCRState(cfg.namespace, e2e.RayJobGVR, rayJobName)
		tc.DumpCRList(cfg.namespace, e2e.WorkloadGVR)
		tc.DumpPods(cfg.namespace, "")
		tc.DumpEvents(cfg.namespace)
	})

	cleanupMatrixResources(t, tc, cfg.namespace, rayJobName)
	t.Cleanup(func() {
		cleanupMatrixResources(t, tc, cfg.namespace, rayJobName)
	})
	var err error
	if e2e.TauRunEnabled() {
		require.NoError(t, e2e.SubmitTauRun(t, tc.Ctx(), matrixTauConfig(cfg, workload, rayJobName), map[string][]byte{
			workload + "_gpu.py":  []byte(script),
			"runtime_contract.py": []byte(runtimeContractScript),
		}))
	} else {
		configMapName := rayJobName + "-script"
		_, err = tc.KubeClient().CoreV1().ConfigMaps(cfg.namespace).Create(tc.Ctx(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: configMapName,
				Labels: map[string]string{
					"tau.azure.com/e2e-hardware-matrix": cfg.target,
				},
			},
			Data: map[string]string{
				workload + "_gpu.py":  script,
				"runtime_contract.py": runtimeContractScript,
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err, "create matrix script ConfigMap")

		rayJob := buildRayJob(cfg, workload, rayJobName, configMapName)
		require.NoError(t, e2e.ConfigureWorkloadPriority(rayJob))
		_, err = tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(cfg.namespace).Create(
			tc.Ctx(), rayJob, metav1.CreateOptions{})
		require.NoError(t, err, "create %s RayJob", workload)
	}

	admission, err := tc.WaitForWorkloadAdmittedByRayJob(cfg.namespace, rayJobName, 5*time.Minute)
	require.NoError(t, err, "Kueue should admit %s matrix workload", workload)
	rayJob, err := tc.DynamicClient().Resource(e2e.RayJobGVR).Namespace(cfg.namespace).Get(
		tc.Ctx(), rayJobName, metav1.GetOptions{})
	require.NoError(t, err, "read admitted %s RayJob", workload)
	if !e2e.TauRunEnabled() || cfg.profile.placement != profile.PlacementUnconstrained {
		requireTopologyAssignment(t, admission.Workload, rayJob, workerPods)
	}
	if e2e.TauRunEnabled() {
		cfg.clusterName, err = tc.WaitForRayJobClusterName(cfg.namespace, rayJobName, 5*time.Minute)
		require.NoError(t, err, "KubeRay should initialize the admitted RayJob cluster name")
	}

	if recoveryNode := strings.TrimSpace(os.Getenv("MATRIX_SCHEDULER_RECOVERY_NODE")); recoveryNode != "" && gpusPerWorker == 1 {
		require.Equal(t, "h200", cfg.target, "scheduler recovery is restricted to the verified H200 target")
		require.Equal(t, 16, cfg.workers, "scheduler recovery is restricted to the verified 16-GPU shape")
		err = tc.RecoverSingleSchedulerBlockedGPUWorker(e2e.SchedulerRecoveryOptions{
			Namespace:       cfg.namespace,
			WorkerSelector:  matrixWorkerSelector(cfg, workload),
			TargetNode:      recoveryNode,
			ExpectedWorkers: workerPods,
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
		workerPods,
		20*time.Minute,
	)
	require.NoError(t, err, "all %s GPU workers should become ready", workload)
	requirePlacement(t, tc, cfg, workload, workerPods, gpusPerWorker)

	err = tc.WaitForRayJobStatus(cfg.namespace, rayJobName, "SUCCEEDED", 40*time.Minute)
	require.NoError(t, err, "%s RayJob should succeed", workload)

	success := "SUCCESS: Ray Serve GPU inference completed on every replica"
	logMarkers := []string{success}
	if workload == "train" {
		success = "SUCCESS: Ray Train completed on every GPU worker"
		logMarkers = []string{"MATRIX_TRAIN_RESULT=", success}
	}
	if e2e.TauRunEnabled() {
		_, err = tc.WaitForFullPodContainerLogsByLabelContaining(
			cfg.namespace,
			"ray.io/node-type=head,ray.io/cluster="+cfg.clusterName,
			"ray-driver-log-offload", logMarkers, 3*time.Minute)
	} else {
		_, err = tc.WaitForFullPodLogsByLabelContaining(
			cfg.namespace,
			fmt.Sprintf("batch.kubernetes.io/job-name=%s", rayJobName),
			logMarkers,
			2*time.Minute,
		)
	}
	require.NoError(t, err, "%s submitter logs should contain the success sentinel", workload)
}

func matrixTauConfig(cfg matrixConfig, workload, name string) map[string]any {
	policy := e2e.GovernedTauPolicy(cfg.namespace, "auto", cfg.profile.name, cfg.profile.lane)
	return map[string]any{
		"name":   name,
		"engine": "rayjob",
		"run": map[string]any{
			"entrypoint":                 workload + "_gpu.py",
			"working_dir":                ".",
			"ttl_seconds_after_finished": 600,
		},
		"runtime": map[string]any{
			"image": cfg.image,
			"pip":   []string{cfg.torchSpec},
			"env": map[string]string{
				"MATRIX_WORKERS":         strconv.Itoa(cfg.workers),
				"MATRIX_RAY_VERSION":     cfg.rayVersion,
				"MATRIX_TORCH_SPEC":      cfg.torchSpec,
				"MATRIX_TORCH_INDEX_URL": cfg.torchIndexURL,
				"NCCL_IB_DISABLE":        "1",
				"PIP_INDEX_URL":          cfg.torchIndexURL,
			},
		},
		"compute": map[string]any{
			"workers":          cfg.profile.workerPods,
			"gpus_per_worker":  cfg.profile.gpusPerWorker,
			"head_cpu_request": "1", "head_memory_request": "4Gi",
			"head_cpu_limit": "2", "head_memory_limit": "8Gi",
			"worker_cpu_request": "1", "worker_memory_request": "4Gi",
			"worker_cpu_limit": "2", "worker_memory_limit": "8Gi",
		},
		"policy":    policy,
		"execution": map[string]any{"launcher": "ray-train", "allow_nccl_override": true},
	}
}

func matrixEnabled() bool {
	return os.Getenv("AI_RUNTIME_E2E") == "1" && os.Getenv("E2E_HARDWARE_MATRIX") == "1"
}

func configFromEnv(t *testing.T) matrixConfig {
	t.Helper()

	workers := requirePositiveEnvInt(t, "MATRIX_WORKERS")
	selectedProfile, found := authoritativeMatrixProfile(workers)
	require.True(t, found, "MATRIX_WORKERS must map to an authoritative live workload profile")
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
		profile:          selectedProfile,
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
		"initContainers": []any{baseImageGuard(cfg)},
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
				"pip:\n  packages:\n    - %s\n  pip_install_options:\n    - --no-cache-dir\n    - --index-url\n    - %s\n  pip_check: false\nenv_vars:\n  NCCL_IB_DISABLE: \"1\"\n  PYTHONPATH: /matrix\n  MATRIX_RAY_VERSION: %q\n  MATRIX_TORCH_SPEC: %q\n  MATRIX_TORCH_INDEX_URL: %q\n",
				cfg.torchSpec,
				cfg.torchIndexURL,
				cfg.rayVersion,
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
							"initContainers": []any{baseImageGuard(cfg)},
							"nodeSelector":   map[string]any{"kubernetes.azure.com/mode": "system"},
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

func baseImageGuard(cfg matrixConfig) map[string]any {
	return map[string]any{
		"name":  "base-image-check",
		"image": cfg.image,
		"command": []any{
			"python", "/matrix/runtime_contract.py",
			"--ray-version", cfg.rayVersion,
		},
		"volumeMounts": []any{
			map[string]any{"name": "matrix-script", "mountPath": "/matrix", "readOnly": true},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "250m", "memory": "256Mi"},
			"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
		},
	}
}

func requireTopologyAssignment(t *testing.T, workload, rayJob *unstructured.Unstructured, workers int) {
	t.Helper()
	require.NotNil(t, workload)
	require.NotNil(t, rayJob)
	groups, found, err := unstructured.NestedSlice(rayJob.Object, "spec", "rayClusterSpec", "workerGroupSpecs")
	require.NoError(t, err)
	require.True(t, found, "admitted RayJob should contain workerGroupSpecs")
	require.Len(t, groups, 1, "matrix RayJob should contain exactly one worker group")
	group, ok := groups[0].(map[string]any)
	require.True(t, ok, "RayJob worker group should be an object")
	workerPodSet, found, err := unstructured.NestedString(group, "groupName")
	require.NoError(t, err)
	require.True(t, found, "RayJob worker group should contain groupName")
	require.NotEmpty(t, workerPodSet)
	assignments, found, err := unstructured.NestedSlice(
		workload.Object, "status", "admission", "podSetAssignments")
	require.NoError(t, err)
	require.True(t, found, "admitted Workload should contain podSetAssignments")

	for _, raw := range assignments {
		assignment, ok := raw.(map[string]any)
		if !ok || assignment["name"] != workerPodSet {
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

func requirePlacement(t *testing.T, tc *e2e.TestContext, cfg matrixConfig, workload string, workerPods, gpusPerWorker int) {
	t.Helper()
	pods, err := tc.KubeClient().CoreV1().Pods(cfg.namespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: matrixWorkerSelector(cfg, workload),
	})
	require.NoError(t, err)
	require.Len(t, pods.Items, workerPods)

	perHost := make(map[string]int)
	for i := range pods.Items {
		pod := &pods.Items[i]
		require.NotEmpty(t, pod.Spec.NodeName)
		node, err := tc.KubeClient().CoreV1().Nodes().Get(tc.Ctx(), pod.Spec.NodeName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, cfg.site, node.Labels["tau.azure.com/site"])
		require.Equal(t, cfg.selectorValue, node.Labels[cfg.selectorKey])
		perHost[pod.Spec.NodeName] += gpusPerWorker
		require.Equal(t, resource.MustParse(strconv.Itoa(gpusPerWorker)),
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
	if e2e.TauRunEnabled() {
		require.NoError(t, e2e.CancelTauRun(tc.Ctx(), namespace, rayJobName))
		return
	}
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
	if cfg.clusterName != "" {
		return "ray.io/node-type=worker,ray.io/cluster=" + cfg.clusterName
	}
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

func TestNativeTauMatrixRendering(t *testing.T) {
	binary := e2e.BuildTauForTest(t)
	for target, counts := range map[string][]int{"a100": {1, 8, 16}, "h200": {1, 8, 16}, "dgx-spark": {1, 2}} {
		for _, count := range counts {
			for _, workload := range []string{"serve", "train"} {
				t.Run(fmt.Sprintf("%s/%dgpu/%s", target, count, workload), func(t *testing.T) {
					selectedProfile, found := authoritativeMatrixProfile(count)
					require.True(t, found)
					cfg := matrixConfig{
						namespace: "nightly", queue: "matrix-" + target, target: target,
						site: "site-" + target, selectorKey: "tau.azure.com/gpu-class", selectorValue: target,
						image: "registry.example.com/ray:py3.12-ray2.58.0", rayVersion: "2.58.0",
						torchSpec: "torch==2.7.1", torchIndexURL: "https://download.pytorch.org/whl/cu128",
						workers: count, profile: selectedProfile,
					}
					platform := profile.WorkloadProfile{
						Name:        selectedProfile.name,
						WorkerCount: int32(selectedProfile.workerPods), GPUsPerWorker: int32(selectedProfile.gpusPerWorker),
						Mode: "fixed", Placement: selectedProfile.placement, DefaultLocalQueue: cfg.queue,
						Priorities: profile.ProfilePriorities{
							WorkloadPriorityClassName: "taugrid-default", PodPriorityClassName: "taugrid-default",
						},
					}
					script := trainScript
					if workload == "serve" {
						script = serveScript
					}
					job := e2e.RenderTauRunOffline(t, binary, matrixTauConfig(cfg, workload, "e2e-matrix-"+workload+"-42-1"),
						map[string][]byte{workload + "_gpu.py": []byte(script), "runtime_contract.py": []byte(runtimeContractScript)}, platform)
					require.Equal(t, "RayJob", job.GetKind())
					require.Equal(t, "taugrid-default", job.GetLabels()["kueue.x-k8s.io/priority-class"])
					ttl, found, err := unstructured.NestedInt64(job.Object, "spec", "ttlSecondsAfterFinished")
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, int64(600), ttl)
					groups, _, err := unstructured.NestedSlice(job.Object, "spec", "rayClusterSpec", "workerGroupSpecs")
					require.NoError(t, err)
					require.Len(t, groups, 1)
					group := groups[0].(map[string]any)
					require.Equal(t, int64(selectedProfile.workerPods), group["replicas"])
					if selectedProfile.placement != profile.PlacementUnconstrained {
						admitted := matrixTopologyAdmission(t, group["groupName"].(string), selectedProfile.workerPods)
						requireTopologyAssignment(t, admitted, job, selectedProfile.workerPods)
					}
					spec, _, err := unstructured.NestedMap(group, "template", "spec")
					require.NoError(t, err)
					require.Equal(t, "taugrid-default", spec["priorityClassName"])
					containers, _, err := unstructured.NestedSlice(spec, "containers")
					require.NoError(t, err)
					resources := containers[0].(map[string]any)["resources"].(map[string]any)
					requests := resources["requests"].(map[string]any)
					require.Equal(t, int64(selectedProfile.gpusPerWorker), requests["nvidia.com/gpu"])
					annotations, _, err := unstructured.NestedStringMap(group, "template", "metadata", "annotations")
					require.NoError(t, err)
					if selectedProfile.placement == profile.PlacementUnconstrained {
						require.NotContains(t, annotations, "kueue.x-k8s.io/podset-required-topology")
					} else {
						require.NotEmpty(t, annotations["kueue.x-k8s.io/podset-required-topology"])
					}
				})
			}
		}
	}
}

func matrixTopologyAdmission(t *testing.T, workerPodSet string, workers int) *unstructured.Unstructured {
	t.Helper()
	workload := &unstructured.Unstructured{}
	data, err := json.Marshal(map[string]any{
		"apiVersion": e2e.WorkloadGVR.GroupVersion().String(),
		"kind":       "Workload",
		"status": map[string]any{
			"admission": map[string]any{
				"podSetAssignments": []any{
					map[string]any{"name": "head"},
					map[string]any{
						"name": "unrelated-workers",
						"topologyAssignment": map[string]any{
							"domains": []any{map[string]any{"count": workers + 1}},
						},
					},
					map[string]any{
						"name": workerPodSet,
						"topologyAssignment": map[string]any{
							"domains": []any{map[string]any{"count": workers}},
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, workload.UnmarshalJSON(data))
	return workload
}

func TestBuildRayJobContract(t *testing.T) {
	for _, workerCount := range []int{8, 16} {
		t.Run(fmt.Sprintf("%dgpu", workerCount), func(t *testing.T) {
			cfg := matrixConfig{
				namespace:        "nightly",
				queue:            "h200",
				target:           "h200",
				site:             "site-h200",
				selectorKey:      "kueue.azure.com/gpu-series",
				selectorValue:    "nd-h200-v5",
				image:            "ray:test",
				rayVersion:       "2.58.0",
				torchSpec:        "torch==2.7.1",
				torchIndexURL:    "https://download.pytorch.org/whl/cu128",
				workers:          workerCount,
				expectedHosts:    workerCount / 8,
				expectedPerHost:  8,
				requiredTopology: "kubernetes.io/hostname",
			}
			if cfg.expectedHosts > 1 {
				cfg.requiredTopology = "tau.azure.com/site"
			}
			job := buildRayJob(cfg, "train", trainRayJobName, trainRayJobName+"-script")
			t.Setenv("E2E_WORKLOAD_PRIORITY_CLASS", "aks-low-priority")
			t.Setenv("E2E_POD_PRIORITY_CLASS", "tau-nightly-low")
			require.NoError(t, e2e.ConfigureWorkloadPriority(job))
			require.Equal(t, "aks-low-priority", job.GetLabels()["kueue.x-k8s.io/priority-class"])
			for _, path := range [][]string{
				{"spec", "submitterPodTemplate", "spec"},
				{"spec", "rayClusterSpec", "headGroupSpec", "template", "spec"},
			} {
				priority, _, err := unstructured.NestedString(job.Object, append(path, "priorityClassName")...)
				require.NoError(t, err)
				require.Equal(t, "tau-nightly-low", priority)
				policy, _, err := unstructured.NestedString(job.Object, append(path, "preemptionPolicy")...)
				require.NoError(t, err)
				require.Equal(t, "Never", policy)
			}

			runtimeEnvYAML, found, err := unstructured.NestedString(job.Object, "spec", "runtimeEnvYAML")
			require.NoError(t, err)
			require.True(t, found)
			var runtimeEnv struct {
				EnvVars map[string]any `json:"env_vars"`
				Pip     map[string]any `json:"pip"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(runtimeEnvYAML), &runtimeEnv))
			require.Equal(t, "1", runtimeEnv.EnvVars["NCCL_IB_DISABLE"])
			require.Equal(t, "/matrix", runtimeEnv.EnvVars["PYTHONPATH"])
			require.Equal(t, cfg.rayVersion, runtimeEnv.EnvVars["MATRIX_RAY_VERSION"])
			pipSpec, err := os.ReadFile("../../../scripts/images/ray-torch/pip_spec.json")
			require.NoError(t, err)
			var expectedPip map[string]any
			require.NoError(t, json.Unmarshal(pipSpec, &expectedPip))
			require.Equal(t, expectedPip, runtimeEnv.Pip)

			workers, found, err := unstructured.NestedSlice(
				job.Object, "spec", "rayClusterSpec", "workerGroupSpecs")
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, workers, 1)
			group := workers[0].(map[string]any)
			requireTopologyAssignment(t, matrixTopologyAdmission(t, group["groupName"].(string), workerCount), job, workerCount)
			priority, _, err := unstructured.NestedString(group, "template", "spec", "priorityClassName")
			require.NoError(t, err)
			require.Equal(t, "tau-nightly-low", priority)
			policy, _, err := unstructured.NestedString(group, "template", "spec", "preemptionPolicy")
			require.NoError(t, err)
			require.Equal(t, "Never", policy)
			require.Equal(t, int64(workerCount), group["replicas"])
			annotations, found, err := unstructured.NestedStringMap(
				group, "template", "metadata", "annotations")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, cfg.requiredTopology,
				annotations["kueue.x-k8s.io/podset-required-topology"])
			spread, found, err := unstructured.NestedSlice(group, "template", "spec", "topologySpreadConstraints")
			require.NoError(t, err)
			require.Equal(t, cfg.expectedHosts > 1, found)
			if found {
				require.Len(t, spread, 1)
			}

			podSpec, found, err := unstructured.NestedMap(group, "template", "spec")
			require.NoError(t, err)
			require.True(t, found)
			require.NotContains(t, podSpec, "securityContext")
			headSpec, found, err := unstructured.NestedMap(
				job.Object, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec")
			require.NoError(t, err)
			require.True(t, found)
			for _, spec := range []map[string]any{headSpec, podSpec} {
				initContainers, found, err := unstructured.NestedSlice(spec, "initContainers")
				require.NoError(t, err)
				require.True(t, found)
				require.Len(t, initContainers, 1)
				guard := initContainers[0].(map[string]any)
				require.Equal(t, cfg.image, guard["image"])
				command, found, err := unstructured.NestedStringSlice(guard, "command")
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, []string{
					"python", "/matrix/runtime_contract.py",
					"--ray-version", cfg.rayVersion,
				}, command)
				mounts, found, err := unstructured.NestedSlice(guard, "volumeMounts")
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, []any{
					map[string]any{"name": "matrix-script", "mountPath": "/matrix", "readOnly": true},
				}, mounts)
				require.NotContains(t, guard, "securityContext")
				for _, field := range []string{"requests", "limits"} {
					resources, found, err := unstructured.NestedStringMap(guard, "resources", field)
					require.NoError(t, err)
					require.True(t, found)
					require.Len(t, resources, 2)
					require.Contains(t, resources, "cpu")
					require.Contains(t, resources, "memory")
					appContainers, found, err := unstructured.NestedSlice(spec, "containers")
					require.NoError(t, err)
					require.True(t, found)
					appResources, found, err := unstructured.NestedStringMap(
						appContainers[0].(map[string]any), "resources", field)
					require.NoError(t, err)
					require.True(t, found)
					for key, quantity := range resources {
						initQuantity := resource.MustParse(quantity)
						appQuantity := resource.MustParse(appResources[key])
						require.LessOrEqual(t, initQuantity.Cmp(appQuantity), 0,
							"init resources must not increase the effective pod shape")
					}
				}
			}
			containers, found, err := unstructured.NestedSlice(podSpec, "containers")
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, containers, 1)
			container := containers[0].(map[string]any)
			require.NotContains(t, container, "securityContext")
			for _, field := range []string{"requests", "limits"} {
				resources, found, err := unstructured.NestedStringMap(container, "resources", field)
				require.NoError(t, err)
				require.True(t, found)
				require.Len(t, resources, 3)
				require.Contains(t, resources, "cpu")
				require.Contains(t, resources, "memory")
				require.Equal(t, "1", resources["nvidia.com/gpu"])
			}
		})
	}
}

func TestRuntimePackageContract(t *testing.T) {
	command := exec.Command("python3", "-B", "fixtures/test_runtime_contract.py")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
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
