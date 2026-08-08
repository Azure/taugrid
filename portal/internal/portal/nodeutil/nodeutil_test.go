// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeutil

import (
	"context"
	"errors"
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

// joinedRows is the shape the final projection produces: one row per node
// (keyed by instance = Host) with the CPU util, core count, and memory columns
// already computed in KQL. Values arrive as JSON numbers/strings exactly like
// the shell-out parser yields.
var joinedRows = []kustoquery.Row{
	{
		"Cluster": "cluster-a", "instance": "node-0",
		"cpuUtilPct": 82.5, "cpuCores": 64.0,
		"memTotalBytes": 200.0, "memAvailBytes": 50.0, "memUsedPct": 75.0,
	},
	{
		"Cluster": "cluster-a", "instance": "node-1",
		"cpuUtilPct": "12.5", // numeric string (Kusto tostring())
		"cpuCores":   16.0,
		// memory-only fields present too.
		"memTotalBytes": 100.0, "memAvailBytes": 90.0, "memUsedPct": 10.0,
	},
}

func TestBoardAggregatesJoinedRows(t *testing.T) {
	q := &fakeQuerier{rows: joinedRows}
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
	if n0.Instance != "node-0" || n0.CPUUtilPct != 82.5 || n0.CPUCores != 64 {
		t.Fatalf("node0 = %#v, want node-0 82.5%% / 64 cores", n0)
	}
	if n0.MemTotalBytes != 200 || n0.MemAvailBytes != 50 || n0.MemUsedPct != 75 {
		t.Fatalf("node0 memory = %#v", n0)
	}

	// Numeric-string CPU util parsed through Row.Num.
	n1 := snap.Nodes[1]
	if n1.Instance != "node-1" || n1.CPUUtilPct != 12.5 {
		t.Fatalf("node1 = %#v, want node-1 12.5%% (from string)", n1)
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
		"cpuCores * 900.0",
		"Cluster == @'prod-eastus'",
		"Host == @'node-7'",
		"join kind=leftouter memTotal on Cluster, Host",
		"instance = Host",
		"order by cpuUtilPct desc",
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

// TestWindowOverrideChangesDenominator confirms a custom window flows into both
// the ago() literal and the CPU denominator.
func TestWindowOverrideChangesDenominator(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	_, _ = Board(context.Background(), q, Options{Window: 5 * 60 * 1e9}) // 5m in ns
	kql := q.lastKQL
	if !strings.Contains(kql, "ago(300s)") || !strings.Contains(kql, "cpuCores * 300.0") {
		t.Fatalf("5m window not applied to ago()/denominator:\n%s", kql)
	}
}
