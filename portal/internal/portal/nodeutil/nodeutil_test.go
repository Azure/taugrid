// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeutil

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

type fakeQuerier struct {
	rows    []kustoquery.Row
	err     error
	lastKQL string
}

func (f *fakeQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	f.lastKQL = kql
	return f.rows, f.err
}

func sample(seconds, value float64) counterSample {
	return counterSample{Timestamp: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds * float64(time.Second))), Value: &value}
}

func cpuRow(instance, cpu string, samples ...counterSample) kustoquery.Row {
	return kustoquery.Row{"Cluster": "a", "instance": instance, "kind": "cpu", "cpu": cpu, "samples": samples, "sampleCount": float64(len(samples))}
}

func TestCPURate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		samples []counterSample
		want    *float64
		seconds float64
		resets  int
	}{
		{"sparse idle", []counterSample{sample(0, 100), sample(60, 160)}, new(0.0), 60, 0},
		{"single sample", []counterSample{sample(0, 100)}, nil, 0, 0},
		{"empty", nil, nil, 0, 0},
		{"busy zero idle delta", []counterSample{sample(0, 100), sample(60, 100)}, new(100.0), 60, 0},
		{"fractional observed seconds", []counterSample{sample(0, 100), sample(0.5, 100.25)}, new(50.0), 0.5, 0},
		{"reset only", []counterSample{sample(0, 100), sample(60, 10)}, nil, 0, 1},
		{"reset with post reset idle", []counterSample{sample(0, 100), sample(60, 10), sample(120, 70)}, new(0.0), 60, 1},
		{"reset preserves both valid spans", []counterSample{sample(0, 100), sample(60, 130), sample(120, 0), sample(180, 30)}, new(50.0), 120, 1},
		{"unsorted uneven sampling", []counterSample{sample(60, 130), sample(0, 100), sample(10, 105)}, new(50.0), 60, 0},
		{"duplicate timestamp", []counterSample{sample(0, 100), sample(0, 100)}, nil, 0, 0},
		{"duplicate plus idle", []counterSample{sample(0, 100), sample(0, 100), sample(60, 160)}, new(0.0), 60, 0},
		{"conflicting duplicate", []counterSample{sample(0, 100), sample(0, 101), sample(60, 160)}, nil, 0, 0},
		{"impossible counter rate", []counterSample{sample(0, 0), sample(10, 100)}, nil, 0, 0},
		{"negative counter", []counterSample{sample(0, -1), sample(10, 1)}, nil, 0, 0},
		{"nonfinite counter", []counterSample{sample(0, math.Inf(1)), sample(10, 1)}, nil, 0, 0},
		{"missing counter", []counterSample{{Timestamp: sample(0, 0).Timestamp}, sample(10, 1)}, nil, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, coverage := cpuRate(tt.samples)
			assertNumber(t, got, tt.want)
			if coverage.ObservedSeconds != tt.seconds || coverage.CounterResets != tt.resets {
				t.Fatalf("coverage = %+v, want %gs and %d resets", coverage, tt.seconds, tt.resets)
			}
		})
	}
}

func TestBoardAveragesCoresNotSamplingDurations(t *testing.T) {
	q := &fakeQuerier{rows: []kustoquery.Row{
		cpuRow("node", "0", sample(0, 0), sample(60, 60)),
		cpuRow("node", "1", sample(0, 0), sample(300, 0)),
		cpuRow("node", "2", sample(100, 10)),
	}}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := snap.Nodes[0]
	assertNumber(t, n.CPUUtilPct, new(50.0))
	if n.CPUCores != 3 || n.CPUCoverage.UsableCores != 2 || n.CPUCoverage.Samples != 5 || n.CPUCoverage.ObservedSeconds != 120 {
		t.Fatalf("node = %+v", n)
	}
	if math.Abs(n.CPUCoverage.WindowCoveragePct-100*120.0/900) > 1e-9 {
		t.Fatalf("coverage = %+v", n.CPUCoverage)
	}
	if !n.CPUCoverage.FirstSampleAt.Equal(sample(0, 0).Timestamp) || !n.CPUCoverage.LastSampleAt.Equal(sample(300, 0).Timestamp) {
		t.Fatalf("timestamps = %+v", n.CPUCoverage)
	}
	assertNumber(t, n.MemUsedPct, nil)
}

