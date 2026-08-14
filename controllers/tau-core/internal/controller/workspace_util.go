// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

func workspaceLabels(workspace string) map[string]string {
	return map[string]string{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspace,
	}
}

func ownedByWorkspace(labels map[string]string, workspace string) bool {
	return labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] == workspace
}

func reasonFor(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
