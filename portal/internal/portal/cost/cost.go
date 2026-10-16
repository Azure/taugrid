// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
)

const DefaultWindow = 7 * 24 * time.Hour

const DefaultIdleThresholdPct = 20.0

const idleMinSamples = 10

type Options struct {
	Window           time.Duration
	IdleThresholdPct float64
	CostDatabase     string
	Namespace        string
	Cluster          string
}

type WorkspaceCost struct {
	Workspace         string       `json:"workspace"`
	Namespace         string       `json:"namespace"`
	PeakGPUs          float64      `json:"peakGPUs"`
	GPUHours          float64      `json:"gpuHours"`
	EstimatedCostUSD  float64      `json:"estimatedCostUSD"`
	AvgUtilPct        *float64     `json:"avgUtilPct"`
	CostAvailable     bool         `json:"costAvailable"`
	GPUHoursAvailable bool         `json:"gpuHoursAvailable"`
	Coverage          CostCoverage `json:"coverage"`
}

type CostCoverage struct {
	ObservedSamples    int `json:"observedSamples"`
	GPUHoursSamples    int `json:"gpuHoursSamples"`
	CostSamples        int `json:"costSamples"`
	UtilizationSamples int `json:"utilizationSamples"`
}

type IdleCoverage struct {
	ObservedGPUs    int `json:"observedGPUs"`
	MeasuredGPUs    int `json:"measuredGPUs"`
	EligibleGPUs    int `json:"eligibleGPUs"`
	ObservedSamples int `json:"observedSamples"`
	ValidSamples    int `json:"validSamples"`
}

type IdleGPU struct {
	Instance   string  `json:"instance"`
	GPU        string  `json:"gpu"`
	ModelName  string  `json:"modelName,omitempty"`
	Namespace  string  `json:"namespace,omitempty"`
	Pod        string  `json:"pod,omitempty"`
	AvgUtilPct float64 `json:"avgUtilPct"`
	Samples    int     `json:"samples"`
}

type Snapshot struct {
	Window                string          `json:"window"`
	TotalGPUHours         float64         `json:"totalGPUHours"`
	TotalEstimatedCostUSD float64         `json:"totalEstimatedCostUSD"`
	CostAvailable         bool            `json:"costAvailable"`
	GPUHoursAvailable     bool            `json:"gpuHoursAvailable"`
	CostCoverage          CostCoverage    `json:"costCoverage"`
	IdleAvailable         bool            `json:"idleAvailable"`
	IdleCoverage          IdleCoverage    `json:"idleCoverage"`
	Workspaces            []WorkspaceCost `json:"workspaces"`
	IdleGPUs              []IdleGPU       `json:"idleGPUs"`
}

func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	threshold := opts.IdleThresholdPct
	if threshold <= 0 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		threshold = DefaultIdleThresholdPct
	}

	workspaceRows, err := q.Query(ctx, buildWorkspaceKQL(window, opts.CostDatabase, opts.Namespace, opts.Cluster))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query allocation cost by workspace: %w", err)
	}
	idleRows, err := q.Query(ctx, buildIdleKQL(window, opts.Namespace, opts.Cluster))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query idle gpus: %w", err)
	}
	return assemble(window, threshold, opts.Namespace, opts.Cluster, workspaceRows, idleRows), nil
}

func windowSeconds(window time.Duration) int64 {
	return max(int64(window/time.Second), 1)
}

