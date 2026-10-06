// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nvidiaGPUResourceName = "nvidia.com/gpu"

type discoveredGPUFlavor struct {
	name     string
	gpuClass string
	capacity resource.Quantity
}

func (r *TauClusterReconciler) reconcileDiscoveredGPUFlavors(
	ctx context.Context,
	mutate bool,
) ([]tauv1alpha1.TauManagedResourceStatus, []tauv1alpha1.TauResourceCapacityStatus, bool, error) {
	queueNames, err := r.discoveredGPUFlavorQueueNames(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	if len(queueNames) == 0 {
		return nil, nil, false, nil
	}

	flavors, err := r.discoverGPUFlavors(ctx)
	if err != nil {
		return nil, nil, false, err
	}

	managed := make([]tauv1alpha1.TauManagedResourceStatus, 0, len(flavors))
	capacity := make([]tauv1alpha1.TauResourceCapacityStatus, 0, len(flavors))
	drifted := false
	for _, flavor := range flavors {
		capacity = append(capacity, tauv1alpha1.TauResourceCapacityStatus{
			Flavor:   flavor.name,
			Resource: nvidiaGPUResourceName,
			Capacity: flavor.capacity.DeepCopy(),
		})
		object, changed, err := r.reconcileDiscoveredGPUFlavor(ctx, flavor, mutate)
		if err != nil {
			return managed, capacity, true, err
		}
		drifted = drifted || changed
		if object != nil && object.GetLabels()[labelManagedBy] == labelManagedByValue {
			managed = append(managed, managedResourceStatus(resourceFlavorGVK, object))
		}
	}
	// Preserve the v0 baseline queue contract for installations that explicitly
	// opt into discovery. Team-backed workspaces never use this path: operators
	// assign their quota through TauTeam and TauWorkspace resources.
	for _, queueName := range queueNames {
		changed, err := r.reconcileLegacyDiscoveredGPUQuota(ctx, queueName, flavors, mutate)
		if err != nil {
			return managed, capacity, true, err
		}
		drifted = drifted || changed
	}
	return managed, capacity, drifted, nil
}

func (r *TauClusterReconciler) discoveredGPUFlavorQueueNames(ctx context.Context) ([]string, error) {
	queues := &unstructured.UnstructuredList{}
	queues.SetGroupVersionKind(clusterQueueGVK.GroupVersion().WithKind("ClusterQueueList"))
	if err := r.List(ctx, queues, client.MatchingLabels{labelDiscoverGPUFlavors: "true"}); err != nil {
		return nil, fmt.Errorf("list ClusterQueues enabled for GPU flavor discovery: %w", err)
	}
	names := make([]string, 0, len(queues.Items))
	for i := range queues.Items {
		names = append(names, queues.Items[i].GetName())
	}
	sort.Strings(names)
	return names, nil
}

func (r *TauClusterReconciler) discoverGPUFlavors(ctx context.Context) ([]discoveredGPUFlavor, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes for GPU flavor discovery: %w", err)
	}

	capacityByClass := make(map[string]resource.Quantity)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		gpuClass := strings.TrimSpace(node.Labels[labelkeys.LabelGPUClass])
		capacity := node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)]
		if gpuClass == "" || capacity.Sign() <= 0 {
			continue
		}
		total := capacityByClass[gpuClass]
		total.Add(capacity)
		capacityByClass[gpuClass] = total
	}

	classes := make([]string, 0, len(capacityByClass))
	for gpuClass := range capacityByClass {
		classes = append(classes, gpuClass)
	}
	sort.Strings(classes)

	flavors := make([]discoveredGPUFlavor, 0, len(classes))
	for _, gpuClass := range classes {
		flavors = append(flavors, discoveredGPUFlavor{
			name:     discoveredGPUFlavorName(gpuClass),
			gpuClass: gpuClass,
			capacity: capacityByClass[gpuClass],
		})
	}
	return flavors, nil
}

func discoveredGPUFlavorName(gpuClass string) string {
	return networkDomainLabel("taugrid-gpu", gpuClass)
}

