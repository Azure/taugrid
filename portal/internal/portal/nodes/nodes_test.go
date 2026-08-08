// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodes

import (
	"context"
	"errors"
	"testing"
)

// fakeReader returns canned Nodes JSON so the board can be tested without a
// Kubernetes API.
type fakeReader struct {
	json                string
	err                 error
	daemonSetsJSON      string
	daemonSetsErr       error
	daemonSetsCallCount int
}

func (f *fakeReader) ListNodes(_ context.Context) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.json), nil
}
func (f *fakeReader) ListDaemonSets(_ context.Context) ([]byte, error) {
	f.daemonSetsCallCount++
	if f.daemonSetsErr != nil {
		return nil, f.daemonSetsErr
	}
	if f.daemonSetsJSON != "" {
		return []byte(f.daemonSetsJSON), nil
	}
	return []byte(`{"items":[]}`), nil
}

// nodesJSON mirrors real AKS Node objects: two H100 GPU nodes (one Ready, one
// NotReady) in the h100pool, plus a non-GPU system node. CPU is a plain core
// count, memory a Ki quantity, GPU a whole-device capacity. The GPU nodes carry
// no nvidia.com/gpu.product label (as on the live poc cluster), so GPUProduct is
// empty there; SKU still identifies the hardware.
const nodesJSON = `{"items":[
  {"metadata":{"name":"aks-h100pool-1","labels":{
      "node.kubernetes.io/instance-type":"Standard_NC40ads_H100_v5",
      "kubernetes.azure.com/agentpool":"h100pool",
      "topology.kubernetes.io/region":"westeurope",
      "topology.kubernetes.io/zone":"westeurope-0"}},
   "status":{"capacity":{"cpu":"40","memory":"329974272Ki","nvidia.com/gpu":"1"},
     "allocatable":{"nvidia.com/gpu":"1"},
     "conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}]}},
  {"metadata":{"name":"aks-h100pool-2","labels":{
      "node.kubernetes.io/instance-type":"Standard_NC40ads_H100_v5",
      "kubernetes.azure.com/agentpool":"h100pool"}},
   "status":{"capacity":{"cpu":"40","memory":"329974272Ki","nvidia.com/gpu":"1"},
     "allocatable":{"nvidia.com/gpu":"1"},
     "conditions":[{"type":"Ready","status":"False"}]}},
  {"metadata":{"name":"aks-nodepool1-1","labels":{
      "node.kubernetes.io/instance-type":"Standard_D8s_v3",
      "kubernetes.azure.com/agentpool":"nodepool1"}},
   "status":{"capacity":{"cpu":"8","memory":"32868176Ki"},
     "allocatable":{},
     "conditions":[{"type":"Ready","status":"True"}]}}
]}`

func TestBoardAggregatesFleet(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: nodesJSON}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalNodes != 3 {
		t.Fatalf("TotalNodes = %d, want 3", snap.TotalNodes)
	}
	if snap.ReadyNodes != 2 {
		t.Fatalf("ReadyNodes = %d, want 2 (one h100 is NotReady)", snap.ReadyNodes)
	}
	if snap.GPUNodes != 2 {
		t.Fatalf("GPUNodes = %d, want 2", snap.GPUNodes)
	}
	if snap.TotalGPUs != 2 {
		t.Fatalf("TotalGPUs = %d, want 2", snap.TotalGPUs)
	}
	if snap.TotalCPUCores != 88 { // 40 + 40 + 8
		t.Fatalf("TotalCPUCores = %d, want 88", snap.TotalCPUCores)
	}
	// 2×329974272Ki + 1×32868176Ki = 692816720Ki ≈ 660.7 GiB.
	if snap.TotalMemoryGiB < 660 || snap.TotalMemoryGiB > 661 {
		t.Fatalf("TotalMemoryGiB = %v, want ~660.7", snap.TotalMemoryGiB)
	}
}

func TestBoardParsesNodeFields(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: nodesJSON}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	// Sorted by name: aks-h100pool-1 leads.
	n := snap.Nodes[0]
	if n.Name != "aks-h100pool-1" {
		t.Fatalf("nodes[0] = %q, want aks-h100pool-1 (name-sorted)", n.Name)
	}
	if n.SKU != "Standard_NC40ads_H100_v5" || n.AgentPool != "h100pool" {
		t.Fatalf("sku/pool = %q/%q, want Standard_NC40ads_H100_v5/h100pool", n.SKU, n.AgentPool)
	}
	if n.Region != "westeurope" || n.Zone != "westeurope-0" {
		t.Fatalf("region/zone = %q/%q, want westeurope/westeurope-0", n.Region, n.Zone)
	}
	if n.CPUCores != 40 {
		t.Fatalf("CPUCores = %d, want 40 (parsed from millicores)", n.CPUCores)
	}
	if n.GPUCapacity != 1 || n.GPUAllocatable != 1 {
		t.Fatalf("gpu cap/alloc = %d/%d, want 1/1", n.GPUCapacity, n.GPUAllocatable)
	}
	// 329974272Ki = 337893654528 bytes ≈ 314.7 GiB.
	if n.MemoryGiB < 314 || n.MemoryGiB > 315 {
		t.Fatalf("MemoryGiB = %v, want ~314.7", n.MemoryGiB)
	}
	if !n.Ready {
		t.Fatal("aks-h100pool-1 should be Ready")
	}
}

