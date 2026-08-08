// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package autocapture

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type KubectlClient struct {
	Runner *kube.Runner
}

func NewKubectlClient(kubeContext string) KubectlClient {
	return KubectlClient{Runner: kube.New(kubeContext)}
}

func (c KubectlClient) ListJobs(ctx context.Context, namespace string) ([]Job, error) {
	byKey := map[string]Job{}
	for _, selector := range []string{experiment.LabelRunID, workloadmeta.LabelJob} {
		var list jobList
		if err := c.getList(ctx, namespace, "jobs", selector, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			putJob(byKey, jobItemToJob(item))
		}
	}
	for _, selector := range []string{workloadmeta.LabelJob, experiment.LabelRunID} {
		var list rayJobList
		if err := c.getOptionalList(ctx, namespace, "rayjobs.ray.io", selector, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			putJob(byKey, rayJobItemToJob(item))
		}
	}
	out := make([]Job, 0, len(byKey))
	for _, job := range byKey {
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].Name < out[j].Name
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out, nil
}

func (c KubectlClient) ListWorkloads(ctx context.Context, namespace string) ([]Workload, error) {
	var list workloadList
	if err := c.getOptionalList(ctx, namespace, "workloads.kueue.x-k8s.io", "", &list); err != nil {
		return nil, err
	}
	out := make([]Workload, 0, len(list.Items))
	for _, item := range list.Items {
		workload := Workload{
			Namespace:    item.Metadata.Namespace,
			Name:         item.Metadata.Name,
			UID:          item.Metadata.UID,
			Labels:       item.Metadata.Labels,
			Queue:        item.Spec.QueueName,
			ClusterQueue: item.Status.Admission.ClusterQueue,
			Phase:        "Pending",
		}
		for _, cond := range item.Status.Conditions {
			switch cond.Type {
			case "Admitted":
				if cond.Status == "True" {
					workload.Admitted = true
					workload.Phase = "Admitted"
					workload.AdmittedAt = cond.LastTransitionTime
				} else if cond.Reason != "" {
					workload.Reason = cond.Reason
				}
			case "Finished":
				if cond.Status == "True" {
					workload.Phase = "Finished"
					workload.FinishedAt = cond.LastTransitionTime
					if cond.Reason != "" {
						workload.Reason = cond.Reason
					}
				}
			case "QuotaReserved":
				if cond.Status == "True" && workload.Phase == "Pending" {
					workload.Phase = "QuotaReserved"
				}
			}
			if strings.Contains(strings.ToLower(cond.Type), "evict") || strings.Contains(strings.ToLower(cond.Reason), "preempt") {
				workload.Preemption = firstNonEmpty(cond.Reason, cond.Message, cond.Type)
			}
		}
		out = append(out, workload)
	}
	return out, nil
}

func (c KubectlClient) ListPods(ctx context.Context, namespace string) ([]Pod, error) {
	byKey := map[string]Pod{}
	for _, selector := range []string{experiment.LabelRunID, workloadmeta.LabelJob} {
		var list podList
		if err := c.getList(ctx, namespace, "pods", selector, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			putPod(byKey, podItemToPod(item))
		}
	}
	out := make([]Pod, 0, len(byKey))
	for _, pod := range byKey {
		out = append(out, pod)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].Name < out[j].Name
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out, nil
}

func (c KubectlClient) ListResourceClaims(ctx context.Context, namespace string) ([]ResourceClaim, error) {
	var list resourceClaimList
	if err := c.getOptionalList(ctx, namespace, "resourceclaims.resource.k8s.io", "", &list); err != nil {
		return nil, err
	}
	out := make([]ResourceClaim, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, ResourceClaim{
			Namespace:   item.Metadata.Namespace,
			Name:        item.Metadata.Name,
			UID:         item.Metadata.UID,
			Labels:      item.Metadata.Labels,
			DeviceClass: firstNonEmpty(item.Spec.DeviceClassName, item.Status.Allocation.Devices.Results.firstDeviceClass()),
		})
	}
	return out, nil
}

