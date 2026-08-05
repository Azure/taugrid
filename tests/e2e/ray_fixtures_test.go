package e2e

import (
	"os"
	"strings"
	"testing"
)

type rayDriverLogFixtureCase struct {
	fixture              string
	totalLogAnnotations  int
	workerLogAnnotations int
}

var rayDriverLogFixtureCases = []rayDriverLogFixtureCase{
	{fixture: "tests/e2e/stack/fixtures/inference-rayjob.yaml", totalLogAnnotations: 1, workerLogAnnotations: 0},
	{fixture: "tests/e2e/stack/fixtures/training-rayjob.yaml", totalLogAnnotations: 1, workerLogAnnotations: 0},
	{fixture: "tests/e2e/stack/fixtures/inference-rayjob-gpu.yaml", totalLogAnnotations: 2, workerLogAnnotations: 1},
	{fixture: "tests/e2e/stack/fixtures/training-rayjob-gpu.yaml", totalLogAnnotations: 2, workerLogAnnotations: 1},
	{fixture: "tests/e2e/stack/fixtures/nanogpt-rayjob-large-gpu.yaml", totalLogAnnotations: 1, workerLogAnnotations: 0},
	{fixture: "tests/e2e/stack/fixtures/fineweb-rayjob-16xh200-ib.yaml", totalLogAnnotations: 1, workerLogAnnotations: 0},
}

func TestRayFixturesSetStartupProbe(t *testing.T) {
	fixtures := []string{
		"tests/e2e/kuberay/fixtures/raycluster.yaml",
		"tests/e2e/stack/fixtures/inference-rayjob.yaml",
		"tests/e2e/stack/fixtures/inference-rayjob-gpu.yaml",
		"tests/e2e/stack/fixtures/training-rayjob-gpu.yaml",
	}

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			path, err := findRepoFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", fixture, err)
			}
			text := string(data)

			if got := strings.Count(text, "startupProbe:"); got != 2 {
				t.Fatalf("expected startupProbe on head and worker Ray containers, got %d", got)
			}
			for _, want := range []string{"periodSeconds: 5", "timeoutSeconds: 2", "failureThreshold: 60"} {
				if got := strings.Count(text, want); got != 2 {
					t.Fatalf("expected %s to contain %q twice, got %d", fixture, want, got)
				}
			}
			for _, want := range []string{"path: /api/version", "port: 8265", "path: /api/healthz", "port: 52365"} {
				if got := strings.Count(text, want); got != 1 {
					t.Fatalf("expected %s to contain %q once, got %d", fixture, want, got)
				}
			}
		})
	}
}

func TestRayVersionPrefersExplicitEnv(t *testing.T) {
	t.Setenv("RAY_E2E_VERSION", "2.55.1")
	t.Setenv("RAY_E2E_IMAGE", "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0")

	got, err := rayVersion()
	if err != nil {
		t.Fatalf("rayVersion returned error: %v", err)
	}
	if got != "2.55.1" {
		t.Fatalf("expected explicit RAY_E2E_VERSION to win, got %q", got)
	}
}

func TestRayVersionParsesImageTag(t *testing.T) {
	t.Setenv("RAY_E2E_VERSION", "")
	t.Setenv("RAY_E2E_IMAGE", "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.55.1-cuda13.0")

	got, err := rayVersion()
	if err != nil {
		t.Fatalf("rayVersion returned error: %v", err)
	}
	if got != "2.55.1" {
		t.Fatalf("expected Ray version parsed from image tag, got %q", got)
	}
}

