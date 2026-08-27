// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/workloadmeta"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	rayJobsGVR   = schema.GroupVersionResource{Group: "ray.io", Version: "v1", Resource: "rayjobs"}
	workloadsGVR = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}
)

type KubernetesSource struct {
	core    kubernetes.Interface
	dynamic dynamic.Interface
}

func NewInClusterSource() (*KubernetesSource, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes configuration: %w", err)
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes core client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}
	return &KubernetesSource{core: core, dynamic: dynamicClient}, nil
}

func (s *KubernetesSource) ListJobs(ctx context.Context, namespace string) ([]Job, error) {
	items, err := s.core.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(items.Items))
	for _, item := range items.Items {
		if !hasTauLabel(item.Labels) {
			continue
		}
		metadata := typedMetadata(item.Name, item.Namespace, string(item.UID), item.ResourceVersion, item.Generation, item.CreationTimestamp.Time, item.Labels, item.Annotations, item.OwnerReferences)
		if isRayJobOwned(metadata) {
			continue
		}
		metadata.Deleting = item.DeletionTimestamp != nil
		out = append(out, Job{
			Metadata:       metadata,
			Suspended:      item.Spec.Suspend != nil && *item.Spec.Suspend,
			Active:         item.Status.Active,
			Succeeded:      item.Status.Succeeded,
			Failed:         item.Status.Failed,
			StartTime:      valueTime(item.Status.StartTime),
			CompletionTime: valueTime(item.Status.CompletionTime),
			Conditions:     typedJobConditions(item.Status.Conditions),
		})
	}
	return out, nil
}

func (s *KubernetesSource) ListRayJobs(ctx context.Context, namespace string) ([]RayJob, error) {
	items, err := s.dynamic.Resource(rayJobsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, ErrRayJobCRDMissing
		}
		return nil, err
	}
	out := make([]RayJob, 0, len(items.Items))
	for _, item := range items.Items {
		if !hasTauLabel(item.GetLabels()) {
			continue
		}
		out = append(out, RayJob{
			Metadata:         unstructuredMetadata(item),
			DeploymentStatus: nestedString(item, "status", "jobDeploymentStatus"),
			StartTime:        nestedTime(item, "status", "startTime"),
			CompletionTime:   firstTime(nestedTime(item, "status", "endTime"), nestedTime(item, "status", "completionTime")),
			Conditions:       unstructuredConditions(item),
		})
	}
	return out, nil
}

// ListPods reads the pods behind Tau workloads so a terminal Job failure can be
// explained by exit code or OOM kill rather than by the Job's own opaque
// "BackoffLimitExceeded". Pods are filtered to Tau-labelled workloads, matching
// how Jobs and RayJobs are filtered above.
func (s *KubernetesSource) ListPods(ctx context.Context, namespace string) ([]Pod, error) {
	items, err := s.core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Pod, 0, len(items.Items))
	for _, item := range items.Items {
		if !hasTauLabel(item.Labels) {
			continue
		}
		metadata := typedMetadata(item.Name, item.Namespace, string(item.UID), item.ResourceVersion, item.Generation, item.CreationTimestamp.Time, item.Labels, item.Annotations, item.OwnerReferences)
		metadata.Deleting = item.DeletionTimestamp != nil
		out = append(out, Pod{
			Metadata:   metadata,
			Phase:      string(item.Status.Phase),
			Reason:     item.Status.Reason,
			Containers: containerStates(item.Status),
		})
	}
	return out, nil
}

// containerStates flattens the terminal or blocked state of every container in
// a pod. Waiting containers are only reported when they carry a reason
// (ImagePullBackOff, CreateContainerConfigError and friends) — a container
// merely waiting to start explains nothing.
func containerStates(status corev1.PodStatus) []ContainerState {
	all := make([]corev1.ContainerStatus, 0, len(status.InitContainerStatuses)+len(status.ContainerStatuses))
	all = append(all, status.InitContainerStatuses...)
	all = append(all, status.ContainerStatuses...)

	out := make([]ContainerState, 0, len(all))
	for _, container := range all {
		switch {
		case container.State.Terminated != nil:
			terminated := container.State.Terminated
			out = append(out, ContainerState{
				Name:       container.Name,
				ExitCode:   terminated.ExitCode,
				Reason:     terminated.Reason,
				Message:    terminated.Message,
				Terminated: true,
				OOMKilled:  strings.EqualFold(terminated.Reason, "OOMKilled"),
			})
		case container.State.Waiting != nil && container.State.Waiting.Reason != "":
			out = append(out, ContainerState{
				Name:    container.Name,
				Reason:  container.State.Waiting.Reason,
				Message: container.State.Waiting.Message,
			})
		}
	}
	return out
}

