// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queue

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func testPolicy() topology.Policy {
	return topology.Policy{
		Name:       "test-azure",
		SourceFile: "test-policy.yaml",
		Presets: map[string]topology.Preset{
			"azure.research.training.l": {
				Name:           "azure.research.training.l",
				Team:           "research",
				Lane:           "training",
				Mode:           "fixed",
				Placement:      "independent",
				Shape:          "1xa100-80gb",
				GPUClass:       "a100-80gb",
				QueueName:      "research-training",
				ClusterQueue:   "team-research-reserved-cq",
				ResourceFlavor: "gpu-a100-80gb-dra",
				Workers:        1,
			},
			"azure.research.training.xl": {
				Name:           "azure.research.training.xl",
				Team:           "research",
				Lane:           "training",
				Mode:           "fixed",
				Placement:      "single-node-nvlink",
				Shape:          "8xa100-80gb",
				GPUClass:       "a100-80gb",
				QueueName:      "research-training",
				ClusterQueue:   "team-research-reserved-cq",
				ResourceFlavor: "gpu-a100-80gb-dra",
				Workers:        1,
			},
			"azure.research.large-memory.xl": {
				Name:           "azure.research.large-memory.xl",
				Team:           "research",
				Lane:           "large-memory",
				Mode:           "fixed",
				Placement:      "single-node-nvlink",
				Shape:          "8xh200-141gb",
				GPUClass:       "h200-141gb",
				QueueName:      "research-large-memory",
				ClusterQueue:   "team-research-reserved-cq",
				ResourceFlavor: "gpu-h200-141gb-dra",
				Workers:        1,
			},
		},
	}
}

