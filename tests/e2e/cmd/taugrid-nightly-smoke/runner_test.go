// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestRunnerExecutesTauLifecycleAndWritesPassedResult(t *testing.T) {
	opts := testOptions(t)
	commands := &fakeCommands{statusStates: []string{"running", "succeeded"}, logMarker: true}
	cluster := &fakeCluster{}
	runner := newRunner(opts, commands, cluster)
	runner.stderr = io.Discard

	if err := runner.execute(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := readResult(t, filepath.Join(opts.artifactDir, "tau-native-smoke-result.json"))
	if got.Status != "passed" || got.LifecycleState != "succeeded" {
		t.Fatalf("result = %+v, want passed/succeeded", got)
	}
	if got.ConfigSHA256 == "" {
		t.Fatal("config hash is empty")
	}
	if got.Topology == nil || got.Topology.Level != "kubernetes.io/hostname" ||
		got.Topology.Flavor != "taugrid-default-cpu" ||
		got.Topology.ClusterQueue != "jobqueue" || got.Topology.PodSet != "test-w" {
		t.Fatalf("topology result = %+v, want hostname/default CPU flavor", got.Topology)
	}
	if !cluster.profileEnsured || !cluster.profileReady || !cluster.workspaceReady ||
		!cluster.topologyVerified || !cluster.rayJobDeleted {
		t.Fatalf("cluster calls = %+v, want every lifecycle operation", cluster)
	}

	configPath := filepath.Join(opts.artifactDir, "workload", "tau.yaml")
	for _, expected := range [][]string{
		{
			"workspace", "create", opts.workspace,
			"--principal-name", opts.principalName,
			"--context", opts.contextName,
			"--system-namespace", opts.systemNamespace,
			"--apply",
		},
		{"run", "validate", "--config", configPath},
		{
			"run", "--config", configPath,
			"--context", opts.contextName,
			"--workspace", opts.workspace,
			"--dry-run=client",
		},
		{
			"run", "--config", configPath,
			"--context", opts.contextName,
			"--workspace", opts.workspace,
			"--dry-run=server",
		},
		{
			"run", "--config", configPath,
			"--context", opts.contextName,
			"--workspace", opts.workspace,
		},
		{
			"run", "status", opts.runName,
			"--context", opts.contextName,
			"--workspace", opts.workspace,
			"--output", "json",
		},
		{
			"logs", opts.runName,
			"--context", opts.contextName,
			"-n", opts.workspace,
			"-f",
		},
	} {
		if !commands.hasExact(expected) {
			t.Fatalf("exact Tau invocation %q was not observed: %v", expected, commands.calls)
		}
	}
	logs, err := os.ReadFile(filepath.Join(opts.artifactDir, "logs.txt"))
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if !strings.Contains(string(logs), completionMarker) {
		t.Fatalf("logs %q do not contain completion marker", logs)
	}
}

func TestRunnerWritesFailedResultForTerminalTauState(t *testing.T) {
	opts := testOptions(t)
	commands := &fakeCommands{statusStates: []string{"failed"}}
	cluster := &fakeCluster{}
	runner := newRunner(opts, commands, cluster)
	runner.stderr = io.Discard

	err := runner.execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "terminal state failed") {
		t.Fatalf("execute error = %v, want failed terminal state", err)
	}
	got := readResult(t, filepath.Join(opts.artifactDir, "tau-native-smoke-result.json"))
	if got.Status != "failed" || got.LifecycleState != "failed" {
		t.Fatalf("result = %+v, want failed/failed", got)
	}
	if cluster.rayJobDeleted {
		t.Fatal("failed run must be retained for diagnostics")
	}
}

func TestRunnerFailsWhenCompletionMarkerIsMissing(t *testing.T) {
	opts := testOptions(t)
	commands := &fakeCommands{statusStates: []string{"succeeded"}}
	runner := newRunner(opts, commands, &fakeCluster{})
	runner.stderr = io.Discard

	err := runner.execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not contain") {
		t.Fatalf("execute error = %v, want missing completion marker", err)
	}
	got := readResult(t, filepath.Join(opts.artifactDir, "tau-native-smoke-result.json"))
	if got.Status != "failed" || got.LifecycleState != "succeeded" {
		t.Fatalf("result = %+v, want failed result with succeeded lifecycle", got)
	}
}

func TestOptionsRejectInvalidRunName(t *testing.T) {
	opts := testOptions(t)
	opts.runName = "INVALID_NAME"
	if err := opts.validate(); err == nil {
		t.Fatal("validate accepted an invalid DNS run name")
	}
}

