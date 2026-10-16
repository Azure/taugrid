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

type Rule struct {
	Name                 string            `yaml:"name"`
	MetricName           string            `yaml:"metricName,omitempty"`
	MetricNames          []string          `yaml:"metricNames,omitempty"`
	Labels               map[string]string `yaml:"labels,omitempty"`
	ConditionType        string            `yaml:"conditionType"`
	Mode                 string            `yaml:"mode"`
	Threshold            float64           `yaml:"threshold"`
	Window               time.Duration     `yaml:"window,omitempty"`
	For                  time.Duration     `yaml:"for,omitempty"`
	MinSamples           int               `yaml:"minSamples,omitempty"`
	SampleLabel          string            `yaml:"sampleLabel,omitempty"`
	RequiredSampleValues []string          `yaml:"requiredSampleValues,omitempty"`
	MaxSampleAge         time.Duration     `yaml:"maxSampleAge,omitempty"`
}

func (r Rule) MetricSources() []string {
	if len(r.MetricNames) > 0 {
		return r.MetricNames
	}
	return []string{r.MetricName}
}

type Result struct {
	ConditionType string
	Firing        bool
	Unknown       bool
	Reason        string
	Message       string
}

const DefaultMaxSampleAge = 2 * time.Minute

type Engine struct {
	rules              []Rule
	historyMetricNames map[string]struct{}
	mu                 sync.Mutex
	history            map[string][]sample
	rateBreaks         map[int]map[string]int
	pending            map[string]time.Time
	evalCounter        int
	retention          time.Duration
}

type sample struct {
	time  time.Time
	value float64
	cycle int
}

func NewEngine(rules []Rule) *Engine {
	var maxWindow time.Duration
	historyMetricNames := make(map[string]struct{})
	for _, r := range rules {
		if r.Window > maxWindow {
			maxWindow = r.Window
		}
		if r.Mode == "rate" {
			for _, name := range r.MetricSources() {
				historyMetricNames[name] = struct{}{}
			}
		}
	}
	retention := maxWindow + 1*time.Minute
	if retention < 15*time.Minute {
		retention = 15 * time.Minute
	}

	return &Engine{
		rules:              rules,
		historyMetricNames: historyMetricNames,
		history:            make(map[string][]sample),
		rateBreaks:         make(map[int]map[string]int),
		pending:            make(map[string]time.Time),
		retention:          retention,
	}
}

func (e *Engine) Evaluate(metrics []scraper.Metric) []Result {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	e.evalCounter++
	e.breakMissingRateContinuity(metrics)
	e.recordMetrics(metrics, now, e.evalCounter)

	if e.evalCounter%60 == 0 {
		e.cleanupStaleHistory(now, 1*time.Hour)
	}

	idx := indexMetrics(metrics)

	var results []Result
	for ruleIndex, rule := range e.rules {
		r := e.evaluateRule(ruleIndex, rule, idx, now)
		results = append(results, r)
	}

	return results
}

