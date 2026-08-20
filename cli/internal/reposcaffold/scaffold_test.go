// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reposcaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/runconfig"
)

func TestRenderPythonScaffold(t *testing.T) {
	dir := t.TempDir()
	result, err := Render(Options{
		Name:                "my-experiment",
		OutputDir:           dir,
		Image:               "example.azurecr.io/my-experiment:test",
		Workspace:           "research-ws",
		AzureSubscriptionID: "00000000-0000-0000-0000-000000000000",
		AzureTenantID:       "11111111-1111-1111-1111-111111111111",
		AKSResourceGroup:    "rg-ai",
		AKSCluster:          "aks-ai",
		ACRName:             "example",
	})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, want := range []string{
		"AGENTS.md",
		".env.example",
		".gitignore",
		"pyproject.toml",
		".python-version",
		"README.md",
		"images/train.Dockerfile",
		"tau/smoke.yaml",
		"tau/train.yaml",
		"tau/workspace.connection.yaml",
		"scripts/configure.sh",
		"scripts/doctor.sh",
		"scripts/setup-azure.sh",
		"scripts/setup.sh",
		"scripts/smoke.sh",
		"scripts/train.sh",
		"src/my_experiment/__init__.py",
		"src/my_experiment/smoke.py",
		"src/my_experiment/train.py",
	} {
		if !contains(result.Files, want) {
			t.Fatalf("generated files missing %s: %#v", want, result.Files)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(want))); err != nil {
			t.Fatalf("stat generated %s: %v", want, err)
		}
	}

	agents := readFile(t, filepath.Join(dir, "AGENTS.md"))
	assertContains(t, agents, "`TauWorkspace` owns access, queue, namespace, and status; it does not own storage.")
	assertContains(t, agents, "This repo owns Python source")
	assertContains(t, agents, "experiment-specific data/PVC/secret references")
	assertContains(t, agents, "only reference platform-provided claims")
	assertContains(t, agents, "Do **not** add `policy.workspace`")
	assertContains(t, agents, "Do not commit workspace-derived namespace, queue policy, kubeconfig, or identity credentials")
	assertContains(t, agents, "Never commit `.env`, credentials, kubeconfigs, tokens")
	assertContains(t, agents, "Keep smoke cheap, CPU-sized, public/synthetic")
	assertNotContains(t, agents, "policy.workspace:")
	assertNotContains(t, agents, "git add .env")
	envExample := readFile(t, filepath.Join(dir, ".env.example"))
	assertNotContains(t, envExample, "TAU_WORKSPACE")
	assertNotContains(t, envExample, "AKS_CLUSTER_NAME")
	gitignore := readFile(t, filepath.Join(dir, ".gitignore"))
	for _, want := range []string{".env", ".azure/", ".kube/", "kubeconfig*", "*.kubeconfig"} {
		assertContains(t, gitignore, want)
	}
	pyproject := readFile(t, filepath.Join(dir, "pyproject.toml"))
	assertContains(t, pyproject, "[project.optional-dependencies]")
	assertNotContains(t, pyproject, "git@github.com")
	assertNotContains(t, pyproject, "[tool.uv.sources]")
	readme := readFile(t, filepath.Join(dir, "README.md"))
	assertContains(t, readme, "This step is local-only")
	assertContains(t, readme, "Generated from the standalone `tau-gen` workflow")
	assertContains(t, readme, "docker build -f images/train.Dockerfile -t \"$IMAGE\" .")
	assertContains(t, readme, "tau run smoke")
	assertContains(t, readme, "tau run train")
	assertContains(t, readme, "checked-in")
	assertContains(t, readme, "direct Tau configs")
	assertContains(t, readme, "Tau authoring strategy")
	assertContains(t, readme, "You do not need")
	assertNotContains(t, readme, "--workspace")
	assertNotContains(t, readme, "az aks get-credentials")
	assertNotContains(t, readme, "git add .env")
	assertNotContains(t, readme, "policy.workspace:")
	setup := readFile(t, filepath.Join(dir, "scripts/setup.sh"))
	assertContains(t, setup, "uv sync --extra dev")
	assertNotContains(t, setup, "uv sync --extra dev --extra tau")
	assertContains(t, setup, "Next steps:")
	assertNotContains(t, readme, "Tau SDK setup")
	setupAzure := readFile(t, filepath.Join(dir, "scripts/setup-azure.sh"))
	assertContains(t, setupAzure, "Tau workspace authentication and AKS credentials are handled by tau run")
	assertNotContains(t, setupAzure, "az aks get-credentials")
	assertNotContains(t, setupAzure, "TAU_WORKSPACE")
	doctor := readFile(t, filepath.Join(dir, "scripts/doctor.sh"))
	assertContains(t, doctor, "check_file \"$path\"")
	assertContains(t, doctor, "warn: .env not found")
	assertContains(t, doctor, "warn: IMAGE is not set")
	assertNotContains(t, doctor, "kubelogin")
	// Debian and Ubuntu ship python3 only, so a bare python check always fails there.
	assertContains(t, doctor, "check_cmd python3 || status=1")
	assertNotContains(t, doctor, "check_cmd python || status=1")
	dockerfile := readFile(t, filepath.Join(dir, "images/train.Dockerfile"))
	assertContains(t, dockerfile, "FROM mcr.microsoft.com/azurelinux/base/python:${PYTHON_VERSION}")
	assertContains(t, dockerfile, "ln -sf /usr/bin/python3 /usr/local/bin/python")
	assertContains(t, dockerfile, "python3 -m pip install --no-cache-dir uv")
	assertNotContains(t, dockerfile, "FROM python:")

	for _, rel := range []string{"tau/smoke.yaml", "tau/train.yaml"} {
		raw := readFile(t, filepath.Join(dir, filepath.FromSlash(rel)))
		assertNotContains(t, raw, "policy.workspace")
		assertNotContains(t, raw, "schema_version")
		assertContains(t, raw, "disable_default_priorities: true")
		if _, err := runconfig.Load(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("generated %s is not a valid run config: %v\n%s", rel, err, raw)
		}
	}
	assertContains(t, readFile(t, filepath.Join(dir, "tau/smoke.yaml")), "name: my-experiment-smoke")
	assertContains(t, readFile(t, filepath.Join(dir, "tau/smoke.yaml")), "gpus: 0")
	assertNotContains(t, readFile(t, filepath.Join(dir, "tau/smoke.yaml")), "profile:")
	assertContains(t, readFile(t, filepath.Join(dir, "tau/train.yaml")), "name: my-experiment-train")
	assertContains(t, readFile(t, filepath.Join(dir, "tau/train.yaml")), "gpus: 0")
	assertNotContains(t, readFile(t, filepath.Join(dir, "tau/train.yaml")), "profile:")
	connection := readFile(t, filepath.Join(dir, "tau/workspace.connection.yaml"))
	for _, want := range []string{
		"schema: tau.workspace.connection.v1",
		"workspace: research-ws",
		"systemNamespace: tau-system",
		"mode: workspace-rbac",
		"requiredRole: tau-researcher-v1",
		"resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-ai/providers/Microsoft.ContainerService/managedClusters/aks-ai",
		"tenantID: 11111111-1111-1111-1111-111111111111",
	} {
		assertContains(t, connection, want)
	}
	descriptor, err := workspaceconnection.Parse([]byte(connection))
	if err != nil {
		t.Fatalf("generated workspace connection is invalid: %v\n%s", err, connection)
	}
	if descriptor.Authorization.Mode != workspaceconnection.AuthorizationModeWorkspaceRBAC ||
		descriptor.Authorization.RequiredRole != "tau-researcher-v1" {
		t.Fatalf("generated authorization = %#v, want workspace-rbac with tau-researcher-v1", descriptor.Authorization)
	}

	for _, rel := range []string{"scripts/setup.sh", "scripts/setup-azure.sh", "scripts/doctor.sh", "scripts/configure.sh", "scripts/smoke.sh", "scripts/train.sh"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v", rel, info.Mode().Perm())
		}
	}
}

