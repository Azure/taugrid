// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNotebookRenderedRayJobPopulatesTheRunProfile feeds the exact object the
// notebook submit path renders (core/testdata/notebook-rayjob.json, produced by
// sdk/python/python/tau/jupyter/submit.build_plan) through the status/profile
// consumer.
//
// The defect this guards: the run profile is the surface a researcher uses to
// answer "what happened to my run?", and it renders run_id, workload_kind, and
// profile from the workload metadata. A notebook RayJob that stamps only
// managed-by shows "not declared" for the profile and workload kind even though
// the run is real.
func TestNotebookRenderedRayJobPopulatesTheRunProfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "notebook-rayjob.json"))
	if err != nil {
		t.Fatalf("read notebook fixture: %v", err)
	}

	var snap Snapshot
	hydrateRayJob(&snap, raw)
	if !snap.RayJobFound {
		t.Fatal("hydrateRayJob did not recognize the notebook RayJob fixture")
	}

	fields := map[string]string{}
	for _, field := range RunProfileFields(snap, CostProfile{}) {
		fields[field.Name] = field.Value
	}
	for name, want := range map[string]string{
		"run_id":        "nb-demo",
		"workload_kind": "rayjob",
		"profile":       "notebook-cpu",
		"team":          "research",
	} {
		if got := fields[name]; got != want {
			t.Errorf("run profile %s = %q, want %q", name, got, want)
		}
	}
}
