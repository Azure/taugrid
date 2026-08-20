// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/projectcatalog"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

const runRoutingDescriptor = `schema: tau.workspace.connection.v1
workspace: sample
cluster:
  provider: azure
  resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-ai/providers/Microsoft.ContainerService/managedClusters/taugrid-flex
  contextName: taugrid-flex
identity:
  tenantID: 11111111-1111-1111-1111-111111111111
authorization:
  mode: workspace-rbac
  requiredRole: tau-researcher-v1
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
`

func TestResolveRunRequestCatalogSelection(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	writeRunRoutingFile(t, filepath.Join(root, "beta", "tau", "eval.yaml"), "name: beta-eval\n")
	explicit := filepath.Join(root, "alpha", "experiments", "ablation", "tau.yaml")
	writeRunRoutingFile(t, explicit, "name: alpha-ablation\n")

	t.Run("unique target from root", func(t *testing.T) {
		resolution, err := resolveRunRequest(root, "", "", "eval", false)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Project == nil || resolution.Project.Name != "beta" {
			t.Fatalf("project = %#v", resolution.Project)
		}
		if resolution.Input.ConfigPath != filepath.Join(root, "beta", "tau", "eval.yaml") {
			t.Fatalf("config = %q", resolution.Input.ConfigPath)
		}
	})
	t.Run("explicit config derives unrelated repository", func(t *testing.T) {
		resolution, err := resolveRunRequest(t.TempDir(), "", explicit, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Project == nil || resolution.Project.Name != "alpha" {
			t.Fatalf("project = %#v", resolution.Project)
		}
	})
	t.Run("context cannot resolve duplicate target", func(t *testing.T) {
		_, err := resolveRunRequest(root, "", "", "train", false)
		if err == nil || !strings.Contains(err.Error(), "alpha, beta") {
			t.Fatalf("expected duplicate target ambiguity, got %v", err)
		}
	})
	t.Run("projectless smoke workspace bypass", func(t *testing.T) {
		resolution, err := resolveRunRequest(root, "", "", "smoke", true)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Project != nil || !resolution.Input.BuiltinSmoke {
			t.Fatalf("resolution = %#v", resolution)
		}
	})
	t.Run("smoke without workspace is ambiguous", func(t *testing.T) {
		_, err := resolveRunRequest(root, "", "", "smoke", false)
		if err == nil || !strings.Contains(err.Error(), "--project") {
			t.Fatalf("expected smoke ambiguity, got %v", err)
		}
	})
}

func TestResolveRunRequestConfigSymlinkOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := multiProjectRunRoutingRepo(t)
	actual := filepath.Join(root, "alpha", "experiments", "actual", "tau.yaml")
	writeRunRoutingFile(t, actual, "name: symlink-config\n")

	t.Run("external link to catalog config is rejected", func(t *testing.T) {
		external := filepath.Join(t.TempDir(), "tau.yaml")
		if err := os.Symlink(actual, external); err != nil {
			t.Fatal(err)
		}
		_, err := resolveRunRequest(filepath.Dir(external), "", external, "", false)
		if err == nil || !strings.Contains(err.Error(), "not owned by any Tau project") {
			t.Fatalf("expected external symlink ownership failure, got %v", err)
		}
	})

	t.Run("internal link preserves lexical config path", func(t *testing.T) {
		link := filepath.Join(root, "alpha", "experiments", "linked", "tau.yaml")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(actual, link); err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(root, link)
		if err != nil {
			t.Fatal(err)
		}
		resolution, err := resolveRunRequest(root, "", relative, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if resolution.Project == nil || resolution.Project.Name != "alpha" {
			t.Fatalf("project = %#v", resolution.Project)
		}
		if resolution.Input.ConfigPath != relative {
			t.Fatalf("config path = %q, want lexical spelling %q", resolution.Input.ConfigPath, relative)
		}
	})

	t.Run("internal link escaping project is rejected", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "tau.yaml")
		writeRunRoutingFile(t, outside, "name: outside\n")
		link := filepath.Join(root, "alpha", "experiments", "escape", "tau.yaml")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		_, err := resolveRunRequest(root, "", link, "", false)
		if err == nil || !strings.Contains(err.Error(), "not owned by any Tau project") {
			t.Fatalf("expected escaping symlink ownership failure, got %v", err)
		}
	})

	t.Run("internal directory link escaping project is rejected", func(t *testing.T) {
		outsideDir := t.TempDir()
		writeRunRoutingFile(t, filepath.Join(outsideDir, "tau.yaml"), "name: outside-directory\n")
		linkDir := filepath.Join(root, "alpha", "experiments", "directory-escape")
		if err := os.Symlink(outsideDir, linkDir); err != nil {
			t.Fatal(err)
		}
		_, err := resolveRunRequest(root, "", filepath.Join(linkDir, "tau.yaml"), "", false)
		if err == nil || !strings.Contains(err.Error(), "not owned by any Tau project") {
			t.Fatalf("expected directory symlink ownership failure, got %v", err)
		}
	})

	t.Run("link into nested Git repository is rejected", func(t *testing.T) {
		nested := filepath.Join(root, "alpha", "nested-repo")
		initRunRoutingRepo(t, nested)
		nestedConfig := filepath.Join(nested, "tau.yaml")
		writeRunRoutingFile(t, nestedConfig, "name: nested\n")
		link := filepath.Join(root, "alpha", "experiments", "nested-link.yaml")
		if err := os.Symlink(nestedConfig, link); err != nil {
			t.Fatal(err)
		}
		_, err := resolveRunRequest(root, "", link, "", false)
		if err == nil || !strings.Contains(err.Error(), "crosses Git worktrees") {
			t.Fatalf("expected nested Git boundary failure, got %v", err)
		}
	})

	t.Run("directory link into nested Git repository is rejected", func(t *testing.T) {
		nested := t.TempDir()
		initRunRoutingRepo(t, nested)
		writeRunRoutingFile(t, filepath.Join(nested, "tau.yaml"), "name: nested-directory\n")
		linkDir := filepath.Join(root, "alpha", "experiments", "nested-directory")
		if err := os.Symlink(nested, linkDir); err != nil {
			t.Fatal(err)
		}
		_, err := resolveRunRequest(root, "", filepath.Join(linkDir, "tau.yaml"), "", false)
		if err == nil || !strings.Contains(err.Error(), "crosses Git worktrees") {
			t.Fatalf("expected nested Git directory boundary failure, got %v", err)
		}
	})
}

