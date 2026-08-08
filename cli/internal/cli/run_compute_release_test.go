// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/status"
)

// computeReleaseRunner answers the reads computeHeldByRun makes, and lets
// a test change the answers after N polls so teardown can be simulated.
type computeReleaseRunner struct {
	clusters []string
	pods     []string
	calls    []string
	polls    int
	// onPoll runs before each RayCluster list and may mutate the state.
	onPoll func(r *computeReleaseRunner, poll int)
	errs   map[string]error
}

func (r *computeReleaseRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	joined := strings.Join(args, " ")
	r.calls = append(r.calls, joined)
	for fragment, err := range r.errs {
		if strings.Contains(joined, fragment) {
			return "", err
		}
	}
	switch {
	case strings.Contains(joined, "get rayclusters.ray.io"):
		if r.onPoll != nil {
			r.onPoll(r, r.polls)
		}
		r.polls++
		return jsonList(r.clusters), nil
	case strings.Contains(joined, "get pods"):
		return jsonList(r.pods), nil
	}
	return "", errors.New("unexpected kubectl args: " + joined)
}

func (r *computeReleaseRunner) sawCall(fragment string) bool {
	for _, c := range r.calls {
		if strings.Contains(c, fragment) {
			return true
		}
	}
	return false
}

func jsonList(items []string) string {
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func rayClusterJSON(name, origin string) string {
	return `{"metadata":{"name":"` + name + `","labels":{"` + rayOriginLabel + `":"` + origin + `"}}}`
}

func rayPodJSON(name, cluster, node string) string {
	return `{"metadata":{"name":"` + name + `","labels":{"` + rayClusterLabel + `":"` + cluster + `"}},` +
		`"spec":{"nodeName":"` + node + `"},"status":{"phase":"Running"}}`
}

func testComputeReleaseHooks() computeReleaseHooks {
	now := time.Unix(0, 0)
	return computeReleaseHooks{
		wait: func(_ context.Context, d time.Duration) error {
			now = now.Add(d)
			return nil
		},
		now: func() time.Time { return now },
	}
}

// TestWaitForComputeRelease_BlocksUntilPodsAreActuallyGone is the core
// regression: cancel must not report success while pods still hold the node.
func TestWaitForComputeRelease_BlocksUntilPodsAreActuallyGone(t *testing.T) {
	r := &computeReleaseRunner{
		clusters: []string{rayClusterJSON("run-a-raycluster", "run-a")},
		pods: []string{
			rayPodJSON("head", "run-a-raycluster", "node-1"),
			rayPodJSON("worker-0", "run-a-raycluster", "node-1"),
		},
		onPoll: func(r *computeReleaseRunner, poll int) {
			// GC removes the cluster, then pods drain.
			if poll == 1 {
				r.clusters = nil
			}
			if poll == 2 {
				r.pods = nil
			}
		},
	}
	var out strings.Builder
	err := waitForComputeRelease(context.Background(), r, "ray", "run-a", "run-a-raycluster", &out,
		computeReleaseOptions{Enabled: true, Wait: true, Timeout: time.Minute, Interval: time.Second},
		testComputeReleaseHooks())
	if err != nil {
		t.Fatalf("waitForComputeRelease() error = %v", err)
	}
	if r.polls < 3 {
		t.Fatalf("expected to keep polling until pods drained, polled %d times", r.polls)
	}
	got := out.String()
	if !strings.Contains(got, "waiting for run-a to release compute") {
		t.Fatalf("expected progress output naming the run, got %q", got)
	}
	if !strings.Contains(got, "2 pod(s) still holding capacity") || !strings.Contains(got, "node-1") {
		t.Fatalf("expected pod count and node in progress output, got %q", got)
	}
	if !strings.Contains(got, "compute released") {
		t.Fatalf("expected an explicit release confirmation, got %q", got)
	}
}

func TestWaitForComputeRelease_ReturnsImmediatelyWhenAlreadyClean(t *testing.T) {
	r := &computeReleaseRunner{}
	var out strings.Builder
	err := waitForComputeRelease(context.Background(), r, "ray", "run-a", "run-a-raycluster", &out,
		computeReleaseOptions{Enabled: true, Wait: true, Timeout: time.Minute, Interval: time.Second},
		testComputeReleaseHooks())
	if err != nil {
		t.Fatalf("waitForComputeRelease() error = %v", err)
	}
	if out.String() != "" {
		t.Fatalf("expected no output when nothing was left, got %q", out.String())
	}
}

func TestWaitForComputeRelease_TimeoutNamesClusterPodsAndRecovery(t *testing.T) {
	r := &computeReleaseRunner{
		clusters: []string{rayClusterJSON("run-a-raycluster", "run-a")},
		pods:     []string{rayPodJSON("head", "run-a-raycluster", "node-1")},
	}
	var out strings.Builder
	err := waitForComputeRelease(context.Background(), r, "ray", "run-a", "run-a-raycluster", &out,
		computeReleaseOptions{Enabled: true, Wait: true, Timeout: 10 * time.Second, Interval: time.Second},
		testComputeReleaseHooks())
	if err == nil {
		t.Fatal("expected a non-nil error when compute was never released")
	}
	msg := err.Error()
	for _, want := range []string{
		"timed out",
		"run-a-raycluster",
		"1 pod(s) still holding capacity",
		"do NOT resubmit yet",
		"kubectl -n ray delete rayclusters.ray.io run-a-raycluster",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected timeout error to contain %q, got %q", want, msg)
		}
	}
}

