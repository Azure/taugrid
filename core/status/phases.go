// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type PhaseStatus string

const (
	phasePending PhaseStatus = "pending"
	phaseActive  PhaseStatus = "active"
	phaseDone    PhaseStatus = "done"
	phaseWarning PhaseStatus = "warning"
	phaseSkipped PhaseStatus = "skipped"

	PhasePending = phasePending
	PhaseActive  = phaseActive
	PhaseDone    = phaseDone
	PhaseWarning = phaseWarning
	PhaseSkipped = phaseSkipped
)

type Phase struct {
	Name      string      `json:"name"`
	Status    PhaseStatus `json:"status"`
	Detail    string      `json:"detail"`
	Hint      string      `json:"hint,omitempty"`
	StartedAt time.Time   `json:"startedAt,omitempty,omitzero"`
}

// StartupPhases returns the structured lifecycle phases used by the human
// renderer. Machine consumers should use this instead of parsing Render.
func StartupPhases(s Snapshot) []Phase {
	return startupPhasesAt(s, time.Now())
}

func renderStartupPhasesAt(s Snapshot, now time.Time) string {
	phases := startupPhasesAt(s, now)
	var b strings.Builder
	b.WriteString("Startup phases:\n")
	for _, p := range phases {
		fmt.Fprintf(&b, "  %s  %-24s %s", phaseMarker(p.Status), p.Name, dash(p.Detail))
		if !p.StartedAt.IsZero() && p.Status != phasePending && p.Status != phaseSkipped {
			fmt.Fprintf(&b, " (%s)", fmtDuration(now.Sub(p.StartedAt)))
		}
		b.WriteByte('\n')
		if p.Hint != "" {
			fmt.Fprintf(&b, "      tip: %s\n", p.Hint)
		}
	}
	return b.String()
}

func startupPhasesAt(s Snapshot, now time.Time) []Phase {
	rj := primaryRayJob(s)
	phases := []Phase{
		submittedPhase(s),
		kueuePhase(s),
	}
	if s.ManagerOnlyMultiKueueView() {
		phases = append(phases, multiKueuePlacementPhase(s))
	}
	if rj.Found {
		phases = append(phases, rayClusterPhase(s))
	}
	if s.ManagerOnlyMultiKueueView() {
		phases = append(phases, skippedManagerViewPodPhases()...)
	} else {
		phases = append(phases, podLifecyclePhases(s)...)
	}
	if rj.Found {
		phases = append(phases, rayJobPhase(s))
		phases = append(phases, computeReleasePhase(s))
	}
	return phases
}

func podLifecyclePhases(s Snapshot) []Phase {
	phases := []Phase{
		podSchedulingPhase(s),
		draPhase(s),
		imagePullPhase(s),
		initPhase(s),
		containerStartPhase(s),
		readyPhase(s),
	}
	rj := primaryRayJob(s)
	if rayJobStatusFailed(rj) && len(s.Pods) == 0 {
		detail := "pod state unavailable after terminal RayJob failure"
		if s.PodsObserved {
			detail = "RayJob failed; post-run RayCluster teardown removed pod evidence"
		}
		for i := range phases {
			if phases[i].Status == phaseWarning {
				continue
			}
			phases[i].Status = phaseSkipped
			phases[i].Detail = detail
			phases[i].Hint = ""
		}
		return phases
	}
	if !rayJobStatusSucceeded(rj) {
		return phases
	}
	for i := range phases {
		if phases[i].Status == phaseDone || phases[i].Status == phaseSkipped {
			continue
		}
		phases[i].Status = phaseDone
		phases[i].Detail = "RayJob completed successfully; post-run RayCluster teardown"
		phases[i].Hint = ""
	}
	return phases
}

func StartupComplete(s Snapshot) bool {
	if primaryWorkloadSucceeded(s) {
		return true
	}
	if s.ManagerOnlyMultiKueueView() {
		return false
	}
	for _, phase := range startupPhasesAt(s, time.Now()) {
		if phase.Status != phaseDone && phase.Status != phaseSkipped {
			return false
		}
	}
	return true
}