func TestBoardMemoryAbsenceAndZero(t *testing.T) {
	for _, tt := range []struct {
		name             string
		total, available *float64
		want             *float64
	}{
		{"missing both", nil, nil, nil},
		{"missing total", nil, new(0.0), nil},
		{"missing available", new(100.0), nil, nil},
		{"zero total", new(0.0), new(0.0), nil},
		{"all available", new(100.0), new(100.0), new(0.0)},
		{"none available", new(100.0), new(0.0), new(100.0)},
		{"partial use", new(100.0), new(75.0), new(25.0)},
		{"available exceeds total", new(100.0), new(101.0), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := []kustoquery.Row{cpuRow("node", "0", sample(0, 0))}
			for kind, value := range map[string]*float64{"memory_total": tt.total, "memory_available": tt.available} {
				if value != nil {
					rows = append(rows, kustoquery.Row{"Cluster": "a", "instance": "node", "kind": kind, "memoryValue": *value, "memoryTimestamp": "2026-09-01T00:01:00Z"})
				}
			}
			snap, err := Board(context.Background(), &fakeQuerier{rows: rows}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			n := snap.Nodes[0]
			assertNumber(t, n.CPUUtilPct, nil)
			assertNumber(t, n.MemUsedPct, tt.want)
			assertNumber(t, n.MemTotalBytes, tt.total)
			assertNumber(t, n.MemAvailBytes, tt.available)
			raw, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"cpuUtilPct":null`) {
				t.Fatalf("unknown CPU lost: %s", raw)
			}
		})
	}
}

func TestBoardMemoryOnlyAndSort(t *testing.T) {
	q := &fakeQuerier{rows: []kustoquery.Row{
		{"Cluster": "a", "instance": "memory-only", "kind": "memory_total", "memoryValue": "100", "memoryTimestamp": "2026-09-01T00:01:00Z"},
		cpuRow("idle", "0", sample(0, 0), sample(60, 60)),
		cpuRow("busy", "0", sample(0, 0), sample(60, 0)),
	}}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 3 || snap.Nodes[0].Instance != "busy" || snap.Nodes[1].Instance != "idle" || snap.Nodes[2].Instance != "memory-only" {
		t.Fatalf("ordering = %+v", snap.Nodes)
	}
	if snap.Availability != "ready" || snap.QueriedAt.IsZero() {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestBoardEmptyAndQueryError(t *testing.T) {
	snap, err := Board(context.Background(), &fakeQuerier{}, Options{})
	if err != nil || snap.Nodes == nil || len(snap.Nodes) != 0 || snap.Availability != "empty" {
		t.Fatalf("empty = %+v, %v", snap, err)
	}
	sentinel := errors.New("kusto down")
	_, err = Board(context.Background(), &fakeQuerier{err: sentinel}, Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
}

func TestBoardRejectsIncompleteSeries(t *testing.T) {
	for _, row := range []kustoquery.Row{
		{"kind": "overflow"},
		{"kind": "cpu", "instance": "node", "cpu": "0", "samples": `[]`, "sampleCount": 10.0},
		{"kind": "cpu", "instance": "node", "cpu": "0", "samples": `garbage`, "sampleCount": 1.0},
	} {
		if _, err := Board(context.Background(), &fakeQuerier{rows: []kustoquery.Row{row}}, Options{}); err == nil {
			t.Fatalf("row %+v did not fail", row)
		}
	}
}

func TestKustoDynamicSampleEncodings(t *testing.T) {
	for _, encoded := range []string{
		`[{"timestamp":"2026-09-01T00:00:00Z","value":100},{"timestamp":"2026-09-01T00:01:00Z","value":160}]`,
		`"[{\"timestamp\":\"2026-09-01T00:00:00Z\",\"value\":100},{\"timestamp\":\"2026-09-01T00:01:00Z\",\"value\":160}]"`,
	} {
		rows, err := kustoquery.ParseRows([]byte(`[{"kind":"cpu","instance":"node","cpu":"0","sampleCount":2,"samples":` + encoded + `}]`))
		if err != nil {
			t.Fatal(err)
		}
		snap, err := Board(context.Background(), &fakeQuerier{rows: rows}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		assertNumber(t, snap.Nodes[0].CPUUtilPct, new(0.0))
	}
}

func TestBuildKQL(t *testing.T) {
	kql := buildKQL(Options{Cluster: "prod", Instance: "node' | project"})
	for _, want := range []string{
		"NodeCpuSecondsTotal", "tostring(Labels.mode) == 'idle'",
		"NodeMemoryMemTotalBytes", "NodeMemoryMemAvailableBytes", "ago(900s)",
		"Cluster == @'prod'", "Host == @'node'' | project'",
		"bag_pack('timestamp', Timestamp, 'value', todouble(Value)), 4096",
		"sampleCount = count()", "cpuSampleCount <= 250000",
		"cpuSampleCount > 250000", "union cpu, memTotal, memAvail, overflow",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("missing %q:\n%s", want, kql)
		}
	}
	if strings.Contains(kql, "min(Value)") || strings.Contains(kql, "max(Value)") || strings.Contains(kql, "cpuCores *") {
		t.Fatalf("obsolete counter aggregation remains:\n%s", kql)
	}
	for _, tt := range []struct {
		window time.Duration
		want   string
	}{
		{0, "900s"}, {-time.Second, "900s"}, {5 * time.Minute, "300s"}, {time.Nanosecond, "1s"},
	} {
		if kql := buildKQL(Options{Window: tt.window}); !strings.Contains(kql, "ago("+tt.want+")") || strings.Contains(kql, "Cluster ==") || strings.Contains(kql, "Host ==") {
			t.Fatalf("window/filter mismatch: %s", kql)
		}
	}
}

func TestBuildKQLQuotesReservedKindColumn(t *testing.T) {
	kql := buildKQL(Options{})
	for _, kind := range []string{"cpu", "memory_total", "memory_available", "overflow"} {
		if want := "['kind'] = '" + kind + "'"; !strings.Contains(kql, want) {
			t.Errorf("missing escaped row discriminator %q", want)
		}
	}
	if strings.Contains(kql, " kind = ") {
		t.Fatal("unescaped kind column is rejected by the Kusto parser")
	}
}

func assertNumber(t *testing.T, got, want *float64) {
	t.Helper()
	if got == nil || want == nil {
		if (got == nil) != (want == nil) {
			t.Fatalf("got %v want %v", got, want)
		}
		return
	}
	if math.Abs(*got-*want) > 1e-9 {
		t.Fatalf("got %g want %g", *got, *want)
	}
}
