// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadtelemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

type fakeQuerier struct {
	kql  string
	rows []kustoquery.Row
}

func (f *fakeQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	f.kql = kql
	return f.rows, nil
}

func TestFetchScopesByPodNodeAndAbsoluteWindow(t *testing.T) {
	q := &fakeQuerier{rows: []kustoquery.Row{{
		"instance": "gpu-a", "pod": "trainer-0", "gpu": "0", "modelName": "H100",
		"samples": "16", "utilizationSamples": "2", "averageUtilizationPct": "75",
		"peakUtilizationPct": "98", "maxTemperatureCelsius": "70", "maxRowRemapFailure": "0",
	}}}
	start := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	got, err := Fetch(context.Background(), q, Query{
		Cluster: "cluster-a", Namespace: "team-a",
		Pods:  []PodTarget{{Pod: "trainer-0", Instance: "gpu-a"}},
		Start: start, End: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"datetime(2026-07-02T10:00:00Z)", "datetime(2026-07-02T11:00:00Z)", "@'cluster-a'", "@'team-a'", "@'trainer-0', @'gpu-a'"} {
		if !strings.Contains(q.kql, want) {
			t.Fatalf("query missing %q:\n%s", want, q.kql)
		}
	}
	if got.GPUCount != 1 || got.SampleCount != 16 || got.AverageUtilizationPct == nil || *got.AverageUtilizationPct != 75 || got.Coverage != "observed" {
		t.Fatalf("summary = %+v", got)
	}
}

func TestFetchRejectsIncompleteIdentity(t *testing.T) {
	_, err := Fetch(context.Background(), &fakeQuerier{}, Query{
		Namespace: "team-a", Pods: []PodTarget{{Pod: "trainer-0", Instance: "gpu-a"}},
		Start: time.Now().Add(-time.Hour), End: time.Now(),
	})
	if err == nil {
		t.Fatal("expected incomplete query error")
	}
}