func StartupFailed(s Snapshot) bool {
	if primaryWorkloadFailed(s) {
		return true
	}
	if primaryWorkloadSucceeded(s) {
		return false
	}
	if s.ManagerOnlyMultiKueueView() && s.AnyAdmissionCheckRejected() {
		return true
	}
	if s.JobFound || snapshotRayJob(s).Found {
		for _, p := range s.Pods {
			for _, c := range append(append([]Container{}, p.InitContainers...), p.Containers...) {
				if c.State == "waiting" && isContainerFailureReason(c.Reason) {
					return true
				}
				if c.State == "waiting" && isImagePullFailureReason(c.Reason) {
					return true
				}
			}
		}
		return false
	}
	for _, p := range s.Pods {
		if p.Phase == "Failed" {
			return true
		}
		for _, c := range append(append([]Container{}, p.InitContainers...), p.Containers...) {
			if c.State == "terminated" && c.ExitCode != nil && *c.ExitCode != 0 {
				return true
			}
			if c.State == "waiting" && isContainerFailureReason(c.Reason) {
				return true
			}
			if c.State == "waiting" && isImagePullFailureReason(c.Reason) {
				return true
			}
		}
	}
	return false
}

func WatchComplete(s Snapshot) bool {
	if primaryWorkloadFailed(s) {
		return false
	}
	if primaryWorkloadSucceeded(s) {
		return true
	}
	if s.ManagerOnlyMultiKueueView() {
		if rj := primaryRayJob(s); firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus) == "Suspended" {
			return false
		}
		if state, ok := managerMultiKueueJobState(s); ok && state == "Running" {
			return true
		}
		if state, ok := managerMultiKueueRayJobState(primaryRayJob(s)); ok && state == "Running" {
			return true
		}
		return s.AllAdmissionChecksReady()
	}
	return StartupComplete(s)
}

func WatchFailed(s Snapshot) bool {
	if primaryWorkloadFailed(s) {
		return true
	}
	if primaryWorkloadSucceeded(s) {
		return false
	}
	if s.ManagerOnlyMultiKueueView() {
		return s.AnyAdmissionCheckRejected()
	}
	return StartupFailed(s)
}

func multiKueuePlacementPhase(s Snapshot) Phase {
	if state, ok := multiKueueTerminalState(s); ok {
		status := phaseDone
		detail := "workload completed"
		if state == "Failed" {
			status = phaseWarning
			detail = "workload failed"
		}
		return Phase{
			Name:      "MultiKueue placement",
			Status:    status,
			Detail:    detail,
			StartedAt: firstAdmissionCheckTransitionTime(s),
		}
	}
	state := s.MultiKueueState()
	detail := "waiting for worker-cluster placement"
	status := phaseActive
	switch state {
	case MultiKueueStateRejected:
		status = phaseWarning
		detail = firstAdmissionCheckMessage(s, state, "worker-cluster placement rejected")
	case MultiKueueStateRetry:
		detail = firstAdmissionCheckMessage(s, state, "retrying worker-cluster placement")
	case MultiKueueStateReady:
		status = phaseDone
		detail = "worker-cluster placement ready"
		if worker := s.PlacementWorkerCluster(); worker != "" {
			detail += " on " + worker
		}
	case MultiKueueStateSelected:
		detail = "selected worker cluster " + dash(s.PlacementWorkerCluster())
	case MultiKueueStateNominated:
		detail = "nominated worker clusters: " + strings.Join(s.NominatedWorkerClusters(), ", ")
	case MultiKueueStatePending:
		if msg := firstAdmissionCheckMessage(s, state, "waiting for worker-cluster placement"); msg != "" {
			detail = msg
		}
	}
	return Phase{
		Name:      "MultiKueue placement",
		Status:    status,
		Detail:    detail,
		StartedAt: firstAdmissionCheckTransitionTime(s),
	}
}

func skippedManagerViewPodPhases() []Phase {
	explanation := "manager view only; worker-cluster pod lifecycle is not visible here"
	return []Phase{
		{Name: "Pod scheduling", Status: phaseSkipped, Detail: explanation},
		{Name: "DRA allocation", Status: phaseSkipped, Detail: explanation},
		{Name: "Image pull", Status: phaseSkipped, Detail: explanation},
		{Name: "Init containers", Status: phaseSkipped, Detail: explanation},
		{Name: "Container start", Status: phaseSkipped, Detail: explanation},
		{Name: "Ready", Status: phaseSkipped, Detail: explanation},
	}
}

func firstAdmissionCheckMessage(s Snapshot, state MultiKueueState, fallback string) string {
	for _, check := range s.AdmissionCheckSummaries() {
		if !isMultiKueueAdmissionCheck(check.AdmissionCheck) {
			continue
		}
		if strings.EqualFold(check.State, string(state)) && strings.TrimSpace(check.Message) != "" {
			return check.Message
		}
	}
	return fallback
}

