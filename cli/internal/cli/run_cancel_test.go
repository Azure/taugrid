// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/status"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestFetchManagerCleanupSnapshot_SurfacesTimeoutAndPreservesSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	snap, err := fetchManagerCleanupSnapshot(ctx, managerCleanupHooks{
		fetchSnapshot: func(ctx context.Context) (status.Snapshot, error) {
			<-ctx.Done()
			return status.Snapshot{
				Name:         "train-001",
				Namespace:    "ray",
				JobFound:     true,
				JobManagedBy: "kueue.x-k8s.io/multikueue",
			}, nil
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if !snap.JobFound || snap.JobManagedBy != "kueue.x-k8s.io/multikueue" {
		t.Fatalf("expected partial snapshot to be preserved, got %+v", snap)
	}
}

func TestFetchManagerCleanupStatusSnapshot_JobPathRemainsAuthoritative(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "job", "train-001", "-o", "json"): {
				{out: `{"metadata":{"uid":"job-uid"},"spec":{"managedBy":""}}`},
			},
			fakeRawKey("-n", "ray", "get", "rayjob", "train-001", "-o", "json"): {
				{err: errors.New(`Error from server (NotFound): rayjobs.ray.io "train-001" not found`)},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"): {
				{out: `{"items":[]}`},
			},
		},
	}

	snap, err := fetchManagerCleanupStatusSnapshot(context.Background(), runner, "ray", "train-001")
	if err != nil {
		t.Fatalf("fetchManagerCleanupStatusSnapshot() error = %v", err)
	}
	if !snap.JobFound || snap.JobUID != "job-uid" {
		t.Fatalf("expected authoritative Job snapshot, got %+v", snap)
	}
	if snap.RayJob.Found {
		t.Fatalf("expected RayJob to remain absent, got %+v", snap.RayJob)
	}
	if snap.IsMultiKueue() {
		t.Fatalf("expected single-cluster snapshot, got %+v", snap)
	}
}

func TestFetchManagerCleanupStatusSnapshot_RayCRDAbsentStillUsesJobState(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "job", "train-001", "-o", "json"): {
				{out: `{"metadata":{"uid":"job-uid"},"spec":{"managedBy":""}}`},
			},
			fakeRawKey("-n", "ray", "get", "rayjob", "train-001", "-o", "json"): {
				{err: errors.New(`the server doesn't have a resource type "rayjob"`)},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"): {
				{out: `{"items":[]}`},
			},
		},
	}

	snap, err := fetchManagerCleanupStatusSnapshot(context.Background(), runner, "ray", "train-001")
	if err != nil {
		t.Fatalf("fetchManagerCleanupStatusSnapshot() error = %v", err)
	}
	if !snap.JobFound || snap.JobUID != "job-uid" {
		t.Fatalf("expected Job snapshot despite missing Ray CRD, got %+v", snap)
	}
	if snap.RayJob.Found {
		t.Fatalf("expected RayJob to remain absent when CRD is missing, got %+v", snap.RayJob)
	}
}

