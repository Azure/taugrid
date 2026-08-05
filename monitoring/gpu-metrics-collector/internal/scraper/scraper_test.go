package scraper

import (
	"strings"
	"testing"
)

func TestParseExposition(t *testing.T) {
	t.Parallel()

	input := `# HELP DCGM_FI_DEV_ECC_DBE_VOL_TOTAL Total double-bit ECC errors
# TYPE DCGM_FI_DEV_ECC_DBE_VOL_TOTAL counter
DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{gpu="0",UUID="GPU-abc"} 0
DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{gpu="1",UUID="GPU-def"} 3
node_cpu_seconds_total{cpu="0",mode="idle"} 12345.67
`

	metrics, err := parseExposition(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(metrics) != 3 {
		t.Fatalf("expected 3 metrics, got %d", len(metrics))
	}

	// First metric.
	if metrics[0].Name != "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL" {
		t.Errorf("unexpected name: %s", metrics[0].Name)
	}
	if metrics[0].Labels["gpu"] != "0" {
		t.Errorf("unexpected gpu label: %s", metrics[0].Labels["gpu"])
	}
	if metrics[0].Value != 0 {
		t.Errorf("unexpected value: %f", metrics[0].Value)
	}

	// Second metric with non-zero value.
	if metrics[1].Value != 3 {
		t.Errorf("expected value 3, got %f", metrics[1].Value)
	}
}

func TestParseLabels_EscapedQuotes(t *testing.T) {
	t.Parallel()

	input := `DCGM_FI_DEV_GPU_UTIL{UUID="GPU-abc\"def",gpu="0"} 42`
	metrics, err := parseExposition(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}
	if metrics[0].Labels["UUID"] != `GPU-abc"def` {
		t.Errorf("unexpected UUID label: %q", metrics[0].Labels["UUID"])
	}
}

func TestParseLine_NoLabels(t *testing.T) {

	m, err := parseLine("up 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "up" || m.Value != 1 {
		t.Errorf("unexpected metric: %+v", m)
	}
}
