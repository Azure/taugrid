// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"testing"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
	"gopkg.in/yaml.v3"
)

func TestMetricSetYAML(t *testing.T) {
	t.Parallel()
	input := `
scrapeTargets:
  - name: dcgm
    url: http://localhost:9400/metrics
rules:
  - name: links
    metricNames: [crc_l0, crc_l1]
    conditionType: LinkError
    mode: rate
    threshold: 0
    window: 1m
    minSamples: 8
    sampleLabel: UUID
`
	var config Config
	if err := yaml.Unmarshal([]byte(input), &config); err != nil {
		t.Fatal(err)
	}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	if config.Rules[0].MetricName != "" || len(config.Rules[0].MetricNames) != 2 {
		t.Fatalf("metric set selector was lost: %+v", config.Rules[0])
	}
	// A coverage reader that ignores metricNames still rejects the empty selector.
	config.Rules[0].MetricNames = nil
	if err := config.validate(); err == nil {
		t.Fatal("ignoring the new selector produced a valid, unmonitored rule")
	}
}

func TestMetricSetContract(t *testing.T) {
	t.Parallel()
	valid := func() rules.Rule {
		return rules.Rule{
			Name: "links", MetricNames: []string{"crc_l0", "crc_l1"},
			ConditionType: "LinkError", Mode: "instant", MinSamples: 8, SampleLabel: "UUID",
		}
	}
	if err := validateCoverage(valid()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*rules.Rule)
	}{
		{"ambiguous selector", func(r *rules.Rule) { r.MetricName = "other" }},
		{"empty set", func(r *rules.Rule) { r.MetricNames = []string{} }},
		{"duplicate", func(r *rules.Rule) { r.MetricNames = []string{"crc_l0", "crc_l0"} }},
		{"blank name", func(r *rules.Rule) { r.MetricNames = []string{"crc_l0", ""} }},
		{"wildcard", func(r *rules.Rule) { r.MetricNames = []string{"crc_*"} }},
		{"no coverage", func(r *rules.Rule) { r.MinSamples = 0 }},
		{"no physical identity", func(r *rules.Rule) { r.SampleLabel = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			rule := valid()
			test.mutate(&rule)
			if err := validateCoverage(rule); err == nil {
				t.Fatal("invalid multi-metric coverage accepted")
			}
		})
	}
}
