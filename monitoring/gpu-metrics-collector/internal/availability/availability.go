// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package availability turns required scrape-target reachability into durable
// Node conditions.
//
// The rule engine can only report on metrics it received. When a required
// exporter disappears, every rule that reads its metrics evaluates to "not
// firing", which is indistinguishable from a healthy node. This package closes
// that gap by tracking each required target's reachability directly and
// emitting a dedicated condition for it.
//
// The condition reports endpoint reachability only. It is deliberately distinct
// from DCGM diagnostic health (NPD's dcgmi checks): a reachable exporter can
// still report unhealthy GPUs, and an unreachable exporter says nothing about
// the GPUs themselves.
package availability

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/state"
)

// Tracker debounces required-target scrape outcomes into condition results.
//
// Ownership: one condition type is owned by exactly one target within a
// collector (enforced at config load), and a node runs exactly one collector
// per profile (enforced by the chart's disjoint instance-type affinity), so no
// two writers contend for the same condition.
type Tracker struct {
	mu       sync.Mutex
	states   map[string]*targetState // conditionType → debounce state
	reserved map[string]struct{}     // condition types owned by rules, never published here
	orphans  []string                // conditions published by a previous run that are no longer configured
}

type targetState struct {
	failingSince time.Time
	healthySince time.Time
	firing       bool
	// established records that this process has enough evidence to own the
	// condition: restored continuity, a sustained failure, or a sustained
	// success. Until then the tracker publishes nothing for it.
	//
	// A restart with a missing, corrupt, or stale snapshot starts with
	// firing=false and no history. Publishing that would clear an outage the
	// server is already reporting, on the strength of a scrape that just
	// failed. Staying silent instead leaves the server's condition untouched
	// until this process has actually proven reachability for availableFor, or
	// proven failure for unavailableFor.
	established bool
}

// New creates a Tracker for the required targets in the given set. Targets
// without an availability condition are ignored.
//
// ruleConditions lists the condition types owned by the rule engine. The
// tracker never publishes those, so a condition type that moves from a scrape
// target to a rule is not clobbered by a stale clear from the old owner.
func New(targets []scraper.ScrapeTarget, ruleConditions []string) *Tracker {
	t := &Tracker{
		states:   make(map[string]*targetState),
		reserved: make(map[string]struct{}, len(ruleConditions)),
	}
	for _, cond := range ruleConditions {
		if cond != "" {
			t.reserved[cond] = struct{}{}
		}
	}
	for _, target := range targets {
		if target.AvailabilityCondition == "" {
			continue
		}
		t.states[target.AvailabilityCondition] = &targetState{}
	}
	return t
}

// Tracked reports how many conditions this tracker owns.
func (t *Tracker) Tracked() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.states)
}

// Evaluate folds this cycle's scrape outcomes into the debounce state and
// returns results for the conditions this process can speak to.
//
// A tracked condition is reported as an explicit False once reachability has
// been proven, rather than being silently absent. It is deliberately omitted
// while this process has no evidence yet — see targetState.established — so an
// unseeded restart cannot clear an outage the server is already reporting.
func (t *Tracker) Evaluate(statuses []scraper.TargetStatus, now time.Time) []rules.Result {
	t.mu.Lock()
	defer t.mu.Unlock()

	var results []rules.Result
	for _, status := range statuses {
		cond := status.Target.AvailabilityCondition
		if cond == "" {
			continue
		}
		st, ok := t.states[cond]
		if !ok {
			st = &targetState{}
			t.states[cond] = st
		}
		result, publish := evaluateTarget(st, status, now)
		if publish {
			results = append(results, result)
		}
	}

	// A condition this collector no longer owns cannot be deleted from the Node,
	// and nothing else will ever clear it. Publish an explicit False so a
	// de-configured or renamed condition does not strand a node as unhealthy.
	//
	// This happens exactly once. Re-publishing every cycle would keep asserting
	// a stale False for the rest of the process lifetime, and results share one
	// strategic merge patch keyed by condition type, where the last entry wins.
	for _, cond := range t.orphans {
		results = append(results, rules.Result{
			ConditionType: cond,
			Firing:        false,
			Reason:        cond + "Ok",
			Message:       "availability tracking is no longer configured for this scrape target",
		})
	}
	t.orphans = nil

	return results
}

