// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"strings"
	"testing"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

func withWorkspaceCreateCLIRunner(t *testing.T, runner tauworkspace.AdoptRunner) {
	t.Helper()
	previous := newWorkspaceCreateRunner
	newWorkspaceCreateRunner = func(string) tauworkspace.AdoptRunner { return runner }
	t.Cleanup(func() { newWorkspaceCreateRunner = previous })
}

func executeWorkspaceCreate(t *testing.T, runner *workspaceAdoptCLIRunner, args ...string) string {
	t.Helper()
	withWorkspaceCreateCLIRunner(t, runner)
	cmd := newWorkspaceCreateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workspace create failed: %v", err)
	}
	return out.String()
}

func newWorkspaceCreateCLIRunner() *workspaceAdoptCLIRunner {
	return &workspaceAdoptCLIRunner{responses: map[string]string{
		"get clusterqueue.kueue.x-k8s.io jobqueue -o json": `{
			"metadata":{"name":"jobqueue","uid":"cq-uid"}
		}`,
		"-n tau-platform get workspace.tau.azure.com -o json": `{"items":[]}`,
	}}
}

func TestWorkspaceCreateIsWired(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"workspace", "create"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Use != "create [NAME]" {
		t.Fatalf("workspace create command not wired: %#v", cmd)
	}
	for _, name := range []string{
		"namespace", "platform-namespace", "queue", "principal-provider",
		"principal-name", "subject-kind", "subject-name", "output-root",
		"priority", "service-account", "workload-identity-client-id",
		"context", "apply",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("workspace create missing --%s", name)
		}
	}
}

func TestWorkspaceCreatePreviewDoesNotMutate(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	out := executeWorkspaceCreate(t, runner, "research", "--principal-name", "researchers")
	for _, want := range []string{
		"# preflight passed:",
		"kind: TauWorkspace",
		"mode: workspace-rbac",
		"createNamespace: true",
		"queue: jobqueue",
		"outputRoot: /data/projects/research/runs",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview missing %q:\n%s", want, out)
		}
	}
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if strings.Contains(args, " create ") || strings.Contains(args, " apply ") {
			t.Fatalf("preview mutated the cluster: %v", call.args)
		}
	}
}

// The workspace is created either way, so a subject nobody asserts has to be
// visible: an operator who never sees it would read the workspace as already
// granting researchers access.
func TestWorkspaceCreateReportsAnInertSubject(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(), "research")
	if !strings.Contains(rendered, `RBAC subject defaulted to Group "research"`) {
		t.Fatalf("inert subject was not reported:\n%s", rendered)
	}
	// Nothing exists before --apply, so the fix is a rerun; naming the edit
	// here would send the operator at an object that returns NotFound.
	if !strings.Contains(rendered, "rerun with --principal-name <group>") {
		t.Fatalf("preview named a remediation that cannot work yet:\n%s", rendered)
	}
	if strings.Contains(rendered, "kubectl edit") {
		t.Fatalf("preview pointed at an object it never created:\n%s", rendered)
	}
	// The default has to reach both the external principal and the RBAC
	// subject, or the rendered subject would be unbindable.
	for _, want := range []string{
		"kubernetesSubject:\n    kind: Group\n    name: research",
		"principalRef:\n    name: research\n    provider: entra",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("manifest missing %q:\n%s", want, rendered)
		}
	}
}

// After --apply the object exists and v0 refuses a second create as
// conflicting intent, so only an edit gets the operator to a real group.
func TestWorkspaceCreateApplyNamesTheEditRemediation(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	createArgs := "-n tau-platform create -f -"
	runner.responses[createArgs+" --dry-run=server"] = "created (server dry run)\n"
	runner.responses[createArgs] = "created\n"

	rendered := executeWorkspaceCreate(t, runner, "research", "--apply")
	if !strings.Contains(rendered, "kubectl edit workspaces.tau.azure.com research -n tau-platform") {
		t.Fatalf("apply did not name the working remediation:\n%s", rendered)
	}
}

