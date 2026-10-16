// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package links

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/kueueapi"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type WorkloadReader interface {
	ListWorkloads(ctx context.Context, namespace string) ([]byte, error)
}

type Workload struct {
	Name                       string    `json:"name"`
	Namespace                  string    `json:"namespace"`
	Job                        string    `json:"job,omitempty"`
	RunID                      string    `json:"runId,omitempty"`
	Owners                     []string  `json:"owners,omitempty"`
	OwnerUIDs                  []string  `json:"-"`
	ResourceUID                string    `json:"resourceUid,omitempty"`
	ResourceKind               string    `json:"resourceKind,omitempty"`
	ResourceName               string    `json:"resourceName,omitempty"`
	Queue                      string    `json:"queue,omitempty"`
	ClusterQueue               string    `json:"clusterQueue,omitempty"`
	Admitted                   bool      `json:"admitted"`
	Finished                   bool      `json:"finished"`
	PendingReason              string    `json:"pendingReason,omitempty"`
	PendingMessage             string    `json:"pendingMessage,omitempty"`
	CreatedAt                  time.Time `json:"createdAt,omitempty"`
	AdmissionPriorityClass     string    `json:"admissionPriorityClass,omitempty"`
	AdmissionPriorityClassKind string    `json:"admissionPriorityClassKind,omitempty"`
	AdmissionPriority          *int32    `json:"admissionPriority,omitempty"`
	PodPriorityClasses         []string  `json:"podPriorityClasses,omitempty"`

	Project         string `json:"project,omitempty"`
	Experiment      string `json:"experiment,omitempty"`
	Group           string `json:"group,omitempty"`
	Workspace       string `json:"workspace,omitempty"`
	ExecutionTarget string `json:"executionTarget,omitempty"`
}

func (w Workload) Running() bool {
	return w.Admitted && !w.Finished
}

func ListWorkloads(ctx context.Context, r WorkloadReader, namespace string) ([]Workload, error) {
	raw, err := r.ListWorkloads(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	return parseWorkloads(raw)
}

func parseWorkloads(raw []byte) ([]Workload, error) {
	var list workloadList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse workloads: %w", err)
	}
	if list.Items == nil {
		return nil, fmt.Errorf("parse workloads: response has no items array")
	}
	out := make([]Workload, 0, len(list.Items))
	for _, it := range list.Items {
		admitted, finished := admissionState(it.Status.Conditions)
		pendingReason, pendingMessage := kueueapi.PendingCause(it.Status.Conditions)
		labels := it.Metadata.Labels
		executionTarget := workloadExecutionTarget(it)
		priorityClass, priorityClassKind := workloadPriorityClass(it.Spec)
		var owners []string
		var ownerUIDs []string
		resourceUID, resourceKind, resourceName := "", "", ""
		for _, ref := range it.Metadata.OwnerReferences {
			if ref.Name != "" {
				owners = append(owners, ref.Name)
			}
			if ref.UID != "" && ref.Controller != nil && *ref.Controller {
				ownerUIDs = append(ownerUIDs, ref.UID)
				if resourceUID == "" && ref.Name != "" && supportedWorkloadOwnerKind(ref.Kind) {
					resourceUID, resourceKind, resourceName = ref.UID, ref.Kind, ref.Name
				}
			}
		}
		out = append(out, Workload{
			Name:                       it.Metadata.Name,
			Namespace:                  it.Metadata.Namespace,
			Job:                        labels[workloadmeta.LabelJob],
			RunID:                      labels[experiment.LabelRunID],
			Owners:                     owners,
			OwnerUIDs:                  ownerUIDs,
			ResourceUID:                resourceUID,
			ResourceKind:               resourceKind,
			ResourceName:               resourceName,
			Queue:                      it.Spec.QueueName,
			ClusterQueue:               it.Status.Admission.ClusterQueue,
			Admitted:                   admitted,
			Finished:                   finished,
			PendingReason:              pendingReason,
			PendingMessage:             pendingMessage,
			CreatedAt:                  it.Metadata.CreationTimestamp,
			AdmissionPriorityClass:     priorityClass,
			AdmissionPriorityClassKind: priorityClassKind,
			AdmissionPriority:          it.Spec.Priority,
			PodPriorityClasses:         podPriorityClasses(it.Spec.PodSets),
			Project:                    labels[workloadmeta.LabelStellarProject],
			Experiment:                 labels[workloadmeta.LabelStellarExperiment],
			Group:                      labels[workloadmeta.LabelStellarGroup],
			Workspace:                  labels[workloadmeta.LabelWorkspace],
			ExecutionTarget:            executionTarget,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		ja, jb := a.sortKey(), b.sortKey()
		return ja < jb
	})
	return out, nil
}

func supportedWorkloadOwnerKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "job", "rayjob", "pod", "rayservice":
		return true
	default:
		return false
	}
}

