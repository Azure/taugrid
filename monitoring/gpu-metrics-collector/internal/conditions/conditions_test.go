// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conditions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
)

func newFakeNode(name string) *fake.Clientset {
	return fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func patches(t *testing.T, client *fake.Clientset) []nodeConditionsPatch {
	t.Helper()
	var out []nodeConditionsPatch
	for _, action := range client.Actions() {
		p, ok := action.(k8stesting.PatchAction)
		if !ok {
			continue
		}
		var decoded nodeConditionsPatch
		if err := json.Unmarshal(p.GetPatch(), &decoded); err != nil {
			t.Fatalf("decoding patch: %v", err)
		}
		out = append(out, decoded)
	}
	return out
}

func conditionOf(t *testing.T, patch nodeConditionsPatch, condType string) corev1.NodeCondition {
	t.Helper()
	for _, c := range patch.Status.Conditions {
		if string(c.Type) == condType {
			return c
		}
	}
	t.Fatalf("patch has no condition %q", condType)
	return corev1.NodeCondition{}
}

func TestWriteConditionsPatchesAvailabilityCondition(t *testing.T) {
	t.Parallel()

	client := newFakeNode("gpu-node-0")
	w := NewWriter(client, "gpu-node-0")

	msg := `scrape target "dcgm-exporter" at http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics unavailable for 2m0s: connection refused`
	err := w.WriteConditions(context.Background(), []rules.Result{
		{ConditionType: "GPUECCDoubleRetired", Firing: false, Reason: "GPUECCDoubleRetiredOk"},
		{ConditionType: "DcgmExporterUnavailable", Firing: true, Reason: "DcgmExporterUnavailable", Message: msg},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := patches(t, client)
	if len(got) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(got))
	}

	cond := conditionOf(t, got[0], "DcgmExporterUnavailable")
	if cond.Status != corev1.ConditionTrue {
		t.Errorf("expected True, got %s", cond.Status)
	}
	if cond.Reason != "DcgmExporterUnavailable" || cond.Message != msg {
		t.Errorf("unexpected reason/message: %q / %q", cond.Reason, cond.Message)
	}
	if cond.LastTransitionTime.IsZero() {
		t.Error("first write must set LastTransitionTime")
	}
	if strings.Contains(cond.Message, "token=") || strings.Contains(cond.Message, "@") {
		t.Errorf("message must not carry credentials or query secrets: %q", cond.Message)
	}

	// A rule condition and an availability condition coexist in one patch.
	if len(got[0].Status.Conditions) != 2 {
		t.Errorf("expected both conditions in the patch, got %d", len(got[0].Status.Conditions))
	}
}

func TestWriteConditionsPatchesOnRecoveryTransition(t *testing.T) {
	t.Parallel()

	client := newFakeNode("gpu-node-0")
	w := NewWriter(client, "gpu-node-0")
	ctx := context.Background()

	firing := []rules.Result{{
		ConditionType: "DcgmExporterUnavailable",
		Firing:        true,
		Reason:        "DcgmExporterUnavailable",
		Message:       "unavailable",
	}}
	recovered := []rules.Result{{
		ConditionType: "DcgmExporterUnavailable",
		Firing:        false,
		Reason:        "DcgmExporterUnavailableOk",
		Message:       `scrape target "dcgm-exporter" at http://localhost:19400/metrics reachable`,
	}}

	if err := w.WriteConditions(ctx, firing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Repeating the same status must not record another transition. It may
	// still emit a throttled heartbeat patch, whose cycle depends on the
	// writer's per-node random jitter offset, so assert on transitions rather
	// than on a raw patch count.
	if err := w.WriteConditions(ctx, firing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := w.WriteConditions(ctx, recovered); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := patches(t, client)
	if len(got) < 2 {
		t.Fatalf("expected at least the set and clear patches, got %d", len(got))
	}

	// LastTransitionTime is set only on an actual status change, so exactly two
	// patches should carry one: the set and the clear.
	var transitions []corev1.NodeCondition
	for _, patch := range got {
		cond := conditionOf(t, patch, "DcgmExporterUnavailable")
		if !cond.LastTransitionTime.IsZero() {
			transitions = append(transitions, cond)
		}
	}
	if len(transitions) != 2 {
		t.Fatalf("expected exactly 2 status transitions, got %d", len(transitions))
	}
	if transitions[0].Status != corev1.ConditionTrue {
		t.Errorf("expected the first transition to be True, got %s", transitions[0].Status)
	}
	if transitions[1].Status != corev1.ConditionFalse {
		t.Errorf("expected False after recovery, got %s", transitions[1].Status)
	}
	if transitions[1].Reason != "DcgmExporterUnavailableOk" {
		t.Errorf("unexpected recovery reason %q", transitions[1].Reason)
	}

	// The final patch must reflect the recovered state.
	if cond := conditionOf(t, got[len(got)-1], "DcgmExporterUnavailable"); cond.Status != corev1.ConditionFalse {
		t.Errorf("expected the last patch to be False, got %s", cond.Status)
	}
}