func TestFetchManagerCleanupStatusSnapshot_TimeoutReturnsPartialSnapshot(t *testing.T) {
	runner := rawRunnerFunc(func(ctx context.Context, extraArgs []string, _ []byte) (string, error) {
		switch fakeRawKey(extraArgs...) {
		case fakeRawKey("-n", "ray", "get", "job", "train-001", "-o", "json"):
			return `{"metadata":{"uid":"job-uid"},"spec":{"managedBy":""}}`, nil
		case fakeRawKey("-n", "ray", "get", "rayjob", "train-001", "-o", "json"):
			return "", errors.New(`Error from server (NotFound): rayjobs.ray.io "train-001" not found`)
		case fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"):
			<-ctx.Done()
			return "", ctx.Err()
		case fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"):
			<-ctx.Done()
			return "", ctx.Err()
		default:
			t.Fatalf("unexpected kubectl args: %v", extraArgs)
			return "", nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	snap, err := fetchManagerCleanupStatusSnapshot(ctx, runner, "ray", "train-001")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if !snap.JobFound || snap.JobUID != "job-uid" {
		t.Fatalf("expected partial Job snapshot on timeout, got %+v", snap)
	}
}

func TestFetchManagerCleanupStatusSnapshot_CapturesKueueQueueMetadataWithoutWorkloads(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "job", "train-001", "-o", "json"): {
				{out: `{"metadata":{"uid":"job-uid","labels":{"kueue.x-k8s.io/queue-name":"jobqueue"},"annotations":{"` + workloadmeta.AnnotationTopologyQueue + `":"fallback-queue"}},"spec":{"managedBy":""}}`},
			},
			fakeRawKey("-n", "ray", "get", "rayjob", "train-001", "-o", "json"): {
				{err: errors.New(`Error from server (NotFound): rayjobs.ray.io "train-001" not found`)},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"): {
				{out: `{"items":[]}`},
			},
		},
	}

	snap, err := fetchManagerCleanupStatusSnapshot(context.Background(), runner, "ray", "train-001")
	if err != nil {
		t.Fatalf("fetchManagerCleanupStatusSnapshot() error = %v", err)
	}
	if got := snap.ManagerLocalQueue(); got != "jobqueue" {
		t.Fatalf("manager queue = %q, want jobqueue", got)
	}
	if !snap.IsKueueManaged() {
		t.Fatalf("expected queue metadata to mark the workload as Kueue-managed, got %+v", snap)
	}
}

func TestManagerCleanupUncertainTimeoutError_UsesKueueWordingForQueueManagedCleanup(t *testing.T) {
	err := managerCleanupUncertainTimeoutError(managerCleanupModeKueue, 30*time.Second, "ray", "train-001", []string{workloadmeta.LabelJob + "=train-001"}, nil, status.Snapshot{}, nil)
	if got := err.Error(); !strings.Contains(got, "waiting to prove Kueue workload cleanup") || strings.Contains(got, "MultiKueue") {
		t.Fatalf("expected Kueue wording without MultiKueue, got %q", got)
	}
}

