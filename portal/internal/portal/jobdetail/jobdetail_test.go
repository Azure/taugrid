// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobdetail

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// fakeReader drives Detail() without a live API. Each read returns its byte
// payload and error independently, so tests can exercise per-source degradation.
type fakeReader struct {
	job, rayJob, pods, events, workloads, services []byte
	jobErr, rayErr, podErr, evtErr, wlErr, svcErr  error
}

func (f fakeReader) GetJob(context.Context, string, string) ([]byte, error) {
	return f.job, f.jobErr
}
func (f fakeReader) GetRayJob(context.Context, string, string) ([]byte, error) {
	return f.rayJob, f.rayErr
}
func (f fakeReader) ListPods(context.Context, string) ([]byte, error) {
	return f.pods, f.podErr
}
func (f fakeReader) ListEvents(context.Context, string) ([]byte, error) {
	return f.events, f.evtErr
}
func (f fakeReader) ListWorkloads(context.Context, string) ([]byte, error) {
	return f.workloads, f.wlErr
}
func (f fakeReader) ListServices(context.Context, string) ([]byte, error) {
	return f.services, f.svcErr
}

// fakeQuerier returns fixed rows (or an error) for the lifecycle query.
type fakeQuerier struct {
	rows []kustoquery.Row
	err  error
}

func (f fakeQuerier) Query(context.Context, string) ([]kustoquery.Row, error) {
	return f.rows, f.err
}

func TestDetailNilReader(t *testing.T) {
	if _, err := Detail(context.Background(), nil, nil, Options{}); err == nil {
		t.Fatal("Detail(nil reader) error = nil, want error")
	}
}

func TestDetailNotFound(t *testing.T) {
	r := fakeReader{
		jobErr: apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "missing"),
		rayErr: apierrors.NewNotFound(schema.GroupResource{Group: "ray.io", Resource: "rayjobs"}, "missing"),
	}
	_, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Detail() error = %v, want ErrNotFound", err)
	}
}

// TestDetailReadErrorPropagates ensures a non-NotFound read failure (e.g. RBAC
// forbidden or a transient API error) is surfaced to the caller rather than
// masquerading as ErrNotFound — so the handler returns 502, not a misleading 404.
func TestDetailReadErrorPropagates(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "ray.io", Resource: "rayjobs"}, "train-a", errors.New("access denied"))
	r := fakeReader{
		rayErr: forbidden,
		jobErr: apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "train-a"),
	}
	_, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-a"})
	if err == nil {
		t.Fatal("Detail() error = nil, want read error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Detail() error = %v, want propagated read error, not ErrNotFound", err)
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Detail() error = %v, want forbidden error", err)
	}
}

