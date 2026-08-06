// Package status fetches and renders the lifecycle of a tau-submitted job.
//
// The shape mirrors the earlier shell CLI's `status` output (Kueue
// workload + Job/RayJob + Pods), but split into two layers:
//
//   - Render: pure function (Snapshot → string). Unit-testable, no kubectl.
//   - Fetch:  shells to kubectl with structured -o jsonpath queries to
//     populate a Snapshot. Integration-testable.
//
// Why split: rendering bugs (missing column, bad ordering) are way more
// common than fetch bugs, and they're the user-facing surface. Cheap unit
// tests on Render catch most regressions.
package status

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Snapshot is the data Render needs to produce the status view.
type Snapshot struct {
	Name      string
	Namespace string

	Labels      map[string]string
	Annotations map[string]string

	// Job (batch/v1) info.
	JobFound       bool
	JobSuspended   bool
	JobActive      int
	JobSucceeded   int
	JobFailed      int
	JobConditions  []Condition // typically Complete, Failed
	JobAgeSeconds  int64
	JobUID         string
	JobCreatedAt   time.Time
	JobStartedAt   time.Time
	JobFinishedAt  time.Time
	JobParallelism int
	// JobManagedBy mirrors spec.managedBy on the batch/v1 Job. It is an
	// immutable opt-in flag set at admission time (not a live routing
	// indicator): Kueue's MultiKueue integration sets it to
	// "kueue.x-k8s.io/multikueue" to declare that the Job is managed by
	// MultiKueue. Actual worker-cluster placement progress is tracked
	// separately via Workload.ClusterName / NominatedClusterNames /
	// AdmissionChecks.
	JobManagedBy string

	// RayJob info. Tau can submit either a batch/v1 Job or a KubeRay RayJob;
	// status keeps both in one snapshot so renderers can show the two-stage
	// RayCluster + submitter lifecycle when present.
	RayJob RayJob
	// RayJob compatibility fields used by run-profile/export paths.
	RayJobFound      bool
	RayJobStatus     string // jobDeploymentStatus: New/Initializing/Running/Complete/Failed/Suspended
	RayJobReason     string
	RayClusterName   string
	RayJobID         string
	RayJobStartedAt  time.Time
	RayJobCreatedAt  time.Time
	RayJobFinishedAt time.Time

	// Kueue Workload(s). Usually one per Job; we tolerate multiples.
	Workloads []Workload

	// Pods carrying job-name=<name>, ray.io/cluster=<clusterName>, or tau.azure.com/job=<name>.
	Pods []Pod
	// PodsObserved is true when at least one pod-list query succeeded, including
	// a successful empty result. Without it, an empty Pods slice may mean RBAC or
	// a transient API failure rather than reusable compute.
	PodsObserved bool

	// DRA ResourceClaims referenced by Pods.
	ResourceClaims []ResourceClaim

	// Events for the Job/RayJob, Kueue Workload, Pods, and ResourceClaims.
	Events []Event
}

type Condition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

type Workload struct {
	Name     string
	Queue    string // local queue name
	Admitted bool
	Phase    string // pending|admitted|finished|...
	Reason   string
	// Message is the condition message explaining why a workload is not
	// admitted. Kueue puts the actionable detail here (which flavors and
	// nodes were excluded, and why) while Reason is often just "Pending".
	Message    string
	Preemption string

	// MultiKueue fields, populated only from manager-visible Workload
	// status — never from worker-cluster credentials or objects.
	//
	// ClusterName is status.clusterName: the worker cluster Kueue has
	// selected for this workload. Empty until selection happens.
	ClusterName string
	// NominatedClusterNames is status.nominatedClusterNames: candidate
	// worker clusters under consideration before a cluster is selected.
	// Mutually exclusive with ClusterName upstream (Kueue resets one
	// when the other is set), but both are modeled so hydration never
	// has to guess which is authoritative.
	NominatedClusterNames []string
	// AdmissionChecks lists status.admissionChecks, sorted by name for
	// deterministic output. The MultiKueue admission check controller
	// reports selection/placement progress here (Pending/Ready/Retry/
	// Rejected).
	AdmissionChecks []AdmissionCheck
}

// waiting reports whether Kueue has neither admitted nor finished this
// workload — the state in which Reason and Message explain the hold-up.
// Once either happens they describe a past state and are cleared.
func (w Workload) waiting() bool {
	return !w.Admitted && !strings.EqualFold(w.Phase, "Finished")
}