func TestManagerCleanupSelectors_AlwaysIncludeStableSelectorAndDeduplicateUIDs(t *testing.T) {
	got := managerCleanupSelectors("train-001", status.Snapshot{
		JobUID: "job-uid",
		RayJob: status.RayJob{UID: "job-uid"},
	})
	want := []string{
		workloadmeta.LabelJob + "=train-001",
		"kueue.x-k8s.io/job-uid=job-uid",
	}
	if len(got) != len(want) {
		t.Fatalf("managerCleanupSelectors() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("managerCleanupSelectors()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCancelWorkload_StrictPrefetchWorkloadErrorDoesNotSkipCleanupProof(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "job", "train-001", "-o", "json"): {
				{out: `{"metadata":{"uid":"job-uid"},"spec":{"managedBy":""}}`},
			},
			fakeRawKey("-n", "ray", "get", "rayjob", "train-001", "-o", "json"): {
				{err: errors.New(`Error from server (NotFound): rayjobs.ray.io "train-001" not found`)},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{err: errors.New("forbidden: manager workload list blocked")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {
				{out: ""},
			},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"): {
				{out: `job.batch "train-001" deleted`},
			},
		},
	}
	waitCalls := 0
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "train-001", "ray", io.Discard, managerCleanupOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(ctx context.Context) (status.Snapshot, error) {
			return fetchManagerCleanupStatusSnapshot(ctx, runner, "ray", "train-001")
		},
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
		now: time.Now,
	})
	if err == nil || !strings.Contains(err.Error(), "forbidden: manager workload list blocked") {
		t.Fatalf("expected strict prefetch error to surface after delete, got %v", err)
	}
	if waitCalls != 0 {
		t.Fatalf("expected no poll wait once strict prefetch re-check fails, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray get job train-001 -o json",
		"-n ray get rayjob train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_MultiKueueWaitsForAllManagerWorkloadsToDisappear(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-b", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-b"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-b\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	var out strings.Builder
	fetchCalls := 0
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", &out, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			fetchCalls++
			if fetchCalls == 1 {
				return multiKueueCancelSnapshot("wl-a", "wl-b"), nil
			}
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, nil
		},
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name poll wait plus one selector absence proof wait, got %d", waitCalls)
	}
	if got := out.String(); !strings.Contains(got, "rayjob.ray.io") || !strings.Contains(got, "job.batch") {
		t.Fatalf("expected delete output for both resources, got %q", got)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-b -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-b -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestDeleteWorkloadAndWaitForManagerCleanup_StrictFetchUnionsSelectorsAndRediscoveryProvesAbsence(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "job", "train-001", "-o", "json"): {
				{out: `{"metadata":{"uid":"job-uid"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"}}`},
			},
			fakeRawKey("-n", "ray", "get", "rayjob", "train-001", "-o", "json"): {
				{out: `{"metadata":{"uid":"ray-uid","name":"train-001"},"spec":{"managedBy":"kueue.x-k8s.io/multikueue"}}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[{"metadata":{"name":"wl-a"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"): {
				{out: `{"items":[{"metadata":{"name":"wl-b"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=ray-uid", "-o", "json"): {
				{out: `{"items":[{"metadata":{"name":"wl-b"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-b", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-b"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-b\" not found")},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "train-001", "ray", io.Discard, managerCleanupOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(ctx context.Context) (status.Snapshot, error) {
			fetchCalls++
			if fetchCalls == 1 {
				return fetchManagerCleanupStatusSnapshot(ctx, runner, "ray", "train-001")
			}
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, nil
		},
		wait: func(context.Context, time.Duration) error {
			waitCalls++
			return nil
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("deleteWorkloadAndWaitForManagerCleanup() error = %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name poll wait plus one selector absence proof wait, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray get job train-001 -o json",
		"-n ray get rayjob train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json",
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-b -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-b -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=ray-uid -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
	for _, call := range runner.joinedCalls() {
		if strings.Contains(call, "worker-a") || strings.Contains(call, "pods") || strings.Contains(call, "events") {
			t.Fatalf("cancel should stay manager-side, got call %q", call)
		}
	}
}

func TestCancelWorkload_MultiKueueTimeoutIncludesPlacementState(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	nowValues := []time.Time{
		base,
		base,
		base,
		base,
		base.Add(31 * time.Second),
		base.Add(31 * time.Second),
	}
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):      {{out: `{"metadata":{"name":"wl-a"}}`}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-b", "-o", "json"):      {{out: `{"metadata":{"name":"wl-b"}}`}},
		},
	}
	fetchCount := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			fetchCount++
			snap := multiKueueCancelSnapshot("wl-a")
			snap.Workloads[0].Admitted = true
			snap.Workloads[0].Phase = "Admitted"
			snap.Workloads[0].AdmissionChecks = []status.AdmissionCheck{
				{
					Name:           "multikueue",
					State:          "Ready",
					Message:        "reservation acquired",
					ControllerName: "kueue.x-k8s.io/multikueue",
				},
				{
					Name:           "quota-check",
					State:          "Rejected",
					Message:        "generic quota rejected",
					ControllerName: "kueue.x-k8s.io/provisioning",
				},
			}
			return snap, nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now: func() time.Time {
			if len(nowValues) == 0 {
				return base.Add(time.Hour)
			}
			cur := nowValues[0]
			nowValues = nowValues[1:]
			return cur
		},
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if fetchCount < 1 {
		t.Fatalf("expected timeout path to use at least the cached snapshot, got %d fetches", fetchCount)
	}
	msg := err.Error()
	for _, want := range []string{
		"timed out after 30s",
		"wl-a",
		"state=Ready",
		"selected-worker=worker-a",
		"admission=wl-a/multikueue=Ready(reservation acquired)",
		"tau run status train-001 -n ray",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected timeout error to contain %q, got %q", want, msg)
		}
	}
}

func TestDeleteWorkloadAndWaitForManagerCleanup_ExactNameGetDeadlineReturnsRichTimeout(t *testing.T) {
	runner := rawRunnerFunc(func(ctx context.Context, extraArgs []string, _ []byte) (string, error) {
		switch fakeRawKey(extraArgs...) {
		case fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"):
			return "rayjob.ray.io \"train-001\" deleted\n", nil
		case fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):
			return "job.batch \"train-001\" deleted\n", nil
		case fakeRawKey("-n", "ray", "delete", "service", "train-001-headless", "--ignore-not-found"):
			return "", nil
		case fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):
			<-ctx.Done()
			return "", ctx.Err()
		default:
			t.Fatalf("unexpected kubectl args: %v", extraArgs)
			return "", nil
		}
	})
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "train-001", "ray", io.Discard, managerCleanupOptions{
		Timeout:  30 * time.Millisecond,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueCancelSnapshot("wl-a"), nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now:  time.Now,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	msg := err.Error()
	for _, want := range []string{
		"timed out after 30ms waiting for MultiKueue manager workload finalizer to remove wl-a",
		"state=Selected",
		"selected-worker=worker-a",
		"rerun `tau run status train-001 -n ray`",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected timeout error to contain %q, got %q", want, msg)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected rich timeout diagnostic instead of raw context deadline, got %v", err)
	}
}

func TestDeleteWorkloadAndWaitForManagerCleanup_ExactNameParentCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := rawRunnerFunc(func(getCtx context.Context, extraArgs []string, _ []byte) (string, error) {
		switch fakeRawKey(extraArgs...) {
		case fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"):
			return "rayjob.ray.io \"train-001\" deleted\n", nil
		case fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):
			return "job.batch \"train-001\" deleted\n", nil
		case fakeRawKey("-n", "ray", "delete", "service", "train-001-headless", "--ignore-not-found"):
			return "", nil
		case fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):
			cancel()
			<-getCtx.Done()
			return "", getCtx.Err()
		default:
			t.Fatalf("unexpected kubectl args: %v", extraArgs)
			return "", nil
		}
	})
	err := deleteWorkloadAndWaitForManagerCleanup(ctx, runner, "train-001", "ray", io.Discard, managerCleanupOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueCancelSnapshot("wl-a"), nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now:  time.Now,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected parent cancellation to propagate, got %v", err)
	}
}

func TestCancelWorkload_MultiKueueTimeoutOmitsConflictingWorkerDetail(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 30, 0, 0, time.UTC)
	nowValues := []time.Time{
		base,
		base,
		base,
		base,
		base.Add(31 * time.Second),
		base.Add(31 * time.Second),
	}
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):      {{out: `{"metadata":{"name":"wl-a"}}`}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-b", "-o", "json"):      {{out: `{"metadata":{"name":"wl-b"}}`}},
		},
	}
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			snap := status.Snapshot{
				Name:         "train-001",
				Namespace:    "ray",
				JobFound:     true,
				JobManagedBy: "kueue.x-k8s.io/multikueue",
				Workloads: []status.Workload{
					{Name: "wl-a", ClusterName: "worker-a"},
					{Name: "wl-b", ClusterName: "worker-b"},
				},
			}
			return snap, nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now: func() time.Time {
			if len(nowValues) == 0 {
				return base.Add(time.Hour)
			}
			cur := nowValues[0]
			nowValues = nowValues[1:]
			return cur
		},
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "state=Selected") {
		t.Fatalf("expected selected aggregate state, got %q", msg)
	}
	if strings.Contains(msg, "selected-worker=") {
		t.Fatalf("expected conflicting worker assignments to omit selected-worker detail, got %q", msg)
	}
}

func TestCancelWorkload_InconclusivePrefetchStillRequiresManagerCleanupProof(t *testing.T) {
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	current := base
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  2 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, context.DeadlineExceeded
		},
		wait: func(_ context.Context, d time.Duration) error {
			waitCalls++
			current = current.Add(d)
			return nil
		},
		now: func() time.Time { return current },
	})
	if err == nil {
		t.Fatal("expected uncertainty timeout once inconclusive rechecks exhaust the deadline")
	}
	for _, want := range []string{
		"timed out after 2s waiting to prove manager workload cleanup",
		"selectors=" + workloadmeta.LabelJob + "=train-001",
		"snapshot-error=context deadline exceeded",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
	if waitCalls != 2 {
		t.Fatalf("expected inconclusive rechecks to keep waiting until the deadline, got %d waits", waitCalls)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_MultiKueueWithNoCapturedWorkloadsRequiresConsistentAbsence(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
			},
		},
	}
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:         "train-001",
				Namespace:    "ray",
				JobFound:     true,
				JobManagedBy: "kueue.x-k8s.io/multikueue",
			}, nil
		},
		wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if waitCalls != 1 {
		t.Fatalf("expected one wait while proving workload absence, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_KueueManagedQueueLabelWithoutCapturedWorkloadsStillProvesCleanup(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				JobFound:  true,
				Labels:    map[string]string{runtopology.QueueLabel: "jobqueue"},
			}, nil
		},
		wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if waitCalls != 1 {
		t.Fatalf("expected one selector-based consistent-absence wait, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_CapturedManagerWorkloadsStillBlockFastPathWithoutQueueSignal(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New(`Error from server (NotFound): workloads.kueue.x-k8s.io "wl-a" not found`)},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	waitCalls := 0
	snapshotCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			snapshotCalls++
			snap := status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
			}
			if snapshotCalls == 1 {
				snap.Workloads = []status.Workload{{Name: "wl-a"}}
			}
			return snap, nil
		},
		wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name wait plus one final absence-proof wait, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_MultiKueueWithNoCapturedWorkloadsHandlesTransientListMiss(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[{"metadata":{"name":"wl-a"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
		},
	}
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:         "train-001",
				Namespace:    "ray",
				JobFound:     true,
				JobManagedBy: "kueue.x-k8s.io/multikueue",
			}, nil
		},
		wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if waitCalls != 3 {
		t.Fatalf("expected three waits (initial proof, finalizer, final consistent-absence proof), got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
	for _, call := range runner.joinedCalls() {
		if strings.Contains(call, "worker-a") || strings.Contains(call, "pods") || strings.Contains(call, "events") {
			t.Fatalf("cancel should stay manager-side, got call %q", call)
		}
	}
}

func TestCancelWorkload_KueueManagedQueueLabelWithNoCapturedWorkloadsHandlesTransientListMiss(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[{"metadata":{"name":"wl-a"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
		},
	}
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				JobFound:  true,
				Labels:    map[string]string{runtopology.QueueLabel: "jobqueue"},
			}, nil
		},
		wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if waitCalls != 3 {
		t.Fatalf("expected three waits (initial proof, finalizer, final consistent-absence proof), got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_MultiKueueWithNoCapturedWorkloadsTimesOutUncertain(t *testing.T) {
	base := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	nowValues := []time.Time{
		base,
		base,
		base,
		base,
		base.Add(31 * time.Second),
		base.Add(31 * time.Second),
	}
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
			},
		},
	}
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:         "train-001",
				Namespace:    "ray",
				JobFound:     true,
				JobManagedBy: "kueue.x-k8s.io/multikueue",
			}, nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now: func() time.Time {
			if len(nowValues) == 0 {
				return base.Add(time.Hour)
			}
			cur := nowValues[0]
			nowValues = nowValues[1:]
			return cur
		},
	})
	if err == nil {
		t.Fatal("expected uncertainty timeout")
	}
	for _, want := range []string{
		"timed out after 30s waiting to prove MultiKueue manager workload cleanup",
		"no manager workload names became visible after delete",
		"selectors=" + workloadmeta.LabelJob + "=train-001",
		"tau run status train-001 -n ray",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
}

func TestDiscoverAndWaitForManagerWorkloadsDeleted_StableSelectorFindsLingeringSameNameWorkload(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[{"metadata":{"name":"wl-old"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", "kueue.x-k8s.io/job-uid=job-uid", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-old", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-old"}}`},
				{err: errors.New(`Error from server (NotFound): workloads.kueue.x-k8s.io "wl-old" not found`)},
			},
		},
	}
	waitCalls := 0
	err := discoverAndWaitForManagerWorkloadsDeleted(
		context.Background(),
		runner,
		"ray",
		"train-001",
		managerCleanupSelectors("train-001", status.Snapshot{JobUID: "job-uid"}),
		status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobUID: "job-uid", JobManagedBy: "kueue.x-k8s.io/multikueue"},
		nil,
		managerCleanupOptions{Timeout: 30 * time.Second, Interval: time.Second},
		managerCleanupHooks{wait: func(context.Context, time.Duration) error { waitCalls++; return nil }, now: time.Now},
		time.Now().Add(30*time.Second),
		managerCleanupModeMultiKueue,
	)
	if err != nil {
		t.Fatalf("discoverAndWaitForManagerWorkloadsDeleted() error = %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one finalizer wait plus one final consistent-absence proof wait, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-old -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-old -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l kueue.x-k8s.io/job-uid=job-uid -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestDiscoverAndWaitForManagerWorkloadsDeleted_RediscoveredNamesReenterSelectorProofLoop(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[{"metadata":{"name":"wl-b"}}]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New(`Error from server (NotFound): workloads.kueue.x-k8s.io "wl-a" not found`)},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-b", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-b"}}`},
				{err: errors.New(`Error from server (NotFound): workloads.kueue.x-k8s.io "wl-b" not found`)},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	err := discoverAndWaitForManagerWorkloadsDeleted(
		context.Background(),
		runner,
		"ray",
		"train-001",
		[]string{workloadmeta.LabelJob + "=train-001"},
		status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobManagedBy: "kueue.x-k8s.io/multikueue"},
		nil,
		managerCleanupOptions{Timeout: 30 * time.Second, Interval: time.Second},
		managerCleanupHooks{
			fetchSnapshot: func(context.Context) (status.Snapshot, error) {
				fetchCalls++
				if fetchCalls == 1 {
					return multiKueueCancelSnapshot("wl-a"), nil
				}
				return status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobManagedBy: "kueue.x-k8s.io/multikueue"}, nil
			},
			wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
			now:  time.Now,
		},
		time.Now().Add(30*time.Second),
		managerCleanupModeMultiKueue,
	)
	if err != nil {
		t.Fatalf("discoverAndWaitForManagerWorkloadsDeleted() error = %v", err)
	}
	if fetchCalls != 3 {
		t.Fatalf("expected one rediscovery plus two authoritative empty proofs, got %d fetches", fetchCalls)
	}
	if waitCalls != 3 {
		t.Fatalf("expected waits for wl-a, wl-b, and final consistent-absence proof, got %d", waitCalls)
	}
	wantCalls := []string{
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-a -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-b -o json",
		"-n ray get workloads.kueue.x-k8s.io wl-b -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
		"-n ray get workloads.kueue.x-k8s.io -l " + workloadmeta.LabelJob + "=train-001 -o json",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestDiscoverAndWaitForManagerWorkloadsDeleted_RediscoveredNamesNeedFinalProofBudget(t *testing.T) {
	base := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	current := base
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New(`Error from server (NotFound): workloads.kueue.x-k8s.io "wl-a" not found`)},
			},
		},
	}
	fetchCalls := 0
	err := discoverAndWaitForManagerWorkloadsDeleted(
		context.Background(),
		runner,
		"ray",
		"train-001",
		[]string{workloadmeta.LabelJob + "=train-001"},
		status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobManagedBy: "kueue.x-k8s.io/multikueue"},
		nil,
		managerCleanupOptions{Timeout: 2 * time.Second, Interval: time.Second},
		managerCleanupHooks{
			fetchSnapshot: func(context.Context) (status.Snapshot, error) {
				fetchCalls++
				if fetchCalls == 1 {
					return multiKueueCancelSnapshot("wl-a"), nil
				}
				return status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobManagedBy: "kueue.x-k8s.io/multikueue"}, nil
			},
			wait: func(context.Context, time.Duration) error {
				current = current.Add(time.Second)
				return nil
			},
			now: func() time.Time { return current },
		},
		base.Add(2*time.Second),
		managerCleanupModeMultiKueue,
	)
	if err == nil {
		t.Fatal("expected timeout when rediscovered finalizer cleanup leaves no budget for the second final proof")
	}
	for _, want := range []string{
		"timed out after 2s waiting to prove MultiKueue manager workload cleanup",
		"manager workloads reappeared after delete but cleanup never stayed consistently absent",
		"rediscovered=wl-a",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
}