func firstAdmissionCheckTransitionTime(s Snapshot) time.Time {
	var first time.Time
	for _, check := range s.AdmissionCheckSummaries() {
		if !isMultiKueueAdmissionCheck(check.AdmissionCheck) {
			continue
		}
		first = earlierNonZero(first, check.LastTransitionTime)
	}
	return earlierNonZero(first, firstWorkloadEventTime(s))
}

func submittedPhase(s Snapshot) Phase {
	rj := snapshotRayJob(s)
	if s.JobFound {
		return Phase{
			Name:      "Submitted",
			Status:    phaseDone,
			Detail:    "batch/v1 Job created",
			StartedAt: s.JobCreatedAt,
		}
	}
	if rj.Found {
		return Phase{
			Name:      "Submitted",
			Status:    phaseDone,
			Detail:    "RayJob created",
			StartedAt: rj.CreatedAt,
		}
	}
	return Phase{
		Name:   "Submitted",
		Status: phasePending,
		Detail: "no batch/v1 Job or RayJob found",
	}
}

func kueuePhase(s Snapshot) Phase {
	if len(s.Workloads) == 0 {
		rj := snapshotRayJob(s)
		status := phasePending
		detail := "waiting for Kueue Workload"
		if s.JobFound || rj.Found {
			status = phaseActive
		} else {
			detail = "workload not observed"
		}
		return Phase{
			Name:   "Kueue admission",
			Status: status,
			Detail: detail,
			Hint:   "check the kueue.x-k8s.io/queue-name label if this stays empty",
		}
	}
	admitted, finished, pending, warning := 0, 0, 0, ""
	queues := map[string]bool{}
	for _, w := range s.Workloads {
		if w.Queue != "" {
			queues[w.Queue] = true
		}
		if strings.EqualFold(w.Phase, "Finished") {
			finished++
			continue
		}
		if w.Admitted {
			admitted++
			continue
		}
		pending++
		if warning == "" {
			warning = firstNonEmpty(w.Preemption, w.Reason)
		}
	}
	detail := fmt.Sprintf("%d/%d admitted", admitted, len(s.Workloads))
	if finished == len(s.Workloads) {
		detail = fmt.Sprintf("%d/%d finished; quota released", finished, len(s.Workloads))
	}
	if q := sortedJoinMapKeys(queues); q != "" {
		detail += " queue=" + q
	}
	if admitted+finished == len(s.Workloads) {
		return Phase{Name: "Kueue admission", Status: phaseDone, Detail: detail, StartedAt: firstWorkloadEventTime(s)}
	}
	status := phaseActive
	hint := ""
	if warning != "" {
		detail += " reason=" + warning
		if strings.Contains(strings.ToLower(warning), "quota") {
			hint = "quota is reserved per LocalQueue/ClusterQueue; check queue capacity and borrowing"
		}
	}
	if pending == len(s.Workloads) && hasEventReason(s, "Evicted", "Preempted") {
		status = phaseWarning
		hint = "Kueue reported eviction/preemption; check Workload conditions"
	}
	return Phase{Name: "Kueue admission", Status: status, Detail: detail, Hint: hint, StartedAt: firstWorkloadEventTime(s)}
}

func rayClusterPhase(s Snapshot) Phase {
	rj := snapshotRayJob(s)
	if rj.RayClusterName == "" {
		return Phase{
			Name:      "RayCluster",
			Status:    phaseActive,
			Detail:    "waiting for KubeRay to create RayCluster",
			StartedAt: rj.CreatedAt,
		}
	}
	detail := "created " + rj.RayClusterName
	if rj.RayClusterStatus != "" {
		detail += " status=" + rj.RayClusterStatus
	}
	status := phaseDone
	if rayJobStatusFailed(rj) {
		status = phaseWarning
	}
	return Phase{Name: "RayCluster", Status: status, Detail: detail, StartedAt: rj.CreatedAt}
}

