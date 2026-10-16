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
		"-n tau-system get workspace.tau.azure.com -o json": `{"items":[]}`,
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
		"namespace", "system-namespace", "platform-namespace", "queue", "principal-provider",
		"principal-name", "subject-kind", "subject-name", "output-root",
		"priority", "service-account", "workload-identity-client-id",
		"context", "apply",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("workspace create missing --%s", name)
		}
	}
}

func TestWorkspaceCreateWorkloadIdentityFlagHelpRequiresBothValues(t *testing.T) {
	cmd := newWorkspaceCreateCmd()
	for flag, required := range map[string]string{
		"service-account":             "--workload-identity-client-id",
		"workload-identity-client-id": "--service-account",
	} {
		usage := cmd.Flags().Lookup(flag).Usage
		if !strings.Contains(usage, "requires "+required) {
			t.Errorf("--%s help = %q, want required pair %s", flag, usage, required)
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

func TestWorkspaceCreateReportsAnInertSubject(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(), "research")
	if !strings.Contains(rendered, `RBAC subject defaulted to Group "research"`) {
		t.Fatalf("inert subject was not reported:\n%s", rendered)
	}
	if !strings.Contains(rendered, "rerun with --principal-name <group>") {
		t.Fatalf("preview named a remediation that cannot work yet:\n%s", rendered)
	}
	if strings.Contains(rendered, "kubectl edit") {
		t.Fatalf("preview pointed at an object it never created:\n%s", rendered)
	}
	for _, want := range []string{
		"kubernetesSubject:\n    kind: Group\n    name: research",
		"principalRef:\n    name: research\n    provider: entra",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestWorkspaceCreateApplyNamesTheEditRemediation(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	createArgs := "-n tau-system create -f -"
	runner.responses[createArgs+" --dry-run=server"] = "created (server dry run)\n"
	runner.responses[createArgs] = "created\n"

	rendered := executeWorkspaceCreate(t, runner, "research", "--apply")
	if !strings.Contains(rendered, "kubectl edit workspaces.tau.azure.com research -n tau-system") {
		t.Fatalf("apply did not name the working remediation:\n%s", rendered)
	}
}

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

func TestWorkspaceCreateStaysSilentWhenPrincipalIsNamed(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"research", "--principal-name", "researchers")
	if strings.Contains(rendered, "defaulted to") {
		t.Fatalf("explicit principal produced a defaulting notice:\n%s", rendered)
	}
}

func TestWorkspaceCreateNoticeResolvesTheDefaultSystemNamespace(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	createArgs := "-n tau-system create -f -"
	runner.responses[createArgs+" --dry-run=server"] = "created (server dry run)\n"
	runner.responses[createArgs] = "created\n"

	rendered := executeWorkspaceCreate(t, runner, "research", "--system-namespace", "", "--apply")
	if !strings.Contains(rendered, "-n tau-system\n") {
		t.Fatalf("notice printed an unrunnable namespace:\n%s", rendered)
	}
}

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

func TestWorkspaceCreateRejectsAnExplicitlyEmptyPrincipal(t *testing.T) {
	err := executeWorkspaceCreateErr(t, "research", "--principal-name", "")
	if !strings.Contains(err.Error(), "--principal-name must not be blank") {
		t.Fatalf("empty --principal-name returned %v, want a blank-value refusal", err)
	}
}

func TestWorkspaceCreateRefusesToGuessAGitHubTeam(t *testing.T) {
	err := executeWorkspaceCreateErr(t, "research", "--principal-provider", "github")
	if !strings.Contains(err.Error(), "--principal-name is required for --principal-provider github") {
		t.Fatalf("github without a principal returned %v, want a refusal", err)
	}
}

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

func TestWorkspaceCreateReportsAnInertSubjectWhenTheProviderIsDefaulted(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"research", "--principal-provider", "")
	if !strings.Contains(rendered, "grants nobody access") {
		t.Fatalf("a defaulted provider suppressed the inert-subject notice:\n%s", rendered)
	}
}

func TestWorkspaceCreateRefusesAnObjectIDShapedName(t *testing.T) {
	err := executeWorkspaceCreateErr(t, "f9a2b52f-f481-4ef7-917a-4d0fef04bba3")
	if !strings.Contains(err.Error(), "object-ID-shaped") {
		t.Fatalf("object-ID-shaped name returned %v, want a refusal", err)
	}
}

func TestWorkspaceCreatePreviewNamesTheEditWhenTheWorkspaceExists(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	runner.responses["-n tau-system get workspace.tau.azure.com -o json"] = `{"items":[{
		"metadata":{"name":"research","namespace":"tau-system"},
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
	if !strings.Contains(rendered, "kubectl edit workspaces.tau.azure.com research -n tau-system") {
		t.Fatalf("preview of an existing workspace did not name the edit:\n%s", rendered)
	}
	if strings.Contains(rendered, "rerun with --principal-name") {
		t.Fatalf("preview recommended a rerun v0 refuses as conflicting intent:\n%s", rendered)
	}
}

func TestWorkspaceCreateApplyCreatesOnlyTauWorkspace(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	createArgs := "-n tau-system create -f -"
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

func TestWorkspaceCreateDefaultsTheWorkspaceName(t *testing.T) {
	rendered := executeWorkspaceCreate(t, newWorkspaceCreateCLIRunner(),
		"--principal-provider", "entra",
		"--principal-name", "research-team",
	)
	for _, want := range []string{
		"metadata:\n  name: " + tauworkspace.DefaultWorkspaceName + "\n  namespace: tau-system",
		"target:\n    createNamespace: true\n    namespace: " + tauworkspace.DefaultWorkspaceName,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestWorkspaceCreateSupportsSeparateSystemAndWorkloadNamespaces(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	runner.responses["-n custom-system get workspace.tau.azure.com -o json"] = `{"items":[]}`
	rendered := executeWorkspaceCreate(t, runner,
		"research",
		"--system-namespace", "custom-system",
		"--namespace", "research-jobs",
		"--principal-name", "researchers",
	)
	for _, want := range []string{
		"metadata:\n  name: research\n  namespace: custom-system",
		"target:\n    createNamespace: true\n    namespace: research-jobs",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, rendered)
		}
	}
}

func TestWorkspaceCreateSupportsDeprecatedPlatformNamespace(t *testing.T) {
	runner := newWorkspaceCreateCLIRunner()
	runner.responses["-n legacy-system get workspace.tau.azure.com -o json"] = `{"items":[]}`
	rendered := executeWorkspaceCreate(t, runner,
		"research",
		"--platform-namespace", "legacy-system",
		"--principal-name", "researchers",
	)
	if !strings.Contains(rendered, "namespace: legacy-system") {
		t.Fatalf("deprecated --platform-namespace was not honoured:\n%s", rendered)
	}
}

func TestWorkspaceCreateRejectsConflictingSystemNamespaceAliases(t *testing.T) {
	err := executeWorkspaceCreateErr(t,
		"research",
		"--system-namespace", "tau-system",
		"--platform-namespace", "legacy-system",
		"--principal-name", "researchers",
	)
	if !strings.Contains(err.Error(), "conflicts with deprecated --platform-namespace") {
		t.Fatalf("conflicting namespace aliases returned %v", err)
	}
}

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