func TestBoardSKURollup(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: nodesJSON}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(snap.SKUs) != 2 {
		t.Fatalf("SKUs = %d, want 2", len(snap.SKUs))
	}
	// GPU SKU sorts first (2 GPUs > 0 GPUs).
	if snap.SKUs[0].SKU != "Standard_NC40ads_H100_v5" || snap.SKUs[0].Nodes != 2 || snap.SKUs[0].GPUs != 2 {
		t.Fatalf("SKUs[0] = %+v, want NC40ads_H100 ×2 with 2 GPUs", snap.SKUs[0])
	}
	if snap.SKUs[1].SKU != "Standard_D8s_v3" || snap.SKUs[1].GPUs != 0 {
		t.Fatalf("SKUs[1] = %+v, want D8s_v3 with 0 GPUs", snap.SKUs[1])
	}
}

func TestBoardEmptyIsNotError(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: `{"items":[]}`}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalNodes != 0 {
		t.Fatalf("TotalNodes = %d, want 0", snap.TotalNodes)
	}
	if snap.Nodes == nil || snap.SKUs == nil {
		t.Fatal("Nodes/SKUs must be non-nil so they serialize as []")
	}
}

func TestBoardUnknownSKU(t *testing.T) {
	// A node with no instance-type label rolls up under "unknown".
	const j = `{"items":[{"metadata":{"name":"x","labels":{}},
	  "status":{"capacity":{"cpu":"4","memory":"8Gi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`
	snap, err := Board(context.Background(), &fakeReader{json: j}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(snap.SKUs) != 1 || snap.SKUs[0].SKU != "unknown" {
		t.Fatalf("SKUs = %+v, want a single 'unknown'", snap.SKUs)
	}
	if snap.Nodes[0].SKU != "" {
		t.Fatalf("node SKU = %q, want empty (unknown is only the rollup label)", snap.Nodes[0].SKU)
	}
}

func TestBoardPropagatesError(t *testing.T) {
	sentinel := errors.New("api down")
	_, err := Board(context.Background(), &fakeReader{err: sentinel}, Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestBoardOnlyReadsDaemonSetsWhenExplicitlyIncluded(t *testing.T) {
	reader := &fakeReader{
		json:           nodesJSON,
		daemonSetsJSON: `{"items":[{"metadata":{"namespace":"gpu-monitoring","name":"gpu-monitoring-h100"},"status":{"desiredNumberScheduled":2,"numberReady":2,"numberAvailable":2}}]}`,
	}

	withoutDaemonSets, err := Board(context.Background(), reader, Options{})
	if err != nil {
		t.Fatalf("Board without DaemonSets: %v", err)
	}
	if reader.daemonSetsCallCount != 0 || len(withoutDaemonSets.DaemonSets) != 0 {
		t.Fatalf("default Board read daemonsets %d times and returned %+v", reader.daemonSetsCallCount, withoutDaemonSets.DaemonSets)
	}

	withDaemonSets, err := Board(context.Background(), reader, Options{IncludeDaemonSets: true})
	if err != nil {
		t.Fatalf("Board with DaemonSets: %v", err)
	}
	if reader.daemonSetsCallCount != 1 || len(withDaemonSets.DaemonSets) != 1 || !withDaemonSets.DaemonSets[0].Healthy {
		t.Fatalf("DaemonSet summary = %+v, calls = %d", withDaemonSets.DaemonSets, reader.daemonSetsCallCount)
	}
}

func TestBoardRejectsBadJSON(t *testing.T) {
	_, err := Board(context.Background(), &fakeReader{json: `not json`}, Options{})
	if err == nil {
		t.Fatal("want decode error on malformed nodes JSON")
	}
}

func TestQuantityHelpers(t *testing.T) {
	if got := quantityMilli("96000m"); got != 96000 {
		t.Fatalf("quantityMilli(96000m) = %d, want 96000", got)
	}
	if got := quantityMilli("40"); got != 40000 {
		t.Fatalf("quantityMilli(40) = %d, want 40000", got)
	}
	if got := quantityValue(""); got != 0 {
		t.Fatalf("quantityValue(empty) = %d, want 0", got)
	}
	if got := quantityValue("garbage"); got != 0 {
		t.Fatalf("quantityValue(garbage) = %d, want 0 (unparseable → 0)", got)
	}
}