func TestRenderNoClobberAndForce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Render(Options{Name: "demo", OutputDir: dir, Image: "example.azurecr.io/demo:test"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected no-clobber error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("no-clobber preflight left partial scaffold file behind: %v", err)
	}
	assertContains(t, readFile(t, filepath.Join(dir, "README.md")), "custom")
	if _, err := Render(Options{Name: "demo", OutputDir: dir, Image: "example.azurecr.io/demo:test", Force: true}); err != nil {
		t.Fatalf("force render failed: %v", err)
	}
	assertContains(t, readFile(t, filepath.Join(dir, "README.md")), "Tau-ready Python research workspace")
}

func TestPackageNameNormalizationAndValidation(t *testing.T) {
	files, err := Preview(Options{Name: "My-Experiment.42", Image: "example.azurecr.io/pkg:test"})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	if !previewContains(files, "src/my_experiment_42/smoke.py") {
		t.Fatalf("hyphenated/dotted name did not normalize to package path: %#v", files)
	}
	assertPreviewContains(t, files, "tau/smoke.yaml", "name: my-experiment-42-smoke")

	files, err = Preview(Options{Name: "123-model", Image: "example.azurecr.io/pkg:test"})
	if err != nil {
		t.Fatalf("Preview numeric-prefixed name failed: %v", err)
	}
	if !previewContains(files, "src/tau_123_model/smoke.py") {
		t.Fatalf("numeric-prefixed name did not get safe package prefix: %#v", files)
	}
	assertPreviewContains(t, files, "tau/smoke.yaml", "name: tau-123-model-smoke")

	for _, opts := range []Options{
		{Name: "!!!", Image: "example.azurecr.io/pkg:test"},
		{Name: "demo", Package: "bad/package", Image: "example.azurecr.io/pkg:test"},
	} {
		if _, err := Preview(opts); err == nil || !strings.Contains(err.Error(), "invalid Python package name") {
			t.Fatalf("expected invalid package error for %#v, got %v", opts, err)
		}
	}
}

