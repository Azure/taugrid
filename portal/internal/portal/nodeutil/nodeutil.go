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
// CPU utilization averages per-core idle rates over observed intervals. Reset
// intervals are excluded rather than mistaken for busy time. KQL packs the
// timestamped samples by core; Go handles ordering, resets, and coverage.
// Memory is (total - available) / total from the latest observed samples.
//
// Data access is the shell-out kustoquery.Querier seam, so tests inject a fake
// with canned Kusto JSON and no live ADX. The tables live in the Metrics
// database (expkusto.DefaultEndpoint/DefaultDatabase), the same target the
// portal's --kusto-* flags already point at.
package nodeutil

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

// DefaultWindow is the look-back for the CPU counter delta and the latest
// memory sample, matching the cluster board's ago(15m).
const DefaultWindow = 15 * time.Minute

// Bound transferred CPU data even for a caller-supplied long window. Overflow
// fails the board explicitly; an incomplete series must never look like a rate.
const maxCPUSamples = 250000
const maxSamplesPerCore = 4096

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

// Node preserves unknown readings as null rather than a measured zero.
type Node struct {
	Cluster          string      `json:"cluster,omitempty"`
	Instance         string      `json:"instance"`
	CPUUtilPct       *float64    `json:"cpuUtilPct"`
	CPUCores         float64     `json:"cpuCores"`
	CPUCoverage      CPUCoverage `json:"cpuCoverage"`
	MemTotalBytes    *float64    `json:"memTotalBytes"`
	MemAvailBytes    *float64    `json:"memAvailBytes"`
	MemUsedPct       *float64    `json:"memUsedPct"`
	MemTotalSampleAt *time.Time  `json:"memTotalSampleAt,omitempty"`
	MemAvailSampleAt *time.Time  `json:"memAvailSampleAt,omitempty"`
}

// CPUCoverage describes observed cores, not the node's provisioned inventory.
// ObservedSeconds is the mean usable interval duration across observed cores.
type CPUCoverage struct {
	Samples           int        `json:"samples"`
	ObservedCores     int        `json:"observedCores"`
	UsableCores       int        `json:"usableCores"`
	ObservedSeconds   float64    `json:"observedSeconds"`
	WindowCoveragePct float64    `json:"windowCoveragePct"`
	CounterResets     int        `json:"counterResets"`
	FirstSampleAt     *time.Time `json:"firstSampleAt,omitempty"`
	LastSampleAt      *time.Time `json:"lastSampleAt,omitempty"`
}

// Snapshot is the node-utilization board payload: known CPU rates sort hottest
// first, followed by nodes whose CPU rate is unknown.
type Snapshot struct {
	Window       string    `json:"window"`
	QueriedAt    time.Time `json:"queriedAt"`
	Availability string    `json:"availability"`
	Nodes        []Node    `json:"nodes"`
}

// Board runs the node-exporter CPU/memory query via the Querier and aggregates
// the rows into a Snapshot.
func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	rows, err := q.Query(ctx, buildKQL(opts))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query node utilization: %w", err)
	}
	return aggregate(rows, opts)
}

func queryWindow(opts Options) time.Duration {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	return time.Duration(max(int64(window/time.Second), 1)) * time.Second
}

