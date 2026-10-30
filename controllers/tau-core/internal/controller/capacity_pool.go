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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const annotationCapacityPool = labelkeys.AnnotationCapacityPool

func selectedCapacityTeam(ctx context.Context, reader client.Reader, name string) (*tauv1alpha1.TauCluster, bool, error) {
	var cluster tauv1alpha1.TauCluster
	if err := reader.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	ref := cluster.Spec.Queues.CapacityTeamRef
	return &cluster, ref != nil && ref.Name == name, nil
}

func discoveredPoolInventory(ctx context.Context, reader client.Reader, cluster *tauv1alpha1.TauCluster) ([]discoveredGPUFlavor, []tauv1alpha1.TauResourceQuota, error) {
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes, client.MatchingLabels(cluster.Spec.Nodes.Selector)); err != nil {
		return nil, nil, fmt.Errorf("list capacity pool nodes: %w", err)
	}
	byFlavor := map[string]corev1.ResourceList{}
	classes := map[string]string{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		class := node.Labels[labelkeys.LabelGPUClass]
		gpus := node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)]
		if node.Labels[corev1.LabelOSStable] != "linux" || class == "" ||
			gpus.Sign() <= 0 {
			continue
		}
		name := discoveredGPUFlavorName(class)
		if previous, found := classes[name]; found && previous != class {
			return nil, nil, fmt.Errorf("GPU classes %q and %q resolve to the same flavor %q", previous, class, name)
		}
		classes[name] = class
		if byFlavor[name] == nil {
			byFlavor[name] = corev1.ResourceList{}
		}
		if !capacityNodeEligible(node) {
			continue
		}
		for resourceName, quantity := range node.Status.Allocatable {
			if quantity.Sign() < 0 {
				return nil, nil, fmt.Errorf("node %q has negative allocatable %s", node.Name, resourceName)
			}
			total := byFlavor[name][resourceName]
			total.Add(quantity)
			byFlavor[name][resourceName] = total
		}
	}
	cohort := newQueueObject(cohortGVK)
	if err := reader.Get(ctx, client.ObjectKey{Name: teamCohortName(cluster.Spec.Queues.CapacityTeamRef.Name)}, cohort); err == nil {
		previous, err := clusterQueueNominalQuota(cohort)
		if err != nil {
			return nil, nil, err
		}
		for key := range previous {
			resourceName, flavor, _ := strings.Cut(key, "\x00")
			if byFlavor[flavor] == nil {
				byFlavor[flavor] = corev1.ResourceList{}
			}
			if _, found := byFlavor[flavor][corev1.ResourceName(resourceName)]; !found {
				byFlavor[flavor][corev1.ResourceName(resourceName)] = resource.Quantity{}
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	resourceNames := map[corev1.ResourceName]bool{corev1.ResourceName(nvidiaGPUResourceName): true}
	for _, resources := range byFlavor {
		for name := range resources {
			resourceNames[name] = true
		}
	}
	names := make([]string, 0, len(byFlavor))
	for name := range byFlavor {
		names = append(names, name)
	}
	sort.Strings(names)
	resources := make([]string, 0, len(resourceNames))
	for name := range resourceNames {
		resources = append(resources, string(name))
	}
	sort.Strings(resources)
	var flavors []discoveredGPUFlavor
	var quotas []tauv1alpha1.TauResourceQuota
	for _, name := range names {
		if class, found := classes[name]; found {
			flavors = append(flavors, discoveredGPUFlavor{name: name, gpuClass: class, nodeLabels: cluster.Spec.Nodes.Selector})
		}
		for _, resourceName := range resources {
			quotas = append(quotas, tauv1alpha1.TauResourceQuota{
				Flavor: name, Resource: resourceName, NominalQuota: byFlavor[name][corev1.ResourceName(resourceName)],
			})
		}
	}
	return flavors, quotas, nil
}

func capacityNodeEligible(node *corev1.Node) bool {
	if node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *TauTeamReconciler) reconcileCapacityPool(ctx context.Context, cluster *tauv1alpha1.TauCluster, team *tauv1alpha1.TauTeam) error {
	if cluster.Spec.ManagementMode != tauv1alpha1.ClusterManagementModeReconcile {
		return fmt.Errorf("capacity pool requires TauCluster Reconcile mode")
	}
	err := r.applyCapacityPool(ctx, cluster, team)
	if err == nil {
		return nil
	}
	cohort := newQueueObject(cohortGVK)
	if getErr := r.quotaReader().Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return err
		}
		return errors.Join(err, getErr)
	}
	if cohort.GetLabels()[labelManagedBy] != labelManagedByValue || cohort.GetLabels()[labelTeam] != team.Name ||
		cohort.GetAnnotations()[annotationOwnerUID] != string(team.UID) || cohort.GetAnnotations()[annotationCapacityPool] != "true" {
		return err
	}
	nominals, readErr := clusterQueueNominalQuota(cohort)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	var zero []tauv1alpha1.TauResourceQuota
	for key := range nominals {
		resourceName, flavor, _ := strings.Cut(key, "\x00")
		zero = append(zero, tauv1alpha1.TauResourceQuota{Resource: resourceName, Flavor: flavor})
	}
	return errors.Join(err, reconcileManagedUnstructuredWithReader(ctx, r.Client, r.quotaReader(),
		desiredCapacityCohort(team, zero), labelTeam, team.Name))
}

