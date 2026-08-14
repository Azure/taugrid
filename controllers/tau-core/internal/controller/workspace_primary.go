// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TauWorkspaceReconciler) resolvePrimaryWorkspace(ctx context.Context) (string, error) {
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := r.APIReader.List(ctx, &workspaces, client.InNamespace(platformNamespace(r.PlatformNamespace))); err != nil {
		return "", err
	}
	var primary *tauv1alpha1.TauWorkspace
	var marked *tauv1alpha1.TauWorkspace
	for i := range workspaces.Items {
		candidate := &workspaces.Items[i]
		if candidate.Annotations[annotationV0Primary] == "true" {
			if marked != nil {
				return "", fmt.Errorf(
					"multiple TauWorkspaces claim the v0 primary marker: %q and %q",
					marked.Name,
					candidate.Name,
				)
			}
			marked = candidate
		}
		if !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		if primary == nil || workspacePrecedes(candidate, primary) {
			primary = candidate
		}
	}
	if marked != nil {
		return marked.Name, nil
	}
	if primary == nil {
		return "", fmt.Errorf("no non-terminating TauWorkspace exists")
	}
	return primary.Name, nil
}

func workspacePrecedes(a, b *tauv1alpha1.TauWorkspace) bool {
	return a.CreationTimestamp.Before(&b.CreationTimestamp) ||
		(a.CreationTimestamp.Equal(&b.CreationTimestamp) && a.Name < b.Name)
}
