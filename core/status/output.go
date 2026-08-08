package status

import (
	"sort"
	"strings"
	"time"
)

const OutputSchemaVersion = "v1alpha1"

type Output struct {
	SchemaVersion string             `json:"schemaVersion"`
	Name          string             `json:"name"`
	Namespace     string             `json:"namespace"`
	Kind          string             `json:"kind"`
	State         string             `json:"state"`
	Degraded      bool               `json:"degraded"`
	Reason        string             `json:"reason,omitempty"`
	Observations  OutputObservations `json:"observations"`
	Phases        []OutputPhase      `json:"phases"`
	Job           *OutputJob         `json:"job,omitempty"`
	RayJob        *OutputRayJob      `json:"rayJob,omitempty"`
	Workloads     []OutputWorkload   `json:"workloads"`
	Pods          []OutputPod        `json:"pods"`
}

type OutputObservations struct {
	Job       OutputObservation `json:"job"`
	RayJob    OutputObservation `json:"rayJob"`
	Workloads OutputObservation `json:"workloads"`
	Pods      OutputObservation `json:"pods"`
}

type OutputObservation struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type OutputPhase struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type OutputCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type OutputJob struct {
	Suspended  bool              `json:"suspended"`
	Active     int               `json:"active"`
	Succeeded  int               `json:"succeeded"`
	Failed     int               `json:"failed"`
	Conditions []OutputCondition `json:"conditions"`
}

type OutputRayJob struct {
	DeploymentStatus string            `json:"deploymentStatus,omitempty"`
	JobStatus        string            `json:"jobStatus,omitempty"`
	RayClusterName   string            `json:"rayClusterName,omitempty"`
	JobID            string            `json:"jobId,omitempty"`
	Reason           string            `json:"reason,omitempty"`
	Message          string            `json:"message,omitempty"`
	Conditions       []OutputCondition `json:"conditions"`
}

type OutputWorkload struct {
	Name              string                 `json:"name"`
	Queue             string                 `json:"queue,omitempty"`
	Phase             string                 `json:"phase"`
	Admitted          bool                   `json:"admitted"`
	Reason            string                 `json:"reason,omitempty"`
	Message           string                 `json:"message,omitempty"`
	WorkerCluster     string                 `json:"workerCluster,omitempty"`
	NominatedClusters []string               `json:"nominatedClusters"`
	AdmissionChecks   []OutputAdmissionCheck `json:"admissionChecks"`
}

type OutputAdmissionCheck struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Controller string `json:"controller,omitempty"`
	Message    string `json:"message,omitempty"`
}

type OutputPod struct {
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	Node       string            `json:"node,omitempty"`
	Ready      string            `json:"ready"`
	Restarts   int               `json:"restarts"`
	Conditions []OutputCondition `json:"conditions"`
	Containers []OutputContainer `json:"containers"`
}

