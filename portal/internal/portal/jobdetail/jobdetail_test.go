// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobdetail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type fakeReader struct {
	job, rayJob, rayCluster, pods, events, workloads, services   []byte
	jobErr, rayErr, rayClusterErr, podErr, evtErr, wlErr, svcErr error
}

func (f fakeReader) GetJob(context.Context, string, string) ([]byte, error) {
	return f.job, f.jobErr
}
func (f fakeReader) GetRayJob(context.Context, string, string) ([]byte, error) {
	return f.rayJob, f.rayErr
}
func (f fakeReader) GetRayCluster(context.Context, string, string) ([]byte, error) {
	return f.rayCluster, f.rayClusterErr
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

type fakeQuerier struct {
	rows []kustoquery.Row
	err  error
}

func (f fakeQuerier) Query(context.Context, string) ([]kustoquery.Row, error) {
	return f.rows, f.err
}

func TestWorkloadTelemetryNonFiniteMetricsKeepDetailSerializable(t *testing.T) {
	started := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Namespace: "team-a",
		Name:      "train",
		Kind:      "Job",
		Object:    ObjectDetail{Created: &started},
		Pods:      []PodDetail{{Name: "trainer-0", Node: "gpu-a", StartedAt: &started}},
	}
	telemetry, diagnostic := workloadTelemetry(context.Background(), fakeQuerier{rows: []kustoquery.Row{{
		"instance": "gpu-a", "pod": "trainer-0", "gpu": "0", "modelName": "H100",
		"samples": "8", "utilizationSamples": "0", "averageUtilizationPct": "NaN",
		"peakUtilizationPct": "+Inf", "maxTemperatureCelsius": "70",
	}}}, snap, Options{Cluster: "cluster-a", Namespace: "team-a"})
	if diagnostic.State != "ready" || telemetry == nil || telemetry.Coverage != "partial" {
		t.Fatalf("telemetry = %+v, diagnostic = %+v", telemetry, diagnostic)
	}
	snap.Telemetry = telemetry
	snap.Diagnostics.Telemetry = diagnostic
	if _, err := json.Marshal(snap); err != nil {
		t.Fatalf("marshal complete workload detail: %v", err)
	}
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

func TestDetailJobPodsUseCurrentMetadataContract(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job: []byte(`{"metadata":{"name":"train-current","namespace":"ray","labels":{
			"` + workloadmeta.LabelManagedBy + `":"tau",
			"` + workloadmeta.LabelRunID + `":"train-current",
			"` + workloadmeta.LabelWorkloadKind + `":"job",
			"` + workloadmeta.LabelWorkspace + `":"research"
		}},"status":{"active":1}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"standard","labels":{"job-name":"train-current","batch.kubernetes.io/job-name":"train-current","` + workloadmeta.LabelRunID + `":"train-current"}},"status":{"phase":"Running"}},
			{"metadata":{"name":"qualified","labels":{"batch.kubernetes.io/job-name":"train-current"}},"status":{"phase":"Pending"}},
			{"metadata":{"name":"run-id-only","labels":{"` + workloadmeta.LabelRunID + `":"train-current"}},"status":{"phase":"Running"}},
			{"metadata":{"name":"legacy","labels":{"` + workloadmeta.LabelJob + `":"train-current"}},"status":{"phase":"Succeeded"}},
			{"metadata":{"name":"other","labels":{"job-name":"other","` + workloadmeta.LabelRunID + `":"other"}},"status":{"phase":"Running"}}
		]}`),
	}

	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-current"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if got := len(snap.Pods); got != 4 {
		t.Fatalf("len(Pods) = %d, want 4 compatible pods without duplicates: %+v", got, snap.Pods)
	}
	want := []string{"standard", "qualified", "run-id-only", "legacy"}
	for i, name := range want {
		if snap.Pods[i].Name != name {
			t.Fatalf("Pods[%d].Name = %q, want %q; pods=%+v", i, snap.Pods[i].Name, name, snap.Pods)
		}
	}
}