func (e *Engine) evaluateRule(ruleIndex int, rule Rule, idx map[string][]scraper.Metric, now time.Time) Result {
	result := Result{
		ConditionType: rule.ConditionType,
		Firing:        false,
		Reason:        rule.ConditionType + "Ok",
		Message:       "",
	}
	if rule.MinSamples > 0 {
		result.Reason = rule.ConditionType + "Observed"
	}

	var valid []scraper.Metric
	var commonIdentities map[string]struct{}
	matchedCount := 0
	requiredIdentities := make(map[string]struct{}, len(rule.RequiredSampleValues))
	for _, identity := range rule.RequiredSampleValues {
		requiredIdentities[identity] = struct{}{}
	}
	maxAge := rule.MaxSampleAge
	if maxAge == 0 {
		maxAge = DefaultMaxSampleAge
	}
	for sourceIndex, name := range rule.MetricSources() {
		matched := matchMetrics(idx, name, rule.Labels)
		identities := make(map[string]struct{}, len(matched))
		validCount := 0
		eligibleCount := 0
		for _, m := range matched {
			key := metricKey(m.Name, m.Labels)
			identity := ""
			if rule.SampleLabel != "" {
				identity = m.Labels[rule.SampleLabel]
				if len(requiredIdentities) > 0 && identity != "" {
					if _, required := requiredIdentities[identity]; !required {
						continue
					}
				}
			}
			eligibleCount++
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
				e.breakRateContinuity(ruleIndex, rule, key)
				continue
			}
			if rule.MinSamples > 0 && !m.Timestamp.IsZero() &&
				(m.Timestamp.Before(now.Add(-maxAge)) || m.Timestamp.After(now.Add(maxAge))) {
				e.breakRateContinuity(ruleIndex, rule, key)
				continue
			}
			if rule.SampleLabel != "" {
				if identity == "" {
					e.breakRateContinuity(ruleIndex, rule, key)
					continue
				}
				identities[identity] = struct{}{}
			}
			validCount++
			valid = append(valid, m)
		}
		matchedCount += eligibleCount
		observed := validCount
		if rule.SampleLabel != "" {
			observed = len(identities)
			if sourceIndex == 0 {
				commonIdentities = identities
			} else {
				for identity := range commonIdentities {
					if _, exists := identities[identity]; !exists {
						delete(commonIdentities, identity)
					}
				}
			}
		}
		if rule.MinSamples > 0 && (observed < rule.MinSamples || validCount != eligibleCount) {
			result.Unknown = true
			result.Reason = "MetricCoverageUnavailable"
			result.Message = fmt.Sprintf("metric %q has %d valid samples/identities; requires at least %d; missing, invalid, or stale input is not healthy",
				name, observed, rule.MinSamples)
		}
	}
	if !result.Unknown && len(rule.MetricNames) > 1 && len(commonIdentities) < rule.MinSamples {
		result.Unknown = true
		result.Reason = "MetricCoverageUnavailable"
		result.Message = fmt.Sprintf("all %d metric families must cover the same %d identities; only %d identities have every required family",
			len(rule.MetricNames), rule.MinSamples, len(commonIdentities))
	}
	if matchedCount == 0 {
		if rule.MinSamples > 0 {
			delete(e.pending, rule.ConditionType)
		}
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
			increase, known := e.computeRate(ruleIndex, key, rule.Window, now, rule.MinSamples > 0, maxAge)
			if !known && rule.MinSamples > 0 {
				result.Unknown = true
				result.Reason = "MetricHistoryUnavailable"
				result.Message = fmt.Sprintf("metric %q requires two current, consecutive counter observations; an interrupted baseline is not healthy", m.Name)
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

	if rule.For > 0 {
		firstSeen, exists := e.pending[rule.ConditionType]
		if !exists {
			e.pending[rule.ConditionType] = now
			return result
		}
		if now.Sub(firstSeen) < rule.For {
			return result
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

func (e *Engine) recordMetrics(metrics []scraper.Metric, now time.Time, cycle int) {
	for _, m := range metrics {
		if _, needed := e.historyMetricNames[m.Name]; !needed {
			continue
		}
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
		e.history[key] = append(e.history[key], sample{time: observedAt, value: m.Value, cycle: cycle})
		e.pruneHistory(key, now, e.retention)
	}
}

func (e *Engine) breakMissingRateContinuity(metrics []scraper.Metric) {
	present := make(map[string]struct{})
	for _, m := range metrics {
		if _, needed := e.historyMetricNames[m.Name]; !needed {
			continue
		}
		present[metricKey(m.Name, m.Labels)] = struct{}{}
	}
	for key := range e.history {
		if _, ok := present[key]; ok {
			continue
		}
		for ruleIndex, rule := range e.rules {
			if ruleUsesMetricKey(rule, key) {
				e.breakRateContinuity(ruleIndex, rule, key)
			}
		}
	}
}

func ruleUsesMetricKey(rule Rule, key string) bool {
	for _, name := range rule.MetricSources() {
		if key == name || strings.HasPrefix(key, name+"|") {
			return true
		}
	}
	return false
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

func (e *Engine) breakRateContinuity(ruleIndex int, rule Rule, key string) {
	if rule.Mode != "rate" || rule.MinSamples == 0 {
		return
	}
	if e.rateBreaks[ruleIndex] == nil {
		e.rateBreaks[ruleIndex] = make(map[string]int)
	}
	e.rateBreaks[ruleIndex][key] = e.evalCounter
}

func (e *Engine) computeRate(ruleIndex int, key string, window time.Duration, now time.Time, requireCoverage bool, maxAge time.Duration) (float64, bool) {
	samples := e.history[key]
	if requireCoverage {
		start := 0
		if breaks := e.rateBreaks[ruleIndex]; breaks != nil {
			breakCycle := breaks[key]
			for start < len(samples) && samples[start].cycle <= breakCycle {
				start++
			}
		}
		for i := start + 1; i < len(samples); i++ {
			if samples[i].time.Sub(samples[i-1].time) > maxAge {
				start = i
			}
		}
		samples = samples[start:]
	}
	if len(samples) < 2 {
		return 0, false
	}

	cutoff := now.Add(-window)
	oldestIndex := sort.Search(len(samples), func(i int) bool {
		return !samples[i].time.Before(cutoff)
	})
	if oldestIndex == len(samples) {
		return 0, false
	}
	oldest := samples[oldestIndex]
	latest := samples[len(samples)-1]
	if !latest.time.After(oldest.time) {
		return 0, false
	}
	increase := latest.value - oldest.value
	if increase < 0 {
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

func metricNameFromKey(key string) string {
	name, _, _ := strings.Cut(key, "|")
	return name
}

func (e *Engine) cleanupStaleHistory(now time.Time, maxAge time.Duration) {
	cutoff := now.Add(-maxAge)
	for key, samples := range e.history {
		if len(samples) == 0 || samples[len(samples)-1].time.Before(cutoff) {
			delete(e.history, key)
			for ruleIndex, breaks := range e.rateBreaks {
				delete(breaks, key)
				if len(breaks) == 0 {
					delete(e.rateBreaks, ruleIndex)
				}
			}
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

func (e *Engine) RestoreState(history map[string][]state.Sample, pending map[string]time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	for k, samples := range history {
		if _, needed := e.historyMetricNames[metricNameFromKey(k)]; !needed {
			continue
		}
		restored := make([]sample, 0, len(samples))
		for _, s := range samples {
			if now.Sub(s.Time) <= e.retention {
				restored = append(restored, sample{time: s.Time, value: s.Value, cycle: e.evalCounter})
			}
		}
		if len(restored) > 0 {
			e.history[k] = restored
			for ruleIndex, rule := range e.rules {
				if rule.MinSamples > 0 && ruleUsesMetricKey(rule, k) {
					e.breakRateContinuity(ruleIndex, rule, k)
				}
			}
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