// TestDetailJobK8sOnly covers a plain Job: status, run-id, Stellar link, pods
// filtered by the job label, and workloads scoped to the job. No Querier → no
// lifecycle tier.
func TestDetailJobK8sOnly(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job: []byte(`{"metadata":{"name":"train-a","namespace":"ray","creationTimestamp":"2026-07-02T10:00:00Z",
			"labels":{"` + workloadmeta.LabelJob + `":"train-a","` + workloadmeta.LabelRunID + `":"run-123"}},
			"status":{"active":1}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"train-a-abc","labels":{"` + workloadmeta.LabelJob + `":"train-a"}},"spec":{"nodeName":"gpu-node-1"},"status":{"phase":"Running","containerStatuses":[{"restartCount":2}]}},
			{"metadata":{"name":"other-pod","labels":{"` + workloadmeta.LabelJob + `":"other"}},"spec":{"nodeName":"gpu-node-2"},"status":{"phase":"Running"}}
		]}`),
		events: []byte(`{"items":[
			{"type":"Warning","reason":"FailedScheduling","message":"no nodes","count":3,"lastTimestamp":"2026-07-02T10:01:00Z","involvedObject":{"kind":"Job","name":"train-a"}},
			{"type":"Normal","reason":"Other","message":"x","involvedObject":{"kind":"Job","name":"someone-else"}}
		]}`),
		workloads: []byte(`{"items":[
			{"metadata":{"name":"wl-1","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"train-a"}},"spec":{"queueName":"jobqueue"},"status":{"conditions":[{"type":"Admitted","status":"True"}]}},
			{"metadata":{"name":"wl-2","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"nope"}},"spec":{"queueName":"jobqueue"}}
		]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-a"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Kind != "Job" || snap.Status != "Running" {
		t.Fatalf("Kind/Status = %s/%s, want Job/Running", snap.Kind, snap.Status)
	}
	if snap.RunID != "run-123" {
		t.Fatalf("RunID = %q, want run-123", snap.RunID)
	}
	if snap.Links.StellarPath != "" {
		t.Fatalf("StellarPath = %q, want empty (no Querier ⇒ no durable Kusto proof)", snap.Links.StellarPath)
	}
	if len(snap.Pods) != 1 || snap.Pods[0].Name != "train-a-abc" || snap.Pods[0].Restarts != 2 {
		t.Fatalf("Pods = %+v, want one train-a-abc with 2 restarts", snap.Pods)
	}
	if snap.Pods[0].NodePath == "" {
		t.Fatal("Pod NodePath empty, want a cluster deep link")
	}
	if len(snap.Events) != 1 || snap.Events[0].Reason != "FailedScheduling" {
		t.Fatalf("Events = %+v, want one FailedScheduling", snap.Events)
	}
	if snap.Events[0].Last == nil || snap.Events[0].Last.IsZero() {
		t.Fatalf("Event Last = %v, want the parsed lastTimestamp", snap.Events[0].Last)
	}
	if len(snap.Workloads) != 1 || snap.Workloads[0].Name != "wl-1" {
		t.Fatalf("Workloads = %+v, want one wl-1", snap.Workloads)
	}
	if snap.Lifecycle != nil {
		t.Fatalf("Lifecycle = %+v, want nil (no Querier)", snap.Lifecycle)
	}
}

// TestDetailWorkloadJoinByOwnerReference covers the Gap #1 fallback: a Kueue
// Workload admitted for a RayJob that carries no tau.azure.com/{job,run-id}
// labels (Kueue only copies labels tau stamped) is still scoped to the run by
// matching its ownerReference name against the RayJob object name.
func TestDetailWorkloadJoinByOwnerReference(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"portal-e2e","namespace":"ray","labels":{"` + workloadmeta.LabelRunID + `":"portal-e2e"}},
			"status":{"jobDeploymentStatus":"Running","rayClusterName":"portal-e2e-raycluster"}}`),
		workloads: []byte(`{"items":[
			{"metadata":{"name":"rayjob-portal-e2e-xyz","namespace":"ray","ownerReferences":[{"name":"portal-e2e"}]},"spec":{"queueName":"team-a"},"status":{"conditions":[{"type":"Admitted","status":"True"}]}},
			{"metadata":{"name":"wl-unrelated","namespace":"ray","ownerReferences":[{"name":"someone-else"}]},"spec":{"queueName":"team-a"}}
		]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "portal-e2e"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if len(snap.Workloads) != 1 || snap.Workloads[0].Name != "rayjob-portal-e2e-xyz" {
		t.Fatalf("Workloads = %+v, want one owned rayjob-portal-e2e-xyz", snap.Workloads)
	}
	if !snap.Workloads[0].Admitted {
		t.Fatalf("owned workload should be Admitted: %+v", snap.Workloads[0])
	}
}

// TestDetailRayJobPrefersRayJob confirms a RayJob is resolved first, its native
// status fields are surfaced, and pods are selected by ray.io/cluster.
func TestDetailRayJobPrefersRayJob(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-train","namespace":"ray","creationTimestamp":"2026-07-02T10:00:00Z",
			"labels":{"` + workloadmeta.LabelRunID + `":"run-9"}},
			"status":{"jobDeploymentStatus":"Running","jobId":"job-xyz","rayClusterName":"ray-train-raycluster"}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"head","labels":{"ray.io/cluster":"ray-train-raycluster"}},"spec":{"nodeName":"n1"},"status":{"phase":"Running"}}
		]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "ray-train"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Kind != "RayJob" || snap.Status != "Running" {
		t.Fatalf("Kind/Status = %s/%s, want RayJob/Running", snap.Kind, snap.Status)
	}
	if snap.Object.RayClusterName != "ray-train-raycluster" || snap.Object.JobID != "job-xyz" {
		t.Fatalf("Object = %+v, want native ray fields", snap.Object)
	}
	if len(snap.Pods) != 1 || snap.Pods[0].Name != "head" {
		t.Fatalf("Pods = %+v, want one head pod", snap.Pods)
	}
	// A RayJob with a named RayCluster gets a per-job Ray dashboard deep-link.
	if snap.Links.RayDashboardPath != "/api/portal/ray/proxy/ray/ray-train-raycluster/" {
		t.Fatalf("RayDashboardPath = %q, want the head-svc proxy path", snap.Links.RayDashboardPath)
	}
}

func TestDetailRayJobDistinguishesQuotaReleaseFromComputeReuse(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"whole-node-h200","namespace":"tau"},
			"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED","rayClusterName":"whole-node-h200-cluster"}}`),
		workloads: []byte(`{"items":[{
			"metadata":{"name":"rayjob-whole-node-h200","ownerReferences":[{"name":"whole-node-h200"}]},
			"spec":{"queueName":"jobqueue"},
			"status":{"conditions":[{"type":"Finished","status":"True"}]}
		}]}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"head","labels":{"ray.io/cluster":"whole-node-h200-cluster"}},"spec":{"nodeName":"h200-a"},"status":{"phase":"Running"}},
			{"metadata":{"name":"worker","labels":{"ray.io/cluster":"whole-node-h200-cluster"}},"spec":{"nodeName":"h200-b"},"status":{"phase":"Running"}}
		]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "tau", Name: "whole-node-h200"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	release := snap.ResourceRelease
	if release == nil {
		t.Fatal("ResourceRelease = nil")
	}
	if release.QuotaState != "released" || release.ComputeState != "releasing" || release.ActivePods != 2 {
		t.Fatalf("ResourceRelease = %+v, want released quota with two pods still releasing", release)
	}
	if got := strings.Join(release.Nodes, ","); got != "h200-a,h200-b" {
		t.Fatalf("ResourceRelease.Nodes = %q, want h200-a,h200-b", got)
	}
	if !strings.Contains(release.Message, "resources are not reusable yet") {
		t.Fatalf("ResourceRelease.Message = %q", release.Message)
	}
}

func TestDetailManagerRayJobLeavesComputeReuseUnknown(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"multikueue-h200","namespace":"tau"},
			"spec":{"managedBy":"kueue.x-k8s.io/multikueue"},
			"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED","rayClusterName":"worker-only-cluster"}}`),
		workloads: []byte(`{"items":[{
			"metadata":{"name":"rayjob-multikueue-h200","ownerReferences":[{"name":"multikueue-h200"}]},
			"status":{"conditions":[{"type":"Finished","status":"True"}]}
		}]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "tau", Name: "multikueue-h200"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	release := snap.ResourceRelease
	if release == nil || release.QuotaState != "released" || release.ComputeState != "unknown" {
		t.Fatalf("ResourceRelease = %+v, want released quota and unknown worker compute", release)
	}
	if !strings.Contains(release.Message, "Manager view only") {
		t.Fatalf("ResourceRelease.Message = %q", release.Message)
	}
	if snap.Object.ExecutionTarget != "multiKueue" {
		t.Fatalf("MultiKueue object identity = %+v", snap.Object)
	}
}

func TestDetailPendingWorkloadDoesNotClaimQuotaReserved(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"queued-ray","namespace":"tau"},
			"status":{"jobDeploymentStatus":"Initializing","rayClusterName":"queued-ray-cluster"}}`),
		workloads: []byte(`{"items":[{
			"metadata":{"name":"rayjob-queued-ray","ownerReferences":[{"name":"queued-ray"}]},
			"spec":{"queueName":"jobqueue"}
		}]}`),
		pods: []byte(`{"items":[]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "tau", Name: "queued-ray"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.ResourceRelease == nil || snap.ResourceRelease.QuotaState != "pending" {
		t.Fatalf("ResourceRelease = %+v, want pending quota", snap.ResourceRelease)
	}
}

func TestDetailRayJobDoesNotClaimComputeReusableWhenPodsUnreadable(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-complete","namespace":"tau"},
			"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED","rayClusterName":"ray-complete-cluster"}}`),
		podErr: errors.New("pods forbidden"),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "tau", Name: "ray-complete"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.ResourceRelease == nil || snap.ResourceRelease.ComputeState != "unknown" {
		t.Fatalf("ResourceRelease = %+v, want unknown compute state", snap.ResourceRelease)
	}
	if !strings.Contains(snap.ResourceRelease.Message, "cannot be confirmed") {
		t.Fatalf("ResourceRelease.Message = %q", snap.ResourceRelease.Message)
	}
}

