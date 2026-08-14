// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package jobdetail builds the portal's job detail page: everything about one
// Tau-managed workload, gathered from the seams the other boards already own.
//
// From the Runs board a researcher clicks a job and lands on
// /portal/runs/<ns>/<name>; this package assembles the backing snapshot in three
// independently-degrading tiers:
//
//   - Tier 1 (Kubernetes truth): the Job or RayJob object, the Kueue Workloads
//     admitted for it (queue/admission state), its Pods (phase/node/restarts),
//     and recent Events. Sourced from the client-go reads in
//     internal/portal/kubeclient.
//   - Tier 2 (cross-links): pure URLs — an "Open in Stellar" deep-link built from
//     the run-id (links.ExperimentPath) and a per-pod Cluster board link
//     (links.ClusterInstancePath). No new data is fetched.
//   - Tier 3 (durable Kusto): optional. When a Querier is configured, the run's
//     terminal lifecycle is derived from the `tau/run_status` marker row the
//     metrics-offload sidecar remote-writes into the ExperimentMetrics table
//     (state/reason/completion/artifact_uri/checkpoint_uri) — the same signal
//     Stellar's cockpit reads — so results survive the K8s object's garbage
//     collection and stay consistent with the Stellar dashboard.
//
// The status vocabulary (Job/RayJob → display status, age formatting) is reused
// from internal/portal/runs so the detail page and the Runs board never diverge.
package jobdetail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/Azure/taugrid/portal/internal/portal/links"
	"github.com/Azure/taugrid/portal/internal/portal/ray"
)

// runIDLabel is the Tau run identity label (mirror of experiment.LabelRunID),
// kept as a literal here to avoid importing the experiment package into this
// aggregation layer — the same choice runs.go and links.go make.

// labelJob is the job-name label tau stamps on Jobs and Kueue copies onto the
// Workload. Used to filter this job's Workloads and to select its Pods.

// rayClusterLabel is the label KubeRay stamps on a RayJob's pods (value is the
// owning RayCluster's name). Used to select a RayJob's pods.
const rayClusterLabel = "ray.io/cluster"

// ErrNotFound signals the requested job (Job and RayJob) does not exist, so the
// handler can return 404 rather than a soft-degraded empty page.
var ErrNotFound = errors.New("job not found")

// errDecode signals a successful API read whose payload could not be parsed (bad
// JSON, or a valid object missing metadata.name). It is distinct from
// ErrNotFound so the handler returns 502 for an upstream decode/schema failure
// rather than a misleading 404 that implies the object is absent.
var errDecode = errors.New("jobdetail: object read succeeded but payload could not be parsed")

// Reader is the client-go read surface the detail page needs. kubeclient.Client
// satisfies it structurally; tests supply a fake so no live API is required.
type Reader interface {
	GetJob(ctx context.Context, namespace, name string) ([]byte, error)
	GetRayJob(ctx context.Context, namespace, name string) ([]byte, error)
	ListPods(ctx context.Context, namespace string) ([]byte, error)
	ListEvents(ctx context.Context, namespace string) ([]byte, error)
	ListWorkloads(ctx context.Context, namespace string) ([]byte, error)
	// ListServices backs the Ray dashboard reachability check: the link is only
	// live when the RayCluster's head Service is discoverable by the same
	// head-Service scan the portal's proxy uses (ray.Board), so "button lit" and
	// "proxy resolves" stay in lockstep and a finished/GC'd RayJob whose head
	// Service no longer matches never shows a clickable-but-404 link.
	ListServices(ctx context.Context, namespace string) ([]byte, error)
}

// Options scopes the detail read to one object by namespace and name.
type Options struct {
	Namespace string
	Name      string
}

// Snapshot is the job detail payload, designed for the page rather than reusing
// a board shape. Optional tiers are omitted when empty so the frontend can
// render each independently.
type Snapshot struct {
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Kind      string           `json:"kind"` // Job | RayJob
	Status    string           `json:"status"`
	RunID     string           `json:"runId,omitempty"`
	Object    ObjectDetail     `json:"object"`
	Workloads []links.Workload `json:"workloads,omitempty"`
	Pods      []PodDetail      `json:"pods,omitempty"`
	Events    []EventDetail    `json:"events,omitempty"`
	Links     DetailLinks      `json:"links"`
	Lifecycle *LifecycleRow    `json:"lifecycle,omitempty"`
	// ResourceRelease distinguishes scheduler quota accounting from physical
	// Ray pod teardown. It is populated for RayJobs after Workloads and Pods are
	// read so the UI never treats "Finished" as proof that GPUs are reusable.
	ResourceRelease *ResourceReleaseDetail `json:"resourceRelease,omitempty"`
	// Experiment is the Stellar identity `tau run` stamped on this object. It is
	// omitted for a workload that carries none (a bare Job, or a run submitted
	// without experiment metadata).
	Experiment *ExperimentIdentity `json:"experiment,omitempty"`
}

