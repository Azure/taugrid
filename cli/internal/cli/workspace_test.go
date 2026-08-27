// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/version"
)

func TestRootIncludesWorkspaceStatus(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"workspace", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Use != "status <name>" {
		t.Fatalf("workspace status command not wired: %#v", cmd)
	}
}

func TestWorkspaceReadCommandsKeepDeprecatedNamespaceAlias(t *testing.T) {
	for _, args := range [][]string{
		{"workspace", "list"},
		{"workspace", "status"},
		{"workspace", "check"},
		{"workspace", "quota", "request"},
		{"workspace", "quota", "show"},
	} {
		cmd, _, err := NewRoot().Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd.Flags().Lookup("system-namespace") == nil {
			t.Fatalf("%v missing --system-namespace", args)
		}
		legacy := cmd.Flags().Lookup("namespace")
		if legacy == nil || legacy.Shorthand != "n" {
			t.Fatalf("%v does not preserve deprecated -n/--namespace", args)
		}
	}
}

func TestRootIncludesWorkspaceInitRepo(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"workspace", "init-repo"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Use != "init-repo NAME" {
		t.Fatalf("workspace init-repo command not wired: %#v", cmd)
	}
}

func TestRepoGenExposesBuildVersion(t *testing.T) {
	if got := NewRepoGenRoot().Version; got != version.Version {
		t.Fatalf("tau-gen version = %q, want %q", got, version.Version)
	}
}

func TestWorkspaceInitRepoGeneratesScaffold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	cmd := NewRoot()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"workspace", "init-repo", "cli-demo",
		"--image", "example.azurecr.io/cli-demo:test",
		"--workspace", "ws-a",
		"--azure-subscription-id", "00000000-0000-0000-0000-000000000000",
		"--azure-tenant-id", "11111111-1111-1111-1111-111111111111",
		"--aks-resource-group", "rg-ai",
		"--aks-cluster", "aks-ai",
		"--acr-name", "example",
		"--output", dir,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	for _, rel := range []string{
		"README.md",
		"AGENTS.md",
		".env.example",
		".gitignore",
		"pyproject.toml",
		"tau/smoke.yaml",
		"tau/train.yaml",
		"tau/workspace.connection.yaml",
		"images/train.Dockerfile",
		"scripts/configure.sh",
		"scripts/doctor.sh",
		"scripts/setup-azure.sh",
		"scripts/setup.sh",
		"scripts/smoke.sh",
		"scripts/train.sh",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
	}

	if !strings.Contains(out.String(), "generated Tau Python repo") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	assertWorkspaceOutputInOrder(t, out.String(),
		`source ./.env`,
		`docker build -f images/train.Dockerfile -t "$IMAGE" .`,
		`docker push "$IMAGE"`,
		`./scripts/configure.sh --image "$IMAGE"`,
		`tau run validate --config tau/train.yaml`,
		`tau run train`,
	)

	env := readWorkspaceTestFile(t, filepath.Join(dir, ".env.example"))
	if strings.Contains(env, "TAU_WORKSPACE") || strings.Contains(env, "AKS_CLUSTER_NAME") {
		t.Fatalf(".env.example leaked workspace coordinates:\n%s", env)
	}
	for _, rel := range []string{"tau/smoke.yaml", "tau/train.yaml"} {
		raw := readWorkspaceTestFile(t, filepath.Join(dir, filepath.FromSlash(rel)))
		if strings.Contains(raw, "policy.workspace") || strings.Contains(raw, "workspace: ws-a") {
			t.Fatalf("%s persisted workspace policy:\n%s", rel, raw)
		}
	}
	connection := readWorkspaceTestFile(t, filepath.Join(dir, "tau/workspace.connection.yaml"))
	for _, want := range []string{"mode: workspace-rbac", "requiredRole: tau-researcher-v1"} {
		if !strings.Contains(connection, want) {
			t.Fatalf("workspace connection missing %q:\n%s", want, connection)
		}
	}
	if strings.Contains(connection, "mode: cluster-wide") {
		t.Fatalf("workspace-scoped repository generated a cluster-wide connection:\n%s", connection)
	}
}

func TestWorkspaceInitRepoRequiresImage(t *testing.T) {
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "init-repo", "missing-image"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--image is required") {
		t.Fatalf("expected image error, got %v", err)
	}
}

func TestWorkspaceInitRepoRejectsInvalidPackage(t *testing.T) {
	cmd := NewRoot()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"workspace", "init-repo", "demo",
		"--image", "example.azurecr.io/demo:test",
		"--package", "bad/package",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid Python package name") {
		t.Fatalf("expected invalid package error, got %v", err)
	}
}

func TestRepoGenInitGeneratesScaffold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	cmd := NewRepoGenRoot()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"init", "gen-demo",
		"--image", "example.azurecr.io/gen-demo:test",
		"--workspace", "ws-a",
		"--output", dir,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out.String(), "generated Tau Python repo") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "tau run validate --config tau/train.yaml") ||
		!strings.Contains(out.String(), "# Ask the platform owner to add tau/workspace.connection.yaml before cluster runs.") {
		t.Fatalf("unconnected next steps missing from output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "tau run train") {
		t.Fatalf("unconnected output included cluster runs:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "tau", "smoke.yaml")); err != nil {
		t.Fatalf("expected smoke config: %v", err)
	}
}

func TestWorkspaceQuotaRequestDryRun(t *testing.T) {
	out := executeTauCommand(t, []string{
		"workspace", "quota", "request", "sample",
		"--resource", "h200",
		"--current", "16",
		"--requested", "32",
		"--duration", "14d",
		"--reason", "checkpoint sweep",
	})
	for _, want := range []string{
		"apiVersion: tau.azure.com/v1alpha1",
		"kind: TauQuotaRequest",
		"name: sample-h200-quota-request",
		"namespace: tau-system",
		"workspace: sample",
		"mutationMode: ReportOnly",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("quota request dry-run missing %q:\n%s", want, out)
		}
	}
}

func TestWorkspaceQuotaRequestValidatesBeforeKubectl(t *testing.T) {
	cmd := newWorkspaceQuotaRequestCmd()
	cmd.SetArgs([]string{"sample", "--resource", "h200", "--requested", "0", "--reason", "x"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--requested must be > 0") {
		t.Fatalf("expected requested validation error, got %v", err)
	}
}

func readWorkspaceTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertWorkspaceOutputInOrder(t *testing.T, got string, wants ...string) {
	t.Helper()
	start := 0
	for _, want := range wants {
		index := strings.Index(got[start:], want)
		if index < 0 {
			t.Fatalf("output missing %q in order:\n%s", want, got)
		}
		start += index + len(want)
	}
}
