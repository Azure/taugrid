// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/status"
)

func TestWriteRunStatusJSONExposesStableAgentContract(t *testing.T) {
	var out bytes.Buffer
	snap := status.Snapshot{
		Name:       "train-001",
		Namespace:  "ray",
		JobFound:   true,
		JobActive:  1,
		JobUID:     "job-uid",
		Workloads:  []status.Workload{{Name: "train-001", Queue: "research", Admitted: true, Phase: "Admitted"}},
		Pods:       []status.Pod{{Name: "train-001-pod", Phase: "Running", Ready: "1/1", Containers: []status.Container{{Name: "trainer", State: "running", Ready: true}}}},
		GPURuntime: status.GPURuntimeEvidence{State: status.GPURuntimeObserved, Source: "dcgm-exporter", NodesExpected: 1, NodesScraped: 1},
	}
	if err := writeRunStatusJSON(&out, snap, true, "research", "/tmp/kubeconfig"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Status     struct {
			Found        bool   `json:"found"`
			WorkloadKind string `json:"workloadKind"`
			State        string `json:"state"`
		} `json:"status"`
		Phases []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"phases"`
		Pods []struct {
			Name       string `json:"name"`
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"pods"`
		Metrics struct {
			GPURuntime struct {
				State string `json:"state"`
			} `json:"gpuRuntime"`
		} `json:"metrics"`
		Diagnostics []runStatusDiagnostic `json:"diagnostics"`
		Actions     []runStatusAction     `json:"actions"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, out.String())
	}
	if got.APIVersion != runStatusAPIVersion || got.Kind != "RunStatus" {
		t.Fatalf("type metadata = %q %q", got.APIVersion, got.Kind)
	}
	if !got.Status.Found || got.Status.WorkloadKind != "Job" || got.Status.State != "running" {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(got.Phases) == 0 || got.Phases[0].Name != "Submitted" || got.Phases[0].Status != "done" {
		t.Fatalf("phases = %+v", got.Phases)
	}
	if len(got.Pods) != 1 || len(got.Pods[0].Containers) != 1 || got.Pods[0].Containers[0].Name != "trainer" {
		t.Fatalf("pods = %+v", got.Pods)
	}
	if got.Metrics.GPURuntime.State != "observed" {
		t.Fatalf("GPU metrics = %+v", got.Metrics.GPURuntime)
	}
	if len(got.Diagnostics) == 0 || got.Diagnostics[len(got.Diagnostics)-1].Code != "DEEP_DIAGNOSTICS" {
		t.Fatalf("diagnostics = %+v", got.Diagnostics)
	}
	if len(got.Actions) != 2 || got.Actions[0].Name != "watch" || got.Actions[1].Name != "logs" {
		t.Fatalf("actions = %+v", got.Actions)
	}
	for _, action := range got.Actions {
		if action.Command.Env["KUBECONFIG"] != "/tmp/kubeconfig" ||
			!strings.HasPrefix(action.Command.Shell, "KUBECONFIG='/tmp/kubeconfig' ") {
			t.Fatalf("action lost activated kubeconfig: %+v", action)
		}
	}
}

func TestWriteRunStatusJSONSuggestsRunListWhenMissing(t *testing.T) {
	var out bytes.Buffer
	if err := writeRunStatusJSON(&out, status.Snapshot{Name: "missing", Namespace: "ray"}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	var got runStatusDocument
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Found || got.Status.State != status.LifecycleNotFound {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != "RUN_NOT_FOUND" {
		t.Fatalf("diagnostics = %+v", got.Diagnostics)
	}
	if len(got.Diagnostics[0].Commands) != 1 ||
		!strings.Contains(got.Diagnostics[0].Commands[0].Shell, "'tau' 'run' 'list'") ||
		!strings.Contains(got.Diagnostics[0].Commands[0].Shell, "'--output' 'json'") ||
		strings.Join(got.Diagnostics[0].Commands[0].Argv, " ") != "tau run list --output json --namespace ray" {
		t.Fatalf("recovery commands = %#v", got.Diagnostics[0].Commands)
	}
	if got.Workloads == nil || got.Pods == nil || got.ResourceClaims == nil || got.Events == nil {
		t.Fatalf("collection fields must encode as arrays: %+v", got)
	}
	if got.Metrics != nil {
		t.Fatalf("metrics should be omitted without --run-profile: %+v", got.Metrics)
	}
	if len(got.Actions) != 1 || got.Actions[0].Name != "list" ||
		!strings.Contains(got.Actions[0].Command.Shell, "'--output' 'json'") ||
		strings.Join(got.Actions[0].Command.Argv, " ") != "tau run list --output json --namespace ray" {
		t.Fatalf("actions = %+v", got.Actions)
	}
	if strings.Contains(out.String(), "0001-01-01") {
		t.Fatalf("unavailable timestamps must be omitted:\n%s", out.String())
	}
}

func TestWriteRunStatusJSONIncludesAvailableJobTimestamps(t *testing.T) {
	created := time.Date(2026, time.April, 27, 10, 0, 0, 0, time.UTC)
	started := created.Add(2 * time.Minute)
	finished := started.Add(30 * time.Minute)
	doc := newRunStatusDocument(status.Snapshot{
		Name:          "train-001",
		Namespace:     "ray",
		JobFound:      true,
		JobCreatedAt:  created,
		JobStartedAt:  started,
		JobFinishedAt: finished,
	}, false, "", "")
	var raw bytes.Buffer
	if err := writeRunStatusJSONDocument(&raw, doc); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Job struct {
			CreatedAt  time.Time `json:"createdAt"`
			StartedAt  time.Time `json:"startedAt"`
			FinishedAt time.Time `json:"finishedAt"`
		} `json:"job"`
	}
	if err := json.Unmarshal(raw.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Job.CreatedAt.Equal(created) || !got.Job.StartedAt.Equal(started) || !got.Job.FinishedAt.Equal(finished) {
		t.Fatalf("job timestamps = %+v", got.Job)
	}

	zeroDoc := newRunStatusDocument(status.Snapshot{JobFound: true}, false, "", "")
	raw.Reset()
	if err := writeRunStatusJSONDocument(&raw, zeroDoc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.String(), `"createdAt"`) ||
		strings.Contains(raw.String(), `"startedAt"`) ||
		strings.Contains(raw.String(), `"finishedAt"`) {
		t.Fatalf("zero job timestamps must be omitted:\n%s", raw.String())
	}
}

func TestWriteRunStatusHumanUsesDecisionOrientedSections(t *testing.T) {
	snap := status.Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 1,
		Workloads: []status.Workload{{
			Name: "train-001", Queue: "research", Admitted: true, Phase: "Admitted",
		}},
		Pods: []status.Pod{{
			Name: "train-001-pod", Phase: "Running", Ready: "1/1", Node: "gpu-01",
		}},
		GPURuntime: status.GPURuntimeEvidence{
			State:         status.GPURuntimeObserved,
			NodesExpected: 1,
			NodesScraped:  1,
			Devices: []status.GPUDeviceEvidence{{
				GPU: "0", UtilizationPercent: 92, UtilizationObserved: true,
			}},
		},
	}
	doc := newRunStatusDocument(snap, true, "research", "/tmp/kubeconfig")
	var out bytes.Buffer
	if err := writeRunStatusHuman(&out, doc); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Job ray/train-001",
		"State: Running",
		"Progress",
		"[x]  Submitted",
		"Resources",
		"Kueue: 1/1 workloads admitted (queue research)",
		"Pods: 1/1 ready, 0 restarts, 1 nodes",
		"GPU telemetry: observed (1/1 nodes), 92% average utilization (1/1 GPUs observed)",
		"Attention",
		"No current issues.",
		"Next",
		"'tau' 'run' 'status' 'train-001' '--watch'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("human status missing %q:\n%s", want, got)
		}
	}
	for _, oldSection := range []string{"Kueue Workloads:", "\nPods:\n"} {
		if strings.Contains(got, oldSection) {
			t.Fatalf("human status retained infrastructure dump section %q:\n%s", oldSection, got)
		}
	}
}

func TestRunStatusHumanAndJSONShareActions(t *testing.T) {
	doc := newRunStatusDocument(status.Snapshot{
		Name:          "train-001",
		Namespace:     "ray",
		JobFound:      true,
		JobConditions: []status.Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}},
	}, false, "", "")

	var human, raw bytes.Buffer
	if err := writeRunStatusHuman(&human, doc); err != nil {
		t.Fatal(err)
	}
	if err := writeRunStatusJSONDocument(&raw, doc); err != nil {
		t.Fatal(err)
	}
	var decoded runStatusDocument
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Actions) != 2 || len(doc.Actions) != len(decoded.Actions) {
		t.Fatalf("actions: document=%+v JSON=%+v", doc.Actions, decoded.Actions)
	}
	for _, action := range decoded.Actions {
		if !strings.Contains(human.String(), action.Command.Shell) {
			t.Fatalf("human output missing JSON action %q:\n%s", action.Command.Shell, human.String())
		}
	}
}

func TestRunStatusFailedRunSurfacesAttention(t *testing.T) {
	doc := newRunStatusDocument(status.Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		JobConditions: []status.Condition{{
			Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", Message: "retry limit reached",
		}},
	}, false, "", "")
	var human bytes.Buffer
	if err := writeRunStatusHuman(&human, doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status.State != status.LifecycleFailed || doc.Status.DisplayState != "Failed" {
		t.Fatalf("status = %+v", doc.Status)
	}
	if len(doc.Diagnostics) == 0 || doc.Diagnostics[0].Code != "RUN_FAILED" {
		t.Fatalf("diagnostics = %+v", doc.Diagnostics)
	}
	for _, want := range []string{"[RUN_FAILED]", "BackoffLimitExceeded", "retry limit reached"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("failed human status missing %q:\n%s", want, human.String())
		}
	}
	if strings.Contains(human.String(), "No current issues.") {
		t.Fatalf("failed run cannot report no current issues:\n%s", human.String())
	}
}

func TestRunStatusRecoveryCommandsPreserveResolvedContext(t *testing.T) {
	doc := newRunStatusDocument(
		status.Snapshot{Name: "missing", Namespace: "ray"},
		false,
		"research-admin",
		"/tmp/kubeconfig",
	)
	if len(doc.Diagnostics) != 1 || len(doc.Diagnostics[0].Commands) != 1 {
		t.Fatalf("diagnostics = %+v", doc.Diagnostics)
	}
	diagnosticArgv := strings.Join(doc.Diagnostics[0].Commands[0].Argv, " ")
	actionArgv := strings.Join(doc.Actions[0].Command.Argv, " ")
	for label, argv := range map[string]string{"diagnostic": diagnosticArgv, "action": actionArgv} {
		if !strings.Contains(argv, "--context research-admin") || !strings.Contains(argv, "--namespace ray") {
			t.Fatalf("%s recovery command lost route: %q", label, argv)
		}
	}
	for label, command := range map[string]runStatusInvocation{
		"diagnostic": doc.Diagnostics[0].Commands[0],
		"action":     doc.Actions[0].Command,
	} {
		if command.Env["KUBECONFIG"] != "/tmp/kubeconfig" ||
			!strings.HasPrefix(command.Shell, "KUBECONFIG='/tmp/kubeconfig' ") {
			t.Fatalf("%s recovery command lost activated kubeconfig: %+v", label, command)
		}
	}
}

func TestRunStatusDiagnosticsIdentifyFailureSource(t *testing.T) {
	tests := []struct {
		name string
		snap status.Snapshot
		code string
	}{
		{
			name: "multikueue rejection",
			snap: status.Snapshot{
				RayJob: status.RayJob{Found: true, ManagedBy: "kueue.x-k8s.io/multikueue"},
				Workloads: []status.Workload{{
					AdmissionChecks: []status.AdmissionCheck{{State: "Rejected", Message: "worker quota exhausted"}},
				}},
			},
			code: "ADMISSION_REJECTED",
		},
		{
			name: "image startup failure",
			snap: status.Snapshot{
				JobFound: true,
				Pods: []status.Pod{{
					Containers: []status.Container{{State: "waiting", Reason: "ImagePullBackOff", Message: "manifest unknown"}},
				}},
			},
			code: "STARTUP_FAILED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := newRunStatusDocument(tt.snap, false, "", "")
			if doc.Status.State != status.LifecycleFailed || doc.Status.DisplayState != "Failed" {
				t.Fatalf("status = %+v", doc.Status)
			}
			if len(doc.Diagnostics) == 0 || doc.Diagnostics[0].Code != tt.code {
				t.Fatalf("diagnostics = %+v, want first code %q", doc.Diagnostics, tt.code)
			}
		})
	}
}

func TestRunStatusPreservesKueuePlacementDetail(t *testing.T) {
	message := `couldn't assign flavors: topology "default-node-topology" excluded 3 nodes for cpu`
	doc := newRunStatusDocument(status.Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		Workloads: []status.Workload{{
			Name: "train-001", Phase: "Pending", Message: message,
		}},
	}, false, "", "")
	if len(doc.Diagnostics) == 0 || doc.Diagnostics[0].Code != "KUEUE_PENDING" ||
		!strings.Contains(doc.Diagnostics[0].Message, "excluded 3 nodes") {
		t.Fatalf("diagnostics = %+v", doc.Diagnostics)
	}
	var human bytes.Buffer
	if err := writeRunStatusHuman(&human, doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "excluded 3 nodes") {
		t.Fatalf("human output dropped placement detail:\n%s", human.String())
	}
}

