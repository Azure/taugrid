// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type rawRunner interface {
	Raw(ctx context.Context, extraArgs []string, stdin []byte) (string, error)
}

// FetchRunLogs populates the minimal Snapshot surface `tau run logs` needs to
// decide between local Job/RayJob logs and manager-side MultiKueue ADX logs.
// Unlike Fetch, it never queries Pods, Events, ResourceClaims, or
// AdmissionChecks. Real kubectl/query failures are returned alongside any
// successfully hydrated partial snapshot so the CLI can surface authoritative
// manager-side placement errors without widening RBAC.
func FetchRunLogs(ctx context.Context, r *kube.Runner, namespace, name string) (Snapshot, error) {
	return fetchRunLogs(ctx, r, namespace, name)
}

func fetchRunLogs(ctx context.Context, r rawRunner, namespace, name string) (Snapshot, error) {
	s := Snapshot{Name: name, Namespace: namespace}
	var firstErr error

	jobJSON, jobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "job", name, false, "job.batch", "jobs.batch")
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("get job %s/%s while resolving run logs placement: %w", namespace, name, err)
	}
	if jobFound {
		s.JobFound = true
		hydrateJob(&s, []byte(jobJSON))
	}

	rayJobJSON, rayJobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "rayjob", name, true, "rayjob.ray.io", "rayjobs.ray.io")
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("get RayJob %s/%s while resolving run logs placement: %w", namespace, name, err)
	}
	if rayJobFound {
		hydrateRayJob(&s, []byte(rayJobJSON))
	}

	workloads, err := fetchManagerCleanupWorkloads(ctx, r, namespace, name, s.JobUID, s.RayJob.UID)
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("list Kueue Workloads for %s/%s while resolving run logs placement: %w", namespace, name, err)
	}
	s.Workloads = workloads
	markRunLogsMultiKueueAdmissionCheckFallbacks(s.Workloads)

	if err := ctx.Err(); err != nil && firstErr == nil {
		firstErr = err
	}
	return s, firstErr
}

// Fetch populates a Snapshot for one job by name+namespace using the
// supplied kube.Runner. Soft-fails (returns partial Snapshot) when
// individual sub-queries fail — `tau run status` should always show what
// it can rather than abort on a missing CRD.
func Fetch(ctx context.Context, r *kube.Runner, namespace, name string) (Snapshot, error) {
	s := Snapshot{Name: name, Namespace: namespace}

	// Job: -o json so we get conditions + status counts.
	jobJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "job", name, "-o", "json"}, nil)
	if err == nil {
		s.JobFound = true
		hydrateJob(&s, []byte(jobJSON))
	}
	// Whether Job exists or not, still try RayJob + Workloads + Pods.
	rayJobJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "rayjob", name, "-o", "json"}, nil)
	if err == nil {
		hydrateRayJob(&s, []byte(rayJobJSON))
	}

	// Prefer the primary workload's immutable UID. The name label can match a
	// stale Job and RayJob simultaneously when both kinds reuse a run name.
	workloadSelectors := make([]string, 0, 2)
	rj := snapshotRayJob(s)
	if s.JobFound && s.JobUID != "" {
		workloadSelectors = append(workloadSelectors, "kueue.x-k8s.io/job-uid="+s.JobUID)
	} else if rj.Found && rj.UID != "" {
		workloadSelectors = append(workloadSelectors, "kueue.x-k8s.io/job-uid="+rj.UID)
	} else {
		workloadSelectors = append(workloadSelectors, workloadmeta.LabelJob+"="+name)
	}
	for _, selector := range workloadSelectors {
		wlJSON, queryErr := r.Raw(ctx, []string{"-n", namespace, "get", "workloads.kueue.x-k8s.io",
			"-l", selector, "-o", "json"}, nil)
		if queryErr == nil {
			s.Workloads = hydrateWorkloads([]byte(wlJSON))
			if len(s.Workloads) > 0 {
				break
			}
		}
	}
	hydrateAdmissionCheckControllers(ctx, r, s.Workloads)

	// A batch Job's standard selector is authoritative. Name-based Tau labels
	// and RayCluster selectors are used only when RayJob is the primary kind.
	selectors := []string{"job-name=" + name}
	if !s.JobFound {
		selectors = []string{workloadmeta.LabelJob + "=" + name}
		if rj.RayClusterName != "" {
			selectors = append(selectors, "ray.io/cluster="+rj.RayClusterName)
		}
	}
	for _, selector := range selectors {
		podJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "pods",
			"-l", selector, "-o", "json"}, nil)
		if err == nil {
			s.PodsObserved = true
			s.Pods = mergePods(s.Pods, hydratePods([]byte(podJSON)))
		}
	}

	s.ResourceClaims = fetchResourceClaims(ctx, r, namespace, uniquePodResourceClaims(s.Pods))
	s.Events = fetchEvents(ctx, r, namespace, eventObjects(s))

	return s, nil
}

