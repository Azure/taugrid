// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package projectcatalog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const testDescriptor = `schema: tau.workspace.connection.v1
workspace: sample
cluster:
  provider: azure
  resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-ai/providers/Microsoft.ContainerService/managedClusters/taugrid-flex
  contextName: taugrid-flex
identity:
  tenantID: 11111111-1111-1111-1111-111111111111
authorization:
  mode: cluster-wide
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
`

func TestParseStrictSchema(t *testing.T) {
	valid := `schema: tau.projects.v1
projects:
  alpha:
    path: projects/alpha
    connection: connections/alpha.yaml
`
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}

	tests := map[string]string{
		"unknown top-level field": valid + "runs: []\n",
		"unknown project field": strings.Replace(
			valid,
			"    connection: connections/alpha.yaml",
			"    connection: connections/alpha.yaml\n    namespace: ray",
			1,
		),
		"duplicate top-level key": valid + "schema: tau.projects.v1\n",
		"duplicate project key": `schema: tau.projects.v1
projects:
  alpha:
    path: projects/alpha
    connection: connections/alpha.yaml
  alpha:
    path: projects/beta
    connection: connections/beta.yaml
`,
		"duplicate entry key": strings.Replace(
			valid,
			"    path: projects/alpha",
			"    path: projects/alpha\n    path: projects/beta",
			1,
		),
		"alias project key": `schema: tau.projects.v1
projects:
  &alpha alpha:
    path: projects/alpha
    connection: connections/alpha.yaml
  *alpha:
    path: projects/beta
    connection: connections/beta.yaml
`,
		"merge key": `schema: tau.projects.v1
projects:
  alpha: &base
    path: projects/alpha
    connection: connections/alpha.yaml
  beta:
    <<: *base
`,
		"semantic numeric project key": `schema: tau.projects.v1
projects:
  1:
    path: projects/one
    connection: connections/one.yaml
  01:
    path: projects/zero-one
    connection: connections/zero-one.yaml
`,
		"tagged binary project key": `schema: tau.projects.v1
projects:
  alpha:
    path: projects/alpha
    connection: connections/alpha.yaml
  !!binary YWxwaGE=:
    path: projects/beta
    connection: connections/beta.yaml
`,
		"tagged binary schema value": `schema: !!binary cnVuZS5wcm9qZWN0cy52MQ==
projects:
  alpha:
    path: projects/alpha
    connection: connections/alpha.yaml
`,
		"tagged binary path value": `schema: tau.projects.v1
projects:
  alpha:
    path: !!binary cHJvamVjdHMvYWxwaGE=
    connection: connections/alpha.yaml