func TestDiscoverAndWaitForManagerWorkloadsDeleted_InconclusiveRechecksRequireConfirmedAbsence(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	err := discoverAndWaitForManagerWorkloadsDeleted(
		context.Background(),
		runner,
		"ray",
		"train-001",
		[]string{workloadmeta.LabelJob + "=train-001"},
		status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobManagedBy: "kueue.x-k8s.io/multikueue"},
		nil,
		managerCleanupOptions{Timeout: 30 * time.Second, Interval: time.Second},
		managerCleanupHooks{
			fetchSnapshot: func(context.Context) (status.Snapshot, error) {
				fetchCalls++
				if fetchCalls <= 2 {
					return status.Snapshot{Name: "train-001", Namespace: "ray"}, context.DeadlineExceeded
				}
				return status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, JobManagedBy: "kueue.x-k8s.io/multikueue"}, nil
			},
			wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
			now:  time.Now,
		},
		time.Now().Add(30*time.Second),
		managerCleanupModeMultiKueue,
	)
	if err != nil {
		t.Fatalf("discoverAndWaitForManagerWorkloadsDeleted() error = %v", err)
	}
	if fetchCalls != 4 {
		t.Fatalf("expected two inconclusive rechecks plus two confirmed absences, got %d fetches", fetchCalls)
	}
	if waitCalls != 3 {
		t.Fatalf("expected three waits before final confirmed absence, got %d", waitCalls)
	}
	if got := runner.joinedCalls(); len(got) != 4 {
		t.Fatalf("expected four empty workload-list probes, got %d: %v", len(got), got)
	}
}

