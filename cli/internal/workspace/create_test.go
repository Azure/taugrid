// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func defaultCreateOptions() CreateOptions {
	return CreateOptions{
		Name:                  "research",
		Namespace:             "research",
		PlatformNamespace:     PlatformNamespace,
		Queue:                 DefaultWorkspaceQueue,
		PrincipalProvider:     "entra",
		PrincipalName:         "researchers",
		KubernetesSubjectKind: "Group",
		KubernetesSubjectName: "researchers",
		OutputRoot:            "/data/projects/research/runs",
		Priority:              "normal",
	}
}

func successfulCreateRunner() *adoptFakeRunner {
	return &adoptFakeRunner{responses: map[string][]adoptFakeResponse{
		"get clusterqueue.kueue.x-k8s.io jobqueue -o json": {{
			out: `{"metadata":{"name":"jobqueue","uid":"cq-uid"}}`,
		}},
		"-n tau-platform get workspace.tau.azure.com -o json": {{
			out: `{"items":[]}`,
		}},
	}}
}

func TestRenderCreationDeclaresResearcherReadyWorkspace(t *testing.T) {
	options := defaultCreateOptions()
	options.ServiceAccountName = "tau-workload"
	options.WorkloadIdentityClientID = "00000000-0000-0000-0000-000000000001"
	raw, err := RenderCreation(options)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"kind: TauWorkspace",
		"name: research",
		"namespace: tau-platform",
		"mode: workspace-rbac",
		"provider: entra",
		"name: researchers",
		"kind: Group",
		"role: tau-researcher-v1",
		"namespace: research",
		"createNamespace: true",
		"queue: jobqueue",
		"outputRoot: /data/projects/research/runs",
		"priority: normal",
		"serviceAccountName: tau-workload",
		"clientId: 00000000-0000-0000-0000-000000000001",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifest missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"LocalQueue", "RoleBinding", "Namespace"} {
		if strings.Contains(out, "kind: "+forbidden) {
			t.Fatalf("manifest unexpectedly creates %s directly:\n%s", forbidden, out)
		}
	}
}

// An operator can stand up an Entra cluster before the researchers' identity
// group exists, so an omitted --principal-name falls back to the workspace
// name rather than blocking. The binding it produces names a group nobody
// asserts, which is inert rather than over-permissive.
func TestRenderCreationDefaultsResearcherIdentityToTheWorkspaceName(t *testing.T) {
	options := defaultCreateOptions()
	options.PrincipalName = ""
	options.KubernetesSubjectName = ""
	options.DefaultPrincipalToName = true
	out, err := RenderCreation(options)
	if err != nil {
		t.Fatalf("omitted principal must not block creation: %v", err)
	}
	// Both the external principal and the RBAC subject must land on the
	// workspace name; a subject left empty would render an unbindable subject.
	for _, want := range []string{
		"kubernetesSubject:\n    kind: Group\n    name: " + options.Name + "\n",
		"principalRef:\n    name: " + options.Name + "\n    provider: entra\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("manifest missing %q:\n%s", want, out)
		}
	}
}

// The fallback is safe only because Entra asserts groups by object ID. A
// GitHub team slug has the same shape as a workspace name, so defaulting there
// could bind a real team; it must fail instead.
func TestRenderCreationRefusesToGuessAGitHubTeam(t *testing.T) {
	options := defaultCreateOptions()
	options.PrincipalProvider = PrincipalProviderGitHub
	options.PrincipalName = ""
	options.KubernetesSubjectName = ""
	options.DefaultPrincipalToName = true
	_, err := RenderCreation(options)
	if err == nil || !strings.Contains(err.Error(), "--principal-name is required for --principal-provider github") {
		t.Fatalf("expected github to refuse the default, got %v", err)
	}
}

// An explicitly-empty --principal-name is a shell variable that did not
// expand. It used to fail loudly; it must not now silently bind a group.
func TestRenderCreationRejectsAnExplicitlyEmptyPrincipal(t *testing.T) {
	options := defaultCreateOptions()
	options.PrincipalName = ""
	options.KubernetesSubjectName = ""
	options.DefaultPrincipalToName = false
	_, err := RenderCreation(options)
	if err == nil || !strings.Contains(err.Error(), "--principal-name must not be blank") {
		t.Fatalf("expected an explicitly-empty principal to fail, got %v", err)
	}
}

