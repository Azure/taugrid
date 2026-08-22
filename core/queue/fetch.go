// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kueueapi"
	"github.com/Azure/taugrid/core/topology"
)

// BuildSnapshot is the pure parser/aggregator over one namespace's raw Kueue
// JSON. Callers own the kubectl/API reads so the same mapping serves both the
// CLI and the portal.
func BuildSnapshot(namespace string, pol topology.Policy, localQueuesRaw, clusterQueuesRaw, workloadsRaw []byte, opts Options) (Snapshot, error) {
	if strings.TrimSpace(namespace) == "" {
		return Snapshot{}, errors.New("queue snapshot namespace is required")
	}
	var lqs kueueapi.LocalQueueList
	if err := json.Unmarshal(localQueuesRaw, &lqs); err != nil {
		return Snapshot{}, fmt.Errorf("parse localqueues: %w", err)
	}
	var cqs kueueapi.ClusterQueueList
	if err := json.Unmarshal(clusterQueuesRaw, &cqs); err != nil {
		return Snapshot{}, fmt.Errorf("parse clusterqueues: %w", err)
	}
	var wls workloadList
	if err := json.Unmarshal(workloadsRaw, &wls); err != nil {
		return Snapshot{}, fmt.Errorf("parse workloads: %w", err)
	}

	localByName := map[string]kueueapi.LocalQueue{}
	for _, q := range lqs.Items {
		if q.Metadata.Namespace != "" && q.Metadata.Namespace != namespace {
			continue
		}
		localByName[q.Metadata.Name] = q
	}
	clusterByName := map[string]kueueapi.ClusterQueue{}
	for _, q := range cqs.Items {
		clusterByName[q.Metadata.Name] = q
	}

	groups, presetToKey, queueToKeys := groupsFromPolicy(pol, namespace, localByName, clusterByName)
	for _, w := range pendingWorkloads(wls) {
		if w.Namespace != "" && w.Namespace != namespace {
			continue
		}
		key, ok := keyForWorkload(w, presetToKey, queueToKeys, groups)
		if !ok {
			continue
		}
		g := groups[key]
		g.PendingWorkloads = append(g.PendingWorkloads, w)
		groups[key] = g
	}

	out := Snapshot{
		Namespace: namespace,
		Groups:    make([]Group, 0, len(groups)),
	}
	filter := normalizedFilter(opts)
	for _, g := range groups {
		sort.Strings(g.Presets)
		sort.Slice(g.PendingWorkloads, func(i, j int) bool {
			a, b := g.PendingWorkloads[i], g.PendingWorkloads[j]
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.Name < b.Name
		})
		if filter.matches(g) {
			out.Groups = append(out.Groups, g)
		}
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		a, b := out.Groups[i], out.Groups[j]
		for _, cmp := range [][2]string{
			{a.Namespace, b.Namespace},
			{a.Team, b.Team},
			{a.Lane, b.Lane},
			{a.GPUClass, b.GPUClass},
			{a.Queue, b.Queue},
			{a.ResourceFlavor, b.ResourceFlavor},
		} {
			if cmp[0] != cmp[1] {
				return cmp[0] < cmp[1]
			}
		}
		return false
	})
	out.Hints = queueHints(out.Groups)
	return out, nil
}

type groupKey struct {
	namespace, team, lane, gpuClass, queue, clusterQueue, resourceFlavor string
}