func TestDiscoverAndWaitForManagerWorkloadsDeleted_RediscoverySnapshotUsesRemainingBudget(t *testing.T) {
	start := time.Now()
	deadline := start.Add(40 * time.Millisecond)
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
			},
		},
	}
	var fetchBudget time.Duration
	err := discoverAndWaitForManagerWorkloadsDeleted(context.Background(), runner, "ray", "train-001", []string{workloadmeta.LabelJob + "=train-001"}, status.Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: "kueue.x-k8s.io/multikueue",
	}, nil, managerCleanupOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(ctx context.Context) (status.Snapshot, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected rediscovery fetch context to have a deadline")
			}
			fetchBudget = time.Until(dl)
			<-ctx.Done()
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, ctx.Err()
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now:  time.Now,
	}, deadline, managerCleanupModeMultiKueue)
	if err == nil || !strings.Contains(err.Error(), "timed out after 30s waiting to prove MultiKueue manager workload cleanup") {
		t.Fatalf("expected uncertainty timeout, got %v", err)
	}
	if fetchBudget <= 0 || fetchBudget > 200*time.Millisecond {
		t.Fatalf("expected rediscovery fetch budget capped by remaining deadline, got %s", fetchBudget)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("expected rediscovery path not to overrun remaining deadline, elapsed %s", elapsed)
	}
}

