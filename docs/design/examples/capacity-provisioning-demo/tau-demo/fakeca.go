package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeCA simulates the AKS Cluster Autoscaler for one node pool on the success
// path: a ProvisioningRequest with best-effort-atomic-scale-up creates all
// requested nodes together after the boot delay, then idle nodes scale down.
type fakeCA struct {
	c         *clients
	scaleUps  map[string]time.Time // ProvisioningRequest UID -> scale-up start
	idleSince map[string]time.Time // node name -> first seen empty
}

func runFakeCA(ctx context.Context, c *clients, _ []string) error {
	ca := &fakeCA{c: c, scaleUps: map[string]time.Time{}, idleSince: map[string]time.Time{}}
	logf("fake-ca", "simulating AKS Cluster Autoscaler (atomic ProvisioningRequest scale-up)")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := ca.loop(ctx); err != nil && ctx.Err() == nil {
			logf("fake-ca", "loop error: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (ca *fakeCA) loop(ctx context.Context) error {
	pool, err := loadPool(ctx, ca.c)
	if err != nil {
		return err
	}
	prs, err := ca.c.dyn.Resource(provReqGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for i := range prs.Items {
		pr := &prs.Items[i]
		live[string(pr.GetUID())] = true
		if err := ca.handleProvisioningRequest(ctx, pool, pr); err != nil {
			logf("fake-ca", "%s/%s: %v", pr.GetNamespace(), pr.GetName(), err)
		}
	}
	for uid := range ca.scaleUps {
		if !live[uid] {
			logf("fake-ca", "ProvisioningRequest deleted before scale-up finished; scale-up aborted")
			delete(ca.scaleUps, uid)
		}
	}
	return ca.scaleDown(ctx, pool)
}

func (ca *fakeCA) handleProvisioningRequest(ctx context.Context, pool nodePool, pr *unstructured.Unstructured) error {
	conds := conditions(pr)
	if conds["Provisioned"].Status == "True" {
		return nil
	}
	key := pr.GetNamespace() + "." + pr.GetName()
	needed, err := ca.nodesNeeded(ctx, pool, pr)
	if err != nil {
		return err
	}
	nodes, err := poolNodes(ctx, ca.c, pool.Name)
	if err != nil {
		return err
	}
	booked := 0
	for _, n := range nodes {
		if n.Labels[labelBooking] == key {
			booked++
		}
	}

	if conds["Accepted"].Status != "True" {
		logf("fake-ca", "ProvisioningRequest %s: accepted (needs %d node(s) of %s)", key, needed, pool.Name)
		return ca.setConditions(ctx, pr, condition{Type: "Accepted", Status: "True", Reason: "Accepted", Message: "ProvisioningRequest accepted"})
	}
	if int64(booked) >= needed {
		logf("fake-ca", "ProvisioningRequest %s: Provisioned=True (%d node(s) booked)", key, booked)
		return ca.setConditions(ctx, pr, condition{Type: "Provisioned", Status: "True", Reason: "Provisioned",
			Message: fmt.Sprintf("capacity booked on %d node(s) of %s", booked, pool.Name)})
	}

	missing := needed - int64(booked)
	if int64(len(nodes))+missing > int64(pool.MaxNodes) {
		msg := fmt.Sprintf("max node group size reached: %s max %d node(s), request needs %d", pool.Name, pool.MaxNodes, needed)
		if conds["Provisioned"].Reason != "CapacityIsNotFound" {
			logf("fake-ca", "ProvisioningRequest %s: %s", key, msg)
			return ca.setConditions(ctx, pr, condition{Type: "Provisioned", Status: "False", Reason: "CapacityIsNotFound", Message: msg})
		}
		return nil
	}

	start, ok := ca.scaleUps[string(pr.GetUID())]
	if !ok {
		ca.scaleUps[string(pr.GetUID())] = time.Now()
		logf("fake-ca", "ProvisioningRequest %s: scaling %s %d -> %d atomically (boot %s)", key, pool.Name, len(nodes), int64(len(nodes))+missing, pool.Boot)
		return ca.setConditions(ctx, pr, condition{Type: "Provisioned", Status: "False", Reason: "ScaleUpInProgress",
			Message: fmt.Sprintf("scaling %s by %d node(s)", pool.Name, missing)})
	}
	if time.Since(start) < pool.Boot {
		return nil
	}
	for i := int64(0); i < missing; i++ {
		if err := ca.createNode(ctx, pool, key); err != nil {
			return err
		}
	}
	delete(ca.scaleUps, string(pr.GetUID()))
	logf("fake-ca", "ProvisioningRequest %s: %d node(s) Ready", key, missing)
	return nil
}

func (ca *fakeCA) nodesNeeded(ctx context.Context, pool nodePool, pr *unstructured.Unstructured) (int64, error) {
	podSets, _, _ := unstructured.NestedSlice(pr.Object, "spec", "podSets")
	var total int64
	for _, item := range podSets {
		ps, _ := item.(map[string]any)
		name, _, _ := unstructured.NestedString(ps, "podTemplateRef", "name")
		count, _, _ := unstructured.NestedInt64(ps, "count")
		tpl, err := ca.c.core.CoreV1().PodTemplates(pr.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("read PodTemplate %s: %w", name, err)
		}
		total += pool.nodesFor(count, podRequests(tpl.Template.Spec))
	}
	return total, nil
}

func podRequests(spec corev1.PodSpec) corev1.ResourceList {
	out := corev1.ResourceList{}
	for _, ctr := range spec.Containers {
		for name, q := range ctr.Resources.Requests {
			sum := out[name]
			sum.Add(q)
			out[name] = sum
		}
	}
	return out
}

func (ca *fakeCA) scaleDown(ctx context.Context, pool nodePool) error {
	nodes, err := poolNodes(ctx, ca.c, pool.Name)
	if err != nil {
		return err
	}
	pods, err := ca.c.core.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	busy := map[string]bool{}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			busy[p.Spec.NodeName] = true
		}
	}
	prs, err := ca.c.dyn.Resource(provReqGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	bookedFor := map[string]bool{}
	for _, pr := range prs.Items {
		bookedFor[pr.GetNamespace()+"."+pr.GetName()] = true
	}
	for _, n := range nodes {
		if busy[n.Name] {
			delete(ca.idleSince, n.Name)
			continue
		}
		// A booked node waits for its pods while the booking (its ProvisioningRequest) is alive
		// and young; after that it's just an empty node.
		if key := n.Labels[labelBooking]; key != "" && bookedFor[key] && time.Since(n.CreationTimestamp.Time) < 2*time.Minute {
			continue
		}
		since, ok := ca.idleSince[n.Name]
		if !ok {
			ca.idleSince[n.Name] = time.Now()
			continue
		}
		if time.Since(since) < pool.Idle {
			continue
		}
		err := ca.c.core.CoreV1().Nodes().Delete(ctx, n.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		delete(ca.idleSince, n.Name)
		logf("fake-ca", "scale-down: node %s empty for %s, removed", n.Name, pool.Idle)
	}
	return nil
}

func (ca *fakeCA) createNode(ctx context.Context, pool nodePool, booking string) error {
	name := fmt.Sprintf("aks-%s-vmss%06d", pool.Name, rand.Intn(1_000_000))
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    pool.CPUPerNode,
		corev1.ResourceMemory: pool.MemoryPerNode,
		corev1.ResourcePods:   resource.MustParse("110"),
		"nvidia.com/gpu":      *resource.NewQuantity(pool.GPUsPerNode, resource.DecimalSI),
	}
	labels := map[string]string{
		labelPool:                pool.Name,
		"type":                   "kwok",
		"kubernetes.io/hostname": name,
		"kubernetes.io/role":     "agent",
	}
	if booking != "" {
		labels[labelBooking] = booking
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: map[string]string{"kwok.x-k8s.io/node": "fake", "node.alpha.kubernetes.io/ttl": "0"},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "kwok.x-k8s.io/node", Value: "fake", Effect: corev1.TaintEffectNoSchedule}}},
		Status: corev1.NodeStatus{Capacity: resources, Allocatable: resources},
	}
	if _, err := ca.c.core.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	logf("fake-ca", "scale-up: node %s created (%d GPU) for ProvisioningRequest %s", name, pool.GPUsPerNode, booking)
	return nil
}

