// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package collector implements the long-running DCGM sample collection daemon.
// It reads DCGM field values and writes them to a SQLite database.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	dcgmclient "github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/dcgm"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/fieldnames"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// ErrTopologyChanged signals that GPU topology changed during collection.
// The collector cannot safely continue with a stale DCGM group and must
// restart so that the orchestrator (systemd / k8s) rebuilds the watch set.
var ErrTopologyChanged = errors.New("gpu topology changed — collector restart required")

// Collector manages the DCGM → SQLite collection loop.
type Collector struct {
	client    *dcgmclient.Client
	db        *store.DB
	cfg       *config.Config
	interval  time.Duration
	retention time.Duration
	verbose   bool
}

// New creates a new Collector.
func New(client *dcgmclient.Client, db *store.DB, cfg *config.Config, interval, retention time.Duration, verbose bool) *Collector {
	return &Collector{
		client:    client,
		db:        db,
		cfg:       cfg,
		interval:  interval,
		retention: retention,
		verbose:   verbose,
	}
}

// Run starts the collection loop. It blocks until the context is cancelled.
func (c *Collector) Run(ctx context.Context) error {
	gpuCount, err := c.client.GPUCount()
	if err != nil {
		return fmt.Errorf("enumerate GPUs: %w", err)
	}
	slog.Info("collector starting", "gpus", gpuCount, "interval", c.interval, "retention", c.retention)

	// Create a DCGM group containing all GPUs.
	groupID, err := dcgm.CreateGroup("gpu_health_checker")
	if err != nil {
		return fmt.Errorf("create dcgm group: %w", err)
	}
	defer func() { _ = dcgm.DestroyGroup(groupID) }()
	for i := uint(0); i < gpuCount; i++ {
		if err := dcgm.AddToGroup(groupID, i); err != nil {
			return fmt.Errorf("add gpu %d to group: %w", i, err)
		}
	}

	// Setup field watches: 1-second update frequency, retain for the collection interval.
	updateFreqMicros := int64(1_000_000) // 1 second
	maxKeepAge := c.retention.Seconds()
	maxKeepSamples := int32(c.retention / time.Second)
	fg, err := dcgmclient.SetupFieldWatch(groupID, updateFreqMicros, maxKeepAge, maxKeepSamples)
	if err != nil {
		return fmt.Errorf("setup field watch: %w", err)
	}
	defer func() { _ = fg.Destroy() }()

	// Collect GPU static info once at startup and periodically.
	if err := c.collectGPUInfo(gpuCount); err != nil {
		slog.Error("initial GPU info collection failed", "error", err)
	}

	// Apply startup jitter to prevent thundering herd across nodes.
	jitter := time.Duration(rand.Int64N(int64(c.interval)))
	slog.Info("applying startup jitter", "jitter", jitter)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(jitter):
	}

	// Run the first collection immediately, then on a ticker.
	if err := c.collect(gpuCount, groupID); err != nil {
		slog.Error("collection cycle failed", "error", err)
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	infoTicker := time.NewTicker(5 * time.Minute)
	defer infoTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("collector shutting down")
			return ctx.Err()
		case <-ticker.C:
			if err := c.collect(gpuCount, groupID); err != nil {
				if errors.Is(err, ErrTopologyChanged) {
					slog.Error("topology change is fatal — exiting for restart", "error", err)
					return err
				}
				slog.Error("collection cycle failed", "error", err)
			}
		case <-infoTicker.C:
			if err := c.collectGPUInfo(gpuCount); err != nil {
				slog.Error("GPU info refresh failed", "error", err)
			}
		}
	}
}