func (c KubectlClient) ListEvents(ctx context.Context, namespace string) ([]KubernetesEvent, error) {
	var list eventList
	if err := c.getOptionalList(ctx, namespace, "events", "", &list); err != nil {
		return nil, err
	}
	out := make([]KubernetesEvent, 0, len(list.Items))
	for _, item := range list.Items {
		regarding := item.Regarding
		if regarding.Kind == "" && item.InvolvedObject.Kind != "" {
			regarding = item.InvolvedObject
		}
		out = append(out, KubernetesEvent{
			Namespace: item.Metadata.Namespace,
			Name:      item.Metadata.Name,
			UID:       item.Metadata.UID,
			Type:      item.Type,
			Reason:    item.Reason,
			Action:    item.Action,
			Message:   item.Message,
			Source:    firstNonEmpty(item.ReportingController, item.ReportingInstance, item.Source.Component),
			Count:     item.Count,
			Time:      firstTime(item.EventTime, item.LastTimestamp, item.FirstTimestamp, item.Metadata.CreationTimestamp),
			Regarding: regarding,
		})
	}
	return out, nil
}

func (c KubectlClient) getList(ctx context.Context, namespace, resource, selector string, out any) error {
	if c.Runner == nil {
		return fmt.Errorf("kubectl runner is required")
	}
	args := namespaceArgs(namespace)
	args = append(args, "get", resource)
	if selector != "" {
		args = append(args, "-l", selector)
	}
	args = append(args, "-o", "json")
	raw, err := c.Runner.Raw(ctx, args, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), out)
}

func (c KubectlClient) getOptionalList(ctx context.Context, namespace, resource, selector string, out any) error {
	err := c.getList(ctx, namespace, resource, selector, out)
	if err != nil && optionalResourceMissing(err) {
		return nil
	}
	return err
}

func namespaceArgs(namespace string) []string {
	if namespace == "" || namespace == "*" || namespace == "all" {
		return []string{"-A"}
	}
	return []string{"-n", namespace}
}

func optionalResourceMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the server doesn't have a resource type") ||
		strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "notfound")
}

