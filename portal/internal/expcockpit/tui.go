package expcockpit

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

const (
	tuiMaxMetricRows = 24
	tuiMaxRunRows    = 16
	tuiMaxChartRows  = 8
	tuiChartWidth    = 48
)

func RenderTUI(ctx context.Context, store *expstore.Store, opts Options) ([]byte, error) {
	snapshot, err := BuildSnapshot(ctx, store, opts)
	if err != nil {
		return nil, err
	}
	return RenderSnapshotTUI(snapshot), nil
}

func RenderSnapshotTUI(snapshot Snapshot) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Stellar dashboard: %s (%s)\n", snapshot.Target, snapshot.TargetType)
	fmt.Fprintf(&buf, "store: %s\n", snapshot.StorePath)
	if snapshot.Experiment != nil {
		fmt.Fprintf(&buf, "experiment: %s (%s)\n", snapshot.Experiment.Name, snapshot.Experiment.ExperimentID)
	}
	fmt.Fprintln(&buf)

	writeTUISummary(&buf, snapshot)
	writeTUIStatus(&buf, snapshot.Status)
	writeTUIMetricCards(&buf, snapshot.Cards)
	writeTUIChart(&buf, snapshot.Chart)
	writeTUIRuns(&buf, snapshot.Runs)
	writeTUIActions(&buf, snapshot.Actions)
	writeTUIWarnings(&buf, snapshot.Warnings)
	return buf.Bytes()
}

func writeTUISummary(buf *bytes.Buffer, snapshot Snapshot) {
	summary := snapshot.Summary
	if summary.Status == "" && summary.CurrentAnswer == "" && summary.BestEvidence == "" && summary.NextAction == "" {
		return
	}
	fmt.Fprintln(buf, "SUMMARY")
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	writeTUIField(tw, "status", summary.Status)
	writeTUIField(tw, "answer", summary.CurrentAnswer)
	writeTUIField(tw, "evidence", summary.BestEvidence)
	writeTUIField(tw, "confidence", summary.Confidence)
	writeTUIField(tw, "seed_coverage", summary.SeedCoverage)
	writeTUIField(tw, "next_action", summary.NextAction)
	writeTUIField(tw, "next_command", summary.NextCommand)
	_ = tw.Flush()
	fmt.Fprintln(buf)
}

func writeTUIStatus(buf *bytes.Buffer, status expstore.Status) {
	fmt.Fprintln(buf, "STATUS")
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNS\tGROUPS\tCONFIGS\tMETRIC_FILES\tARTIFACTS\tOBSERVATIONS\tLATEST_EVENT")
	fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
		status.Runs,
		status.RunGroups,
		status.Configs,
		status.MetricFiles,
		status.Artifacts,
		status.Observations,
		tuiDash(status.LatestEventAt),
	)
	_ = tw.Flush()
	fmt.Fprintf(buf, "states: %s\n", tuiCountMap(status.StateCounts))
	if len(status.LifecycleCounts) > 0 {
		fmt.Fprintf(buf, "lifecycle: %s\n", tuiCountMap(status.LifecycleCounts))
	}
	fmt.Fprintln(buf)
}

func writeTUIMetricCards(buf *bytes.Buffer, cards []CardView) {
	if len(cards) == 0 {
		fmt.Fprintln(buf, "METRICS")
		fmt.Fprintln(buf, "(no scalar metrics)")
		fmt.Fprintln(buf)
		return
	}
	fmt.Fprintln(buf, "METRICS")
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CARD\tMETRIC\tGROUP\tRUNS\tLATEST_STEP\tBEST\tRANGE")
	rows := 0
	for _, card := range cards {
		for _, metric := range card.Metrics {
			for _, group := range metric.Groups {
				if rows >= tuiMaxMetricRows {
					remaining := tuiMetricGroupCount(cards) - rows
					if remaining > 0 {
						fmt.Fprintf(tw, "... \t%d more metric group rows\t\t\t\t\t\n", remaining)
					}
					_ = tw.Flush()
					fmt.Fprintln(buf)
					return
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s..%s\n",
					card.Name,
					metric.Name,
					group.RunGroupID,
					group.RunCount,
					tuiDash(group.LatestStep),
					tuiDash(group.Best),
					tuiDash(group.Min),
					tuiDash(group.Max),
				)
				rows++
			}
		}
	}
	_ = tw.Flush()
	fmt.Fprintln(buf)
}

