package cli

import (
	"context"
	"testing"

	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/core/exptelemetry"
)

func TestRunExperimentMetadataOverridesMetricsOffloadScope(t *testing.T) {
	ctx := withRunExperimentMetadata(context.Background(), runExperimentMetadata{
		Workspace:    "sample",
		Project:      "modernbert",
		ExperimentID: "fineweb-scaling",
		RunGroupID:   "baseline",
		Tags:         map[string]string{"dataset": "fineweb"},
	})

	got := applyRunExperimentMetricsOffload(ctx, manifest.MetricsOffloadOptions{
		Project: "profile-default",
		Group:   "profile-default",
		Tags: map[string]string{
			"recipe":                     "pretrain",
			exptelemetry.TauWorkspaceTag: "spoofed",
		},
	})

	if got.Project != "modernbert" || got.Experiment != "fineweb-scaling" || got.Group != "baseline" {
		t.Fatalf("metrics offload experiment scope = %#v", got)
	}
	if got.Tags["dataset"] != "fineweb" || got.Tags["recipe"] != "pretrain" {
		t.Fatalf("metrics offload tags were not merged: %#v", got.Tags)
	}
	if got.Tags[exptelemetry.TauWorkspaceTag] != "sample" {
		t.Fatalf("workspace tag = %q, want sample", got.Tags[exptelemetry.TauWorkspaceTag])
	}
}
