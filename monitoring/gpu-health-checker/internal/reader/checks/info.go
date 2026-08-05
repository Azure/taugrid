package checks

import (
	"context"
	"fmt"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// InfoReader validates GPU count, driver version, and VBIOS version from the
// collector-populated gpu_info table. Device-file presence (/dev/nvidia*) is
// intentionally NOT checked here — the reader runs inside the NPD container
// which does not mount /dev, and a missing device would already cause DCGM
// operations in the collector to fail, making the collector the authoritative
// signal for device accessibility.
type InfoReader struct{}

func (r *InfoReader) Name() string { return "info" }

func (r *InfoReader) Read(ctx context.Context, db *store.DB, cfg *config.Config) (reader.Result, error) {
	return reader.RunChecks(db, cfg, reader.ReadParams{
		Name:    "info",
		Enabled: cfg.Checks.Info.Enabled,
		Checks: func(f *reader.CheckFindings) []func() error {
			var gpuInfo []store.GPUInfoRow
			return []func() error{
				func() error {
					var err error
					gpuInfo, err = db.QueryAllGPUInfo(ctx)
					if err != nil {
						return fmt.Errorf("query gpu_info: %w", err)
					}
					return nil
				},
				func() error { return r.checkGPUCount(gpuInfo, cfg, f) },
				func() error { return r.checkDriverVersion(gpuInfo, cfg, f) },
				func() error { return r.checkVBIOSVersions(gpuInfo, cfg, f) },
			}
		},
		Critical: &reader.FindingResult{Base: 2},
		Warning:  &reader.FindingResult{Label: "GPUs with VBIOS mismatch", Base: 2},
		OkMsg:    "GPU info validation passed",
	})
}

func (r *InfoReader) checkGPUCount(gpuInfo []store.GPUInfoRow, cfg *config.Config, f *reader.CheckFindings) error {
	if len(gpuInfo) != cfg.ExpectedGPUs {
		f.AddCritical(-1, fmt.Sprintf("expected %d GPUs, found %d", cfg.ExpectedGPUs, len(gpuInfo)))
	}
	return nil
}

func (r *InfoReader) checkDriverVersion(gpuInfo []store.GPUInfoRow, cfg *config.Config, f *reader.CheckFindings) error {
	if len(cfg.DriverVersions) == 0 || len(gpuInfo) == 0 {
		return nil
	}
	if !contains(cfg.DriverVersions, gpuInfo[0].Driver) {
		f.AddWarning(-1, fmt.Sprintf("driver version %q not in allowlist %v", gpuInfo[0].Driver, cfg.DriverVersions))
	}
	return nil
}

func (r *InfoReader) checkVBIOSVersions(gpuInfo []store.GPUInfoRow, cfg *config.Config, f *reader.CheckFindings) error {
	if len(cfg.VBIOSVersions) == 0 {
		return nil
	}
	for _, info := range gpuInfo {
		if !contains(cfg.VBIOSVersions, info.VBIOS) {
			f.AddWarning(info.GPU, fmt.Sprintf("GPU %d: VBIOS %q not in allowlist %v", info.GPU, info.VBIOS, cfg.VBIOSVersions))
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
