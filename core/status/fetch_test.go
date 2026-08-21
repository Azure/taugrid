// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestHydrateAdmissionCheckControllers_DedupesAndLeavesLookupFailuresUnknown(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	scriptPath := filepath.Join(dir, "kubectl")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> "%s"
if [ "$1" = "get" ] && [ "$2" = "admissioncheck" ]; then
  case "$3" in
    deny)
      echo "forbidden" >&2
      exit 1
      ;;
    generic)
      echo '{"spec":{"controllerName":"kueue.x-k8s.io/provisioning"}}'
      exit 0
      ;;
    mk)
      echo '{"spec":{"controllerName":"kueue.x-k8s.io/multikueue"}}'
      exit 0
      ;;
  esac
fi
echo "unexpected args: $@" >&2
exit 1
`, logPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub kubectl: %v", err)
	}

	workloads := []Workload{
		{
			Name: "a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "mk"},
				{Name: "mk"},
				{Name: "deny"},
			},
		},
		{
			Name: "b",
			AdmissionChecks: []AdmissionCheck{
				{Name: "generic"},
			},
		},
	}
	hydrateAdmissionCheckControllers(context.Background(), &kube.Runner{Path: scriptPath}, workloads)

	for i, check := range workloads[0].AdmissionChecks[:2] {
		if check.ControllerName != multiKueueControllerName {
			t.Fatalf("mk check %d controller = %q, want %q", i, check.ControllerName, multiKueueControllerName)
		}
		if check.ControllerLookupFailed {
			t.Fatalf("mk check %d unexpectedly marked lookup-failed", i)
		}
	}
	if got := workloads[0].AdmissionChecks[2].ControllerName; got != "" {
		t.Fatalf("lookup failure should leave controller unknown, got %q", got)
	}
	if !workloads[0].AdmissionChecks[2].ControllerLookupFailed {
		t.Fatal("lookup failure should be recorded for fallback and rendering")
	}
	if got := workloads[1].AdmissionChecks[0].ControllerName; got != "kueue.x-k8s.io/provisioning" {
		t.Fatalf("generic controller = %q, want provisioning controller", got)
	}
	if workloads[1].AdmissionChecks[0].ControllerLookupFailed {
		t.Fatal("successful generic lookup must not be marked failed")
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub kubectl log: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(logData)))
	getCalls := 0
	for _, token := range lines {
		if token == "get" {
			getCalls++
		}
	}
	if getCalls != 3 {
		t.Fatalf("expected 3 deduped admissioncheck lookups, got %d (%q)", getCalls, string(logData))
	}
}

func TestFetchManagerCleanup_UnionsAllWorkloadSelectorsAndDedupesNames(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n ray get job train-001 -o json":
			return `{"metadata":{"uid":"job-uid"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"}}`, nil
		case "-n ray get rayjob train-001 -o json":
			return `{"metadata":{"uid":"ray-uid","name":"train-001"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"}}`, nil
		case "-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json":
			return `{"items":[{"metadata":{"name":"wl-a"}}]}`, nil
		case "-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json":
			return `{"items":[{"metadata":{"name":"wl-b"}}]}`, nil
		case "-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json":
			return `{"items":[{"metadata":{"name":"wl-b"}}]}`, nil
		default:
			if strings.HasPrefix(strings.Join(args, " "), "get admissioncheck ") {
				return "", errors.New("forbidden")
			}
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	snap, err := FetchManagerCleanup(context.Background(), runner, "ray", "train-001")
	if err != nil {
		t.Fatalf("FetchManagerCleanup() error = %v", err)
	}
	got := capturedWorkloadNamesForTest(snap.Workloads)
	want := []string{"wl-a", "wl-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected workloads %v, got %v", want, got)
	}
}

