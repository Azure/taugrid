// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const validDescriptorYAML = `schema: tau.workspace.connection.v1
workspace: sample
cluster:
  contextName: taugrid-flex
  systemNamespace: tau-system
access:
  method: aks
  aks:
    resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-ai/providers/Microsoft.ContainerService/managedClusters/taugrid-flex
    tenantID: 11111111-1111-1111-1111-111111111111
authorization:
  mode: cluster-wide
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: true
  instructions: Connect to network access or VPN.
`

func TestDiscoverWalksParents(t *testing.T) {
	root := t.TempDir()
	initDescriptorGitRepo(t, root)
	path := filepath.Join(root, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Discover(nested)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.Path != path || got.RepositoryRoot != root {
		t.Fatalf("discovery = %#v, want path %s root %s", got, path, root)
	}
	if got.Descriptor.Workspace != "sample" || got.Digest == "" {
		t.Fatalf("descriptor = %#v", got)
	}
}

func TestDescriptorDefaultsSystemNamespace(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "  systemNamespace: tau-system\n", "", 1)
	descriptor, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := descriptor.ResolvedSystemNamespace(); got != "tau-system" {
		t.Fatalf("ResolvedSystemNamespace() = %q, want tau-system", got)
	}
}

func TestParseKubeconfigAccess(t *testing.T) {
	raw := `schema: tau.workspace.connection.v1
workspace: sample
cluster:
  contextName: local-cluster
access:
  method: kubeconfig
authorization:
  mode: cluster-wide
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
`
	descriptor, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if descriptor.AccessIdentity() != "kubeconfig:local-cluster" || descriptor.Access.AKS != nil {
		t.Fatalf("kubeconfig descriptor = %#v", descriptor)
	}
}

func TestParseRejectsAccessMetadataForWrongMethod(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "method: aks", "method: kubeconfig", 1)
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "must be omitted") {
		t.Fatalf("Parse() error = %v, want access.aks rejection", err)
	}
}

func TestDescriptorResolvesConfiguredSystemNamespace(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "  systemNamespace: tau-system\n", "  systemNamespace: custom-system\n", 1)
	descriptor, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := descriptor.ResolvedSystemNamespace(); got != "custom-system" {
		t.Fatalf("ResolvedSystemNamespace() = %q, want custom-system", got)
	}
}

func TestDescriptorRejectsInvalidSystemNamespace(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "  systemNamespace: tau-system\n", "  systemNamespace: Invalid_Namespace\n", 1)
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "cluster.systemNamespace") {
		t.Fatalf("Parse() error = %v, want invalid cluster.systemNamespace", err)
	}
}

func TestDiscoverStopsAtGitRoot(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(parent, "repo")
	initDescriptorGitRepo(t, repo)
	nested := filepath.Join(repo, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(nested); !errors.Is(err, ErrDescriptorNotFound) {
		t.Fatalf("parent descriptor crossed Git root: %v", err)
	}
}

func TestDiscoverNoGitDoesNotWalkParents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(nested); !errors.Is(err, ErrDescriptorNotFound) {
		t.Fatalf("no-Git discovery inherited parent descriptor: %v", err)
	}
}

func TestDiscoverFromSymlinkedDirectoryStaysWithinGitRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	root := t.TempDir()
	initDescriptorGitRepo(t, root)
	descriptorPath := filepath.Join(root, DescriptorRelativePath)
	if err := os.MkdirAll(filepath.Dir(descriptorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(descriptorPath, []byte(validDescriptorYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "project", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(deep, "config.yaml")
	if err := os.WriteFile(config, []byte("name: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shortcut := filepath.Join(root, "shortcut")
	if err := os.Symlink(deep, shortcut); err != nil {
		t.Fatal(err)
	}
	discovery, err := Discover(filepath.Join(shortcut, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Path != descriptorPath || discovery.RepositoryRoot != root {
		t.Fatalf("discovery escaped symlinked worktree boundary: %#v", discovery)
	}
}

func TestDiscoverReturnsTypedNotFound(t *testing.T) {
	_, err := Discover(t.TempDir())
	if !errors.Is(err, ErrDescriptorNotFound) {
		t.Fatalf("expected ErrDescriptorNotFound, got %v", err)
	}
}

func TestParseRejectsUnknownOrSecretFields(t *testing.T) {
	_, err := Parse([]byte(validDescriptorYAML + "token: secret\n"))
	if err == nil || !strings.Contains(err.Error(), "field token not found") {
		t.Fatalf("expected strict unknown-field error, got %v", err)
	}
}

func TestParseRejectsNonAKSResource(t *testing.T) {
	raw := strings.Replace(
		validDescriptorYAML,
		"Microsoft.ContainerService/managedClusters/taugrid-flex",
		"Microsoft.Storage/storageAccounts/notaks",
		1,
	)
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "AKS managed cluster") {
		t.Fatalf("expected AKS resource error, got %v", err)
	}
}

func TestParseRejectsInvalidWorkspaceName(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "workspace: sample", "workspace: Not Valid", 1)
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "workspace") || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected workspace-name error, got %v", err)
	}
}

func TestParseWorkspaceRBACRequiresRole(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "mode: cluster-wide", "mode: workspace-rbac", 1)
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "requiredRole") {
		t.Fatalf("expected workspace-rbac role error, got %v", err)
	}
}

func TestParseRejectsUnknownAuthorizationMode(t *testing.T) {
	raw := strings.Replace(validDescriptorYAML, "mode: cluster-wide", "mode: magic", 1)
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "authorization.mode") {
		t.Fatalf("expected authorization mode error, got %v", err)
	}
}

func TestParseClusterWideRejectsRequiredRole(t *testing.T) {
	raw := strings.Replace(
		validDescriptorYAML,
		"mode: cluster-wide",
		"mode: cluster-wide\n  requiredRole: tau-researcher-v1",
		1,
	)
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "must be omitted") {
		t.Fatalf("expected misleading role error, got %v", err)
	}
}

func TestDigestIgnoresYAMLFormatting(t *testing.T) {
	first, err := Parse([]byte(validDescriptorYAML))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(strings.ReplaceAll(validDescriptorYAML, "  ", "    ")))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := Digest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := Digest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("digest changed with formatting: %s != %s", firstDigest, secondDigest)
	}
}

func TestCheckTauVersion(t *testing.T) {
	if err := CheckTauVersion("v0.4.0", "0.3.0"); err != nil {
		t.Fatalf("newer version rejected: %v", err)
	}
	if err := CheckTauVersion("v0.1.3-edge.8ecf230c", "0.1.2"); err != nil {
		t.Fatalf("edge version with the current release base rejected: %v", err)
	}
	if err := CheckTauVersion("dev", "99.0.0"); err != nil {
		t.Fatalf("development build rejected: %v", err)
	}
	for _, tc := range []struct {
		current string
		minimum string
	}{
		{current: "0.2.9", minimum: "0.3.0"},
		{current: "v0.0.0-edge.8ecf230c", minimum: "0.1.2"},
	} {
		err := CheckTauVersion(tc.current, tc.minimum)
		if err == nil || !strings.Contains(err.Error(), "too old") {
			t.Fatalf("expected old-version error for %s, got %v", tc.current, err)
		}
	}
}

func initDescriptorGitRepo(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", root, "init", "--quiet")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
}
