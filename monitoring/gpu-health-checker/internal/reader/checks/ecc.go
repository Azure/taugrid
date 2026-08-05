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

// ECCReader checks ECC error counters and row remap status.
type ECCReader struct{}

func (r *ECCReader) Name() string { return "ecc" }

func (r *ECCReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "ecc",
		Enabled: cfg.Checks.ECC.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkDBEVolatile(ctx, db, f) },
				func() error { return r.checkDBEAggregate(ctx, db, f) },
				func() error { return r.checkRowRemapFailure(ctx, db, f) },
				func() error { return r.checkRetiredDBE(ctx, db, f) },
				func() error { return r.checkRetiredSBE(ctx, db, f) },
				func() error { return r.checkSBERate(ctx, db, cfg, f) },
				func() error { return r.checkRowRemapPending(ctx, db, f) },
			}
		},
		Critical: &reader.FindingResult{Label: "GPUs with ECC critical errors", Base: 2},
		Warning:  &reader.FindingResult{Label: "GPUs with ECC warnings", Base: 1},
		OkMsg:    "no ECC errors detected",
	})
}

func (r *ECCReader) checkDBEVolatile(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	dbeVol, err := db.QueryLatestSamples(ctx, fieldnames.ECCDBEVol)
	if err != nil {
		return fmt.Errorf("query ECC_DBE_VOL: %w", err)
	}
	for _, s := range dbeVol {
		if s.Value > 0 {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: DBE volatile=%.0f", s.GPU, s.Value))
		}
	}
	return nil
}

func (r *ECCReader) checkDBEAggregate(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	dbeAgg, err := db.QueryLatestSamples(ctx, fieldnames.ECCDBEAgg)
	if err != nil {
		return fmt.Errorf("query ECC_DBE_AGG: %w", err)
	}
	for _, s := range dbeAgg {
		if s.Value > 0 {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: DBE aggregate=%.0f", s.GPU, s.Value))
		}
	}
	return nil
}

func (r *ECCReader) checkRowRemapFailure(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	remapFail, err := db.QueryLatestSamples(ctx, fieldnames.RowRemapFailure)
	if err != nil {
		return fmt.Errorf("query ROW_REMAP_FAILURE: %w", err)
	}
	for _, s := range remapFail {
		if s.Value > 0 {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: row remap failure=%.0f", s.GPU, s.Value))
		}
	}
	return nil
}

func (r *ECCReader) checkRetiredDBE(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	retiredDBE, err := db.QueryLatestSamples(ctx, fieldnames.RetiredDBE)
	if err != nil {
		return fmt.Errorf("query RETIRED_DBE: %w", err)
	}
	for _, s := range retiredDBE {
		if s.Value > 0 {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: retired pages (DBE)=%.0f", s.GPU, s.Value))
		}
	}
	return nil
}

func (r *ECCReader) checkRetiredSBE(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	retiredSBE, err := db.QueryLatestSamples(ctx, fieldnames.RetiredSBE)
	if err != nil {
		return fmt.Errorf("query RETIRED_SBE: %w", err)
	}
	for _, s := range retiredSBE {
		if s.Value > 0 {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: retired pages (SBE)=%.0f", s.GPU, s.Value))
		}
	}
	return nil
}

func (r *ECCReader) checkSBERate(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	sbeWindow := cfg.Checks.ECC.SBERateWindow
	since := time.Now().Add(-sbeWindow)
	sbeVol, err := db.QuerySamples(ctx, fieldnames.ECCSBEVol, -1, since)
	if err != nil {
		return fmt.Errorf("query ECC_SBE_VOL: %w", err)
	}
	sbeRates := reader.ComputeDeltas(sbeVol)
	for gpu, rate := range sbeRates {
		if rate > cfg.Checks.ECC.SBERateThreshold {
			f.AddWarning(gpu, fmt.Sprintf("GPU %d: SBE rate=%.1f in %s (threshold=%.0f)",
				gpu, rate, sbeWindow, cfg.Checks.ECC.SBERateThreshold))
		}
	}
	return nil
}

func (r *ECCReader) checkRowRemapPending(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	remapPend, err := db.QueryLatestSamples(ctx, fieldnames.RowRemapPending)
	if err != nil {
		return fmt.Errorf("query ROW_REMAP_PENDING: %w", err)
	}
	for _, s := range remapPend {
		if s.Value > 0 {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: row remap pending=%.0f", s.GPU, s.Value))
		}
	}
	return nil
}
