// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"testing"

	"github.com/Azure/taugrid/cli/internal/manifest"
)

func TestManagedWorkflowGPUDemandCountsEveryGPUPod(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gpus   int
		worker int
		kind   string
		want   int
	}{
		{"training multi-node", 8, 4, manifest.WorkloadKindRayJob, 32},
		{"training single pod", 8, 1, manifest.WorkloadKindRayJob, 8},
		{"eval keeps one GPU pod", 8, 4, manifest.WorkloadKindRayJobEval, 8},
		{"plain job is one pod", 4, 1, manifest.WorkloadKindJob, 4},
		{"cpu-only", 0, 4, manifest.WorkloadKindRayJob, 0},
	} {
		m := &manifest.Manifest{}
		m.Compute.GPUs = tc.gpus
		m.Compute.Workers = tc.worker
		if got := managedWorkflowGPUDemand(m, tc.kind); got != tc.want {
			t.Errorf("%s: demand = %d, want %d", tc.name, got, tc.want)
		}
	}
}
