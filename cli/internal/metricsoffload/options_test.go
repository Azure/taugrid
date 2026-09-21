// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metricsoffload

import (
	"strings"
	"testing"
	"time"
)

func TestMergeTagsProtectedScopeWins(t *testing.T) {
	got := MergeTags(
		map[string]string{"recipe": "profile", "tau_workspace": "profile"},
		map[string]string{"recipe": "experiment", "dataset": "fineweb", "tau_workspace": "experiment"},
		map[string]string{"tau_workspace": "workspace", "tau_namespace": "research"},
	)

	for key, want := range map[string]string{
		"recipe":        "experiment",
		"dataset":       "fineweb",
		"tau_workspace": "workspace",
		"tau_namespace": "research",
	} {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q in %#v", key, got[key], want, got)
		}
	}
}

func TestValidatePinnedImage(t *testing.T) {
	for _, image := range []string{
		"registry.example.com/taugrid/tau:v0.5.0",
		"registry.example.com/taugrid/tau@sha256:0123456789abcdef",
	} {
		if err := ValidatePinnedImage(image); err != nil {
			t.Fatalf("ValidatePinnedImage(%q): %v", image, err)
		}
	}
	for _, image := range []string{
		"",
		"registry.example.com/taugrid/tau",
		"registry.example.com/taugrid/tau:latest",
	} {
		if err := ValidatePinnedImage(image); err == nil {
			t.Fatalf("ValidatePinnedImage(%q) unexpectedly succeeded", image)
		}
	}
}

func TestValidateRuntimeImageAppliesPinPolicyToAllRuntimes(t *testing.T) {
	for _, runtime := range []string{"", RuntimeCollectorV1} {
		if err := ValidateRuntimeImage(runtime, "registry.example.com/taugrid/metrics:v1"); err != nil {
			t.Fatalf("ValidateRuntimeImage(%q, pinned): %v", runtime, err)
		}
		if err := ValidateRuntimeImage(runtime, "registry.example.com/taugrid/metrics:latest"); err == nil {
			t.Fatalf("ValidateRuntimeImage(%q, latest) unexpectedly succeeded", runtime)
		}
	}
}

func TestRuntimeValidationRejectsUnknownContract(t *testing.T) {
	runtime := testRuntime(t.TempDir())
	runtime.Runtime = "future-v2"
	runtime.Image = ""
	if err := runtime.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Validate() error = %v, want unsupported runtime", err)
	}
}

func TestRuntimeAllowsDefaultDoneTimeout(t *testing.T) {
	runtime := Runtime{
		Image:          "registry.example.com/taugrid/tau:v0.5.0",
		RunID:          "run-1",
		Project:        "project",
		Experiment:     "experiment",
		Group:          "group",
		Store:          "/data/store",
		Out:            "/data/out",
		History:        []string{"metrics.jsonl"},
		CompletionFile: "/data/completion",
		ADXClusterURI:  "https://example.kusto.windows.net",
		ADXDatabase:    "TauGrid",
		ADXClientID:    "00000000-0000-0000-0000-000000000001",
		Interval:       time.Second,
		DoneFile:       "/data/done",
	}
	if err := runtime.Validate(); err != nil {
		t.Fatalf("Validate() with default done timeout: %v", err)
	}
	runtime.DoneTimeout = -time.Second
	if err := runtime.Validate(); err == nil {
		t.Fatal("Validate() unexpectedly accepted a negative done timeout")
	}
}

func TestRuntimeValidatesTypedADXDelivery(t *testing.T) {
	runtime := testRuntime(t.TempDir())
	runtime.Runtime = RuntimeCollectorV1
	runtime.DeliveryMode = DeliveryADXRequired
	runtime.ADXClusterURI = "https://example.kusto.windows.net"
	runtime.ADXDatabase = "TauGrid"
	runtime.ADXClientID = "00000000-0000-0000-0000-000000000001"
	if err := runtime.Validate(); err != nil {
		t.Fatalf("Validate() typed ADX runtime: %v", err)
	}

	runtime.ADXClientID = ""
	if err := runtime.Validate(); err == nil || !strings.Contains(err.Error(), "client ID") {
		t.Fatalf("Validate() missing ADX identity error = %v", err)
	}
}