// TestDetailNoRunIDOmitsStellar confirms a job without a run-id omits the
// Stellar link (no dead link) and skips the Kusto tier even with a Querier.
func TestDetailNoRunIDOmitsStellar(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"j","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"j"}},"status":{"active":1}}`),
	}
	q := fakeQuerier{rows: []kustoquery.Row{{"state": "Completed"}}}
	snap, err := Detail(context.Background(), r, q, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Links.StellarPath != "" {
		t.Fatalf("StellarPath = %q, want empty (no run-id)", snap.Links.StellarPath)
	}
	if snap.Lifecycle != nil {
		t.Fatal("Lifecycle set, want nil (no run-id skips tier 3)")
	}
}

// TestDetailKustoLifecycle confirms tier 3 populates from the Querier when a
// run-id is present.
func TestDetailKustoLifecycle(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"j","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"j","` + workloadmeta.LabelRunID + `":"run-1"}},"status":{"conditions":[{"type":"Complete","status":"True"}]}}`),
	}
	q := fakeQuerier{rows: []kustoquery.Row{{
		"metric_name": "tau/run_status",
		"value":       1.0,
		"wall_time":   "2026-07-02T10:05:00Z",
		"project_id":  "portal-e2e",
		"tags":        `{"tau.status.reason":"Succeeded","tau.status.artifact_uri":"az://bucket/artifacts","tau.status.checkpoint_uri":"az://bucket/ckpt"}`,
	}}}
	snap, err := Detail(context.Background(), r, q, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Status != "Complete" {
		t.Fatalf("Status = %q, want Complete", snap.Status)
	}
	if snap.Lifecycle == nil {
		t.Fatal("Lifecycle = nil, want a row")
	}
	if snap.Lifecycle.ArtifactURI != "az://bucket/artifacts" || snap.Lifecycle.CheckpointURI != "az://bucket/ckpt" {
		t.Fatalf("Lifecycle = %+v, want artifact/checkpoint URIs", snap.Lifecycle)
	}
	if snap.Links.StellarPath == "" {
		t.Fatal("StellarPath empty, want a deep link when a durable lifecycle row exists")
	}
	if snap.Links.StellarPath != "/stellar?target=run-1&project=portal-e2e" {
		t.Fatalf("StellarPath = %q, want run-id target + project= disambiguator", snap.Links.StellarPath)
	}
}

