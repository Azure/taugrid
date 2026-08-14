// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package checks

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/fieldnames"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// PCIeReader checks PCIe replay error rates.
type PCIeReader struct{}

func (r *PCIeReader) Name() string { return "pcie" }

func (r *PCIeReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "pcie",
		Enabled: cfg.Checks.PCIe.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkReplayRate(ctx, db, cfg, f) },
			}
		},
		Warning: &reader.FindingResult{Label: "GPUs with PCIe replay errors", Base: 1},
		OkMsg:   "no PCIe replay errors",
	})
}

func (r *PCIeReader) checkReplayRate(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	window := cfg.Checks.PCIe.ReplayRateWindow
	samples, err := db.QuerySamples(ctx, fieldnames.PCIeReplay, -1, time.Now().Add(-window))
	if err != nil {
		return fmt.Errorf("query PCIE_REPLAY: %w", err)
	}
	for _, delta := range deltasAboveThreshold(samples, cfg.Checks.PCIe.ReplayRateThreshold) {
		f.AddWarning(delta.gpu, fmt.Sprintf("GPU %d: PCIe replay delta=%.0f in %s (threshold=%.0f)",
			delta.gpu, delta.value, window, cfg.Checks.PCIe.ReplayRateThreshold))
	}
	return nil
}
