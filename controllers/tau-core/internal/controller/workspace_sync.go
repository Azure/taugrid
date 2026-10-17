// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TauWorkspaceReconciler) syncWorkspace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) (ctrl.Result, error) {
	targetNamespace := resolvedNamespace(workspace)
	conditions := []metav1.Condition{}

	namespaceReady, namespaceErr := r.reconcileNamespace(ctx, workspace, targetNamespace, false)
	if namespaceErr != nil {
		namespaceReady = false
		reason := "NamespaceReconcileFailed"
		if errors.Is(namespaceErr, errNamespaceOwnershipUnverified) {
			reason = "NamespaceOwnershipUnverified"
		}
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, reason, namespaceErr.Error(), workspace.Generation))
	} else if namespaceReady {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", "target namespace is reconciled", workspace.Generation))
	}
	rbacReady := false
	rbacMessage := "waiting for target namespace reconciliation"
	var rbacErr error
	if namespaceReady {
		rbacReady, rbacMessage, rbacErr = r.reconcileRBAC(ctx, workspace, targetNamespace)
		if rbacErr != nil {
			rbacMessage = rbacErr.Error()
		}
	} else if namespaceErr != nil {
		rbacMessage = namespaceErr.Error()
	}
	rbacReason := "RoleBindingReady"
	if authorizationMode(workspace) == tauv1alpha1.AuthorizationModeClusterWide {
		rbacReason = "ExistingClusterAuthorization"
	}
	conditions = append(conditions, boolCondition(tauv1alpha1.ConditionRBACReady, rbacReady && rbacErr == nil, reasonFor(rbacReady && rbacErr == nil, rbacReason, "AuthorizationNotReady"), rbacMessage, workspace.Generation))

	queueStatus := tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspaceLocalQueueName(workspace)}
	queueReady := false
	queueMessage := "waiting for target namespace reconciliation"
	if namespaceReady {
		candidateQueueStatus, candidateQueueReady, candidateQueueMessage, queueErr :=
			r.reconcileQueue(ctx, workspace, targetNamespace)
		queueReady, queueMessage = candidateQueueReady, candidateQueueMessage
		queueStatus = candidateQueueStatus
		if errors.Is(queueErr, errWorkspaceSpecChanged) {
			return ctrl.Result{Requeue: true}, nil
		}
	} else if namespaceErr != nil {
		queueMessage = namespaceErr.Error()
	}

	workloadIdentityReady := false
	workloadIdentityMessage := "waiting for target namespace reconciliation"
	var workloadIdentityErr error
	if namespaceReady {
		workloadIdentityReady, workloadIdentityMessage, workloadIdentityErr = r.reconcileWorkloadIdentity(ctx, workspace, targetNamespace)
		if workloadIdentityErr != nil {
			workloadIdentityMessage = workloadIdentityErr.Error()
		}
	} else if namespaceErr != nil {
		workloadIdentityMessage = namespaceErr.Error()
	}
	workloadIdentityStatus := metav1.ConditionUnknown
	workloadIdentityReason := "NotConfigured"
	if workspace.Spec.WorkloadIdentity != nil {
		workloadIdentityStatus = metav1.ConditionFalse
		workloadIdentityReason = "ServiceAccountNotReady"
		if workloadIdentityReady && workloadIdentityErr == nil {
			workloadIdentityStatus = metav1.ConditionTrue
			workloadIdentityReason = "ServiceAccountReady"
		}
	}
	conditions = append(conditions, condition(tauv1alpha1.ConditionWorkloadIdentityReady, workloadIdentityStatus, workloadIdentityReason, workloadIdentityMessage, workspace.Generation))

	keepServiceAccount := ""
	if workspace.Spec.WorkloadIdentity != nil {
		keepServiceAccount = workspace.Spec.WorkloadIdentity.ServiceAccountName
	}
	if namespaceReady {
		if err := r.cleanupStaleWorkloadIdentityServiceAccounts(ctx, workspace, targetNamespace, keepServiceAccount); err != nil {
			conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
		}
	}
	previousNamespace := workspace.Status.Target.ResolvedNamespace
	if previousNamespace != "" && previousNamespace != targetNamespace {
		if err := r.cleanupStaleWorkloadIdentityServiceAccounts(ctx, workspace, previousNamespace, keepServiceAccount); err != nil {
			conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
		}
	}
	previousAccessReady := true
	if err := r.reconcilePreviousNamespaceResearcherAccess(ctx, workspace, targetNamespace); err != nil {
		previousAccessReady = false
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
	}

	currentTargetReady := namespaceReady && rbacReady && rbacErr == nil && queueReady && previousAccessReady
	if workspace.Spec.WorkloadIdentity != nil {
		currentTargetReady = currentTargetReady && workloadIdentityReady && workloadIdentityErr == nil
	}
	activationPending := false
	canRemainReadyDuringDrain := workspace.Status.Phase == tauv1alpha1.WorkspacePhaseReady &&
		workspace.Status.Target.ResolvedNamespace == targetNamespace
	if currentTargetReady {
		drained, drainMessage, drainErr := r.staleWorkspaceLocalQueuesDrained(
			ctx,
			workspace,
			targetNamespace,
			queueStatus.LocalQueue,
		)
		if drainErr != nil || !drained {
			currentTargetReady = false
			if drainErr != nil {
				queueReady = false
				queueMessage = drainErr.Error()
			} else if canRemainReadyDuringDrain {
				activationPending = true
				queueMessage = "managed queue is ready; " + drainMessage
			} else {
				queueReady = false
				queueMessage = drainMessage
			}
		}
	}
	if currentTargetReady {
		refreshed, activationErr := r.reconcileNamespace(ctx, workspace, targetNamespace, true)
		if activationErr != nil || !refreshed {
			currentTargetReady = false
			namespaceReady = false
			queueReady = false
			if activationErr != nil {
				queueMessage = activationErr.Error()
				conditions = append(conditions, boolCondition(
					tauv1alpha1.ConditionDriftDetected,
					true,
					"NamespaceQueueActivationFailed",
					activationErr.Error(),
					workspace.Generation,
				))
			}
		}
	}
	conditions = append(conditions, boolCondition(tauv1alpha1.ConditionQueueReady, queueReady, reasonFor(queueReady, "QueueReady", "QueueNotReady"), queueMessage, workspace.Generation))

	cleanupReady := true
	if currentTargetReady {
		keepResearcherBinding := authorizationMode(workspace) != tauv1alpha1.AuthorizationModeClusterWide
		if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspace, targetNamespace, queueStatus.LocalQueue); err != nil {
			cleanupReady = false
			conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "QueueCleanupFailed", err.Error(), workspace.Generation))
		}
		if err := r.cleanupStaleTargetRBAC(ctx, workspace, targetNamespace, keepServiceAccount, keepResearcherBinding); err != nil {
			cleanupReady = false
			conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
		}
		if previousNamespace != "" && previousNamespace != targetNamespace {
			if err := r.cleanupStaleNamespaceMetadata(ctx, workspace, previousNamespace); err != nil {
				cleanupReady = false
				conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceCleanupFailed", err.Error(), workspace.Generation))
			}
		}
	}

	resolvedTarget := workspace.Status.Target.ResolvedNamespace
	if currentTargetReady && cleanupReady {
		resolvedTarget = targetNamespace
	}
	clientQueueStatus, err := r.workspaceClientQueueStatus(ctx, workspace, resolvedTarget, queueStatus)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              workspacePhase(conditions),
		ObservedGeneration: workspace.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: resolvedTarget},
		Queue:              clientQueueStatus,
		Conditions:         mergeConditions(workspace.Status.Conditions, conditions),
	}
	if !equalWorkspaceStatus(workspace.Status, desired) {
		workspace.Status = desired
		if err := r.Status().Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	if desired.Phase == tauv1alpha1.WorkspacePhaseReady && !activationPending {
		return ctrl.Result{RequeueAfter: readyRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
}

func (r *TauWorkspaceReconciler) workspaceClientQueueStatus(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespace string,
	queueStatus tauv1alpha1.WorkspaceQueueStatus,
) (tauv1alpha1.WorkspaceQueueStatus, error) {
	if workspace.Spec.TeamRef == nil {
		return queueStatus, nil
	}
	canonical := workspaceLocalQueueName(workspace)
	previous := strings.TrimSpace(workspace.Status.Queue.LocalQueue)
	if previous == "" || previous == canonical {
		return queueStatus, nil
	}
	localQueue := newQueueObject(localQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: previous, Namespace: namespace}, localQueue); err != nil {
		if apierrors.IsNotFound(err) {
			return workspace.Status.Queue, nil
		}
		return tauv1alpha1.WorkspaceQueueStatus{}, err
	}
	clusterQueueName, _, _ := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	clusterQueueName = strings.TrimSpace(clusterQueueName)
	if clusterQueueName == "" {
		return tauv1alpha1.WorkspaceQueueStatus{}, fmt.Errorf(
			"compatible LocalQueue %q in namespace %q does not reference a ClusterQueue",
			previous,
			namespace,
		)
	}
	clusterQueue := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{}, fmt.Errorf(
			"compatible LocalQueue %q backing ClusterQueue %q is not ready: %w",
			previous,
			clusterQueueName,
			err,
		)
	}
	return tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      previous,
		ClusterQueue:    clusterQueueName,
		ClusterQueueUID: string(clusterQueue.GetUID()),
	}, nil
}
