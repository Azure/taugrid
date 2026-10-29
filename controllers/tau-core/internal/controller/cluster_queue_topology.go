// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TauClusterReconciler) reconcileOptedInQueueTopologies(
	ctx context.Context,
	mutate bool,
) ([]tauv1alpha1.TauManagedResourceStatus, bool, error) {
	queues := &unstructured.UnstructuredList{}
	queues.SetGroupVersionKind(clusterQueueGVK.GroupVersion().WithKind("ClusterQueueList"))
	if err := r.List(ctx, queues, client.MatchingLabels{labelkeys.LabelUseGPUTopology: "true"}); err != nil {
		return nil, false, fmt.Errorf("list ClusterQueues enabled for GPU topology: %w", err)
	}
	sort.Slice(queues.Items, func(i, j int) bool { return queues.Items[i].GetName() < queues.Items[j].GetName() })
	var managed []tauv1alpha1.TauManagedResourceStatus
	managedNames := map[string]bool{}
	var reconcileErr error
	drifted := false
	for i := range queues.Items {
		resources, changed, err := r.reconcileQueueTopology(ctx, &queues.Items[i], mutate)
		for _, resource := range resources {
			if !managedNames[resource.Name] {
				managed = append(managed, resource)
				managedNames[resource.Name] = true
			}
		}
		drifted = drifted || changed
		reconcileErr = errors.Join(reconcileErr, err)
	}
	return managed, drifted, reconcileErr
}

func (r *TauClusterReconciler) reconcileQueueTopology(
	ctx context.Context,
	queue *unstructured.Unstructured,
	mutate bool,
) ([]tauv1alpha1.TauManagedResourceStatus, bool, error) {
	quotas, err := clusterQueueNominalQuota(queue)
	if err != nil {
		return nil, true, err
	}
	cohort, err := optionalNestedString(queue, "spec", "cohort")
	if err != nil {
		return nil, true, fmt.Errorf("read ClusterQueue %q cohort: %w", queue.GetName(), err)
	}
	if cohort != "" {
		return nil, true, fmt.Errorf("ClusterQueue %q belongs to cohort %q; migrate flavor references through the cohort quota authority before opting in", queue.GetName(), cohort)
	}
	if queue.GetLabels()[labelManagedBy] == labelManagedByValue && queue.GetLabels()[labelWorkspace] != "" {
		return nil, true, fmt.Errorf("ClusterQueue %q is managed by TauWorkspace %q; configure topology-bound flavors through the owning TauTeam and TauWorkspace quota instead", queue.GetName(), queue.GetLabels()[labelWorkspace])
	}
	copies := map[string]*unstructured.Unstructured{}
	replacements := map[string]string{}
	var managed []tauv1alpha1.TauManagedResourceStatus
	var gpuFlavors []string
	for key, quota := range quotas {
		name, gpu := strings.CutPrefix(key, nvidiaGPUResourceName+"\x00")
		if !gpu || quota.IsZero() {
			continue
		}
		if quota.Sign() < 0 {
			return nil, true, fmt.Errorf("ClusterQueue %q flavor %q has negative GPU quota", queue.GetName(), name)
		}
		gpuFlavors = append(gpuFlavors, name)
	}
	sort.Strings(gpuFlavors)
	for _, name := range gpuFlavors {
		source := newQueueObject(resourceFlavorGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: name}, source); err != nil {
			return nil, true, fmt.Errorf("read ClusterQueue %q ResourceFlavor %q: %w", queue.GetName(), name, err)
		}
		topologyName, err := optionalNestedString(source, "spec", "topologyName")
		if err != nil {
			return nil, true, fmt.Errorf("read ResourceFlavor %q topologyName: %w", name, err)
		}
		if topologyName == tauGPUNodeTopologyName {
			if source.GetLabels()[labelManagedBy] == labelManagedByValue &&
				source.GetLabels()[labelkeys.LabelUseGPUTopology] == "true" {
				managed = append(managed, managedResourceStatus(resourceFlavorGVK, source))
			}
			continue
		}
		desired, err := topologyFlavorCopy(source)
		if err != nil {
			return nil, true, err
		}
		copies[desired.GetName()] = desired
		replacements[name] = desired.GetName()
	}
	if len(copies) == 0 || !mutate {
		return managed, len(copies) > 0, nil
	}
	if err := queueTopologyMigrationSafe(queue); err != nil {
		return managed, true, err
	}
	if err := replaceQueueTopologyFlavors(queue, replacements); err != nil {
		return managed, true, err
	}
	names := make([]string, 0, len(copies))
	for name := range copies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		desired := copies[name]
		current := newQueueObject(resourceFlavorGVK)
		err := r.Get(ctx, client.ObjectKey{Name: name}, current)
		switch {
		case apierrors.IsNotFound(err):
			if err := r.Create(ctx, desired); err != nil {
				return managed, true, fmt.Errorf("create topology ResourceFlavor %q: %w", name, err)
			}
			current = desired
		case err != nil:
			return managed, true, fmt.Errorf("read topology ResourceFlavor %q: %w", name, err)
		default:
			if current.GetLabels()[labelManagedBy] != labelManagedByValue ||
				current.GetLabels()[labelkeys.LabelUseGPUTopology] != "true" ||
				!reflect.DeepEqual(current.Object["spec"], desired.Object["spec"]) {
				return managed, true, fmt.Errorf("topology ResourceFlavor %q has an ownership conflict or immutable spec drift", name)
			}
		}
		managed = append(managed, managedResourceStatus(resourceFlavorGVK, current))
	}
	if err := r.Update(ctx, queue); err != nil {
		return managed, true, fmt.Errorf("update ClusterQueue %q topology flavors: %w", queue.GetName(), err)
	}
	return managed, false, nil
}

