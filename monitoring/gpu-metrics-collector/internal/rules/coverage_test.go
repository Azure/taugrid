// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rules

import (
	"math"
	"testing"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
)

func gpuSample(id string, value float64, timestamp time.Time) scraper.Metric {
	return scraper.Metric{
		Name: "gpu_errors", Labels: map[string]string{"UUID": id},
		Value: value, Timestamp: timestamp,
	}
}

func continuousRule() Rule {
	return Rule{
		Name: "gpu-errors", MetricName: "gpu_errors", ConditionType: "GPUError",
		Mode: "instant", MinSamples: 2, SampleLabel: "UUID",
	}
}

func TestContinuousCoverage(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name    string
		metrics []scraper.Metric
		unknown bool
		firing  bool
	}{
		{"empty successful scrape", nil, true, false},
		{"explicit zero on every GPU", []scraper.Metric{gpuSample("a", 0, now), gpuSample("b", 0, now)}, false, false},
		{"missing one GPU", []scraper.Metric{gpuSample("a", 0, now)}, true, false},
		{"duplicate does not replace missing GPU", []scraper.Metric{gpuSample("a", 0, now), gpuSample("a", 0, now)}, true, false},
		{"missing identity", []scraper.Metric{gpuSample("", 0, now), gpuSample("b", 0, now)}, true, false},
		{"NaN", []scraper.Metric{gpuSample("a", math.NaN(), now), gpuSample("b", 0, now)}, true, false},
		{"infinity", []scraper.Metric{gpuSample("a", math.Inf(1), now), gpuSample("b", 0, now)}, true, false},
		{"stale sample", []scraper.Metric{gpuSample("a", 0, now.Add(-3*time.Minute)), gpuSample("b", 0, now)}, true, false},
		{"future sample", []scraper.Metric{gpuSample("a", 0, now.Add(3*time.Minute)), gpuSample("b", 0, now)}, true, false},
		{"known fault with incomplete coverage", []scraper.Metric{gpuSample("a", 1, now)}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := NewEngine([]Rule{continuousRule()}).Evaluate(tt.metrics)[0]
			if result.Unknown != tt.unknown || result.Firing != tt.firing {
				t.Fatalf("got %+v; want Unknown=%t Firing=%t", result, tt.unknown, tt.firing)
			}
			if result.Unknown && (result.Reason != "MetricCoverageUnavailable" || result.Message == "") {
				t.Fatalf("missing actionable coverage reason: %+v", result)
			}
		})
	}
}

func TestSparseEventAbsenceIsNotMissingContinuousCoverage(t *testing.T) {
	t.Parallel()
	rule := Rule{
		Name: "xid-48", MetricName: "xid_errors", ConditionType: "XIDError48",
		Mode: "instant", Labels: map[string]string{"err_code": "48"},
	}
	for _, metrics := range [][]scraper.Metric{
		nil,
		{{Name: "xid_errors", Labels: map[string]string{"err_code": "43"}, Value: 1}},
	} {
		result := NewEngine([]Rule{rule}).Evaluate(metrics)[0]
		if result.Unknown || result.Firing {
			t.Fatalf("sparse event absence was mistaken for unavailable continuous input: %+v", result)
		}
	}
}

func TestContinuousRateRequiresUninterruptedBaseline(t *testing.T) {
	t.Parallel()
	rule := continuousRule()
	rule.Mode, rule.Window = "rate", time.Minute
	engine := NewEngine([]Rule{rule})
	now := time.Now().Add(-30 * time.Second)
	both := func(at time.Time) []scraper.Metric {
		return []scraper.Metric{gpuSample("a", 0, at), gpuSample("b", 0, at)}
	}
	assertUnknown := func(metrics []scraper.Metric, want bool) {
		t.Helper()
		result := engine.Evaluate(metrics)[0]
		if result.Unknown != want || result.Firing {
			t.Fatalf("got %+v; want Unknown=%t", result, want)
		}
	}
	assertUnknown(both(now), true)
	assertUnknown(both(now.Add(time.Second)), false)
	assertUnknown([]scraper.Metric{gpuSample("a", 0, now.Add(2*time.Second))}, true)
	assertUnknown(both(now.Add(3*time.Second)), true)
	assertUnknown(both(now.Add(4*time.Second)), false)
}

func TestContinuousRateDoesNotBridgeLongCollectionGap(t *testing.T) {
	t.Parallel()
	rule := continuousRule()
	rule.MinSamples, rule.Mode, rule.Window = 1, "rate", 10*time.Minute
	engine := NewEngine([]Rule{rule})
	engine.Evaluate([]scraper.Metric{gpuSample("a", 0, time.Now().Add(-5*time.Minute))})
	result := engine.Evaluate([]scraper.Metric{gpuSample("a", 0, time.Now())})[0]
	if !result.Unknown {
		t.Fatalf("collection gap was counted as continuous healthy data: %+v", result)
	}
}

func TestMissingInputBreaksPendingDuration(t *testing.T) {
	t.Parallel()
	rule := continuousRule()
	rule.MinSamples, rule.For = 1, time.Minute
	engine := NewEngine([]Rule{rule})
	metrics := []scraper.Metric{gpuSample("a", 1, time.Time{})}
	engine.Evaluate(metrics)
	engine.pending[rule.ConditionType] = time.Now().Add(-2 * time.Minute)
	if result := engine.Evaluate(nil)[0]; !result.Unknown || result.Firing {
		t.Fatalf("missing input must be Unknown: %+v", result)
	}
	if result := engine.Evaluate(metrics)[0]; result.Firing {
		t.Fatalf("missing interval satisfied the for duration: %+v", result)
	}
}

func TestRequiredRateDoesNotRestoreEvidenceAcrossRestart(t *testing.T) {
	t.Parallel()
	rule := continuousRule()
	rule.MinSamples, rule.Mode, rule.Window = 1, "rate", time.Minute
	old := NewEngine([]Rule{rule})
	old.Evaluate([]scraper.Metric{gpuSample("a", 0, time.Time{})})
	history, pending := old.ExportState()
	pending[rule.ConditionType] = time.Now().Add(-time.Hour)
	engine := NewEngine([]Rule{rule})
	engine.RestoreState(history, pending)
	if result := engine.Evaluate([]scraper.Metric{gpuSample("a", 0, time.Time{})})[0]; !result.Unknown {
		t.Fatalf("old snapshot established a current rate baseline: %+v", result)
	}
	if _, exists := engine.pending[rule.ConditionType]; exists {
		t.Fatal("required rule restored a pending timer through unobserved downtime")
	}
}

func TestRateWarmupOnAnotherGPUIsUnknownWhileFaultIsPending(t *testing.T) {
	t.Parallel()
	rule := continuousRule()
	rule.Mode, rule.Window, rule.For = "rate", time.Minute, time.Minute
	engine := NewEngine([]Rule{rule})
	engine.Evaluate([]scraper.Metric{gpuSample("a", 0, time.Time{})})
	result := engine.Evaluate([]scraper.Metric{gpuSample("a", 1, time.Time{}), gpuSample("b", 0, time.Time{})})[0]
	if !result.Unknown || result.Firing {
		t.Fatalf("pending fault hid missing rate history on the other GPU: %+v", result)
	}
}
