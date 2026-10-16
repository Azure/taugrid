// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

const testWorkspaceConnectionDescriptor = `schema: tau.workspace.connection.v1
workspace: sample
cluster:
  contextName: aks-flex
access:
  method: kubeconfig
authorization:
  mode: cluster-wide
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
`

func TestWorkspaceConnectionHasNoOfflineFlag(t *testing.T) {
	if flag := newWorkspaceConnectionCmd().Flags().Lookup("offline"); flag != nil {
		t.Fatalf("unexpected --offline flag: %#v", flag)
	}
}

func TestWorkspaceConnectionDiscoversParentDescriptor(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	writeWorkspaceConnectionDescriptor(t, root)
	nested := filepath.Join(root, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:         "sample",
		AuthorizationMode: workspaceconnection.AuthorizationModeClusterWide,
		Namespace:         "tau-default",
		Queue:             "jobqueue",
	}}
	cmd := newWorkspaceConnectionCmdWithEnsurer(ensurer)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{nested})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if ensurer.calls != 1 || len(ensurer.discoveries) != 1 {
		t.Fatalf("connection activation calls=%d discoveries=%d", ensurer.calls, len(ensurer.discoveries))
	}
	for _, want := range []string{
		"Connected.",
		"Workspace:     sample",
		"Descriptor:    tau/workspace.connection.yaml",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("connection output missing %q:\n%s", want, out.String())
		}
	}
}

func TestWorkspaceConnectionActivatesResolvedDescriptor(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	writeWorkspaceConnectionDescriptor(t, root)
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:         "sample",
		AuthorizationMode: workspaceconnection.AuthorizationModeClusterWide,
		Namespace:         "tau-default",
		Queue:             "jobqueue",
	}}
	cmd := newWorkspaceConnectionCmdWithEnsurer(ensurer)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{root})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if ensurer.calls != 1 || len(ensurer.discoveries) != 1 {
		t.Fatalf("connection activation calls=%d discoveries=%d", ensurer.calls, len(ensurer.discoveries))
	}
	for _, want := range []string{
		"Connected.",
		"Workspace:     sample",
		"Status:        Ready",
		"Namespace:     tau-default",
		"Queue:         jobqueue",
		"Authorization: cluster-wide",
		"Ready:         tau run can now use this workspace.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("live connection output missing %q:\n%s", want, out.String())
		}
	}
}

func TestWorkspaceConnectionGuidesInteractiveReview(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	writeWorkspaceConnectionDescriptor(t, root)
	ensurer := &fakeRunConnectionEnsurer{err: workspaceconnection.ErrInteractiveRequired}
	cmd := newWorkspaceConnectionCmdWithEnsurer(ensurer)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{root})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected interactive review error")
	}
	for _, want := range []string{
		"Owner: Researcher action required",
		"Run `tau workspace connection` in an interactive terminal",
		"then retry your original command",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("guided error missing %q:\n%s", want, err)
		}
	}
}

