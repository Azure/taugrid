// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nodes builds the portal's Cluster Nodes board.
//
// Unlike the Cluster Health board (which reports per-GPU *runtime* metrics from
// Kusto: utilization, temperature, ECC errors), this board answers "what is the
// fleet made of" — the static hardware inventory: how many nodes, of which
// Azure SKU, in which agentpool, with how much CPU / memory / GPU capacity. It
// reads core v1 Node objects via internal/portal/kubeclient (client-go), so it
// shares the Jobs/Ray Kubernetes reader and needs no Kusto access.
//
// It mirrors what the gpudash TUI extracts per node:
// SKU from node.kubernetes.io/instance-type, pool from
// kubernetes.azure.com/agentpool, region/zone from topology.kubernetes.io/*,
// and CPU/memory/GPU from .status.capacity/.allocatable. Capacity quantities are
// parsed with k8s.io/apimachinery resource.Quantity (cpu "40"/"96000m" →
// millicores, memory "329974272Ki" → bytes).
//
// The board degrades gracefully: a portal without Kubernetes access disables it
// (the handler returns 503); an empty cluster is a normal empty board.
package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Label keys for the node attributes the board surfaces. Region/zone and SKU
// have legacy (beta) aliases still present on some AKS nodes, so each is
// resolved from a preference-ordered list.
const (
	gpuResourceKey     = "nvidia.com/gpu"
	rdmaResourcePrefix = "rdma/"

	labelAgentPool       = "kubernetes.azure.com/agentpool"
	labelAgentPoolLegacy = "agentpool"
	labelGPUProduct      = "nvidia.com/gpu.product"
)

var (
	skuLabels                 = []string{"node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type", "kubernetes.azure.com/sku"}
	regionLabels              = []string{"topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"}
	zoneLabels                = []string{"topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"}
	operationalConditionTypes = map[string]struct{}{
		"GPUECCDoubleRetired":      {},
		"GPUECCDoubleVolatile":     {},
		"GPUNVLinkCRCFlitErrors":   {},
		"GPUNVLinkCRCDataErrors":   {},
		"GPUNVLinkReplayErrors":    {},
		"GPUThermalViolation":      {},
		"GPUPowerViolation":        {},
		"GPUECCSingleVolatileRate": {},
		"GPUECCSingleRetired":      {},
		"GPUPCIeReplayErrors":      {},
		"IBLinkDown":               {},
		"IBSymbolError":            {},
	}
)

// Reader lists the raw Nodes JSON the board needs. kubeclient.Client satisfies
// this; tests supply a fake so no live API is required.
type Reader interface {
	// ListNodes returns the cluster-scoped core Nodes list as raw JSON.
	ListNodes(ctx context.Context) ([]byte, error)
	ListDaemonSets(ctx context.Context) ([]byte, error)
}

// Options controls optional infrastructure signals attached to the node
// inventory. DaemonSets are cluster-wide objects, so callers serving a
// workspace-scoped audience must opt in only after authorizing that scope.
type Options struct {
	IncludeDaemonSets bool
}

// Node is one node's static hardware inventory. CPU is reported in whole cores
// (from millicores, so a 40-core node reads 40, not 40000); Memory in bytes and
// a human GiB convenience; GPU counts are whole devices.
type Node struct {
	Name           string         `json:"name"`
	Ready          bool           `json:"ready"`
	AgentPool      string         `json:"agentPool,omitempty"`
	AgentPoolLabel string         `json:"agentPoolLabel,omitempty"`
	SKU            string         `json:"sku,omitempty"`
	GPUProduct     string         `json:"gpuProduct,omitempty"`
	Region         string         `json:"region,omitempty"`
	RegionLabel    string         `json:"regionLabel,omitempty"`
	Zone           string         `json:"zone,omitempty"`
	ZoneLabel      string         `json:"zoneLabel,omitempty"`
	CPUCores       int64          `json:"cpuCores"`
	MemoryBytes    int64          `json:"memoryBytes"`
	MemoryGiB      float64        `json:"memoryGiB"`
	GPUCapacity    int64          `json:"gpuCapacity"`
	GPUAllocatable int64          `json:"gpuAllocatable"`
	RDMAResources  []RDMAResource `json:"rdmaResources,omitempty"`
	Conditions     []Condition    `json:"operationalConditions,omitempty"`
}

// RDMAResource is one device-plugin resource advertised by a node. Presence
// establishes schedulable RDMA capability, not link health or validation.
type RDMAResource struct {
	Name        string `json:"name"`
	Capacity    int64  `json:"capacity"`
	Allocatable int64  `json:"allocatable"`
}

// Condition is one allowlisted GPU, NVLink, or InfiniBand Node condition. Raw
// status and timestamps are preserved so the Portal can fail closed on
// missing, stale, invalid, or future evidence instead of manufacturing health.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastHeartbeatTime  string `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// DaemonSet is a GPU/runtime-relevant DaemonSet. It is deliberately a compact
// readiness summary: detailed Pod inspection remains outside the Fleet view.
type DaemonSet struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Desired   int64  `json:"desired"`
	Ready     int64  `json:"ready"`
	Available int64  `json:"available"`
	Healthy   bool   `json:"healthy"`
}

