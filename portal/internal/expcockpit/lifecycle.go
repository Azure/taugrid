// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"strings"
	"time"
)

const (
	defaultNotRespondingAfter = 15 * time.Minute
	defaultUnknownAfter       = 60 * time.Minute
)

type LifecycleEvidence struct {
	ExplicitOutcome          string
	ExplicitReason           string
	ExplicitSource           string
	TerminalAt               time.Time
	LatestMetricAt           time.Time
	LatestControlPlaneAt     time.Time
	WorkloadAbsent           bool
	WorkloadAbsenceConfirmed time.Time
	Now                      time.Time
	NotRespondingAfter       time.Duration
	UnknownAfter             time.Duration
}

type LifecycleTruth struct {
	OutcomeState             string
	LivenessState            string
	Reason                   string
	Source                   string
	LastEvidenceAt           time.Time
	Freshness                time.Duration
	Explicit                 bool
	WorkloadAbsenceConfirmed bool
}

func ResolveLifecycle(evidence LifecycleEvidence) LifecycleTruth {
	now := evidence.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	notRespondingAfter := evidence.NotRespondingAfter
	if notRespondingAfter <= 0 {
		notRespondingAfter = defaultNotRespondingAfter
	}
	unknownAfter := evidence.UnknownAfter
	if unknownAfter <= 0 {
		unknownAfter = defaultUnknownAfter
	}

	if outcome := terminalOutcome(evidence.ExplicitOutcome); outcome != "" {
		at := evidence.TerminalAt.UTC()
		return LifecycleTruth{
			OutcomeState:   outcome,
			Reason:         firstNonEmptyString(strings.TrimSpace(evidence.ExplicitReason), "run emitted an explicit terminal outcome"),
			Source:         firstNonEmptyString(strings.TrimSpace(evidence.ExplicitSource), "explicit_terminal"),
			LastEvidenceAt: at,
			Freshness:      evidenceAge(now, at),
			Explicit:       true,
		}
	}

	latest, source := latestLifecycleEvidence(evidence.LatestMetricAt, evidence.LatestControlPlaneAt)
	if latest.IsZero() {
		return LifecycleTruth{
			Reason: "run has no terminal outcome or current liveness evidence",
			Source: "unavailable",
		}
	}
	age := evidenceAge(now, latest)
	if age <= notRespondingAfter {
		return LifecycleTruth{
			LivenessState:  "running",
			Reason:         "run has recent " + source + " evidence and no terminal outcome",
			Source:         source,
			LastEvidenceAt: latest,
			Freshness:      age,
		}
	}
	if evidence.WorkloadAbsent && age >= unknownAfter && !evidence.WorkloadAbsenceConfirmed.IsZero() {
		return LifecycleTruth{
			LivenessState:            "unknown",
			Reason:                   "run is beyond the liveness grace period and its workload absence was explicitly confirmed",
			Source:                   "workload_absence",
			LastEvidenceAt:           latest,
			Freshness:                age,
			WorkloadAbsenceConfirmed: true,
		}
	}
	return LifecycleTruth{
		LivenessState:  "not_responding",
		Reason:         "run has no recent liveness evidence and no terminal outcome",
		Source:         source,
		LastEvidenceAt: latest,
		Freshness:      age,
	}
}

func (truth LifecycleTruth) legacyState() string {
	if truth.OutcomeState != "" {
		return truth.OutcomeState
	}
	switch truth.LivenessState {
	case "running":
		return "running"
	case "not_responding":
		return "stale"
	case "unknown":
		return "unknown"
	default:
		return "pending"
	}
}

func applyLifecycleTruth(run *RunView, truth LifecycleTruth) {
	run.OutcomeState = truth.OutcomeState
	run.LivenessState = truth.LivenessState
	run.LifecycleReason = truth.Reason
	run.LifecycleSource = truth.Source
	run.LifecycleExplicit = truth.Explicit
	run.WorkloadAbsenceConfirmed = truth.WorkloadAbsenceConfirmed
	run.LifecycleState = truth.legacyState()
	if run.State == "" || run.State == "pending" || run.State == "running" || run.State == "stale" || run.State == "unknown" {
		run.State = run.LifecycleState
	}
	if !truth.LastEvidenceAt.IsZero() {
		run.LastEvidenceAt = truth.LastEvidenceAt.UTC().Format(time.RFC3339Nano)
		freshnessSeconds := int64(truth.Freshness / time.Second)
		run.FreshnessSeconds = &freshnessSeconds
	}
}

func terminalOutcome(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "succeeded", "success", "completed", "complete", "done":
		return "succeeded"
	case "failed", "failure":
		return "failed"
	case "cancelled", "canceled", "cancel":
		return "cancelled"
	default:
		return ""
	}
}

func latestLifecycleEvidence(metricAt, controlPlaneAt time.Time) (time.Time, string) {
	metricAt = metricAt.UTC()
	controlPlaneAt = controlPlaneAt.UTC()
	switch {
	case metricAt.IsZero():
		return controlPlaneAt, "control_plane"
	case controlPlaneAt.IsZero(), metricAt.After(controlPlaneAt):
		return metricAt, "metrics"
	default:
		return controlPlaneAt, "control_plane"
	}
}

func evidenceAge(now, evidenceAt time.Time) time.Duration {
	if evidenceAt.IsZero() || now.Before(evidenceAt) {
		return 0
	}
	return now.Sub(evidenceAt)
}

func parseLifecycleTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
