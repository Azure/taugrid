package status

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// CostProfile is the optional cost slice of a run profile.
type CostProfile struct {
	Status     string
	Profile    string
	GPUType    string
	GPUsPerPod int
	Pods       int
	Hours      float64
	UsdPerHour float64
	TotalUsd   float64
	Error      string
}

// RenderRunProfile appends the researcher-facing "what happened to my run?"
// view: queue wait, runtime, shape, cost, artifact hints, and explicit gaps.
func RenderRunProfile(s Snapshot, c CostProfile) string {
	var b strings.Builder
	b.WriteString("\nRun profile:\n")

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		fmt.Fprintf(tw, "  %s\t%s\n", k, v)
	}

	row("queue_wait", queueWait(s))
	row("queue_wait_seconds", secondsOrUnavailable(queueWaitSeconds(s)))
	row("admission", admissionSummary(s))
	row("runtime", runtimeSummary(s))
	row("runtime_seconds", secondsOrUnavailable(runtimeSeconds(s)))
	row("run_id", firstNonEmpty(label(s, experiment.LabelRunID), s.Name))
	row("workload_kind", firstNonEmpty(label(s, experiment.LabelWorkloadKind), "not declared"))
	row("capture_version", annotationOrDefault(s, experiment.AnnotationCaptureVersion, "not declared"))
	row("namespace", annotationOrDefault(s, experiment.AnnotationNamespace, s.Namespace))
	row("tau_command", annotationOrDefault(s, experiment.AnnotationTauCommand, "not declared"))
	row("image", annotationOrDefault(s, experiment.AnnotationImage, "not declared"))
	row("image_digest", annotationOrDefault(s, experiment.AnnotationImageDigest, "not declared"))
	row("code_sha", annotationOrDefault(s, experiment.AnnotationCodeSHA, "not collected"))
	row("config_hash", annotationOrDefault(s, experiment.AnnotationConfigHash, "not declared"))
	row("profile", firstNonEmpty(label(s, workloadmeta.LabelProfile), c.Profile, "not declared"))
	row("preset", firstNonEmpty(label(s, workloadmeta.LabelPreset), "not declared"))
	row("queue", firstNonEmpty(s.ManagerLocalQueue(), workloadQueue(s), "not collected"))
	row("team", firstNonEmpty(label(s, workloadmeta.LabelTeam), "not declared"))
	row("lane", firstNonEmpty(label(s, workloadmeta.LabelLane), "not declared"))
	row("topology", firstNonEmpty(label(s, workloadmeta.LabelTopology), "not declared"))
	row("gpu_class", firstNonEmpty(canonicalGPUClassLabel(label(s, workloadmeta.LabelGPUClass)), "not declared"))
	row("gpu_count", firstNonEmpty(annotationOrDefault(s, experiment.AnnotationGPUCount, ""), label(s, workloadmeta.AnnotationGPUCount), "not declared"))
	row("dra_claim_template", annotationOrDefault(s, experiment.AnnotationDRAClaim, "not declared"))
	row("storage_mounts", annotationOrDefault(s, experiment.AnnotationStorageMounts, "not declared"))
	row("kueue_workload", workloadNames(s))
	row("local_queue", firstNonEmpty(s.EffectiveLocalQueue(), "not collected"))
	row("kueue_phase", workloadPhases(s))
	row("kueue_admitted", workloadAdmitted(s))
	row("preemption_status", preemptionStatus(s))
	row("pod_names", podNames(s))
	row("pod_uid", podUIDs(s))
	row("nodes", nodesSummary(s))
	row("node_names", nodesSummary(s))
	row("resource_claims", resourceClaims(s))
	row("pod_restarts", podRestartsSummary(s))
	row("oom_killed", oomKilledSummary(s))
	row("container_reason", containerReasons(s))
	row("exit_code", exitCodes(s))
	row("shape", shapeSummary(c))
	row("pod_start_latency", podStartLatency(s))
	row("image_pull_latency", "not collected (needs pod event/container timing integration)")
	row("cost", costSummary(c))
	row("gpu_hours", gpuHoursSummary(c))
	row("estimated_cost", estimatedCostSummary(c))
	row("gpu_utilization", "not collected (DCGM/Prometheus integration pending)")
	row("gpu_memory", "not collected (DCGM/Prometheus integration pending)")
	row("results", annotationOrDefault(s, workloadmeta.AnnotationResultPath, "not declared on Job"))
	if v := annotationOrDefault(s, workloadmeta.AnnotationResultArtifacts, ""); v != "" {
		row("artifacts", v)
	}

	if v := annotationOrDefault(s, workloadmeta.AnnotationResultNote, ""); v != "" {
		row("note", v)
	}
	if v := annotationOrDefault(s, workloadmeta.AnnotationPresetExplain, ""); v != "" {
		row("preset_explain", v)
	}
	_ = tw.Flush()
	return b.String()
}

func canonicalGPUClassLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	canonical, _ := topology.NormalizeGPUClass(v)
	return canonical
}

