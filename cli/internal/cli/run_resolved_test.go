// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"testing"

	"github.com/Azure/taugrid/core/runconfig"
)

func TestResolveRunManagedWorkflowOptionsPreservesTTL(t *testing.T) {
	const ttlSecondsAfterFinished = int64(3600)

	input := unresolvedRunOptions{}
	input.ttlSecondsAfterFinished = ttlSecondsAfterFinished
	got := resolveRunManagedWorkflowOptions(input)
	if got.ttlSecondsAfterFinished != ttlSecondsAfterFinished {
		t.Fatalf("ttlSecondsAfterFinished = %d, want %d", got.ttlSecondsAfterFinished, ttlSecondsAfterFinished)
	}
}

func TestResolveRunJobOptionsPreservesExplicitTTL(t *testing.T) {
	input := unresolvedRunOptions{}
	input.ttlSecondsAfterFinished = 3600
	input.ttlExplicit = true
	got := resolveRunJobOptions(input)
	if got.ttlSecondsAfterFinished != 3600 || !got.ttlExplicit {
		t.Fatalf("ttl = %d explicit=%v, want 3600 explicit=true", got.ttlSecondsAfterFinished, got.ttlExplicit)
	}
}

func TestRunConfigMarksTTLExplicit(t *testing.T) {
	ttl := int64(3600)
	got, err := configToDispatch(runconfig.Config{Run: runconfig.Run{TTLSecondsAfterFinished: &ttl}}, "tau.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got.ttlSecondsAfterFinished != ttl || !got.ttlExplicit {
		t.Fatalf("ttl = %d explicit=%v, want 3600 explicit=true", got.ttlSecondsAfterFinished, got.ttlExplicit)
	}
	got, err = configToDispatch(runconfig.Config{}, "tau.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got.ttlExplicit {
		t.Fatal("ttlExplicit set without run.ttl_seconds_after_finished")
	}
}
