// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/status"
)

func TestRunStatusRegistersRunProfileFlag(t *testing.T) {
	if flag := newRunStatusCmd().Flags().Lookup("run-profile"); flag == nil {
		t.Fatal("tau run status must support the --run-profile handoff")
	}
}

func TestRunStatusRegistersDiagnosticHintsFlag(t *testing.T) {
	if flag := newRunStatusCmd().Flags().Lookup("diagnostic-hints"); flag == nil {
		t.Fatal("tau run status must support --diagnostic-hints")
	}
}

func TestRunStatusRegistersMachineReadableOutputFlag(t *testing.T) {
	flag := newRunStatusCmd().Flags().Lookup("output")
	if flag == nil {
		t.Fatal("tau run status must support --output")
	}
	if flag.DefValue != "table" {
		t.Fatalf("tau run status --output default = %q, want table", flag.DefValue)
	}
}

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

func TestResolveRunStatusConnectionKeepsJSONStdoutClean(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	_, _, _, err := resolveRunStatusConnection(cmd, "json", func(cmd *cobra.Command) (string, string, func(), error) {
		_, _ = io.WriteString(cmd.OutOrStdout(), "Activating workspace connection...\n")
		return "research", "ray", func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(cmd.OutOrStdout(), `{"kind":"RunStatus"}`)
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "Activating workspace connection") {
		t.Fatalf("connection progress was not redirected to stderr: %q", got)
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

func TestActiveKubeconfigPathRequiresSingleResolvedPath(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/resolved-kubeconfig")
	if got := activeKubeconfigPath(); got != "/tmp/resolved-kubeconfig" {
		t.Fatalf("active kubeconfig = %q", got)
	}
	t.Setenv("KUBECONFIG", strings.Join([]string{"/tmp/first", "/tmp/second"}, string(os.PathListSeparator)))
	if got := activeKubeconfigPath(); got != "" {
		t.Fatalf("multi-file kubeconfig must not become --kubeconfig: %q", got)
	}
}

func TestRenderKubectlDiagnosticHintsUsesResolvedPodAndContainer(t *testing.T) {
	got := renderKubectlDiagnosticHints("research'admin", "/tmp/research kube'config", "tau-default", "external-batch-job", status.Snapshot{
		Pods: []status.Pod{{
			Name: "external-batch-pod",
			Containers: []status.Container{{
				Name: "trainer", State: "running", RestartCount: 1,
			}},
		}},
	})
	for _, want := range []string{
		`'--context' 'research'"'"'admin'`,
		`'--kubeconfig' '/tmp/research kube'"'"'config'`,
		`'top' 'pod' 'external-batch-pod' '--containers'`,
		`'logs' 'external-batch-pod' '-c' 'trainer' '--previous' '--timestamps=true'`,
		`'exec' '-it' 'external-batch-pod' '-c' 'trainer' '--' '/bin/sh'`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hints %q missing %q", got, want)
		}
	}
}

func TestRenderKubectlDiagnosticHintsUsesRayPodNamesAndCurrentLogs(t *testing.T) {
	got := renderKubectlDiagnosticHints("", "", "ray", "train", status.Snapshot{
		RayJob: status.RayJob{Found: true, RayClusterName: "train-cluster"},
		Pods: []status.Pod{{
			Name:       "train-worker",
			Containers: []status.Container{{Name: "ray-worker", State: "running"}},
		}, {
			Name:       "train-head",
			Containers: []status.Container{{Name: "ray-head", State: "running"}},
		}},
	})
	if !strings.Contains(got, "'top' 'pod' 'train-worker' '--containers'") ||
		!strings.Contains(got, "'top' 'pod' 'train-head' '--containers'") ||
		strings.Contains(got, "'train-worker' 'train-head'") {
		t.Fatalf("Ray top hint = %q", got)
	}
	if !strings.Contains(got, "'logs' 'train-worker' '-c' 'ray-worker' '--timestamps=true'") || strings.Contains(got, "'--previous'") {
		t.Fatalf("Ray log hint = %q", got)
	}
}

func TestRenderKubectlDiagnosticHintsPrefersCurrentFailureOverPreviousAttempt(t *testing.T) {
	exitCode := int32(1)
	lastExitCode := int32(2)
	got := renderKubectlDiagnosticHints("", "", "tau-default", "external-batch-job", status.Snapshot{
		Pods: []status.Pod{{
			Name: "external-batch-pod",
			Containers: []status.Container{{
				Name: "restarted-worker", RestartCount: 1, LastExitCode: &lastExitCode,
			}, {
				Name: "failed-worker", ExitCode: &exitCode,
			}},
		}},
	})
	if !strings.Contains(got, "'logs' 'external-batch-pod' '-c' 'failed-worker' '--timestamps=true'") {
		t.Fatalf("hints must target current failed container: %q", got)
	}
	if strings.Contains(got, "'--previous'") {
		t.Fatalf("hints must not request previous logs while a current failure exists: %q", got)
	}
}

func TestRenderKubectlDiagnosticHintsUsesCurrentLogsForRestartedFinalFailure(t *testing.T) {
	exitCode := int32(1)
	lastExitCode := int32(2)
	got := renderKubectlDiagnosticHints("", "", "tau-default", "external-batch-job", status.Snapshot{
		Pods: []status.Pod{{
			Name: "external-batch-pod",
			Containers: []status.Container{{
				Name: "failed-worker", RestartCount: 1, ExitCode: &exitCode, LastExitCode: &lastExitCode,
			}},
		}},
	})
	if !strings.Contains(got, "'logs' 'external-batch-pod' '-c' 'failed-worker' '--timestamps=true'") {
		t.Fatalf("hints must target current failed container: %q", got)
	}
	if strings.Contains(got, "'--previous'") {
		t.Fatalf("hints must not request previous logs for a final current failure: %q", got)
	}
}

func TestRenderKubectlDiagnosticHintsOmitsExecForTerminalContainer(t *testing.T) {
	exitCode := int32(1)
	got := renderKubectlDiagnosticHints("", "", "tau-default", "external-batch-job", status.Snapshot{
		Pods: []status.Pod{{
			Name: "external-batch-pod",
			Containers: []status.Container{{
				Name: "failed-worker", State: "terminated", ExitCode: &exitCode,
			}},
		}},
	})
	if !strings.Contains(got, "'logs' 'external-batch-pod' '-c' 'failed-worker' '--timestamps=true'") {
		t.Fatalf("terminal hints must retain current logs: %q", got)
	}
	if strings.Contains(got, "'exec'") {
		t.Fatalf("terminal hints must not emit an unusable exec command: %q", got)
	}
}

func TestRenderKubectlDiagnosticHintsWithoutPodsStaysSelectorBased(t *testing.T) {
	got := renderKubectlDiagnosticHints("", "", "tau-default", "external-batch-job", status.Snapshot{})
	if !strings.Contains(got, "'logs' '-l' 'job-name=external-batch-job' '--all-containers=true'") {
		t.Fatalf("hints = %q", got)
	}
	if strings.Contains(got, "'exec'") {
		t.Fatalf("hints should not render exec without a pod: %q", got)
	}
}

func TestRenderKubectlDiagnosticHintsUsesJobPrecedenceForSameNameCollision(t *testing.T) {
	got := renderKubectlDiagnosticHints("", "", "tau-default", "shared-name", status.Snapshot{
		JobFound: true,
		RayJob:   status.RayJob{Found: true, RayClusterName: "shared-name-cluster"},
		Pods: []status.Pod{{
			Name:       "batch-pod",
			Containers: []status.Container{{Name: "worker", State: "running"}},
		}, {
			Name:       "ray-pod",
			Containers: []status.Container{{Name: "ray-worker", State: "running"}},
		}},
	})
	if !strings.Contains(got, "'job-name=shared-name'") {
		t.Fatalf("collision hints must use Job selector: %q", got)
	}
	for _, unwanted := range []string{"ray.io/cluster", "batch-pod", "ray-pod", "'exec'"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("collision hints must not contain %q: %q", unwanted, got)
		}
	}
}

func TestRunLogsRegistersDetailedBatchFlags(t *testing.T) {
	cmd := newRunLogsCmd()
	for _, name := range []string{"container", "all-containers", "previous", "timestamps", "prefix"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("tau run logs missing --%s", name)
		}
	}
}

