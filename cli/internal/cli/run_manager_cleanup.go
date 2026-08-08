// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	defaultManagerCleanupTimeout       = 30 * time.Second
	defaultManagerCleanupInterval      = 2 * time.Second
	managerCleanupSnapshotWindow       = 2 * time.Second
	managerCleanupRequiredAbsenceProof = 2
)

type managerCleanupOptions struct {
	Timeout  time.Duration
	Interval time.Duration
	// Release controls the post-delete wait that proves compute was actually
	// freed. Deleting the RayJob only starts teardown; see
	// waitForComputeRelease.
	Release computeReleaseOptions
}

type cancelWorkloadOptions = managerCleanupOptions

type managerCleanupMode string

const (
	managerCleanupModeUnknown    managerCleanupMode = ""
	managerCleanupModeKueue      managerCleanupMode = "Kueue"
	managerCleanupModeMultiKueue managerCleanupMode = "MultiKueue"
)

type managerCleanupHooks struct {
	fetchSnapshot func(context.Context) (status.Snapshot, error)
	wait          func(context.Context, time.Duration) error
	now           func() time.Time
}

type cancelWorkloadHooks = managerCleanupHooks

func deleteWorkloadAndWaitForManagerCleanup(ctx context.Context, r kubeRawRunner, name, ns string, w io.Writer, opts managerCleanupOptions, hooks managerCleanupHooks) error {
	opts, err := normalizeManagerCleanupOptions(opts)
	if err != nil {
		return err
	}
	hooks = normalizeManagerCleanupHooks(hooks)
	before, beforeErr := fetchManagerCleanupSnapshot(ctx, hooks)
	if beforeErr != nil && ctx.Err() != nil {
		return beforeErr
	}
	// Capture the RayCluster name before the delete. Once the RayJob is gone
	// its .status.rayClusterName is unrecoverable, which is precisely why the
	// old code could not verify its own work.
	capturedCluster := before.RayJob.RayClusterName
	if capturedCluster == "" {
		capturedCluster = before.RayClusterName
	}
	if err := deleteWorkload(ctx, r, name, ns, w); err != nil {
		return err
	}
	if err := waitForManagerCleanup(ctx, r, name, ns, before, beforeErr, opts, hooks); err != nil {
		return err
	}
	// Runs on every path, including the direct/unqueued early return above:
	// a run with no Kueue Workload still leaves a RayCluster behind.
	return waitForComputeRelease(ctx, r, ns, name, capturedCluster, w, opts.Release, computeReleaseHooks{
		wait: hooks.wait,
		now:  hooks.now,
	})
}

func waitForManagerCleanup(ctx context.Context, r kubeRawRunner, name, ns string, before status.Snapshot, beforeErr error, opts managerCleanupOptions, hooks managerCleanupHooks) error {
	deadline := hooks.now().Add(opts.Timeout)
	workloadNames := capturedWorkloadNames(before)
	if !before.IsMultiKueue() && !before.IsKueueManaged() && len(workloadNames) == 0 && beforeErr == nil {
		return nil
	}
	mode := classifyManagerCleanupMode(before)

	selectors := managerCleanupSelectors(name, before)
	if len(workloadNames) > 0 {
		if err := waitForManagerWorkloadsDeleted(ctx, r, ns, name, workloadNames, before, beforeErr, opts, hooks, deadline, mode); err != nil {
			return err
		}
	}
	return discoverAndWaitForManagerWorkloadsDeleted(ctx, r, ns, name, selectors, before, beforeErr, opts, hooks, deadline, mode)
}

func normalizeManagerCleanupOptions(opts managerCleanupOptions) (managerCleanupOptions, error) {
	if opts.Timeout == 0 {
		opts.Timeout = defaultManagerCleanupTimeout
	}
	if opts.Interval == 0 {
		opts.Interval = defaultManagerCleanupInterval
	}
	if opts.Timeout <= 0 {
		return managerCleanupOptions{}, fmt.Errorf("--timeout must be > 0")
	}
	if opts.Interval <= 0 {
		return managerCleanupOptions{}, fmt.Errorf("--interval must be > 0")
	}
	return opts, nil
}

func normalizeManagerCleanupHooks(hooks managerCleanupHooks) managerCleanupHooks {
	if hooks.wait == nil {
		hooks.wait = waitStatusInterval
	}
	if hooks.now == nil {
		hooks.now = time.Now
	}
	return hooks
}

