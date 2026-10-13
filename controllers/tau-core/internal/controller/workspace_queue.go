// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
	errWorkspaceSpecChanged  = errors.New("workspace spec changed while waiting for quota locks")
)

// getDefaultWorkspaceQueue reads the cluster-wide workspace queue default from the
// TauCluster singleton. It is the same name the TauGrid distribution gives its
// baseline ClusterQueue, so a workspace that omits spec.queue still lands on a
// reviewed queue instead of guessing.
func (r *TauWorkspaceReconciler) getDefaultWorkspaceQueue(ctx context.Context) (string, error) {
	var cluster tauv1alpha1.TauCluster
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &cluster); err != nil {
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
func (r *TauWorkspaceReconciler) reconcileQueue(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace string,
) (tauv1alpha1.WorkspaceQueueStatus, bool, string, error) {
	localQueueName := workspaceLocalQueueName(workspace)
	desiredClusterQueue, err := r.reconcileWorkspaceClusterQueue(ctx, workspace)
	if err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName}, false, err.Error(), err
	}
	localQueue := newQueueObject(localQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: localQueueName, Namespace: targetNamespace}, localQueue); err != nil {
		if !apierrors.IsNotFound(err) {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName}, false, err.Error(), nil
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err), nil
		}
		localQueue = newWorkspaceLocalQueue(workspace, targetNamespace, localQueueName, desiredClusterQueue, true)
		if err := r.Create(ctx, localQueue); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
					"workspace LocalQueue changed concurrently; retrying", nil
			}
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("failed to reconcile workspace LocalQueue: %v", err), nil
		}
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue, ClusterQueueUID: string(clusterQueue.GetUID()),
			}, true,
			"workspace LocalQueue is reconciled", nil
	}
	clusterQueueName, _, _ := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	labels := localQueue.GetLabels()
	if labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] != workspace.Name {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("LocalQueue %q is owned by TauWorkspace %q", localQueueName, labels[labelWorkspace]), nil
	}
	if ownedByWorkspace(labels, workspace.Name) {
		if ownerUID := localQueue.GetAnnotations()[annotationOwnerUID]; ownerUID != "" && ownerUID != string(workspace.UID) {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
				fmt.Sprintf("LocalQueue %q belongs to a different TauWorkspace UID %q", localQueueName, ownerUID), nil
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err), nil
		}

		changed := false
		if clusterQueueName != desiredClusterQueue {
			deleteOptions := []client.DeleteOption{}
			if uid := localQueue.GetUID(); uid != "" {
				deleteOptions = append(deleteOptions, client.Preconditions{UID: &uid})
			}
			if err := r.Delete(ctx, localQueue, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to replace workspace LocalQueue: %v", err), nil
			}
			return tauv1alpha1.WorkspaceQueueStatus{
					LocalQueue: localQueueName, ClusterQueue: clusterQueueName,
				}, false,
				fmt.Sprintf(
					"replacing workspace LocalQueue immutable ClusterQueue target %q with %q",
					clusterQueueName,
					desiredClusterQueue,
				), nil
		}
		if workspace.UID != "" && localQueue.GetAnnotations()[annotationOwnerUID] != string(workspace.UID) {
			setOwnerUIDAnnotation(localQueue, workspace.UID)
			changed = true
		}
		if changed {
			if err := r.Update(ctx, localQueue); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err), nil
			}
			clusterQueueName = desiredClusterQueue
		}
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: localQueueName, ClusterQueue: clusterQueueName, ClusterQueueUID: string(clusterQueue.GetUID()),
			}, true,
			"workspace LocalQueue is reconciled", nil
	}
	if workspace.Spec.TeamRef != nil {
		if clusterQueueName != desiredClusterQueue {
			return tauv1alpha1.WorkspaceQueueStatus{
					LocalQueue: localQueueName, ClusterQueue: clusterQueueName,
				}, false,
				fmt.Sprintf(
					"Team-managed LocalQueue %q is not owned by TauWorkspace %q and targets ClusterQueue %q instead of %q",
					localQueueName,
					workspace.Name,
					clusterQueueName,
					desiredClusterQueue,
				), nil
		}
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: localQueueName, ClusterQueue: clusterQueueName,
			}, false,
			fmt.Sprintf(
				"Team-managed LocalQueue %q is not owned by TauWorkspace %q",
				localQueueName,
				workspace.Name,
			), nil
	}
	if strings.TrimSpace(clusterQueueName) == "" {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName}, false,
			fmt.Sprintf("LocalQueue %q does not reference a ClusterQueue", localQueueName), nil
	}
	clusterQueue := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("backing ClusterQueue %q is not ready: %v", clusterQueueName, err), nil
	}
	return tauv1alpha1.WorkspaceQueueStatus{
			LocalQueue: localQueueName, ClusterQueue: clusterQueueName, ClusterQueueUID: string(clusterQueue.GetUID()),
		}, true,
		"workspace queue and backing ClusterQueue are readable", nil
}