// SKUCount is the node count and rolled-up GPU total for one Azure SKU, for the
// board's fleet-composition summary.
type SKUCount struct {
	SKU   string `json:"sku"`
	Nodes int    `json:"nodes"`
	GPUs  int64  `json:"gpus"`
}

// Snapshot is the Cluster Nodes board payload: per-node rows plus fleet rollups
// (totals and per-SKU counts).
type Snapshot struct {
	TotalNodes             int         `json:"totalNodes"`
	ReadyNodes             int         `json:"readyNodes"`
	GPUNodes               int         `json:"gpuNodes"`
	TotalCPUCores          int64       `json:"totalCPUCores"`
	TotalMemoryGiB         float64     `json:"totalMemoryGiB"`
	TotalGPUs              int64       `json:"totalGPUs"`
	RDMAAdvertisedGPUNodes int         `json:"rdmaAdvertisedGpuNodes"`
	SKUs                   []SKUCount  `json:"skus"`
	Nodes                  []Node      `json:"nodes"`
	DaemonSets             []DaemonSet `json:"daemonSets,omitempty"`
	DaemonSetsError        string      `json:"daemonSetsError,omitempty"`
}

// Board lists Nodes via the Reader and aggregates them into a Snapshot. An empty
// cluster is not an error — the board simply reports zero nodes.
func Board(ctx context.Context, r Reader, opts Options) (Snapshot, error) {
	raw, err := r.ListNodes(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list nodes: %w", err)
	}
	snap, err := aggregate(raw)
	if err != nil {
		return Snapshot{}, err
	}
	if !opts.IncludeDaemonSets {
		return snap, nil
	}
	dsRaw, err := r.ListDaemonSets(ctx)
	if err != nil {
		snap.DaemonSetsError = fmt.Sprintf("list daemonsets: %v", err)
		return snap, nil
	}
	snap.DaemonSets, err = daemonSets(dsRaw)
	if err != nil {
		snap.DaemonSetsError = err.Error()
	}
	return snap, nil
}

var runtimeDaemonSetHints = []string{"gpu", "nvidia", "dcgm", "node-problem", "metrics-collector", "node-exporter"}

