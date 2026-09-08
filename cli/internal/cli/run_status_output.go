// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const runStatusAPIVersion = "tau.azure.com/v1alpha1"

type runStatusDocument struct {
	APIVersion     string                 `json:"apiVersion"`
	Kind           string                 `json:"kind"`
	Metadata       runStatusMetadata      `json:"metadata"`
	Status         runStatusSummary       `json:"status"`
	Phases         []status.Phase         `json:"phases"`
	Job            *runStatusJob          `json:"job,omitempty"`
	RayJob         *status.RayJob         `json:"rayJob,omitempty"`
	Workloads      []status.Workload      `json:"workloads"`
	Pods           []status.Pod           `json:"pods"`
	ResourceClaims []status.ResourceClaim `json:"resourceClaims"`
	Events         []status.Event         `json:"events"`
	Metrics        *runStatusMetrics      `json:"metrics,omitempty"`
	Profile        []status.ProfileField  `json:"profile,omitempty"`
	Diagnostics    []runStatusDiagnostic  `json:"diagnostics"`
	Actions        []runStatusAction      `json:"actions"`
}

type runStatusMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type runStatusSummary struct {
	Found           bool                  `json:"found"`
	WorkloadKind    string                `json:"workloadKind,omitempty"`
	State           status.LifecycleState `json:"state"`
	DisplayState    string                `json:"displayState"`
	StartupComplete bool                  `json:"startupComplete"`
	StartupFailed   bool                  `json:"startupFailed"`
}

type runStatusJob struct {
	Found       bool               `json:"found"`
	UID         string             `json:"uid,omitempty"`
	CreatedAt   time.Time          `json:"createdAt,omitempty,omitzero"`
	StartedAt   time.Time          `json:"startedAt,omitempty,omitzero"`
	FinishedAt  time.Time          `json:"finishedAt,omitempty,omitzero"`
	Suspended   bool               `json:"suspended"`
	Active      int                `json:"active"`
	Succeeded   int                `json:"succeeded"`
	Failed      int                `json:"failed"`
	Parallelism int                `json:"parallelism"`
	ManagedBy   string             `json:"managedBy,omitempty"`
	Conditions  []status.Condition `json:"conditions"`
}

type runStatusMetrics struct {
	GPURuntime status.GPURuntimeEvidence `json:"gpuRuntime"`
}

type runStatusDiagnostic struct {
	Code       string                `json:"code"`
	Severity   string                `json:"severity"`
	Message    string                `json:"message"`
	Suggestion string                `json:"suggestion,omitempty"`
	Commands   []runStatusInvocation `json:"commands,omitempty"`
}

type runStatusAction struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Command     runStatusInvocation `json:"command"`
}

type runStatusInvocation struct {
	Argv  []string          `json:"argv"`
	Env   map[string]string `json:"env,omitempty"`
	Shell string            `json:"shell"`
}

func writeRunStatusJSON(w io.Writer, snap status.Snapshot, runProfile bool, kubeContext, kubeconfig string) error {
	doc := newRunStatusDocument(snap, runProfile, kubeContext, kubeconfig)
	return writeRunStatusJSONDocument(w, doc)
}

func writeRunStatusJSONDocument(w io.Writer, doc runStatusDocument) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(doc)
}

