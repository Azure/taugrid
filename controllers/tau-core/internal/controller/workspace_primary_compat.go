// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	conditionV0PrimaryDiscovery      = "V0PrimaryDiscovery"
	reasonAdditionalWorkspaceBlocked = "AdditionalWorkspaceBlocked"
)

func (r *TauWorkspaceReconciler) ensureV0PrimaryDiscoveryCompatibility(
	ctx context.Context,
	workspace *tauv1alpha1.TauWorkspace,
) (bool, error) {
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := r.apiReader().List(ctx, &workspaces, client.InNamespace(systemNamespace(r.SystemNamespace))); err != nil {
		return false, err
	}
	primaryName := ""
	for i := range workspaces.Items {
		candidate := &workspaces.Items[i]
		if candidate.Name != workspace.Name &&
			candidate.Annotations[annotationV0Primary] == "true" {
			primaryName = candidate.Name
			break
		}
	}

	existingIndex := -1
	for i := range workspace.Status.Conditions {
		if workspace.Status.Conditions[i].Type == conditionV0PrimaryDiscovery {
			existingIndex = i
			break
		}
	}
	if primaryName == "" {
		if existingIndex < 0 {
			return false, nil
		}
		workspace.Status.Conditions = append(
			workspace.Status.Conditions[:existingIndex],
			workspace.Status.Conditions[existingIndex+1:]...,
		)
		if err := r.Status().Update(ctx, workspace); err != nil {
			return false, err
		}
		return true, nil
	}

	desired := condition(
		conditionV0PrimaryDiscovery,
		metav1.ConditionFalse,
		reasonAdditionalWorkspaceBlocked,
		"excluded from v0 primary discovery while TauWorkspace "+primaryName+" remains primary",
		workspace.Generation,
	)
	if existingIndex >= 0 {
		existing := workspace.Status.Conditions[existingIndex]
		if existing.Status == desired.Status &&
			existing.Reason == desired.Reason &&
			existing.Message == desired.Message &&
			existing.ObservedGeneration == desired.ObservedGeneration {
			return false, nil
		}
		if existing.Status == desired.Status &&
			existing.Reason == desired.Reason &&
			existing.Message == desired.Message {
			desired.LastTransitionTime = existing.LastTransitionTime
		}
		workspace.Status.Conditions[existingIndex] = desired
	} else {
		workspace.Status.Conditions = append(workspace.Status.Conditions, desired)
	}
	if err := r.Status().Update(ctx, workspace); err != nil {
		return false, err
	}
	return true, nil
}

func preserveV0PrimaryDiscoveryCondition(workspace *tauv1alpha1.TauWorkspace, conditions []metav1.Condition) []metav1.Condition {
	for _, existing := range workspace.Status.Conditions {
		if existing.Type == conditionV0PrimaryDiscovery {
			return append(conditions, existing)
		}
	}
	return conditions
}
