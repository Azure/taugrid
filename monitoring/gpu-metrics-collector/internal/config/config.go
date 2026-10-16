// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
)

var kubernetesCoreConditionTypes = map[string]struct{}{
	"Ready":              {},
	"MemoryPressure":     {},
	"DiskPressure":       {},
	"PIDPressure":        {},
	"NetworkUnavailable": {},
}

type Config struct {
	ScrapeTargets []scraper.ScrapeTarget `yaml:"scrapeTargets"`
	Rules         []rules.Rule           `yaml:"rules"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if len(cfg.Rules) == 0 {
		return nil, fmt.Errorf("config must define at least one rule")
	}
	if len(cfg.ScrapeTargets) == 0 {
		return nil, fmt.Errorf("config must define at least one scrapeTarget")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) RuleConditionTypes() []string {
	out := make([]string, 0, len(c.Rules))
	for _, r := range c.Rules {
		if r.ConditionType != "" {
			out = append(out, r.ConditionType)
		}
	}
	return out
}

func (c *Config) validate() error {
	owners := make(map[string]string, len(c.Rules)+len(c.ScrapeTargets))
	coverageOwners := make(map[string]bool)
	for _, r := range c.Rules {
		if err := validateCoverage(r); err != nil {
			return err
		}
		if r.ConditionType == "" {
			continue
		}
		if prev, ok := owners[r.ConditionType]; ok {
			if r.MinSamples > 0 || coverageOwners[r.ConditionType] {
				return fmt.Errorf("condition type %q has multiple rule owners with a metric coverage contract", r.ConditionType)
			}
			slog.Warn("duplicate rule condition type; the last evaluated rule wins",
				"conditionType", r.ConditionType, "owner", prev, "rule", r.Name)
			continue
		}
		owners[r.ConditionType] = fmt.Sprintf("rule %q", r.Name)
		coverageOwners[r.ConditionType] = r.MinSamples > 0
	}

	seenNames := make(map[string]bool, len(c.ScrapeTargets))
	for i, t := range c.ScrapeTargets {
		declaresAvailability := t.Required || t.AvailabilityCondition != ""

		if t.Name == "" {
			if declaresAvailability {
				return fmt.Errorf("scrapeTarget at index %d declares an availability contract but sets no name", i)
			}
			slog.Warn("scrapeTarget has no name", "index", i, "url", scraper.SafeURL(t.URL))
		}
		if t.URL == "" {
			if declaresAvailability {
				return fmt.Errorf("scrapeTarget %q declares an availability contract but sets no url", t.Name)
			}
			slog.Warn("scrapeTarget has no url and can never be scraped", "index", i, "target", t.Name)
		}
		if t.Name != "" {
			if previousDeclaresAvailability, seen := seenNames[t.Name]; seen {
				if declaresAvailability || previousDeclaresAvailability {
					return fmt.Errorf("duplicate scrapeTarget name %q where at least one target declares an availability contract", t.Name)
				}
				slog.Warn("duplicate scrapeTarget name; the endpoint will be scraped more than once", "target", t.Name)
			}
			seenNames[t.Name] = seenNames[t.Name] || declaresAvailability
		}

		if err := validateAvailability(t); err != nil {
			return err
		}
		if t.AvailabilityCondition == "" {
			continue
		}
		if prev, ok := owners[t.AvailabilityCondition]; ok {
			return fmt.Errorf("condition type %q is claimed by both %s and scrapeTarget %q",
				t.AvailabilityCondition, prev, t.Name)
		}
		owners[t.AvailabilityCondition] = fmt.Sprintf("scrapeTarget %q", t.Name)
	}
	return nil
}

func validateAvailability(t scraper.ScrapeTarget) error {
	if t.Required && t.AvailabilityCondition == "" {
		return fmt.Errorf("scrapeTarget %q is required but sets no availabilityCondition; "+
			"a required target must publish its availability as a Node condition", t.Name)
	}
	if t.AvailabilityCondition != "" && !t.Required {
		return fmt.Errorf("scrapeTarget %q sets availabilityCondition without required: true", t.Name)
	}
	if _, reserved := kubernetesCoreConditionTypes[t.AvailabilityCondition]; reserved {
		return fmt.Errorf("scrapeTarget %q availabilityCondition %q is owned by Kubernetes", t.Name, t.AvailabilityCondition)
	}
	if t.UnavailableFor < 0 || t.AvailableFor < 0 {
		return fmt.Errorf("scrapeTarget %q must not set a negative unavailableFor or availableFor", t.Name)
	}
	if !t.Required {
		return nil
	}
	u, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("scrapeTarget %q has an unparseable url", t.Name)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("scrapeTarget %q must use an absolute http or https url", t.Name)
	}
	return nil
}
