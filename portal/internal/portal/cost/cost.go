// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cost builds the portal's Cost board.
//
// The board's spine is allocation-based GPU-hours and estimated cost by TauGrid
// workspace from CostTracking.GpuCostHourly. A second query uses GpuHealth()
// utilization to list underutilized physical GPUs so operators can reclaim
// capacity without treating exporter pods as owners.
//
// Data access is the shell-out kustoquery.Querier seam (shared with the Cluster
// board), so tests inject a fake with canned Kusto JSON and no live ADX.
// The portal queries Metrics and reaches CostTracking through a same-cluster
// cross-database reference.
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

// DefaultWindow is the chargeback look-back.
const DefaultWindow = 7 * 24 * time.Hour

// DefaultIdleThresholdPct flags a GPU as underutilized when its average
// utilization over the window is below this.
const DefaultIdleThresholdPct = 20.0

// idleMinSamples requires a GPU to have enough utilization samples before it can
// be called idle, so a GPU seen once at 0% is not reported. Mirrors the panel's
// `Samples > 10`.
const idleMinSamples = 10

// Options controls the board queries. Window defaults to DefaultWindow;
// IdleThresholdPct defaults to DefaultIdleThresholdPct. Namespace and Cluster,
// when set, scope every query to one workspace's rows in the shared Metrics
// database (both safe KQL literals).
type Options struct {
	Window           time.Duration
	IdleThresholdPct float64
	CostDatabase     string
	Namespace        string
	Cluster          string
}

// WorkspaceCost is one workspace's allocation chargeback over the window.
type WorkspaceCost struct {
	Workspace        string  `json:"workspace"`
	Namespace        string  `json:"namespace"`
	PeakGPUs         float64 `json:"peakGPUs"`
	GPUHours         float64 `json:"gpuHours"`
	EstimatedCostUSD float64 `json:"estimatedCostUSD"`
	AvgUtilPct       float64 `json:"avgUtilPct"`
}

// IdleGPU is one underutilized GPU (average utilization below the threshold).
type IdleGPU struct {
	Instance   string  `json:"instance"`
	GPU        string  `json:"gpu"`
	ModelName  string  `json:"modelName,omitempty"`
	Namespace  string  `json:"namespace,omitempty"`
	Pod        string  `json:"pod,omitempty"`
	AvgUtilPct float64 `json:"avgUtilPct"`
	Samples    int     `json:"samples"`
}

// Snapshot is the Cost board payload: per-workspace allocation cost plus the
// physical idle-GPU list and window-total rollups.
type Snapshot struct {
	Window                string          `json:"window"`
	TotalGPUHours         float64         `json:"totalGPUHours"`
	TotalEstimatedCostUSD float64         `json:"totalEstimatedCostUSD"`
	Workspaces            []WorkspaceCost `json:"workspaces"`
	IdleGPUs              []IdleGPU       `json:"idleGPUs"`
}

// Board runs the allocation-cost and GpuHealth utilization queries via the
// Querier and assembles the Snapshot.
func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	threshold := opts.IdleThresholdPct
	if threshold <= 0 {
		threshold = DefaultIdleThresholdPct
	}

	workspaceRows, err := q.Query(ctx, buildWorkspaceKQL(window, opts.CostDatabase, opts.Namespace, opts.Cluster))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query allocation cost by workspace: %w", err)
	}
	idleRows, err := q.Query(ctx, buildIdleKQL(window, threshold, opts.Namespace, opts.Cluster))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query idle gpus: %w", err)
	}
	return assemble(window, opts.Namespace, workspaceRows, idleRows), nil
}

// windowSeconds renders a duration as an integer-second count for ago(), never
// below 1s (injection-proof literal).
func windowSeconds(window time.Duration) int64 {
	return max(int64(window/time.Second), 1)
}

