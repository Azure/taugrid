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

// Config is the top-level collector configuration.
type Config struct {
	ScrapeTargets []scraper.ScrapeTarget `yaml:"scrapeTargets"`
	Rules         []rules.Rule           `yaml:"rules"`
}

// Load reads config from a YAML file.
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

// validate fails closed on any configuration that would leave a required target
// unmonitored or leave two writers contending for one Node condition.
//
// Hard failures are limited to the availability contract, which no existing
// config uses. Shapes that a previous version accepted stay accepted and are
// only warned about: refusing to start would turn an image bump into a
// CrashLoopBackOff that freezes every condition this collector owns, which is
// strictly worse than the degraded-but-running behavior it replaces.
func (c *Config) validate() error {
	owners := make(map[string]string, len(c.Rules)+len(c.ScrapeTargets))
	for _, r := range c.Rules {
		if r.ConditionType == "" {
			continue
		}
		if prev, ok := owners[r.ConditionType]; ok {
			slog.Warn("duplicate rule condition type; the last evaluated rule wins",
				"conditionType", r.ConditionType, "owner", prev, "rule", r.Name)
			continue
		}
		owners[r.ConditionType] = fmt.Sprintf("rule %q", r.Name)
	}

	seenNames := make(map[string]bool, len(c.ScrapeTargets))
	for i, t := range c.ScrapeTargets {
		declaresAvailability := t.Required || t.AvailabilityCondition != ""

		// Never interpolate a raw target URL: it may carry userinfo or query
		// credentials, and this runs before any scrape redaction.
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
		if t.Name != "" && seenNames[t.Name] {
			if declaresAvailability {
				return fmt.Errorf("duplicate scrapeTarget name %q declares an availability contract", t.Name)
			}
			slog.Warn("duplicate scrapeTarget name; the endpoint will be scraped more than once", "target", t.Name)
		}
		seenNames[t.Name] = true

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
	if t.UnavailableFor < 0 || t.AvailableFor < 0 {
		return fmt.Errorf("scrapeTarget %q must not set a negative unavailableFor or availableFor", t.Name)
	}
	if !t.Required {
		return nil
	}
	// A required target's URL is published in a Node condition message and must
	// be a plain HTTP(S) endpoint we can reason about.
	u, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("scrapeTarget %q has an unparseable url", t.Name)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("scrapeTarget %q must use an absolute http or https url", t.Name)
	}
	return nil
}
