// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/workloadmeta"
)

// fakeKubectl installs a kubectl on PATH that records every invocation's
// argv to a log file and replies with the given script body.
func fakeKubectl(t *testing.T, body string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n" + body
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func discoverCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return cmd
}

// The researcher Role the tau-core controller creates grants `get` on the one
// workspace by ResourceName and nothing else; the tau-core Kind suite asserts
// `can-i list workspaces` is denied on purpose. Discovery must therefore work
// with list permanently Forbidden, or the v0 default-workspace feature fails
// for exactly the persona it exists to serve.
func TestDiscoverPrimaryWorkspaceWorksWithoutListPermission(t *testing.T) {
	logPath := fakeKubectl(t, `
case "$*" in
  *"get workspaces.tau.azure.com taugrid-default"*)
    cat <<'JSON'
{"apiVersion":"tau.azure.com/v1alpha1","kind":"TauWorkspace",
 "metadata":{"name":"taugrid-default","creationTimestamp":"2026-01-01T00:00:00Z",
  "annotations":{"`+workloadmeta.AnnotationV0PrimaryWorkspace+`":"true"}},
 "status":{"phase":"Ready"}}
JSON
    exit 0;;
  *)
    echo 'Error from server (Forbidden): workspaces.tau.azure.com is forbidden' >&2
    exit 1;;
esac
`)

	got, err := discoverPrimaryWorkspace(discoverCmd(), "")
	if err != nil {
		t.Fatalf("discovery must succeed with only named-get permission, got: %v", err)
	}
	if got.Metadata.Name != "taugrid-default" {
		t.Fatalf("got %q, want taugrid-default", got.Metadata.Name)
	}

	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(strings.TrimSpace(string(calls)), "\n", 2)[0]
	if !strings.Contains(first, "taugrid-default") {
		t.Fatalf("the first lookup must be a named get, got: %q", first)
	}
}

// writeConnectionDescriptor creates a repo containing a workspace connection
// descriptor naming the given workspace, and makes it the working directory.
func writeConnectionDescriptor(t *testing.T, workspace string) {
	t.Helper()
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	path := filepath.Join(root, "tau", "workspace.connection.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "schema: tau.workspace.connection.v1\n" +
		"workspace: " + workspace + "\n" +
		"cluster:\n" +
		"  contextName: taugrid\n" +
		"access:\n" +
		"  method: kubeconfig\n" +
		"authorization:\n" +
		"  mode: cluster-wide\n" +
		"requirements:\n" +
		"  minTauVersion: 0.3.0\n" +
		"network:\n" +
		"  privateCluster: false\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
}

// An operator may rename the one workspace off the default. That researcher's
// Role is keyed to the custom name, so both the default-name get and the list
// are Forbidden: guessing the name cannot work, and the name has to come from
// somewhere. It comes from the connection descriptor, which already carries a
// required `workspace` field for exactly this purpose.
func TestDiscoverPrimaryWorkspaceUsesDescriptorNameWhenOnlyThatGetIsPermitted(t *testing.T) {
	writeConnectionDescriptor(t, "research")
	logPath := fakeKubectl(t, `
case "$*" in
  *"get workspaces.tau.azure.com research"*)
    cat <<'JSON'
{"apiVersion":"tau.azure.com/v1alpha1","kind":"TauWorkspace",
 "metadata":{"name":"research","creationTimestamp":"2026-01-01T00:00:00Z",
  "annotations":{"`+workloadmeta.AnnotationV0PrimaryWorkspace+`":"true"}},
 "status":{"phase":"Ready"}}
JSON
    exit 0;;
  *)
    echo 'Error from server (Forbidden): workspaces.tau.azure.com is forbidden' >&2
    exit 1;;
esac
`)

	got, err := discoverPrimaryWorkspace(discoverCmd(), "")
	if err != nil {
		t.Fatalf("a custom-named workspace must be discoverable with only its own named get, got: %v", err)
	}
	if got.Metadata.Name != "research" {
		t.Fatalf("got %q, want research", got.Metadata.Name)
	}

	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// The descriptor names the workspace outright, so spending a round trip on
	// a default-name get that is guaranteed to be forbidden is pure waste.
	first := strings.SplitN(strings.TrimSpace(string(calls)), "\n", 2)[0]
	if !strings.Contains(first, "research") {
		t.Fatalf("the descriptor name must be tried first, got: %q", first)
	}
	if strings.Contains(string(calls), "-o json\n") && strings.Contains(first, "taugrid-default") {
		t.Fatalf("must not guess the default name ahead of the descriptor, calls:\n%s", calls)
	}
}

// The reviewer's scenario on #1282: an operator runs a custom-named primary,
// and a later `taugrid-default` lingers as a blocked CR. A named get finds the
// blocked object first, so discovery must not stop there -- it has to reach the
// list, where the real primary is visible.
func TestDiscoverPrimaryWorkspaceDoesNotSelectBlockedNamedWorkspace(t *testing.T) {
	fakeKubectl(t, `
case "$*" in
  *"get workspaces.tau.azure.com taugrid-default"*)
    cat <<'JSON'
{"apiVersion":"tau.azure.com/v1alpha1","kind":"TauWorkspace",
 "metadata":{"name":"taugrid-default","creationTimestamp":"2026-01-01T00:00:00Z"},
 "status":{"phase":"Blocked","conditions":[
   {"type":"RBACReady","status":"False","reason":"AdditionalWorkspaceBlocked"}]}}
JSON
    exit 0;;
  *"get workspaces.tau.azure.com -o json"*)
    cat <<'JSON'
{"apiVersion":"v1","kind":"List","items":[
 {"metadata":{"name":"taugrid-default","creationTimestamp":"2026-01-01T00:00:00Z"},
  "status":{"phase":"Blocked","conditions":[
    {"type":"RBACReady","status":"False","reason":"AdditionalWorkspaceBlocked"}]}},
 {"metadata":{"name":"research-primary","creationTimestamp":"2026-02-01T00:00:00Z",
   "annotations":{"`+workloadmeta.AnnotationV0PrimaryWorkspace+`":"true"}},
  "status":{"phase":"Ready"}}]}
JSON
    exit 0;;
  *)
    exit 1;;
esac
`)

	got, err := discoverPrimaryWorkspace(discoverCmd(), "")
	if err != nil {
		t.Fatalf("discovery must fall through to the list, got: %v", err)
	}
	if got.Metadata.Name != "research-primary" {
		t.Fatalf("got %q, want research-primary; a blocked named workspace won the "+
			"fast path and lifecycle commands would target its torn-down namespace",
			got.Metadata.Name)
	}
}
