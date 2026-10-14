// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	errNamespaceOwnershipUnverified        = errors.New("namespace ownership is not verified")
	errLegacyNamespaceOwnershipStampFailed = errors.New("legacy namespace ownership stamp failed")
)

func resolvedNamespace(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Target.Namespace != "" {
		return workspace.Spec.Target.Namespace
	}
	return workspace.Name
}

func (r *TauWorkspaceReconciler) reconcileNamespace(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace string,
	activateQueue bool,
) (bool, error) {
	if err := validateQuotaValues(workspace.Spec.Quota); err != nil {
		return false, err
	}
	if reason := reservedNamespaceReason(targetNamespace, systemNamespace(r.SystemNamespace)); reason != "" {
		return false, fmt.Errorf("refusing to manage namespace %q: %s", targetNamespace, reason)
	}
	var namespace corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: targetNamespace}, &namespace)
	if apierrors.IsNotFound(err) && workspace.Spec.Target.CreateNamespace {
		labels := workspaceNamespaceLabels(workspace.Name, "")
		delete(labels, labelWorkspaceLocalQueue)
		delete(labels, labelKueueDefaultLocalQueue)
		namespace = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        targetNamespace,
				Labels:      labels,
				Annotations: workspaceNamespaceAnnotations(workspace),
			},
		}
		if err := r.Create(ctx, &namespace); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	owner := namespace.Labels[labelWorkspace]
	if owner != "" && owner != workspace.Name {
		return false, fmt.Errorf(
			"target namespace %q is retained for TauWorkspace %q; explicit operator adoption is required",
			targetNamespace,
			owner,
		)
	}
	explicitAdoption := hasExplicitNamespaceAdoptionEvidence(workspace, &namespace)
	ownerUID := namespace.Annotations[annotationOwnerUID]
	legacyExplicitAdoption := owner == "" &&
		ownerUID == "" &&
		!explicitAdoption &&
		hasLegacyExplicitNamespaceAdoptionEvidence(workspace, &namespace)
	legacyPartialContinuity := ownerUID == "" &&
		workspace.UID != "" &&
		!explicitAdoption &&
		hasLegacyPartialReconcileNamespaceContinuity(workspace, &namespace)
	if ownerUID != "" && ownerUID != string(workspace.UID) && !explicitAdoption {
		return false, fmt.Errorf(
			"target namespace %q is retained from a different TauWorkspace UID %q; explicit operator adoption is required",
			targetNamespace,
			ownerUID,
		)
	}
	if ownerUID == "" &&
		workspace.UID != "" &&
		!explicitAdoption &&
		!legacyExplicitAdoption &&
		!hasAuthoritativeLegacyNamespaceContinuity(workspace, &namespace, targetNamespace) {
		ownership := "has no TauWorkspace ownership metadata"
		if owner == workspace.Name {
			ownership = "has legacy ownership metadata without an owner UID"
		}
		return false, fmt.Errorf(
			"%w: target namespace %q %s; explicit operator adoption is required",
			errNamespaceOwnershipUnverified,
			targetNamespace,
			ownership,
		)
	}
	changed := false
	requiredLabels := map[string]string{
		labelWorkspace: workspace.Name,
	}
	if workspace.Spec.Target.CreateNamespace {
		requiredLabels = workspaceNamespaceLabels(workspace.Name, "")
		delete(requiredLabels, labelWorkspaceLocalQueue)
		delete(requiredLabels, labelKueueDefaultLocalQueue)
	}
	if activateQueue {
		localQueueName, err := r.namespaceLocalQueueName(ctx, workspace, targetNamespace)
		if err != nil {
			return false, err
		}
		requiredLabels[labelWorkspaceLocalQueue] = localQueueName
		requiredLabels[labelKueueDefaultLocalQueue] = localQueueName
	}
	if workspace.Spec.TeamRef != nil {
		requiredLabels[labelTeam] = workspace.Spec.TeamRef.Name
	} else if _, ok := namespace.Labels[labelTeam]; ok {
		delete(namespace.Labels, labelTeam)
		changed = true
	}
	for k, v := range requiredLabels {
		if namespace.Labels[k] != v {
			namespace.Labels[k] = v
			changed = true
		}
	}
	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	if workspace.UID != "" && namespace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		namespace.Annotations[annotationOwnerUID] = string(workspace.UID)
		changed = true
	}
	resultScope := strings.TrimSpace(workspace.Spec.Defaults.OutputRoot)
	if resultScope == "" {
		if _, ok := namespace.Annotations[annotationResultScope]; ok {
			delete(namespace.Annotations, annotationResultScope)
			changed = true
		}
	} else if namespace.Annotations[annotationResultScope] != resultScope {
		namespace.Annotations[annotationResultScope] = resultScope
		changed = true
	}
	if changed {
		if err := r.Update(ctx, &namespace); err != nil {
			if legacyPartialContinuity || legacyExplicitAdoption {
				return false, fmt.Errorf("%w: %w", errLegacyNamespaceOwnershipStampFailed, err)
			}
			return false, err
		}
	}
	return true, nil
}