func desiredDiscoveredGPUFlavor(flavor discoveredGPUFlavor) *unstructured.Unstructured {
	object := newQueueObject(resourceFlavorGVK)
	object.SetName(flavor.name)
	object.SetLabels(map[string]string{
		labelManagedBy:          labelManagedByValue,
		labelkeys.LabelGPUClass: flavor.gpuClass,
	})
	object.Object["spec"] = map[string]any{
		"nodeLabels": map[string]any{
			"kubernetes.io/os":      "linux",
			labelkeys.LabelGPUClass: flavor.gpuClass,
		},
		"nodeTaints": []any{
			map[string]any{
				"key":    "sku",
				"value":  "gpu",
				"effect": string(corev1.TaintEffectNoSchedule),
			},
		},
		"topologyName": tauGPUNodeTopologyName,
	}
	return object
}

func (r *TauClusterReconciler) reconcileDiscoveredGPUFlavor(
	ctx context.Context,
	flavor discoveredGPUFlavor,
	mutate bool,
) (*unstructured.Unstructured, bool, error) {
	current := newQueueObject(resourceFlavorGVK)
	err := r.Get(ctx, client.ObjectKey{Name: flavor.name}, current)
	if apierrors.IsNotFound(err) {
		desired := desiredDiscoveredGPUFlavor(flavor)
		if !mutate {
			return desired, true, nil
		}
		if err := r.Create(ctx, desired); err != nil {
			return nil, true, fmt.Errorf("create discovered ResourceFlavor %q: %w", flavor.name, err)
		}
		return desired, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("get discovered ResourceFlavor %q: %w", flavor.name, err)
	}
	if current.GetLabels()[labelManagedBy] != labelManagedByValue {
		return current, true, fmt.Errorf(
			"ResourceFlavor %q exists but is not owned by %s",
			flavor.name,
			labelManagedByValue,
		)
	}

	desiredSpec, _, _ := unstructured.NestedMap(desiredDiscoveredGPUFlavor(flavor).Object, "spec")
	currentSpec, _, _ := unstructured.NestedMap(current.Object, "spec")
	if !reflect.DeepEqual(currentSpec, desiredSpec) {
		return current, true, fmt.Errorf(
			"ResourceFlavor %q has immutable drift from discovered GPU class %q",
			flavor.name,
			flavor.gpuClass,
		)
	}
	return current, false, nil
}

func (r *TauClusterReconciler) reconcileLegacyDiscoveredGPUQuota(
	ctx context.Context,
	queueName string,
	flavors []discoveredGPUFlavor,
	mutate bool,
) (bool, error) {
	queue := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: queueName}, queue); err != nil {
		return true, fmt.Errorf("get legacy ClusterQueue %q for GPU discovery: %w", queueName, err)
	}
	if queue.GetLabels()[labelDiscoverGPUFlavors] != "true" {
		return true, fmt.Errorf("refusing to update ClusterQueue %q without %s=true", queueName, labelDiscoverGPUFlavors)
	}

	groups, found, err := unstructured.NestedSlice(queue.Object, "spec", "resourceGroups")
	if err != nil || !found {
		return true, fmt.Errorf("ClusterQueue %q has no readable resourceGroups", queueName)
	}
	groupIndex := gpuResourceGroupIndex(groups)
	if groupIndex < 0 {
		return true, fmt.Errorf("ClusterQueue %q does not cover %s", queueName, nvidiaGPUResourceName)
	}

	group, ok := groups[groupIndex].(map[string]any)
	if !ok {
		return true, fmt.Errorf("ClusterQueue %q GPU resource group is malformed", queueName)
	}
	queueFlavors, _, err := unstructured.NestedSlice(group, "flavors")
	if err != nil {
		return true, fmt.Errorf("ClusterQueue %q GPU flavors are malformed: %w", queueName, err)
	}

	changed := false
	for _, flavor := range flavors {
		var flavorChanged bool
		queueFlavors, flavorChanged, err = ensureQueueFlavorCapacity(queueFlavors, flavor)
		if err != nil {
			return true, fmt.Errorf("ClusterQueue %q flavor %q: %w", queueName, flavor.name, err)
		}
		changed = changed || flavorChanged
	}
	if !changed || !mutate {
		return changed, nil
	}

	group["flavors"] = queueFlavors
	groups[groupIndex] = group
	if err := unstructured.SetNestedSlice(queue.Object, groups, "spec", "resourceGroups"); err != nil {
		return true, fmt.Errorf("set ClusterQueue %q discovered GPU flavors: %w", queueName, err)
	}
	if err := r.Update(ctx, queue); err != nil {
		return true, fmt.Errorf("update ClusterQueue %q discovered GPU flavors: %w", queueName, err)
	}
	return false, nil
}

