// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

type workspaceAdoptCLICall struct {
	args  []string
	stdin string
}

type workspaceAdoptCLIRunner struct {
	responses map[string]string
	calls     []workspaceAdoptCLICall
}

func (f *workspaceAdoptCLIRunner) Raw(_ context.Context, args []string, stdin []byte) (string, error) {
	f.calls = append(f.calls, workspaceAdoptCLICall{args: append([]string(nil), args...), stdin: string(stdin)})
	key := strings.Join(args, " ")
	out, ok := f.responses[key]
	if !ok {
		return "", errors.New("unexpected kubectl call: " + key)
	}
	return out, nil
}

func newWorkspaceAdoptCLIRunner() *workspaceAdoptCLIRunner {
	return &workspaceAdoptCLIRunner{responses: map[string]string{
		"-n tau-system get workspace.tau.azure.com -o json": `{"items":[]}`,
		"get namespace sample -o json": `{
			"metadata":{"name":"sample","uid":"ns-uid","labels":{"kueue.x-k8s.io/default-local-queue":"jobqueue"}}
		}`,
		"-n sample get localqueue.kueue.x-k8s.io jobqueue -o json": `{
			"metadata":{"name":"jobqueue","uid":"queue-uid"},"spec":{"clusterQueue":"shared-cq"}
		}`,
		"get clusterqueue.kueue.x-k8s.io shared-cq -o json": `{
			"metadata":{"name":"shared-cq","uid":"cq-uid"}
		}`,
		"-n sample get persistentvolumeclaim blob-training -o json": `{
			"metadata":{"name":"blob-training","uid":"pvc-uid"},
			"spec":{"storageClassName":"azureblob-fuse-premium"},"status":{"phase":"Bound"}
		}`,
		"get storageclass.storage.k8s.io azureblob-fuse-premium -o json": `{
			"metadata":{"name":"azureblob-fuse-premium","uid":"storage-class-uid"}
		}`,
		"-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json": "",
	}}
}

func withWorkspaceAdoptCLIRunner(t *testing.T, runner tauworkspace.AdoptRunner) {
	t.Helper()
	previous := newWorkspaceAdoptRunner
	newWorkspaceAdoptRunner = func(string) tauworkspace.AdoptRunner { return runner }
	t.Cleanup(func() { newWorkspaceAdoptRunner = previous })
}

func executeWorkspaceAdopt(t *testing.T, runner *workspaceAdoptCLIRunner, args ...string) string {
	t.Helper()
	withWorkspaceAdoptCLIRunner(t, runner)
	cmd := newWorkspaceAdoptCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workspace adopt failed: %v", err)
	}
	return out.String()
}

func TestWorkspaceAdoptIsWired(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"workspace", "adopt"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Use != "adopt NAME" {
		t.Fatalf("workspace adopt command not wired: %#v", cmd)
	}
	for _, name := range []string{
		"namespace", "queue", "system-namespace", "platform-namespace", "context", "data-pvc",
		"namespace-uid", "queue-uid", "pvc-uid", "storage-class",
		"cluster-queue", "output-root", "priority", "apply",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("workspace adopt missing --%s", name)
		}
	}
}

func TestWorkspaceAdoptPreviewDoesNotMutate(t *testing.T) {
	runner := newWorkspaceAdoptCLIRunner()
	out := executeWorkspaceAdopt(t, runner, "sample")
	for _, want := range []string{
		"# preflight passed:",
		"kind: TauWorkspace",
		"namespace: sample",
		"createNamespace: false",
		"mode: cluster-wide",
		"queue: jobqueue",
		"outputRoot: /data/projects/sample/runs",
		"tau.azure.com/adoption-namespace-uid: ns-uid",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview missing %q:\n%s", want, out)
		}
	}
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if strings.Contains(args, " apply ") || strings.Contains(args, " create ") {
			t.Fatalf("preview mutated the cluster: %v", call.args)
		}
	}
}

func TestWorkspaceAdoptApplyOnlyTauWorkspace(t *testing.T) {
	runner := newWorkspaceAdoptCLIRunner()
	createArgs := "-n tau-system create -f -"
	runner.responses[createArgs+" --dry-run=server"] = "workspace.example.com/sample created (server dry run)\n"
	runner.responses[createArgs] = "workspace.example.com/sample created\n"

	out := executeWorkspaceAdopt(t, runner, "sample", "--apply")
	if !strings.Contains(out, "preflight passed:") || !strings.Contains(out, "created") {
		t.Fatalf("unexpected apply output:\n%s", out)
	}
	var mutations int
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if !strings.Contains(args, " create ") {
			continue
		}
		if !strings.Contains(call.stdin, "kind: TauWorkspace") {
			t.Fatalf("non-workspace create input:\n%s", call.stdin)
		}
		if !strings.Contains(call.stdin, "tau.azure.com/adoption-namespace-uid: ns-uid") {
			t.Fatalf("TauWorkspace create is missing Namespace UID adoption evidence:\n%s", call.stdin)
		}
		if !strings.Contains(args, "--dry-run=server") {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("mutating create calls = %d, want 1", mutations)
	}
}

func TestWorkspaceAdoptApplyRepairsCompatibleWorkspaceEvidence(t *testing.T) {
	runner := newWorkspaceAdoptCLIRunner()
	existingWorkspace := `{
		"apiVersion":"tau.azure.com/v1alpha1",
		"kind":"TauWorkspace",
		"metadata":{"name":"sample","namespace":"tau-system","uid":"workspace-uid","resourceVersion":"17"},
		"spec":{
			"authorization":{"mode":"cluster-wide"},
			"target":{"namespace":"sample","createNamespace":false},
			"queue":"jobqueue",
			"defaults":{"outputRoot":"/data/projects/sample/runs"}
		}
	}`
	runner.responses["-n tau-system get workspace.tau.azure.com -o json"] = `{"items":[` + existingWorkspace + `]}`
	runner.responses["-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json"] = existingWorkspace
	patchArgs := `-n tau-system patch workspace.tau.azure.com sample --type=merge -p {"metadata":{"annotations":{"tau.azure.com/adoption-namespace-uid":"ns-uid"},"resourceVersion":"17"}}`
	runner.responses[patchArgs] = "workspace.tau.azure.com/sample patched\n"

	out := executeWorkspaceAdopt(t, runner, "sample", "--apply")
	if !strings.Contains(out, "preflight passed:") || !strings.Contains(out, "patched") {
		t.Fatalf("unexpected retry output:\n%s", out)
	}
	var patches int
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if args == patchArgs {
			patches++
		}
		if strings.Contains(args, " create ") {
			t.Fatalf("compatible workspace was recreated: %v", call.args)
		}
	}
	if patches != 1 {
		t.Fatalf("adoption evidence patch calls = %d, want 1", patches)
	}
}
