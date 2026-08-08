package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	// rayOriginLabel is set by KubeRay on the RayCluster it creates for a RayJob.
	rayOriginLabel = "ray.io/originated-from-cr-name"
	// rayClusterLabel is set by KubeRay on every pod belonging to a RayCluster.
	rayClusterLabel = "ray.io/cluster"
	// defaultComputeReleaseTimeout covers the 600s terminationGracePeriodSeconds
	// Tau puts on Ray worker pods, plus slack for GC and kubelet.
	defaultComputeReleaseTimeout  = 12 * time.Minute
	defaultComputeReleaseInterval = 5 * time.Second
)

type computeReleaseOptions struct {
	// Enabled turns on the post-delete compute-release check. It is off by
	// default so callers that only exercise manager cleanup are unaffected.
	Enabled bool
	// Wait reports whether to block until compute is actually released.
	Wait     bool
	Timeout  time.Duration
	Interval time.Duration
}

type computeReleaseHooks struct {
	wait func(context.Context, time.Duration) error
	now  func() time.Time
}

// heldCompute is what a run still holds: its RayCluster, and the pods that are
// still occupying nodes.
type heldCompute struct {
	Clusters []string
	Pods     int
	Nodes    []string
}

func (h heldCompute) released() bool { return len(h.Clusters) == 0 && h.Pods == 0 }

func (h heldCompute) String() string {
	parts := make([]string, 0, 3)
	if len(h.Clusters) > 0 {
		parts = append(parts, "RayCluster "+strings.Join(h.Clusters, ","))
	}
	parts = append(parts, fmt.Sprintf("%d pod(s) still holding capacity", h.Pods))
	if len(h.Nodes) > 0 {
		parts = append(parts, "on "+strings.Join(h.Nodes, ","))
	}
	return strings.Join(parts, ", ")
}

func normalizeComputeReleaseOptions(opts computeReleaseOptions) (computeReleaseOptions, error) {
	if opts.Timeout == 0 {
		opts.Timeout = defaultComputeReleaseTimeout
	}
	if opts.Interval == 0 {
		opts.Interval = defaultComputeReleaseInterval
	}
	if opts.Timeout <= 0 {
		return computeReleaseOptions{}, fmt.Errorf("--teardown-timeout must be > 0")
	}
	if opts.Interval <= 0 {
		return computeReleaseOptions{}, fmt.Errorf("--interval must be > 0")
	}
	return opts, nil
}

func normalizeComputeReleaseHooks(hooks computeReleaseHooks) computeReleaseHooks {
	if hooks.wait == nil {
		hooks.wait = waitStatusInterval
	}
	if hooks.now == nil {
		hooks.now = time.Now
	}
	return hooks
}

// waitForComputeRelease blocks until the run's RayCluster and its pods are
// gone, so that "cancelled" means the node is actually free.
//
// This exists because deleting the RayJob does not release compute. KubeRay's
// RayJob deletion path only stops the Ray job and drops its finalizer; the
// RayCluster goes away through Kubernetes owner-reference garbage collection,
// and the pods then drain through a termination grace period that Tau sets to
// 600s. `kubectl delete rayjob` returns as soon as the RayJob object is gone,
// which is seconds before any of that finishes.
//
// Reporting success at that point is what makes the obvious recovery action --
// cancel, then resubmit -- the thing that breaks the cluster: the new run is
// admitted by Kueue against quota the old pods are still consuming, lands on a
// node it cannot fit on, and Ray's autoscaler then wedges with
// "No available node types can fulfill cluster constraint" indefinitely.
func waitForComputeRelease(
	ctx context.Context,
	r kubeRawRunner,
	ns, runName, capturedCluster string,
	w io.Writer,
	opts computeReleaseOptions,
	hooks computeReleaseHooks,
) error {
	opts, err := normalizeComputeReleaseOptions(opts)
	if err != nil {
		return err
	}
	if !opts.Enabled {
		return nil
	}
	hooks = normalizeComputeReleaseHooks(hooks)

	if !opts.Wait {
		reportAsyncTeardown(ctx, r, ns, runName, capturedCluster, w)
		return nil
	}

	deadline := hooks.now().Add(opts.Timeout)
	announced := false
	var last heldCompute

	for {
		held, err := computeHeldByRun(ctx, r, ns, runName, capturedCluster)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isMissingRayCRD(err) {
				return nil
			}
			return err
		}
		last = held
		if held.released() {
			if announced {
				fmt.Fprintf(w, "compute released: no RayCluster or pods remain for %s\n", runName)
			}
			return nil
		}
		if !announced {
			announced = true
			fmt.Fprintf(w, "waiting for %s to release compute (%s)\n", runName, held)
		}

		if !hooks.now().Before(deadline) {
			break
		}
		if err := waitComputeReleaseInterval(ctx, hooks, deadline, opts.Interval); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return computeReleaseTimeoutError(opts.Timeout, ns, runName, last)
}

