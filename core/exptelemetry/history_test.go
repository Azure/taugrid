// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package exptelemetry

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestProjectJSONLHistoryRowGolden(t *testing.T) {
	row := decodeHistoryRow(t, `{
		"_step": 7,
		"_timestamp": 1790274612.25,
		"_runtime": 12,
		"train/loss": 0.75,
		"gpu/memory_allocated_gb": 42,
		"phase": "train",
		"ready": true,
		"nothing": null
	}`)
	opts := validHistoryOptions()
	opts.MetricPrefix = "trial"
	events, err := ProjectJSONLHistoryRow(row, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	var got []byte
	for _, event := range events {
		raw, err := EncodeMetricNDJSON(event)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, raw...)
	}
	assertGolden(t, "history_events.golden", got)

	if events[0].MetricName != "trial/gpu/memory_allocated_gb" || events[1].MetricName != "trial/train/loss" {
		t.Fatalf("events are not deterministically ordered by raw key: %+v", events)
	}
	if events[0].Tags["jsonl.raw_key"] != "gpu/memory_allocated_gb" ||
		events[0].Tags["jsonl.history_file"] != "history.jsonl" ||
		events[0].Tags["jsonl.history_line"] != "9" ||
		events[0].Tags["jsonl.importer"] != JSONLImporterVersion ||
		events[0].Tags["tau.metric.card"] != "Other metrics" ||
		events[0].Tags["tau.metric.standard"] != "false" {
		t.Fatalf("unexpected JSONL metadata tags: %+v", events[0].Tags)
	}
}

func TestProjectJSONLHistoryRowMatchesImporterScalarRules(t *testing.T) {
	row := map[string]any{
		"_step":      json.Number("2"),
		"_timestamp": json.Number("1770000000.000001"),
		"_metadata":  float64(99),
		"float":      float64(1.5),
		"integer":    int64(2),
		"string":     "3",
		"boolean":    true,
		"array":      []any{1},
	}
	events, err := ProjectJSONLHistoryRow(row, validHistoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].MetricName != "float" || events[1].MetricName != "integer" {
		t.Fatalf("unexpected scalar projection: %+v", events)
	}
	if events[0].WallTime.UnixMicro() != 1770000000000001 {
		t.Fatalf("timestamp = %s, want microsecond epoch projection", events[0].WallTime)
	}
}