func computeReleasePhase(s Snapshot) Phase {
	rj := snapshotRayJob(s)
	if !rayJobStatusSucceeded(rj) && !rayJobStatusFailed(rj) {
		return Phase{
			Name:      "Compute release",
			Status:    phasePending,
			Detail:    "waiting for RayJob terminal status",
			StartedAt: rj.CreatedAt,
		}
	}
	if s.ManagerOnlyMultiKueueView() {
		return Phase{
			Name:      "Compute release",
			Status:    phaseSkipped,
			Detail:    "manager view only; Kueue quota can be released before worker-cluster GPUs are reusable",
			Hint:      "check worker-cluster RayCluster and pod teardown before resubmitting into constrained capacity",
			StartedAt: rj.FinishedAt,
		}
	}
	if !s.PodsObserved && len(s.Pods) == 0 {
		return Phase{
			Name:      "Compute release",
			Status:    phaseWarning,
			Detail:    "Ray pod state unavailable; physical resource reusability unknown",
			Hint:      "retry status with permission to list pods before resubmitting into constrained capacity",
			StartedAt: rj.FinishedAt,
		}
	}

	activePods, nodes := activeRayCompute(s.Pods)
	quotaReleased := allWorkloadsFinished(s.Workloads)
	if activePods > 0 {
		detail := fmt.Sprintf("%d Ray pod(s) still hold %d node(s)", activePods, len(nodes))
		if len(nodes) > 0 {
			detail += ": " + strings.Join(nodes, ", ")
		}
		if quotaReleased {
			detail = "quota released; resources NOT reusable; " + detail
		} else {
			detail = "resources NOT reusable; " + detail
		}
		return Phase{
			Name:      "Compute release",
			Status:    phaseActive,
			Detail:    detail,
			Hint:      "wait for KubeRay to delete the RayCluster and for all node-bound Ray pods to terminate",
			StartedAt: rj.FinishedAt,
		}
	}

	detail := "resources reusable; no active Ray pods remain"
	status := phaseDone
	if quotaReleased {
		detail = "quota released; " + detail
	} else if len(s.Workloads) > 0 {
		status = phaseActive
		detail += "; waiting for Kueue quota release"
	}
	return Phase{
		Name:      "Compute release",
		Status:    status,
		Detail:    detail,
		StartedAt: rj.FinishedAt,
	}
}

func allWorkloadsFinished(workloads []Workload) bool {
	if len(workloads) == 0 {
		return false
	}
	for _, workload := range workloads {
		if !strings.EqualFold(workload.Phase, "Finished") {
			return false
		}
	}
	return true
}

func activeRayCompute(pods []Pod) (int, []string) {
	nodes := map[string]bool{}
	active := 0
	for _, pod := range pods {
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
			continue
		}
		active++
		if pod.Node != "" {
			nodes[pod.Node] = true
		}
	}
	names := make([]string, 0, len(nodes))
	for node := range nodes {
		names = append(names, node)
	}
	sort.Strings(names)
	return active, names
}

func podSchedulingPhase(s Snapshot) Phase {
	if len(s.Pods) == 0 {
		return Phase{
			Name:   "Pod scheduling",
			Status: phasePending,
			Detail: "no pods observed yet",
		}
	}
	assigned := 0
	for _, p := range s.Pods {
		if p.Node != "" {
			assigned++
		}
	}
	detail := fmt.Sprintf("%d/%d assigned to nodes", assigned, len(s.Pods))
	if assigned == len(s.Pods) {
		return Phase{Name: "Pod scheduling", Status: phaseDone, Detail: detail, StartedAt: firstPodCreatedAt(s)}
	}
	hint := ""
	status := phaseActive
	if reason := podSchedulingBlockReason(s); reason != "" {
		status = phaseWarning
		detail += " reason=" + reason
		hint = "check node selectors, queue admission, and DRA ResourceClaim allocation"
	}
	return Phase{Name: "Pod scheduling", Status: status, Detail: detail, Hint: hint, StartedAt: firstPodCreatedAt(s)}
}

