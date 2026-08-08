// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reader

import (
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// ReadParams describes the common structure of a Read method.
type ReadParams struct {
	Name     string                                // reader name for the "disabled" message
	Enabled  bool                                  // whether this check is enabled
	Checks   func(f *CheckFindings) []func() error // builds per-check closures
	Critical *FindingResult                        // nil to skip critical branch
	Warning  *FindingResult                        // nil to skip warning branch
	OkMsg    string                                // message when no findings
}

// FindingResult describes how to build a Result from a set of failed GPUs.
type FindingResult struct {
	Label string // short description after GPU count; "" for raw parts only
	Base  int    // criticalBase for applyTolerance, or raw exit code when label is ""
}

// RunChecks executes the standard Read workflow: enabled gate, staleness
// check, sub-checks, and result assembly.
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