func TestRunLifecycleQueriesRegisterWorkspaceFlag(t *testing.T) {
	tests := map[string]func() *cobra.Command{
		"status": newRunStatusCmd,
		"logs":   newRunLogsCmd,
		"get":    newRunGetCmd,
		"cancel": newRunCancelCmd,
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := build()
			flag := cmd.Flags().Lookup("workspace")
			if flag == nil {
				t.Fatalf("tau run %s must accept --workspace", name)
			}
			if flag.DefValue != "" {
				t.Fatalf("tau run %s --workspace default = %q, want empty", name, flag.DefValue)
			}
			if err := cmd.Flags().Set("workspace", "research"); err != nil {
				t.Fatalf("set tau run %s --workspace: %v", name, err)
			}
			if got, err := cmd.Flags().GetString("workspace"); err != nil || got != "research" {
				t.Fatalf("tau run %s --workspace = %q, %v", name, got, err)
			}
		})
	}
}

func TestWatchStatusCommand_MultiKueueProgressesUntilReady(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	snapshots := []status.Snapshot{
		multiKueueWatchSnapshot(status.MultiKueueStatePending),
		multiKueueWatchSnapshot(status.MultiKueueStateNominated),
		multiKueueWatchSnapshot(status.MultiKueueStateSelected),
		multiKueueWatchSnapshot(status.MultiKueueStateRetry),
		multiKueueWatchSnapshot(status.MultiKueueStateReady),
	}
	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			snap := snapshots[calls]
			calls++
			return snap, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("watch should stop on Ready, got %v", err)
	}
	if calls != len(snapshots) {
		t.Fatalf("expected %d snapshots, got %d", len(snapshots), calls)
	}
}