func TestResolveRunRequestRejectsConfigInUninitializedGitlink(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	source := t.TempDir()
	initRunRoutingRepo(t, source)
	runRunRoutingGit(t, source, "config", "user.email", "tau-test@example.com")
	runRunRoutingGit(t, source, "config", "user.name", "Tau Test")
	writeRunRoutingFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runRunRoutingGit(t, source, "add", "README.md")
	runRunRoutingGit(t, source, "commit", "-m", "submodule")
	runRunRoutingGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", source, "alpha/vendor/model")
	if err := os.RemoveAll(filepath.Join(root, "alpha", "vendor", "model")); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "alpha", "vendor", "model", "tau.yaml")
	writeRunRoutingFile(t, config, "name: gitlink-config\n")
	_, err := resolveRunRequest(root, "", config, "", false)
	if err == nil || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected explicit config gitlink failure, got %v", err)
	}
}

func TestCatalogNamedTargetUsesExactSharedConnection(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	configureRunRoutingProfile(t)
	writeRunRoutingFile(t, filepath.Join(root, "beta", "train.sh"), "#!/bin/sh\necho train\n")
	writeRunRoutingFile(t, filepath.Join(root, "beta", "tau", "eval.yaml"), `name: beta-eval
engine: job
entrypoint: ../train.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  queue: jobqueue
  namespace: catalog-namespace
`)
	withRunRoutingCWD(t, root)

	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "catalog-context",
		Namespace:   "catalog-namespace",
	}}
	cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer {
		return ensurer
	})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"eval", "--dry-run=client"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("catalog target command: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	if ensurer.calls != 1 || len(ensurer.discoveries) != 1 {
		t.Fatalf("connection calls=%d discoveries=%d", ensurer.calls, len(ensurer.discoveries))
	}
	wantConnection, err := filepath.EvalSymlinks(filepath.Join(root, "connections", "shared.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if ensurer.discoveries[0].Path != wantConnection {
		t.Fatalf("exact descriptor = %q, want %q", ensurer.discoveries[0].Path, wantConnection)
	}
	for _, want := range []string{"kind: Job", "name: beta-eval", "namespace: catalog-namespace"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("render missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestCatalogRunRejectsConfigWorkspaceConflictBeforeConnection(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	configureRunRoutingProfile(t)
	writeRunRoutingFile(t, filepath.Join(root, "beta", "train.sh"), "#!/bin/sh\necho train\n")
	writeRunRoutingFile(t, filepath.Join(root, "beta", "tau", "eval.yaml"), `name: beta-eval
engine: job
entrypoint: ../train.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  workspace: other
`)
	withRunRoutingCWD(t, root)
	ensurer := &fakeRunConnectionEnsurer{}
	cmd := newRunCmdWithConnectionFactory(func(*cobra.Command) runConnectionEnsurer {
		return ensurer
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"eval", "--dry-run=client"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `policy.workspace "other"`) {
		t.Fatalf("expected workspace conflict, got %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("workspace conflict activated connection %d times", ensurer.calls)
	}
}

func TestNoCatalogNamedTargetPreservesRenderedWorkloadSpec(t *testing.T) {
	root := t.TempDir()
	configureRunRoutingProfile(t)
	writeRunRoutingFile(t, filepath.Join(root, "train.sh"), "#!/bin/sh\necho train\n")
	writeRunRoutingFile(t, filepath.Join(root, "tau", "train.yaml"), `name: parity-job
engine: job
entrypoint: ../train.sh
runtime:
  image: busybox:1.36
compute:
  gpus: 0
  cpu_request: "1"
  memory_request: 1Gi
policy:
  profile: test-routing
  queue: jobqueue
`)
	withRunRoutingCWD(t, root)

	named := executeRunRoutingRoot(t, []string{"run", "train", "--context", "explicit", "--dry-run=client"})
	explicit := executeRunRoutingRoot(t, []string{"run", "--config", "tau/train.yaml", "--context", "explicit", "--dry-run=client"})
	namedObject := parseRunRoutingYAML(t, named)
	explicitObject := parseRunRoutingYAML(t, explicit)
	for _, field := range []string{"apiVersion", "kind", "spec"} {
		if !reflect.DeepEqual(namedObject[field], explicitObject[field]) {
			t.Fatalf("%s changed between literal target and explicit config\nnamed:\n%s\nexplicit:\n%s", field, named, explicit)
		}
	}
}

func TestNoCatalogTargetRejectsYAMLExtensionCollision(t *testing.T) {
	root := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(root, "tau", "train.yaml"), "name: train\n")
	writeRunRoutingFile(t, filepath.Join(root, "tau", "train.yml"), "name: train\n")
	if _, err := discoverRunInput(root, "", "train"); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("expected extension collision, got %v", err)
	}
}

func TestNoGitSubmissionConnectionDiscoveryIsCWDBounded(t *testing.T) {
	root := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(root, "tau", "workspace.connection.yaml"), runRoutingDescriptor)
	writeRunRoutingFile(t, filepath.Join(root, "tau", "train.yaml"), "name: train\n")
	resolution, err := resolveRunRequest(root, "", "", "train", false)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Connection.Git || resolution.Connection.StartDir != root {
		t.Fatalf("no-Git connection source = %#v", resolution.Connection)
	}
}

func TestNoCatalogSymlinkUsesLexicalConnectionStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := t.TempDir()
	initRunRoutingRepo(t, root)
	actual := filepath.Join(root, "physical", "tau.yaml")
	writeRunRoutingFile(t, actual, "name: physical\n")
	link := filepath.Join(root, "logical", "tau.yaml")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveRunRequest(root, "", link, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Repository.Catalog != nil {
		t.Fatal("unexpected catalog in no-catalog fixture")
	}
	if resolution.Connection.StartDir != link {
		t.Fatalf("connection start = %q, want lexical config %q", resolution.Connection.StartDir, link)
	}
}

func TestExternalSymlinkToNoCatalogRepoUsesLexicalBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	repo := t.TempDir()
	initRunRoutingRepo(t, repo)
	actual := filepath.Join(repo, "tau.yaml")
	writeRunRoutingFile(t, actual, "name: physical\n")
	externalDir := t.TempDir()
	link := filepath.Join(externalDir, "tau.yaml")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveRunRequest(externalDir, "", link, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Connection.Git || resolution.Connection.StartDir != externalDir {
		t.Fatalf("external no-catalog symlink route = %#v", resolution.Connection)
	}
}

func TestNoCatalogInternalDirectorySymlinkToNoGitUsesPhysicalBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	repo := t.TempDir()
	initRunRoutingRepo(t, repo)
	outside := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(outside, "tau.yaml"), "name: outside\n")
	linkDir := filepath.Join(repo, "linked")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveRunRequest(repo, "", filepath.Join(linkDir, "tau.yaml"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Connection.Git {
		t.Fatalf("mixed Git/non-Git symlink retained Git routing: %#v", resolution.Connection)
	}
}

func executeRunRoutingRoot(t *testing.T, args []string) string {
	t.Helper()
	cmd := NewRoot()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command %v: %v\nstderr:\n%s\nstdout:\n%s", args, err, stderr.String(), stdout.String())
	}
	return stdout.String()
}