func podResourceClaims(specClaims []podResourceClaim, statusClaims []podResourceClaimStatus) []string {
	statusByName := map[string]string{}
	for _, claim := range statusClaims {
		if claim.Name != "" && claim.ResourceClaimName != "" {
			statusByName[claim.Name] = claim.ResourceClaimName
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, claim := range specClaims {
		value := firstNonEmpty(statusByName[claim.Name], claim.ResourceClaimName, claim.ResourceClaimTemplateName, claim.Name)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func putJob(byKey map[string]Job, job Job) {
	key := job.UID
	if key == "" {
		key = job.Namespace + "/" + job.Name
	}
	byKey[key] = job
}

func putPod(byKey map[string]Pod, pod Pod) {
	key := pod.UID
	if key == "" {
		key = pod.Namespace + "/" + pod.Name
	}
	byKey[key] = pod
}

func jobItemToJob(item jobItem) Job {
	job := Job{
		Namespace:   item.Metadata.Namespace,
		Name:        item.Metadata.Name,
		UID:         item.Metadata.UID,
		Labels:      copyLabelsWithRunID(item.Metadata.Labels, item.Metadata.Name),
		Annotations: item.Metadata.Annotations,
		CreatedAt:   item.Metadata.CreationTimestamp,
		StartedAt:   item.Status.StartTime,
		FinishedAt:  item.Status.CompletionTime,
		Active:      item.Status.Active,
		Succeeded:   item.Status.Succeeded,
		Failed:      item.Status.Failed,
	}
	if item.Spec.Suspend != nil {
		job.Suspended = *item.Spec.Suspend
	}
	if item.Spec.Parallelism != nil {
		job.Parallelism = *item.Spec.Parallelism
	}
	for _, cond := range item.Status.Conditions {
		job.Conditions = append(job.Conditions, conditionFromKube(cond))
	}
	return job
}

func rayJobItemToJob(item rayJobItem) Job {
	job := Job{
		Namespace:   item.Metadata.Namespace,
		Name:        item.Metadata.Name,
		UID:         item.Metadata.UID,
		Labels:      copyLabelsWithRunID(item.Metadata.Labels, item.Metadata.Name),
		Annotations: item.Metadata.Annotations,
		CreatedAt:   item.Metadata.CreationTimestamp,
		StartedAt:   firstTime(item.Status.StartTime, item.Status.RayJobInfo.StartTime),
		FinishedAt:  firstTime(item.Status.EndTime, item.Status.RayJobInfo.EndTime),
		Parallelism: 1,
	}
	for _, cond := range item.Status.Conditions {
		job.Conditions = append(job.Conditions, conditionFromKube(cond))
	}

	statusText := strings.ToLower(firstNonEmpty(item.Status.JobStatus, item.Status.JobDeploymentStatus))
	switch {
	case strings.Contains(statusText, "fail"):
		job.Failed = 1
		job.Conditions = appendTerminalCondition(job.Conditions, "Failed", item.Status.JobStatus, item.Status.Message, job.FinishedAt)
	case strings.Contains(statusText, "succeed") || strings.Contains(statusText, "complete"):
		job.Succeeded = 1
		job.Conditions = appendTerminalCondition(job.Conditions, "Complete", item.Status.JobStatus, item.Status.Message, job.FinishedAt)
	case strings.Contains(statusText, "run") || strings.Contains(statusText, "pending") || statusText == "":
		if job.FinishedAt.IsZero() {
			job.Active = 1
		}
	}
	return job
}

func podItemToPod(item podItem) Pod {
	ready, total, restarts := 0, len(item.Status.ContainerStatuses), 0
	reason := ""
	var exitCode *int32
	oomKilled := false
	for _, cs := range item.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
		if cs.State.Waiting != nil && reason == "" {
			reason = cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil {
			if reason == "" {
				reason = cs.State.Terminated.Reason
			}
			code := cs.State.Terminated.ExitCode
			exitCode = &code
			if cs.State.Terminated.Reason == "OOMKilled" {
				oomKilled = true
			}
		}
		if cs.LastTerminationState.Terminated != nil {
			if reason == "" {
				reason = cs.LastTerminationState.Terminated.Reason
			}
			if exitCode == nil {
				code := cs.LastTerminationState.Terminated.ExitCode
				exitCode = &code
			}
			if cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
				oomKilled = true
			}
		}
	}
	return Pod{
		Namespace:       item.Metadata.Namespace,
		Name:            item.Metadata.Name,
		UID:             item.Metadata.UID,
		Labels:          item.Metadata.Labels,
		Phase:           item.Status.Phase,
		Node:            item.Spec.NodeName,
		Restarts:        restarts,
		Ready:           fmt.Sprintf("%d/%d", ready, total),
		StartedAt:       item.Status.StartTime,
		ResourceClaims:  podResourceClaims(item.Spec.ResourceClaims, item.Status.ResourceClaimStatuses),
		ContainerReason: reason,
		ExitCode:        exitCode,
		OOMKilled:       oomKilled,
	}
}

func conditionFromKube(cond kubeCondition) Condition {
	return Condition{
		Type:               cond.Type,
		Status:             cond.Status,
		Reason:             cond.Reason,
		Message:            cond.Message,
		LastTransitionTime: cond.LastTransitionTime,
	}
}

func appendTerminalCondition(conditions []Condition, conditionType, reason, message string, transitionTime time.Time) []Condition {
	for _, cond := range conditions {
		if cond.Type == conditionType && cond.Status == "True" {
			return conditions
		}
	}
	return append(conditions, Condition{
		Type:               conditionType,
		Status:             "True",
		Reason:             reason,
		Message:            message,
		LastTransitionTime: transitionTime,
	})
}

func copyLabelsWithRunID(labels map[string]string, fallbackName string) map[string]string {
	out := copyStringMap(labels)
	runID := firstNonEmpty(out[experiment.LabelRunID], out[workloadmeta.LabelJob], strings.TrimPrefix(fallbackName, "tau-"), fallbackName)
	if runID == "" {
		return out
	}
	if out == nil {
		out = map[string]string{}
	}
	out[experiment.LabelRunID] = runID
	return out
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

type objectMeta struct {
	Namespace         string            `json:"namespace"`
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
}

type kubeCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

type jobList struct {
	Items []jobItem `json:"items"`
}

type jobItem struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Suspend     *bool `json:"suspend"`
		Parallelism *int  `json:"parallelism"`
	} `json:"spec"`
	Status struct {
		StartTime      time.Time       `json:"startTime"`
		CompletionTime time.Time       `json:"completionTime"`
		Active         int             `json:"active"`
		Succeeded      int             `json:"succeeded"`
		Failed         int             `json:"failed"`
		Conditions     []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type rayJobList struct {
	Items []rayJobItem `json:"items"`
}

type rayJobItem struct {
	Metadata objectMeta `json:"metadata"`
	Status   struct {
		JobStatus           string          `json:"jobStatus"`
		JobDeploymentStatus string          `json:"jobDeploymentStatus"`
		Message             string          `json:"message"`
		StartTime           time.Time       `json:"startTime"`
		EndTime             time.Time       `json:"endTime"`
		Conditions          []kubeCondition `json:"conditions"`
		RayJobInfo          struct {
			StartTime time.Time `json:"startTime"`
			EndTime   time.Time `json:"endTime"`
		} `json:"rayJobInfo"`
	} `json:"status"`
}

type workloadList struct {
	Items []struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			QueueName string `json:"queueName"`
		} `json:"spec"`
		Status struct {
			Admission struct {
				ClusterQueue string `json:"clusterQueue"`
			} `json:"admission"`
			Conditions []kubeCondition `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

type podResourceClaim struct {
	Name                      string `json:"name"`
	ResourceClaimName         string `json:"resourceClaimName"`
	ResourceClaimTemplateName string `json:"resourceClaimTemplateName"`
}

type podResourceClaimStatus struct {
	Name              string `json:"name"`
	ResourceClaimName string `json:"resourceClaimName"`
}

type podList struct {
	Items []podItem `json:"items"`
}

type podItem struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName       string             `json:"nodeName"`
		ResourceClaims []podResourceClaim `json:"resourceClaims"`
	} `json:"spec"`
	Status struct {
		StartTime             time.Time                `json:"startTime"`
		Phase                 string                   `json:"phase"`
		ResourceClaimStatuses []podResourceClaimStatus `json:"resourceClaimStatuses"`
		ContainerStatuses     []struct {
			Ready        bool `json:"ready"`
			RestartCount int  `json:"restartCount"`
			State        struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Terminated *struct {
					Reason   string `json:"reason"`
					ExitCode int32  `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
			LastTerminationState struct {
				Terminated *struct {
					Reason   string `json:"reason"`
					ExitCode int32  `json:"exitCode"`
				} `json:"terminated"`
			} `json:"lastState"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type resourceClaimList struct {
	Items []struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			DeviceClassName string `json:"deviceClassName"`
		} `json:"spec"`
		Status struct {
			Allocation struct {
				Devices struct {
					Results deviceResults `json:"results"`
				} `json:"devices"`
			} `json:"allocation"`
		} `json:"status"`
	} `json:"items"`
}

type deviceResults []struct {
	DeviceClassName string `json:"deviceClassName"`
}

func (d deviceResults) firstDeviceClass() string {
	for _, result := range d {
		if result.DeviceClassName != "" {
			return result.DeviceClassName
		}
	}
	return ""
}

type eventList struct {
	Items []struct {
		Metadata            objectMeta `json:"metadata"`
		Type                string     `json:"type"`
		Reason              string     `json:"reason"`
		Action              string     `json:"action"`
		Message             string     `json:"message"`
		Count               int32      `json:"count"`
		ReportingController string     `json:"reportingController"`
		ReportingInstance   string     `json:"reportingInstance"`
		EventTime           time.Time  `json:"eventTime"`
		FirstTimestamp      time.Time  `json:"firstTimestamp"`
		LastTimestamp       time.Time  `json:"lastTimestamp"`
		Source              struct {
			Component string `json:"component"`
		} `json:"source"`
		Regarding      ObjectRef `json:"regarding"`
		InvolvedObject ObjectRef `json:"involvedObject"`
	} `json:"items"`
}