func writeTUIChart(buf *bytes.Buffer, chart ChartView) {
	if !chart.HasData {
		fmt.Fprintln(buf, "CHART")
		fmt.Fprintln(buf, "(no chart data)")
		fmt.Fprintln(buf)
		return
	}
	fmt.Fprintf(buf, "CHART %s (step %s..%s, value %s..%s)\n",
		chart.MetricName,
		tuiDash(chart.XMin),
		tuiDash(chart.XMax),
		tuiDash(chart.YMin),
		tuiDash(chart.YMax),
	)
	if chart.Smoothing != nil {
		fmt.Fprintf(buf, "smoothing: %s alpha=%.2f (%s)\n", chart.Smoothing.Method, chart.Smoothing.Alpha, chart.Smoothing.Reason)
	}
	rows := len(chart.Series)
	if rows > tuiMaxChartRows {
		rows = tuiMaxChartRows
	}
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tGROUP\tPOINTS\tTREND")
	for i := 0; i < rows; i++ {
		series := chart.Series[i]
		points := series.Values
		if len(series.SmoothedValues) > 0 {
			points = series.SmoothedValues
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\n",
			series.RunID,
			series.RunGroupID,
			series.RenderedPoints,
			series.PointCount,
			tuiSparkline(points, tuiChartWidth),
		)
	}
	if len(chart.Series) > rows {
		fmt.Fprintf(tw, "... \t%d more series\t\t\n", len(chart.Series)-rows)
	}
	_ = tw.Flush()
	fmt.Fprintln(buf)
}

func writeTUIRuns(buf *bytes.Buffer, runs []RunView) {
	if len(runs) == 0 {
		fmt.Fprintln(buf, "RUNS")
		fmt.Fprintln(buf, "(no runs)")
		fmt.Fprintln(buf)
		return
	}
	fmt.Fprintln(buf, "RUNS")
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tLIFECYCLE\tSTATE\tGROUP\tMETRICS\tCOMMAND")
	rows := len(runs)
	if rows > tuiMaxRunRows {
		rows = tuiMaxRunRows
	}
	for i := 0; i < rows; i++ {
		run := runs[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			run.RunID,
			tuiDash(run.LifecycleState),
			tuiDash(run.State),
			tuiDash(run.RunGroupID),
			tuiShortList(run.MetricNames, 4),
			tuiDash(run.ObserveCLI),
		)
	}
	if len(runs) > rows {
		fmt.Fprintf(tw, "... \t%d more runs\t\t\t\t\n", len(runs)-rows)
	}
	_ = tw.Flush()
	fmt.Fprintln(buf)
}

func writeTUIActions(buf *bytes.Buffer, actions ActionView) {
	openCLI := actions.OpenCLI
	if openCLI == "" {
		openCLI = actions.CopyCLI
	}
	if openCLI == "" && actions.CopyCLI == "" && actions.NextCommand == "" && actions.CopySQL == "" && actions.ExportPacket == "" {
		return
	}
	fmt.Fprintln(buf, "ACTIONS")
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	writeTUIField(tw, "open_dashboard", openCLI)
	writeTUIField(tw, "save_html", actions.CopyCLI)
	writeTUIField(tw, "next", actions.NextCommand)
	writeTUIField(tw, "query", actions.CopySQL)
	writeTUIField(tw, "export", actions.ExportPacket)
	_ = tw.Flush()
	fmt.Fprintln(buf)
}

func writeTUIWarnings(buf *bytes.Buffer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(buf, "WARNINGS")
	for _, warning := range warnings {
		fmt.Fprintf(buf, "- %s\n", warning)
	}
}

func writeTUIField(tw *tabwriter.Writer, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fmt.Fprintf(tw, "%s:\t%s\n", name, value)
}

func tuiMetricGroupCount(cards []CardView) int {
	total := 0
	for _, card := range cards {
		for _, metric := range card.Metrics {
			total += len(metric.Groups)
		}
	}
	return total
}

func tuiCountMap(counts map[string]int) string {
	if len(counts) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}

func tuiShortList(values []string, limit int) string {
	if len(values) == 0 {
		return "-"
	}
	if limit <= 0 || len(values) <= limit {
		return strings.Join(values, ",")
	}
	visible := append([]string(nil), values[:limit]...)
	visible = append(visible, fmt.Sprintf("+%d", len(values)-limit))
	return strings.Join(visible, ",")
}

func tuiDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func tuiSparkline(points []ChartPoint, width int) string {
	if len(points) == 0 {
		return "-"
	}
	if width <= 0 {
		width = tuiChartWidth
	}
	values := sampleChartValues(points, width)
	minValue, maxValue := values[0], values[0]
	for _, value := range values[1:] {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	const levels = "_.:-=+*#%@"
	if minValue == maxValue {
		return strings.Repeat("-", len(values))
	}
	var b strings.Builder
	for _, value := range values {
		ratio := (value - minValue) / (maxValue - minValue)
		index := int(math.Round(ratio * float64(len(levels)-1)))
		if index < 0 {
			index = 0
		}
		if index >= len(levels) {
			index = len(levels) - 1
		}
		b.WriteByte(levels[index])
	}
	return b.String()
}

func sampleChartValues(points []ChartPoint, width int) []float64 {
	if len(points) <= width {
		out := make([]float64, 0, len(points))
		for _, point := range points {
			out = append(out, point.Value)
		}
		return out
	}
	out := make([]float64, 0, width)
	for i := 0; i < width; i++ {
		index := int(math.Round(float64(i) * float64(len(points)-1) / float64(width-1)))
		out = append(out, points[index].Value)
	}
	return out
}
