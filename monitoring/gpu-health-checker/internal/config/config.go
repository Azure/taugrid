// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ExpectedGPUs   int           `yaml:"expectedGPUs"`
	GPUType        string        `yaml:"gpuType"`
	DriverVersions []string      `yaml:"driverVersions"`
	VBIOSVersions  []string      `yaml:"vbiosVersions"`
	MaxFailedGPUs  int           `yaml:"maxFailedGPUs"`
	NVLink         NVLinkConfig  `yaml:"nvlink"`
	Checks         ChecksConfig  `yaml:"checks"`
	CheckInterval  time.Duration `yaml:"checkInterval"`

	criticalXIDSet map[int]bool
	warningXIDSet  map[int]bool
}

type NVLinkConfig struct {
	ExpectedActiveLinksPerGPU int  `yaml:"expectedActiveLinksPerGPU"`
	CheckC2C                  bool `yaml:"checkC2C"`
	CheckNVSwitch             bool `yaml:"checkNVSwitch"`
}

type ChecksConfig struct {
	XID     XIDCheckConfig     `yaml:"xid"`
	ECC     ECCCheckConfig     `yaml:"ecc"`
	NVLink  CheckEnabled       `yaml:"nvlink"`
	Thermal ThermalCheckConfig `yaml:"thermal"`
	PCIe    PCIeCheckConfig    `yaml:"pcie"`
	Clocks  CheckEnabled       `yaml:"clocks"`
	Health  HealthCheckConfig  `yaml:"health"`
	Info    CheckEnabled       `yaml:"info"`
}

type CheckEnabled struct {
	Enabled bool `yaml:"enabled"`
}

type XIDCheckConfig struct {
	Enabled       bool  `yaml:"enabled"`
	CriticalCodes []int `yaml:"criticalCodes"`
	WarningCodes  []int `yaml:"warningCodes"`
}

type ECCCheckConfig struct {
	Enabled          bool          `yaml:"enabled"`
	SBERateThreshold float64       `yaml:"sbeRateThreshold"`
	SBERateWindow    time.Duration `yaml:"sbeRateWindow"`
}

type PCIeCheckConfig struct {
	Enabled             bool          `yaml:"enabled"`
	ReplayRateThreshold float64       `yaml:"replayRateThreshold"`
	ReplayRateWindow    time.Duration `yaml:"replayRateWindow"`
}

type ThermalCheckConfig struct {
	Enabled              bool    `yaml:"enabled"`
	GPUTempWarning       float64 `yaml:"gpuTempWarning"`
	GPUTempCritical      float64 `yaml:"gpuTempCritical"`
	CPUTempWarning       float64 `yaml:"cpuTempWarning"`
	CPUTempCritical      float64 `yaml:"cpuTempCritical"`
	NVSwitchTempWarning  float64 `yaml:"nvswitchTempWarning"`
	NVSwitchTempCritical float64 `yaml:"nvswitchTempCritical"`
}

type HealthCheckConfig struct {
	Enabled bool `yaml:"enabled"`
	Level   int  `yaml:"level"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.CheckInterval == 0 {
		c.CheckInterval = 60 * time.Second
	}
	if c.Checks.ECC.SBERateThreshold == 0 {
		c.Checks.ECC.SBERateThreshold = 10
	}
	if c.Checks.ECC.SBERateWindow == 0 {
		c.Checks.ECC.SBERateWindow = 10 * time.Minute
	}
	if c.Checks.PCIe.ReplayRateThreshold == 0 {
		c.Checks.PCIe.ReplayRateThreshold = 5
	}
	if c.Checks.PCIe.ReplayRateWindow == 0 {
		c.Checks.PCIe.ReplayRateWindow = 5 * time.Minute
	}
	if c.Checks.Thermal.GPUTempWarning == 0 {
		c.Checks.Thermal.GPUTempWarning = 85
	}
	if c.Checks.Thermal.GPUTempCritical == 0 {
		c.Checks.Thermal.GPUTempCritical = 95
	}
	if c.Checks.Thermal.CPUTempWarning == 0 {
		c.Checks.Thermal.CPUTempWarning = 90
	}
	if c.Checks.Thermal.CPUTempCritical == 0 {
		c.Checks.Thermal.CPUTempCritical = 100
	}
	if c.Checks.Thermal.NVSwitchTempWarning == 0 {
		c.Checks.Thermal.NVSwitchTempWarning = 90
	}
	if c.Checks.Thermal.NVSwitchTempCritical == 0 {
		c.Checks.Thermal.NVSwitchTempCritical = 100
	}
	if c.Checks.Health.Level == 0 {
		c.Checks.Health.Level = 1
	}
	if len(c.Checks.XID.CriticalCodes) == 0 {
		c.Checks.XID.CriticalCodes = []int{48, 63, 64, 68, 73, 74, 79, 94, 95}
	}
	if len(c.Checks.XID.WarningCodes) == 0 {
		c.Checks.XID.WarningCodes = []int{56, 57, 58, 62, 65, 69, 80, 81, 92, 119, 120}
	}
	c.criticalXIDSet = make(map[int]bool, len(c.Checks.XID.CriticalCodes))
	for _, code := range c.Checks.XID.CriticalCodes {
		c.criticalXIDSet[code] = true
	}
	c.warningXIDSet = make(map[int]bool, len(c.Checks.XID.WarningCodes))
	for _, code := range c.Checks.XID.WarningCodes {
		c.warningXIDSet[code] = true
	}
}

func (c *Config) validate() error {
	if c.ExpectedGPUs <= 0 {
		return fmt.Errorf("expectedGPUs must be > 0, got %d", c.ExpectedGPUs)
	}
	if c.GPUType == "" {
		return fmt.Errorf("gpuType is required")
	}
	if c.MaxFailedGPUs < 0 {
		return fmt.Errorf("maxFailedGPUs must be >= 0, got %d", c.MaxFailedGPUs)
	}
	if c.MaxFailedGPUs >= c.ExpectedGPUs {
		return fmt.Errorf("maxFailedGPUs (%d) must be less than expectedGPUs (%d)", c.MaxFailedGPUs, c.ExpectedGPUs)
	}
	if c.Checks.Health.Level < 1 || c.Checks.Health.Level > 3 {
		return fmt.Errorf("health check level must be 1, 2, or 3, got %d", c.Checks.Health.Level)
	}
	if c.Checks.Thermal.GPUTempWarning >= c.Checks.Thermal.GPUTempCritical {
		return fmt.Errorf("gpuTempWarning (%.0f) must be less than gpuTempCritical (%.0f)",
			c.Checks.Thermal.GPUTempWarning, c.Checks.Thermal.GPUTempCritical)
	}
	if c.Checks.Thermal.CPUTempWarning >= c.Checks.Thermal.CPUTempCritical {
		return fmt.Errorf("cpuTempWarning (%.0f) must be less than cpuTempCritical (%.0f)",
			c.Checks.Thermal.CPUTempWarning, c.Checks.Thermal.CPUTempCritical)
	}
	if c.Checks.Thermal.NVSwitchTempWarning >= c.Checks.Thermal.NVSwitchTempCritical {
		return fmt.Errorf("nvswitchTempWarning (%.0f) must be less than nvswitchTempCritical (%.0f)",
			c.Checks.Thermal.NVSwitchTempWarning, c.Checks.Thermal.NVSwitchTempCritical)
	}
	return nil
}

func (c *Config) IsCriticalXID(xid int) bool {
	return c.criticalXIDSet[xid]
}

func (c *Config) IsWarningXID(xid int) bool {
	return c.warningXIDSet[xid]
}
