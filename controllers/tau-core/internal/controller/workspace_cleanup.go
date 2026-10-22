// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TauWorkspaceReconciler) getAndValidateWorkspaceOwnership(
	ctx context.Context,
	obj client.Object,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	if workspace.UID == "" {
		return fmt.Errorf("TauWorkspace %q has no UID", workspace.Name)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkspace(obj.GetLabels(), workspace.Name) {
		return fmt.Errorf("%T %s already exists and is not owned by workspace %q", obj, client.ObjectKeyFromObject(obj), workspace.Name)
	}
	return r.ensureWorkspaceObjectUID(ctx, obj, workspace)
}

func validateWorkspaceObjectUID(obj client.Object, workspace *tauv1alpha1.TauWorkspace) error {
	ownerUID := obj.GetAnnotations()[annotationOwnerUID]
	if workspace.UID == "" || ownerUID != string(workspace.UID) {
		return fmt.Errorf(
			"%T %s owner UID is %q, want %q",
			obj,
			client.ObjectKeyFromObject(obj),
			ownerUID,
			workspace.UID,
		)
	}
	return nil
}

func (r *TauWorkspaceReconciler) ensureWorkspaceObjectUID(
	ctx context.Context,
	obj client.Object,
	workspace *tauv1alpha1.TauWorkspace,
) error {
	if err := validateWorkspaceObjectUID(obj, workspace); err == nil {
		return nil
	}
	if obj.GetAnnotations()[annotationOwnerUID] != "" ||
		workspace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		return validateWorkspaceObjectUID(obj, workspace)
	}
	setOwnerUIDAnnotation(obj, workspace.UID)
	if err := r.Update(ctx, obj); err != nil {
		return fmt.Errorf("persist owner UID on %T %s: %w", obj, client.ObjectKeyFromObject(obj), err)
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupStaleNamespaceMetadata(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespaceName string,
) error {
	namespace, err := r.workspaceNamespaceForMetadataCleanup(ctx, workspace, namespaceName)
	if err != nil || namespace == nil {
		return err
	}
	inUse, message, err := r.namespaceRoutingMetadataInUse(ctx, workspace, namespace)
	if err != nil {
		return err
	}
	if inUse {
		return errors.New(message)
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
	return r.Update(ctx, namespace)
}

func (r *TauWorkspaceReconciler) workspaceNamespaceForMetadataCleanup(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespaceName string,
) (*corev1.Namespace, error) {
	if reservedNamespaceReason(namespaceName, systemNamespace(r.SystemNamespace)) != "" {
		return nil, nil
	}
	var namespace corev1.Namespace
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: namespaceName}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if namespace.Labels[labelWorkspace] != workspace.Name ||
		namespace.Annotations[annotationOwnerUID] != string(workspace.UID) ||
		workspace.UID == "" {
		return nil, nil
	}
	return &namespace, nil
}

func (r *TauWorkspaceReconciler) waitForNamespaceRoutingMetadataDrain(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespaceName string,
) error {
	namespace, err := r.workspaceNamespaceForMetadataCleanup(ctx, workspace, namespaceName)
	if err != nil || namespace == nil {
		return err
	}
	inUse, message, err := r.namespaceRoutingMetadataInUse(ctx, workspace, namespace)
	if err != nil {
		return err
	}
	if inUse {
		return errors.New(message)
	}
	return nil
}

func (r *TauWorkspaceReconciler) namespaceRoutingMetadataInUse(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	namespace *corev1.Namespace,
) (bool, string, error) {
	workloads := &unstructured.UnstructuredList{}
	workloads.SetGroupVersionKind(schema.GroupVersionKind{
		Group: workloadGVK.Group, Version: workloadGVK.Version, Kind: workloadGVK.Kind + "List",
	})
	if err := r.apiReader().List(ctx, workloads, client.InNamespace(namespace.Name)); err != nil {
		return false, "", fmt.Errorf("list Workloads in namespace %q before routing metadata cleanup: %w", namespace.Name, err)
	}
	cleanedLabels := make(map[string]string, len(namespace.Labels))
	for key, value := range namespace.Labels {
		cleanedLabels[key] = value
	}
	for _, key := range []string{
		labelManagedBy,
		labelTeam,
		labelWorkspace,
		labelWorkspaceLocalQueue,
		labelKueueDefaultLocalQueue,
	} {
		delete(cleanedLabels, key)
	}
	for i := range workloads.Items {
		workload := &workloads.Items[i]
		if workloadFinished(workload) {
			continue
		}
		queueName, found, err := unstructured.NestedString(workload.Object, "spec", "queueName")
		if err != nil {
			return false, "", fmt.Errorf("read Workload %s queueName: %w", client.ObjectKeyFromObject(workload), err)
		}
		queueName = strings.TrimSpace(queueName)
		if !found || queueName == "" {
			continue
		}
		localQueue := newQueueObject(localQueueGVK)
		localQueueKey := client.ObjectKey{Name: queueName, Namespace: namespace.Name}
		if err := r.apiReader().Get(ctx, localQueueKey, localQueue); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, "", fmt.Errorf("read LocalQueue %s before routing metadata cleanup: %w", localQueueKey, err)
		}
		clusterQueueName, found, err := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
		if err != nil {
			return false, "", fmt.Errorf("read LocalQueue %s backing ClusterQueue: %w", localQueueKey, err)
		}
		if !found || strings.TrimSpace(clusterQueueName) == "" {
			return false, "", fmt.Errorf("LocalQueue %s has no backing ClusterQueue", localQueueKey)
		}
		admittedClusterQueue, admitted, err := unstructured.NestedString(workload.Object, "status", "admission", "clusterQueue")
		if err != nil {
			return false, "", fmt.Errorf("read Workload %s admitted ClusterQueue: %w", client.ObjectKeyFromObject(workload), err)
		}
		if admitted && strings.TrimSpace(admittedClusterQueue) != "" {
			clusterQueueName = admittedClusterQueue
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.apiReader().Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
			return false, "", fmt.Errorf("read ClusterQueue %s before routing metadata cleanup: %w", clusterQueueName, err)
		}
		if expectedUID := strings.TrimSpace(workspace.Status.Queue.ClusterQueueUID); expectedUID != "" &&
			clusterQueueName == strings.TrimSpace(workspace.Status.Queue.ClusterQueue) &&
			string(clusterQueue.GetUID()) != expectedUID {
			return false, "", fmt.Errorf(
				"ClusterQueue %q UID changed from %q to %q before routing metadata cleanup",
				clusterQueueName,
				expectedUID,
				clusterQueue.GetUID(),
			)
		}
		selectorMap, found, err := unstructured.NestedMap(clusterQueue.Object, "spec", "namespaceSelector")
		if err != nil {
			return false, "", fmt.Errorf("read ClusterQueue %q namespaceSelector: %w", clusterQueueName, err)
		}
		labelSelector := metav1.LabelSelector{}
		if found {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selectorMap, &labelSelector); err != nil {
				return false, "", fmt.Errorf("read ClusterQueue %q namespaceSelector: %w", clusterQueueName, err)
			}
		}
		selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
		if err != nil {
			return false, "", fmt.Errorf("read ClusterQueue %q namespaceSelector: %w", clusterQueueName, err)
		}
		if !selector.Matches(labels.Set(namespace.Labels)) {
			return false, "", fmt.Errorf(
				"ClusterQueue %q namespaceSelector does not match namespace %q for unfinished Workload %s",
				clusterQueueName,
				namespace.Name,
				client.ObjectKeyFromObject(workload),
			)
		}
		if selector.Matches(labels.Set(cleanedLabels)) {
			continue
		}
		return true, fmt.Sprintf(
			"waiting for Workload %s using retained LocalQueue %q and ClusterQueue %q before removing namespace routing metadata",
			client.ObjectKeyFromObject(workload),
			queueName,
			clusterQueueName,
		), nil
	}
	return false, "", nil
}