func TestCloseFileAddsCloseFailureToExistingError(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "closed.txt"))
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	original := errors.New("command failed")

	closeFile(file, &original, "closing test artifact")

	if !strings.Contains(original.Error(), "command failed") ||
		!strings.Contains(original.Error(), "closing test artifact") {
		t.Fatalf("joined error = %q, want original and close errors", original)
	}
}

func TestEnsureProfileAppendsWithoutReplacingExistingProfiles(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tau.azure.com/v1alpha1",
		"kind":       "TauCluster",
		"metadata": map[string]any{
			"name": "cluster",
		},
		"spec": map[string]any{
			"workloadProfiles": []any{
				map[string]any{"name": "existing"},
			},
		},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := client.Resource(tauClusterGVR).Create(
		context.Background(), cluster, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	kube := newKubernetesClient(client, time.Millisecond)

	if err := kube.ensureProfile(
		context.Background(), "unbounded.cpu.topology-smoke", "taugrid-default",
	); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	got, err := client.Resource(tauClusterGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	profiles, found, err := unstructured.NestedSlice(got.Object, "spec", "workloadProfiles")
	if err != nil || !found {
		t.Fatalf("profiles found=%t err=%v", found, err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profile count = %d, want 2", len(profiles))
	}
	if profiles[0].(map[string]any)["name"] != "existing" {
		t.Fatalf("existing profile was replaced: %v", profiles)
	}
	if profiles[1].(map[string]any)["name"] != "unbounded.cpu.topology-smoke" {
		t.Fatalf("new profile was not appended: %v", profiles)
	}
}

func TestEnsureProfileDoesNotDuplicateExistingProfile(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tau.azure.com/v1alpha1",
		"kind":       "TauCluster",
		"metadata": map[string]any{
			"name": "cluster",
		},
		"spec": map[string]any{
			"workloadProfiles": []any{
				map[string]any{
					"name":          "unbounded.cpu.topology-smoke",
					"placement":     "same-host",
					"workerCount":   int64(2),
					"gpusPerWorker": int64(0),
				},
			},
		},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, err := client.Resource(tauClusterGVR).Create(
		context.Background(), cluster, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	kube := newKubernetesClient(client, time.Millisecond)

	if err := kube.ensureProfile(
		context.Background(), "unbounded.cpu.topology-smoke", "taugrid-default",
	); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	got, err := client.Resource(tauClusterGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cluster: %v", err)
	}
	profiles, _, err := unstructured.NestedSlice(got.Object, "spec", "workloadProfiles")
	if err != nil {
		t.Fatalf("read profiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("profile count = %d, want 1", len(profiles))
	}
}

// Regression: RayJob worker PodSets use generated *-w names and Kueue v1beta2
// compresses topology assignments into slices. The verifier must match the
// exact RayJob group and parse that schema instead of timing out after admission.
func TestTopologyEvidenceFromWorkloadRequiresIntendedAdmission(t *testing.T) {
	workload := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "rayjob-test-123"},
		"status": map[string]any{
			"admission": map[string]any{
				"clusterQueue": "jobqueue",
				"podSetAssignments": []any{
					map[string]any{
						"name":  "test-w",
						"count": int64(2),
						"flavors": map[string]any{
							"cpu":    "taugrid-default-cpu",
							"memory": "taugrid-default-cpu",
						},
						"topologyAssignment": map[string]any{
							"levels": []any{"kubernetes.io/hostname"},
							"slices": []any{
								map[string]any{
									"domainCount": int64(1),
									"podCounts": map[string]any{
										"universal": int64(2),
									},
									"valuesPerLevel": []any{
										map[string]any{"universal": "node-1"},
									},
								},
							},
						},
					},
				},
			},
			"conditions": []any{
				map[string]any{"type": "Admitted", "status": "True"},
			},
		},
	}}

	got, found, err := topologyEvidenceFromWorkload(
		workload, "test-w", "taugrid-default-cpu", "jobqueue", 2,
	)
	if err != nil {
		t.Fatalf("topology evidence: %v", err)
	}
	if !found {
		t.Fatal("worker topology assignment was not found")
	}
	if got.Level != "kubernetes.io/hostname" || got.Flavor != "taugrid-default-cpu" ||
		got.ClusterQueue != "jobqueue" || got.Workload != "rayjob-test-123" ||
		got.PodSet != "test-w" ||
		got.AssignedWorkers != 2 || len(got.Domains) != 1 ||
		got.Domains[0].Values[0] != "node-1" {
		t.Fatalf("topology evidence = %+v", got)
	}
}

func TestTopologyEvidenceRejectsUnexpectedFlavor(t *testing.T) {
	workload := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"admission": map[string]any{
				"clusterQueue": "jobqueue",
				"podSetAssignments": []any{
					map[string]any{
						"name":  "test-w",
						"count": int64(2),
						"flavors": map[string]any{
							"cpu":    "wrong",
							"memory": "taugrid-default-cpu",
						},
					},
				},
			},
			"conditions": []any{
				map[string]any{"type": "Admitted", "status": "True"},
			},
		},
	}}

	_, _, err := topologyEvidenceFromWorkload(
		workload, "test-w", "taugrid-default-cpu", "jobqueue", 2,
	)
	if err == nil || !strings.Contains(err.Error(), `worker cpu flavor is "wrong"`) {
		t.Fatalf("error = %v, want unexpected CPU flavor", err)
	}
}

