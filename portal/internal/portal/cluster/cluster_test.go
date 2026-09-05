// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

// pivotedRows is the shape GpuHealth() | evaluate pivot(...) produces: one row
// per GPU with each metric as a column. Values arrive as JSON numbers/strings
// exactly like the shell-out parser yields.
var pivotedRows = []kustoquery.Row{
	{
		"Cluster": "cluster-a", "instance": "node-0", "gpu": "0", "modelName": "H100",
		"namespace": "ray", "pod": "train-0",
		"gpu_utilization": 91.0, "gpu_temperature_celsius": 63.0, "gpu_power_watts": 410.0,
		"fb_memory_used_mb": 70000.0, "fb_memory_free_mb": 10000.0,
		"correctable_remapped_rows": 2.0, "uncorrectable_remapped_rows": 0.0, "row_remap_failure": 0.0,
	},
	{
		"Cluster": "cluster-a", "instance": "node-0", "gpu": "1", "modelName": "H100",
		"gpu_utilization": 5.0, "gpu_temperature_celsius": 40.0, "gpu_power_watts": 90.0,
		// This GPU has an uncorrectable remapped row → unhealthy.
		"uncorrectable_remapped_rows": 1.0, "row_remap_failure": 0.0,
	},
	{
		"Cluster": "cluster-a", "instance": "node-1", "gpu": "0", "modelName": "A100",
		"gpu_utilization":   "77", // numeric string (Kusto tostring())
		"row_remap_failure": 3.0,
	},
}

func TestBoardAggregatesPivotedRows(t *testing.T) {
	q := &fakeQuerier{rows: pivotedRows}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalGPUs != 3 {
		t.Fatalf("TotalGPUs = %d, want 3", snap.TotalGPUs)
	}
	// node-0/gpu1 (uncorrectable) and node-1/gpu0 (row_remap_failure) → 2 errors.
	if snap.ErrorGPUs != 2 {
		t.Fatalf("ErrorGPUs = %d, want 2", snap.ErrorGPUs)
	}
	if snap.Window != DefaultWindow.String() {
		t.Fatalf("Window = %q, want %q", snap.Window, DefaultWindow.String())
	}

	// Models sorted by GPU count desc: H100 (2) before A100 (1).
	if len(snap.Models) != 2 {
		t.Fatalf("models = %d, want 2: %#v", len(snap.Models), snap.Models)
	}
	if snap.Models[0].ModelName != "H100" || snap.Models[0].GPUs != 2 {
		t.Fatalf("models[0] = %#v, want H100:2", snap.Models[0])
	}
	if snap.Models[1].ModelName != "A100" || snap.Models[1].GPUs != 1 {
		t.Fatalf("models[1] = %#v, want A100:1", snap.Models[1])
	}

	// First GPU: healthy, values mapped through.
	g0 := snap.GPUs[0]
	if g0.Instance != "node-0" || g0.GPU != "0" || g0.Healthy == nil || !*g0.Healthy {
		t.Fatalf("gpu0 = %#v, want node-0/0 healthy", g0)
	}
	if g0.UtilizationPct == nil || *g0.UtilizationPct != 91 ||
		g0.TemperatureCelsius == nil || *g0.TemperatureCelsius != 63 ||
		g0.MemoryUsedMB == nil || *g0.MemoryUsedMB != 70000 {
		t.Fatalf("gpu0 metrics = %#v", g0)
	}
	if g0.CorrectableRemappedRows == nil || *g0.CorrectableRemappedRows != 2 {
		t.Fatalf("gpu0 correctable = %v, want 2", g0.CorrectableRemappedRows)
	}

	// Third GPU: numeric-string utilization parsed, unhealthy via row_remap_failure.
	g2 := snap.GPUs[2]
	if g2.UtilizationPct == nil || *g2.UtilizationPct != 77 {
		t.Fatalf("gpu2 utilization = %v, want 77 (from string)", g2.UtilizationPct)
	}
	if g2.Healthy == nil || *g2.Healthy {
		t.Fatal("gpu2 healthy must be false (row_remap_failure > 0)")
	}
	if !snap.TelemetryAvailable || snap.UtilizationObservedGPUs != 3 || snap.HealthObservedGPUs != 3 || snap.UnknownHealthGPUs != 0 {
		t.Fatalf("coverage = %+v", snap)
	}
}

