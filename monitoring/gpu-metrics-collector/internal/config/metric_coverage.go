// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"fmt"
	"math"
	"regexp"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
)

var labelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// RequireMetricCoverage rejects a missing contract during a coverage-enabled rollout.
// The corresponding CLI flag is deliberately unknown to old images.
func (c *Config) RequireMetricCoverage() error {
	for _, rule := range c.Rules {
		if rule.MinSamples > 0 {
			return nil
		}
	}
	return fmt.Errorf("metric coverage is required but no rule configures minSamples")
}

func validateCoverage(r rules.Rule) error {
	if r.MinSamples < 0 || r.MaxSampleAge < 0 {
		return fmt.Errorf("rule %q has negative minSamples or maxSampleAge", r.Name)
	}
	if r.MinSamples == 0 {
		if r.SampleLabel != "" || r.MaxSampleAge != 0 {
			return fmt.Errorf("rule %q requires minSamples when sampleLabel or maxSampleAge is set", r.Name)
		}
		return nil
	}
	if r.Name == "" || r.MetricName == "" || r.ConditionType == "" {
		return fmt.Errorf("metric coverage rule requires name, metricName, and conditionType")
	}
	if _, reserved := kubernetesCoreConditionTypes[r.ConditionType]; reserved {
		return fmt.Errorf("rule %q condition %q is owned by Kubernetes", r.Name, r.ConditionType)
	}
	if r.SampleLabel != "" && !labelNamePattern.MatchString(r.SampleLabel) {
		return fmt.Errorf("rule %q sampleLabel must be a Prometheus label name", r.Name)
	}
	if r.Mode != "instant" && r.Mode != "rate" {
		return fmt.Errorf("rule %q metric coverage requires instant or rate mode", r.Name)
	}
	if (r.Mode == "rate" && r.Window <= 0) || r.For < 0 {
		return fmt.Errorf("rule %q requires a positive rate window and nonnegative for duration", r.Name)
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return fmt.Errorf("rule %q threshold must be finite", r.Name)
	}
	return nil
}