func evaluateTarget(st *targetState, status scraper.TargetStatus, now time.Time) (rules.Result, bool) {
	target := status.Target
	cond := target.AvailabilityCondition
	unavailableFor := target.UnavailableWindow()
	availableFor := target.AvailableWindow()

	if status.OK {
		st.failingSince = time.Time{}
		if st.healthySince.IsZero() {
			st.healthySince = now
		}
		if now.Sub(st.healthySince) >= availableFor {
			// Proven reachable for the full window: safe to own and to clear.
			st.firing = false
			st.established = true
		}
	} else {
		st.healthySince = time.Time{}
		if st.failingSince.IsZero() {
			st.failingSince = now
		}
		if now.Sub(st.failingSince) >= unavailableFor {
			st.firing = true
			st.established = true
		}
	}

	if !st.established {
		return rules.Result{}, false
	}

	result := rules.Result{ConditionType: cond, Firing: st.firing}
	if st.firing {
		result.Reason = cond
	} else {
		result.Reason = cond + "Ok"
	}

	switch {
	case status.OK && st.firing:
		result.Message = fmt.Sprintf("scrape target %q at %s reachable for %s; condition clears after %s",
			target.Name, status.SafeURL, roundDuration(now.Sub(st.healthySince)), availableFor)
	case status.OK:
		result.Message = fmt.Sprintf("scrape target %q at %s reachable", target.Name, status.SafeURL)
	case st.firing:
		result.Message = fmt.Sprintf("scrape target %q at %s unavailable for %s: %s",
			target.Name, status.SafeURL, roundDuration(now.Sub(st.failingSince)), status.Err)
	default:
		result.Message = fmt.Sprintf("scrape target %q at %s unavailable for %s (condition sets after %s): %s",
			target.Name, status.SafeURL, roundDuration(now.Sub(st.failingSince)), unavailableFor, status.Err)
	}

	return result, true
}

func roundDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}

// ExportState returns the debounce state for persistence across restarts, so a
// collector restart neither re-arms a failure timer from zero nor clears a
// firing condition before the recovery window elapses.
func (t *Tracker) ExportState() map[string]state.Availability {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make(map[string]state.Availability, len(t.states))
	for cond, st := range t.states {
		out[cond] = state.Availability{
			FailingSince: st.failingSince,
			HealthySince: st.healthySince,
			Firing:       st.firing,
			Established:  st.established,
		}
	}
	return out
}

// RestoreState loads previously persisted debounce state, captured at savedAt
// and restored at now.
//
// The collector's own downtime is not evidence of anything, so both timers are
// shifted forward by the gap between savedAt and now: a restart neither counts
// downtime toward the failure window (which would let one failed scrape set the
// condition immediately) nor toward the recovery window (which would let one
// successful scrape clear it). Progress made before the restart is preserved,
// as is a firing condition.
//
// Conditions that are no longer configured are recorded as orphans so they can
// be cleared once instead of being stranded True on the Node forever.
func (t *Tracker) RestoreState(saved map[string]state.Availability, savedAt, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	gap := now.Sub(savedAt)
	if gap < 0 || savedAt.IsZero() {
		gap = 0
	}

	for cond, s := range saved {
		st, ok := t.states[cond]
		if !ok {
			// A condition type that a rule now owns must not be cleared here:
			// the rule is the live owner, and a stale clear sharing the same
			// merge key would overwrite a genuinely firing condition.
			if _, claimed := t.reserved[cond]; !claimed {
				t.orphans = append(t.orphans, cond)
			}
			continue
		}
		st.failingSince = shift(s.FailingSince, gap)
		st.healthySince = shift(s.HealthySince, gap)
		st.firing = s.Firing
		// Only continuity with a run that was actually publishing this
		// condition lets this process speak to it immediately. A snapshot from
		// a run that had not yet proven the state (or one written before this
		// field existed) leaves it unestablished, so the restored process still
		// has to prove the state before it can patch.
		st.established = s.Established
	}
	sort.Strings(t.orphans)
}

func shift(t time.Time, gap time.Duration) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Add(gap)
}