func TestDetailJobUsesUIDToRejectStaleIncarnationMetadata(t *testing.T) {
	r := fakeReader{
		rayErr: errors.New("no rayjob"),
		job: []byte(`{"metadata":{"name":"train","namespace":"ray","uid":"job-new",
			"labels":{"batch.kubernetes.io/job-name":"train","` + workloadmeta.LabelRunID + `":"run-shared"}},"status":{"active":1}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"train-new","uid":"pod-new","labels":{"batch.kubernetes.io/job-name":"train"},"ownerReferences":[{"uid":"job-new","controller":true}]},"status":{"phase":"Running"}},
			{"metadata":{"name":"train-old","uid":"pod-old","labels":{"batch.kubernetes.io/job-name":"train"},"ownerReferences":[{"uid":"job-old","controller":true}]},"status":{"phase":"Failed"}}
		]}`),
		workloads: []byte(`{"items":[
			{"metadata":{"name":"job-train-new","ownerReferences":[{"name":"train","uid":"job-new","controller":true}]}},
			{"metadata":{"name":"job-train-observer","ownerReferences":[{"name":"train","uid":"job-new","controller":false}]}},
			{"metadata":{"name":"job-train-old","ownerReferences":[{"name":"train","uid":"job-old","controller":true}]}}
		]}`),
		events: []byte(`{"items":[
			{"reason":"CurrentJob","involvedObject":{"kind":"Job","name":"train","uid":"job-new"}},
			{"reason":"CurrentPod","involvedObject":{"kind":"Pod","name":"train-new","uid":"pod-new"}},
			{"reason":"OldJob","involvedObject":{"kind":"Job","name":"train","uid":"job-old"}},
			{"reason":"OldPod","involvedObject":{"kind":"Pod","name":"train-old","uid":"pod-old"}},
			{"reason":"WrongKind","involvedObject":{"kind":"Job","name":"train-new","uid":"pod-new"}},
			{"reason":"Sibling","involvedObject":{"kind":"Pod","name":"train-big-abc","uid":"pod-sibling"}}
		]}`),
	}

	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if len(snap.Pods) != 1 || snap.Pods[0].Name != "train-new" {
		t.Fatalf("Pods = %+v, want only the current Job incarnation", snap.Pods)
	}
	if len(snap.Workloads) != 1 || snap.Workloads[0].Name != "job-train-new" {
		t.Fatalf("Workloads = %+v, want only the current Job incarnation", snap.Workloads)
	}
	if len(snap.Events) != 2 {
		t.Fatalf("Events = %+v, want current Job and Pod events only", snap.Events)
	}
}