// ExperimentIdentity is the Stellar identity every Tau run path stamps on its
// Job/RayJob via experiment.Metadata.KubernetesMetadata. It is read from the
// "tau.azure.com/stellar-*-value" ANNOTATIONS rather than the matching labels:
// the labels are normalized by experiment.KubernetesLabelValue (lowercased,
// punctuation folded) while Stellar matches ?project= exactly against the Kusto
// row, so only the annotation round-trips. Kueue does not copy annotations onto
// the Workload, which is why this is resolved from the object here and left out
// of links.Workload.
type ExperimentIdentity struct {
	Project      string `json:"project,omitempty"`
	ExperimentID string `json:"experimentId,omitempty"`
	Title        string `json:"title,omitempty"`
	Group        string `json:"group,omitempty"`
}

// empty reports whether no Stellar identity was stamped at all.
func (e ExperimentIdentity) empty() bool {
	return e.Project == "" && e.ExperimentID == "" && e.Title == "" && e.Group == ""
}

// experimentIdentity extracts the Stellar identity from an object's annotations.
func experimentIdentity(annotations map[string]string) ExperimentIdentity {
	experimentID := strings.TrimSpace(annotations[workloadmeta.AnnotationStellarExperimentID])
	if experimentID == "" {
		experimentID = strings.TrimSpace(annotations[workloadmeta.AnnotationStellarQuestion])
	}
	return ExperimentIdentity{
		Project:      strings.TrimSpace(annotations[workloadmeta.AnnotationStellarProject]),
		ExperimentID: experimentID,
		Title:        strings.TrimSpace(annotations[workloadmeta.AnnotationStellarExperimentTitle]),
		Group:        strings.TrimSpace(annotations[workloadmeta.AnnotationStellarGroup]),
	}
}

// ObjectDetail is the tier-1 object header: identity fields plus the RayJob-
// native status the Runs board's status mapping does not surface.
type ObjectDetail struct {
	// Created is a pointer so an unparseable/absent creationTimestamp is omitted
	// rather than serialized as the false "0001-01-01T00:00:00Z" zero time, which
	// the frontend would render as a real (and misleading) date.
	Created     *time.Time        `json:"created,omitempty"`
	Age         string            `json:"age"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// RayJob-native fields (empty for a plain Job).
	JobDeploymentStatus string `json:"jobDeploymentStatus,omitempty"`
	RayClusterName      string `json:"rayClusterName,omitempty"`
	JobID               string `json:"jobId,omitempty"`
	ManagedBy           string `json:"managedBy,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Message             string `json:"message,omitempty"`
}

type ResourceReleaseDetail struct {
	QuotaState   string   `json:"quotaState"`   // unknown | pending | reserved | released
	ComputeState string   `json:"computeState"` // unknown | in-use | releasing | reusable
	ActivePods   int      `json:"activePods"`
	Nodes        []string `json:"nodes,omitempty"`
	Message      string   `json:"message"`
}

// PodDetail is one pod backing the run: phase, placement, and restart count.
type PodDetail struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Node     string `json:"node,omitempty"`
	Restarts int    `json:"restarts"`
	// NodePath deep-links the Cluster board to this pod's node (empty when the
	// pod is unscheduled).
	NodePath string `json:"nodePath,omitempty"`
}

// EventDetail is one recent Kubernetes event for troubleshooting. Last is a
// pointer so an event with no resolvable timestamp is omitted rather than
// serialized as the false "0001-01-01T00:00:00Z" zero time (which the frontend
// would render as a truthy date). Modern core/v1 Events may leave the legacy
// lastTimestamp empty and carry the time in eventTime or series.lastObservedTime
// instead, so parseEvents falls back through all three.
type EventDetail struct {
	Type    string     `json:"type"`
	Reason  string     `json:"reason"`
	Message string     `json:"message"`
	Count   int        `json:"count"`
	Last    *time.Time `json:"last,omitempty"`
}