func TestWaitForComputeRelease_NoWaitSaysTeardownIsAsynchronous(t *testing.T) {
	r := &computeReleaseRunner{
		clusters: []string{rayClusterJSON("run-a-raycluster", "run-a")},
		pods:     []string{rayPodJSON("head", "run-a-raycluster", "node-1")},
	}
	var out strings.Builder
	err := waitForComputeRelease(context.Background(), r, "ray", "run-a", "run-a-raycluster", &out,
		computeReleaseOptions{Enabled: true, Wait: false, Timeout: time.Minute, Interval: time.Second},
		testComputeReleaseHooks())
	if err != nil {
		t.Fatalf("--wait=false must not fail, got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "teardown is asynchronous and NOT complete") {
		t.Fatalf("expected an explicit async-teardown notice, got %q", got)
	}
	if !strings.Contains(got, "still hold their nodes") {
		t.Fatalf("expected the resubmit hazard to be stated, got %q", got)
	}
	if r.sawCall("delete rayclusters.ray.io") {
		t.Fatal("--wait=false must not delete anything")
	}
}

func TestWaitForComputeRelease_DisabledIsANoop(t *testing.T) {
	r := &computeReleaseRunner{}
	if err := waitForComputeRelease(context.Background(), r, "ray", "run-a", "", &strings.Builder{},
		computeReleaseOptions{}, testComputeReleaseHooks()); err != nil {
		t.Fatalf("waitForComputeRelease() error = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("expected no kubectl calls when disabled, got %v", r.calls)
	}
}

func TestWaitForComputeRelease_MissingRayCRDIsTolerated(t *testing.T) {
	r := &computeReleaseRunner{
		errs: map[string]error{
			"get rayclusters.ray.io": errors.New(`the server doesn't have a resource type "rayclusters"`),
		},
	}
	if err := waitForComputeRelease(context.Background(), r, "ray", "run-a", "", &strings.Builder{},
		computeReleaseOptions{Enabled: true, Wait: true, Timeout: time.Minute, Interval: time.Second},
		testComputeReleaseHooks()); err != nil {
		t.Fatalf("expected missing Ray CRD to be tolerated, got %v", err)
	}
}

// cancelFlowRunner drives the whole cancel path: deletes first, then the
// compute-release reads.
type cancelFlowRunner struct {
	computeReleaseRunner
}

func (r *cancelFlowRunner) Raw(ctx context.Context, args []string, stdin []byte) (string, error) {
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "-n ray delete rayjob.ray.io") ||
		strings.HasPrefix(joined, "-n ray delete job") ||
		strings.HasPrefix(joined, "-n ray delete service") {
		r.calls = append(r.calls, joined)
		return "", nil
	}
	return r.computeReleaseRunner.Raw(ctx, args, stdin)
}

// TestDeleteWorkloadAndWaitForManagerCleanup_CapturesRayClusterNameBeforeDelete
// pins the reason the original bug could not self-diagnose: once the RayJob is
// deleted its .status.rayClusterName is gone, so the name must be read first.
func TestDeleteWorkloadAndWaitForManagerCleanup_CapturesRayClusterNameBeforeDelete(t *testing.T) {
	r := &cancelFlowRunner{computeReleaseRunner: computeReleaseRunner{
		// No origin label, so only the pre-delete captured name can match.
		clusters: []string{`{"metadata":{"name":"captured-raycluster","creationTimestamp":"2024-01-01T00:00:00Z"}}`},
		pods:     []string{rayPodJSON("head", "captured-raycluster", "node-1")},
		onPoll: func(r *computeReleaseRunner, poll int) {
			if poll == 1 {
				r.clusters = nil
				r.pods = nil
			}
		},
	}}
	clock := testComputeReleaseHooks()
	var out strings.Builder
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), r, "run-a", "ray", &out,
		managerCleanupOptions{
			Timeout:  time.Minute,
			Interval: time.Second,
			Release:  computeReleaseOptions{Enabled: true, Wait: true, Timeout: time.Minute, Interval: time.Second},
		},
		managerCleanupHooks{
			fetchSnapshot: func(context.Context) (status.Snapshot, error) {
				return status.Snapshot{
					Name:      "run-a",
					Namespace: "ray",
					RayJob:    status.RayJob{Found: true, RayClusterName: "captured-raycluster"},
				}, nil
			},
			wait: clock.wait,
			now:  clock.now,
		})
	if err != nil {
		t.Fatalf("deleteWorkloadAndWaitForManagerCleanup() error = %v", err)
	}
	if !strings.Contains(out.String(), "captured-raycluster") {
		t.Fatalf("expected the pre-delete RayCluster name to be used after delete, got %q", out.String())
	}
	// The compute-release wait must run even on the direct/unqueued path,
	// which returns early from the Kueue manager wait.
	if !r.sawCall("get rayclusters.ray.io") {
		t.Fatalf("expected compute release to run on the unqueued path, calls: %v", r.calls)
	}
	deleteIdx, listIdx := -1, -1
	for i, c := range r.calls {
		if deleteIdx < 0 && strings.Contains(c, "delete rayjob.ray.io") {
			deleteIdx = i
		}
		if listIdx < 0 && strings.Contains(c, "get rayclusters.ray.io") {
			listIdx = i
		}
	}
	if deleteIdx < 0 || listIdx < deleteIdx {
		t.Fatalf("expected the release check to run after the delete, calls: %v", r.calls)
	}
}

