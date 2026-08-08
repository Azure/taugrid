// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/taugrid/tests/e2e/bundle"
)

const pollInterval = 2 * time.Second

// WorkloadGVR is the Kueue Workload GVR — used to query Workload objects via the dynamic client.
var WorkloadGVR = schema.GroupVersionResource{
	Group:    "kueue.x-k8s.io",
	Version:  "v1beta1",
	Resource: "workloads",
}

// LocalQueue GVR — used to query LocalQueue status via the dynamic client.
var localQueueGVR = schema.GroupVersionResource{
	Group:    "kueue.x-k8s.io",
	Version:  "v1beta1",
	Resource: "localqueues",
}

// RayJobGVR is the RayJob GVR — used to query RayJob status via the dynamic client.
var RayJobGVR = schema.GroupVersionResource{
	Group:    "ray.io",
	Version:  "v1",
	Resource: "rayjobs",
}

// RayClusterGVR is the RayCluster GVR — used to query RayCluster status via the dynamic client.
var RayClusterGVR = schema.GroupVersionResource{
	Group:    "ray.io",
	Version:  "v1",
	Resource: "rayclusters",
}

var mutatingWebhookConfigurationGVR = schema.GroupVersionResource{
	Group:    "admissionregistration.k8s.io",
	Version:  "v1",
	Resource: "mutatingwebhookconfigurations",
}

var validatingWebhookConfigurationGVR = schema.GroupVersionResource{
	Group:    "admissionregistration.k8s.io",
	Version:  "v1",
	Resource: "validatingwebhookconfigurations",
}

// WaitForDeploymentAvailable waits for a Deployment to have the Available condition.
func (tc *TestContext) WaitForDeploymentAvailable(ns, name string, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			tc.DumpDeployment(ns, name)
			tc.DumpPods(ns, "")
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for deployment %s/%s to be Available", ns, name)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
			deploy, err := tc.kubeClient.AppsV1().Deployments(ns).Get(tc.ctx, name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			for _, cond := range deploy.Status.Conditions {
				if cond.Type == "Available" && cond.Status == "True" {
					return nil
				}
			}
		}
	}
}

// WaitForDaemonSet waits for a DaemonSet to exist.
func (tc *TestContext) WaitForDaemonSet(ns, name string, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			tc.DumpDaemonSet(ns, name)
			tc.DumpPods(ns, "")
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for daemonset %s/%s to exist", ns, name)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
			_, err := tc.kubeClient.AppsV1().DaemonSets(ns).Get(tc.ctx, name, metav1.GetOptions{})
			if err == nil {
				return nil
			}
		}
	}
}

// WorkloadConditionResult holds the matched Workload and condition details.
type WorkloadConditionResult struct {
	Workload *unstructured.Unstructured
	Reason   string
	Message  string
}

// WaitForWorkloadAdmitted waits for the Kueue Workload associated with a Job to have Admitted=True.
func (tc *TestContext) WaitForWorkloadAdmitted(ns, jobName string, timeout time.Duration) (*WorkloadConditionResult, error) {
	return tc.waitForWorkloadCondition(ns, jobName, "Admitted", "True", timeout)
}

// WaitForWorkloadQuotaNotReserved waits for the Kueue Workload associated with a Job
// to have QuotaReserved=False, meaning Kueue evaluated the workload but could not
// reserve quota (e.g., insufficient resources or insufficient unused quota).
func (tc *TestContext) WaitForWorkloadQuotaNotReserved(ns, jobName string, timeout time.Duration) (*WorkloadConditionResult, error) {
	return tc.waitForWorkloadCondition(ns, jobName, "QuotaReserved", "False", timeout)
}

