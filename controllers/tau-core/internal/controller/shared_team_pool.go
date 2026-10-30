// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	sharedTeamCohortName      = "tau-shared-teams"
	labelSharedTeamQuotas     = labelkeys.LabelSharedTeamQuotas
	annotationSharedQuotaHold = labelkeys.AnnotationSharedQuotaHold
)

type sharedTeamAllocation struct {
	team    *tauv1alpha1.TauTeam
	current map[string]resource.Quantity
	desired []tauv1alpha1.TauResourceQuota
}

func (r *TauTeamReconciler) reconcileSharedTeamPool(ctx context.Context, cluster *tauv1alpha1.TauCluster) error {
	err := r.applySharedTeamPool(ctx, cluster)
	if err != nil {
		return errors.Join(err, r.holdSharedTeamQueues(ctx))
	}
	return nil
}

func (r *TauTeamReconciler) holdSharedTeamQueues(ctx context.Context) error {
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := r.quotaReader().List(ctx, &workspaces, client.InNamespace(systemNamespace(r.SystemNamespace))); err != nil {
		return err
	}
	var result error
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		queue := newQueueObject(clusterQueueGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, queue); err != nil {
			result = errors.Join(result, client.IgnoreNotFound(err))
			continue
		}
		owned, err := authoritativeWorkspaceClusterQueue(queue, workspace)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if !owned {
			continue
		}
		policy, _, err := unstructured.NestedString(queue.Object, "spec", "stopPolicy")
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if policy == "Hold" || policy == "HoldAndDrain" {
			continue
		}
		if err := unstructured.SetNestedField(queue.Object, "Hold", "spec", "stopPolicy"); err != nil {
			result = errors.Join(result, err)
			continue
		}
		annotations := queue.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[annotationSharedQuotaHold] = "true"
		queue.SetAnnotations(annotations)
		result = errors.Join(result, r.Update(ctx, queue))
	}
	return result
}

