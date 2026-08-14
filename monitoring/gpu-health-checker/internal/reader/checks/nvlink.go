// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package checks

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/fieldnames"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// NVLinkReader checks NVLink status and CRC error rates.
type NVLinkReader struct{}

func (r *NVLinkReader) Name() string { return "nvlink" }

func (r *NVLinkReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "nvlink",
		Enabled: cfg.Checks.NVLink.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkCRCErrors(ctx, db, cfg, f) },
				func() error { return r.checkNVLinkErrors(ctx, db, f) },
				func() error { return r.checkC2CLink(ctx, db, cfg, f) },
				func() error { return r.checkActiveLinkCount(ctx, db, cfg, f) },
				func() error { return r.checkNVSwitchLinks(ctx, db, cfg, f) },
			}
		},
		Critical: &reader.FindingResult{Label: "GPUs with NVLink/NVSwitch critical errors", Base: 2},
		Warning:  &reader.FindingResult{Label: "GPUs with NVLink CRC errors", Base: 1},
		OkMsg:    "NVLink healthy",
	})
}

func (r *NVLinkReader) checkCRCErrors(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	since := time.Now().Add(-cfg.CheckInterval)
	for _, field := range []string{fieldnames.NVLinkCRCFlit, fieldnames.NVLinkCRCData, fieldnames.NVLinkReplay, fieldnames.NVLinkRecovery} {
		samples, err := db.QuerySamples(ctx, field, -1, since)
		if err != nil {
			return fmt.Errorf("query %s: %w", field, err)
		}
		for _, delta := range deltasAboveThreshold(samples, 0) {
			f.AddWarning(delta.gpu, fmt.Sprintf("GPU %d: %s delta=%.0f", delta.gpu, field, delta.value))
		}
	}
	return nil
}

func (r *NVLinkReader) checkNVLinkErrors(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	samples, err := db.QueryLatestSamples(ctx, fieldnames.GPUNVLinkErrors)
	if err != nil {
		return fmt.Errorf("query GPU_NVLINK_ERRORS: %w", err)
	}
	for _, sample := range samples {
		if sample.Value > 0 {
			f.AddCritical(sample.GPU, fmt.Sprintf("GPU %d: NVLink errors=%.0f", sample.GPU, sample.Value))
		}
	}
	return nil
}

func (r *NVLinkReader) checkC2CLink(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	if !cfg.NVLink.CheckC2C {
		return nil
	}
	c2cSamples, err := db.QueryLatestSamples(ctx, fieldnames.C2CLinkStatus)
	if err != nil {
		return fmt.Errorf("query C2C_LINK_STATUS: %w", err)
	}
	for _, s := range c2cSamples {
		if s.Value == 0 {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: C2C link down", s.GPU))
		}
	}
	return nil
}

func (r *NVLinkReader) checkActiveLinkCount(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	if cfg.NVLink.ExpectedActiveLinksPerGPU <= 0 {
		return nil
	}
	linkSamples, err := db.QueryLatestSamples(ctx, fieldnames.NVLinkActiveLinks)
	if err != nil {
		return fmt.Errorf("query NVLINK_ACTIVE_LINKS: %w", err)
	}
	for _, s := range linkSamples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value < 0 {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: invalid NVLink active link count: %v", s.GPU, s.Value))
			continue
		}
		active := int(s.Value)
		if active < cfg.NVLink.ExpectedActiveLinksPerGPU {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: NVLink active=%d expected=%d",
				s.GPU, active, cfg.NVLink.ExpectedActiveLinksPerGPU))
		}
	}
	return nil
}

func (r *NVLinkReader) checkNVSwitchLinks(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	if !cfg.NVLink.CheckNVSwitch {
		return nil
	}
	swSamples, err := db.QueryLatestSamples(ctx, fieldnames.NVSwitchActiveLinks)
	if err != nil {
		return fmt.Errorf("query NVSWITCH_ACTIVE_LINKS: %w", err)
	}
	totalSamples, err := db.QueryLatestSamples(ctx, fieldnames.NVSwitchTotalLinks)
	if err != nil {
		return fmt.Errorf("query NVSWITCH_TOTAL_LINKS: %w", err)
	}
	totalBySwitch := make(map[int]int)
	for _, s := range totalSamples {
		totalBySwitch[s.GPU] = int(s.Value)
	}
	for _, s := range swSamples {
		active := int(s.Value)
		total, ok := totalBySwitch[s.GPU]
		if !ok {
			slog.Warn("NVSwitch missing total link count", "switch", -(s.GPU + 1))
			continue
		}
		if total > 0 && active < total {
			f.AddNote(fmt.Sprintf("NVSwitch %d: active=%d/%d links", -(s.GPU + 1), active, total))
		}
	}
	return nil
}