func TestExternalGitHubTemplateExtendsPythonContract(t *testing.T) {
	dir := t.TempDir()
	result, err := Render(Options{
		Name:          "open-repo",
		Template:      TemplateExternalGitHub,
		OutputDir:     dir,
		Image:         "example.azurecr.io/open-repo:test",
		UpstreamRepo:  "https://github.com/example/project.git",
		UpstreamRef:   "abc123",
		PackageImport: "project",
	})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, want := range []string{
		"pyproject.toml",
		"src/open_repo/smoke.py",
		"src/open_repo/train.py",
		"scripts/setup.sh",
		"scripts/configure.sh",
		"tau/smoke.yaml",
		"tau/train.yaml",
		"images/train.Dockerfile",
	} {
		if !contains(result.Files, want) {
			t.Fatalf("external-github generated files missing %s: %#v", want, result.Files)
		}
	}
	assertContains(t, readFile(t, filepath.Join(dir, "images/train.Dockerfile")), `git clone "${UPSTREAM_REPO}"`)
	assertContains(t, readFile(t, filepath.Join(dir, "src/open_repo/smoke.py")), `package_import = "project"`)
	for _, rel := range []string{"tau/smoke.yaml", "tau/train.yaml"} {
		if _, err := runconfig.Load(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("generated external-github %s is not a valid run config: %v", rel, err)
		}
	}
}