func TestBoardEmpty(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalGPUs != 0 || snap.ErrorGPUs != 0 || len(snap.GPUs) != 0 {
		t.Fatalf("empty snapshot = %#v", snap)
	}
	if snap.TelemetryAvailable || snap.UtilizationObservedGPUs != 0 || snap.HealthObservedGPUs != 0 || snap.UnknownHealthGPUs != 0 {
		t.Fatalf("empty coverage = %+v", snap)
	}
	// GPUs and Models must be non-nil slices so they serialize as [] not null.
	if snap.GPUs == nil {
		t.Fatal("GPUs is nil, want empty slice")
	}
	if snap.Models == nil {
		t.Fatal("Models is nil, want empty slice")
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
		Cluster:   "prod-eastus",
		Namespace: "team-alpha",
		Instance:  "node-7",
		Model:     "H100",
	})
	kql := q.lastKQL

	for _, want := range []string{
		"GpuHealth()",
		"| where Timestamp > ago(900s)", // DefaultWindow = 15m
		"Cluster == @'prod-eastus'",
		"namespace == @'team-alpha'",
		"instance == @'node-7'",
		"modelName == @'H100'",
		"let latest_attribution = samples",
		"arg_max(Timestamp, Value) by Cluster, instance, gpu, metric",
		"arg_max(Timestamp, namespace, pod, modelName) by Cluster, instance, gpu",
		"evaluate pivot(metric",
		"join kind=leftouter latest_attribution on Cluster, instance, gpu",
		"@'uncorrectable_remapped_rows'",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("KQL missing %q:\n%s", want, kql)
		}
	}
}

func TestBuildKQLUsesPhysicalGPUIdentity(t *testing.T) {
	kql := buildKQL(Options{})
	if strings.Contains(kql, "by Cluster, instance, gpu, modelName, namespace, pod, metric") {
		t.Fatalf("KQL still treats workload attribution as GPU identity:\n%s", kql)
	}
	for _, want := range []string{
		"by Cluster, instance, gpu, metric",
		"by Cluster, instance, gpu",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("KQL missing physical GPU grouping %q:\n%s", want, kql)
		}
	}
}

func TestBuildKQLNoFilters(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	_, _ = Board(context.Background(), q, Options{})
	kql := q.lastKQL
	// With no filters, no Cluster/instance/modelName equality clauses appear.
	if strings.Contains(kql, "Cluster ==") || strings.Contains(kql, "namespace ==") ||
		strings.Contains(kql, "instance ==") || strings.Contains(kql, "modelName ==") {
		t.Fatalf("unfiltered KQL should have no equality filters:\n%s", kql)
	}
}

// TestBuildKQLQuotesInjection verifies a filter value with an embedded quote is
// escaped, not able to break out of the KQL string literal.
func TestBuildKQLQuotesInjection(t *testing.T) {
	q := &fakeQuerier{rows: nil}
	_, _ = Board(context.Background(), q, Options{Instance: "node' | project"})
	if !strings.Contains(q.lastKQL, "instance == @'node'' | project'") {
		t.Fatalf("injection not escaped:\n%s", q.lastKQL)
	}
}

func TestBoardEnforcesNamespaceInQueryAndResult(t *testing.T) {
	q := &fakeQuerier{rows: []kustoquery.Row{
		{"Cluster": "cluster-a", "instance": "alpha-node", "gpu": "0", "namespace": "team-alpha", "pod": "alpha-pod"},
		{"Cluster": "cluster-a", "instance": "beta-node", "gpu": "0", "namespace": "team-beta", "pod": "beta-pod"},
		{"Cluster": "cluster-b", "instance": "alpha-node", "gpu": "0", "namespace": "team-alpha", "pod": "other-cluster-pod"},
	}}
	snap, err := Board(context.Background(), q, Options{Cluster: "cluster-a", Namespace: "team-alpha"})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, want := range []string{"Cluster == @'cluster-a'", "namespace == @'team-alpha'"} {
		if !strings.Contains(q.lastKQL, want) {
			t.Fatalf("KQL missing %q:\n%s", want, q.lastKQL)
		}
	}
	if snap.TotalGPUs != 1 || len(snap.GPUs) != 1 || snap.GPUs[0].Pod != "alpha-pod" {
		t.Fatalf("snapshot = %+v, want only alpha-pod", snap)
	}
}