// collect reads all watched DCGM fields for all GPUs, inserts samples,
// runs health diagnostics, and prunes old data.
func (c *Collector) collect(gpuCount uint, groupID dcgm.GroupHandle) error {
	// Check for GPU count changes (hot-add/removal).
	currentCount, err := c.client.GPUCount()
	if err != nil {
		slog.Warn("GPU count check failed", "error", err)
	} else if currentCount != gpuCount {
		return fmt.Errorf("%w: %d → %d", ErrTopologyChanged, gpuCount, currentCount)
	}

	now := time.Now().Unix()
	fields := dcgmclient.AllCounterFields()

	samples := make([]store.Sample, 0, int(gpuCount)*len(fields))
	for i := uint(0); i < gpuCount; i++ {
		values, err := dcgmclient.ReadFieldValues(i, fields)
		if err != nil {
			slog.Error("read field values failed", "gpu", i, "error", err)
			continue
		}
		for fid, val := range values {
			samples = append(samples, store.Sample{
				Timestamp: now,
				GPU:       int(i),
				Field:     fieldName(fid),
				Value:     val,
			})
		}
	}

	if len(samples) > 0 {
		if c.verbose {
			for _, s := range samples {
				slog.Debug("sample collected", "gpu", s.GPU, "field", s.Field, "value", s.Value)
			}
		}
		if err := c.db.InsertSamples(samples); err != nil {
			return fmt.Errorf("insert samples: %w", err)
		}
	}

	// Collect NVLink and NVSwitch link status.
	if err := c.collectNVLinkStatus(now); err != nil {
		slog.Error("nvlink status collection failed", "error", err)
	}

	// Run DCGM health diagnostics if enabled.
	if c.cfg.Checks.Health.Enabled {
		if err := c.collectHealthChecks(groupID, now); err != nil {
			slog.Error("health check collection failed", "error", err, "level", c.cfg.Checks.Health.Level)
		}
	}

	// Prune old data.
	if err := c.db.Prune(c.retention); err != nil {
		slog.Error("prune failed", "error", err)
	}

	slog.Debug("collection cycle complete", "samples", len(samples))
	return nil
}

// healthWatchMask returns the DCGM health watch bitmask for the configured level.
// When checkNVSwitch is false, NVSwitch watches are excluded even at level 3.
//
//	Level 1: PCIe, Memory, Thermal, Power, Driver (core hardware)
//	Level 2: + NVLink, PMU, MCU, SM, InfoROM
//	Level 3: + NVSwitch nonfatal/fatal (only if checkNVSwitch is true)
func healthWatchMask(level int, checkNVSwitch bool) dcgm.HealthSystem {
	mask := dcgm.DCGM_HEALTH_WATCH_PCIE |
		dcgm.DCGM_HEALTH_WATCH_MEM |
		dcgm.DCGM_HEALTH_WATCH_THERMAL |
		dcgm.DCGM_HEALTH_WATCH_POWER |
		dcgm.DCGM_HEALTH_WATCH_DRIVER

	if level >= 2 {
		mask |= dcgm.DCGM_HEALTH_WATCH_NVLINK |
			dcgm.DCGM_HEALTH_WATCH_PMU |
			dcgm.DCGM_HEALTH_WATCH_MCU |
			dcgm.DCGM_HEALTH_WATCH_SM |
			dcgm.DCGM_HEALTH_WATCH_INFOROM
	}

	if level >= 3 && checkNVSwitch {
		mask |= dcgm.DCGM_HEALTH_WATCH_NVSWITCH_NONFATAL |
			dcgm.DCGM_HEALTH_WATCH_NVSWITCH_FATAL
	}

	return mask
}

