// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"math"
	"testing"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
)

func TestLoadMetricCoverage(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `
scrapeTargets:
  - name: dcgm
    url: http://localhost:9400/metrics
rules:
  - name: ecc
    metricName: DCGM_FI_DEV_ECC_DBE_VOL_TOTAL
    conditionType: GPUECC
    mode: rate
    window: 1m
    minSamples: 8
    sampleLabel: UUID
    maxSampleAge: 45s
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireMetricCoverage(); err != nil {
		t.Fatal(err)
	}
	rule := cfg.Rules[0]
	if rule.MinSamples != 8 || rule.SampleLabel != "UUID" || rule.MaxSampleAge != 45*time.Second {
		t.Fatalf("coverage contract was not parsed: %+v", rule)
	}
	if err := (&Config{Rules: []rules.Rule{{Name: "legacy"}}}).RequireMetricCoverage(); err == nil {
		t.Fatal("coverage flag accepted a stale config with no coverage contract")
	}
}

func TestValidateMetricCoverage(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*rules.Rule){
		"negative count":      func(r *rules.Rule) { r.MinSamples = -1 },
		"label without count": func(r *rules.Rule) { r.MinSamples = 0 },
		"negative age":        func(r *rules.Rule) { r.MaxSampleAge = -time.Second },
		"missing name":        func(r *rules.Rule) { r.Name = "" },
		"missing metric":      func(r *rules.Rule) { r.MetricName = "" },
		"missing condition":   func(r *rules.Rule) { r.ConditionType = "" },
		"reserved condition":  func(r *rules.Rule) { r.ConditionType = "Ready" },
		"invalid label":       func(r *rules.Rule) { r.SampleLabel = "gpu-id" },
		"invalid mode":        func(r *rules.Rule) { r.Mode = "unknown" },
		"missing rate window": func(r *rules.Rule) { r.Window = 0 },
		"negative for":        func(r *rules.Rule) { r.For = -time.Second },
		"nonfinite threshold": func(r *rules.Rule) { r.Threshold = math.NaN() },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rule := rules.Rule{
				Name: "ecc", MetricName: "ecc", ConditionType: "ECCError",
				Mode: "rate", Window: time.Minute, MinSamples: 8, SampleLabel: "UUID",
			}
			mutate(&rule)
			if err := validateCoverage(rule); err == nil {
				t.Fatalf("invalid coverage contract accepted: %+v", rule)
			}
		})
	}
}