// buildWorkspaceKQL renders allocation GPU-hours and estimated cost from the
// hourly chargeback table. It accepts only schema-v4 rows because older rows
// have no trustworthy cluster identity in shared ADX databases. gpu_count is
// fractional GPU-hours; peak_gpu_count is each cluster's sampled peak.
func buildWorkspaceKQL(window time.Duration, database, namespace, cluster string) string {
	if strings.TrimSpace(database) == "" {
		database = "CostTracking"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "let CostRows = database(%s).GpuCostHourly\n", kustoquery.QuoteString(database))
	fmt.Fprintf(&b, "| where Timestamp > ago(%ds)\n", windowSeconds(window))
	if namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kustoquery.QuoteString(namespace))
	}
	b.WriteString("| extend workspace=tostring(column_ifexists('workspace', '')), reported_peak_gpu_count=toreal(column_ifexists('peak_gpu_count', real(null))), schema_version=tolong(column_ifexists('schema_version', long(null)))\n")
	b.WriteString("| extend workspace=iff(isempty(workspace), namespace, workspace);\n")
	b.WriteString("CostRows\n")
	b.WriteString("| where schema_version == 4\n")
	if cluster != "" {
		fmt.Fprintf(&b, "| where Cluster == %s\n", kustoquery.QuoteString(cluster))
	}
	b.WriteString("| where isnotempty(workspace) and isnotempty(namespace)\n")
	b.WriteString("| summarize ClusterGpuHours=sum(gpu_count), ClusterCost=sum(hourly_cost), ClusterPeakGpus=max(reported_peak_gpu_count), ClusterUtil=avg(avg_util) by Timestamp, workspace, namespace, Cluster\n")
	b.WriteString("| summarize HourlyGpuHours=sum(ClusterGpuHours), HourlyCost=sum(ClusterCost), HourlyPeakGpus=sum(ClusterPeakGpus), HourlyUtil=avg(ClusterUtil) by Timestamp, workspace, namespace\n")
	b.WriteString("| summarize GpuHours=round(sum(HourlyGpuHours), 1), EstimatedCostUSD=round(sum(HourlyCost), 2), PeakGpus=round(max(HourlyPeakGpus), 2), AvgUtil=round(avg(HourlyUtil), 1) by workspace, namespace\n")
	b.WriteString("| project workspace, namespace, GpuHours, EstimatedCostUSD, PeakGpus, AvgUtil\n")
	b.WriteString("| order by GpuHours desc")
	return b.String()
}

// buildIdleKQL renders the underutilized-GPU query (avg < threshold with enough
// samples). The threshold is emitted with %g so it round-trips as a KQL number.
// When namespace or cluster is set the query is scoped to that workspace.
func buildIdleKQL(window time.Duration, threshold float64, namespace, cluster string) string {
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
	b.WriteString("| summarize AvgUtil=round(avg(Value), 1), Samples=count(), arg_max(Timestamp, modelName, namespace, pod) by Cluster, instance, gpu\n")
	fmt.Fprintf(&b, "| where AvgUtil < %g and Samples > %d\n", threshold, idleMinSamples)
	b.WriteString("| project instance, gpu, modelName, namespace, pod, AvgUtil, Samples\n")
	b.WriteString("| order by AvgUtil asc")
	return b.String()
}

// assemble folds the two result sets into a Snapshot and totals GPU-hours.
func assemble(window time.Duration, namespace string, workspaceRows, idleRows []kustoquery.Row) Snapshot {
	snap := Snapshot{
		Window:     window.String(),
		Workspaces: make([]WorkspaceCost, 0, len(workspaceRows)),
		IdleGPUs:   make([]IdleGPU, 0, len(idleRows)),
	}
	for _, row := range workspaceRows {
		if namespace != "" && row.Str("namespace") != namespace {
			continue
		}
		gpuHours, _ := row.Num("GpuHours")
		estimatedCost, _ := row.Num("EstimatedCostUSD")
		peakGPUs, _ := row.Num("PeakGpus")
		avgUtil, _ := row.Num("AvgUtil")
		workspace := row.Str("workspace")
		if workspace == "" {
			workspace = row.Str("namespace")
		}
		snap.Workspaces = append(snap.Workspaces, WorkspaceCost{
			Workspace:        workspace,
			Namespace:        row.Str("namespace"),
			PeakGPUs:         peakGPUs,
			GPUHours:         gpuHours,
			EstimatedCostUSD: estimatedCost,
			AvgUtilPct:       avgUtil,
		})
		snap.TotalGPUHours += gpuHours
		snap.TotalEstimatedCostUSD += estimatedCost
	}
	snap.TotalGPUHours = round1(snap.TotalGPUHours)
	snap.TotalEstimatedCostUSD = round2(snap.TotalEstimatedCostUSD)
	sort.SliceStable(snap.Workspaces, func(i, j int) bool {
		return snap.Workspaces[i].GPUHours > snap.Workspaces[j].GPUHours
	})

	for _, row := range idleRows {
		if namespace != "" && row.Str("namespace") != namespace {
			continue
		}
		avgUtil, _ := row.Num("AvgUtil")
		samples, _ := row.Num("Samples")
		snap.IdleGPUs = append(snap.IdleGPUs, IdleGPU{
			Instance:   row.Str("instance"),
			GPU:        row.Str("gpu"),
			ModelName:  row.Str("modelName"),
			Namespace:  row.Str("namespace"),
			Pod:        row.Str("pod"),
			AvgUtilPct: avgUtil,
			Samples:    int(samples),
		})
	}
	sort.SliceStable(snap.IdleGPUs, func(i, j int) bool {
		return snap.IdleGPUs[i].AvgUtilPct < snap.IdleGPUs[j].AvgUtilPct
	})
	return snap
}

// round1 rounds to one decimal, matching the KQL round(..., 1) so summed totals
// don't accumulate float noise in the payload.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