func (tc *TestContext) waitForWorkloadCondition(ns, jobName, condType, wantStatus string, timeout time.Duration) (result *WorkloadConditionResult, err error) {
	defer func() {
		if err != nil {
			tc.DumpPods(ns, fmt.Sprintf("job-name=%s", jobName))
			tc.DumpJobs(ns)
			tc.DumpWorkloads(ns)
			tc.DumpLocalQueues(ns)
			tc.DumpEvents(ns)
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastReason, lastStatus, lastMessage string
	for {
		select {
		case <-deadline:
			if lastReason == "" && lastStatus == "" {
				return nil, fmt.Errorf("timed out waiting for workload of job %s/%s to have %s=%s (condition %s never observed)",
					ns, jobName, condType, wantStatus, condType)
			}
			return nil, fmt.Errorf("timed out waiting for workload of job %s/%s to have %s=%s\n  last seen: %s=%s (Reason=%s, Message=%s)",
				ns, jobName, condType, wantStatus, condType, lastStatus, lastReason, lastMessage)
		case <-tc.ctx.Done():
			return nil, tc.ctx.Err()
		case <-ticker.C:
			wl, err := tc.findWorkloadForJob(ns, jobName)
			if err != nil {
				continue
			}
			status, reason, message, found := getWorkloadCondition(wl, condType)
			if found {
				lastStatus = status
				lastReason = reason
				lastMessage = message
				if status == wantStatus {
					return &WorkloadConditionResult{
						Workload: wl.DeepCopy(),
						Reason:   reason,
						Message:  message,
					}, nil
				}
			}
		}
	}
}

// findWorkloadForJob finds the Kueue Workload for a given Job by matching the job-uid label.
func (tc *TestContext) findWorkloadForJob(ns, jobName string) (*unstructured.Unstructured, error) {
	job, err := tc.kubeClient.BatchV1().Jobs(ns).Get(tc.ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	workloads, err := tc.dynamicClient.Resource(WorkloadGVR).Namespace(ns).List(tc.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("kueue.x-k8s.io/job-uid=%s", job.UID),
	})
	if err != nil {
		return nil, err
	}
	if len(workloads.Items) == 0 {
		return nil, fmt.Errorf("no workload found for job %s/%s", ns, jobName)
	}

	return &workloads.Items[0], nil
}

// WaitForWorkloadAdmittedByRayJob waits for the Kueue Workload owned by the given
// RayJob to have Admitted=True. It fetches the RayJob to obtain its UID, then
// matches Workloads by ownerReference (kind=RayJob, name, uid). This is more
// deterministic than prefix-matching, which could match stale workloads.
func (tc *TestContext) WaitForWorkloadAdmittedByRayJob(ns, rayJobName string, timeout time.Duration) (result *WorkloadConditionResult, err error) {
	defer func() {
		if err != nil {
			if managerWorkloadOnly() {
				tc.DumpWorkloads(ns)
				return
			}
			tc.DumpPods(ns, "")
			tc.DumpJobs(ns)
			tc.DumpWorkloads(ns)
			tc.DumpLocalQueues(ns)
			tc.DumpEvents(ns)
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastErr error
	var lastReason, lastStatus, lastMessage, lastWorkload string
	for {
		select {
		case <-deadline:
			msg := fmt.Sprintf("timed out waiting for workload owned by rayjob %s/%s to have Admitted=True", ns, rayJobName)
			if lastWorkload == "" {
				msg += " (no matching workload observed)"
			} else {
				msg += fmt.Sprintf(" (last workload: %s", lastWorkload)
				if lastStatus != "" {
					msg += fmt.Sprintf(", last Admitted=%s", lastStatus)
				}
				if lastReason != "" {
					msg += fmt.Sprintf(", reason=%s", lastReason)
				}
				if lastMessage != "" {
					msg += fmt.Sprintf(", message=%s", lastMessage)
				}
				msg += ")"
			}
			if lastErr != nil {
				msg += fmt.Sprintf("; last poll error: %v", lastErr)
			}
			return nil, fmt.Errorf("%s", msg)
		case <-tc.ctx.Done():
			return nil, tc.ctx.Err()
		case <-ticker.C:
			// Fetch the RayJob to get its UID (may not exist yet on first polls).
			rj, err := tc.dynamicClient.Resource(RayJobGVR).Namespace(ns).Get(tc.ctx, rayJobName, metav1.GetOptions{})
			if err != nil {
				lastErr = err
				continue
			}
			lastErr = nil
			rjUID := string(rj.GetUID())

			workloads, err := tc.dynamicClient.Resource(WorkloadGVR).Namespace(ns).List(tc.ctx, metav1.ListOptions{})
			if err != nil {
				lastErr = err
				continue
			}
			for i := range workloads.Items {
				wl := &workloads.Items[i]
				if !hasOwnerRef(wl, "RayJob", rayJobName, rjUID) {
					continue
				}
				lastWorkload = wl.GetName()
				status, reason, message, found := getWorkloadCondition(wl, "Admitted")
				if !found {
					continue
				}
				lastStatus = status
				lastReason = reason
				lastMessage = message
				if status == "True" {
					return &WorkloadConditionResult{
						Workload: wl.DeepCopy(),
						Reason:   reason,
						Message:  message,
					}, nil
				}
			}
		}
	}
}

// hasOwnerRef returns true if the object has an ownerReference matching the given kind, name, and UID.
func hasOwnerRef(obj *unstructured.Unstructured, kind, name, uid string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == kind && ref.Name == name && string(ref.UID) == uid {
			return true
		}
	}
	return false
}

// WaitForRayJobStatus polls a RayJob's .status.jobStatus field until it matches
// targetStatus. Terminal job states ("FAILED", "STOPPED") and terminal deployment
// failures cause the helper to return immediately rather than waiting for the timeout.
func (tc *TestContext) WaitForRayJobStatus(ns, name, targetStatus string, timeout time.Duration) (err error) {
	defer func() {
		if err == nil {
			return
		}
		if managerWorkloadOnly() {
			tc.DumpWorkloads(ns)
			return
		}
		tc.DumpPods(ns, fmt.Sprintf("batch.kubernetes.io/job-name=%s", name))
		tc.DumpPods(ns, "ray.io/node-type=head")
		tc.DumpPods(ns, "ray.io/node-type=worker")
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastJobStatus, lastDeploymentStatus string
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for rayjob %s/%s to reach status %q (last seen: jobStatus=%q jobDeploymentStatus=%q)",
				ns, name, targetStatus, lastJobStatus, lastDeploymentStatus)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
			obj, err := tc.dynamicClient.Resource(RayJobGVR).Namespace(ns).Get(tc.ctx, name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			jobStatus, _, _ := unstructured.NestedString(obj.Object, "status", "jobStatus")
			deploymentStatus, _, _ := unstructured.NestedString(obj.Object, "status", "jobDeploymentStatus")
			lastJobStatus = jobStatus
			lastDeploymentStatus = deploymentStatus
			if jobStatus == targetStatus {
				return nil
			}
			if terminal, reason := rayJobTerminalFailure(jobStatus, deploymentStatus); terminal {
				return fmt.Errorf("rayjob %s/%s reached terminal state %s (wanted %q; jobStatus=%q jobDeploymentStatus=%q)",
					ns, name, reason, targetStatus, jobStatus, deploymentStatus)
			}
		}
	}
}

// WaitForRayJobDeleted waits for one named RayJob to disappear without listing
// pods or other namespace resources. Manager-routed tests use this because their
// workload identity is intentionally limited to manager-visible objects.
func (tc *TestContext) WaitForRayJobDeleted(ns, name string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		_, err := tc.dynamicClient.Resource(RayJobGVR).Namespace(ns).Get(tc.ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			lastErr = err
		default:
			lastErr = nil
		}

		select {
		case <-deadline.C:
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for rayjob %s/%s to be deleted; last poll error: %w", ns, name, lastErr)
			}
			return fmt.Errorf("timed out waiting for rayjob %s/%s to be deleted", ns, name)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
		}
	}
}

func rayJobTerminalFailure(jobStatus, deploymentStatus string) (bool, string) {
	switch jobStatus {
	case "FAILED", "STOPPED":
		return true, "jobStatus=" + jobStatus
	}
	if deploymentStatus == "Failed" {
		return true, "jobDeploymentStatus=Failed"
	}
	return false, ""
}

// GetPodLogsByLabel returns the logs of the most recently created pod matching the given
// label selector. Reads at most the last 500 lines. Useful for asserting on submitter/job
// pod output.
func (tc *TestContext) GetPodLogsByLabel(ns, labelSelector string) (string, error) {
	return tc.getPodLogsByLabelTail(ns, labelSelector, int64Ptr(500))
}

// GetFullPodLogsByLabel returns the complete logs (no tail limit) of the most recently
// created pod matching the given label selector. Use this when assertions must span the
// entire run — e.g. an early NCCL init line plus a late checkpoint/success sentinel that
// would not coexist in a tailed window of verbose (NCCL_DEBUG=INFO) submitter output.
func (tc *TestContext) GetFullPodLogsByLabel(ns, labelSelector string) (string, error) {
	return tc.getPodLogsByLabelTail(ns, labelSelector, nil)
}

func (tc *TestContext) getPodLogsByLabelTail(ns, labelSelector string, tailLines *int64) (string, error) {
	pods, err := tc.kubeClient.CoreV1().Pods(ns).List(tc.ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("listing pods with selector %q in %s: %w", labelSelector, ns, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found with selector %q in %s", labelSelector, ns)
	}
	// Pick the most recently created pod to avoid reading logs from a stale retry.
	latest := pods.Items[0]
	for _, p := range pods.Items[1:] {
		if p.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = p
		}
	}
	logOpts := &corev1.PodLogOptions{TailLines: tailLines}
	logs, err := tc.kubeClient.CoreV1().Pods(ns).GetLogs(latest.Name, logOpts).Do(tc.ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("getting logs from pod %s/%s: %w", ns, latest.Name, err)
	}
	return string(logs), nil
}

