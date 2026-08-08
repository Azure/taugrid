// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nodeutil builds the portal's node resource-utilization board.
//
// It is the CPU/memory sibling of internal/portal/cluster (which reads GPU DCGM
// health): node-exporter metrics from the Metrics ADX database, folded into one
// row per node — CPU utilization and memory-used percentage. The Utilization
// page renders it beneath the per-GPU table so operators see the whole fleet's
// resource pressure, not just GPUs.
//
// It reads the raw node-exporter tables (NodeCpuSecondsTotal,
// NodeMemoryMemTotalBytes, NodeMemoryMemAvailableBytes) rather than the
// NodeHealth() ADX function, because on the deployed adx-mon the node identity
// lives in the ingestion-level Host column, while NodeHealth() derives its
// instance from Labels.instance — a label node-exporter never emits (Labels
// carries only {cpu, mode}). Reading Host directly is what makes the per-node
// breakdown work; keying on the always-empty instance collapsed every node into
// one bogus group.
//
// CPU utilization is derived from the node_cpu_seconds idle counter, a per-core
// monotonic counter: the counter delta over the window, summed across cores,
// divided by (cores × windowSeconds) is the idle fraction, and 100 − that is
// utilization. That differencing is done in KQL (the same math as the
// node-resources-hourly adx-mon SummaryRule) so the Go aggregator stays a thin
// row→struct fold, like cluster.parseGPU. Memory is the simpler
// (total − available) / total from the latest sample.
//
// Data access is the shell-out kustoquery.Querier seam, so tests inject a fake
// with canned Kusto JSON and no live ADX. The tables live in the Metrics
// database (expkusto.DefaultEndpoint/DefaultDatabase), the same target the
// portal's --kusto-* flags already point at.
package nodeutil

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

// DefaultWindow is the look-back for the CPU counter delta and the latest
// memory sample, matching the cluster board's ago(15m).
const DefaultWindow = 15 * time.Minute

// Options controls the board query. Window defaults to DefaultWindow; the
// optional Cluster/Instance filters are interpolated as safe KQL string
// literals (kustoquery.QuoteString) so the board can scope to one cluster/node.
// Instance filters the node identity, which node-exporter carries in the
// ingestion-level Host column (Labels holds only {cpu, mode}); it is surfaced
// back to the frontend as the row's instance field.
type Options struct {
	Window   time.Duration
	Cluster  string
	Instance string
}

// Node is one node's latest resource sample. CPUUtilPct is 100 − idle over the
// window; MemUsedPct is (total − available) / total. Byte totals are kept so the
// frontend can show absolute memory alongside the percentage.
type Node struct {
	Cluster       string  `json:"cluster,omitempty"`
	Instance      string  `json:"instance"`
	CPUUtilPct    float64 `json:"cpuUtilPct"`
	CPUCores      float64 `json:"cpuCores"`
	MemTotalBytes float64 `json:"memTotalBytes"`
	MemAvailBytes float64 `json:"memAvailBytes"`
	MemUsedPct    float64 `json:"memUsedPct"`
}

// Snapshot is the node-utilization board payload: per-node rows, ordered
// hottest-CPU-first by the query.
type Snapshot struct {
	Window string `json:"window"`
	Nodes  []Node `json:"nodes"`
}

// Board runs the node-exporter CPU/memory query via the Querier and aggregates
// the rows into a Snapshot.
func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	rows, err := q.Query(ctx, buildKQL(opts))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query node utilization: %w", err)
	}
	return aggregate(rows, opts), nil
}