func parseRunRoutingYAML(t *testing.T, rendered string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &object); err != nil {
		t.Fatalf("parse rendered YAML: %v\n%s", err, rendered)
	}
	return object
}

func configureRunRoutingProfile(t *testing.T) {
	t.Helper()
	installClusterProfileClientForTest(
		t,
		resolvedWorkloadProfileForTest("test-routing", "jobqueue", 0, 1),
	)
}

func multiProjectRunRoutingRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	initRunRoutingRepo(t, root)
	writeRunRoutingFile(t, filepath.Join(root, "connections", "shared.yaml"), runRoutingDescriptor)
	writeRunRoutingFile(t, filepath.Join(root, "alpha", "tau", "train.yaml"), "name: alpha-train\n")
	writeRunRoutingFile(t, filepath.Join(root, "beta", "tau", "train.yaml"), "name: beta-train\n")
	writeRunRoutingCatalog(t, root, map[string]projectcatalog.ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		"beta":  {Path: "beta", Connection: "connections/shared.yaml"},
	})
	return root
}

func initRunRoutingRepo(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput()
	if err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
}

func runRunRoutingGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeRunRoutingCatalog(t *testing.T, root string, projects map[string]projectcatalog.ProjectSpec) {
	t.Helper()
	var builder strings.Builder
	builder.WriteString("schema: tau.projects.v1\nprojects:\n")
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		project := projects[name]
		fmt.Fprintf(&builder, "  %s:\n    path: %s\n    connection: %s\n", name, project.Path, project.Connection)
	}
	writeRunRoutingFile(t, filepath.Join(root, projectcatalog.Filename), builder.String())
}

func writeRunRoutingFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func withRunRoutingCWD(t *testing.T, cwd string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}