func TestWatchStatusCommand_MultiKueueRejectedFails(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			return multiKueueWatchSnapshot(status.MultiKueueStateRejected), nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err == nil || !strings.Contains(err.Error(), "startup phase failed") {
		t.Fatalf("expected rejected placement failure, got %v", err)
	}
}

func TestWatchStatusCommand_MultiKueueManagerViewWithoutLocalPodsDoesNotExitEarly(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return multiKueueWatchSnapshot(status.MultiKueueStateSelected), nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("selected manager-view status should keep watching until max iterations, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_GenericAdmissionChecksDoNotTriggerMultiKueueTermination(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				JobFound:  true,
				Workloads: []status.Workload{{
					Name: "train-001",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "quota-check", State: "Rejected", Message: "quota full"},
					},
				}},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("generic admission checks should not be treated as MultiKueue watch failure, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueReadyPlusGenericPendingKeepsWatching(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			snap := multiKueueWatchSnapshot(status.MultiKueueStateReady)
			snap.JobActive = 0
			snap.Workloads[0].AdmissionChecks = append(snap.Workloads[0].AdmissionChecks,
				status.AdmissionCheck{Name: "quota-check", State: "Pending", ControllerName: "kueue.x-k8s.io/provisioning"},
			)
			return snap, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("generic pending admission checks should keep watching, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueReadyPlusGenericRejectedFails(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			snap := multiKueueWatchSnapshot(status.MultiKueueStateReady)
			snap.JobActive = 0
			snap.Workloads[0].AdmissionChecks = append(snap.Workloads[0].AdmissionChecks,
				status.AdmissionCheck{Name: "quota-check", State: "Rejected", ControllerName: "kueue.x-k8s.io/provisioning"},
			)
			return snap, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err == nil || !strings.Contains(err.Error(), "startup phase failed") {
		t.Fatalf("generic rejected admission checks should fail watch even when placement is ready, got %v", err)
	}
}

func TestWatchStatusCommand_MultiKueueReadyStopsWithoutWaitingForMirroredRayJobRunning(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					ManagedBy:           "kueue.x-k8s.io/multikueue",
					JobDeploymentStatus: "Initializing",
				},
				Workloads: []status.Workload{{
					Name:        "train-001",
					Admitted:    true,
					Phase:       "Admitted",
					ClusterName: "worker-a",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("watch should stop on MultiKueue placement Ready, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected ready placement to stop after one snapshot, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueSuspendedRayJobKeepsWatching(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					ManagedBy:           "kueue.x-k8s.io/multikueue",
					JobDeploymentStatus: "Suspended",
				},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("suspended manager-view RayJob should keep watching until max iterations, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueSuspendedReadyRayJobKeepsWatching(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					ManagedBy:           "kueue.x-k8s.io/multikueue",
					JobDeploymentStatus: "Suspended",
				},
				Workloads: []status.Workload{{
					Name:        "train-001",
					ClusterName: "worker-a",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("suspended manager-view RayJob with ready placement should keep watching until max iterations, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueTerminalPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		snap    status.Snapshot
		wantErr string
	}{
		{
			name: "complete beats rejected placement",
			snap: status.Snapshot{
				Name:          "train-001",
				Namespace:     "ray",
				JobFound:      true,
				JobConditions: []status.Condition{{Type: "Complete", Status: "True"}},
				Workloads: []status.Workload{{
					Name: "train-001",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			},
		},
		{
			name: "failed beats ready placement",
			snap: status.Snapshot{
				Name:          "train-001",
				Namespace:     "ray",
				JobFound:      true,
				JobConditions: []status.Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}},
				Workloads: []status.Workload{{
					Name: "train-001",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			},
			wantErr: "startup phase failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetContext(context.Background())
			err := watchStatusCommandWithHooks(cmd, statusRunOptions{
				Namespace: "ray",
				Interval:  time.Millisecond,
			}, "train-001", watchStatusHooks{
				fetch: func(context.Context) (status.Snapshot, error) {
					return tt.snap, nil
				},
				wait:        func(context.Context, time.Duration) error { return nil },
				clearScreen: func(io.Writer) {},
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func multiKueueWatchSnapshot(state status.MultiKueueState) status.Snapshot {
	snap := status.Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: "kueue.x-k8s.io/multikueue",
	}
	workload := status.Workload{Name: "train-001"}
	switch state {
	case status.MultiKueueStateNominated:
		workload.NominatedClusterNames = []string{"worker-a", "worker-b"}
	case status.MultiKueueStateSelected:
		workload.ClusterName = "worker-a"
	case status.MultiKueueStateRetry:
		workload.ClusterName = "worker-a"
		workload.AdmissionChecks = []status.AdmissionCheck{{Name: "multikueue", State: "Retry", Message: "retrying reservation", ControllerName: "kueue.x-k8s.io/multikueue"}}
	case status.MultiKueueStateReady:
		snap.JobActive = 1
		workload.Admitted = true
		workload.Phase = "Admitted"
		workload.ClusterName = "worker-a"
		workload.AdmissionChecks = []status.AdmissionCheck{{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"}}
	case status.MultiKueueStateRejected:
		workload.AdmissionChecks = []status.AdmissionCheck{{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: "kueue.x-k8s.io/multikueue"}}
	}
	if state != status.MultiKueueStatePending {
		snap.Workloads = []status.Workload{workload}
	}
	return snap
}
