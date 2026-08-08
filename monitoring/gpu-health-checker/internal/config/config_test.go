// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	content := `
expectedGPUs: 4
gpuType: GB200
maxFailedGPUs: 0
checkInterval: 30s
checks:
  health:
    level: 2
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ExpectedGPUs != 4 {
		t.Errorf("expectedGPUs=%d, want 4", cfg.ExpectedGPUs)
	}
	if cfg.Checks.Health.Level != 2 {
		t.Errorf("health level=%d, want 2", cfg.Checks.Health.Level)
	}
}

func TestLoad_Defaults(t *testing.T) {
	content := `
expectedGPUs: 8
gpuType: H100
maxFailedGPUs: 1
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Check defaults are applied.
	if cfg.CheckInterval.Seconds() != 60 {
		t.Errorf("checkInterval=%v, want 60s", cfg.CheckInterval)
	}
	if cfg.Checks.ECC.SBERateThreshold != 10 {
		t.Errorf("sbeRateThreshold=%v, want 10", cfg.Checks.ECC.SBERateThreshold)
	}
	if cfg.Checks.Health.Level != 1 {
		t.Errorf("health level=%d, want 1 (default)", cfg.Checks.Health.Level)
	}
	if len(cfg.Checks.XID.CriticalCodes) == 0 {
		t.Error("expected default critical XID codes")
	}
	if len(cfg.Checks.XID.WarningCodes) == 0 {
		t.Error("expected default warning XID codes")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"missing gpuType", `expectedGPUs: 4
maxFailedGPUs: 0`},
		{"zero expectedGPUs", `expectedGPUs: 0
gpuType: H100
maxFailedGPUs: 0`},
		{"negative maxFailedGPUs", `expectedGPUs: 4
gpuType: H100
maxFailedGPUs: -1`},
		{"maxFailed >= expected", `expectedGPUs: 4
gpuType: H100
maxFailedGPUs: 4`},
		{"invalid health level", `expectedGPUs: 4
gpuType: H100
maxFailedGPUs: 0
checks:
  health:
    level: 5`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := Load(path)
			if err == nil {
				t.Errorf("expected validation error for %q", tt.name)
			}
		})
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestIsCriticalXID(t *testing.T) {
	content := `
expectedGPUs: 4
gpuType: H100
maxFailedGPUs: 0
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(path, []byte(content), 0644)
	cfg, _ := Load(path)

	// Default critical codes include 48.
	if !cfg.IsCriticalXID(48) {
		t.Error("XID 48 should be critical")
	}
	if cfg.IsCriticalXID(999) {
		t.Error("XID 999 should not be critical")
	}
}

func TestIsWarningXID(t *testing.T) {
	content := `
expectedGPUs: 4
gpuType: H100
maxFailedGPUs: 0
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(path, []byte(content), 0644)
	cfg, _ := Load(path)

	// Default warning codes include 56.
	if !cfg.IsWarningXID(56) {
		t.Error("XID 56 should be warning")
	}
	if cfg.IsWarningXID(48) {
		t.Error("XID 48 should not be warning (it's critical)")
	}
}
