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
	workloadGVK              = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta1", Kind: "Workload"}
	admissionCheckGVK        = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "AdmissionCheck"}
	resourceFlavorGVK        = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "ResourceFlavor"}
	topologyGVK              = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "Topology"}
	workloadPriorityClassGVK = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "WorkloadPriorityClass"}
	errWorkspaceSpecChanged  = errors.New("workspace spec changed while waiting for quota locks")
)

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

func (r *TauWorkspaceReconciler) reportUnresolvedQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, message string) (ctrl.Result, error) {
	conditions := []metav1.Condition{
		boolCondition(tauv1alpha1.ConditionRBACReady, false, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionQueueReady, false, "QueueUnresolved", message, workspace.Generation),
		condition(tauv1alpha1.ConditionWorkloadIdentityReady, metav1.ConditionUnknown, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", message, workspace.Generation),
	}
	conditions = preserveV0PrimaryDiscoveryCondition(workspace, conditions)
	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              tauv1alpha1.WorkspacePhaseDegraded,
		ObservedGeneration: workspace.Generation,
		Target:             workspace.Status.Target,
		Queue:              workspace.Status.Queue,
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
		if ready, message, err := r.reconcileWorkspaceCompatibilityLocalQueues(ctx, workspace, targetNamespace, desiredClusterQueue); !ready {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false, message, err
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
		if err := r.ensureWorkspaceObjectUID(ctx, localQueue, workspace); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
				fmt.Sprintf("LocalQueue %q ownership is not authoritative: %v", localQueueName, err), nil
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err), nil
		}
		if clusterQueueName != desiredClusterQueue {
			if err := r.deleteWithPreconditions(ctx, localQueue); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to replace workspace LocalQueue: %v", err), nil
			}
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
				fmt.Sprintf("replacing workspace LocalQueue immutable ClusterQueue target %q with %q", clusterQueueName, desiredClusterQueue), nil
		}
		if ready, message, err := r.reconcileWorkspaceCompatibilityLocalQueues(ctx, workspace, targetNamespace, desiredClusterQueue); !ready {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: desiredClusterQueue}, false, message, err
		}
		return tauv1alpha1.WorkspaceQueueStatus{
				LocalQueue: localQueueName, ClusterQueue: clusterQueueName, ClusterQueueUID: string(clusterQueue.GetUID()),
			}, true,
			"workspace LocalQueue is reconciled", nil
	}
	if workspace.Spec.TeamRef != nil {
		if clusterQueueName == desiredClusterQueue {
			clusterQueue := newQueueObject(clusterQueueGVK)
			if err := r.Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
					fmt.Sprintf("backing ClusterQueue %q is not ready: %v", clusterQueueName, err), nil
			}
			owned, err := authoritativeWorkspaceClusterQueue(clusterQueue, workspace)
			if err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
					fmt.Sprintf("ClusterQueue %q ownership is not authoritative: %v", clusterQueueName, err), nil
			}
			if !owned {
				return tauv1alpha1.WorkspaceQueueStatus{
						LocalQueue: localQueueName, ClusterQueue: clusterQueueName, ClusterQueueUID: string(clusterQueue.GetUID()),
					}, true,
					"established external workspace queue is readable", nil
			}
		}
		if clusterQueueName != desiredClusterQueue {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
				fmt.Sprintf(
					"Team-managed LocalQueue %q is not owned by TauWorkspace %q and targets ClusterQueue %q instead of %q",
					localQueueName, workspace.Name, clusterQueueName, desiredClusterQueue,
				), nil
		}
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: localQueueName, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("Team-managed LocalQueue %q is not owned by TauWorkspace %q", localQueueName, workspace.Name), nil
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