func TestDeleteWorkloadAndWaitForManagerCleanup_FailsWhenComputeIsNeverReleased(t *testing.T) {
	r := &cancelFlowRunner{computeReleaseRunner: computeReleaseRunner{
		clusters: []string{rayClusterJSON("run-a-raycluster", "run-a")},
		pods:     []string{rayPodJSON("head", "run-a-raycluster", "node-1")},
	}}
	clock := testComputeReleaseHooks()
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), r, "run-a", "ray", &strings.Builder{},
		managerCleanupOptions{
			Timeout:  time.Minute,
			Interval: time.Second,
			Release:  computeReleaseOptions{Enabled: true, Wait: true, Timeout: 5 * time.Second, Interval: time.Second},
		},
		managerCleanupHooks{
			fetchSnapshot: func(context.Context) (status.Snapshot, error) {
				return status.Snapshot{Name: "run-a", Namespace: "ray"}, nil
			},
			wait: clock.wait,
			now:  clock.now,
		})
	if err == nil || !strings.Contains(err.Error(), "release compute") {
		t.Fatalf("expected cancel to fail rather than report success, got %v", err)
	}
}

func TestRunCancelCmd_ExposesWaitAndTeardownTimeoutFlags(t *testing.T) {
	cmd := newRunCancelCmd()
	wait := cmd.Flags().Lookup("wait")
	if wait == nil {
		t.Fatal("expected a --wait flag")
	}
	if wait.DefValue != "true" {
		t.Fatalf("--wait must default to true; the failure mode of not waiting is a silent multi-hour stall, got %q", wait.DefValue)
	}
	if cmd.Flags().Lookup("teardown-timeout") == nil {
		t.Fatal("expected a --teardown-timeout flag")
	}
}
