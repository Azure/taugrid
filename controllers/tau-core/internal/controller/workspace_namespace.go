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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errNamespaceOwnershipUnverified = errors.New("namespace ownership is not verified")

func resolvedNamespace(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Target.Namespace != "" {
		return workspace.Spec.Target.Namespace
	}
	return workspace.Name
}

func (r *TauWorkspaceReconciler) migrateLegacyWorkspaceOwnership(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
) (bool, error) {
	if workspace.UID == "" {
		return false, fmt.Errorf("%w: TauWorkspace %q has no UID", errNamespaceOwnershipUnverified, workspace.Name)
	}
	if ownerUID := workspace.Annotations[annotationOwnerUID]; ownerUID != "" {
		if ownerUID != string(workspace.UID) {
			return false, fmt.Errorf("TauWorkspace %q owner UID is %q, want %q", workspace.Name, ownerUID, workspace.UID)
		}
		return false, nil
	}
	if workspace.Annotations[annotationV0Primary] != "true" {
		return false, nil
	}

	targetNamespace := resolvedNamespace(workspace)
	var namespace corev1.Namespace
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: targetNamespace}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			if workspace.DeletionTimestamp.IsZero() {
				return false, nil
			}
			return r.persistLegacyWorkspaceOwnerUID(ctx, workspace)
		}
		return false, fmt.Errorf("verify legacy TauWorkspace %q namespace ownership: %w", workspace.Name, err)
	}
	if !legacyNamespaceMatchesWorkspace(&namespace, workspace) {
		return false, nil
	}
	if ownerUID := namespace.Annotations[annotationOwnerUID]; ownerUID != "" && ownerUID != string(workspace.UID) {
		return false, nil
	}
	if namespace.Annotations[annotationOwnerUID] == "" {
		setOwnerUIDAnnotation(&namespace, workspace.UID)
		if err := r.Update(ctx, &namespace); err != nil {
			return false, fmt.Errorf("persist legacy Namespace owner UID: %w", err)
		}
		return true, nil
	}

	return r.persistLegacyWorkspaceOwnerUID(ctx, workspace)
}

func legacyNamespaceMatchesWorkspace(
	namespace *corev1.Namespace,
	workspace *tauv1alpha1.TauWorkspace,
) bool {
	if namespace.Labels[labelWorkspace] != workspace.Name {
		return false
	}
	if workspace.Spec.Target.CreateNamespace {
		return namespace.Labels[labelManagedBy] == labelManagedByValue
	}
	queue := strings.TrimSpace(workspace.Spec.Queue)
	return queue != "" &&
		namespace.Labels[labelWorkspaceLocalQueue] == queue &&
		namespace.Labels[labelKueueDefaultLocalQueue] == queue
}

func (r *TauWorkspaceReconciler) persistLegacyWorkspaceOwnerUID(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
) (bool, error) {
	before := workspace.DeepCopy()
	setOwnerUIDAnnotation(workspace, workspace.UID)
	if err := r.Patch(ctx, workspace, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("persist legacy TauWorkspace ownership migration: %w", err)
	}
	return true, nil
}

func (r *TauWorkspaceReconciler) reconcileNamespace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, error) {
	if workspace.UID == "" {
		return false, fmt.Errorf("%w: TauWorkspace %q has no UID", errNamespaceOwnershipUnverified, workspace.Name)
	}
	if reason := reservedNamespaceReason(targetNamespace, systemNamespace(r.SystemNamespace)); reason != "" {
		return false, fmt.Errorf("refusing to manage namespace %q: %s", targetNamespace, reason)
	}
	var namespace corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: targetNamespace}, &namespace)
	if apierrors.IsNotFound(err) && workspace.Spec.Target.CreateNamespace {
		namespace = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        targetNamespace,
				Labels:      workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue),
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
	adoptionNamespaceUID := strings.TrimSpace(workspace.Annotations[annotationAdoptionNamespaceUID])
	if adoptionNamespaceUID != "" && adoptionNamespaceUID != string(namespace.UID) {
		return false, fmt.Errorf(
			"target namespace %q UID is %q, but adoption evidence names %q",
			targetNamespace,
			namespace.UID,
			adoptionNamespaceUID,
		)
	}
	explicitAdoption := hasExplicitNamespaceAdoptionEvidence(workspace, &namespace)
	if owner != "" && owner != workspace.Name {
		if !explicitAdoption {
			return false, fmt.Errorf(
				"target namespace %q is assigned to TauWorkspace %q; explicit UID-bound adoption is required",
				targetNamespace,
				owner,
			)
		}
		var existingOwner tauv1alpha1.TauWorkspace
		err := r.apiReader().Get(ctx, client.ObjectKey{Name: owner, Namespace: workspace.Namespace}, &existingOwner)
		if err == nil {
			return false, fmt.Errorf(
				"target namespace %q is still assigned to existing TauWorkspace %q",
				targetNamespace,
				owner,
			)
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("verify existing TauWorkspace %q: %w", owner, err)
		}
	}
	ownerUID := namespace.Annotations[annotationOwnerUID]
	if ownerUID != "" && ownerUID != string(workspace.UID) && !explicitAdoption {
		return false, fmt.Errorf(
			"target namespace %q belongs to a different TauWorkspace UID %q; explicit UID-bound adoption is required",
			targetNamespace,
			ownerUID,
		)
	}
	if ownerUID == "" && workspace.UID != "" && !explicitAdoption {
		ownership := "has no TauWorkspace ownership metadata"
		if owner == workspace.Name {
			ownership = "has a workspace label without an authoritative owner UID"
		}
		return false, fmt.Errorf(
			"%w: target namespace %q %s; explicit UID-bound adoption is required",
			errNamespaceOwnershipUnverified,
			targetNamespace,
			ownership,
		)
	}
	changed := false
	requiredLabels := map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    workspace.Spec.Queue,
		labelKueueDefaultLocalQueue: workspace.Spec.Queue,
	}
	if workspace.Spec.Target.CreateNamespace {
		requiredLabels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
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
			return false, err
		}
	}
	return true, nil
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
