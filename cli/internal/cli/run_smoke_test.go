// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/onboarding"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type fakeBuiltinSmokeRunner struct {
	options onboarding.SmokeOptions
	result  onboarding.SmokeResult
}

func (f *fakeBuiltinSmokeRunner) Run(_ context.Context, options onboarding.SmokeOptions) (onboarding.SmokeResult, error) {
	f.options = options
	return f.result, nil
}

func TestExecuteBuiltinSmokeUsesVerifiedWorkspaceConnection(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:      "sample",
		ContextName:    "aks-flex",
		KubeconfigPath: t.TempDir() + "/kubeconfig",
		ServiceAccount: "tau-workload",
	}}
	smoke := &fakeBuiltinSmokeRunner{result: onboarding.SmokeResult{
		RunID: "smoke-1234", Phase: "Succeeded",
	}}
	command := &cobra.Command{}
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&bytes.Buffer{})
	command.SetContext(context.Background())

	err := executeBuiltinSmoke(command, builtinSmokeCLIOptions{
		Connection: runConnectionSource{StartDir: "/repo"},
		ConnectionFactory: func(*cobra.Command) runConnectionEnsurer {
			return ensurer
		},
		WorkspaceFetcher: func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
			return tauworkspace.Workspace{
				Metadata: tauworkspace.ObjectMeta{Name: "sample", Generation: 5},
				Spec: tauworkspace.WorkspaceSpec{
					Queue:            "jobqueue",
					Target:           tauworkspace.WorkspaceTarget{Namespace: "sample"},
					Defaults:         tauworkspace.WorkspaceDefaults{OutputRoot: "/data/workspaces/sample"},
					WorkloadIdentity: &tauworkspace.WorkspaceWorkloadIdentity{ServiceAccountName: "tau-workload"},
				},
				Status: tauworkspace.WorkspaceStatus{
					Phase:              "Ready",
					ObservedGeneration: 5,
					Target:             tauworkspace.WorkspaceTargetStatus{ResolvedNamespace: "sample"},
					Queue:              tauworkspace.WorkspaceQueueStatus{LocalQueue: "jobqueue"},
				},
			}, nil
		},
		SmokeRunner: smoke,
	})
	if err != nil {
		t.Fatalf("executeBuiltinSmoke: %v", err)
	}
	if smoke.options.Namespace != "sample" || smoke.options.Queue != "jobqueue" || smoke.options.ServiceAccount != "tau-workload" || smoke.options.Workspace != "sample" || smoke.options.ResultScope != "/data/workspaces/sample" {
		t.Fatalf("smoke options = %#v", smoke.options)
	}
	for _, want := range []string{"Run: smoke-1234", "Workspace: sample", "Phase: Succeeded", "Platform reachable:"} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("smoke output missing %q:\n%s", want, out.String())
		}
	}
}