// TestDetailRayJobWithLifecyclePreservesBothLinks is the regression for peni's
// review: a running RayJob that also has a durable Kusto lifecycle row must emit
// BOTH the Ray dashboard deep-link and the Stellar deep-link (they are
// independent tiers), and — because its head Service is discoverable by the same
// head-Service scan the proxy uses — RayDashboardReachable must be true so the
// frontend renders the dashboard link enabled.
func TestDetailRayJobWithLifecyclePreservesBothLinks(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-train","namespace":"ray","creationTimestamp":"2026-07-02T10:00:00Z",
			"labels":{"` + workloadmeta.LabelRunID + `":"run-7"}},
			"status":{"jobDeploymentStatus":"Running","jobId":"job-abc","rayClusterName":"ray-train-raycluster"}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"head","namespace":"ray","labels":{"ray.io/cluster":"ray-train-raycluster","ray.io/node-type":"head"}},"spec":{"nodeName":"n1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}
		]}`),
		services: []byte(`{"items":[
			{"metadata":{"name":"ray-train-raycluster-head-svc","namespace":"ray","labels":{"ray.io/cluster":"ray-train-raycluster","ray.io/node-type":"head"}},"spec":{"type":"ClusterIP"}}
		]}`),
	}
	q := fakeQuerier{rows: []kustoquery.Row{{
		"metric_name": "tau/run_status",
		"value":       1.0,
		"wall_time":   "2026-07-02T10:05:00Z",
		"project_id":  "portal-e2e",
		"tags":        `{"tau.status.reason":"Succeeded"}`,
	}}}
	snap, err := Detail(context.Background(), r, q, Options{Namespace: "ray", Name: "ray-train"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Links.RayDashboardPath != "/api/portal/ray/proxy/ray/ray-train-raycluster/" {
		t.Fatalf("RayDashboardPath = %q, want the head-svc proxy path", snap.Links.RayDashboardPath)
	}
	if !snap.Links.RayDashboardReachable {
		t.Fatal("RayDashboardReachable = false, want true when the head Service is discoverable")
	}
	if snap.Links.StellarPath == "" {
		t.Fatal("StellarPath empty, want the Stellar deep-link preserved alongside the Ray link")
	}
}

// TestDetailKustoErrorDropsTier3 confirms a Kusto error (including
// ErrNoQueryCommand) omits tier 3 without failing the page.
func TestDetailKustoErrorDropsTier3(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"j","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"j","` + workloadmeta.LabelRunID + `":"run-1"}},"status":{"active":1}}`),
	}
	q := fakeQuerier{err: kustoquery.ErrNoQueryCommand}
	snap, err := Detail(context.Background(), r, q, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Lifecycle != nil {
		t.Fatalf("Lifecycle = %+v, want nil on Kusto error", snap.Lifecycle)
	}
	if snap.Links.StellarPath != "" {
		t.Fatalf("StellarPath = %q, want empty when Kusto has no durable row", snap.Links.StellarPath)
	}
}

