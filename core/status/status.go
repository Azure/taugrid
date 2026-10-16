// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type Snapshot struct {
	Name      string
	Namespace string

	Labels      map[string]string
	Annotations map[string]string

	JobFound       bool
	JobSuspended   bool
	JobActive      int
	JobSucceeded   int
	JobFailed      int
	JobConditions  []Condition
	JobAgeSeconds  int64
	JobUID         string
	JobCreatedAt   time.Time
	JobStartedAt   time.Time
	JobFinishedAt  time.Time
	JobParallelism int
	JobManagedBy   string

	RayJob           RayJob
	RayJobFound      bool
	RayJobStatus     string
	RayJobReason     string
	RayClusterName   string
	RayJobID         string
	RayJobStartedAt  time.Time
	RayJobCreatedAt  time.Time
	RayJobFinishedAt time.Time

	Workloads []Workload

	Pods         []Pod
	PodsObserved bool

	ResourceClaims []ResourceClaim

	GPURuntime GPURuntimeEvidence

	Events []Event
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type Workload struct {
	Name       string `json:"name"`
	Queue      string `json:"queue,omitempty"`
	Admitted   bool   `json:"admitted"`
	Phase      string `json:"phase"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	Preemption string `json:"preemption,omitempty"`

	ClusterName           string           `json:"clusterName,omitempty"`
	NominatedClusterNames []string         `json:"nominatedClusterNames,omitempty"`
	AdmissionChecks       []AdmissionCheck `json:"admissionChecks,omitempty"`
}

func (w Workload) waiting() bool {
	return !w.Admitted && !strings.EqualFold(w.Phase, "Finished")
}

type AdmissionCheck struct {
	Name                   string    `json:"name"`
	State                  string    `json:"state"`
	Message                string    `json:"message,omitempty"`
	LastTransitionTime     time.Time `json:"lastTransitionTime,omitempty,omitzero"`
	ControllerName         string    `json:"controllerName,omitempty"`
	ControllerLookupFailed bool      `json:"controllerLookupFailed,omitempty"`
}

type Pod struct {
	Name            string      `json:"name"`
	UID             string      `json:"uid,omitempty"`
	CreatedAt       time.Time   `json:"createdAt,omitempty,omitzero"`
	Phase           string      `json:"phase"`
	Node            string      `json:"node,omitempty"`
	Restarts        int         `json:"restarts"`
	Ready           string      `json:"ready"`
	StartedAt       time.Time   `json:"startedAt,omitempty,omitzero"`
	ResourceClaims  []string    `json:"resourceClaims,omitempty"`
	ContainerReason string      `json:"containerReason,omitempty"`
	ExitCode        *int32      `json:"exitCode,omitempty"`
	OOMKilled       bool        `json:"oomKilled,omitempty"`
	Conditions      []Condition `json:"conditions,omitempty"`
	InitContainers  []Container `json:"initContainers,omitempty"`
	Containers      []Container `json:"containers,omitempty"`
}

type Container struct {
	Name         string    `json:"name"`
	Image        string    `json:"image,omitempty"`
	Ready        bool      `json:"ready"`
	RestartCount int       `json:"restartCount"`
	State        string    `json:"state"`
	Reason       string    `json:"reason,omitempty"`
	Message      string    `json:"message,omitempty"`
	LastReason   string    `json:"lastReason,omitempty"`
	LastMessage  string    `json:"lastMessage,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitempty,omitzero"`
	FinishedAt   time.Time `json:"finishedAt,omitempty,omitzero"`
	ExitCode     *int32    `json:"exitCode,omitempty"`
	LastExitCode *int32    `json:"lastExitCode,omitempty"`
}

type ResourceClaim struct {
	Name        string      `json:"name"`
	CreatedAt   time.Time   `json:"createdAt,omitempty,omitzero"`
	Allocated   bool        `json:"allocated"`
	Allocation  string      `json:"allocation,omitempty"`
	Conditions  []Condition `json:"conditions,omitempty"`
	LastReason  string      `json:"lastReason,omitempty"`
	LastMessage string      `json:"lastMessage,omitempty"`
}

type Event struct {
	InvolvedKind string    `json:"involvedKind"`
	InvolvedName string    `json:"involvedName"`
	Type         string    `json:"type"`
	Reason       string    `json:"reason,omitempty"`
	Message      string    `json:"message,omitempty"`
	Count        int       `json:"count"`
	FirstSeen    time.Time `json:"firstSeen,omitempty,omitzero"`
	LastSeen     time.Time `json:"lastSeen,omitempty,omitzero"`
}

type RayJob struct {
	Found               bool        `json:"found"`
	Name                string      `json:"name"`
	UID                 string      `json:"uid,omitempty"`
	CreatedAt           time.Time   `json:"createdAt,omitempty,omitzero"`
	StartedAt           time.Time   `json:"startedAt,omitempty,omitzero"`
	FinishedAt          time.Time   `json:"finishedAt,omitempty,omitzero"`
	RayClusterName      string      `json:"rayClusterName,omitempty"`
	JobID               string      `json:"jobId,omitempty"`
	JobDeploymentStatus string      `json:"jobDeploymentStatus,omitempty"`
	JobStatus           string      `json:"jobStatus,omitempty"`
	RayClusterStatus    string      `json:"rayClusterStatus,omitempty"`
	Reason              string      `json:"reason,omitempty"`
	Message             string      `json:"message,omitempty"`
	Conditions          []Condition `json:"conditions,omitempty"`
	ManagedBy           string      `json:"managedBy,omitempty"`
}

type LifecycleState string

const (
	LifecycleNotFound  LifecycleState = "not_found"
	LifecycleQueued    LifecycleState = "queued"
	LifecycleStarting  LifecycleState = "starting"
	LifecycleRunning   LifecycleState = "running"
	LifecycleSucceeded LifecycleState = "succeeded"
	LifecycleFailed    LifecycleState = "failed"
)

func RunFound(s Snapshot) bool {
	return s.JobFound || snapshotRayJob(s).Found
}

func CanonicalLifecycleState(s Snapshot) LifecycleState {
	if !RunFound(s) {
		return LifecycleNotFound
	}
	rj := snapshotRayJob(s)
	if StartupFailed(s) {
		return LifecycleFailed
	}
	if primaryWorkloadSucceeded(s) {
		return LifecycleSucceeded
	}
	if s.JobFound {
		if s.JobActive > 0 {
			return LifecycleRunning
		}
		if s.JobSuspended {
			return LifecycleQueued
		}
	} else {
		if strings.EqualFold(rj.JobDeploymentStatus, "Running") ||
			strings.EqualFold(rj.JobStatus, "Running") {
			return LifecycleRunning
		}
		if strings.EqualFold(rj.JobDeploymentStatus, "Suspended") ||
			strings.EqualFold(rj.JobStatus, "Suspended") {
			return LifecycleQueued
		}
	}
	for _, workload := range s.Workloads {
		if !workload.Admitted && !strings.EqualFold(workload.Phase, "Finished") {
			return LifecycleQueued
		}
	}
	return LifecycleStarting
}

func CanonicalRayJob(s Snapshot) RayJob {
	return snapshotRayJob(s)
}

func HeadlineState(s Snapshot) string {
	switch CanonicalLifecycleState(s) {
	case LifecycleNotFound:
		return "NotFound"
	case LifecycleFailed:
		return "Failed"
	case LifecycleSucceeded:
		return "Complete"
	default:
		return deriveState(s)
	}
}

func Render(s Snapshot) string {
	var b strings.Builder
	rj := snapshotRayJob(s)

	if rj.Found && !s.JobFound {
		fmt.Fprintf(&b, "RayJob: %s/%s\n", s.Namespace, s.Name)
		headline := dash(firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus))
		if s.IsMultiKueue() {
			headline = deriveState(s)
		}
		fmt.Fprintf(&b, "  status:    %s\n", headline)
		if rj.RayClusterName != "" {
			fmt.Fprintf(&b, "  cluster:   %s\n", rj.RayClusterName)
		}
		if rj.JobID != "" {
			fmt.Fprintf(&b, "  jobId:     %s\n", rj.JobID)
		}
		if rj.Reason != "" || rj.Message != "" {
			fmt.Fprintf(&b, "  reason:    %s\n", strings.TrimSpace(rj.Reason+" "+rj.Message))
		}
	} else {
		fmt.Fprintf(&b, "Job: %s/%s\n", s.Namespace, s.Name)
	}

	if !s.JobFound && !rj.Found {
		b.WriteString("  (no batch/v1 Job or RayJob found with that name)\n")
	} else if s.JobFound {
		fmt.Fprintf(&b, "  state:     %s\n", deriveState(s))
		fmt.Fprintf(&b, "  pods:      active=%d succeeded=%d failed=%d\n", s.JobActive, s.JobSucceeded, s.JobFailed)
		if len(s.JobConditions) > 0 {
			fmt.Fprintln(&b, "  conditions:")
			for _, c := range s.JobConditions {
				line := fmt.Sprintf("    %s=%s", c.Type, c.Status)
				if c.Reason != "" {
					line += " reason=" + c.Reason
				}
				if c.Message != "" {
					line += " msg=" + c.Message
				}
				fmt.Fprintln(&b, line)
			}
		}
	}

	if rj.Found && s.JobFound {
		fmt.Fprintf(&b, "\nRayJob: %s/%s\n", s.Namespace, rj.Name)
		fmt.Fprintf(&b, "  cluster:   %s\n", dash(rj.RayClusterName))
		fmt.Fprintf(&b, "  job:       %s\n", dash(firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus)))
		if rj.JobID != "" {
			fmt.Fprintf(&b, "  job_id:    %s\n", rj.JobID)
		}
		if rj.RayClusterStatus != "" {
			fmt.Fprintf(&b, "  raycluster:%s\n", " "+rj.RayClusterStatus)
		}
		if rj.Reason != "" || rj.Message != "" {
			fmt.Fprintf(&b, "  reason:    %s\n", strings.TrimSpace(rj.Reason+" "+rj.Message))
		}
	}

	b.WriteString("\n")
	b.WriteString(renderStartupPhasesAt(s, time.Now()))
	if s.ManagerOnlyMultiKueueView() {
		b.WriteString("\n")
		b.WriteString(renderMultiKueueSection(s))
	}

	b.WriteString("\nKueue Workloads:\n")
	if len(s.Workloads) == 0 {
		b.WriteString("  (none — Kueue did not see this workload; check the queue label)\n")
	} else {
		cols := []string{"NAME", "QUEUE", "PHASE", "ADMITTED", "REASON"}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  "+strings.Join(cols, "\t"))
		for _, w := range s.Workloads {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%t\t%s\n", w.Name, w.Queue, w.Phase, w.Admitted, w.Reason)
			if w.waiting() && w.Message != "" {
				fmt.Fprintf(tw, "  %s└─ %s\n", strings.Repeat("\t", len(cols)-1), singleLine(w.Message))
			}
		}
		tw.Flush()
	}

	b.WriteString("\nPods:\n")
	if len(s.Pods) == 0 {
		if s.ManagerOnlyMultiKueueView() {
			b.WriteString("  (none — manager view only; worker-cluster pods are not visible here)\n")
		} else if rayJobStatusSucceeded(rj) {
			b.WriteString("  (none — RayCluster pods cleaned up after successful completion)\n")
		} else if rayJobStatusFailed(rj) && s.PodsObserved {
			b.WriteString("  (none — RayCluster pods cleaned up after terminal failure)\n")
		} else if rayJobStatusFailed(rj) {
			b.WriteString("  (none — pod state unavailable for failed RayJob)\n")
		} else {
			b.WriteString("  (none — workload is suspended or not yet admitted)\n")
		}
	} else {
		sorted := append([]Pod(nil), s.Pods...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Phase != sorted[j].Phase {
				return sorted[i].Phase < sorted[j].Phase
			}
			return sorted[i].Name < sorted[j].Name
		})
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tPHASE\tREADY\tRESTARTS\tNODE")
		for _, p := range sorted {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\n", p.Name, podDisplayPhase(rj, p), p.Ready, p.Restarts, p.Node)
		}
		tw.Flush()
	}

	return b.String()
}

func podDisplayPhase(rj RayJob, p Pod) string {
	if p.Phase == "Failed" && rayJobStatusSucceeded(rj) {
		return "Teardown"
	}
	return p.Phase
}

func renderMultiKueueSection(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintln(&b, "MultiKueue:")
	fmt.Fprintf(&b, "  placement: %s\n", dash(multiKueuePlacementDisplayState(s)))
	if worker := s.PlacementWorkerCluster(); worker != "" {
		fmt.Fprintf(&b, "  worker:    %s\n", worker)
	}
	if nominated := s.NominatedWorkerClusters(); len(nominated) > 0 {
		fmt.Fprintf(&b, "  nominated: %s\n", strings.Join(nominated, ", "))
	}
	checks := s.AdmissionCheckSummaries()
	fmt.Fprintln(&b, "  admission checks:")
	if len(checks) == 0 {
		fmt.Fprintln(&b, "    (none observed)")
		return b.String()
	}
	for _, check := range checks {
		line := fmt.Sprintf("    - %s/%s=%s", check.WorkloadName, check.Name, dash(check.State))
		line += " controller=" + admissionCheckControllerDisplay(check.AdmissionCheck)
		if check.Message != "" {
			line += " msg=" + check.Message
		}
		fmt.Fprintln(&b, line)
	}
	return b.String()
}

func admissionCheckControllerDisplay(check AdmissionCheck) string {
	if strings.TrimSpace(check.ControllerName) != "" {
		return check.ControllerName
	}
	if check.ControllerLookupFailed {
		return "unknown (lookup failed)"
	}
	return "unknown"
}

func multiKueuePlacementDisplayState(s Snapshot) string {
	if state, ok := multiKueueTerminalState(s); ok {
		return state
	}
	return string(s.MultiKueueState())
}

func deriveState(s Snapshot) string {
	if s.ManagerOnlyMultiKueueView() {
		if state, ok := multiKueueTerminalState(s); ok {
			return state
		}
		if state, ok := managerMultiKueueJobState(s); ok {
			return state
		}
		if state, ok := managerMultiKueueRayJobState(primaryRayJob(s)); ok {
			return state
		}
		return string(s.MultiKueueState())
	}
	rj := primaryRayJob(s)
	if rj.Found && !s.JobFound {
		switch firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus) {
		case "Failed":
			return "Failed"
		case "Complete":
			return "Complete"
		case "Running":
			return "Running"
		case "Suspended":
			return "Pending (suspended; Kueue not yet admitted)"
		case "Initializing":
			return "Initializing"
		case "New":
			return "New"
		default:
			return dash(firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus))
		}
	}
	for _, c := range s.JobConditions {
		if c.Type == "Failed" && c.Status == "True" {
			return "Failed"
		}
	}
	for _, c := range s.JobConditions {
		if c.Type == "Complete" && c.Status == "True" {
			return "Complete"
		}
	}
	if s.JobActive > 0 {
		return "Running"
	}
	if s.JobSuspended {
		return "Pending (suspended; Kueue not yet admitted)"
	}
	return "Admitted (no active pods yet)"
}

func managerMultiKueueJobState(s Snapshot) (string, bool) {
	if !s.JobFound {
		return "", false
	}
	if s.JobActive > 0 {
		return "Running", true
	}
	return "", false
}

func managerMultiKueueRayJobState(rj RayJob) (string, bool) {
	if !rj.Found {
		return "", false
	}
	switch firstNonEmpty(rj.JobDeploymentStatus, rj.JobStatus) {
	case "Running":
		return "Running", true
	case "Suspended":
		return "Pending (suspended; Kueue not yet admitted)", true
	case "Initializing":
		return "Initializing", true
	case "New":
		return "New", true
	}
	return "", false
}

func multiKueueTerminalState(s Snapshot) (string, bool) {
	for _, c := range s.JobConditions {
		if c.Type == "Failed" && c.Status == "True" {
			return "Failed", true
		}
	}
	rj := primaryRayJob(s)
	if rayJobStatusFailed(rj) {
		return "Failed", true
	}
	for _, c := range s.JobConditions {
		if c.Type == "Complete" && c.Status == "True" {
			return "Complete", true
		}
	}
	if rayJobStatusSucceeded(rj) {
		return "Complete", true
	}
	return "", false
}
