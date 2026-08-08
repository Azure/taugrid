// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"testing"
	"time"
)

func TestResolveLifecyclePrecedenceAndLiveness(t *testing.T) {
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		evidence LifecycleEvidence
		outcome  string
		liveness string
		source   string
		explicit bool
		absence  bool
		legacy   string
	}{
		{
			name: "explicit terminal outcome wins over recent metrics",
			evidence: LifecycleEvidence{
				ExplicitOutcome: "failed",
				ExplicitSource:  "tau_terminal_marker",
				TerminalAt:      now.Add(-time.Minute),
				LatestMetricAt:  now,
				Now:             now,
			},
			outcome:  "failed",
			source:   "tau_terminal_marker",
			explicit: true,
			legacy:   "failed",
		},
		{
			name: "recent metrics mean running",
			evidence: LifecycleEvidence{
				LatestMetricAt: now.Add(-14 * time.Minute),
				Now:            now,
			},
			liveness: "running",
			source:   "metrics",
			legacy:   "running",
		},
		{
			name: "stale metrics are reversibly not responding",
			evidence: LifecycleEvidence{
				LatestMetricAt: now.Add(-16 * time.Minute),
				Now:            now,
			},
			liveness: "not_responding",
			source:   "metrics",
			legacy:   "stale",
		},
		{
			name: "absence flag without confirmation cannot become unknown",
			evidence: LifecycleEvidence{
				LatestMetricAt: now.Add(-2 * time.Hour),
				WorkloadAbsent: true,
				Now:            now,
			},
			liveness: "not_responding",
			source:   "metrics",
			legacy:   "stale",
		},
		{
			name: "confirmed absence after grace becomes unknown",
			evidence: LifecycleEvidence{
				LatestMetricAt:           now.Add(-61 * time.Minute),
				WorkloadAbsent:           true,
				WorkloadAbsenceConfirmed: now.Add(-time.Minute),
				Now:                      now,
			},
			liveness: "unknown",
			source:   "workload_absence",
			absence:  true,
			legacy:   "unknown",
		},
		{
			name: "control plane evidence can restore running",
			evidence: LifecycleEvidence{
				LatestMetricAt:       now.Add(-2 * time.Hour),
				LatestControlPlaneAt: now.Add(-time.Minute),
				Now:                  now,
			},
			liveness: "running",
			source:   "control_plane",
			legacy:   "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveLifecycle(tt.evidence)
			if got.OutcomeState != tt.outcome {
				t.Fatalf("OutcomeState = %q, want %q", got.OutcomeState, tt.outcome)
			}
			if got.LivenessState != tt.liveness {
				t.Fatalf("LivenessState = %q, want %q", got.LivenessState, tt.liveness)
			}
			if got.Source != tt.source {
				t.Fatalf("Source = %q, want %q", got.Source, tt.source)
			}
			if got.Explicit != tt.explicit {
				t.Fatalf("Explicit = %t, want %t", got.Explicit, tt.explicit)
			}
			if got.WorkloadAbsenceConfirmed != tt.absence {
				t.Fatalf("WorkloadAbsenceConfirmed = %t, want %t", got.WorkloadAbsenceConfirmed, tt.absence)
			}
			if got.legacyState() != tt.legacy {
				t.Fatalf("legacyState() = %q, want %q", got.legacyState(), tt.legacy)
			}
		})
	}
}

func TestResolveLifecycleNeverInfersCancellation(t *testing.T) {
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	for _, evidence := range []LifecycleEvidence{
		{LatestMetricAt: now.Add(-24 * time.Hour), Now: now},
		{
			LatestMetricAt:           now.Add(-24 * time.Hour),
			WorkloadAbsent:           true,
			WorkloadAbsenceConfirmed: now.Add(-time.Hour),
			Now:                      now,
		},
	} {
		got := ResolveLifecycle(evidence)
		if got.OutcomeState != "" || got.legacyState() == "cancelled" {
			t.Fatalf("silence/absence inferred cancellation: %#v", got)
		}
	}
}

func TestResolveLifecycleLateTerminalReconcilesUnknown(t *testing.T) {
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	evidence := LifecycleEvidence{
		LatestMetricAt:           now.Add(-2 * time.Hour),
		WorkloadAbsent:           true,
		WorkloadAbsenceConfirmed: now.Add(-time.Hour),
		Now:                      now,
	}
	if got := ResolveLifecycle(evidence); got.LivenessState != "unknown" {
		t.Fatalf("initial LivenessState = %q, want unknown", got.LivenessState)
	}

	evidence.ExplicitOutcome = "succeeded"
	evidence.ExplicitSource = "tau_terminal_marker"
	evidence.TerminalAt = now.Add(time.Minute)
	got := ResolveLifecycle(evidence)
	if got.OutcomeState != "succeeded" || got.LivenessState != "" || !got.Explicit {
		t.Fatalf("late terminal truth = %#v, want explicit succeeded outcome", got)
	}
}
