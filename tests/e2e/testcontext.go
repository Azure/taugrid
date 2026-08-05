// Package e2e provides the test framework for taugrid e2e integration tests.
//
// Tests only run when AI_RUNTIME_E2E=1 is set. This prevents accidental execution
// against a production cluster when running `go test ./...` locally.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/tests/e2e/bundle"
	"github.com/Azure/taugrid/tests/e2e/internal/scriptpayload"
	"github.com/Azure/taugrid/tests/e2e/results"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestContext provides K8s clients and helpers for e2e tests.
// It embeds *testing.T so all test methods (Logf, Helper, Cleanup, Failed, …)
// are available directly on tc. It also stores a context.Context tied to the
// test's lifetime and an artifact bundle for diagnostic capture on failure.
type TestContext struct {
	*testing.T
	ctx           context.Context
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	bundle        *bundle.Writer
	outcomeName   string
}

// SkipUnlessE2E skips the test unless AI_RUNTIME_E2E=1 is set.
func SkipUnlessE2E(t testing.TB) {
	t.Helper()
	if os.Getenv("AI_RUNTIME_E2E") != "1" {
		t.Skip("Skipping e2e test: set AI_RUNTIME_E2E=1 to run")
	}
}

func managerWorkloadOnly() bool {
	return os.Getenv("AI_RUNTIME_E2E_MANAGER_WORKLOAD_ONLY") == "1" ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("LARGE_GPU_WORKLOAD_ACCESS_MODE")), "manager")
}

