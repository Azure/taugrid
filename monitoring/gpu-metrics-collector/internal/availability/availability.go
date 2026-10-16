// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

type Tracker struct {
	mu       sync.Mutex
	states   map[string]*targetState
	reserved map[string]struct{}
	orphans  []string
}

type targetState struct {
	failingSince time.Time
	healthySince time.Time
	firing       bool
	established  bool
}

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

func (t *Tracker) Tracked() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.states)
}

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
			if _, claimed := t.reserved[cond]; !claimed {
				t.orphans = append(t.orphans, cond)
			}
			continue
		}
		st.failingSince = shift(s.FailingSince, gap)
		st.healthySince = shift(s.HealthySince, gap)
		st.firing = s.Firing
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