func (r *TauWorkspaceReconciler) namespaceLocalQueueName(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	targetNamespace string,
) (string, error) {
	if workspace.Spec.TeamRef == nil {
		return workspace.Spec.Queue, nil
	}
	if current := strings.TrimSpace(workspace.Status.Queue.LocalQueue); current != "" {
		return current, nil
	}
	name := workspaceLocalQueueName(workspace)
	queue := newQueueObject(localQueueGVK)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name, Namespace: targetNamespace}, queue); err != nil {
		if apierrors.IsNotFound(err) {
			return workspace.Spec.Queue, nil
		}
		return "", fmt.Errorf("read Team-managed LocalQueue %q: %w", name, err)
	}
	return name, nil
}

func hasAuthoritativeLegacyNamespaceContinuity(
	workspace *tauv1alpha1.TauWorkspace,
	namespace *corev1.Namespace,
	targetNamespace string,
) bool {
	if namespace.Labels[labelWorkspace] != workspace.Name {
		return false
	}
	if hasLegacyPartialReconcileNamespaceContinuity(workspace, namespace) {
		return true
	}
	if workspace.Status.Target.ResolvedNamespace != targetNamespace {
		return false
	}
	for _, condition := range workspace.Status.Conditions {
		if condition.Reason == "AdditionalWorkspaceBlocked" {
			return false
		}
	}
	if workspace.Status.Phase == tauv1alpha1.WorkspacePhaseReady {
		return true
	}
	if workspace.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded ||
		workspace.Status.ObservedGeneration == 0 {
		return false
	}
	drift := findCondition(workspace.Status.Conditions, tauv1alpha1.ConditionDriftDetected)
	if drift == nil {
		return false
	}
	if drift.Status == metav1.ConditionFalse {
		return true
	}
	switch drift.Reason {
	case "NamespaceReconcileFailed", "RBACCleanupFailed", "QueueCleanupFailed", "NamespaceCleanupFailed":
		return true
	default:
		return false
	}
}

func hasLegacyPartialReconcileNamespaceContinuity(
	workspace *tauv1alpha1.TauWorkspace,
	namespace *corev1.Namespace,
) bool {
	if !hasPristineLegacyWorkspaceStatus(workspace) {
		return false
	}
	if workspace.Spec.Target.CreateNamespace {
		if namespace.CreationTimestamp.IsZero() {
			return false
		}
		return !namespace.CreationTimestamp.Before(&workspace.CreationTimestamp)
	}
	if namespace.Labels[labelWorkspaceLocalQueue] != workspace.Spec.Queue ||
		namespace.Labels[labelKueueDefaultLocalQueue] != workspace.Spec.Queue {
		return false
	}
	return managedFieldsOwnLabelAfter(
		namespace.ManagedFields,
		labelWorkspace,
		workspace.CreationTimestamp,
	)
}

func hasPristineLegacyWorkspaceStatus(workspace *tauv1alpha1.TauWorkspace) bool {
	return workspace.Generation == 1 &&
		workspace.Status.ObservedGeneration == 0 &&
		workspace.Status.Target.ResolvedNamespace == "" &&
		workspace.Status.Queue.LocalQueue == "" &&
		workspace.Status.Queue.ClusterQueue == "" &&
		workspace.Status.Queue.ClusterQueueUID == "" &&
		workspace.Status.Phase == "" &&
		len(workspace.Status.Conditions) == 0 &&
		!workspace.CreationTimestamp.IsZero()
}