`,
		"multiple documents": valid + "---\nschema: tau.projects.v1\nprojects: {}\n",
		"unsupported schema": strings.Replace(valid, Schema, "tau.projects.v2", 1),
		"empty projects":     "schema: tau.projects.v1\nprojects: {}\n",
		"invalid name": strings.Replace(
			valid,
			"  alpha:",
			"  Alpha:",
			1,
		),
		"absolute path": strings.Replace(valid, "projects/alpha", "/tmp/alpha", 1),
		"Windows absolute path": strings.Replace(
			valid,
			"projects/alpha",
			"C:/projects/alpha",
			1,
		),
		"parent path": strings.Replace(valid, "projects/alpha", "../alpha", 1),
		"dot path":    strings.Replace(valid, "projects/alpha", "projects/./alpha", 1),
		"empty component": strings.Replace(
			valid,
			"projects/alpha",
			"projects//alpha",
			1,
		),
		"backslash path": strings.Replace(valid, "projects/alpha", `projects\alpha`, 1),
		"missing path":   strings.Replace(valid, "    path: projects/alpha\n", "", 1),
		"missing connection": strings.Replace(
			valid,
			"    connection: connections/alpha.yaml\n",
			"",
			1,
		),
	}

	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(tests[name])); err == nil {
				t.Fatalf("expected strict parse failure for:\n%s", tests[name])
			}
		})
	}
}

func TestDiscoverLexicalConfigIgnoresNestedCatalogMarker(t *testing.T) {
	root := newGitRepo(t)
	config := filepath.Join(root, "scratch", "experiment", "tau.yaml")
	writeFile(t, config, "name: scratch\n")
	writeFile(t, filepath.Join(root, "scratch", Filename), "schema: tau.projects.v1\nprojects: {}\n")
	discovery, err := DiscoverLexicalConfig(config)
	if err != nil {
		t.Fatalf("nested catalog marker changed no-catalog routing: %v", err)
	}
	if discovery.Catalog != nil || !discovery.Boundary.Git || discovery.Boundary.Root == "" {
		t.Fatalf("lexical discovery = %#v", discovery)
	}
}

func TestLoadRejectsConnectionInUninitializedSubmodule(t *testing.T) {
	parent := newGitRepo(t)
	mkdir(t, filepath.Join(parent, "alpha"))
	source := newGitRepo(t)
	writeFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "submodule")
	runGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "vendor/model")
	if err := os.RemoveAll(filepath.Join(parent, "vendor", "model")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(parent, "vendor", "model", "connection.yaml"), testDescriptor)
	writeCatalog(t, parent, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "vendor/model/connection.yaml"},
	})
	if _, err := Load(parent); err == nil || !strings.Contains(err.Error(), "connection") || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected uninitialized submodule connection error, got %v", err)
	}
}

func TestLoadRejectsTargetInUninitializedSubmodule(t *testing.T) {
	parent := newGitRepo(t)
	mkdir(t, filepath.Join(parent, "alpha"))
	writeFile(t, filepath.Join(parent, "connections", "shared.yaml"), testDescriptor)
	source := newGitRepo(t)
	writeFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "submodule")
	runGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "alpha/tau")
	if err := os.RemoveAll(filepath.Join(parent, "alpha", "tau")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(parent, "alpha", "tau", "train.yaml"), "name: train\n")
	writeCatalog(t, parent, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(parent); err == nil || !strings.Contains(err.Error(), `target "train"`) || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected uninitialized submodule target error, got %v", err)
	}
}

func TestLoadDerivesTargetsAndAllowsSharedConnectionAndZeroTargets(t *testing.T) {
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	writeFile(t, filepath.Join(root, "alpha", "tau", "train.yaml"), "name: alpha-train\n")
	writeFile(t, filepath.Join(root, "alpha", "tau", "smoke.yaml"), "name: alpha-smoke\n")
	writeFile(t, filepath.Join(root, "alpha", "tau.yaml"), "name: alpha-default\n")
	mkdir(t, filepath.Join(root, "beta", "tau"))
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		"beta":  {Path: "beta", Connection: "connections/shared.yaml"},
	})

	catalog, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	alpha := catalog.Projects["alpha"]
	beta := catalog.Projects["beta"]
	if got := alpha.Targets["train"]; got != filepath.Join(root, "alpha", "tau", "train.yaml") {
		t.Fatalf("train target = %q", got)
	}
	if _, ok := alpha.Targets["smoke"]; ok {
		t.Fatal("reserved smoke was exposed as a public target")
	}
	if len(beta.Targets) != 0 {
		t.Fatalf("zero-target project has targets: %#v", beta.Targets)
	}
	if alpha.Connection.Path != beta.Connection.Path {
		t.Fatalf("shared descriptor resolved differently: %s != %s", alpha.Connection.Path, beta.Connection.Path)
	}
	if alpha.DefaultConfigPath != filepath.Join(root, "alpha", "tau.yaml") {
		t.Fatalf("default config = %q", alpha.DefaultConfigPath)
	}
}

func TestLoadRejectsTargetExtensionCollision(t *testing.T) {
	root := singleProjectRepo(t)
	writeFile(t, filepath.Join(root, "alpha", "tau", "train.yaml"), "name: train\n")
	writeFile(t, filepath.Join(root, "alpha", "tau", "train.yml"), "name: train\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), `run target "train"`) {
		t.Fatalf("expected extension collision, got %v", err)
	}
}

func TestLoadRejectsMultipleDefaultConfigs(t *testing.T) {
	root := singleProjectRepo(t)
	writeFile(t, filepath.Join(root, "alpha", "tau.yaml"), "name: first\n")
	writeFile(t, filepath.Join(root, "alpha", "tau.yml"), "name: second\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "multiple default Tau configs") {
		t.Fatalf("expected default collision, got %v", err)
	}
}

func TestLoadRejectsOverlappingProjectRoots(t *testing.T) {
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	mkdir(t, filepath.Join(root, "projects", "alpha", "nested"))
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha":  {Path: "projects/alpha", Connection: "connections/shared.yaml"},
		"nested": {Path: "projects/alpha/nested", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "must be disjoint") {
		t.Fatalf("expected overlapping-root error, got %v", err)
	}
}

func TestLoadRejectsNestedGitProject(t *testing.T) {
	root := newGitRepo(t)
	nested := filepath.Join(root, "vendor", "model")
	initGitRepo(t, nested)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	writeCatalog(t, root, map[string]ProjectSpec{
		"model": {Path: "vendor/model", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "not catalog worktree") {
		t.Fatalf("expected nested-Git boundary error, got %v", err)
	}
}

func TestLoadRejectsTargetInNestedGitRepository(t *testing.T) {
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	mkdir(t, filepath.Join(root, "alpha"))
	nestedTargets := filepath.Join(root, "alpha", "tau")
	initGitRepo(t, nestedTargets)
	writeFile(t, filepath.Join(nestedTargets, "train.yaml"), "name: nested\n")
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), `target "train" belongs to Git worktree`) {
		t.Fatalf("expected nested target boundary error, got %v", err)
	}
}

func TestLoadRejectsSubmoduleProject(t *testing.T) {
	parent := newGitRepo(t)
	writeFile(t, filepath.Join(parent, "README.md"), "parent\n")
	runGit(t, parent, "add", "README.md")
	runGit(t, parent, "commit", "-m", "parent")

	source := newGitRepo(t)
	writeFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "submodule")
	runGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "vendor/model")

	writeFile(t, filepath.Join(parent, "connections", "shared.yaml"), testDescriptor)
	writeCatalog(t, parent, map[string]ProjectSpec{
		"model": {Path: "vendor/model", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(parent); err == nil || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected submodule boundary error, got %v", err)
	}
}

func TestLoadRejectsUninitializedSubmoduleProject(t *testing.T) {
	parent := newGitRepo(t)
	writeFile(t, filepath.Join(parent, "README.md"), "parent\n")
	runGit(t, parent, "add", "README.md")
	runGit(t, parent, "commit", "-m", "parent")

	source := newGitRepo(t)
	writeFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "submodule")
	runGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "vendor/model")
	if err := os.RemoveAll(filepath.Join(parent, "vendor", "model")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(parent, "vendor", "model"))

	writeFile(t, filepath.Join(parent, "connections", "shared.yaml"), testDescriptor)
	writeCatalog(t, parent, map[string]ProjectSpec{
		"model": {Path: "vendor/model", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(parent); err == nil || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected uninitialized submodule boundary error, got %v", err)
	}
}

func TestLoadRejectsFilesystemAliasToUninitializedSubmodule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	parent := newGitRepo(t)
	writeFile(t, filepath.Join(parent, "README.md"), "parent\n")
	runGit(t, parent, "add", "README.md")
	runGit(t, parent, "commit", "-m", "parent")

	source := newGitRepo(t)
	writeFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "submodule")
	runGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", source, "vendor/model")
	if err := os.RemoveAll(filepath.Join(parent, "vendor", "model")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(parent, "vendor", "model"))
	if err := os.Symlink(filepath.Join(parent, "vendor", "model"), filepath.Join(parent, "model-link")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(parent, "connections", "shared.yaml"), testDescriptor)
	writeCatalog(t, parent, map[string]ProjectSpec{
		"model": {Path: "model-link", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(parent); err == nil || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected filesystem alias submodule boundary error, got %v", err)
	}
}

func TestLoadRejectsCaseInsensitiveFilesystemAliases(t *testing.T) {
	root := newGitRepo(t)
	upper := filepath.Join(root, "Projects", "Alpha")
	lower := filepath.Join(root, "projects", "alpha")
	mkdir(t, upper)
	upperInfo, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Stat(lower)
	if err != nil || !os.SameFile(upperInfo, lowerInfo) {
		t.Skip("test filesystem is case-sensitive")
	}
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "Projects/Alpha", Connection: "connections/shared.yaml"},
		"alias": {Path: "projects/alpha", Connection: "connections/shared.yaml"},
	})
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "must be disjoint") {
		t.Fatalf("expected filesystem alias overlap, got %v", err)
	}
}

func TestLoadSupportsLinkedWorktree(t *testing.T) {
	mainRoot := newGitRepo(t)
	writeFile(t, filepath.Join(mainRoot, "README.md"), "main\n")
	runGit(t, mainRoot, "add", "README.md")
	runGit(t, mainRoot, "commit", "-m", "initial")

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, mainRoot, "worktree", "add", "-b", "linked-test", linked)
	writeFile(t, filepath.Join(linked, "connections", "shared.yaml"), testDescriptor)
	mkdir(t, filepath.Join(linked, "alpha"))
	writeCatalog(t, linked, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
	})

	catalog, err := Load(linked)
	if err != nil {
		t.Fatalf("linked worktree Load: %v", err)
	}
	if catalog.LexicalRoot != linked {
		t.Fatalf("catalog lexical root = %q, want linked worktree %q", catalog.LexicalRoot, linked)
	}
}

func TestLoadAcceptsCaseInsensitiveWorktreeAlias(t *testing.T) {
	parent := t.TempDir()
	canonical := filepath.Join(parent, "CaseRepo")
	initGitRepo(t, canonical)
	writeFile(t, filepath.Join(canonical, "connections", "shared.yaml"), testDescriptor)
	mkdir(t, filepath.Join(canonical, "alpha"))
	writeCatalog(t, canonical, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
	})
	alias := filepath.Join(parent, "caserepo")
	canonicalInfo, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(canonicalInfo, aliasInfo) {
		t.Skip("test filesystem is case-sensitive")
	}
	catalog, err := Load(alias)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Projects) != 1 || catalog.Projects["alpha"] == nil {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestDiscoverUsesExactGitRootAndIgnoresParentCatalog(t *testing.T) {
	parent := newGitRepo(t)
	writeFile(t, filepath.Join(parent, "connections", "shared.yaml"), testDescriptor)
	mkdir(t, filepath.Join(parent, "alpha"))
	writeCatalog(t, parent, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
	})

	nested := filepath.Join(parent, "nested")
	initGitRepo(t, nested)
	repository, err := Discover(nested)
	if err != nil {
		t.Fatal(err)
	}
	if repository.Catalog != nil || repository.Boundary.LexicalRoot != nested {
		t.Fatalf("nested repository inherited parent catalog: %#v", repository)
	}
}

func TestDiscoverNoGitIsStartDirectoryBounded(t *testing.T) {
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, Filename), "not: relevant\n")
	child := filepath.Join(parent, "child")
	mkdir(t, child)

	repository, err := Discover(child)
	if err != nil {
		t.Fatal(err)
	}
	if repository.Boundary.Git || repository.Boundary.LexicalRoot != child || repository.Catalog != nil {
		t.Fatalf("no-Git discovery escaped CWD: %#v", repository)
	}
}

func TestDiscoverMalformedCatalogFailsClosed(t *testing.T) {
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, Filename), "schema: tau.projects.v1\nprojects:\n  alpha:\n    path: alpha\n    unknown: value\n")
	if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("malformed catalog did not fail closed: %v", err)
	}
}

func TestDiscoverValidatesRootCatalogFile(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		root := newGitRepo(t)
		mkdir(t, filepath.Join(root, Filename))
		if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("catalog directory was accepted: %v", err)
		}
	})
	if runtime.GOOS == "windows" {
		return
	}
	t.Run("dangling symlink", func(t *testing.T) {
		root := newGitRepo(t)
		if err := os.Symlink(filepath.Join(root, "missing.yaml"), filepath.Join(root, Filename)); err != nil {
			t.Fatal(err)
		}
		if _, err := Discover(root); err == nil {
			t.Fatal("dangling catalog symlink silently fell back to no-catalog mode")
		}
	})
	t.Run("outside symlink", func(t *testing.T) {
		root := newGitRepo(t)
		outside := filepath.Join(t.TempDir(), "catalog.yaml")
		writeFile(t, outside, "schema: tau.projects.v1\nprojects: {}\n")
		if err := os.Symlink(outside, filepath.Join(root, Filename)); err != nil {
			t.Fatal(err)
		}
		if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "resolves outside Git worktree") {
			t.Fatalf("outside catalog symlink was accepted: %v", err)
		}
	})
	t.Run("contained symlink", func(t *testing.T) {
		root := newGitRepo(t)
		writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
		mkdir(t, filepath.Join(root, "alpha"))
		actual := filepath.Join(root, "config", "catalog.yaml")
		writeCatalog(t, root, map[string]ProjectSpec{
			"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		})
		mkdir(t, filepath.Dir(actual))
		if err := os.Rename(filepath.Join(root, Filename), actual); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(actual, filepath.Join(root, Filename)); err != nil {
			t.Fatal(err)
		}
		if _, err := Discover(root); err != nil {
			t.Fatalf("contained catalog symlink rejected: %v", err)
		}
	})
	t.Run("nested Git symlink", func(t *testing.T) {
		root := newGitRepo(t)
		nested := filepath.Join(root, "nested")
		initGitRepo(t, nested)
		actual := filepath.Join(nested, "catalog.yaml")
		writeFile(t, actual, "schema: tau.projects.v1\nprojects: {}\n")
		if err := os.Symlink(actual, filepath.Join(root, Filename)); err != nil {
			t.Fatal(err)
		}
		if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "belongs to Git worktree") {
			t.Fatalf("nested Git catalog symlink was accepted: %v", err)
		}
	})
	t.Run("uninitialized gitlink symlink", func(t *testing.T) {
		root := newGitRepo(t)
		source := newGitRepo(t)
		writeFile(t, filepath.Join(source, "catalog.yaml"), "schema: tau.projects.v1\nprojects: {}\n")
		runGit(t, source, "add", "catalog.yaml")
		runGit(t, source, "commit", "-m", "catalog")
		runGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", source, "vendor/model")
		if err := os.RemoveAll(filepath.Join(root, "vendor", "model")); err != nil {
			t.Fatal(err)
		}
		actual := filepath.Join(root, "vendor", "model", "catalog.yaml")
		writeFile(t, actual, "schema: tau.projects.v1\nprojects: {}\n")
		if err := os.Symlink(actual, filepath.Join(root, Filename)); err != nil {
			t.Fatal(err)
		}
		if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "Git submodule") {
			t.Fatalf("uninitialized gitlink catalog symlink was accepted: %v", err)
		}
	})
}

func TestLoadRejectsSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	t.Run("project", func(t *testing.T) {
		root := newGitRepo(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "alpha")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
		writeCatalog(t, root, map[string]ProjectSpec{
			"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		})
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "outside catalog worktree") {
			t.Fatalf("expected project symlink escape, got %v", err)
		}
	})
	t.Run("connection", func(t *testing.T) {
		root := newGitRepo(t)
		mkdir(t, filepath.Join(root, "alpha"))
		outside := filepath.Join(t.TempDir(), "connection.yaml")
		writeFile(t, outside, testDescriptor)
		mkdir(t, filepath.Join(root, "connections"))
		if err := os.Symlink(outside, filepath.Join(root, "connections", "shared.yaml")); err != nil {
			t.Fatal(err)
		}
		writeCatalog(t, root, map[string]ProjectSpec{
			"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		})
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "outside catalog worktree") {
			t.Fatalf("expected connection symlink escape, got %v", err)
		}
	})
	t.Run("target leaves project", func(t *testing.T) {
		root := newGitRepo(t)
		writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
		writeFile(t, filepath.Join(root, "beta", "tau", "train.yaml"), "name: beta\n")
		mkdir(t, filepath.Join(root, "alpha", "tau"))
		if err := os.Symlink(
			filepath.Join(root, "beta", "tau", "train.yaml"),
			filepath.Join(root, "alpha", "tau", "train.yaml"),
		); err != nil {
			t.Fatal(err)
		}
		writeCatalog(t, root, map[string]ProjectSpec{
			"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
			"beta":  {Path: "beta", Connection: "connections/shared.yaml"},
		})
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "escapes project root") {
			t.Fatalf("expected target symlink escape, got %v", err)
		}
	})
}

func singleProjectRepo(t *testing.T) string {
	t.Helper()
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "alpha", "tau", "workspace.connection.yaml"), testDescriptor)
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "alpha/tau/workspace.connection.yaml"},
	})
	return root
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	initGitRepo(t, root)
	return root
}

func initGitRepo(t *testing.T, root string) {
	t.Helper()
	mkdir(t, root)
	runGit(t, root, "init", "--quiet")
	runGit(t, root, "config", "user.email", "tau-test@example.com")
	runGit(t, root, "config", "user.name", "Tau Test")
}

func runGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeCatalog(t *testing.T, root string, projects map[string]ProjectSpec) {
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
	writeFile(t, filepath.Join(root, Filename), builder.String())
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
