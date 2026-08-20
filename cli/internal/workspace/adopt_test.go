// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

type adoptFakeResponse struct {
	out string
	err error
}

type adoptFakeCall struct {
	args  []string
	stdin string
}

type adoptFakeRunner struct {
	responses map[string][]adoptFakeResponse
	calls     []adoptFakeCall
}

func (f *adoptFakeRunner) Raw(_ context.Context, args []string, stdin []byte) (string, error) {
	f.calls = append(f.calls, adoptFakeCall{args: append([]string(nil), args...), stdin: string(stdin)})
	key := strings.Join(args, " ")
	responses, ok := f.responses[key]
	if !ok || len(responses) == 0 {
		return "", errors.New("unexpected kubectl call: " + key)
	}
	response := responses[0]
	if len(responses) > 1 {
		f.responses[key] = responses[1:]
	}
	return response.out, response.err
}

func successfulAdoptRunner() *adoptFakeRunner {
	return &adoptFakeRunner{responses: map[string][]adoptFakeResponse{
		"-n tau-system get workspace.tau.azure.com -o json": {{
			out: `{"items":[]}`,
		}},
		"get namespace sample -o json": {{
			out: `{"metadata":{"name":"sample","uid":"ns-uid","labels":{"kueue.x-k8s.io/default-local-queue":"jobqueue"}}}`,
		}},
		"-n sample get localqueue.kueue.x-k8s.io jobqueue -o json": {{
			out: `{"metadata":{"name":"jobqueue","uid":"queue-uid"},"spec":{"clusterQueue":"shared-cq"}}`,
		}},
		"get clusterqueue.kueue.x-k8s.io shared-cq -o json": {{
			out: `{"metadata":{"name":"shared-cq","uid":"cq-uid"}}`,
		}},
		"-n sample get persistentvolumeclaim blob-training -o json": {{
			out: `{"metadata":{"name":"blob-training","uid":"pvc-uid"},"spec":{"storageClassName":"azureblob-fuse-premium"},"status":{"phase":"Bound"}}`,
		}},
		"get storageclass.storage.k8s.io azureblob-fuse-premium -o json": {{
			out: `{"metadata":{"name":"azureblob-fuse-premium","uid":"storage-class-uid"}}`,
		}},
		"-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json": {{
			out: "",
		}},
	}}
}

func defaultAdoptOptions() AdoptOptions {
	return AdoptOptions{
		Name:            "sample",
		Namespace:       "sample",
		Queue:           "jobqueue",
		SystemNamespace: "tau-system",
		DataPVC:         "blob-training",
		OutputRoot:      "/data/projects/sample/runs",
	}
}

func TestRenderAdoptionManifest(t *testing.T) {
	options := defaultAdoptOptions()
	options.Priority = "default"
	raw, err := RenderAdoption(options)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"apiVersion: tau.azure.com/v1alpha1",
		"kind: TauWorkspace",
		"name: sample",
		"namespace: tau-system",
		"mode: cluster-wide",
		"namespace: sample",
		"createNamespace: false",
		"queue: jobqueue",
		"outputRoot: /data/projects/sample/runs",
		"priority: default",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifest missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"LocalQueue", "PersistentVolumeClaim", "StorageClass", "ClusterQueue", "RoleBinding", "Secret"} {
		if strings.Contains(out, "kind: "+forbidden) {
			t.Fatalf("manifest unexpectedly contains %s:\n%s", forbidden, out)
		}
	}
}

func TestRenderAdoptionRejectsUnknownPriority(t *testing.T) {
	options := defaultAdoptOptions()
	options.Priority = "urgent"
	_, err := RenderAdoption(options)
	if err == nil || !strings.Contains(err.Error(), "--priority must be one of") {
		t.Fatalf("expected priority validation error, got %v", err)
	}
}

func TestRenderAdoptionRejectsNamesThatCannotBeControllerLabels(t *testing.T) {
	options := defaultAdoptOptions()
	options.Name = strings.Repeat("a", 64)
	_, err := RenderAdoption(options)
	if err == nil || !strings.Contains(err.Error(), "controller label value") {
		t.Fatalf("expected label value validation error, got %v", err)
	}
}

