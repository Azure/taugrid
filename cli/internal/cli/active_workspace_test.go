// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type cachingRunConnectionEnsurer struct {
	*fakeRunConnectionEnsurer
	configDir string
}

func (e *cachingRunConnectionEnsurer) ConfigDirectory() (string, error) {
	return e.configDir, nil
}

func TestActiveWorkspaceResolverCentralizesConnectionAndPlacement(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/original")
	configDir := t.TempDir()
	repositoryRoot := "/repo"
	discovery := workspaceconnection.Discovery{
		Path:           filepath.Join(repositoryRoot, "tau", "workspace.connection.yaml"),
		RepositoryRoot: repositoryRoot,
		Digest:         "descriptor-digest",
	}
	connection := workspaceconnection.ActiveConnection{
		Workspace:       "sample",
		WorkspaceUID:    "workspace-uid",
		ContextName:     "aks-flex",
		SystemNamespace: "custom-system",
		KubeconfigPath:  "/tmp/workspace-kubeconfig",
		Namespace:       "sample",
		Queue:           "jobqueue",
	}
	ensurer := &cachingRunConnectionEnsurer{
		fakeRunConnectionEnsurer: &fakeRunConnectionEnsurer{connection: connection},
		configDir:                configDir,
	}
	fetched := false
	resolver := newActiveWorkspaceResolver(
		func(*cobra.Command) runConnectionEnsurer { return ensurer },
		func(_ *cobra.Command, context, systemNamespace, name string) (tauworkspace.Workspace, error) {
			fetched = true
			if context != "aks-flex" || systemNamespace != "custom-system" || name != "sample" {
				t.Fatalf("fetch context=%q systemNamespace=%q name=%q", context, systemNamespace, name)
			}
			if got := os.Getenv("KUBECONFIG"); got != connection.KubeconfigPath {
				t.Fatalf("KUBECONFIG during fetch = %q", got)
			}
			workspace := readyWorkspace()
			workspace.Metadata.UID = connection.WorkspaceUID
			workspace.Spec.Queue = connection.Queue
			workspace.Status.Queue.LocalQueue = connection.Queue
			workspace.Status.Queue.ClusterQueue = "gpu-cq"
			return workspace, nil
		},
	)
	resolvedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.FixedZone("test", -7*60*60))
	resolver.now = func() time.Time { return resolvedAt }

	resolution, err := resolver.Resolve(&cobra.Command{}, activeWorkspaceRequest{
		Source:                  runConnectionSource{Discovery: &discovery},
		Namespace:               "sample",
		Queue:                   "jobqueue",
		RequireRepositoryTarget: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fetched || ensurer.calls != 1 || !resolution.Connected {
		t.Fatalf("fetched=%t connection calls=%d resolution=%+v", fetched, ensurer.calls, resolution)
	}
	if resolution.Context != "aks-flex" ||
		resolution.Placement.Workspace != "sample" ||
		resolution.Placement.Namespace != "sample" ||
		resolution.Placement.LocalQueue != "jobqueue" ||
		resolution.Placement.ClusterQueue != "gpu-cq" {
		t.Fatalf("resolution = %+v", resolution)
	}
	resolution.Restore()
	if got := os.Getenv("KUBECONFIG"); got != "/tmp/original" {
		t.Fatalf("KUBECONFIG after restore = %q", got)
	}
	raw, err := os.ReadFile(filepath.Join(configDir, activeWorkspaceCacheFilename))
	if err != nil {
		t.Fatal(err)
	}
	var cache activeWorkspaceCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	if cache.Schema != activeWorkspaceCacheSchema ||
		cache.Workspace != "sample" ||
		cache.WorkspaceUID != "workspace-uid" ||
		cache.ContextName != "aks-flex" ||
		cache.SystemNamespace != "custom-system" ||
		cache.Namespace != "sample" ||
		cache.LocalQueue != "jobqueue" ||
		cache.ClusterQueue != "gpu-cq" ||
		cache.RepositoryRoot != repositoryRoot ||
		cache.DescriptorPath != discovery.Path ||
		cache.DescriptorDigest != "descriptor-digest" ||
		!cache.ResolvedAt.Equal(resolvedAt) {
		t.Fatalf("cache = %+v", cache)
	}

	blockedConfigDir := filepath.Join(configDir, "not-a-directory")
	if err := os.WriteFile(blockedConfigDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	ensurer.configDir = blockedConfigDir
	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)
	resolution, err = resolver.Resolve(cmd, activeWorkspaceRequest{
		Source:                  runConnectionSource{Discovery: &discovery},
		RequireRepositoryTarget: true,
	})
	if err != nil {
		t.Fatalf("non-authoritative cache failure blocked resolution: %v", err)
	}
	resolution.Restore()
	if !strings.Contains(stderr.String(), "warning: could not update active workspace cache") {
		t.Fatalf("cache warning missing from stderr: %q", stderr.String())
	}
}

func TestActiveWorkspaceResolverPreservesExplicitUnconnectedRun(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{err: workspaceconnection.ErrDescriptorNotFound}
	resolver := newActiveWorkspaceResolver(
		func(*cobra.Command) runConnectionEnsurer { return ensurer },
		func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
			t.Fatal("explicit unconnected run must not fetch a TauWorkspace")
			return tauworkspace.Workspace{}, nil
		},
	)

	resolution, err := resolver.Resolve(&cobra.Command{}, activeWorkspaceRequest{
		Workspace:           "operator-workspace",
		WorkspaceExplicit:   true,
		KubeContext:         "operator-context",
		KubeContextExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Connected || ensurer.calls != 0 || resolution.Context != "operator-context" {
		t.Fatalf("connection calls=%d resolution=%+v", ensurer.calls, resolution)
	}
}
