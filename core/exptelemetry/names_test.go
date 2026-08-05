package exptelemetry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelemetryNamesPreserveHostedAndCompatibilityContracts(t *testing.T) {
	if RemoteWriteMetricName != "experiment_metrics" {
		t.Fatalf("remote-write Prometheus metric = %q, want experiment_metrics", RemoteWriteMetricName)
	}
	if RemoteWriteDatabase != "Metrics" || RemoteWriteTable != "ExperimentMetrics" || RemoteWriteDashboardFunction != "ExperimentMetricsDashboardRows" {
		t.Fatalf("remote-write ADX contract = %s.%s -> %s(), want Metrics.ExperimentMetrics -> ExperimentMetricsDashboardRows()", RemoteWriteDatabase, RemoteWriteTable, RemoteWriteDashboardFunction)
	}
	if ProjectionTable != "TauExpMetrics" || ProjectionMetricsSpoolFile != "TauExpMetrics.jsonl" || ProjectionDashboardFunction != "TauExpMetricsDashboardRows" {
		t.Fatalf("projection compatibility contract = %s/%s/%s()", ProjectionTable, ProjectionMetricsSpoolFile, ProjectionDashboardFunction)
	}
	if RunStatusMetricName != "tau/run_status" {
		t.Fatalf("run terminal marker metric = %q, want tau/run_status", RunStatusMetricName)
	}
}

func TestValidateIDMatchesExpstoreContract(t *testing.T) {
	for _, tt := range []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid", value: "modernbert.v1_fine-web"},
		{name: "uppercase", value: "ModernBERT", wantErr: true},
		{name: "space", value: "modern bert", wantErr: true},
		{name: "leading separator", value: "-modernbert", wantErr: true},
		{name: "trailing separator", value: "modernbert.", wantErr: true},
		{name: "path separator", value: "modernbert/run", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateID("project", tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateID(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestTelemetrySchemaVersioningDocNamesContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "cli", "SDK_GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{
		fmt.Sprintf("Prometheus remote-write metric: `%s`", RemoteWriteMetricName),
		fmt.Sprintf("ADX database/table: `%s.%s`", RemoteWriteDatabase, RemoteWriteTable),
		fmt.Sprintf("Remote-write dashboard function: `%s()`", RemoteWriteDashboardFunction),
		fmt.Sprintf("Local metrics spool: `%s`", ProjectionMetricsSpoolFile),
		fmt.Sprintf("Projection dashboard function: `%s()`", ProjectionDashboardFunction),
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("telemetry schema versioning doc missing %q", want)
		}
	}
}
