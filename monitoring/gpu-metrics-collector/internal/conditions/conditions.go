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
	// heartbeatInterval controls how often we patch the API when nothing changed.
	// With the default 15s scrape interval, 20 cycles ≈ 5 minutes.
	// Note: if --scrape-interval is changed, the effective heartbeat period scales accordingly.
	heartbeatCycles = 20
)

// Writer patches Node conditions based on rule evaluation results.
// It tracks the last-known status per condition type so that
// LastTransitionTime is only updated on actual status changes.
// Heartbeat patches are jittered to avoid thundering herd at scale.
type Writer struct {
	clientset        kubernetes.Interface
	nodeName         string
	lastStatus       map[string]corev1.ConditionStatus // conditionType → last written status
	heartbeatCounter int                               // tracks cycles for throttled heartbeats
	jitterOffset     int                               // random offset so not all nodes patch at the same cycle
}

// NewWriter creates a new condition Writer.
func NewWriter(clientset kubernetes.Interface, nodeName string) *Writer {
	return &Writer{
		clientset:    clientset,
		nodeName:     nodeName,
		lastStatus:   make(map[string]corev1.ConditionStatus),
		jitterOffset: rand.IntN(heartbeatCycles), // each node gets a random offset
	}
}

// WriteConditions patches the Node's status conditions based on rule results.
func (w *Writer) WriteConditions(ctx context.Context, results []rules.Result) error {
	now := metav1.NewTime(time.Now())
	var conditions []corev1.NodeCondition
	changed := false

	for _, r := range results {
		status := corev1.ConditionFalse
		if r.Firing {
			status = corev1.ConditionTrue
		}

		cond := corev1.NodeCondition{
			Type:              corev1.NodeConditionType(r.ConditionType),
			Status:            status,
			LastHeartbeatTime: now,
			Reason:            r.Reason,
			Message:           r.Message,
		}

		if prev, ok := w.lastStatus[r.ConditionType]; ok && prev == status {
			// Status unchanged — keep existing transition time on the server.
			// We still include the condition so heartbeat updates.
		} else {
			// Status changed (or first write) — set transition time.
			cond.LastTransitionTime = now
			changed = true
		}

		conditions = append(conditions, cond)
	}

	if len(conditions) == 0 {
		return nil
	}

	// Patch immediately on status change. Otherwise heartbeat every ~5min, jittered.
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

	// Update last-known status after successful patch.
	for _, r := range results {
		status := corev1.ConditionFalse
		if r.Firing {
			status = corev1.ConditionTrue
		}
		w.lastStatus[r.ConditionType] = status
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

type nodeConditionsPatch struct {
	Status nodeStatusPatch `json:"status"`
}

type nodeStatusPatch struct {
	Conditions []corev1.NodeCondition `json:"conditions"`
}

// ExportLastStatus returns the last-known condition statuses for persistence.
func (w *Writer) ExportLastStatus() map[string]string {
	out := make(map[string]string, len(w.lastStatus))
	for k, v := range w.lastStatus {
		out[k] = string(v)
	}
	return out
}

// RestoreLastStatus loads previously persisted condition statuses.
func (w *Writer) RestoreLastStatus(statuses map[string]string) {
	for k, v := range statuses {
		w.lastStatus[k] = corev1.ConditionStatus(v)
	}
}