func fetchManagerCleanupSnapshot(ctx context.Context, hooks managerCleanupHooks) (status.Snapshot, error) {
	return fetchManagerCleanupStrictSnapshot(ctx, hooks, managerCleanupSnapshotWindow)
}

func fetchManagerCleanupStrictSnapshot(ctx context.Context, hooks managerCleanupHooks, budget time.Duration) (status.Snapshot, error) {
	if hooks.fetchSnapshot == nil {
		return status.Snapshot{}, nil
	}
	if budget <= 0 {
		return status.Snapshot{}, context.DeadlineExceeded
	}
	if budget > managerCleanupSnapshotWindow {
		budget = managerCleanupSnapshotWindow
	}
	fetchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	snap, err := hooks.fetchSnapshot(fetchCtx)
	if err == nil && fetchCtx.Err() != nil {
		err = fetchCtx.Err()
	}
	return snap, err
}

func fetchManagerCleanupStrictSnapshotBefore(ctx context.Context, hooks managerCleanupHooks, deadline time.Time) (status.Snapshot, error) {
	return fetchManagerCleanupStrictSnapshot(ctx, hooks, deadline.Sub(hooks.now()))
}

func fetchManagerCleanupDiagnosticSnapshot(ctx context.Context, hooks managerCleanupHooks, deadline time.Time, fallback status.Snapshot) (status.Snapshot, error) {
	remaining := deadline.Sub(hooks.now())
	if remaining <= 0 {
		if managerCleanupSnapshotKnown(fallback) {
			return fallback, nil
		}
		return status.Snapshot{}, context.DeadlineExceeded
	}
	snap, err := fetchManagerCleanupStrictSnapshotBefore(ctx, hooks, deadline)
	if err != nil && managerCleanupSnapshotKnown(fallback) {
		return fallback, err
	}
	return snap, err
}

func managerCleanupSnapshotInconclusive(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func managerCleanupSnapshotError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func managerCleanupSnapshotKnown(snap status.Snapshot) bool {
	return snap.Name != "" || snap.Namespace != "" || snap.JobFound || snap.RayJob.Found || len(snap.Workloads) > 0
}

func capturedWorkloadNames(snap status.Snapshot) []string {
	seen := make(map[string]bool, len(snap.Workloads))
	names := make([]string, 0, len(snap.Workloads))
	for _, workload := range snap.Workloads {
		if workload.Name == "" || seen[workload.Name] {
			continue
		}
		seen[workload.Name] = true
		names = append(names, workload.Name)
	}
	sort.Strings(names)
	return names
}

func managerCleanupSelectors(runName string, snap status.Snapshot) []string {
	selectors := make([]string, 0, 4)
	if runName != "" {
		selectors = append(selectors, workloadmeta.LabelJob+"="+runName)
	}
	if snap.JobUID != "" {
		selectors = append(selectors, "kueue.x-k8s.io/job-uid="+snap.JobUID)
	}
	if snap.RayJob.UID != "" {
		selectors = append(selectors, "kueue.x-k8s.io/job-uid="+snap.RayJob.UID)
	}
	seen := make(map[string]bool, len(selectors))
	out := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		if selector == "" || seen[selector] {
			continue
		}
		seen[selector] = true
		out = append(out, selector)
	}
	return out
}

func waitForManagerCleanupInterval(ctx context.Context, hooks managerCleanupHooks, deadline time.Time, interval time.Duration) error {
	if !hooks.now().Before(deadline) {
		return nil
	}
	waitFor := interval
	if untilDeadline := deadline.Sub(hooks.now()); untilDeadline < waitFor {
		waitFor = untilDeadline
	}
	if waitFor <= 0 {
		return nil
	}
	return hooks.wait(ctx, waitFor)
}

func classifyManagerCleanupMode(snap status.Snapshot) managerCleanupMode {
	if snap.IsMultiKueue() {
		return managerCleanupModeMultiKueue
	}
	if snap.IsKueueManaged() || len(snap.Workloads) > 0 {
		return managerCleanupModeKueue
	}
	return managerCleanupModeUnknown
}

func managerCleanupWorkloadLabel(mode managerCleanupMode) string {
	if mode == managerCleanupModeMultiKueue {
		return "MultiKueue manager workload"
	}
	if mode == managerCleanupModeKueue {
		return "Kueue workload"
	}
	return "manager workload"
}

