// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *TauWorkspaceReconciler) syncWorkspace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) (ctrl.Result, error) {
	targetNamespace := resolvedNamespace(workspace)
	if workspace.Spec.TeamRef != nil &&
		workspace.Status.Target.ResolvedNamespace != "" &&
		workspace.Status.Target.ResolvedNamespace != targetNamespace &&
		workspace.Status.Queue.LocalQueue != "" &&
		workspace.Status.Queue.LocalQueue != workspaceLocalQueueName(workspace) {
		return ctrl.Result{}, fmt.Errorf(
			"cannot change target namespace from %q to %q while Team opt-in retains legacy LocalQueue %q",
			workspace.Status.Target.ResolvedNamespace,
			targetNamespace,
			workspace.Status.Queue.LocalQueue,
		)
	}
	conditions := []metav1.Condition{}

	namespaceReady, namespaceErr := r.reconcileNamespace(ctx, workspace, targetNamespace, false)
	if namespaceErr != nil {
		if errors.Is(namespaceErr, errLegacyNamespaceOwnershipStampFailed) {
			return ctrl.Result{}, namespaceErr
		}
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

	queueStatus := workspace.Status.Queue
	if recovered, ok := r.recoverRetainedWorkspaceQueueStatus(ctx, workspace, targetNamespace); ok {
		queueStatus = recovered
	}
	if queueStatus.LocalQueue == "" && workspace.Spec.TeamRef == nil {
		queueStatus.LocalQueue = workspaceLocalQueueName(workspace)
	}
	queueReady := false
	queueMessage := "waiting for target namespace reconciliation"
	if namespaceReady {
		var queueErr error
		candidateQueueStatus, candidateQueueReady, candidateQueueMessage, candidateQueueErr :=
			r.reconcileQueue(ctx, workspace, targetNamespace)
		queueReady, queueMessage, queueErr = candidateQueueReady, candidateQueueMessage, candidateQueueErr
		preserveLegacyRouting := workspace.Spec.TeamRef != nil &&
			queueStatus.LocalQueue != "" &&
			queueStatus.LocalQueue != candidateQueueStatus.LocalQueue
		if queueReady && preserveLegacyRouting {
			queueReady, queueMessage, queueErr = r.reconcileRetainedWorkspaceQueue(ctx, workspace, targetNamespace, queueStatus)
		}
		if queueReady && !preserveLegacyRouting || queueStatus.LocalQueue == "" {
			queueStatus = candidateQueueStatus
		}
		if errors.Is(queueErr, errWorkspaceSpecChanged) {
			return ctrl.Result{Requeue: true}, nil
		}
		if queueReady {
			activationWorkspace := workspace.DeepCopy()
			activationWorkspace.Status.Queue = queueStatus
			var refreshed bool
			refreshed, namespaceErr = r.reconcileNamespace(ctx, activationWorkspace, targetNamespace, true)
			if namespaceErr != nil || !refreshed {
				namespaceReady = false
				queueReady = false
				if namespaceErr != nil {
					queueMessage = namespaceErr.Error()
					conditions = append(conditions, boolCondition(
						tauv1alpha1.ConditionDriftDetected,
						true,
						"NamespaceQueueActivationFailed",
						namespaceErr.Error(),
						workspace.Generation,
					))
				}
			}
		}
	} else if namespaceErr != nil {
		queueMessage = namespaceErr.Error()
	}
	conditions = append(conditions, boolCondition(tauv1alpha1.ConditionQueueReady, queueReady, reasonFor(queueReady, "QueueReady", "QueueNotReady"), queueMessage, workspace.Generation))
	queueNamespace, queueName := "", ""
	if queueReady {
		queueNamespace, queueName = targetNamespace, queueStatus.LocalQueue
	}
	if queueReady {
		if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspace, queueNamespace, queueName); err != nil {
			conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "QueueCleanupFailed", err.Error(), workspace.Generation))
		}
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

	currentTargetReady := namespaceReady && rbacReady && rbacErr == nil && queueReady
	if workspace.Spec.WorkloadIdentity != nil {
		currentTargetReady = currentTargetReady && workloadIdentityReady && workloadIdentityErr == nil
	}
	keepNamespace, keepServiceAccount := "", ""
	keepResearcherBinding := false
	if currentTargetReady {
		keepNamespace = targetNamespace
		keepResearcherBinding = authorizationMode(workspace) != tauv1alpha1.AuthorizationModeClusterWide
		if workspace.Spec.WorkloadIdentity != nil {
			keepServiceAccount = workspace.Spec.WorkloadIdentity.ServiceAccountName
		}
		if err := r.cleanupStaleTargetRBAC(ctx, workspace, keepNamespace, keepServiceAccount, keepResearcherBinding); err != nil {
			conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
		}
	}
	namespaceCleanupReady := true
	previousNamespace := workspace.Status.Target.ResolvedNamespace
	if currentTargetReady {
		if previousNamespace != "" && previousNamespace != targetNamespace {
			if err := r.cleanupStaleNamespaceMetadata(ctx, workspace, previousNamespace, true); err != nil {
				namespaceCleanupReady = false
				conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceCleanupFailed", err.Error(), workspace.Generation))
			}
		}
		if namespaceCleanupReady {
			if err := r.cleanupStaleWorkspaceNamespaces(ctx, workspace, targetNamespace); err != nil {
				namespaceCleanupReady = false
				conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceCleanupFailed", err.Error(), workspace.Generation))
			}
		}
	}

	resolvedTarget := workspace.Status.Target.ResolvedNamespace
	if namespaceReady &&
		(previousNamespace == "" || previousNamespace == targetNamespace || currentTargetReady && namespaceCleanupReady) {
		resolvedTarget = targetNamespace
	}
	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              workspacePhase(conditions),
		ObservedGeneration: workspace.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: resolvedTarget},
		Queue:              queueStatus,
		Conditions:         mergeConditions(workspace.Status.Conditions, conditions),
	}
	if !equalWorkspaceStatus(workspace.Status, desired) {
		workspace.Status = desired
		if err := r.Status().Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	if desired.Phase == tauv1alpha1.WorkspacePhaseReady {
		return ctrl.Result{RequeueAfter: readyRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
}