func draPhase(s Snapshot) Phase {
	claimNames := uniqueResourceClaimNames(s)
	if len(claimNames) == 0 {
		return Phase{Name: "DRA allocation", Status: phaseSkipped, Detail: "no ResourceClaims requested"}
	}
	if len(s.ResourceClaims) == 0 {
		return Phase{
			Name:   "DRA allocation",
			Status: phaseActive,
			Detail: fmt.Sprintf("%d ResourceClaim(s) referenced; allocation not observed", len(claimNames)),
			Hint:   "if this stays pending, check ResourceClaim objects and ResourceSlice availability",
		}
	}
	allocated := 0
	var detailParts []string
	var blocked string
	claimsByName := map[string]ResourceClaim{}
	for _, c := range s.ResourceClaims {
		claimsByName[c.Name] = c
	}
	for _, name := range claimNames {
		c, ok := claimsByName[name]
		if !ok {
			detailParts = append(detailParts, name+"=not observed")
			continue
		}
		if c.Allocated {
			allocated++
		}
		part := c.Name
		if c.Allocation != "" {
			part += "=" + c.Allocation
		} else if c.Allocated {
			part += "=allocated"
		} else {
			part += "=allocating"
		}
		detailParts = append(detailParts, part)
		if blocked == "" {
			blocked = firstNonEmpty(c.LastReason, resourceClaimConditionReason(c), resourceClaimEventReason(s, c.Name))
		}
	}
	sort.Strings(detailParts)
	detail := fmt.Sprintf("%d/%d allocated", allocated, len(claimNames))
	if len(detailParts) > 0 {
		detail += " " + strings.Join(detailParts, ", ")
	}
	if allocated == len(claimNames) {
		return Phase{Name: "DRA allocation", Status: phaseDone, Detail: detail, StartedAt: firstResourceClaimCreatedAt(s)}
	}
	status := phaseActive
	hint := "ResourceClaim allocation >30s often means no matching GPU device; check ResourceSlices for the pool"
	if blocked != "" {
		detail += " reason=" + blocked
	}
	if hasResourceClaimFailure(s) {
		status = phaseWarning
	}
	return Phase{Name: "DRA allocation", Status: status, Detail: detail, Hint: hint, StartedAt: firstResourceClaimCreatedAt(s)}
}

func imagePullPhase(s Snapshot) Phase {
	if len(s.Pods) == 0 {
		return Phase{Name: "Image pull", Status: phasePending, Detail: "waiting for pods"}
	}
	if reason := imagePullFailureReason(s); reason != "" {
		return Phase{
			Name:      "Image pull",
			Status:    phaseWarning,
			Detail:    reason,
			Hint:      "verify the image name, tag, registry credentials, and node egress",
			StartedAt: firstImagePullEventTime(s),
		}
	}
	if msg := latestEventMessage(s, "Pulling"); msg != "" && !allPodsPastImagePull(s) {
		return Phase{
			Name:      "Image pull",
			Status:    phaseActive,
			Detail:    trimMessage(msg),
			Hint:      "byte-level pull progress is not exposed by kubelet events",
			StartedAt: firstImagePullEventTime(s),
		}
	}
	if allPodsPastImagePull(s) {
		return Phase{Name: "Image pull", Status: phaseDone, Detail: pulledImagesSummary(s), StartedAt: firstImagePullEventTime(s)}
	}
	return Phase{Name: "Image pull", Status: phaseActive, Detail: "waiting for kubelet image pull events", StartedAt: firstPodCreatedAt(s)}
}

func initPhase(s Snapshot) Phase {
	total := 0
	done := 0
	var blocked string
	for _, p := range s.Pods {
		total += len(p.InitContainers)
		for _, c := range p.InitContainers {
			if c.State == "terminated" && c.ExitCode != nil && *c.ExitCode == 0 {
				done++
				continue
			}
			if blocked == "" {
				blocked = firstNonEmpty(c.Reason, c.Message)
			}
		}
	}
	if total == 0 {
		return Phase{Name: "Init containers", Status: phaseSkipped, Detail: "no init containers"}
	}
	detail := fmt.Sprintf("%d/%d complete", done, total)
	if done == total {
		return Phase{Name: "Init containers", Status: phaseDone, Detail: detail, StartedAt: firstPodCreatedAt(s)}
	}
	status := phaseActive
	hint := ""
	if blocked != "" {
		detail += " reason=" + blocked
		if strings.Contains(strings.ToLower(blocked), "mount") {
			hint = "storage or blobfuse mount setup is still blocking init"
		}
	}
	return Phase{Name: "Init containers", Status: status, Detail: detail, Hint: hint, StartedAt: firstPodCreatedAt(s)}
}

