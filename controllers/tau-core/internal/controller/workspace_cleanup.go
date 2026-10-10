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

func (r *TauWorkspaceReconciler) getAndValidateWorkspaceOwnership(
	ctx context.Context,
	obj client.Object,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkspace(obj.GetLabels(), workspace.Name) {
		return fmt.Errorf("%T %s already exists and is not owned by workspace %q", obj, client.ObjectKeyFromObject(obj), workspace.Name)
	}
	if err := validateWorkspaceObjectUID(obj, workspace); err != nil {
		return err
	}
	return nil
}

func validateWorkspaceObjectUID(obj client.Object, workspace *tauv1alpha1.TauWorkspace) error {
	ownerUID := obj.GetAnnotations()[annotationOwnerUID]
	if ownerUID != "" && ownerUID != string(workspace.UID) {
		return fmt.Errorf("%T %s belongs to a different workspace UID %q", obj, client.ObjectKeyFromObject(obj), ownerUID)
	}
	if ownerUID == "" && workspace.UID != "" && workspace.Status.Target.ResolvedNamespace == "" {
		return fmt.Errorf(
			"%T %s has legacy ownership metadata without an owner UID; explicit operator adoption is required",
			obj,
			client.ObjectKeyFromObject(obj),
		)
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupStaleNamespaceMetadata(ctx context.Context, workspaceName, namespaceName string) error {
	// The previous namespace comes off status and never passes through
	// reconcileNamespace, so it needs its own reserved-namespace check: a
	// workspace that once targeted a reserved namespace must not strip its
	// labels on the way out.
	if reservedNamespaceReason(namespaceName, systemNamespace(r.SystemNamespace)) != "" {
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
		labelTeam,
		labelWorkspace,
		labelWorkspaceLocalQueue,
		labelKueueDefaultLocalQueue,
	} {
		delete(namespace.Labels, key)
	}
	if namespace.Annotations != nil {
		delete(namespace.Annotations, annotationResultScope)
		delete(namespace.Annotations, annotationOwnerUID)
	}
	return r.Update(ctx, &namespace)
}

func (r *TauWorkspaceReconciler) cleanupWorkspaceAccess(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	if err := r.cleanupStaleTargetRBAC(ctx, workspace, "", "", false); err != nil {
		return err
	}
	if err := r.cleanupClusterQueueReaderRBAC(ctx, workspace); err != nil {
		return err
	}
	if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspace, "", ""); err != nil {
		return err
	}
	if err := r.cleanupWorkspaceClusterQueue(ctx, workspace); err != nil {
		return err
	}
	return r.cleanupSystemReaderRBAC(ctx, workspace)
}

func (r *TauWorkspaceReconciler) cleanupWorkspaceClusterQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	workspaceName := workspace.Name
	queue := newQueueObject(clusterQueueGVK)
	queue.SetName(workspaceClusterQueueName(workspaceName))
	if err := r.Get(ctx, client.ObjectKeyFromObject(queue), queue); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkspace(queue.GetLabels(), workspaceName) {
		return fmt.Errorf("refusing to delete ClusterQueue %q: object is not owned by workspace %q", queue.GetName(), workspaceName)
	}
	ownerUID := queue.GetAnnotations()[annotationOwnerUID]
	if ownerUID != "" && ownerUID != string(workspace.UID) {
		return fmt.Errorf("refusing to delete ClusterQueue %q: owner UID is %q, not %q", queue.GetName(), ownerUID, workspace.UID)
	}
	if ownerUID == "" && queue.GetUID() != "" && workspace.UID != "" {
		return fmt.Errorf("refusing to delete ClusterQueue %q without authoritative owner UID", queue.GetName())
	}
	if expectedUID := workspace.Status.Queue.ClusterQueueUID; expectedUID != "" && string(queue.GetUID()) != expectedUID {
		return fmt.Errorf("refusing to delete ClusterQueue %q: UID changed from %q to %q", queue.GetName(), expectedUID, queue.GetUID())
	}
	if err := r.Delete(ctx, queue); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	remaining := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKeyFromObject(queue), remaining); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return fmt.Errorf(
		"waiting for ClusterQueue %q deletion to complete; finalizers=%v",
		remaining.GetName(),
		remaining.GetFinalizers(),
	)
}

func (r *TauWorkspaceReconciler) cleanupStaleTargetRBAC(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	keepNamespace, keepServiceAccount string,
	keepResearcherBinding bool,
) error {
	workspaceName := workspace.Name
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
			if obj.GetNamespace() == systemNamespace(r.SystemNamespace) {
				continue
			}
			if keepNamespace != "" && obj.GetNamespace() == keepNamespace && candidate.keep(obj) {
				continue
			}
			if err := validateWorkspaceObjectUID(obj, workspace); err != nil {
				return fmt.Errorf("refusing to delete stale workspace access: %w", err)
			}
			if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupStaleWorkspaceLocalQueues(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	keepNamespace, keepName string,
) error {
	workspaceName := workspace.Name
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
		if err := validateWorkspaceObjectUID(localQueue, workspace); err != nil {
			return fmt.Errorf("refusing to delete LocalQueue %q: %w", localQueue.GetName(), err)
		}
		if err := r.Delete(ctx, localQueue); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupSystemReaderRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	name := workspaceReaderRBACName(workspace.Name)
	for _, obj := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: systemNamespace(r.SystemNamespace)}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: systemNamespace(r.SystemNamespace)}},
	} {
		if err := r.deleteOwnedObject(ctx, obj, workspace); err != nil {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) deleteOwnedObject(
	ctx context.Context,
	obj client.Object,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkspace(obj.GetLabels(), workspace.Name) {
		return fmt.Errorf("refusing to delete %T %s: object is not owned by workspace %q", obj, key, workspace.Name)
	}
	if err := validateWorkspaceObjectUID(obj, workspace); err != nil {
		return fmt.Errorf("refusing to delete %T %s: %w", obj, key, err)
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
