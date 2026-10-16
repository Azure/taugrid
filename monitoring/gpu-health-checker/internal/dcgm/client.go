// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dcgm

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
)

type Capabilities struct {
	HasNVSwitch bool
	NVSwitchIDs []uint
}

type Client struct {
	closeOnce sync.Once
	cleanup   func()
	Caps      Capabilities
}

type GPUInfo struct {
	Index  uint
	UUID   string
	PCIBus string
	Name   string
	VBIOS  string
	Driver string
}

func NewClient() (*Client, error) {
	cleanup, err := dcgm.Init(dcgm.Embedded)
	if err != nil {
		return nil, fmt.Errorf("dcgm init (embedded): %w", err)
	}
	caps := probeCapabilities()
	return &Client{cleanup: cleanup, Caps: caps}, nil
}

func probeCapabilities() Capabilities {
	var caps Capabilities

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

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.cleanup != nil {
			c.cleanup()
		}
	})
	return nil
}

func (c *Client) GPUCount() (uint, error) {
	count, err := dcgm.GetAllDeviceCount()
	if err != nil {
		return 0, fmt.Errorf("dcgm get device count: %w", err)
	}
	return count, nil
}

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

type NVLinkSummary struct {
	Supported     bool
	GPULinks      map[uint]NVLinkCounts
	NVSwitchLinks map[uint]NVLinkCounts
}

type NVLinkCounts struct {
	Active int
	Total  int
}

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

func IsNotSupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "not_supported") ||
		strings.Contains(msg, "not compatible")
}