// WaitForPodLogsByLabelContaining polls the most recent pod matching labelSelector until
// its logs contain all expected substrings. RayJob submitter pods can report SUCCEEDED
// before forwarded worker stdout has fully appeared in the Kubernetes pod log stream.
// Reads a tailed window (last 500 lines); use WaitForFullPodLogsByLabelContaining when the
// expected substrings span the whole run.
func (tc *TestContext) WaitForPodLogsByLabelContaining(ns, labelSelector string, expected []string, timeout time.Duration) (lastLogs string, err error) {
	return tc.waitForPodLogsByLabelContaining(ns, labelSelector, expected, timeout, false)
}

// WaitForFullPodLogsByLabelContaining behaves like WaitForPodLogsByLabelContaining but reads
// the complete pod log (no tail limit) on each poll. Use this when assertions must match
// substrings emitted far apart in time (e.g. an early NCCL NET/IB init line plus a late
// checkpoint sentinel) that would never coexist in a tailed window of verbose output.
func (tc *TestContext) WaitForFullPodLogsByLabelContaining(ns, labelSelector string, expected []string, timeout time.Duration) (lastLogs string, err error) {
	return tc.waitForPodLogsByLabelContaining(ns, labelSelector, expected, timeout, true)
}

func (tc *TestContext) waitForPodLogsByLabelContaining(ns, labelSelector string, expected []string, timeout time.Duration, full bool) (lastLogs string, err error) {
	defer func() {
		if err != nil {
			tc.DumpPods(ns, labelSelector)
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	getLogs := tc.GetPodLogsByLabel
	if full {
		getLogs = tc.GetFullPodLogsByLabel
	}

	missing := expected
	var lastErr error
	for {
		select {
		case <-deadline:
			if lastErr != nil && lastLogs == "" {
				return "", fmt.Errorf("timed out waiting for logs from pod selector %q in %s to contain %v: last error: %w",
					labelSelector, ns, expected, lastErr)
			}
			return lastLogs, fmt.Errorf("timed out waiting for logs from pod selector %q in %s to contain %v; missing %v; last logs:\n%s",
				labelSelector, ns, expected, missing, lastLogs)
		case <-tc.ctx.Done():
			return lastLogs, tc.ctx.Err()
		case <-ticker.C:
			logs, err := getLogs(ns, labelSelector)
			if err != nil {
				lastErr = err
				continue
			}
			lastLogs = logs
			missing = nil
			for _, want := range expected {
				if !strings.Contains(logs, want) {
					missing = append(missing, want)
				}
			}
			if len(missing) == 0 {
				return logs, nil
			}
		}
	}
}

// getWorkloadCondition extracts a condition from an unstructured Workload.
// Returns (status, reason, message, found).
func getWorkloadCondition(wl *unstructured.Unstructured, condType string) (string, string, string, bool) {
	conditions, found, err := unstructured.NestedSlice(wl.Object, "status", "conditions")
	if err != nil || !found {
		return "", "", "", false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == condType {
			status, _ := cond["status"].(string)
			reason, _ := cond["reason"].(string)
			message, _ := cond["message"].(string)
			return status, reason, message, true
		}
	}
	return "", "", "", false
}

// LocalQueueCounts holds the pending and admitted workload counts for a LocalQueue.
type LocalQueueCounts struct {
	Pending  int
	Admitted int
}

// GetLocalQueueCounts returns the pending and admitted workload counts for a LocalQueue.
// On error, dumps all LocalQueues in the namespace for diagnostic context.
func (tc *TestContext) GetLocalQueueCounts(ns, name string) (_ LocalQueueCounts, err error) {
	defer func() {
		if err != nil {
			tc.DumpLocalQueues(ns)
			tc.DumpEvents(ns)
		}
	}()

	lq, err := tc.dynamicClient.Resource(localQueueGVR).Namespace(ns).Get(tc.ctx, name, metav1.GetOptions{})
	if err != nil {
		return LocalQueueCounts{}, fmt.Errorf("getting localqueue %s/%s: %w", ns, name, err)
	}

	pending, _, _ := unstructured.NestedInt64(lq.Object, "status", "pendingWorkloads")
	admitted, _, _ := unstructured.NestedInt64(lq.Object, "status", "admittedWorkloads")

	return LocalQueueCounts{
		Pending:  int(pending),
		Admitted: int(admitted),
	}, nil
}

// WaitForLocalQueueCounts polls until the LocalQueue's pending and admitted counts match
// the expected values. This avoids a race where the Workload status updates before
// the LocalQueue reconciler bumps the counters.
//
// Unlike GetLocalQueueCounts, this function queries the dynamic client directly to
// avoid triggering diagnostic dumps on every transient poll failure.
func (tc *TestContext) WaitForLocalQueueCounts(ns, name string, wantPending, wantAdmitted int, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			tc.DumpLocalQueues(ns)
			tc.DumpEvents(ns)
		}
	}()

	var lastCounts LocalQueueCounts
	var lastErr error
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			msg := fmt.Sprintf("timed out waiting for localqueue %s/%s counts (pending=%d, admitted=%d); last seen (pending=%d, admitted=%d)",
				ns, name, wantPending, wantAdmitted, lastCounts.Pending, lastCounts.Admitted)
			if lastErr != nil {
				msg += fmt.Sprintf("; last poll error: %v", lastErr)
			}
			return fmt.Errorf("%s", msg)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-tick.C:
			lq, err := tc.dynamicClient.Resource(localQueueGVR).Namespace(ns).Get(tc.ctx, name, metav1.GetOptions{})
			if err != nil {
				lastErr = err
				continue
			}
			pending, _, _ := unstructured.NestedInt64(lq.Object, "status", "pendingWorkloads")
			admitted, _, _ := unstructured.NestedInt64(lq.Object, "status", "admittedWorkloads")
			lastCounts = LocalQueueCounts{Pending: int(pending), Admitted: int(admitted)}
			if lastCounts.Pending == wantPending && lastCounts.Admitted == wantAdmitted {
				return nil
			}
		}
	}
}