func managerCleanupSummaryPrefix(mode managerCleanupMode) string {
	if mode == managerCleanupModeMultiKueue {
		return "manager workload"
	}
	if mode == managerCleanupModeKueue {
		return "Kueue workload"
	}
	return "manager workload"
}

func waitForManagerWorkloadsDeleted(ctx context.Context, r kubeRawRunner, ns, runName string, workloadNames []string, last status.Snapshot, lastErr error, opts managerCleanupOptions, hooks managerCleanupHooks, deadline time.Time, mode managerCleanupMode) error {
	remaining := append([]string(nil), workloadNames...)
	for {
		next := remaining[:0]
		timedOut := false
	exactChecks:
		for i, workloadName := range remaining {
			timeLeft := deadline.Sub(hooks.now())
			if timeLeft <= 0 {
				next = append(next, remaining[i:]...)
				timedOut = true
				break
			}
			getCtx, cancel := context.WithTimeout(ctx, timeLeft)
			_, err := r.Raw(getCtx, []string{"-n", ns, "get", "workloads.kueue.x-k8s.io", workloadName, "-o", "json"}, nil)
			cancel()
			switch {
			case err == nil:
				next = append(next, workloadName)
			case isExactObjectNotFound(err, workloadName, "workload.kueue.x-k8s.io", "workloads.kueue.x-k8s.io"):
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
				next = append(next, remaining[i:]...)
				timedOut = true
				break exactChecks
			default:
				return err
			}
		}
		remaining = next
		if len(remaining) == 0 {
			return nil
		}
		if timedOut || !hooks.now().Before(deadline) {
			break
		}
		if err := waitForManagerCleanupInterval(ctx, hooks, deadline, opts.Interval); err != nil {
			return err
		}
	}

	latest, snapErr := fetchManagerCleanupDiagnosticSnapshot(ctx, hooks, deadline, last)
	return managerCleanupTimeoutError(mode, opts.Timeout, ns, runName, remaining, latest, managerCleanupSnapshotError(snapErr, lastErr))
}

func discoverAndWaitForManagerWorkloadsDeleted(ctx context.Context, r kubeRawRunner, ns, runName string, selectors []string, last status.Snapshot, lastErr error, opts managerCleanupOptions, hooks managerCleanupHooks, deadline time.Time, mode managerCleanupMode) error {
	absenceProofs := 0
	rediscoveredNames := make([]string, 0)
	for {
		if !hooks.now().Before(deadline) {
			break
		}
		workloadNames, err := listManagerWorkloadNames(ctx, r, ns, selectors, hooks, deadline)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			return err
		}
		absenceConfirmed := false
		if len(workloadNames) == 0 {
			snap, snapErr := fetchManagerCleanupStrictSnapshotBefore(ctx, hooks, deadline)
			if snapErr == nil {
				last = snap
				lastErr = nil
				workloadNames = capturedWorkloadNames(snap)
				absenceConfirmed = len(workloadNames) == 0
			} else {
				last = snap
				lastErr = snapErr
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !managerCleanupSnapshotInconclusive(ctx, snapErr) {
					return snapErr
				}
			}
		}
		if len(workloadNames) > 0 {
			absenceProofs = 0
			rediscoveredNames = appendUniqueSortedWorkloadNames(rediscoveredNames, workloadNames)
			if err := waitForManagerWorkloadsDeleted(ctx, r, ns, runName, workloadNames, last, lastErr, opts, hooks, deadline, mode); err != nil {
				return err
			}
			continue
		}
		if absenceConfirmed {
			absenceProofs++
			if absenceProofs >= managerCleanupRequiredAbsenceProof {
				return nil
			}
		}
		if !hooks.now().Before(deadline) {
			break
		}
		if err := waitForManagerCleanupInterval(ctx, hooks, deadline, opts.Interval); err != nil {
			return err
		}
	}

	latest, snapErr := fetchManagerCleanupDiagnosticSnapshot(ctx, hooks, deadline, last)
	return managerCleanupUncertainTimeoutError(mode, opts.Timeout, ns, runName, selectors, rediscoveredNames, latest, managerCleanupSnapshotError(snapErr, lastErr))
}