func TestRunStatusCompletedRunOmitsStalePendingDiagnostic(t *testing.T) {
	doc := newRunStatusDocument(status.Snapshot{
		Name:          "train-001",
		Namespace:     "ray",
		JobFound:      true,
		JobConditions: []status.Condition{{Type: "Complete", Status: "True"}},
		Workloads: []status.Workload{{
			Name: "train-001", Phase: "Pending", Message: "stale quota message",
		}},
	}, false, "", "")
	for _, diagnostic := range doc.Diagnostics {
		if diagnostic.Code == "KUEUE_PENDING" {
			t.Fatalf("completed run retained stale pending diagnostic: %+v", doc.Diagnostics)
		}
	}
}

func TestRunStatusProfileHasHumanJSONParity(t *testing.T) {
	doc := newRunStatusDocument(status.Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		Labels:    map[string]string{"tau.azure.com/profile": "h200"},
	}, true, "", "")
	if len(doc.Profile) == 0 {
		t.Fatal("run profile was not added to shared document")
	}

	var human, raw bytes.Buffer
	if err := writeRunStatusHuman(&human, doc); err != nil {
		t.Fatal(err)
	}
	if err := writeRunStatusJSONDocument(&raw, doc); err != nil {
		t.Fatal(err)
	}
	var decoded runStatusDocument
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Profile) != len(doc.Profile) {
		t.Fatalf("profile fields: document=%d JSON=%d", len(doc.Profile), len(decoded.Profile))
	}
	for _, field := range decoded.Profile {
		if !strings.Contains(human.String(), field.Name) || !strings.Contains(human.String(), field.Value) {
			t.Fatalf("human profile missing JSON field %+v:\n%s", field, human.String())
		}
	}
}
