package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	workloadGVR       = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}
	localQueueGVR     = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "localqueues"}
	clusterQueueGVR   = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "clusterqueues"}
	admissionCheckGVR = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "admissionchecks"}
	provReqGVR        = schema.GroupVersionResource{Group: "autoscaling.x-k8s.io", Version: "v1", Resource: "provisioningrequests"}
)

const (
	provisioningController = "kueue.x-k8s.io/provisioning-request"

	labelQueue   = "kueue.x-k8s.io/queue-name"
	labelProfile = "tau.azure.com/profile"

	labelPool    = "agentpool"
	labelBooking = "demo.tau/provisioning-request"
)

type clients struct {
	core kubernetes.Interface
	dyn  dynamic.Interface
}

func newClients() (*clients, error) {
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.QPS, cfg.Burst = 50, 100
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &clients{core: core, dyn: dyn}, nil
}

// condition is the subset of metav1.Condition the demo reads from unstructured objects.
type condition struct {
	Type, Status, Reason, Message string
	LastTransition                time.Time
}

func conditions(obj *unstructured.Unstructured) map[string]condition {
	out := map[string]condition{}
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := condition{Type: str(m["type"]), Status: str(m["status"]), Reason: str(m["reason"]), Message: str(m["message"])}
		c.LastTransition, _ = time.Parse(time.RFC3339, str(m["lastTransitionTime"]))
		out[c.Type] = c
	}
	return out
}

func isTrue(obj *unstructured.Unstructured, condType string) bool {
	return conditions(obj)[condType].Status == string(metav1.ConditionTrue)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

type nodePool struct {
	Name          string
	MaxNodes      int
	GPUsPerNode   int64
	CPUPerNode    resource.Quantity
	MemoryPerNode resource.Quantity
	Boot, Idle    time.Duration
}

func loadPool(ctx context.Context, c *clients) (nodePool, error) {
	cm, err := c.core.CoreV1().ConfigMaps("kube-system").Get(ctx, "demo-nodepool", metav1.GetOptions{})
	if err != nil {
		return nodePool{}, fmt.Errorf("read kube-system/demo-nodepool: %w", err)
	}
	atoi := func(k string, def int) int {
		if v, err := strconv.Atoi(cm.Data[k]); err == nil {
			return v
		}
		return def
	}
	qty := func(k, def string) resource.Quantity {
		if q, err := resource.ParseQuantity(cm.Data[k]); err == nil {
			return q
		}
		return resource.MustParse(def)
	}
	return nodePool{
		Name:          cm.Data["name"],
		MaxNodes:      atoi("maxNodes", 2),
		GPUsPerNode:   int64(atoi("gpusPerNode", 8)),
		CPUPerNode:    qty("cpuPerNode", "16"),
		MemoryPerNode: qty("memoryPerNode", "128Gi"),
		Boot:          time.Duration(atoi("bootSeconds", 15)) * time.Second,
		Idle:          time.Duration(atoi("idleSeconds", 15)) * time.Second,
	}, nil
}

// podsPerNode is how many pods with these requests fit on one pool node.
func (p nodePool) podsPerNode(req corev1.ResourceList) int64 {
	best := int64(1 << 30)
	fit := func(capacity resource.Quantity, name corev1.ResourceName) {
		r, ok := req[name]
		if !ok || r.IsZero() {
			return
		}
		if n := capacity.MilliValue() / r.MilliValue(); n < best {
			best = n
		}
	}
	fit(*resource.NewQuantity(p.GPUsPerNode, resource.DecimalSI), "nvidia.com/gpu")
	fit(p.CPUPerNode, corev1.ResourceCPU)
	fit(p.MemoryPerNode, corev1.ResourceMemory)
	return best
}

func (p nodePool) nodesFor(count int64, req corev1.ResourceList) int64 {
	per := p.podsPerNode(req)
	if per <= 0 {
		return 1 << 30
	}
	return (count + per - 1) / per
}

func poolNodes(ctx context.Context, c *clients, pool string) ([]corev1.Node, error) {
	list, err := c.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: labelPool + "=" + pool})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// workloadFor finds the Kueue Workload owned by a Job.
func workloadFor(ctx context.Context, c *clients, ns string, jobUID string) (*unstructured.Unstructured, error) {
	list, err := c.dyn.Resource(workloadGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		for _, ref := range list.Items[i].GetOwnerReferences() {
			if string(ref.UID) == jobUID {
				return &list.Items[i], nil
			}
		}
	}
	return nil, nil
}

// provisioningRequestFor returns the newest ProvisioningRequest Kueue created for a Workload.
func provisioningRequestFor(ctx context.Context, c *clients, wl *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	list, err := c.dyn.Resource(provReqGVR).Namespace(wl.GetNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var newest *unstructured.Unstructured
	for i := range list.Items {
		for _, ref := range list.Items[i].GetOwnerReferences() {
			if ref.UID == wl.GetUID() {
				if newest == nil || list.Items[i].GetCreationTimestamp().After(newest.GetCreationTimestamp().Time) {
					newest = &list.Items[i]
				}
			}
		}
	}
	return newest, nil
}

// parseWithPositional parses flags that may appear before or after positional args
// (the flag package alone stops at the first positional).
func parseWithPositional(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func logf(component, format string, args ...any) {
	fmt.Printf("%s [%s] %s\n", time.Now().Format("15:04:05"), component, fmt.Sprintf(format, args...))
}
