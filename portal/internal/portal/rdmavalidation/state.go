// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import "time"

const (
	StatePassed  = "passed"
	StateFailed  = "failed"
	StateRunning = "running"
	StateUnknown = "unknown"
	StateStale   = "stale"

	FreshnessFresh         = "fresh"
	FreshnessStale         = "stale"
	FreshnessUnknown       = "unknown"
	FreshnessNotApplicable = "not_applicable"
)

func DeriveState(now, observedAt, validUntil time.Time, historicalStatus string, running, verified bool) (string, string, *int64) {
	if running {
		return StateRunning, FreshnessNotApplicable, ageSeconds(now, observedAt)
	}
	if !verified || observedAt.IsZero() || validUntil.IsZero() {
		return StateUnknown, FreshnessUnknown, ageSeconds(now, observedAt)
	}
	if now.After(validUntil) {
		return StateStale, FreshnessStale, ageSeconds(now, observedAt)
	}
	switch historicalStatus {
	case "pass":
		return StatePassed, FreshnessFresh, ageSeconds(now, observedAt)
	case "fail":
		return StateFailed, FreshnessFresh, ageSeconds(now, observedAt)
	default:
		return StateUnknown, FreshnessFresh, ageSeconds(now, observedAt)
	}
}

func ageSeconds(now, observedAt time.Time) *int64 {
	if observedAt.IsZero() || now.Before(observedAt) {
		return nil
	}
	age := int64(now.Sub(observedAt) / time.Second)
	return &age
}
