// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package projectcatalog

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSubmissionProjectSelectionMatrix(t *testing.T) {
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	writeFile(t, filepath.Join(root, "alpha", "tau", "train.yaml"), "name: alpha-train\n")
	writeFile(t, filepath.Join(root, "beta", "tau", "train.yml"), "name: beta-train\n")
	writeFile(t, filepath.Join(root, "beta", "tau", "eval.yaml"), "name: beta-eval\n")
	alphaConfig := filepath.Join(root, "alpha", "experiments", "ablation", "tau.yaml")
	writeFile(t, alphaConfig, "name: alpha-ablation\n")
	unowned := filepath.Join(root, "scratch", "tau.yaml")
	writeFile(t, unowned, "name: scratch\n")
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		"beta":  {Path: "beta", Connection: "connections/shared.yaml"},
	})
	catalog, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("explicit project", func(t *testing.T) {
		project, err := catalog.SelectProject(SelectionOptions{ProjectName: "beta", CWD: root})
		if err != nil || project.Name != "beta" {
			t.Fatalf("project=%v err=%v", project, err)
		}
	})
	t.Run("explicit config from unrelated CWD", func(t *testing.T) {
		project, err := catalog.SelectProject(SelectionOptions{ConfigPath: alphaConfig, CWD: t.TempDir()})
		if err != nil || project.Name != "alpha" {
			t.Fatalf("project=%v err=%v", project, err)
		}
	})
	t.Run("project config mismatch", func(t *testing.T) {
		_, err := catalog.SelectProject(SelectionOptions{ProjectName: "beta", ConfigPath: alphaConfig})
		if err == nil || !strings.Contains(err.Error(), "does not own") {
			t.Fatalf("expected mismatch, got %v", err)
		}
	})
	t.Run("unowned config", func(t *testing.T) {
		_, err := catalog.SelectProject(SelectionOptions{ConfigPath: unowned})
		if err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Fatalf("expected unowned config failure, got %v", err)
		}
	})
	t.Run("containing CWD affinity", func(t *testing.T) {
		cwd := filepath.Join(root, "alpha", "src")
		mkdir(t, cwd)
		project, err := catalog.SelectProject(SelectionOptions{CWD: cwd, Target: "eval"})
		if err != nil || project.Name != "alpha" {
			t.Fatalf("project=%v err=%v", project, err)
		}
		if _, err := project.ResolveInput("", "eval"); err == nil || !strings.Contains(err.Error(), `project "alpha"`) {
			t.Fatalf("sticky project should report local target absence, got %v", err)
		}
	})
	t.Run("unique root target", func(t *testing.T) {
		project, err := catalog.SelectProject(SelectionOptions{CWD: root, Target: "eval"})
		if err != nil || project.Name != "beta" {
			t.Fatalf("project=%v err=%v", project, err)
		}
	})
	t.Run("duplicate root target", func(t *testing.T) {
		_, err := catalog.SelectProject(SelectionOptions{CWD: root, Target: "train"})
		if err == nil || !strings.Contains(err.Error(), "alpha, beta") || !strings.Contains(err.Error(), "--project") {
			t.Fatalf("expected actionable target ambiguity, got %v", err)
		}
	})
	t.Run("built-in smoke root ambiguity", func(t *testing.T) {
		_, err := catalog.SelectProject(SelectionOptions{CWD: root, Target: "smoke"})
		if err == nil || !strings.Contains(err.Error(), "Valid projects: alpha, beta") {
			t.Fatalf("expected smoke ambiguity, got %v", err)
		}
	})
	t.Run("explicit project smoke", func(t *testing.T) {
		project, err := catalog.SelectProject(SelectionOptions{ProjectName: "alpha", CWD: root, Target: "smoke"})
		if err != nil {
			t.Fatal(err)
		}
		input, err := project.ResolveInput("", "smoke")
		if err != nil || !input.BuiltinSmoke {
			t.Fatalf("input=%#v err=%v", input, err)
		}
	})
}

