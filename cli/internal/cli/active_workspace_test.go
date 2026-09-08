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

func TestActiveWorkspaceResolverStrictTargetUsesConnectionUnlessWorkspaceExplicit(t *testing.T) {
	connection := workspaceconnection.ActiveConnection{
		Workspace:      "connected",
		WorkspaceUID:   "workspace-uid",
		ContextName:    "connected-context",
		KubeconfigPath: filepath.Join(t.TempDir(), "kubeconfig"),
		Namespace:      "connected-namespace",
		Queue:          "connected-queue",
	}
	if err := os.WriteFile(connection.KubeconfigPath, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := newActiveWorkspaceResolver(
		func(*cobra.Command) runConnectionEnsurer {
			return &fakeRunConnectionEnsurer{connection: connection}
		},
		func(_ *cobra.Command, context, _, name string) (tauworkspace.Workspace, error) {
			if context != connection.ContextName || name != connection.Workspace {
				t.Fatalf("fetch context=%q workspace=%q", context, name)
			}
			workspace := readyWorkspace()
			workspace.Metadata.Name = connection.Workspace
			workspace.Metadata.UID = connection.WorkspaceUID
			workspace.Status.Target.ResolvedNamespace = connection.Namespace
			workspace.Status.Queue.LocalQueue = connection.Queue
			return workspace, nil
		},
	)

	resolution, err := resolver.Resolve(&cobra.Command{}, activeWorkspaceRequest{
		Workspace:               "inherited",
		RequireRepositoryTarget: true,
	})
	if err != nil {
		t.Fatalf("inherited workspace blocked strict resolution: %v", err)
	}
	resolution.Restore()
	if resolution.Placement.Workspace != connection.Workspace {
		t.Fatalf("workspace = %q, want %q", resolution.Placement.Workspace, connection.Workspace)
	}

	_, err = resolver.Resolve(&cobra.Command{}, activeWorkspaceRequest{
		Workspace:               "explicit",
		WorkspaceExplicit:       true,
		RequireRepositoryTarget: true,
	})
	if err == nil || !strings.Contains(err.Error(), `workspace "explicit" conflicts with active repository workspace connection "connected"`) {
		t.Fatalf("explicit workspace conflict = %v", err)
	}
}