func writeRunStatusHuman(w io.Writer, doc runStatusDocument) error {
	kind := doc.Status.WorkloadKind
	if kind == "" {
		kind = "Run"
	}
	fmt.Fprintf(w, "%s %s/%s\n", kind, doc.Metadata.Namespace, doc.Metadata.Name)
	fmt.Fprintf(w, "State: %s\n", doc.Status.DisplayState)

	fmt.Fprintln(w, "\nProgress")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, phase := range doc.Phases {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", humanPhaseMarker(phase.Status), phase.Name, phase.Detail)
		if phase.Hint != "" && (phase.Status == status.PhaseWarning || phase.Status == status.PhaseActive) {
			fmt.Fprintf(tw, "\t\tHint: %s\n", phase.Hint)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nResources")
	resourceLines := humanResourceLines(doc)
	if len(resourceLines) == 0 {
		fmt.Fprintln(w, "  No runtime resources observed.")
	} else {
		for _, line := range resourceLines {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	if len(doc.Profile) > 0 {
		fmt.Fprintln(w, "\nRun profile")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, field := range doc.Profile {
			fmt.Fprintf(tw, "  %s\t%s\n", field.Name, field.Value)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintln(w, "\nAttention")
	attention := humanAttentionDiagnostics(doc.Diagnostics)
	if len(attention) == 0 {
		fmt.Fprintln(w, "  No current issues.")
	} else {
		for _, diagnostic := range attention {
			fmt.Fprintf(w, "  [%s] %s\n", diagnostic.Code, diagnostic.Message)
			if diagnostic.Suggestion != "" {
				fmt.Fprintf(w, "    %s\n", diagnostic.Suggestion)
			}
		}
	}

	fmt.Fprintln(w, "\nNext")
	if len(doc.Actions) == 0 {
		fmt.Fprintln(w, "  No action required.")
		return nil
	}
	for _, action := range doc.Actions {
		fmt.Fprintf(w, "  %s\n", action.Command.Shell)
		fmt.Fprintf(w, "    %s\n", action.Description)
	}
	return nil
}

func newRunStatusDocument(snap status.Snapshot, runProfile bool, kubeContext, kubeconfig string) runStatusDocument {
	phases := status.StartupPhases(snap)
	doc := runStatusDocument{
		APIVersion: runStatusAPIVersion,
		Kind:       "RunStatus",
		Metadata: runStatusMetadata{
			Name:        snap.Name,
			Namespace:   snap.Namespace,
			Labels:      snap.Labels,
			Annotations: snap.Annotations,
		},
		Status: runStatusSummary{
			Found:           status.RunFound(snap),
			WorkloadKind:    runStatusWorkloadKind(snap),
			State:           status.CanonicalLifecycleState(snap),
			DisplayState:    status.HeadlineState(snap),
			StartupComplete: status.StartupComplete(snap),
			StartupFailed:   status.StartupFailed(snap),
		},
		Phases:         phases,
		Workloads:      nonNilWorkloads(snap.Workloads),
		Pods:           nonNilPods(snap.Pods),
		ResourceClaims: nonNilResourceClaims(snap.ResourceClaims),
		Events:         nonNilEvents(snap.Events),
		Diagnostics:    runStatusDiagnostics(snap, phases, kubeContext, kubeconfig),
		Actions:        runStatusActions(snap, kubeContext, kubeconfig),
	}
	if snap.JobFound {
		doc.Job = &runStatusJob{
			Found:       true,
			UID:         snap.JobUID,
			CreatedAt:   snap.JobCreatedAt,
			StartedAt:   snap.JobStartedAt,
			FinishedAt:  snap.JobFinishedAt,
			Suspended:   snap.JobSuspended,
			Active:      snap.JobActive,
			Succeeded:   snap.JobSucceeded,
			Failed:      snap.JobFailed,
			Parallelism: snap.JobParallelism,
			ManagedBy:   snap.JobManagedBy,
			Conditions:  nonNilConditions(snap.JobConditions),
		}
	}
	rayJob := status.CanonicalRayJob(snap)
	if rayJob.Found && !snap.JobFound {
		doc.RayJob = &rayJob
	}
	if runProfile {
		gpuRuntime := snap.GPURuntime
		if gpuRuntime.Devices == nil {
			gpuRuntime.Devices = []status.GPUDeviceEvidence{}
		}
		doc.Metrics = &runStatusMetrics{GPURuntime: gpuRuntime}
		doc.Profile = status.RunProfileFields(snap, status.CostProfile{})
	}
	return doc
}

func runStatusWorkloadKind(snap status.Snapshot) string {
	switch {
	case snap.JobFound:
		return "Job"
	case status.CanonicalRayJob(snap).Found:
		return "RayJob"
	default:
		return ""
	}
}

func runStatusDiagnostics(snap status.Snapshot, phases []status.Phase, kubeContext, kubeconfig string) []runStatusDiagnostic {
	diagnostics := make([]runStatusDiagnostic, 0)
	tauCommand := runStatusTauCommand(snap, kubeContext, kubeconfig)
	if !status.RunFound(snap) {
		diagnostics = append(diagnostics, runStatusDiagnostic{
			Code:       "RUN_NOT_FOUND",
			Severity:   "error",
			Message:    "no batch Job or RayJob was found with this name",
			Suggestion: "list runs in the resolved namespace and verify the run name",
			Commands:   []runStatusInvocation{tauCommand("list", "--output", "json")},
		})
		return diagnostics
	}
	failureCode := ""
	if failure := terminalFailureDiagnostic(snap, phases); failure != nil {
		failureCode = failure.Code
		diagnostics = append(diagnostics, *failure)
	}
	state := status.CanonicalLifecycleState(snap)
	if failureCode != "ADMISSION_REJECTED" &&
		state != status.LifecycleSucceeded &&
		state != status.LifecycleFailed {
		for _, workload := range snap.Workloads {
			if workload.Admitted || strings.EqualFold(workload.Phase, "Finished") || strings.TrimSpace(workload.Message) == "" {
				continue
			}
			diagnostics = append(diagnostics, runStatusDiagnostic{
				Code:       "KUEUE_PENDING",
				Severity:   "warning",
				Message:    strings.Join(strings.Fields(workload.Message), " "),
				Suggestion: "check queue quota, resource flavors, and topology constraints",
			})
		}
	}
	for _, phase := range phases {
		if phase.Hint == "" && phase.Status != status.PhaseWarning {
			continue
		}
		severity := "info"
		if phase.Status == status.PhaseWarning {
			severity = "warning"
		}
		diagnostics = append(diagnostics, runStatusDiagnostic{
			Code:       "STARTUP_" + diagnosticCode(phase.Name),
			Severity:   severity,
			Message:    phase.Detail,
			Suggestion: phase.Hint,
		})
	}
	commands := kubectlDiagnosticCommands(kubeContext, kubeconfig, snap.Namespace, snap.Name, snap)
	rendered := make([]runStatusInvocation, 0, len(commands))
	for _, command := range commands {
		rendered = append(rendered, newRunStatusInvocation(command))
	}
	if len(rendered) > 0 {
		diagnostics = append(diagnostics, runStatusDiagnostic{
			Code:     "DEEP_DIAGNOSTICS",
			Severity: "info",
			Message:  "scoped Kubernetes commands for deeper runtime inspection",
			Commands: rendered,
		})
	}
	return diagnostics
}

func runStatusActions(snap status.Snapshot, kubeContext, kubeconfig string) []runStatusAction {
	command := runStatusTauCommand(snap, kubeContext, kubeconfig)
	if !status.RunFound(snap) {
		return []runStatusAction{{
			Name:        "list",
			Description: "List runs in the resolved namespace and verify the run name.",
			Command:     command("list", "--output", "json"),
		}}
	}
	state := status.CanonicalLifecycleState(snap)
	switch {
	case state == status.LifecycleFailed:
		actions := make([]runStatusAction, 0, 2)
		if runStatusLogsRetrievable(snap) {
			actions = append(actions, runStatusAction{
				Name:        "logs",
				Description: "Inspect the workload's execution logs.",
				Command:     command("logs", snap.Name, "--tail", "200"),
			})
		}
		if len(kubectlDiagnosticCommands(kubeContext, kubeconfig, snap.Namespace, snap.Name, snap)) > 0 {
			actions = append(actions, runStatusAction{
				Name:        "diagnostics",
				Description: "Show scoped deep-diagnostic commands.",
				Command:     command("status", snap.Name, "--diagnostic-hints"),
			})
		}
		return actions
	case state == status.LifecycleSucceeded:
		actions := make([]runStatusAction, 0, 2)
		if runStatusLogsRetrievable(snap) {
			actions = append(actions, runStatusAction{
				Name:        "logs",
				Description: "Inspect the completed workload's execution logs.",
				Command:     command("logs", snap.Name, "--tail", "200"),
			})
		}
		if runStatusResultsRetrievable(snap) {
			actions = append(actions, runStatusAction{
				Name:        "results",
				Description: "Fetch persisted run results and artifacts.",
				Command:     command("get", snap.Name),
			})
		}
		return actions
	default:
		actions := []runStatusAction{{
			Name:        "watch",
			Description: "Follow lifecycle progress until the run is ready or fails.",
			Command:     command("status", snap.Name, "--watch"),
		}}
		if runStatusLogsRetrievable(snap) {
			actions = append(actions, runStatusAction{
				Name:        "logs",
				Description: "Inspect currently available execution logs.",
				Command:     command("logs", snap.Name, "--tail", "200"),
			})
		}
		return actions
	}
}

func runStatusLogsRetrievable(snap status.Snapshot) bool {
	if snap.JobFound {
		return !snap.ManagerOnlyMultiKueueView() && len(snap.Pods) > 0
	}
	rayJob := status.CanonicalRayJob(snap)
	return rayJob.Found &&
		!snap.ManagerOnlyMultiKueueView() &&
		strings.TrimSpace(rayJob.JobID) != "" &&
		strings.TrimSpace(rayJob.RayClusterName) != ""
}

func runStatusResultsRetrievable(snap status.Snapshot) bool {
	if snap.JobFound && status.CanonicalRayJob(snap).Found {
		return false
	}
	return strings.TrimSpace(snap.Annotations[workloadmeta.AnnotationResultPath]) != "" &&
		strings.TrimSpace(snap.Annotations[workloadmeta.AnnotationResultPVC]) != ""
}

func runStatusTauCommand(snap status.Snapshot, kubeContext, kubeconfig string) func(...string) runStatusInvocation {
	return func(args ...string) runStatusInvocation {
		base := []string{"tau", "run"}
		base = append(base, args...)
		if strings.TrimSpace(kubeContext) != "" {
			base = append(base, "--context", kubeContext)
		}
		if strings.TrimSpace(snap.Namespace) != "" {
			base = append(base, "--namespace", snap.Namespace)
		}
		env := map[string]string{}
		if strings.TrimSpace(kubeconfig) != "" {
			env["KUBECONFIG"] = kubeconfig
		}
		return newRunStatusInvocationWithEnv(base, env)
	}
}

func terminalFailureDiagnostic(snap status.Snapshot, phases []status.Phase) *runStatusDiagnostic {
	if status.CanonicalLifecycleState(snap) != status.LifecycleFailed {
		return nil
	}
	for _, condition := range snap.JobConditions {
		if condition.Type != "Failed" || condition.Status != "True" {
			continue
		}
		return &runStatusDiagnostic{
			Code:       "RUN_FAILED",
			Severity:   "error",
			Message:    defaultString(strings.TrimSpace(condition.Reason+" "+condition.Message), "batch Job failed"),
			Suggestion: "inspect execution logs and deep diagnostics before retrying",
		}
	}
	rayJob := status.CanonicalRayJob(snap)
	if !snap.JobFound && rayJob.Found && status.RayJobFailed(rayJob) {
		return &runStatusDiagnostic{
			Code:       "RUN_FAILED",
			Severity:   "error",
			Message:    defaultString(strings.TrimSpace(rayJob.Reason+" "+rayJob.Message), "RayJob failed"),
			Suggestion: "inspect execution logs and deep diagnostics before retrying",
		}
	}
	if snap.ManagerOnlyMultiKueueView() {
		for _, workload := range snap.Workloads {
			for _, check := range workload.AdmissionChecks {
				if !strings.EqualFold(check.State, "Rejected") {
					continue
				}
				message := defaultString(strings.TrimSpace(check.Message), "Kueue admission rejected the workload")
				return &runStatusDiagnostic{
					Code:       "ADMISSION_REJECTED",
					Severity:   "error",
					Message:    message,
					Suggestion: "inspect queue policy, worker placement, and admission-check configuration",
				}
			}
		}
	}
	for _, phase := range phases {
		if phase.Status != status.PhaseWarning {
			continue
		}
		return &runStatusDiagnostic{
			Code:       "STARTUP_FAILED",
			Severity:   "error",
			Message:    phase.Detail,
			Suggestion: phase.Hint,
		}
	}
	return &runStatusDiagnostic{
		Code:       "RUN_FAILED",
		Severity:   "error",
		Message:    "run failed",
		Suggestion: "inspect execution logs and deep diagnostics before retrying",
	}
}

func newRunStatusInvocation(argv []string) runStatusInvocation {
	return newRunStatusInvocationWithEnv(argv, nil)
}

func newRunStatusInvocationWithEnv(argv []string, env map[string]string) runStatusInvocation {
	shell := renderShellCommand(argv)
	if kubeconfig := strings.TrimSpace(env["KUBECONFIG"]); kubeconfig != "" {
		shell = "KUBECONFIG=" + renderShellCommand([]string{kubeconfig}) + " " + shell
	}
	return runStatusInvocation{
		Argv:  append([]string(nil), argv...),
		Env:   env,
		Shell: shell,
	}
}

func humanPhaseMarker(phaseStatus status.PhaseStatus) string {
	switch phaseStatus {
	case status.PhaseDone:
		return "[x]"
	case status.PhaseActive:
		return "[>]"
	case status.PhaseWarning:
		return "[!]"
	case status.PhaseSkipped:
		return "[-]"
	default:
		return "[ ]"
	}
}

func humanResourceLines(doc runStatusDocument) []string {
	lines := make([]string, 0, 4)
	if len(doc.Workloads) > 0 {
		queues := make(map[string]bool)
		admitted := 0
		for _, workload := range doc.Workloads {
			if workload.Queue != "" {
				queues[workload.Queue] = true
			}
			if workload.Admitted {
				admitted++
			}
		}
		queueNames := make([]string, 0, len(queues))
		for queue := range queues {
			queueNames = append(queueNames, queue)
		}
		sort.Strings(queueNames)
		line := fmt.Sprintf("Kueue: %d/%d workloads admitted", admitted, len(doc.Workloads))
		if len(queueNames) > 0 {
			line += " (queue " + strings.Join(queueNames, ", ") + ")"
		}
		lines = append(lines, line)
	}
	if len(doc.Pods) > 0 {
		ready, restarts := 0, 0
		nodes := make(map[string]bool)
		for _, pod := range doc.Pods {
			if podReady(pod.Ready) {
				ready++
			}
			restarts += pod.Restarts
			if pod.Node != "" {
				nodes[pod.Node] = true
			}
		}
		line := fmt.Sprintf("Pods: %d/%d ready, %d restarts", ready, len(doc.Pods), restarts)
		if len(nodes) > 0 {
			line += fmt.Sprintf(", %d nodes", len(nodes))
		}
		lines = append(lines, line)
	}
	if doc.RayJob != nil && doc.RayJob.RayClusterName != "" {
		lines = append(lines, "Ray cluster: "+doc.RayJob.RayClusterName)
	}
	if doc.Metrics != nil {
		gpu := doc.Metrics.GPURuntime
		line := fmt.Sprintf("GPU telemetry: %s (%d/%d nodes)", defaultString(string(gpu.State), "not requested"), gpu.NodesScraped, gpu.NodesExpected)
		if average, count := averageGPUUtilization(gpu.Devices); count > 0 {
			line += fmt.Sprintf(", %.0f%% average utilization (%d/%d GPUs observed)", average, count, len(gpu.Devices))
		}
		if gpu.Reason != "" {
			line += " - " + gpu.Reason
		}
		lines = append(lines, line)
	}
	return lines
}

func humanAttentionDiagnostics(diagnostics []runStatusDiagnostic) []runStatusDiagnostic {
	out := make([]runStatusDiagnostic, 0)
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "warning" || diagnostic.Severity == "error" {
			out = append(out, diagnostic)
		}
	}
	return out
}

func podReady(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && parts[0] != "0" && parts[0] == parts[1]
}

func averageGPUUtilization(devices []status.GPUDeviceEvidence) (float64, int) {
	var total float64
	count := 0
	for _, device := range devices {
		if !device.UtilizationObserved {
			continue
		}
		total += device.UtilizationPercent
		count++
	}
	if count == 0 {
		return 0, 0
	}
	return total / float64(count), count
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func diagnosticCode(name string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "_", "-", "_", "/", "_").Replace(name))
}

func nonNilConditions(values []status.Condition) []status.Condition {
	if values == nil {
		return []status.Condition{}
	}
	return values
}

func nonNilWorkloads(values []status.Workload) []status.Workload {
	if values == nil {
		return []status.Workload{}
	}
	return values
}

func nonNilPods(values []status.Pod) []status.Pod {
	if values == nil {
		return []status.Pod{}
	}
	return values
}

func nonNilResourceClaims(values []status.ResourceClaim) []status.ResourceClaim {
	if values == nil {
		return []status.ResourceClaim{}
	}
	return values
}

func nonNilEvents(values []status.Event) []status.Event {
	if values == nil {
		return []status.Event{}
	}
	return values
}
