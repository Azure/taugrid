// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kubeclient

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	profile "github.com/Azure/taugrid/core/resourceprofile"
)

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
	nodeMetricsGVR = schema.GroupVersionResource{
		Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes",
	}
	jobGVR = schema.GroupVersionResource{
		Group: "batch", Version: "v1", Resource: "jobs",
	}
	rayJobGVR = schema.GroupVersionResource{
		Group: "ray.io", Version: "v1", Resource: "rayjobs",
	}
	rayServiceGVR = schema.GroupVersionResource{
		Group: "ray.io", Version: "v1", Resource: "rayservices",
	}
	rayClusterGVR = schema.GroupVersionResource{
		Group: "ray.io", Version: "v1", Resource: "rayclusters",
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

type Client struct {
	dyn  dynamic.Interface
	core corev1client.CoreV1Interface
}

func New(kubeconfig string) (*Client, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build typed client: %w", err)
	}
	return &Client{dyn: dyn, core: core}, nil
}

func NewForDynamic(dyn dynamic.Interface) *Client {
	return &Client{dyn: dyn}
}

func (c *Client) ProfileSet(ctx context.Context) (profile.ProfileSet, error) {
	return profile.NewClusterProvider(c.dyn).ProfileSet(ctx)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
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

func (c *Client) ListLocalQueues(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, localQueueGVR, namespace)
}

func (c *Client) ListClusterQueues(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, clusterQueueGVR, "")
}

func (c *Client) ListWorkloads(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, workloadGVR, namespace)
}

func (c *Client) ListServices(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, serviceGVR, namespace)
}

func (c *Client) ListNodes(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, nodeGVR, "")
}

func (c *Client) ListNodeMetrics(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, nodeMetricsGVR, "")
}

func (c *Client) ListDaemonSets(ctx context.Context) ([]byte, error) {
	return c.listRaw(ctx, daemonSetGVR, "")
}

func (c *Client) ListJobs(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, jobGVR, namespace)
}

func (c *Client) ListRayJobs(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, rayJobGVR, namespace)
}

func (c *Client) ListPods(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, podGVR, namespace)
}

func (c *Client) ListEvents(ctx context.Context, namespace string) ([]byte, error) {
	return c.listRaw(ctx, eventGVR, namespace)
}

func (c *Client) GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool, tailLines, limitBytes int64) ([]byte, error) {
	if c.core == nil {
		return nil, fmt.Errorf("typed Kubernetes client is unavailable")
	}
	opts := &corev1.PodLogOptions{
		Container:  container,
		Previous:   previous,
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
		Timestamps: true,
	}
	data, err := c.core.Pods(namespace).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("get pod logs %s/%s container %s: %w", namespace, pod, container, err)
	}
	return data, nil
}

func (c *Client) GetJob(ctx context.Context, namespace, name string) ([]byte, error) {
	return c.getRaw(ctx, jobGVR, namespace, name)
}

func (c *Client) GetRayJob(ctx context.Context, namespace, name string) ([]byte, error) {
	return c.getRaw(ctx, rayJobGVR, namespace, name)
}

func (c *Client) GetRayService(ctx context.Context, namespace, name string) ([]byte, error) {
	return c.getRaw(ctx, rayServiceGVR, namespace, name)
}

func (c *Client) GetRayCluster(ctx context.Context, namespace, name string) ([]byte, error) {
	return c.getRaw(ctx, rayClusterGVR, namespace, name)
}

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

func (c *Client) listRaw(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, error) {
	var ri dynamic.ResourceInterface = c.dyn.Resource(gvr)
	if namespace != "" {
		ri = c.dyn.Resource(gvr).Namespace(namespace)
	}
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