func (r *TauWorkspaceReconciler) reconcileWorkspaceCompatibilityLocalQueues(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespace, clusterQueue string,
) (bool, string, error) {
	if workspace.Spec.TeamRef == nil {
		return true, "", nil
	}
	for _, name := range workspaceCompatibilityLocalQueueNames(workspace) {
		queue := newQueueObject(localQueueGVK)
		key := client.ObjectKey{Name: name, Namespace: namespace}
		if err := r.Get(ctx, key, queue); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err.Error(), err
			}
			targetClusterQueue, err := r.compatibilityLocalQueueClusterQueue(ctx, workspace, namespace, name, clusterQueue)
			if err != nil {
				return false, err.Error(), err
			}
			queue = newWorkspaceLocalQueue(workspace, namespace, name, targetClusterQueue, true)
			if err := r.Create(ctx, queue); err != nil {
				return false, fmt.Sprintf("failed to reconcile compatible LocalQueue %q: %v", name, err), nil
			}
			continue
		}
		currentClusterQueue, _, _ := unstructured.NestedString(queue.Object, "spec", "clusterQueue")
		if _, selected, err := selectedCapacityTeam(ctx, r.apiReader(), workspace.Spec.TeamRef.Name); err != nil {
			return false, err.Error(), err
		} else if selected && currentClusterQueue != clusterQueue {
			message := fmt.Sprintf("compatible LocalQueue %q must target capacity pool ClusterQueue %q; external routing is not adopted", name, clusterQueue)
			return false, message, nil
		}
		if strings.TrimSpace(currentClusterQueue) == "" {
			return false, fmt.Sprintf("compatible LocalQueue %q does not reference a ClusterQueue", name), nil
		}
		backingQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: currentClusterQueue}, backingQueue); err != nil {
			return false, fmt.Sprintf("compatible LocalQueue %q backing ClusterQueue %q is not ready: %v", name, currentClusterQueue, err), nil
		}
	}
	return true, "compatible LocalQueues are reconciled", nil
}

func (r *TauWorkspaceReconciler) compatibilityLocalQueueClusterQueue(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespace, localQueueName, managedClusterQueue string,
) (string, error) {
	if workspace.Spec.TeamRef != nil {
		if _, selected, err := selectedCapacityTeam(ctx, r.apiReader(), workspace.Spec.TeamRef.Name); err != nil {
			return "", err
		} else if selected {
			return managedClusterQueue, nil
		}
	}
	if namespace != workspace.Status.Target.ResolvedNamespace {
		return managedClusterQueue, nil
	}
	candidate := ""
	if localQueueName == strings.TrimSpace(workspace.Status.Queue.LocalQueue) {
		candidate = strings.TrimSpace(workspace.Status.Queue.ClusterQueue)
	}
	if candidate == "" && localQueueName == strings.TrimSpace(workspace.Spec.Queue) {
		candidate = strings.TrimSpace(workspace.Spec.Queue)
	}
	if candidate == "" || candidate == managedClusterQueue {
		return managedClusterQueue, nil
	}
	queue := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: candidate}, queue); err != nil {
		if apierrors.IsNotFound(err) {
			return managedClusterQueue, nil
		}
		return "", err
	}
	return candidate, nil
}