func workspaceLocalQueueName(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.TeamRef != nil {
		return workspaceClusterQueueName(workspace.Name)
	}
	return workspace.Spec.Queue
}

func (r *TauWorkspaceReconciler) reconcileWorkspaceClusterQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) (string, error) {
	if workspace.Spec.TeamRef == nil {
		managedQueue := newQueueObject(clusterQueueGVK)
		managedQueue.SetName(workspaceClusterQueueName(workspace.Name))
		if err := r.Get(ctx, client.ObjectKeyFromObject(managedQueue), managedQueue); err != nil {
			if !apierrors.IsNotFound(err) {
				return "", err
			}
		} else if ownedByWorkspace(managedQueue.GetLabels(), workspace.Name) {
			if err := validateWorkspaceObjectUID(managedQueue, workspace); err != nil {
				return "", fmt.Errorf("cannot remove teamRef: %w", err)
			}
			return "", fmt.Errorf(
				"cannot remove teamRef while managed ClusterQueue %q still exists; restore teamRef or delete and recreate the workspace",
				managedQueue.GetName(),
			)
		}
		return workspace.Spec.Queue, nil
	}
	workspaceKey := client.ObjectKeyFromObject(workspace)
	workspaceUID := workspace.UID
	requestedTeam := workspace.Spec.TeamRef.Name
	lockKeys := []string{teamQuotaLockKey(workspace.Namespace, requestedTeam)}
	appliedQueue := newQueueObject(clusterQueueGVK)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, appliedQueue); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	} else if appliedTeam := appliedQueue.GetLabels()[labelTeam]; appliedTeam != "" {
		lockKeys = append(lockKeys, teamQuotaLockKey(workspace.Namespace, appliedTeam))
	}
	unlock := lockTeamQuota(lockKeys...)
	defer unlock()

	var refreshed tauv1alpha1.TauWorkspace
	if err := r.apiReader().Get(ctx, workspaceKey, &refreshed); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if refreshed.UID != workspaceUID {
		return "", fmt.Errorf("workspace %q changed identity while waiting for quota locks; retrying", workspace.Name)
	}
	if !refreshed.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("workspace %q is deleting; retrying", workspace.Name)
	}
	refreshedSpec := refreshed.Spec.DeepCopy()
	if strings.TrimSpace(refreshedSpec.Queue) == "" {
		resolved, err := r.getDefaultWorkspaceQueue(ctx)
		if err != nil {
			return "", err
		}
		refreshedSpec.Queue = resolved
	}
	if !apiequality.Semantic.DeepEqual(*refreshedSpec, workspace.Spec) {
		return "", fmt.Errorf("%w: workspace %q", errWorkspaceSpecChanged, workspace.Name)
	}
	if err := r.validateTeamQueueTransitionExecution(ctx, &refreshed); err != nil {
		return "", err
	}

	var team tauv1alpha1.TauTeam
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: workspace.Spec.TeamRef.Name, Namespace: workspace.Namespace}, &team); err != nil {
		return "", fmt.Errorf("team %q is not ready: %w", workspace.Spec.TeamRef.Name, err)
	}
	if !team.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("team %q is deleting", workspace.Spec.TeamRef.Name)
	}
	teamReconciler := &TauTeamReconciler{Client: r.Client, APIReader: r.apiReader(), SystemNamespace: r.SystemNamespace}
	appliedCohort, _, incomingReservations, err := teamReconciler.appliedWorkspaceQueue(ctx, workspace)
	if err != nil {
		return "", err
	}
	if appliedCohort != "" && appliedCohort != teamCohortName(team.Name) {
		if err := teamReconciler.validateTeamReservationsWithAdditional(ctx, &team, incomingReservations); err != nil {
			return "", fmt.Errorf("refusing workspace migration to team %q: %w", team.Name, err)
		}
	}
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
	sharedQuota, err := teamReconciler.sharedTeamQuotaForWorkspace(ctx, &team, workspace)
	if err != nil && !reductionOnly {
		return "", err
	}
	if reductionOnly && (!teamReady || err != nil) {
		if err := reconcileManagedUnstructuredWithReader(ctx, r.Client, r.apiReader(), queue, labelWorkspace, workspace.Name); err != nil {
			return "", fmt.Errorf("reconcile workspace ClusterQueue reduction: %w", err)
		}
		sharedQuota, err = teamReconciler.sharedTeamQuotaForWorkspace(ctx, &team, workspace)
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
	if err := reconcileManagedUnstructuredWithReader(ctx, r.Client, r.apiReader(), queue, labelWorkspace, workspace.Name); err != nil {
		return "", fmt.Errorf("reconcile workspace ClusterQueue: %w", err)
	}
	sharedQuota, err = teamReconciler.sharedTeamQuotaForWorkspace(ctx, &team, workspace)
	if err != nil {
		return "", err
	}
	if err := teamReconciler.reconcileTeamCohort(ctx, &team, sharedQuota); err != nil {
		return "", fmt.Errorf("reconcile team Cohort after workspace quota: %w", err)
	}
	return queue.GetName(), nil
}