func containerStartPhase(s Snapshot) Phase {
	if len(s.Pods) == 0 {
		return Phase{Name: "Container start", Status: phasePending, Detail: "waiting for pods"}
	}
	started, total := 0, 0
	var blocked string
	status := phaseActive
	for _, p := range s.Pods {
		for _, c := range p.Containers {
			total++
			switch c.State {
			case "running":
				started++
			case "terminated":
				if c.ExitCode != nil && *c.ExitCode == 0 {
					started++
				} else {
					status = phaseWarning
				}
			case "waiting":
				if blocked == "" {
					blocked = firstNonEmpty(c.Reason, c.Message)
				}
				if isContainerFailureReason(c.Reason) {
					status = phaseWarning
				}
			}
		}
	}
	if total == 0 {
		return Phase{Name: "Container start", Status: phasePending, Detail: "container statuses not available"}
	}
	detail := fmt.Sprintf("%d/%d started", started, total)
	if started == total {
		return Phase{Name: "Container start", Status: phaseDone, Detail: detail, StartedAt: firstPodCreatedAt(s)}
	}
	hint := ""
	if blocked != "" {
		detail += " reason=" + blocked
		if strings.EqualFold(blocked, "ContainerCreating") || strings.EqualFold(blocked, "PodInitializing") {
			hint = "container startup is still waiting on image, volume, or runtime setup"
		}
	}
	return Phase{Name: "Container start", Status: status, Detail: detail, Hint: hint, StartedAt: firstPodCreatedAt(s)}
}

func readyPhase(s Snapshot) Phase {
	if len(s.Pods) == 0 {
		return Phase{Name: "Ready", Status: phasePending, Detail: "waiting for pods"}
	}
	readyPods := 0
	failed := ""
	for _, p := range s.Pods {
		if podReadyOrSucceeded(p) {
			readyPods++
		}
		if failed == "" && podFailed(p) {
			failed = firstNonEmpty(p.ContainerReason, p.Phase)
		}
	}
	detail := fmt.Sprintf("%d/%d pods ready", readyPods, len(s.Pods))
	if readyPods == len(s.Pods) {
		return Phase{Name: "Ready", Status: phaseDone, Detail: detail, StartedAt: firstPodCreatedAt(s)}
	}
	if failed != "" {
		return Phase{Name: "Ready", Status: phaseWarning, Detail: detail + " reason=" + failed, StartedAt: firstPodCreatedAt(s)}
	}
	return Phase{Name: "Ready", Status: phaseActive, Detail: detail, StartedAt: firstPodCreatedAt(s)}
}

func rayJobPhase(s Snapshot) Phase {
	rj := snapshotRayJob(s)
	detail := firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus, "waiting for RayJob status")
	status := phaseActive
	if rayJobStatusFailed(rj) {
		status = phaseWarning
		if reason := firstNonEmpty(rj.Reason, rj.Message); reason != "" {
			detail += " reason=" + reason
		}
	} else if rayJobStatusComplete(rj) {
		status = phaseDone
	} else if strings.EqualFold(detail, "waiting for RayJob status") {
		status = phasePending
	}
	return Phase{Name: "RayJob status", Status: status, Detail: detail, StartedAt: rj.CreatedAt}
}

func phaseMarker(status PhaseStatus) string {
	switch status {
	case phaseDone:
		return "[x]"
	case phaseActive:
		return "[>]"
	case phaseWarning:
		return "[!]"
	case phaseSkipped:
		return "[-]"
	default:
		return "[ ]"
	}
}

func snapshotRayJob(s Snapshot) RayJob {
	rj := s.RayJob
	if rj.Found {
		return rj
	}
	if !s.RayJobFound {
		return RayJob{}
	}
	return RayJob{
		Found:               true,
		Name:                s.Name,
		CreatedAt:           s.RayJobCreatedAt,
		StartedAt:           s.RayJobStartedAt,
		FinishedAt:          s.RayJobFinishedAt,
		RayClusterName:      s.RayClusterName,
		JobID:               s.RayJobID,
		JobDeploymentStatus: s.RayJobStatus,
		Reason:              s.RayJobReason,
	}
}

func primaryRayJob(s Snapshot) RayJob {
	if s.JobFound {
		return RayJob{}
	}
	return snapshotRayJob(s)
}

func sortedJoinMapKeys(values map[string]bool) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func firstWorkloadEventTime(s Snapshot) time.Time {
	names := make(map[string]bool, len(s.Workloads))
	for _, w := range s.Workloads {
		names[w.Name] = true
	}
	return firstEventTimeForNames(s, names)
}

func firstPodCreatedAt(s Snapshot) time.Time {
	var first time.Time
	for _, p := range s.Pods {
		first = earlierNonZero(first, p.CreatedAt)
	}
	return first
}

func firstResourceClaimCreatedAt(s Snapshot) time.Time {
	var first time.Time
	for _, c := range s.ResourceClaims {
		first = earlierNonZero(first, c.CreatedAt)
	}
	return first
}

