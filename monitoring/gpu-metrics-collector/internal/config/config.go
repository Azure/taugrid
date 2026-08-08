// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"fmt"
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
	return &cfg, nil
}