func managedFieldsOwnLabelAfter(
	entries []metav1.ManagedFieldsEntry,
	label string,
	notBefore metav1.Time,
) bool {
	if notBefore.IsZero() {
		return false
	}
	for _, entry := range entries {
		if entry.Operation != metav1.ManagedFieldsOperationUpdate ||
			entry.Time == nil ||
			entry.Time.Before(&notBefore) ||
			entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		metadata, _ := fields["f:metadata"].(map[string]any)
		labels, _ := metadata["f:labels"].(map[string]any)
		if _, ok := labels["f:"+label]; ok {
			return true
		}
	}
	return false
}

func hasExplicitNamespaceAdoptionEvidence(
	workspace *tauv1alpha1.TauWorkspace,
	namespace *corev1.Namespace,
) bool {
	expectedUID := strings.TrimSpace(workspace.Annotations[annotationAdoptionNamespaceUID])
	return expectedUID != "" &&
		namespace.UID != "" &&
		expectedUID == string(namespace.UID)
}

func hasLegacyExplicitNamespaceAdoptionEvidence(
	workspace *tauv1alpha1.TauWorkspace,
	namespace *corev1.Namespace,
) bool {
	return workspace.Spec.Target.CreateNamespace == false &&
		authorizationMode(workspace) == tauv1alpha1.AuthorizationModeClusterWide &&
		hasLegacyExplicitAdoptionStatus(workspace) &&
		!workspace.CreationTimestamp.IsZero() &&
		!namespace.CreationTimestamp.IsZero() &&
		!namespace.CreationTimestamp.After(workspace.CreationTimestamp.Time)
}

func hasLegacyExplicitAdoptionStatus(workspace *tauv1alpha1.TauWorkspace) bool {
	if hasPristineLegacyWorkspaceStatus(workspace) {
		return true
	}
	if workspace.Generation != 1 ||
		workspace.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded ||
		workspace.Status.ObservedGeneration == 0 ||
		workspace.Status.Target.ResolvedNamespace != "" ||
		workspace.Status.Queue.ClusterQueue != "" ||
		workspace.Status.Queue.ClusterQueueUID != "" ||
		workspace.CreationTimestamp.IsZero() {
		return false
	}
	if localQueue := strings.TrimSpace(workspace.Status.Queue.LocalQueue); localQueue != "" && localQueue != workspace.Spec.Queue {
		return false
	}
	for _, condition := range workspace.Status.Conditions {
		if condition.Reason == "AdditionalWorkspaceBlocked" {
			return false
		}
	}
	drift := findCondition(workspace.Status.Conditions, tauv1alpha1.ConditionDriftDetected)
	if drift == nil || drift.Status != metav1.ConditionTrue {
		return false
	}
	return drift.Reason == "NamespaceReconcileFailed" ||
		drift.Reason == "NamespaceOwnershipUnverified"
}

// reservedNamespaceReason names why a namespace may never be adopted as a
// workspace target, or "" when the namespace is fair game. Adoption is not
// read-only: reconcileNamespace stamps ownership labels onto whatever it
// resolves, and a later retarget hands the previous namespace to
// cleanupStaleNamespaceMetadata, which strips them again. Pointing a workspace
// at the platform or a kube-system namespace would therefore overwrite the
// Helm ownership metadata and drop the Kueue default-queue label of a
// namespace TauGrid does not own.
func reservedNamespaceReason(name, systemNamespace string) string {
	switch {
	case name == systemNamespace, name == tauv1alpha1.SystemNamespace, name == tauv1alpha1.LegacySystemNamespace:
		return "the Tau system namespace is owned by the platform installer"
	case strings.HasPrefix(name, "kube-"):
		return "kube-* namespaces are reserved by Kubernetes"
	case name == metav1.NamespaceDefault:
		return "the default namespace is shared cluster-wide"
	default:
		return ""
	}
}

func workspaceNamespaceLabels(workspace, localQueue string) map[string]string {
	labels := workspaceLabels(workspace)
	labels[labelWorkspaceLocalQueue] = localQueue
	labels[labelKueueDefaultLocalQueue] = localQueue
	labels[labelPSAEnforce] = "baseline"
	labels[labelPSAAudit] = "restricted"
	labels[labelPSAWarn] = "restricted"
	return labels
}

func workspaceNamespaceAnnotations(workspace *tauv1alpha1.TauWorkspace) map[string]string {
	annotations := map[string]string{}
	if workspace.UID != "" {
		annotations[annotationOwnerUID] = string(workspace.UID)
	}
	if resultScope := strings.TrimSpace(workspace.Spec.Defaults.OutputRoot); resultScope != "" {
		annotations[annotationResultScope] = resultScope
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}