func (r *TauTeamReconciler) applySharedTeamPool(ctx context.Context, cluster *tauv1alpha1.TauCluster) error {
	if cluster.Spec.Queues.CapacityTeamRef != nil || cluster.Spec.ManagementMode != tauv1alpha1.ClusterManagementModeReconcile ||
		!cluster.DeletionTimestamp.IsZero() || cluster.UID == "" {
		return fmt.Errorf("shared team quotas require a non-deleting TauCluster with a UID in Reconcile mode and no capacityTeamRef")
	}
	inventory, err := discoveredPoolInventory(ctx, r.quotaReader(), cluster)
	if err != nil {
		return err
	}
	for _, flavor := range inventory.flavors {
		object := newQueueObject(resourceFlavorGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: flavor.name}, object); err != nil {
			return err
		}
		if err := validateDiscoveredGPUFlavor(object, flavor); err != nil {
			return err
		}
	}
	var teams tauv1alpha1.TauTeamList
	if err := r.quotaReader().List(ctx, &teams, client.InNamespace(systemNamespace(r.SystemNamespace))); err != nil {
		return err
	}
	sort.Slice(teams.Items, func(i, j int) bool { return teams.Items[i].Name < teams.Items[j].Name })
	supply := quotaNominals(inventory.quotas)
	allocated := map[string]resource.Quantity{}
	transitional := map[string]resource.Quantity{}
	teamNames := map[string]*tauv1alpha1.TauTeam{}
	allocations := make([]sharedTeamAllocation, 0, len(teams.Items))
	for i := range teams.Items {
		team := &teams.Items[i]
		if team.UID == "" || !controllerutil.ContainsFinalizer(team, teamFinalizer) || len(team.Spec.Quota) == 0 {
			return fmt.Errorf("shared capacity team %q requires its UID, cleanup finalizer and explicit spec.quota", team.Name)
		}
		if err := validateQuotaValues(team.Spec.Quota); err != nil {
			return err
		}
		current := map[string]resource.Quantity{}
		cohort := newQueueObject(cohortGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); err == nil {
			if err := validateSharedPoolOwner(cohort, labelTeam, team.Name, string(team.UID)); err != nil {
				return err
			}
			current, err = clusterQueueNominalQuota(cohort)
			if err != nil {
				return err
			}
			if cohort.GetAnnotations()[annotationCapacityPool] == "true" {
				for _, quantity := range current {
					if quantity.Sign() > 0 {
						return fmt.Errorf("team %q retains an exclusive capacity budget; drain and zero it before migration", team.Name)
					}
				}
			}
			parent, _, err := unstructured.NestedString(cohort.Object, "spec", "parentName")
			if err != nil {
				return err
			}
			if parent != "" && parent != sharedTeamCohortName {
				return fmt.Errorf("team %q Cohort belongs to another parent", team.Name)
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		requested := map[string]tauv1alpha1.TauResourceQuota{}
		for _, quota := range team.Spec.Quota {
			if quota.LendingLimit != nil && quota.LendingLimit.Cmp(quota.NominalQuota) > 0 {
				return fmt.Errorf("team %q lending limit exceeds its guarantee", team.Name)
			}
			key := quotaKey(quota)
			if _, found := supply[key]; !found {
				return fmt.Errorf("team %q quota %q is outside discovered capacity", team.Name, key)
			}
			requested[key] = quota
		}
		desired := make([]tauv1alpha1.TauResourceQuota, 0, len(supply))
		for _, capacity := range inventory.quotas {
			key := quotaKey(capacity)
			quota, found := requested[key]
			if !found {
				quota = tauv1alpha1.TauResourceQuota{Flavor: capacity.Flavor, Resource: capacity.Resource}
			}
			desired = append(desired, quota)
			addQuotaQuantity(allocated, key, quota.NominalQuota)
			amount := quota.NominalQuota
			if previous := current[key]; previous.Cmp(amount) > 0 {
				amount = previous
			}
			addQuotaQuantity(transitional, key, amount)
		}
		for key, quantity := range current {
			if _, found := supply[key]; !found && quantity.Sign() > 0 {
				return fmt.Errorf("team %q has an applied quota outside discovered capacity", team.Name)
			}
		}
		allocations = append(allocations, sharedTeamAllocation{team: team, current: current, desired: desired})
		teamNames[team.Name] = team
	}
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := r.quotaReader().List(ctx, &workspaces, client.InNamespace(systemNamespace(r.SystemNamespace))); err != nil {
		return err
	}
	selected := map[string]*tauv1alpha1.TauWorkspace{}
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		if workspace.Spec.TeamRef == nil {
			continue
		}
		if teamNames[workspace.Spec.TeamRef.Name] == nil || len(workspace.Spec.Quota) != 0 ||
			workspace.UID == "" || !controllerutil.ContainsFinalizer(workspace, workspaceFinalizer) {
			return fmt.Errorf("shared capacity workspace %q requires an existing team, a UID, its cleanup finalizer and no spec.quota", workspace.Name)
		}
		selected[workspaceClusterQueueName(workspace.Name)] = workspace
	}
	if err := r.validateSharedPoolConflicts(ctx, cluster, teamNames, selected, inventory); err != nil {
		return err
	}
	reservations := map[string]resource.Quantity{}
	teamReservations := map[string]map[string]resource.Quantity{}
	for name, workspace := range selected {
		queue := newQueueObject(clusterQueueGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: name}, queue); err == nil {
			usage, err := clusterQueueReservations(queue)
			if err != nil {
				return err
			}
			team := workspace.Spec.TeamRef.Name
			if teamReservations[team] == nil {
				teamReservations[team] = map[string]resource.Quantity{}
			}
			for key, quantity := range usage {
				addQuotaQuantity(reservations, key, quantity)
				addQuotaQuantity(teamReservations[team], key, quantity)
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	for _, allocation := range allocations {
		for _, quota := range allocation.desired {
			if quota.BorrowingLimit != nil {
				maximum := quota.NominalQuota.DeepCopy()
				maximum.Add(*quota.BorrowingLimit)
				if reserved := teamReservations[allocation.team.Name][quotaKey(quota)]; reserved.Cmp(maximum) > 0 {
					return fmt.Errorf("team %q maximum would fall below active reservations; wait for borrowed jobs to finish", allocation.team.Name)
				}
			}
		}
	}
	root := desiredSharedTeamRoot(cluster, inventory.quotas)
	existing := newQueueObject(cohortGVK)
	currentResidual := map[string]resource.Quantity{}
	if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: sharedTeamCohortName}, existing); err == nil {
		if err := validateSharedPoolOwner(existing, labelSharedTeamQuotas, cluster.Name, string(cluster.UID)); err != nil {
			return err
		}
		currentResidual, err = clusterQueueNominalQuota(existing)
		if err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	intermediate := make([]tauv1alpha1.TauResourceQuota, len(inventory.quotas))
	residual := make([]tauv1alpha1.TauResourceQuota, len(inventory.quotas))
	var shortage error
	for i, quota := range inventory.quotas {
		key := quotaKey(quota)
		remaining := quota.NominalQuota.DeepCopy()
		remaining.Sub(allocated[key])
		if remaining.Sign() < 0 {
			shortage = errors.Join(shortage, fmt.Errorf("team guarantees exceed discovered capacity for %q", key))
			remaining = resource.Quantity{}
		}
		residual[i] = tauv1alpha1.TauResourceQuota{Flavor: quota.Flavor, Resource: quota.Resource, NominalQuota: remaining}
		available := quota.NominalQuota.DeepCopy()
		available.Sub(transitional[key])
		if available.Sign() < 0 {
			available = resource.Quantity{}
		}
		if current := currentResidual[key]; current.Cmp(available) < 0 {
			available = current.DeepCopy()
		}
		intermediate[i] = tauv1alpha1.TauResourceQuota{Flavor: quota.Flavor, Resource: quota.Resource, NominalQuota: available}
	}
	root.Object["spec"].(map[string]any)["resourceGroups"] = quotaResourceGroups(intermediate, false)
	if err := reconcileManagedUnstructuredWithReader(ctx, r.Client, r.quotaReader(), root, labelSharedTeamQuotas, cluster.Name); err != nil {
		return err
	}
	if shortage != nil {
		return shortage
	}
	for _, allocation := range allocations {
		reduced := make([]tauv1alpha1.TauResourceQuota, len(allocation.desired))
		for i, quota := range allocation.desired {
			reduced[i] = quota
			if current := allocation.current[quotaKey(quota)]; current.Cmp(quota.NominalQuota) < 0 {
				reduced[i].NominalQuota = current.DeepCopy()
			}
			if quota.LendingLimit != nil && quota.LendingLimit.Cmp(reduced[i].NominalQuota) > 0 {
				limit := reduced[i].NominalQuota.DeepCopy()
				reduced[i].LendingLimit = &limit
			}
		}
		if err := r.applySharedTeamCohort(ctx, allocation.team, reduced); err != nil {
			return err
		}
	}
	for _, allocation := range allocations {
		if err := r.applySharedTeamCohort(ctx, allocation.team, allocation.desired); err != nil {
			return err
		}
	}
	root.Object["spec"].(map[string]any)["resourceGroups"] = quotaResourceGroups(residual, false)
	if err := reconcileManagedUnstructuredWithReader(ctx, r.Client, r.quotaReader(), root, labelSharedTeamQuotas, cluster.Name); err != nil {
		return err
	}
	for key, quantity := range reservations {
		if capacity := supply[key]; quantity.Cmp(capacity) > 0 {
			return fmt.Errorf("active reservations exceed discovered capacity for %q; admitted jobs are retained", key)
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if selected[name].DeletionTimestamp.IsZero() {
			if err := r.reconcileCapacityWorkspace(ctx, selected[name], inventory.quotas, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *TauTeamReconciler) applySharedTeamCohort(ctx context.Context, team *tauv1alpha1.TauTeam, quotas []tauv1alpha1.TauResourceQuota) error {
	var current tauv1alpha1.TauTeam
	if err := r.quotaReader().Get(ctx, client.ObjectKeyFromObject(team), &current); err != nil {
		return err
	}
	if current.UID != team.UID || !apiequality.Semantic.DeepEqual(current.Spec, team.Spec) ||
		!current.DeletionTimestamp.Equal(team.DeletionTimestamp) {
		return fmt.Errorf("team %q changed during shared quota accounting; retry reconciliation", team.Name)
	}
	cohort := desiredTeamCohort(team, quotas)
	cohort.Object["spec"].(map[string]any)["parentName"] = sharedTeamCohortName
	groups := quotaResourceGroups(quotas, false)
	byKey := map[string]tauv1alpha1.TauResourceQuota{}
	for _, quota := range quotas {
		byKey[quotaKey(quota)] = quota
	}
	for _, rawGroup := range groups {
		for _, rawFlavor := range rawGroup.(map[string]any)["flavors"].([]any) {
			flavor := rawFlavor.(map[string]any)
			for _, rawResource := range flavor["resources"].([]any) {
				entry := rawResource.(map[string]any)
				quota := byKey[fmt.Sprint(entry["name"])+"\x00"+fmt.Sprint(flavor["name"])]
				if quota.BorrowingLimit != nil {
					entry["borrowingLimit"] = quota.BorrowingLimit.String()
				}
				if quota.LendingLimit != nil {
					entry["lendingLimit"] = quota.LendingLimit.String()
				}
			}
		}
	}
	cohort.Object["spec"].(map[string]any)["resourceGroups"] = groups
	return reconcileManagedUnstructuredWithReader(ctx, r.Client, r.quotaReader(), cohort, labelTeam, team.Name)
}

func desiredSharedTeamRoot(cluster *tauv1alpha1.TauCluster, quotas []tauv1alpha1.TauResourceQuota) *unstructured.Unstructured {
	root := newQueueObject(cohortGVK)
	root.SetName(sharedTeamCohortName)
	root.SetLabels(map[string]string{labelManagedBy: labelManagedByValue, labelSharedTeamQuotas: cluster.Name})
	setOwnerUIDAnnotation(root, cluster.UID)
	root.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups(quotas, false)}
	return root
}

func validateSharedPoolOwner(object *unstructured.Unstructured, label, name, uid string) error {
	if object.GetLabels()[labelManagedBy] != labelManagedByValue || object.GetLabels()[label] != name ||
		object.GetAnnotations()[annotationOwnerUID] != uid {
		return fmt.Errorf("%s %q does not have authoritative shared quota ownership", object.GetKind(), object.GetName())
	}
	return nil
}

func quotaNominals(quotas []tauv1alpha1.TauResourceQuota) map[string]resource.Quantity {
	out := make(map[string]resource.Quantity, len(quotas))
	for _, quota := range quotas {
		out[quotaKey(quota)] = quota.NominalQuota.DeepCopy()
	}
	return out
}

func addQuotaQuantity(values map[string]resource.Quantity, key string, quantity resource.Quantity) {
	total := values[key]
	total.Add(quantity)
	values[key] = total
}

func (r *TauTeamReconciler) validateSharedPoolConflicts(ctx context.Context, cluster *tauv1alpha1.TauCluster,
	teams map[string]*tauv1alpha1.TauTeam, workspaces map[string]*tauv1alpha1.TauWorkspace, inventory *capacityPoolInventory) error {
	overlaps := func(name string) (bool, error) {
		for _, quota := range inventory.quotas {
			if quota.Flavor == name {
				return true, nil
			}
		}
		flavor := newQueueObject(resourceFlavorGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: name}, flavor); err != nil {
			return false, err
		}
		selector, _, err := unstructured.NestedStringMap(flavor.Object, "spec", "nodeLabels")
		if err != nil {
			return false, err
		}
		for _, node := range inventory.nodes {
			matches := true
			for key, value := range selector {
				matches = matches && node.Labels[key] == value
			}
			if matches {
				return true, nil
			}
		}
		return false, nil
	}
	for _, gvk := range []struct{ queue bool }{{true}, {false}} {
		kind := cohortGVK
		if gvk.queue {
			kind = clusterQueueGVK
		}
		objects := &unstructured.UnstructuredList{}
		objects.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
		if err := r.quotaReader().List(ctx, objects); err != nil {
			return err
		}
		for i := range objects.Items {
			object := &objects.Items[i]
			if !gvk.queue {
				if object.GetName() == sharedTeamCohortName {
					if err := validateSharedPoolOwner(object, labelSharedTeamQuotas, cluster.Name, string(cluster.UID)); err != nil {
						return err
					}
					parent, _, err := unstructured.NestedString(object.Object, "spec", "parentName")
					if err != nil {
						return err
					}
					if parent != "" {
						return fmt.Errorf("shared team root must not have a parent")
					}
					continue
				}
				if team := teams[object.GetLabels()[labelTeam]]; team != nil && object.GetName() == teamCohortName(team.Name) {
					continue
				}
			}
			workspace := workspaces[object.GetName()]
			if gvk.queue && workspace != nil {
				owned, err := authoritativeWorkspaceClusterQueue(object, workspace)
				if err != nil {
					return err
				}
				if !owned || object.GetAnnotations()[annotationOwnerUID] != string(workspace.UID) {
					return fmt.Errorf("shared workspace queue %q has no authoritative ownership", object.GetName())
				}
				nominals, err := clusterQueueNominalQuota(object)
				if err != nil {
					return err
				}
				for _, quantity := range nominals {
					if quantity.Sign() != 0 {
						return fmt.Errorf("workspace queue %q has a duplicate nominal budget; drain and zero it before migration", object.GetName())
					}
				}
				cohort, _, err := unstructured.NestedString(object.Object, "spec", "cohortName")
				if err != nil {
					return err
				}
				appliedTeam := object.GetLabels()[labelTeam]
				if cohort != "" && (teams[appliedTeam] == nil || cohort != teamCohortName(appliedTeam)) {
					return fmt.Errorf("workspace queue %q belongs to an external Cohort", object.GetName())
				}
				if object.GetLabels()[labelDiscoverGPUFlavors] == "true" {
					return fmt.Errorf("shared workspace queue %q enables legacy discovery", object.GetName())
				}
				continue
			}
			if err := validateCapacityPoolObject(object, gvk.queue, "", nil, overlaps); err != nil {
				return err
			}
		}
	}
	return nil
}