func TestWorkspaceConnectionResolvesCatalogProject(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	projectPath := filepath.Join(root, "projects", "alpha")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "projects", "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	connectionPath := filepath.Join(root, "connections", "shared.yaml")
	if err := os.MkdirAll(filepath.Dir(connectionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connectionPath, []byte(testWorkspaceConnectionDescriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := `schema: tau.projects.v1
projects:
  alpha:
    path: projects/alpha
    connection: connections/shared.yaml
  beta:
    path: projects/beta
    connection: connections/shared.yaml
`
	if err := os.WriteFile(filepath.Join(root, "tau.projects.yaml"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:         "sample",
		AuthorizationMode: workspaceconnection.AuthorizationModeClusterWide,
		Namespace:         "tau-default",
		Queue:             "jobqueue",
	}}
	show := newWorkspaceConnectionCmdWithEnsurer(ensurer)
	var showOut bytes.Buffer
	show.SetOut(&showOut)
	show.SetErr(&bytes.Buffer{})
	show.SetArgs([]string{projectPath})
	if err := show.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Project:       alpha", "Workspace:     sample", "Descriptor:    connections/shared.yaml"} {
		if !strings.Contains(showOut.String(), want) {
			t.Fatalf("catalog connection output missing %q:\n%s", want, showOut.String())
		}
	}

	ambiguous := newWorkspaceConnectionCmd()
	ambiguous.SetOut(&bytes.Buffer{})
	ambiguous.SetErr(&bytes.Buffer{})
	ambiguous.SetArgs([]string{root})
	if err := ambiguous.Execute(); err == nil || !strings.Contains(err.Error(), "pass a path inside the intended project") {
		t.Fatalf("catalog-root connection error = %v", err)
	}
}

func TestWorkspaceConnectionAssignInspectActivateAndClear(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	originalList := listWorkspaceAssignmentCandidates
	listWorkspaceAssignmentCandidates = func(string) ([]workspaceconnection.AssignableConnection, error) {
		return []workspaceconnection.AssignableConnection{{
			ActiveConnection: workspaceconnection.ActiveConnection{
				Workspace:    "sample",
				WorkspaceUID: "sample-uid",
				ContextName:  "aks-flex",
				Namespace:    "sample",
				Queue:        "jobqueue",
			},
			Descriptor: mustWorkspaceConnectionDescriptor(t, testWorkspaceConnectionDescriptor),
		}}, nil
	}
	t.Cleanup(func() { listWorkspaceAssignmentCandidates = originalList })

	assign := newWorkspaceConnectionCmd()
	var assignOut bytes.Buffer
	assign.SetOut(&assignOut)
	assign.SetErr(&bytes.Buffer{})
	assign.SetArgs([]string{"assign", "sample", "--path", root})
	if err := assign.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assignOut.String(), "Assigned.") ||
		!strings.Contains(assignOut.String(), "Workspace:     sample") {
		t.Fatalf("assign output:\n%s", assignOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "tau", "workspace.connection.yaml")); !os.IsNotExist(err) {
		t.Fatalf("assign wrote repository descriptor: %v", err)
	}

	inspect := newWorkspaceConnectionCmd()
	var inspectOut bytes.Buffer
	inspect.SetOut(&inspectOut)
	inspect.SetErr(&bytes.Buffer{})
	inspect.SetArgs([]string{"inspect", "--path", root, "--output", "json"})
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	var inspection workspaceConnectionInspection
	if err := json.Unmarshal(inspectOut.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Source != "local assignment" ||
		inspection.Workspace != "sample" ||
		inspection.WorkspaceUID != "sample-uid" {
		t.Fatalf("inspection = %#v", inspection)
	}

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:         "sample",
		AuthorizationMode: workspaceconnection.AuthorizationModeClusterWide,
		Namespace:         "sample",
		Queue:             "jobqueue",
	}}
	connect := newWorkspaceConnectionCmdWithEnsurer(ensurer)
	connect.SetOut(&bytes.Buffer{})
	connect.SetErr(&bytes.Buffer{})
	connect.SetArgs([]string{root})
	if err := connect.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(ensurer.discoveries) != 1 ||
		ensurer.discoveries[0].Descriptor.Workspace != "sample" ||
		!strings.Contains(ensurer.discoveries[0].Path, "assignments") {
		t.Fatalf("activation discoveries = %#v", ensurer.discoveries)
	}

	clear := newWorkspaceConnectionCmd()
	clear.SetOut(&bytes.Buffer{})
	clear.SetErr(&bytes.Buffer{})
	clear.SetArgs([]string{"clear", "--path", root, "--yes"})
	if err := clear.Execute(); err != nil {
		t.Fatal(err)
	}
	inspect = newWorkspaceConnectionCmd()
	inspect.SetOut(&bytes.Buffer{})
	inspect.SetErr(&bytes.Buffer{})
	inspect.SetArgs([]string{"inspect", "--path", root})
	if err := inspect.Execute(); err == nil ||
		!strings.Contains(err.Error(), "connection assign") {
		t.Fatalf("inspect after clear error = %v", err)
	}
}

