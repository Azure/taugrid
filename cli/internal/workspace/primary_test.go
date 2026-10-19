// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func ws(name, created, deleted string, primary bool) Workspace {
	w := Workspace{}
	w.Metadata.Name = name
	w.Metadata.CreationTimestamp = created
	w.Metadata.DeletionTimestamp = deleted
	if primary {
		w.Metadata.Annotations = map[string]string{AnnotationV0Primary: "true"}
	}
	return w
}

func TestSelectPrimaryReturnsTheOnlyWorkspace(t *testing.T) {
	got, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("research", "2026-01-01T00:00:00Z", "", false),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Metadata.Name != "research" {
		t.Fatalf("got %q, want research", got.Metadata.Name)
	}
}

func TestSelectPrimaryPrefersAnnotatedWorkspaceOverOlderOne(t *testing.T) {
	got, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("older", "2026-01-01T00:00:00Z", "", false),
		ws("marked", "2026-06-01T00:00:00Z", "", true),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Metadata.Name != "marked" {
		t.Fatalf("got %q, want marked (annotation must beat age)", got.Metadata.Name)
	}
}

func TestSelectPrimaryFallsBackToOldestWorkspace(t *testing.T) {
	got, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("newer", "2026-06-01T00:00:00Z", "", false),
		ws("oldest", "2026-01-01T00:00:00Z", "", false),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Metadata.Name != "oldest" {
		t.Fatalf("got %q, want oldest", got.Metadata.Name)
	}
}

func TestSelectPrimaryBreaksCreationTiesByName(t *testing.T) {
	same := "2026-01-01T00:00:00Z"
	got, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("beta", same, "", false),
		ws("alpha", same, "", false),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Metadata.Name != "alpha" {
		t.Fatalf("got %q, want alpha", got.Metadata.Name)
	}
}

func TestSelectPrimarySkipsTerminatingWorkspace(t *testing.T) {
	got, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("dying", "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z", false),
		ws("live", "2026-02-01T00:00:00Z", "", false),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Metadata.Name != "live" {
		t.Fatalf("got %q, want live (terminating workspace must be skipped)", got.Metadata.Name)
	}
}

func TestSelectPrimaryReportsTerminatingAnnotatedWorkspace(t *testing.T) {
	_, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("dying", "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z", true),
		ws("live", "2026-02-01T00:00:00Z", "", false),
	}})
	if !errors.Is(err, ErrPrimaryTerminating) {
		t.Fatalf("got %v, want ErrPrimaryTerminating", err)
	}
	if !strings.Contains(err.Error(), "dying") {
		t.Fatalf("error must name the terminating workspace, got: %v", err)
	}
}

func TestSelectPrimaryRejectsConflictingMarkers(t *testing.T) {
	_, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("one", "2026-01-01T00:00:00Z", "", true),
		ws("two", "2026-02-01T00:00:00Z", "", true),
	}})
	if err == nil {
		t.Fatal("expected an error when two workspaces claim the primary marker")
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name the conflicting workspaces, got: %v", err)
		}
	}
}

func TestSelectPrimaryReportsNoWorkspaces(t *testing.T) {
	if _, err := SelectPrimary(WorkspaceList{}); !errors.Is(err, ErrNoWorkspaces) {
		t.Fatalf("got %v, want ErrNoWorkspaces", err)
	}
}

func TestSelectPrimaryDistinguishesTeardownFromEmptyCluster(t *testing.T) {
	_, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("dying", "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z", false),
	}})
	if !errors.Is(err, ErrPrimaryTerminating) {
		t.Fatalf("got %v, want ErrPrimaryTerminating", err)
	}
	if errors.Is(err, ErrNoWorkspaces) {
		t.Fatal("a terminating workspace must not be reported as an unconfigured cluster")
	}
}

