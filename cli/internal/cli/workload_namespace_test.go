// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

func withWorkloadNamespaceDiscoverer(t *testing.T, fn workspaceDiscoverer) {
	t.Helper()
	previous := workloadNamespaceDiscoverer
	workloadNamespaceDiscoverer = fn
	t.Cleanup(func() { workloadNamespaceDiscoverer = previous })
}

func TestResolveWorkloadNamespaceDiscoversWhenUnset(t *testing.T) {
	called := 0
	withWorkloadNamespaceDiscoverer(t, func(*cobra.Command, string) (tauworkspace.Workspace, error) {
		called++
		ws := tauworkspace.Workspace{}
		ws.Metadata.Name = "taugrid-default"
		ws.Status.Target.ResolvedNamespace = "taugrid-default"
		return ws, nil
	})

	got, err := resolveWorkloadNamespace(&cobra.Command{}, "some-context", "")
	if err != nil {
		t.Fatalf("resolveWorkloadNamespace: %v", err)
	}
	if got != "taugrid-default" {
		t.Fatalf("namespace = %q, want taugrid-default", got)
	}
	if called != 1 {
		t.Fatalf("discoverer called %d times, want 1", called)
	}
}

func TestResolveWorkloadNamespaceExplicitWins(t *testing.T) {
	withWorkloadNamespaceDiscoverer(t, func(*cobra.Command, string) (tauworkspace.Workspace, error) {
		t.Fatal("discovery must not run when a namespace was already resolved")
		return tauworkspace.Workspace{}, nil
	})

	got, err := resolveWorkloadNamespace(&cobra.Command{}, "some-context", "  team-a  ")
	if err != nil {
		t.Fatalf("resolveWorkloadNamespace: %v", err)
	}
	if got != "team-a" {
		t.Fatalf("namespace = %q, want team-a", got)
	}
}

func TestResolveWorkloadNamespaceFallsBackToSpecThenName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() tauworkspace.Workspace
		want  string
	}{
		{
			name: "spec target when status is empty",
			build: func() tauworkspace.Workspace {
				ws := tauworkspace.Workspace{}
				ws.Metadata.Name = "taugrid-default"
				ws.Spec.Target.Namespace = "from-spec"
				return ws
			},
			want: "from-spec",
		},
		{
			name: "workspace name when neither is set",
			build: func() tauworkspace.Workspace {
				ws := tauworkspace.Workspace{}
				ws.Metadata.Name = "taugrid-default"
				return ws
			},
			want: "taugrid-default",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withWorkloadNamespaceDiscoverer(t, func(*cobra.Command, string) (tauworkspace.Workspace, error) {
				return tc.build(), nil
			})
			got, err := resolveWorkloadNamespace(&cobra.Command{}, "ctx", "")
			if err != nil {
				t.Fatalf("resolveWorkloadNamespace: %v", err)
			}
			if got != tc.want {
				t.Fatalf("namespace = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveWorkloadNamespaceReportsGuidanceOnDiscoveryFailure(t *testing.T) {
	withWorkloadNamespaceDiscoverer(t, func(*cobra.Command, string) (tauworkspace.Workspace, error) {
		return tauworkspace.Workspace{}, errors.New("connection refused")
	})

	_, err := resolveWorkloadNamespace(&cobra.Command{}, "ctx", "")
	if err == nil {
		t.Fatal("expected an error when discovery fails and no namespace was given")
	}
	_, want := requireWorkloadNamespace("")
	if err.Error() != want.Error() {
		t.Fatalf("error = %q, want the requireWorkloadNamespace guidance %q", err, want)
	}
}
