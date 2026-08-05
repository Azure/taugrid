package checks

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/fieldnames"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// ClocksReader checks GPU clock throttle reasons.
type ClocksReader struct{}

func (r *ClocksReader) Name() string { return "clocks" }

// Problematic throttle reason bits from the DCGM_FI_DEV_CLOCKS_EVENT_REASONS
// bitmask. See DCGM documentation "Clock Throttle Reasons" and
// nvml.h ClocksEventReasons constants.
const (
	throttleHWSlowdown   uint64 = 0x0000000000000008 // nvmlClocksEventReasonHwSlowdown
	throttleHWThermal    uint64 = 0x0000000000000040 // nvmlClocksEventReasonHwThermalSlowdown
	throttleHWPowerBrake uint64 = 0x0000000000000080 // nvmlClocksEventReasonHwPowerBrakeSlowdown
	throttleSWThermal    uint64 = 0x0000000000000020 // nvmlClocksEventReasonSwThermalSlowdown
)

var throttleReasonNames = map[uint64]string{
	throttleHWSlowdown:   "HW_SLOWDOWN",
	throttleHWThermal:    "HW_THERMAL_SLOWDOWN",
	throttleHWPowerBrake: "HW_POWER_BRAKE_SLOWDOWN",
	throttleSWThermal:    "SW_THERMAL_SLOWDOWN",
}

func (r *ClocksReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "clocks",
		Enabled: cfg.Checks.Clocks.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkThrottleReasons(ctx, db, f) },
			}
		},
		Warning: &reader.FindingResult{Label: "GPUs throttled", Base: 1},
		OkMsg:   "no problematic clock throttling",
	})
}

func (r *ClocksReader) checkThrottleReasons(ctx context.Context, db *store.DB, f *reader.CheckFindings) error {
	samples, err := db.QueryLatestSamples(ctx, fieldnames.ClockThrottleReasons)
	if err != nil {
		return fmt.Errorf("query CLOCK_THROTTLE_REASONS: %w", err)
	}
	for _, s := range samples {
		bitmask := uint64(s.Value)
		var reasons []string
		for bit, name := range throttleReasonNames {
			if bitmask&bit != 0 {
				reasons = append(reasons, name)
			}
		}
		if len(reasons) > 0 {
			f.AddWarning(s.GPU, fmt.Sprintf("GPU %d: throttle reasons=[%s]", s.GPU, strings.Join(reasons, ",")))
		}
	}
	return nil
}
