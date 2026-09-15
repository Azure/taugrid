// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodes

import (
	"context"
	"errors"
	"strings"
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
	podsJSON            string
	podsErr             error
	podsCallCount       int
	nodeMetricsJSON     string
	nodeMetricsErr      error
	nodeMetricsCalls    int
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
func (f *fakeReader) ListPods(_ context.Context, _ string) ([]byte, error) {
	f.podsCallCount++
	if f.podsErr != nil {
		return nil, f.podsErr
	}
	if f.podsJSON != "" {
		return []byte(f.podsJSON), nil
	}
	return []byte(`{"items":[]}`), nil
}
func (f *fakeReader) ListNodeMetrics(_ context.Context) ([]byte, error) {
	f.nodeMetricsCalls++
	if f.nodeMetricsErr != nil {
		return nil, f.nodeMetricsErr
	}
	if f.nodeMetricsJSON != "" {
		return []byte(f.nodeMetricsJSON), nil
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
      "unbounded-cloud.io/site":"cluster",
      "topology.kubernetes.io/region":"westeurope",
      "topology.kubernetes.io/zone":"westeurope-0"}},
   "status":{"capacity":{"cpu":"40","memory":"329974272Ki","nvidia.com/gpu":"1","rdma/rdma_shared_device_a":"1"},
     "allocatable":{"nvidia.com/gpu":"1","rdma/rdma_shared_device_a":"1"},
     "conditions":[
       {"type":"MemoryPressure","status":"False"},
       {"type":"GPUNVLinkCRCDataErrors","status":"False","reason":"GPUNVLinkCRCDataErrorsOk","lastHeartbeatTime":"2026-09-14T20:03:00Z","lastTransitionTime":"2026-09-11T02:51:06Z"},
       {"type":"IBLinkDown","status":"Unknown","reason":"IBLinkDownMetricCoverageUnknown","message":"required metric coverage is incomplete","lastHeartbeatTime":"2026-09-14T20:03:01+00:00"},
       {"type":"Ready","status":"True"}]}},
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
	if snap.GPUAllocatable != 2 {
		t.Fatalf("GPUAllocatable = %d, want 2", snap.GPUAllocatable)
	}
	if snap.GPUSchedulable != 1 {
		t.Fatalf("GPUSchedulable = %d, want 1 (the second GPU node is NotReady)", snap.GPUSchedulable)
	}
	if snap.RDMAAdvertisedGPUNodes != 1 {
		t.Fatalf("RDMAAdvertisedGPUNodes = %d, want 1", snap.RDMAAdvertisedGPUNodes)
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
	if n.Site != "cluster" || n.SiteLabel != labelUnboundedSite {
		t.Fatalf("site/source = %q/%q, want cluster/%s", n.Site, n.SiteLabel, labelUnboundedSite)
	}
	if n.CPUCores != 40 {
		t.Fatalf("CPUCores = %d, want 40 (parsed from millicores)", n.CPUCores)
	}
	if n.GPUCapacity != 1 || n.GPUAllocatable != 1 {
		t.Fatalf("gpu cap/alloc = %d/%d, want 1/1", n.GPUCapacity, n.GPUAllocatable)
	}
	if n.AgentPoolLabel != labelAgentPool ||
		n.RegionLabel != "topology.kubernetes.io/region" ||
		n.ZoneLabel != "topology.kubernetes.io/zone" {
		t.Fatalf("location label sources = pool %q region %q zone %q", n.AgentPoolLabel, n.RegionLabel, n.ZoneLabel)
	}
	if len(n.RDMAResources) != 1 ||
		n.RDMAResources[0].Name != "rdma/rdma_shared_device_a" ||
		n.RDMAResources[0].Capacity != 1 ||
		n.RDMAResources[0].Allocatable != 1 {
		t.Fatalf("RDMA resources = %+v", n.RDMAResources)
	}
	if len(n.Conditions) != 2 ||
		n.Conditions[0].Type != "GPUNVLinkCRCDataErrors" ||
		n.Conditions[0].Status != "False" ||
		n.Conditions[0].LastHeartbeatTime != "2026-09-14T20:03:00Z" ||
		n.Conditions[1].Type != "IBLinkDown" ||
		n.Conditions[1].Status != "Unknown" ||
		n.Conditions[1].Message != "required metric coverage is incomplete" ||
		n.Conditions[1].LastHeartbeatTime != "2026-09-14T20:03:01Z" {
		t.Fatalf("operational conditions = %+v", n.Conditions)
	}
	// 329974272Ki = 337893654528 bytes ≈ 314.7 GiB.
	if n.MemoryGiB < 314 || n.MemoryGiB > 315 {
		t.Fatalf("MemoryGiB = %v, want ~314.7", n.MemoryGiB)
	}
	if !n.Ready {
		t.Fatal("aks-h100pool-1 should be Ready")
	}
	if !n.Schedulable {
		t.Fatal("aks-h100pool-1 should be schedulable")
	}
}

