// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metricsoffload

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testRuntime(dir string) Runtime {
	return Runtime{
		Image:          "example.test/tau:v1",
		RunID:          "ray-run",
		Project:        "pretraining",
		Experiment:     "modernbert-fineweb",
		Group:          "fwe100",
		Tags:           map[string]string{"tau_workspace": "research-workspace"},
		Source:         "stellar-online",
		Store:          filepath.Join(dir, "store"),
		Out:            filepath.Join(dir, "out"),
		History:        []string{filepath.Join(dir, "metrics-*.jsonl")},
		CompletionFile: filepath.Join(dir, "completion.json"),
		ADXClusterURI:  "https://example.kusto.windows.net",
		ADXDatabase:    "TauGrid",
		ADXClientID:    "00000000-0000-0000-0000-000000000001",
		Interval:       time.Second,
		ReadyFile:      filepath.Join(dir, "ready"),
		ReadyTimeout:   time.Second,
		DoneFile:       filepath.Join(dir, "done"),
		DoneTimeout:    50 * time.Millisecond,
	}
}

func TestWrapCommandSurfacesMissingTerminalPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test executes a Bash wrapper")
	}
	runtime := testRuntime(t.TempDir())
	if err := os.WriteFile(runtime.ReadyFile, []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrapped, err := WrapCommand([]string{"bash", "-c", "exit 0"}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	err = exec.Command(wrapped[0], wrapped[1:]...).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 125 {
		t.Fatalf("wrapped command error = %v, want exit 125 when sidecar does not acknowledge publication", err)
	}
	if _, err := os.Stat(runtime.CompletionFile); err != nil {
		t.Fatalf("workload completion was not published before the sidecar timeout: %v", err)
	}
}

func TestBuildContainerCarriesHardenedRuntimeContract(t *testing.T) {
	runtime := testRuntime("/data/run")
	runtime.BaselineExistingHistory = true
	container := BuildContainer(runtime, []Mount{{Name: "data", Path: "/data"}})
	rendered := strings.ReplaceAll(strings.TrimSpace(toText(container)), "\n", " ")
	for _, want := range []string{
		"baseline-existing-history",
		"--experiment modernbert-fineweb",
		"ready-file",
		"done-file",
		"tau_workspace=research-workspace",
		"/var/run/tau",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("container missing %q: %s", want, rendered)
		}
	}
}

func TestBuildContainerRuntimeCommands(t *testing.T) {
	runtime := testRuntime("/data/run")
	collector := BuildContainer(runtime, nil)
	if got := collector["command"]; !reflect.DeepEqual(got, []any{CollectorSidecarCommand}) {
		t.Fatalf("collector command = %#v", got)
	}
	args := collector["args"].([]any)
	if len(args) < 2 || args[0] != "collect" || args[1] != "--watch" {
		t.Fatalf("collector args = %#v", args)
	}
}

func TestBuildContainerCarriesTypedADXContract(t *testing.T) {
	runtime := testRuntime("/data/run")
	runtime.Runtime = RuntimeCollectorV1
	runtime.DeliveryMode = DeliveryADXRequired
	runtime.ADXClusterURI = "https://example.kusto.windows.net"
	runtime.ADXDatabase = "TauGrid"
	runtime.ADXClientID = "00000000-0000-0000-0000-000000000001"
	runtime.ADXMaxAttempts = 4
	runtime.ADXRetryBackoff = 2 * time.Second
	runtime.ADXFinalStatusTimeout = 5 * time.Minute

	rendered := toText(BuildContainer(runtime, nil))
	for _, want := range []string{
		"--delivery-mode adx-required",
		"--adx-cluster-uri https://example.kusto.windows.net",
		"--adx-database TauGrid",
		"--adx-table TauExpMetricEventsV1",
		"--adx-mapping TauExpMetricEventsV1Json",
		"--adx-client-id 00000000-0000-0000-0000-000000000001",
		"--adx-max-attempts 4",
		"--adx-retry-backoff 2s",
		"--adx-final-status-timeout 5m0s",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("collector container missing %q: %s", want, rendered)
		}
	}
}

func toText(value any) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(strings.Join(flatten(value), " ")), "[", ""), "]", "")
}

func flatten(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, flatten(item)...)
		}
		return out
	case map[string]any:
		var out []string
		for key, item := range typed {
			out = append(out, key)
			out = append(out, flatten(item)...)
		}
		return out
	default:
		return nil
	}
}
