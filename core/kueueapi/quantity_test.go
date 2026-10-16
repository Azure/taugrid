// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kueueapi

import (
	"encoding/json"
	"testing"
)

func TestQuantityInt64(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"0", 0},
		{"1", 1},
		{"8", 8},
		{"900", 900},
		{"1000", 1000},
		{"100000", 100000},

		{"1k", 1000},
		{"100k", 100000},
		{"1M", 1000000},
		{"1G", 1000000000},

		{"100Ki", 102400},
		{"900Mi", 943718400},
		{"900Gi", 966367641600},
		{"100Ti", 109951162777600},

		{"500m", 1},
		{"100m", 1},
		{"1500m", 2},
		{"2000m", 2},

		{" 8 ", 8},
		{" 1k ", 1000},

		{"", 0},
		{"  ", 0},

		{"abc", 0},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			q := Quantity{raw: tc.raw}
			if got := q.Int64(); got != tc.want {
				t.Errorf("Quantity{%q}.Int64() = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestQuantityUnmarshalJSON_Number(t *testing.T) {
	var q Quantity
	if err := json.Unmarshal([]byte(`1000`), &q); err != nil {
		t.Fatal(err)
	}
	if got := q.Int64(); got != 1000 {
		t.Errorf("Int64() = %d after unmarshal from number 1000, want 1000", got)
	}
}

func TestQuantityBug17_KubernetesNormalizedGPUQuota(t *testing.T) {
	raw := `{
		"spec": {
			"resourceGroups": [{
				"flavors": [{
					"name": "taugrid-default",
					"resources": [
						{"name": "cpu", "nominalQuota": "100k"},
						{"name": "memory", "nominalQuota": "100Ti"},
						{"name": "nvidia.com/gpu", "nominalQuota": "1k"}
					]
				}]
			}]
		}
	}`
	var cq ClusterQueue
	if err := json.Unmarshal([]byte(raw), &cq); err != nil {
		t.Fatal(err)
	}

	gpuCap, ok := cq.NominalGPU("")
	if !ok {
		t.Fatal("NominalGPU returned ok=false, want true")
	}
	if gpuCap != 1000 {
		t.Errorf("NominalGPU = %d, want 1000", gpuCap)
	}

	maxCap, ok := cq.MaxGPUCapacity("", GPUResourceDevicePlugin)
	if !ok {
		t.Fatal("MaxGPUCapacity returned ok=false, want true")
	}
	if maxCap != 1000 {
		t.Errorf("MaxGPUCapacity = %d, want 1000", maxCap)
	}

	flavorName, flavorCap, ok := cq.BestGPUFlavorFor(nil, GPUResourceDevicePlugin)
	if !ok {
		t.Fatal("BestGPUFlavorFor returned ok=false, want true")
	}
	if flavorName != "taugrid-default" {
		t.Errorf("BestGPUFlavorFor name = %q, want %q", flavorName, "taugrid-default")
	}
	if flavorCap != 1000 {
		t.Errorf("BestGPUFlavorFor cap = %d, want 1000", flavorCap)
	}
}