func (r *TauWorkspaceReconciler) validateTeamQueueTransitionExecution(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	managedQueueName := workspaceClusterQueueName(workspace.Name)
	if workspace.Status.Queue.LocalQueue == managedQueueName {
		return nil
	}
	sourceLocalQueue := strings.TrimSpace(workspace.Status.Queue.LocalQueue)
	if sourceLocalQueue == "" {
		sourceLocalQueue = strings.TrimSpace(workspace.Spec.Queue)
	}
	if sourceLocalQueue == "" || sourceLocalQueue == managedQueueName {
		return nil
	}
	localQueue := newQueueObject(localQueueGVK)
	if err := r.apiReader().Get(ctx, client.ObjectKey{
		Name:      sourceLocalQueue,
		Namespace: resolvedNamespace(workspace),
	}, localQueue); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read source LocalQueue %q before team migration: %w", sourceLocalQueue, err)
	}
	sourceClusterQueue, found, err := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	if err != nil {
		return fmt.Errorf("read source LocalQueue %q ClusterQueue: %w", sourceLocalQueue, err)
	}
	sourceClusterQueue = strings.TrimSpace(sourceClusterQueue)
	if !found || sourceClusterQueue == "" {
		return fmt.Errorf("source LocalQueue %q has no ClusterQueue before team migration", sourceLocalQueue)
	}
	clusterQueue := newQueueObject(clusterQueueGVK)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: sourceClusterQueue}, clusterQueue); err != nil {
		return fmt.Errorf("read source ClusterQueue %q before team migration: %w", sourceClusterQueue, err)
	}
	admissionChecks, err := clusterQueueAdmissionCheckNames(clusterQueue)
	if err != nil {
		return fmt.Errorf("read source ClusterQueue %q admission checks before team migration: %w", sourceClusterQueue, err)
	}
	for _, name := range admissionChecks {
		admissionCheck := newQueueObject(admissionCheckGVK)
		if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, admissionCheck); err != nil {
			return fmt.Errorf("read source AdmissionCheck %q before team migration: %w", name, err)
		}
		controllerName, err := admissionCheckControllerName(admissionCheck)
		if err != nil {
			return fmt.Errorf("read source AdmissionCheck %q controller before team migration: %w", name, err)
		}
		if controllerName == multiKueueAdmissionCheckController {
			return fmt.Errorf(
				"cannot migrate workspace %q to team %q: source ClusterQueue %q uses MultiKueue AdmissionCheck %q, "+
					"but the Team-managed ClusterQueue does not preserve MultiKueue execution wiring",
				workspace.Name,
				workspace.Spec.TeamRef.Name,
				sourceClusterQueue,
				name,
			)
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) workspaceQueueReductionOnly(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	desired *unstructured.Unstructured,
) (bool, error) {
	existing := newQueueObject(clusterQueueGVK)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: desired.GetName()}, existing); err != nil {
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

func newWorkspaceLocalQueue(
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace, localQueueName, clusterQueue string,
	teamManaged bool,
) *unstructured.Unstructured {
	queue := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": localQueueGVK.GroupVersion().String(),
		"kind":       localQueueGVK.Kind,
		"metadata": map[string]any{
			"name":      localQueueName,
			"namespace": targetNamespace,
		},
		"spec": map[string]any{"clusterQueue": clusterQueue},
	}}
	queue.SetGroupVersionKind(localQueueGVK)
	labels := workspaceLabels(workspace.Name)
	if teamManaged && workspace.Spec.TeamRef != nil {
		labels[labelTeam] = workspace.Spec.TeamRef.Name
	}
	queue.SetLabels(labels)
	setOwnerUIDAnnotation(queue, workspace.UID)
	return queue
}
