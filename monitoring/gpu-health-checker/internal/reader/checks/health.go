package checks

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// HealthReader checks DCGM health diagnostic results.
type HealthReader struct{}

func (r *HealthReader) Name() string { return "health" }

func (r *HealthReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "health",
		Enabled: cfg.Checks.Health.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkHealthDiagnostics(ctx, db, f) },
			}
		},
		Critical: &reader.FindingResult{Label: "GPUs with DCGM health failures", Base: 2},
		Warning:  &reader.FindingResult{Label: "GPUs with DCGM health warnings", Base: 1},
		OkMsg:    "DCGM health diagnostics passed",
	})
}

func (r *HealthReader) checkHealthDiagnostics(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	healthChecks, err := db.QueryLatestHealthChecks(ctx)
	if err != nil {
		return fmt.Errorf("query health checks: %w", err)
	}
	for _, hc := range healthChecks {
		switch hc.Status {
		case "critical":
			f.AddCritical(hc.GPU, fmt.Sprintf("GPU %d: %s %s — %s", hc.GPU, hc.System, hc.Status, hc.Message))
		case "warning":
			f.AddWarning(hc.GPU, fmt.Sprintf("GPU %d: %s %s — %s", hc.GPU, hc.System, hc.Status, hc.Message))
		case "healthy":
			// expected, no action
		default:
			slog.Warn("unexpected DCGM health status", "status", hc.Status, "gpu", hc.GPU, "system", hc.System)
		}
	}
	return nil
}