func groupsFromPolicy(pol topology.Policy, namespace string, localByName map[string]kueueapi.LocalQueue, clusterByName map[string]kueueapi.ClusterQueue) (map[groupKey]Group, map[string]groupKey, map[string][]groupKey) {
	groups := map[groupKey]Group{}
	presetToKey := map[string]groupKey{}
	queueToKeys := map[string][]groupKey{}
	for _, name := range pol.Names() {
		p := pol.Presets[name]
		if p.Disabled {
			continue
		}
		key := groupKey{
			namespace:      namespace,
			team:           p.Team,
			lane:           p.Lane,
			gpuClass:       p.GPUClass,
			queue:          p.QueueName,
			clusterQueue:   p.ClusterQueue,
			resourceFlavor: p.ResourceFlavor,
		}
		g := groups[key]
		if g.Team == "" {
			g = Group{
				Namespace:      namespace,
				Team:           p.Team,
				Lane:           p.Lane,
				GPUClass:       p.GPUClass,
				Queue:          p.QueueName,
				ClusterQueue:   p.ClusterQueue,
				ResourceFlavor: p.ResourceFlavor,
			}
			if q, ok := localByName[p.QueueName]; ok {
				g.QueueFound = true
				g.Pending = q.Status.PendingWorkloads
				g.Admitted = q.Status.AdmittedWorkloads
				g.Reserving = q.Status.ReservingWorkloads
				g.Conditions = append(g.Conditions, conditionsFrom(q.Status.Conditions)...)
			}
			if cq, ok := clusterByName[p.ClusterQueue]; ok {
				applyClusterQueueQuota(&g, cq, p.ResourceFlavor)
			}
		}
		g.Presets = append(g.Presets, p.Name)
		groups[key] = g
		presetToKey[p.Name] = key
		if !containsKey(queueToKeys[p.QueueName], key) {
			queueToKeys[p.QueueName] = append(queueToKeys[p.QueueName], key)
		}
	}
	return groups, presetToKey, queueToKeys
}

func applyClusterQueueQuota(g *Group, cq kueueapi.ClusterQueue, flavor string) {
	nominal, nominalOK := cq.NominalGPU(flavor)
	reserved, borrowed, reservationOK := cq.StatusGPU(cq.Status.FlavorsReservation, flavor)
	used, _, usageOK := cq.StatusGPU(cq.Status.FlavorsUsage, flavor)
	g.QuotaFound = nominalOK || reservationOK || usageOK
	g.GPUNominal = nominal
	g.GPUReserved = reserved
	g.GPUUsed = used
	g.GPUBorrowed = borrowed
	headroom := nominal - reserved
	if headroom < 0 {
		headroom = 0
	}
	g.GPUHeadroom = headroom
	g.Conditions = append(g.Conditions, conditionsFrom(cq.Status.Conditions)...)
}

func keyForWorkload(w PendingWorkload, presetToKey map[string]groupKey, queueToKeys map[string][]groupKey, groups map[groupKey]Group) (groupKey, bool) {
	if w.Preset != "" {
		if key, ok := presetToKey[w.Preset]; ok {
			return key, true
		}
	}
	keys := queueToKeys[w.Queue]
	for _, key := range keys {
		hasTopologyHint := w.Team != "" || w.Lane != "" || w.GPUClass != ""
		if !hasTopologyHint {
			continue
		}
		g := groups[key]
		if w.Team != "" && w.Team != g.Team {
			continue
		}
		if w.Lane != "" && w.Lane != g.Lane {
			continue
		}
		if w.GPUClass != "" && g.GPUClass != topology.GPUClassAny && w.GPUClass != g.GPUClass {
			continue
		}
		return key, true
	}
	if len(keys) == 1 {
		return keys[0], true
	}
	for _, key := range keys {
		g := groups[key]
		if g.GPUClass == topology.GPUClassAny && g.Lane == "training" {
			return key, true
		}
	}
	for _, key := range keys {
		if groups[key].GPUClass == topology.GPUClassAny {
			return key, true
		}
	}
	return groupKey{}, false
}

func pendingWorkloads(l workloadList) []PendingWorkload {
	out := []PendingWorkload{}
	for _, it := range l.Items {
		admitted, finished, reason, message := workloadConditions(it.Status.Conditions)
		if admitted || finished {
			continue
		}
		labels := it.Metadata.Labels
		gpuClass, _ := topology.NormalizeGPUClass(labels[topology.LabelGPUClass])
		out = append(out, PendingWorkload{
			Name:         it.Metadata.Name,
			Namespace:    it.Metadata.Namespace,
			Queue:        it.Spec.QueueName,
			ClusterQueue: it.Status.Admission.ClusterQueue,
			Team:         labels[topology.LabelTeam],
			Lane:         labels[topology.LabelLane],
			GPUClass:     gpuClass,
			Shape:        labels[topology.LabelShape],
			Preset:       labels[topology.LabelPreset],
			GPURequested: requestedGPU(it.Spec.PodSets),
			Reason:       reason,
			Message:      message,
			CreatedAt:    it.Metadata.CreationTimestamp,
		})
	}
	return out
}