// TestDetailEventTimeFallbacks covers Fix #1: a modern core/v1 Event that leaves
// the legacy lastTimestamp empty still resolves Last from eventTime or
// series.lastObservedTime, and an event with no timestamp at all leaves Last nil
// (omitted) rather than serializing the false 0001-01-01 zero time.
func TestDetailEventTimeFallbacks(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"train-a","namespace":"ray","creationTimestamp":"2026-07-02T10:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"train-a"}},"status":{"active":1}}`),
		events: []byte(`{"items":[
			{"type":"Warning","reason":"OnlyEventTime","message":"x","involvedObject":{"kind":"Job","name":"train-a"},"eventTime":"2026-07-02T10:05:00Z"},
			{"type":"Warning","reason":"OnlySeries","message":"y","involvedObject":{"kind":"Job","name":"train-a"},"series":{"lastObservedTime":"2026-07-02T10:06:00Z"}},
			{"type":"Normal","reason":"NoTimestamp","message":"z","involvedObject":{"kind":"Job","name":"train-a"}}
		]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-a"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if len(snap.Events) != 3 {
		t.Fatalf("Events = %+v, want 3", snap.Events)
	}
	byReason := map[string]EventDetail{}
	for _, e := range snap.Events {
		byReason[e.Reason] = e
	}
	if e := byReason["OnlyEventTime"]; e.Last == nil || e.Last.IsZero() {
		t.Fatalf("OnlyEventTime Last = %v, want parsed eventTime", e.Last)
	}
	if e := byReason["OnlySeries"]; e.Last == nil || e.Last.IsZero() {
		t.Fatalf("OnlySeries Last = %v, want parsed series.lastObservedTime", e.Last)
	}
	if e := byReason["NoTimestamp"]; e.Last != nil {
		t.Fatalf("NoTimestamp Last = %v, want nil (absence preserved, not 0001-01-01)", e.Last)
	}
}

// TestDetailAbsentCreatedIsNil covers Fix #1 for ObjectDetail.Created: an
// unparseable/absent creationTimestamp yields a nil Created (omitted) rather than
// the false 0001-01-01 zero time.
func TestDetailAbsentCreatedIsNil(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"train-a","namespace":"ray","creationTimestamp":"not-a-timestamp","labels":{"` + workloadmeta.LabelJob + `":"train-a"}},"status":{"active":1}}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-a"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Object.Created != nil {
		t.Fatalf("Object.Created = %v, want nil (unparseable creationTimestamp omitted)", snap.Object.Created)
	}
}

// TestDetailDecodeErrorNotNotFound covers Fix #2: a Get that returns
// (malformed payload, nil) alongside a NotFound on the other kind must surface a
// distinct decode error, not ErrNotFound — a successful-but-garbage response
// proves the API answered (→ 502), not that the object is absent (→ 404).
func TestDetailDecodeErrorNotNotFound(t *testing.T) {
	// Malformed RayJob success + NotFound Job.
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":`), // truncated JSON
		jobErr: apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "train-a"),
	}
	_, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-a"})
	if err == nil {
		t.Fatal("Detail() error = nil, want decode error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Detail() error = %v, want decode error, not ErrNotFound", err)
	}

	// Inverse: valid-JSON but missing metadata.name Job success + NotFound RayJob.
	r2 := fakeReader{
		rayErr: apierrors.NewNotFound(schema.GroupResource{Group: "ray.io", Resource: "rayjobs"}, "train-a"),
		job:    []byte(`{"metadata":{"namespace":"ray"},"status":{"active":1}}`),
	}
	_, err = Detail(context.Background(), r2, nil, Options{Namespace: "ray", Name: "train-a"})
	if err == nil {
		t.Fatal("Detail() error = nil, want decode error for name-less payload")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Detail() error = %v, want decode error, not ErrNotFound", err)
	}
}

