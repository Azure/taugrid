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
//   - Tier 2 (cross-links): an "Open in Stellar" deep-link backed by indexed
//     metrics and a per-pod Cluster board link (links.ClusterInstancePath).
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
	"math"
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
	"github.com/Azure/taugrid/portal/internal/portal/workloadlogs"
	"github.com/Azure/taugrid/portal/internal/portal/workloadtelemetry"
)

// rayClusterLabel is the label KubeRay stamps on a RayJob's pods (value is the
// owning RayCluster's name). Used to select a RayJob's pods.
const (
	rayClusterLabel       = "ray.io/cluster"
	jobNameLabel          = "job-name"
	qualifiedJobNameLabel = "batch.kubernetes.io/job-name"
)

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
	GetRayCluster(ctx context.Context, namespace, name string) ([]byte, error)
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

type rayServiceReader interface {
	GetRayService(ctx context.Context, namespace, name string) ([]byte, error)
}

// Options scopes the detail read to one object by namespace and name.
type Options struct {
	Namespace string
	Name      string
	// WorkspaceID is the authoritative selected workspace, not a normalized
	// object label. Empty retains legacy cross-workspace discovery.
	WorkspaceID string
	Cluster     string
}

// Snapshot is the job detail payload, designed for the page rather than reusing
// a board shape. Optional tiers are omitted when empty so the frontend can
// render each independently.
type Snapshot struct {
	Namespace   string                     `json:"namespace"`
	Name        string                     `json:"name"`
	Kind        string                     `json:"kind"` // Job | RayJob | Pod | RayService
	ResourceUID string                     `json:"resourceUid,omitempty"`
	ObjectState string                     `json:"objectState"` // live | deleted
	Status      string                     `json:"status"`
	RunID       string                     `json:"runId,omitempty"`
	Object      ObjectDetail               `json:"object"`
	Workloads   []links.Workload           `json:"workloads,omitempty"`
	Pods        []PodDetail                `json:"pods,omitempty"`
	Events      []EventDetail              `json:"events,omitempty"`
	Links       DetailLinks                `json:"links"`
	Lifecycle   *LifecycleRow              `json:"lifecycle,omitempty"`
	History     []runs.LifecycleEvent      `json:"history,omitempty"`
	Telemetry   *workloadtelemetry.Summary `json:"telemetry,omitempty"`
	Stages      LifecycleStages            `json:"stages"`
	// ResourceRelease distinguishes scheduler quota accounting from physical
	// Ray pod teardown. It is populated for RayJobs after Workloads and Pods are
	// read so the UI never treats "Finished" as proof that GPUs are reusable.
	ResourceRelease *ResourceReleaseDetail `json:"resourceRelease,omitempty"`
	// Experiment is the Stellar identity `tau run` stamped on this object. It is
	// omitted for a workload that carries none (a bare Job, or a run submitted
	// without experiment metadata).
	Experiment  *ExperimentIdentity `json:"experiment,omitempty"`
	Diagnostics Diagnostics         `json:"diagnostics"`
}

type WorkloadReference struct {
	Kind string
	Name string
	UID  string
}

func SupportsKind(r Reader, kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "job", "rayjob", "pod":
		return true
	case "rayservice":
		_, ok := r.(rayServiceReader)
		return ok
	default:
		return false
	}
}

// Diagnostics keeps source failures distinct from successful empty reads.
type Diagnostics struct {
	Workloads SourceDiagnostic `json:"workloads"`
	Pods      SourceDiagnostic `json:"pods"`
	Events    SourceDiagnostic `json:"events"`
	Tracking  SourceDiagnostic `json:"tracking"`
	Telemetry SourceDiagnostic `json:"telemetry"`
}

type LifecycleStages struct {
	Object      string `json:"object"`
	Admission   string `json:"admission"`
	Scheduling  string `json:"scheduling"`
	Application string `json:"application"`
	Tracking    string `json:"tracking"`
}

type SourceDiagnostic struct {
	State   string `json:"state"` // ready | empty | unavailable | not_configured
	Message string `json:"message,omitempty"`
}

func sourceDiagnostic(source string, count int, err error) SourceDiagnostic {
	if err != nil {
		return SourceDiagnostic{State: "unavailable", Message: source + " could not be read. Retry this detail view."}
	}
	if count == 0 {
		return SourceDiagnostic{State: "empty", Message: "No matching " + source + " were found."}
	}
	return SourceDiagnostic{State: "ready"}
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
	ExecutionTarget     string `json:"executionTarget,omitempty"`
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
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	Node       string            `json:"node,omitempty"`
	Restarts   int               `json:"restarts"`
	Containers []ContainerDetail `json:"containers,omitempty"`
	StartedAt  *time.Time        `json:"startedAt,omitempty"`
	// NodePath deep-links the Cluster board to this pod's node (empty when the
	// pod is unscheduled).
	NodePath string `json:"nodePath,omitempty"`
}

