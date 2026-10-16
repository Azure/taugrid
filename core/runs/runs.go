// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/workloadmeta"
)

const tauLabelPrefix = workloadmeta.Domain

const (
	labelQueue                    = "kueue.x-k8s.io/queue-name"
	multiKueueManagedBy           = "kueue.x-k8s.io/multikueue"
	experimentTrackingTracked     = "tracked"
	experimentTrackingAvailable   = "available"
	experimentTrackingUntracked   = "untracked"
	experimentTrackingUnavailable = "unavailable"
)

type ExperimentSurfaceState string

const (
	ExperimentSurfaceUnconfigured ExperimentSurfaceState = ""
	ExperimentSurfaceAvailable    ExperimentSurfaceState = "available"
	ExperimentSurfaceUnavailable  ExperimentSurfaceState = "unavailable"
)

type Reader interface {
	ListJobs(ctx context.Context, namespace string) ([]byte, error)
	ListRayJobs(ctx context.Context, namespace string) ([]byte, error)
}

type Options struct {
	Namespace         string
	Queue             string
	IncludeExternal   bool
	ExperimentSurface ExperimentSurfaceState
	History           HistoryReader
	HistoryScope      HistoryScope
}

type Run struct {
	Name               string    `json:"name"`
	Kind               string    `json:"kind"`
	Status             string    `json:"status"`
	Created            time.Time `json:"created"`
	Age                string    `json:"age"`
	RunID              string    `json:"runId,omitempty"`
	Queue              string    `json:"queue,omitempty"`
	Namespace          string    `json:"namespace,omitempty"`
	Cluster            string    `json:"cluster,omitempty"`
	ResourceUID        string    `json:"resourceUid,omitempty"`
	DurableID          string    `json:"durableId,omitempty"`
	Source             string    `json:"source,omitempty"`
	ExperimentTracking string    `json:"experimentTracking"`
	ExperimentPath     string    `json:"experimentPath,omitempty"`
	ExecutionTarget    string    `json:"executionTarget,omitempty"`
}

type Snapshot struct {
	Namespace         string `json:"namespace,omitempty"`
	Total             int    `json:"total"`
	Runs              []Run  `json:"runs"`
	HistoryState      string `json:"historyState"`
	HistoryDiagnostic string `json:"historyDiagnostic,omitempty"`
}

func Board(ctx context.Context, r Reader, opts Options) (Snapshot, error) {
	jobsJSON, jobsErr := r.ListJobs(ctx, opts.Namespace)
	if jobsErr != nil {
		jobsJSON = nil
	}
	rayJSON, rayErr := r.ListRayJobs(ctx, opts.Namespace)
	if rayErr != nil {
		rayJSON = nil
	}
	snap := aggregateWithExternal(time.Now(), jobsJSON, rayJSON, opts.IncludeExternal)
	snap.Namespace = opts.Namespace
	liveUnavailable := jobsErr != nil && rayErr != nil
	snap.Runs = filterQueue(snap.Runs, opts.Queue)
	for i := range snap.Runs {
		snap.Runs[i].ExperimentTracking = trackingWithSurface(snap.Runs[i].ExperimentTracking, opts.ExperimentSurface)
		if snap.Runs[i].Namespace == "" {
			snap.Runs[i].Namespace = opts.Namespace
		}
		if snap.Runs[i].Cluster == "" {
			snap.Runs[i].Cluster = opts.HistoryScope.Cluster
		}
		if snap.Runs[i].DurableID == "" && snap.Runs[i].Cluster != "" && snap.Runs[i].Namespace != "" && snap.Runs[i].ResourceUID != "" {
			snap.Runs[i].DurableID = snap.Runs[i].Cluster + "/" + snap.Runs[i].Namespace + "/" + snap.Runs[i].ResourceUID
		}
	}
	if opts.History == nil {
		if liveUnavailable {
			return snap, fmt.Errorf("list runs: jobs: %v; rayjobs: %v", jobsErr, rayErr)
		}
		snap.HistoryState = historyStateLiveOnly
		snap.Total = len(snap.Runs)
		return snap, nil
	}
	historyScope := opts.HistoryScope
	if historyScope.Namespace == "" {
		historyScope.Namespace = opts.Namespace
	}
	if historyScope.LocalQueue == "" {
		historyScope.LocalQueue = opts.Queue
	}
	history, err := opts.History.ListHistory(ctx, historyScope)
	if err != nil {
		snap.HistoryState = historyStateUnavailable
		snap.HistoryDiagnostic = "durable run history query failed"
		snap.Total = len(snap.Runs)
		if liveUnavailable {
			return snap, fmt.Errorf("list live and durable runs: jobs: %v; rayjobs: %v; history: %w", jobsErr, rayErr, err)
		}
		return snap, nil
	}
	history = filterHistoryScope(history, historyScope)
	if opts.IncludeExternal {
		for i := range history {
			history[i].Source = "tau"
		}
	}
	snap.Runs = mergeHistory(snap.Runs, filterQueue(history, opts.Queue))
	snap.HistoryState = historyStateAvailable
	snap.Total = len(snap.Runs)
	return snap, nil
}