func TestExecuteBuiltinSmokeClientDryRunIsOffline(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{err: context.Canceled}
	smoke := &fakeBuiltinSmokeRunner{result: onboarding.SmokeResult{
		Phase: "DryRun", Manifest: []byte("kind: Job\n"),
	}}
	command := &cobra.Command{}
	var out, stderr bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetContext(context.Background())

	err := executeBuiltinSmoke(command, builtinSmokeCLIOptions{
		DryRun:     "client",
		Connection: runConnectionSource{StartDir: t.TempDir()},
		ConnectionFactory: func(*cobra.Command) runConnectionEnsurer {
			return ensurer
		},
		WorkspaceDiscoverer: func(*cobra.Command, string) (tauworkspace.Workspace, error) {
			t.Fatal("client dry-run must not discover a live workspace")
			return tauworkspace.Workspace{}, nil
		},
		WorkspaceFetcher: func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
			t.Fatal("client dry-run must not fetch a live workspace")
			return tauworkspace.Workspace{}, nil
		},
		SmokeRunner: smoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("client dry-run activated the connection %d times", ensurer.calls)
	}
	if smoke.options.Namespace != clientDryRunNamespacePlaceholder ||
		smoke.options.Queue != clientDryRunQueuePlaceholder ||
		smoke.options.Workspace != clientDryRunWorkspacePlaceholder ||
		smoke.options.ServiceAccount != clientDryRunServiceAccountPlaceholder {
		t.Fatalf("smoke options = %#v", smoke.options)
	}
	if out.String() != "kind: Job\n" || !bytes.Contains(stderr.Bytes(), []byte("does not contact the cluster")) {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

// v0 clusters activate exactly one workspace, so `tau run smoke` with no
// --workspace and no descriptor workspace must resolve it from the cluster
// rather than refusing to run. This is the researcher-facing half of the
// "the user should never have to know the workspace name" contract.
func TestExecuteBuiltinSmokeDiscoversPrimaryWorkspaceWhenUnset(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName:    "aks-flex",
		KubeconfigPath: t.TempDir() + "/kubeconfig",
	}}
	smoke := &fakeBuiltinSmokeRunner{result: onboarding.SmokeResult{
		RunID: "smoke-auto", Phase: "Succeeded",
	}}
	command := &cobra.Command{}
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&bytes.Buffer{})
	command.SetContext(context.Background())

	discovered := false
	err := executeBuiltinSmoke(command, builtinSmokeCLIOptions{
		Connection: runConnectionSource{StartDir: "/repo"},
		ConnectionFactory: func(*cobra.Command) runConnectionEnsurer {
			return ensurer
		},
		WorkspaceDiscoverer: func(*cobra.Command, string) (tauworkspace.Workspace, error) {
			discovered = true
			return tauworkspace.Workspace{
				Metadata: tauworkspace.ObjectMeta{Name: "auto"},
			}, nil
		},
		WorkspaceFetcher: func(_ *cobra.Command, _, _, name string) (tauworkspace.Workspace, error) {
			if name != "auto" {
				t.Fatalf("fetched workspace %q, want the discovered one", name)
			}
			return tauworkspace.Workspace{
				Metadata: tauworkspace.ObjectMeta{Name: "auto", Generation: 1},
				Spec: tauworkspace.WorkspaceSpec{
					Queue:  "jobqueue",
					Target: tauworkspace.WorkspaceTarget{Namespace: "auto"},
				},
				Status: tauworkspace.WorkspaceStatus{
					Phase:              "Ready",
					ObservedGeneration: 1,
					Target:             tauworkspace.WorkspaceTargetStatus{ResolvedNamespace: "auto"},
					Queue:              tauworkspace.WorkspaceQueueStatus{LocalQueue: "jobqueue"},
				},
			}, nil
		},
		SmokeRunner: smoke,
	})
	if err != nil {
		t.Fatalf("executeBuiltinSmoke: %v", err)
	}
	if !discovered {
		t.Fatal("expected smoke to discover the primary workspace when none was supplied")
	}
	if smoke.options.Workspace != "auto" || smoke.options.Namespace != "auto" {
		t.Fatalf("smoke options = %#v", smoke.options)
	}
}

// An explicit --workspace must still win, so operators keep an escape hatch on
// clusters where discovery is not what they want.
func TestExecuteBuiltinSmokeDoesNotDiscoverWhenWorkspaceSupplied(t *testing.T) {
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName:    "aks-flex",
		KubeconfigPath: t.TempDir() + "/kubeconfig",
	}}
	smoke := &fakeBuiltinSmokeRunner{result: onboarding.SmokeResult{
		RunID: "smoke-explicit", Phase: "Succeeded",
	}}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(context.Background())

	err := executeBuiltinSmoke(command, builtinSmokeCLIOptions{
		Workspace:  "explicit",
		Connection: runConnectionSource{StartDir: "/repo"},
		ConnectionFactory: func(*cobra.Command) runConnectionEnsurer {
			return ensurer
		},
		WorkspaceDiscoverer: func(*cobra.Command, string) (tauworkspace.Workspace, error) {
			t.Fatal("discovery must not run when --workspace was supplied")
			return tauworkspace.Workspace{}, nil
		},
		WorkspaceFetcher: func(_ *cobra.Command, _, _, name string) (tauworkspace.Workspace, error) {
			if name != "explicit" {
				t.Fatalf("fetched %q, want explicit", name)
			}
			return tauworkspace.Workspace{
				Metadata: tauworkspace.ObjectMeta{Name: "explicit", Generation: 1},
				Spec: tauworkspace.WorkspaceSpec{
					Queue:  "jobqueue",
					Target: tauworkspace.WorkspaceTarget{Namespace: "explicit"},
				},
				Status: tauworkspace.WorkspaceStatus{
					Phase:              "Ready",
					ObservedGeneration: 1,
					Target:             tauworkspace.WorkspaceTargetStatus{ResolvedNamespace: "explicit"},
					Queue:              tauworkspace.WorkspaceQueueStatus{LocalQueue: "jobqueue"},
				},
			}, nil
		},
		SmokeRunner: smoke,
	})
	if err != nil {
		t.Fatalf("executeBuiltinSmoke: %v", err)
	}
	if smoke.options.Workspace != "explicit" {
		t.Fatalf("smoke options = %#v", smoke.options)
	}
}