// A blank --principal-name is a typo, not an omission, so it must still fail
// rather than silently taking the workspace name.
func TestRenderCreationRejectsBlankResearcherIdentity(t *testing.T) {
	options := defaultCreateOptions()
	options.PrincipalName = "   "
	options.DefaultPrincipalToName = true
	_, err := RenderCreation(options)
	if err == nil || !strings.Contains(err.Error(), "--principal-name must not be blank") {
		t.Fatalf("expected principal validation error, got %v", err)
	}
}

// The "binds nobody" guarantee rests on Entra asserting groups by OBJECT ID,
// which makes an object-ID-shaped workspace name the one string that could
// make the defaulted subject real. A UUID is a valid DNS-1123 subdomain, so
// nothing else stops it from becoming a workspace name.
func TestRenderCreationRefusesToDefaultAnObjectIDShapedName(t *testing.T) {
	options := defaultCreateOptions()
	options.Name = "f9a2b52f-f481-4ef7-917a-4d0fef04bba3"
	options.Namespace = "research"
	options.OutputRoot = "/data/projects/research/runs"
	options.PrincipalName = ""
	options.KubernetesSubjectName = ""
	options.DefaultPrincipalToName = true
	_, err := RenderCreation(options)
	if err == nil || !strings.Contains(err.Error(), "object-ID-shaped") {
		t.Fatalf("expected an object-ID-shaped name to refuse the default, got %v", err)
	}
}

// Naming the group explicitly is always allowed: the operator has stated the
// subject, so there is nothing left to guess.
func TestRenderCreationAllowsAnObjectIDShapedNameWithAnExplicitPrincipal(t *testing.T) {
	options := defaultCreateOptions()
	options.Name = "f9a2b52f-f481-4ef7-917a-4d0fef04bba3"
	options.Namespace = "research"
	options.OutputRoot = "/data/projects/research/runs"
	if _, err := RenderCreation(options); err != nil {
		t.Fatalf("an explicit principal must still be accepted: %v", err)
	}
}

// The guard keys on shape, not on any registry, so it must match the casing and
// reject near-misses rather than any name containing a dash.
func TestEntraObjectIDMatchesOnlyTheClaimShape(t *testing.T) {
	for _, name := range []string{
		"f9a2b52f-f481-4ef7-917a-4d0fef04bba3",
		"F9A2B52F-F481-4EF7-917A-4D0FEF04BBA3",
	} {
		if !entraObjectID.MatchString(name) {
			t.Errorf("%q is object-ID-shaped but was not matched", name)
		}
	}
	for _, name := range []string{
		"research",
		"taugrid-default",
		"my-team-2026",
		// One hex digit short in the first group.
		"f9a2b52-f481-4ef7-917a-4d0fef04bba3",
		// A UUID with a suffix is a different name, not an object ID.
		"f9a2b52f-f481-4ef7-917a-4d0fef04bba3-dev",
	} {
		if entraObjectID.MatchString(name) {
			t.Errorf("%q is an ordinary workspace name but was refused", name)
		}
	}
}

func TestRenderCreationRequiresCompleteWorkloadIdentity(t *testing.T) {
	options := defaultCreateOptions()
	options.ServiceAccountName = "tau-workload"
	_, err := RenderCreation(options)
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("expected workload identity validation error, got %v", err)
	}
}

func TestRenderCreationRejectsNamesThatCannotBeControllerLabels(t *testing.T) {
	options := defaultCreateOptions()
	options.Name = strings.Repeat("a", 64)
	options.Namespace = "research"
	_, err := RenderCreation(options)
	if err == nil || !strings.Contains(err.Error(), "controller label value") {
		t.Fatalf("expected label value validation error, got %v", err)
	}
}

func TestPreflightCreationSuccess(t *testing.T) {
	report, err := PreflightCreation(context.Background(), successfulCreateRunner(), defaultCreateOptions())
	if err != nil {
		t.Fatal(err)
	}
	if report.ClusterQueueUID != "cq-uid" || report.ExistingWorkspaceIntent != "" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if !strings.Contains(report.Summary(), "controller will create the Namespace, researcher RBAC, and LocalQueue") {
		t.Fatalf("unexpected summary: %s", report.Summary())
	}
}

