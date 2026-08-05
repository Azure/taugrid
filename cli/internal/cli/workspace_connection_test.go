package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceConnectionInspectDiscoversParentDescriptor(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	path := filepath.Join(root, "tau", "workspace.connection.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`schema: tau.workspace.connection.v1
workspace: sample
cluster:
  provider: azure
  resourceID: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks-flex
  contextName: aks-flex
identity:
  tenantID: 11111111-1111-1111-1111-111111111111
authorization:
  mode: cluster-wide
requirements:
  minTauVersion: 0.3.0
network:
  privateCluster: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := newWorkspaceConnectionInspectCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{nested, "-o", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"workspace": "sample"`, `"contextName": "aks-flex"`, `"digest": "sha256:`, `"connectionKey": "sample-`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("inspection missing %q:\n%s", want, out.String())
		}
	}
}