type OutputContainer struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	State        string `json:"state"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	Reason       string `json:"reason,omitempty"`
	ExitCode     *int32 `json:"exitCode"`
	LastReason   string `json:"lastReason,omitempty"`
	LastExitCode *int32 `json:"lastExitCode"`
}

func NewOutput(snapshot Snapshot) Output {
	rayJob := snapshotRayJob(snapshot)
	out := Output{
		SchemaVersion: OutputSchemaVersion,
		Name:          snapshot.Name,
		Namespace:     snapshot.Namespace,
		Kind:          outputKind(snapshot),
		State:         outputState(snapshot),
		Observations: OutputObservations{
			Job:       outputObservation(snapshot.Observations.Job),
			RayJob:    outputObservation(snapshot.Observations.RayJob),
			Workloads: outputObservation(snapshot.Observations.Workloads),
			Pods:      outputObservation(snapshot.Observations.Pods),
		},
		Workloads: make([]OutputWorkload, 0, len(snapshot.Workloads)),
		Pods:      make([]OutputPod, 0, len(snapshot.Pods)),
	}
	for _, phase := range startupPhasesAt(snapshot, stablePhaseTime(snapshot)) {
		out.Phases = append(out.Phases, OutputPhase{
			Name: phase.Name, Status: string(phase.Status), Detail: phase.Detail, Hint: phase.Hint,
		})
	}
	if snapshot.JobFound {
		out.Job = &OutputJob{
			Suspended:  snapshot.JobSuspended,
			Active:     snapshot.JobActive,
			Succeeded:  snapshot.JobSucceeded,
			Failed:     snapshot.JobFailed,
			Conditions: outputConditions(snapshot.JobConditions),
		}
	}
	if rayJob.Found {
		out.RayJob = &OutputRayJob{
			DeploymentStatus: rayJob.JobDeploymentStatus,
			JobStatus:        rayJob.JobStatus,
			RayClusterName:   rayJob.RayClusterName,
			JobID:            rayJob.JobID,
			Reason:           rayJob.Reason,
			Message:          rayJob.Message,
			Conditions:       outputConditions(rayJob.Conditions),
		}
	}
	for _, workload := range snapshot.Workloads {
		item := OutputWorkload{
			Name:              workload.Name,
			Queue:             workload.Queue,
			Phase:             workload.Phase,
			Admitted:          workload.Admitted,
			Reason:            workload.Reason,
			Message:           workload.Message,
			WorkerCluster:     workload.ClusterName,
			NominatedClusters: append([]string{}, workload.NominatedClusterNames...),
			AdmissionChecks:   make([]OutputAdmissionCheck, 0, len(workload.AdmissionChecks)),
		}
		for _, check := range workload.AdmissionChecks {
			item.AdmissionChecks = append(item.AdmissionChecks, OutputAdmissionCheck{
				Name: check.Name, State: check.State, Controller: check.ControllerName, Message: check.Message,
			})
		}
		sort.Slice(item.AdmissionChecks, func(i, j int) bool {
			return item.AdmissionChecks[i].Name < item.AdmissionChecks[j].Name
		})
		sort.Strings(item.NominatedClusters)
		out.Workloads = append(out.Workloads, item)
	}
	sort.Slice(out.Workloads, func(i, j int) bool {
		return out.Workloads[i].Name < out.Workloads[j].Name
	})
	for _, pod := range snapshot.Pods {
		item := OutputPod{
			Name:       pod.Name,
			Phase:      podDisplayPhase(rayJob, pod),
			Node:       pod.Node,
			Ready:      pod.Ready,
			Restarts:   pod.Restarts,
			Conditions: outputConditions(pod.Conditions),
			Containers: make([]OutputContainer, 0, len(pod.InitContainers)+len(pod.Containers)),
		}
		for _, container := range pod.InitContainers {
			item.Containers = append(item.Containers, outputContainer("init", container))
		}
		for _, container := range pod.Containers {
			item.Containers = append(item.Containers, outputContainer("app", container))
		}
		sort.Slice(item.Containers, func(i, j int) bool {
			if item.Containers[i].Kind != item.Containers[j].Kind {
				return item.Containers[i].Kind < item.Containers[j].Kind
			}
			return item.Containers[i].Name < item.Containers[j].Name
		})
		out.Pods = append(out.Pods, item)
	}
	sort.Slice(out.Pods, func(i, j int) bool {
		if out.Pods[i].Phase != out.Pods[j].Phase {
			return out.Pods[i].Phase < out.Pods[j].Phase
		}
		return out.Pods[i].Name < out.Pods[j].Name
	})
	if issue := firstCurrentContainerIssue(snapshot); issue != nil {
		out.Degraded = true
		out.Reason = issue.summary()
	} else {
		out.Reason = outputFailureReason(snapshot)
		out.Degraded = out.Reason != "" || WorkloadFailed(snapshot)
	}
	return out
}

func outputKind(snapshot Snapshot) string {
	job, rayJob := snapshot.JobFound, snapshotRayJob(snapshot).Found
	switch {
	case job && rayJob:
		return "Job+RayJob"
	case rayJob:
		return "RayJob"
	case job:
		return "Job"
	default:
		return "Unknown"
	}
}

func outputState(snapshot Snapshot) string {
	if snapshot.JobFound || snapshotRayJob(snapshot).Found {
		return deriveState(snapshot)
	}
	if snapshot.Observations.Job.State == ObservationNotFound &&
		(snapshot.Observations.RayJob.State == ObservationNotFound ||
			optionalResourceTypeMissing(snapshot.Observations.RayJob)) {
		return "NotFound"
	}
	return "Unknown"
}

func outputObservation(observation ResourceObservation) OutputObservation {
	state := string(observation.State)
	if state == "" {
		state = "unknown"
	}
	return OutputObservation{State: state, Reason: observation.Reason}
}

func outputConditions(conditions []Condition) []OutputCondition {
	out := make([]OutputCondition, 0, len(conditions))
	for _, condition := range conditions {
		out = append(out, OutputCondition{
			Type: condition.Type, Status: condition.Status, Reason: condition.Reason, Message: condition.Message,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Type < out[j].Type
	})
	return out
}

func outputContainer(kind string, container Container) OutputContainer {
	return OutputContainer{
		Kind:         kind,
		Name:         container.Name,
		State:        container.State,
		Ready:        container.Ready,
		RestartCount: container.RestartCount,
		Reason:       container.Reason,
		ExitCode:     container.ExitCode,
		LastReason:   container.LastReason,
		LastExitCode: container.LastExitCode,
	}
}

func outputFailureReason(snapshot Snapshot) string {
	for _, condition := range snapshot.JobConditions {
		if condition.Type == "Failed" && condition.Status == "True" {
			return strings.TrimSpace(strings.Join([]string{condition.Reason, condition.Message}, " "))
		}
	}
	rayJob := snapshotRayJob(snapshot)
	if rayJobStatusFailed(rayJob) {
		return strings.TrimSpace(strings.Join([]string{rayJob.Reason, rayJob.Message}, " "))
	}
	return ""
}

func stablePhaseTime(snapshot Snapshot) (latestTime time.Time) {
	for _, candidate := range []time.Time{
		snapshot.JobCreatedAt, snapshot.JobStartedAt, snapshot.JobFinishedAt,
		snapshot.RayJob.CreatedAt, snapshot.RayJob.StartedAt, snapshot.RayJob.FinishedAt,
	} {
		if candidate.After(latestTime) {
			latestTime = candidate
		}
	}
	for _, pod := range snapshot.Pods {
		for _, candidate := range []time.Time{pod.CreatedAt, pod.StartedAt} {
			if candidate.After(latestTime) {
				latestTime = candidate
			}
		}
	}
	if latestTime.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return latestTime
}