func TestSelectPrimarySortsMissingTimestampLast(t *testing.T) {
	got, err := SelectPrimary(WorkspaceList{Items: []Workspace{
		ws("undated", "", "", false),
		ws("dated", "2026-06-01T00:00:00Z", "", false),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Metadata.Name != "dated" {
		t.Fatalf("got %q, want dated", got.Metadata.Name)
	}
}

func TestParseListPreservesAnnotationsAndCreationTimestamp(t *testing.T) {
	raw := []byte(fmt.Sprintf(`{"items":[{"metadata":{
		"name":"only-workspace",
		"creationTimestamp":"2026-01-01T00:00:00Z",
		"annotations":{%q:"true"}
	}}]}`, AnnotationV0Primary))
	list, err := ParseList(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(list.Items))
	}
	meta := list.Items[0].Metadata
	if meta.CreationTimestamp != "2026-01-01T00:00:00Z" {
		t.Fatalf("creationTimestamp not decoded, got %q", meta.CreationTimestamp)
	}
	if meta.Annotations[AnnotationV0Primary] != "true" {
		t.Fatalf("annotations not decoded, got %v", meta.Annotations)
	}
	got, err := SelectPrimary(list)
	if err != nil || got.Metadata.Name != "only-workspace" {
		t.Fatalf("SelectPrimary over parsed list: got %q, err %v", got.Metadata.Name, err)
	}
}

func TestDefaultWorkspaceNameIsAValidNamespace(t *testing.T) {
	if errs := validation.IsDNS1123Label(DefaultWorkspaceName); len(errs) > 0 {
		t.Fatalf("DefaultWorkspaceName %q is not a valid namespace: %v", DefaultWorkspaceName, errs)
	}
}

func blocked(w Workspace) Workspace {
	w.Status.Phase = "Pending"
	w.Status.Conditions = []Condition{{
		Type:   "Ready",
		Status: "False",
		Reason: reasonAdditionalWorkspaceBlocked,
	}}
	return w
}

func TestProvablyPrimaryRejectsBlockedWorkspace(t *testing.T) {
	if ProvablyPrimary(blocked(ws("blocked", "2026-01-01T00:00:00Z", "", false))) {
		t.Fatal("a blocked workspace must not short-circuit the list: its RBAC and " +
			"queue bindings are torn down, so lifecycle commands would target a dead namespace")
	}
}

func TestProvablyPrimaryRejectsUnreconciledWorkspace(t *testing.T) {
	if ProvablyPrimary(ws("fresh", "2026-01-01T00:00:00Z", "", false)) {
		t.Fatal("a workspace with no status has not been selected by the controller; " +
			"accepting it would let a stale name win over the real primary")
	}
}

func TestProvablyPrimaryRejectsReconciledUnmarkedWorkspace(t *testing.T) {
	workspace := ws("active", "2026-01-01T00:00:00Z", "", false)
	workspace.Status.Phase = "Ready"
	if ProvablyPrimary(workspace) {
		t.Fatal("a named get cannot prove an unmarked workspace is primary when another workspace may carry the v0 marker")
	}
}

func TestProvablyPrimaryAcceptsMarkedWorkspace(t *testing.T) {
	if !ProvablyPrimary(ws("marked", "2026-01-01T00:00:00Z", "", true)) {
		t.Fatal("an explicit primary marker is self-evident from the object alone")
	}
}

func TestNamedGetFallbackAcceptsReadyUnmarkedWorkspace(t *testing.T) {
	workspace := ws("active", "2026-01-01T00:00:00Z", "", false)
	workspace.Status.Phase = "Ready"
	if !NamedGetFallback(workspace) {
		t.Fatal("a Ready unmarked workspace must remain usable when scoped RBAC forbids listing")
	}
}

func TestNamedGetFallbackRejectsBlockedWorkspace(t *testing.T) {
	workspace := blocked(ws("blocked", "2026-01-01T00:00:00Z", "", false))
	workspace.Status.Phase = "Ready"
	if NamedGetFallback(workspace) {
		t.Fatal("a previously blocked workspace must not become a named-get fallback until its blocking condition is cleared")
	}
}