type ContainerDetail struct {
	Name              string `json:"name"`
	Ready             bool   `json:"ready"`
	Restarts          int    `json:"restarts"`
	State             string `json:"state,omitempty"`
	Reason            string `json:"reason,omitempty"`
	Message           string `json:"message,omitempty"`
	PreviousAvailable bool   `json:"previousAvailable,omitempty"`
}

// EventDetail is one recent Kubernetes event for troubleshooting. Last is a
// pointer so an event with no resolvable timestamp is omitted rather than
// serialized as the false "0001-01-01T00:00:00Z" zero time (which the frontend
// would render as a truthy date). Modern core/v1 Events may leave the legacy
// lastTimestamp empty and carry the time in eventTime or series.lastObservedTime
// instead, so parseEvents falls back through all three.
type EventDetail struct {
	Type             string     `json:"type"`
	Reason           string     `json:"reason"`
	Message          string     `json:"message"`
	Count            int        `json:"count"`
	Last             *time.Time `json:"last,omitempty"`
	Truncated        bool       `json:"truncated,omitempty"`
	RedactionApplied bool       `json:"redactionApplied,omitempty"`
}

// DetailLinks holds the tier-2 cross-links. StellarPath requires indexed metrics
// or a terminal lifecycle marker with an unambiguous project/workspace identity.
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
	return detailResolved(ctx, r, q, opts, obj)
}

func DetailReference(ctx context.Context, r Reader, q kustoquery.Querier, opts Options, ref WorkloadReference) (Snapshot, error) {
	if r == nil {
		return Snapshot{}, errors.New("jobdetail: nil reader")
	}
	obj, err := resolveReference(ctx, r, opts, ref)
	if err != nil {
		return Snapshot{}, err
	}
	return detailResolved(ctx, r, q, opts, obj)
}

func detailResolved(ctx context.Context, r Reader, q kustoquery.Querier, opts Options, obj resolved) (Snapshot, error) {
	snap := Snapshot{
		Namespace:   opts.Namespace,
		Name:        opts.Name,
		Kind:        obj.kind,
		ResourceUID: obj.uid,
		ObjectState: "live",
		Status:      obj.status,
		RunID:       obj.runID,
		Object:      safeObjectDetail(obj.detail),
	}
	if !obj.experiment.empty() {
		identity := obj.experiment
		snap.Experiment = &identity
	}

	// Tier 1b: Workloads admitted for this job (best-effort).
	wls, workloadErr := links.ListWorkloads(ctx, r, opts.Namespace)
	if workloadErr == nil {
		snap.Workloads = filterWorkloads(wls, obj)
	}
	snap.Diagnostics.Workloads = sourceDiagnostic("workloads", len(snap.Workloads), workloadErr)
	if apierrors.IsNotFound(workloadErr) {
		snap.Diagnostics.Workloads = SourceDiagnostic{State: "not_configured", Message: "Kueue workloads are not available in this cluster."}
	}

	podOwnerUID := obj.uid
	rayClusterUID := ""
	var rayOwnershipErr error
	if (obj.kind == "RayJob" || obj.kind == "RayService") && obj.uid != "" {
		podOwnerUID = ""
		if obj.rayClusterName == "" {
			rayOwnershipErr = errors.New("RayCluster identity is not available")
		} else {
			raw, err := r.GetRayCluster(ctx, opts.Namespace, obj.rayClusterName)
			if err != nil {
				rayOwnershipErr = fmt.Errorf("read RayCluster ownership: %w", err)
			} else {
				rayClusterUID, rayOwnershipErr = parseOwnedObjectUID(raw, obj.uid)
			}
			podOwnerUID = rayClusterUID
		}
	}

	// Tier 2: per-job Ray dashboard deep-link. UID-bearing RayJobs only expose
	// the link after proving the named RayCluster belongs to this incarnation.
	// Legacy UID-less payloads retain name-based compatibility behavior.
	if obj.rayClusterName != "" && (obj.uid == "" || rayOwnershipErr == nil && rayClusterUID != "") {
		snap.Links.RayDashboardPath = links.RayDashboardPath(opts.Namespace, obj.rayClusterName)
		// Reachability uses the same head-Service discovery as the Ray proxy.
		snap.Links.RayDashboardReachable = rayClusterDiscoverable(ctx, r, opts.Namespace, obj.rayClusterName)
	}

	// Tier 1b: Pods backing the run (best-effort). Labels discover compatible
	// candidates; owner UID proves they belong to this object incarnation.
	podUIDs := map[string]string{}
	var podErr error
	if rayOwnershipErr != nil {
		podErr = rayOwnershipErr
	} else {
		rawPods, err := r.ListPods(ctx, opts.Namespace)
		podErr = err
		if podErr == nil {
			if obj.directPod {
				snap.Pods, podUIDs, podErr = parsePodByUID(rawPods, obj.uid)
			} else {
				snap.Pods, podUIDs, podErr = parsePodsWithStatus(rawPods, podOwnerUID, obj.uid != "", obj.podSelectors)
			}
		}
	}
	snap.Diagnostics.Pods = sourceDiagnostic("pods", len(snap.Pods), podErr)
	podsVisible := podErr == nil && (podOwnerUID != "" || obj.uid == "" && hasUsablePodSelector(obj.podSelectors))
	if obj.kind == "RayJob" {
		snap.ResourceRelease = rayResourceRelease(obj.detail, snap.Workloads, snap.Pods, podsVisible)
	}

	// Tier 1b: recent Events (best-effort).
	rawEvents, eventErr := r.ListEvents(ctx, opts.Namespace)
	if eventErr == nil && obj.uid != "" && podErr != nil {
		// UID-fenced Pod Events depend on a complete ownership set. Treat a
		// failed Pod or RayCluster ownership read as an Event-section outage so
		// the frontend can retain the last complete same-incarnation evidence.
		eventErr = fmt.Errorf("resolve event ownership: %w", podErr)
	}
	if eventErr == nil {
		if podUIDs == nil {
			podUIDs = map[string]string{}
		}
		eventObjects := podUIDs
		eventObjects[obj.uid] = obj.kind
		eventObjects[rayClusterUID] = "RayCluster"
		delete(eventObjects, "")
		snap.Events, eventErr = parseEvents(rawEvents, eventObjects, obj.uid != "", opts.Name, obj.rayClusterName)
	}
	snap.Diagnostics.Events = sourceDiagnostic("events", len(snap.Events), eventErr)

	snap.Links.StellarPath, snap.Lifecycle, snap.Diagnostics.Tracking = tracking(ctx, q, obj.runID, opts)
	snap.Telemetry, snap.Diagnostics.Telemetry = workloadTelemetry(ctx, q, snap, opts)
	snap.Stages = lifecycleStages(snap)

	return snap, nil
}

