// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rules

import (
	"testing"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
)

func TestEvaluate_InstantThreshold(t *testing.T) {
	t.Parallel()

	engine := NewEngine([]Rule{
		{Name: "ecc-dbe", MetricName: "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", ConditionType: "GPUECCDouble", Mode: "instant", Threshold: 0},
	})

	metrics := []scraper.Metric{
		{Name: "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", Labels: map[string]string{"gpu": "0"}, Value: 5},
	}

	results := engine.Evaluate(metrics)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Firing {
		t.Error("expected firing, got not firing")
	}
	if results[0].ConditionType != "GPUECCDouble" {
		t.Errorf("unexpected condition type: %s", results[0].ConditionType)
	}
}

func TestEvaluate_InstantBelowThreshold(t *testing.T) {
	t.Parallel()

	engine := NewEngine([]Rule{
		{Name: "ecc-dbe", MetricName: "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", ConditionType: "GPUECCDouble", Mode: "instant", Threshold: 10},
	})

	metrics := []scraper.Metric{
		{Name: "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", Labels: map[string]string{"gpu": "0"}, Value: 5},
	}

	results := engine.Evaluate(metrics)
	if results[0].Firing {
		t.Error("expected not firing (below threshold)")
	}
}

func TestEvaluate_RateThreshold(t *testing.T) {
	engine := NewEngine([]Rule{
		{Name: "ecc-sbe-rate", MetricName: "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", ConditionType: "GPUECCSingleRate", Mode: "rate", Threshold: 10, Window: 10 * time.Minute},
	})

	// First scrape: baseline.
	engine.Evaluate([]scraper.Metric{
		{Name: "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", Labels: map[string]string{"gpu": "0"}, Value: 100},
	})

	// Simulate time passing and value increasing.
	engine.mu.Lock()
	key := metricKey("DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", map[string]string{"gpu": "0"})
	engine.history[key][0].time = time.Now().Add(-5 * time.Minute)
	engine.mu.Unlock()

	// Second scrape: increase of 15 (> threshold of 10).
	results := engine.Evaluate([]scraper.Metric{
		{Name: "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", Labels: map[string]string{"gpu": "0"}, Value: 115},
	})

	if !results[0].Firing {
		t.Error("expected firing (rate exceeds threshold)")
	}
}

func TestEvaluate_ForDuration(t *testing.T) {
	engine := NewEngine([]Rule{
		{Name: "nvlink-bw", MetricName: "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", ConditionType: "NVLinkBandwidthLow", Mode: "instant", Threshold: -1, For: 5 * time.Minute},
	})

	// Value 100 > threshold -1, so condition is met but "for" duration not reached.
	metrics := []scraper.Metric{
		{Name: "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", Labels: map[string]string{"gpu": "0"}, Value: 100},
	}

	results := engine.Evaluate(metrics)
	if results[0].Firing {
		t.Error("should not fire yet — 'for' duration not reached")
	}

	// Simulate pending time exceeding "for" duration.
	engine.mu.Lock()
	engine.pending["NVLinkBandwidthLow"] = time.Now().Add(-6 * time.Minute)
	engine.mu.Unlock()

	results = engine.Evaluate(metrics)
	if !results[0].Firing {
		t.Error("should fire — 'for' duration exceeded")
	}
}

func TestEvaluate_XIDInstantGaugeWithErrCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		metrics    []scraper.Metric
		wantFiring bool
	}{
		{
			name: "matching nonzero gauge fires",
			metrics: []scraper.Metric{
				{Name: "DCGM_FI_DEV_XID_ERRORS", Labels: map[string]string{"gpu": "0", "err_code": "48"}, Value: 1},
			},
			wantFiring: true,
		},
		{
			name: "matching zero gauge does not fire",
			metrics: []scraper.Metric{
				{Name: "DCGM_FI_DEV_XID_ERRORS", Labels: map[string]string{"gpu": "0", "err_code": "48"}, Value: 0},
			},
		},
		{
			name: "nonmatching gauge does not fire",
			metrics: []scraper.Metric{
				{Name: "DCGM_FI_DEV_XID_ERRORS", Labels: map[string]string{"gpu": "0", "err_code": "63"}, Value: 5},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			engine := NewEngine([]Rule{
				{Name: "xid-48", MetricName: "DCGM_FI_DEV_XID_ERRORS", Labels: map[string]string{"err_code": "48"}, ConditionType: "XIDError48", Mode: "instant", Threshold: 0},
			})

			results := engine.Evaluate(tt.metrics)
			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}
			if results[0].Firing != tt.wantFiring {
				t.Errorf("firing = %t, want %t", results[0].Firing, tt.wantFiring)
			}
		})
	}
}
