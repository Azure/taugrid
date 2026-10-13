// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import "github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"

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

func reasonFor(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
