// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rules

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/state"
)

// Rule defines a threshold check against a scraped metric.
type Rule struct {
	Name          string            `yaml:"name"`
	MetricName    string            `yaml:"metricName"`
	Labels        map[string]string `yaml:"labels,omitempty"`
	ConditionType string            `yaml:"conditionType"`
	// Threshold evaluation mode.
	// "instant": fire if current value > Threshold
	// "rate": fire if increase over Window > Threshold
	Mode      string        `yaml:"mode"` // "instant" or "rate"
	Threshold float64       `yaml:"threshold"`
	Window    time.Duration `yaml:"window,omitempty"`
	// Duration the condition must persist before firing.
	For time.Duration `yaml:"for,omitempty"`
}

// Result is the evaluation outcome of a single rule.
type Result struct {
	ConditionType string
	Firing        bool
	Reason        string
	Message       string
}

// Engine evaluates rules against scraped metrics.
type Engine struct {
	rules       []Rule
	mu          sync.Mutex
	history     map[string][]sample  // metric key → time series for rate calculations
	pending     map[string]time.Time // conditionType → first time condition was met (for "for" duration)
	evalCounter int                  // tracks cycles for periodic cleanup
	retention   time.Duration        // how long to keep history samples
}

type sample struct {
	time  time.Time
	value float64
}

// NewEngine creates a rule engine.
func NewEngine(rules []Rule) *Engine {
	// Compute max window across all rate rules for history retention.
	var maxWindow time.Duration
	for _, r := range rules {
		if r.Window > maxWindow {
			maxWindow = r.Window
		}
	}
	// Retain at least 15m, or the longest rule window + 1m buffer.
	retention := maxWindow + 1*time.Minute
	if retention < 15*time.Minute {
		retention = 15 * time.Minute
	}

	return &Engine{
		rules:     rules,
		history:   make(map[string][]sample),
		pending:   make(map[string]time.Time),
		retention: retention,
	}
}

// Evaluate runs all rules against the current metrics and returns results.
func (e *Engine) Evaluate(metrics []scraper.Metric) []Result {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	e.recordMetrics(metrics, now)

	// Cleanup stale history every ~60 cycles (~15min at 15s interval).
	e.evalCounter++
	if e.evalCounter%60 == 0 {
		e.cleanupStaleHistory(now, 1*time.Hour)
	}

	// Build index for fast lookup.
	idx := indexMetrics(metrics)

	var results []Result
	for _, rule := range e.rules {
		r := e.evaluateRule(rule, idx, now)
		results = append(results, r)
	}

	return results
}

func (e *Engine) evaluateRule(rule Rule, idx map[string][]scraper.Metric, now time.Time) Result {
	result := Result{
		ConditionType: rule.ConditionType,
		Firing:        false,
		Reason:        rule.ConditionType + "Ok",
		Message:       "",
	}

	matched := matchMetrics(idx, rule.MetricName, rule.Labels)
	if len(matched) == 0 {
		return result
	}

	var firing bool
	switch rule.Mode {
	case "instant":
		for _, m := range matched {
			if m.Value > rule.Threshold {
				firing = true
				result.Message = "metric value exceeds threshold"
				break
			}
		}
	case "rate":
		for _, m := range matched {
			key := metricKey(m.Name, m.Labels)
			increase := e.computeRate(key, rule.Window, now)
			if increase > rule.Threshold {
				firing = true
				result.Message = "metric rate of increase exceeds threshold"
				break
			}
		}
	default:
		slog.Warn("unknown rule mode", "rule", rule.Name, "mode", rule.Mode)
		return result
	}

	if !firing {
		delete(e.pending, rule.ConditionType)
		return result
	}

	// Apply "for" duration if configured.
	if rule.For > 0 {
		firstSeen, exists := e.pending[rule.ConditionType]
		if !exists {
			e.pending[rule.ConditionType] = now
			return result // Not firing yet — pending duration.
		}
		if now.Sub(firstSeen) < rule.For {
			return result // Still within "for" window.
		}
	}

	result.Firing = true
	result.Reason = rule.ConditionType
	return result
}

func (e *Engine) recordMetrics(metrics []scraper.Metric, now time.Time) {
	for _, m := range metrics {
		key := metricKey(m.Name, m.Labels)
		e.history[key] = append(e.history[key], sample{time: now, value: m.Value})
		e.pruneHistory(key, now, e.retention)
	}
}

func (e *Engine) pruneHistory(key string, now time.Time, maxAge time.Duration) {
	samples := e.history[key]
	cutoff := now.Add(-maxAge)
	i := 0
	for i < len(samples) && samples[i].time.Before(cutoff) {
		i++
	}
	if i > 0 {
		e.history[key] = samples[i:]
	}
}

func (e *Engine) computeRate(key string, window time.Duration, now time.Time) float64 {
	samples := e.history[key]
	if len(samples) < 2 {
		return 0
	}

	cutoff := now.Add(-window)
	var oldest *sample
	for i := range samples {
		if !samples[i].time.Before(cutoff) {
			oldest = &samples[i]
			break
		}
	}
	if oldest == nil {
		return 0
	}

	latest := samples[len(samples)-1]
	increase := latest.value - oldest.value
	if increase < 0 {
		// Counter reset detected — use latest value as the increase since reset.
		return latest.value
	}
	return increase
}

func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	key := name
	for _, k := range keys {
		key += "|" + k + "=" + labels[k]
	}
	return key
}

// cleanupStaleHistory removes history entries for metrics not seen within maxAge.
func (e *Engine) cleanupStaleHistory(now time.Time, maxAge time.Duration) {
	cutoff := now.Add(-maxAge)
	for key, samples := range e.history {
		if len(samples) == 0 || samples[len(samples)-1].time.Before(cutoff) {
			delete(e.history, key)
		}
	}
}

func indexMetrics(metrics []scraper.Metric) map[string][]scraper.Metric {
	idx := make(map[string][]scraper.Metric)
	for _, m := range metrics {
		idx[m.Name] = append(idx[m.Name], m)
	}
	return idx
}

func matchMetrics(idx map[string][]scraper.Metric, name string, labels map[string]string) []scraper.Metric {
	candidates := idx[name]
	if len(labels) == 0 {
		return candidates
	}

	var matched []scraper.Metric
	for _, m := range candidates {
		if labelsMatch(m.Labels, labels) {
			matched = append(matched, m)
		}
	}
	return matched
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ExportState returns the engine's history and pending state for persistence.
func (e *Engine) ExportState() (map[string][]state.Sample, map[string]time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	history := make(map[string][]state.Sample, len(e.history))
	for k, samples := range e.history {
		exported := make([]state.Sample, len(samples))
		for i, s := range samples {
			exported[i] = state.Sample{Time: s.time, Value: s.value}
		}
		history[k] = exported
	}

	pending := make(map[string]time.Time, len(e.pending))
	for k, v := range e.pending {
		pending[k] = v
	}

	return history, pending
}

// RestoreState loads previously persisted history and pending state.
func (e *Engine) RestoreState(history map[string][]state.Sample, pending map[string]time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	for k, samples := range history {
		restored := make([]sample, 0, len(samples))
		for _, s := range samples {
			if now.Sub(s.Time) <= e.retention {
				restored = append(restored, sample{time: s.Time, value: s.Value})
			}
		}
		if len(restored) > 0 {
			e.history[k] = restored
		}
	}

	for k, v := range pending {
		e.pending[k] = v
	}
}
