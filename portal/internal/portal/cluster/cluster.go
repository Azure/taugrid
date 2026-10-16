// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

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

type Options struct {
	Window    time.Duration
	Cluster   string
	Namespace string
	Instance  string
	Model     string
}

type GPU struct {
	Cluster                   string   `json:"cluster,omitempty"`
	Instance                  string   `json:"instance"`
	GPU                       string   `json:"gpu"`
	ModelName                 string   `json:"modelName,omitempty"`
	Namespace                 string   `json:"namespace,omitempty"`
	Pod                       string   `json:"pod,omitempty"`
	UtilizationPct            *float64 `json:"utilizationPct"`
	TemperatureCelsius        *float64 `json:"temperatureCelsius"`
	PowerWatts                *float64 `json:"powerWatts"`
	MemoryUsedMB              *float64 `json:"memoryUsedMB"`
	MemoryFreeMB              *float64 `json:"memoryFreeMB"`
	CorrectableRemappedRows   *float64 `json:"correctableRemappedRows"`
	UncorrectableRemappedRows *float64 `json:"uncorrectableRemappedRows"`
	RowRemapFailure           *float64 `json:"rowRemapFailure"`
	Healthy                   *bool    `json:"healthy"`
}

type ModelCount struct {
	ModelName string `json:"modelName"`
	GPUs      int    `json:"gpus"`
}

type Snapshot struct {
	Window                  string       `json:"window"`
	TotalGPUs               int          `json:"totalGPUs"`
	ErrorGPUs               int          `json:"errorGPUs"`
	TelemetryAvailable      bool         `json:"telemetryAvailable"`
	UtilizationObservedGPUs int          `json:"utilizationObservedGPUs"`
	HealthObservedGPUs      int          `json:"healthObservedGPUs"`
	UnknownHealthGPUs       int          `json:"unknownHealthGPUs"`
	Models                  []ModelCount `json:"models"`
	GPUs                    []GPU        `json:"gpus"`
}

func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	rows, err := q.Query(ctx, buildKQL(opts))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query gpu health: %w", err)
	}
	return aggregate(rows, opts), nil
}

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
	b.WriteString("let samples = materialize(\nGpuHealth()\n")
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
	b.WriteString("| project Timestamp, Cluster, instance, gpu, metric, Value, namespace, pod, modelName\n")
	b.WriteString(");\n")
	b.WriteString("let latest_attribution = samples\n")
	b.WriteString("| summarize arg_max(Timestamp, namespace, pod, modelName) by Cluster, instance, gpu\n")
	b.WriteString("| project Cluster, instance, gpu, namespace, pod, modelName;\n")
	b.WriteString("samples\n")
	b.WriteString("| summarize arg_max(Timestamp, Value) by Cluster, instance, gpu, metric\n")
	b.WriteString("| evaluate pivot(metric, take_any(Value), Cluster, instance, gpu)\n")
	b.WriteString("| join kind=leftouter latest_attribution on Cluster, instance, gpu\n")
	b.WriteString("| order by Cluster asc, instance asc, gpu asc")
	return b.String()
}

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
		if opts.Cluster != "" && row.Str("Cluster") != opts.Cluster {
			continue
		}
		if opts.Namespace != "" && row.Str("namespace") != opts.Namespace {
			continue
		}
		gpu := parseGPU(row)
		snap.GPUs = append(snap.GPUs, gpu)
		snap.TotalGPUs++
		if gpu.UtilizationPct != nil {
			snap.UtilizationObservedGPUs++
		}
		if gpu.Healthy == nil {
			snap.UnknownHealthGPUs++
		} else {
			snap.HealthObservedGPUs++
			if !*gpu.Healthy {
				snap.ErrorGPUs++
			}
		}
		for _, value := range []*float64{
			gpu.UtilizationPct, gpu.TemperatureCelsius, gpu.PowerWatts,
			gpu.MemoryUsedMB, gpu.MemoryFreeMB, gpu.CorrectableRemappedRows,
			gpu.UncorrectableRemappedRows, gpu.RowRemapFailure,
		} {
			if value != nil {
				snap.TelemetryAvailable = true
				break
			}
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

func parseGPU(row kustoquery.Row) GPU {
	num := func(col string) *float64 {
		v, ok := row.Num(col)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil
		}
		return &v
	}
	uncorrectable := num("uncorrectable_remapped_rows")
	failure := num("row_remap_failure")
	var healthy *bool
	if (uncorrectable != nil && *uncorrectable > 0) || (failure != nil && *failure > 0) {
		value := false
		healthy = &value
	} else if uncorrectable != nil && failure != nil {
		value := true
		healthy = &value
	}
	util := num("gpu_utilization")
	if util != nil && *util > 100 {
		util = nil
	}
	return GPU{
		Cluster:                   row.Str("Cluster"),
		Instance:                  row.Str("instance"),
		GPU:                       row.Str("gpu"),
		ModelName:                 row.Str("modelName"),
		Namespace:                 row.Str("namespace"),
		Pod:                       row.Str("pod"),
		UtilizationPct:            util,
		TemperatureCelsius:        num("gpu_temperature_celsius"),
		PowerWatts:                num("gpu_power_watts"),
		MemoryUsedMB:              num("fb_memory_used_mb"),
		MemoryFreeMB:              num("fb_memory_free_mb"),
		CorrectableRemappedRows:   num("correctable_remapped_rows"),
		UncorrectableRemappedRows: uncorrectable,
		RowRemapFailure:           failure,
		Healthy:                   healthy,
	}
}
