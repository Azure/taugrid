// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	localQueueGVK            = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "LocalQueue"}
	clusterQueueGVK          = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "ClusterQueue"}
	cohortGVK                = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "Cohort"}
	admissionCheckGVK        = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "AdmissionCheck"}
	resourceFlavorGVK        = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "ResourceFlavor"}
	topologyGVK              = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "Topology"}
	workloadPriorityClassGVK = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "WorkloadPriorityClass"}
)

// getDefaultWorkspaceQueue reads the cluster-wide workspace queue default from the
// TauCluster singleton. It is the same name the TauGrid distribution gives its
// baseline ClusterQueue, so a workspace that omits spec.queue still lands on a
// reviewed queue instead of guessing.
func (r *TauWorkspaceReconciler) getDefaultWorkspaceQueue(ctx context.Context) (string, error) {
	var cluster tauv1alpha1.TauCluster
	if err := r.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &cluster); err != nil {
		return "", fmt.Errorf("spec.queue is empty and the TauCluster default is unavailable: %w", err)
	}
	queue := strings.TrimSpace(cluster.Spec.WorkspaceDefaults.DefaultQueue)
	if queue == "" {
		return "", fmt.Errorf("spec.queue is empty and TauCluster %q declares no workspaceDefaults.defaultQueue", cluster.Name)
	}
	return queue, nil
}

// reportUnresolvedQueue keeps an unresolvable workspace visible and Degraded
// rather than reconciling namespace, RBAC, or identity against a guessed queue.
func (r *TauWorkspaceReconciler) reportUnresolvedQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, message string) (ctrl.Result, error) {
	conditions := []metav1.Condition{
		boolCondition(tauv1alpha1.ConditionRBACReady, false, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionQueueReady, false, "QueueUnresolved", message, workspace.Generation),
		condition(tauv1alpha1.ConditionWorkloadIdentityReady, metav1.ConditionUnknown, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", message, workspace.Generation),
	}
	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              tauv1alpha1.WorkspacePhaseDegraded,
		ObservedGeneration: workspace.Generation,
		Target:             workspace.Status.Target,
		Conditions:         mergeConditions(workspace.Status.Conditions, conditions),
	}
	if !equalWorkspaceStatus(workspace.Status, desired) {
		workspace.Status = desired
		if err := r.Status().Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
}

// reconcileQueue accepts an existing platform-owned LocalQueue or creates the
// workspace LocalQueue when a same-named ClusterQueue exists. The latter is the
// portable TauGrid bootstrap contract: Helm owns the ClusterQueue, while this
// controller owns state that depends on a future workspace namespace.
func (r *TauWorkspaceReconciler) reconcileQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (tauv1alpha1.WorkspaceQueueStatus, bool, string) {
	desiredClusterQueue, err := r.reconcileWorkspaceClusterQueue(ctx, workspace)
	if err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false, err.Error()
	}
	localQueue := newQueueObject(localQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue, Namespace: targetNamespace}, localQueue); err != nil {
		if !apierrors.IsNotFound(err) {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false, err.Error()
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err)
		}
		localQueue = newWorkspaceLocalQueue(workspace, targetNamespace, desiredClusterQueue)
		if err := r.Create(ctx, localQueue); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					"workspace LocalQueue changed concurrently; retrying"
			}
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("failed to reconcile workspace LocalQueue: %v", err)
		}
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue, ClusterQueueUID: string(clusterQueue.GetUID()),
			}, true,
			"workspace LocalQueue is reconciled"
	}
	clusterQueueName, _, _ := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	labels := localQueue.GetLabels()
	if labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] != workspace.Name {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("LocalQueue %q is owned by TauWorkspace %q", workspace.Spec.Queue, labels[labelWorkspace])
	}
	if ownedByWorkspace(labels, workspace.Name) {
		if ownerUID := localQueue.GetAnnotations()[annotationOwnerUID]; ownerUID != "" && ownerUID != string(workspace.UID) {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
				fmt.Sprintf("LocalQueue %q belongs to a different TauWorkspace UID %q", workspace.Spec.Queue, ownerUID)
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err)
		}

		changed := false
		if clusterQueueName != desiredClusterQueue {
			if err := unstructured.SetNestedField(localQueue.Object, desiredClusterQueue, "spec", "clusterQueue"); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err)
			}
			changed = true
		}
		if workspace.UID != "" && localQueue.GetAnnotations()[annotationOwnerUID] != string(workspace.UID) {
			setOwnerUIDAnnotation(localQueue, workspace.UID)
			changed = true
		}
		if changed {
			if err := r.Update(ctx, localQueue); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err)
			}
			clusterQueueName = desiredClusterQueue
		}
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName, ClusterQueueUID: string(clusterQueue.GetUID()),
			}, true,
			"workspace LocalQueue is reconciled"
	}
	if strings.TrimSpace(clusterQueueName) == "" {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false,
			fmt.Sprintf("LocalQueue %q does not reference a ClusterQueue", workspace.Spec.Queue)
	}
	if workspace.Spec.TeamRef != nil && clusterQueueName != desiredClusterQueue {
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName,
			}, false,
			fmt.Sprintf(
				"LocalQueue %q targets ClusterQueue %q, but team-backed TauWorkspace %q requires %q; update or remove the LocalQueue before migration",
				workspace.Spec.Queue,
				clusterQueueName,
				workspace.Name,
				desiredClusterQueue,
			)
	}
	clusterQueue := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("backing ClusterQueue %q is not ready: %v", clusterQueueName, err)
	}
	return tauv1alpha1.WorkspaceQueueStatus{
			LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName, ClusterQueueUID: string(clusterQueue.GetUID()),
		}, true,
		"workspace queue and backing ClusterQueue are readable"
}

