// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kustoquery"
)

// scriptedQuerier returns a different result per call so the two-query board
// (namespace chargeback, then idle GPUs) can be driven from one fake. It records
// each KQL it received in order.
type scriptedQuerier struct {
	results [][]kustoquery.Row
	err     error
	kqls    []string
	calls   int
}

func (s *scriptedQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	s.kqls = append(s.kqls, kql)
	if s.err != nil {
		return nil, s.err
	}
	i := s.calls
	s.calls++
	if i < len(s.results) {
		return s.results[i], nil
	}
	return nil, nil
}

var workspaceRows = []kustoquery.Row{
	{"workspace": "research-lab", "namespace": "research", "GpuHours": 120.5, "EstimatedCostUSD": 442.24, "PeakGpus": 8.0, "AvgUtil": 71.0},
	{"workspace": "infra-lab", "namespace": "infra", "GpuHours": 12.0, "EstimatedCostUSD": 44.04, "PeakGpus": 2.0, "AvgUtil": 15.0},
}

var idleRows = []kustoquery.Row{
	{"instance": "node-3", "gpu": "2", "modelName": "A100", "namespace": "infra", "pod": "idle-pod",
		"AvgUtil": 3.5, "Samples": 240.0},
	{"instance": "node-9", "gpu": "0", "modelName": "H100", "namespace": "", "pod": "",
		"AvgUtil": "12", "Samples": "50"}, // numeric strings (Kusto tostring())
}

func TestBoardAssemblesBothQueries(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{workspaceRows, idleRows}}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}

	if q.calls != 2 {
		t.Fatalf("querier calls = %d, want 2", q.calls)
	}
	if snap.Window != DefaultWindow.String() {
		t.Fatalf("window = %q, want %q", snap.Window, DefaultWindow.String())
	}

	// Workspaces sorted by GPU-hours desc; totals are rounded sums.
	if len(snap.Workspaces) != 2 {
		t.Fatalf("workspaces = %d, want 2", len(snap.Workspaces))
	}
	if snap.Workspaces[0].Workspace != "research-lab" || snap.Workspaces[0].Namespace != "research" ||
		snap.Workspaces[0].GPUHours != 120.5 || snap.Workspaces[0].PeakGPUs != 8 {
		t.Fatalf("workspace[0] = %#v, want research-lab 120.5h/8gpu", snap.Workspaces[0])
	}
	if snap.Workspaces[0].AvgUtilPct != 71 || snap.Workspaces[0].EstimatedCostUSD != 442.24 {
		t.Fatalf("workspace[0] metrics = %#v", snap.Workspaces[0])
	}
	if snap.TotalGPUHours != 132.5 {
		t.Fatalf("total gpu-hours = %v, want 132.5", snap.TotalGPUHours)
	}
	if snap.TotalEstimatedCostUSD != 486.28 {
		t.Fatalf("total estimated cost = %v, want 486.28", snap.TotalEstimatedCostUSD)
	}

	// Idle GPUs sorted by avg util asc; numeric-string values parse.
	if len(snap.IdleGPUs) != 2 {
		t.Fatalf("idle gpus = %d, want 2", len(snap.IdleGPUs))
	}
	if snap.IdleGPUs[0].Instance != "node-3" || snap.IdleGPUs[0].AvgUtilPct != 3.5 || snap.IdleGPUs[0].Samples != 240 {
		t.Fatalf("idle[0] = %#v, want node-3 3.5%% 240 samples", snap.IdleGPUs[0])
	}
	if snap.IdleGPUs[1].AvgUtilPct != 12 || snap.IdleGPUs[1].Samples != 50 {
		t.Fatalf("idle[1] = %#v, want 12%% 50 samples (from strings)", snap.IdleGPUs[1])
	}
}

func TestBoardEmpty(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{nil, nil}}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.TotalGPUHours != 0 || snap.TotalEstimatedCostUSD != 0 || len(snap.Workspaces) != 0 || len(snap.IdleGPUs) != 0 {
		t.Fatalf("empty snapshot = %#v", snap)
	}
	// Slices must be non-nil so they serialize as [] not null.
	if snap.Workspaces == nil || snap.IdleGPUs == nil {
		t.Fatal("empty slices are nil, want non-nil")
	}
}

