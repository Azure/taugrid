// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package links builds the portal's cross-board ID mapping.
//
// Cross-linking is the portal's core value-add over five separate dashboards.
// The join keys already exist on the Kubernetes objects the runtime stamps:
// every current Tau Job carries tau.azure.com/run-id (the Tau run identity), and
// Kueue copies Job-level labels onto the Workload it admits. This package reads
// the same Kueue Workloads the Jobs board already fetches, projects the run-id
// plus the compatibility tau.azure.com/job key when present — including admitted
// (running) workloads, which the researcher-facing queue.Snapshot intentionally
// drops — and centralizes the cross-link URL scheme so the backend overview and
// the frontend agree on one contract.
//
// It deliberately does not touch queue.Snapshot: that is the `tau queue status`
// compatibility surface. The portal's board-to-board wiring is additive here.
package links

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// WorkloadReader fetches the raw Kueue Workloads list as JSON. Both
// jobs.Reader and kubeclient.Client satisfy it structurally, so the overview
// reuses the Jobs board's reader without importing that package.
type WorkloadReader interface {
	ListWorkloads(ctx context.Context, namespace string) ([]byte, error)
}

// Workload is the portal's cross-link projection of a Kueue Workload: the join
// keys (RunID, Job) plus the queue context and admission state. Unlike
// queue.PendingWorkload it keeps RunID and surfaces admitted/finished workloads,
// so the overview can list what is running now and link each row to its
// experiment.
type Workload struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Job          string    `json:"job,omitempty"`
	RunID        string    `json:"runId,omitempty"`
	Owners       []string  `json:"owners,omitempty"`
	Queue        string    `json:"queue,omitempty"`
	ClusterQueue string    `json:"clusterQueue,omitempty"`
	Admitted     bool      `json:"admitted"`
	Finished     bool      `json:"finished"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`

	// Project, Experiment, Group, and Workspace are the Stellar identity every
	// `tau run` stamps alongside run-id (experiment.Metadata.KubernetesMetadata)
	// and Kueue copies onto the Workload with the rest of the Job's labels.
	// Surfacing them here lets the overview group and label a running row by the
	// experiment it belongs to instead of showing a bare run-id.
	//
	// These are LABEL values, so they went through
	// experiment.KubernetesLabelValue: lowercased and punctuation-normalized. A
	// project titled "NanoGPT FineWeb" appears here as "nanogpt-fineweb". Stellar
	// matches ?project= EXACTLY against the Kusto row's project, so these must
	// not be fed into ExperimentProjectPath — a normalized value that does not
	// round-trip produces a link that silently resolves to nothing, which is
	// worse than the unscoped link. The exact values live in the
	// "tau.azure.com/stellar-*-value" annotations on the Job/RayJob, which Kueue
	// does not copy; jobdetail reads those from the object itself.
	Project    string `json:"project,omitempty"`
	Experiment string `json:"experiment,omitempty"`
	Group      string `json:"group,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	// ExecutionTarget is observation-only runtime identity. It never gates
	// status access when the current capability/profile is unavailable.
	ExecutionTarget string `json:"executionTarget,omitempty"`
}

// Running reports whether the workload is admitted and not yet finished — the
// definition of "running now" the overview uses.
func (w Workload) Running() bool {
	return w.Admitted && !w.Finished
}

// ListWorkloads fetches and projects the Kueue Workloads in namespace. An empty
// namespace lists cluster-wide (subject to the reader's scope). The returned
// slice is sorted by namespace, then job/name, for stable rendering.
func ListWorkloads(ctx context.Context, r WorkloadReader, namespace string) ([]Workload, error) {
	raw, err := r.ListWorkloads(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	return parseWorkloads(raw)
}

// parseWorkloads is the pure parser used by ListWorkloads and tests.
func parseWorkloads(raw []byte) ([]Workload, error) {
	var list workloadList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse workloads: %w", err)
	}
	out := make([]Workload, 0, len(list.Items))
	for _, it := range list.Items {
		admitted, finished := admissionState(it.Status.Conditions)
		labels := it.Metadata.Labels
		executionTarget := workloadExecutionTarget(it)
		var owners []string
		for _, ref := range it.Metadata.OwnerReferences {
			if ref.Name != "" {
				owners = append(owners, ref.Name)
			}
		}
		out = append(out, Workload{
			Name:            it.Metadata.Name,
			Namespace:       it.Metadata.Namespace,
			Job:             labels[workloadmeta.LabelJob],
			RunID:           labels[experiment.LabelRunID],
			Owners:          owners,
			Queue:           it.Spec.QueueName,
			ClusterQueue:    it.Status.Admission.ClusterQueue,
			Admitted:        admitted,
			Finished:        finished,
			CreatedAt:       it.Metadata.CreationTimestamp,
			Project:         labels[workloadmeta.LabelStellarProject],
			Experiment:      labels[workloadmeta.LabelStellarExperiment],
			Group:           labels[workloadmeta.LabelStellarGroup],
			Workspace:       labels[workloadmeta.LabelWorkspace],
			ExecutionTarget: executionTarget,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		ja, jb := a.sortKey(), b.sortKey()
		return ja < jb
	})
	return out, nil
}

func workloadExecutionTarget(it workloadItem) string {
	if it.Status.ClusterName != "" || len(it.Status.NominatedClusterNames) > 0 {
		return "multiKueue"
	}
	return ""
}

// sortKey orders workloads by job name when present, falling back to the
// Workload name, so rows group by the researcher-visible job.
func (w Workload) sortKey() string {
	if w.Job != "" {
		return w.Job
	}
	return w.Name
}

// admissionState mirrors queue.workloadConditions: admitted when the Admitted
// condition is True, finished when the Finished condition is True.
func admissionState(conditions []conditionJSON) (admitted, finished bool) {
	for _, c := range conditions {
		if c.Type == "Admitted" && c.Status == "True" {
			admitted = true
		}
		if c.Type == "Finished" && c.Status == "True" {
			finished = true
		}
	}
	return admitted, finished
}

// ExperimentPath returns the portal path that deep-links a run to its
// Experiments (Stellar) view. It is the single source of truth for the
// Job→Experiment URL contract; an empty runID yields "" so callers omit the
// link rather than emit a dead one.
func ExperimentPath(runID string, workspace ...string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ""
	}
	return WorkspacePath("/stellar?target="+url.QueryEscape(runID), firstWorkspace(workspace))
}

// ExperimentProjectPath is ExperimentPath with an explicit project scope. When a
// run's durable Kusto row carries a project, appending project= lets Stellar
// resolve the target unambiguously. An empty runID yields ""; an empty project
// degrades to the plain target link. The workspace is threaded like
// ExperimentPath so portal navigation keeps the selected workspace.
func ExperimentProjectPath(runID, project string, workspace ...string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ""
	}
	path := "/stellar?target=" + url.QueryEscape(runID)
	if p := strings.TrimSpace(project); p != "" {
		path += "&project=" + url.QueryEscape(p)
	}
	return WorkspacePath(path, firstWorkspace(workspace))
}

// RayDashboardPath returns the portal path that reverse-proxies a RayJob's own
// Ray dashboard (port 8265) via the head-Service proxy the Ray board already
// owns (/api/portal/ray/proxy/{ns}/{cluster}/). It is the single source of truth
// for the Job→RayDashboard URL contract: the Ray board's buildCluster delegates
// here rather than reformatting the path itself. The routing prefix constant
// (portalapi.rayProxyPrefix) that strips this path server-side must stay in sync
// with the "/api/portal/ray/proxy/" literal below. An empty namespace or
// rayClusterName yields "" so callers omit the link rather than emit one that
// cannot resolve; the dashboard is only reachable while the RayCluster is running.
func RayDashboardPath(namespace, rayClusterName string, workspace ...string) string {
	namespace = strings.TrimSpace(namespace)
	rayClusterName = strings.TrimSpace(rayClusterName)
	if namespace == "" || rayClusterName == "" {
		return ""
	}
	path := "/api/portal/ray/proxy/" + url.PathEscape(namespace) + "/" + url.PathEscape(rayClusterName) + "/"
	return WorkspacePath(path, firstWorkspace(workspace))
}

// ClusterInstancePath deep-links the Cluster Health board to a single node by
// its GpuHealth() instance. Used to drill from a job/GPU row to that node's
// health. An empty instance yields "".
func ClusterInstancePath(instance string, workspace ...string) string {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		return ""
	}
	return WorkspacePath("/portal/cluster?instance="+url.QueryEscape(instance), firstWorkspace(workspace))
}

// WorkspacePath preserves the selected workspace across portal and Stellar
// links. Empty workspace IDs keep the legacy URL unchanged.
func WorkspacePath(path, workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return path
	}
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	q := u.Query()
	q.Set("workspace", workspace)
	u.RawQuery = q.Encode()
	return u.String()
}

func firstWorkspace(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// --- JSON shapes (subset of a Kueue Workload) ---

type workloadList struct {
	Items []workloadItem `json:"items"`
}

type workloadItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		OwnerReferences   []struct {
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		QueueName string `json:"queueName"`
	} `json:"spec"`
	Status struct {
		Admission struct {
			ClusterQueue string `json:"clusterQueue"`
		} `json:"admission"`
		Conditions            []conditionJSON `json:"conditions"`
		ClusterName           string          `json:"clusterName"`
		NominatedClusterNames []string        `json:"nominatedClusterNames"`
	} `json:"status"`
}

type conditionJSON struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}