// DetailLinks holds the tier-2 cross-links. StellarPath is empty unless the run
// has a durable Kusto lifecycle row (proof it was mirrored to Stellar), so the
// frontend omits the button rather than emitting a dead "record not found" link.
// RayDashboardPath is set only for a RayJob that has a named RayCluster: it
// reverse-proxies that cluster's own Ray dashboard (tasks/actors/logs) and is
// only reachable while the cluster is running.
type DetailLinks struct {
	StellarPath      string `json:"stellarPath,omitempty"`
	RayDashboardPath string `json:"rayDashboardPath,omitempty"`
	// RayDashboardReachable reports whether the RayCluster head pod is currently
	// Ready, i.e. the reverse-proxied dashboard at RayDashboardPath will actually
	// serve. The head Service (and thus the proxy path) can outlive the head pod
	// after a RayJob finishes — shutdownAfterJobFinishes keeps the cluster up for
	// ttlSecondsAfterFinished (24h) but the pod goes NotReady before GC — so a
	// non-empty RayDashboardPath does not imply reachability. The frontend greys
	// the link out (rather than hiding it) when this is false. It defaults to true
	// when readiness cannot be determined (pod list unavailable or head pod absent)
	// so a transient read failure never greys a healthy link. Not omitempty: false
	// must serialize so the frontend can distinguish "unreachable" from "unknown".
	RayDashboardReachable bool `json:"rayDashboardReachable"`
}

