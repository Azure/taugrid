// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kueueapi

import (
	"encoding/json"
	"testing"
)

func clusterQueueFrom(t *testing.T, raw string) ClusterQueue {
	t.Helper()
	var cq ClusterQueue
	if err := json.Unmarshal([]byte(raw), &cq); err != nil {
		t.Fatalf("unmarshal ClusterQueue: %v", err)
	}
	return cq
}

// gpuFlavors renders a ClusterQueue whose flavors each carry one
// nvidia.com/gpu nominalQuota, so the tests below only vary what matters.
func gpuFlavors(t *testing.T, quotas map[string]string) ClusterQueue {
	t.Helper()
	raw := `{"spec":{"resourceGroups":[{"flavors":[`
	first := true
	for name, quota := range quotas {
		if !first {
			raw += ","
		}
		first = false
		raw += `{"name":"` + name + `","resources":[{"name":"nvidia.com/gpu","nominalQuota":"` + quota + `"}]}`
	}
	raw += `]}]}}`
	return clusterQueueFrom(t, raw)
}

// A workload lands on exactly one flavor, so the unpinned ceiling is the
// largest single flavor -- not the sum.
func TestMaxGPUCapacityUnpinnedReportsLargestFlavorNotSum(t *testing.T) {
	cq := gpuFlavors(t, map[string]string{"a100": "8", "h100": "24", "h200": "16"})
	got, ok := cq.MaxGPUCapacity("", "nvidia.com/gpu")
	if !ok {
		t.Fatal("MaxGPUCapacity reported no GPU quota for a queue that has three GPU flavors")
	}
	if got != 24 {
		t.Errorf("MaxGPUCapacity(\"\") = %d, want 24 (largest flavor, not the 48 sum)", got)
	}
}

func TestMaxGPUCapacityNoFlavorsOrNoGPUResource(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"no resource groups", `{"spec":{}}`},
		{"empty flavor list", `{"spec":{"resourceGroups":[{"flavors":[]}]}}`},
		{
			"flavors without GPU resources",
			`{"spec":{"resourceGroups":[{"flavors":[{"name":"cpu","resources":[{"name":"cpu","nominalQuota":"100"}]}]}]}}`,
		},
	} {
		cq := clusterQueueFrom(t, tc.raw)
		if got, ok := cq.MaxGPUCapacity("", "nvidia.com/gpu"); ok || got != 0 {
			t.Errorf("%s: MaxGPUCapacity(\"\") = (%d, %v), want (0, false)", tc.name, got, ok)
		}
	}
}

func TestMaxGPUCapacityNamedFlavorIgnoresOtherFlavors(t *testing.T) {
	cq := gpuFlavors(t, map[string]string{"a100": "8", "h100": "24"})
	if got, ok := cq.MaxGPUCapacity("a100", "nvidia.com/gpu"); !ok || got != 8 {
		t.Errorf("MaxGPUCapacity(\"a100\") = (%d, %v), want (8, true)", got, ok)
	}
	if got, ok := cq.MaxGPUCapacity("missing", "nvidia.com/gpu"); ok || got != 0 {
		t.Errorf("MaxGPUCapacity(\"missing\") = (%d, %v), want (0, false)", got, ok)
	}
}

// borrowingLimit is part of what a single flavor can admit, so it still counts
// toward that flavor's capacity -- the fix only changes how flavors combine.
func TestMaxGPUCapacityIncludesBorrowingLimitPerFlavor(t *testing.T) {
	cq := clusterQueueFrom(t, `{"spec":{"resourceGroups":[{"flavors":[
      {"name":"a100","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8","borrowingLimit":"4"}]},
      {"name":"h200","resources":[{"name":"nvidia.com/gpu","nominalQuota":"10"}]}
    ]}]}}`)
	if got, ok := cq.MaxGPUCapacity("", "nvidia.com/gpu"); !ok || got != 12 {
		t.Errorf("MaxGPUCapacity(\"\") = (%d, %v), want (12, true)", got, ok)
	}
}
