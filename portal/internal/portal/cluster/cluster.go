// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cluster builds the portal's Cluster Health board.
//
// A single GpuHealth() KQL takes the latest sample per GPU for the
// health metrics, pivots metric→column, and the pure aggregator here folds the
// generic rows into a typed Snapshot (per-GPU rows + summary counts).
//
// Data access is the shell-out kustoquery.Querier seam, so tests inject a fake
// with canned Kusto JSON and no live ADX. GpuHealth() lives in the Metrics
// database (expkusto.DefaultEndpoint/DefaultDatabase), the same target the
// portal's --kusto-* flags already point at.
package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

// DefaultWindow is the look-back for "latest sample per GPU".
const DefaultWindow = 15 * time.Minute

// healthMetrics are the GpuHealth() metric rows the board pivots into columns.
// Order is irrelevant (they become column names), but the list is the contract
// with parseGPU below.
var healthMetrics = []string{
	"gpu_utilization",
	"gpu_temperature_celsius",
	"gpu_power_watts",
	"fb_memory_used_mb",
	"fb_memory_free_mb",
	"correctable_remapped_rows",
	"uncorrectable_remapped_rows",
	"row_remap_failure",
}

// Options controls the board query. Window defaults to DefaultWindow; the
// optional Cluster/Namespace/Instance/Model filters are interpolated as safe KQL
// string literals (kustoquery.QuoteString) so the board can scope to one
// workspace or node.
type Options struct {
	Window    time.Duration
	Cluster   string
	Namespace string
	Instance  string
	Model     string
}

// GPU is one GPU's latest health sample. Counter metrics (remapped rows) keep
// their raw values; Healthy is false when the GPU shows uncorrectable remapped
// rows or a row-remap failure.
type GPU struct {
	Cluster                   string  `json:"cluster,omitempty"`
	Instance                  string  `json:"instance"`
	GPU                       string  `json:"gpu"`
	ModelName                 string  `json:"modelName,omitempty"`
	Namespace                 string  `json:"namespace,omitempty"`
	Pod                       string  `json:"pod,omitempty"`
	UtilizationPct            float64 `json:"utilizationPct"`
	TemperatureCelsius        float64 `json:"temperatureCelsius"`
	PowerWatts                float64 `json:"powerWatts"`
	MemoryUsedMB              float64 `json:"memoryUsedMB"`
	MemoryFreeMB              float64 `json:"memoryFreeMB"`
	CorrectableRemappedRows   float64 `json:"correctableRemappedRows"`
	UncorrectableRemappedRows float64 `json:"uncorrectableRemappedRows"`
	RowRemapFailure           float64 `json:"rowRemapFailure"`
	Healthy                   bool    `json:"healthy"`
}

// ModelCount is the GPU count for one model, for the model-distribution summary.
type ModelCount struct {
	ModelName string `json:"modelName"`
	GPUs      int    `json:"gpus"`
}

// Snapshot is the Cluster Health board payload: per-GPU rows plus rollup counts.
type Snapshot struct {
	Window    string       `json:"window"`
	TotalGPUs int          `json:"totalGPUs"`
	ErrorGPUs int          `json:"errorGPUs"`
	Models    []ModelCount `json:"models"`
	GPUs      []GPU        `json:"gpus"`
}

// Board runs the GpuHealth() pivot via the Querier and aggregates the rows into
// a Snapshot.
func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	rows, err := q.Query(ctx, buildKQL(opts))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query gpu health: %w", err)
	}
	return aggregate(rows, opts), nil
}

