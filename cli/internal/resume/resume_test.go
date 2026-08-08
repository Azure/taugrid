// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package resume

import (
	"testing"

	"github.com/Azure/taugrid/core/status"
)

func TestClassifyFailure_OOMKilled(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{Phase: "Failed", OOMKilled: true},
		},
	}
	reason := ClassifyFailure(snap)
	if reason != ReasonOOMKilled {
		t.Errorf("got %v, want OOMKilled", reason)
	}
	if !IsRetryable(reason) {
		t.Error("OOMKilled should be retryable")
	}
	if !IsOOM(reason) {
		t.Error("OOMKilled should return true for IsOOM")
	}
}

func TestClassifyFailure_OOMKilledContainerReason(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{
				Phase: "Failed",
				Containers: []status.Container{
					{Reason: "OOMKilled"},
				},
			},
		},
	}
	if got := ClassifyFailure(snap); got != ReasonOOMKilled {
		t.Errorf("got %v, want OOMKilled", got)
	}
}

func TestClassifyFailure_OOMKilledLastReason(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{
				Phase: "Failed",
				Containers: []status.Container{
					{LastReason: "OOMKilled"},
				},
			},
		},
	}
	if got := ClassifyFailure(snap); got != ReasonOOMKilled {
		t.Errorf("got %v, want OOMKilled", got)
	}
}

func TestClassifyFailure_Preempted_DisruptionTarget(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{
				Phase: "Failed",
				Conditions: []status.Condition{
					{Type: "DisruptionTarget", Status: "True"},
				},
			},
		},
	}
	reason := ClassifyFailure(snap)
	if reason != ReasonPreempted {
		t.Errorf("got %v, want Preempted", reason)
	}
	if !IsRetryable(reason) {
		t.Error("Preempted should be retryable")
	}
	if IsOOM(reason) {
		t.Error("Preempted should not be OOM")
	}
}

func TestClassifyFailure_Preempted_Event(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{Phase: "Failed"},
		},
		Events: []status.Event{
			{Reason: "Preempted", Message: "preempted by higher priority workload"},
		},
	}
	if got := ClassifyFailure(snap); got != ReasonPreempted {
		t.Errorf("got %v, want Preempted", got)
	}
}

func TestClassifyFailure_Evicted(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{
				Phase: "Failed",
				Conditions: []status.Condition{
					{Reason: "Evicted"},
				},
			},
		},
	}
	reason := ClassifyFailure(snap)
	if reason != ReasonEvicted {
		t.Errorf("got %v, want Evicted", reason)
	}
	if !IsRetryable(reason) {
		t.Error("Evicted should be retryable")
	}
}

func TestClassifyFailure_Evicted_Event(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{Phase: "Failed"},
		},
		Events: []status.Event{
			{Reason: "Evicted"},
		},
	}
	if got := ClassifyFailure(snap); got != ReasonEvicted {
		t.Errorf("got %v, want Evicted", got)
	}
}

func TestClassifyFailure_Completed(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Complete", Status: "True"},
		},
	}
	reason := ClassifyFailure(snap)
	if reason != ReasonCompleted {
		t.Errorf("got %v, want Completed", reason)
	}
	if IsRetryable(reason) {
		t.Error("Completed should not be retryable")
	}
}

func TestClassifyFailure_Running(t *testing.T) {
	snap := status.Snapshot{
		JobFound:  true,
		JobActive: 1,
	}
	reason := ClassifyFailure(snap)
	if reason != ReasonRunning {
		t.Errorf("got %v, want Running", reason)
	}
	if IsRetryable(reason) {
		t.Error("Running should not be retryable")
	}
}

func TestClassifyFailure_Unknown(t *testing.T) {
	snap := status.Snapshot{
		JobFound: true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{Phase: "Failed"},
		},
	}
	reason := ClassifyFailure(snap)
	if reason != ReasonUnknown {
		t.Errorf("got %v, want Unknown", reason)
	}
	if IsRetryable(reason) {
		t.Error("Unknown should not be retryable")
	}
}

func TestClassifyFailure_RayJobFailed(t *testing.T) {
	snap := status.Snapshot{
		RayJob: status.RayJob{
			Found:               true,
			JobDeploymentStatus: "Failed",
		},
		Pods: []status.Pod{
			{Phase: "Failed", OOMKilled: true},
		},
	}
	if got := ClassifyFailure(snap); got != ReasonOOMKilled {
		t.Errorf("got %v, want OOMKilled", got)
	}
}

func TestFailureReason_String(t *testing.T) {
	tests := []struct {
		reason FailureReason
		want   string
	}{
		{ReasonUnknown, "Unknown"},
		{ReasonOOMKilled, "OOMKilled"},
		{ReasonPreempted, "Preempted"},
		{ReasonEvicted, "Evicted"},
		{ReasonCompleted, "Completed"},
		{ReasonRunning, "Running"},
	}
	for _, tt := range tests {
		if got := tt.reason.String(); got != tt.want {
			t.Errorf("FailureReason(%d).String() = %q, want %q", tt.reason, got, tt.want)
		}
	}
}
