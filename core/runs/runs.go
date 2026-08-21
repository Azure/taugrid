// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runs builds the portal's Jobs board: the list of Tau-managed
// workloads — batch/v1 Jobs and ray.io RayJobs — in a namespace.
//
// This is distinct from the Jobs/Queue board (internal/portal/jobs), which
// reports Kueue *queue* pressure (admission, quota, headroom). This board
// answers a researcher's question instead: "what did I submit, and what state
// is it in?" — name, kind, status, and age per workload.
//
// The parsing/aggregation here is the single source of truth shared by both the
// portal (/api/portal/runs, via internal/portal/kubeclient) and the CLI
// (`tau run list`, via internal/kube). Both hand it the API's raw list JSON, so
// the filter/dedup/status/age rules live in exactly one place.
//
// Only workloads created by Tau (carrying an tau.azure.com/ label) are shown;
// KubeRay-internal Jobs (owned by a RayJob) are excluded to avoid duplicates.
// The board degrades gracefully: a missing RayJob CRD (KubeRay not installed)
// drops just the RayJob rows, and a portal without Kubernetes access disables
// the board entirely (the handler returns 503).
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

// tauLabelPrefix marks a workload as Tau-managed. Any label under this prefix
// (e.g. tau.azure.com/job, tau.azure.com/run) qualifies.
const tauLabelPrefix = workloadmeta.Domain

const (
	labelQueue                    = "kueue.x-k8s.io/queue-name"
	experimentTrackingTracked     = "tracked"
	experimentTrackingAvailable   = "available"
	experimentTrackingUntracked   = "untracked"
	experimentTrackingUnavailable = "unavailable"
)

// ExperimentSurfaceState describes whether the caller's resolved workspace has
// a usable experiment surface. It is intentionally not per-run evidence.
type ExperimentSurfaceState string

const (
	ExperimentSurfaceUnconfigured ExperimentSurfaceState = ""
	ExperimentSurfaceAvailable    ExperimentSurfaceState = "available"
	ExperimentSurfaceUnavailable  ExperimentSurfaceState = "unavailable"
)

// Reader lists the raw Jobs and RayJobs JSON the board needs. Both
// kubeclient.Client (portal, client-go) and a kubectl shim (CLI) satisfy this;
// tests supply a fake so no live API is required.
type Reader interface {
	// ListJobs returns the namespaced batch/v1 Jobs list as raw JSON.
	ListJobs(ctx context.Context, namespace string) ([]byte, error)
	// ListRayJobs returns the namespaced ray.io RayJobs list as raw JSON.
	ListRayJobs(ctx context.Context, namespace string) ([]byte, error)
}

// Options scopes the listing. Namespace, when set, restricts both listings to
// one namespace.
type Options struct {
	Namespace         string
	Queue             string
	IncludeExternal   bool
	ExperimentSurface ExperimentSurfaceState
	History           HistoryReader
	HistoryScope      HistoryScope
}

// Run is one Tau-managed workload. Created is the object's creation time (for
// transparent sorting); Age is that time preformatted with the same buckets the
// CLI prints (e.g. "30s"/"45m"/"2h"/"3d"), so the portal frontend and the CLI
// show identical age strings without duplicating the formatting logic.
// ExperimentTracking is "tracked" only with exact per-run evidence; "available"
// describes the workspace surface, not confirmed membership for this run.
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
}

// Snapshot is the Jobs board payload: the Tau-managed workloads, newest first.
type Snapshot struct {
	Namespace         string `json:"namespace,omitempty"`
	Total             int    `json:"total"`
	Runs              []Run  `json:"runs"`
	HistoryState      string `json:"historyState"`
	HistoryDiagnostic string `json:"historyDiagnostic,omitempty"`
}

// Board lists Jobs and RayJobs via the Reader and aggregates them. Each source
// degrades on its own: a listing error drops that source's rows (a missing
// RayJob CRD is normal when KubeRay is absent). Only when *both* listings fail
// does Board return an error, so the handler can surface a real outage (502)
// rather than an empty board.
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

// ownerRef is the subset of an ownerReference the board reads.
type ownerRef struct {
	Kind string `json:"kind"`
}

// jobList is the subset of a batch/v1 Job list the board reads.
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
		Status struct {
			Conditions []StatusCondition `json:"conditions"`
			Active     int               `json:"active"`
			Succeeded  int               `json:"succeeded"`
			Failed     int               `json:"failed"`
		} `json:"status"`
	} `json:"items"`
}

// rayJobList is the subset of a ray.io RayJob list the board reads.
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
		Status struct {
			JobDeploymentStatus string `json:"jobDeploymentStatus"`
			JobStatus           string `json:"jobStatus"`
		} `json:"status"`
	} `json:"items"`
}

// parseJobs decodes the Jobs list and keeps Tau-managed, non-RayJob-owned Jobs.
// Invalid JSON (e.g. a "CRD not found" message where JSON was expected) yields no
// rows rather than an error, matching the board's graceful-degradation contract.
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
		})
	}
	return out
}

// parseRayJobs decodes the RayJobs list and keeps Tau-managed RayJobs.
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
		})
	}
	return out
}

// hasTauLabel reports whether any label key is under the Tau prefix.
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

// ownedByRayJob reports whether any owner reference is a RayJob, so the
// KubeRay-created submitter Job can be excluded (the RayJob itself is listed).
func ownedByRayJob(refs []ownerRef) bool {
	for _, ref := range refs {
		if ref.Kind == "RayJob" {
			return true
		}
	}
	return false
}

// StatusCondition is the minimal Job condition shape (type + status) the status
// mappers read. Exported so callers outside the package (e.g. the Job detail
// board) can reuse JobStatus without re-deriving the display mapping.
type StatusCondition struct {
	Type   string
	Status string
}

// JobStatus maps a batch/v1 Job's conditions and counters to the same display
// status the runs board shows. First matching true condition wins in slice
// order (Complete/Failed/Suspended), then active>0 is Running, else Pending.
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

// RayJobStatus maps a RayJob's deployment/job status to the same display status
// the runs board shows, preferring the deployment status and falling back to
// Pending. Exported so the Job detail board reuses the identical mapping.
func RayJobStatus(deploymentStatus, jobStatus string) string {
	if deploymentStatus != "" {
		return deploymentStatus
	}
	if jobStatus != "" {
		return jobStatus
	}
	return "Pending"
}

// parseTime parses an RFC3339 creation timestamp, returning the zero time on any
// error so a malformed/absent timestamp reads as "unknown" age rather than
// failing the row.
func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// FormatAge renders the age of an object created at `created` as of `now`, using
// coarse single-unit buckets (seconds, minutes, hours, days). A zero created
// time reads "<unknown>". Exported so the CLI renders the exact same column the
// portal serves.
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
