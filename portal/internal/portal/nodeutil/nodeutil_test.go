// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kustoquery"
)

// fakeQuerier records the KQL it was asked to run and returns canned rows, so
// Board is exercised without a live Kusto.
type fakeQuerier struct {
	rows    []kustoquery.Row
	err     error
	lastKQL string
}

func (f *fakeQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	f.lastKQL = kql
	return f.rows, f.err
}

func TestBoardAggregatesJoinedRows(t *testing.T) {
	// CPU rates are now reduced from per-core samples in Go rather than KQL.
	var rows []kustoquery.Row
	for _, node := range []struct {
		instance string
		cores    int
		idle     float64
		total    float64
		avail    float64
	}{
		{"node-0", 64, 10.5, 200, 50},
		{"node-1", 16, 52.5, 100, 90},
	} {
		for cpu := 0; cpu < node.cores; cpu++ {
			rows = append(rows, kustoquery.Row{
				"Cluster": "cluster-a", "instance": node.instance, "kind": "cpu",
				"cpu": fmt.Sprint(cpu), "sampleCount": 2.0,
				"samples": []any{
					map[string]any{"timestamp": "2026-09-01T00:00:00Z", "value": 0.0},
					map[string]any{"timestamp": "2026-09-01T00:01:00Z", "value": node.idle},
				},
			})
		}
		rows = append(rows,
			kustoquery.Row{"Cluster": "cluster-a", "instance": node.instance, "kind": "memory_total", "memoryValue": node.total, "memoryTimestamp": "2026-09-01T00:01:00Z"},
			kustoquery.Row{"Cluster": "cluster-a", "instance": node.instance, "kind": "memory_available", "memoryValue": node.avail, "memoryTimestamp": "2026-09-01T00:01:00Z"},
		)
	}
	q := &fakeQuerier{rows: rows}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(snap.Nodes) != 2 {
		t.Fatalf("Nodes = %d, want 2", len(snap.Nodes))
	}
	if snap.Window != DefaultWindow.String() {
		t.Fatalf("Window = %q, want %q", snap.Window, DefaultWindow.String())
	}

	// Ordered hottest-CPU-first: node-0 (82.5) before node-1 (12.5).
	n0 := snap.Nodes[0]
	if n0.Instance != "node-0" || n0.CPUUtilPct == nil || *n0.CPUUtilPct != 82.5 || n0.CPUCores != 64 {
		t.Fatalf("node0 = %#v, want node-0 82.5%% / 64 cores", n0)
	}
	if n0.MemTotalBytes == nil || *n0.MemTotalBytes != 200 ||
		n0.MemAvailBytes == nil || *n0.MemAvailBytes != 50 ||
		n0.MemUsedPct == nil || *n0.MemUsedPct != 75 {
		t.Fatalf("node0 memory = %#v", n0)
	}

	n1 := snap.Nodes[1]
	if n1.Instance != "node-1" || n1.CPUUtilPct == nil || *n1.CPUUtilPct != 12.5 {
		t.Fatalf("node1 = %#v, want node-1 12.5%%", n1)
	}
}

func TestBoardEmpty(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(snap.Nodes) != 0 {
		t.Fatalf("empty snapshot = %#v", snap)
	}
	// Nodes must be a non-nil slice so it serializes as [] not null.
	if snap.Nodes == nil {
		t.Fatal("Nodes is nil, want empty slice")
	}
}

func TestBoardPropagatesError(t *testing.T) {
	sentinel := errors.New("kusto down")
	q := &fakeQuerier{err: sentinel}
	_, err := Board(context.Background(), q, Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestBuildKQLFiltersAndWindow(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	_, _ = Board(context.Background(), q, Options{
		Cluster:  "prod-eastus",
		Instance: "node-7",
	})
	kql := q.lastKQL

	for _, want := range []string{
		"NodeCpuSecondsTotal",
		"tostring(Labels.mode) == 'idle'",
		"NodeMemoryMemTotalBytes",
		"NodeMemoryMemAvailableBytes",
		"ago(900s)", // DefaultWindow = 15m
		"Cluster == @'prod-eastus'",
		"Host == @'node-7'",
		"instance = Host",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("KQL missing %q:\n%s", want, kql)
		}
	}
}

func TestBuildKQLNoFilters(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	_, _ = Board(context.Background(), q, Options{})
	kql := q.lastKQL
	// With no filters, no Cluster/Host equality clauses appear.
	if strings.Contains(kql, "Cluster ==") || strings.Contains(kql, "Host ==") {
		t.Fatalf("unfiltered KQL should have no equality filters:\n%s", kql)
	}
}

// TestBuildKQLQuotesInjection verifies a filter value with an embedded quote is
// escaped, not able to break out of the KQL string literal.
func TestBuildKQLQuotesInjection(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	_, _ = Board(context.Background(), q, Options{Instance: "node' | project"})
	if !strings.Contains(q.lastKQL, "Host == @'node'' | project'") {
		t.Fatalf("injection not escaped:\n%s", q.lastKQL)
	}
}

// The window now bounds observations; CPU denominators use observed intervals.
func TestWindowOverrideChangesDenominator(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	snap, err := Board(context.Background(), q, Options{Window: 5 * 60 * 1e9}) // 5m in ns
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if !strings.Contains(q.lastKQL, "ago(300s)") || snap.Window != "5m0s" {
		t.Fatalf("5m window not applied: %q:\n%s", snap.Window, q.lastKQL)
	}
}
