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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func teamCohortName(team string) string {
	return networkDomainLabel("tau-team", team)
}

func workspaceClusterQueueName(workspace string) string {
	return networkDomainLabel("tau-ws", workspace)
}

func quotaKey(quota tauv1alpha1.TauResourceQuota) string {
	return quota.Resource + "\x00" + quota.Flavor
}

func normalizedQuotaValue(value *resource.Quantity) string {
	if value == nil {
		return "0"
	}
	return value.String()
}

func validateQuotaFlavors(ctx context.Context, reader client.Reader, quotas []tauv1alpha1.TauResourceQuota) error {
	if err := validateQuotaValues(quotas); err != nil {
		return err
	}
	for _, quota := range quotas {
		flavor := newQueueObject(resourceFlavorGVK)
		if err := reader.Get(ctx, client.ObjectKey{Name: quota.Flavor}, flavor); err != nil {
			return fmt.Errorf("ResourceFlavor %q is not ready: %w", quota.Flavor, err)
		}
	}
	return nil
}

func validateQuotaValues(quotas []tauv1alpha1.TauResourceQuota) error {
	resourcesByFlavor := map[string]map[string]struct{}{}
	for _, quota := range quotas {
		if quota.NominalQuota.Sign() < 0 {
			return fmt.Errorf("quota for flavor %q resource %q must be non-negative", quota.Flavor, quota.Resource)
		}
		for name, value := range map[string]*resource.Quantity{
			"borrowingLimit": quota.BorrowingLimit,
			"lendingLimit":   quota.LendingLimit,
		} {
			if value != nil && value.Sign() < 0 {
				return fmt.Errorf("%s for flavor %q resource %q must be non-negative", name, quota.Flavor, quota.Resource)
			}
		}
		resources := resourcesByFlavor[quota.Flavor]
		if resources == nil {
			resources = map[string]struct{}{}
			resourcesByFlavor[quota.Flavor] = resources
		}
		if _, exists := resources[quota.Resource]; exists {
			return fmt.Errorf("quota for flavor %q resource %q is duplicated", quota.Flavor, quota.Resource)
		}
		resources[quota.Resource] = struct{}{}
	}
	flavors := make([]string, 0, len(resourcesByFlavor))
	for flavor := range resourcesByFlavor {
		flavors = append(flavors, flavor)
	}
	sort.Strings(flavors)
	for i, firstFlavor := range flavors {
		firstResources := resourcesByFlavor[firstFlavor]
		for _, secondFlavor := range flavors[i+1:] {
			secondResources := resourcesByFlavor[secondFlavor]
			overlap := false
			for resourceName := range firstResources {
				if _, exists := secondResources[resourceName]; exists {
					overlap = true
					break
				}
			}
			if overlap && !reflect.DeepEqual(firstResources, secondResources) {
				return fmt.Errorf(
					"flavors %q and %q must cover identical or disjoint resource sets",
					firstFlavor,
					secondFlavor,
				)
			}
		}
	}
	return nil
}

func quotaResourceGroups(quotas []tauv1alpha1.TauResourceQuota, includeLimits bool) []any {
	byFlavor := map[string][]tauv1alpha1.TauResourceQuota{}
	for _, quota := range quotas {
		byFlavor[quota.Flavor] = append(byFlavor[quota.Flavor], quota)
	}
	type quotaGroup struct {
		resources []string
		flavors   []string
	}
	byResourceSet := map[string]*quotaGroup{}
	for flavorName, entries := range byFlavor {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Resource < entries[j].Resource
		})
		resources := make([]string, 0, len(entries))
		for _, entry := range entries {
			resources = append(resources, entry.Resource)
		}
		key := strings.Join(resources, "\x00")
		group := byResourceSet[key]
		if group == nil {
			group = &quotaGroup{resources: resources}
			byResourceSet[key] = group
		}
		group.flavors = append(group.flavors, flavorName)
	}
	keys := make([]string, 0, len(byResourceSet))
	for key := range byResourceSet {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	groups := make([]any, 0, len(keys))
	for _, key := range keys {
		group := byResourceSet[key]
		sort.Strings(group.flavors)
		flavors := make([]any, 0, len(group.flavors))
		for _, flavorName := range group.flavors {
			resources := make([]any, 0, len(byFlavor[flavorName]))
			for _, quota := range byFlavor[flavorName] {
				resourceQuota := map[string]any{
					"name":         quota.Resource,
					"nominalQuota": quota.NominalQuota.String(),
				}
				if includeLimits {
					resourceQuota["borrowingLimit"] = normalizedQuotaValue(quota.BorrowingLimit)
					resourceQuota["lendingLimit"] = normalizedQuotaValue(quota.LendingLimit)
				}
				resources = append(resources, resourceQuota)
			}
			flavors = append(flavors, map[string]any{
				"name":      flavorName,
				"resources": resources,
			})
		}
		coveredResources := make([]any, 0, len(group.resources))
		for _, resourceName := range group.resources {
			coveredResources = append(coveredResources, resourceName)
		}
		groups = append(groups, map[string]any{
			"coveredResources": coveredResources,
			"flavors":          flavors,
		})
	}
	return groups
}

