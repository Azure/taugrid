// Package dcgm wraps the NVIDIA DCGM Go bindings, managing the DCGM lifecycle
// and providing GPU enumeration and device info queries.
package dcgm

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
)

// Capabilities describes which hardware features are present on the platform.
// Probed once at client creation time via DCGM entity enumeration.
type Capabilities struct {
	HasNVSwitch bool   // NVSwitch entities detected
	NVSwitchIDs []uint // NVSwitch entity IDs
}

// Client manages the DCGM connection and provides GPU enumeration.
type Client struct {
	closeOnce sync.Once
	cleanup   func()
	Caps      Capabilities // hardware capabilities probed at init
}

// GPUInfo holds static information about a single GPU.
type GPUInfo struct {
	Index  uint   // GPU index (0-based)
	UUID   string // GPU UUID
	PCIBus string // PCI bus ID (e.g., "00000000:3B:00.0")
	Name   string // GPU product name (e.g., "NVIDIA H100 80GB HBM3")
	VBIOS  string // VBIOS version string
	Driver string // Driver version string
}

// NewClient initializes DCGM in embedded mode and returns a client.
// Embedded mode loads libdcgm.so in-process — no nv-hostengine required.
// After init, it probes hardware capabilities (NVSwitch presence, etc.).
func NewClient() (*Client, error) {
	cleanup, err := dcgm.Init(dcgm.Embedded)
	if err != nil {
		return nil, fmt.Errorf("dcgm init (embedded): %w", err)
	}
	caps := probeCapabilities()
	return &Client{cleanup: cleanup, Caps: caps}, nil
}

// probeCapabilities detects hardware features by querying DCGM entity groups.
func probeCapabilities() Capabilities {
	var caps Capabilities

	// Detect NVSwitch entities.
	switchIDs, err := dcgm.GetEntityGroupEntities(dcgm.FE_SWITCH)
	if err != nil {
		slog.Info("NVSwitch entity enumeration not available", "error", err)
	} else if len(switchIDs) > 0 {
		caps.HasNVSwitch = true
		caps.NVSwitchIDs = switchIDs
		slog.Info("NVSwitch hardware detected", "count", len(switchIDs))
	} else {
		slog.Info("no NVSwitch hardware detected")
	}

	return caps
}

// Close shuts down the DCGM connection. Safe to call multiple times.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.cleanup != nil {
			c.cleanup()
		}
	})
	return nil
}

// GPUCount returns the number of GPUs visible to DCGM.
func (c *Client) GPUCount() (uint, error) {
	count, err := dcgm.GetAllDeviceCount()
	if err != nil {
		return 0, fmt.Errorf("dcgm get device count: %w", err)
	}
	return count, nil
}

// ListGPUs enumerates all GPUs and returns their static info.
func (c *Client) ListGPUs() ([]GPUInfo, error) {
	count, err := c.GPUCount()
	if err != nil {
		return nil, err
	}
	gpus := make([]GPUInfo, 0, count)
	for i := uint(0); i < count; i++ {
		info, err := c.GetDeviceInfo(i)
		if err != nil {
			return nil, fmt.Errorf("gpu %d: %w", i, err)
		}
		gpus = append(gpus, info)
	}
	return gpus, nil
}

// GetDeviceInfo returns static information for a single GPU by index.
func (c *Client) GetDeviceInfo(gpuIndex uint) (GPUInfo, error) {
	dev, err := dcgm.GetDeviceInfo(gpuIndex)
	if err != nil {
		return GPUInfo{}, fmt.Errorf("dcgm get device info (gpu %d): %w", gpuIndex, err)
	}

	return GPUInfo{
		Index:  gpuIndex,
		UUID:   dev.UUID,
		PCIBus: dev.PCI.BusID,
		Name:   dev.Identifiers.Brand,
		VBIOS:  dev.Identifiers.Vbios,
		Driver: dev.Identifiers.DriverVersion,
	}, nil
}

// NVLinkSummary holds per-GPU and per-NVSwitch link counts.
type NVLinkSummary struct {
	Supported     bool                  // false if the platform does not support NVLink
	GPULinks      map[uint]NVLinkCounts // GPU index -> link counts
	NVSwitchLinks map[uint]NVLinkCounts // NVSwitch index -> link counts
}

// NVLinkCounts holds active/total link counts for one entity.
type NVLinkCounts struct {
	Active int
	Total  int // links in any state other than NOT_SUPPORTED
}

// GetNVLinkStatus queries DCGM for NVLink link states and returns a summary
// of active vs total links per GPU and per NVSwitch.
// Returns an empty summary (not an error) if the API call fails with a
// not-supported error (e.g., on SKUs without NVLink).
func (c *Client) GetNVLinkStatus() (*NVLinkSummary, error) {
	links, err := dcgm.GetNvLinkLinkStatus()
	if err != nil {
		if IsNotSupportedError(err) {
			return &NVLinkSummary{
				Supported:     false,
				GPULinks:      make(map[uint]NVLinkCounts),
				NVSwitchLinks: make(map[uint]NVLinkCounts),
			}, nil
		}
		return nil, fmt.Errorf("dcgm get nvlink status: %w", err)
	}
	if links == nil {
		return &NVLinkSummary{
			Supported:     false,
			GPULinks:      make(map[uint]NVLinkCounts),
			NVSwitchLinks: make(map[uint]NVLinkCounts),
		}, nil
	}

	summary := &NVLinkSummary{
		Supported:     true,
		GPULinks:      make(map[uint]NVLinkCounts),
		NVSwitchLinks: make(map[uint]NVLinkCounts),
	}

	for _, link := range links {
		if link.State == dcgm.LS_NOT_SUPPORTED {
			continue
		}

		var m map[uint]NVLinkCounts
		switch link.ParentType {
		case dcgm.FE_GPU:
			m = summary.GPULinks
		case dcgm.FE_SWITCH:
			m = summary.NVSwitchLinks
		default:
			continue
		}

		counts := m[link.ParentId]
		counts.Total++
		if link.State == dcgm.LS_UP {
			counts.Active++
		}
		m[link.ParentId] = counts
	}

	return summary, nil
}

// IsNotSupportedError returns true if the DCGM error indicates the feature
// is not supported on the current hardware (e.g., no NVSwitch on the SKU).
func IsNotSupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "not_supported") ||
		strings.Contains(msg, "not compatible")
}