// LifecycleRow is the tier-3 durable projection of the run from Kusto.
type LifecycleRow struct {
	State          string `json:"state,omitempty"`
	EffectiveState string `json:"effectiveState,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Message        string `json:"message,omitempty"`
	CompletionTime string `json:"completionTime,omitempty"`
	ArtifactURI    string `json:"artifactUri,omitempty"`
	CheckpointURI  string `json:"checkpointUri,omitempty"`
	// Project is the Kusto project the run belongs to, extracted from the
	// remote-write row's Labels['project']. It disambiguates the Stellar
	// deep-link when a run-id matches more than one project.
	Project string `json:"project,omitempty"`
}

// Detail assembles the snapshot. It first resolves the object (RayJob preferred,
// then Job); when neither exists it returns ErrNotFound. Tiers 1b (workloads,
// pods, events) and 3 (Kusto) degrade independently: a per-source read error
// drops just that section rather than failing the page. Querier may be nil (no
// --kusto-query-command), which skips tier 3.
func Detail(ctx context.Context, r Reader, q kustoquery.Querier, opts Options) (Snapshot, error) {
	if r == nil {
		return Snapshot{}, errors.New("jobdetail: nil reader")
	}
	obj, err := resolveObject(ctx, r, opts)
	if err != nil {
		return Snapshot{}, err
	}

	snap := Snapshot{
		Namespace: opts.Namespace,
		Name:      opts.Name,
		Kind:      obj.kind,
		Status:    obj.status,
		RunID:     obj.runID,
		Object:    obj.detail,
	}
	if !obj.experiment.empty() {
		identity := obj.experiment
		snap.Experiment = &identity
	}

	// Tier 2: per-job Ray dashboard deep-link. Only a RayJob with a named
	// RayCluster has one — the link reverse-proxies that cluster's head Service
	// (:8265) and is reachable only while the cluster runs. RayDashboardPath
	// returns "" for a plain Job or a RayJob whose cluster is not yet named, so
	// the frontend omits the button.
	snap.Links.RayDashboardPath = links.RayDashboardPath(opts.Namespace, obj.rayClusterName)
	// Reachability is judged by the SAME head-Service discovery the portal's Ray
	// proxy uses (ray.Board): the link only works if validateRayTarget can resolve
	// <ns>/<rayClusterName> to a discoverable head Service. Judging reachability by
	// head-pod readiness alone is wrong — a finished/GC'd RayJob can leave a head
	// Service whose name/labels no longer match rayClusterName (KubeRay names it
	// after the RayJob, e.g. "<rayjob>-head-svc" with an empty ray.io/cluster
	// label), so the proxy 404s even though a stale head pod might look Ready.
	// Aligning "button lit" with "proxy resolves" removes the clickable-but-404 gap.
	if obj.rayClusterName != "" {
		snap.Links.RayDashboardReachable = rayClusterDiscoverable(ctx, r, opts.Namespace, obj.rayClusterName)
	}

	// Tier 1b: Workloads admitted for this job (best-effort).
	if wls, err := links.ListWorkloads(ctx, r, opts.Namespace); err == nil {
		snap.Workloads = filterWorkloads(wls, opts.Name, opts.Name, obj.runID)
	}

	// Tier 1b: Pods backing the run (best-effort). Filter by RayCluster (RayJob)
	// or job label (Job).
	podsVisible := false
	if raw, err := r.ListPods(ctx, opts.Namespace); err == nil {
		snap.Pods, podsVisible = parsePodsWithStatus(raw, obj.podSelectorKey, obj.podSelectorValue)
	}
	if obj.kind == "RayJob" {
		snap.ResourceRelease = rayResourceRelease(obj.detail, snap.Workloads, snap.Pods, podsVisible)
	}

	// Tier 1b: recent Events (best-effort).
	if raw, err := r.ListEvents(ctx, opts.Namespace); err == nil {
		snap.Events = parseEvents(raw, opts.Name, obj.rayClusterName)
	}

	// Tier 3: durable Kusto lifecycle (optional, best-effort). The Stellar
	// deep-link is emitted only when this lookup succeeds: a run-id alone does not
	// prove the run was ever mirrored to Kusto (a bare Job / `tau ray submit`
	// without metric offload never writes ExperimentMetrics), and linking on run-id
	// alone yields a dead "experiment store record not found" page. The lifecycle
	// row is the one signal that the run is durably indexed and Stellar can render.
	if q != nil && obj.runID != "" {
		if row, ok := lifecycle(ctx, q, obj.runID); ok {
			snap.Lifecycle = row
			// Scope the link with the Kusto row's project ONLY, never with the
			// project stamped on the object. metricsoffload.Runtime.Validate
			// rejects an empty project, so a run whose marker row exists always
			// has one; row.Project == "" therefore means the projection did not
			// carry it, not that the run has no project, and substituting the
			// annotation would filter a working unscoped link down to rows that
			// do not match. The two values are also not interchangeable in
			// general: the direct `tau run --config` path passes
			// experiment.project straight through, but the manifest path
			// defaults the offload project to "tau-finetune" independently.
			snap.Links.StellarPath = links.ExperimentProjectPath(obj.runID, row.Project)
		}
	}

	return snap, nil
}

// resolved carries the fields extracted from whichever object was found.
type resolved struct {
	kind             string
	status           string
	runID            string
	experiment       ExperimentIdentity
	detail           ObjectDetail
	rayClusterName   string
	podSelectorKey   string
	podSelectorValue string
}

// resolveObject tries the RayJob first (its pods and native status are richer),
// then the plain Job. It distinguishes four outcomes:
//   - found: a readable object → (res, nil)
//   - genuinely absent: both reads return a Kubernetes NotFound (the object does
//     not exist, or its CRD is not installed) → (zero, ErrNotFound)
//   - unreadable: a read failed for another reason (RBAC forbidden, API timeout,
//     transient 5xx) → (zero, that error), so the handler surfaces 502/503
//     instead of a misleading 404.
//   - undecodable: a read succeeded (err == nil) but the payload could not be
//     parsed (malformed JSON, or a valid object missing metadata.name) →
//     (zero, errDecode). A successful-but-garbage response proves the API
//     answered, not that the object is absent, so it must not collapse to 404.
//
// A NotFound on one kind but a hard error on the other still returns the hard
// error: we cannot prove absence when a read did not complete.
func resolveObject(ctx context.Context, r Reader, opts Options) (resolved, error) {
	rayRaw, rayErr := r.GetRayJob(ctx, opts.Namespace, opts.Name)
	if rayErr == nil {
		if res, ok := parseRayJob(rayRaw); ok {
			return res, nil
		}
	}
	jobRaw, jobErr := r.GetJob(ctx, opts.Namespace, opts.Name)
	if jobErr == nil {
		if res, ok := parseJob(jobRaw); ok {
			return res, nil
		}
	}
	// Neither object was usable. A non-NotFound read error means we could not
	// prove absence, so propagate it (→ 502/503).
	if rayErr != nil && !apierrors.IsNotFound(rayErr) {
		return resolved{}, rayErr
	}
	if jobErr != nil && !apierrors.IsNotFound(jobErr) {
		return resolved{}, jobErr
	}
	// A read that succeeded but did not parse is a decode failure, not absence:
	// the API answered, so 404 would be misleading. Only when every read was a
	// genuine NotFound do we conclude the object is absent.
	if rayErr == nil || jobErr == nil {
		// At least one read returned (payload, nil) but failed to parse.
		return resolved{}, errDecode
	}
	return resolved{}, ErrNotFound
}

// objectMeta is the metadata subset shared by Job and RayJob.
type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
}

type jobObject struct {
	Metadata objectMeta `json:"metadata"`
	Status   struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		Active    int `json:"active"`
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
	} `json:"status"`
}

type rayJobObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ManagedBy string `json:"managedBy"`
	} `json:"spec"`
	Status struct {
		JobDeploymentStatus string `json:"jobDeploymentStatus"`
		JobStatus           string `json:"jobStatus"`
		RayClusterName      string `json:"rayClusterName"`
		JobID               string `json:"jobId"`
		Reason              string `json:"reason"`
		Message             string `json:"message"`
	} `json:"status"`
}

func parseJob(data []byte) (resolved, bool) {
	var o jobObject
	if err := json.Unmarshal(data, &o); err != nil || o.Metadata.Name == "" {
		return resolved{}, false
	}
	created := parseTime(o.Metadata.CreationTimestamp)
	conds := make([]runs.StatusCondition, 0, len(o.Status.Conditions))
	for _, c := range o.Status.Conditions {
		conds = append(conds, runs.StatusCondition{Type: c.Type, Status: c.Status})
	}
	return resolved{
		kind:   "Job",
		status: runs.JobStatus(conds, o.Status.Active, o.Status.Succeeded, o.Status.Failed),
		runID:  o.Metadata.Labels[workloadmeta.LabelRunID],

		experiment: experimentIdentity(o.Metadata.Annotations),
		detail: ObjectDetail{
			Created:     optionalTime(created),
			Age:         runs.FormatAge(time.Now(), created),
			Labels:      o.Metadata.Labels,
			Annotations: o.Metadata.Annotations,
		},
		podSelectorKey:   workloadmeta.LabelJob,
		podSelectorValue: o.Metadata.Name,
	}, true
}

func parseRayJob(data []byte) (resolved, bool) {
	var o rayJobObject
	if err := json.Unmarshal(data, &o); err != nil || o.Metadata.Name == "" {
		return resolved{}, false
	}
	created := parseTime(o.Metadata.CreationTimestamp)
	return resolved{
		kind:   "RayJob",
		status: runs.RayJobStatus(o.Status.JobDeploymentStatus, o.Status.JobStatus),
		runID:  o.Metadata.Labels[workloadmeta.LabelRunID],

		experiment: experimentIdentity(o.Metadata.Annotations),
		detail: ObjectDetail{
			Created:             optionalTime(created),
			Age:                 runs.FormatAge(time.Now(), created),
			Labels:              o.Metadata.Labels,
			Annotations:         o.Metadata.Annotations,
			JobDeploymentStatus: o.Status.JobDeploymentStatus,
			RayClusterName:      o.Status.RayClusterName,
			JobID:               o.Status.JobID,
			ManagedBy:           o.Spec.ManagedBy,
			Reason:              o.Status.Reason,
			Message:             o.Status.Message,
		},
		rayClusterName:   o.Status.RayClusterName,
		podSelectorKey:   rayClusterLabel,
		podSelectorValue: o.Status.RayClusterName,
	}, true
}

// filterWorkloads keeps only Workloads that belong to this job. Kueue copies the
// job's tau.azure.com/{job,run-id} labels onto the Workload it admits, so those
// are the primary match. But that copy only happens when tau stamps the labels
// on the Job/RayJob (the finetune `tau run --config` path); Workloads admitted
// for objects tau did not label carry neither. As a fallback we match the
// Workload's ownerReference name against the K8s object name (objName): Kueue
// always sets an ownerReference back to the admitting Job/RayJob, so this scopes
// the namespace-wide list to the one run even when the join labels are absent.
func filterWorkloads(all []links.Workload, objName, jobName, runID string) []links.Workload {
	out := make([]links.Workload, 0, len(all))
	for _, w := range all {
		if w.Job == jobName || (runID != "" && w.RunID == runID) || ownedBy(w, objName) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ownedBy reports whether objName (the RayJob/Job K8s object name) is one of the
// Workload's ownerReferences. An empty objName never matches.
func ownedBy(w links.Workload, objName string) bool {
	if objName == "" {
		return false
	}
	for _, owner := range w.Owners {
		if owner == objName {
			return true
		}
	}
	return false
}

// podList is the subset of the core v1 Pod list the detail page reads.
type podList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				RestartCount int `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// parsePodsWithStatus keeps pods whose selectorKey label equals selectorValue.
// An empty selectorValue matches nothing until the RayCluster exists.
func parsePodsWithStatus(data []byte, selectorKey, selectorValue string) ([]PodDetail, bool) {
	if selectorValue == "" {
		return nil, false
	}
	var list podList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, false
	}
	var out []PodDetail
	for _, it := range list.Items {
		if it.Metadata.Labels[selectorKey] != selectorValue {
			continue
		}
		restarts := 0
		for _, cs := range it.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
		out = append(out, PodDetail{
			Name:     it.Metadata.Name,
			Phase:    it.Status.Phase,
			Node:     it.Spec.NodeName,
			Restarts: restarts,
			NodePath: links.ClusterInstancePath(it.Spec.NodeName),
		})
	}
	return out, true
}

