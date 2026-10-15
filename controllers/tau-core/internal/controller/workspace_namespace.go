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
	if ownerUID == "" &&
		workspace.UID != "" &&
		!explicitAdoption {
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
			return false, err
		}
	}
	return true, nil
}

func (r *TauWorkspaceReconciler) namespaceLocalQueueName(
	_ context.Context,
	workspace *tauv1alpha1.TauWorkspace,
	_ string,
) (string, error) {
	return workspaceLocalQueueName(workspace), nil
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