func TestBoardResolvesExactUnboundedSiteLabels(t *testing.T) {
	const j = `{"items":[
	  {"metadata":{"name":"canonical","labels":{
	    "unbounded-cloud.io/site":"canonical-site",
	    "net.unbounded-cloud.io/site":"legacy-site"}},
	   "status":{"capacity":{"cpu":"1","memory":"1Gi","nvidia.com/gpu":"1"}}},
	  {"metadata":{"name":"legacy","labels":{
	    "net.unbounded-cloud.io/site":"legacy-site"}},
	   "status":{"capacity":{"cpu":"1","memory":"1Gi","nvidia.com/gpu":"1"}}},
	  {"metadata":{"name":"lookalike","labels":{
	    "example.com/unbounded-site":"wrong-site"}},
	   "status":{"capacity":{"cpu":"1","memory":"1Gi","nvidia.com/gpu":"1"}}}
	]}`
	snap, err := Board(context.Background(), &fakeReader{json: j}, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if got := snap.Nodes[0]; got.Site != "canonical-site" || got.SiteLabel != labelUnboundedSite {
		t.Fatalf("canonical site = %q/%q, want canonical-site/%s", got.Site, got.SiteLabel, labelUnboundedSite)
	} else if !got.SiteLabelConflict {
		t.Fatal("canonical and legacy disagreement must be exposed as a conflict")
	}
	if got := snap.Nodes[1]; got.Site != "legacy-site" || got.SiteLabel != labelUnboundedLegacy {
		t.Fatalf("legacy site = %q/%q, want legacy-site/%s", got.Site, got.SiteLabel, labelUnboundedLegacy)
	} else if got.SiteLabelConflict {
		t.Fatal("legacy-only site must not report a conflict")
	}
	if got := snap.Nodes[2]; got.Site != "" || got.SiteLabel != "" {
		t.Fatalf("lookalike site = %q/%q, want empty/empty", got.Site, got.SiteLabel)
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

func TestBoardCountsActiveScheduledGPUAssignments(t *testing.T) {
	const gpuNodeJSON = `{"items":[
	  {"metadata":{"name":"gpu-node","labels":{}},
	   "status":{"capacity":{"cpu":"8","memory":"64Gi","nvidia.com/gpu":"4"},
	     "allocatable":{"nvidia.com/gpu":"4"},
	     "conditions":[{"type":"Ready","status":"True"}]}}
	]}`
	const podsJSON = `{"items":[
	  {"spec":{"nodeName":"gpu-node",
	      "containers":[
	        {"resources":{"requests":{"nvidia.com/gpu":"1"}}},
	        {"resources":{"requests":{"nvidia.com/gpu":"1"}}}],
	      "initContainers":[
	        {"restartPolicy":"Always","resources":{"requests":{"nvidia.com/gpu":"1"}}},
	        {"resources":{"requests":{"nvidia.com/gpu":"3"}}},
	        {"resources":{"requests":{"nvidia.com/gpu":"1"}}}]},
	   "status":{"phase":"Running"}},
	  {"spec":{"containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]},
	   "status":{"phase":"Pending"}},
	  {"spec":{"nodeName":"gpu-node","containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]},
	   "status":{"phase":"Succeeded"}},
	  {"spec":{"nodeName":"gpu-node","containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]},
	   "status":{"phase":"Failed"}}
	]}`
	reader := &fakeReader{json: gpuNodeJSON, podsJSON: podsJSON}

	snap, err := Board(context.Background(), reader, Options{IncludeAllocations: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if reader.podsCallCount != 1 {
		t.Fatalf("ListPods calls = %d, want 1", reader.podsCallCount)
	}
	if !snap.GPUAllocationKnown || snap.GPUAllocated != 4 || snap.GPUAvailable != 0 {
		t.Fatalf("GPU allocation = known %t, allocated %d, available %d; want true/4/0",
			snap.GPUAllocationKnown, snap.GPUAllocated, snap.GPUAvailable)
	}
	if snap.Nodes[0].GPUAllocated == nil || *snap.Nodes[0].GPUAllocated != 4 ||
		snap.Nodes[0].GPUAvailable == nil || *snap.Nodes[0].GPUAvailable != 0 {
		t.Fatalf("node allocation = allocated %v, available %v; want 4/0",
			snap.Nodes[0].GPUAllocated, snap.Nodes[0].GPUAvailable)
	}
}

func TestBoardDoesNotCountUnavailableNodesAsFree(t *testing.T) {
	snap, err := Board(context.Background(), &fakeReader{json: nodesJSON}, Options{IncludeAllocations: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if !snap.GPUAllocationKnown || snap.GPUSchedulable != 1 || snap.GPUAvailable != 1 {
		t.Fatalf("GPU availability = known %t, schedulable %d, available %d; want true/1/1",
			snap.GPUAllocationKnown, snap.GPUSchedulable, snap.GPUAvailable)
	}
	for _, node := range snap.Nodes {
		if node.Name == "aks-h100pool-2" {
			if node.Schedulable || node.GPUAvailable == nil || *node.GPUAvailable != 0 {
				t.Fatalf("NotReady node = schedulable %t, available %v; want false/0", node.Schedulable, node.GPUAvailable)
			}
			return
		}
	}
	t.Fatal("NotReady GPU node not found")
}

func TestBoardDoesNotCountCordonedNodesAsFree(t *testing.T) {
	const cordonedNodeJSON = `{"items":[
	  {"metadata":{"name":"cordoned","labels":{}},"spec":{"unschedulable":true},
	   "status":{"capacity":{"cpu":"8","memory":"64Gi","nvidia.com/gpu":"1"},
	     "allocatable":{"nvidia.com/gpu":"1"},
	     "conditions":[{"type":"Ready","status":"True"}]}}
	]}`
	snap, err := Board(context.Background(), &fakeReader{json: cordonedNodeJSON}, Options{IncludeAllocations: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if !snap.GPUAllocationKnown || snap.GPUSchedulable != 0 || snap.GPUAvailable != 0 ||
		snap.Nodes[0].Schedulable || snap.Nodes[0].GPUAvailable == nil || *snap.Nodes[0].GPUAvailable != 0 {
		t.Fatalf("cordoned allocation snapshot = %+v, node = %+v; want known with zero schedulable/free GPUs", snap, snap.Nodes[0])
	}
}

func TestBoardFailsClosedForUnsupportedGPUAssignmentTypes(t *testing.T) {
	const podsJSON = `{"items":[
	  {"spec":{"nodeName":"aks-h100pool-1","containers":[{"resources":{"requests":{"nvidia.com/mig-1g.10gb":"1"}}}]},"status":{"phase":"Running"}}
	]}`
	snap, err := Board(context.Background(), &fakeReader{json: nodesJSON, podsJSON: podsJSON}, Options{IncludeAllocations: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.GPUAllocationKnown || snap.GPUAllocationError == "" {
		t.Fatalf("GPU allocation = known %t, error %q; want unknown with error", snap.GPUAllocationKnown, snap.GPUAllocationError)
	}
}

func TestBoardPreservesInventoryWhenGPUAssignmentsAreUnavailable(t *testing.T) {
	reader := &fakeReader{json: nodesJSON, podsErr: errors.New("forbidden")}
	snap, err := Board(context.Background(), reader, Options{IncludeAllocations: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalNodes != 3 || snap.GPUAllocatable != 2 {
		t.Fatalf("inventory totals = nodes %d, allocatable %d; want 3/2", snap.TotalNodes, snap.GPUAllocatable)
	}
	if snap.GPUAllocationKnown || snap.GPUAvailable != 0 || snap.GPUAllocationError == "" {
		t.Fatalf("GPU allocation = known %t, available %d, error %q; want unknown with error",
			snap.GPUAllocationKnown, snap.GPUAvailable, snap.GPUAllocationError)
	}
	for _, node := range snap.Nodes {
		if node.GPUAllocated != nil || node.GPUAvailable != nil {
			t.Fatalf("node %q unexpectedly has allocation values: allocated %v, available %v",
				node.Name, node.GPUAllocated, node.GPUAvailable)
		}
	}
}

func TestBoardOnlyReadsPodsWhenExplicitlyIncluded(t *testing.T) {
	reader := &fakeReader{json: nodesJSON}
	if _, err := Board(context.Background(), reader, Options{}); err != nil {
		t.Fatalf("Board: %v", err)
	}
	if reader.podsCallCount != 0 {
		t.Fatalf("ListPods calls = %d, want 0", reader.podsCallCount)
	}
}

func TestBoardAttachesCurrentNodeMetrics(t *testing.T) {
	const metricsJSON = `{"items":[
	  {"metadata":{"name":"aks-h100pool-1"},"timestamp":"2026-09-15T20:00:00.123456789Z","window":"15.001s",
	   "usage":{"cpu":"2","memory":"164987136Ki"}},
	  {"metadata":{"name":"stale-node"},"timestamp":"2026-09-15T20:00:00Z","window":"15s",
	   "usage":{"cpu":"99","memory":"99Gi"}}
	]}`
	reader := &fakeReader{json: nodesJSON, nodeMetricsJSON: metricsJSON}
	snap, err := Board(context.Background(), reader, Options{IncludeMetrics: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if reader.nodeMetricsCalls != 1 {
		t.Fatalf("ListNodeMetrics calls = %d, want 1", reader.nodeMetricsCalls)
	}
	node := snap.Nodes[0]
	if node.CPUUtilPct == nil || *node.CPUUtilPct != 5 {
		t.Fatalf("CPUUtilPct = %v, want 5", node.CPUUtilPct)
	}
	if node.MemoryUsedPct == nil || *node.MemoryUsedPct != 50 {
		t.Fatalf("MemoryUsedPct = %v, want 50", node.MemoryUsedPct)
	}
	if node.MetricsObservedAt != "2026-09-15T20:00:00.123456789Z" || node.MetricsWindow != "15.001s" {
		t.Fatalf("metrics evidence = %q / %q", node.MetricsObservedAt, node.MetricsWindow)
	}
	if snap.NodeMetricsError != "" {
		t.Fatalf("NodeMetricsError = %q, want empty", snap.NodeMetricsError)
	}
}

func TestBoardPreservesInventoryWhenNodeMetricsAreUnavailable(t *testing.T) {
	reader := &fakeReader{json: nodesJSON, nodeMetricsErr: errors.New("forbidden")}
	snap, err := Board(context.Background(), reader, Options{IncludeMetrics: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalNodes != 3 || snap.NodeMetricsError == "" {
		t.Fatalf("snapshot totals/error = %d/%q, want inventory plus explicit metrics error",
			snap.TotalNodes, snap.NodeMetricsError)
	}
	for _, node := range snap.Nodes {
		if node.CPUUtilPct != nil || node.MemoryUsedPct != nil {
			t.Fatalf("node %q unexpectedly has metrics: %+v", node.Name, node)
		}
	}
}

func TestBoardKeepsValidPartialNodeMetrics(t *testing.T) {
	const metricsJSON = `{"items":[
	  {"metadata":{"name":"aks-h100pool-1"},"timestamp":"2026-09-15T20:00:00Z","window":"15s",
	   "usage":{"cpu":"bad","memory":"164987136Ki"}},
	  {"metadata":{"name":"aks-h100pool-2"},"timestamp":"bad","window":"15s",
	   "usage":{"cpu":"1","memory":"1Gi"}}
	]}`
	snap, err := Board(context.Background(), &fakeReader{json: nodesJSON, nodeMetricsJSON: metricsJSON}, Options{IncludeMetrics: true})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	first := snap.Nodes[0]
	if first.CPUUtilPct != nil || first.MemoryUsedPct == nil || *first.MemoryUsedPct != 50 {
		t.Fatalf("partial metrics = CPU %v memory %v, want nil/50", first.CPUUtilPct, first.MemoryUsedPct)
	}
	if snap.NodeMetricsError == "" ||
		!strings.Contains(snap.NodeMetricsError, "aks-h100pool-1 has invalid CPU usage") ||
		!strings.Contains(snap.NodeMetricsError, "aks-h100pool-2 has invalid timestamp or window") {
		t.Fatalf("NodeMetricsError = %q, want both malformed sample diagnostics", snap.NodeMetricsError)
	}
}

func TestBoardOnlyReadsNodeMetricsWhenExplicitlyIncluded(t *testing.T) {
	reader := &fakeReader{json: nodesJSON}
	if _, err := Board(context.Background(), reader, Options{}); err != nil {
		t.Fatalf("Board: %v", err)
	}
	if reader.nodeMetricsCalls != 0 {
		t.Fatalf("ListNodeMetrics calls = %d, want 0", reader.nodeMetricsCalls)
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
