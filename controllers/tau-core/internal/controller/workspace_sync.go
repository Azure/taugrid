// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *TauWorkspaceReconciler) syncWorkspace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) (ctrl.Result, error) {
	targetNamespace := resolvedNamespace(workspace)
	conditions := []metav1.Condition{}

	namespaceReady, namespaceErr := r.reconcileNamespace(ctx, workspace, targetNamespace)
	if namespaceErr != nil {
		namespaceReady = false
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceReconcileFailed", namespaceErr.Error(), workspace.Generation))
	} else if namespaceReady {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", "target namespace is reconciled", workspace.Generation))
	}
	if namespaceReady {
		previousNamespace := workspace.Status.Target.ResolvedNamespace
		if previousNamespace != "" && previousNamespace != targetNamespace {
			if err := r.cleanupStaleNamespaceMetadata(ctx, workspace.Name, previousNamespace); err != nil {
				conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceCleanupFailed", err.Error(), workspace.Generation))
			}
		}
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

	queueStatus := tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}
	queueReady := false
	queueMessage := "waiting for target namespace reconciliation"
	if namespaceReady {
		queueStatus, queueReady, queueMessage = r.reconcileQueue(ctx, workspace, targetNamespace)
	} else if namespaceErr != nil {
		queueMessage = namespaceErr.Error()
	}
	conditions = append(conditions, boolCondition(tauv1alpha1.ConditionQueueReady, queueReady, reasonFor(queueReady, "QueueReady", "QueueNotReady"), queueMessage, workspace.Generation))
	queueNamespace, queueName := "", ""
	if namespaceReady {
		queueNamespace, queueName = targetNamespace, workspace.Spec.Queue
	}
	if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspace.Name, queueNamespace, queueName); err != nil {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "QueueCleanupFailed", err.Error(), workspace.Generation))
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

	keepNamespace, keepServiceAccount := "", ""
	keepResearcherBinding := false
	if namespaceReady {
		keepNamespace = targetNamespace
		keepResearcherBinding = authorizationMode(workspace) != tauv1alpha1.AuthorizationModeClusterWide
		if workspace.Spec.WorkloadIdentity != nil {
			keepServiceAccount = workspace.Spec.WorkloadIdentity.ServiceAccountName
		}
	}
	if err := r.cleanupStaleTargetRBAC(ctx, workspace.Name, keepNamespace, keepServiceAccount, keepResearcherBinding); err != nil {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
	}

	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              workspacePhase(conditions),
		ObservedGeneration: workspace.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: targetNamespace},
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
