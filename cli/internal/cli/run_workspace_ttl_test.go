// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"testing"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

func workspaceWithTTL(ttl *int64) tauworkspace.Workspace {
	w := readyWorkspace()
	w.Spec.Defaults.TTLSecondsAfterFinished = ttl
	return w
}

func int64Ptr(v int64) *int64 { return &v }

func TestWorkspaceTTLDefaultAppliesWhenRunIsSilent(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.ttlSecondsAfterFinished = 0

	got, err := applyWorkspaceDefaults(o, workspaceWithTTL(int64Ptr(604800)), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.ttlSecondsAfterFinished != 604800 {
		t.Errorf("ttlSecondsAfterFinished = %d, want 604800", got.ttlSecondsAfterFinished)
	}
}

func TestExplicitRunTTLBeatsWorkspaceDefault(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.ttlSecondsAfterFinished = 900

	got, err := applyWorkspaceDefaults(o, workspaceWithTTL(int64Ptr(604800)), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.ttlSecondsAfterFinished != 900 {
		t.Errorf("ttlSecondsAfterFinished = %d, want the run's own 900", got.ttlSecondsAfterFinished)
	}
}

func TestUnsetWorkspaceTTLLeavesBuiltInRetention(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.ttlSecondsAfterFinished = 0

	got, err := applyWorkspaceDefaults(o, workspaceWithTTL(nil), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.ttlSecondsAfterFinished != 0 {
		t.Errorf("ttlSecondsAfterFinished = %d, want 0 so the renderer keeps its built-in default", got.ttlSecondsAfterFinished)
	}
}

func TestWorkspaceTTLBelowTheDurabilityFloorIsIgnored(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.ttlSecondsAfterFinished = 0

	got, err := applyWorkspaceDefaults(o, workspaceWithTTL(int64Ptr(1)), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.ttlSecondsAfterFinished != 0 {
		t.Errorf("ttlSecondsAfterFinished = %d; a sub-floor workspace default must be ignored so the built-in retention applies",
			got.ttlSecondsAfterFinished)
	}
}

func TestWorkspaceTTLAtTheFloorIsAccepted(t *testing.T) {
	o := defaultRunDispatchOptions()
	o.ttlSecondsAfterFinished = 0

	got, err := applyWorkspaceDefaults(o, workspaceWithTTL(int64Ptr(tauworkspace.MinTTLSecondsAfterFinished)), "smoke")
	if err != nil {
		t.Fatalf("applyWorkspaceDefaults: %v", err)
	}
	if got.ttlSecondsAfterFinished != tauworkspace.MinTTLSecondsAfterFinished {
		t.Errorf("ttlSecondsAfterFinished = %d, want %d", got.ttlSecondsAfterFinished, tauworkspace.MinTTLSecondsAfterFinished)
	}
}
