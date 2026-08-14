// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func resolvedNamespace(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Target.Namespace != "" {
		return workspace.Spec.Target.Namespace
	}
	return workspace.Name
}

func (r *TauWorkspaceReconciler) reconcileNamespace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, error) {
	if reason := reservedNamespaceReason(targetNamespace, platformNamespace(r.PlatformNamespace)); reason != "" {
		return false, fmt.Errorf("refusing to manage namespace %q: %s", targetNamespace, reason)
	}
	var namespace corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: targetNamespace}, &namespace)
	if apierrors.IsNotFound(err) && workspace.Spec.Target.CreateNamespace {
		namespace = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        targetNamespace,
				Labels:      workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue),
				Annotations: workspaceNamespaceAnnotations(workspace.Spec.Defaults.OutputRoot),
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
	if owner := namespace.Labels[labelWorkspace]; owner != "" && owner != workspace.Name {
		orphaned, err := r.ownerWorkspaceAbsent(ctx, owner)
		if err != nil {
			return false, err
		}
		if !orphaned {
			return false, fmt.Errorf("target namespace %q is already assigned to TauWorkspace %q", targetNamespace, owner)
		}
		log.FromContext(ctx).Info("reclaiming namespace from deleted workspace",
			"namespace", targetNamespace, "previousOwner", owner, "workspace", workspace.Name)
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

// reservedNamespaceReason names why a namespace may never be adopted as a
// workspace target, or "" when the namespace is fair game. Adoption is not
// read-only: reconcileNamespace stamps ownership labels onto whatever it
// resolves, and a later retarget hands the previous namespace to
// cleanupStaleNamespaceMetadata, which strips them again. Pointing a workspace
// at the platform or a kube-system namespace would therefore overwrite the
// Helm ownership metadata and drop the Kueue default-queue label of a
// namespace TauGrid does not own.
func reservedNamespaceReason(name, platformNamespace string) string {
	switch {
	case name == platformNamespace, name == tauv1alpha1.PlatformNamespace:
		return "the Tau platform namespace is owned by the platform installer"
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

func workspaceNamespaceAnnotations(resultScope string) map[string]string {
	if strings.TrimSpace(resultScope) == "" {
		return nil
	}
	return map[string]string{annotationResultScope: resultScope}
}