func safeObjectDetail(detail ObjectDetail) ObjectDetail {
	labels := map[string]string{}
	for key, value := range detail.Labels {
		if strings.HasPrefix(key, "tau.azure.com/") || strings.HasPrefix(key, "kueue.x-k8s.io/") || strings.HasPrefix(key, "ray.io/") {
			labels[key] = value
		}
	}
	detail.Labels = labels
	detail.Annotations = nil
	return detail
}

func lifecycleStages(snap Snapshot) LifecycleStages {
	stages := LifecycleStages{Object: snap.ObjectState, Admission: "not_managed", Scheduling: "no_pods", Application: "unknown", Tracking: "unlinked"}
	for _, workload := range snap.Workloads {
		switch {
		case workload.Finished:
			stages.Admission = "finished"
		case workload.Admitted && stages.Admission != "finished":
			stages.Admission = "admitted"
		case stages.Admission == "not_managed":
			stages.Admission = "pending"
		}
	}
	if snap.Diagnostics.Workloads.State == "unavailable" {
		stages.Admission = "unavailable"
	}
	scheduled := 0
	completed := 0
	for _, pod := range snap.Pods {
		if pod.Node != "" {
			scheduled++
		}
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
			completed++
		}
	}
	switch {
	case len(snap.Pods) == 0 && snap.Diagnostics.Pods.State == "unavailable":
		stages.Scheduling = "unavailable"
	case len(snap.Pods) == 0:
		stages.Scheduling = "no_pods"
	case completed == len(snap.Pods):
		stages.Scheduling = "completed"
	case scheduled == 0:
		stages.Scheduling = "unscheduled"
	case scheduled < len(snap.Pods):
		stages.Scheduling = "partially_scheduled"
	default:
		stages.Scheduling = "scheduled"
	}
	switch strings.ToLower(snap.Status) {
	case "pending", "suspended":
		stages.Application = "waiting"
	case "running":
		stages.Application = "running"
	case "succeeded", "complete", "completed":
		stages.Application = "succeeded"
	case "failed", "cancelled", "canceled":
		stages.Application = "failed"
	}
	switch snap.Diagnostics.Tracking.State {
	case "ready":
		stages.Tracking = "linked"
	case "unavailable":
		stages.Tracking = "unavailable"
	}
	return stages
}

