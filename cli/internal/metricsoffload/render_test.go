package metricsoffload

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRuntime(dir string) Runtime {
	return Runtime{
		Image:               "example.test/tau:v1",
		RunID:               "ray-run",
		Project:             "pretraining",
		Experiment:          "modernbert-fineweb",
		Group:               "fwe100",
		Tags:                map[string]string{"tau_workspace": "research-workspace"},
		Source:              "stellar-online",
		Store:               filepath.Join(dir, "store"),
		Out:                 filepath.Join(dir, "out"),
		History:             []string{filepath.Join(dir, "metrics-*.jsonl")},
		CompletionFile:      filepath.Join(dir, "completion.json"),
		RemoteWriteEndpoint: "http://127.0.0.1:3100/receive",
		Interval:            time.Second,
		ReadyFile:           filepath.Join(dir, "ready"),
		ReadyTimeout:        time.Second,
		DoneFile:            filepath.Join(dir, "done"),
		DoneTimeout:         50 * time.Millisecond,
	}
}

func TestWrapCommandSurfacesMissingTerminalPublication(t *testing.T) {
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