// TestDetailToleratesBadListJSON confirms a per-source read that returns
// non-JSON (e.g. a CRD-not-found message) drops just that section.
func TestDetailToleratesBadListJSON(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"j","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"j"}},"status":{"active":1}}`),
		pods:   []byte(`the server doesn't have a resource type "pods"`),
		events: []byte(`nope`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Pods != nil || snap.Events != nil {
		t.Fatalf("Pods/Events = %+v/%+v, want nil on bad JSON", snap.Pods, snap.Events)
	}
}

// TestDetailSurfacesStampedStellarIdentity checks that the Stellar identity
// `tau run` stamps is surfaced on the snapshot, and — deliberately — that it
// does NOT leak into the deep-link scope. The row's project and the annotation's
// project are different knobs (offload sidecar --project vs the run config's
// experiment.project), so a row with no project must keep the unscoped link.
func TestDetailSurfacesStampedStellarIdentity(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job: []byte(`{"metadata":{"name":"j","namespace":"ray",
          "labels":{"` + workloadmeta.LabelRunID + `":"run-1"},
          "annotations":{
            "` + workloadmeta.AnnotationStellarProject + `":"NanoGPT FineWeb",
            "` + workloadmeta.AnnotationStellarExperimentID + `":"nanogpt-api-surface",
            "` + workloadmeta.AnnotationStellarExperimentTitle + `":"NanoGPT API surface",
            "` + workloadmeta.AnnotationStellarGroup + `":"safe-stack-h200"}},
        "status":{"conditions":[{"type":"Complete","status":"True"}]}}`),
	}
	// A lifecycle row with no project_id: the marker proves durability, the
	// annotation supplies the scope.
	q := fakeQuerier{rows: []kustoquery.Row{{
		"metric_name": "tau/run_status",
		"value":       1.0,
		"wall_time":   "2026-07-02T10:05:00Z",
	}}}
	snap, err := Detail(context.Background(), r, q, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Experiment == nil {
		t.Fatal("Experiment = nil, want the stamped Stellar identity")
	}
	if snap.Experiment.Project != "NanoGPT FineWeb" ||
		snap.Experiment.ExperimentID != "nanogpt-api-surface" ||
		snap.Experiment.Title != "NanoGPT API surface" ||
		snap.Experiment.Group != "safe-stack-h200" {
		t.Fatalf("Experiment = %+v, want the exact annotation values", *snap.Experiment)
	}
	// The annotation must not become the link scope: the rows behind this run
	// carry no project, so filtering them by one would match nothing.
	if snap.Links.StellarPath != "/stellar?target=run-1" {
		t.Fatalf("StellarPath = %q, want an unscoped link when the row has no project", snap.Links.StellarPath)
	}
}

// TestDetailKustoProjectIsTheOnlyLinkScope keeps the rule explicit: the link is
// scoped by what Stellar indexed (the row), never by the object annotation.
func TestDetailKustoProjectIsTheOnlyLinkScope(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job: []byte(`{"metadata":{"name":"j","namespace":"ray",
          "labels":{"` + workloadmeta.LabelRunID + `":"run-1"},
          "annotations":{"` + workloadmeta.AnnotationStellarProject + `":"different-project"}},
        "status":{"conditions":[{"type":"Complete","status":"True"}]}}`),
	}
	q := fakeQuerier{rows: []kustoquery.Row{{
		"metric_name": "tau/run_status",
		"value":       1.0,
		"wall_time":   "2026-07-02T10:05:00Z",
		"project_id":  "portal-e2e",
	}}}
	snap, err := Detail(context.Background(), r, q, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Links.StellarPath != "/stellar?target=run-1&project=portal-e2e" {
		t.Fatalf("StellarPath = %q, want the Kusto row project as the only scope", snap.Links.StellarPath)
	}
}

// TestDetailNoStellarIdentityOmitsExperiment guards the unstamped case: no
// annotations means no Experiment block, and the Kusto gate still governs the
// link.
func TestDetailNoStellarIdentityOmitsExperiment(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job:    []byte(`{"metadata":{"name":"j","namespace":"ray","labels":{"` + workloadmeta.LabelRunID + `":"run-1"}},"status":{}}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "j"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Experiment != nil {
		t.Fatalf("Experiment = %+v, want nil for an unstamped object", *snap.Experiment)
	}
	if snap.Links.StellarPath != "" {
		t.Fatalf("StellarPath = %q, want empty without a durable lifecycle row", snap.Links.StellarPath)
	}
}
