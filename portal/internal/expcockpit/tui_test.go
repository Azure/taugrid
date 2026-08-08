// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"strings"
	"testing"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestRenderSnapshotTUILabelsExperimentRecord(t *testing.T) {
	out := string(RenderSnapshotTUI(Snapshot{
		Target:     "nanogpt-api-surface",
		TargetType: "experiment",
		StorePath:  "kusto://ExperimentMetrics",
		Experiment: &expstore.ExperimentRecord{
			ExperimentID: "nanogpt-api-surface",
			Name:         "NanoGPT API surface",
		},
	}))

	if !strings.Contains(out, "experiment: NanoGPT API surface (nanogpt-api-surface)\n") {
		t.Fatalf("TUI output missing experiment record label:\n%s", out)
	}
	if strings.Count(out, "experiment:") != 1 {
		t.Fatalf("TUI output should contain exactly one bare experiment label:\n%s", out)
	}
}
