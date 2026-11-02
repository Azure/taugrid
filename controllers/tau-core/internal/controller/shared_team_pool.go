// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	corev1 "k8s.io/api/core/v1"
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
	root := newQueueObject(cohortGVK)
	if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: sharedTeamCohortName}, root); err != nil {
		return fmt.Errorf("provide the platform-owned static Cohort %q before enabling shared quotas: %w", sharedTeamCohortName, err)
	}
	if root.GetLabels()[labelSharedTeamQuotas] != cluster.Name || root.GetLabels()[labelManagedBy] == labelManagedByValue {
		return fmt.Errorf("Cohort %q must be platform-owned static spare, labeled %s=%s, not an automatic TauGrid budget", root.GetName(), labelSharedTeamQuotas, cluster.Name)
	}
	spare, err := sharedTeamSpare(root)
	if err != nil {
		return err
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
	declared := map[string]bool{}
	for key, quantity := range spare {
		allocated[key] = quantity.DeepCopy()
		declared[key] = true
	}
	for _, team := range teams.Items {
		cohort := newQueueObject(cohortGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); err == nil {
			if err := validateSharedPoolOwner(cohort, labelTeam, team.Name, string(team.UID)); err != nil {
				return err
			}
			current, err := clusterQueueNominalQuota(cohort)
			if err != nil {
				return err
			}
			for key, quantity := range current {
				if quantity.IsZero() {
					declared[key] = true
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		} else if !team.DeletionTimestamp.IsZero() {
			continue
		}
		for _, quota := range team.Spec.Quota {
			declared[quotaKey(quota)] = true
		}
	}
	keys := make([]string, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	poolQuotas := make([]tauv1alpha1.TauResourceQuota, 0, len(keys))
	for _, key := range keys {
		resourceName, flavor, _ := strings.Cut(key, "\x00")
		poolQuotas = append(poolQuotas, tauv1alpha1.TauResourceQuota{Resource: resourceName, Flavor: flavor})
	}
	if err := validateQuotaValues(poolQuotas); err != nil {
		return fmt.Errorf("shared parent and team declarations have incompatible resource groups: %w", err)
	}
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
		} else if !team.DeletionTimestamp.IsZero() {
			continue
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
		desired := make([]tauv1alpha1.TauResourceQuota, 0, len(poolQuotas))
		for _, capacity := range poolQuotas {
			key := quotaKey(capacity)
			quota, found := requested[key]
			if !found {
				quota = tauv1alpha1.TauResourceQuota{Flavor: capacity.Flavor, Resource: capacity.Resource}
			}
			desired = append(desired, quota)
			addQuotaQuantity(allocated, key, quota.NominalQuota)
		}
		for key, quantity := range current {
			if _, found := supply[key]; !found && quantity.Sign() > 0 {
				return fmt.Errorf("team %q has an applied quota outside discovered capacity", team.Name)
			}
		}
		allocations = append(allocations, sharedTeamAllocation{team: team, desired: desired})
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
		team := teamNames[workspace.Spec.TeamRef.Name]
		if team == nil || len(workspace.Spec.Quota) != 0 ||
			workspace.UID == "" || !controllerutil.ContainsFinalizer(workspace, workspaceFinalizer) {
			return fmt.Errorf("shared capacity workspace %q requires an existing team, a UID, its cleanup finalizer and no spec.quota", workspace.Name)
		}
		if !team.DeletionTimestamp.IsZero() {
			cohort, _, _, _, err := r.appliedWorkspaceQueue(ctx, workspace)
			if err != nil {
				return err
			}
			if cohort != teamCohortName(team.Name) {
				return fmt.Errorf("workspace %q cannot join deleting team %q", workspace.Name, team.Name)
			}
		}
		selected[workspaceClusterQueueName(workspace.Name)] = workspace
	}
	if err := r.validateSharedPoolConflicts(ctx, cluster, teamNames, selected, inventory); err != nil {
		return err
	}
	reservations := map[string]resource.Quantity{}
	teamReservations := map[string]map[string]resource.Quantity{}
	hasQueues := false
	for name, workspace := range selected {
		queue := newQueueObject(clusterQueueGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: name}, queue); err == nil {
			hasQueues = true
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
	for key, quantity := range reservations {
		if capacity := supply[key]; quantity.Cmp(capacity) > 0 {
			return fmt.Errorf("active reservations exceed discovered capacity for %q; admitted jobs are retained", key)
		}
	}
	for i := range poolQuotas {
		key := quotaKey(poolQuotas[i])
		total := allocated[key]
		if capacity := supply[key]; total.Cmp(capacity) > 0 {
			return fmt.Errorf("fixed team guarantees plus static spare exceed discovered capacity for %q; quotas are unchanged", key)
		}
		poolQuotas[i].NominalQuota = total.DeepCopy()
	}
	for _, allocation := range allocations {
		if err := r.applySharedTeamCohort(ctx, allocation.team, allocation.desired, !hasQueues); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if selected[name].DeletionTimestamp.IsZero() {
			if err := r.reconcileCapacityWorkspace(ctx, selected[name], poolQuotas, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *TauTeamReconciler) applySharedTeamCohort(ctx context.Context, team *tauv1alpha1.TauTeam, quotas []tauv1alpha1.TauResourceQuota, allowCreate bool) error {
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
	existing := newQueueObject(cohortGVK)
	if err := r.quotaReader().Get(ctx, client.ObjectKeyFromObject(cohort), existing); err == nil {
		if err := validateSharedPoolOwner(existing, labelTeam, team.Name, string(team.UID)); err != nil {
			return err
		}
		if !apiequality.Semantic.DeepEqual(existing.Object["spec"], cohort.Object["spec"]) {
			return fmt.Errorf("team %q has an established fixed quota; stop the scheduler and reconcile its Cohort under operator maintenance before changing quotas", team.Name)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if !allowCreate {
		return fmt.Errorf("cannot create team %q quota in an established hierarchy; stop the scheduler and remove workspace queues under operator maintenance first", team.Name)
	}
	return r.Create(ctx, cohort)
}

func validateSharedPoolOwner(object *unstructured.Unstructured, label, name, uid string) error {
	if object.GetLabels()[labelManagedBy] != labelManagedByValue || object.GetLabels()[label] != name ||
		object.GetAnnotations()[annotationOwnerUID] != uid {
		return fmt.Errorf("%s %q does not have authoritative shared quota ownership", object.GetKind(), object.GetName())
	}
	return nil
}

func sharedTeamSpare(root *unstructured.Unstructured) (map[string]resource.Quantity, error) {
	if _, found, err := unstructured.NestedFieldNoCopy(root.Object, "spec", "resourceGroups"); err != nil {
		return nil, err
	} else if !found {
		return map[string]resource.Quantity{}, nil
	}
	return clusterQueueNominalQuota(root)
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
			gpus := node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)]
			if node.Labels[corev1.LabelOSStable] != "linux" || node.Labels[labelkeys.LabelGPUClass] == "" ||
				gpus.Sign() <= 0 || !capacityNodeEligible(&node) {
				continue
			}
			if nodeHasLabels(&node, selector) {
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
			parentField := "parentName"
			if gvk.queue {
				parentField = "cohortName"
			}
			parent, _, err := unstructured.NestedString(object.Object, "spec", parentField)
			if err != nil {
				return err
			}
			for name := range teams {
				if parent == teamCohortName(name) {
					return fmt.Errorf("%s %q is an external member of the shared hierarchy", object.GetKind(), object.GetName())
				}
			}
			if parent == sharedTeamCohortName {
				return fmt.Errorf("%s %q is an external member of the shared hierarchy", object.GetKind(), object.GetName())
			}
			if err := validateCapacityPoolObject(object, gvk.queue, "", nil, overlaps); err != nil {
				return err
			}
		}
	}
	return nil
}