func TestCancelWorkload_AlreadyCleanedReturnsSuccess(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: ""}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: ""}},
		},
	}
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, nil
		},
		wait: func(context.Context, time.Duration) error {
			t.Fatal("wait hook should not run for an already cleaned single-cluster workload")
			return nil
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
}

func TestCancelWorkload_DirectUnqueuedReturnsImmediatelyWithoutExtraCallsOrOutput(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: ""}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: ""}},
		},
	}
	var out strings.Builder
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", &out, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{Name: "train-001", Namespace: "ray", JobFound: true}, nil
		},
		wait: func(context.Context, time.Duration) error {
			t.Fatal("wait hook should not run for a direct unqueued workload")
			return nil
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected no delete output for direct unqueued workload, got %q", got)
	}
	wantCalls := []string{
		"-n ray delete rayjob.ray.io train-001 --ignore-not-found",
		"-n ray delete job train-001 --ignore-not-found",
		"-n ray delete service train-001-headless --ignore-not-found",
	}
	if got := runner.joinedCalls(); len(got) != len(wantCalls) {
		t.Fatalf("expected %d kubectl calls, got %d: %v", len(wantCalls), len(got), got)
	} else {
		for i := range wantCalls {
			if got[i] != wantCalls[i] {
				t.Fatalf("call[%d] = %q, want %q", i, got[i], wantCalls[i])
			}
		}
	}
}

