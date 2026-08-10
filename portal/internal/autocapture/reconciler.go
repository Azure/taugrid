// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package autocapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
	"github.com/Azure/taugrid/portal/internal/expcapture"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

type Reconciler struct {
	Client Client
}

func (r Reconciler) Reconcile(ctx context.Context, store *expstore.Store, opts Options) (Result, error) {
	if r.Client == nil {
		return Result{}, fmt.Errorf("autocapture client is required")
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}
	jobs, err := r.Client.ListJobs(ctx, namespace)
	if err != nil {
		return Result{}, err
	}
	workloads, err := r.Client.ListWorkloads(ctx, namespace)
	if err != nil {
		return Result{}, err
	}
	pods, err := r.Client.ListPods(ctx, namespace)
	if err != nil {
		return Result{}, err
	}
	claims, err := r.Client.ListResourceClaims(ctx, namespace)
	if err != nil {
		return Result{}, err
	}
	events, err := r.Client.ListEvents(ctx, namespace)
	if err != nil {
		return Result{}, err
	}

	var result Result
	for _, job := range jobs {
		runID := runIDForJob(job)
		if runID == "" {
			continue
		}
		jobWorkloads := workloadsForJob(job, workloads)
		jobPods := podsForJob(job, pods)
		jobClaims := claimsForJob(job, jobPods, claims)
		jobEvents := eventsForJob(job, jobWorkloads, jobPods, jobClaims, events)
		snap := snapshotForJob(job, jobWorkloads, jobPods)
		record, err := expcapture.RunData(snap, status.CostProfile{}, status.ExperimentRunDataOptions{
			Project:       opts.Project,
			RunGroupID:    opts.RunGroupID,
			Owner:         firstNonEmpty(opts.Owner, "tau-controller"),
			Cluster:       opts.Cluster,
			CaptureSource: "controller-autocapture",
		})
		if err != nil {
			return Result{}, err
		}
		augmentRunContext(record.RunContext, jobWorkloads, jobClaims)
		enriched, err := store.EnrichRunData(ctx, expstore.EnrichRunDataOptions{
			Run:        record.Run,
			RunContext: record.RunContext,
			Tags:       record.Tags,
			Events:     captureEvents(runID, job, jobWorkloads, jobPods, jobClaims, jobEvents),
			Command:    "exp autocapture",
		})
		if err != nil {
			return Result{}, err
		}
		result.Runs++
		if enriched.CreatedRun {
			result.CreatedRuns++
		}
		if enriched.UpdatedRun {
			result.UpdatedRuns++
		}
		if enriched.CreatedRunContext {
			result.CreatedRunContexts++
		}
		if enriched.UpdatedRunContext {
			result.UpdatedRunContexts++
		}
		result.Events += enriched.Events
		result.Tags += enriched.Tags
		if enriched.Reused {
			result.Reused++
		}
	}
	return result, nil
}

func runIDForJob(job Job) string {
	return firstNonEmpty(
		job.Labels[experiment.LabelRunID],
		job.Labels[workloadmeta.LabelJob],
		strings.TrimPrefix(job.Name, "tau-"),
		job.Name,
	)
}

func snapshotForJob(job Job, workloads []Workload, pods []Pod) status.Snapshot {
	labels := copyLabelsWithRunID(job.Labels, job.Name)
	snap := status.Snapshot{
		Name:           job.Name,
		Namespace:      firstNonEmpty(job.Namespace, "default"),
		Labels:         labels,
		Annotations:    job.Annotations,
		JobFound:       true,
		JobSuspended:   job.Suspended,
		JobActive:      job.Active,
		JobSucceeded:   job.Succeeded,
		JobFailed:      job.Failed,
		JobCreatedAt:   job.CreatedAt,
		JobStartedAt:   job.StartedAt,
		JobFinishedAt:  job.FinishedAt,
		JobParallelism: job.Parallelism,
	}
	if snap.JobParallelism == 0 {
		snap.JobParallelism = 1
	}
	for _, cond := range job.Conditions {
		snap.JobConditions = append(snap.JobConditions, status.Condition{
			Type:    cond.Type,
			Status:  cond.Status,
			Reason:  cond.Reason,
			Message: cond.Message,
		})
	}
	for _, workload := range workloads {
		snap.Workloads = append(snap.Workloads, status.Workload{
			Name:       workload.Name,
			Queue:      workload.Queue,
			Admitted:   workload.Admitted,
			Phase:      workload.Phase,
			Reason:     workload.Reason,
			Preemption: workload.Preemption,
		})
	}
	for _, pod := range pods {
		snap.Pods = append(snap.Pods, status.Pod{
			Name:            pod.Name,
			UID:             pod.UID,
			Phase:           pod.Phase,
			Node:            pod.Node,
			Restarts:        pod.Restarts,
			Ready:           pod.Ready,
			StartedAt:       pod.StartedAt,
			ResourceClaims:  pod.ResourceClaims,
			ContainerReason: pod.ContainerReason,
			ExitCode:        pod.ExitCode,
			OOMKilled:       pod.OOMKilled,
		})
	}
	return snap
}