func TestOneProjectFallbackAndLifecycleSelection(t *testing.T) {
	root := singleProjectRepo(t)
	catalog, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := catalog.SelectProject(SelectionOptions{CWD: root, Target: "missing"})
	if err != nil || project.Name != "alpha" {
		t.Fatalf("submission fallback project=%v err=%v", project, err)
	}
	project, err = catalog.SelectLifecycleProject("", root)
	if err != nil || project.Name != "alpha" {
		t.Fatalf("lifecycle fallback project=%v err=%v", project, err)
	}
}

func TestLifecycleRootAmbiguityAndContainingProject(t *testing.T) {
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	mkdir(t, filepath.Join(root, "alpha", "src"))
	mkdir(t, filepath.Join(root, "beta"))
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		"beta":  {Path: "beta", Connection: "connections/shared.yaml"},
	})
	catalog, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SelectLifecycleProject("", root); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("expected root lifecycle ambiguity, got %v", err)
	}
	project, err := catalog.SelectLifecycleProject("", filepath.Join(root, "alpha", "src"))
	if err != nil || project.Name != "alpha" {
		t.Fatalf("containing lifecycle project=%v err=%v", project, err)
	}
	project, err = catalog.SelectLifecycleProject("beta", filepath.Join(root, "alpha", "src"))
	if err != nil || project.Name != "beta" {
		t.Fatalf("explicit lifecycle project=%v err=%v", project, err)
	}
}

func TestLifecycleCWDRejectsUninitializedGitlink(t *testing.T) {
	root := newGitRepo(t)
	mkdir(t, filepath.Join(root, "alpha"))
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	source := newGitRepo(t)
	writeFile(t, filepath.Join(source, "README.md"), "submodule\n")
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "submodule")
	runGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", source, "alpha/vendor/model")
	if err := os.RemoveAll(filepath.Join(root, "alpha", "vendor", "model")); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "alpha", "vendor", "model")
	mkdir(t, cwd)
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
	})
	catalog, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.SelectLifecycleProject("", cwd); err == nil || !strings.Contains(err.Error(), "Git submodule") {
		t.Fatalf("expected lifecycle CWD gitlink rejection, got %v", err)
	}
}

func TestExplicitProjectSmokeConfigLoadsFile(t *testing.T) {
	root := singleProjectRepo(t)
	smokeConfig := filepath.Join(root, "alpha", "tau", "smoke.yaml")
	writeFile(t, smokeConfig, "name: project-smoke\n")
	catalog, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := catalog.SelectProject(SelectionOptions{ConfigPath: smokeConfig, Target: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := project.ResolveInput(smokeConfig, "smoke")
	if err != nil {
		t.Fatal(err)
	}
	if input.BuiltinSmoke || !input.ExplicitConfig || input.ConfigPath != smokeConfig {
		t.Fatalf("explicit project smoke resolved incorrectly: %#v", input)
	}
}

func TestProjectForPathRejectsCrossProjectSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := newGitRepo(t)
	writeFile(t, filepath.Join(root, "connections", "shared.yaml"), testDescriptor)
	betaConfig := filepath.Join(root, "beta", "experiments", "tau.yaml")
	writeFile(t, betaConfig, "name: beta\n")
	mkdir(t, filepath.Join(root, "alpha", "experiments"))
	link := filepath.Join(root, "alpha", "experiments", "tau.yaml")
	if err := os.Symlink(betaConfig, link); err != nil {
		t.Fatal(err)
	}
	writeCatalog(t, root, map[string]ProjectSpec{
		"alpha": {Path: "alpha", Connection: "connections/shared.yaml"},
		"beta":  {Path: "beta", Connection: "connections/shared.yaml"},
	})
	catalog, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ProjectForPath(link); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected cross-project symlink rejection, got %v", err)
	}
}

func TestResolveExplicitConfigPreservesRelativeSpelling(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "experiments", "tau.yaml"), "name: test\n")
	config, err := ResolveExplicitConfig(root, "experiments/tau.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if config.Original != filepath.Join("experiments", "tau.yaml") {
		t.Fatalf("original = %q", config.Original)
	}
	if config.Lexical != filepath.Join(root, "experiments", "tau.yaml") {
		t.Fatalf("lexical = %q", config.Lexical)
	}
	wantResolved, err := filepath.EvalSymlinks(filepath.Join(root, "experiments", "tau.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Resolved != wantResolved {
		t.Fatalf("resolved = %q, want %q", config.Resolved, wantResolved)
	}
}