func TestFetchRunLogs_UsesOnlyMinimalReadSurfaceAcrossRoutes(t *testing.T) {
	type response struct {
		out string
		err error
	}
	tests := []struct {
		name             string
		responses        map[string]response
		wantCalls        []string
		wantJobFound     bool
		wantRayJobFound  bool
		wantIsMultiKueue bool
		wantState        MultiKueueState
		wantWorker       string
	}{
		{
			name: "local-job",
			responses: map[string]response{
				"-n ray get job train-001 -o json":                                                       {out: `{"metadata":{"uid":"job-uid"}}`},
				"-n ray get rayjob train-001 -o json":                                                    {err: errors.New(`rayjobs.ray.io "train-001" not found`)},
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json": {out: `{"items":[]}`},
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json":          {out: `{"items":[]}`},
			},
			wantCalls: []string{
				"-n ray get job train-001 -o json",
				"-n ray get rayjob train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
			},
			wantJobFound:     true,
			wantRayJobFound:  false,
			wantIsMultiKueue: false,
		},
		{
			name: "local-rayjob",
			responses: map[string]response{
				"-n ray get job train-001 -o json":                                                       {err: errors.New(`jobs.batch "train-001" not found`)},
				"-n ray get rayjob train-001 -o json":                                                    {out: `{"metadata":{"uid":"ray-uid","name":"train-001"},"spec":{"managedBy":"ray.io/kuberay-operator"},"status":{"rayClusterName":"train-001-raycluster"}}`},
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json": {out: `{"items":[]}`},
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json":          {out: `{"items":[]}`},
			},
			wantCalls: []string{
				"-n ray get job train-001 -o json",
				"-n ray get rayjob train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json",
			},
			wantJobFound:     false,
			wantRayJobFound:  true,
			wantIsMultiKueue: false,
		},
		{
			name: "manager-pending",
			responses: map[string]response{
				"-n ray get job train-001 -o json":                                                       {err: errors.New(`jobs.batch "train-001" not found`)},
				"-n ray get rayjob train-001 -o json":                                                    {out: `{"metadata":{"uid":"ray-uid","name":"train-001"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"},"status":{"rayClusterName":"train-001-raycluster"}}`},
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json": {out: `{"items":[{"metadata":{"name":"wl-a"},"status":{"admissionChecks":[{"name":"multikueue","state":"Pending"}]}}]}`},
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json":          {out: `{"items":[]}`},
			},
			wantCalls: []string{
				"-n ray get job train-001 -o json",
				"-n ray get rayjob train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json",
			},
			wantJobFound:     false,
			wantRayJobFound:  true,
			wantIsMultiKueue: true,
			wantState:        MultiKueueStatePending,
		},
		{
			name: "manager-selected",
			responses: map[string]response{
				"-n ray get job train-001 -o json":                                                       {err: errors.New(`jobs.batch "train-001" not found`)},
				"-n ray get rayjob train-001 -o json":                                                    {out: `{"metadata":{"uid":"ray-uid","name":"train-001"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"},"status":{"rayClusterName":"train-001-raycluster"}}`},
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json": {out: `{"items":[{"metadata":{"name":"wl-a"},"status":{"clusterName":"worker-a","admissionChecks":[{"name":"multikueue","state":"Selected"}]}}]}`},
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json":          {out: `{"items":[]}`},
			},
			wantCalls: []string{
				"-n ray get job train-001 -o json",
				"-n ray get rayjob train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
				"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json",
			},
			wantJobFound:     false,
			wantRayJobFound:  true,
			wantIsMultiKueue: true,
			wantState:        MultiKueueStateSelected,
			wantWorker:       "worker-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
				call := strings.Join(args, " ")
				calls = append(calls, call)
				for _, forbidden := range []string{" get pods ", " get events", " get resourceclaims", "get admissioncheck ", " get nodes ", " get node "} {
					if strings.Contains(" "+call+" ", forbidden) {
						t.Fatalf("FetchRunLogs must not query %q, got call %q", strings.TrimSpace(forbidden), call)
					}
				}
				resp, ok := tt.responses[call]
				if !ok {
					t.Fatalf("unexpected kubectl args: %v", args)
				}
				return resp.out, resp.err
			})

			snap, err := fetchRunLogs(context.Background(), runner, "ray", "train-001")
			if err != nil {
				t.Fatalf("fetchRunLogs() error = %v", err)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("unexpected kubectl calls:\nwant: %v\ngot:  %v", tt.wantCalls, calls)
			}
			if snap.JobFound != tt.wantJobFound {
				t.Fatalf("JobFound = %v, want %v", snap.JobFound, tt.wantJobFound)
			}
			if snap.RayJob.Found != tt.wantRayJobFound {
				t.Fatalf("RayJob.Found = %v, want %v", snap.RayJob.Found, tt.wantRayJobFound)
			}
			if got := snap.IsMultiKueue(); got != tt.wantIsMultiKueue {
				t.Fatalf("IsMultiKueue() = %v, want %v", got, tt.wantIsMultiKueue)
			}
			if got := snap.MultiKueueState(); got != tt.wantState {
				t.Fatalf("MultiKueueState() = %q, want %q", got, tt.wantState)
			}
			if got := snap.PlacementWorkerCluster(); got != tt.wantWorker {
				t.Fatalf("PlacementWorkerCluster() = %q, want %q", got, tt.wantWorker)
			}
			if tt.wantState == MultiKueueStatePending && len(snap.Workloads) > 0 {
				check := snap.Workloads[0].AdmissionChecks[0]
				if check.Name != multiKueueAdmissionCheckName || !check.ControllerLookupFailed {
					t.Fatalf("expected exact-name multikueue fallback on pending manager snapshot, got %+v", check)
				}
			}
		})
	}
}