func queueWait(s Snapshot) string {
	created := s.JobCreatedAt
	started := s.JobStartedAt
	found := s.JobFound || s.RayJobFound
	if s.RayJobFound && !s.JobFound {
		created = s.RayJobCreatedAt
		started = s.RayJobStartedAt
	}
	if !found {
		return "not collected (job not found)"
	}
	if created.IsZero() {
		return "not collected (job creation timestamp unavailable)"
	}
	if !started.IsZero() {
		return fmtDuration(started.Sub(created))
	}
	return "pending for " + fmtDuration(time.Since(created)) + " (not started)"
}

func admissionSummary(s Snapshot) string {
	if len(s.Workloads) == 0 {
		return "not collected (no Kueue Workload found)"
	}
	w := s.Workloads[0]
	parts := []string{"phase=" + dash(w.Phase), "admitted=" + fmt.Sprintf("%t", w.Admitted)}
	if w.Queue != "" {
		parts = append(parts, "queue="+w.Queue)
	}
	if w.Reason != "" {
		parts = append(parts, "reason="+w.Reason)
	}
	return strings.Join(parts, " ")
}

func runtimeSummary(s Snapshot) string {
	found := s.JobFound || s.RayJobFound
	started := s.JobStartedAt
	finished := s.JobFinishedAt
	if s.RayJobFound && !s.JobFound {
		started = s.RayJobStartedAt
		finished = s.RayJobFinishedAt
	}
	if !found {
		return "not collected (job not found)"
	}
	if started.IsZero() {
		return "not started"
	}
	end := finished
	state := "running"
	if end.IsZero() {
		end = time.Now()
	} else {
		state = "finished"
	}
	// For RayJobs, use deployment status to determine if finished.
	if s.RayJobFound && !s.JobFound {
		switch s.RayJobStatus {
		case "Complete", "Failed":
			state = "finished"
		}
	}
	return fmt.Sprintf("%s (%s)", fmtDuration(end.Sub(started)), state)
}

func queueWaitSeconds(s Snapshot) (int64, bool) {
	created := s.JobCreatedAt
	started := s.JobStartedAt
	found := s.JobFound || s.RayJobFound
	if s.RayJobFound && !s.JobFound {
		created = s.RayJobCreatedAt
		started = s.RayJobStartedAt
	}
	if !found || created.IsZero() || started.IsZero() {
		return 0, false
	}
	d := started.Sub(created)
	if d < 0 {
		d = 0
	}
	return int64(d.Seconds()), true
}

func runtimeSeconds(s Snapshot) (int64, bool) {
	started := s.JobStartedAt
	finished := s.JobFinishedAt
	found := s.JobFound || s.RayJobFound
	if s.RayJobFound && !s.JobFound {
		started = s.RayJobStartedAt
		finished = s.RayJobFinishedAt
	}
	if !found || started.IsZero() {
		return 0, false
	}
	end := finished
	if end.IsZero() {
		end = time.Now()
	}
	d := end.Sub(started)
	if d < 0 {
		d = 0
	}
	return int64(d.Seconds()), true
}

func secondsOrUnavailable(seconds int64, ok bool) string {
	if !ok {
		return "not collected"
	}
	return strconv.FormatInt(seconds, 10)
}