func rayResourceRelease(object ObjectDetail, workloads []links.Workload, pods []PodDetail, podsVisible bool) *ResourceReleaseDetail {
	detail := &ResourceReleaseDetail{QuotaState: "unknown", ComputeState: "unknown"}
	if len(workloads) > 0 {
		allFinished := true
		anyAdmitted := false
		for _, workload := range workloads {
			if !workload.Finished {
				allFinished = false
			}
			anyAdmitted = anyAdmitted || workload.Admitted
		}
		switch {
		case allFinished:
			detail.QuotaState = "released"
		case anyAdmitted:
			detail.QuotaState = "reserved"
		default:
			detail.QuotaState = "pending"
		}
	}

	nodes := map[string]bool{}
	for _, pod := range pods {
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
			continue
		}
		detail.ActivePods++
		if pod.Node != "" {
			nodes[pod.Node] = true
		}
	}
	for node := range nodes {
		detail.Nodes = append(detail.Nodes, node)
	}
	sort.Strings(detail.Nodes)

	terminal := strings.EqualFold(object.JobDeploymentStatus, "Complete") ||
		strings.EqualFold(object.JobDeploymentStatus, "Failed")
	if detail.ActivePods > 0 {
		detail.ComputeState = "in-use"
		if terminal {
			detail.ComputeState = "releasing"
		}
	} else if terminal {
		detail.ComputeState = "reusable"
	}

	if object.ManagedBy == "kueue.x-k8s.io/multikueue" && detail.ActivePods == 0 {
		detail.ComputeState = "unknown"
		detail.Message = "Manager view only: Kueue quota may be released before worker-cluster GPUs are reusable."
		return detail
	}
	if !podsVisible {
		detail.ComputeState = "unknown"
		detail.Message = "Ray pod state is unavailable; physical resource reusability cannot be confirmed."
		return detail
	}

	switch {
	case detail.QuotaState == "released" && detail.ComputeState == "releasing":
		detail.Message = fmt.Sprintf("Quota released, but %d Ray pod(s) still hold %d node(s); resources are not reusable yet.", detail.ActivePods, len(detail.Nodes))
	case detail.QuotaState == "released" && detail.ComputeState == "reusable":
		detail.Message = "Quota released and no active Ray pods remain; resources are reusable."
	case detail.ComputeState == "in-use":
		detail.Message = "Ray pods are still running and hold their assigned nodes."
	case detail.ComputeState == "reusable":
		detail.Message = "No active Ray pods remain; physical resources are reusable."
	default:
		detail.Message = "Physical resource reusability cannot be confirmed from the visible Ray pods."
	}
	return detail
}