func TestDetailRayJobTraversesRayClusterUIDForPods(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-train","namespace":"ray","uid":"rayjob-new"},
			"status":{"jobDeploymentStatus":"Running","rayClusterName":"ray-cluster"}}`),
		rayCluster: []byte(`{"metadata":{"name":"ray-cluster","uid":"cluster-new",
			"ownerReferences":[{"uid":"rayjob-new","controller":true}]}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"head-new","uid":"head-new","labels":{"ray.io/cluster":"ray-cluster"},"ownerReferences":[{"uid":"cluster-new","controller":true}]},"status":{"phase":"Running"}},
			{"metadata":{"name":"head-old","uid":"head-old","labels":{"ray.io/cluster":"ray-cluster"},"ownerReferences":[{"uid":"cluster-old","controller":true}]},"status":{"phase":"Running"}}
		]}`),
		events: []byte(`{"items":[
			{"reason":"RayJob","involvedObject":{"kind":"RayJob","name":"ray-train","uid":"rayjob-new"}},
			{"reason":"RayCluster","involvedObject":{"kind":"RayCluster","name":"ray-cluster","uid":"cluster-new"}},
			{"reason":"HeadPod","involvedObject":{"kind":"Pod","name":"head-new","uid":"head-new"}},
			{"reason":"OldHeadPod","involvedObject":{"kind":"Pod","name":"head-old","uid":"head-old"}}
		]}`),
	}

	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "ray-train"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if len(snap.Pods) != 1 || snap.Pods[0].Name != "head-new" {
		t.Fatalf("Pods = %+v, want only Pods controlled by the current RayCluster", snap.Pods)
	}
	if len(snap.Events) != 3 {
		t.Fatalf("Events = %+v, want current RayJob, RayCluster, and Pod events", snap.Events)
	}
}

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
	if snap.Diagnostics.Pods.State != "unavailable" {
		t.Fatalf("Pods diagnostic = %+v, want unavailable", snap.Diagnostics.Pods)
	}
	if !strings.Contains(snap.ResourceRelease.Message, "cannot be confirmed") {
		t.Fatalf("ResourceRelease.Message = %q", snap.ResourceRelease.Message)
	}
}

func TestDetailRayJobDoesNotClaimComputeReusableWhenRayClusterOwnershipIsUnresolved(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-complete","namespace":"tau","uid":"rayjob-current"},
			"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED","rayClusterName":"ray-complete-cluster"}}`),
		rayClusterErr: errors.New("raycluster forbidden"),
		pods:          []byte(`{"items":[]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "tau", Name: "ray-complete"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.ResourceRelease == nil || snap.ResourceRelease.ComputeState != "unknown" {
		t.Fatalf("ResourceRelease = %+v, want unknown compute state", snap.ResourceRelease)
	}
	if snap.Diagnostics.Pods.State != "unavailable" {
		t.Fatalf("Pods diagnostic = %+v, want unavailable", snap.Diagnostics.Pods)
	}
	if snap.Links.RayDashboardPath != "" || snap.Links.RayDashboardReachable {
		t.Fatalf("Ray dashboard link = %+v, want hidden without validated RayCluster ownership", snap.Links)
	}
}

func TestDetailEventsUnavailableWhenUIDOwnershipCannotBeCompleted(t *testing.T) {
	tests := []struct {
		name        string
		requestName string
		reader      fakeReader
	}{
		{
			name:        "job pods unreadable",
			requestName: "train",
			reader: fakeReader{
				rayErr: apierrors.NewNotFound(schema.GroupResource{Group: "ray.io", Resource: "rayjobs"}, "train"),
				job: []byte(`{"metadata":{"name":"train","namespace":"tau","uid":"job-current",
					"labels":{"batch.kubernetes.io/job-name":"train"}},"status":{"active":1}}`),
				podErr: errors.New("pods forbidden"),
				events: []byte(`{"items":[
					{"reason":"JobEvent","involvedObject":{"kind":"Job","name":"train","uid":"job-current"}},
					{"reason":"PodEvent","involvedObject":{"kind":"Pod","name":"train-current","uid":"pod-current"}}
				]}`),
			},
		},
		{
			name:        "raycluster ownership unreadable",
			requestName: "ray-train",
			reader: fakeReader{
				rayJob: []byte(`{"metadata":{"name":"ray-train","namespace":"tau","uid":"rayjob-current"},
					"status":{"jobDeploymentStatus":"Running","rayClusterName":"ray-train-cluster"}}`),
				rayClusterErr: errors.New("raycluster forbidden"),
				events: []byte(`{"items":[
					{"reason":"RayJobEvent","involvedObject":{"kind":"RayJob","name":"ray-train","uid":"rayjob-current"}}
				]}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := Detail(context.Background(), tt.reader, nil, Options{Namespace: "tau", Name: tt.requestName})
			if err != nil {
				t.Fatalf("Detail() error = %v", err)
			}
			if snap.Events != nil {
				t.Fatalf("Events = %+v, want nil when the UID ownership set is incomplete", snap.Events)
			}
			if snap.Diagnostics.Events.State != "unavailable" {
				t.Fatalf("Events diagnostic = %+v, want unavailable", snap.Diagnostics.Events)
			}
		})
	}
}