func (r *TauTeamReconciler) applyCapacityPool(ctx context.Context, cluster *tauv1alpha1.TauCluster, team *tauv1alpha1.TauTeam) error {
	if !cluster.DeletionTimestamp.IsZero() {
		return fmt.Errorf("capacity pool requires a non-deleting TauCluster in Reconcile mode")
	}
	if !team.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(team, teamFinalizer) || len(team.Spec.Quota) != 0 {
		return fmt.Errorf("capacity pool team %q must have its cleanup finalizer, be non-deleting and omit spec.quota", team.Name)
	}
	flavors, quotas, err := discoveredPoolInventory(ctx, r.quotaReader(), cluster)
	if err != nil {
		return err
	}
	for _, flavor := range flavors {
		current := newQueueObject(resourceFlavorGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: flavor.name}, current); err != nil {
			return err
		}
		if err := validateDiscoveredGPUFlavor(current, flavor); err != nil {
			return err
		}
	}
	selected, err := capacityPoolWorkspaces(ctx, r.quotaReader(), team)
	if err != nil {
		return err
	}
	if err := r.validateCapacityPoolConflicts(ctx, cluster, team, selected, quotas); err != nil {
		return err
	}
	if err := validateQuotaFlavors(ctx, r.quotaReader(), quotas); err != nil {
		return err
	}
	if err := reconcileManagedUnstructuredWithReader(ctx, r.Client, r.quotaReader(),
		desiredCapacityCohort(team, quotas), labelTeam, team.Name); err != nil {
		return err
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		workspace := selected[name]
		if !workspace.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(workspace, workspaceFinalizer) {
			continue
		}
		if err := r.reconcileCapacityWorkspace(ctx, workspace, quotas); err != nil {
			return err
		}
	}
	return nil
}

func capacityPoolWorkspaces(ctx context.Context, reader client.Reader, team *tauv1alpha1.TauTeam) (map[string]*tauv1alpha1.TauWorkspace, error) {
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := reader.List(ctx, &workspaces, client.InNamespace(team.Namespace)); err != nil {
		return nil, err
	}
	selected := map[string]*tauv1alpha1.TauWorkspace{}
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		if workspace.Spec.TeamRef == nil || workspace.Spec.TeamRef.Name != team.Name {
			continue
		}
		if len(workspace.Spec.Quota) != 0 {
			return nil, fmt.Errorf("capacity pool workspace %q must omit spec.quota", workspace.Name)
		}
		selected[workspaceClusterQueueName(workspace.Name)] = workspace
	}
	return selected, nil
}

func desiredCapacityCohort(team *tauv1alpha1.TauTeam, quotas []tauv1alpha1.TauResourceQuota) *unstructured.Unstructured {
	cohort := desiredTeamCohort(team, quotas)
	annotations := cohort.GetAnnotations()
	annotations[annotationCapacityPool] = "true"
	cohort.SetAnnotations(annotations)
	return cohort
}