func firstImagePullEventTime(s Snapshot) time.Time {
	var first time.Time
	for _, e := range s.Events {
		if eventReasonIs(e, "Pulling", "Pulled", "Failed", "ErrImagePull", "ImagePullBackOff") {
			first = earlierNonZero(first, eventTime(e))
		}
	}
	if first.IsZero() {
		return firstPodCreatedAt(s)
	}
	return first
}

func firstEventTimeForNames(s Snapshot, names map[string]bool) time.Time {
	var first time.Time
	for _, e := range s.Events {
		if names[e.InvolvedName] {
			first = earlierNonZero(first, eventTime(e))
		}
	}
	return first
}

func earlierNonZero(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func eventTime(e Event) time.Time {
	if !e.FirstSeen.IsZero() {
		return e.FirstSeen
	}
	return e.LastSeen
}

func hasEventReason(s Snapshot, reasons ...string) bool {
	for _, e := range s.Events {
		if eventReasonIs(e, reasons...) {
			return true
		}
	}
	return false
}

func eventReasonIs(e Event, reasons ...string) bool {
	for _, reason := range reasons {
		if strings.EqualFold(e.Reason, reason) {
			return true
		}
	}
	return false
}

func podSchedulingBlockReason(s Snapshot) string {
	for _, p := range s.Pods {
		for _, c := range p.Conditions {
			if c.Type == "PodScheduled" && c.Status == "False" {
				return firstNonEmpty(c.Reason, c.Message)
			}
		}
	}
	for _, e := range s.Events {
		if eventReasonIs(e, "FailedScheduling", "SchedulingFailed") {
			return firstNonEmpty(e.Reason, e.Message)
		}
	}
	return ""
}

func uniqueResourceClaimNames(s Snapshot) []string {
	seen := map[string]bool{}
	var names []string
	for _, p := range s.Pods {
		for _, claim := range p.ResourceClaims {
			if claim == "" || seen[claim] {
				continue
			}
			seen[claim] = true
			names = append(names, claim)
		}
	}
	sort.Strings(names)
	return names
}

func resourceClaimConditionReason(c ResourceClaim) string {
	for _, cond := range c.Conditions {
		if cond.Status == "False" || cond.Status == "Unknown" {
			return firstNonEmpty(cond.Reason, cond.Message, cond.Type)
		}
	}
	return ""
}

func resourceClaimEventReason(s Snapshot, name string) string {
	for _, e := range s.Events {
		if e.InvolvedName == name && strings.Contains(strings.ToLower(e.Message), "resourceclaim") {
			return firstNonEmpty(e.Reason, e.Message)
		}
	}
	return ""
}

func hasResourceClaimFailure(s Snapshot) bool {
	for _, c := range s.ResourceClaims {
		if c.Allocated {
			continue
		}
		if isAllocationFailureReason(c.LastReason) || isAllocationFailureReason(resourceClaimConditionReason(c)) {
			return true
		}
	}
	for _, e := range s.Events {
		if isAllocationFailureReason(e.Reason) || strings.Contains(strings.ToLower(e.Message), "no suitable devices") {
			return true
		}
	}
	return false
}

func isAllocationFailureReason(reason string) bool {
	reason = strings.ToLower(reason)
	return strings.Contains(reason, "failed") || strings.Contains(reason, "unschedulable")
}

func imagePullFailureReason(s Snapshot) string {
	for _, p := range s.Pods {
		for _, c := range p.Containers {
			if c.State == "waiting" && isImagePullFailureReason(c.Reason) {
				return c.Name + "=" + c.Reason
			}
		}
		for _, c := range p.InitContainers {
			if c.State == "waiting" && isImagePullFailureReason(c.Reason) {
				return c.Name + "=" + c.Reason
			}
		}
	}
	for _, e := range s.Events {
		if isImagePullFailureReason(e.Reason) {
			return trimMessage(firstNonEmpty(e.Message, e.Reason))
		}
	}
	return ""
}

func isImagePullFailureReason(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return true
	default:
		return false
	}
}

func latestEventMessage(s Snapshot, reasons ...string) string {
	var latest Event
	for _, e := range s.Events {
		if !eventReasonIs(e, reasons...) {
			continue
		}
		if latest.Reason == "" || latest.LastSeen.Before(e.LastSeen) {
			latest = e
		}
	}
	return latest.Message
}

