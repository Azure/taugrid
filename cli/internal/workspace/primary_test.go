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

// The common v0 case: one workspace on the cluster, so the researcher never
// needs to name it.
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

// An operator can override the default choice with the same annotation the
// controller honours, so the two cannot disagree about which workspace is live.
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

// Without an explicit marker the controller keeps the oldest workspace and
// blocks the rest, so the CLI must resolve to that same one.
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

// Identical creation timestamps must still resolve deterministically, or two
// invocations could target different namespaces.
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

// A workspace being deleted still appears in the API while its finalizer runs.
// Selecting it would send work to a namespace that is going away.
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

// An operator-marked workspace stays primary while it terminates, because the
// controller's own marker check has no deletion filter. Silently promoting the
// other workspace would contradict the controller, which is at that moment
// blocking it.
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

// Two claimed primaries is an operator error the CLI must not paper over by
// guessing, because the controller refuses to guess either.
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

// An empty cluster is reported with a sentinel so callers can distinguish a
// platform-setup gap from a genuine lookup failure.
func TestSelectPrimaryReportsNoWorkspaces(t *testing.T) {
	if _, err := SelectPrimary(WorkspaceList{}); !errors.Is(err, ErrNoWorkspaces) {
		t.Fatalf("got %v, want ErrNoWorkspaces", err)
	}
}

// A cluster mid-teardown is not the same as a cluster that was never set up,
// and the two want different advice: wait, versus create a workspace.
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

// The API server always stamps creationTimestamp, but a hand-written or
// partially-populated object must not win the ordering by accident.
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

// The discovery path decodes real kubectl list output, so the metadata fields
// the precedence rules depend on must actually survive parsing.
func TestParseListPreservesAnnotationsAndCreationTimestamp(t *testing.T) {
	// Built from the constant rather than spelled literally so the fixture
	// cannot drift away from the key the discovery path actually reads.
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

// The default workspace name becomes a Kubernetes Namespace name, so it has to
// be a valid DNS-1123 label. A default that cannot be created would turn a
// zero-argument `tau workspace create` into a guaranteed failure.
func TestDefaultWorkspaceNameIsAValidNamespace(t *testing.T) {
	if errs := validation.IsDNS1123Label(DefaultWorkspaceName); len(errs) > 0 {
		t.Fatalf("DefaultWorkspaceName %q is not a valid namespace: %v", DefaultWorkspaceName, errs)
	}
}

// blocked marks a workspace the way the controller marks every workspace it
// refuses to activate.
func blocked(w Workspace) Workspace {
	w.Status.Phase = "Pending"
	w.Status.Conditions = []Condition{{
		Type:   "Ready",
		Status: "False",
		Reason: reasonAdditionalWorkspaceBlocked,
	}}
	return w
}

// A named `get` is all a researcher's RBAC permits, but it proves only that the
// object exists. SelectPrimary's fallback -- oldest non-terminating -- is a
// claim about a population, so a one-item list satisfies it vacuously. These
// pin which objects may short-circuit the list and which must not.
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

func TestProvablyPrimaryAcceptsMarkedWorkspace(t *testing.T) {
	if !ProvablyPrimary(ws("marked", "2026-01-01T00:00:00Z", "", true)) {
		t.Fatal("an explicit primary marker is self-evident from the object alone")
	}
}