// eventList is the subset of the core v1 Event list the detail page reads.
// A modern core/v1.Event may leave the legacy lastTimestamp empty and instead
// carry the time in eventTime (a single MicroTime) or, for aggregated events, in
// series.lastObservedTime; parseEvents falls back through all three.
type eventList struct {
	Items []struct {
		Type          string `json:"type"`
		Reason        string `json:"reason"`
		Message       string `json:"message"`
		Count         int    `json:"count"`
		LastTimestamp string `json:"lastTimestamp"`
		EventTime     string `json:"eventTime"`
		Series        struct {
			LastObservedTime string `json:"lastObservedTime"`
		} `json:"series"`
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
	} `json:"items"`
}

// parseEvents keeps events relevant to the run: those emitted against the
// Job/RayJob object itself, and — crucially — those emitted against its Pods
// (OOMKilled, FailedScheduling, image-pull failures land on the Pod, not the
// Job). Pods are named <owner>-<suffix>, so an exact match or an owner+"-"
// prefix match catches both without attributing a sibling whose name merely
// starts with the same string (e.g. "train" must not match "train-big"'s
// events). Newest last-timestamp first.
func parseEvents(data []byte, jobName, rayClusterName string) []EventDetail {
	var list eventList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil
	}
	var out []EventDetail
	for _, it := range list.Items {
		name := it.InvolvedObject.Name
		if !eventBelongsTo(name, jobName) && !eventBelongsTo(name, rayClusterName) {
			continue
		}
		out = append(out, EventDetail{
			Type:    it.Type,
			Reason:  it.Reason,
			Message: it.Message,
			Count:   it.Count,
			Last:    resolveEventTime(it.LastTimestamp, it.EventTime, it.Series.LastObservedTime),
		})
	}
	if len(out) == 0 {
		return nil
	}
	sortEventsNewestFirst(out)
	return out
}