func testOptions(t *testing.T) options {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "tau.yaml"), []byte("name: __RUN_NAME__\n"), 0o644); err != nil {
		t.Fatalf("write tau config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "smoke.py"), []byte("print('smoke')\n"), 0o644); err != nil {
		t.Fatalf("write smoke.py: %v", err)
	}
	return options{
		tauBinary:        "/test/tau",
		contextName:      "test",
		systemNamespace:  "tau-system",
		workspace:        "taugrid-default",
		profile:          "unbounded.cpu.topology-smoke",
		cpuFlavor:        "taugrid-default-cpu",
		queue:            "jobqueue",
		principalName:    "nightly-test-researcher",
		sourceDir:        source,
		artifactDir:      filepath.Join(t.TempDir(), "artifacts"),
		runName:          "taugrid-nightly-42-1",
		profileTimeout:   time.Second,
		workspaceTimeout: time.Second,
		topologyTimeout:  time.Second,
		lifecycleTimeout: time.Second,
		logsTimeout:      time.Second,
		cleanupTimeout:   time.Second,
		pollInterval:     time.Millisecond,
	}
}

func readResult(t *testing.T, path string) result {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return got
}

type fakeCommands struct {
	mu           sync.Mutex
	calls        [][]string
	statusStates []string
	logMarker    bool
}

func (f *fakeCommands) run(
	ctx context.Context, stdout, _ io.Writer, _ string, args ...string,
) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()

	if len(args) > 0 && args[0] == "logs" {
		if f.logMarker {
			_, _ = io.WriteString(stdout, completionMarker+"\n")
			return nil
		}
		return nil
	}
	if len(args) >= 2 && args[0] == "run" && args[1] == "status" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.statusStates) == 0 {
			return errors.New("no fake status state configured")
		}
		state := f.statusStates[0]
		if len(f.statusStates) > 1 {
			f.statusStates = f.statusStates[1:]
		}
		_, _ = io.WriteString(stdout, `{"status":{"state":"`+state+`"}}`)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (f *fakeCommands) hasExact(expected []string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if len(call) != len(expected) {
			continue
		}
		match := true
		for i := range call {
			if call[i] != expected[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

type fakeCluster struct {
	profileEnsured   bool
	profileReady     bool
	workspaceReady   bool
	topologyVerified bool
	rayJobDeleted    bool
}

func (f *fakeCluster) ensureProfile(context.Context, string, string) error {
	f.profileEnsured = true
	return nil
}

func (f *fakeCluster) waitProfileReady(context.Context, string, time.Duration) error {
	f.profileReady = true
	return nil
}

func (f *fakeCluster) waitWorkspaceReady(context.Context, string, string, time.Duration) error {
	f.workspaceReady = true
	return nil
}

func (f *fakeCluster) waitTopologyVerified(
	context.Context, string, string, string, string, int64, time.Duration,
) (topologyEvidence, error) {
	f.topologyVerified = true
	return topologyEvidence{
		Level:           "kubernetes.io/hostname",
		Flavor:          "taugrid-default-cpu",
		ClusterQueue:    "jobqueue",
		Workload:        "rayjob-test",
		PodSet:          "test-w",
		AssignedWorkers: 2,
		Domains:         []topologyDomain{{Values: []string{"node-1"}, Count: 2}},
		PodNodes:        []string{"node-1", "node-1"},
	}, nil
}

func (f *fakeCluster) deleteRayJob(context.Context, string, string, time.Duration) error {
	f.rayJobDeleted = true
	return nil
}