func workspaceCompatibilityLocalQueueNames(workspace *tauv1alpha1.TauWorkspace) []string {
	if workspace.Spec.TeamRef == nil {
		return nil
	}
	canonical := workspaceLocalQueueName(workspace)
	seen := map[string]bool{canonical: true}
	var names []string
	for _, name := range []string{workspace.Spec.Queue, workspace.Status.Queue.LocalQueue} {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
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
	if cluster, selected, err := selectedCapacityTeam(ctx, r.apiReader(), requestedTeam); err != nil {
		return "", err
	} else if selected {
		var team tauv1alpha1.TauTeam
		if err := r.apiReader().Get(ctx, client.ObjectKey{Namespace: workspace.Namespace, Name: requestedTeam}, &team); err != nil {
			return "", err
		}
		reconciler := &TauTeamReconciler{Client: r.Client, APIReader: r.apiReader(), SystemNamespace: r.SystemNamespace}
		if err := reconciler.reconcileCapacityPool(ctx, cluster, &team); err != nil {
			return "", err
		}
		return workspaceClusterQueueName(workspace.Name), nil
	}
	if legacyQueue, retained, err := retainedEstablishedWorkspaceClusterQueue(ctx, r.apiReader(), workspace); err != nil {
		return "", err
	} else if retained {
		return legacyQueue, nil
	}

	var team tauv1alpha1.TauTeam
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: workspace.Spec.TeamRef.Name, Namespace: workspace.Namespace}, &team); err != nil {
		return "", fmt.Errorf("team %q is not ready: %w", workspace.Spec.TeamRef.Name, err)
	}
	teamReconciler := &TauTeamReconciler{Client: r.Client, APIReader: r.apiReader(), SystemNamespace: r.SystemNamespace}
	appliedCohort, _, incomingReservations, _, err := teamReconciler.appliedWorkspaceQueue(ctx, workspace)
	if err != nil {
		return "", err
	}
	if appliedCohort != "" && appliedCohort != teamCohortName(team.Name) {
		if err := teamReconciler.validateTeamReservationsWithAdditional(ctx, &team, incomingReservations); err != nil {
			return "", fmt.Errorf("refusing workspace migration to team %q: %w", team.Name, err)
		}
	}
	if err := validateQuotaFlavors(ctx, r.Client, workspace.Spec.Quota); err != nil {
		return "", err
	}
	queue := desiredWorkspaceClusterQueue(workspace)
	if err := r.preserveEstablishedWorkspaceQueueContract(ctx, workspace, &team, queue); err != nil {
		return "", err
	}
	teamReady := team.Status.Phase == tauv1alpha1.TeamPhaseReady &&
		team.Status.ObservedGeneration == team.Generation
	reductionOnly, err := r.workspaceQueueReductionOnly(ctx, workspace, queue)
	if err != nil {
		return "", err
	}
	if !team.DeletionTimestamp.IsZero() && !reductionOnly {
		return "", fmt.Errorf("team %q is deleting", workspace.Spec.TeamRef.Name)
	}
	if !reductionOnly {
		if err := teamReconciler.validateTeamCapacity(ctx, &team); err != nil {
			return "", err
		}
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

func retainedEstablishedWorkspaceClusterQueue(
	ctx context.Context,
	reader client.Reader,
	workspace *tauv1alpha1.TauWorkspace,
) (string, bool, error) {
	sourceName := strings.TrimSpace(workspace.Status.Queue.ClusterQueue)
	resolvedLocalQueue := false
	if sourceName == "" {
		var err error
		sourceName, err = resolvedWorkspaceQueueName(ctx, reader, workspace)
		if err != nil {
			return "", false, err
		}
		sourceName, resolvedLocalQueue, err = workspaceQueueClusterQueueName(ctx, reader, workspace, sourceName)
		if err != nil {
			return "", false, err
		}
	}
	if sourceName == "" {
		return "", false, nil
	}
	source := newQueueObject(clusterQueueGVK)
	if err := reader.Get(ctx, client.ObjectKey{Name: sourceName}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if sourceName == workspaceClusterQueueName(workspace.Name) {
		owned, err := authoritativeWorkspaceClusterQueue(source, workspace)
		if err != nil {
			return "", false, err
		}
		if owned {
			return "", false, nil
		}
	}
	if len(workspace.Spec.Quota) == 0 {
		return source.GetName(), true, nil
	}
	if resolvedLocalQueue && workspace.Spec.TeamRef != nil {
		cohort, _, err := unstructured.NestedString(source.Object, "spec", "cohortName")
		if err != nil {
			return "", false, fmt.Errorf("read ClusterQueue %q cohortName: %w", source.GetName(), err)
		}
		if strings.TrimSpace(cohort) == teamCohortName(workspace.Spec.TeamRef.Name) {
			return source.GetName(), true, nil
		}
	}
	if strings.TrimSpace(workspace.Status.Queue.LocalQueue) != "" &&
		strings.TrimSpace(workspace.Status.Queue.ClusterQueue) == source.GetName() {
		return source.GetName(), true, nil
	}
	return "", false, nil
}

func authoritativeWorkspaceClusterQueue(
	queue *unstructured.Unstructured,
	workspace *tauv1alpha1.TauWorkspace,
) (bool, error) {
	labels := queue.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue {
		return false, nil
	}
	if labels[labelWorkspace] != workspace.Name {
		return false, fmt.Errorf("ClusterQueue %q is managed for workspace %q", queue.GetName(), labels[labelWorkspace])
	}
	if err := validateWorkspaceObjectUID(queue, workspace); err != nil {
		return false, err
	}
	return true, nil
}

func workspaceQueueClusterQueueName(
	ctx context.Context,
	reader client.Reader,
	workspace *tauv1alpha1.TauWorkspace,
	queueName string,
) (string, bool, error) {
	localQueue := newQueueObject(localQueueGVK)
	if err := reader.Get(ctx, client.ObjectKey{
		Name:      queueName,
		Namespace: resolvedNamespace(workspace),
	}, localQueue); err != nil {
		if apierrors.IsNotFound(err) {
			return queueName, false, nil
		}
		return "", false, err
	}
	clusterQueue, _, err := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	if err != nil {
		return "", false, fmt.Errorf("read LocalQueue %q backing ClusterQueue: %w", localQueue.GetName(), err)
	}
	if strings.TrimSpace(clusterQueue) == "" {
		return "", false, fmt.Errorf("LocalQueue %q has no backing ClusterQueue", localQueue.GetName())
	}
	return clusterQueue, true, nil
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

func quotaValuesNonIncreasing(existing, desired map[string]resource.Quantity) bool {
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