func workloadTelemetry(ctx context.Context, q kustoquery.Querier, snap Snapshot, opts Options) (*workloadtelemetry.Summary, SourceDiagnostic) {
	if q == nil {
		return nil, SourceDiagnostic{State: "not_configured", Message: "GPU telemetry is not configured on this Portal."}
	}
	targets := make([]workloadtelemetry.PodTarget, 0, len(snap.Pods))
	start := time.Time{}
	for _, pod := range snap.Pods {
		if pod.Node == "" {
			continue
		}
		targets = append(targets, workloadtelemetry.PodTarget{Pod: pod.Name, Instance: pod.Node})
		if pod.StartedAt != nil && (start.IsZero() || pod.StartedAt.Before(start)) {
			start = *pod.StartedAt
		}
	}
	if len(targets) == 0 {
		return nil, SourceDiagnostic{State: "empty", Message: "GPU telemetry starts after workload pods are placed on nodes."}
	}
	if start.IsZero() && snap.Object.Created != nil {
		start = *snap.Object.Created
	}
	end := time.Now()
	if snap.Lifecycle != nil && snap.Lifecycle.CompletionTime != "" {
		if completed, err := time.Parse(time.RFC3339Nano, snap.Lifecycle.CompletionTime); err == nil {
			end = completed
		}
	}
	summary, err := workloadtelemetry.Fetch(ctx, q, workloadtelemetry.Query{
		Cluster: opts.Cluster, Namespace: opts.Namespace, Pods: targets, Start: start, End: end,
	})
	if err != nil {
		return nil, SourceDiagnostic{State: "unavailable", Message: "Workload GPU telemetry could not be read. Retry this detail view."}
	}
	if summary.SampleCount == 0 {
		return &summary, SourceDiagnostic{State: "empty", Message: "No GPU telemetry samples matched this workload and time range."}
	}
	return &summary, SourceDiagnostic{State: "ready"}
}

// resolved carries the fields extracted from whichever object was found.
type resolved struct {
	kind           string
	name           string
	uid            string
	status         string
	runID          string
	experiment     ExperimentIdentity
	detail         ObjectDetail
	rayClusterName string
	podSelectors   []podLabelSelector
	directPod      bool
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

func resolveReference(ctx context.Context, r Reader, opts Options, ref WorkloadReference) (resolved, error) {
	if ref.UID == "" || ref.Name == "" || !SupportsKind(r, ref.Kind) {
		return resolved{}, ErrNotFound
	}
	var (
		obj resolved
		ok  bool
		err error
	)
	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "job":
		var raw []byte
		raw, err = r.GetJob(ctx, opts.Namespace, ref.Name)
		if err == nil {
			obj, ok = parseJob(raw)
		}
	case "rayjob":
		var raw []byte
		raw, err = r.GetRayJob(ctx, opts.Namespace, ref.Name)
		if err == nil {
			obj, ok = parseRayJob(raw)
		}
	case "pod":
		var raw []byte
		raw, err = r.ListPods(ctx, opts.Namespace)
		if err == nil {
			obj, ok, err = parsePodObject(raw, ref.UID)
		}
	case "rayservice":
		var raw []byte
		raw, err = r.(rayServiceReader).GetRayService(ctx, opts.Namespace, ref.Name)
		if err == nil {
			obj, ok = parseRayService(raw)
		}
	}
	if err != nil {
		if errors.Is(err, errDecode) {
			return resolved{}, err
		}
		if apierrors.IsNotFound(err) {
			return resolved{}, ErrNotFound
		}
		return resolved{}, err
	}
	if !ok {
		return resolved{}, errDecode
	}
	if obj.uid != ref.UID || !strings.EqualFold(obj.kind, ref.Kind) {
		return resolved{}, ErrNotFound
	}
	return obj, nil
}

// objectMeta is the metadata subset shared by Job and RayJob.
type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
}

type jobObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ManagedBy string `json:"managedBy"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type string `json:"type"`
			// Reason and Message carry the Job's terminal explanation
			// ("BackoffLimitExceeded", "DeadlineExceeded"). RayJobs expose the
			// equivalent on status directly and it has always been surfaced;
			// discarding it here made batch Jobs the only workload kind whose
			// terminal cause was invisible in the UI.
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
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

type rayServiceObject struct {
	Metadata objectMeta `json:"metadata"`
	Status   struct {
		ServiceStatus       string `json:"serviceStatus"`
		ActiveServiceStatus struct {
			RayClusterName string `json:"rayClusterName"`
		} `json:"activeServiceStatus"`
		PendingServiceStatus struct {
			RayClusterName string `json:"rayClusterName"`
		} `json:"pendingServiceStatus"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
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
	reason, message := jobTerminalExplanation(o)
	executionTarget := objectExecutionTarget(o.Spec.ManagedBy)
	return resolved{
		kind:   "Job",
		name:   o.Metadata.Name,
		uid:    o.Metadata.UID,
		status: runs.JobStatus(conds, o.Status.Active, o.Status.Succeeded, o.Status.Failed),
		runID:  o.Metadata.Labels[workloadmeta.LabelRunID],

		experiment: experimentIdentity(o.Metadata.Annotations),
		detail: ObjectDetail{
			Created:         optionalTime(created),
			Age:             runs.FormatAge(time.Now(), created),
			Reason:          reason,
			Message:         message,
			Labels:          o.Metadata.Labels,
			Annotations:     o.Metadata.Annotations,
			ManagedBy:       o.Spec.ManagedBy,
			ExecutionTarget: executionTarget,
		},
		// Kubernetes Job controller labels are canonical pod ownership. run-id is
		// Tau's run identity; tau.azure.com/job remains a reader-only compatibility
		// selector for workloads produced by older or custom clients.
		podSelectors: []podLabelSelector{
			{key: jobNameLabel, value: o.Metadata.Name},
			{key: qualifiedJobNameLabel, value: o.Metadata.Name},
			{key: workloadmeta.LabelRunID, value: o.Metadata.Labels[workloadmeta.LabelRunID]},
			{key: workloadmeta.LabelJob, value: o.Metadata.Name},
		},
	}, true
}

