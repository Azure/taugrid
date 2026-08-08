// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package autocapture

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestReconcileWritesRunContextEventsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()
	created := mustTime("2026-05-19T10:00:00Z")
	started := mustTime("2026-05-19T10:05:00Z")
	finished := mustTime("2026-05-19T10:45:00Z")
	client := &fakeClient{
		jobs: []Job{{
			Namespace: "tau",
			Name:      "train-001",
			UID:       "job-uid",
			Labels: map[string]string{
				experiment.LabelRunID:        "train-001",
				experiment.LabelWorkloadKind: "job",
				workloadmeta.LabelTeam:       "research",
				workloadmeta.LabelProfile:    "research-train-gpu",
				workloadmeta.LabelLane:       "training",
				workloadmeta.LabelGPUClass:   "h100",
				"kueue.x-k8s.io/queue-name":  "training-queue",
			},
			Annotations: map[string]string{
				experiment.AnnotationNamespace:     "tau",
				experiment.AnnotationTauCommand:    "tau submit train-001",
				experiment.AnnotationConfigHash:    "config123",
				experiment.AnnotationCodeSHA:       "abc123",
				experiment.AnnotationGPUCount:      "8",
				experiment.AnnotationStorageMounts: `[{"source":"pvc","path":"/data"}]`,
			},
			CreatedAt:  created,
			StartedAt:  started,
			FinishedAt: finished,
			Succeeded:  1,
			Conditions: []Condition{{Type: "Complete", Status: "True", LastTransitionTime: finished}},
		}},
		workloads: []Workload{{
			Namespace:    "tau",
			Name:         "train-001-wl",
			UID:          "wl-uid",
			Labels:       map[string]string{experiment.LabelRunID: "train-001", "kueue.x-k8s.io/job-uid": "job-uid"},
			Queue:        "training-queue",
			ClusterQueue: "research-gpu",
			Admitted:     true,
			Phase:        "Finished",
			AdmittedAt:   started,
			FinishedAt:   finished,
		}},
		pods: []Pod{{
			Namespace:      "tau",
			Name:           "train-001-pod",
			UID:            "pod-uid",
			Labels:         map[string]string{experiment.LabelRunID: "train-001", "job-name": "train-001"},
			Phase:          "Succeeded",
			Node:           "node-a",
			Ready:          "0/1",
			StartedAt:      started.Add(30 * time.Second),
			ResourceClaims: []string{"claim-a"},
		}},
		claims: []ResourceClaim{{
			Namespace:   "tau",
			Name:        "claim-a",
			UID:         "claim-uid",
			Labels:      map[string]string{experiment.LabelRunID: "train-001"},
			NodeName:    "node-a",
			DeviceClass: "gpu.nvidia.com",
		}},
		events: []KubernetesEvent{{
			Namespace: "tau",
			Name:      "train-001-scheduled",
			UID:       "event-uid",
			Type:      "Normal",
			Reason:    "Scheduled",
			Message:   "Successfully assigned tau/train-001-pod to node-a",
			Source:    "default-scheduler",
			Count:     1,
			Time:      started.Add(20 * time.Second),
			Regarding: ObjectRef{Kind: "Pod", Namespace: "tau", Name: "train-001-pod", UID: "pod-uid"},
		}},
	}
	reconciler := Reconciler{Client: client}
	result, err := reconciler.Reconcile(ctx, store, Options{
		Namespace:  "tau",
		Cluster:    "kind-taugrid",
		Project:    "sample-project",
		RunGroupID: "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Runs != 1 || result.CreatedRuns != 1 || result.CreatedRunContexts != 1 || result.Events == 0 {
		t.Fatalf("unexpected reconcile result: %+v", result)
	}
	rows, err := store.Query(ctx, "select state, started_at, completed_at from runs where run_id = 'train-001'")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Rows[0]["state"] != "succeeded" || rows.Rows[0]["started_at"] != started.Format(time.RFC3339) || rows.Rows[0]["completed_at"] != finished.Format(time.RFC3339) {
		t.Fatalf("unexpected run row: %+v", rows.Rows)
	}
	contextRows, err := store.Query(ctx, "select cluster, namespace, local_queue, cluster_queue, pod_uid, resource_claims, node_names, queue_wait_seconds from run_context where run_id = 'train-001'")
	if err != nil {
		t.Fatal(err)
	}
	row := contextRows.Rows[0]
	if row["cluster"] != "kind-taugrid" || row["namespace"] != "tau" || row["local_queue"] != "training-queue" || row["cluster_queue"] != "research-gpu" {
		t.Fatalf("queue context not captured: %+v", row)
	}
	if row["pod_uid"] != "pod-uid" || row["resource_claims"] != "claim-a" || row["node_names"] != "node-a" || fmt.Sprint(row["queue_wait_seconds"]) != "300" {
		t.Fatalf("pod/resource context not captured: %+v", row)
	}
	eventCount := countRows(t, store, "select count(*) as count from events where run_id = 'train-001'")
	if eventCount < 5 {
		t.Fatalf("expected selected lifecycle and Kubernetes events, got %d", eventCount)
	}
	again, err := reconciler.Reconcile(ctx, store, Options{
		Namespace:  "tau",
		Cluster:    "kind-taugrid",
		Project:    "sample-project",
		RunGroupID: "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Runs != 1 || again.Reused != 1 || again.Events != 0 {
		t.Fatalf("repeated reconcile should be idempotent, got %+v", again)
	}
	if got := countRows(t, store, "select count(*) as count from events where run_id = 'train-001'"); got != eventCount {
		t.Fatalf("event count changed after idempotent reconcile: got %d want %d", got, eventCount)
	}
}

func TestReconcileUpdatesRunningAndFailedTransitions(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()
	created := mustTime("2026-05-19T11:00:00Z")
	started := mustTime("2026-05-19T11:02:00Z")
	finished := mustTime("2026-05-19T11:10:00Z")
	client := &fakeClient{jobs: []Job{{
		Namespace:  "tau",
		Name:       "train-fail",
		UID:        "job-fail",
		Labels:     map[string]string{experiment.LabelRunID: "train-fail"},
		CreatedAt:  created,
		StartedAt:  started,
		Active:     1,
		Conditions: nil,
	}}}
	reconciler := Reconciler{Client: client}
	if _, err := reconciler.Reconcile(ctx, store, Options{Namespace: "tau", Project: "sample-project", RunGroupID: "reference-group"}); err != nil {
		t.Fatal(err)
	}
	if state := runState(t, store, "train-fail"); state != "running" {
		t.Fatalf("state=%s, want running", state)
	}
	client.jobs[0].Active = 0
	client.jobs[0].Failed = 1
	client.jobs[0].FinishedAt = finished
	client.jobs[0].Conditions = []Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", LastTransitionTime: finished}}
	if _, err := reconciler.Reconcile(ctx, store, Options{Namespace: "tau", Project: "sample-project", RunGroupID: "reference-group"}); err != nil {
		t.Fatal(err)
	}
	if state := runState(t, store, "train-fail"); state != "failed" {
		t.Fatalf("state=%s, want failed", state)
	}
	if got := countRows(t, store, "select count(*) as count from events where run_id = 'train-fail' and type = 'failed'"); got != 1 {
		t.Fatalf("failed event count=%d, want 1", got)
	}
}

func TestReconcileCapturesRayJobWithTauGridJobLabel(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()
	created := mustTime("2026-05-21T17:00:00Z")
	started := mustTime("2026-05-21T17:03:00Z")
	client := &fakeClient{
		jobs: []Job{{
			Namespace: "tau",
			Name:      "tau-sample-rayjob",
			UID:       "rayjob-uid",
			Labels: map[string]string{
				workloadmeta.LabelJob:                     "sample-rayjob",
				experiment.LabelWorkloadKind:              "sample-training",
				workloadmeta.LabelTeam:                    "sample-team",
				workloadmeta.LabelGPUClass:                "gpu-large",
				"kueue.x-k8s.io/queue-name":               "sample-queue",
				workloadmeta.AnnotationTopologyGPUFamily:  "nvidia-gpu",
				workloadmeta.AnnotationTopologyGPUProfile: "large-memory",
			},
			CreatedAt: created,
			StartedAt: started,
			Active:    1,
		}},
		workloads: []Workload{{
			Namespace:    "tau",
			Name:         "tau-sample-rayjob-wl",
			UID:          "wl-rayjob",
			Labels:       map[string]string{labelKueueJobUID: "rayjob-uid"},
			Queue:        "sample-queue",
			ClusterQueue: "sample-cluster-queue",
			Admitted:     true,
			Phase:        "Admitted",
			AdmittedAt:   started,
		}},
		pods: []Pod{{
			Namespace:      "tau",
			Name:           "tau-sample-rayjob-head",
			UID:            "ray-pod-uid",
			Labels:         map[string]string{workloadmeta.LabelJob: "sample-rayjob"},
			Phase:          "Running",
			Node:           "gpu-node-0",
			Ready:          "1/1",
			StartedAt:      started.Add(30 * time.Second),
			ResourceClaims: []string{"claim-gpu"},
		}},
		claims: []ResourceClaim{{
			Namespace:   "tau",
			Name:        "claim-gpu",
			UID:         "claim-gpu-uid",
			Labels:      map[string]string{workloadmeta.LabelJob: "sample-rayjob"},
			NodeName:    "gpu-node-0",
			DeviceClass: "gpu.nvidia.com",
		}},
		events: []KubernetesEvent{{
			Namespace: "tau",
			Name:      "rayjob-running",
			UID:       "event-rayjob",
			Type:      "Normal",
			Reason:    "Running",
			Message:   "RayJob is running",
			Source:    "kuberay",
			Count:     1,
			Time:      started,
			Regarding: ObjectRef{Kind: "RayJob", Namespace: "tau", Name: "tau-sample-rayjob", UID: "rayjob-uid"},
		}},
	}
	reconciler := Reconciler{Client: client}
	result, err := reconciler.Reconcile(ctx, store, Options{
		Namespace:  "tau",
		Cluster:    "kind-taugrid",
		Project:    "sample-project",
		RunGroupID: "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Runs != 1 || result.CreatedRuns != 1 || result.CreatedRunContexts != 1 || result.Events == 0 {
		t.Fatalf("unexpected RayJob reconcile result: %+v", result)
	}
	if state := runState(t, store, "sample-rayjob"); state != "running" {
		t.Fatalf("state=%s, want running", state)
	}
	if got := countRows(t, store, "select count(*) as count from runs where run_id = 'tau-sample-rayjob'"); got != 0 {
		t.Fatalf("RayJob resource name should not become run_id, found %d rows", got)
	}
	contextRows, err := store.Query(ctx, "select cluster, namespace, local_queue, cluster_queue, pod_uid, resource_claims, node_names from run_context where run_id = 'sample-rayjob'")
	if err != nil {
		t.Fatal(err)
	}
	row := contextRows.Rows[0]
	if row["cluster"] != "kind-taugrid" || row["namespace"] != "tau" || row["local_queue"] != "sample-queue" || row["cluster_queue"] != "sample-cluster-queue" {
		t.Fatalf("RayJob queue context not captured: %+v", row)
	}
	if row["pod_uid"] != "ray-pod-uid" || row["resource_claims"] != "claim-gpu" || row["node_names"] != "gpu-node-0" {
		t.Fatalf("RayJob pod/resource context not captured: %+v", row)
	}
}

func TestRayJobItemToJobSynthesizesRunIDAndLifecycle(t *testing.T) {
	started := mustTime("2026-05-21T17:03:00Z")
	item := rayJobItem{
		Metadata: objectMeta{
			Namespace:         "tau",
			Name:              "tau-sample-rayjob",
			UID:               "rayjob-uid",
			Labels:            map[string]string{workloadmeta.LabelJob: "sample-rayjob"},
			CreationTimestamp: mustTime("2026-05-21T17:00:00Z"),
		},
	}
	item.Status.JobStatus = "RUNNING"
	item.Status.StartTime = started

	job := rayJobItemToJob(item)
	if got := runIDForJob(job); got != "sample-rayjob" {
		t.Fatalf("runIDForJob()=%q, want sample-rayjob", got)
	}
	if job.Active != 1 || job.StartedAt != started {
		t.Fatalf("RayJob lifecycle not converted: %+v", job)
	}
	if job.Labels[experiment.LabelRunID] != "sample-rayjob" {
		t.Fatalf("synthetic run-id label missing: %+v", job.Labels)
	}
}

func newTestStore(t *testing.T) *expstore.Store {
	t.Helper()
	store, _, err := expstore.Init(context.Background(), filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "sample-experiment",
		Project:     "sample-project",
		Description: "Compare baseline and candidate training runs.",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func runState(t *testing.T, store *expstore.Store, runID string) string {
	t.Helper()
	rows, err := store.Query(context.Background(), fmt.Sprintf("select state from runs where run_id = '%s'", runID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("run %s not found: %+v", runID, rows.Rows)
	}
	return fmt.Sprint(rows.Rows[0]["state"])
}

func countRows(t *testing.T, store *expstore.Store, query string) int {
	t.Helper()
	rows, err := store.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("count query returned %+v", rows.Rows)
	}
	var got int
	if _, err := fmt.Sscan(fmt.Sprint(rows.Rows[0]["count"]), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func mustTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

type fakeClient struct {
	jobs      []Job
	workloads []Workload
	pods      []Pod
	claims    []ResourceClaim
	events    []KubernetesEvent
}

func (f *fakeClient) ListJobs(context.Context, string) ([]Job, error) {
	return append([]Job(nil), f.jobs...), nil
}

func (f *fakeClient) ListWorkloads(context.Context, string) ([]Workload, error) {
	return append([]Workload(nil), f.workloads...), nil
}

func (f *fakeClient) ListPods(context.Context, string) ([]Pod, error) {
	return append([]Pod(nil), f.pods...), nil
}

func (f *fakeClient) ListResourceClaims(context.Context, string) ([]ResourceClaim, error) {
	return append([]ResourceClaim(nil), f.claims...), nil
}

func (f *fakeClient) ListEvents(context.Context, string) ([]KubernetesEvent, error) {
	return append([]KubernetesEvent(nil), f.events...), nil
}