func allPodsPastImagePull(s Snapshot) bool {
	for _, p := range s.Pods {
		if p.Phase == "Pending" && p.ContainerReason == "" && len(p.Containers) == 0 {
			return false
		}
		for _, c := range append(append([]Container{}, p.InitContainers...), p.Containers...) {
			if c.State == "waiting" {
				switch c.Reason {
				case "ContainerCreating", "PodInitializing":
					return false
				}
				if isImagePullFailureReason(c.Reason) {
					return false
				}
			}
		}
	}
	return true
}

func pulledImagesSummary(s Snapshot) string {
	images := map[string]bool{}
	for _, p := range s.Pods {
		for _, c := range p.Containers {
			if c.Image != "" {
				images[c.Image] = true
			}
		}
		for _, c := range p.InitContainers {
			if c.Image != "" {
				images[c.Image] = true
			}
		}
	}
	if len(images) == 0 {
		return "images available"
	}
	return "available: " + truncateList(sortedMapKeys(images), 2)
}

func sortedMapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truncateList(values []string, max int) string {
	if len(values) <= max {
		return strings.Join(values, ", ")
	}
	return strings.Join(values[:max], ", ") + fmt.Sprintf(", +%d more", len(values)-max)
}

func isContainerFailureReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "Error":
		return true
	default:
		return false
	}
}

func podReadyOrSucceeded(p Pod) bool {
	if p.Phase == "Succeeded" {
		return true
	}
	for _, c := range p.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	for _, c := range p.Containers {
		if !c.Ready {
			return false
		}
	}
	return len(p.Containers) > 0
}

func podFailed(p Pod) bool {
	if p.Phase == "Failed" {
		return true
	}
	for _, c := range append(append([]Container{}, p.InitContainers...), p.Containers...) {
		if c.State == "terminated" && c.ExitCode != nil && *c.ExitCode != 0 {
			return true
		}
		if c.State == "waiting" && (isContainerFailureReason(c.Reason) || isImagePullFailureReason(c.Reason)) {
			return true
		}
	}
	return false
}

func rayJobStatusComplete(rj RayJob) bool {
	status := strings.ToLower(firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus))
	return strings.Contains(status, "succeed") || strings.Contains(status, "complete") || strings.Contains(status, "running")
}

func WorkloadSucceeded(s Snapshot) bool {
	return primaryWorkloadSucceeded(s)
}

func primaryWorkloadSucceeded(s Snapshot) bool {
	if s.JobFound {
		return jobStatusSucceeded(s)
	}
	return rayJobStatusSucceeded(snapshotRayJob(s))
}

func WorkloadFailed(s Snapshot) bool {
	if primaryWorkloadFailed(s) {
		return true
	}
	return s.ManagerOnlyMultiKueueView() && s.AnyAdmissionCheckRejected()
}

func primaryWorkloadFailed(s Snapshot) bool {
	if s.JobFound {
		return jobStatusFailed(s)
	}
	return rayJobStatusFailed(snapshotRayJob(s))
}

func jobStatusSucceeded(s Snapshot) bool {
	for _, c := range s.JobConditions {
		if c.Type == "Complete" && c.Status == "True" {
			return true
		}
	}
	return false
}

func jobStatusFailed(s Snapshot) bool {
	for _, c := range s.JobConditions {
		if c.Type == "Failed" && c.Status == "True" {
			return true
		}
	}
	return false
}

func rayJobStatusSucceeded(rj RayJob) bool {
	if rayJobStatusFailed(rj) {
		return false
	}
	for _, value := range []string{rj.JobStatus, rj.JobDeploymentStatus} {
		value = strings.ToLower(strings.TrimSpace(value))
		if strings.Contains(value, "succeed") || strings.Contains(value, "complete") {
			return true
		}
	}
	return false
}

func rayJobStatusFailed(rj RayJob) bool {
	for _, value := range []string{rj.JobStatus, rj.JobDeploymentStatus, rj.Reason} {
		value = strings.ToLower(strings.TrimSpace(value))
		if strings.Contains(value, "fail") ||
			strings.Contains(value, "error") ||
			strings.Contains(value, "stop") ||
			strings.Contains(value, "cancel") {
			return true
		}
	}
	return false
}

// singleLine collapses all whitespace runs into single spaces. Kubernetes
// condition messages wrap across lines, and a raw newline would break the
// rendered row it is placed in.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func trimMessage(message string) string {
	message = singleLine(message)
	const max = 120
	if len(message) <= max {
		return message
	}
	return message[:max-3] + "..."
}