func appendUniqueSortedWorkloadNames(existing, names []string) []string {
	if len(names) == 0 {
		return existing
	}
	seen := make(map[string]bool, len(existing)+len(names))
	for _, name := range existing {
		if name == "" {
			continue
		}
		seen[name] = true
	}
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		existing = append(existing, name)
	}
	sort.Strings(existing)
	return existing
}

type workloadListJSON struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

func listManagerWorkloadNames(ctx context.Context, r kubeRawRunner, ns string, selectors []string, hooks managerCleanupHooks, deadline time.Time) ([]string, error) {
	seen := make(map[string]bool)
	names := make([]string, 0)
	for _, selector := range selectors {
		timeLeft := deadline.Sub(hooks.now())
		if timeLeft <= 0 {
			return names, context.DeadlineExceeded
		}
		listCtx, cancel := context.WithTimeout(ctx, timeLeft)
		out, err := r.Raw(listCtx, []string{"-n", ns, "get", "workloads.kueue.x-k8s.io", "-l", selector, "-o", "json"}, nil)
		cancel()
		if err != nil {
			return nil, err
		}
		var parsed workloadListJSON
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			return nil, fmt.Errorf("parsing workload list for selector %q: %w", selector, err)
		}
		for _, item := range parsed.Items {
			if item.Metadata.Name == "" || seen[item.Metadata.Name] {
				continue
			}
			seen[item.Metadata.Name] = true
			names = append(names, item.Metadata.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func managerCleanupTimeoutError(mode managerCleanupMode, timeout time.Duration, ns, runName string, remaining []string, snap status.Snapshot, snapErr error) error {
	var detail []string
	if state := strings.TrimSpace(string(snap.MultiKueueState())); state != "" {
		detail = append(detail, "state="+state)
	}
	if worker := strings.TrimSpace(snap.PlacementWorkerCluster()); worker != "" {
		detail = append(detail, "selected-worker="+worker)
	} else if nominated := snap.NominatedWorkerClusters(); len(nominated) > 0 {
		detail = append(detail, "nominated-workers="+strings.Join(nominated, ","))
	}
	if admission := describeAdmissionStates(snap, remaining); admission != "" {
		detail = append(detail, "admission="+admission)
	}
	if snapErr != nil {
		detail = append(detail, "snapshot-error="+snapErr.Error())
	}
	if len(detail) == 0 {
		detail = append(detail, "rerun `tau run status "+runName+" -n "+ns+"` for manager-side state")
	} else {
		detail = append(detail, "rerun `tau run status "+runName+" -n "+ns+"` for the full manager-side snapshot")
	}
	return fmt.Errorf("timed out after %s waiting for %s finalizer to remove %s: %s",
		timeout,
		managerCleanupWorkloadLabel(mode),
		strings.Join(remaining, ", "),
		strings.Join(detail, "; "),
	)
}

func managerCleanupUncertainTimeoutError(mode managerCleanupMode, timeout time.Duration, ns, runName string, selectors, rediscoveredNames []string, snap status.Snapshot, snapErr error) error {
	var detail []string
	if len(rediscoveredNames) > 0 {
		detail = append(detail, "rediscovered="+strings.Join(rediscoveredNames, ","))
	}
	if len(selectors) > 0 {
		detail = append(detail, "selectors="+strings.Join(selectors, ","))
	}
	if state := strings.TrimSpace(string(snap.MultiKueueState())); state != "" {
		detail = append(detail, "state="+state)
	}
	if worker := strings.TrimSpace(snap.PlacementWorkerCluster()); worker != "" {
		detail = append(detail, "selected-worker="+worker)
	}
	if snapErr != nil {
		detail = append(detail, "snapshot-error="+snapErr.Error())
	}
	detail = append(detail, "rerun `tau run status "+runName+" -n "+ns+"` and confirm the "+managerCleanupSummaryPrefix(mode)+" is gone before retrying")
	prefix := managerCleanupSummaryPrefix(mode)
	summary := "no " + prefix + " names became visible after delete"
	if len(rediscoveredNames) > 0 {
		summary = prefix + "s reappeared after delete but cleanup never stayed consistently absent"
	}
	return fmt.Errorf("timed out after %s waiting to prove %s cleanup for %s/%s: %s; %s",
		timeout,
		managerCleanupWorkloadLabel(mode),
		ns,
		runName,
		summary,
		strings.Join(detail, "; "),
	)
}