func augmentRunContext(runContext *expstore.RunContextRecord, workloads []Workload, claims []ResourceClaim) {
	if runContext == nil {
		return
	}
	var claimNames, claimNodes []string
	for _, claim := range claims {
		claimNames = appendNonEmpty(claimNames, claim.Name)
		claimNodes = appendNonEmpty(claimNodes, claim.NodeName)
		if runContext.GPUClass == "" {
			runContext.GPUClass = strings.TrimSpace(claim.DeviceClass)
		}
	}
	runContext.ResourceClaims = mergeListString(runContext.ResourceClaims, claimNames)
	runContext.NodeNames = mergeListString(runContext.NodeNames, claimNodes)
	for _, workload := range workloads {
		if runContext.ClusterQueue == "" {
			runContext.ClusterQueue = strings.TrimSpace(workload.ClusterQueue)
		}
	}
}

func workloadsForJob(job Job, workloads []Workload) []Workload {
	var out []Workload
	runID := runIDForJob(job)
	for _, workload := range workloads {
		labels := workload.Labels
		if labels[experiment.LabelRunID] == runID ||
			labels[workloadmeta.LabelJob] == runID ||
			labels[workloadmeta.LabelJob] == job.Name ||
			labels[labelKueueJobUID] == job.UID {
			out = append(out, workload)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func podsForJob(job Job, pods []Pod) []Pod {
	var out []Pod
	runID := runIDForJob(job)
	for _, pod := range pods {
		labels := pod.Labels
		if labels[experiment.LabelRunID] == runID ||
			labels[workloadmeta.LabelJob] == runID ||
			labels[workloadmeta.LabelJob] == job.Name ||
			labels["job-name"] == job.Name {
			out = append(out, pod)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func claimsForJob(job Job, pods []Pod, claims []ResourceClaim) []ResourceClaim {
	claimNames := map[string]bool{}
	for _, pod := range pods {
		for _, claim := range pod.ResourceClaims {
			claimNames[claim] = true
		}
	}
	runID := runIDForJob(job)
	var out []ResourceClaim
	for _, claim := range claims {
		if claim.Labels[experiment.LabelRunID] == runID ||
			claim.Labels[workloadmeta.LabelJob] == runID ||
			claim.Labels[workloadmeta.LabelJob] == job.Name ||
			claimNames[claim.Name] {
			out = append(out, claim)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func eventsForJob(job Job, workloads []Workload, pods []Pod, claims []ResourceClaim, events []KubernetesEvent) []KubernetesEvent {
	names := map[string]map[string]bool{
		"Job":           {job.Name: true},
		"Workload":      {},
		"Pod":           {},
		"ResourceClaim": {},
	}
	uids := map[string]bool{}
	if job.UID != "" {
		uids[job.UID] = true
	}
	for _, workload := range workloads {
		names["Workload"][workload.Name] = true
		if workload.UID != "" {
			uids[workload.UID] = true
		}
	}
	for _, pod := range pods {
		names["Pod"][pod.Name] = true
		if pod.UID != "" {
			uids[pod.UID] = true
		}
	}
	for _, claim := range claims {
		names["ResourceClaim"][claim.Name] = true
		if claim.UID != "" {
			uids[claim.UID] = true
		}
	}
	var out []KubernetesEvent
	for _, event := range events {
		if event.Regarding.UID != "" && uids[event.Regarding.UID] {
			out = append(out, event)
			continue
		}
		if byName := names[event.Regarding.Kind]; byName != nil && byName[event.Regarding.Name] {
			out = append(out, event)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time.Equal(out[j].Time) {
			return out[i].Name < out[j].Name
		}
		return out[i].Time.Before(out[j].Time)
	})
	return out
}

func captureEvents(runID string, job Job, workloads []Workload, pods []Pod, claims []ResourceClaim, k8sEvents []KubernetesEvent) []expstore.EventRecord {
	var events []expstore.EventRecord
	add := func(t time.Time, typ, source, severity, message string, payload any) {
		if t.IsZero() {
			return
		}
		payloadJSON := ""
		if payload != nil {
			raw, err := json.Marshal(payload)
			if err == nil {
				payloadJSON = string(raw)
			}
		}
		events = append(events, expstore.EventRecord{
			EventID:  eventID(runID, typ, source, t.UTC().Format(time.RFC3339), message),
			RunID:    runID,
			Time:     t.UTC().Format(time.RFC3339),
			Type:     typ,
			Source:   source,
			Severity: severity,
			Message:  message,
			Payload:  payloadJSON,
		})
	}
	add(job.CreatedAt, "submitted", "kubernetes/job", "info", fmt.Sprintf("Job %s/%s was submitted", job.Namespace, job.Name), map[string]string{"job": job.Name, "namespace": job.Namespace})
	if runningAt := runningTime(job, pods); !runningAt.IsZero() {
		add(runningAt, "running", "kubernetes/job", "info", fmt.Sprintf("Job %s/%s started running", job.Namespace, job.Name), map[string]string{"job": job.Name})
	}
	if failedAt := conditionTime(job, "Failed"); !failedAt.IsZero() || job.Failed > 0 {
		add(firstTime(failedAt, job.FinishedAt, job.StartedAt, job.CreatedAt), "failed", "kubernetes/job", "error", fmt.Sprintf("Job %s/%s failed", job.Namespace, job.Name), map[string]any{"job": job.Name, "failed": job.Failed})
	} else if succeededAt := conditionTime(job, "Complete"); !succeededAt.IsZero() || job.Succeeded > 0 {
		add(firstTime(succeededAt, job.FinishedAt, job.StartedAt, job.CreatedAt), "succeeded", "kubernetes/job", "info", fmt.Sprintf("Job %s/%s succeeded", job.Namespace, job.Name), map[string]any{"job": job.Name, "succeeded": job.Succeeded})
	}
	for _, workload := range workloads {
		if workload.Admitted {
			add(firstTime(workload.AdmittedAt, job.StartedAt, job.CreatedAt), "kueue-admitted", "kueue/workload", "info", fmt.Sprintf("Workload %s was admitted to queue %s", workload.Name, workload.Queue), map[string]string{"workload": workload.Name, "local_queue": workload.Queue, "cluster_queue": workload.ClusterQueue})
		}
		if strings.EqualFold(workload.Phase, "Finished") {
			add(firstTime(workload.FinishedAt, job.FinishedAt, job.StartedAt, job.CreatedAt), "kueue-finished", "kueue/workload", "info", fmt.Sprintf("Workload %s finished", workload.Name), map[string]string{"workload": workload.Name, "phase": workload.Phase})
		}
	}
	for _, pod := range pods {
		if pod.Node != "" {
			add(firstTime(pod.StartedAt, job.StartedAt, job.CreatedAt), "pod-scheduled", "kubernetes/pod", "info", fmt.Sprintf("Pod %s scheduled on %s", pod.Name, pod.Node), map[string]string{"pod": pod.Name, "node": pod.Node})
		}
		if strings.EqualFold(pod.Phase, "Failed") || pod.OOMKilled {
			add(firstTime(pod.StartedAt, job.StartedAt, job.CreatedAt), "pod-failed", "kubernetes/pod", "warning", fmt.Sprintf("Pod %s failed: %s", pod.Name, firstNonEmpty(pod.ContainerReason, pod.Phase)), map[string]any{"pod": pod.Name, "reason": pod.ContainerReason, "oom_killed": pod.OOMKilled})
		}
	}
	for _, claim := range claims {
		add(job.CreatedAt, "resource-claim-observed", "kubernetes/resourceclaim", "info", fmt.Sprintf("ResourceClaim %s observed", claim.Name), map[string]string{"resource_claim": claim.Name, "node": claim.NodeName, "device_class": claim.DeviceClass})
	}
	for _, event := range k8sEvents {
		t := firstTime(event.Time, job.CreatedAt)
		severity := strings.ToLower(event.Type)
		if severity == "" || severity == "normal" {
			severity = "info"
		}
		if severity == "warning" {
			severity = "warning"
		}
		source := firstNonEmpty(event.Source, "kubernetes/event")
		message := strings.TrimSpace(event.Message)
		if message == "" {
			message = firstNonEmpty(event.Reason, event.Action, "Kubernetes event")
		}
		add(t, "kubernetes-event", source, severity, message, map[string]any{
			"event":     event.Name,
			"reason":    event.Reason,
			"action":    event.Action,
			"type":      event.Type,
			"count":     event.Count,
			"regarding": event.Regarding,
		})
	}
	return dedupeEvents(events)
}

func conditionTime(job Job, typ string) time.Time {
	for _, cond := range job.Conditions {
		if cond.Type == typ && cond.Status == "True" {
			return cond.LastTransitionTime
		}
	}
	return time.Time{}
}

func runningTime(job Job, pods []Pod) time.Time {
	if !job.StartedAt.IsZero() {
		return job.StartedAt
	}
	for _, pod := range pods {
		if strings.EqualFold(pod.Phase, "Running") && !pod.StartedAt.IsZero() {
			return pod.StartedAt
		}
	}
	if job.Active > 0 {
		return job.CreatedAt
	}
	return time.Time{}
}

func eventID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "event-" + hex.EncodeToString(sum[:])[:16]
}

func dedupeEvents(events []expstore.EventRecord) []expstore.EventRecord {
	seen := map[string]bool{}
	out := make([]expstore.EventRecord, 0, len(events))
	for _, event := range events {
		if seen[event.EventID] {
			continue
		}
		seen[event.EventID] = true
		out = append(out, event)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time == out[j].Time {
			return out[i].EventID < out[j].EventID
		}
		return out[i].Time < out[j].Time
	})
	return out
}

func firstTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func appendNonEmpty(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	return append(values, value)
}

func mergeListString(existing string, values []string) string {
	for _, value := range strings.Split(existing, ",") {
		values = appendNonEmpty(values, value)
	}
	sort.Strings(values)
	out := values[:0]
	var last string
	for _, value := range values {
		if value == "" || value == last {
			continue
		}
		out = append(out, value)
		last = value
	}
	return strings.Join(out, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
