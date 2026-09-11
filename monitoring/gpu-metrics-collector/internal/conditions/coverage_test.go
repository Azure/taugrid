// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conditions

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
)

func TestRecoveryClearsOldDiagnosticOnNode(t *testing.T) {
	t.Parallel()
	client := newFakeNode("gpu-node-0")
	writer := NewWriter(client, "gpu-node-0")
	ctx := context.Background()
	for _, result := range []rules.Result{
		{ConditionType: "GPUError", Unknown: true, Reason: "MetricHistoryUnavailable", Message: "waiting for counter baseline"},
		{ConditionType: "GPUError", Reason: "GPUErrorOk"},
	} {
		if err := writer.WriteConditions(ctx, []rules.Result{result}); err != nil {
			t.Fatal(err)
		}
	}
	node, err := client.CoreV1().Nodes().Get(ctx, "gpu-node-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	condition := node.Status.Conditions[0]
	if condition.Status != corev1.ConditionFalse || condition.Reason != "GPUErrorOk" || condition.Message != "" {
		t.Fatalf("recovered Node kept an obsolete diagnostic: %+v", condition)
	}
}

func TestHeartbeatPreservesServerTransitionTime(t *testing.T) {
	t.Parallel()
	client := newFakeNode("gpu-node-0")
	writer := NewWriter(client, "gpu-node-0")
	ctx := context.Background()
	results := []rules.Result{{ConditionType: "GPUError", Reason: "GPUErrorOk"}}
	if err := writer.WriteConditions(ctx, results); err != nil {
		t.Fatal(err)
	}
	before, err := client.CoreV1().Nodes().Get(ctx, "gpu-node-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writer.heartbeatCounter = heartbeatCycles - 1
	writer.jitterOffset = 0
	if err := writer.WriteConditions(ctx, results); err != nil {
		t.Fatal(err)
	}
	after, err := client.CoreV1().Nodes().Get(ctx, "gpu-node-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	previous := before.Status.Conditions[0].LastTransitionTime
	current := after.Status.Conditions[0].LastTransitionTime
	if current.IsZero() || !previous.Equal(&current) {
		t.Fatalf("heartbeat changed server transition time: %v -> %v", previous, current)
	}
}

func TestWriteConditionsPreservesUnknownTransitions(t *testing.T) {
	t.Parallel()
	client := newFakeNode("gpu-node-0")
	writer := NewWriter(client, "gpu-node-0")
	results := []rules.Result{
		{ConditionType: "GPUError", Reason: "GPUErrorOk"},
		{ConditionType: "GPUError", Unknown: true, Reason: "MetricCoverageUnavailable"},
		{ConditionType: "GPUError", Firing: true, Reason: "GPUError"},
		{ConditionType: "GPUError", Unknown: true, Reason: "MetricCoverageUnavailable"},
		{ConditionType: "GPUError", Reason: "GPUErrorOk"},
	}
	want := []corev1.ConditionStatus{
		corev1.ConditionFalse, corev1.ConditionUnknown, corev1.ConditionTrue,
		corev1.ConditionUnknown, corev1.ConditionFalse,
	}
	for i, result := range results {
		if err := writer.WriteConditions(context.Background(), []rules.Result{result}); err != nil {
			t.Fatal(err)
		}
		if status := writer.ExportLastStatus()["GPUError"]; status != string(want[i]) {
			t.Fatalf("persisted status %q, want %q", status, want[i])
		}
	}
	got := patches(t, client)
	if len(got) != len(want) {
		t.Fatalf("got %d patches for %d actual status transitions", len(got), len(want))
	}
	for i, patch := range got {
		condition := conditionOf(t, patch, "GPUError")
		if condition.Status != want[i] || condition.LastTransitionTime.IsZero() {
			t.Fatalf("transition %d: %+v", i, condition)
		}
	}
}

func TestRestoredUnknownDoesNotBecomeFalse(t *testing.T) {
	t.Parallel()
	client := newFakeNode("gpu-node-0")
	writer := NewWriter(client, "gpu-node-0")
	writer.RestoreLastStatus(map[string]string{"GPUError": "Unknown"})
	writer.heartbeatCounter = heartbeatCycles - 1
	writer.jitterOffset = 0
	if err := writer.WriteConditions(context.Background(), []rules.Result{
		{ConditionType: "GPUError", Unknown: true, Reason: "MetricCoverageUnavailable"},
	}); err != nil {
		t.Fatal(err)
	}
	got := patches(t, client)
	if len(got) != 1 {
		t.Fatalf("expected one heartbeat, got %d", len(got))
	}
	condition := conditionOf(t, got[0], "GPUError")
	if condition.Status != corev1.ConditionUnknown || !condition.LastTransitionTime.IsZero() {
		t.Fatalf("restored Unknown changed status or transition time: %+v", condition)
	}
}
