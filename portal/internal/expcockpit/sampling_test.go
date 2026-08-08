// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"math"
	"reflect"
	"testing"
)

func TestHybridSamplingPreservesExtremaMilestonesAndBudget(t *testing.T) {
	const (
		pointCount = 10_000
		budget     = 120
	)
	points := make([]metricPoint, 0, pointCount)
	for step := 0; step < pointCount; step++ {
		value := math.Sin(float64(step) / 100)
		switch step {
		case 4321:
			value = -1000
		case 8765:
			value = 1000
		}
		points = append(points, metricPoint{RunID: "run-a", MetricName: "train/loss", Step: int64(step), Value: value})
	}
	milestones := map[int64]bool{1000: true, 5000: true, 9000: true}

	got, metadata := sampleMetricPoints(points, budget, milestones)
	if len(got) != budget {
		t.Fatalf("rendered points = %d, want exact budget %d", len(got), budget)
	}
	assertSampleSteps(t, got, 0, 1000, 4321, 5000, 8765, 9000, pointCount-1)
	for idx := 1; idx < len(got); idx++ {
		if got[idx-1].Step >= got[idx].Step {
			t.Fatalf("sample steps are not unique and increasing at %d: %d >= %d", idx, got[idx-1].Step, got[idx].Step)
		}
	}
	if !metadata.FirstRetained || !metadata.LastRetained || !metadata.MinRetained || !metadata.MaxRetained {
		t.Fatalf("sampling did not retain required endpoint/extrema metadata: %+v", metadata)
	}
	if metadata.MilestonePoints != len(milestones) || metadata.MilestonesRetained != len(milestones) {
		t.Fatalf("milestone metadata = %+v, want all %d retained", metadata, len(milestones))
	}
	if metadata.Algorithm != "minmax_lttb" || metadata.PreselectedPoints > budget*4+len(milestones) {
		t.Fatalf("unexpected preselection metadata: %+v", metadata)
	}

	repeated, repeatedMetadata := sampleMetricPoints(points, budget, milestones)
	if !reflect.DeepEqual(got, repeated) || !reflect.DeepEqual(metadata, repeatedMetadata) {
		t.Fatal("hybrid sampling is not deterministic")
	}
}

func TestHybridSamplingLeavesShortSeriesUnchanged(t *testing.T) {
	points := []metricPoint{
		{Step: 0, Value: 1},
		{Step: 1, Value: 2},
		{Step: 2, Value: 3},
	}
	got, metadata := sampleMetricPoints(points, 10, nil)
	if !reflect.DeepEqual(got, points) {
		t.Fatalf("short series changed: got %+v want %+v", got, points)
	}
	if metadata.Algorithm != "none" || metadata.Truncated {
		t.Fatalf("unexpected short-series metadata: %+v", metadata)
	}
}

func TestHybridSamplingDeduplicatesStepsDeterministically(t *testing.T) {
	points := []metricPoint{
		{Step: 2, Value: 2},
		{Step: 1, Value: 1},
		{Step: 1, Value: 10},
		{Step: 3, Value: 3},
	}
	got, _ := sampleMetricPoints(points, 10, nil)
	want := []metricPoint{
		{Step: 1, Value: 10},
		{Step: 2, Value: 2},
		{Step: 3, Value: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deduplicated points = %+v, want %+v", got, want)
	}
}

func TestValidationMilestonesAreDerivedFromCompanionMetrics(t *testing.T) {
	points := []metricPoint{
		{RunID: "run-a", MetricName: "train/loss", Step: 10},
		{RunID: "run-a", MetricName: "eval/accuracy", Step: 10},
		{RunID: "run-a", MetricName: "validation/loss", Step: 20},
		{RunID: "run-b", MetricName: "final/score", Step: 30},
		{RunID: "run-a", MetricName: "system/gpu", Step: 40},
	}
	got := validationMilestoneSteps(points, "train/loss")
	if !got["run-a"][10] || !got["run-a"][20] || got["run-a"][40] || !got["run-b"][30] {
		t.Fatalf("unexpected milestone steps: %+v", got)
	}
}

func BenchmarkHybridSamplingMillionPoints(b *testing.B) {
	points := make([]metricPoint, 1_000_000)
	for idx := range points {
		points[idx] = metricPoint{
			RunID:      "run-a",
			MetricName: "train/loss",
			Step:       int64(idx),
			Value:      math.Sin(float64(idx) / 1000),
		}
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		got, _ := sampleMetricPoints(points, 1200, nil)
		if len(got) != 1200 {
			b.Fatalf("sample size = %d, want 1200", len(got))
		}
	}
}

func assertSampleSteps(t *testing.T, points []metricPoint, steps ...int) {
	t.Helper()
	got := make(map[int64]bool, len(points))
	for _, point := range points {
		got[point.Step] = true
	}
	for _, step := range steps {
		if !got[int64(step)] {
			t.Fatalf("required step %d was not retained", step)
		}
	}
}