// collectHealthChecks runs DCGM health diagnostics and stores results.
// NVSwitch health watches are only included when NVSwitch hardware is present
// (detected at client init via entity enumeration).
func (c *Collector) collectHealthChecks(groupID dcgm.GroupHandle, now int64) error {
	hasNVSwitch := c.client.Caps.HasNVSwitch && c.cfg.NVLink.CheckNVSwitch
	mask := healthWatchMask(c.cfg.Checks.Health.Level, hasNVSwitch)
	if err := dcgm.HealthSet(groupID, mask); err != nil {
		if hasNVSwitch && c.cfg.Checks.Health.Level >= 3 {
			slog.Warn("health set with NVSwitch watches failed, falling back without NVSwitch", "error", err)
			mask = healthWatchMask(c.cfg.Checks.Health.Level, false)
			if err := dcgm.HealthSet(groupID, mask); err != nil {
				return fmt.Errorf("dcgm health set (fallback): %w", err)
			}
		} else {
			return fmt.Errorf("dcgm health set: %w", err)
		}
	}

	result, err := dcgm.HealthCheck(groupID)
	if err != nil {
		return fmt.Errorf("dcgm health check: %w", err)
	}

	var checks []store.HealthCheck
	for _, incident := range result.Incidents {
		status := "healthy"
		switch incident.Health {
		case dcgm.DCGM_HEALTH_RESULT_WARN:
			status = "warning"
		case dcgm.DCGM_HEALTH_RESULT_FAIL:
			status = "critical"
		}
		checks = append(checks, store.HealthCheck{
			Timestamp: now,
			GPU:       int(incident.EntityInfo.EntityId),
			System:    healthSystemName(incident.System),
			Status:    status,
			Message:   incident.Error.Message,
		})
	}

	if len(checks) > 0 {
		return c.db.InsertHealthChecks(checks)
	}
	return nil
}

// collectNVLinkStatus queries NVLink link states and stores per-GPU and
// per-NVSwitch active link counts as samples.
func (c *Collector) collectNVLinkStatus(now int64) error {
	summary, err := c.client.GetNVLinkStatus()
	if err != nil {
		return err
	}
	if !summary.Supported {
		slog.Debug("NVLink status not supported on this platform, skipping link collection")
		return nil
	}

	var samples []store.Sample
	for gpuIdx, counts := range summary.GPULinks {
		samples = append(samples, store.Sample{
			Timestamp: now,
			GPU:       int(gpuIdx),
			Field:     fieldnames.NVLinkActiveLinks,
			Value:     float64(counts.Active),
		})
		samples = append(samples, store.Sample{
			Timestamp: now,
			GPU:       int(gpuIdx),
			Field:     fieldnames.NVLinkTotalLinks,
			Value:     float64(counts.Total),
		})
	}
	for swIdx, counts := range summary.NVSwitchLinks {
		// Store NVSwitch link counts with negative GPU index to distinguish
		// from GPU entities. GPU field is int so -1000-swIdx gives a unique key.
		gpu := -1 - int(swIdx)
		samples = append(samples, store.Sample{
			Timestamp: now,
			GPU:       gpu,
			Field:     fieldnames.NVSwitchActiveLinks,
			Value:     float64(counts.Active),
		})
		samples = append(samples, store.Sample{
			Timestamp: now,
			GPU:       gpu,
			Field:     fieldnames.NVSwitchTotalLinks,
			Value:     float64(counts.Total),
		})
	}

	if c.verbose && len(samples) > 0 {
		for _, s := range samples {
			slog.Debug("link status collected", "field", s.Field, "gpu", s.GPU, "value", s.Value)
		}
	}

	if len(samples) > 0 {
		return c.db.InsertSamples(samples)
	}
	return nil
}