// eventBelongsTo reports whether an event's involved-object name belongs to the
// given owner: either the object itself (exact match) or one of its Pods, which
// KubeRay/Job controllers name <owner>-<suffix>. The owner+"-" prefix guard
// stops a sibling whose name merely starts with the same string (e.g. "train"
// must not swallow "train-big"'s events). An empty owner matches nothing.
func eventBelongsTo(name, owner string) bool {
	if owner == "" {
		return false
	}
	return name == owner || strings.HasPrefix(name, owner+"-")
}

// lifecycle derives the run's durable lifecycle from the metrics table's
// tau/run_status terminal marker — the same signal Stellar's cockpit reads —
// rather than the writer-less TauExpRunLifecycle projection. The metrics-offload
// sidecar remote-writes ExperimentMetrics (including a step-less tau/run_status
// row whose value sign and tags encode the terminal state); querying that table
// is what actually lights up tier 3 for offloaded runs. Any error (including
// ErrNoQueryCommand) or the absence of a terminal marker yields ok=false so tier
// 3 is simply omitted (and, with it, the Stellar deep-link).
func lifecycle(ctx context.Context, q kustoquery.Querier, runID string) (*LifecycleRow, bool) {
	rows, err := q.Query(ctx, runStatusQuery(runID))
	if err != nil {
		return nil, false
	}
	row, ok := latestRunStatusRow(rows)
	if !ok {
		return nil, false
	}
	tags := runStatusTags(row)
	state := runStatusState(row, tags)
	if state != "succeeded" && state != "failed" && state != "cancelled" {
		// No terminal marker yet: don't emit a lifecycle row or the Stellar link.
		return nil, false
	}
	return &LifecycleRow{
		State:          state,
		EffectiveState: state,
		Reason:         strings.TrimSpace(tags[expkusto.RunStatusReasonTag]),
		Message:        strings.TrimSpace(tags[expkusto.RunStatusMessageTag]),
		CompletionTime: row.Str("wall_time"),
		ArtifactURI:    strings.TrimSpace(tags[expkusto.RunStatusArtifactURITag]),
		CheckpointURI:  strings.TrimSpace(tags[expkusto.RunStatusCheckpointURITag]),
		Project:        strings.TrimSpace(row.Str("project_id")),
	}, true
}

// runStatusQuery builds a step-less KQL over the remote-write metrics table for
// the run's tau/run_status marker rows, newest first. A dedicated query is
// required because the standard metrics query drops step-less rows (the marker
// carries no step).
func runStatusQuery(runID string) string {
	var b strings.Builder
	b.WriteString(expkusto.DefaultRemoteWriteTable + "\n")
	// Labels['project'] uses bracket notation because `project` is a KQL reserved
	// keyword; the dotted form Labels.project fails to parse (HTTP 400).
	b.WriteString("| extend run_id=tostring(Labels.run_id), metric_name=tostring(Labels.metric_name), tags=tostring(Labels.tags), project_id=tostring(Labels['project']), value=todouble(Value), wall_time=Timestamp\n")
	b.WriteString("| where run_id == " + kustoquery.QuoteString(runID) + "\n")
	b.WriteString("| where metric_name == " + kustoquery.QuoteString(expkusto.RunStatusMetricName) + "\n")
	b.WriteString("| project run_id, metric_name, value, wall_time, tags, project_id\n")
	b.WriteString("| order by wall_time desc\n")
	return b.String()
}

