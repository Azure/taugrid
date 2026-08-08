// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package topology

import (
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

// TauGrid v0 admits exactly one workspace per cluster, so there is exactly one
// namespace that workload LocalQueues can live in. These tests lock that in.
//
// They exist because the shipped policy previously pinned every preset to a
// "taugrid-team" namespace that no install created, which meant a preset that
// reached the namespace fallback searched somewhere permanently empty.

func TestEveryEmbeddedPresetTargetsTheDefaultWorkspaceNamespace(t *testing.T) {
	policy, err := LoadPolicy("")
	if err != nil {
		t.Fatalf("load embedded policy: %v", err)
	}
	names := policy.Names()
	if len(names) == 0 {
		t.Fatal("embedded policy exposes no presets; this test would pass vacuously")
	}
	for _, name := range names {
		resolved, err := ResolvePreset("", name)
		if err != nil {
			t.Fatalf("resolve preset %s: %v", name, err)
		}
		// Empty namespace is the fallback path a caller hits when it cannot
		// determine a workspace, which is exactly where the old bug lived.
		if got := PresetLocalQueueNamespace("", resolved); got != workloadmeta.DefaultWorkspaceName {
			t.Errorf("preset %s resolves LocalQueues in namespace %q, want %q; no TauGrid install creates any other namespace",
				name, got, workloadmeta.DefaultWorkspaceName)
		}
	}
}

func TestDefaultLocalQueueNamespaceMatchesDefaultWorkspace(t *testing.T) {
	if DefaultLocalQueueNamespace != workloadmeta.DefaultWorkspaceName {
		t.Fatalf("DefaultLocalQueueNamespace=%q but the default workspace namespace is %q; presets would look for queues where none are created",
			DefaultLocalQueueNamespace, workloadmeta.DefaultWorkspaceName)
	}
}

func TestExplicitNamespaceStillOverridesPreset(t *testing.T) {
	// Operators who do rename the workspace must keep working: an explicit
	// namespace has to beat whatever the preset carries.
	resolved, err := ResolvePreset("", "azure.research.training.l")
	if err != nil {
		t.Fatalf("resolve preset: %v", err)
	}
	if resolved.Preset.Namespace == "" {
		t.Fatal("preset carries no namespace; override test would pass vacuously")
	}
	if got := PresetLocalQueueNamespace("renamed-workspace", resolved); got != "renamed-workspace" {
		t.Fatalf("explicit namespace = %q, want %q", got, "renamed-workspace")
	}
}