// FetchManagerCleanup populates the subset of Snapshot needed by manager-side
// cleanup logic before delete. Unlike Fetch, every manager-cluster query is
// authoritative: exact object NotFound and missing RayJob CRDs are tolerated,
// but real kubectl/query failures are surfaced while still preserving any
// successfully hydrated partial snapshot.
func FetchManagerCleanup(ctx context.Context, r rawRunner, namespace, name string) (Snapshot, error) {
	s := Snapshot{Name: name, Namespace: namespace}
	var firstErr error

	jobJSON, jobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "job", name, false, "job.batch", "jobs.batch")
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if jobFound {
		s.JobFound = true
		hydrateJob(&s, []byte(jobJSON))
	}

	rayJobJSON, rayJobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "rayjob", name, true, "rayjob.ray.io", "rayjobs.ray.io")
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if rayJobFound {
		hydrateRayJob(&s, []byte(rayJobJSON))
	}

	workloads, err := fetchManagerCleanupWorkloads(ctx, r, namespace, name, s.JobUID, s.RayJob.UID)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	s.Workloads = workloads
	hydrateAdmissionCheckControllers(ctx, r, s.Workloads)

	if err := ctx.Err(); err != nil && firstErr == nil {
		firstErr = err
	}
	return s, firstErr
}

func fetchManagerCleanupObject(ctx context.Context, r rawRunner, namespace, resource, name string, allowUnknownResource bool, resourceKinds ...string) (string, bool, error) {
	out, err := r.Raw(ctx, []string{"-n", namespace, "get", resource, name, "-o", "json"}, nil)
	switch {
	case err == nil:
		return out, true, nil
	case allowUnknownResource && cleanupUnknownResourceError(err):
		return "", false, nil
	case cleanupExactObjectNotFound(err, name, resourceKinds...):
		return "", false, nil
	default:
		return "", false, err
	}
}

func fetchManagerCleanupWorkloads(ctx context.Context, r rawRunner, namespace, name, jobUID, rayJobUID string) ([]Workload, error) {
	var (
		workloads []Workload
		firstErr  error
	)
	for _, selector := range managerCleanupWorkloadSelectors(name, jobUID, rayJobUID) {
		selected, err := fetchManagerCleanupWorkloadsBySelector(ctx, r, namespace, selector)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		workloads = mergeManagerCleanupWorkloads(workloads, selected)
	}
	return workloads, firstErr
}

func fetchManagerCleanupWorkloadsBySelector(ctx context.Context, r rawRunner, namespace, selector string) ([]Workload, error) {
	out, err := r.Raw(ctx, []string{"-n", namespace, "get", "workloads.kueue.x-k8s.io", "-l", selector, "-o", "json"}, nil)
	if err != nil {
		return nil, err
	}
	return hydrateWorkloads([]byte(out)), nil
}

func managerCleanupWorkloadSelectors(name, jobUID, rayJobUID string) []string {
	selectors := make([]string, 0, 3)
	if name != "" {
		selectors = append(selectors, workloadmeta.LabelJob+"="+name)
	}
	if jobUID != "" {
		selectors = append(selectors, "kueue.x-k8s.io/job-uid="+jobUID)
	}
	if rayJobUID != "" {
		selectors = append(selectors, "kueue.x-k8s.io/job-uid="+rayJobUID)
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

func mergeManagerCleanupWorkloads(existing, incoming []Workload) []Workload {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]Workload, 0, len(existing)+len(incoming))
	for _, workload := range existing {
		out = append(out, workload)
		if workload.Name != "" {
			seen[workload.Name] = true
		}
	}
	for _, workload := range incoming {
		if workload.Name != "" && seen[workload.Name] {
			continue
		}
		out = append(out, workload)
		if workload.Name != "" {
			seen[workload.Name] = true
		}
	}
	return out
}

func cleanupUnknownResourceError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the server doesn't have a resource type") ||
		strings.Contains(msg, "server doesn't have a resource type") ||
		strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "resource type") && strings.Contains(msg, "not found")
}

func cleanupExactObjectNotFound(err error, name string, resourceKinds ...string) bool {
	if err == nil || name == "" {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	quotedName := `"` + strings.ToLower(name) + `"`
	for _, resourceKind := range resourceKinds {
		pattern := strings.ToLower(resourceKind) + " " + quotedName + " not found"
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