// WaitForRunningJobPods waits until a Job has exactly wantCount pods in Running phase.
func (tc *TestContext) WaitForRunningJobPods(ns, jobName string, wantCount int, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			tc.DumpPods(ns, fmt.Sprintf("job-name=%s", jobName))
			tc.DumpJobs(ns)
			tc.DumpWorkloads(ns)
			tc.DumpLocalQueues(ns)
			tc.DumpEvents(ns)
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastCount int
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for job %s/%s to have %d running pods (last seen: %d)", ns, jobName, wantCount, lastCount)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
			pods, err := tc.kubeClient.CoreV1().Pods(ns).List(tc.ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("job-name=%s", jobName),
			})
			if err != nil {
				continue
			}
			lastCount = 0
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					lastCount++
				}
			}
			if lastCount == wantCount {
				return nil
			}
		}
	}
}

// WaitForRunningPodsByLabel waits until exactly wantCount pods matching a label selector
// are in Running phase with all containers Ready.
func (tc *TestContext) WaitForRunningPodsByLabel(ns, labelSelector string, wantCount int, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			tc.DumpPods(ns, labelSelector)
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastCount int
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for %d running+ready pods with selector %q in %s (last seen: %d)",
				wantCount, labelSelector, ns, lastCount)
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
			pods, err := tc.kubeClient.CoreV1().Pods(ns).List(tc.ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				continue
			}
			lastCount = 0
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning && isPodReady(&p) {
					lastCount++
				}
			}
			if lastCount == wantCount {
				return nil
			}
		}
	}
}

// WaitForNoPods waits until a namespace has no remaining pods, including
// terminating pods that may still hold scarce resources such as a single GPU.
func (tc *TestContext) WaitForNoPods(ns string, timeout time.Duration) (err error) {
	defer func() {
		if err != nil {
			tc.DumpPods(ns, "")
		}
	}()

	deadline := time.After(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastSeen []string
	for {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for namespace %s to have no pods (last seen: %s)",
				ns, strings.Join(lastSeen, ", "))
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
			pods, err := tc.kubeClient.CoreV1().Pods(ns).List(tc.ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			lastSeen = lastSeen[:0]
			for _, p := range pods.Items {
				state := string(p.Status.Phase)
				if p.DeletionTimestamp != nil {
					state += "/Terminating"
				}
				lastSeen = append(lastSeen, fmt.Sprintf("%s(%s)", p.Name, state))
			}
			if len(lastSeen) == 0 {
				return nil
			}
		}
	}
}

// WaitForNoPodsByLabel waits until no pods matching a non-empty label selector
// remain. It is safe for shared namespaces because unrelated pods are ignored.
func (tc *TestContext) WaitForNoPodsByLabel(ns, labelSelector string, timeout time.Duration) (err error) {
	labelSelector = strings.TrimSpace(labelSelector)
	if labelSelector == "" {
		return fmt.Errorf("label selector is required when waiting for scoped pod cleanup in %s", ns)
	}
	defer func() {
		if err != nil {
			tc.DumpPods(ns, labelSelector)
		}
	}()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastSeen []string
	for {
		pods, listErr := tc.kubeClient.CoreV1().Pods(ns).List(tc.ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if listErr == nil {
			lastSeen = lastSeen[:0]
			for _, p := range pods.Items {
				state := string(p.Status.Phase)
				if p.DeletionTimestamp != nil {
					state += "/Terminating"
				}
				lastSeen = append(lastSeen, fmt.Sprintf("%s(%s)", p.Name, state))
			}
			if len(lastSeen) == 0 {
				return nil
			}
		}

		select {
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for pods matching %q in namespace %s to be deleted (last seen: %s)",
				labelSelector, ns, strings.Join(lastSeen, ", "))
		case <-tc.ctx.Done():
			return tc.ctx.Err()
		case <-ticker.C:
		}
	}
}

// DumpPods writes detailed pod diagnostics for pods matching a label selector
// to the test's artifact bundle (tees to t.Logf). Best-effort: never fails the test.
func (tc *TestContext) DumpPods(ns, labelSelector string) {
	tc.Helper()
	safeName := sanitizeFileName(labelSelector)
	if safeName == "" {
		safeName = "all"
	}
	fileName := fmt.Sprintf("pods-%s-%s.txt", ns, safeName)
	w := tc.bundle.WriterFor(fileName)
	writePodDiagnostics(w, tc.ctx, tc.kubeClient, ns, labelSelector)
	writeEventDiagnostics(w, tc.ctx, tc.kubeClient, ns)
}

// DumpEvents writes namespace events to their own bundle file.
// Best-effort: never fails the test.
func (tc *TestContext) DumpEvents(ns string) {
	tc.Helper()
	fileName := fmt.Sprintf("events-%s.txt", ns)
	w := tc.bundle.WriterFor(fileName)
	writeEventDiagnostics(w, tc.ctx, tc.kubeClient, ns)
}

// DumpDeployment writes a detailed description of a Deployment to the bundle,
// similar to `kubectl describe deployment`. Best-effort: never fails the test.
func (tc *TestContext) DumpDeployment(ns, name string) {
	tc.Helper()
	fileName := fmt.Sprintf("deployment-%s-%s.txt", ns, name)
	w := tc.bundle.WriterFor(fileName)
	writeDeploymentDiagnostics(w, tc.ctx, tc.kubeClient, ns, name)
}

// DumpDaemonSet writes a detailed description of a DaemonSet to the bundle,
// similar to `kubectl describe daemonset`. Best-effort: never fails the test.
func (tc *TestContext) DumpDaemonSet(ns, name string) {
	tc.Helper()
	fileName := fmt.Sprintf("daemonset-%s-%s.txt", ns, name)
	w := tc.bundle.WriterFor(fileName)
	writeDaemonSetDiagnostics(w, tc.ctx, tc.kubeClient, ns, name)
}

