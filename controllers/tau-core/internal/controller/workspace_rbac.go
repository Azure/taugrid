// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *TauWorkspaceReconciler) reconcileRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, string, error) {
	if authorizationMode(workspace) == tauv1alpha1.AuthorizationModeClusterWide {
		if err := r.cleanupResearcherRBAC(ctx, workspace.Name, targetNamespace); err != nil {
			return false, "failed to remove subject-specific researcher RBAC for cluster-wide authorization", err
		}
		if err := r.cleanupClusterQueueReaderRBAC(ctx, workspace.Name); err != nil {
			return false, "failed to remove subject-specific ClusterQueue reader RBAC for cluster-wide authorization", err
		}
		if err := r.cleanupPlatformReaderRBAC(ctx, workspace.Name); err != nil {
			return false, "failed to remove subject-specific workspace reader RBAC for cluster-wide authorization", err
		}
		return true, "workspace relies on pre-existing cluster authorization; the controller grants no researcher access", nil
	}
	if workspace.Spec.PrincipalRef == nil || workspace.Spec.KubernetesSubject == nil {
		return false, "workspace-rbac authorization requires principalRef and kubernetesSubject", nil
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: targetNamespace}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, binding, workspace.Name); err != nil {
		return false, "refusing to adopt existing researcher RoleBinding", err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: defaultRoleName}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, targetNamespace)}
		return nil
	})
	if err != nil {
		return false, "failed to reconcile researcher RoleBinding", err
	}
	if err := r.reconcileClusterQueueReaderRBAC(ctx, workspace, targetNamespace); err != nil {
		return false, "failed to reconcile ClusterQueue reader ClusterRoleBinding", err
	}
	if err := r.reconcilePlatformReaderRBAC(ctx, workspace); err != nil {
		return false, "failed to reconcile workspace reader RoleBinding", err
	}
	return true, "researcher namespace and ClusterQueue reader bindings are reconciled", nil
}

func authorizationMode(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Authorization == nil || workspace.Spec.Authorization.Mode == "" {
		return tauv1alpha1.AuthorizationModeWorkspaceRBAC
	}
	return workspace.Spec.Authorization.Mode
}

func (r *TauWorkspaceReconciler) cleanupResearcherRBAC(ctx context.Context, workspaceName, targetNamespace string) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: targetNamespace}}
	return r.deleteOwnedObject(ctx, binding, workspaceName)
}

func clusterQueueReaderBindingName(workspaceName string) string {
	return "tau-clusterqueue-reader-" + workspaceName
}

func workspaceReaderRBACName(workspaceName string) string {
	return "tau-workspace-reader-" + workspaceName
}

func (r *TauWorkspaceReconciler) reconcileClusterQueueReaderRBAC(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	serviceAccountNamespace string,
) error {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterQueueReaderBindingName(workspace.Name)}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, binding, workspace.Name); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterQueueReaderRoleName}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, serviceAccountNamespace)}
		return nil
	})
	return err
}

func (r *TauWorkspaceReconciler) cleanupClusterQueueReaderRBAC(ctx context.Context, workspaceName string) error {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterQueueReaderBindingName(workspaceName)}}
	return r.deleteOwnedObject(ctx, binding, workspaceName)
}

func (r *TauWorkspaceReconciler) reconcileWorkloadIdentity(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, string, error) {
	if workspace.Spec.WorkloadIdentity == nil {
		return false, "workspace does not configure workload identity defaults", nil
	}
	wi := workspace.Spec.WorkloadIdentity
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: wi.ServiceAccountName, Namespace: targetNamespace}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, serviceAccount, workspace.Name); err != nil {
		return false, "refusing to adopt existing workload identity ServiceAccount", err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		if serviceAccount.Labels == nil {
			serviceAccount.Labels = map[string]string{}
		}
		for k, v := range workspaceLabels(workspace.Name) {
			serviceAccount.Labels[k] = v
		}
		serviceAccount.Labels[labelAzureWIUse] = "true"
		if serviceAccount.Annotations == nil {
			serviceAccount.Annotations = map[string]string{}
		}
		serviceAccount.Annotations[annotationAzureWIClientID] = wi.ClientID
		return nil
	})
	if err != nil {
		return false, "failed to reconcile workload identity ServiceAccount", err
	}
	return true, "workload identity ServiceAccount is reconciled", nil
}

func (r *TauWorkspaceReconciler) reconcilePlatformReaderRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	name := workspaceReaderRBACName(workspace.Name)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNamespace(r.PlatformNamespace)}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, role, workspace.Name); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = workspaceLabels(workspace.Name)
		role.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{"tau.azure.com"}, Resources: []string{"workspaces", "workspaces/status"}, ResourceNames: []string{workspace.Name}, Verbs: []string{"get"}},
			{APIGroups: []string{"tau.azure.com"}, Resources: []string{"quotarequests"}, Verbs: []string{"create", "get"}},
		}
		return nil
	})
	if err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNamespace(r.PlatformNamespace)}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, binding, workspace.Name); err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, platformNamespace(r.PlatformNamespace))}
		return nil
	})
	return err
}

func rbacSubject(subject tauv1alpha1.KubernetesSubject, serviceAccountNamespace string) rbacv1.Subject {
	out := rbacv1.Subject{Kind: subject.Kind, Name: subject.Name}
	switch subject.Kind {
	case rbacv1.UserKind, rbacv1.GroupKind:
		out.APIGroup = rbacv1.GroupName
	case rbacv1.ServiceAccountKind:
		out.Namespace = serviceAccountNamespace
	}
	return out
}
