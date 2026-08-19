// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

const rulesBlock = `
rules:
  - name: ecc-dbe-retired
    metricName: DCGM_FI_DEV_ECC_DBE_AGG_TOTAL
    conditionType: GPUECCDoubleRetired
    mode: rate
    threshold: 0
    window: 1m
    for: 1m
`

func TestLoadAcceptsRequiredAvailabilityTarget(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
scrapeTargets:
  - name: dcgm-exporter
    url: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
    unavailableFor: 2m
    availableFor: 1m
  - name: node-exporter
    url: http://localhost:9100/metrics
`+rulesBlock)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	target := cfg.ScrapeTargets[0]
	if !target.Required || target.AvailabilityCondition != "DcgmExporterUnavailable" {
		t.Fatalf("availability contract not parsed: %+v", target)
	}
	if target.UnavailableFor != 2*time.Minute || target.AvailableFor != time.Minute {
		t.Errorf("unexpected windows: %+v", target)
	}
	if cfg.ScrapeTargets[1].Required {
		t.Error("optional targets must stay optional")
	}
}

func TestLoadAcceptsLegacyTargetsWithoutAvailability(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
scrapeTargets:
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
  - name: node-exporter
    url: http://localhost:9100/metrics
  - name: node-problem-detector
    url: http://localhost:20261/metrics
`+rulesBlock)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.ScrapeTargets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(cfg.ScrapeTargets))
	}
}

func TestLoadRejectsInvalidAvailabilityContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		targets string
		wantErr string
	}{
		{
			name: "required without condition",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    required: true
`,
			wantErr: "sets no availabilityCondition",
		},
		{
			name: "condition without required",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    availabilityCondition: DcgmExporterUnavailable
`,
			wantErr: "without required: true",
		},
		{
			name: "condition collides with a rule",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    required: true
    availabilityCondition: GPUECCDoubleRetired
`,
			wantErr: "claimed by both",
		},
		{
			name: "two targets claim one condition",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
  - name: dcgm-exporter-backup
    url: http://localhost:19401/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
`,
			wantErr: "claimed by both",
		},
		{
			name: "duplicate target name declaring availability",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
  - name: dcgm-exporter
    url: http://localhost:19401/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
`,
			wantErr: "declares an availability contract",
		},
		{
			name: "optional duplicate after availability owner",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
  - name: dcgm-exporter
    url: http://localhost:19401/metrics
`,
			wantErr: "at least one target declares an availability contract",
		},
		{
			name: "required target with no url",
			targets: `
  - name: dcgm-exporter
    url: ""
    required: true
    availabilityCondition: DcgmExporterUnavailable
`,
			wantErr: "declares an availability contract but sets no url",
		},
		{
			name: "unnamed target declaring availability",
			targets: `
  - name: ""
    url: http://localhost:19400/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
`,
			wantErr: "declares an availability contract but sets no name",
		},
		{
			name: "required target with relative url",
			targets: `
  - name: dcgm-exporter
    url: localhost:19400/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
`,
			wantErr: "absolute http or https url",
		},
		{
			name: "negative window",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    required: true
    availabilityCondition: DcgmExporterUnavailable
    unavailableFor: -1m
`,
			wantErr: "negative unavailableFor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "scrapeTargets:"+tc.targets+rulesBlock)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadRejectsKubernetesOwnedAvailabilityConditions(t *testing.T) {
	t.Parallel()

	for _, condition := range []string{
		"Ready",
		"MemoryPressure",
		"DiskPressure",
		"PIDPressure",
		"NetworkUnavailable",
	} {
		t.Run(condition, func(t *testing.T) {
			path := writeConfig(t, `
scrapeTargets:
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
    required: true
    availabilityCondition: `+condition+`
`+rulesBlock)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "owned by Kubernetes") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadAcceptsLegacyShapesThatPreviouslyLoaded(t *testing.T) {
	t.Parallel()

	// Every one of these was accepted before the availability contract existed.
	// Refusing to start would CrashLoopBackOff the collector on an image bump
	// and freeze every condition it owns, so they must still load.
	tests := []struct {
		name    string
		targets string
	}{
		{
			name: "target with no url",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
  - name: extra
    url: ""
`,
		},
		{
			name: "target with no name",
			targets: `
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
  - name: ""
    url: http://localhost:9100/metrics
`,
		},
		{
			name: "duplicate target names",
			targets: `
  - name: node-problem-detector
    url: http://localhost:20261/metrics
  - name: node-problem-detector
    url: http://localhost:20261/metrics
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, "scrapeTargets:"+tc.targets+rulesBlock))
			if err != nil {
				t.Fatalf("legacy config must still load: %v", err)
			}
			for _, target := range cfg.ScrapeTargets {
				if target.Required || target.AvailabilityCondition != "" {
					t.Fatalf("legacy config must stay inert: %+v", target)
				}
			}
		})
	}
}

func TestLoadAcceptsDuplicateRuleConditions(t *testing.T) {
	t.Parallel()

	// Previously accepted with last-writer-wins semantics in the condition
	// writer; it must not become a fatal startup error.
	path := writeConfig(t, `
scrapeTargets:
  - name: dcgm-exporter
    url: http://localhost:19400/metrics
rules:
  - name: a
    metricName: M
    conditionType: Same
    mode: instant
    threshold: 0
  - name: b
    metricName: N
    conditionType: Same
    mode: instant
    threshold: 0
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("duplicate rule condition types must still load: %v", err)
	}
}

func TestValidationErrorsNeverLeakCredentials(t *testing.T) {
	t.Parallel()

	// An unnamed target whose URL carries userinfo and a query token: the
	// startup error must identify the field, never echo the raw URL.
	path := writeConfig(t, `
scrapeTargets:
  - name: ""
    url: http://user:s3cret@localhost:19400/metrics?token=abc123
    required: true
    availabilityCondition: DcgmExporterUnavailable
`+rulesBlock)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"s3cret", "abc123", "user:s3cret", "token="} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("validation error %q leaked %q", err, secret)
		}
	}
	if !strings.Contains(err.Error(), "sets no name") {
		t.Errorf("error should name the missing field, got %q", err)
	}
}
