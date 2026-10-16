// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeutil

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

const DefaultWindow = 15 * time.Minute

const maxCPUSamples = 250000

type Options struct {
	Window   time.Duration
	Cluster  string
	Instance string
}

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

type Snapshot struct {
	Window       string    `json:"window"`
	QueriedAt    time.Time `json:"queriedAt"`
	Availability string    `json:"availability"`
	Nodes        []Node    `json:"nodes"`
}

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

func buildKQL(opts Options) string {
	seconds := int64(queryWindow(opts) / time.Second)

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
	b.WriteString("  | project Cluster, Host, cpu = tostring(Labels.cpu), Timestamp, value = todouble(Value);\n")
	b.WriteString("let cpuSampleCount = toscalar(cpuSamples | count);\n")
	fmt.Fprintf(&b, "let cpuPoints = cpuSamples | where cpuSampleCount <= %d\n", maxCPUSamples)
	b.WriteString("  | summarize minValue = min(value), maxValue = max(value) by Cluster, Host, cpu, Timestamp\n")
	b.WriteString("  | extend valid = isnotnull(minValue) and minValue == maxValue and minValue >= 0.0\n")
	b.WriteString("  | order by Cluster asc, Host asc, cpu asc, Timestamp asc\n")
	b.WriteString("  | serialize\n")
	b.WriteString("  | extend sameCore = Cluster == prev(Cluster) and Host == prev(Host) and cpu == prev(cpu),\n")
	b.WriteString("      previousTimestamp = prev(Timestamp), previousValue = prev(minValue), previousValid = prev(valid)\n")
	b.WriteString("  | extend seconds = datetime_diff('millisecond', Timestamp, previousTimestamp) / 1000.0,\n")
	b.WriteString("      delta = minValue - previousValue\n")
	b.WriteString("  | extend usable = sameCore and valid and previousValid and seconds > 0.0 and delta >= 0.0 and delta <= seconds,\n")
	b.WriteString("      reset = sameCore and valid and previousValid and delta < 0.0;\n")
	b.WriteString("let cpuCores = cpuPoints\n")
	b.WriteString("  | summarize samples = countif(valid), observedSeconds = sumif(seconds, usable), idleSeconds = sumif(delta, usable),\n")
	b.WriteString("      counterResets = countif(reset), firstSampleAt = minif(Timestamp, valid), lastSampleAt = maxif(Timestamp, valid)\n")
	b.WriteString("      by Cluster, Host, cpu\n")
	b.WriteString("  | extend utilization = iff(observedSeconds > 0.0, 100.0 * (1.0 - idleSeconds / observedSeconds), real(null));\n")
	b.WriteString("let cpu = cpuCores\n")
	b.WriteString("  | summarize cpuCores = count(), samples = sum(samples), observedCores = count(),\n")
	b.WriteString("      usableCores = countif(observedSeconds > 0.0), observedSeconds = avg(observedSeconds),\n")
	b.WriteString("      counterResets = sum(counterResets), firstSampleAt = min(firstSampleAt), lastSampleAt = max(lastSampleAt),\n")
	b.WriteString("      cpuUtilPct = avgif(utilization, observedSeconds > 0.0) by Cluster, Host\n")
	b.WriteString("  | project Cluster, instance = Host, ['kind'] = 'cpu', cpuCores, cpuUtilPct, samples,\n")
	b.WriteString("      observedCores, usableCores, observedSeconds, counterResets, firstSampleAt, lastSampleAt;\n")
	b.WriteString("let memTotal = NodeMemoryMemTotalBytes\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds) and Timestamp <= now()\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | summarize arg_max(Timestamp, Value) by Cluster, Host\n")
	b.WriteString("  | project Cluster, instance = Host, ['kind'] = 'memory_total', memoryValue = todouble(Value), memoryTimestamp = Timestamp;\n")
	b.WriteString("let memAvail = NodeMemoryMemAvailableBytes\n")
	fmt.Fprintf(&b, "  | where Timestamp > ago(%ds) and Timestamp <= now()\n", seconds)
	b.WriteString(scope.String())
	b.WriteString("  | summarize arg_max(Timestamp, Value) by Cluster, Host\n")
	b.WriteString("  | project Cluster, instance = Host, ['kind'] = 'memory_available', memoryValue = todouble(Value), memoryTimestamp = Timestamp;\n")
	fmt.Fprintf(&b, "let overflow = print ['kind'] = 'overflow' | where cpuSampleCount > %d;\n", maxCPUSamples)
	b.WriteString("union cpu, memTotal, memAvail, overflow")
	return b.String()
}

func aggregate(rows []kustoquery.Row, opts Options) (Snapshot, error) {
	window := queryWindow(opts)
	snap := Snapshot{Window: window.String(), QueriedAt: time.Now().UTC(), Availability: "empty", Nodes: make([]Node, 0)}
	type identity struct{ cluster, instance string }
	type nodeSamples struct {
		node Node
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
			cpuCores, err := rowNumber(row, "cpuCores")
			if err != nil {
				return Snapshot{}, err
			}
			samples, err := rowInt(row, "samples")
			if err != nil {
				return Snapshot{}, err
			}
			observedCores, err := rowInt(row, "observedCores")
			if err != nil {
				return Snapshot{}, err
			}
			usableCores, err := rowInt(row, "usableCores")
			if err != nil {
				return Snapshot{}, err
			}
			observedSeconds, err := rowNumber(row, "observedSeconds")
			if err != nil {
				return Snapshot{}, err
			}
			counterResets, err := rowInt(row, "counterResets")
			if err != nil {
				return Snapshot{}, err
			}
			n.CPUCores = cpuCores
			n.CPUCoverage = CPUCoverage{
				Samples:           samples,
				ObservedCores:     observedCores,
				UsableCores:       usableCores,
				ObservedSeconds:   observedSeconds,
				CounterResets:     counterResets,
				FirstSampleAt:     optionalTime(row.Str("firstSampleAt")),
				LastSampleAt:      optionalTime(row.Str("lastSampleAt")),
				WindowCoveragePct: 0,
			}
			if util, ok := row.Num("cpuUtilPct"); ok && finite(util) && util >= 0 && util <= 100 {
				n.CPUUtilPct = &util
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
		if n.CPUCoverage.ObservedCores > 0 {
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

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func rowNumber(row kustoquery.Row, name string) (float64, error) {
	value, ok := row.Num(name)
	if !ok || !finite(value) || value < 0 {
		return 0, fmt.Errorf("node CPU row has invalid %s", name)
	}
	return value, nil
}

func rowInt(row kustoquery.Row, name string) (int, error) {
	value, err := rowNumber(row, name)
	if err != nil || value != math.Trunc(value) {
		return 0, fmt.Errorf("node CPU row has invalid %s", name)
	}
	return int(value), nil
}

func optionalTime(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}
