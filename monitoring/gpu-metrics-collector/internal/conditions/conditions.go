// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conditions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
)

const (
	heartbeatCycles = 20
)

type Writer struct {
	clientset        kubernetes.Interface
	nodeName         string
	lastStatus       map[string]corev1.ConditionStatus
	heartbeatCounter int
	jitterOffset     int
}

func NewWriter(clientset kubernetes.Interface, nodeName string) *Writer {
	return &Writer{
		clientset:    clientset,
		nodeName:     nodeName,
		lastStatus:   make(map[string]corev1.ConditionStatus),
		jitterOffset: rand.IntN(heartbeatCycles),
	}
}

func (w *Writer) WriteConditions(ctx context.Context, results []rules.Result) error {
	now := metav1.NewTime(time.Now())
	var conditions []corev1.NodeCondition
	changed := false

	for _, r := range results {
		status := resultStatus(r)

		cond := corev1.NodeCondition{
			Type:              corev1.NodeConditionType(r.ConditionType),
			Status:            status,
			LastHeartbeatTime: now,
			Reason:            r.Reason,
			Message:           r.Message,
		}

		if prev, ok := w.lastStatus[r.ConditionType]; ok && prev == status {
		} else {
			cond.LastTransitionTime = now
			changed = true
		}

		conditions = append(conditions, cond)
	}

	if len(conditions) == 0 {
		return nil
	}

	w.heartbeatCounter++
	if !changed && (w.heartbeatCounter+w.jitterOffset)%heartbeatCycles != 0 {
		return nil
	}

	patch := nodeConditionsPatch{
		Status: nodeStatusPatch{Conditions: conditions},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	_, err = w.clientset.CoreV1().Nodes().Patch(ctx, w.nodeName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		return fmt.Errorf("patching node %s conditions: %w", w.nodeName, err)
	}

	for _, r := range results {
		w.lastStatus[r.ConditionType] = resultStatus(r)
	}

	firingCount := 0
	for _, r := range results {
		if r.Firing {
			firingCount++
			slog.Info("condition firing", "type", r.ConditionType, "reason", r.Reason)
		}
	}
	slog.Debug("conditions written", "total", len(results), "firing", firingCount)

	return nil
}

func resultStatus(r rules.Result) corev1.ConditionStatus {
	if r.Firing {
		return corev1.ConditionTrue
	}
	if r.Unknown {
		return corev1.ConditionUnknown
	}
	return corev1.ConditionFalse
}

type nodeConditionsPatch struct {
	Status nodeStatusPatch `json:"status"`
}

type nodeStatusPatch struct {
	Conditions []corev1.NodeCondition `json:"conditions"`
}

func (p nodeStatusPatch) MarshalJSON() ([]byte, error) {
	type conditionPatch struct {
		Type               corev1.NodeConditionType `json:"type"`
		Status             corev1.ConditionStatus   `json:"status"`
		LastHeartbeatTime  metav1.Time              `json:"lastHeartbeatTime"`
		LastTransitionTime *metav1.Time             `json:"lastTransitionTime,omitempty"`
		Reason             string                   `json:"reason"`
		Message            string                   `json:"message"`
	}
	conditions := make([]conditionPatch, 0, len(p.Conditions))
	for _, condition := range p.Conditions {
		patch := conditionPatch{
			Type: condition.Type, Status: condition.Status,
			LastHeartbeatTime: condition.LastHeartbeatTime,
			Reason:            condition.Reason, Message: condition.Message,
		}
		if !condition.LastTransitionTime.IsZero() {
			patch.LastTransitionTime = &condition.LastTransitionTime
		}
		conditions = append(conditions, patch)
	}
	return json.Marshal(struct {
		Conditions []conditionPatch `json:"conditions"`
	}{Conditions: conditions})
}

func (w *Writer) ExportLastStatus() map[string]string {
	out := make(map[string]string, len(w.lastStatus))
	for k, v := range w.lastStatus {
		out[k] = string(v)
	}
	return out
}

func (w *Writer) RestoreLastStatus(statuses map[string]string) {
	for k, v := range statuses {
		w.lastStatus[k] = corev1.ConditionStatus(v)
	}
}
