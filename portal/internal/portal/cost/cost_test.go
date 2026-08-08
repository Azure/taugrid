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

var namespaceRows = []kustoquery.Row{
	{"namespace": "research", "GpuHours": 120.5, "Gpus": 8.0, "AvgUtil": 71.0},
	{"namespace": "infra", "GpuHours": 12.0, "Gpus": 2.0, "AvgUtil": 15.0},
}

var idleRows = []kustoquery.Row{
	{"instance": "node-3", "gpu": "2", "modelName": "A100", "namespace": "infra", "pod": "idle-pod",
		"AvgUtil": 3.5, "Samples": 240.0},
	{"instance": "node-9", "gpu": "0", "modelName": "H100", "namespace": "", "pod": "",
		"AvgUtil": "12", "Samples": "50"}, // numeric strings (Kusto tostring())
}

func TestBoardAssemblesBothQueries(t *testing.T) {
	q := &scriptedQuerier{results: [][]kustoquery.Row{namespaceRows, idleRows}}
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

	// Namespaces sorted by GPU-hours desc; total is the rounded sum.
	if len(snap.Namespaces) != 2 {
		t.Fatalf("namespaces = %d, want 2", len(snap.Namespaces))
	}
	if snap.Namespaces[0].Namespace != "research" || snap.Namespaces[0].GPUHours != 120.5 || snap.Namespaces[0].GPUs != 8 {
		t.Fatalf("ns[0] = %#v, want research 120.5h/8gpu", snap.Namespaces[0])
	}
	if snap.Namespaces[0].AvgUtilPct != 71 {
		t.Fatalf("ns[0] avgUtil = %v, want 71", snap.Namespaces[0].AvgUtilPct)
	}
	if snap.TotalGPUHours != 132.5 {
		t.Fatalf("total gpu-hours = %v, want 132.5", snap.TotalGPUHours)
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
	if snap.TotalGPUHours != 0 || len(snap.Namespaces) != 0 || len(snap.IdleGPUs) != 0 {
		t.Fatalf("empty snapshot = %#v", snap)
	}
	// Slices must be non-nil so they serialize as [] not null.
	if snap.Namespaces == nil || snap.IdleGPUs == nil {
		t.Fatal("empty slices are nil, want non-nil")
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

func TestBuildNamespaceKQL(t *testing.T) {
	kql := buildNamespaceKQL(DefaultWindow, "research", "")
	for _, want := range []string{
		"GpuHealth()",
		"| where Timestamp > ago(604800s)", // 7d
		"metric == 'gpu_utilization'",
		"isnotempty(namespace)",
		"namespace == @'research'",
		"GpuHours=round(datetime_diff('second', MaxT, MinT) / 3600.0 * Gpus, 1)",
		"order by GpuHours desc",
	} {
		if !strings.Contains(kql, want) {
			t.Fatalf("namespace KQL missing %q:\n%s", want, kql)
		}
	}
}

func TestBuildNamespaceKQLNoFilter(t *testing.T) {
	kql := buildNamespaceKQL(DefaultWindow, "", "")
	if strings.Contains(kql, "namespace ==") {
		t.Fatalf("unfiltered namespace KQL should have no equality filter:\n%s", kql)
	}
	if strings.Contains(kql, "Cluster ==") {
		t.Fatalf("unscoped namespace KQL should have no cluster filter:\n%s", kql)
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
	kql := buildNamespaceKQL(DefaultWindow, "ns' | project", "")
	if !strings.Contains(kql, "namespace == @'ns'' | project'") {
		t.Fatalf("injection not escaped:\n%s", kql)
	}
}

// TestBuildKQLClusterScope verifies a cluster value scopes both queries and is
// escaped as a safe KQL literal.
func TestBuildKQLClusterScope(t *testing.T) {
	nsKQL := buildNamespaceKQL(DefaultWindow, "", "taugrid-flex")
	if !strings.Contains(nsKQL, "Cluster == @'taugrid-flex'") {
		t.Fatalf("namespace KQL missing cluster scope:\n%s", nsKQL)
	}
	idleKQL := buildIdleKQL(DefaultWindow, DefaultIdleThresholdPct, "", "taugrid-flex")
	if !strings.Contains(idleKQL, "Cluster == @'taugrid-flex'") {
		t.Fatalf("idle KQL missing cluster scope:\n%s", idleKQL)
	}
	// Injection through the cluster value is escaped, not able to break out.
	inj := buildNamespaceKQL(DefaultWindow, "", "c' | project")
	if !strings.Contains(inj, "Cluster == @'c'' | project'") {
		t.Fatalf("cluster injection not escaped:\n%s", inj)
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
			{"namespace": "team-alpha", "GpuHours": 10.0, "Gpus": 1.0, "AvgUtil": 50.0},
			{"namespace": "team-beta", "GpuHours": 99.0, "Gpus": 8.0, "AvgUtil": 90.0},
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
	if len(snap.Namespaces) != 1 || snap.Namespaces[0].Namespace != "team-alpha" {
		t.Fatalf("namespaces = %+v, want only team-alpha", snap.Namespaces)
	}
	if len(snap.IdleGPUs) != 1 || snap.IdleGPUs[0].Pod != "alpha-pod" {
		t.Fatalf("idle GPUs = %+v, want only alpha-pod", snap.IdleGPUs)
	}
}