func workloadNames(s Snapshot) string {
	if len(s.Workloads) == 0 {
		return "not collected"
	}
	names := make([]string, 0, len(s.Workloads))
	for _, w := range s.Workloads {
		if w.Name != "" {
			names = append(names, w.Name)
		}
	}
	if len(names) == 0 {
		return "not collected"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func workloadPhases(s Snapshot) string {
	if len(s.Workloads) == 0 {
		return "not collected"
	}
	parts := make([]string, 0, len(s.Workloads))
	for _, w := range s.Workloads {
		phase := dash(w.Phase)
		if w.Name != "" {
			phase = w.Name + "=" + phase
		}
		parts = append(parts, phase)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func workloadAdmitted(s Snapshot) string {
	if len(s.Workloads) == 0 {
		return "not collected"
	}
	parts := make([]string, 0, len(s.Workloads))
	for _, w := range s.Workloads {
		value := fmt.Sprintf("%t", w.Admitted)
		if w.Name != "" {
			value = w.Name + "=" + value
		}
		parts = append(parts, value)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func preemptionStatus(s Snapshot) string {
	var statuses []string
	for _, w := range s.Workloads {
		if w.Preemption == "" {
			continue
		}
		value := w.Preemption
		if w.Name != "" {
			value = w.Name + "=" + value
		}
		statuses = append(statuses, value)
	}
	if len(statuses) == 0 {
		return "not collected"
	}
	sort.Strings(statuses)
	return strings.Join(statuses, ", ")
}

func nodesSummary(s Snapshot) string {
	if len(s.Pods) == 0 {
		return "not assigned yet"
	}
	seen := map[string]bool{}
	for _, p := range s.Pods {
		if p.Node != "" {
			seen[p.Node] = true
		}
	}
	if len(seen) == 0 {
		return "not assigned yet"
	}
	nodes := make([]string, 0, len(seen))
	for n := range seen {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return strings.Join(nodes, ", ")
}

func podNames(s Snapshot) string {
	if len(s.Pods) == 0 {
		return "not collected"
	}
	names := make([]string, 0, len(s.Pods))
	for _, p := range s.Pods {
		if p.Name != "" {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func podUIDs(s Snapshot) string {
	var uids []string
	for _, p := range s.Pods {
		if p.UID != "" {
			uids = append(uids, p.UID)
		}
	}
	if len(uids) == 0 {
		return "not collected"
	}
	sort.Strings(uids)
	return strings.Join(uids, ", ")
}

func resourceClaims(s Snapshot) string {
	seen := map[string]bool{}
	var claims []string
	for _, p := range s.Pods {
		for _, claim := range p.ResourceClaims {
			if claim == "" || seen[claim] {
				continue
			}
			seen[claim] = true
			claims = append(claims, claim)
		}
	}
	if len(claims) == 0 {
		return "not collected"
	}
	sort.Strings(claims)
	return strings.Join(claims, ", ")
}

func totalRestarts(s Snapshot) int {
	total := 0
	for _, p := range s.Pods {
		total += p.Restarts
	}
	return total
}

func podRestartsSummary(s Snapshot) string {
	if len(s.Pods) == 0 {
		return "not collected"
	}
	return strconv.Itoa(totalRestarts(s))
}

func anyOOMKilled(s Snapshot) bool {
	for _, p := range s.Pods {
		if p.OOMKilled {
			return true
		}
	}
	return false
}

func oomKilledSummary(s Snapshot) string {
	if len(s.Pods) == 0 {
		return "not collected"
	}
	if anyOOMKilled(s) {
		return "true"
	}
	return "false"
}

func containerReasons(s Snapshot) string {
	seen := map[string]bool{}
	var reasons []string
	for _, p := range s.Pods {
		if p.ContainerReason == "" || seen[p.ContainerReason] {
			continue
		}
		seen[p.ContainerReason] = true
		reasons = append(reasons, p.ContainerReason)
	}
	if len(reasons) == 0 {
		return "not collected"
	}
	sort.Strings(reasons)
	return strings.Join(reasons, ", ")
}

func exitCodes(s Snapshot) string {
	seen := map[int32]bool{}
	var codes []int
	for _, p := range s.Pods {
		if p.ExitCode == nil || seen[*p.ExitCode] {
			continue
		}
		seen[*p.ExitCode] = true
		codes = append(codes, int(*p.ExitCode))
	}
	if len(codes) == 0 {
		return "not collected"
	}
	sort.Ints(codes)
	values := make([]string, len(codes))
	for i, code := range codes {
		values[i] = strconv.Itoa(code)
	}
	return strings.Join(values, ", ")
}

func shapeSummary(c CostProfile) string {
	if c.Error != "" {
		return "not collected (" + c.Error + ")"
	}
	if c.GPUType == "" || c.GPUsPerPod <= 0 || c.Pods <= 0 {
		return "not priced (profile lacks cost info or GPU shape not resolved)"
	}
	return fmt.Sprintf("%d pod(s) x %d x %s = %d GPU(s)", c.Pods, c.GPUsPerPod, c.GPUType, c.Pods*c.GPUsPerPod)
}

func podStartLatency(s Snapshot) string {
	if s.JobCreatedAt.IsZero() || len(s.Pods) == 0 {
		return "not collected"
	}
	var first time.Time
	for _, p := range s.Pods {
		if p.StartedAt.IsZero() {
			continue
		}
		if first.IsZero() || p.StartedAt.Before(first) {
			first = p.StartedAt
		}
	}
	if first.IsZero() {
		return "not collected"
	}
	return fmtDuration(first.Sub(s.JobCreatedAt))
}

func costSummary(c CostProfile) string {
	if c.Error != "" {
		return "not collected (" + c.Error + ")"
	}
	if c.UsdPerHour <= 0 {
		return "not priced (profile lacks cost info)"
	}
	return fmt.Sprintf("$%.2f total so far/actual, $%.2f/hr, %.2f hr", c.TotalUsd, c.UsdPerHour, c.Hours)
}

func gpuHoursSummary(c CostProfile) string {
	if c.Error != "" {
		return "not collected (" + c.Error + ")"
	}
	if c.Hours <= 0 {
		return "not collected"
	}
	gpus := c.GPUsPerPod * c.Pods
	if gpus <= 0 {
		return "not collected"
	}
	return fmt.Sprintf("%.2f", c.Hours*float64(gpus))
}

func estimatedCostSummary(c CostProfile) string {
	if c.Error != "" {
		return "not collected (" + c.Error + ")"
	}
	if c.TotalUsd <= 0 {
		return "not priced"
	}
	return fmt.Sprintf("$%.2f", c.TotalUsd)
}

func workloadQueue(s Snapshot) string {
	if len(s.Workloads) == 0 {
		return ""
	}
	return s.Workloads[0].Queue
}

func label(s Snapshot, key string) string {
	if s.Labels == nil {
		return ""
	}
	return s.Labels[key]
}

func annotationOrDefault(s Snapshot, key, fallback string) string {
	if s.Annotations == nil {
		return fallback
	}
	if v := s.Annotations[key]; v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	default:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