func TestCancelWorkload_IgnoresMissingRayJobCRDForMultiKueue(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{err: errors.New("the server doesn't have a resource type \"rayjob\"")}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):      {{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	var out strings.Builder
	fetchCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", &out, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			fetchCalls++
			if fetchCalls == 1 {
				return multiKueueCancelSnapshot("wl-a"), nil
			}
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("cancelWorkload() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "job.batch") {
		t.Fatalf("expected Job delete output, got %q", got)
	}
}

func TestCancelWorkload_MultiKueueNamespaceNotFoundWhilePollingReturnsError(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):      {{err: errors.New("Error from server (NotFound): namespaces \"ray\" not found")}},
		},
	}
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueCancelSnapshot("wl-a"), nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now:  time.Now,
	})
	if err == nil || !strings.Contains(err.Error(), "namespaces \"ray\" not found") {
		t.Fatalf("expected namespace not found to propagate, got %v", err)
	}
}

func TestCancelWorkload_MultiKueueWrongWorkloadNotFoundReturnsError(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):      {{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-b\" not found")}},
		},
	}
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueCancelSnapshot("wl-a"), nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now:  time.Now,
	})
	if err == nil || !strings.Contains(err.Error(), "\"wl-b\" not found") {
		t.Fatalf("expected wrong workload not found to propagate, got %v", err)
	}
}

