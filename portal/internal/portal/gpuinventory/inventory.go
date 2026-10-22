// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpuinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

type podList struct {
	Items []podObject `json:"items"`
}

type podObject struct {
	Metadata objectMetadata `json:"metadata"`
	Spec     struct {
		Containers     []podContainer    `json:"containers"`
		InitContainers []podContainer    `json:"initContainers"`
		Overhead       map[string]string `json:"overhead"`
		ResourceClaims []json.RawMessage `json:"resourceClaims"`
		NodeName       string            `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type podContainer struct {
	RestartPolicy string `json:"restartPolicy"`
	Resources     struct {
		Requests map[string]string `json:"requests"`
		Limits   map[string]string `json:"limits"`
		Claims   []json.RawMessage `json:"claims"`
	} `json:"resources"`
}

type workloadAccumulator struct {
	workload Workload
	total    quantityMap
	pending  quantityMap
	bound    quantityMap
	nodes    map[string]struct{}
}

func Inventory(ctx context.Context, reader Reader, opts Options) (Snapshot, error) {
	if reader == nil {
		return Snapshot{}, errors.New("GPU inventory reader is not configured")
	}
	namespace := strings.TrimSpace(opts.Namespace)
	localQueue := strings.TrimSpace(opts.LocalQueue)
	if opts.ManagedOnly && (namespace == "" || localQueue == "") {
		return Snapshot{}, errors.New("workspace GPU inventory requires namespace and LocalQueue")
	}
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	maxWorkloads := opts.MaxWorkloads
	if maxWorkloads <= 0 {
		maxWorkloads = 1000
	}

	podsRaw, err := reader.ListPods(ctx, namespace)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list Pods for GPU inventory: %w", err)
	}
	var pods podList
	if err := json.Unmarshal(podsRaw, &pods); err != nil {
		return Snapshot{}, errors.New("decode Pods for GPU inventory")
	}

	owners, sourceDiagnostics, partial := loadOwnerIndex(ctx, reader, namespace)
	owners.indexWorkloadAttribution()
	snapshot := Snapshot{
		GeneratedAt: now,
		Workloads:   []Workload{},
		Sources: append([]SourceDiagnostic{{
			Source: "pods",
			State:  "ready",
		}}, sourceDiagnostics...),
		Partial: partial,
	}
	groups := map[string]*workloadAccumulator{}
	for _, pod := range pods.Items {
		if namespace != "" && pod.Metadata.Namespace != "" && pod.Metadata.Namespace != namespace {
			continue
		}
		phase := strings.TrimSpace(pod.Status.Phase)
		if phase != "Pending" && phase != "Running" {
			continue
		}
		resources := calculatePodResources(pod)
		if len(resources.total) == 0 && !resources.countUnknown {
			continue
		}
		resolved := owners.resolve(pod)
		attrs := owners.attribution(pod.Metadata.Namespace, resolved)
		if opts.ManagedOnly && (!attrs.managed || attrs.queue != localQueue) {
			continue
		}
		key := groupKey(pod, resolved.owner)
		group := groups[key]
		if group == nil {
			group = &workloadAccumulator{
				workload: Workload{
					Namespace:        pod.Metadata.Namespace,
					Owner:            resolved.owner,
					Managed:          attrs.managed,
					Queue:            attrs.queue,
					Workspace:        attrs.workspace,
					Team:             attrs.team,
					RunID:            attrs.runID,
					Lane:             attrs.lane,
					Profile:          attrs.profile,
					GPUClass:         attrs.gpuClass,
					Nodes:            []string{},
					Resources:        []ResourceQuantity{},
					PendingResources: []ResourceQuantity{},
					BoundResources:   []ResourceQuantity{},
					Diagnostics:      []Diagnostic{},
				},
				total:   quantityMap{},
				pending: quantityMap{},
				bound:   quantityMap{},
				nodes:   map[string]struct{}{},
			}
			if attrs.managed && (resolved.owner.Kind == "Job" || resolved.owner.Kind == "RayJob") && resolved.owner.UID != "" {
				group.workload.Detail = &DetailReference{
					ResourceUID: resolved.owner.UID,
					Path:        "/portal/workloads/" + url.PathEscape(resolved.owner.UID),
				}
			}
			groups[key] = group
		}
		group.workload.PodCount++
		if podReady(pod) {
			group.workload.ReadyPods++
		}
		if phase == "Pending" {
			group.workload.Phases.Pending++
		} else {
			group.workload.Phases.Running++
		}
		if strings.TrimSpace(pod.Spec.NodeName) == "" {
			group.workload.PendingPods++
		} else {
			group.workload.BoundPods++
			group.nodes[pod.Spec.NodeName] = struct{}{}
		}
		if resources.countUnknown {
			group.workload.CountUnknown = true
			group.workload.CountUnknownPods++
		}
		for _, diagnostic := range resources.diagnostics {
			group.workload.Diagnostics = appendDiagnostic(group.workload.Diagnostics, diagnostic)
		}
		if resolved.diagnostic != nil {
			group.workload.Diagnostics = appendDiagnostic(group.workload.Diagnostics, *resolved.diagnostic)
		}
		addQuantities(group.total, resources.total)
		addQuantities(group.pending, resources.pending)
		addQuantities(group.bound, resources.bound)
	}

	values := make([]Workload, 0, len(groups))
	for _, group := range groups {
		group.workload.Resources = quantityList(group.total)
		group.workload.PendingResources = quantityList(group.pending)
		group.workload.BoundResources = quantityList(group.bound)
		group.workload.RequestedFullGPUs = wholeGPUQuantity(group.total)
		group.workload.PendingFullGPUs = wholeGPUQuantity(group.pending)
		group.workload.BoundFullGPUs = wholeGPUQuantity(group.bound)
		for node := range group.nodes {
			group.workload.Nodes = append(group.workload.Nodes, node)
		}
		sort.Strings(group.workload.Nodes)
		sort.Slice(group.workload.Diagnostics, func(i, j int) bool {
			return group.workload.Diagnostics[i].Code < group.workload.Diagnostics[j].Code
		})
		values = append(values, group.workload)
	}
	sort.Slice(values, func(i, j int) bool {
		a, b := values[i], values[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Owner.Kind != b.Owner.Kind {
			return a.Owner.Kind < b.Owner.Kind
		}
		if a.Owner.Name != b.Owner.Name {
			return a.Owner.Name < b.Owner.Name
		}
		return a.Owner.UID < b.Owner.UID
	})
	if len(values) > maxWorkloads {
		values = values[:maxWorkloads]
		snapshot.Truncated = true
	}
	snapshot.Workloads = values
	snapshot.Summary = summarize(values)
	return snapshot, nil
}

func summarize(workloads []Workload) Summary {
	out := Summary{Workloads: len(workloads)}
	for _, workload := range workloads {
		out.Pods += workload.PodCount
		out.ReadyPods += workload.ReadyPods
		out.PendingPods += workload.PendingPods
		out.BoundPods += workload.BoundPods
		out.RequestedFullGPUs += workload.RequestedFullGPUs
		out.PendingFullGPUs += workload.PendingFullGPUs
		out.BoundFullGPUs += workload.BoundFullGPUs
		out.CountUnknownPods += workload.CountUnknownPods
	}
	return out
}

func groupKey(pod podObject, owner Owner) string {
	if owner.UID != "" {
		return pod.Metadata.Namespace + "\x00" + owner.Kind + "\x00" + owner.UID
	}
	return pod.Metadata.Namespace + "\x00Pod\x00" + pod.Metadata.UID
}

func podReady(pod podObject) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return true
		}
	}
	return false
}