func TestNanoGPTWorkloadHandlesMissingRayResultMetrics(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/fixtures/nanogpt_ray_train.py")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read nanogpt_ray_train.py: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"def final_metrics_from_result(result) -> dict:",
		`getattr(result, "metrics", None)`,
		`getattr(result, "metrics_dataframe", None)`,
		`final_step = int(metrics.get("step", steps))`,
		`world_size = int(metrics.get("world_size", workers))`,
		`final_loss_text = f"{final_loss:.4f}" if loss_metric is not None else "unreported"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected nanoGPT workload to handle missing Ray result metrics with %q", want)
		}
	}
}

func TestNanoGPTWorkloadGuardsTokenVocabRange(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/fixtures/nanogpt_ray_train.py")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read nanogpt_ray_train.py: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		`vocab_size = env_int("NANOGPT_VOCAB_SIZE", 65536)`,
		`max_token = max(int(x.max()), int(y.max()))`,
		`if max_token >= vocab_size:`,
		`increase NANOGPT_VOCAB_SIZE`,
		`GPT(GPTConfig(block_size=block_size, vocab_size=vocab_size))`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected nanoGPT workload to guard token IDs before CUDA execution with %q", want)
		}
	}
}

func TestGPURayJobFixturesPinSubmitterToCPUPool(t *testing.T) {
	fixtures := []string{
		"tests/e2e/stack/fixtures/inference-rayjob-gpu.yaml",
		"tests/e2e/stack/fixtures/training-rayjob-gpu.yaml",
	}

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			path, err := findRepoFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", fixture, err)
			}
			text := string(data)

			for _, want := range []string{
				"submitterPodTemplate:",
				"restartPolicy: Never",
				"{{RAY_SUBMITTER_NODE_SELECTOR_KEY}}: {{RAY_SUBMITTER_NODE_SELECTOR_VALUE}}",
				"name: ray-job-submitter",
				"image: {{RAY_IMAGE}}",
				"cpu: \"500m\"",
				"memory: \"200Mi\"",
				"cpu: \"1\"",
				"memory: \"1Gi\"",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("expected %s to contain %q", fixture, want)
				}
			}
		})
	}
}

func TestRayJobFixturesShareRayTempForDriverLogs(t *testing.T) {
	for _, tc := range rayDriverLogFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			text := readFixtureText(t, tc.fixture)
			for _, want := range []string{
				"name: prepare-ray-tmp",
				"name: ray-driver-log-offload",
				"chmod 1777 /tmp/ray",
				"mountPath: /tmp/ray",
				"subPathExpr: $(POD_NAME)",
				"rm -rf /tmp/ray/* || true",
				"job-driver-*.log",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("expected %s to contain %q", tc.fixture, want)
				}
			}
			if got := strings.Count(text, "mountPath: /tmp/ray"); got != 5 {
				t.Fatalf("expected %s to mount /tmp/ray in head init + head containers (main + log offload) and worker init + worker container, got %d", tc.fixture, got)
			}
			if got := strings.Count(text, "name: prepare-ray-tmp"); got != 2 {
				t.Fatalf("expected %s to prepare /tmp/ray on head and worker, got %d", tc.fixture, got)
			}
		})
	}
}

func TestNanoGPTRayJobFixtureUsesBaselineCompatibleRayTemp(t *testing.T) {
	const fixture = "tests/e2e/stack/fixtures/nanogpt-rayjob-large-gpu.yaml"
	text := readFixtureText(t, fixture)

	if strings.Contains(text, "hostPath:") {
		t.Fatalf("expected %s to avoid hostPath volumes enforced against by the taugrid-e2e baseline Pod Security policy", fixture)
	}
	if got := strings.Count(text, "- name: ray-tmp\n            emptyDir: {}"); got != 2 {
		t.Fatalf("expected %s to use pod-local emptyDir for head and worker ray-tmp volumes, got %d", fixture, got)
	}
}

func TestOtherRayJobFixturesUseResourceDiskForRayTemp(t *testing.T) {
	for _, tc := range rayDriverLogFixtureCases {
		if tc.fixture == "tests/e2e/stack/fixtures/nanogpt-rayjob-large-gpu.yaml" {
			continue
		}
		t.Run(tc.fixture, func(t *testing.T) {
			text := readFixtureText(t, tc.fixture)
			if !strings.Contains(text, "path: /mnt/taugrid-ray-tmp") {
				t.Fatalf("expected %s to keep the resource-disk ray-tmp contract", tc.fixture)
			}
		})
	}
}

// TestGPURayJobFixturesShipWorkerStdoutToKusto asserts the GPU worker pod
// template carries the adx-mon/log-destination annotation. When a Ray worker
// crashes, KubeRay deletes the pod and recreates it under a new name, so the
// crash output is otherwise lost from kubelet (`kubectl logs --previous` only
// covers a restart within a surviving pod). The annotation makes the node-local
// adx-mon collector ship the worker's stdout to the Logs/ContainerLogs Kusto
// table while the pod runs, so the failure reason survives post-mortem. The
// head now also uses the same annotation for the driver-log offload sidecar, so
// this test specifically asserts the worker copy remains in the worker section.
func TestGPURayJobFixturesShipWorkerStdoutToKusto(t *testing.T) {
	fixtures := []string{
		"tests/e2e/stack/fixtures/inference-rayjob-gpu.yaml",
		"tests/e2e/stack/fixtures/training-rayjob-gpu.yaml",
	}

	const annotation = `adx-mon/log-destination: "Logs:ContainerLogs"`

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			path, err := findRepoFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", fixture, err)
			}
			text := string(data)

			if got := strings.Count(text, annotation); got != 2 {
				t.Fatalf("expected %s to set %q twice (head driver offload + worker stdout shipping), got %d", fixture, annotation, got)
			}

			workerIdx := strings.Index(text, "workerGroupSpecs:")
			if workerIdx < 0 {
				t.Fatalf("%s: missing workerGroupSpecs", fixture)
			}
			if strings.Count(text[workerIdx:], annotation) != 1 {
				t.Fatalf("%s: expected exactly one worker-side %q after workerGroupSpecs", fixture, annotation)
			}
			if strings.LastIndex(text, annotation) < workerIdx {
				t.Fatalf("%s: adx-mon log-destination annotation must be on the worker pod template "+
					"(after workerGroupSpecs), not the RayJob/head/submitter", fixture)
			}
		})
	}
}

func TestLiveRayJobFixturesOffloadDriverLogsFromHead(t *testing.T) {
	const annotation = `adx-mon/log-destination: "Logs:ContainerLogs"`

	for _, tc := range rayDriverLogFixtureCases {
		t.Run(tc.fixture, func(t *testing.T) {
			text := readFixtureText(t, tc.fixture)

			if got := strings.Count(text, annotation); got != tc.totalLogAnnotations {
				t.Fatalf("expected %s to carry %q %d time(s), got %d", tc.fixture, annotation, tc.totalLogAnnotations, got)
			}

			headSection, workerSection, workerIdx := splitFixtureSections(t, tc.fixture, text)
			headIdx := strings.Index(headSection, annotation)
			if headIdx < 0 || workerIdx < 0 {
				t.Fatalf("%s: missing head annotation or workerGroupSpecs", tc.fixture)
			}
			if got := strings.Count(headSection, annotation); got != 1 {
				t.Fatalf("%s: expected exactly one head-side %q before workerGroupSpecs, got %d", tc.fixture, annotation, got)
			}
			if got := strings.Count(workerSection, annotation); got != tc.workerLogAnnotations {
				t.Fatalf("%s: expected %d worker-side %q after workerGroupSpecs, got %d", tc.fixture, tc.workerLogAnnotations, annotation, got)
			}
			if got := strings.Count(text, "name: ray-driver-log-offload"); got != 1 {
				t.Fatalf("%s: expected exactly one head-side ray-driver-log-offload sidecar, got %d", tc.fixture, got)
			}
			if strings.Contains(workerSection, "name: ray-driver-log-offload") {
				t.Fatalf("%s: worker section must not include the head-only ray-driver-log-offload sidecar", tc.fixture)
			}
			for _, want := range []string{
				"name: ray-driver-log-offload",
				`command: ["/bin/bash", "-lc", "--"]`,
				"job-driver-*.log",
				"readOnly: true",
				"allowPrivilegeEscalation: false",
				"readOnlyRootFilesystem: true",
				"runAsNonRoot: true",
				"runAsUser: 65532",
				"runAsGroup: 65532",
				"chmod 1777 /tmp/ray",
			} {
				if !strings.Contains(headSection, want) {
					t.Fatalf("expected %s head section to contain %q", tc.fixture, want)
				}
			}
		})
	}
}

func readFixtureText(t *testing.T, fixture string) string {
	t.Helper()
	path, err := findRepoFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", fixture, err)
	}
	return string(data)
}

func splitFixtureSections(t *testing.T, fixture, text string) (string, string, int) {
	t.Helper()
	workerIdx := strings.Index(text, "workerGroupSpecs:")
	if workerIdx < 0 {
		t.Fatalf("%s: missing workerGroupSpecs", fixture)
	}
	return text[:workerIdx], text[workerIdx:], workerIdx
}

func TestStackRayJobTestsWaitForSubmitterLogMarkers(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/integration_test.go")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stack integration test: %v", err)
	}
	text := string(data)

	if strings.Contains(text, "GetPodLogsByLabel(") {
		t.Fatal("stack RayJob tests should wait for submitter log markers instead of reading logs once; Ray worker stdout can lag RayJob SUCCEEDED")
	}
	// The FineWeb IB test uses the full-log variant (WaitForFullPodLogsByLabelContaining)
	// because its NET/IB init and late checkpoint/success sentinels span the whole run and
	// cannot coexist in a tailed window; the other five use the tailed variant. Both satisfy
	// "wait for submitter log markers", so count both toward the expected six.
	waitMarkers := strings.Count(text, "WaitForPodLogsByLabelContaining(") +
		strings.Count(text, "WaitForFullPodLogsByLabelContaining(")
	if waitMarkers != 6 {
		t.Fatalf("expected CPU inference/training, GPU inference/training, large GPU nanoGPT, and FineWeb IB tests to wait for submitter log markers, got %d", waitMarkers)
	}
}

func TestStackGPURayJobTestsAssertPlacement(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/integration_test.go")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stack integration test: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"GPU_NODE_SELECTOR_KEY",
		"GPU_NODE_SELECTOR_VALUE",
		"RAY_SUBMITTER_NODE_SELECTOR_KEY",
		"RAY_SUBMITTER_NODE_SELECTOR_VALUE",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected stack GPU tests to assert placement using %q", want)
		}
	}
	if got := strings.Count(text, "requirePodsOnSelectedNodes(t,"); got != 10 {
		t.Fatalf("expected GPU inference/training workers, submitters, large GPU nanoGPT, and FineWeb IB head/worker/submitter placement assertions, got %d", got)
	}
}

func TestTrainingJobRunsWorkOnGPUWorker(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/fixtures/training_job.py")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read training_job.py: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"@ray.remote(num_gpus=1)",
		"result = ray.get(train_once.remote())",
		"Cluster resources:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected training_job.py to contain %q", want)
		}
	}
}

func TestCPUTrainingJobAssertsDeterministicActorProgress(t *testing.T) {
	path, err := findRepoFile("tests/e2e/stack/fixtures/training_job_cpu.py")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read training_job_cpu.py: %v", err)
	}
	text := string(data)

	for _, want := range []string{
		"torch.manual_seed(0)",
		"INNER_STEPS",
		"mean_first_loss",
		"mean_last_loss",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected CPU training fixture to contain %q", want)
		}
	}
	if strings.Contains(text, "Final per-epoch mean losses") {
		t.Fatal("CPU training fixture should not compare losses across independent Ray Data actor pools")
	}
}