func TestCancelWorkload_PostDeleteTimeoutBudgetStartsAfterDelete(t *testing.T) {
	base := time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC)
	current := base
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train-001", "--ignore-not-found"): {{out: "rayjob.ray.io \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "train-001", "--ignore-not-found"):           {{out: "job.batch \"train-001\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=train-001", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	err := cancelWorkload(context.Background(), runner, "train-001", "ray", io.Discard, cancelWorkloadOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, cancelWorkloadHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			fetchCalls++
			if fetchCalls == 1 {
				current = base.Add(31 * time.Second)
				return multiKueueCancelSnapshot("wl-a"), nil
			}
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, nil
		},
		wait: func(_ context.Context, d time.Duration) error {
			waitCalls++
			current = current.Add(d)
			return nil
		},
		now: func() time.Time { return current },
	})
	if err != nil {
		t.Fatalf("expected cleanup to use the full post-delete budget, got %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name wait plus one selector absence proof wait after delete, got %d", waitCalls)
	}
}

func multiKueueCancelSnapshot(workloadNames ...string) status.Snapshot {
	workloads := make([]status.Workload, 0, len(workloadNames))
	for _, workloadName := range workloadNames {
		workloads = append(workloads, status.Workload{
			Name:        workloadName,
			ClusterName: "worker-a",
		})
	}
	return status.Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: "kueue.x-k8s.io/multikueue",
		Workloads:    workloads,
	}
}

type scriptedRawRunner struct {
	steps map[string][]scriptedRawResponse
	calls [][]string
}

type scriptedRawResponse struct {
	out string
	err error
}

type rawRunnerFunc func(context.Context, []string, []byte) (string, error)

func (f rawRunnerFunc) Raw(ctx context.Context, args []string, stdin []byte) (string, error) {
	return f(ctx, args, stdin)
}

func (f *scriptedRawRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	key := fakeRawKey(args...)
	steps, ok := f.steps[key]
	if !ok || len(steps) == 0 {
		return "", errors.New("unexpected kubectl args: " + strings.Join(args, " "))
	}
	resp := steps[0]
	if len(steps) == 1 {
		f.steps[key] = steps
	} else {
		f.steps[key] = steps[1:]
	}
	return resp.out, resp.err
}

func (f *scriptedRawRunner) joinedCalls() []string {
	out := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		out = append(out, strings.Join(call, " "))
	}
	return out
}