// --subject-name alone binds a real group, so the workspace grants real
// access. Claiming it is inert there would be the one wrong moment to be
// wrong: the RoleBinding names kubernetesSubject, never principalRef.
func TestWorkspaceCreateStaysSilentWhenOnlyTheSubjectIsNamed(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"research", "--subject-name", "research-team")
	if !strings.Contains(rendered, "name: research-team") {
		t.Fatalf("explicit subject was not honoured:\n%s", rendered)
	}
	if strings.Contains(rendered, "grants nobody access") {
		t.Fatalf("a workspace bound to a real group was reported as inert:\n%s", rendered)
	}
}

// Naming the group is the normal path and must stay silent, or the notice
// becomes noise operators learn to skip past.
func TestWorkspaceCreateStaysSilentWhenPrincipalIsNamed(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"research", "--principal-name", "researchers")
	if strings.Contains(rendered, "defaulted to") {
		t.Fatalf("explicit principal produced a defaulting notice:\n%s", rendered)
	}
}

// An explicitly-empty --platform-namespace still resolves for the manifest, so
// the notice has to resolve it too or it prints a command ending in a bare -n.
func TestWorkspaceCreateNoticeResolvesTheDefaultPlatformNamespace(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	createArgs := "-n tau-platform create -f -"
	runner.responses[createArgs+" --dry-run=server"] = "created (server dry run)\n"
	runner.responses[createArgs] = "created\n"

	rendered := executeWorkspaceCreate(t, runner, "research", "--platform-namespace", "", "--apply")
	if !strings.Contains(rendered, "-n tau-platform\n") {
		t.Fatalf("notice printed an unrunnable namespace:\n%s", rendered)
	}
}

// executeWorkspaceCreateErr runs the command expecting failure, for the paths
// where refusing is the behaviour under test.
func executeWorkspaceCreateErr(t *testing.T, args ...string) error {
	t.Helper()
	withWorkspaceCreateCLIRunner(t, newWorkspaceCreateCLIRunner())
	cmd := newWorkspaceCreateCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("workspace create %v unexpectedly succeeded", args)
	}
	return err
}

// `--principal-name "$GROUP"` with GROUP unset used to fail loudly. It must not
// now bind Group/<workspace-name> instead, so only an absent flag defaults.
func TestWorkspaceCreateRejectsAnExplicitlyEmptyPrincipal(t *testing.T) {
	err := executeWorkspaceCreateErr(t, "research", "--principal-name", "")
	if !strings.Contains(err.Error(), "--principal-name must not be blank") {
		t.Fatalf("empty --principal-name returned %v, want a blank-value refusal", err)
	}
}

// The fallback is safe only because Entra asserts groups by object ID. A GitHub
// team slug is shaped like a workspace name, so guessing one could grant a real
// team researcher access.
func TestWorkspaceCreateRefusesToGuessAGitHubTeam(t *testing.T) {
	err := executeWorkspaceCreateErr(t, "research", "--principal-provider", "github")
	if !strings.Contains(err.Error(), "--principal-name is required for --principal-provider github") {
		t.Fatalf("github without a principal returned %v, want a refusal", err)
	}
}

// The notice hardcodes "Group", so the default must not survive a subject kind
// it does not describe. Entra asserts Users by UPN and the controller can
// create a ServiceAccount in the workspace namespace, so neither fallback is
// provably inert.
func TestWorkspaceCreateRefusesToGuessANonGroupSubject(t *testing.T) {
	for _, kind := range []string{"User", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			err := executeWorkspaceCreateErr(t, "research", "--subject-kind", kind)
			if !strings.Contains(err.Error(), "--subject-kind "+kind) {
				t.Fatalf("--subject-kind %s returned %v, want a refusal naming the kind", kind, err)
			}
		})
	}
}

// An explicitly-empty --principal-provider still resolves to entra, so the
// principal does default. Reading the raw flag would leave that workspace
// created inert with nothing said about it.
func TestWorkspaceCreateReportsAnInertSubjectWhenTheProviderIsDefaulted(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"research", "--principal-provider", "")
	if !strings.Contains(rendered, "grants nobody access") {
		t.Fatalf("a defaulted provider suppressed the inert-subject notice:\n%s", rendered)
	}
}

