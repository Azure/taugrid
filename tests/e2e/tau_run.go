// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TauRunEnabled() bool {
	return os.Getenv("E2E_TAU_RUN") == "1"
}

func TauRunName(name string) string {
	if id := os.Getenv("E2E_TAU_RUN_ID"); id != "" {
		return name + "-" + id
	}
	return name
}

func NightlyTauPolicy(namespace, queue, profile string) map[string]any {
	return map[string]any{
		"namespace":               namespace,
		"queue":                   queue,
		"profile":                 profile,
		"team":                    "research",
		"lane":                    "training",
		"workload_priority_class": "aks-low-priority",
		"pod_priority_class":      "tau-nightly-low",
		"pod_preemption_policy":   "Never",
	}
}

func SubmitTauRun(t *testing.T, ctx context.Context, config map[string]any, files map[string][]byte) error {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv("TAU_BINARY"))
	if binary == "" {
		return fmt.Errorf("E2E_TAU_RUN requires TAU_BINARY; direct Kubernetes submission is not a fallback")
	}
	directory, path, err := prepareTauRun(t, config, files)
	if err != nil {
		return err
	}
	name := config["name"].(string)
	submission, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args := []string{"run", "--config", path}
	if kubeContext := os.Getenv("AI_RUNTIME_E2E_KUBE_CONTEXT"); kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	if workspace := os.Getenv("E2E_TAU_WORKSPACE"); workspace != "" {
		args = append(args, "--workspace", workspace)
	}
	if systemNamespace := os.Getenv("E2E_TAU_SYSTEM_NAMESPACE"); systemNamespace != "" {
		args = append(args, "--system-namespace", systemNamespace)
	}
	command := exec.CommandContext(submission, binary, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if writeErr := os.WriteFile(filepath.Join(directory, "submission.log"), output, 0600); writeErr != nil {
		return writeErr
	}
	t.Logf("tau run %s:\n%s", name, output)
	if err != nil {
		return fmt.Errorf("tau run %s: %w\n%s", name, err, output)
	}
	return nil
}

func CancelTauRun(ctx context.Context, namespace, name string) error {
	binary := strings.TrimSpace(os.Getenv("TAU_BINARY"))
	if binary == "" {
		return fmt.Errorf("Tau nightly cleanup requires TAU_BINARY")
	}
	cleanup, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := []string{"run", "cancel", name, "--namespace", namespace, "--teardown-timeout", "15m"}
	if kubeContext := os.Getenv("AI_RUNTIME_E2E_KUBE_CONTEXT"); kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	if workspace := os.Getenv("E2E_TAU_WORKSPACE"); workspace != "" {
		args = append(args, "--workspace", workspace)
	}
	if systemNamespace := os.Getenv("E2E_TAU_SYSTEM_NAMESPACE"); systemNamespace != "" {
		args = append(args, "--system-namespace", systemNamespace)
	}
	output, err := exec.CommandContext(cleanup, binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tau run cancel %s/%s: %w\n%s", namespace, name, err, output)
	}
	return nil
}

func prepareTauRun(t *testing.T, config map[string]any, files map[string][]byte) (string, string, error) {
	t.Helper()
	name, ok := config["name"].(string)
	if !ok || name == "" {
		return "", "", fmt.Errorf("Tau nightly config requires a name")
	}
	directory := t.TempDir()
	if artifacts := os.Getenv("E2E_TAU_CONFIG_DIR"); artifacts != "" {
		var err error
		directory, err = os.MkdirTemp(artifacts, name+"-")
		if err != nil {
			return "", "", fmt.Errorf("create Tau run evidence directory: %w", err)
		}
	}
	for name, data := range files {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return "", "", fmt.Errorf("invalid Tau source file %q", name)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			return "", "", err
		}
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(directory, "tau.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", "", err
	}
	cfg, err := runconfig.Load(path)
	if err != nil {
		return "", "", fmt.Errorf("nightly Tau config: %w", err)
	}
	engine := cfg.Run.Engine
	if engine == "" {
		engine = cfg.Engine
	}
	if err := cfg.ValidateExecution(engine); err != nil {
		return "", "", fmt.Errorf("nightly Tau execution: %w", err)
	}
	return directory, path, nil
}

func BuildTauForTest(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("TAU_BINARY"); binary != "" {
		return binary
	}
	module, err := findRepoFile("cli/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "tau")
	command := exec.Command("go", "build", "-o", binary, "./cmd/tau")
	command.Dir = filepath.Dir(module)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Tau for native rendering: %v\n%s", err, output)
	}
	return binary
}

func RenderTauRunOffline(t *testing.T, binary string, config map[string]any, files map[string][]byte, platform profile.WorkloadProfile) *unstructured.Unstructured {
	t.Helper()
	resolved := profile.ResolvedWorkloadProfile{
		WorkloadProfile: platform,
		ClusterQueues:   []string{"nightly-cq"},
		Conditions:      []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}},
	}
	snapshot, err := profile.NewProfileSetSnapshot(1, []profile.ResolvedWorkloadProfile{resolved})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	config["policy"].(map[string]any)["workload_profile_snapshot"] = path
	directory, configPath, err := prepareTauRun(t, config, files)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "run", "--config", configPath, "--dry-run=client")
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		if failure, ok := err.(*exec.ExitError); ok {
			t.Fatalf("native Tau rendering: %v\n%s", err, failure.Stderr)
		}
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "rendered.yaml"), output, 0600); err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	json, err := yaml.YAMLToJSON(output)
	if err != nil {
		t.Fatalf("decode Tau-rendered YAML: %v\n%s", err, output)
	}
	if err := obj.UnmarshalJSON(json); err != nil {
		t.Fatalf("decode Tau-rendered workload: %v\n%s", err, output)
	}
	return obj
}