func TestPreflightAdoptionSuccess(t *testing.T) {
	runner := successfulAdoptRunner()
	report, err := PreflightAdoption(context.Background(), runner, defaultAdoptOptions())
	if err != nil {
		t.Fatal(err)
	}
	if report.NamespaceUID != "ns-uid" || report.QueueUID != "queue-uid" ||
		report.PVCUID != "pvc-uid" || report.ResolvedClusterQueue != "shared-cq" ||
		report.ResolvedStorageClass != "azureblob-fuse-premium" ||
		report.StorageClassUID != "storage-class-uid" ||
		report.ExistingWorkspaceIntent != "absent" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if !strings.Contains(report.Summary(), "preflight passed") {
		t.Fatalf("unexpected summary: %s", report.Summary())
	}
}

func TestPreflightAdoptionEnforcesSingleV0Workspace(t *testing.T) {
	runner := successfulAdoptRunner()
	runner.responses["-n tau-system get workspace.tau.azure.com -o json"] = []adoptFakeResponse{{
		out: `{"items":[{"metadata":{"name":"other"},"spec":{"queue":"jobqueue"}}]}`,
	}}
	_, err := PreflightAdoption(context.Background(), runner, defaultAdoptOptions())
	if err == nil || !strings.Contains(err.Error(), `TauWorkspace "other" already exists with different intent`) {
		t.Fatalf("expected singleton refusal, got %v", err)
	}
}

