// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/workloadmeta"
)

func fakeKubectl(t *testing.T, body string) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl uses a POSIX shell script")
	}
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
	first := strings.SplitN(strings.TrimSpace(string(calls)), "\n", 2)[0]
	if !strings.Contains(first, "research") {
		t.Fatalf("the descriptor name must be tried first, got: %q", first)
	}
	if strings.Contains(string(calls), "-o json\n") && strings.Contains(first, "taugrid-default") {
		t.Fatalf("must not guess the default name ahead of the descriptor, calls:\n%s", calls)
	}
}

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