// DumpJobs writes a summary of all Jobs in a namespace to the bundle, including
// labels, completions, parallelism, and status (active/succeeded/failed).
func (tc *TestContext) DumpJobs(ns string) {
	tc.Helper()
	fileName := fmt.Sprintf("jobs-%s.txt", ns)
	w := tc.bundle.WriterFor(fileName)

	jobs, err := tc.kubeClient.BatchV1().Jobs(ns).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to list jobs in %s: %v\n", ns, err)
		return
	}
	if len(jobs.Items) == 0 {
		fmt.Fprintf(w, "No jobs found in %s\n", ns)
		return
	}
	for _, j := range jobs.Items {
		labels := formatLabels(j.Labels)
		fmt.Fprintf(w, "Job %s/%s  labels=[%s]\n", j.Namespace, j.Name, labels)
		fmt.Fprintf(w, "  Completions=%s Parallelism=%s Suspend=%v\n",
			fmtInt32Ptr(j.Spec.Completions), fmtInt32Ptr(j.Spec.Parallelism), ptrBool(j.Spec.Suspend))
		fmt.Fprintf(w, "  Active=%d Succeeded=%d Failed=%d\n",
			j.Status.Active, j.Status.Succeeded, j.Status.Failed)
		for _, cond := range j.Status.Conditions {
			fmt.Fprintf(w, "  Condition %s=%s (Reason=%s / Message=%s)\n", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
	}
}

// DumpWorkloads writes a summary of all Kueue Workloads in a namespace to the bundle,
// including labels, ownership, admission metadata, and all status conditions.
func (tc *TestContext) DumpWorkloads(ns string) {
	tc.Helper()
	fileName := fmt.Sprintf("workloads-%s.txt", ns)
	w := tc.bundle.WriterFor(fileName)

	workloads, err := tc.dynamicClient.Resource(WorkloadGVR).Namespace(ns).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to list workloads in %s: %v\n", ns, err)
		return
	}
	if len(workloads.Items) == 0 {
		fmt.Fprintf(w, "No workloads found in %s\n", ns)
		return
	}
	for _, wl := range workloads.Items {
		labels := formatLabels(wl.GetLabels())
		fmt.Fprintf(w, "Workload %s/%s  labels=[%s]\n", wl.GetNamespace(), wl.GetName(), labels)
		fmt.Fprintf(w, "  UID=%s ResourceVersion=%s CreatedAt=%s\n",
			wl.GetUID(), wl.GetResourceVersion(), formatMetaTime(wl.GetCreationTimestamp()))
		fmt.Fprintf(w, "  OwnerReferences=[%s]\n", formatOwnerRefs(wl.GetOwnerReferences()))
		queueName, _, _ := unstructured.NestedString(wl.Object, "spec", "queueName")
		priorityClassName, _, _ := unstructured.NestedString(wl.Object, "spec", "priorityClassName")
		clusterQueue, _, _ := unstructured.NestedString(wl.Object, "status", "admission", "clusterQueue")
		fmt.Fprintf(w, "  Spec queueName=%q priorityClassName=%q\n", queueName, priorityClassName)
		fmt.Fprintf(w, "  Admission clusterQueue=%q\n", clusterQueue)
		podSetAssignments, _, _ := unstructured.NestedSlice(wl.Object, "status", "admission", "podSetAssignments")
		for _, raw := range podSetAssignments {
			assignment, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			fmt.Fprintf(w, "  PodSetAssignment name=%v flavors=%v count=%v resourceUsage=%v\n",
				assignment["name"], assignment["flavors"], assignment["count"], assignment["resourceUsage"])
		}
		conditions, _, _ := unstructured.NestedSlice(wl.Object, "status", "conditions")
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			fmt.Fprintf(w, "  Condition %s=%s (Reason=%s / LastTransitionTime=%s / ObservedGeneration=%v / Message=%s)\n",
				cond["type"], cond["status"], cond["reason"], cond["lastTransitionTime"], cond["observedGeneration"], cond["message"])
		}
	}
}

// DumpLocalQueues writes a summary of all LocalQueues in a namespace to the bundle,
// including admitted, pending, and reserving workload counts and conditions.
func (tc *TestContext) DumpLocalQueues(ns string) {
	tc.Helper()
	fileName := fmt.Sprintf("localqueues-%s.txt", ns)
	w := tc.bundle.WriterFor(fileName)

	queues, err := tc.dynamicClient.Resource(localQueueGVR).Namespace(ns).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to list localqueues in %s: %v\n", ns, err)
		return
	}
	if len(queues.Items) == 0 {
		fmt.Fprintf(w, "No localqueues found in %s\n", ns)
		return
	}
	for _, lq := range queues.Items {
		labels := formatLabels(lq.GetLabels())
		fmt.Fprintf(w, "LocalQueue %s/%s  labels=[%s]\n", lq.GetNamespace(), lq.GetName(), labels)
		pending, _, _ := unstructured.NestedInt64(lq.Object, "status", "pendingWorkloads")
		admitted, _, _ := unstructured.NestedInt64(lq.Object, "status", "admittedWorkloads")
		reserving, _, _ := unstructured.NestedInt64(lq.Object, "status", "reservingWorkloads")
		fmt.Fprintf(w, "  Admitted=%d Pending=%d Reserving=%d\n", admitted, pending, reserving)
		conditions, _, _ := unstructured.NestedSlice(lq.Object, "status", "conditions")
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			fmt.Fprintf(w, "  Condition %s=%s (Reason=%s, Message=%s)\n",
				cond["type"], cond["status"], cond["reason"], cond["message"])
		}
	}
}

// DumpKueueAdmissionDiagnostics captures Kueue webhook/controller state that
// explains admission flakes without requiring workflow-level kubectl plumbing.
func (tc *TestContext) DumpKueueAdmissionDiagnostics(ns string) {
	tc.Helper()
	fileName := fmt.Sprintf("kueue-admission-%s.txt", ns)
	w := tc.bundle.WriterFor(fileName)

	fmt.Fprintf(w, "=== Kueue controller deployment ===\n")
	writeDeploymentDiagnostics(w, tc.ctx, tc.kubeClient, ns, "kueue-controller-manager")
	fmt.Fprintf(w, "\n=== Kueue controller pods ===\n")
	writePodDiagnostics(w, tc.ctx, tc.kubeClient, ns, "control-plane=controller-manager")
	fmt.Fprintf(w, "\n=== Kueue webhook service ===\n")
	writeServiceDiagnostics(w, tc.ctx, tc.kubeClient, ns, "kueue-webhook-service")
	fmt.Fprintf(w, "\n=== Kueue webhook endpoints ===\n")
	writeEndpointDiagnostics(w, tc.ctx, tc.kubeClient, ns, "kueue-webhook-service")
	fmt.Fprintf(w, "\n=== Kueue webhook EndpointSlices ===\n")
	writeEndpointSliceDiagnostics(w, tc.ctx, tc.kubeClient, ns, "kueue-webhook-service")
	fmt.Fprintf(w, "\n=== Kueue webhook configuration: mworkload.kb.io ===\n")
	writeWebhookConfigurationDiagnostics(w, tc.ctx, tc.dynamicClient, mutatingWebhookConfigurationGVR, "mworkload.kb.io")
	fmt.Fprintf(w, "\n=== Kueue webhook configuration: vrayjob.kb.io ===\n")
	writeWebhookConfigurationDiagnostics(w, tc.ctx, tc.dynamicClient, validatingWebhookConfigurationGVR, "vrayjob.kb.io")
	fmt.Fprintf(w, "\n=== Kueue system events ===\n")
	writeEventDiagnostics(w, tc.ctx, tc.kubeClient, ns)
}

