// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ray

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/portal/internal/portal/links"
)

const (
	historyKind             = "RayJob"
	historyStateAvailable   = "available"
	historyStateLiveOnly    = "live-only"
	historyStateUnavailable = "history-unavailable"
)

const clusterLabel = "ray.io/cluster"

const (
	nodeTypeLabel = "ray.io/node-type"
	nodeTypeHead  = "head"
)

type Reader interface {
	ListServices(ctx context.Context, namespace string) ([]byte, error)
	ListPods(ctx context.Context, namespace string) ([]byte, error)
}

type Options struct {
	Namespace    string
	History      runs.HistoryReader
	HistoryScope runs.HistoryScope
}

type Cluster struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Type      string `json:"type,omitempty"`
	ProxyPath string `json:"proxyPath"`
	Available bool   `json:"available"`
}

type Snapshot struct {
	Namespace         string     `json:"namespace,omitempty"`
	Total             int        `json:"total"`
	Clusters          []Cluster  `json:"clusters"`
	History           []runs.Run `json:"history,omitempty"`
	HistoryState      string     `json:"historyState"`
	HistoryDiagnostic string     `json:"historyDiagnostic,omitempty"`
}

func Board(ctx context.Context, r Reader, opts Options) (Snapshot, error) {
	raw, err := r.ListServices(ctx, opts.Namespace)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list services: %w", err)
	}
	clusters, err := parseServices(raw)
	if err != nil {
		return Snapshot{}, err
	}
	ready := readyHeadClusters(ctx, r, opts.Namespace)
	for i := range clusters {
		clusters[i].Available = ready == nil || ready[clusters[i].Namespace+"/"+clusters[i].Name]
	}
	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Namespace != clusters[j].Namespace {
			return clusters[i].Namespace < clusters[j].Namespace
		}
		return clusters[i].Name < clusters[j].Name
	})
	return Snapshot{
		Namespace: opts.Namespace,
		Total:     len(clusters),
		Clusters:  clusters,
	}.withHistory(ctx, opts), nil
}

func (s Snapshot) withHistory(ctx context.Context, opts Options) Snapshot {
	if opts.History == nil {
		s.HistoryState = historyStateLiveOnly
		return s
	}
	scope := opts.HistoryScope
	scope.Kind = historyKind
	history, err := opts.History.ListHistory(ctx, scope)
	if err != nil {
		s.HistoryState = historyStateUnavailable
		s.HistoryDiagnostic = "durable RayJob history query failed"
		return s
	}
	s.History = filterRayJobs(history, scope)
	s.HistoryState = historyStateAvailable
	return s
}

func filterRayJobs(rows []runs.Run, scope runs.HistoryScope) []runs.Run {
	out := make([]runs.Run, 0, len(rows))
	for _, run := range rows {
		if !strings.EqualFold(run.Kind, historyKind) {
			continue
		}
		if scope.Cluster != "" && !strings.EqualFold(run.Cluster, scope.Cluster) {
			continue
		}
		if scope.Namespace != "" && !strings.EqualFold(run.Namespace, scope.Namespace) {
			continue
		}
		out = append(out, run)
	}
	return out
}

type serviceList struct {
	Items []serviceObj `json:"items"`
}

type serviceObj struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Type string `json:"type"`
	} `json:"spec"`
}

func parseServices(data []byte) ([]Cluster, error) {
	var list serviceList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	out := make([]Cluster, 0, len(list.Items))
	for _, svc := range list.Items {
		if !isHeadService(svc) {
			continue
		}
		out = append(out, buildCluster(svc))
	}
	return out, nil
}

func isHeadService(svc serviceObj) bool {
	cluster, ok := svc.Metadata.Labels[clusterLabel]
	if !ok {
		return false
	}
	if svc.Metadata.Labels[nodeTypeLabel] != nodeTypeHead {
		return false
	}
	return svc.Metadata.Name == cluster+"-head-svc"
}

func buildCluster(svc serviceObj) Cluster {
	name := svc.Metadata.Labels[clusterLabel]
	ns := svc.Metadata.Namespace
	return Cluster{
		Name:      name,
		Namespace: ns,
		Service:   svc.Metadata.Name,
		Type:      svc.Spec.Type,
		ProxyPath: links.RayDashboardPath(ns, name),
	}
}

type podList struct {
	Items []podObj `json:"items"`
}

type podObj struct {
	Metadata struct {
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func readyHeadClusters(ctx context.Context, r Reader, namespace string) map[string]bool {
	raw, err := r.ListPods(ctx, namespace)
	if err != nil {
		return nil
	}
	var list podList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	ready := make(map[string]bool)
	for _, pod := range list.Items {
		if pod.Metadata.Labels[nodeTypeLabel] != nodeTypeHead {
			continue
		}
		cluster, ok := pod.Metadata.Labels[clusterLabel]
		if !ok {
			continue
		}
		if !isPodReady(pod) {
			continue
		}
		ready[pod.Metadata.Namespace+"/"+cluster] = true
	}
	return ready
}

func isPodReady(pod podObj) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
