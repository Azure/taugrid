// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	admissionCheckListGVK = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "AdmissionCheckList"}
	multiKueueConfigGVK   = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta1", Kind: "MultiKueueConfig"}
	multiKueueClusterGVK  = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta1", Kind: "MultiKueueCluster"}
)

type MultiKueuePrerequisiteStatus struct {
	Ready   bool
	Message string
}

type MultiKueuePrerequisiteReader interface {
	Check(context.Context) (MultiKueuePrerequisiteStatus, error)
}

type KubernetesMultiKueuePrerequisites struct {
	Reader client.Reader
}

func (r *TauClusterReconciler) multiKueueReadinessCondition(
	ctx context.Context,
	generation int64,
) (metav1.Condition, error) {
	if r.MultiKueuePrerequisites == nil {
		return condition(
			tauv1alpha1.ConditionMultiKueueReady,
			metav1.ConditionFalse,
			"PrerequisitesNotReady",
			"MultiKueue prerequisite reader is not configured",
			generation,
		), nil
	}

	status, err := r.MultiKueuePrerequisites.Check(ctx)
	conditionStatus := metav1.ConditionFalse
	reason := "PrerequisitesNotReady"
	if status.Ready && err == nil {
		conditionStatus = metav1.ConditionTrue
		reason = "Ready"
	}
	return condition(
		tauv1alpha1.ConditionMultiKueueReady,
		conditionStatus,
		reason,
		status.Message,
		generation,
	), err
}

func (c *KubernetesMultiKueuePrerequisites) Check(ctx context.Context) (MultiKueuePrerequisiteStatus, error) {
	if c == nil || c.Reader == nil {
		return MultiKueuePrerequisiteStatus{Message: "MultiKueue prerequisite reader is not configured"}, nil
	}

	var checks unstructured.UnstructuredList
	checks.SetGroupVersionKind(admissionCheckListGVK)
	if err := c.Reader.List(ctx, &checks); err != nil {
		return MultiKueuePrerequisiteStatus{
			Message: "cannot list manager-cluster AdmissionChecks",
		}, fmt.Errorf("list AdmissionChecks: %w", err)
	}

	issues := make([]string, 0)
	found := false
	for i := range checks.Items {
		check := &checks.Items[i]
		controllerName, _, _ := unstructured.NestedString(check.Object, "spec", "controllerName")
		if strings.TrimSpace(controllerName) != multiKueueAdmissionCheckController {
			continue
		}
		found = true
		ready, issue, err := c.admissionCheckReady(ctx, check)
		if err != nil {
			return MultiKueuePrerequisiteStatus{Message: issue}, err
		}
		if ready {
			return MultiKueuePrerequisiteStatus{
				Ready:   true,
				Message: fmt.Sprintf("AdmissionCheck %q has an active MultiKueue config and worker", check.GetName()),
			}, nil
		}
		issues = append(issues, issue)
	}
	if !found {
		return MultiKueuePrerequisiteStatus{
			Message: "no AdmissionCheck uses controller kueue.x-k8s.io/multikueue",
		}, nil
	}
	sort.Strings(issues)
	return MultiKueuePrerequisiteStatus{Message: strings.Join(issues, "; ")}, nil
}

func (c *KubernetesMultiKueuePrerequisites) admissionCheckReady(
	ctx context.Context,
	check *unstructured.Unstructured,
) (bool, string, error) {
	name := check.GetName()
	if !conditionIsTrue(check, "Active") {
		return false, fmt.Sprintf("AdmissionCheck %q is not Active", name), nil
	}

	apiGroup, _, _ := unstructured.NestedString(check.Object, "spec", "parameters", "apiGroup")
	kind, _, _ := unstructured.NestedString(check.Object, "spec", "parameters", "kind")
	configName, _, _ := unstructured.NestedString(check.Object, "spec", "parameters", "name")
	if apiGroup != "kueue.x-k8s.io" || kind != "MultiKueueConfig" || strings.TrimSpace(configName) == "" {
		return false, fmt.Sprintf("AdmissionCheck %q does not reference a MultiKueueConfig", name), nil
	}

	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(multiKueueConfigGVK)
	if err := c.Reader.Get(ctx, client.ObjectKey{Name: configName}, config); err != nil {
		issue := fmt.Sprintf("AdmissionCheck %q references unreadable MultiKueueConfig %q", name, configName)
		if apierrors.IsNotFound(err) {
			return false, issue, nil
		}
		return false, issue, fmt.Errorf("get MultiKueueConfig %q: %w", configName, err)
	}
	clusters, _, _ := unstructured.NestedStringSlice(config.Object, "spec", "clusters")
	if len(clusters) == 0 {
		return false, fmt.Sprintf("MultiKueueConfig %q has no worker clusters", configName), nil
	}
	sort.Strings(clusters)
	for _, clusterName := range clusters {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(multiKueueClusterGVK)
		if err := c.Reader.Get(ctx, client.ObjectKey{Name: clusterName}, cluster); err != nil {
			issue := fmt.Sprintf("MultiKueueConfig %q references unreadable worker %q", configName, clusterName)
			if apierrors.IsNotFound(err) {
				return false, issue, nil
			}
			return false, issue, fmt.Errorf("get MultiKueueCluster %q: %w", clusterName, err)
		}
		if conditionIsTrue(cluster, "Active") {
			return true, "", nil
		}
	}
	return false, fmt.Sprintf("MultiKueueConfig %q has no Active worker clusters", configName), nil
}

func conditionIsTrue(object *unstructured.Unstructured, conditionType string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if condition["type"] == conditionType && condition["status"] == "True" {
			return true
		}
	}
	return false
}