func TestPreflightCreationRefusesMissingOrTerminatingClusterQueue(t *testing.T) {
	tests := []struct {
		name   string
		change func(*adoptFakeRunner)
		want   string
	}{
		{
			name: "missing",
			change: func(r *adoptFakeRunner) {
				r.responses["get clusterqueue.kueue.x-k8s.io jobqueue -o json"] = []adoptFakeResponse{{err: errors.New("NotFound")}}
			},
			want: `read ClusterQueue "jobqueue"`,
		},
		{
			name: "terminating",
			change: func(r *adoptFakeRunner) {
				r.responses["get clusterqueue.kueue.x-k8s.io jobqueue -o json"] = []adoptFakeResponse{{
					out: `{"metadata":{"name":"jobqueue","uid":"cq-uid","deletionTimestamp":"2026-07-20T00:00:00Z"}}`,
				}}
			},
			want: `ClusterQueue "jobqueue" is terminating`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := successfulCreateRunner()
			tt.change(runner)
			_, err := PreflightCreation(context.Background(), runner, defaultCreateOptions())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
		})
	}
}

func TestPreflightCreationEnforcesSingleWorkspace(t *testing.T) {
	runner := successfulCreateRunner()
	runner.responses["-n tau-platform get workspace.tau.azure.com -o json"] = []adoptFakeResponse{{
		out: `{"items":[
			{"metadata":{"name":"first"},"spec":{"queue":"jobqueue"}},
			{"metadata":{"name":"second"},"spec":{"queue":"jobqueue"}}
		]}`,
	}}
	_, err := PreflightCreation(context.Background(), runner, defaultCreateOptions())
	if err == nil || !strings.Contains(err.Error(), "v0 supports one TauWorkspace, but 2 already exist") {
		t.Fatalf("expected singleton refusal, got %v", err)
	}
}

func TestPreflightCreationCompatibleWorkspaceIsNoOp(t *testing.T) {
	runner := successfulCreateRunner()
	runner.responses["-n tau-platform get workspace.tau.azure.com -o json"] = []adoptFakeResponse{{
		out: `{"items":[{
			"metadata":{"name":"research","namespace":"tau-platform"},
			"spec":{
				"authorization":{"mode":"workspace-rbac"},
				"principalRef":{"provider":"entra","name":"researchers"},
				"kubernetesSubject":{"kind":"Group","name":"researchers"},
				"role":"tau-researcher-v1",
				"target":{"namespace":"research","createNamespace":true},
				"queue":"jobqueue",
				"defaults":{"outputRoot":"/data/projects/research/runs","priority":"normal"}
			}
		}]}`,
	}}
	report, err := PreflightCreation(context.Background(), runner, defaultCreateOptions())
	if err != nil {
		t.Fatal(err)
	}
	if report.ExistingWorkspaceIntent != "compatible" || report.ExistingWorkspaceName != "research" {
		t.Fatalf("unexpected report: %#v", report)
	}
	out, err := ApplyCreation(context.Background(), runner, defaultCreateOptions(), report, []byte("manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no changes made") {
		t.Fatalf("unexpected no-op output: %s", out)
	}
}

func TestApplyCreationRechecksAndConditionallyCreatesWorkspaceOnly(t *testing.T) {
	runner := successfulCreateRunner()
	runner.responses["get clusterqueue.kueue.x-k8s.io jobqueue -o json"] = []adoptFakeResponse{
		{out: `{"metadata":{"name":"jobqueue","uid":"cq-uid"}}`},
		{out: `{"metadata":{"name":"jobqueue","uid":"cq-uid"}}`},
	}
	runner.responses["-n tau-platform get workspace.tau.azure.com -o json"] = []adoptFakeResponse{
		{out: `{"items":[]}`},
		{out: `{"items":[]}`},
	}
	createArgs := "-n tau-platform create -f -"
	runner.responses[createArgs+" --dry-run=server"] = []adoptFakeResponse{{out: "dry run passed\n"}}
	runner.responses[createArgs] = []adoptFakeResponse{{out: "workspace.example.com/research created\n"}}

	options := defaultCreateOptions()
	manifest, err := RenderCreation(options)
	if err != nil {
		t.Fatal(err)
	}
	report, err := PreflightCreation(context.Background(), runner, options)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyCreation(context.Background(), runner, options, report, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "created") {
		t.Fatalf("unexpected output: %s", out)
	}
	var mutations int
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if !strings.Contains(args, " create ") {
			continue
		}
		if !strings.Contains(call.stdin, "kind: TauWorkspace") {
			t.Fatalf("create input was not a TauWorkspace:\n%s", call.stdin)
		}
		if !strings.Contains(args, "--dry-run=server") {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("mutating create calls = %d, want 1", mutations)
	}
}