func TestBuildSnapshotMapsA100PressureAndH200Headroom(t *testing.T) {
	snap, err := BuildSnapshot("ray", testPolicy(), []byte(localQueuesJSON), []byte(clusterQueuesJSON), []byte(workloadsJSON), Options{Team: "research"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Namespace != "ray" || snap.PolicySource != "test-policy.yaml" {
		t.Fatalf("snapshot metadata not preserved: %#v", snap)
	}
	if len(snap.Groups) != 2 {
		t.Fatalf("groups=%d want 2: %#v", len(snap.Groups), snap.Groups)
	}

	a100 := findGroup(t, snap, "a100-80gb")
	if a100.Pending != 2 || a100.Admitted != 2 || a100.Reserving != 0 {
		t.Fatalf("a100 queue counts wrong: %#v", a100)
	}
	if !a100.QuotaFound || a100.GPUNominal != 16 || a100.GPUReserved != 16 || a100.GPUUsed != 16 || a100.GPUHeadroom != 0 {
		t.Fatalf("a100 quota wrong: %#v", a100)
	}
	if got := strings.Join(a100.Presets, ","); got != "azure.research.training.l,azure.research.training.xl" {
		t.Fatalf("a100 presets=%q", got)
	}
	if len(a100.PendingWorkloads) != 1 || a100.PendingWorkloads[0].Name != "a100-waiting" {
		t.Fatalf("a100 pending workloads not attached: %#v", a100.PendingWorkloads)
	}
	if a100.PendingWorkloads[0].GPURequested != 8 {
		t.Fatalf("gpu request=%d want 8", a100.PendingWorkloads[0].GPURequested)
	}

	h200 := findGroup(t, snap, "h200-141gb")
	if h200.Pending != 0 || h200.Admitted != 0 {
		t.Fatalf("h200 queue counts wrong: %#v", h200)
	}
	if !h200.QuotaFound || h200.GPUNominal != 8 || h200.GPUReserved != 0 || h200.GPUHeadroom != 8 {
		t.Fatalf("h200 quota wrong: %#v", h200)
	}
	if len(snap.Hints) != 1 || !strings.Contains(snap.Hints[0], "A100 training queue is saturated") {
		t.Fatalf("expected saturation hint, got %#v", snap.Hints)
	}
}

func TestBuildSnapshotFiltersNormalizeInput(t *testing.T) {
	snap, err := BuildSnapshot("ray", testPolicy(), []byte(localQueuesJSON), []byte(clusterQueuesJSON), []byte(workloadsJSON), Options{
		Team:     "Research",
		Lane:     "large_memory",
		GPUClass: "H200 NVLINK 141GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Groups) != 1 {
		t.Fatalf("groups=%d want 1: %#v", len(snap.Groups), snap.Groups)
	}
	if snap.Groups[0].GPUClass != "h200-141gb" || snap.Groups[0].Lane != "large-memory" {
		t.Fatalf("wrong filtered group: %#v", snap.Groups[0])
	}
	if len(snap.Hints) != 0 {
		t.Fatalf("filtered H200-only view should not emit A100 hint: %#v", snap.Hints)
	}
}

func TestSnapshotCarriesHintsAndPendingWorkloads(t *testing.T) {
	snap, err := BuildSnapshot("ray", testPolicy(), []byte(localQueuesJSON), []byte(clusterQueuesJSON), []byte(workloadsJSON), Options{Team: "research"})
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]Group{}
	for _, g := range snap.Groups {
		classes[g.GPUClass] = g
	}
	for _, want := range []string{"a100-80gb", "h200-141gb"} {
		if _, ok := classes[want]; !ok {
			t.Fatalf("missing GPU class %q in groups: %#v", want, snap.Groups)
		}
	}
	if got := classes["a100-80gb"].GPUReserved; got != 16 {
		t.Fatalf("a100 GPUReserved=%d want 16", got)
	}
	if len(snap.Hints) == 0 {
		t.Fatalf("expected hints, got none")
	}

	var pending []PendingWorkload
	for _, g := range snap.Groups {
		pending = append(pending, g.PendingWorkloads...)
	}
	var found *PendingWorkload
	for i := range pending {
		if pending[i].Name == "a100-waiting" {
			found = &pending[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("missing pending workload a100-waiting: %#v", pending)
	}
	if found.Preset != "azure.research.training.xl" {
		t.Fatalf("preset=%q want azure.research.training.xl", found.Preset)
	}
	if found.Shape != "8xa100-80gb" {
		t.Fatalf("shape=%q want 8xa100-80gb", found.Shape)
	}
	if found.Reason != "QuotaNotReserved" {
		t.Fatalf("reason=%q want QuotaNotReserved", found.Reason)
	}
}

func TestSnapshotJSONIsMachineReadable(t *testing.T) {
	snap, err := BuildSnapshot("ray", testPolicy(), []byte(localQueuesJSON), []byte(clusterQueuesJSON), []byte(workloadsJSON), Options{Team: "research"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"gpuClass":"a100-80gb"`, `"gpuHeadroom":8`, `"pendingWorkloads"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in JSON: %s", want, raw)
		}
	}
}

func TestBuildSnapshotRejectsBadJSON(t *testing.T) {
	_, err := BuildSnapshot("ray", testPolicy(), []byte("not-json"), []byte(clusterQueuesJSON), []byte(workloadsJSON), Options{})
	if err == nil || !strings.Contains(err.Error(), "parse localqueues") {
		t.Fatalf("expected localqueue parse error, got %v", err)
	}
}

func findGroup(t *testing.T, snap Snapshot, gpuClass string) Group {
	t.Helper()
	for _, g := range snap.Groups {
		if g.GPUClass == gpuClass {
			return g
		}
	}
	t.Fatalf("group %s not found in %#v", gpuClass, snap.Groups)
	return Group{}
}

const localQueuesJSON = `{
  "items": [
    {
      "metadata": {"name": "research-training"},
      "spec": {"clusterQueue": "team-research-reserved-cq"},
      "status": {"pendingWorkloads": 2, "admittedWorkloads": 2, "reservingWorkloads": 0}
    },
    {
      "metadata": {"name": "research-large-memory"},
      "spec": {"clusterQueue": "team-research-reserved-cq"},
      "status": {"pendingWorkloads": 0, "admittedWorkloads": 0, "reservingWorkloads": 0}
    }
  ]
}`

const clusterQueuesJSON = `{
  "items": [
    {
      "metadata": {"name": "team-research-reserved-cq"},
      "spec": {
        "resourceGroups": [
          {
            "coveredResources": ["gpu.nvidia.com"],
            "flavors": [
              {
                "name": "gpu-a100-80gb-dra",
                "resources": [{"name": "gpu.nvidia.com", "nominalQuota": "16"}]
              },
              {
                "name": "gpu-h200-141gb-dra",
                "resources": [{"name": "gpu.nvidia.com", "nominalQuota": "8"}]
              }
            ]
          }
        ]
      },
      "status": {
        "flavorsReservation": [
          {"name": "gpu-a100-80gb-dra", "resources": [{"name": "gpu.nvidia.com", "total": "16", "borrowed": "0"}]},
          {"name": "gpu-h200-141gb-dra", "resources": [{"name": "gpu.nvidia.com", "total": "0", "borrowed": "0"}]}
        ],
        "flavorsUsage": [
          {"name": "gpu-a100-80gb-dra", "resources": [{"name": "gpu.nvidia.com", "total": "16", "borrowed": "0"}]},
          {"name": "gpu-h200-141gb-dra", "resources": [{"name": "gpu.nvidia.com", "total": "0", "borrowed": "0"}]}
        ]
      }
    }
  ]
}`

const workloadsJSON = `{
  "items": [
    {
      "metadata": {
        "name": "a100-waiting",
        "namespace": "ray",
        "creationTimestamp": "2026-05-03T20:00:00Z",
        "labels": {
          "` + workloadmeta.LabelTeam + `": "research",
          "` + workloadmeta.LabelLane + `": "training",
          "` + workloadmeta.LabelGPUClass + `": "a100-nvlink-80gb",
          "` + workloadmeta.LabelShape + `": "8xa100-80gb",
          "` + workloadmeta.LabelPreset + `": "azure.research.training.xl"
        }
      },
      "spec": {
        "queueName": "research-training",
        "podSets": [
          {
            "count": 1,
            "template": {"spec": {"containers": [{"resources": {"requests": {"gpu.nvidia.com": "8"}}}]}}
          }
        ]
      },
      "status": {
        "conditions": [
          {"type": "Admitted", "status": "False", "reason": "QuotaNotReserved", "message": "insufficient gpu.nvidia.com quota"}
        ]
      }
    },
    {
      "metadata": {
        "name": "a100-running",
        "namespace": "ray",
        "creationTimestamp": "2026-05-03T19:00:00Z",
        "labels": {
          "` + workloadmeta.LabelTeam + `": "research",
          "` + workloadmeta.LabelLane + `": "training",
          "` + workloadmeta.LabelGPUClass + `": "a100-80gb",
          "` + workloadmeta.LabelPreset + `": "azure.research.training.l"
        }
      },
      "spec": {"queueName": "research-training"},
      "status": {"conditions": [{"type": "Admitted", "status": "True"}]}
    }
  ]
}`

func TestBuildSnapshotRequiresExplicitNamespace(t *testing.T) {
	_, err := BuildSnapshot("", testPolicy(), []byte(`{"items":[]}`), []byte(`{"items":[]}`), []byte(`{"items":[]}`), Options{})
	if err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("error = %v, want required namespace", err)
	}
}
