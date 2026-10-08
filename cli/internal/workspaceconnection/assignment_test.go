// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAssignmentRoundTripAndIsolation(t *testing.T) {
	repositoryRoot := t.TempDir()
	projectRoot := filepath.Join(repositoryRoot, "projects", "vision")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	scope := AssignmentScope{
		RepositoryRoot:     repositoryRoot,
		RealRepositoryRoot: repositoryRoot,
		Project:            "vision",
		ProjectRoot:        projectRoot,
		RealProjectRoot:    projectRoot,
	}
	configDir := t.TempDir()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	descriptor := assignmentTestDescriptor("vision", "aks-west")

	assignment, path, err := SaveAssignment(
		configDir,
		scope,
		descriptor,
		"workspace-uid",
		"cached",
		false,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Descriptor.Workspace != "vision" || assignment.AssignedAt != now {
		t.Fatalf("assignment = %#v", assignment)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("assignment mode = %o, want 600", info.Mode().Perm())
	}
	discovery, loaded, loadedPath, err := AssignmentDiscovery(configDir, scope)
	if err != nil {
		t.Fatal(err)
	}
	if loadedPath != path ||
		loaded.WorkspaceUID != "workspace-uid" ||
		discovery.Descriptor.Workspace != "vision" ||
		discovery.Path != path ||
		discovery.RepositoryRoot != repositoryRoot {
		t.Fatalf("discovery=%#v assignment=%#v path=%s", discovery, loaded, loadedPath)
	}

	otherScope := scope
	otherScope.Project = "language"
	if _, _, err := LoadAssignment(configDir, otherScope); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("other project load error = %v", err)
	}
}

func TestAssignmentReplacementAndClear(t *testing.T) {
	root := t.TempDir()
	scope := AssignmentScope{
		RepositoryRoot:     root,
		RealRepositoryRoot: root,
		ProjectRoot:        root,
		RealProjectRoot:    root,
	}
	configDir := t.TempDir()
	now := time.Now()
	if _, _, err := SaveAssignment(
		configDir, scope, assignmentTestDescriptor("first", "east"), "uid-1", "cached", false, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SaveAssignment(
		configDir, scope, assignmentTestDescriptor("second", "west"), "uid-2", "cached", false, now,
	); err == nil {
		t.Fatal("expected replacement guard")
	}
	if _, _, err := SaveAssignment(
		configDir, scope, assignmentTestDescriptor("second", "west"), "uid-2", "cached", true, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAssignment(configDir, scope); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadAssignment(configDir, scope); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("load after clear error = %v", err)
	}
}

func TestAssignmentRejectsProjectOutsideRepository(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	scope := AssignmentScope{
		RepositoryRoot:     root,
		RealRepositoryRoot: root,
		ProjectRoot:        other,
		RealProjectRoot:    other,
	}
	if _, err := AssignmentPath(t.TempDir(), scope); err == nil {
		t.Fatal("expected containment error")
	}
}

func assignmentTestDescriptor(workspace, contextName string) Descriptor {
	return Descriptor{
		Schema:    DescriptorSchema,
		Workspace: workspace,
		Cluster: ClusterDescriptor{
			ContextName:     contextName,
			SystemNamespace: "tau-system",
		},
		Access: AccessDescriptor{Method: AccessMethodKubeconfig},
		Authorization: AuthorizationDescriptor{
			Mode: AuthorizationModeClusterWide,
		},
		Requirements: RequirementsDescriptor{MinTauVersion: "0.3.0"},
		Network:      NetworkDescriptor{PrivateCluster: false},
	}
}