// latestRunStatusRow returns the newest tau/run_status row. The query already
// orders by wall_time desc, but tolerate unordered input by scanning. wall_time
// is compared as a parsed timestamp (falling back to lexical order only when it
// does not parse) so mixed offsets/precisions don't misorder the marker.
func latestRunStatusRow(rows []kustoquery.Row) (kustoquery.Row, bool) {
	var latest kustoquery.Row
	var latestWall time.Time
	ok := false
	for _, row := range rows {
		wall := parseRunStatusWallTime(row.Str("wall_time"))
		if !ok || wall.After(latestWall) {
			latest = row
			latestWall = wall
			ok = true
		}
	}
	return latest, ok
}

// parseRunStatusWallTime parses the marker's wall_time. ADX renders a datetime
// as RFC3339 (often with nanosecond precision), so try that first; on failure
// return the zero time so a malformed value sorts oldest rather than winning.
func parseRunStatusWallTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// runStatusState mirrors expcockpit.kustoRunStatusState: prefer the explicit
// tau.status.state tag, else derive from the value sign (>0 succeeded, <-1
// cancelled, <0 failed, else running).
func runStatusState(row kustoquery.Row, tags map[string]string) string {
	if state := normalizeRunStatusState(tags[expkusto.RunStatusStateTag]); state != "" {
		switch state {
		case "succeeded", "failed", "cancelled":
			return state
		}
	}
	value, _ := row.Num("value")
	switch {
	case value > 0:
		return "succeeded"
	case value < -1:
		return "cancelled"
	case value < 0:
		return "failed"
	default:
		return "running"
	}
}

func normalizeRunStatusState(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "success", "successful", "completed", "complete", "done":
		return "succeeded"
	case "cancel", "canceled":
		return "cancelled"
	default:
		return value
	}
}

// runStatusTags JSON-decodes the marker's tags column into a string map.
func runStatusTags(row kustoquery.Row) map[string]string {
	raw := strings.TrimSpace(row.Str("tags"))
	if raw == "" || raw == "{}" {
		return nil
	}
	values := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = fmt.Sprint(value)
	}
	return out
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// optionalTime returns nil for an unparseable/absent (zero) time so a false
// "0001-01-01T00:00:00Z" is never serialized; otherwise a pointer to the value.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// resolveEventTime parses the given timestamp sources in order (lastTimestamp →
// eventTime → series.lastObservedTime) and returns the first that parses. When
// none parse it returns nil so the event's Last is omitted rather than emitted
// as the false zero time.
func resolveEventTime(sources ...string) *time.Time {
	for _, s := range sources {
		if t := parseTime(s); !t.IsZero() {
			return &t
		}
	}
	return nil
}

// sortEventsNewestFirst orders events by Last descending. A nil Last (no
// resolvable timestamp) sorts oldest so timestamped events lead.
func sortEventsNewestFirst(events []EventDetail) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && eventLater(events[j].Last, events[j-1].Last); j-- {
			events[j], events[j-1] = events[j-1], events[j]
		}
	}
}

// eventLater reports whether a is strictly newer than b, treating nil as oldest.
func eventLater(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

// rayClusterDiscoverable reports whether <namespace>/<cluster> resolves to a
// head Service the portal's Ray proxy can dial. It runs the same head-Service
// scan (ray.Board) that validateRayTarget uses, so the Job-detail link is
// marked reachable exactly when the proxy would resolve it — no clickable link
// that 404s. A list error or an absent cluster yields false (grey the link)
// rather than a default-true guess, because the proxy would itself fail closed.
func rayClusterDiscoverable(ctx context.Context, r ray.Reader, namespace, cluster string) bool {
	snap, err := ray.Board(ctx, r, ray.Options{Namespace: namespace})
	if err != nil {
		return false
	}
	for _, c := range snap.Clusters {
		if c.Namespace == namespace && c.Name == cluster {
			return true
		}
	}
	return false
}
