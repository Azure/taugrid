// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ray builds the portal's Ray board.
//
// Discovery is head-Service based: KubeRay auto-creates a `<raycluster>-head-svc`
// (headless ClusterIP) for every RayCluster, carrying labels ray.io/cluster=<name>
// + ray.io/node-type=head and exposing the dashboard port (8265). This board lists
// those Services via internal/portal/kubeclient (client-go) and, for each, emits a
// same-origin proxy path the portal reverse-proxies to the head pod's :8265.
//
// The board needs no per-cluster dashboard Service and does not link out to an
// internal load balancer. It proxies the dashboard over in-cluster DNS
// (http://<cluster>-head-svc.<ns>.svc:8265), so any RayCluster is reachable
// while it is running.
//
// The live dashboard is only available while the RayCluster exists; a durable,
// Kusto-backed RayJob history page is tracked separately (work item #941).
package ray

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Azure/taugrid/portal/internal/portal/links"
)

// clusterLabel is the selector key the head Service carries; its value is the owning
// RayCluster's name.
const clusterLabel = "ray.io/cluster"

// nodeTypeLabel/nodeTypeHead identify the head Service among a RayCluster's Services
// (KubeRay also creates worker/serve Services that carry ray.io/cluster).
const (
	nodeTypeLabel = "ray.io/node-type"
	nodeTypeHead  = "head"
)

// Reader lists the raw Services and Pods JSON the board needs. kubeclient.Client
// satisfies this; tests supply a fake so no live API is required.
type Reader interface {
	// ListServices returns the core Services list as raw JSON. An empty
	// namespace lists cluster-wide.
	ListServices(ctx context.Context, namespace string) ([]byte, error)
	// ListPods returns the core Pods list as raw JSON. An empty namespace lists
	// cluster-wide. The board uses it to tell whether each cluster's head pod is
	// Ready (dashboard reachable) before offering an "open" link.
	ListPods(ctx context.Context, namespace string) ([]byte, error)
}

// Options scopes discovery. Namespace, when set, restricts the listing to one
// namespace; empty lists cluster-wide.
type Options struct {
	Namespace string
}

// Cluster is one discovered Ray head Service: the owning RayCluster's identity and
// the same-origin path the portal proxies to its dashboard.
type Cluster struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	// Type is the Service type (usually ClusterIP for the headless head-svc).
	Type string `json:"type,omitempty"`
	// ProxyPath is the portal-relative path that reverse-proxies this cluster's
	// Ray dashboard (:8265). Empty is never emitted for a discovered head Service.
	ProxyPath string `json:"proxyPath"`
	// Available reports whether the cluster's head pod is currently Ready, i.e.
	// the dashboard is reachable. The head Service can outlive its head pod (the
	// pod goes NotReady on suspend/terminal/crash before KubeRay GCs the
	// Service); when Available is false the board offers no live "open" link so
	// the user does not click into a dead dashboard. Defaults to true when pod
	// state cannot be determined, so a transient pod-list failure never hides a
	// healthy cluster's link.
	Available bool `json:"available"`
}

// Snapshot is the Ray board payload: the discovered dashboards plus rollups.
type Snapshot struct {
	Namespace string    `json:"namespace,omitempty"`
	Total     int       `json:"total"`
	Clusters  []Cluster `json:"clusters"`
}

// Board lists Services via the Reader, keeps only the `<rc>-head-svc` ones, and
// builds each cluster's proxy path. Empty results are not an error.
func Board(ctx context.Context, r Reader, opts Options) (Snapshot, error) {
	raw, err := r.ListServices(ctx, opts.Namespace)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list services: %w", err)
	}
	clusters, err := parseServices(raw)
	if err != nil {
		return Snapshot{}, err
	}
	// Determine dashboard reachability per cluster from head-pod readiness. A
	// head Service can outlive its head pod, so a listed cluster is not
	// necessarily reachable. If the pod list cannot be obtained we leave
	// Available at its default (true) so a transient failure never hides a
	// healthy cluster's link.
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
	}, nil
}

// serviceList is the subset of the core v1 Service list the board reads.
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

// parseServices decodes the Services list and keeps only Ray head Services.
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

// isHeadService recognizes a KubeRay head Service by the ray.io/cluster label plus
// ray.io/node-type=head, and verifies the Service name matches KubeRay's naming
// convention (<cluster>-head-svc). This prevents a mislabeled Service from being
// accepted as a valid proxy target.
func isHeadService(svc serviceObj) bool {
	cluster, ok := svc.Metadata.Labels[clusterLabel]
	if !ok {
		return false
	}
	if svc.Metadata.Labels[nodeTypeLabel] != nodeTypeHead {
		return false
	}
	// KubeRay always names the head Service "<cluster>-head-svc".
	return svc.Metadata.Name == cluster+"-head-svc"
}

// buildCluster maps one head Service to a Cluster with its portal proxy path.
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

// podList is the subset of the core v1 Pod list the board reads to determine
// head-pod readiness.
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

// readyHeadClusters lists pods and returns the set of "<namespace>/<cluster>"
// keys whose Ray head pod is currently Ready. It returns nil when the pod list
// cannot be obtained or decoded, signaling callers to degrade to "available"
// rather than hide healthy links on a transient failure.
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

// HeadPodReady reports whether the Ray head pod of <namespace>/<cluster> is
// currently Ready, operating on an already-fetched core v1 Pod list JSON so
// callers (e.g. the Job-detail board) can reuse pod bytes they already read
// instead of issuing a second ListPods call.
//
// It returns known=false when the JSON cannot be decoded or no head pod for the
// cluster is present in the list; callers should then default to "reachable"
// rather than hide a healthy dashboard link on a transient/absent signal. When
// known=true, ready reflects the head pod's Ready condition.
func HeadPodReady(podsJSON []byte, namespace, cluster string) (ready, known bool) {
	var list podList
	if err := json.Unmarshal(podsJSON, &list); err != nil {
		return false, false
	}
	for _, pod := range list.Items {
		if pod.Metadata.Labels[nodeTypeLabel] != nodeTypeHead {
			continue
		}
		if pod.Metadata.Labels[clusterLabel] != cluster {
			continue
		}
		if pod.Metadata.Namespace != namespace {
			continue
		}
		return isPodReady(pod), true
	}
	return false, false
}

// isPodReady reports whether the pod carries a Ready condition set to True.
func isPodReady(pod podObj) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
