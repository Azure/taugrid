// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"strings"
	"testing"
)

func floatPtr(v float64) *float64 { return &v }

func TestParseModelRef(t *testing.T) {
	tests := []struct {
		in   string
		want modelRef
	}{
		{"sample", modelRef{Model: "sample", Alias: "default"}},
		{"sample:best-loss", modelRef{Model: "sample", Alias: "best-loss"}},
		{"sample@run-1", modelRef{Model: "sample", Run: "run-1"}},
	}
	for _, tt := range tests {
		got, err := parseModelRef(tt.in)
		if err != nil {
			t.Fatalf("parseModelRef(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("parseModelRef(%q)=%+v want %+v", tt.in, got, tt.want)
		}
	}
	if _, err := parseModelRef("sample:"); err == nil {
		t.Fatal("expected empty alias to fail")
	}
}

func TestSelectBestModelRecord(t *testing.T) {
	records := []modelRegistryRecord{
		{
			Model:   "sample",
			Run:     "run-a",
			Metrics: map[string]float64{"loss": 0.4},
			PrimaryMetric: modelRegistryMetric{
				Name:      "loss",
				Value:     floatPtr(0.4),
				Direction: "lower",
			},
		},
		{
			Model:   "sample",
			Run:     "run-b",
			Metrics: map[string]float64{"loss": 0.2},
			PrimaryMetric: modelRegistryMetric{
				Name:      "loss",
				Value:     floatPtr(0.2),
				Direction: "lower",
			},
		},
	}
	best, err := selectBestModelRecord(records, "loss", "")
	if err != nil {
		t.Fatal(err)
	}
	if best.Run != "run-b" {
		t.Fatalf("best=%s want run-b", best.Run)
	}
}

func TestSelectBestModelRecordRejectsMixedPrimaryMetrics(t *testing.T) {
	records := []modelRegistryRecord{
		{
			Model:   "sample",
			Run:     "run-a",
			Metrics: map[string]float64{"loss": 0.2},
			PrimaryMetric: modelRegistryMetric{
				Name:      "loss",
				Value:     floatPtr(0.2),
				Direction: "lower",
			},
		},
		{
			Model:   "sample",
			Run:     "run-b",
			Metrics: map[string]float64{"accuracy": 0.9},
			PrimaryMetric: modelRegistryMetric{
				Name:      "accuracy",
				Value:     floatPtr(0.9),
				Direction: "higher",
			},
		},
	}
	_, err := selectBestModelRecord(records, "", "")
	if err == nil || !strings.Contains(err.Error(), "pass --metric") {
		t.Fatalf("expected mixed primary metrics error, got %v", err)
	}
	best, err := selectBestModelRecord(records, "accuracy", "higher")
	if err != nil {
		t.Fatal(err)
	}
	if best.Run != "run-b" {
		t.Fatalf("best=%s want run-b", best.Run)
	}
}

func TestFilterAndPrintModelRecords(t *testing.T) {
	records := []modelRegistryRecord{
		{
			Model:   "sample",
			Run:     "run-a",
			Tags:    map[string]string{"dataset": "era5"},
			Metrics: map[string]float64{"loss": 0.4},
			Artifacts: []managedWorkflowArtifact{{
				Name:         "checkpoint",
				ManifestPath: "final.safetensors",
				Status:       "ready",
			}},
		},
		{
			Model:   "sample",
			Run:     "run-b",
			Tags:    map[string]string{"dataset": "other"},
			Metrics: map[string]float64{"loss": 0.2},
		},
	}
	filtered := filterModelRecords(records, map[string]string{"dataset": "era5"}, "loss")
	if len(filtered) != 1 || filtered[0].Run != "run-a" {
		t.Fatalf("filtered=%+v", filtered)
	}
	var buf bytes.Buffer
	if err := printModelRecordsTable(&buf, filtered, "loss"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"MODEL", "sample", "run-a", "0.4", "dataset=era5", "ready"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSelectModelArtifact(t *testing.T) {
	record := modelRegistryRecord{
		Model: "sample",
		Run:   "run-a",
		Artifacts: []managedWorkflowArtifact{{
			Name:         "checkpoint",
			ManifestPath: "rank0/final.safetensors",
			DurablePath:  "/data/checkpoints/finetunes/run-a/artifacts/rank0/final.safetensors",
			Status:       "ready",
			FileCount:    1,
		}},
	}
	artifact, err := selectModelArtifact(record, "rank0/final.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.DurablePath == "" || artifact.FileCount != 1 {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestNumericMetricsFromJSON(t *testing.T) {
	got := numericMetricsFromJSON([]byte(`{"loss":0.1,"nested":{"accuracy":0.9},"ignored":true}`))
	if got["loss"] != 0.1 || got["nested.accuracy"] != 0.9 {
		t.Fatalf("metrics=%+v", got)
	}
	if _, ok := got["ignored"]; ok {
		t.Fatalf("boolean should not be numeric: %+v", got)
	}
}

func TestInferPrimaryMetricKeepsEvalLossUserMetric(t *testing.T) {
	metric := inferPrimaryMetric(map[string]float64{
		"eval.loss":     0.2,
		"workload.loss": 0.3,
	}, "", "")
	if metric.Name != "eval.loss" {
		t.Fatalf("primary metric = %q, want eval.loss", metric.Name)
	}
}

func TestHelperPodPayloadArgSizeGuard(t *testing.T) {
	encoded, err := helperPodPayloadArg([]byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" {
		t.Fatal("expected encoded payload")
	}
	if _, err := helperPodPayloadArg(make([]byte, maxHelperPodScriptArgPayload)); err == nil {
		t.Fatal("expected oversized helper pod payload to fail")
	}
}