func clusterQueueReservations(queue *unstructured.Unstructured) (map[string]resource.Quantity, error) {
	reservations, found, err := unstructured.NestedSlice(queue.Object, "status", "flavorsReservation")
	if err != nil {
		return nil, fmt.Errorf("read ClusterQueue %q reservations: %w", queue.GetName(), err)
	}
	out := map[string]resource.Quantity{}
	if !found {
		return out, nil
	}
	for _, rawFlavor := range reservations {
		flavor, ok := rawFlavor.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ClusterQueue %q has malformed reservation status", queue.GetName())
		}
		flavorName := fmt.Sprint(flavor["name"])
		resources, _, err := unstructured.NestedSlice(flavor, "resources")
		if err != nil {
			return nil, fmt.Errorf("ClusterQueue %q has malformed reservation resources: %w", queue.GetName(), err)
		}
		for _, rawResource := range resources {
			reservation, ok := rawResource.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ClusterQueue %q has malformed reservation resource", queue.GetName())
			}
			resourceName := fmt.Sprint(reservation["name"])
			total, err := resource.ParseQuantity(fmt.Sprint(reservation["total"]))
			if err != nil {
				return nil, fmt.Errorf(
					"ClusterQueue %q has invalid reservation for flavor %q resource %q: %w",
					queue.GetName(), flavorName, resourceName, err,
				)
			}
			out[resourceName+"\x00"+flavorName] = total
		}
	}
	return out, nil
}

func desiredTeamCohort(team *tauv1alpha1.TauTeam, sharedQuota []tauv1alpha1.TauResourceQuota) *unstructured.Unstructured {
	cohort := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cohortGVK.GroupVersion().String(),
		"kind":       cohortGVK.Kind,
		"metadata": map[string]any{
			"name": teamCohortName(team.Name),
		},
		"spec": map[string]any{
			"resourceGroups": quotaResourceGroups(sharedQuota, false),
		},
	}}
	cohort.SetGroupVersionKind(cohortGVK)
	cohort.SetLabels(teamLabels(team.Name))
	setOwnerUIDAnnotation(cohort, team.UID)
	return cohort
}

func desiredWorkspaceClusterQueue(workspace *tauv1alpha1.TauWorkspace) *unstructured.Unstructured {
	name := workspaceClusterQueueName(workspace.Name)
	queue := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": clusterQueueGVK.GroupVersion().String(),
		"kind":       clusterQueueGVK.Kind,
		"metadata": map[string]any{
			"name": name,
		},
		"spec": map[string]any{
			"cohortName":       teamCohortName(workspace.Spec.TeamRef.Name),
			"queueingStrategy": "BestEffortFIFO",
			"namespaceSelector": map[string]any{
				"matchLabels": map[string]any{labelWorkspace: workspace.Name},
			},
			"preemption": map[string]any{
				"withinClusterQueue":  "LowerPriority",
				"reclaimWithinCohort": "Never",
				"borrowWithinCohort": map[string]any{
					"policy": "Never",
				},
			},
			"resourceGroups": quotaResourceGroups(workspace.Spec.Quota, true),
		},
	}}
	queue.SetGroupVersionKind(clusterQueueGVK)
	labels := workspaceLabels(workspace.Name)
	labels[labelTeam] = workspace.Spec.TeamRef.Name
	queue.SetLabels(labels)
	setOwnerUIDAnnotation(queue, workspace.UID)
	return queue
}

