// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
)

const AnnotationV0Primary = workloadmeta.AnnotationV0PrimaryWorkspace

var ErrNoWorkspaces = errors.New("no TauWorkspace found")

var ErrPrimaryTerminating = errors.New("primary TauWorkspace is terminating")

func hasPrimaryWorkspaceHistory(item Workspace) bool {
	if item.Status.Phase == "" && item.Status.ObservedGeneration == 0 && len(item.Status.Conditions) == 0 {
		return false
	}
	return !isAdditionalWorkspaceBlocked(item)
}

func isAdditionalWorkspaceBlocked(item Workspace) bool {
	for _, condition := range item.Status.Conditions {
		if condition.Reason == reasonAdditionalWorkspaceBlocked {
			return true
		}
	}
	return false
}

func ProvablyPrimary(item Workspace) bool {
	if isAdditionalWorkspaceBlocked(item) {
		return false
	}
	return item.Metadata.Annotations[AnnotationV0Primary] == "true"
}

const reasonAdditionalWorkspaceBlocked = "AdditionalWorkspaceBlocked"

func SelectPrimary(list WorkspaceList) (Workspace, error) {
	var marked []Workspace
	var incumbent *Workspace
	var fallback *Workspace

	for i := range list.Items {
		item := list.Items[i]
		if item.Metadata.Annotations[AnnotationV0Primary] == "true" {
			marked = append(marked, item)
		}
		if hasPrimaryWorkspaceHistory(item) && (incumbent == nil || precedes(item, *incumbent)) {
			incumbent = &list.Items[i]
		}
		if strings.TrimSpace(item.Metadata.DeletionTimestamp) != "" {
			continue
		}
		if fallback == nil || precedes(item, *fallback) {
			fallback = &list.Items[i]
		}
	}

	if len(marked) > 1 {
		return Workspace{}, fmt.Errorf(
			"multiple TauWorkspaces claim the v0 primary marker (%s); remove the annotation from all but one",
			strings.Join(names(marked), ", "),
		)
	}

	var selected *Workspace
	switch {
	case len(marked) == 1:
		selected = &marked[0]
	case incumbent != nil:
		selected = incumbent
	case fallback != nil:
		selected = fallback
	default:
		if len(list.Items) == 0 {
			return Workspace{}, ErrNoWorkspaces
		}
		return Workspace{}, ErrPrimaryTerminating
	}

	if strings.TrimSpace(selected.Metadata.DeletionTimestamp) != "" {
		return Workspace{}, fmt.Errorf("%w: %q is being deleted; wait for its replacement to activate",
			ErrPrimaryTerminating, selected.Metadata.Name)
	}
	return *selected, nil
}

func precedes(a, b Workspace) bool {
	at := strings.TrimSpace(a.Metadata.CreationTimestamp)
	bt := strings.TrimSpace(b.Metadata.CreationTimestamp)
	if at != bt {
		if at == "" {
			return false
		}
		if bt == "" {
			return true
		}
		return at < bt
	}
	return a.Metadata.Name < b.Metadata.Name
}

func names(items []Workspace) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Metadata.Name)
	}
	sort.Strings(out)
	return out
}