// buildKQL renders the per-node CPU/memory query over the raw node-exporter
// tables. The window is emitted as an integer-second ago() literal
// (injection-proof) and reused as the CPU denominator; optional Cluster/Instance
// filters use QuoteString. CPU folds the idle-mode node_cpu_seconds counter
// delta into a utilization percent; memory joins the latest total/available
// samples. Node identity is the Host column (node-exporter never sets
// Labels.instance), aliased to instance in the projection so the API shape is
// unchanged. Left outer joins anchor every row on a CPU-reporting node, zeroing
// memory for a node that had no memory sample in the window.
func buildKQL(opts Options) string {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	seconds := max(int64(window/time.Second), 1)

	// scope is the optional cluster/host filter appended to each table leg so the
	// CPU and memory sides see the same nodes. Host is node-exporter's node key;
	// Instance in Options filters it.
	var scope strings.Builder
	if opts.Cluster != "" {
		fmt.Fprintf(&scope, "  | where Cluster == %s\n", kustoquery.QuoteString(opts.Cluster))
	}
	if opts.Instance != "" {
		fmt.Fprintf(&scope, "  | where Host == %s\n", kustoquery.QuoteString(opts.Instance))
	}

	var b strings.Builder
	b.WriteString("let cpu = NodeCpuSecondsTotal\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds) and tostring(Labels.mode) == 'idle'\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | extend cpu = tostring(Labels.cpu)\n")
	b.WriteString("  | summarize mn = min(Value), mx = max(Value) by Cluster, Host, cpu\n")
	b.WriteString("  | summarize idleDelta = sum(mx - mn), cpuCores = dcount(cpu) by Cluster, Host\n")
	fmt.Fprintf(&b, "  | extend cpuUtilPct = 100.0 - iff(cpuCores > 0, idleDelta / (cpuCores * %d.0) * 100.0, 0.0);\n", seconds)
	b.WriteString("let memTotal = NodeMemoryMemTotalBytes\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds)\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | summarize arg_max(Timestamp, Value) by Cluster, Host\n")
	b.WriteString("  | project Cluster, Host, memTotalBytes = Value;\n")
	b.WriteString("let memAvail = NodeMemoryMemAvailableBytes\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds)\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | summarize arg_max(Timestamp, Value) by Cluster, Host\n")
	b.WriteString("  | project Cluster, Host, memAvailBytes = Value;\n")
	b.WriteString("cpu\n")
	b.WriteString("| join kind=leftouter memTotal on Cluster, Host\n")
	b.WriteString("| join kind=leftouter memAvail on Cluster, Host\n")
	b.WriteString("| extend memUsedPct = iff(memTotalBytes > 0, (1.0 - memAvailBytes / memTotalBytes) * 100.0, 0.0)\n")
	b.WriteString("| project Cluster, instance = Host, cpuUtilPct, cpuCores, memTotalBytes, memAvailBytes, memUsedPct\n")
	b.WriteString("| order by cpuUtilPct desc")
	return b.String()
}

// aggregate folds the per-node rows into the Snapshot, preserving the query's
// hottest-CPU-first order (a stable secondary sort by instance keeps ties
// deterministic for tests).
func aggregate(rows []kustoquery.Row, opts Options) Snapshot {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	snap := Snapshot{Window: window.String(), Nodes: make([]Node, 0, len(rows))}
	for _, row := range rows {
		snap.Nodes = append(snap.Nodes, parseNode(row))
	}
	sort.SliceStable(snap.Nodes, func(i, j int) bool {
		if snap.Nodes[i].CPUUtilPct != snap.Nodes[j].CPUUtilPct {
			return snap.Nodes[i].CPUUtilPct > snap.Nodes[j].CPUUtilPct
		}
		return snap.Nodes[i].Instance < snap.Nodes[j].Instance
	})
	return snap
}

// parseNode reads one row into a Node. The final projection names its own
// output columns (Cluster, instance, and the metric floats), so a missing
// memory column defaults to 0 (Row.Num reports ok=false) — a node that reported
// no memory sample in the window still yields a row with those fields zeroed.
func parseNode(row kustoquery.Row) Node {
	num := func(col string) float64 {
		v, _ := row.Num(col)
		return v
	}
	return Node{
		Cluster:       row.Str("Cluster"),
		Instance:      row.Str("instance"),
		CPUUtilPct:    num("cpuUtilPct"),
		CPUCores:      num("cpuCores"),
		MemTotalBytes: num("memTotalBytes"),
		MemAvailBytes: num("memAvailBytes"),
		MemUsedPct:    num("memUsedPct"),
	}
}
