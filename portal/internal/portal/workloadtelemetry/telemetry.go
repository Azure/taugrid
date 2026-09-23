// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadtelemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

type PodTarget struct {
	Pod      string
	Instance string
}

type Query struct {
	Cluster   string
	Namespace string
	Pods      []PodTarget
	Start     time.Time
	End       time.Time
}

type GPU struct {
	Instance                 string   `json:"instance"`
	Pod                      string   `json:"pod"`
	GPU                      string   `json:"gpu"`
	ModelName                string   `json:"modelName,omitempty"`
	Samples                  int64    `json:"samples"`
	UtilizationSamples       int64    `json:"utilizationSamples"`
	FirstSample              string   `json:"firstSample,omitempty"`
	LastSample               string   `json:"lastSample,omitempty"`
	AverageUtilizationPct    *float64 `json:"averageUtilizationPct,omitempty"`
	PeakUtilizationPct       *float64 `json:"peakUtilizationPct,omitempty"`
	MaxTemperatureCelsius    *float64 `json:"maxTemperatureCelsius,omitempty"`
	MaxPowerWatts            *float64 `json:"maxPowerWatts,omitempty"`
	MaxMemoryUsedMB          *float64 `json:"maxMemoryUsedMB,omitempty"`
	MaxCorrectableRemapped   *float64 `json:"maxCorrectableRemappedRows,omitempty"`
	MaxUncorrectableRemapped *float64 `json:"maxUncorrectableRemappedRows,omitempty"`
	MaxRowRemapFailure       *float64 `json:"maxRowRemapFailure,omitempty"`
}

type Summary struct {
	Start                  string   `json:"start"`
	End                    string   `json:"end"`
	GPUCount               int      `json:"gpuCount"`
	SampleCount            int64    `json:"sampleCount"`
	UtilizationSampleCount int64    `json:"utilizationSampleCount"`
	AverageUtilizationPct  *float64 `json:"averageUtilizationPct,omitempty"`
	Coverage               string   `json:"coverage"`
	GPUs                   []GPU    `json:"gpus"`
}

func Fetch(ctx context.Context, querier kustoquery.Querier, query Query) (Summary, error) {
	if querier == nil {
		return Summary{}, fmt.Errorf("GPU telemetry querier is not configured")
	}
	if query.Cluster == "" || query.Namespace == "" || query.Start.IsZero() || query.End.IsZero() || !query.End.After(query.Start) || len(query.Pods) == 0 {
		return Summary{}, fmt.Errorf("GPU telemetry query is incomplete")
	}
	rows, err := querier.Query(ctx, buildKQL(query))
	if err != nil {
		return Summary{}, fmt.Errorf("query workload GPU telemetry: %w", err)
	}
	return aggregate(rows, query), nil
}

func buildKQL(query Query) string {
	var targets []string
	for _, pod := range query.Pods {
		if pod.Pod == "" || pod.Instance == "" {
			continue
		}
		targets = append(targets, fmt.Sprintf("%s, %s", kustoquery.QuoteString(pod.Pod), kustoquery.QuoteString(pod.Instance)))
	}
	return fmt.Sprintf(`let targets = datatable(pod:string, instance:string)[%s];
GpuHealth()
| where Timestamp between (datetime(%s) .. datetime(%s))
| where Cluster == %s and namespace == %s
| join kind=inner targets on pod, instance
| summarize samples=count(), utilizationSamples=countif(metric == "gpu_utilization"),
    firstSample=min(Timestamp), lastSample=max(Timestamp),
    averageUtilizationPct=avgif(Value, metric == "gpu_utilization"),
    peakUtilizationPct=maxif(Value, metric == "gpu_utilization"),
    maxTemperatureCelsius=maxif(Value, metric == "gpu_temperature_celsius"),
    maxPowerWatts=maxif(Value, metric == "gpu_power_watts"),
    maxMemoryUsedMB=maxif(Value, metric == "fb_memory_used_mb"),
    maxCorrectableRemappedRows=maxif(Value, metric == "correctable_remapped_rows"),
    maxUncorrectableRemappedRows=maxif(Value, metric == "uncorrectable_remapped_rows"),
    maxRowRemapFailure=maxif(Value, metric == "row_remap_failure")
  by Cluster, instance, gpu, modelName, pod
| order by instance asc, gpu asc, pod asc`,
		strings.Join(targets, ", "),
		query.Start.UTC().Format(time.RFC3339Nano), query.End.UTC().Format(time.RFC3339Nano),
		kustoquery.QuoteString(query.Cluster), kustoquery.QuoteString(query.Namespace))
}

func aggregate(rows []kustoquery.Row, query Query) Summary {
	summary := Summary{
		Start:    query.Start.UTC().Format(time.RFC3339Nano),
		End:      query.End.UTC().Format(time.RFC3339Nano),
		Coverage: "empty",
		GPUs:     make([]GPU, 0, len(rows)),
	}
	var weightedUtilization float64
	for _, row := range rows {
		gpu := GPU{
			Instance: row.Str("instance"), Pod: row.Str("pod"), GPU: row.Str("gpu"), ModelName: row.Str("modelName"),
			Samples: int64Number(row, "samples"), UtilizationSamples: int64Number(row, "utilizationSamples"),
			FirstSample: row.Str("firstSample"), LastSample: row.Str("lastSample"),
			AverageUtilizationPct: number(row, "averageUtilizationPct"), PeakUtilizationPct: number(row, "peakUtilizationPct"),
			MaxTemperatureCelsius: number(row, "maxTemperatureCelsius"), MaxPowerWatts: number(row, "maxPowerWatts"),
			MaxMemoryUsedMB: number(row, "maxMemoryUsedMB"), MaxCorrectableRemapped: number(row, "maxCorrectableRemappedRows"),
			MaxUncorrectableRemapped: number(row, "maxUncorrectableRemappedRows"), MaxRowRemapFailure: number(row, "maxRowRemapFailure"),
		}
		summary.GPUs = append(summary.GPUs, gpu)
		summary.SampleCount += gpu.Samples
		summary.UtilizationSampleCount += gpu.UtilizationSamples
		if gpu.AverageUtilizationPct != nil {
			weightedUtilization += *gpu.AverageUtilizationPct * float64(gpu.UtilizationSamples)
		}
	}
	summary.GPUCount = len(summary.GPUs)
	if summary.UtilizationSampleCount > 0 {
		value := weightedUtilization / float64(summary.UtilizationSampleCount)
		summary.AverageUtilizationPct = &value
		summary.Coverage = "observed"
	} else if summary.SampleCount > 0 {
		summary.Coverage = "partial"
	}
	return summary
}

func number(row kustoquery.Row, key string) *float64 {
	value, ok := row.Num(key)
	if !ok {
		return nil
	}
	return &value
}

func int64Number(row kustoquery.Row, key string) int64 {
	value, _ := row.Num(key)
	return int64(value)
}