// buildKQL keeps CPU samples paired with their timestamps. make_list order is
// deliberately not assumed; sampleCount detects truncation at Kusto's list cap.
// Unioning latest memory readings retains nodes with no CPU observations.
func buildKQL(opts Options) string {
	seconds := int64(queryWindow(opts) / time.Second)

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
	b.WriteString("let cpuSamples = NodeCpuSecondsTotal\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds) and Timestamp <= now() and tostring(Labels.mode) == 'idle'\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | extend cpu = tostring(Labels.cpu);\n")
	b.WriteString("let cpuSampleCount = toscalar(cpuSamples | count);\n")
	fmt.Fprintf(&b, "let cpu = cpuSamples | where cpuSampleCount <= %d\n", maxCPUSamples)
	fmt.Fprintf(&b, "  | summarize samples = make_list(bag_pack('timestamp', Timestamp, 'value', todouble(Value)), %d), sampleCount = count() by Cluster, Host, cpu\n", maxSamplesPerCore)
	b.WriteString("  | project Cluster, instance = Host, kind = 'cpu', cpu, samples, sampleCount;\n")
	b.WriteString("let memTotal = NodeMemoryMemTotalBytes\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds) and Timestamp <= now()\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | summarize arg_max(Timestamp, Value) by Cluster, Host\n")
	b.WriteString("  | project Cluster, instance = Host, kind = 'memory_total', memoryValue = todouble(Value), memoryTimestamp = Timestamp;\n")
	b.WriteString("let memAvail = NodeMemoryMemAvailableBytes\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds) and Timestamp <= now()\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | summarize arg_max(Timestamp, Value) by Cluster, Host\n")
	b.WriteString("  | project Cluster, instance = Host, kind = 'memory_available', memoryValue = todouble(Value), memoryTimestamp = Timestamp;\n")
	fmt.Fprintf(&b, "let overflow = print kind = 'overflow' | where cpuSampleCount > %d;\n", maxCPUSamples)
	b.WriteString("union cpu, memTotal, memAvail, overflow")
	return b.String()
}

func aggregate(rows []kustoquery.Row, opts Options) (Snapshot, error) {
	window := queryWindow(opts)
	snap := Snapshot{Window: window.String(), QueriedAt: time.Now().UTC(), Availability: "empty", Nodes: make([]Node, 0)}
	type identity struct{ cluster, instance string }
	type nodeSamples struct {
		node    Node
		utilSum float64
	}
	nodes := map[identity]*nodeSamples{}
	for _, row := range rows {
		if row.Str("kind") == "overflow" {
			return Snapshot{}, fmt.Errorf("node CPU sample limit exceeded; retry with a shorter window or one instance")
		}
		key := identity{row.Str("Cluster"), row.Str("instance")}
		if key.instance == "" {
			return Snapshot{}, fmt.Errorf("node utilization row has no instance")
		}
		entry := nodes[key]
		if entry == nil {
			entry = &nodeSamples{node: Node{Cluster: key.cluster, Instance: key.instance}}
			nodes[key] = entry
		}
		n := &entry.node
		switch row.Str("kind") {
		case "cpu":
			if row.Str("cpu") == "" {
				return Snapshot{}, fmt.Errorf("node CPU row has no core identity")
			}
			samples, err := decodeSamples(row)
			if err != nil {
				return Snapshot{}, fmt.Errorf("decode node CPU samples: %w", err)
			}
			rate, coverage := cpuRate(samples)
			n.CPUCores++
			n.CPUCoverage.ObservedCores++
			n.CPUCoverage.Samples += coverage.Samples
			n.CPUCoverage.CounterResets += coverage.CounterResets
			n.CPUCoverage.ObservedSeconds += coverage.ObservedSeconds
			n.CPUCoverage.FirstSampleAt = earliest(n.CPUCoverage.FirstSampleAt, coverage.FirstSampleAt)
			n.CPUCoverage.LastSampleAt = latest(n.CPUCoverage.LastSampleAt, coverage.LastSampleAt)
			if rate != nil {
				n.CPUCoverage.UsableCores++
				entry.utilSum += *rate
			}
		case "memory_total", "memory_available":
			value, valid := row.Num("memoryValue")
			at, err := time.Parse(time.RFC3339Nano, row.Str("memoryTimestamp"))
			if err != nil || !valid || !finite(value) || value < 0 {
				continue
			}
			if row.Str("kind") == "memory_total" {
				n.MemTotalBytes, n.MemTotalSampleAt = &value, &at
			} else {
				n.MemAvailBytes, n.MemAvailSampleAt = &value, &at
			}
		default:
			return Snapshot{}, fmt.Errorf("unknown node utilization row kind")
		}
	}
	for _, entry := range nodes {
		n := entry.node
		if n.CPUCoverage.UsableCores > 0 {
			util := entry.utilSum / float64(n.CPUCoverage.UsableCores)
			n.CPUUtilPct = &util
		}
		if n.CPUCoverage.ObservedCores > 0 {
			n.CPUCoverage.ObservedSeconds /= float64(n.CPUCoverage.ObservedCores)
			n.CPUCoverage.WindowCoveragePct = min(100, n.CPUCoverage.ObservedSeconds/window.Seconds()*100)
		}
		if n.MemTotalBytes != nil && n.MemAvailBytes != nil && *n.MemTotalBytes > 0 && *n.MemAvailBytes <= *n.MemTotalBytes {
			used := (1 - *n.MemAvailBytes / *n.MemTotalBytes) * 100
			n.MemUsedPct = &used
		}
		snap.Nodes = append(snap.Nodes, n)
	}
	if len(snap.Nodes) > 0 {
		snap.Availability = "ready"
	}
	sort.SliceStable(snap.Nodes, func(i, j int) bool {
		a, b := snap.Nodes[i], snap.Nodes[j]
		if a.CPUUtilPct == nil || b.CPUUtilPct == nil {
			if (a.CPUUtilPct == nil) != (b.CPUUtilPct == nil) {
				return a.CPUUtilPct != nil
			}
		} else if *a.CPUUtilPct != *b.CPUUtilPct {
			return *a.CPUUtilPct > *b.CPUUtilPct
		}
		if a.Cluster != b.Cluster {
			return a.Cluster < b.Cluster
		}
		return a.Instance < b.Instance
	})
	return snap, nil
}