func TestParseGPUMetricAvailability(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  *float64
	}{
		{name: "null"},
		{name: "malformed", value: "bad"},
		{name: "nan", value: math.NaN()},
		{name: "infinity", value: math.Inf(1)},
		{name: "string infinity", value: "Inf"},
		{name: "negative", value: -1.0},
		{name: "above percent range", value: 101.0},
		{name: "zero", value: 0.0, want: new(float64)},
		{name: "numeric string", value: "25", want: floatPointer(25)},
		{name: "full utilization", value: 100.0, want: floatPointer(100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gpu := parseGPU(kustoquery.Row{"gpu_utilization": tt.value})
			if (gpu.UtilizationPct == nil) != (tt.want == nil) ||
				(tt.want != nil && *gpu.UtilizationPct != *tt.want) {
				t.Fatalf("utilization = %v, want %v", gpu.UtilizationPct, tt.want)
			}
			if gpu.Healthy != nil {
				t.Fatalf("healthy = %v without error counter observations", gpu.Healthy)
			}
			if _, err := json.Marshal(gpu); err != nil {
				t.Fatalf("marshal: %v", err)
			}
		})
	}
}

func TestBoardHealthAvailability(t *testing.T) {
	tests := []struct {
		name       string
		row        kustoquery.Row
		healthy    *bool
		telemetry  bool
		errorCount int
	}{
		{name: "no metrics", row: kustoquery.Row{}},
		{name: "null counters", row: kustoquery.Row{"uncorrectable_remapped_rows": nil, "row_remap_failure": nil}},
		{name: "temperature only", row: kustoquery.Row{"gpu_temperature_celsius": 50.0}, telemetry: true},
		{name: "partial zero", row: kustoquery.Row{"uncorrectable_remapped_rows": 0.0}, telemetry: true},
		{name: "both zero", row: kustoquery.Row{"uncorrectable_remapped_rows": 0.0, "row_remap_failure": "0"}, healthy: boolPointer(true), telemetry: true},
		{name: "known error missing other counter", row: kustoquery.Row{"uncorrectable_remapped_rows": 1.0}, healthy: boolPointer(false), telemetry: true, errorCount: 1},
		{name: "failure missing other counter", row: kustoquery.Row{"row_remap_failure": 1.0}, healthy: boolPointer(false), telemetry: true, errorCount: 1},
		{name: "invalid counter", row: kustoquery.Row{"uncorrectable_remapped_rows": math.NaN(), "row_remap_failure": 0.0}, telemetry: true},
		{name: "negative counter", row: kustoquery.Row{"uncorrectable_remapped_rows": -1.0, "row_remap_failure": 0.0}, telemetry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := aggregate([]kustoquery.Row{tt.row}, Options{})
			got := snap.GPUs[0].Healthy
			if (got == nil) != (tt.healthy == nil) || (tt.healthy != nil && *got != *tt.healthy) {
				t.Fatalf("healthy = %v, want %v", got, tt.healthy)
			}
			if snap.TelemetryAvailable != tt.telemetry || snap.ErrorGPUs != tt.errorCount {
				t.Fatalf("availability/error count = %+v", snap)
			}
			wantObserved := 0
			if tt.healthy != nil {
				wantObserved = 1
			}
			if snap.HealthObservedGPUs != wantObserved || snap.UnknownHealthGPUs != 1-wantObserved {
				t.Fatalf("health coverage = %+v", snap)
			}
		})
	}
}

func TestBoardJSONPreservesMissingAndZero(t *testing.T) {
	snap := aggregate([]kustoquery.Row{
		{"gpu": "0"},
		{"gpu": "1", "gpu_utilization": 0.0, "uncorrectable_remapped_rows": 0.0, "row_remap_failure": 0.0},
	}, Options{})
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"utilizationPct":null`, `"utilizationPct":0`, `"healthy":null`, `"healthy":true`,
		`"temperatureCelsius":null`, `"errorGPUs":0`, `"totalGPUs":2`,
		`"telemetryAvailable":true`, `"utilizationObservedGPUs":1`,
		`"healthObservedGPUs":1`, `"unknownHealthGPUs":1`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("JSON missing %s: %s", want, data)
		}
	}
}

func floatPointer(value float64) *float64 { return &value }

func boolPointer(value bool) *bool { return &value }