func (r *TauWorkspaceReconciler) cleanupWorkspaceAccess(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	targetNamespace := strings.TrimSpace(workspace.Status.Target.ResolvedNamespace)
	if targetNamespace == "" {
		targetNamespace = resolvedNamespace(workspace)
	}
	if err := r.waitForNamespaceRoutingMetadataDrain(ctx, workspace, targetNamespace); err != nil {
		return err
	}
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
	if err := r.cleanupSystemReaderRBAC(ctx, workspace); err != nil {
		return err
	}
	return r.cleanupStaleNamespaceMetadata(ctx, workspace, targetNamespace)
}

func (r *TauWorkspaceReconciler) cleanupWorkspaceClusterQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	queue := newQueueObject(clusterQueueGVK)
	queue.SetName(workspaceClusterQueueName(workspace.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(queue), queue); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkspace(queue.GetLabels(), workspace.Name) {
		if queue.GetLabels()[labelManagedBy] != labelManagedByValue {
			return nil
		}
		retainedQueue, retained, err := retainedEstablishedWorkspaceClusterQueue(ctx, r.apiReader(), workspace)
		if err != nil {
			return err
		}
		if retained && retainedQueue == queue.GetName() {
			return nil
		}
		return fmt.Errorf("refusing to delete ClusterQueue %q: object is not owned by workspace %q", queue.GetName(), workspace.Name)
	}
	if err := validateWorkspaceObjectUID(queue, workspace); err != nil {
		return fmt.Errorf("refusing to delete ClusterQueue %q: %w", queue.GetName(), err)
	}
	if expectedUID := workspace.Status.Queue.ClusterQueueUID; workspace.Status.Queue.ClusterQueue == queue.GetName() &&
		expectedUID != "" && string(queue.GetUID()) != expectedUID {
		return fmt.Errorf("refusing to delete ClusterQueue %q: UID changed from %q to %q", queue.GetName(), expectedUID, queue.GetUID())
	}
	if err := r.deleteWithPreconditions(ctx, queue); err != nil {
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
			if role, ok := obj.(*rbacv1.Role); ok && canDeleteLegacyWorkspaceRole(role, workspace) {
				if err := r.deleteWithPreconditions(ctx, role); err != nil {
					return err
				}
				continue
			}
			if err := r.ensureWorkspaceObjectUID(ctx, obj, workspace); err != nil {
				return fmt.Errorf("refusing to delete stale workspace access: %w", err)
			}
			if err := r.deleteWithPreconditions(ctx, obj); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupOwnedNamespaceAccess(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace, keepServiceAccount string,
) error {
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces, client.MatchingLabels{
		labelWorkspace: workspace.Name,
	}); err != nil {
		return err
	}
	for i := range namespaces.Items {
		namespace := &namespaces.Items[i]
		if workspace.UID == "" ||
			namespace.Annotations[annotationOwnerUID] != string(workspace.UID) {
			continue
		}
		if err := r.cleanupStaleWorkloadIdentityServiceAccounts(
			ctx,
			workspace,
			namespace.Name,
			keepServiceAccount,
		); err != nil {
			return err
		}
	}
	return r.reconcileOwnedNamespaceResearcherAccess(ctx, workspace, targetNamespace)
}

func canDeleteLegacyWorkspaceRole(role *rbacv1.Role, workspace *tauv1alpha1.TauWorkspace) bool {
	return workspace.UID != "" &&
		workspace.Annotations[annotationOwnerUID] == string(workspace.UID) &&
		role.UID != "" &&
		role.Annotations[annotationOwnerUID] == "" &&
		ownedByWorkspace(role.Labels, workspace.Name)
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
		if preserveWorkspaceLocalQueue(workspace, localQueue, keepNamespace, keepName) {
			continue
		}
		if err := r.ensureWorkspaceObjectUID(ctx, localQueue, workspace); err != nil {
			return fmt.Errorf("refusing to delete LocalQueue %q: %w", localQueue.GetName(), err)
		}
		if err := r.deleteWithPreconditions(ctx, localQueue); err != nil {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) staleWorkspaceLocalQueuesDrained(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	keepNamespace, keepName string,
) (bool, string, error) {
	queues := &unstructured.UnstructuredList{}
	queues.SetGroupVersionKind(schema.GroupVersionKind{
		Group: localQueueGVK.Group, Version: localQueueGVK.Version, Kind: localQueueGVK.Kind + "List",
	})
	if err := r.List(ctx, queues, client.MatchingLabels{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspace.Name,
	}); err != nil {
		return false, "", err
	}
	for i := range queues.Items {
		localQueue := &queues.Items[i]
		if preserveWorkspaceLocalQueue(workspace, localQueue, keepNamespace, keepName) {
			continue
		}
		if err := r.ensureWorkspaceObjectUID(ctx, localQueue, workspace); err != nil {
			return false, "", fmt.Errorf("cannot verify stale LocalQueue %q: %w", localQueue.GetName(), err)
		}
		workloads := &unstructured.UnstructuredList{}
		workloads.SetGroupVersionKind(schema.GroupVersionKind{
			Group: workloadGVK.Group, Version: workloadGVK.Version, Kind: workloadGVK.Kind + "List",
		})
		if err := r.List(ctx, workloads, client.InNamespace(localQueue.GetNamespace())); err != nil {
			return false, "", err
		}
		for j := range workloads.Items {
			workload := &workloads.Items[j]
			queueName, _, err := unstructured.NestedString(workload.Object, "spec", "queueName")
			if err != nil {
				return false, "", fmt.Errorf("read Workload %s queueName: %w", client.ObjectKeyFromObject(workload), err)
			}
			if queueName != localQueue.GetName() || workloadFinished(workload) {
				continue
			}
			return false, fmt.Sprintf(
				"waiting for Workload %s to finish before removing LocalQueue %q",
				client.ObjectKeyFromObject(workload),
				localQueue.GetName(),
			), nil
		}
	}
	return true, "", nil
}

func preserveWorkspaceLocalQueue(
	workspace *tauv1alpha1.TauWorkspace,
	localQueue *unstructured.Unstructured,
	keepNamespace, keepName string,
) bool {
	if localQueue.GetNamespace() != keepNamespace {
		return false
	}
	if localQueue.GetName() == keepName {
		return true
	}
	for _, name := range workspaceCompatibilityLocalQueueNames(workspace) {
		if localQueue.GetName() == name {
			return true
		}
	}
	return false
}

func workloadFinished(workload *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(workload.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, rawCondition := range conditions {
		condition, ok := rawCondition.(map[string]any)
		if ok && condition["type"] == "Finished" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func (r *TauWorkspaceReconciler) cleanupStaleWorkloadIdentityServiceAccounts(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace, keepServiceAccount string,
) error {
	var accounts corev1.ServiceAccountList
	if err := r.List(ctx, &accounts, client.InNamespace(targetNamespace), client.MatchingLabels{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspace.Name,
	}); err != nil {
		return err
	}
	for i := range accounts.Items {
		account := &accounts.Items[i]
		desiredClientID := ""
		if workspace.Spec.WorkloadIdentity != nil {
			desiredClientID = strings.TrimSpace(workspace.Spec.WorkloadIdentity.ClientID)
		}
		if keepServiceAccount != "" &&
			account.Name == keepServiceAccount &&
			strings.TrimSpace(account.Annotations[annotationAzureWIClientID]) == desiredClientID {
			continue
		}
		if err := r.ensureWorkspaceObjectUID(ctx, account, workspace); err != nil {
			return fmt.Errorf("refusing to delete stale workload identity ServiceAccount: %w", err)
		}
		if err := r.deleteWithPreconditions(ctx, account); err != nil {
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
	if err := r.ensureWorkspaceObjectUID(ctx, obj, workspace); err != nil {
		return fmt.Errorf("refusing to delete %T %s: %w", obj, key, err)
	}
	return r.deleteWithPreconditions(ctx, obj)
}

func (r *TauWorkspaceReconciler) deleteWithPreconditions(ctx context.Context, obj client.Object) error {
	return deleteObjectWithPreconditions(ctx, r.Client, obj)
}

func deleteObjectWithPreconditions(ctx context.Context, writer client.Client, obj client.Object) error {
	deleteOptions := []client.DeleteOption{}
	uid := obj.GetUID()
	resourceVersion := obj.GetResourceVersion()
	if uid != "" || resourceVersion != "" {
		preconditions := client.Preconditions{}
		if uid != "" {
			preconditions.UID = &uid
		}
		if resourceVersion != "" {
			preconditions.ResourceVersion = &resourceVersion
		}
		deleteOptions = append(deleteOptions, preconditions)
	}
	if err := writer.Delete(ctx, obj, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
