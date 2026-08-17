// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"testing"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

// TauWorkspace.spec.defaults.ttlSecondsAfterFinished lets an operator set the
// retention for finished batch Jobs cluster-wide without editing every run
// config. It is an override, not a floor: a run that names its own TTL keeps
// it, and a workspace that names none leaves tau's built-in retention exactly
// as it was.

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

// An explicit run.ttl_seconds_after_finished has already been applied by
// configToDispatch before workspace defaults are merged, and must survive.
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

// The whole point of the pointer in the CRD: absence must not be read as zero,
// which Kubernetes would treat as "delete immediately".
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