func TestBoardPreservesFractionalMIGPeak(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{
		{{
			"workspace": "mig-lab", "namespace": "mig", "GpuHours": 0.1,
			"EstimatedCostUSD": 0.52, "PeakGpus": 0.14, "AvgUtil": 60.0,
		}},
		nil,
	}}
	snap, err := Board(context.Background(), q, Options{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if got := snap.Workspaces[0].PeakGPUs; got != 0.14 {
		t.Fatalf("fractional peak = %v, want 0.14", got)
	}
}

func TestBoardPropagatesError(t *testing.T) {
	sentinel := errors.New("kusto down")
	q := &scriptedQuerier{err: sentinel}
	_, err := Board(context.Background(), q, Options{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestBuildWorkspaceKQL(t *testing.T) {
	kql := buildWorkspaceKQL(DefaultWindow, "", "research", "")
	for _, want := range []string{
		"database(@'CostTracking').GpuCostHourly",
		"| where Timestamp > ago(604800s)", // 7d
		"namespace == @'research'",
		"workspace=iff(isempty(workspace), namespace, workspace)",
		"schema_version == 4",
		"ClusterGpuHours=sum(gpu_count)",
		"HourlyGpuHours=sum(ClusterGpuHours)",
		"ClusterPeakGpus=max(reported_peak_gpu_count)",
		"HourlyPeakGpus=sum(ClusterPeakGpus)",
		"GpuHours=round(sum(HourlyGpuHours), 1)",
		"EstimatedCostUSD=round(sum(HourlyCost), 2)",
		"PeakGpus=round(max(HourlyPeakGpus), 2)",
		"order by GpuHours desc",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("namespace KQL missing %q:\n%s", want, kql)
		}
	}
}

func TestBuildWorkspaceKQLNoFilter(t *testing.T) {
	kql := buildWorkspaceKQL(DefaultWindow, "", "", "")
	if strings.Contains(kql, "namespace ==") {
		t.Fatalf("unfiltered namespace KQL should have no equality filter:\n%s", kql)
	}

	if strings.Contains(kql, "Cluster ==") {
		t.Fatalf("unscoped namespace KQL should have no cluster filter:\n%s", kql)
	}
}

func TestBuildWorkspaceKQLClusterScopeExcludesUnattributableLegacyRows(t *testing.T) {
	kql := buildWorkspaceKQL(DefaultWindow, "", "", "cluster-a")
	for _, want := range []string{
		"column_ifexists('peak_gpu_count'",
		"schema_version == 4",
		"Cluster == @'cluster-a'",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("cluster-scoped KQL missing %q:\n%s", want, kql)
		}
	}
	if strings.Contains(kql, "isnull(schema_version)") {
		t.Fatalf("cluster-scoped KQL must not include unattributable legacy rows:\n%s", kql)
	}
}

func TestBuildIdleKQLThreshold(t *testing.T) {
	kql := buildIdleKQL(DefaultWindow, DefaultIdleThresholdPct, "", "")
	if !strings.Contains(kql, "where AvgUtil < 20 and Samples > 10") {
		t.Fatalf("idle KQL threshold clause wrong:\n%s", kql)
	}
	if strings.Contains(kql, "Cluster ==") {
		t.Fatalf("unscoped idle KQL should have no cluster filter:\n%s", kql)
	}
}

// TestBoardCustomThreshold verifies a non-default threshold reaches the idle KQL.
func TestBoardCustomThreshold(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{nil, nil}}
	_, err := Board(context.Background(), q, Options{IdleThresholdPct: 5})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	// kqls[1] is the idle query.
	if len(q.kqls) != 2 || !strings.Contains(q.kqls[1], "AvgUtil < 5 and") {
		t.Fatalf("custom threshold not applied: %v", q.kqls)
	}
}

// TestBuildNamespaceKQLInjection verifies a namespace filter with an embedded
// quote is escaped, not able to break out of the KQL literal.
func TestBuildNamespaceKQLInjection(t *testing.T) {
	kql := buildWorkspaceKQL(DefaultWindow, "", "ns' | project", "")
	if !strings.Contains(kql, "namespace == @'ns'' | project'") {
		t.Fatalf("injection not escaped:\n%s", kql)
	}
}

// TestBuildKQLClusterScope verifies a cluster value scopes both queries and is
// escaped as a safe KQL literal.
func TestBuildKQLClusterScope(t *testing.T) {
	nsKQL := buildWorkspaceKQL(DefaultWindow, "", "", "taugrid-flex")
	if !strings.Contains(nsKQL, "Cluster == @'taugrid-flex'") {
		t.Fatalf("namespace KQL missing cluster scope:\n%s", nsKQL)
	}
	idleKQL := buildIdleKQL(DefaultWindow, DefaultIdleThresholdPct, "", "taugrid-flex")
	if !strings.Contains(idleKQL, "Cluster == @'taugrid-flex'") {
		t.Fatalf("idle KQL missing cluster scope:\n%s", idleKQL)
	}
	// Injection through the cluster value is escaped, not able to break out.
	inj := buildWorkspaceKQL(DefaultWindow, "", "", "c' | project")
	if !strings.Contains(inj, "Cluster == @'c'' | project'") {
		t.Fatalf("cluster injection not escaped:\n%s", inj)
	}
}

func TestBuildWorkspaceKQLQuotesDatabase(t *testing.T) {
	kql := buildWorkspaceKQL(DefaultWindow, "Cost'Tracking", "", "")
	if !strings.Contains(kql, "database(@'Cost''Tracking').GpuCostHourly") {
		t.Fatalf("database is not safely quoted:\n%s", kql)
	}
}

// TestBoardThreadsCluster verifies Options.Cluster reaches both queries.
func TestBoardThreadsCluster(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{nil, nil}}
	_, err := Board(context.Background(), q, Options{Cluster: "taugrid-flex"})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(q.kqls) != 2 {
		t.Fatalf("want 2 queries, got %d", len(q.kqls))
	}
	for i, kql := range q.kqls {
		if !strings.Contains(kql, "Cluster == @'taugrid-flex'") {
			t.Fatalf("query %d missing cluster scope:\n%s", i, kql)
		}
	}
}