func buildWorkspaceKQL(window time.Duration, database, namespace, cluster string) string {
	if strings.TrimSpace(database) == "" {
		database = "CostTracking"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "let CostRows = materialize(database(%s).GpuCostHourly\n", kustoquery.QuoteString(database))
	fmt.Fprintf(&b, "| where Timestamp > ago(%ds)\n", windowSeconds(window))
	if namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kustoquery.QuoteString(namespace))
	}
	b.WriteString("| extend workspace=tostring(column_ifexists('workspace', '')), reported_peak_gpu_count=toreal(column_ifexists('peak_gpu_count', real(null))), schema_version=tolong(column_ifexists('schema_version', long(null)))\n")
	b.WriteString("| extend workspace=iff(isempty(workspace), namespace, workspace)\n")
	b.WriteString("| where schema_version == 4\n")
	if cluster != "" {
		fmt.Fprintf(&b, "| where Cluster == %s\n", kustoquery.QuoteString(cluster))
	}
	b.WriteString("| where isnotempty(workspace) and isnotempty(namespace));\n")
	b.WriteString("let WorkspaceCosts = CostRows\n")
	b.WriteString("| extend gpu_count=iff(isfinite(gpu_count) and gpu_count >= 0, gpu_count, real(null)), hourly_cost=iff(isfinite(hourly_cost) and hourly_cost >= 0, hourly_cost, real(null)), reported_peak_gpu_count=iff(isfinite(reported_peak_gpu_count) and reported_peak_gpu_count >= 0, reported_peak_gpu_count, real(null))\n")
	b.WriteString("| summarize ClusterGpuHours=sum(gpu_count), ClusterCost=sum(hourly_cost), ClusterPeakGpus=max(reported_peak_gpu_count), ObservedSamples=count(), GPUHoursSamples=countif(isnotnull(gpu_count)), CostSamples=countif(isnotnull(hourly_cost)) by Timestamp, workspace, namespace, Cluster\n")
	b.WriteString("| summarize HourlyGpuHours=sum(ClusterGpuHours), HourlyCost=sum(ClusterCost), HourlyPeakGpus=sum(ClusterPeakGpus), ObservedSamples=sum(ObservedSamples), GPUHoursSamples=sum(GPUHoursSamples), CostSamples=sum(CostSamples) by Timestamp, workspace, namespace\n")
	b.WriteString("| summarize GpuHours=round(sum(HourlyGpuHours), 1), EstimatedCostUSD=round(sum(HourlyCost), 2), PeakGpus=round(max(HourlyPeakGpus), 2), ObservedSamples=sum(ObservedSamples), GPUHoursSamples=sum(GPUHoursSamples), CostSamples=sum(CostSamples) by workspace, namespace;\n")
	b.WriteString("let WorkspaceUtil = GpuHealth()\n")
	fmt.Fprintf(&b, "| where Timestamp > ago(%ds)\n", windowSeconds(window))
	b.WriteString("| where metric == 'gpu_utilization'\n")
	if cluster != "" {
		fmt.Fprintf(&b, "| where Cluster == %s\n", kustoquery.QuoteString(cluster))
	}
	if namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kustoquery.QuoteString(namespace))
	}
	b.WriteString("| where isfinite(Value) and Value between (0.0 .. 100.0)\n")
	b.WriteString("| summarize UtilSum=sum(Value), UtilizationSamples=count() by Cluster, namespace\n")
	b.WriteString("| join kind=inner (CostRows | distinct Cluster, workspace, namespace) on Cluster, namespace\n")
	b.WriteString("| summarize AvgUtil=round(sum(UtilSum) / sum(UtilizationSamples), 1), UtilizationSamples=sum(UtilizationSamples) by workspace, namespace;\n")
	b.WriteString("WorkspaceCosts\n")
	b.WriteString("| join kind=leftouter WorkspaceUtil on workspace, namespace\n")
	b.WriteString("| project workspace, namespace, GpuHours, EstimatedCostUSD, PeakGpus, AvgUtil, ObservedSamples, GPUHoursSamples, CostSamples, UtilizationSamples\n")
	b.WriteString("| order by GpuHours desc")
	return b.String()
}

func buildIdleKQL(window time.Duration, namespace, cluster string) string {
	var b strings.Builder
	b.WriteString("GpuHealth()\n")
	fmt.Fprintf(&b, "| where Timestamp > ago(%ds)\n", windowSeconds(window))
	b.WriteString("| where metric == 'gpu_utilization'\n")
	if cluster != "" {
		fmt.Fprintf(&b, "| where Cluster == %s\n", kustoquery.QuoteString(cluster))
	}
	if namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kustoquery.QuoteString(namespace))
	}
	b.WriteString("| extend ValidUtil=iff(isfinite(Value) and Value between (0.0 .. 100.0), Value, real(null))\n")
	b.WriteString("| summarize AvgUtil=round(avg(ValidUtil), 1), Samples=countif(isnotnull(ValidUtil)), ObservedSamples=count(), arg_max(Timestamp, modelName, namespace, pod) by Cluster, instance, gpu\n")
	b.WriteString("| project Cluster, instance, gpu, modelName, namespace, pod, AvgUtil, Samples, ObservedSamples\n")
	b.WriteString("| order by AvgUtil asc")
	return b.String()
}

