// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/version"
)

func TestRunGlobalHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "taugrid-metrics-collector collect [flags]") {
		t.Fatalf("help output = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunGlobalVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := version.Version, version.Commit, version.Date
	t.Cleanup(func() {
		version.Version, version.Commit, version.Date = oldVersion, oldCommit, oldDate
	})
	version.Version, version.Commit, version.Date = "v1.2.3", "abc123", "2026-09-18T00:00:00Z"

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{binaryName + " v1.2.3", "commit: abc123", "built:  2026-09-18T00:00:00Z"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("version output %q missing %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCollectHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"collect", "--help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "-delivery-mode") ||
		!strings.Contains(stderr.String(), "-adx-cluster-uri") ||
		!strings.Contains(stderr.String(), "-adx-mapping") ||
		!strings.Contains(stderr.String(), "-adx-client-id") {
		t.Fatalf("collect help lacks ADX configuration: %q", stderr.String())
	}
}

func TestRunCollectUsesManagedWorkflowEnvironment(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	if err := os.WriteFile(history, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]string{
		"TAU_METRICS_HISTORY":                 history,
		"TAU_METRICS_OFFLOAD_RUN":             "managed-run",
		"TAU_METRICS_OFFLOAD_PROJECT":         "managed-project",
		"TAU_METRICS_OFFLOAD_EXPERIMENT":      "managed-experiment",
		"TAU_METRICS_OFFLOAD_GROUP":           "managed-group",
		"TAU_METRICS_OFFLOAD_SOURCE":          "managed-source",
		"TAU_METRICS_OFFLOAD_OUT":             filepath.Join(root, "out"),
		"TAU_METRICS_OFFLOAD_COMPLETION_FILE": filepath.Join(root, "completion.json"),
		"TAU_METRICS_OFFLOAD_INTERVAL":        "1ms",
		"TAU_METRICS_OFFLOAD_TAGS":            "tau_workspace=workspace,tau_namespace=namespace",
		"TAU_METRICS_OFFLOAD_ADX_CLUSTER_URI": "https://cluster.kusto.windows.net",
		"TAU_METRICS_OFFLOAD_ADX_DATABASE":    "metrics",
		"TAU_METRICS_OFFLOAD_ADX_CLIENT_ID":   "00000000-0000-0000-0000-000000000001",
	} {
		t.Setenv(name, value)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"collect", "--watch", "--max-iterations", "1"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"completed":false`) {
		t.Fatalf("collector output = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCollectAcceptsRequiredADXEnvironment(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	if err := os.WriteFile(history, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"TAU_METRICS_HISTORY":                          history,
		"TAU_METRICS_OFFLOAD_RUN":                      "run",
		"TAU_METRICS_OFFLOAD_PROJECT":                  "project",
		"TAU_METRICS_OFFLOAD_EXPERIMENT":               "experiment",
		"TAU_METRICS_OFFLOAD_GROUP":                    "group",
		"TAU_METRICS_OFFLOAD_SOURCE":                   "source",
		"TAU_METRICS_OFFLOAD_OUT":                      filepath.Join(root, "out"),
		"TAU_METRICS_OFFLOAD_DELIVERY_MODE":            "adx-required",
		"TAU_METRICS_OFFLOAD_ADX_CLUSTER_URI":          "https://cluster.kusto.windows.net",
		"TAU_METRICS_OFFLOAD_ADX_DATABASE":             "metrics",
		"TAU_METRICS_OFFLOAD_ADX_TABLE":                "MetricEvents",
		"TAU_METRICS_OFFLOAD_ADX_MAPPING":              "MetricEventChunkNDJSON",
		"TAU_METRICS_OFFLOAD_ADX_CLIENT_ID":            "00000000-0000-0000-0000-000000000001",
		"TAU_METRICS_OFFLOAD_ADX_MAX_ATTEMPTS":         "2",
		"TAU_METRICS_OFFLOAD_ADX_RETRY_BACKOFF":        "1ms",
		"TAU_METRICS_OFFLOAD_ADX_FINAL_STATUS_TIMEOUT": "1s",
	} {
		t.Setenv(name, value)
	}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"collect"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"completed":false`) {
		t.Fatalf("collector output = %q", stdout.String())
	}
}

func TestRunCollectRejectsPartialADXConfiguration(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	if err := os.WriteFile(history, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"collect", "--run", "run", "--project", "project", "--experiment", "experiment",
		"--group", "group", "--out", filepath.Join(root, "out"), "--history", history,
		"--delivery-mode", "adx-required",
		"--adx-cluster-uri", "https://cluster.kusto.windows.net",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--adx-database") {
		t.Fatalf("partial ADX error=%v", err)
	}
}

func TestRunCollectRejectsUnknownDeliveryMode(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	if err := os.WriteFile(history, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"collect", "--run", "run", "--project", "project", "--experiment", "experiment",
		"--group", "group", "--out", filepath.Join(root, "out"), "--history", history,
		"--delivery-mode", "adx-only",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--delivery-mode") {
		t.Fatalf("unknown delivery mode error=%v", err)
	}
}