func filterQueue(rows []Run, queue string) []Run {
	if queue == "" {
		return rows
	}
	filtered := make([]Run, 0, len(rows))
	for _, run := range rows {
		if run.Queue == queue {
			filtered = append(filtered, run)
		}
	}
	return filtered
}

func filterHistoryScope(rows []Run, scope HistoryScope) []Run {
	filtered := make([]Run, 0, len(rows))
	for _, run := range rows {
		if scope.Cluster != "" && run.Cluster != scope.Cluster {
			continue
		}
		if scope.Namespace != "" && run.Namespace != scope.Namespace {
			continue
		}
		if scope.LocalQueue != "" && run.Queue != scope.LocalQueue {
			continue
		}
		filtered = append(filtered, run)
	}
	return filtered
}

func aggregateWithExternal(now time.Time, jobsJSON, rayJSON []byte, includeExternal bool) Snapshot {
	runs := make([]Run, 0)
	runs = append(runs, parseJobs(now, jobsJSON, includeExternal)...)
	runs = append(runs, parseRayJobs(now, rayJSON, includeExternal)...)
	sortRuns(runs)
	return Snapshot{Total: len(runs), Runs: runs, HistoryState: historyStateLiveOnly}
}

func sortRuns(runs []Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].Created.After(runs[j].Created)
	})
}

type ownerRef struct {
	Kind string `json:"kind"`
}

type jobList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			Namespace         string            `json:"namespace"`
			UID               string            `json:"uid"`
			CreationTimestamp string            `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
			Annotations       map[string]string `json:"annotations"`
			OwnerReferences   []ownerRef        `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			ManagedBy string `json:"managedBy"`
		} `json:"spec"`
		Status struct {
			Conditions []StatusCondition `json:"conditions"`
			Active     int               `json:"active"`
			Succeeded  int               `json:"succeeded"`
			Failed     int               `json:"failed"`
		} `json:"status"`
	} `json:"items"`
}