func TestPreviewDPRTemplateExtendsPythonContract(t *testing.T) {
	files, err := Preview(Options{Name: "dpr-tau", Template: TemplateDPR, Image: "example.azurecr.io/dpr:test"})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	byPath := map[string]RenderedFile{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	assertContains(t, byPath["src/dpr_tau/smoke.py"].Content, "package_import = \"dpr\"")
	assertContains(t, byPath["images/train.Dockerfile"].Content, "git clone \"${UPSTREAM_REPO}\"")
	assertContains(t, byPath["images/train.Dockerfile"].Content, "mcr.microsoft.com/azurelinux/base/python")
	assertContains(t, byPath["tau/smoke.yaml"].Content, "UPSTREAM_REPO: https://github.com/facebookresearch/DPR.git")
	assertContains(t, byPath["tau/smoke.yaml"].Content, "name: dpr-tau-smoke")
	assertNotContains(t, byPath["pyproject.toml"].Content, "[tool.uv.sources]")
	assertContains(t, byPath["scripts/setup.sh"].Content, "uv sync --extra dev")
	assertNotContains(t, byPath["tau/smoke.yaml"].Content, "policy.workspace")
	assertContains(t, byPath["AGENTS.md"].Content, "`TauWorkspace` owns access")
}

func TestCommandQuotingInGeneratedScriptsAndYAML(t *testing.T) {
	files, err := Preview(Options{
		Name:         "quote-test",
		Image:        "example.azurecr.io/quote:test",
		TrainCommand: `python -c 'print("hello world")'`,
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	byPath := map[string]RenderedFile{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	assertContains(t, byPath["scripts/train.sh"].Content, `TRAIN_COMMAND='python -c '"'"'print("hello world")'"'"''`)
	assertContains(t, byPath["tau/train.yaml"].Content, `TRAIN_COMMAND: 'python -c ''print("hello world")'''`)
}

func TestGeneratedShellSyntax(t *testing.T) {
	dir := t.TempDir()
	if _, err := Render(Options{Name: "shell-test", OutputDir: dir, Image: "example.azurecr.io/shell:test"}); err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, rel := range []string{"scripts/setup.sh", "scripts/setup-azure.sh", "scripts/doctor.sh", "scripts/configure.sh", "scripts/smoke.sh", "scripts/train.sh"} {
		cmd := exec.Command("bash", "-n", filepath.Join(dir, filepath.FromSlash(rel)))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n %s failed: %v\n%s", rel, err, out)
		}
	}
}

func TestConfigureScriptKeepsEnvInSync(t *testing.T) {
	const newImage = "example.azurecr.io/shell:configured"

	t.Run("existing env is updated", func(t *testing.T) {
		dir := renderForConfigure(t)
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("IMAGE=example.azurecr.io/shell:stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runConfigure(t, dir, newImage)
		assertContains(t, readFile(t, filepath.Join(dir, ".env")), "IMAGE="+newImage)
		assertContains(t, readFile(t, filepath.Join(dir, ".env.example")), "IMAGE="+newImage)
	})

	t.Run("missing env is created from the example", func(t *testing.T) {
		dir := renderForConfigure(t)
		runConfigure(t, dir, newImage)
		env := readFile(t, filepath.Join(dir, ".env"))
		assertContains(t, env, "IMAGE="+newImage)
		if env != readFile(t, filepath.Join(dir, ".env.example")) {
			t.Fatalf(".env diverged from .env.example:\n%s", env)
		}
	})
}

func TestMissingRequiredOptions(t *testing.T) {
	if _, err := Preview(Options{Name: "missing-image"}); err == nil || !strings.Contains(err.Error(), "--image is required") {
		t.Fatalf("expected image error, got %v", err)
	}
	if _, err := Preview(Options{Name: "external", Template: TemplateExternalGitHub, Image: "example.azurecr.io/ext:test"}); err == nil || !strings.Contains(err.Error(), "--upstream is required") {
		t.Fatalf("expected upstream error, got %v", err)
	}
}

func renderForConfigure(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := Render(Options{Name: "shell-test", OutputDir: dir, Image: "example.azurecr.io/shell:test"}); err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	return dir
}

func runConfigure(t *testing.T, dir, image string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(dir, "scripts", "configure.sh"), "--image", image)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure.sh failed: %v\n%s", err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func assertNotContains(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Fatalf("unexpected %q in:\n%s", want, got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func previewContains(files []RenderedFile, want string) bool {
	for _, file := range files {
		if file.Path == want {
			return true
		}
	}
	return false
}

func assertPreviewContains(t *testing.T, files []RenderedFile, path, want string) {
	t.Helper()
	for _, file := range files {
		if file.Path == path {
			assertContains(t, file.Content, want)
			return
		}
	}
	t.Fatalf("preview missing %s", path)
}