// formatLabels formats a map as "key=value,key2=value2" for display.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func formatOwnerRefs(refs []metav1.OwnerReference) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		controller := ""
		if ref.Controller != nil {
			controller = fmt.Sprintf(",controller=%t", *ref.Controller)
		}
		parts = append(parts, fmt.Sprintf("%s/%s uid=%s%s", ref.Kind, ref.Name, ref.UID, controller))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func formatMetaTime(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// fmtInt32Ptr formats an *int32 as a string, returning "<nil>" if nil.
func fmtInt32Ptr(p *int32) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *p)
}

// --- Shared diagnostic writers (used by both TestContext methods and DumpDeploymentDiagnostics) ---

// writeDeploymentDiagnostics writes deployment details to w. Same format as DumpDeployment.
func writeDeploymentDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns, name string) {
	deploy, err := kubeClient.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to get deployment %s/%s: %v\n", ns, name, err)
		return
	}
	fmt.Fprintf(w, "Deployment %s/%s\n", deploy.Namespace, deploy.Name)
	fmt.Fprintf(w, "  Replicas: %d desired | %d updated | %d total | %d available | %d unavailable\n",
		ptrInt32(deploy.Spec.Replicas), deploy.Status.UpdatedReplicas, deploy.Status.Replicas,
		deploy.Status.AvailableReplicas, deploy.Status.UnavailableReplicas)
	fmt.Fprintf(w, "  Strategy: %s\n", deploy.Spec.Strategy.Type)
	for _, cond := range deploy.Status.Conditions {
		fmt.Fprintf(w, "  Condition %s=%s (Reason=%s, Message=%s)\n", cond.Type, cond.Status, cond.Reason, cond.Message)
	}
}

// writeDaemonSetDiagnostics writes daemonset details to w. Same format as DumpDaemonSet.
func writeDaemonSetDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns, name string) {
	ds, err := kubeClient.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to get daemonset %s/%s: %v\n", ns, name, err)
		return
	}
	fmt.Fprintf(w, "DaemonSet %s/%s\n", ds.Namespace, ds.Name)
	fmt.Fprintf(w, "  Desired: %d | Current: %d | Ready: %d | Up-to-date: %d | Available: %d\n",
		ds.Status.DesiredNumberScheduled, ds.Status.CurrentNumberScheduled,
		ds.Status.NumberReady, ds.Status.UpdatedNumberScheduled, ds.Status.NumberAvailable)
	for _, cond := range ds.Status.Conditions {
		fmt.Fprintf(w, "  Condition %s=%s (Reason=%s, Message=%s)\n", cond.Type, cond.Status, cond.Reason, cond.Message)
	}
}

func writeServiceDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns, name string) {
	svc, err := kubeClient.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to get service %s/%s: %v\n", ns, name, err)
		return
	}
	fmt.Fprintf(w, "Service %s/%s type=%s clusterIP=%s labels=[%s] selector=[%s]\n",
		svc.Namespace, svc.Name, svc.Spec.Type, svc.Spec.ClusterIP, formatLabels(svc.Labels), formatLabels(svc.Spec.Selector))
	for _, port := range svc.Spec.Ports {
		fmt.Fprintf(w, "  Port name=%s port=%d targetPort=%s protocol=%s\n",
			port.Name, port.Port, port.TargetPort.String(), port.Protocol)
	}
}

func writeEndpointDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns, name string) {
	eps, err := kubeClient.CoreV1().Endpoints(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to get endpoints %s/%s: %v\n", ns, name, err)
		return
	}
	if len(eps.Subsets) == 0 {
		fmt.Fprintf(w, "Endpoints %s/%s has no subsets\n", ns, name)
		return
	}
	for i, subset := range eps.Subsets {
		fmt.Fprintf(w, "Subset %d\n", i)
		for _, port := range subset.Ports {
			fmt.Fprintf(w, "  Port name=%s port=%d protocol=%s\n", port.Name, port.Port, port.Protocol)
		}
		for _, addr := range subset.Addresses {
			targetKind, targetName := endpointTarget(addr.TargetRef)
			fmt.Fprintf(w, "  Ready address=%s node=%s target=%s/%s\n",
				addr.IP, ptrString(addr.NodeName), targetKind, targetName)
		}
		for _, addr := range subset.NotReadyAddresses {
			targetKind, targetName := endpointTarget(addr.TargetRef)
			fmt.Fprintf(w, "  NotReady address=%s node=%s target=%s/%s\n",
				addr.IP, ptrString(addr.NodeName), targetKind, targetName)
		}
	}
}

func endpointTarget(ref *corev1.ObjectReference) (kind, name string) {
	if ref == nil {
		return "", ""
	}
	return ref.Kind, ref.Name
}

func writeEndpointSliceDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns, serviceName string) {
	slices, err := kubeClient.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + serviceName,
	})
	if err != nil {
		fmt.Fprintf(w, "Failed to list endpointslices for service %s/%s: %v\n", ns, serviceName, err)
		return
	}
	if len(slices.Items) == 0 {
		fmt.Fprintf(w, "No EndpointSlices found for service %s/%s\n", ns, serviceName)
		return
	}
	for _, slice := range slices.Items {
		fmt.Fprintf(w, "EndpointSlice %s/%s addressType=%s labels=[%s]\n",
			slice.Namespace, slice.Name, slice.AddressType, formatLabels(slice.Labels))
		for _, port := range slice.Ports {
			fmt.Fprintf(w, "  Port name=%s port=%s protocol=%s\n",
				ptrString(port.Name), ptrInt32String(port.Port), ptrProtocolString(port.Protocol))
		}
		for _, ep := range slice.Endpoints {
			targetKind, targetName := "", ""
			if ep.TargetRef != nil {
				targetKind = ep.TargetRef.Kind
				targetName = ep.TargetRef.Name
			}
			fmt.Fprintf(w, "  Endpoint addresses=%s ready=%s serving=%s terminating=%s node=%s target=%s/%s\n",
				strings.Join(ep.Addresses, ","),
				ptrBoolString(ep.Conditions.Ready),
				ptrBoolString(ep.Conditions.Serving),
				ptrBoolString(ep.Conditions.Terminating),
				ptrString(ep.NodeName),
				targetKind,
				targetName)
		}
	}
}

