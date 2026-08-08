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

// XIDReader checks for XID errors on each GPU.
type XIDReader struct{}

func (r *XIDReader) Name() string { return "xid" }

func (r *XIDReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "xid",
		Enabled: cfg.Checks.XID.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			return []func() error{
				func() error { return r.checkXIDErrors(ctx, db, cfg, f) },
			}
		},
		Critical: &reader.FindingResult{Label: "GPUs with critical XIDs", Base: 2},
		Warning:  &reader.FindingResult{Label: "GPUs with warning XIDs", Base: 1},
		OkMsg:    "no XID errors detected",
	})
}

type gpuXIDs struct {
	critical []int
	warning  []int
}

func (r *XIDReader) checkXIDErrors(ctx context.Context, db *store.DB, cfg *config.Config, f *reader.CheckFindings) error {
	since := time.Now().Add(-cfg.CheckInterval)
	samples, err := db.QuerySamples(ctx, fieldnames.XIDErrors, -1, since)
	if err != nil {
		return fmt.Errorf("query XID samples: %w", err)
	}
	perGPU := r.groupByGPU(samples, cfg)
	for gpu, xids := range perGPU {
		if len(xids.critical) > 0 {
			f.AddCritical(gpu, fmt.Sprintf("GPU %d: critical XIDs %v", gpu, xids.critical))
		}
		if len(xids.warning) > 0 {
			f.AddWarning(gpu, fmt.Sprintf("GPU %d: warning XIDs %v", gpu, xids.warning))
		}
	}
	return nil
}

func (r *XIDReader) groupByGPU(samples []store.Sample, cfg *config.Config) map[int]*gpuXIDs {
	perGPU := make(map[int]*gpuXIDs)
	for _, s := range samples {
		xid := int(s.Value)
		if xid == 0 {
			continue
		}
		gx, ok := perGPU[s.GPU]
		if !ok {
			gx = &gpuXIDs{}
			perGPU[s.GPU] = gx
		}
		if cfg.IsCriticalXID(xid) {
			gx.critical = reader.AppendUnique(gx.critical, xid)
		} else if cfg.IsWarningXID(xid) {
			gx.warning = reader.AppendUnique(gx.warning, xid)
		}
	}
	return perGPU
}
