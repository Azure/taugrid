// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rules

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
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
	// MinSamples opts a continuous rule into coverage checks. Zero preserves
	// optional/sparse-event behavior. SampleLabel counts distinct identities.
	MinSamples   int           `yaml:"minSamples,omitempty"`
	SampleLabel  string        `yaml:"sampleLabel,omitempty"`
	MaxSampleAge time.Duration `yaml:"maxSampleAge,omitempty"`
}

// Result is the evaluation outcome of a single rule.
type Result struct {
	ConditionType string
	Firing        bool
	Unknown       bool
	Reason        string
	Message       string
}

const DefaultMaxSampleAge = 2 * time.Minute

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
	e.removeMissingHistory(metrics)
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
	valid := make([]scraper.Metric, 0, len(matched))
	identities := make(map[string]struct{}, len(matched))
	maxAge := rule.MaxSampleAge
	if maxAge == 0 {
		maxAge = DefaultMaxSampleAge
	}
	for _, m := range matched {
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			continue
		}
		if rule.MinSamples > 0 && !m.Timestamp.IsZero() &&
			(m.Timestamp.Before(now.Add(-maxAge)) || m.Timestamp.After(now.Add(maxAge))) {
			continue
		}
		if rule.SampleLabel != "" {
			identity := m.Labels[rule.SampleLabel]
			if identity == "" {
				continue
			}
			identities[identity] = struct{}{}
		}
		valid = append(valid, m)
	}
	observed := len(valid)
	if rule.SampleLabel != "" {
		observed = len(identities)
	}
	if rule.MinSamples > 0 && (observed < rule.MinSamples || len(valid) != len(matched)) {
		result.Unknown = true
		result.Reason = "MetricCoverageUnavailable"
		result.Message = fmt.Sprintf("metric %q has %d valid samples/identities; requires at least %d; missing, invalid, or stale input is not healthy",
			rule.MetricName, observed, rule.MinSamples)
	}
	if len(matched) == 0 {
		delete(e.pending, rule.ConditionType)
		return result
	}

	var firing bool
	switch rule.Mode {
	case "instant":
		for _, m := range valid {
			if m.Value > rule.Threshold {
				firing = true
				result.Message = "metric value exceeds threshold"
				break
			}
		}
	case "rate":
		for _, m := range valid {
			key := metricKey(m.Name, m.Labels)
			increase, known := e.computeRate(key, rule.Window, now, rule.MinSamples > 0, maxAge)
			if !known && rule.MinSamples > 0 {
				result.Unknown = true
				result.Reason = "MetricHistoryUnavailable"
				result.Message = fmt.Sprintf("metric %q requires two current, consecutive counter observations; an interrupted baseline is not healthy", rule.MetricName)
				continue
			}
			if increase > rule.Threshold {
				firing = true
			}
		}
	default:
		slog.Warn("unknown rule mode", "rule", rule.Name, "mode", rule.Mode)
		if rule.MinSamples > 0 {
			result.Unknown = true
			result.Reason = "InvalidRuleMode"
			result.Message = "continuous metric rule has an unsupported evaluation mode"
		}
		delete(e.pending, rule.ConditionType)
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
	result.Unknown = false
	result.Reason = rule.ConditionType
	if rule.Mode == "rate" {
		result.Message = "metric rate of increase exceeds threshold"
	}
	return result
}

func (e *Engine) recordMetrics(metrics []scraper.Metric, now time.Time) {
	for _, m := range metrics {
		key := metricKey(m.Name, m.Labels)
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			delete(e.history, key)
			continue
		}
		observedAt := now
		if !m.Timestamp.IsZero() {
			observedAt = m.Timestamp
		}
		history := e.history[key]
		if len(history) > 0 && !observedAt.After(history[len(history)-1].time) {
			if observedAt.Equal(history[len(history)-1].time) && m.Value == history[len(history)-1].value {
				continue
			}
			delete(e.history, key)
		}
		e.history[key] = append(e.history[key], sample{time: observedAt, value: m.Value})
		e.pruneHistory(key, now, e.retention)
	}
}

func (e *Engine) removeMissingHistory(metrics []scraper.Metric) {
	present := make(map[string]struct{}, len(metrics))
	for _, m := range metrics {
		present[metricKey(m.Name, m.Labels)] = struct{}{}
	}
	for key := range e.history {
		if _, ok := present[key]; !ok {
			delete(e.history, key)
		}
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

func (e *Engine) computeRate(key string, window time.Duration, now time.Time, requireCoverage bool, maxAge time.Duration) (float64, bool) {
	samples := e.history[key]
	if len(samples) < 2 {
		return 0, false
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
		return 0, false
	}

	latest := samples[len(samples)-1]
	if !latest.time.After(oldest.time) {
		return 0, false
	}
	if requireCoverage {
		previous := samples[len(samples)-2]
		if latest.time.Sub(previous.time) > maxAge || previous.time.After(now.Add(maxAge)) {
			e.history[key] = samples[len(samples)-1:]
			return 0, false
		}
	}
	increase := latest.value - oldest.value
	if increase < 0 {
		// Counter reset detected — use latest value as the increase since reset.
		return latest.value, true
	}
	return increase, true
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
		required := false
		for _, rule := range e.rules {
			if rule.MinSamples > 0 && (k == rule.MetricName || strings.HasPrefix(k, rule.MetricName+"|")) {
				required = true
				break
			}
		}
		if required {
			continue // Collector downtime cannot establish continuous coverage.
		}
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
		required := false
		for _, rule := range e.rules {
			if rule.MinSamples > 0 && rule.ConditionType == k {
				required = true
				break
			}
		}
		if !required {
			e.pending[k] = v
		}
	}
}