// jobTerminalExplanation returns the reason and message of the Job's terminal
// condition. Only Complete and Failed are terminal; a transient condition such
// as Suspended would otherwise present itself as the outcome.
func jobTerminalExplanation(o jobObject) (string, string) {
	for i := len(o.Status.Conditions) - 1; i >= 0; i-- {
		c := o.Status.Conditions[i]
		if !strings.EqualFold(c.Status, "true") {
			continue
		}
		if c.Type == "Complete" || c.Type == "Failed" {
			return c.Reason, c.Message
		}
	}
	return "", ""
}

func parseRayJob(data []byte) (resolved, bool) {
	var o rayJobObject
	if err := json.Unmarshal(data, &o); err != nil || o.Metadata.Name == "" {
		return resolved{}, false
	}
	created := parseTime(o.Metadata.CreationTimestamp)
	executionTarget := objectExecutionTarget(o.Spec.ManagedBy)
	return resolved{
		kind:   "RayJob",
		name:   o.Metadata.Name,
		uid:    o.Metadata.UID,
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
			ExecutionTarget:     executionTarget,
			Reason:              o.Status.Reason,
			Message:             o.Status.Message,
		},
		rayClusterName: o.Status.RayClusterName,
		podSelectors: []podLabelSelector{
			{key: rayClusterLabel, value: o.Status.RayClusterName},
		},
	}, true
}

func parseRayService(data []byte) (resolved, bool) {
	var o rayServiceObject
	if err := json.Unmarshal(data, &o); err != nil || o.Metadata.Name == "" {
		return resolved{}, false
	}
	created := parseTime(o.Metadata.CreationTimestamp)
	clusterName := firstNonEmpty(o.Status.ActiveServiceStatus.RayClusterName, o.Status.PendingServiceStatus.RayClusterName)
	reason, message := "", ""
	for i := len(o.Status.Conditions) - 1; i >= 0; i-- {
		if strings.EqualFold(o.Status.Conditions[i].Status, "true") {
			reason, message = o.Status.Conditions[i].Reason, o.Status.Conditions[i].Message
			break
		}
	}
	return resolved{
		kind:       "RayService",
		name:       o.Metadata.Name,
		uid:        o.Metadata.UID,
		status:     rayServiceStatus(o.Status.ServiceStatus),
		runID:      o.Metadata.Labels[workloadmeta.LabelRunID],
		experiment: experimentIdentity(o.Metadata.Annotations),
		detail: ObjectDetail{
			Created:             optionalTime(created),
			Age:                 runs.FormatAge(time.Now(), created),
			Labels:              o.Metadata.Labels,
			Annotations:         o.Metadata.Annotations,
			JobDeploymentStatus: o.Status.ServiceStatus,
			RayClusterName:      clusterName,
			Reason:              reason,
			Message:             message,
		},
		rayClusterName: clusterName,
		podSelectors: []podLabelSelector{
			{key: rayClusterLabel, value: clusterName},
		},
	}, true
}

func rayServiceStatus(status string) string {
	switch {
	case strings.EqualFold(status, "running"):
		return "Running"
	case strings.Contains(strings.ToLower(status), "fail"):
		return "Failed"
	default:
		return "Pending"
	}
}