func TestPreflightAdoptionRefusals(t *testing.T) {
	tests := []struct {
		name    string
		options func(*AdoptOptions)
		change  func(*adoptFakeRunner)
		want    string
	}{
		{
			name: "namespace missing",
			change: func(r *adoptFakeRunner) {
				r.responses["get namespace sample -o json"] = []adoptFakeResponse{{err: errors.New("NotFound")}}
			},
			want: `read Namespace "sample"`,
		},
		{
			name: "namespace UID mismatch",
			options: func(o *AdoptOptions) {
				o.NamespaceUID = "expected"
			},
			want: `Namespace "sample" UID is "ns-uid", want exactly "expected"`,
		},
		{
			name: "namespace assigned elsewhere",
			change: func(r *adoptFakeRunner) {
				r.responses["get namespace sample -o json"] = []adoptFakeResponse{{
					out: fmt.Sprintf(`{"metadata":{"name":"sample","uid":"ns-uid","labels":{"%s":"other"}}}`, workloadmeta.LabelWorkspace),
				}}
			},
			want: `assigned to TauWorkspace "other"`,
		},
		{
			name: "namespace queue mismatch",
			change: func(r *adoptFakeRunner) {
				r.responses["get namespace sample -o json"] = []adoptFakeResponse{{
					out: `{"metadata":{"name":"sample","uid":"ns-uid","labels":{"kueue.x-k8s.io/default-local-queue":"other"}}}`,
				}}
			},
			want: `default LocalQueue is "other"`,
		},
		{
			name: "queue missing",
			change: func(r *adoptFakeRunner) {
				r.responses["-n sample get localqueue.kueue.x-k8s.io jobqueue -o json"] = []adoptFakeResponse{{err: errors.New("NotFound")}}
			},
			want: "read LocalQueue sample/jobqueue",
		},
		{
			name: "queue UID mismatch",
			options: func(o *AdoptOptions) {
				o.QueueUID = "expected"
			},
			want: `LocalQueue "sample/jobqueue" UID is "queue-uid", want exactly "expected"`,
		},
		{
			name: "queue has no ClusterQueue",
			change: func(r *adoptFakeRunner) {
				r.responses["-n sample get localqueue.kueue.x-k8s.io jobqueue -o json"] = []adoptFakeResponse{{
					out: `{"metadata":{"name":"jobqueue","uid":"queue-uid"},"spec":{}}`,
				}}
			},
			want: "does not reference a ClusterQueue",
		},
		{
			name: "ClusterQueue expectation mismatch",
			options: func(o *AdoptOptions) {
				o.ClusterQueue = "expected-cq"
			},
			want: `references ClusterQueue "shared-cq", want exactly "expected-cq"`,
		},
		{
			name: "ClusterQueue unreadable",
			change: func(r *adoptFakeRunner) {
				r.responses["get clusterqueue.kueue.x-k8s.io shared-cq -o json"] = []adoptFakeResponse{{err: errors.New("Forbidden")}}
			},
			want: `read ClusterQueue "shared-cq"`,
		},
		{
			name: "PVC missing",
			change: func(r *adoptFakeRunner) {
				r.responses["-n sample get persistentvolumeclaim blob-training -o json"] = []adoptFakeResponse{{err: errors.New("NotFound")}}
			},
			want: "read PVC sample/blob-training",
		},
		{
			name: "PVC UID mismatch",
			options: func(o *AdoptOptions) {
				o.PVCUID = "expected"
			},
			want: `PVC "sample/blob-training" UID is "pvc-uid", want exactly "expected"`,
		},
		{
			name: "PVC not Bound",
			change: func(r *adoptFakeRunner) {
				r.responses["-n sample get persistentvolumeclaim blob-training -o json"] = []adoptFakeResponse{{
					out: `{"metadata":{"name":"blob-training","uid":"pvc-uid"},"spec":{"storageClassName":"azureblob-fuse-premium"},"status":{"phase":"Pending"}}`,
				}}
			},
			want: `phase is "Pending", want exactly "Bound"`,
		},
		{
			name: "StorageClass mismatch",
			options: func(o *AdoptOptions) {
				o.StorageClass = "expected-class"
			},
			want: `storageClassName is "azureblob-fuse-premium", want exactly "expected-class"`,
		},
		{
			name: "StorageClass unreadable",
			change: func(r *adoptFakeRunner) {
				r.responses["get storageclass.storage.k8s.io azureblob-fuse-premium -o json"] = []adoptFakeResponse{{err: errors.New("Forbidden")}}
			},
			want: `read StorageClass "azureblob-fuse-premium"`,
		},
		{
			name: "existing workspace conflict",
			change: func(r *adoptFakeRunner) {
				r.responses["-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json"] = []adoptFakeResponse{{
					out: compatibleWorkspaceJSON("other-queue"),
				}}
			},
			want: "has a conflicting spec",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := successfulAdoptRunner()
			options := defaultAdoptOptions()
			if test.options != nil {
				test.options(&options)
			}
			if test.change != nil {
				test.change(runner)
			}
			_, err := PreflightAdoption(context.Background(), runner, options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestPreflightAdoptionAllowsEmptyDataPVC(t *testing.T) {
	runner := successfulAdoptRunner()
	options := defaultAdoptOptions()
	options.DataPVC = ""
	report, err := PreflightAdoption(context.Background(), runner, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.DataPVC != "" || report.PVCUID != "" {
		t.Fatalf("unexpected PVC report: %#v", report)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.args, " "), "persistentvolumeclaim") {
			t.Fatalf("empty --data-pvc still read a PVC: %v", call.args)
		}
	}
}

func TestPreflightAdoptionRejectsTerminatingDependency(t *testing.T) {
	runner := successfulAdoptRunner()
	runner.responses["get namespace sample -o json"] = []adoptFakeResponse{{
		out: `{"metadata":{"name":"sample","uid":"ns-uid","deletionTimestamp":"2026-07-28T00:00:00Z"}}`,
	}}
	_, err := PreflightAdoption(context.Background(), runner, defaultAdoptOptions())
	if err == nil || !strings.Contains(err.Error(), `Namespace "sample" is terminating`) {
		t.Fatalf("expected terminating Namespace refusal, got %v", err)
	}
}

func compatibleWorkspaceJSON(queue string) string {
	return `{
		"apiVersion":"tau.azure.com/v1alpha1",
		"kind":"TauWorkspace",
		"metadata":{"name":"sample","namespace":"tau-system","uid":"workspace-uid","resourceVersion":"17"},
		"spec":{
			"authorization":{"mode":"cluster-wide"},
			"target":{"namespace":"sample","createNamespace":false},
			"queue":"` + queue + `",
			"defaults":{"outputRoot":"/data/projects/sample/runs"}
		}
	}`
}

func TestPreflightAdoptionAcceptsCompatibleWorkspace(t *testing.T) {
	runner := successfulAdoptRunner()
	runner.responses["-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json"] = []adoptFakeResponse{{
		out: compatibleWorkspaceJSON("jobqueue"),
	}}
	report, err := PreflightAdoption(context.Background(), runner, defaultAdoptOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !report.ExistingWorkspace || report.ExistingWorkspaceIntent != "compatible" ||
		report.ExistingWorkspaceUID != "workspace-uid" || report.ExistingWorkspaceRV != "17" {
		t.Fatalf("unexpected existing workspace report: %#v", report)
	}
}

func TestPreflightAdoptionRejectsTerminatingWorkspace(t *testing.T) {
	runner := successfulAdoptRunner()
	terminating := strings.Replace(
		compatibleWorkspaceJSON("jobqueue"),
		`"resourceVersion":"17"`,
		`"resourceVersion":"17","deletionTimestamp":"2026-07-28T00:00:00Z"`,
		1,
	)
	runner.responses["-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json"] = []adoptFakeResponse{{
		out: terminating,
	}}
	_, err := PreflightAdoption(context.Background(), runner, defaultAdoptOptions())
	if err == nil || !strings.Contains(err.Error(), `TauWorkspace "tau-system/sample" is terminating`) {
		t.Fatalf("expected terminating TauWorkspace refusal, got %v", err)
	}
}

func TestApplyAdoptionCreatesOnlyTauWorkspace(t *testing.T) {
	runner := successfulAdoptRunner()
	options := defaultAdoptOptions()
	initial, err := PreflightAdoption(context.Background(), runner, options)
	if err != nil {
		t.Fatal(err)
	}
	createArgs := "-n tau-system create -f -"
	runner.responses[createArgs+" --dry-run=server"] = []adoptFakeResponse{{out: "dry-run"}}
	runner.responses[createArgs] = []adoptFakeResponse{{out: "workspace.example.com/sample created\n"}}

	out, err := ApplyAdoption(context.Background(), runner, options, initial)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "created") {
		t.Fatalf("unexpected apply output: %q", out)
	}
	var writes int
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if !strings.Contains(args, " create ") {
			continue
		}
		if !strings.Contains(call.stdin, "kind: TauWorkspace") {
			t.Fatalf("create input was not a TauWorkspace:\n%s", call.stdin)
		}
		for _, forbidden := range []string{"kind: Namespace", "kind: LocalQueue", "kind: PersistentVolumeClaim", "kind: StorageClass", "kind: ClusterQueue", "kind: Secret"} {
			if strings.Contains(call.stdin, forbidden) {
				t.Fatalf("create input contains %s:\n%s", forbidden, call.stdin)
			}
		}
		if !strings.Contains(args, "--dry-run=server") {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("real create calls = %d, want 1", writes)
	}
}

func TestApplyAdoptionIsIdempotentForCompatibleWorkspace(t *testing.T) {
	runner := successfulAdoptRunner()
	runner.responses["-n tau-system get workspace.tau.azure.com sample --ignore-not-found -o json"] = []adoptFakeResponse{{
		out: compatibleWorkspaceJSON("jobqueue"),
	}}
	options := defaultAdoptOptions()
	initial, err := PreflightAdoption(context.Background(), runner, options)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyAdoption(context.Background(), runner, options, initial)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "already exists with compatible intent") {
		t.Fatalf("unexpected compatible no-op output: %q", out)
	}
	for _, call := range runner.calls {
		args := strings.Join(call.args, " ")
		if strings.Contains(args, " apply ") || strings.Contains(args, " create ") {
			t.Fatalf("compatible workspace should not be mutated: %v", call.args)
		}
	}
}

func TestApplyAdoptionRefusesReplacedResource(t *testing.T) {
	runner := successfulAdoptRunner()
	options := defaultAdoptOptions()
	initial, err := PreflightAdoption(context.Background(), runner, options)
	if err != nil {
		t.Fatal(err)
	}
	runner.responses["get namespace sample -o json"] = []adoptFakeResponse{{
		out: `{"metadata":{"name":"sample","uid":"replacement","labels":{"kueue.x-k8s.io/default-local-queue":"jobqueue"}}}`,
	}}
	_, err = ApplyAdoption(context.Background(), runner, options, initial)
	if err == nil || !strings.Contains(err.Error(), "Namespace identity changed before apply") {
		t.Fatalf("expected identity refusal, got %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.args, " "), " apply ") {
			t.Fatalf("apply ran after identity mismatch: %v", call.args)
		}
	}
}