func TestDetailRayJobDoesNotClaimComputeReusableWithoutRayClusterIdentity(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-complete","namespace":"tau","uid":"rayjob-current"},
			"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED"}}`),
		pods: []byte(`{"items":[]}`),
	}
	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "tau", Name: "ray-complete"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.ResourceRelease == nil || snap.ResourceRelease.ComputeState != "unknown" {
		t.Fatalf("ResourceRelease = %+v, want unknown compute state", snap.ResourceRelease)
	}
	if snap.Diagnostics.Pods.State != "unavailable" {
		t.Fatalf("Pods diagnostic = %+v, want unavailable", snap.Diagnostics.Pods)
	}
}

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

func TestDetailRayDashboardIsUnavailableWithoutReadyHeadPod(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-train","namespace":"ray"},
			"status":{"jobDeploymentStatus":"Running","rayClusterName":"ray-train-raycluster"}}`),
		pods: []byte(`{"items":[
			{"metadata":{"name":"head","namespace":"ray","labels":{"ray.io/cluster":"ray-train-raycluster","ray.io/node-type":"head"}},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}}
		]}`),
		services: []byte(`{"items":[
			{"metadata":{"name":"ray-train-raycluster-head-svc","namespace":"ray","labels":{"ray.io/cluster":"ray-train-raycluster","ray.io/node-type":"head"}},"spec":{"type":"ClusterIP"}}
		]}`),
	}

	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "ray-train"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Links.RayDashboardPath == "" {
		t.Fatal("RayDashboardPath empty, want the known dashboard path")
	}
	if snap.Links.RayDashboardReachable {
		t.Fatal("RayDashboardReachable = true, want false without a ready head pod")
	}
}

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

func TestDetailDecodeErrorNotNotFound(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":`),
		jobErr: apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "train-a"),
	}
	_, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "train-a"})
	if err == nil {
		t.Fatal("Detail() error = nil, want decode error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Detail() error = %v, want decode error, not ErrNotFound", err)
	}

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
	if snap.Diagnostics.Pods.State != "unavailable" || snap.Diagnostics.Events.State != "unavailable" {
		t.Fatalf("diagnostics = %+v, want malformed sources unavailable", snap.Diagnostics)
	}
}

func TestDetailRejectsMalformedListEnvelopes(t *testing.T) {
	r := fakeReader{
		rayJob: []byte(`{"metadata":{"name":"ray-complete","namespace":"ray","uid":"rayjob-current"},
			"status":{"jobDeploymentStatus":"Complete","jobStatus":"SUCCEEDED","rayClusterName":"ray-current"}}`),
		rayCluster: []byte(`{"metadata":{"name":"ray-current","uid":"cluster-current",
			"ownerReferences":[{"uid":"rayjob-current","controller":true}]}}`),
		workloads: []byte(`{}`),
		pods:      []byte(`{}`),
		events:    []byte(`{}`),
	}

	snap, err := Detail(context.Background(), r, nil, Options{Namespace: "ray", Name: "ray-complete"})
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if snap.Diagnostics.Workloads.State != "unavailable" ||
		snap.Diagnostics.Pods.State != "unavailable" ||
		snap.Diagnostics.Events.State != "unavailable" {
		t.Fatalf("diagnostics = %+v, want malformed list envelopes unavailable", snap.Diagnostics)
	}
	if snap.ResourceRelease == nil || snap.ResourceRelease.ComputeState != "unknown" {
		t.Fatalf("ResourceRelease = %+v, want unknown compute state", snap.ResourceRelease)
	}
}

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
	if snap.Links.StellarPath != "" {
		t.Fatalf("StellarPath = %q, want no link with unresolved project", snap.Links.StellarPath)
	}
}

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
