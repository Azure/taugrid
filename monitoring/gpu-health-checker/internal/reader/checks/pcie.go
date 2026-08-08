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
	since := time.Now().Add(-window)
	samples, err := db.QuerySamples(ctx, fieldnames.PCIeReplay, -1, since)
	if err != nil {
		return fmt.Errorf("query PCIE_REPLAY: %w", err)
	}
	rates := reader.ComputeDeltas(samples)
	for gpu, rate := range rates {
		if rate > cfg.Checks.PCIe.ReplayRateThreshold {
			f.AddWarning(gpu, fmt.Sprintf("GPU %d: PCIe replay delta=%.0f in %s (threshold=%.0f)",
				gpu, rate, window, cfg.Checks.PCIe.ReplayRateThreshold))
		}
	}
	return nil
}