// BuildClients creates typed and dynamic K8s clients from the current kubeconfig.
// Use this in TestMain or other contexts where no *testing.T is available.
func BuildClients() (kubernetes.Interface, dynamic.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if ctx := strings.TrimSpace(os.Getenv("AI_RUNTIME_E2E_KUBE_CONTEXT")); ctx != "" {
		overrides.CurrentContext = ctx
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return kubeClient, dynClient, nil
}

// NewTestContext creates a TestContext from the current kubeconfig.
// It skips the test if AI_RUNTIME_E2E=1 is not set.
func NewTestContext(t *testing.T, ctx context.Context) *TestContext {
	t.Helper()

	// Initialise the result emitter (idempotent). Errors are non-fatal —
	// a broken emitter must never block the actual test.
	emitter, _ := results.Init()

	startTime := time.Now()
	suite := suiteFromCaller()
	tc := &TestContext{
		T:           t,
		ctx:         ctx,
		bundle:      bundle.New(t),
		outcomeName: t.Name(),
	}

	// Register outcome recording BEFORE SkipUnlessE2E — t.Skip calls
	// runtime.Goexit(), so anything after it won't execute, but t.Cleanup
	// callbacks registered before the skip still run.
	t.Cleanup(func() {
		if emitter == nil {
			return
		}
		status := results.StatusPass
		if t.Failed() {
			status = results.StatusFail
		} else if t.Skipped() {
			status = results.StatusSkip
		}
		o := results.Outcome{
			RunID:       results.RunID(),
			RunAttempt:  results.RunAttempt(),
			TestName:    tc.outcomeName,
			Suite:       suite,
			Status:      status,
			DurationSec: time.Since(startTime).Seconds(),
			Timestamp:   startTime.UTC(),
			Branch:      results.Branch(),
		}
		_ = emitter.Record(ctx, o)
	})

	SkipUnlessE2E(t)

	kubeClient, dynClient, err := BuildClients()
	if err != nil {
		t.Fatalf("Failed to build K8s clients: %v", err)
	}

	tc.kubeClient = kubeClient
	tc.dynamicClient = dynClient

	// Generic controller-log capture on failure — covers kueue + kuberay
	// without per-test boilerplate. Test-specific dumps (CR state, workload
	// lists) are registered via tc.OnFailure in each test.
	t.Cleanup(func() {
		if !t.Failed() || managerWorkloadOnly() {
			return
		}
		tc.DumpKueueAdmissionDiagnostics("kueue-system")
		tc.DumpControllerLogs("kuberay-system", "app.kubernetes.io/name=kuberay-operator", 100)
	})

	return tc
}

// RecordOutcomeAs overrides the default TestOutcomes name for this test.
// Most tests should keep the default t.Name(); matrix workflows can set a
// test-local name that includes the target SKU or topology.
func (tc *TestContext) RecordOutcomeAs(name string) {
	tc.Helper()
	if strings.TrimSpace(name) == "" {
		return
	}
	tc.outcomeName = name
}

// KubeClient returns the typed Kubernetes client for direct API calls from tests
// (e.g., fetching ConfigMap/DaemonSet object specs without polling helpers).
func (tc *TestContext) KubeClient() kubernetes.Interface {
	return tc.kubeClient
}

// DynamicClient returns the dynamic Kubernetes client for applying arbitrary CR fixtures.
func (tc *TestContext) DynamicClient() dynamic.Interface {
	return tc.dynamicClient
}

// Ctx returns the test-lifetime context passed to NewTestContext.
func (tc *TestContext) Ctx() context.Context {
	return tc.ctx
}

// Bundle returns the per-test artifact writer for custom diagnostic capture.
func (tc *TestContext) Bundle() *bundle.Writer {
	return tc.bundle
}

// OnFailure registers a cleanup function that runs only when the test has failed.
// Use this to capture test-specific diagnostics (CR state, workload lists) that
// complement the automatic pod/controller-log capture in WaitFor* methods.
func (tc *TestContext) OnFailure(fn func()) {
	tc.Helper()
	tc.Cleanup(func() {
		if !tc.Failed() {
			return
		}
		fn()
	})
}

// readFixture reads a file from the fixtures/ directory and expands template placeholders.
// Supported placeholders:
//   - {{RAY_IMAGE}}: value of the RAY_E2E_IMAGE env var (required when used)
//   - {{RAY_VERSION}}: RAY_E2E_VERSION, Ray version parsed from RAY_E2E_IMAGE,
//     or RAY_VERSION from images/ray/Makefile
//   - {{TORCH_SPEC}}: value of TORCH_SPEC env var (e.g. "torch==2.4.1"); used by
//     GPU fixtures to pin per-SKU driver-compatible wheels
//   - {{TORCH_INDEX_URL}}: value of TORCH_INDEX_URL env var (e.g.
//     "https://download.pytorch.org/whl/cu121")
//   - {{GPU_NODE_SELECTOR_KEY}} / {{GPU_NODE_SELECTOR_VALUE}}: values of
//     GPU_NODE_SELECTOR_KEY / GPU_NODE_SELECTOR_VALUE env vars; together they
//     pin the RayJob worker to a specific node (e.g. `agentpool: a10` for
//     managed pools, `kubernetes.io/hostname: flex-h100-...` for flex nodes)
//   - {{RAY_SUBMITTER_NODE_SELECTOR_KEY}} / {{RAY_SUBMITTER_NODE_SELECTOR_VALUE}}:
//     values of RAY_SUBMITTER_NODE_SELECTOR_KEY / RAY_SUBMITTER_NODE_SELECTOR_VALUE
//     env vars; defaults to `kubernetes.azure.com/mode: system` so the KubeRay
//     submitter Job stays off GPU nodes unless CI pins it to a dedicated CPU pool
//   - {{NANOGPT_*}}: large-GPU Ray Train workload settings. Dataset URI, SHA256,
//     and token-count placeholders are required when the large fixture is used;
//     worker count and 2,000-step training defaults are provided for the live
//     conformance path. The TAS annotation placeholder is rendered only for the
//     ArgoCD queue path because the local fixture flavor is not topology-aware.
//   - {{FINEWEB_*}}: 1.7B FineWeb FSDP/InfiniBand conformance workload settings.
//     Dataset URI/SHA256/token-count placeholders are required when the FineWeb
//     fixture is used; model-shape, bounded-step, checkpoint, and IB NCCL
//     placeholders default to the 1.716B / first-checkpoint conformance values.
//   - {{STACK_NAMESPACE}}, {{STACK_QUEUE}}, and {{STACK_LARGE_GPU_QUEUE}}:
//     Kueue namespace/LocalQueue routing. Tests default to the local stack fixture
//     queues unless E2E_STACK_USE_ARGOCD_QUEUE or explicit stack queue env vars opt
//     into a pre-provisioned queue.
//   - {{INFERENCE_SCRIPT_PAYLOAD_B64}} / {{INFERENCE_SCRIPT_PAYLOAD_DIGEST}},
//     {{FINEWEB_SCRIPT_PAYLOAD_B64}} / {{FINEWEB_SCRIPT_PAYLOAD_DIGEST}},
//     {{TRAINING_CPU_SCRIPT_PAYLOAD_B64}} / {{TRAINING_CPU_SCRIPT_PAYLOAD_DIGEST}},
//     {{TRAINING_GPU_SCRIPT_PAYLOAD_B64}} / {{TRAINING_GPU_SCRIPT_PAYLOAD_DIGEST}},
//     and {{NANOGPT_SCRIPT_PAYLOAD_B64}} / {{NANOGPT_SCRIPT_PAYLOAD_DIGEST}}:
//     computed (not env-sourced) from the actual fixtures/inference_job.py,
//     fixtures/fineweb_ray_train.py, fixtures/training_job_cpu.py,
//     fixtures/training_job.py, and fixtures/nanogpt_ray_train.py driver
//     scripts via the tests/e2e/internal/scriptpayload package (a test-only
//     mirror of PR1's cli/internal/payload wire format), so
//     every manager-routed RayJob fixture embeds a self-contained, head-only
//     payload instead of depending on a ConfigMap that MultiKueue does not
//     replicate to worker clusters.
func readFixture(name string) ([]byte, error) {
	path := filepath.Join("fixtures", name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading fixture %s: %w", name, err)
	}
	if bytes.Contains(data, []byte("{{RAY_VERSION}}")) {
		rayVer, err := rayVersion()
		if err != nil {
			return nil, fmt.Errorf("resolving RAY_VERSION for fixture %s: %w", name, err)
		}
		data = bytes.ReplaceAll(data, []byte("{{RAY_VERSION}}"), []byte(rayVer))
	}
	if bytes.Contains(data, []byte("{{RAY_IMAGE}}")) {
		img := os.Getenv("RAY_E2E_IMAGE")
		if img == "" {
			return nil, fmt.Errorf("fixture %s uses {{RAY_IMAGE}} but RAY_E2E_IMAGE env var is not set", name)
		}
		data = bytes.ReplaceAll(data, []byte("{{RAY_IMAGE}}"), []byte(img))
	}
	if bytes.Contains(data, []byte("{{NANOGPT_TAS_ANNOTATION}}")) {
		annotation := "# TAS omitted: the local stack queue flavor is not topology-aware"
		if liveConformanceUsesArgoCDQueue() {
			annotation = `kueue.x-k8s.io/podset-unconstrained-topology: "true"`
		}
		data = bytes.ReplaceAll(data, []byte("{{NANOGPT_TAS_ANNOTATION}}"), []byte(annotation))
	}
	for _, scriptSub := range []struct {
		b64Placeholder, digestPlaceholder, scriptFile string
	}{
		{"{{INFERENCE_SCRIPT_PAYLOAD_B64}}", "{{INFERENCE_SCRIPT_PAYLOAD_DIGEST}}", "inference_job.py"},
		{"{{FINEWEB_SCRIPT_PAYLOAD_B64}}", "{{FINEWEB_SCRIPT_PAYLOAD_DIGEST}}", "fineweb_ray_train.py"},
		{"{{TRAINING_CPU_SCRIPT_PAYLOAD_B64}}", "{{TRAINING_CPU_SCRIPT_PAYLOAD_DIGEST}}", "training_job_cpu.py"},
		{"{{TRAINING_GPU_SCRIPT_PAYLOAD_B64}}", "{{TRAINING_GPU_SCRIPT_PAYLOAD_DIGEST}}", "training_job.py"},
		{"{{NANOGPT_SCRIPT_PAYLOAD_B64}}", "{{NANOGPT_SCRIPT_PAYLOAD_DIGEST}}", "nanogpt_ray_train.py"},
	} {
		if !bytes.Contains(data, []byte(scriptSub.b64Placeholder)) && !bytes.Contains(data, []byte(scriptSub.digestPlaceholder)) {
			continue
		}
		b64, digest, err := scriptPayload(scriptSub.scriptFile)
		if err != nil {
			return nil, fmt.Errorf("computing embedded script payload for fixture %s: %w", name, err)
		}
		data = bytes.ReplaceAll(data, []byte(scriptSub.b64Placeholder), []byte(b64))
		data = bytes.ReplaceAll(data, []byte(scriptSub.digestPlaceholder), []byte(digest))
	}
	for _, sub := range []struct {
		placeholder, envVar, defaultValue string
	}{
		{"{{TORCH_SPEC}}", "TORCH_SPEC", ""},
		{"{{TORCH_INDEX_URL}}", "TORCH_INDEX_URL", ""},
		{"{{GPU_NODE_SELECTOR_KEY}}", "GPU_NODE_SELECTOR_KEY", ""},
		{"{{GPU_NODE_SELECTOR_VALUE}}", "GPU_NODE_SELECTOR_VALUE", ""},
		{"{{RAY_SUBMITTER_NODE_SELECTOR_KEY}}", "RAY_SUBMITTER_NODE_SELECTOR_KEY", "kubernetes.azure.com/mode"},
		{"{{RAY_SUBMITTER_NODE_SELECTOR_VALUE}}", "RAY_SUBMITTER_NODE_SELECTOR_VALUE", "system"},
		{"{{STACK_NAMESPACE}}", "E2E_STACK_NAMESPACE", defaultStackNamespace()},
		{"{{STACK_QUEUE}}", "E2E_STACK_QUEUE", defaultStackQueue()},
		{"{{STACK_LARGE_GPU_QUEUE}}", "E2E_STACK_LARGE_GPU_QUEUE", defaultStackLargeGPUQueue()},
		{"{{NANOGPT_DATASET_URIS}}", "NANOGPT_DATASET_URIS", ""},
		{"{{NANOGPT_DATASET_SHA256S}}", "NANOGPT_DATASET_SHA256S", ""},
		{"{{NANOGPT_DATASET_TOKEN_COUNTS}}", "NANOGPT_DATASET_TOKEN_COUNTS", ""},
		{"{{NANOGPT_MIN_TOTAL_TOKENS}}", "NANOGPT_MIN_TOTAL_TOKENS", "100000000"},
		{"{{NANOGPT_TRAIN_WORKERS}}", "NANOGPT_TRAIN_WORKERS", "16"},
		{"{{NANOGPT_TRAIN_STEPS}}", "NANOGPT_TRAIN_STEPS", "2000"},
		{"{{NANOGPT_BATCH_SIZE}}", "NANOGPT_BATCH_SIZE", "2"},
		{"{{NANOGPT_BLOCK_SIZE}}", "NANOGPT_BLOCK_SIZE", "256"},
		{"{{NANOGPT_VOCAB_SIZE}}", "NANOGPT_VOCAB_SIZE", "65536"},
		{"{{NANOGPT_REPORT_EVERY}}", "NANOGPT_REPORT_EVERY", "25"},
		{"{{NANOGPT_TORCH_SPEC}}", "NANOGPT_TORCH_SPEC", "torch==2.7.1"},
		{"{{NANOGPT_TORCH_INDEX_URL}}", "NANOGPT_TORCH_INDEX_URL", "https://download.pytorch.org/whl/cu128"},
		{"{{NANOGPT_NCCL_SOCKET_IFNAME}}", "NANOGPT_NCCL_SOCKET_IFNAME", "eth0"},
		{"{{NANOGPT_NCCL_IB_DISABLE}}", "NANOGPT_NCCL_IB_DISABLE", "1"},
		{"{{NANOGPT_NCCL_DEBUG}}", "NANOGPT_NCCL_DEBUG", "WARN"},
		{"{{FINEWEB_DATASET_URIS}}", "FINEWEB_DATASET_URIS", ""},
		{"{{FINEWEB_DATASET_SHA256S}}", "FINEWEB_DATASET_SHA256S", ""},
		{"{{FINEWEB_DATASET_TOKEN_COUNTS}}", "FINEWEB_DATASET_TOKEN_COUNTS", ""},
		{"{{FINEWEB_MIN_TOTAL_TOKENS}}", "FINEWEB_MIN_TOTAL_TOKENS", "10000000"},
		{"{{FINEWEB_TRAIN_WORKERS}}", "FINEWEB_TRAIN_WORKERS", "16"},
		{"{{FINEWEB_TRAIN_STEPS}}", "FINEWEB_TRAIN_STEPS", "60"},
		{"{{FINEWEB_CHECKPOINT_INTERVAL}}", "FINEWEB_CHECKPOINT_INTERVAL", "50"},
		{"{{FINEWEB_CHECKPOINT_DIR}}", "FINEWEB_CHECKPOINT_DIR", "/mnt/fineweb-ckpt"},
		{"{{FINEWEB_BATCH_SIZE}}", "FINEWEB_BATCH_SIZE", "1"},
		{"{{FINEWEB_BLOCK_SIZE}}", "FINEWEB_BLOCK_SIZE", "1024"},
		{"{{FINEWEB_VOCAB_SIZE}}", "FINEWEB_VOCAB_SIZE", "65536"},
		{"{{FINEWEB_N_LAYER}}", "FINEWEB_N_LAYER", "32"},
		{"{{FINEWEB_N_HEAD}}", "FINEWEB_N_HEAD", "16"},
		{"{{FINEWEB_N_EMBD}}", "FINEWEB_N_EMBD", "2048"},
		{"{{FINEWEB_REPORT_EVERY}}", "FINEWEB_REPORT_EVERY", "10"},
		{"{{FINEWEB_TORCH_SPEC}}", "FINEWEB_TORCH_SPEC", "torch==2.7.1"},
		{"{{FINEWEB_TORCH_INDEX_URL}}", "FINEWEB_TORCH_INDEX_URL", "https://download.pytorch.org/whl/cu128"},
		{"{{FINEWEB_NCCL_IB_HCA}}", "FINEWEB_NCCL_IB_HCA", "mlx5_ib0,mlx5_ib1,mlx5_ib2,mlx5_ib3,mlx5_ib4,mlx5_ib5,mlx5_ib6,mlx5_ib7"},
		{"{{FINEWEB_NCCL_SOCKET_IFNAME}}", "FINEWEB_NCCL_SOCKET_IFNAME", "eth0"},
		{"{{FINEWEB_NCCL_DEBUG}}", "FINEWEB_NCCL_DEBUG", "INFO"},
	} {
		if bytes.Contains(data, []byte(sub.placeholder)) {
			v := os.Getenv(sub.envVar)
			if v == "" {
				if sub.defaultValue == "" {
					return nil, fmt.Errorf("fixture %s uses %s but %s env var is not set", name, sub.placeholder, sub.envVar)
				}
				v = sub.defaultValue
			}
			data = bytes.ReplaceAll(data, []byte(sub.placeholder), []byte(v))
		}
	}
	return data, nil
}

// ReadFixtureWithSubstitutions reads a YAML fixture and performs the exact
// same placeholder substitution readFixture applies at apply/delete time
// (including computing the embedded script payload placeholders), without
// applying anything to a cluster. It exists so tests outside this package
// (e.g. the stack package's payload fixture shape/integrity tests) can
// assert against the fully-substituted fixture bytes using the same
// substitution path exercised by ApplyFixtureWithClient.
func ReadFixtureWithSubstitutions(name string) ([]byte, error) {
	return readFixture(name)
}

// scriptPayload reads scriptFile from the fixtures directory and encodes it
// as a single-file scriptpayload envelope, returning the base64-encoded
// envelope and its hex SHA-256 digest for embedding into a fixture's
// tau-payload initContainer env vars and payload-digest annotation.
func scriptPayload(scriptFile string) (encoded string, digest string, err error) {
	scriptPath := filepath.Join("fixtures", scriptFile)
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		return "", "", fmt.Errorf("reading script %s: %w", scriptPath, err)
	}
	return scriptpayload.Encode(map[string][]byte{scriptFile: scriptBytes})
}

