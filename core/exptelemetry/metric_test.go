// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package exptelemetry

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricEventGolden(t *testing.T) {
	event := validMetricEvent(t)
	event.Tags = map[string]string{"zebra": "last", "alpha": "first", "html": "<ok>"}
	got, err := EncodeMetricNDJSON(event)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "metric_event.golden", got)

	if strings.Count(string(got), "\n") != 1 || !strings.HasSuffix(string(got), "\n") {
		t.Fatalf("NDJSON must contain exactly one newline: %q", got)
	}
	if strings.Contains(string(got), `\u003c`) {
		t.Fatalf("canonical encoding unexpectedly HTML-escaped tags: %s", got)
	}
	if !strings.Contains(string(got), `"wall_time":"2026-09-18T18:30:12.123456789Z"`) {
		t.Fatalf("wall_time was not encoded as RFC3339Nano: %s", got)
	}
}

func TestMetricEventValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MetricEvent)
		want   string
	}{
		{"unknown schema", func(e *MetricEvent) { e.SchemaVersion = "tau.experiment.metric.v2" }, "unknown metric schema"},
		{"project required", func(e *MetricEvent) { e.Project = "" }, "project is required"},
		{"experiment required", func(e *MetricEvent) { e.ExperimentID = "" }, "experiment_id is required"},
		{"group required", func(e *MetricEvent) { e.RunGroupID = "" }, "run_group_id is required"},
		{"run required", func(e *MetricEvent) { e.RunID = "" }, "run_id is required"},
		{"metric required", func(e *MetricEvent) { e.MetricName = " " }, "metric_name is required"},
		{"negative step", func(e *MetricEvent) { e.Step = -1 }, "step must be nonnegative"},
		{"zero wall time", func(e *MetricEvent) { e.WallTime = time.Time{} }, "wall_time is required"},
		{"invalid wall time", func(e *MetricEvent) { e.WallTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, "wall_time is invalid"},
		{"zero exported at", func(e *MetricEvent) { e.ExportedAt = time.Time{} }, "exported_at is required"},
		{"nan", func(e *MetricEvent) { e.Value = math.NaN() }, "value must be finite"},
		{"positive infinity", func(e *MetricEvent) { e.Value = math.Inf(1) }, "value must be finite"},
		{"protected tag", func(e *MetricEvent) { e.Tags = map[string]string{"run_id": "other"} }, `tag "run_id" cannot override`},
		{"missing event id", func(e *MetricEvent) { e.EventID = "" }, "event_id is required"},
		{"wrong event id", func(e *MetricEvent) { e.EventID = "metric-wrong" }, "does not match deterministic identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validMetricEvent(t)
			test.mutate(&event)
			if event.EventID != "" && test.name != "wrong event id" {
				event.EventID = event.DeterministicEventID()
			}
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestDeterministicEventIDUsesCurrentRawChartPointIdentity(t *testing.T) {
	event := validMetricEvent(t)
	first := event.DeterministicEventID()

	event.ExportedAt = event.ExportedAt.Add(time.Hour)
	event.Tags = map[string]string{"attempt": "2"}
	event.Value++
	event.Unit = "different-unit"
	event.Source = "different-source"
	event.Split = "eval"
	event.MetricFilePath = "different/path"
	if got := event.DeterministicEventID(); got != first {
		t.Fatalf("non-identity chart metadata changed event ID: %q != %q", got, first)
	}

	event.Cluster = "different-cluster"
	if got := event.DeterministicEventID(); got == first {
		t.Fatal("cluster did not change raw chart point identity")
	}
	event.Cluster = "cluster-a"
	event.Namespace = "different-namespace"
	if got := event.DeterministicEventID(); got == first {
		t.Fatal("namespace did not change metric event identity")
	}
	event.Namespace = "namespace-a"
	event.Step++
	if got := event.DeterministicEventID(); got == first {
		t.Fatal("raw chart point identity did not change event ID")
	}
}

func TestEncodeMetricNDJSONNormalizesSchemaIDAndTimezone(t *testing.T) {
	event := validMetricEvent(t)
	event.SchemaVersion = ""
	event.EventID = ""
	event.WallTime = time.Date(2026, 9, 18, 11, 30, 12, 0, time.FixedZone("PDT", -7*60*60))
	raw, err := EncodeMetricNDJSON(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MetricEvent
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != MetricSchemaV1 || decoded.EventID == "" {
		t.Fatalf("schema and event ID were not populated: %+v", decoded)
	}
	if decoded.WallTime.Location() != time.UTC || decoded.WallTime.Hour() != 18 {
		t.Fatalf("wall_time was not normalized to UTC: %s", decoded.WallTime)
	}
}

func validMetricEvent(t *testing.T) MetricEvent {
	t.Helper()
	event, err := NewMetricEvent(MetricEvent{
		WorkspaceID:    "workspace-a",
		Cluster:        "cluster-a",
		Namespace:      "namespace-a",
		SourceStoreID:  "store-a",
		Project:        "project-alpha",
		ExperimentID:   "experiment-alpha",
		RunGroupID:     "baseline",
		RunID:          "seed-1",
		MetricName:     "train/loss",
		Step:           42,
		WallTime:       time.Date(2026, 9, 18, 18, 30, 12, 123456789, time.UTC),
		Value:          0.125,
		Unit:           "ratio",
		Source:         "jsonl",
		Split:          "train",
		MetricFileID:   "metrics-seed-1",
		MetricFilePath: "metrics/history.jsonl",
		Tags:           map[string]string{"suite": "smoke"},
		ExportedAt:     time.Date(2026, 9, 18, 18, 31, 0, 987654321, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch for %s\nwant: %s\ngot:  %s", name, want, got)
	}
}