func validateRetainedCapacityPools(ctx context.Context, reader client.Reader, exceptTeam string) error {
	cohorts := &unstructured.UnstructuredList{}
	cohorts.SetGroupVersionKind(cohortGVK.GroupVersion().WithKind("CohortList"))
	if err := reader.List(ctx, cohorts); err != nil {
		return err
	}
	for i := range cohorts.Items {
		cohort := &cohorts.Items[i]
		if cohort.GetAnnotations()[annotationCapacityPool] != "true" ||
			(exceptTeam != "" && cohort.GetLabels()[labelTeam] == exceptTeam) {
			continue
		}
		quotas, err := clusterQueueNominalQuota(cohort)
		if err != nil {
			return err
		}
		for _, quota := range quotas {
			if quota.Sign() > 0 {
				return fmt.Errorf("Cohort %q retains a discovered capacity pool; drain and reconcile its owner before reallocating capacity", cohort.GetName())
			}
		}
	}
	return nil
}

func (r *TauTeamReconciler) reconcileCapacityWorkspace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, quotas []tauv1alpha1.TauResourceQuota) error {
	queue := desiredWorkspaceClusterQueue(workspace)
	borrowing := make([]tauv1alpha1.TauResourceQuota, len(quotas))
	reservations := map[string]resource.Quantity{}
	existing := newQueueObject(clusterQueueGVK)
	if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: queue.GetName()}, existing); err == nil {
		reservations, err = clusterQueueReservations(existing)
		if err != nil {
			return err
		}
		for _, field := range []string{"admissionChecks", "admissionChecksStrategy"} {
			value, found, err := unstructured.NestedFieldCopy(existing.Object, "spec", field)
			if err != nil {
				return err
			}
			if found {
				if err := unstructured.SetNestedField(queue.Object, value, "spec", field); err != nil {
					return err
				}
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	for i, quota := range quotas {
		limit := quota.NominalQuota.DeepCopy()
		if reserved := reservations[quotaKey(quota)]; reserved.Cmp(limit) > 0 {
			limit = reserved.DeepCopy()
		}
		borrowing[i] = tauv1alpha1.TauResourceQuota{Flavor: quota.Flavor, Resource: quota.Resource, BorrowingLimit: &limit}
	}
	queue.Object["spec"].(map[string]any)["resourceGroups"] = quotaResourceGroups(borrowing, true)
	if err := setWorkspaceManagedQuotaKeys(workspace, queue); err != nil {
		return err
	}
	return reconcileManagedUnstructuredWithReader(ctx, r.Client, r.quotaReader(), queue, labelWorkspace, workspace.Name)
}

func (r *TauTeamReconciler) validateCapacityPoolConflicts(ctx context.Context, cluster *tauv1alpha1.TauCluster, team *tauv1alpha1.TauTeam, workspaces map[string]*tauv1alpha1.TauWorkspace, quotas []tauv1alpha1.TauResourceQuota) error {
	var nodes corev1.NodeList
	if err := r.quotaReader().List(ctx, &nodes, client.MatchingLabels(cluster.Spec.Nodes.Selector)); err != nil {
		return err
	}
	poolFlavors := map[string]bool{}
	for _, quota := range quotas {
		poolFlavors[quota.Flavor] = true
	}
	overlaps := func(name string) (bool, error) {
		if poolFlavors[name] {
			return true, nil
		}
		flavor := newQueueObject(resourceFlavorGVK)
		if err := r.quotaReader().Get(ctx, client.ObjectKey{Name: name}, flavor); err != nil {
			return false, fmt.Errorf("read budget ResourceFlavor %q: %w", name, err)
		}
		selector, _, err := unstructured.NestedStringMap(flavor.Object, "spec", "nodeLabels")
		if err != nil {
			return false, err
		}
		for i := range nodes.Items {
			node := &nodes.Items[i]
			if node.Labels[corev1.LabelOSStable] == "linux" && node.Labels[labelkeys.LabelGPUClass] != "" &&
				labels.SelectorFromSet(selector).Matches(labels.Set(node.Labels)) {
				return true, nil
			}
		}
		return false, nil
	}
	var teams tauv1alpha1.TauTeamList
	if err := r.quotaReader().List(ctx, &teams, client.InNamespace(team.Namespace)); err != nil {
		return err
	}
	for _, candidate := range teams.Items {
		if candidate.Name == team.Name {
			continue
		}
		for _, quota := range candidate.Spec.Quota {
			if quota.NominalQuota.Sign() == 0 {
				continue
			}
			if conflict, err := overlaps(quota.Flavor); err != nil {
				return err
			} else if conflict {
				return fmt.Errorf("team %q budget overlaps exclusive capacity pool", candidate.Name)
			}
		}
	}
	for _, gvk := range []schema.GroupVersionKind{clusterQueueGVK, cohortGVK} {
		isQueue := gvk == clusterQueueGVK
		objects := &unstructured.UnstructuredList{}
		objects.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.quotaReader().List(ctx, objects); err != nil {
			return err
		}
		for i := range objects.Items {
			object := &objects.Items[i]
			if !isQueue && object.GetName() == teamCohortName(team.Name) {
				continue
			}
			if object.GetLabels()[labelDiscoverGPUFlavors] == "true" {
				return fmt.Errorf("%s %q enables legacy discovery alongside the exclusive capacity pool", object.GetKind(), object.GetName())
			}
			nominals := map[string]resource.Quantity{}
			if _, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "resourceGroups"); err != nil {
				return err
			} else if found {
				nominals, err = clusterQueueNominalQuota(object)
				if err != nil {
					return err
				}
			}
			if workspace := workspaces[object.GetName()]; isQueue && workspace != nil {
				owned, err := authoritativeWorkspaceClusterQueue(object, workspace)
				if err != nil {
					return err
				}
				if !owned {
					return fmt.Errorf("capacity pool ClusterQueue %q is externally owned", object.GetName())
				}
				cohort, _, err := unstructured.NestedString(object.Object, "spec", "cohortName")
				if err != nil {
					return err
				}
				if cohort != teamCohortName(team.Name) && len(nominals) != 0 {
					return fmt.Errorf("capacity pool ClusterQueue %q belongs to another Cohort", object.GetName())
				}
				for _, nominal := range nominals {
					if nominal.Sign() != 0 {
						return fmt.Errorf("capacity pool ClusterQueue %q has a duplicate nominal budget", object.GetName())
					}
				}
				continue
			}
			if isQueue {
				reservations, err := clusterQueueReservations(object)
				if err != nil {
					return err
				}
				for key, quantity := range reservations {
					if quantity.Sign() <= 0 {
						continue
					}
					_, flavor, _ := strings.Cut(key, "\x00")
					if conflict, err := overlaps(flavor); err != nil {
						return err
					} else if conflict {
						return fmt.Errorf("ClusterQueue %q retains reservations overlapping the exclusive capacity pool", object.GetName())
					}
				}
			}
			quotaObjects, err := clusterQueueQuotaObjects(object)
			if err != nil {
				return err
			}
			cohort, _, err := unstructured.NestedString(object.Object, "spec", "cohortName")
			if err != nil {
				return err
			}
			for flavor, resources := range quotaObjects {
				active := false
				for _, quota := range resources {
					nominal, err := resource.ParseQuantity(fmt.Sprint(quota["nominalQuota"]))
					if err != nil {
						return err
					}
					active = active || nominal.Sign() != 0
					if isQueue && cohort != "" {
						limit, found := quota["borrowingLimit"]
						if !found {
							active = true
						} else {
							borrowing, err := resource.ParseQuantity(fmt.Sprint(limit))
							if err != nil {
								return err
							}
							active = active || borrowing.Sign() != 0
						}
					}
				}
				if !active {
					continue
				}
				if conflict, err := overlaps(flavor); err != nil {
					return err
				} else if conflict {
					return fmt.Errorf("%s %q flavor %q budget overlaps exclusive capacity pool", object.GetKind(), object.GetName(), flavor)
				}
			}
		}
	}
	return nil
}
