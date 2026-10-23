// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNotebookRenderedRayJobPopulatesTheRunList feeds the exact object the
// notebook submit path renders (core/testdata/notebook-rayjob.json, produced by
// sdk/python/python/tau/jupyter/submit.build_plan) through the run-list reader.
//
// The defect this guards: a notebook RayJob stamped only tau.azure.com/managed-by
// is discovered by the board but has an empty RunID, so it cannot be deduplicated
// or addressed by run identity. The Python test pins the renderer to this file;
// this test pins the consumer to it, so the two cannot drift apart.
func TestNotebookRenderedRayJobPopulatesTheRunList(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "notebook-rayjob.json"))
	if err != nil {
		t.Fatalf("read notebook fixture: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatal("notebook fixture is not valid JSON")
	}
	list := append([]byte(`{"items":[`), raw...)
	list = append(list, []byte(`]}`)...)

	rows := parseRayJobs(time.Now(), list, false)
	if len(rows) != 1 {
		t.Fatalf("parseRayJobs returned %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Name != "nb-demo" {
		t.Errorf("Name = %q, want %q", row.Name, "nb-demo")
	}
	if row.Kind != "RayJob" {
		t.Errorf("Kind = %q, want %q", row.Kind, "RayJob")
	}
	// The identity the run list reads. Empty here means the row is unusable.
	if row.RunID != "nb-demo" {
		t.Errorf("RunID = %q, want %q (tau.azure.com/run-id must be stamped)", row.RunID, "nb-demo")
	}
	if row.Queue != "namespace-q" {
		t.Errorf("Queue = %q, want %q", row.Queue, "namespace-q")
	}
	if row.Namespace != "ray" {
		t.Errorf("Namespace = %q, want %q", row.Namespace, "ray")
	}
}