func (ca *fakeCA) setConditions(ctx context.Context, pr *unstructured.Unstructured, updates ...condition) error {
	existing, _, _ := unstructured.NestedSlice(pr.Object, "status", "conditions")
	byType := map[string]map[string]any{}
	var order []string
	for _, item := range existing {
		if m, ok := item.(map[string]any); ok {
			t := str(m["type"])
			byType[t] = m
			order = append(order, t)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, u := range updates {
		m, ok := byType[u.Type]
		if !ok {
			m = map[string]any{"type": u.Type}
			byType[u.Type] = m
			order = append(order, u.Type)
		}
		if str(m["status"]) != u.Status || str(m["lastTransitionTime"]) == "" {
			m["lastTransitionTime"] = now
		}
		m["status"], m["reason"], m["message"] = u.Status, u.Reason, u.Message
		m["observedGeneration"] = pr.GetGeneration()
	}
	out := make([]any, 0, len(order))
	for _, t := range order {
		out = append(out, byType[t])
	}
	if err := unstructured.SetNestedSlice(pr.Object, out, "status", "conditions"); err != nil {
		return err
	}
	_, err := ca.c.dyn.Resource(provReqGVR).Namespace(pr.GetNamespace()).UpdateStatus(ctx, pr, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return nil
	}
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}
