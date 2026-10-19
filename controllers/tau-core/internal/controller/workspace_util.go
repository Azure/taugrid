// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func workspaceLabels(workspace string) map[string]string {
	return map[string]string{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspace,
	}
}

func teamLabels(team string) map[string]string {
	return map[string]string{
		labelManagedBy:      labelManagedByValue,
		labelkeys.LabelTeam: team,
	}
}

func ownedByWorkspace(labels map[string]string, workspace string) bool {
	return labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] == workspace
}

func setOwnerUIDAnnotation(object metav1.Object, uid types.UID) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[annotationOwnerUID] = string(uid)
	object.SetAnnotations(annotations)
}

func reasonFor(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
