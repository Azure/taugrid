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

// AnnotationV0Primary marks the one workspace a v0 cluster activates.
//
// The tau-core controller is authoritative for this annotation's meaning, but
// its labelkeys package is internal/ in a separate Go module and so cannot be
// imported here. The key is therefore declared once in core/workloadmeta,
// which both sides can depend on, rather than spelled as a literal in each.
const AnnotationV0Primary = workloadmeta.AnnotationV0PrimaryWorkspace

// ErrNoWorkspaces reports a cluster that has no TauWorkspace at all, which is
// a platform-setup gap rather than a researcher mistake.
var ErrNoWorkspaces = errors.New("no TauWorkspace found")

// ErrPrimaryTerminating reports that the workspace the controller still treats
// as primary is being deleted.
//
// This is a transient state with no correct target, so it is a distinct error
// rather than a silent fallback. Any other workspace present during the
// transition is one the controller has marked AdditionalWorkspaceBlocked, and
// its namespace, RBAC, and queue bindings have been torn down — routing work
// there would fail confusingly at admission instead of here.
var ErrPrimaryTerminating = errors.New("primary TauWorkspace is terminating")

func hasPrimaryWorkspaceHistory(item Workspace) bool {
	if item.Status.Phase == "" && item.Status.ObservedGeneration == 0 && len(item.Status.Conditions) == 0 {
		return false
	}
	return !isAdditionalWorkspaceBlocked(item)
}

// isAdditionalWorkspaceBlocked reports whether the controller has refused to
// activate this workspace.
func isAdditionalWorkspaceBlocked(item Workspace) bool {
	for _, condition := range item.Status.Conditions {
		if condition.Reason == reasonAdditionalWorkspaceBlocked {
			return true
		}
	}
	return false
}

// ProvablyPrimary reports whether a workspace can be accepted as the v0 primary
// from the object alone, without comparing it against its peers.
//
// SelectPrimary's fallback — the oldest workspace that is not terminating —
// is a statement about a population, so it is only meaningful once every
// candidate is present. A caller holding a single object (a named `get`, which
// is all a researcher's RBAC permits) therefore cannot use it: any live
// workspace satisfies "oldest non-terminating" when it is the only one in the
// list, including one the controller marked AdditionalWorkspaceBlocked.
//
// A controller marker or reconciled, non-blocked status proves the object was
// selected without requiring the list permission researcher RBAC omits.
func ProvablyPrimary(item Workspace) bool {
	if isAdditionalWorkspaceBlocked(item) {
		return false
	}
	return item.Metadata.Annotations[AnnotationV0Primary] == "true" || hasPrimaryWorkspaceHistory(item)
}

// reasonAdditionalWorkspaceBlocked is the condition reason the controller sets
// on every workspace it refuses to activate.
const reasonAdditionalWorkspaceBlocked = "AdditionalWorkspaceBlocked"

// SelectPrimary resolves the single workspace a v0 cluster activates.
//
// TauGrid v0 runs exactly one workspace per cluster, so a researcher should
// never have to name it. This mirrors the precedence in the tau-core workspace
// controller's own selection, because the two disagreeing about which
// workspace is live is worse than either being wrong alone:
//
//  1. an explicit AnnotationV0Primary marker, which an operator can set to
//     override the default choice;
//  2. otherwise the oldest workspace already reconciled as primary;
//  3. otherwise the oldest workspace that is not terminating.
//
// Deletion is deliberately *not* a filter for the marker. The controller keeps a
// terminating incumbent as primary and marks any replacement
// AdditionalWorkspaceBlocked, so skipping it here would select the blocked
// replacement and route work to a namespace whose access was never activated.
// A terminating selection is reported as ErrPrimaryTerminating instead.
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

	// An operator marking two workspaces as primary is a genuine conflict. The
	// controller fails loudly here rather than guessing, and so does this: the
	// two would otherwise disagree about which workspace is live.
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
		// Every workspace is terminating, so there is no live target.
		return Workspace{}, ErrPrimaryTerminating
	}

	if strings.TrimSpace(selected.Metadata.DeletionTimestamp) != "" {
		return Workspace{}, fmt.Errorf("%w: %q is being deleted; wait for its replacement to activate",
			ErrPrimaryTerminating, selected.Metadata.Name)
	}
	return *selected, nil
}

// precedes orders workspaces the way the controller does: oldest first, with
// the name as a deterministic tie-break so two workspaces created in the same
// instant still resolve identically on every call.
func precedes(a, b Workspace) bool {
	at := strings.TrimSpace(a.Metadata.CreationTimestamp)
	bt := strings.TrimSpace(b.Metadata.CreationTimestamp)
	if at != bt {
		// RFC3339 timestamps from the API server are lexicographically
		// ordered, so no parsing is needed. An empty timestamp sorts last so a
		// partially-populated object never wins by accident.
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