func workloadConditions(conditions []kueueapi.Condition) (admitted, finished bool, reason, message string) {
	for _, c := range conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Finished":
			finished = true
		case "Admitted":
			admitted = true
		}
	}
	reason, message = kueueapi.PendingCause(conditions)
	return admitted, finished, reason, message
}

func requestedGPU(podSets []workloadPodSet) int64 {
	var total int64
	for _, ps := range podSets {
		count := ps.Count
		if count == 0 {
			count = 1
		}
		var perPod int64
		for _, c := range ps.Template.Spec.Containers {
			for _, name := range kueueapi.GPUResourceNames {
				if q, ok := c.Resources.Requests[name]; ok {
					perPod += q.Int64()
					continue
				}
				if q, ok := c.Resources.Limits[name]; ok {
					perPod += q.Int64()
				}
			}
		}
		total += int64(count) * perPod
	}
	return total
}

func queueHints(groups []Group) []string {
	byTeam := map[string][]Group{}
	for _, g := range groups {
		byTeam[g.Team] = append(byTeam[g.Team], g)
	}
	hints := []string{}
	for team, teamGroups := range byTeam {
		a100Blocked := false
		h200Headroom := false
		for _, g := range teamGroups {
			if strings.HasPrefix(g.GPUClass, "a100") && g.Lane == "training" && g.QueueFound && g.QuotaFound && g.Pending > 0 && g.GPUHeadroom == 0 {
				a100Blocked = true
			}
			if strings.HasPrefix(g.GPUClass, "h200") && g.Lane == "large-memory" && g.QuotaFound && g.GPUHeadroom > 0 {
				h200Headroom = true
			}
		}
		if a100Blocked && h200Headroom {
			hints = append(hints, fmt.Sprintf("%s: A100 training queue is saturated; H200 large-memory presets currently have quota headroom. Use only if the workload benefits from H200 memory/topology.", team))
		}
	}
	sort.Strings(hints)
	return hints
}

type filter struct {
	team, lane, gpuClass string
}

func normalizedFilter(o Options) filter {
	gpuClass, _ := topology.NormalizeGPUClass(o.GPUClass)
	return filter{
		team:     normalize(o.Team),
		lane:     normalize(o.Lane),
		gpuClass: gpuClass,
	}
}

func (f filter) matches(g Group) bool {
	if f.team != "" && f.team != g.Team {
		return false
	}
	if f.lane != "" && f.lane != g.Lane {
		return false
	}
	return f.gpuClass == "" || f.gpuClass == g.GPUClass
}

func normalize(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.ReplaceAll(v, "_", "-")
	v = strings.ReplaceAll(v, " ", "-")
	return v
}

func containsKey(keys []groupKey, want groupKey) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

func conditionsFrom(in []kueueapi.Condition) []Condition {
	out := make([]Condition, 0, len(in))
	for _, c := range in {
		out = append(out, Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message})
	}
	return out
}

// --- JSON shapes (subsets of Kueue resources) ---

type workloadList struct {
	Items []workloadItem `json:"items"`
}

type workloadItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		QueueName string           `json:"queueName"`
		PodSets   []workloadPodSet `json:"podSets"`
	} `json:"spec"`
	Status struct {
		Admission struct {
			ClusterQueue string `json:"clusterQueue"`
		} `json:"admission"`
		Conditions []kueueapi.Condition `json:"conditions"`
	} `json:"status"`
}

type workloadPodSet struct {
	Count    int `json:"count"`
	Template struct {
		Spec struct {
			Containers []struct {
				Resources struct {
					Requests map[string]kueueapi.Quantity `json:"requests"`
					Limits   map[string]kueueapi.Quantity `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	} `json:"template"`
}
