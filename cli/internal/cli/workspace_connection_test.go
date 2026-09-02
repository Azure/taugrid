// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
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