func TestFetchRunLogs_SurfacesWorkloadErrorsWithPartialManagerSnapshot(t *testing.T) {
	runner := statusRawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n ray get job train-001 -o json":
			return "", errors.New(`jobs.batch "train-001" not found`)
		case "-n ray get rayjob train-001 -o json":
			return `{"metadata":{"uid":"ray-uid","name":"train-001"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"},"status":{"rayClusterName":"train-001-raycluster"}}`, nil
		case "-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json":
			return "", errors.New("forbidden: workloads")
		case "-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json":
			return "", errors.New("forbidden: workloads")
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	snap, err := fetchRunLogs(context.Background(), runner, "ray", "train-001")
	if err == nil || !strings.Contains(err.Error(), "list Kueue Workloads for ray/train-001 while resolving run logs placement") {
		t.Fatalf("expected actionable workload error, got %v", err)
	}
	if !snap.IsMultiKueue() {
		t.Fatalf("expected partial snapshot to preserve MultiKueue identity, got %+v", snap)
	}
}

type statusRawRunnerFunc func(context.Context, []string, []byte) (string, error)

func (f statusRawRunnerFunc) Raw(ctx context.Context, extraArgs []string, stdin []byte) (string, error) {
	return f(ctx, extraArgs, stdin)
}

func capturedWorkloadNamesForTest(workloads []Workload) []string {
	seen := make(map[string]bool, len(workloads))
	names := make([]string, 0, len(workloads))
	for _, workload := range workloads {
		if workload.Name == "" || seen[workload.Name] {
			continue
		}
		seen[workload.Name] = true
		names = append(names, workload.Name)
	}
	return names
}

func TestHydrateWorkloads_DropsStaleMessageOnceAdmitted(t *testing.T) {
	data := []byte(`{"items":[{"metadata":{"name":"w-1"},"spec":{"queueName":"jobqueue"},"status":{"conditions":[
		{"type":"QuotaReserved","status":"False","reason":"Pending","message":"stale flavor failure"},
		{"type":"Admitted","status":"True","reason":"Admitted","message":""}
	]}}]}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("hydrateWorkloads returned %d workloads, want 1", len(got))
	}
	if !got[0].Admitted {
		t.Fatalf("expected workload to be admitted, got %+v", got[0])
	}
	if got[0].Message != "" {
		t.Errorf("Message = %q, want empty once admitted", got[0].Message)
	}
}

// Kueue emits only QuotaReserved while a workload waits for flavor
// assignment — there is no Admitted condition to fall back on. Observed on
// the `ai` cluster with a workload that could not fit any node.
func TestHydrateWorkloads_QuotaReservedOnlyPopulatesReasonAndMessage(t *testing.T) {
	data := []byte(`{"items":[{"metadata":{"name":"job-bug13-repro-b5426"},"spec":{"queueName":"jobqueue"},"status":{"conditions":[
		{"type":"QuotaReserved","status":"False","reason":"Pending","message":"couldn't assign flavors to pod set main: topology \"default-node-topology\" doesn't allow to fit any of 1 pod(s). Total nodes: 4; excluded: resource \"cpu\": 3, taint \"nvidia.com/gpu:NoSchedule\": 1"}
	]}}]}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("hydrateWorkloads returned %d workloads, want 1", len(got))
	}
	if got[0].Reason != "Pending" {
		t.Errorf("Reason = %q, want %q", got[0].Reason, "Pending")
	}
	if !strings.Contains(got[0].Message, "excluded: resource \"cpu\": 3") {
		t.Errorf("Message missing node-exclusion detail, got %q", got[0].Message)
	}
}

func TestHydrateWorkloads_FinishedWorkloadKeepsTerminalReason(t *testing.T) {
	data := []byte(`{"items":[{"metadata":{"name":"w-1"},"spec":{"queueName":"jobqueue"},"status":{"conditions":[
		{"type":"QuotaReserved","status":"False","reason":"Pending","message":"stale"},
		{"type":"Finished","status":"True","reason":"Succeeded","message":"job finished"}
	]}}]}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("hydrateWorkloads returned %d workloads, want 1", len(got))
	}
	if got[0].Reason != "Succeeded" {
		t.Errorf("Reason = %q, want the terminal reason %q", got[0].Reason, "Succeeded")
	}
	if got[0].Message != "" {
		t.Errorf("Message = %q, want empty for a finished workload", got[0].Message)
	}
}