func replaceQueueTopologyFlavors(queue *unstructured.Unstructured, replacements map[string]string) error {
	groups, _, _ := unstructured.NestedSlice(queue.Object, "spec", "resourceGroups")
	for i, raw := range groups {
		group := raw.(map[string]any)
		flavors, _, _ := unstructured.NestedSlice(group, "flavors")
		for j, raw := range flavors {
			flavor := raw.(map[string]any)
			if replacement, found := replacements[fmt.Sprint(flavor["name"])]; found {
				flavor["name"] = replacement
				flavors[j] = flavor
			}
		}
		if err := unstructured.SetNestedSlice(group, flavors, "flavors"); err != nil {
			return fmt.Errorf("set ClusterQueue %q topology flavors: %w", queue.GetName(), err)
		}
		groups[i] = group
	}
	if err := unstructured.SetNestedSlice(queue.Object, groups, "spec", "resourceGroups"); err != nil {
		return fmt.Errorf("set ClusterQueue %q topology resourceGroups: %w", queue.GetName(), err)
	}
	return replaceTopologyAdmissionCheckFlavors(queue, replacements)
}

func replaceTopologyAdmissionCheckFlavors(queue *unstructured.Unstructured, replacements map[string]string) error {
	checks, found, err := unstructured.NestedSlice(queue.Object, "spec", "admissionChecksStrategy", "admissionChecks")
	if err != nil {
		return fmt.Errorf("read ClusterQueue %q admissionChecksStrategy: %w", queue.GetName(), err)
	}
	if !found {
		return nil
	}
	for i, raw := range checks {
		check, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("ClusterQueue %q admission check %d is malformed", queue.GetName(), i)
		}
		flavors, found, err := unstructured.NestedStringSlice(check, "onFlavors")
		if err != nil {
			return fmt.Errorf("read ClusterQueue %q admission check %d onFlavors: %w", queue.GetName(), i, err)
		}
		if !found {
			continue
		}
		for j, name := range flavors {
			if replacement, ok := replacements[name]; ok {
				flavors[j] = replacement
			}
		}
		if err := unstructured.SetNestedStringSlice(check, flavors, "onFlavors"); err != nil {
			return fmt.Errorf("set ClusterQueue %q admission check %d onFlavors: %w", queue.GetName(), i, err)
		}
		checks[i] = check
	}
	if err := unstructured.SetNestedSlice(queue.Object, checks, "spec", "admissionChecksStrategy", "admissionChecks"); err != nil {
		return fmt.Errorf("set ClusterQueue %q admissionChecksStrategy: %w", queue.GetName(), err)
	}
	return nil
}

func topologyFlavorCopy(source *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	spec, found, err := unstructured.NestedMap(source.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("ResourceFlavor %q has no readable spec", source.GetName())
	}
	spec["topologyName"] = tauGPUNodeTopologyName
	copy := newQueueObject(resourceFlavorGVK)
	copy.SetName(networkDomainLabel("taugrid-topology", source.GetName()))
	copy.SetLabels(map[string]string{
		labelManagedBy:                labelManagedByValue,
		labelkeys.LabelUseGPUTopology: "true",
	})
	copy.Object["spec"] = spec
	return copy, nil
}

func queueTopologyMigrationSafe(queue *unstructured.Unstructured) error {
	policy, err := optionalNestedString(queue, "spec", "stopPolicy")
	if err != nil {
		return fmt.Errorf("read ClusterQueue %q stopPolicy: %w", queue.GetName(), err)
	}
	if policy != "Hold" && policy != "HoldAndDrain" {
		return fmt.Errorf("ClusterQueue %q GPU topology migration requires spec.stopPolicy=Hold and no active reservations; pause the queue before opting in", queue.GetName())
	}
	conditions, _, err := unstructured.NestedSlice(queue.Object, "status", "conditions")
	if err != nil {
		return fmt.Errorf("read ClusterQueue %q conditions: %w", queue.GetName(), err)
	}
	stopped := false
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("ClusterQueue %q has a malformed condition", queue.GetName())
		}
		if condition["type"] == "Active" && condition["status"] == string(metav1.ConditionFalse) && condition["reason"] == "Stopped" {
			if queue.GetGeneration() > 0 {
				observed, found, err := unstructured.NestedInt64(condition, "observedGeneration")
				if err != nil {
					return fmt.Errorf("read ClusterQueue %q Active observedGeneration: %w", queue.GetName(), err)
				}
				if !found || observed != queue.GetGeneration() {
					continue
				}
			}
			stopped = true
		}
	}
	if !stopped {
		return fmt.Errorf("ClusterQueue %q GPU topology migration is waiting for Kueue to report Active=False with reason Stopped", queue.GetName())
	}
	reservations, err := clusterQueueReservations(queue)
	if err != nil {
		return err
	}
	for _, quantity := range reservations {
		if !quantity.IsZero() {
			return fmt.Errorf("ClusterQueue %q GPU topology migration is waiting for active reservations to finish", queue.GetName())
		}
	}
	for _, field := range []string{"reservingWorkloads", "admittedWorkloads"} {
		count, _, err := unstructured.NestedInt64(queue.Object, "status", field)
		if err != nil {
			return fmt.Errorf("read ClusterQueue %q %s: %w", queue.GetName(), field, err)
		}
		if count != 0 {
			return fmt.Errorf("ClusterQueue %q GPU topology migration is waiting for %s to reach zero", queue.GetName(), field)
		}
	}
	return nil
}