type rayJobList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			Namespace         string            `json:"namespace"`
			UID               string            `json:"uid"`
			CreationTimestamp string            `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
			Annotations       map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			ManagedBy string `json:"managedBy"`
		} `json:"spec"`
		Status struct {
			JobDeploymentStatus string `json:"jobDeploymentStatus"`
			JobStatus           string `json:"jobStatus"`
		} `json:"status"`
	} `json:"items"`
}

func parseJobs(now time.Time, data []byte, includeExternal bool) []Run {
	var list jobList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil
	}
	var out []Run
	for _, item := range list.Items {
		if ownedByRayJob(item.Metadata.OwnerReferences) {
			continue
		}
		managed := hasTauLabel(item.Metadata.Labels)
		if !managed && !includeExternal {
			continue
		}
		source := ""
		if includeExternal {
			source = "external"
			if managed {
				source = "tau"
			}
		}
		created := parseTime(item.Metadata.CreationTimestamp)
		executionTarget := runtimeExecutionTarget(item.Spec.ManagedBy)
		out = append(out, Run{
			Name:               item.Metadata.Name,
			Kind:               "Job",
			Status:             JobStatus(item.Status.Conditions, item.Status.Active, item.Status.Succeeded, item.Status.Failed),
			Created:            created,
			Age:                FormatAge(now, created),
			RunID:              runID(item.Metadata.Labels),
			Queue:              item.Metadata.Labels[labelQueue],
			Namespace:          item.Metadata.Namespace,
			ResourceUID:        item.Metadata.UID,
			DurableID:          durableID(item.Metadata.Labels, item.Metadata.Annotations),
			Source:             source,
			ExperimentTracking: experimentTracking(item.Metadata.Annotations),
			ExecutionTarget:    executionTarget,
		})
	}
	return out
}

func parseRayJobs(now time.Time, data []byte, includeExternal bool) []Run {
	var list rayJobList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil
	}
	var out []Run
	for _, item := range list.Items {
		managed := hasTauLabel(item.Metadata.Labels)
		if !managed && !includeExternal {
			continue
		}
		source := ""
		if includeExternal {
			source = "external"
			if managed {
				source = "tau"
			}
		}
		created := parseTime(item.Metadata.CreationTimestamp)
		executionTarget := runtimeExecutionTarget(item.Spec.ManagedBy)
		out = append(out, Run{
			Name:               item.Metadata.Name,
			Kind:               "RayJob",
			Status:             RayJobStatus(item.Status.JobDeploymentStatus, item.Status.JobStatus),
			Created:            created,
			Age:                FormatAge(now, created),
			RunID:              runID(item.Metadata.Labels),
			Queue:              item.Metadata.Labels[labelQueue],
			Namespace:          item.Metadata.Namespace,
			ResourceUID:        item.Metadata.UID,
			DurableID:          durableID(item.Metadata.Labels, item.Metadata.Annotations),
			Source:             source,
			ExperimentTracking: experimentTracking(item.Metadata.Annotations),
			ExecutionTarget:    executionTarget,
		})
	}

	return out
}

func runtimeExecutionTarget(managedBy string) string {
	if strings.TrimSpace(managedBy) == multiKueueManagedBy {
		return "multiKueue"
	}
	return ""
}

func hasTauLabel(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, tauLabelPrefix) {
			return true
		}
	}
	return false
}

func runID(labels map[string]string) string {
	return firstNonEmpty(labels[workloadmeta.LabelRunID], labels[workloadmeta.LabelRun])
}

func durableID(labels, annotations map[string]string) string {
	return firstNonEmpty(
		annotations[workloadmeta.AnnotationDurableID],
		labels[workloadmeta.AnnotationDurableID],
		annotations[workloadmeta.AnnotationDurableIDUnderscore],
		labels[workloadmeta.AnnotationDurableIDUnderscore],
	)
}

func experimentTracking(annotations map[string]string) string {
	if strings.EqualFold(strings.TrimSpace(annotations[workloadmeta.AnnotationExperimentSource]), "stellar") &&
		strings.TrimSpace(annotations[workloadmeta.AnnotationMetricsSession]) != "" {
		return experimentTrackingTracked
	}
	return experimentTrackingUntracked
}

func trackingWithSurface(current string, surface ExperimentSurfaceState) string {
	if current == experimentTrackingTracked {
		return current
	}
	switch surface {
	case ExperimentSurfaceAvailable:
		return experimentTrackingAvailable
	case ExperimentSurfaceUnavailable:
		return experimentTrackingUnavailable
	default:
		return experimentTrackingUntracked
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func ownedByRayJob(refs []ownerRef) bool {
	for _, ref := range refs {
		if ref.Kind == "RayJob" {
			return true
		}
	}
	return false
}

type StatusCondition struct {
	Type   string
	Status string
}

func JobStatus(conditions []StatusCondition, active, succeeded, failed int) string {
	for _, c := range conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			return "Complete"
		case "Failed":
			return "Failed"
		case "Suspended":
			return "Suspended"
		}
	}
	if active > 0 {
		return "Running"
	}
	return "Pending"
}

func RayJobStatus(deploymentStatus, jobStatus string) string {
	if deploymentStatus != "" {
		return deploymentStatus
	}
	if jobStatus != "" {
		return jobStatus
	}
	return "Pending"
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func FormatAge(now, created time.Time) string {
	if created.IsZero() {
		return "<unknown>"
	}
	d := now.Sub(created)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