// The notice asserts an absolute security property, so it must not survive the
// one name shape that could make the defaulted subject real. Entra asserts
// groups by object ID, and a UUID is a valid workspace name.
func TestWorkspaceCreateRefusesAnObjectIDShapedName(t *testing.T) {
	err := executeWorkspaceCreateErr(t, "f9a2b52f-f481-4ef7-917a-4d0fef04bba3")
	if !strings.Contains(err.Error(), "object-ID-shaped") {
		t.Fatalf("object-ID-shaped name returned %v, want a refusal", err)
	}
}

// Previewing against a workspace that already exists must not recommend the
// rerun: v0 permits one workspace, so `create --principal-name <group>` is
// refused as conflicting intent and only an edit reaches a real group.
func TestWorkspaceCreatePreviewNamesTheEditWhenTheWorkspaceExists(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	runner.responses["-n tau-platform get workspace.tau.azure.com -o json"] = `{"items":[{
		"metadata":{"name":"research","namespace":"tau-platform"},
		"spec":{
			"authorization":{"mode":"workspace-rbac"},
			"principalRef":{"provider":"entra","name":"research"},
			"kubernetesSubject":{"kind":"Group","name":"research"},
			"role":"tau-researcher-v1",
			"target":{"namespace":"research","createNamespace":true},
			"queue":"jobqueue",
			"defaults":{"outputRoot":"/data/projects/research/runs","priority":"normal"}
		}
	}]}`

	rendered := executeWorkspaceCreate(t, runner, "research")
	if !strings.Contains(rendered, "kubectl edit workspaces.tau.azure.com research -n tau-platform") {
		t.Fatalf("preview of an existing workspace did not name the edit:\n%s", rendered)
	}
	if strings.Contains(rendered, "rerun with --principal-name") {
		t.Fatalf("preview recommended a rerun v0 refuses as conflicting intent:\n%s", rendered)
	}
}

func TestWorkspaceCreateApplyCreatesOnlyTauWorkspace(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	createArgs := "-n tau-platform create -f -"
	runner.responses[createArgs+" --dry-run=server"] = "workspace.example.com/research created (server dry run)\n"
	runner.responses[createArgs] = "workspace.example.com/research created\n"

	out := executeWorkspaceCreate(t, runner, "research", "--principal-name", "researchers", "--apply")
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
		if !strings.Contains(args, "--dry-run=server") {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("mutating create calls = %d, want 1", mutations)
	}
}

// A stock TauGrid install must look the same on every cluster, so `tau
// workspace create` with no NAME has to produce the canonical workspace rather
// than refusing. The name stays overridable; what must not vary is the default.
func TestWorkspaceCreateDefaultsTheWorkspaceName(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"--principal-provider", "entra",
		"--principal-name", "research-team",
	)
	for _, want := range []string{
		"name: " + tauworkspace.DefaultWorkspaceName,
		"namespace: " + tauworkspace.DefaultWorkspaceName,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, rendered)
		}
	}
}

// The default must not become load-bearing: an operator naming the workspace
// still wins, which is what keeps "clusters look the same" a default rather
// than a constraint.
func TestWorkspaceCreateExplicitNameOverridesDefault(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"research",
		"--principal-provider", "entra",
		"--principal-name", "research-team",
	)
	if !strings.Contains(rendered, "name: research") {
		t.Fatalf("explicit name not honoured:\n%s", rendered)
	}
	if strings.Contains(rendered, tauworkspace.DefaultWorkspaceName) {
		t.Fatalf("default leaked into an explicitly named workspace:\n%s", rendered)
	}
}

func TestWorkspaceCreateRejectsMultipleNames(t *testing.T) {
	cmd := newWorkspaceCreateCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"research", "other"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
		t.Fatalf("workspace create with multiple names returned %v, want at-most-one argument error", err)
	}
}