func waitComputeReleaseInterval(ctx context.Context, hooks computeReleaseHooks, deadline time.Time, interval time.Duration) error {
	waitFor := interval
	if untilDeadline := deadline.Sub(hooks.now()); untilDeadline < waitFor {
		waitFor = untilDeadline
	}
	if waitFor <= 0 {
		return nil
	}
	if err := hooks.wait(ctx, waitFor); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// reportAsyncTeardown states plainly that compute is still held, so --wait=false
// cannot be mistaken for "the node is free now".
func reportAsyncTeardown(ctx context.Context, r kubeRawRunner, ns, runName, capturedCluster string, w io.Writer) {
	held, err := computeHeldByRun(ctx, r, ns, runName, capturedCluster)
	if err != nil {
		fmt.Fprintf(w, "teardown is asynchronous: the RayCluster and its pods may still hold nodes; re-run with --wait to confirm release\n")
		return
	}
	if held.released() {
		return
	}
	fmt.Fprintf(w, "teardown is asynchronous and NOT complete: %s\n", held)
	fmt.Fprintf(w, "those pods still hold their nodes, so resubmitting now can be admitted against quota they are still using\n")
	fmt.Fprintf(w, "re-run with --wait, or poll: kubectl -n %s get pods -l %s\n", ns, rayClusterLabel)
}

func computeReleaseTimeoutError(timeout time.Duration, ns, runName string, held heldCompute) error {
	return fmt.Errorf(
		"timed out after %s waiting for %s/%s to release compute: %s; do NOT resubmit yet or the new run will be admitted against quota these pods still hold; recover with `tau run delete %s -n %s`, which verifies immutable Tau ownership before removing any orphaned RayCluster",
		timeout, ns, runName, held, runName, ns,
	)
}

// computeHeldByRun reports the RayClusters and pods the run still has on nodes.
// Pods are read even when no RayCluster remains, because they outlive it while
// they drain -- and it is the pods, not the RayCluster object, that hold quota.
func computeHeldByRun(ctx context.Context, r kubeRawRunner, ns, runName, capturedCluster string) (heldCompute, error) {
	clusters, err := listRunRayClusters(ctx, r, ns, runName, capturedCluster)
	if err != nil {
		return heldCompute{}, err
	}
	held := heldCompute{Clusters: clusters}

	out, err := r.Raw(ctx, []string{"-n", ns, "get", "pods", "-l", rayClusterLabel, "-o", "json"}, nil)
	if err != nil {
		return heldCompute{}, fmt.Errorf("listing Ray pods in %s: %w", ns, err)
	}
	var podList struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &podList); err != nil {
		return heldCompute{}, fmt.Errorf("decoding Ray pods in %s: %w", ns, err)
	}

	wanted := make(map[string]bool, len(clusters)+1)
	for _, c := range clusters {
		wanted[c] = true
	}
	if capturedCluster != "" {
		wanted[capturedCluster] = true
	}
	nodes := map[string]bool{}
	for _, pod := range podList.Items {
		if !holdsCapacity(pod.Status.Phase) {
			continue
		}
		cluster := pod.Metadata.Labels[rayClusterLabel]
		if !wanted[cluster] && cluster != runName && !strings.HasPrefix(cluster, runName+"-") {
			continue
		}
		held.Pods++
		if pod.Spec.NodeName != "" {
			nodes[pod.Spec.NodeName] = true
		}
	}
	held.Nodes = sortedNodeNames(nodes)
	return held, nil
}

func listRunRayClusters(ctx context.Context, r kubeRawRunner, ns, runName, capturedCluster string) ([]string, error) {
	out, err := r.Raw(ctx, []string{"-n", ns, "get", "rayclusters.ray.io", "-o", "json"}, nil)
	if err != nil {
		if isMissingRayCRD(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing RayClusters in %s: %w", ns, err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decoding RayClusters in %s: %w", ns, err)
	}
	var names []string
	for _, item := range list.Items {
		if item.Metadata.Name == capturedCluster || item.Metadata.Labels[rayOriginLabel] == runName {
			names = append(names, item.Metadata.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// holdsCapacity reports whether a pod in this phase still occupies its node.
// Terminating pods report Running, which is correct here: they hold capacity
// until they are gone.
func holdsCapacity(phase string) bool {
	switch phase {
	case "Succeeded", "Failed":
		return false
	default:
		return true
	}
}

func isMissingRayCRD(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the server doesn't have a resource type") ||
		strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "could not find the requested resource")
}

func sortedNodeNames(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
