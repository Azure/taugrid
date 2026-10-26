// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *TauWorkspaceReconciler) reconcileRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, string, error) {
	if authorizationMode(workspace) == tauv1alpha1.AuthorizationModeClusterWide {
		if err := r.cleanupResearcherRBAC(ctx, workspace, targetNamespace); err != nil {
			return false, "failed to remove subject-specific researcher RBAC for cluster-wide authorization", err
		}
		if err := r.cleanupClusterQueueReaderRBAC(ctx, workspace); err != nil {
			return false, "failed to remove subject-specific ClusterQueue reader RBAC for cluster-wide authorization", err
		}
		if err := r.cleanupSystemReaderRBAC(ctx, workspace); err != nil {
			return false, "failed to remove subject-specific workspace reader RBAC for cluster-wide authorization", err
		}
		return true, "workspace relies on pre-existing cluster authorization; the controller grants no researcher access", nil
	}
	if workspace.Spec.PrincipalRef == nil || workspace.Spec.KubernetesSubject == nil {
		return false, "workspace-rbac authorization requires principalRef and kubernetesSubject", nil
	}
	if err := r.reconcileResearcherRoleBinding(ctx, workspace, targetNamespace); err != nil {
		return false, "failed to reconcile researcher RoleBinding", err
	}
	if err := r.reconcileClusterQueueReaderRBAC(ctx, workspace, targetNamespace); err != nil {
		return false, "failed to reconcile ClusterQueue reader ClusterRoleBinding", err
	}
	if err := r.reconcileSystemReaderRBAC(ctx, workspace); err != nil {
		return false, "failed to reconcile workspace reader RoleBinding", err
	}
	return true, "researcher namespace and ClusterQueue reader bindings are reconciled", nil
}

func (r *TauWorkspaceReconciler) reconcileResearcherRoleBinding(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespace string,
) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: namespace}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, binding, workspace); err != nil {
		return fmt.Errorf("refusing to adopt existing researcher RoleBinding: %w", err)
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		setOwnerUIDAnnotation(binding, workspace.UID)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: defaultRoleName}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, namespace)}
		return nil
	})
	return err
}

func authorizationMode(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Authorization == nil || workspace.Spec.Authorization.Mode == "" {
		return tauv1alpha1.AuthorizationModeWorkspaceRBAC
	}
	return workspace.Spec.Authorization.Mode
}

func (r *TauWorkspaceReconciler) reconcileOwnedNamespaceResearcherAccess(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace string,
) error {
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces, client.MatchingLabels{
		labelWorkspace: workspace.Name,
	}); err != nil {
		return err
	}
	for i := range namespaces.Items {
		namespace := &namespaces.Items[i]
		if namespace.Name == targetNamespace ||
			workspace.UID == "" ||
			namespace.Annotations[annotationOwnerUID] != string(workspace.UID) {
			continue
		}
		if err := r.reconcileExistingResearcherAccess(ctx, workspace, namespace.Name); err != nil {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) reconcileExistingResearcherAccess(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespace string,
) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		if apierrors.IsNotFound(err) &&
			namespace == workspace.Status.Target.ResolvedNamespace &&
			authorizationMode(workspace) != tauv1alpha1.AuthorizationModeClusterWide &&
			workspace.Spec.PrincipalRef != nil && workspace.Spec.KubernetesSubject != nil {
			return r.reconcileResearcherRoleBinding(ctx, workspace, namespace)
		}
		return client.IgnoreNotFound(err)
	}
	if !ownedByWorkspace(binding.Labels, workspace.Name) {
		return fmt.Errorf("RoleBinding %s is not owned by workspace %q", client.ObjectKeyFromObject(binding), workspace.Name)
	}
	if err := r.ensureWorkspaceObjectUID(ctx, binding, workspace); err != nil {
		return err
	}
	if authorizationMode(workspace) == tauv1alpha1.AuthorizationModeClusterWide ||
		workspace.Spec.KubernetesSubject == nil {
		return r.deleteWithPreconditions(ctx, binding)
	}
	desiredSubjects := []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, namespace)}
	desiredRoleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: defaultRoleName}
	if apiequality.Semantic.DeepEqual(binding.Subjects, desiredSubjects) &&
		apiequality.Semantic.DeepEqual(binding.RoleRef, desiredRoleRef) {
		return nil
	}
	binding.Subjects = desiredSubjects
	binding.RoleRef = desiredRoleRef
	return r.Update(ctx, binding)
}

func (r *TauWorkspaceReconciler) cleanupResearcherRBAC(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace string,
) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: targetNamespace}}
	return r.deleteOwnedObject(ctx, binding, workspace)
}

func (r *TauWorkspaceReconciler) cleanupOwnedResearcherRoleBindings(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	var bindings rbacv1.RoleBindingList
	if err := r.List(ctx, &bindings, client.MatchingLabels{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspace.Name,
	}); err != nil {
		return err
	}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.Name != defaultRoleName {
			continue
		}
		if err := r.deleteOwnedObject(ctx, binding, workspace); err != nil {
			return err
		}
	}
	return nil
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
	if err := r.getAndValidateWorkspaceOwnership(ctx, binding, workspace); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		setOwnerUIDAnnotation(binding, workspace.UID)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterQueueReaderRoleName}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, serviceAccountNamespace)}
		return nil
	})
	return err
}

func (r *TauWorkspaceReconciler) cleanupClusterQueueReaderRBAC(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterQueueReaderBindingName(workspace.Name)}}
	return r.deleteOwnedObject(ctx, binding, workspace)
}

func (r *TauWorkspaceReconciler) reconcileWorkloadIdentity(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, string, error) {
	if workspace.Spec.WorkloadIdentity == nil {
		return false, "workspace does not configure workload identity defaults", nil
	}
	wi := workspace.Spec.WorkloadIdentity
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: wi.ServiceAccountName, Namespace: targetNamespace}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, serviceAccount, workspace); err != nil {
		return false, "refusing to adopt existing workload identity ServiceAccount", err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		if serviceAccount.Labels == nil {
			serviceAccount.Labels = map[string]string{}
		}
		for k, v := range workspaceLabels(workspace.Name) {
			serviceAccount.Labels[k] = v
		}
		setOwnerUIDAnnotation(serviceAccount, workspace.UID)
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

func (r *TauWorkspaceReconciler) reconcileSystemReaderRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	name := workspaceReaderRBACName(workspace.Name)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: systemNamespace(r.SystemNamespace)}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, role, workspace); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = workspaceLabels(workspace.Name)
		setOwnerUIDAnnotation(role, workspace.UID)
		role.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{"tau.azure.com"}, Resources: []string{"workspaces", "workspaces/status"}, ResourceNames: []string{workspace.Name}, Verbs: []string{"get"}},
			{APIGroups: []string{"tau.azure.com"}, Resources: []string{"quotarequests"}, Verbs: []string{"create", "get"}},
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"tau-log-connection"}, Verbs: []string{"get"}},
		}
		return nil
	})
	if err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: systemNamespace(r.SystemNamespace)}}
	if err := r.getAndValidateWorkspaceOwnership(ctx, binding, workspace); err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		setOwnerUIDAnnotation(binding, workspace.UID)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, systemNamespace(r.SystemNamespace))}
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
