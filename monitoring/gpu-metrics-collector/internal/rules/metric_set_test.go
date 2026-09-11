// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rules

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
)

func linkRule() Rule {
	rule := continuousRule()
	rule.MetricName = ""
	rule.MetricNames = []string{"crc_l0", "crc_l1"}
	return rule
}

func linkSamples(at time.Time) []scraper.Metric {
	var metrics []scraper.Metric
	for _, name := range []string{"crc_l0", "crc_l1"} {
		for _, id := range []string{"a", "b"} {
			metric := gpuSample(id, 400, at)
			metric.Name = name
			metrics = append(metrics, metric)
		}
	}
	return metrics
}

func TestMetricSetCoverage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func([]scraper.Metric) []scraper.Metric
	}{
		{"missing whole link", func(m []scraper.Metric) []scraper.Metric { return m[:2] }},
		{"missing one GPU on one link", func(m []scraper.Metric) []scraper.Metric { return m[:3] }},
		{"duplicate GPU cannot replace missing peer", func(m []scraper.Metric) []scraper.Metric {
			m[3] = m[2]
			return m
		}},
		{"different physical devices across links", func(m []scraper.Metric) []scraper.Metric {
			m[3].Labels["UUID"] = "c"
			return m
		}},
		{"nonfinite link", func(m []scraper.Metric) []scraper.Metric {
			m[3].Value = math.NaN()
			return m
		}},
		{"stale link", func(m []scraper.Metric) []scraper.Metric {
			m[3].Timestamp = time.Now().Add(-3 * time.Minute)
			return m
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rule := linkRule()
			rule.Threshold = 500
			result := NewEngine([]Rule{rule}).Evaluate(test.mutate(linkSamples(time.Now())))[0]
			if !result.Unknown || result.Firing {
				t.Fatalf("incomplete link coverage reported healthy: %+v", result)
			}
		})
	}
}

func TestMetricSetRatesAndFaultPrecedence(t *testing.T) {
	t.Parallel()
	rule := linkRule()
	rule.Mode, rule.Window = "rate", time.Minute
	engine := NewEngine([]Rule{rule})
	now := time.Now().Add(-20 * time.Second)
	if result := engine.Evaluate(linkSamples(now))[0]; !result.Unknown || result.Firing {
		t.Fatalf("first observation must warm up: %+v", result)
	}
	if result := engine.Evaluate(linkSamples(now.Add(time.Second)))[0]; result.Unknown || result.Firing {
		t.Fatalf("unchanged accumulated counters are not new errors: %+v", result)
	}
	if result := engine.Evaluate(linkSamples(now.Add(2 * time.Second))[:3])[0]; !result.Unknown {
		t.Fatalf("one missing physical link must break coverage: %+v", result)
	}
	if result := engine.Evaluate(linkSamples(now.Add(3 * time.Second)))[0]; !result.Unknown {
		t.Fatalf("restored link needs a new baseline: %+v", result)
	}
	engine.Evaluate(linkSamples(now.Add(4 * time.Second)))
	fault := linkSamples(now.Add(5 * time.Second))
	fault[0].Value = 401
	fault[2].Value = 0 // A different link resetting must not hide a real increase.
	if result := engine.Evaluate(fault[:3])[0]; !result.Firing || result.Unknown {
		t.Fatalf("a measured fault must take precedence over incomplete coverage: %+v", result)
	}
}

func TestMetricSetDoesNotRestoreLinkHistory(t *testing.T) {
	t.Parallel()
	rule := linkRule()
	rule.Mode, rule.Window = "rate", time.Minute
	old := NewEngine([]Rule{rule})
	old.Evaluate(linkSamples(time.Now().Add(-time.Second)))
	history, pending := old.ExportState()
	next := NewEngine([]Rule{rule})
	next.RestoreState(history, pending)
	if result := next.Evaluate(linkSamples(time.Now()))[0]; !result.Unknown || result.Firing {
		t.Fatalf("restart bridged unobserved link counters: %+v", result)
	}
}

func TestH200MetricSetRequiresAllEighteenLinksOnEightGPUs(t *testing.T) {
	t.Parallel()
	rule := linkRule()
	rule.MetricNames, rule.MinSamples, rule.Threshold = nil, 8, 500
	var metrics []scraper.Metric
	for link := range 18 {
		name := fmt.Sprintf("crc_l%d", link)
		rule.MetricNames = append(rule.MetricNames, name)
		for gpu := range 8 {
			metric := gpuSample(fmt.Sprintf("gpu%d", gpu), 0, time.Now())
			metric.Name = name
			metrics = append(metrics, metric)
		}
	}
	engine := NewEngine([]Rule{rule})
	if result := engine.Evaluate(metrics)[0]; result.Unknown || result.Firing {
		t.Fatalf("complete 144-series coverage rejected: %+v", result)
	}
	if result := engine.Evaluate(metrics[:len(metrics)-1])[0]; !result.Unknown {
		t.Fatalf("143 of 144 physical link samples reported healthy: %+v", result)
	}
}