func workloadPriorityClass(spec workloadSpec) (string, string) {
	if spec.PriorityClassRef.Name != "" {
		return spec.PriorityClassRef.Name, spec.PriorityClassRef.Kind
	}
	return spec.PriorityClassName, spec.PriorityClassSource
}

func podPriorityClasses(podSets []workloadPodSet) []string {
	seen := map[string]struct{}{}
	for _, podSet := range podSets {
		name := strings.TrimSpace(podSet.Template.Spec.PriorityClassName)
		if name != "" {
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func workloadExecutionTarget(it workloadItem) string {
	if it.Status.ClusterName != "" || len(it.Status.NominatedClusterNames) > 0 {
		return "multiKueue"
	}
	return ""
}

func (w Workload) sortKey() string {
	if w.Job != "" {
		return w.Job
	}
	return w.Name
}

func admissionState(conditions []kueueapi.Condition) (admitted, finished bool) {
	for _, c := range conditions {
		if c.Type == "Admitted" && c.Status == "True" {
			admitted = true
		}
		if c.Type == "Finished" && c.Status == "True" {
			finished = true
		}
	}
	return admitted, finished
}

func ExperimentPath(runID string, workspace ...string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ""
	}
	return WorkspacePath("/stellar?target="+url.QueryEscape(runID), firstWorkspace(workspace))
}

func ExperimentProjectPath(runID, project string, workspace ...string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ""
	}
	path := "/stellar?target=" + url.QueryEscape(runID)
	if p := strings.TrimSpace(project); p != "" {
		path += "&project=" + url.QueryEscape(p)
	}
	return WorkspacePath(path, firstWorkspace(workspace))
}

func RayDashboardPath(namespace, rayClusterName string, workspace ...string) string {
	namespace = strings.TrimSpace(namespace)
	rayClusterName = strings.TrimSpace(rayClusterName)
	if namespace == "" || rayClusterName == "" {
		return ""
	}
	path := "/api/portal/ray/proxy/" + url.PathEscape(namespace) + "/" + url.PathEscape(rayClusterName) + "/"
	return WorkspacePath(path, firstWorkspace(workspace))
}

func ClusterInstancePath(instance string, workspace ...string) string {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		return ""
	}
	return WorkspacePath("/portal/cluster?instance="+url.QueryEscape(instance), firstWorkspace(workspace))
}

func WorkspacePath(path, workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return path
	}
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	q := u.Query()
	q.Set("workspace", workspace)
	u.RawQuery = q.Encode()
	return u.String()
}

func firstWorkspace(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

type workloadList struct {
	Items []workloadItem `json:"items"`
}

type workloadItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		OwnerReferences   []struct {
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			UID        string `json:"uid"`
			Controller *bool  `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec   workloadSpec `json:"spec"`
	Status struct {
		Admission struct {
			ClusterQueue string `json:"clusterQueue"`
		} `json:"admission"`
		Conditions            []kueueapi.Condition `json:"conditions"`
		ClusterName           string               `json:"clusterName"`
		NominatedClusterNames []string             `json:"nominatedClusterNames"`
	} `json:"status"`
}
type workloadSpec struct {
	QueueName           string `json:"queueName"`
	Priority            *int32 `json:"priority"`
	PriorityClassName   string `json:"priorityClassName"`
	PriorityClassSource string `json:"priorityClassSource"`
	PriorityClassRef    struct {
		Name  string `json:"name"`
		Group string `json:"group"`
		Kind  string `json:"kind"`
	} `json:"priorityClassRef"`
	PodSets []workloadPodSet `json:"podSets"`
}

type workloadPodSet struct {
	Template struct {
		Spec struct {
			PriorityClassName string `json:"priorityClassName"`
		} `json:"spec"`
	} `json:"template"`
}