func defaultStackNamespace() string {
	if liveConformanceUsesArgoCDQueue() {
		return "taugrid-e2e"
	}
	return "e2e-stack"
}

func defaultStackQueue() string {
	if liveConformanceUsesArgoCDQueue() {
		return "jobqueue"
	}
	return "e2e-stack-queue"
}

func defaultStackLargeGPUQueue() string {
	if liveConformanceUsesArgoCDQueue() {
		return "jobqueue"
	}
	return "e2e-stack-large-gpu-queue"
}

func liveConformanceUsesArgoCDQueue() bool {
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

// rayVersion resolves the KubeRay rayVersion that must match the test image.
func rayVersion() (string, error) {
	if rayVer := os.Getenv("RAY_E2E_VERSION"); rayVer != "" {
		return rayVer, nil
	}
	if rayVer := rayVersionFromImage(os.Getenv("RAY_E2E_IMAGE")); rayVer != "" {
		return rayVer, nil
	}
	makefilePath, err := findRepoFile("images/ray/Makefile")
	if err != nil {
		return "", err
	}
	vars, err := parseMakefileVars(makefilePath, "RAY_VERSION")
	if err != nil {
		return "", err
	}
	rayVer := vars["RAY_VERSION"]
	if rayVer == "" {
		return "", fmt.Errorf("RAY_VERSION not found in %s", makefilePath)
	}
	return rayVer, nil
}

func rayVersionFromImage(image string) string {
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash || lastColon == len(image)-1 {
		return ""
	}
	tag := image[lastColon+1:]
	start := strings.Index(tag, "ray")
	if start < 0 {
		return ""
	}
	version := tag[start+len("ray"):]
	if version == "" || version[0] < '0' || version[0] > '9' {
		return ""
	}
	if end := strings.Index(version, "-"); end >= 0 {
		version = version[:end]
	}
	return version
}

// findRepoFile walks up from the current directory looking for a file relative to the repo root.
// It detects the root by the presence of a go.work or .git directory.
func findRepoFile(relPath string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		// Check for repo root markers.
		for _, marker := range []string{".git", "go.work"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				candidate := filepath.Join(dir, relPath)
				if _, err := os.Stat(candidate); err == nil {
					return candidate, nil
				}
				return "", fmt.Errorf("%s not found at repo root %s", relPath, dir)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repo root from %s", dir)
		}
		dir = parent
	}
}