func TestProjectJSONLHistoryRowRejectsInvalidContractInputs(t *testing.T) {
	tests := []struct {
		name   string
		row    map[string]any
		mutate func(*JSONLHistoryOptions)
		want   string
	}{
		{"missing step", map[string]any{"_timestamp": 1.0, "loss": 1.0}, nil, "_step"},
		{"negative step", map[string]any{"_step": -1.0, "_timestamp": 1.0, "loss": 1.0}, nil, "nonnegative"},
		{"missing timestamp", map[string]any{"_step": 1.0, "loss": 1.0}, nil, "_timestamp"},
		{"nonfinite timestamp", map[string]any{"_step": 1.0, "_timestamp": math.Inf(1), "loss": 1.0}, nil, "finite epoch seconds"},
		{"nonfinite metric", map[string]any{"_step": 1.0, "_timestamp": 1.0, "loss": math.NaN()}, nil, "finite number"},
		{"protected JSONL tag", map[string]any{"_step": 1.0, "_timestamp": 1.0, "loss": 1.0}, func(opts *JSONLHistoryOptions) {
			opts.Tags["tau.metric.card"] = "override"
		}, "cannot override protected JSONL metadata"},
		{"protected identity tag", map[string]any{"_step": 1.0, "_timestamp": 1.0, "loss": 1.0}, func(opts *JSONLHistoryOptions) {
			opts.Tags["project"] = "override"
		}, "cannot override a protected metric field"},
		{"invalid line", map[string]any{"_step": 1.0, "_timestamp": 1.0, "loss": 1.0}, func(opts *JSONLHistoryOptions) {
			opts.HistoryLine = 0
		}, "history line must be positive"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := validHistoryOptions()
			if test.mutate != nil {
				test.mutate(&opts)
			}
			_, err := ProjectJSONLHistoryRow(test.row, opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestProjectJSONLHistoryRowTruncatesFractionalStepLikePortalImporter(t *testing.T) {
	row := map[string]any{"_step": json.Number("1.75"), "_timestamp": 1.0, "loss": 1.0}
	events, err := ProjectJSONLHistoryRow(row, validHistoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Step != 1 {
		t.Fatalf("fractional history step projection = %+v, want step 1", events)
	}
}

func TestProjectJSONLHistoryRowPreservesInt64Step(t *testing.T) {
	row := map[string]any{"_step": int64(math.MaxInt64), "_timestamp": 1.0, "loss": 1.0}
	events, err := ProjectJSONLHistoryRow(row, validHistoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Step != math.MaxInt64 {
		t.Fatalf("int64 history step projection = %+v, want MaxInt64", events)
	}
}

func TestProjectJSONLHistoryRowPreservesMaxInt64JSONStep(t *testing.T) {
	row := map[string]any{"_step": json.Number("9223372036854775807"), "_timestamp": 1.0, "loss": 1.0}
	events, err := ProjectJSONLHistoryRow(row, validHistoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Step != math.MaxInt64 {
		t.Fatalf("JSON history step projection = %+v, want MaxInt64", events)
	}
}

func TestProjectJSONLHistoryRowRejectsOverflowingJSONIntegerStep(t *testing.T) {
	row := map[string]any{"_step": json.Number("9223372036854775808"), "_timestamp": 1.0, "loss": 1.0}
	_, err := ProjectJSONLHistoryRow(row, validHistoryOptions())
	if err == nil || !strings.Contains(err.Error(), "representable as int64") {
		t.Fatalf("error = %v, want int64 range error", err)
	}
}

func TestProjectJSONLHistoryRowRejectsFloatStepAtInt64Limit(t *testing.T) {
	row := map[string]any{"_step": math.Exp2(63), "_timestamp": 1.0, "loss": 1.0}
	_, err := ProjectJSONLHistoryRow(row, validHistoryOptions())
	if err == nil || !strings.Contains(err.Error(), "representable as int64") {
		t.Fatalf("error = %v, want int64 range error", err)
	}
}

func TestResearchMetricMappingMatchesPortalImporter(t *testing.T) {
	tests := []struct {
		name     string
		card     string
		standard bool
	}{
		{"train/return", "Outcome", true},
		{"train/loss", "World model", true},
		{"train/grad_norm", "Optimization", true},
		{"train/input_tokens", "Throughput", true},
		{"gpu/memory_allocated_gb", "Systems", true},
		{"checkpoint/bytes", "Checkpoint", true},
		{"feature/image_text_alignment", "Model diagnostics", true},
		{"policy/entropy", "Behavior", true},
		{"custom/value", "Other metrics", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ResearchMetricCard(test.name); got != test.card {
				t.Fatalf("ResearchMetricCard() = %q, want %q", got, test.card)
			}
			if got := IsStandardResearchMetric(test.name); got != test.standard {
				t.Fatalf("IsStandardResearchMetric() = %v, want %v", got, test.standard)
			}
		})
	}
}

func decodeHistoryRow(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var row map[string]any
	if err := decoder.Decode(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func validHistoryOptions() JSONLHistoryOptions {
	return JSONLHistoryOptions{
		WorkspaceID:    "workspace-a",
		Cluster:        "cluster-a",
		SourceStoreID:  "store-a",
		Project:        "project-alpha",
		ExperimentID:   "experiment-alpha",
		RunGroupID:     "baseline",
		RunID:          "seed-1",
		Source:         "captioner-jsonl",
		Tags:           map[string]string{" dataset ": " vision ", "recipe": "vit-enc"},
		HistoryPath:    "runs/seed-1/history.jsonl",
		HistoryLine:    9,
		ExportedAt:     time.Date(2026, 9, 18, 19, 0, 0, 0, time.UTC),
		MetricFileID:   "jsonl-seed-1",
		MetricFilePath: "metrics/seed-1.parquet",
	}
}