func objectExecutionTarget(managedBy string) string {
	if strings.TrimSpace(managedBy) == "kueue.x-k8s.io/multikueue" {
		return "multiKueue"
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// filterWorkloads uses Kueue's owner UID as proof of the exact Job/RayJob
// incarnation. Name and Tau-label matching is retained only for legacy test or
// imported payloads that omit the Kubernetes-assigned object UID.
func filterWorkloads(all []links.Workload, obj resolved) []links.Workload {
	out := make([]links.Workload, 0, len(all))
	for _, w := range all {
		if obj.uid != "" {
			if ownedByUID(w, obj.uid) {
				out = append(out, w)
			}
			continue
		}
		legacyJobName := obj.detail.Labels[workloadmeta.LabelJob]
		if (legacyJobName != "" && w.Job == legacyJobName) ||
			(obj.runID != "" && w.RunID == obj.runID) ||
			ownedByName(w, obj.name) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ownedByName is the compatibility path for UID-less payloads.
func ownedByName(w links.Workload, objName string) bool {
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

func ownedByUID(w links.Workload, uid string) bool {
	for _, ownerUID := range w.OwnerUIDs {
		if ownerUID == uid {
			return true
		}
	}
	return false
}

type ownedObject struct {
	Metadata struct {
		UID             string `json:"uid"`
		OwnerReferences []struct {
			UID        string `json:"uid"`
			Controller *bool  `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
}

func parseOwnedObjectUID(data []byte, ownerUID string) (string, error) {
	var obj ownedObject
	if ownerUID == "" {
		return "", errors.New("owner UID is empty")
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return "", fmt.Errorf("parse owned object: %w", err)
	}
	if obj.Metadata.UID == "" {
		return "", errors.New("owned object UID is empty")
	}
	for _, owner := range obj.Metadata.OwnerReferences {
		if owner.UID == ownerUID && owner.Controller != nil && *owner.Controller {
			return obj.Metadata.UID, nil
		}
	}
	return "", errors.New("object is not controlled by the resolved owner UID")
}

// podList is the subset of the core v1 Pod list the detail page reads.
type podList struct {
	Items []podItem `json:"items"`
}

type podItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		OwnerReferences   []struct {
			UID        string `json:"uid"`
			Controller *bool  `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		StartTime         string `json:"startTime"`
		ContainerStatuses []struct {
			Name         string             `json:"name"`
			Ready        bool               `json:"ready"`
			RestartCount int                `json:"restartCount"`
			State        containerStateJSON `json:"state"`
			LastState    containerStateJSON `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type containerStateJSON struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt string `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason   string `json:"reason"`
		Message  string `json:"message"`
		ExitCode int32  `json:"exitCode"`
	} `json:"terminated"`
}

type podLabelSelector struct {
	key   string
	value string
}

// parsePodsWithStatus uses labels to discover candidates, then validates the
// controller owner UID whenever the resolved Job/RayJob carries a UID.
func parsePodsWithStatus(data []byte, ownerUID string, requireOwnerUID bool, selectors []podLabelSelector) ([]PodDetail, map[string]string, error) {
	if requireOwnerUID && ownerUID == "" || !requireOwnerUID && !hasUsablePodSelector(selectors) {
		return nil, nil, nil
	}
	var list podList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, nil, err
	}
	if list.Items == nil {
		return nil, nil, errors.New("pod response has no items array")
	}
	var out []PodDetail
	uids := map[string]string{}
	for _, it := range list.Items {
		if !podLabelsMatch(it.Metadata.Labels, selectors) {
			continue
		}
		if requireOwnerUID && !metadataControlledByUID(it.Metadata.OwnerReferences, ownerUID) {
			continue
		}
		out = append(out, podDetail(it))
		if it.Metadata.UID != "" {
			uids[it.Metadata.UID] = "Pod"
		}
	}
	return out, uids, nil
}

func parsePodObject(data []byte, uid string) (resolved, bool, error) {
	var list podList
	if err := json.Unmarshal(data, &list); err != nil {
		return resolved{}, false, errDecode
	}
	if list.Items == nil {
		return resolved{}, false, errDecode
	}
	for _, item := range list.Items {
		if item.Metadata.UID != uid {
			continue
		}
		created := parseTime(item.Metadata.CreationTimestamp)
		return resolved{
			kind:       "Pod",
			name:       item.Metadata.Name,
			uid:        item.Metadata.UID,
			status:     firstNonEmpty(item.Status.Phase, "Unknown"),
			runID:      item.Metadata.Labels[workloadmeta.LabelRunID],
			experiment: experimentIdentity(item.Metadata.Annotations),
			detail: ObjectDetail{
				Created:     optionalTime(created),
				Age:         runs.FormatAge(time.Now(), created),
				Labels:      item.Metadata.Labels,
				Annotations: item.Metadata.Annotations,
			},
			directPod: true,
		}, true, nil
	}
	return resolved{}, false, nil
}

func parsePodByUID(data []byte, uid string) ([]PodDetail, map[string]string, error) {
	if uid == "" {
		return nil, nil, errors.New("pod UID is empty")
	}
	var list podList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, nil, err
	}
	if list.Items == nil {
		return nil, nil, errors.New("pod response has no items array")
	}
	for _, item := range list.Items {
		if item.Metadata.UID == uid {
			return []PodDetail{podDetail(item)}, map[string]string{uid: "Pod"}, nil
		}
	}
	return nil, nil, ErrNotFound
}

func podDetail(item podItem) PodDetail {
	restarts := 0
	statuses := make(map[string]struct {
		ready    bool
		restarts int
		state    containerStateJSON
		previous containerStateJSON
	}, len(item.Status.ContainerStatuses))
	for _, cs := range item.Status.ContainerStatuses {
		restarts += cs.RestartCount
		statuses[cs.Name] = struct {
			ready    bool
			restarts int
			state    containerStateJSON
			previous containerStateJSON
		}{cs.Ready, cs.RestartCount, cs.State, cs.LastState}
	}
	containers := make([]ContainerDetail, 0, len(item.Spec.Containers))
	for _, container := range item.Spec.Containers {
		status := statuses[container.Name]
		state, reason, message := describeContainerState(status.state)
		containers = append(containers, ContainerDetail{
			Name: container.Name, Ready: status.ready, Restarts: status.restarts,
			State: state, Reason: reason, Message: message,
			PreviousAvailable: status.previous.Terminated != nil,
		})
	}
	return PodDetail{
		Name:       item.Metadata.Name,
		Phase:      item.Status.Phase,
		Node:       item.Spec.NodeName,
		Restarts:   restarts,
		Containers: containers,
		StartedAt:  optionalTime(parseTime(item.Status.StartTime)),
		NodePath:   links.ClusterInstancePath(item.Spec.NodeName),
	}
}

func describeContainerState(state containerStateJSON) (name, reason, message string) {
	switch {
	case state.Waiting != nil:
		return "waiting", state.Waiting.Reason, state.Waiting.Message
	case state.Running != nil:
		return "running", "", ""
	case state.Terminated != nil:
		return "terminated", state.Terminated.Reason, state.Terminated.Message
	default:
		return "unknown", "", ""
	}
}

func hasUsablePodSelector(selectors []podLabelSelector) bool {
	for _, selector := range selectors {
		if selector.key != "" && selector.value != "" {
			return true
		}
	}
	return false
}

func podLabelsMatch(labels map[string]string, selectors []podLabelSelector) bool {
	for _, selector := range selectors {
		if selector.value != "" && labels[selector.key] == selector.value {
			return true
		}
	}
	return false
}

func metadataControlledByUID(owners []struct {
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}, uid string) bool {
	if uid == "" {
		return false
	}
	for _, owner := range owners {
		if owner.UID == uid && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
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
			UID  string `json:"uid"`
		} `json:"involvedObject"`
	} `json:"items"`
}

// parseEvents matches involvedObject.uid against the resolved workload,
// RayCluster, and Pod UID set. Name-prefix matching remains only for UID-less
// compatibility payloads. Newest last-timestamp first.
func parseEvents(data []byte, objectKinds map[string]string, requireUID bool, jobName, rayClusterName string) ([]EventDetail, error) {
	var list eventList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	if list.Items == nil {
		return nil, errors.New("event response has no items array")
	}
	var out []EventDetail
	for _, it := range list.Items {
		expectedKind, uidMatches := objectKinds[it.InvolvedObject.UID]
		if requireUID && (!uidMatches || it.InvolvedObject.Kind != expectedKind) {
			continue
		}
		if !requireUID && !eventBelongsTo(it.InvolvedObject.Name, jobName) && !eventBelongsTo(it.InvolvedObject.Name, rayClusterName) {
			continue
		}
		message, truncated, redacted := workloadlogs.Sanitize([]byte(it.Message), 4096)
		out = append(out, EventDetail{
			Type:             it.Type,
			Reason:           it.Reason,
			Message:          message,
			Count:            it.Count,
			Last:             resolveEventTime(it.LastTimestamp, it.EventTime, it.Series.LastObservedTime),
			Truncated:        truncated,
			RedactionApplied: redacted,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	sortEventsNewestFirst(out)
	if len(out) > 100 {
		out = out[:100]
	}
	return out, nil
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

func tracking(ctx context.Context, q kustoquery.Querier, runID string, opts Options) (string, *LifecycleRow, SourceDiagnostic) {
	if q == nil {
		return "", nil, SourceDiagnostic{State: "not_configured", Message: "Experiment tracking lookup is not configured."}
	}
	if runID == "" {
		return "", nil, SourceDiagnostic{State: "not_configured", Message: "This workload has no experiment run identity."}
	}
	rows, err := q.Query(ctx, trackingQuery(runID, opts.WorkspaceID, opts.Cluster))
	if err != nil {
		if errors.Is(err, kustoquery.ErrNoQueryCommand) {
			return "", nil, SourceDiagnostic{State: "not_configured", Message: "Experiment tracking lookup is not configured."}
		}
		return "", nil, sourceDiagnostic("Experiment tracking", 0, err)
	}
	var scoped []kustoquery.Row
	project, workspace, cluster := "", "", ""
	hasMetrics := false
	for _, row := range rows {
		if opts.WorkspaceID != "" && row.Str("workspace_id") != opts.WorkspaceID {
			continue
		}
		if opts.Cluster != "" && row.Str("cluster") != opts.Cluster {
			continue
		}
		p := strings.TrimSpace(row.Str("project_id"))
		w := strings.TrimSpace(row.Str("workspace_id"))
		c := strings.TrimSpace(row.Str("cluster"))
		if p == "" || (len(scoped) > 0 && (p != project || w != workspace || c != cluster)) {
			return "", nil, SourceDiagnostic{State: "unavailable", Message: "Indexed tracking identity is missing or ambiguous; a scoped Stellar link cannot be resolved."}
		}
		project, workspace, cluster = p, w, c
		scoped = append(scoped, row)
		_, hasStep := row.Num("step")
		value, hasValue := row.Num("value")
		hasValue = hasValue && !math.IsNaN(value) && !math.IsInf(value, 0)
		hasMetrics = hasMetrics || (row.Str("metric_name") != "" && row.Str("metric_name") != expkusto.RunStatusMetricName && hasStep && hasValue)
	}
	lifecycleRow, _ := lifecycle(scoped)
	if !hasMetrics && lifecycleRow == nil {
		return "", nil, SourceDiagnostic{State: "empty", Message: "No indexed metrics were found; metric offload may be disabled or indexing may still be pending."}
	}
	return links.ExperimentProjectPath(runID, project, workspace), lifecycleRow, SourceDiagnostic{State: "ready"}
}

// lifecycle derives only final status, independently of tracking existence.
func lifecycle(rows []kustoquery.Row) (*LifecycleRow, bool) {
	row, ok := latestRunStatusRow(rows)
	if !ok {
		return nil, false
	}
	tags := runStatusTags(row)
	state := runStatusState(row, tags)
	if state != "succeeded" && state != "failed" && state != "cancelled" {
		// A running marker is not a final lifecycle result.
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

// trackingQuery uses the same step/value eligibility as Stellar metrics while
// retaining step-less lifecycle markers. Two row kinds per identity suffice;
// three returned rows are enough to detect ambiguous projects/workspaces.
func trackingQuery(runID, workspaceID, cluster string) string {
	var b strings.Builder
	b.WriteString(expkusto.DefaultRemoteWriteTable + "\n")
	// Labels['project'] uses bracket notation because `project` is a KQL reserved
	// keyword; the dotted form Labels.project fails to parse (HTTP 400).
	b.WriteString("| extend run_id=tostring(Labels.run_id), metric_name=tostring(Labels.metric_name), tags=tostring(Labels.tags), project_id=tostring(Labels['project']), workspace_id=tostring(Labels.workspace_id), cluster=tostring(Cluster), step=tolong(Labels.step), value=todouble(Value), wall_time=Timestamp\n")
	b.WriteString("| where run_id == " + kustoquery.QuoteString(runID) + "\n")
	if workspaceID != "" {
		b.WriteString("| where workspace_id == " + kustoquery.QuoteString(workspaceID) + "\n")
	}
	if cluster != "" {
		b.WriteString("| where cluster == " + kustoquery.QuoteString(cluster) + "\n")
	}
	b.WriteString("| where isnotempty(metric_name) and isnotnull(value) and isfinite(value)\n")
	b.WriteString("| where isnotnull(step) or metric_name == " + kustoquery.QuoteString(expkusto.RunStatusMetricName) + "\n")
	b.WriteString("| extend row_kind = iff(metric_name == " + kustoquery.QuoteString(expkusto.RunStatusMetricName) + ", 'lifecycle', 'metrics')\n")
	b.WriteString("| summarize arg_max(wall_time, *) by project_id, workspace_id, cluster, row_kind\n")
	b.WriteString("| project run_id, metric_name, step, value, wall_time, tags, project_id, workspace_id, cluster\n")
	b.WriteString("| take 3\n")
	return b.String()
}

// latestRunStatusRow returns the newest tau/run_status row without treating
// ordinary positive-valued training metrics as completion markers.
func latestRunStatusRow(rows []kustoquery.Row) (kustoquery.Row, bool) {
	var latest kustoquery.Row
	var latestWall time.Time
	ok := false
	for _, row := range rows {
		if row.Str("metric_name") != expkusto.RunStatusMetricName {
			continue
		}
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

// rayClusterDiscoverable reports whether <namespace>/<cluster> has a
// discoverable head Service and a reachable dashboard according to ray.Board.
// A list error or absent cluster yields false rather than guessing that a stale
// Service still has a ready head Pod.
func rayClusterDiscoverable(ctx context.Context, r ray.Reader, namespace, cluster string) bool {
	snap, err := ray.Board(ctx, r, ray.Options{Namespace: namespace})
	if err != nil {
		return false
	}
	for _, c := range snap.Clusters {
		if c.Namespace == namespace && c.Name == cluster {
			return c.Available
		}
	}
	return false
}
