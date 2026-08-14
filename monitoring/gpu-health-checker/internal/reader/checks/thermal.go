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

// ThermalReader checks thermal and power violation counters.
type ThermalReader struct{}

func (r *ThermalReader) Name() string { return "thermal" }

func (r *ThermalReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "thermal",
		Enabled: cfg.Checks.Thermal.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkThermalViolations(ctx, db, cfg, f) },
				func() error { return r.checkGPUTemp(ctx, db, cfg, f) },
				func() error { r.logMemoryTemps(ctx, db); return nil },
				func() error { return r.checkPowerViolations(ctx, db, cfg, f) },
				func() error { return r.checkCPUTemp(ctx, db, cfg, f) },
				func() error { return r.checkNVSwitchTemp(ctx, db, cfg, f) },
			}
		},
		Critical: &reader.FindingResult{Label: "critical temperature exceeded", Base: 2},
		Warning:  &reader.FindingResult{Label: "GPUs with thermal/power issues", Base: 1},
		OkMsg:    "no thermal or power violations",
	})
}

func (r *ThermalReader) checkThermalViolations(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	samples, err := db.QuerySamples(ctx, fieldnames.ThermalViolation, -1, time.Now().Add(-cfg.CheckInterval))
	if err != nil {
		return fmt.Errorf("query THERMAL_VIOLATION: %w", err)
	}
	for _, delta := range deltasAboveThreshold(samples, 0) {
		tempStr := getLatestTemp(ctx, db, delta.gpu)
		f.AddWarning(delta.gpu, fmt.Sprintf("GPU %d: thermal violations delta=%.0f%s", delta.gpu, delta.value, tempStr))
	}
	return nil
}

func (r *ThermalReader) logMemoryTemps(ctx context.Context, db *store.DB) {
	memTempSamples, err := db.QueryLatestSamples(ctx, fieldnames.MemoryTemp)
	if err != nil {
		return
	}
	for _, s := range memTempSamples {
		if s.Value > 0 {
			gpuTemp := getLatestTemp(ctx, db, s.GPU)
			slog.Debug("memory temp", "gpu", s.GPU, "memTemp", s.Value, "gpuTemp", gpuTemp)
		}
	}
}

func (r *ThermalReader) checkPowerViolations(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	samples, err := db.QuerySamples(ctx, fieldnames.PowerViolation, -1, time.Now().Add(-cfg.CheckInterval))
	if err != nil {
		return fmt.Errorf("query POWER_VIOLATION: %w", err)
	}
	for _, delta := range deltasAboveThreshold(samples, 0) {
		f.AddWarning(delta.gpu, fmt.Sprintf("GPU %d: power violations delta=%.0f", delta.gpu, delta.value))
	}
	return nil
}

func (r *ThermalReader) checkGPUTemp(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	samples, err := db.QueryLatestSamples(ctx, fieldnames.GPUTemp)
	if err != nil {
		return fmt.Errorf("query GPU_TEMP: %w", err)
	}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: invalid temperature reading", s.GPU))
			continue
		}
		if s.Value >= cfg.Checks.Thermal.GPUTempCritical {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: temp=%.0f°C (critical threshold=%.0f°C)", s.GPU, s.Value, cfg.Checks.Thermal.GPUTempCritical))
		} else if s.Value >= cfg.Checks.Thermal.GPUTempWarning {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: temp=%.0f°C (warning threshold=%.0f°C)", s.GPU, s.Value, cfg.Checks.Thermal.GPUTempWarning))
		}
	}
	return nil
}

func (r *ThermalReader) checkCPUTemp(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	samples, err := db.QueryLatestSamples(ctx, fieldnames.CPUTemp)
	if err != nil {
		return fmt.Errorf("query CPU_TEMP: %w", err)
	}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: invalid CPU temperature reading", s.GPU))
			continue
		}
		if s.Value >= cfg.Checks.Thermal.CPUTempCritical {
			f.AddCritical(s.GPU, fmt.Sprintf("GPU %d: CPU temp=%.0f°C (critical threshold=%.0f°C)", s.GPU, s.Value, cfg.Checks.Thermal.CPUTempCritical))
		} else if s.Value >= cfg.Checks.Thermal.CPUTempWarning {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: CPU temp=%.0f°C (warning threshold=%.0f°C)", s.GPU, s.Value, cfg.Checks.Thermal.CPUTempWarning))
		}
	}
	return nil
}

func (r *ThermalReader) checkNVSwitchTemp(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	samples, err := db.QueryLatestSamples(ctx, fieldnames.NVSwitchTemp)
	if err != nil {
		return fmt.Errorf("query NVSWITCH_TEMP: %w", err)
	}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			f.AddWarning(s.GPU, fmt.Sprintf("NVSwitch %d: invalid temperature reading", -(s.GPU+1)))
			continue
		}
		if s.Value >= cfg.Checks.Thermal.NVSwitchTempCritical {
			f.AddCritical(s.GPU, fmt.Sprintf("NVSwitch %d: temp=%.0f°C (critical threshold=%.0f°C)", -(s.GPU+1), s.Value, cfg.Checks.Thermal.NVSwitchTempCritical))
		} else if s.Value >= cfg.Checks.Thermal.NVSwitchTempWarning {
			f.AddWarning(s.GPU, fmt.Sprintf("NVSwitch %d: temp=%.0f°C (warning threshold=%.0f°C)", -(s.GPU+1), s.Value, cfg.Checks.Thermal.NVSwitchTempWarning))
		}
	}
	return nil
}

// getLatestTemp returns a formatted string with the latest GPU temperature,
// or an empty string if unavailable.
func getLatestTemp(ctx context.Context, db *store.DB, gpu int) string {
	samples, err := db.QuerySamples(ctx, fieldnames.GPUTemp, gpu, time.Time{})
	if err != nil || len(samples) == 0 {
		return ""
	}
	// Get the latest sample.
	latest := samples[len(samples)-1]
	return fmt.Sprintf(" (temp=%.0f°C)", latest.Value)
}