func TestBoardEnforcesNamespaceOnEveryQueryAndResult(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{
		{
			{"workspace": "alpha", "namespace": "team-alpha", "GpuHours": 10.0, "PeakGpus": 1.0, "AvgUtil": 50.0},
			{"workspace": "beta", "namespace": "team-beta", "GpuHours": 99.0, "PeakGpus": 8.0, "AvgUtil": 90.0},
		},
		{
			{"instance": "alpha-node", "gpu": "0", "namespace": "team-alpha", "pod": "alpha-pod", "AvgUtil": 4.0, "Samples": 20.0},
			{"instance": "beta-node", "gpu": "0", "namespace": "team-beta", "pod": "beta-pod", "AvgUtil": 1.0, "Samples": 20.0},
		},
	}}
	snap, err := Board(context.Background(), q, Options{Namespace: "team-alpha", Cluster: "cluster-a"})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(q.kqls) != 2 {
		t.Fatalf("queries = %d, want 2", len(q.kqls))
	}
	for i, kql := range q.kqls {
		for _, want := range []string{"Cluster == @'cluster-a'", "namespace == @'team-alpha'"} {
			if !strings.Contains(kql, want) {
				t.Fatalf("query %d missing %q:\n%s", i, want, kql)
			}
		}
	}
	if len(snap.Workspaces) != 1 || snap.Workspaces[0].Workspace != "alpha" {
		t.Fatalf("workspaces = %+v, want only alpha", snap.Workspaces)
	}
	if len(snap.IdleGPUs) != 1 || snap.IdleGPUs[0].Pod != "alpha-pod" {
		t.Fatalf("idle GPUs = %+v, want only alpha-pod", snap.IdleGPUs)
	}
}
