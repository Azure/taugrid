// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"github.com/Azure/taugrid/core/workloadmeta"
	"strings"
	"testing"
)

func TestGPUContractFromProfileParsesExplicitContract(t *testing.T) {
	p := Profile{
		Name: "sample-project-stt-a100",
		Spec: map[string]any{
			"resources": map[string]any{
				"gpu": map[string]any{
					"count":        1,
					"size":         "L",
					"placement":    "single_device",
					"memoryGiBMin": 80,
				},
				"dra": map[string]any{"claimTemplate": "full-gpu"},
			},
		},
	}

	c, ok, err := GPUContractFromProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected GPU contract")
	}
	if c.Count != 1 || c.Size != "l" || c.Placement != gpuPlacementSingleDevice || c.MemoryGiBMin != 80 {
		t.Fatalf("contract parsed incorrectly: %#v", c)
	}
	plan, err := BuildGPUSchedulingPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Labels[workloadmeta.AnnotationGPUCount] != "1" || plan.Labels[workloadmeta.LabelGPUPlacement] != gpuPlacementSingleDevice {
		t.Fatalf("GPU labels missing: %v", plan.Labels)
	}
	if plan.Annotations[workloadmeta.AnnotationGPUContract] != "count=1,size=l,placement=single-device,memoryGiBMin=80" {
		t.Fatalf("GPU contract annotation missing: %v", plan.Annotations)
	}
	if plan.PackingAffinity == nil {
		t.Fatal("single-device count=1 contract should request packing preference")
	}
}

func TestShouldBinPackGPUNode(t *testing.T) {
	cases := []struct {
		name string
		c    GPUContract
		want bool
	}{
		{name: "single device", c: GPUContract{Count: 1, Placement: gpuPlacementSingleDevice}, want: true},
		{name: "same node two GPUs", c: GPUContract{Count: 2, Placement: gpuPlacementSameNode}, want: true},
		{name: "same node four GPUs", c: GPUContract{Count: 4, Placement: gpuPlacementSameNode}, want: true},
		{name: "same node five GPUs", c: GPUContract{Count: 5, Placement: gpuPlacementSameNode}, want: false},
		{name: "distributed workers", c: GPUContract{Count: 4, Placement: gpuPlacementDistributedWorkers}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldBinPackGPUNode(tc.c); got != tc.want {
				t.Fatalf("shouldBinPackGPUNode(%#v) = %v, want %v", tc.c, got, tc.want)
			}
			if got := gpuBinPackingAffinity(tc.c) != nil; got != tc.want {
				t.Fatalf("gpuBinPackingAffinity(%#v) present = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}

func TestValidateGPUContractCatchesClaimMismatch(t *testing.T) {
	p := Profile{
		Name: "sample-project-llm-a100-tp2",
		Spec: map[string]any{
			"resources": map[string]any{
				"gpu": map[string]any{
					"count":     2,
					"placement": gpuPlacementSameNode,
				},
				"dra": map[string]any{"claimTemplate": "full-gpu"},
			},
		},
	}

	err := validateGPUContract(p)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if !strings.Contains(err.Error(), "count=2") || !strings.Contains(err.Error(), "full-gpu") {
		t.Fatalf("error should describe count/claim mismatch, got: %v", err)
	}
}

func TestGPUCountFromClaimTemplate(t *testing.T) {
	for claim, want := range map[string]int{
		"full-gpu":    1,
		"ds-full-gpu": 1,
		"two-gpus":    2,
		"ds-2gpus":    2,
		"ds-8gpus":    8,
	} {
		got, ok := GPUCountFromClaimTemplate(claim)
		if !ok || got != want {
			t.Fatalf("GPUCountFromClaimTemplate(%q) = %d,%v; want %d,true", claim, got, ok, want)
		}
	}
}