// buildKQL renders the GpuHealth() latest-per-GPU pivot. The window is emitted
// as an integer-second ago() literal (injection-proof); optional filters use
// QuoteString.
func buildKQL(opts Options) string {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	seconds := max(int64(window/time.Second), 1)

	metrics := make([]string, len(healthMetrics))
	for i, m := range healthMetrics {
		metrics[i] = kustoquery.QuoteString(m)
	}

	var b strings.Builder
	b.WriteString("GpuHealth()\n")
	fmt.Fprintf(&b, "| where Timestamp > ago(%ds)\n", seconds)
	fmt.Fprintf(&b, "| where metric in (%s)\n", strings.Join(metrics, ", "))
	if opts.Cluster != "" {
		fmt.Fprintf(&b, "| where Cluster == %s\n", kustoquery.QuoteString(opts.Cluster))
	}
	if opts.Namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kustoquery.QuoteString(opts.Namespace))
	}
	if opts.Instance != "" {
		fmt.Fprintf(&b, "| where instance == %s\n", kustoquery.QuoteString(opts.Instance))
	}
	if opts.Model != "" {
		fmt.Fprintf(&b, "| where modelName == %s\n", kustoquery.QuoteString(opts.Model))
	}
	b.WriteString("| summarize arg_max(Timestamp, Value) by Cluster, instance, gpu, modelName, namespace, pod, metric\n")
	b.WriteString("| evaluate pivot(metric, take_any(Value), Cluster, instance, gpu, modelName, namespace, pod)\n")
	b.WriteString("| order by Cluster asc, instance asc, gpu asc")
	return b.String()
}

// aggregate folds pivoted rows into the Snapshot: one GPU per row, plus error
// and per-model rollups.
func aggregate(rows []kustoquery.Row, opts Options) Snapshot {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	snap := Snapshot{
		Window: window.String(),
		Models: []ModelCount{},
		GPUs:   make([]GPU, 0, len(rows)),
	}
	modelIndex := map[string]int{}
	for _, row := range rows {
		if opts.Namespace != "" && row.Str("namespace") != opts.Namespace {
			continue
		}
		gpu := parseGPU(row)
		snap.GPUs = append(snap.GPUs, gpu)
		snap.TotalGPUs++
		if !gpu.Healthy {
			snap.ErrorGPUs++
		}
		model := gpu.ModelName
		if model == "" {
			model = "unknown"
		}
		if idx, ok := modelIndex[model]; ok {
			snap.Models[idx].GPUs++
		} else {
			modelIndex[model] = len(snap.Models)
			snap.Models = append(snap.Models, ModelCount{ModelName: model, GPUs: 1})
		}
	}
	sort.Slice(snap.Models, func(i, j int) bool {
		if snap.Models[i].GPUs != snap.Models[j].GPUs {
			return snap.Models[i].GPUs > snap.Models[j].GPUs
		}
		return snap.Models[i].ModelName < snap.Models[j].ModelName
	})
	return snap
}

// parseGPU reads one pivoted row into a GPU. Missing metric columns default to
// 0 (Row.Num reports ok=false), so a GPU that reported no remapped-row counter
// is treated as zero errors, not unhealthy.
func parseGPU(row kustoquery.Row) GPU {
	num := func(col string) float64 {
		v, _ := row.Num(col)
		return v
	}
	uncorrectable := num("uncorrectable_remapped_rows")
	failure := num("row_remap_failure")
	return GPU{
		Cluster:                   row.Str("Cluster"),
		Instance:                  row.Str("instance"),
		GPU:                       row.Str("gpu"),
		ModelName:                 row.Str("modelName"),
		Namespace:                 row.Str("namespace"),
		Pod:                       row.Str("pod"),
		UtilizationPct:            num("gpu_utilization"),
		TemperatureCelsius:        num("gpu_temperature_celsius"),
		PowerWatts:                num("gpu_power_watts"),
		MemoryUsedMB:              num("fb_memory_used_mb"),
		MemoryFreeMB:              num("fb_memory_free_mb"),
		CorrectableRemappedRows:   num("correctable_remapped_rows"),
		UncorrectableRemappedRows: uncorrectable,
		RowRemapFailure:           failure,
		Healthy:                   uncorrectable == 0 && failure == 0,
	}
}