func daemonSets(data []byte) ([]DaemonSet, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Desired   int64 `json:"desiredNumberScheduled"`
				Ready     int64 `json:"numberReady"`
				Available int64 `json:"numberAvailable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode daemonsets: %w", err)
	}
	out := []DaemonSet{}
	for _, item := range list.Items {
		name := strings.ToLower(item.Metadata.Namespace + "/" + item.Metadata.Name)
		matched := false
		for _, hint := range runtimeDaemonSetHints {
			if strings.Contains(name, hint) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// A DaemonSet intentionally scheduled on no nodes is not a readiness
		// problem and adds noise to the operator-facing runtime summary.
		if item.Status.Desired == 0 {
			continue
		}
		out = append(out, DaemonSet{Namespace: item.Metadata.Namespace, Name: item.Metadata.Name, Desired: item.Status.Desired, Ready: item.Status.Ready, Available: item.Status.Available, Healthy: item.Status.Ready >= item.Status.Desired})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name })
	return out, nil
}

// nodeList is the subset of the core v1 Node list the board reads.
type nodeList struct {
	Items []nodeObj `json:"items"`
}

type nodeObj struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Capacity    map[string]string `json:"capacity"`
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			Reason             string `json:"reason"`
			Message            string `json:"message"`
			LastHeartbeatTime  string `json:"lastHeartbeatTime"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// aggregate folds the Node list into a Snapshot: one row per node, plus fleet
// totals and per-SKU rollups. The per-node rows are sorted by name; the SKU
// rollup by GPU count then node count (descending) so the biggest GPU pools lead.
func aggregate(data []byte) (Snapshot, error) {
	var list nodeList
	if err := json.Unmarshal(data, &list); err != nil {
		return Snapshot{}, fmt.Errorf("decode nodes: %w", err)
	}
	snap := Snapshot{Nodes: make([]Node, 0, len(list.Items)), SKUs: []SKUCount{}}
	skuIndex := map[string]int{}
	for _, obj := range list.Items {
		n := parseNode(obj)
		snap.Nodes = append(snap.Nodes, n)
		snap.TotalNodes++
		if n.Ready {
			snap.ReadyNodes++
		}
		snap.TotalCPUCores += n.CPUCores
		snap.TotalMemoryGiB += n.MemoryGiB
		snap.TotalGPUs += n.GPUCapacity
		if n.GPUCapacity > 0 {
			snap.GPUNodes++
		}
		if n.GPUCapacity > 0 && len(n.RDMAResources) > 0 {
			snap.RDMAAdvertisedGPUNodes++
		}
		sku := n.SKU
		if sku == "" {
			sku = "unknown"
		}
		if idx, ok := skuIndex[sku]; ok {
			snap.SKUs[idx].Nodes++
			snap.SKUs[idx].GPUs += n.GPUCapacity
		} else {
			skuIndex[sku] = len(snap.SKUs)
			snap.SKUs = append(snap.SKUs, SKUCount{SKU: sku, Nodes: 1, GPUs: n.GPUCapacity})
		}
	}
	// Round the fleet memory total to one decimal to avoid float noise in JSON.
	snap.TotalMemoryGiB = round1(snap.TotalMemoryGiB)
	sort.SliceStable(snap.Nodes, func(i, j int) bool {
		return snap.Nodes[i].Name < snap.Nodes[j].Name
	})
	sort.SliceStable(snap.SKUs, func(i, j int) bool {
		if snap.SKUs[i].GPUs != snap.SKUs[j].GPUs {
			return snap.SKUs[i].GPUs > snap.SKUs[j].GPUs
		}
		if snap.SKUs[i].Nodes != snap.SKUs[j].Nodes {
			return snap.SKUs[i].Nodes > snap.SKUs[j].Nodes
		}
		return snap.SKUs[i].SKU < snap.SKUs[j].SKU
	})
	return snap, nil
}

// parseNode reads one Node object into a Node row.
func parseNode(obj nodeObj) Node {
	labels := obj.Metadata.Labels
	milli := quantityMilli(obj.Status.Capacity["cpu"])
	memBytes := quantityValue(obj.Status.Capacity["memory"])
	agentPoolLabel, agentPool := firstLabelWithKey(labels, labelAgentPool, labelAgentPoolLegacy)
	regionLabel, region := firstLabelWithKey(labels, regionLabels...)
	zoneLabel, zone := firstLabelWithKey(labels, zoneLabels...)
	return Node{
		Name:           obj.Metadata.Name,
		Ready:          isReady(obj),
		AgentPool:      agentPool,
		AgentPoolLabel: agentPoolLabel,
		SKU:            firstLabel(labels, skuLabels...),
		GPUProduct:     labels[labelGPUProduct],
		Region:         region,
		RegionLabel:    regionLabel,
		Zone:           zone,
		ZoneLabel:      zoneLabel,
		CPUCores:       milli / 1000,
		MemoryBytes:    memBytes,
		MemoryGiB:      round1(float64(memBytes) / (1024 * 1024 * 1024)),
		GPUCapacity:    quantityValue(obj.Status.Capacity[gpuResourceKey]),
		GPUAllocatable: quantityValue(obj.Status.Allocatable[gpuResourceKey]),
		RDMAResources:  rdmaResources(obj.Status.Capacity, obj.Status.Allocatable),
		Conditions:     operationalConditions(obj),
	}
}

func operationalConditions(obj nodeObj) []Condition {
	out := make([]Condition, 0, len(obj.Status.Conditions))
	for _, condition := range obj.Status.Conditions {
		if _, ok := operationalConditionTypes[condition.Type]; !ok {
			continue
		}
		out = append(out, Condition{
			Type: condition.Type, Status: condition.Status,
			Reason: condition.Reason, Message: condition.Message,
			LastHeartbeatTime:  normalizeTimestamp(condition.LastHeartbeatTime),
			LastTransitionTime: normalizeTimestamp(condition.LastTransitionTime),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

func normalizeTimestamp(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}

func rdmaResources(capacity, allocatable map[string]string) []RDMAResource {
	names := make(map[string]struct{})
	for name := range capacity {
		if strings.HasPrefix(strings.ToLower(name), rdmaResourcePrefix) {
			names[name] = struct{}{}
		}
	}
	for name := range allocatable {
		if strings.HasPrefix(strings.ToLower(name), rdmaResourcePrefix) {
			names[name] = struct{}{}
		}
	}
	out := make([]RDMAResource, 0, len(names))
	for name := range names {
		resource := RDMAResource{
			Name: name, Capacity: quantityValue(capacity[name]), Allocatable: quantityValue(allocatable[name]),
		}
		if resource.Capacity > 0 || resource.Allocatable > 0 {
			out = append(out, resource)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// isReady reports whether the node's Ready condition is True.
func isReady(obj nodeObj) bool {
	for _, c := range obj.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// quantityValue parses a Kubernetes resource quantity to its integer base-unit
// value (bytes for memory, whole devices for gpu). An empty or unparseable
// quantity is 0, so a node that reports no GPU capacity reads 0, not an error.
func quantityValue(s string) int64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

// quantityMilli parses a quantity to millis (cpu "40" → 40000, "96000m" →
// 96000), so whole cores are milli/1000 without losing fractional-core SKUs.
func quantityMilli(s string) int64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

// firstLabel returns the first non-empty value among the given label keys.
func firstLabel(labels map[string]string, keys ...string) string {
	_, value := firstLabelWithKey(labels, keys...)
	return value
}

func firstLabelWithKey(labels map[string]string, keys ...string) (string, string) {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return k, v
		}
	}
	return "", ""
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
