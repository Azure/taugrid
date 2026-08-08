// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package kubeclient is the portal's only Kubernetes access path.
//
// Unlike the rest of the tau CLI (which shells out to kubectl via
// internal/kube), the portal is a long-lived in-cluster server on a distroless
// image with no kubectl binary. It therefore reads the API directly with
// client-go, authenticating with the mounted ServiceAccount token in-cluster
// and falling back to a kubeconfig for local runs.
//
// The reader returns raw list JSON (the API's `{items:[...]}` envelope) so
// callers can feed it to existing pure aggregators such as
// queue.BuildSnapshot() without a second typed decode.
package kubeclient

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Kueue resource versions. The repo's Kueue chart serves both v1beta1 and
// v1beta2 and stores v1beta2, which is also what `kubectl get
// <resource>.kueue.x-k8s.io` resolves to today. Pinning v1beta2 keeps the
// portal's reads byte-compatible with queue.Fetch()'s kubectl output.
var (
	localQueueGVR = schema.GroupVersionResource{
		Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "localqueues",
	}
	clusterQueueGVR = schema.GroupVersionResource{
		Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "clusterqueues",
	}
	workloadGVR = schema.GroupVersionResource{
		Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads",
	}
	serviceGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "services",
	}
	nodeGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "nodes",
	}
	jobGVR = schema.GroupVersionResource{
		Group: "batch", Version: "v1", Resource: "jobs",
	}
	// RayJobs are served by the KubeRay CRD. v1 is the stored/served version in
	// the repo's kuberay-operator chart, matching `kubectl get rayjobs.ray.io`.
	rayJobGVR = schema.GroupVersionResource{
		Group: "ray.io", Version: "v1", Resource: "rayjobs",
	}
	podGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "pods",
	}
	daemonSetGVR = schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "daemonsets",
	}
	eventGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "events",
	}
)

// Client reads Kubernetes objects for the portal via the dynamic client.
type Client struct {
	dyn dynamic.Interface
}

// New builds a Client. It prefers in-cluster config (the mounted ServiceAccount
// token); when that is unavailable it loads a kubeconfig, honoring an explicit
// path, then $KUBECONFIG, then the default loading rules. This lets
// `taugrid-portal portal serve` run both in-cluster and on a developer machine.
func New(kubeconfig string) (*Client, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return &Client{dyn: dyn}, nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	// An explicit --kubeconfig wins as a single-file path. $KUBECONFIG is
	// deliberately not read here: NewDefaultClientConfigLoadingRules already
	// splits it (colon-separated) into loadingRules.Precedence, so assigning the
	// raw value to ExplicitPath would both duplicate that and break on multi-path
	// lists (ExplicitPath expects one file).
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kube config (not in-cluster and no usable kubeconfig): %w", err)
	}
	return cfg, nil
}

// ListLocalQueues returns the namespaced localqueues list as raw JSON.
func (c *Client) ListLocalQueues(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, localQueueGVR, namespace)
}

// ListClusterQueues returns the cluster-scoped clusterqueues list as raw JSON.
func (c *Client) ListClusterQueues(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, clusterQueueGVR, "")
}

// ListWorkloads returns the namespaced workloads list as raw JSON.
func (c *Client) ListWorkloads(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, workloadGVR, namespace)
}

// ListServices returns the core Services list as raw JSON. An empty namespace
// lists across all namespaces (the Ray board discovers head Services
// cluster-wide).
func (c *Client) ListServices(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, serviceGVR, namespace)
}

// ListNodes returns the cluster-scoped core Nodes list as raw JSON. The Nodes
// board reads each node's .status.capacity/.allocatable and topology labels
// (SKU, agentpool, region/zone) to describe the fleet's hardware.
func (c *Client) ListNodes(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, nodeGVR, "")
}

// ListDaemonSets returns DaemonSets across all namespaces for the Fleet Compute
// runtime-plumbing summary.
func (c *Client) ListDaemonSets(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, daemonSetGVR, "")
}

// ListJobs returns the namespaced batch/v1 Jobs list as raw JSON. The Jobs
// (runs) board filters these to Tau-managed workloads and excludes
// RayJob-owned submitter Jobs.
func (c *Client) ListJobs(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, jobGVR, namespace)
}

// ListRayJobs returns the namespaced ray.io RayJobs list as raw JSON. A missing
// CRD (KubeRay not installed) surfaces as a list error, which the runs board
// tolerates by dropping just the RayJob rows.
func (c *Client) ListRayJobs(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, rayJobGVR, namespace)
}

// ListPods returns the namespaced core Pods list as raw JSON. The job detail
// page filters these to a run's pods (by ray.io/cluster or tau.azure.com/job) to
// report per-pod phase, node placement, and restart counts.
func (c *Client) ListPods(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, podGVR, namespace)
}

// ListEvents returns the namespaced core Events list as raw JSON. The job detail
// page surfaces recent scheduling/image-pull/failure events for troubleshooting.
func (c *Client) ListEvents(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, eventGVR, namespace)
}

// GetJob returns a single batch/v1 Job as raw JSON. The job detail page reads the
// object directly rather than filtering a list.
func (c *Client) GetJob(ctx context.Context, namespace, name string) ([]byte, error) {
	return c.getRaw(ctx, jobGVR, namespace, name)
}

// GetRayJob returns a single ray.io RayJob as raw JSON. A missing CRD (KubeRay
// not installed) surfaces as a get error, which the detail page tolerates.
func (c *Client) GetRayJob(ctx context.Context, namespace, name string) ([]byte, error) {
	return c.getRaw(ctx, rayJobGVR, namespace, name)
}

// getRaw fetches a single namespaced object by name and returns its JSON bytes.
func (c *Client) getRaw(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) ([]byte, error) {
	obj, err := c.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s.%s %s/%s: %w", gvr.Resource, gvr.Group, namespace, name, err)
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal %s.%s %s/%s: %w", gvr.Resource, gvr.Group, namespace, name, err)
	}
	return data, nil
}

// listRaw lists a resource (namespaced when namespace != "") and returns the
// dynamic client's `{items:[...]}` envelope as JSON bytes.
func (c *Client) listRaw(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, error) {
	var ri dynamic.ResourceInterface = c.dyn.Resource(gvr)
	if namespace != "" {
		ri = c.dyn.Resource(gvr).Namespace(namespace)
	}
	// TODO: pass ListOptions.Limit + follow Continue tokens for large clusters —
	// today every board lists its resource cluster-/namespace-wide unpaginated,
	// which is fine at the current deployment scale but could pressure the API
	// server (and this process's memory) on very large fleets.
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s.%s: %w", gvr.Resource, gvr.Group, err)
	}
	data, err := list.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal %s.%s: %w", gvr.Resource, gvr.Group, err)
	}
	return data, nil
}
