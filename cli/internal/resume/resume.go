// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package resume

import (
	"strings"

	"github.com/Azure/taugrid/core/status"
)

type FailureReason int

const (
	ReasonUnknown   FailureReason = iota
	ReasonOOMKilled               // container OOMKilled
	ReasonPreempted               // Kueue preemption or pod disruption
	ReasonEvicted                 // node-pressure eviction
	ReasonCompleted               // workload succeeded — not a failure
	ReasonRunning                 // workload still in progress
)

func (r FailureReason) String() string {
	switch r {
	case ReasonOOMKilled:
		return "OOMKilled"
	case ReasonPreempted:
		return "Preempted"
	case ReasonEvicted:
		return "Evicted"
	case ReasonCompleted:
		return "Completed"
	case ReasonRunning:
		return "Running"
	default:
		return "Unknown"
	}
}

func IsRetryable(reason FailureReason) bool {
	return reason == ReasonOOMKilled || reason == ReasonPreempted || reason == ReasonEvicted
}

func IsOOM(reason FailureReason) bool {
	return reason == ReasonOOMKilled
}

func ClassifyFailure(snap status.Snapshot) FailureReason {
	if status.StartupComplete(snap) {
		return ReasonCompleted
	}
	if !status.StartupFailed(snap) {
		return ReasonRunning
	}

	for _, p := range snap.Pods {
		if p.OOMKilled {
			return ReasonOOMKilled
		}
		for _, c := range allContainers(p) {
			if c.Reason == "OOMKilled" || c.LastReason == "OOMKilled" {
				return ReasonOOMKilled
			}
		}
	}

	for _, p := range snap.Pods {
		for _, cond := range p.Conditions {
			if cond.Type == "DisruptionTarget" && cond.Status == "True" {
				return ReasonPreempted
			}
		}
	}
	for _, ev := range snap.Events {
		reason := strings.ToLower(ev.Reason)
		if reason == "preempted" || strings.Contains(reason, "preempt") {
			return ReasonPreempted
		}
	}

	for _, p := range snap.Pods {
		if p.Phase == "Failed" {
			if hasConditionReason(p.Conditions, "Evicted") {
				return ReasonEvicted
			}
		}
	}
	for _, ev := range snap.Events {
		if strings.EqualFold(ev.Reason, "Evicted") {
			return ReasonEvicted
		}
	}

	return ReasonUnknown
}

func allContainers(p status.Pod) []status.Container {
	out := make([]status.Container, 0, len(p.InitContainers)+len(p.Containers))
	out = append(out, p.InitContainers...)
	out = append(out, p.Containers...)
	return out
}

func hasConditionReason(conds []status.Condition, reason string) bool {
	for _, c := range conds {
		if strings.EqualFold(c.Reason, reason) {
			return true
		}
	}
	return false
}