func TestWorkspaceConnectionAssignRequiresReplace(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	originalList := listWorkspaceAssignmentCandidates
	candidates := []workspaceconnection.AssignableConnection{
		{
			ActiveConnection: workspaceconnection.ActiveConnection{
				Workspace: "first", WorkspaceUID: "uid-1", ContextName: "east",
			},
			Descriptor: assignmentCommandDescriptor("first", "east"),
		},
		{
			ActiveConnection: workspaceconnection.ActiveConnection{
				Workspace: "second", WorkspaceUID: "uid-2", ContextName: "west",
			},
			Descriptor: assignmentCommandDescriptor("second", "west"),
		},
	}
	listWorkspaceAssignmentCandidates = func(string) ([]workspaceconnection.AssignableConnection, error) {
		return candidates, nil
	}
	t.Cleanup(func() { listWorkspaceAssignmentCandidates = originalList })

	run := func(args ...string) error {
		cmd := newWorkspaceConnectionCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	if err := run("assign", "first", "--path", root); err != nil {
		t.Fatal(err)
	}
	if err := run("assign", "second", "--path", root); err == nil ||
		!strings.Contains(err.Error(), "--replace") {
		t.Fatalf("replace guard error = %v", err)
	}
	if err := run("assign", "second", "--path", root, "--replace"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceConnectionAssignCurrentAndAmbiguity(t *testing.T) {
	sourceRoot := initWorkspaceConnectionRepo(t)
	writeWorkspaceConnectionDescriptor(t, sourceRoot)
	targetRoot := initWorkspaceConnectionRepo(t)
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	t.Setenv("TAU_CONTEXT", "")
	originalList := listWorkspaceAssignmentCandidates
	candidates := []workspaceconnection.AssignableConnection{
		{
			ActiveConnection: workspaceconnection.ActiveConnection{
				Workspace: "sample", WorkspaceUID: "uid-east", ContextName: "east",
			},
			Descriptor: assignmentCommandDescriptor("sample", "east"),
		},
		{
			ActiveConnection: workspaceconnection.ActiveConnection{
				Workspace: "sample", WorkspaceUID: "uid-west", ContextName: "west",
			},
			Descriptor: assignmentCommandDescriptor("sample", "west"),
		},
	}
	listWorkspaceAssignmentCandidates = func(string) ([]workspaceconnection.AssignableConnection, error) {
		return candidates, nil
	}
	t.Cleanup(func() { listWorkspaceAssignmentCandidates = originalList })

	ambiguous := newWorkspaceConnectionCmd()
	ambiguous.SetOut(&bytes.Buffer{})
	ambiguous.SetErr(&bytes.Buffer{})
	ambiguous.SetArgs([]string{"assign", "sample", "--path", targetRoot})
	if err := ambiguous.Execute(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity error = %v", err)
	}

	activatedDescriptor := strings.Replace(testWorkspaceConnectionDescriptor, "contextName: aks-flex", "contextName: west", 1)
	if err := os.WriteFile(
		filepath.Join(sourceRoot, "tau", "workspace.connection.yaml"),
		[]byte(activatedDescriptor),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	ensurer := &cachingRunConnectionEnsurer{
		fakeRunConnectionEnsurer: &fakeRunConnectionEnsurer{
			connection: workspaceconnection.ActiveConnection{
				Workspace:         "sample",
				WorkspaceUID:      "uid-west",
				AuthorizationMode: workspaceconnection.AuthorizationModeClusterWide,
				ContextName:       "west",
				SystemNamespace:   "tau-system",
				Namespace:         "sample",
				Queue:             "jobqueue",
			},
		},
		configDir: configDir,
	}
	connect := newWorkspaceConnectionCmdWithEnsurer(ensurer)
	connect.SetOut(&bytes.Buffer{})
	connect.SetErr(&bytes.Buffer{})
	connect.SetArgs([]string{sourceRoot})
	if err := connect.Execute(); err != nil {
		t.Fatal(err)
	}

	current := newWorkspaceConnectionCmd()
	current.SetOut(&bytes.Buffer{})
	current.SetErr(&bytes.Buffer{})
	current.SetArgs([]string{"assign", "--current", "--path", targetRoot})
	if err := current.Execute(); err != nil {
		t.Fatal(err)
	}
	inspect := newWorkspaceConnectionCmd()
	var out bytes.Buffer
	inspect.SetOut(&out)
	inspect.SetErr(&bytes.Buffer{})
	inspect.SetArgs([]string{"inspect", "--path", targetRoot, "--output", "json"})
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	var got workspaceConnectionInspection
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Context != "west" || got.WorkspaceUID != "uid-west" {
		t.Fatalf("current assignment = %#v", got)
	}
}

func TestWorkspaceConnectionAssignFiltersCachedSystemNamespace(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	t.Setenv("TAU_CONTEXT", "")
	originalList := listWorkspaceAssignmentCandidates
	defaultNamespace := assignmentCommandDescriptor("sample", "east")
	customNamespace := assignmentCommandDescriptor("sample", "east")
	customNamespace.Cluster.SystemNamespace = "custom-system"
	listWorkspaceAssignmentCandidates = func(string) ([]workspaceconnection.AssignableConnection, error) {
		return []workspaceconnection.AssignableConnection{
			{
				ActiveConnection: workspaceconnection.ActiveConnection{
					Workspace: "sample", WorkspaceUID: "uid-default", ContextName: "east",
				},
				Descriptor: defaultNamespace,
			},
			{
				ActiveConnection: workspaceconnection.ActiveConnection{
					Workspace: "sample", WorkspaceUID: "uid-custom", ContextName: "east",
				},
				Descriptor: customNamespace,
			},
		}, nil
	}
	t.Cleanup(func() { listWorkspaceAssignmentCandidates = originalList })

	assign := newWorkspaceConnectionCmd()
	assign.SetOut(&bytes.Buffer{})
	assign.SetErr(&bytes.Buffer{})
	assign.SetArgs([]string{
		"assign", "sample",
		"--path", root,
		"--context", "east",
		"--system-namespace", "custom-system",
	})
	if err := assign.Execute(); err != nil {
		t.Fatal(err)
	}
	inspect := newWorkspaceConnectionCmd()
	var out bytes.Buffer
	inspect.SetOut(&out)
	inspect.SetErr(&bytes.Buffer{})
	inspect.SetArgs([]string{"inspect", "--path", root, "--output", "json"})
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	var got workspaceConnectionInspection
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SystemNamespace != "custom-system" || got.WorkspaceUID != "uid-custom" {
		t.Fatalf("system namespace assignment = %#v", got)
	}
}

func TestCatalogProjectWithoutConnectionUsesLocalAssignment(t *testing.T) {
	root := initWorkspaceConnectionRepo(t)
	projectRoot := filepath.Join(root, "projects", "alpha")
	if err := os.MkdirAll(filepath.Join(projectRoot, "tau"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "tau.projects.yaml"),
		[]byte("schema: tau.projects.v1\nprojects:\n  alpha:\n    path: projects/alpha\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "tau", "train.yaml"), []byte("name: train\nengine: job\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAU_CONFIG_DIR", t.TempDir())
	originalList := listWorkspaceAssignmentCandidates
	listWorkspaceAssignmentCandidates = func(string) ([]workspaceconnection.AssignableConnection, error) {
		return []workspaceconnection.AssignableConnection{{
			ActiveConnection: workspaceconnection.ActiveConnection{
				Workspace: "sample", WorkspaceUID: "uid", ContextName: "aks-flex",
			},
			Descriptor: assignmentCommandDescriptor("sample", "aks-flex"),
		}}, nil
	}
	t.Cleanup(func() { listWorkspaceAssignmentCandidates = originalList })

	assign := newWorkspaceConnectionCmd()
	assign.SetOut(&bytes.Buffer{})
	assign.SetErr(&bytes.Buffer{})
	assign.SetArgs([]string{"assign", "sample", "--path", projectRoot})
	if err := assign.Execute(); err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveRunRequest(projectRoot, "", "", "train")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Project == nil ||
		resolution.Project.Name != "alpha" ||
		resolution.Connection.Discovery == nil ||
		resolution.Connection.Discovery.Descriptor.Workspace != "sample" {
		t.Fatalf("resolution = %#v", resolution)
	}

	checkedIn := strings.Replace(testWorkspaceConnectionDescriptor, "workspace: sample", "workspace: portable", 1)
	checkedIn = strings.Replace(checkedIn, "contextName: aks-flex", "contextName: checked-in", 1)
	if err := os.WriteFile(
		filepath.Join(projectRoot, "tau", "workspace.connection.yaml"),
		[]byte(checkedIn),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	resolution, err = resolveRunRequest(projectRoot, "", "", "train")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Connection.Discovery == nil ||
		resolution.Connection.Discovery.Descriptor.Workspace != "portable" {
		t.Fatalf("checked-in connection did not replace local assignment: %#v", resolution.Connection)
	}

	inspect := newWorkspaceConnectionCmd()
	var out bytes.Buffer
	inspect.SetOut(&out)
	inspect.SetErr(&bytes.Buffer{})
	inspect.SetArgs([]string{"inspect", "--path", projectRoot, "--output", "json"})
	if err := inspect.Execute(); err != nil {
		t.Fatal(err)
	}
	var inspection workspaceConnectionInspection
	if err := json.Unmarshal(out.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Source != "checked-in" ||
		inspection.Workspace != "portable" ||
		!inspection.LocalShadowed {
		t.Fatalf("checked-in inspection = %#v", inspection)
	}
}

func mustWorkspaceConnectionDescriptor(t *testing.T, raw string) workspaceconnection.Descriptor {
	t.Helper()
	descriptor, err := workspaceconnection.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func assignmentCommandDescriptor(workspace, contextName string) workspaceconnection.Descriptor {
	return workspaceconnection.Descriptor{
		Schema:    workspaceconnection.DescriptorSchema,
		Workspace: workspace,
		Cluster: workspaceconnection.ClusterDescriptor{
			ContextName:     contextName,
			SystemNamespace: "tau-system",
		},
		Access: workspaceconnection.AccessDescriptor{Method: workspaceconnection.AccessMethodKubeconfig},
		Authorization: workspaceconnection.AuthorizationDescriptor{
			Mode: workspaceconnection.AuthorizationModeClusterWide,
		},
		Requirements: workspaceconnection.RequirementsDescriptor{MinTauVersion: "0.3.0"},
		Network:      workspaceconnection.NetworkDescriptor{PrivateCluster: false},
	}
}

func initWorkspaceConnectionRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	return root
}

func writeWorkspaceConnectionDescriptor(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "tau", "workspace.connection.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testWorkspaceConnectionDescriptor), 0o644); err != nil {
		t.Fatal(err)
	}
}