// collectGPUInfo refreshes GPU static info in the database.
func (c *Collector) collectGPUInfo(gpuCount uint) error {
	now := time.Now().Unix()
	for i := uint(0); i < gpuCount; i++ {
		info, err := c.client.GetDeviceInfo(i)
		if err != nil {
			return fmt.Errorf("get device info (gpu %d): %w", i, err)
		}
		if err := c.db.UpsertGPUInfo(store.GPUInfoRow{
			GPU:     int(i),
			UUID:    info.UUID,
			PCIBus:  info.PCIBus,
			Name:    info.Name,
			VBIOS:   info.VBIOS,
			Driver:  info.Driver,
			Updated: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// fieldNameMap maps DCGM field IDs to their string names in the database.
var fieldNameMap = map[dcgmclient.FieldID]string{
	dcgmclient.FieldXIDErrors:            fieldnames.XIDErrors,
	dcgmclient.FieldECCDBEVolTotal:       fieldnames.ECCDBEVol,
	dcgmclient.FieldECCDBEAggTotal:       fieldnames.ECCDBEAgg,
	dcgmclient.FieldECCSBEVolTotal:       fieldnames.ECCSBEVol,
	dcgmclient.FieldECCSBEAggTotal:       fieldnames.ECCSBEAgg,
	dcgmclient.FieldRowRemapPending:      fieldnames.RowRemapPending,
	dcgmclient.FieldRowRemapFailure:      fieldnames.RowRemapFailure,
	dcgmclient.FieldNVLinkCRCFlit:        fieldnames.NVLinkCRCFlit,
	dcgmclient.FieldNVLinkCRCData:        fieldnames.NVLinkCRCData,
	dcgmclient.FieldNVLinkReplay:         fieldnames.NVLinkReplay,
	dcgmclient.FieldNVLinkRecovery:       fieldnames.NVLinkRecovery,
	dcgmclient.FieldGPUNVLinkErrors:      fieldnames.GPUNVLinkErrors,
	dcgmclient.FieldThermalViolation:     fieldnames.ThermalViolation,
	dcgmclient.FieldPowerViolation:       fieldnames.PowerViolation,
	dcgmclient.FieldGPUTemp:              fieldnames.GPUTemp,
	dcgmclient.FieldMemoryTemp:           fieldnames.MemoryTemp,
	dcgmclient.FieldPowerUsage:           fieldnames.PowerUsage,
	dcgmclient.FieldPCIeReplay:           fieldnames.PCIeReplay,
	dcgmclient.FieldClockThrottleReasons: fieldnames.ClockThrottleReasons,
	dcgmclient.FieldRetiredDBE:           fieldnames.RetiredDBE,
	dcgmclient.FieldRetiredSBE:           fieldnames.RetiredSBE,
	dcgmclient.FieldC2CLinkStatus:        fieldnames.C2CLinkStatus,
	dcgmclient.FieldCPUTemp:              fieldnames.CPUTemp,
	dcgmclient.FieldNVSwitchTemp:         fieldnames.NVSwitchTemp,
}

// fieldName maps a DCGM field ID to a human-readable name used as the
// "field" column value in the samples table.
func fieldName(fid dcgmclient.FieldID) string {
	if name, ok := fieldNameMap[fid]; ok {
		return name
	}
	return fmt.Sprintf("FIELD_%d", fid)
}
func init() {
	for _, fid := range dcgmclient.AllCounterFields() {
		if _, ok := fieldNameMap[fid]; !ok {
			panic(fmt.Sprintf("BUG: DCGM field %d missing from fieldNameMap", fid))
		}
	}
}

// healthSystemName maps a DCGM health system bitmask to a name.
func healthSystemName(sys dcgm.HealthSystem) string {
	switch sys {
	case dcgm.DCGM_HEALTH_WATCH_PCIE:
		return "PCIe"
	case dcgm.DCGM_HEALTH_WATCH_NVLINK:
		return "NVLink"
	case dcgm.DCGM_HEALTH_WATCH_PMU:
		return "PMU"
	case dcgm.DCGM_HEALTH_WATCH_MCU:
		return "MCU"
	case dcgm.DCGM_HEALTH_WATCH_MEM:
		return "Memory"
	case dcgm.DCGM_HEALTH_WATCH_SM:
		return "SM"
	case dcgm.DCGM_HEALTH_WATCH_INFOROM:
		return "InfoROM"
	case dcgm.DCGM_HEALTH_WATCH_THERMAL:
		return "Thermal"
	case dcgm.DCGM_HEALTH_WATCH_POWER:
		return "Power"
	case dcgm.DCGM_HEALTH_WATCH_DRIVER:
		return "Driver"
	case dcgm.DCGM_HEALTH_WATCH_NVSWITCH_NONFATAL:
		return "NVSwitch"
	case dcgm.DCGM_HEALTH_WATCH_NVSWITCH_FATAL:
		return "NVSwitch"
	default:
		return fmt.Sprintf("System_%d", sys)
	}
}