func writeWebhookConfigurationDiagnostics(w io.Writer, ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, webhookName string) {
	list, err := dynClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(w, "Failed to list %s: %v\n", gvr.Resource, err)
		return
	}
	found := false
	for _, cfg := range list.Items {
		webhooks, _, _ := unstructured.NestedSlice(cfg.Object, "webhooks")
		for _, raw := range webhooks {
			wh, ok := raw.(map[string]interface{})
			if !ok || fmt.Sprint(wh["name"]) != webhookName {
				continue
			}
			found = true
			serviceName, _, _ := unstructured.NestedString(wh, "clientConfig", "service", "name")
			serviceNamespace, _, _ := unstructured.NestedString(wh, "clientConfig", "service", "namespace")
			servicePath, _, _ := unstructured.NestedString(wh, "clientConfig", "service", "path")
			servicePort, _, _ := unstructured.NestedInt64(wh, "clientConfig", "service", "port")
			rules, _ := json.Marshal(wh["rules"])
			fmt.Fprintf(w, "WebhookConfiguration %s webhook=%s failurePolicy=%v timeoutSeconds=%v sideEffects=%v service=%s/%s path=%s port=%d rules=%s\n",
				cfg.GetName(), webhookName, wh["failurePolicy"], wh["timeoutSeconds"], wh["sideEffects"],
				serviceNamespace, serviceName, servicePath, servicePort, string(rules))
		}
	}
	if !found {
		fmt.Fprintf(w, "Webhook %s not found in %s\n", webhookName, gvr.Resource)
	}
}

// writePodDiagnostics writes pod status, container state, and logs to w. Same format as DumpPods.
func writePodDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns, labelSelector string) {
	pods, err := kubeClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		fmt.Fprintf(w, "Failed to list pods with selector %q in %s: %v\n", labelSelector, ns, err)
		return
	}
	if len(pods.Items) == 0 {
		fmt.Fprintf(w, "No pods found with selector %q in %s\n", labelSelector, ns)
		return
	}
	for _, p := range pods.Items {
		fmt.Fprintf(w, "Pod %s/%s: Phase=%s\n", p.Namespace, p.Name, p.Status.Phase)
		for _, cond := range p.Status.Conditions {
			fmt.Fprintf(w, "  Condition %s=%s (Reason=%s, Message=%s)\n", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
		for _, cs := range p.Status.ContainerStatuses {
			fmt.Fprintf(w, "  Container %s: Ready=%v Started=%v RestartCount=%d\n",
				cs.Name, cs.Ready, ptrBool(cs.Started), cs.RestartCount)
			if cs.State.Waiting != nil {
				fmt.Fprintf(w, "    State: Waiting (Reason=%s, Message=%s)\n", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
			if cs.State.Running != nil {
				fmt.Fprintf(w, "    State: Running (StartedAt=%s)\n", cs.State.Running.StartedAt.Time)
			}
			if cs.State.Terminated != nil {
				fmt.Fprintf(w, "    State: Terminated (Reason=%s, ExitCode=%d)\n", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
			}
			if cs.LastTerminationState.Terminated != nil {
				ls := cs.LastTerminationState.Terminated
				fmt.Fprintf(w, "    LastState: Terminated (Reason=%s, ExitCode=%d, FinishedAt=%s)\n", ls.Reason, ls.ExitCode, ls.FinishedAt.Time)
			}
		}

		// Log container logs (current and previous) for crash diagnosis.
		for _, cs := range p.Status.ContainerStatuses {
			logOpts := &corev1.PodLogOptions{Container: cs.Name, TailLines: int64Ptr(50)}
			logs, err := kubeClient.CoreV1().Pods(ns).GetLogs(p.Name, logOpts).Do(ctx).Raw()
			if err == nil && len(logs) > 0 {
				fmt.Fprintf(w, "  Logs [%s]:\n%s\n", cs.Name, string(logs))
			}
			if cs.RestartCount > 0 {
				prevOpts := &corev1.PodLogOptions{Container: cs.Name, Previous: true, TailLines: int64Ptr(50)}
				prevLogs, err := kubeClient.CoreV1().Pods(ns).GetLogs(p.Name, prevOpts).Do(ctx).Raw()
				if err == nil && len(prevLogs) > 0 {
					fmt.Fprintf(w, "  Previous logs [%s]:\n%s\n", cs.Name, string(prevLogs))
				}
			}
		}
	}
}

// writeEventDiagnostics writes namespace events to w. Same format as DumpPods event section.
func writeEventDiagnostics(w io.Writer, ctx context.Context, kubeClient kubernetes.Interface, ns string) {
	events, err := kubeClient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	if len(events.Items) == 0 {
		return
	}
	fmt.Fprintf(w, "Events in namespace %s:\n", ns)
	for _, e := range events.Items {
		fmt.Fprintf(w, "  %s last=%s first=%s count=%d %s/%s: %s — %s\n",
			e.Type,
			e.LastTimestamp.UTC().Format(time.RFC3339Nano),
			e.FirstTimestamp.UTC().Format(time.RFC3339Nano),
			e.Count,
			e.InvolvedObject.Kind,
			e.InvolvedObject.Name,
			e.Reason,
			e.Message)
	}
}

func ptrBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

func ptrBoolString(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%t", *b)
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrInt32String(i *int32) string {
	if i == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *i)
}

func ptrProtocolString(p *corev1.Protocol) string {
	if p == nil {
		return "<nil>"
	}
	return string(*p)
}

func ptrInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func int64Ptr(i int64) *int64 {
	return &i
}

// isPodReady returns true if all containers in the pod have the Ready condition set to True.
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// applyYAMLWithClient applies multi-document YAML using a dynamic client directly.
func applyYAMLWithClient(ctx context.Context, dynClient dynamic.Interface, yamlBytes []byte) error {
	return forEachYAMLDocument(yamlBytes, func(obj *unstructured.Unstructured) error {
		gvr, err := gvrFromObject(obj)
		if err != nil {
			return err
		}

		var client dynamic.ResourceInterface
		if obj.GetNamespace() != "" {
			client = dynClient.Resource(gvr).Namespace(obj.GetNamespace())
		} else {
			client = dynClient.Resource(gvr)
		}

		obj.SetManagedFields(nil)
		_, err = client.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
			FieldManager: "e2e-test",
			Force:        true,
		})
		return err
	})
}

// deleteYAMLWithClient deletes resources described in multi-document YAML using a dynamic client directly.
func deleteYAMLWithClient(ctx context.Context, dynClient dynamic.Interface, yamlBytes []byte) error {
	return forEachYAMLDocument(yamlBytes, func(obj *unstructured.Unstructured) error {
		gvr, err := gvrFromObject(obj)
		if err != nil {
			return err
		}

		var client dynamic.ResourceInterface
		if obj.GetNamespace() != "" {
			client = dynClient.Resource(gvr).Namespace(obj.GetNamespace())
		} else {
			client = dynClient.Resource(gvr)
		}

		propagation := metav1.DeletePropagationBackground
		_ = client.Delete(ctx, obj.GetName(), metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		return nil
	})
}

// forEachYAMLDocument splits multi-doc YAML and calls fn for each document.
func forEachYAMLDocument(yamlBytes []byte, fn func(*unstructured.Unstructured) error) error {
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(yamlBytes)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading YAML document: %w", err)
		}

		// Skip empty documents
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(obj); err != nil {
			return fmt.Errorf("decoding YAML document: %w", err)
		}

		// Skip empty objects (e.g., YAML separators with only comments)
		if obj.GetAPIVersion() == "" {
			continue
		}

		if err := fn(obj); err != nil {
			return err
		}
	}
	return nil
}

// gvrFromObject infers a GroupVersionResource from an unstructured object.
// This is a simplified mapper that handles common resource types without
// requiring a full RESTMapper (which needs API server discovery).
func gvrFromObject(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parsing apiVersion %q: %w", obj.GetAPIVersion(), err)
	}

	// Map Kind → plural resource name for types we use in tests.
	kindToResource := map[string]string{
		"ResourceFlavor": "resourceflavors",
		"ClusterQueue":   "clusterqueues",
		"LocalQueue":     "localqueues",
		"Workload":       "workloads",
		"Job":            "jobs",
		"Namespace":      "namespaces",
		"ConfigMap":      "configmaps",
		"Secret":         "secrets",
		"Deployment":     "deployments",
		"DaemonSet":      "daemonsets",
		"Service":        "services",
		"RayCluster":     "rayclusters",
		"RayJob":         "rayjobs",
	}

	resource, ok := kindToResource[obj.GetKind()]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("unknown Kind %q — add to kindToResource map in k8s.go", obj.GetKind())
	}

	return schema.GroupVersionResource{
		Group:    gv.Group,
		Version:  gv.Version,
		Resource: resource,
	}, nil
}

