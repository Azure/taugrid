// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metricsoffload

import (
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

func TestRuntimeAllowsDefaultDoneTimeout(t *testing.T) {
	runtime := Runtime{
		Image:               "registry.example.com/taugrid/tau:v0.5.0",
		RunID:               "run-1",
		Project:             "project",
		Experiment:          "experiment",
		Group:               "group",
		Store:               "/data/store",
		Out:                 "/data/out",
		History:             []string{"metrics.jsonl"},
		CompletionFile:      "/data/completion",
		RemoteWriteEndpoint: "http://localhost/receive",
		Interval:            time.Second,
		DoneFile:            "/data/done",
	}
	if err := runtime.Validate(); err != nil {
		t.Fatalf("Validate() with default done timeout: %v", err)
	}
	runtime.DoneTimeout = -time.Second
	if err := runtime.Validate(); err == nil {
		t.Fatal("Validate() unexpectedly accepted a negative done timeout")
	}
}