// AdmissionCheck is one entry from a Kueue Workload's
// status.admissionChecks[]. State is one of Pending, Ready, Retry, or
// Rejected (see the Kueue Workload CRD). ControllerName is resolved from
// the manager-cluster AdmissionCheck object when RBAC allows it. When the
// lookup fails (for example because the batch-user role cannot get
// AdmissionChecks yet), ControllerLookupFailed is set so manager-side
// placement can fall back to the exact upstream MultiKueue check name until
// platform RBAC and activation tests lock that contract down.
type AdmissionCheck struct {
	Name                   string
	State                  string
	Message                string
	LastTransitionTime     time.Time
	ControllerName         string
	ControllerLookupFailed bool
}

type Pod struct {
	Name            string
	UID             string
	CreatedAt       time.Time
	Phase           string
	Node            string
	Restarts        int
	Ready           string // "1/1"
	StartedAt       time.Time
	ResourceClaims  []string
	ContainerReason string
	ExitCode        *int32
	OOMKilled       bool
	Conditions      []Condition
	InitContainers  []Container
	Containers      []Container
}

type Container struct {
	Name         string
	Image        string
	Ready        bool
	RestartCount int
	State        string // waiting|running|terminated
	Reason       string
	Message      string
	LastReason   string
	LastMessage  string
	StartedAt    time.Time
	FinishedAt   time.Time
	ExitCode     *int32
	LastExitCode *int32
}

type ResourceClaim struct {
	Name        string
	CreatedAt   time.Time
	Allocated   bool
	Allocation  string
	Conditions  []Condition
	LastReason  string
	LastMessage string
}

type Event struct {
	InvolvedKind string
	InvolvedName string
	Type         string
	Reason       string
	Message      string
	Count        int
	FirstSeen    time.Time
	LastSeen     time.Time
}

type RayJob struct {
	Found               bool
	Name                string
	UID                 string
	CreatedAt           time.Time
	StartedAt           time.Time
	FinishedAt          time.Time
	RayClusterName      string
	JobID               string
	JobDeploymentStatus string
	JobStatus           string
	RayClusterStatus    string
	Reason              string
	Message             string
	Conditions          []Condition
	// ManagedBy mirrors spec.managedBy on the RayJob. It is an immutable
	// opt-in flag set at admission time (not a live routing indicator):
	// KubeRay's MultiKueue integration sets it to
	// "kueue.x-k8s.io/multikueue" to declare that the RayJob is managed
	// by MultiKueue; the default (single-cluster) value is
	// "ray.io/kuberay-operator". Actual worker-cluster placement progress
	// is tracked separately via Workload.ClusterName /
	// NominatedClusterNames / AdmissionChecks.
	ManagedBy string
}

// Render returns a human-readable multi-section status view.
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
	b.WriteString(renderStartupPhases(s))
	if s.managerOnlyMultiKueueView() {
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
			// Kueue's message is the only place that says which flavors and
			// nodes were excluded. It goes in the trailing cell — the one
			// tabwriter does not pad — so its length cannot widen the
			// aligned columns. The tab run must reach that last cell, or
			// the short line ends the column block and re-aligns the rows
			// below it.
			if w.waiting() && w.Message != "" {
				fmt.Fprintf(tw, "  %s└─ %s\n", strings.Repeat("\t", len(cols)-1), singleLine(w.Message))
			}
		}
		tw.Flush()
	}

	b.WriteString("\nPods:\n")
	if len(s.Pods) == 0 {
		if s.managerOnlyMultiKueueView() {
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
		// Stable sort by phase then name to keep output deterministic.
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

// deriveState returns a one-word headline state.
//
// For RayJobs, maps jobDeploymentStatus directly. For batch/v1 Jobs:
//
//	Failed     — a Failed condition is true
//	Complete   — a Complete condition is true
//	Running    — at least one Active pod
//	Pending    — Job exists, suspended (Kueue not yet admitted)
//	Admitted   — Job exists, not suspended, no active pods yet
func deriveState(s Snapshot) string {
	if s.managerOnlyMultiKueueView() {
		if state, ok := multiKueueTerminalState(s); ok {
			return state
		}
		if state, ok := managerMultiKueueJobState(s); ok {
			return state
		}
		if state, ok := managerMultiKueueRayJobState(snapshotRayJob(s)); ok {
			return state
		}
		return string(s.MultiKueueState())
	}
	rj := snapshotRayJob(s)
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
	rj := snapshotRayJob(s)
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