func gpuResourceGroupIndex(groups []any) int {
	for i, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		resources, _, _ := unstructured.NestedStringSlice(group, "coveredResources")
		for _, name := range resources {
			if name == nvidiaGPUResourceName {
				return i
			}
		}
	}
	return -1
}

func ensureQueueFlavorCapacity(
	queueFlavors []any,
	discovered discoveredGPUFlavor,
) ([]any, bool, error) {
	for i, raw := range queueFlavors {
		flavor, ok := raw.(map[string]any)
		if !ok || flavor["name"] != discovered.name {
			continue
		}
		resources, _, err := unstructured.NestedSlice(flavor, "resources")
		if err != nil {
			return queueFlavors, false, err
		}
		for j, resourceValue := range resources {
			resourceQuota, ok := resourceValue.(map[string]any)
			if !ok || resourceQuota["name"] != nvidiaGPUResourceName {
				continue
			}
			current, err := resource.ParseQuantity(fmt.Sprint(resourceQuota["nominalQuota"]))
			if err != nil {
				return queueFlavors, false, fmt.Errorf("invalid GPU nominalQuota: %w", err)
			}
			if current.Cmp(discovered.capacity) >= 0 {
				return queueFlavors, false, nil
			}
			resourceQuota["nominalQuota"] = discovered.capacity.String()
			resources[j] = resourceQuota
			flavor["resources"] = resources
			queueFlavors[i] = flavor
			return queueFlavors, true, nil
		}
		flavor["resources"] = append(resources, map[string]any{
			"name":         nvidiaGPUResourceName,
			"nominalQuota": discovered.capacity.String(),
		})
		queueFlavors[i] = flavor
		return queueFlavors, true, nil
	}

	resources, err := discoveredFlavorResources(queueFlavors, discovered.capacity)
	if err != nil {
		return queueFlavors, false, err
	}
	return append(queueFlavors, map[string]any{
		"name":      discovered.name,
		"resources": resources,
	}), true, nil
}

func discoveredFlavorResources(queueFlavors []any, capacity resource.Quantity) ([]any, error) {
	if len(queueFlavors) == 0 {
		return nil, fmt.Errorf("cannot derive non-GPU quotas without an existing base flavor")
	}
	base, ok := queueFlavors[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("base flavor is malformed")
	}
	resources, _, err := unstructured.NestedSlice(base, "resources")
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(resources)+1)
	gpuFound := false
	for _, raw := range resources {
		resourceQuota, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("base flavor resource quota is malformed")
		}
		copied := make(map[string]any, len(resourceQuota))
		for key, value := range resourceQuota {
			copied[key] = value
		}
		if copied["name"] == nvidiaGPUResourceName {
			copied["nominalQuota"] = capacity.String()
			gpuFound = true
		}
		out = append(out, copied)
	}
	if !gpuFound {
		out = append(out, map[string]any{
			"name":         nvidiaGPUResourceName,
			"nominalQuota": capacity.String(),
		})
	}
	return out, nil
}

func managedResourceStatus(
	gvk schema.GroupVersionKind,
	object *unstructured.Unstructured,
) tauv1alpha1.TauManagedResourceStatus {
	return tauv1alpha1.TauManagedResourceStatus{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       object.GetName(),
		UID:        string(object.GetUID()),
		Ownership:  tauv1alpha1.ClusterOwnershipManage,
	}
}