func (s *KubernetesSource) ListWorkloads(ctx context.Context, namespace string) ([]Workload, error) {
	items, err := s.dynamic.Resource(workloadsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, ErrWorkloadCRDMissing
		}
		return nil, err
	}
	out := make([]Workload, 0, len(items.Items))
	for _, item := range items.Items {
		conditions := unstructuredConditions(item)
		admitted, admittedAt := admittedCondition(conditions)
		out = append(out, Workload{
			Metadata:     unstructuredMetadata(item),
			Queue:        nestedString(item, "spec", "queueName"),
			ClusterQueue: nestedString(item, "status", "admission", "clusterQueue"),
			Phase:        nestedString(item, "status", "phase"),
			Admitted:     admitted,
			AdmittedAt:   admittedAt,
			FinishedAt:   conditionTime(conditions, "Finished"),
			Conditions:   conditions,
		})
	}
	return out, nil
}

func typedMetadata(name, namespace, uid, version string, generation int64, created time.Time, labels, annotations map[string]string, owners []metav1.OwnerReference) Metadata {
	ownerKind, ownerName, ownerUID := ownerReference(owners)
	return Metadata{Name: name, Namespace: namespace, UID: uid, ResourceVersion: version, Generation: generation, CreatedAt: created, Labels: labels, Annotations: annotations, OwnerKind: ownerKind, OwnerName: ownerName, OwnerUID: ownerUID}
}

func unstructuredMetadata(item unstructured.Unstructured) Metadata {
	ownerKind, ownerName, ownerUID := ownerReference(item.GetOwnerReferences())
	return Metadata{
		Name: item.GetName(), Namespace: item.GetNamespace(), UID: string(item.GetUID()), ResourceVersion: item.GetResourceVersion(),
		Generation: item.GetGeneration(), CreatedAt: item.GetCreationTimestamp().Time, Labels: item.GetLabels(), Annotations: item.GetAnnotations(),
		OwnerKind: ownerKind, OwnerName: ownerName, OwnerUID: ownerUID,
		Deleting: item.GetDeletionTimestamp() != nil,
	}
}

func isRayJobOwned(metadata Metadata) bool {
	return strings.EqualFold(metadata.OwnerKind, "RayJob")
}

func hasTauLabel(labels map[string]string) bool {
	for key := range labels {
		if strings.HasPrefix(key, workloadmeta.Domain) {
			return true
		}
	}
	return false
}

func ownerReference(owners []metav1.OwnerReference) (string, string, string) {
	for _, owner := range owners {
		if owner.Controller != nil && *owner.Controller {
			return owner.Kind, owner.Name, string(owner.UID)
		}
	}
	if len(owners) > 0 {
		return owners[0].Kind, owners[0].Name, string(owners[0].UID)
	}
	return "", "", ""
}

func typedJobConditions(conditions []batchv1.JobCondition) []Condition {
	out := make([]Condition, 0, len(conditions))
	for _, condition := range conditions {
		out = append(out, Condition{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message, LastTransitionTime: condition.LastTransitionTime.Time})
	}
	return out
}

func unstructuredConditions(item unstructured.Unstructured) []Condition {
	raw, found, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
	if !found {
		return nil
	}
	out := make([]Condition, 0, len(raw))
	for _, value := range raw {
		condition, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, Condition{
			Type: stringValue(condition["type"]), Status: stringValue(condition["status"]), Reason: stringValue(condition["reason"]),
			Message: stringValue(condition["message"]), LastTransitionTime: parseTime(stringValue(condition["lastTransitionTime"])),
		})
	}
	return out
}

func nestedString(item unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(item.Object, fields...)
	return value
}

func nestedTime(item unstructured.Unstructured, fields ...string) time.Time {
	return parseTime(nestedString(item, fields...))
}

func valueTime(value *metav1.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.Time
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func admittedCondition(conditions []Condition) (bool, time.Time) {
	for _, condition := range conditions {
		if condition.Type == "Admitted" && condition.Status == "True" {
			return true, condition.LastTransitionTime
		}
	}
	return false, time.Time{}
}

func conditionTime(conditions []Condition, typ string) time.Time {
	for _, condition := range conditions {
		if condition.Type == typ && condition.Status == "True" {
			return condition.LastTransitionTime
		}
	}
	return time.Time{}
}

func firstTime(times ...time.Time) time.Time {
	for _, value := range times {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func stringValue(value interface{}) string {
	stringValue, _ := value.(string)
	return stringValue
}