// parseMakefileVars extracts simple "VAR ?= value" or "VAR := value" assignments from a Makefile.
func parseMakefileVars(path string, keys ...string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}

	result := make(map[string]string, len(keys))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		for _, sep := range []string{"?=", ":="} {
			if idx := strings.Index(line, sep); idx > 0 {
				key := strings.TrimSpace(line[:idx])
				if want[key] {
					result[key] = strings.TrimSpace(line[idx+len(sep):])
				}
			}
		}
	}
	return result, scanner.Err()
}

// ApplyFixtureWithClient reads a YAML fixture and applies it using a dynamic client directly.
// Use this in TestMain or setup/teardown where no TestContext is available.
func ApplyFixtureWithClient(ctx context.Context, dynClient dynamic.Interface, name string) error {
	yamlBytes, err := readFixture(name)
	if err != nil {
		return err
	}
	return applyYAMLWithClient(ctx, dynClient, yamlBytes)
}

// DeleteFixtureWithClient reads a YAML fixture and deletes the resources using a dynamic client directly.
// Use this in TestMain or setup/teardown where no TestContext is available.
func DeleteFixtureWithClient(ctx context.Context, dynClient dynamic.Interface, name string) error {
	yamlBytes, err := readFixture(name)
	if err != nil {
		return err
	}
	return deleteYAMLWithClient(ctx, dynClient, yamlBytes)
}

// ApplyFixture reads a YAML fixture, applies it to the cluster, and registers
// cleanup to delete the resources when the test finishes.
func (tc *TestContext) ApplyFixture(t *testing.T, name string) {
	t.Helper()
	if err := ApplyFixtureWithClient(tc.ctx, tc.dynamicClient, name); err != nil {
		t.Fatalf("applying fixture %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = DeleteFixtureWithClient(tc.ctx, tc.dynamicClient, name)
	})
}

// suiteFromCaller derives the suite name from the calling test's directory.
// NewTestContext is called from tests/e2e/kueue/kueue_test.go → suite = "kueue".
//
// WARNING: This assumes a fixed call depth (2 frames up = the test function).
// If NewTestContext is ever wrapped by an intermediate helper, the suite
// derivation will silently return the wrong directory. If that happens,
// switch to walking the stack for the first _test.go frame.
func suiteFromCaller() string {
	// Skip: 0=suiteFromCaller, 1=NewTestContext, 2=the actual test function.
	_, file, _, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	return filepath.Base(filepath.Dir(file))
}