func (r *TauWorkspaceReconciler) reconcileWorkspaceClusterQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) (string, error) {
	if workspace.Spec.TeamRef == nil {
		return workspace.Spec.Queue, nil
	}
	var team tauv1alpha1.TauTeam
	if err := r.Get(ctx, client.ObjectKey{Name: workspace.Spec.TeamRef.Name, Namespace: workspace.Namespace}, &team); err != nil {
		return "", fmt.Errorf("team %q is not ready: %w", workspace.Spec.TeamRef.Name, err)
	}
	teamReconciler := &TauTeamReconciler{Client: r.Client, SystemNamespace: r.SystemNamespace}
	if err := teamReconciler.validateTeamCapacity(ctx, &team); err != nil {
		return "", err
	}
	if err := validateQuotaFlavors(ctx, r.Client, workspace.Spec.Quota); err != nil {
		return "", err
	}
	queue := desiredWorkspaceClusterQueue(workspace)
	teamReady := team.Status.Phase == tauv1alpha1.TeamPhaseReady &&
		team.Status.ObservedGeneration == team.Generation
	reductionOnly, err := r.workspaceQueueReductionOnly(ctx, workspace, queue)
	if err != nil {
		return "", err
	}
	if !teamReady && !reductionOnly {
		return "", fmt.Errorf("team %q is not Ready", workspace.Spec.TeamRef.Name)
	}
	sharedQuota, err := teamReconciler.sharedTeamQuota(ctx, &team)
	if err != nil && !reductionOnly {
		return "", err
	}
	if reductionOnly && (!teamReady || err != nil) {
		if err := reconcileManagedUnstructured(ctx, r.Client, queue, labelWorkspace, workspace.Name); err != nil {
			return "", fmt.Errorf("reconcile workspace ClusterQueue reduction: %w", err)
		}
		sharedQuota, err = teamReconciler.sharedTeamQuota(ctx, &team)
		if err != nil {
			return "", err
		}
		if err := teamReconciler.reconcileTeamCohort(ctx, &team, sharedQuota); err != nil {
			return "", fmt.Errorf("reconcile team Cohort after workspace quota reduction: %w", err)
		}
		return queue.GetName(), nil
	}
	if err := teamReconciler.reconcileTeamCohort(ctx, &team, sharedQuota); err != nil {
		return "", fmt.Errorf("reconcile team Cohort before workspace quota: %w", err)
	}
	if err := reconcileManagedUnstructured(ctx, r.Client, queue, labelWorkspace, workspace.Name); err != nil {
		return "", fmt.Errorf("reconcile workspace ClusterQueue: %w", err)
	}
	sharedQuota, err = teamReconciler.sharedTeamQuota(ctx, &team)
	if err != nil {
		return "", err
	}
	if err := teamReconciler.reconcileTeamCohort(ctx, &team, sharedQuota); err != nil {
		return "", fmt.Errorf("reconcile team Cohort after workspace quota: %w", err)
	}
	return queue.GetName(), nil
}

func (r *TauWorkspaceReconciler) workspaceQueueReductionOnly(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	desired *unstructured.Unstructured,
) (bool, error) {
	existing := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: desired.GetName()}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	cohort, _, err := unstructured.NestedString(existing.Object, "spec", "cohortName")
	if err != nil {
		return false, fmt.Errorf("read ClusterQueue %q cohortName: %w", existing.GetName(), err)
	}
	if strings.TrimSpace(cohort) != teamCohortName(workspace.Spec.TeamRef.Name) {
		return false, nil
	}
	existingNominals, err := clusterQueueNominalQuota(existing)
	if err != nil {
		return false, err
	}
	desiredNominals, err := clusterQueueNominalQuota(desired)
	if err != nil {
		return false, err
	}
	if !quotaValuesNonIncreasing(existingNominals, desiredNominals) {
		return false, nil
	}
	existingMaximums, err := clusterQueueMaximumQuota(existing)
	if err != nil {
		return false, err
	}
	desiredMaximums, err := clusterQueueMaximumQuota(desired)
	if err != nil {
		return false, err
	}
	return quotaValuesNonIncreasing(existingMaximums, desiredMaximums), nil
}

func quotaValuesNonIncreasing(
	existing, desired map[string]resource.Quantity,
) bool {
	for key, desiredValue := range desired {
		existingValue, ok := existing[key]
		if !ok || desiredValue.Cmp(existingValue) > 0 {
			return false
		}
	}
	return true
}

func newQueueObject(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	queue := &unstructured.Unstructured{}
	queue.SetGroupVersionKind(gvk)
	return queue
}

func newWorkspaceLocalQueue(workspace *tauv1alpha1.TauWorkspace, targetNamespace, clusterQueue string) *unstructured.Unstructured {
	queue := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": localQueueGVK.GroupVersion().String(),
		"kind":       localQueueGVK.Kind,
		"metadata": map[string]any{
			"name":      workspace.Spec.Queue,
			"namespace": targetNamespace,
		},
		"spec": map[string]any{"clusterQueue": clusterQueue},
	}}
	queue.SetGroupVersionKind(localQueueGVK)
	labels := workspaceLabels(workspace.Name)
	if workspace.Spec.TeamRef != nil {
		labels[labelTeam] = workspace.Spec.TeamRef.Name
	}
	queue.SetLabels(labels)
	setOwnerUIDAnnotation(queue, workspace.UID)
	return queue
}
