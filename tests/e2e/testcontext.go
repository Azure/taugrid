// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

type TestContext struct {
	*testing.T
	ctx           context.Context
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
	bundle        *bundle.Writer
	outcomeName   string
}

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

func NewTestContext(t *testing.T, ctx context.Context) *TestContext {
	t.Helper()

	emitter, _ := results.Init()

	startTime := time.Now()
	suite := suiteFromCaller()
	tc := &TestContext{
		T:           t,
		ctx:         ctx,
		bundle:      bundle.New(t),
		outcomeName: t.Name(),
	}

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

	t.Cleanup(func() {
		if !t.Failed() || managerWorkloadOnly() {
			return
		}
		tc.DumpKueueAdmissionDiagnostics("kueue-system")
		tc.DumpControllerLogs("kuberay-system", "app.kubernetes.io/name=kuberay-operator", 100)
	})

	return tc
}

func (tc *TestContext) RecordOutcomeAs(name string) {
	tc.Helper()
	if strings.TrimSpace(name) == "" {
		return
	}
	tc.outcomeName = name
}

func (tc *TestContext) KubeClient() kubernetes.Interface {
	return tc.kubeClient
}

func (tc *TestContext) DynamicClient() dynamic.Interface {
	return tc.dynamicClient
}

func (tc *TestContext) Ctx() context.Context {
	return tc.ctx
}

func (tc *TestContext) Bundle() *bundle.Writer {
	return tc.bundle
}

func (tc *TestContext) OnFailure(fn func()) {
	tc.Helper()
	tc.Cleanup(func() {
		if !tc.Failed() {
			return
		}
		fn()
	})
}

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
	if bytes.Contains(data, []byte("{{FINEWEB_TAS_ANNOTATIONS}}")) {
		mode := strings.ToLower(strings.TrimSpace(os.Getenv("FINEWEB_TAS_MODE")))
		if mode == "" {
			mode = "required"
		}
		var annotation string
		switch mode {
		case "required":
			annotation = "annotations:\n            kueue.x-k8s.io/podset-required-topology: tau.azure.com/network-domain"
		case "site":
			annotation = "annotations:\n            kueue.x-k8s.io/podset-required-topology: tau.azure.com/site"
		case "hostname":
			annotation = "annotations:\n            kueue.x-k8s.io/podset-required-topology: kubernetes.io/hostname"
		case "disabled":
			annotation = "# TAS omitted: hostname spread enforces the 8+8 worker placement"
		default:
			return nil, fmt.Errorf("FINEWEB_TAS_MODE must be required, site, hostname, or disabled, got %q", mode)
		}
		data = bytes.ReplaceAll(data, []byte("{{FINEWEB_TAS_ANNOTATIONS}}"), []byte(annotation))
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
		{"{{FINEWEB_DATA_MODE}}", "FINEWEB_DATA_MODE", "dataset"},
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
					if strings.HasPrefix(sub.placeholder, "{{FINEWEB_DATASET_") &&
						strings.EqualFold(strings.TrimSpace(os.Getenv("FINEWEB_DATA_MODE")), "synthetic") {
						data = bytes.ReplaceAll(data, []byte(sub.placeholder), nil)
						continue
					}
					return nil, fmt.Errorf("fixture %s uses %s but %s env var is not set", name, sub.placeholder, sub.envVar)
				}
				v = sub.defaultValue
			}
			data = bytes.ReplaceAll(data, []byte(sub.placeholder), []byte(v))
		}
	}
	return data, nil
}

func ReadFixtureWithSubstitutions(name string) ([]byte, error) {
	return readFixture(name)
}

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

func findRepoFile(relPath string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
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

func ApplyFixtureWithClient(ctx context.Context, dynClient dynamic.Interface, name string) error {
	yamlBytes, err := readFixture(name)
	if err != nil {
		return err
	}
	return applyYAMLWithClient(ctx, dynClient, yamlBytes)
}

func DeleteFixtureWithClient(ctx context.Context, dynClient dynamic.Interface, name string) error {
	yamlBytes, err := readFixture(name)
	if err != nil {
		return err
	}
	return deleteYAMLWithClient(ctx, dynClient, yamlBytes)
}

func DeleteFixtureWithClientAndWait(ctx context.Context, dynClient dynamic.Interface, name string, timeout time.Duration) error {
	yamlBytes, err := readFixture(name)
	if err != nil {
		return err
	}
	return deleteYAMLWithClientAndWait(ctx, dynClient, yamlBytes, timeout)
}

func (tc *TestContext) ApplyFixture(t *testing.T, name string) {
	t.Helper()
	if err := ApplyFixtureWithClient(tc.ctx, tc.dynamicClient, name); err != nil {
		t.Fatalf("applying fixture %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = DeleteFixtureWithClient(tc.ctx, tc.dynamicClient, name)
	})
}

func suiteFromCaller() string {
	_, file, _, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	return filepath.Base(filepath.Dir(file))
}