// DumpCRState fetches a custom resource by GVR and name, marshals it to
// indented JSON, and writes it to the bundle as "<resource>-<name>.json".
// Best-effort: logs a warning on error rather than failing the test.
func (tc *TestContext) DumpCRState(ns string, gvr schema.GroupVersionResource, name string) {
	tc.Helper()
	obj, err := tc.dynamicClient.Resource(gvr).Namespace(ns).Get(tc.ctx, name, metav1.GetOptions{})
	if err != nil {
		tc.Logf("DumpCRState: failed to get %s/%s in %s: %v", gvr.Resource, name, ns, err)
		return
	}
	data, err := json.MarshalIndent(obj.Object, "", "  ")
	if err != nil {
		tc.Logf("DumpCRState: failed to marshal %s/%s: %v", gvr.Resource, name, err)
		return
	}
	fileName := fmt.Sprintf("%s-%s.json", gvr.Resource, name)
	if err := tc.bundle.WriteFile(fileName, data); err != nil {
		tc.Logf("DumpCRState: failed to write bundle file %s: %v", fileName, err)
	}
}

// DumpCRList fetches all custom resources of a given GVR in a namespace and
// writes them as a single JSON array to "<resource>-<ns>.json" in the bundle.
// Use this instead of DumpCRState when the exact resource name is not known
// (e.g., Kueue Workloads whose names include a generated hash).
func (tc *TestContext) DumpCRList(ns string, gvr schema.GroupVersionResource) {
	tc.Helper()
	list, err := tc.dynamicClient.Resource(gvr).Namespace(ns).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		tc.Logf("DumpCRList: failed to list %s in %s: %v", gvr.Resource, ns, err)
		return
	}
	data, err := json.MarshalIndent(list.Items, "", "  ")
	if err != nil {
		tc.Logf("DumpCRList: failed to marshal %s list: %v", gvr.Resource, err)
		return
	}
	fileName := fmt.Sprintf("%s-%s.json", gvr.Resource, ns)
	if err := tc.bundle.WriteFile(fileName, data); err != nil {
		tc.Logf("DumpCRList: failed to write bundle file %s: %v", fileName, err)
	}
}

// DumpControllerLogs captures the last tailLines of logs from all pods
// matching labelSelector in ns, and writes them to "controller-<ns>.txt"
// in the bundle. Best-effort: logs a warning on errors, never fails the test.
func (tc *TestContext) DumpControllerLogs(ns, labelSelector string, tailLines int64) {
	tc.Helper()
	fileName := fmt.Sprintf("controller-%s.txt", ns)
	w := tc.bundle.WriterFor(fileName)

	pods, err := tc.kubeClient.CoreV1().Pods(ns).List(tc.ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		tc.Logf("DumpControllerLogs: failed to list pods with selector %q in %s: %v", labelSelector, ns, err)
		return
	}
	if len(pods.Items) == 0 {
		tc.Logf("DumpControllerLogs: no pods found with selector %q in %s", labelSelector, ns)
		return
	}
	for _, p := range pods.Items {
		fmt.Fprintf(w, "=== Pod %s/%s ===\n", p.Namespace, p.Name)
		for _, cs := range p.Spec.Containers {
			logOpts := &corev1.PodLogOptions{Container: cs.Name, TailLines: &tailLines}
			logs, err := tc.kubeClient.CoreV1().Pods(ns).GetLogs(p.Name, logOpts).Do(tc.ctx).Raw()
			if err != nil {
				tc.Logf("DumpControllerLogs: failed to get logs from %s/%s container %s: %v", ns, p.Name, cs.Name, err)
				continue
			}
			fmt.Fprintf(w, "--- Container: %s ---\n%s\n", cs.Name, string(logs))
		}
	}
}

// sanitizeFileName replaces characters unsafe for filenames with underscores.
func sanitizeFileName(s string) string {
	r := strings.NewReplacer("/", "_", "=", "_", ":", "_", " ", "_")
	return r.Replace(s)
}

// DumpDeploymentDiagnostics prints deployment, pod, and event diagnostics to
// stderr AND writes them to a file under the e2e-bundle directory. Use this in
// TestMain when setup fails and no *testing.T or TestContext is available.
func DumpDeploymentDiagnostics(ctx context.Context, kubeClient kubernetes.Interface, ns, deploymentName string) {
	dir := fmt.Sprintf("%s/TestMain-%s", bundle.Root(), ns)
	_ = os.MkdirAll(dir, 0o755)
	fileName := fmt.Sprintf("%s/deployment-%s.txt", dir, deploymentName)
	f, err := os.Create(fileName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create bundle file %s: %v\n", fileName, err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Diagnostics for deployment %s/%s ===\n", ns, deploymentName)
	writeDeploymentDiagnostics(f, ctx, kubeClient, ns, deploymentName)
	writePodDiagnostics(f, ctx, kubeClient, ns, "")
	writeEventDiagnostics(f, ctx, kubeClient, ns)
}