func reconcileManagedUnstructured(
	ctx context.Context,
	kubeClient client.Client,
	desired *unstructured.Unstructured,
	ownerLabel, ownerName string,
) error {
	existing := newQueueObject(desired.GroupVersionKind())
	key := client.ObjectKeyFromObject(desired)
	if err := kubeClient.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return kubeClient.Create(ctx, desired)
		}
		return err
	}
	labels := existing.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue || labels[ownerLabel] != ownerName {
		return fmt.Errorf("%s %q already exists and is not owned by %s %q", desired.GetKind(), desired.GetName(), ownerLabel, ownerName)
	}
	if desiredUID := desired.GetAnnotations()[annotationOwnerUID]; desiredUID != "" {
		existingUID := existing.GetAnnotations()[annotationOwnerUID]
		if existingUID != "" && existingUID != desiredUID {
			return fmt.Errorf("%s %q belongs to a different owner UID %q", desired.GetKind(), desired.GetName(), existingUID)
		}
		if existingUID == "" && existing.GetUID() != "" {
			return fmt.Errorf(
				"%s %q has legacy ownership metadata without an owner UID; explicit operator adoption is required",
				desired.GetKind(),
				desired.GetName(),
			)
		}
	}
	existingSpec, _, err := unstructured.NestedMap(existing.Object, "spec")
	if err != nil {
		return err
	}
	desiredSpec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existingSpec, desiredSpec) &&
		reflect.DeepEqual(existing.GetLabels(), desired.GetLabels()) &&
		reflect.DeepEqual(existing.GetAnnotations(), desired.GetAnnotations()) {
		return nil
	}
	if desired.GroupVersionKind() == clusterQueueGVK {
		if err := validateClusterQueueQuotaReduction(existing, desired); err != nil {
			return err
		}
	}
	if err := unstructured.SetNestedMap(existing.Object, desiredSpec, "spec"); err != nil {
		return err
	}
	existing.SetLabels(desired.GetLabels())
	existing.SetAnnotations(desired.GetAnnotations())
	return kubeClient.Update(ctx, existing)
}

func setOwnerUIDAnnotation(object metav1.Object, uid types.UID) {
	if uid == "" {
		return
	}
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[annotationOwnerUID] = string(uid)
	object.SetAnnotations(annotations)
}

func validateClusterQueueQuotaReduction(existing, desired *unstructured.Unstructured) error {
	reservations, err := clusterQueueReservations(existing)
	if err != nil {
		return err
	}
	if len(reservations) == 0 {
		return nil
	}
	maximums, err := clusterQueueMaximumQuota(desired)
	if err != nil {
		return err
	}
	for key, total := range reservations {
		maximum := maximums[key]
		if total.Cmp(maximum) > 0 {
			resourceName, flavorName, _ := strings.Cut(key, "\x00")
			return fmt.Errorf(
				"refusing to reduce ClusterQueue %q flavor %q resource %q below active reservation %s (new maximum %s)",
				existing.GetName(), flavorName, resourceName, total.String(), maximum.String(),
			)
		}
	}
	return nil
}

func clusterQueueMaximumQuota(queue *unstructured.Unstructured) (map[string]resource.Quantity, error) {
	return clusterQueueQuota(queue, true)
}

func clusterQueueNominalQuota(queue *unstructured.Unstructured) (map[string]resource.Quantity, error) {
	return clusterQueueQuota(queue, false)
}

func clusterQueueQuota(queue *unstructured.Unstructured, includeBorrowing bool) (map[string]resource.Quantity, error) {
	groups, found, err := unstructured.NestedSlice(queue.Object, "spec", "resourceGroups")
	if err != nil || !found {
		return nil, fmt.Errorf("ClusterQueue %q has no readable resourceGroups", queue.GetName())
	}
	maximums := map[string]resource.Quantity{}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ClusterQueue %q has malformed resourceGroups", queue.GetName())
		}
		flavors, _, err := unstructured.NestedSlice(group, "flavors")
		if err != nil {
			return nil, fmt.Errorf("ClusterQueue %q has malformed flavors: %w", queue.GetName(), err)
		}
		for _, rawFlavor := range flavors {
			flavor, ok := rawFlavor.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ClusterQueue %q has malformed flavor quota", queue.GetName())
			}
			flavorName := fmt.Sprint(flavor["name"])
			resources, _, err := unstructured.NestedSlice(flavor, "resources")
			if err != nil {
				return nil, fmt.Errorf("ClusterQueue %q has malformed flavor resources: %w", queue.GetName(), err)
			}
			for _, rawResource := range resources {
				quota, ok := rawResource.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("ClusterQueue %q has malformed resource quota", queue.GetName())
				}
				resourceName := fmt.Sprint(quota["name"])
				nominal, err := resource.ParseQuantity(fmt.Sprint(quota["nominalQuota"]))
				if err != nil {
					return nil, fmt.Errorf("ClusterQueue %q has invalid nominal quota: %w", queue.GetName(), err)
				}
				if includeBorrowing {
					borrowing := strings.TrimSpace(fmt.Sprint(quota["borrowingLimit"]))
					if borrowing == "" || borrowing == "<nil>" {
						maximums[resourceName+"\x00"+flavorName] = nominal
						continue
					}
					limit, err := resource.ParseQuantity(borrowing)
					if err != nil {
						return nil, fmt.Errorf("ClusterQueue %q has invalid borrowing limit: %w", queue.GetName(), err)
					}
					nominal.Add(limit)
				}
				maximums[resourceName+"\x00"+flavorName] = nominal
			}
		}
	}
	return maximums, nil
}