func assemble(window time.Duration, threshold float64, namespace, cluster string, workspaceRows, idleRows []kustoquery.Row) Snapshot {
	snap := Snapshot{
		Window:     window.String(),
		Workspaces: make([]WorkspaceCost, 0, len(workspaceRows)),
		IdleGPUs:   make([]IdleGPU, 0, len(idleRows)),
	}
	for _, row := range workspaceRows {
		if namespace != "" && row.Str("namespace") != namespace {
			continue
		}
		gpuHours, hoursOK := nonnegativeNumber(row, "GpuHours")
		estimatedCost, costOK := nonnegativeNumber(row, "EstimatedCostUSD")
		peakGPUs, _ := nonnegativeNumber(row, "PeakGpus")
		coverage := CostCoverage{
			ObservedSamples:    sampleCount(row, "ObservedSamples"),
			GPUHoursSamples:    sampleCount(row, "GPUHoursSamples"),
			CostSamples:        sampleCount(row, "CostSamples"),
			UtilizationSamples: sampleCount(row, "UtilizationSamples"),
		}
		var avgUtil *float64
		if value, ok := utilization(row); ok && coverage.UtilizationSamples > 0 {
			avgUtil = &value
		}
		workspace := row.Str("workspace")
		if workspace == "" {
			workspace = row.Str("namespace")
		}
		snap.Workspaces = append(snap.Workspaces, WorkspaceCost{
			Workspace:         workspace,
			Namespace:         row.Str("namespace"),
			PeakGPUs:          peakGPUs,
			GPUHours:          gpuHours,
			EstimatedCostUSD:  estimatedCost,
			AvgUtilPct:        avgUtil,
			CostAvailable:     costOK && coverage.CostSamples > 0,
			GPUHoursAvailable: hoursOK && coverage.GPUHoursSamples > 0,
			Coverage:          coverage,
		})
		snap.CostAvailable = snap.CostAvailable || (costOK && coverage.CostSamples > 0)
		snap.GPUHoursAvailable = snap.GPUHoursAvailable || (hoursOK && coverage.GPUHoursSamples > 0)
		snap.CostCoverage.ObservedSamples += coverage.ObservedSamples
		snap.CostCoverage.GPUHoursSamples += coverage.GPUHoursSamples
		snap.CostCoverage.CostSamples += coverage.CostSamples
		snap.CostCoverage.UtilizationSamples += coverage.UtilizationSamples
		snap.TotalGPUHours += gpuHours
		snap.TotalEstimatedCostUSD += estimatedCost
	}
	snap.TotalGPUHours = round1(snap.TotalGPUHours)
	snap.TotalEstimatedCostUSD = round2(snap.TotalEstimatedCostUSD)
	sort.SliceStable(snap.Workspaces, func(i, j int) bool {
		return snap.Workspaces[i].GPUHours > snap.Workspaces[j].GPUHours
	})

	for _, row := range idleRows {
		if cluster != "" && row.Str("Cluster") != cluster {
			continue
		}
		if namespace != "" && row.Str("namespace") != namespace {
			continue
		}
		snap.IdleCoverage.ObservedGPUs++
		observed := sampleCount(row, "ObservedSamples")
		snap.IdleCoverage.ObservedSamples += observed
		avgUtil, ok := utilization(row)
		samples := sampleCount(row, "Samples")
		if !ok || samples == 0 || samples > observed {
			continue
		}
		snap.IdleCoverage.MeasuredGPUs++
		snap.IdleCoverage.ValidSamples += samples
		if samples <= idleMinSamples {
			continue
		}
		snap.IdleCoverage.EligibleGPUs++
		snap.IdleAvailable = true
		if avgUtil >= threshold {
			continue
		}
		snap.IdleGPUs = append(snap.IdleGPUs, IdleGPU{
			Instance:   row.Str("instance"),
			GPU:        row.Str("gpu"),
			ModelName:  row.Str("modelName"),
			Namespace:  row.Str("namespace"),
			Pod:        row.Str("pod"),
			AvgUtilPct: avgUtil,
			Samples:    samples,
		})
	}
	sort.SliceStable(snap.IdleGPUs, func(i, j int) bool {
		return snap.IdleGPUs[i].AvgUtilPct < snap.IdleGPUs[j].AvgUtilPct
	})
	return snap
}

func nonnegativeNumber(row kustoquery.Row, column string) (float64, bool) {
	value, ok := row.Num(column)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, true
}

func utilization(row kustoquery.Row) (float64, bool) {
	value, ok := nonnegativeNumber(row, "AvgUtil")
	return value, ok && value <= 100
}

func sampleCount(row kustoquery.Row, column string) int {
	value, ok := nonnegativeNumber(row, column)
	if !ok || value >= float64(int(^uint(0)>>1)) || math.Trunc(value) != value {
		return 0
	}
	return int(value)
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
