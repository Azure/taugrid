// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reader

import (
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

type ReadParams struct {
	Name     string
	Enabled  bool
	Checks   func(f *CheckFindings) []func() error
	Critical *FindingResult
	Warning  *FindingResult
	OkMsg    string
}

type FindingResult struct {
	Label string
	Base  int
}

func RunChecks(db *store.DB, cfg *config.Config, p ReadParams) (Result, error) {
	if !p.Enabled {
		return Result{ExitCode: 0, Message: p.Name + " check disabled"}, nil
	}
	stale := checkStaleness(db, 2*cfg.CheckInterval)
	if stale != nil {
		return *stale, nil
	}
	var f CheckFindings
	for _, check := range p.Checks(&f) {
		if err := check(); err != nil {
			return Result{}, err
		}
	}
	if p.Critical != nil && len(f.Critical) > 0 {
		return f.buildResult(f.Critical, cfg, *p.Critical), nil
	}
	if p.Warning != nil && len(f.Warning) > 0 {
		return f.buildResult(f.Warning, cfg, *p.Warning), nil
	}
	return Result{ExitCode: 0, Message: p.OkMsg}, nil
}
