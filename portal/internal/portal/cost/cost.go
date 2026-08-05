// Package cost builds the portal's Cost board.
//
// The board's spine is GPU-hours by namespace: from GpuHealth() gpu_utilization
// samples it derives, per namespace, the distinct GPU count, the observed span,
// GPU-hours (span_hours * gpus), and average utilization — the chargeback rows
// the FinOps view leads with. A second query lists idle/underutilized GPUs
// (avg < threshold) so owners can reclaim capacity.
//
// Data access is the shell-out kustoquery.Querier seam (shared with the Cluster
// board), so tests inject a fake with canned Kusto JSON and no live ADX.
// GpuHealth() lives in the Metrics database, the same target the portal's
// --kusto-* flags already point at.
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
	Namespace        string
	Cluster          string
}

// NamespaceCost is one namespace's GPU chargeback row: GPU-hours over the
// window plus the distinct GPU count and average utilization.
type NamespaceCost struct {
	Namespace  string  `json:"namespace"`
	GPUs       int     `json:"gpus"`
	GPUHours   float64 `json:"gpuHours"`
	AvgUtilPct float64 `json:"avgUtilPct"`
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

// Snapshot is the Cost board payload: per-namespace GPU-hours plus the idle-GPU
// list and window-total rollups.
type Snapshot struct {
	Window        string          `json:"window"`
	TotalGPUHours float64         `json:"totalGPUHours"`
	Namespaces    []NamespaceCost `json:"namespaces"`
	IdleGPUs      []IdleGPU       `json:"idleGPUs"`
}

// Board runs the two GpuHealth()-derived queries via the Querier and assembles
// the Snapshot.
func Board(ctx context.Context, q kustoquery.Querier, opts Options) (Snapshot, error) {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	threshold := opts.IdleThresholdPct
	if threshold <= 0 {
		threshold = DefaultIdleThresholdPct
	}

	nsRows, err := q.Query(ctx, buildNamespaceKQL(window, opts.Namespace, opts.Cluster))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query gpu hours by namespace: %w", err)
	}
	idleRows, err := q.Query(ctx, buildIdleKQL(window, threshold, opts.Namespace, opts.Cluster))
	if err != nil {
		return Snapshot{}, fmt.Errorf("query idle gpus: %w", err)
	}
	return assemble(window, opts.Namespace, nsRows, idleRows), nil
}

// windowSeconds renders a duration as an integer-second count for ago(), never
// below 1s (injection-proof literal).
func windowSeconds(window time.Duration) int64 {
	return max(int64(window/time.Second), 1)
}

// buildNamespaceKQL renders the GPU-hours-by-namespace chargeback query,
// combining the GPU-hours and average-utilization aggregations in one pass. When
// cluster is set the query is scoped to that cluster's rows so a shared Metrics
// database's other clusters do not inflate the chargeback totals.
func buildNamespaceKQL(window time.Duration, namespace, cluster string) string {
	var b strings.Builder
	b.WriteString("GpuHealth()\n")
	fmt.Fprintf(&b, "| where Timestamp > ago(%ds)\n", windowSeconds(window))
	b.WriteString("| where metric == 'gpu_utilization'\n")
	if cluster != "" {
		fmt.Fprintf(&b, "| where Cluster == %s\n", kustoquery.QuoteString(cluster))
	}
	b.WriteString("| where isnotempty(namespace)\n")
	if namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kustoquery.QuoteString(namespace))
	}
	b.WriteString("| summarize MinT=min(Timestamp), MaxT=max(Timestamp), Gpus=dcount(strcat(instance, gpu)), AvgUtil=round(avg(Value), 1) by namespace\n")
	b.WriteString("| extend GpuHours=round(datetime_diff('second', MaxT, MinT) / 3600.0 * Gpus, 1)\n")
	b.WriteString("| project namespace, GpuHours, Gpus, AvgUtil\n")
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
	b.WriteString("| summarize AvgUtil=round(avg(Value), 1), Samples=count() by instance, gpu, modelName, namespace, pod\n")
	fmt.Fprintf(&b, "| where AvgUtil < %g and Samples > %d\n", threshold, idleMinSamples)
	b.WriteString("| project instance, gpu, modelName, namespace, pod, AvgUtil, Samples\n")
	b.WriteString("| order by AvgUtil asc")
	return b.String()
}

// assemble folds the two result sets into a Snapshot and totals GPU-hours.
func assemble(window time.Duration, namespace string, nsRows, idleRows []kustoquery.Row) Snapshot {
	snap := Snapshot{
		Window:     window.String(),
		Namespaces: make([]NamespaceCost, 0, len(nsRows)),
		IdleGPUs:   make([]IdleGPU, 0, len(idleRows)),
	}
	for _, row := range nsRows {
		if namespace != "" && row.Str("namespace") != namespace {
			continue
		}
		gpuHours, _ := row.Num("GpuHours")
		gpus, _ := row.Num("Gpus")
		avgUtil, _ := row.Num("AvgUtil")
		snap.Namespaces = append(snap.Namespaces, NamespaceCost{
			Namespace:  row.Str("namespace"),
			GPUs:       int(gpus),
			GPUHours:   gpuHours,
			AvgUtilPct: avgUtil,
		})
		snap.TotalGPUHours += gpuHours
	}
	snap.TotalGPUHours = round1(snap.TotalGPUHours)
	sort.SliceStable(snap.Namespaces, func(i, j int) bool {
		return snap.Namespaces[i].GPUHours > snap.Namespaces[j].GPUHours
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