type counterSample struct {
	Timestamp time.Time `json:"timestamp"`
	Value     *float64  `json:"value"`
}

func decodeSamples(row kustoquery.Row) ([]counterSample, error) {
	var raw []byte
	if text, ok := row["samples"].(string); ok {
		raw = []byte(text)
	} else {
		var err error
		raw, err = json.Marshal(row["samples"])
		if err != nil {
			return nil, err
		}
	}
	var samples []counterSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return nil, err
	}
	count, ok := row.Num("sampleCount")
	if !ok || count != float64(len(samples)) {
		return nil, fmt.Errorf("CPU sample list is missing or truncated; retry with a shorter window")
	}
	return samples, nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// cpuRate excludes resets because their occurrence time is unknown. Identical
// duplicate samples are collapsed; conflicting duplicates invalidate that
// timestamp. Rates are duration-weighted within a core, never between cores.
func cpuRate(samples []counterSample) (*float64, CPUCoverage) {
	sort.Slice(samples, func(i, j int) bool { return samples[i].Timestamp.Before(samples[j].Timestamp) })
	unique := make([]counterSample, 0, len(samples))
	for _, sample := range samples {
		if len(unique) > 0 && sample.Timestamp.Equal(unique[len(unique)-1].Timestamp) {
			previous := &unique[len(unique)-1]
			if previous.Value == nil || sample.Value == nil || *previous.Value != *sample.Value {
				previous.Value = nil
			}
			continue
		}
		unique = append(unique, sample)
	}
	coverage := CPUCoverage{}
	idle := 0.0
	for i, sample := range unique {
		if sample.Timestamp.IsZero() || sample.Value == nil || !finite(*sample.Value) || *sample.Value < 0 {
			continue
		}
		coverage.Samples++
		coverage.FirstSampleAt = earliest(coverage.FirstSampleAt, &sample.Timestamp)
		coverage.LastSampleAt = latest(coverage.LastSampleAt, &sample.Timestamp)
		if i == 0 {
			continue
		}
		previous := unique[i-1]
		if previous.Timestamp.IsZero() || previous.Value == nil || !finite(*previous.Value) || *previous.Value < 0 {
			continue
		}
		delta := *sample.Value - *previous.Value
		seconds := sample.Timestamp.Sub(previous.Timestamp).Seconds()
		if delta < 0 {
			coverage.CounterResets++
			continue
		}
		if seconds <= 0 || delta > seconds {
			continue
		}
		idle += delta
		coverage.ObservedSeconds += seconds
	}
	if coverage.ObservedSeconds == 0 {
		return nil, coverage
	}
	util := 100 * (1 - idle/coverage.ObservedSeconds)
	return &util, coverage
}

func earliest(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.Before(*a)) {
		return b
	}
	return a
}

func latest(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.After(*a)) {
		return b
	}
	return a
}
