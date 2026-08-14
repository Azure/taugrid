// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TauWorkspaceReconciler) getAndValidateWorkspaceOwnership(ctx context.Context, obj client.Object, workspaceName string) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if ownedByWorkspace(obj.GetLabels(), workspaceName) {
		return nil
	}
	return fmt.Errorf("%T %s already exists and is not owned by workspace %q", obj, client.ObjectKeyFromObject(obj), workspaceName)
}

func (r *TauWorkspaceReconciler) cleanupStaleNamespaceMetadata(ctx context.Context, workspaceName, namespaceName string) error {
	// The previous namespace comes off status and never passes through
	// reconcileNamespace, so it needs its own reserved-namespace check: a
	// workspace that once targeted a reserved namespace must not strip its
	// labels on the way out.
	if reservedNamespaceReason(namespaceName, platformNamespace(r.PlatformNamespace)) != "" {
		return nil
	}
	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if namespace.Labels[labelWorkspace] != workspaceName {
		return nil
	}
	for _, key := range []string{
		labelManagedBy,
		labelWorkspace,
		labelWorkspaceLocalQueue,
		labelKueueDefaultLocalQueue,
	} {
		delete(namespace.Labels, key)
	}
	if namespace.Annotations != nil {
		delete(namespace.Annotations, annotationResultScope)
	}
	return r.Update(ctx, &namespace)
}

func (r *TauWorkspaceReconciler) cleanupWorkspaceAccess(ctx context.Context, workspaceName string) error {
	if err := r.cleanupStaleTargetRBAC(ctx, workspaceName, "", "", false); err != nil {
		return err
	}
	if err := r.cleanupClusterQueueReaderRBAC(ctx, workspaceName); err != nil {
		return err
	}
	if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspaceName, "", ""); err != nil {
		return err
	}
	return r.cleanupPlatformReaderRBAC(ctx, workspaceName)
}

// ownerWorkspaceAbsent reports whether the TauWorkspace named on a namespace's
// ownership label no longer exists. Namespace ownership metadata is retained on
// deletion, so this check allows a later workspace to reclaim an orphaned target.
func (r *TauWorkspaceReconciler) ownerWorkspaceAbsent(ctx context.Context, owner string) (bool, error) {
	var existing tauv1alpha1.TauWorkspace
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: owner, Namespace: platformNamespace(r.PlatformNamespace)}, &existing)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (r *TauWorkspaceReconciler) cleanupStaleTargetRBAC(
	ctx context.Context,
	workspaceName, keepNamespace, keepServiceAccount string,
	keepResearcherBinding bool,
) error {
	lists := []struct {
		list client.ObjectList
		keep func(client.Object) bool
	}{
		{list: &rbacv1.RoleList{}, keep: func(client.Object) bool { return false }},
		{
			list: &rbacv1.RoleBindingList{},
			keep: func(obj client.Object) bool {
				return keepResearcherBinding && obj.GetName() == defaultRoleName
			},
		},
		{
			list: &corev1.ServiceAccountList{},
			keep: func(obj client.Object) bool {
				return keepServiceAccount != "" && obj.GetName() == keepServiceAccount
			},
		},
	}
	for _, candidate := range lists {
		if err := r.List(ctx, candidate.list, client.MatchingLabels{
			labelManagedBy: labelManagedByValue,
			labelWorkspace: workspaceName,
		}); err != nil {
			return err
		}
		items, err := meta.ExtractList(candidate.list)
		if err != nil {
			return err
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			if obj.GetNamespace() == platformNamespace(r.PlatformNamespace) {
				continue
			}
			if keepNamespace != "" && obj.GetNamespace() == keepNamespace && candidate.keep(obj) {
				continue
			}
			if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupStaleWorkspaceLocalQueues(ctx context.Context, workspaceName, keepNamespace, keepName string) error {
	queues := &unstructured.UnstructuredList{}
	queues.SetGroupVersionKind(schema.GroupVersionKind{
		Group: localQueueGVK.Group, Version: localQueueGVK.Version, Kind: localQueueGVK.Kind + "List",
	})
	if err := r.List(ctx, queues, client.MatchingLabels{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspaceName,
	}); err != nil {
		return err
	}
	for i := range queues.Items {
		localQueue := &queues.Items[i]
		if localQueue.GetNamespace() == keepNamespace && localQueue.GetName() == keepName {
			continue
		}
		if err := r.Delete(ctx, localQueue); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupPlatformReaderRBAC(ctx context.Context, workspaceName string) error {
	name := workspaceReaderRBACName(workspaceName)
	for _, obj := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNamespace(r.PlatformNamespace)}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNamespace(r.PlatformNamespace)}},
	} {
		if err := r.deleteOwnedObject(ctx, obj, workspaceName); err != nil {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) deleteOwnedObject(ctx context.Context, obj client.Object, workspaceName string) error {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkspace(obj.GetLabels(), workspaceName) {
		return fmt.Errorf("refusing to delete %T %s: object is not owned by workspace %q", obj, key, workspaceName)
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
