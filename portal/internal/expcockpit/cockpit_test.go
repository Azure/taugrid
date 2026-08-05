package expcockpit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func dashboardSectionSourceIndex(source, id string) int {
	return strings.Index(source, fmt.Sprintf(`id: "%s"`, id))
}

func TestKustoActionsSuppressUnwritableObservationCommand(t *testing.T) {
	actions := kustoActions("kusto://ExperimentMetrics", "seed-1", "run", nil, "")
	if actions.ObserveCLI != "" {
		t.Fatalf("Kusto observation command requires a locally materialized scope, got %q", actions.ObserveCLI)
	}
}

func TestBuildChartDensitySamplesMillionPointOverlay(t *testing.T) {
	points := make([]metricPoint, 0, 20000)
	for i := 0; i < 20000; i++ {
		points = append(points, metricPoint{
			RunID:      "seed-1",
			RunGroupID: "reference-group",
			MetricName: "train/return",
			Card:       "Outcome",
			Step:       int64(i),
			Value:      float64(i % 997),
		})
	}

	chart := buildChart(points, "train/return", map[string]string{"reference-group": "group-a"})
	if !chart.HasData || len(chart.Series) != 1 {
		t.Fatalf("unexpected chart: %+v", chart)
	}
	series := chart.Series[0]
	if series.PointCount != len(points) {
		t.Fatalf("point_count=%d, want %d", series.PointCount, len(points))
	}
	if !series.Decimated || series.RenderedPoints >= series.PointCount || series.RenderedPoints > chartMaxRenderedPoints {
		t.Fatalf("expected sampled series within render budget: %+v", series)
	}
	if len(series.Values) != series.RenderedPoints {
		t.Fatalf("values=%d rendered=%d", len(series.Values), series.RenderedPoints)
	}
	if series.Overlay.Source != "local" || series.Overlay.StartStep != 0 || series.Overlay.EndStep != 19999 {
		t.Fatalf("unexpected overlay metadata: %+v", series.Overlay)
	}
	if series.Values[0].Step != 0 || series.Values[len(series.Values)-1].Step != 19999 {
		t.Fatalf("density sampling should preserve endpoints: first=%+v last=%+v", series.Values[0], series.Values[len(series.Values)-1])
	}
	if series.RenderedPoints > chartWidthPixels*chartSamplesPerPixel {
		t.Fatalf("rendered dense chart above pixel-scale budget: %+v", series)
	}
}

func TestBuildChartSmoothsDenseTrainingLossWithoutDroppingRawValues(t *testing.T) {
	points := make([]metricPoint, 0, chartSmoothingDensePointThreshold+20)
	for i := 0; i < chartSmoothingDensePointThreshold+20; i++ {
		value := 10.0
		if i%2 == 1 {
			value = 0
		}
		points = append(points, metricPoint{
			RunID:      "seed-1",
			RunGroupID: "vision-r3",
			MetricName: "train/loss",
			Step:       int64(i),
			Value:      value,
		})
	}

	chart := buildChart(points, "train/loss", map[string]string{"vision-r3": "group-vision-r3"})

	if chart.Smoothing == nil || chart.Smoothing.Method != "ema" || !chart.Smoothing.RawPreserved {
		t.Fatalf("dense train/loss should default to presentation smoothing with raw preservation: %+v", chart.Smoothing)
	}
	if len(chart.Series) != 1 {
		t.Fatalf("series=%d, want 1", len(chart.Series))
	}
	series := chart.Series[0]
	if len(series.Values) != len(points) || len(series.SmoothedValues) != len(series.Values) {
		t.Fatalf("raw and smoothed rendered values should both be present: raw=%d smoothed=%d points=%d", len(series.Values), len(series.SmoothedValues), len(points))
	}
	if series.Values[1].Value != 0 {
		t.Fatalf("raw value should remain unsmoothed, got %+v", series.Values[1])
	}
	if series.SmoothedValues[1].Value == series.Values[1].Value {
		t.Fatalf("smoothed value should differ from jagged raw point: raw=%+v smoothed=%+v", series.Values[1], series.SmoothedValues[1])
	}
}

func TestBuildChartDoesNotSmoothVisiblePointLossCharts(t *testing.T) {
	const visiblePointCount = 509
	points := make([]metricPoint, 0, visiblePointCount)
	for i := 0; i < visiblePointCount; i++ {
		points = append(points, metricPoint{
			RunID:      "sample-baseline-run",
			RunGroupID: "demo-exp",
			MetricName: "pretrain/loss",
			Step:       int64(i),
			Value:      20 - float64(i)/100,
		})
	}

	chart := buildChart(points, "pretrain/loss", map[string]string{"demo-exp": "group-demo-exp"})

	if chart.Smoothing != nil {
		t.Fatalf("visible-point loss charts should stay raw-only; smoothing=%+v", chart.Smoothing)
	}
	if len(chart.Series) != 1 {
		t.Fatalf("series=%d, want 1", len(chart.Series))
	}
	if got := len(chart.Series[0].SmoothedValues); got != 0 {
		t.Fatalf("visible-point loss chart should not emit smoothed values, got %d", got)
	}
}

func TestBuildChartDoesNotSmoothSparseOrEvalMetricsByDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		metric string
		points int
	}{
		{name: "sparse train loss", metric: "train/loss", points: 12},
		{name: "dense eval auprc", metric: "eval/macro_auprc", points: chartSmoothingDensePointThreshold + 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := make([]metricPoint, 0, tc.points)
			for i := 0; i < tc.points; i++ {
				points = append(points, metricPoint{
					RunID:      "seed-1",
					RunGroupID: "vision-r3",
					MetricName: tc.metric,
					Step:       int64(i),
					Value:      float64(i) / 100,
				})
			}

			chart := buildChart(points, tc.metric, map[string]string{"vision-r3": "group-vision-r3"})

			if chart.Smoothing != nil {
				t.Fatalf("%s should not default to smoothing: %+v", tc.metric, chart.Smoothing)
			}
			if len(chart.Series) != 1 || len(chart.Series[0].SmoothedValues) != 0 {
				t.Fatalf("unexpected smoothed values for %s: %+v", tc.metric, chart.Series)
			}
		})
	}
}

func TestDashboardDecisionMetricPrefersFinalOutcomeOverFocusedTrainingLoss(t *testing.T) {
	points := []metricPoint{
		{RunID: "r3-live", RunGroupID: "workflow-r3", MetricName: "train/loss", Step: 1, Value: 0.42},
		{RunID: "r3-live", RunGroupID: "workflow-r3", MetricName: "train/loss", Step: 2, Value: 0.31},
		{RunID: "r3-final", RunGroupID: "workflow-r3", MetricName: "final/macro_auprc", Step: 1, Value: 0.744354},
		{RunID: "r2-final", RunGroupID: "workflow-r2", MetricName: "final/macro_auprc", Step: 1, Value: 0.701},
		{RunID: "r3-live", RunGroupID: "workflow-r3", MetricName: "eval/macro_auprc", Step: 20, Value: 0.737},
	}
	groupClasses := map[string]string{
		"workflow-r2": "group-workflow-r2",
		"workflow-r3": "group-workflow-r3",
	}
	chart := buildChart(points, "train/loss", groupClasses)
	if chart.MetricName != "train/loss" {
		t.Fatalf("test setup should keep graph focus on train/loss, got %q", chart.MetricName)
	}

	decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
	compare := buildCompareInsights(decision.Points, decision.MetricName, nil, nil, nil, nil, decision.BestGroupID)
	experiment := &expstore.ExperimentRecord{ExperimentID: "vision-vitenc-workflow-r3", Project: "vision", Name: "Fine-tune ViT-Enc"}
	summary := buildExperimentSummary("vision-vitenc-workflow-r3", "experiment", experiment, nil, nil, decision.Cards, decisionChart(chart, decision.MetricName), nil, decision.BestGroupID, defaultNextCommand("/tmp/tau-expstore", "vision-vitenc-workflow-r3", decision.MetricName))

	if decision.MetricName != "final/macro_auprc" {
		t.Fatalf("decision metric=%q, want final/macro_auprc", decision.MetricName)
	}
	if decision.BestGroupID != "workflow-r3" {
		t.Fatalf("best group=%q, want workflow-r3", decision.BestGroupID)
	}
	for label, got := range map[string]string{
		"compare summary": compare.Summary,
		"current answer":  summary.CurrentAnswer,
		"best evidence":   summary.BestEvidence,
		"next action":     summary.NextAction,
		"next command":    summary.NextCommand,
	} {
		if strings.Contains(got, "train/loss") {
			t.Fatalf("%s should not use graph-focused train/loss as the decision metric: %q", label, got)
		}
		if !strings.Contains(got, "final/macro_auprc") {
			t.Fatalf("%s should use final/macro_auprc as the decision metric: %q", label, got)
		}
	}
}

func TestDashboardDecisionMetricPrefersGenericOutcomeOverFocusedTrainingLoss(t *testing.T) {
	points := []metricPoint{
		{RunID: "policy-a", RunGroupID: "baseline", MetricName: "train/loss", Step: 1, Value: 1.4},
		{RunID: "policy-a", RunGroupID: "baseline", MetricName: "train/loss", Step: 2, Value: 0.9},
		{RunID: "policy-b", RunGroupID: "tuned", MetricName: "train/loss", Step: 1, Value: 1.3},
		{RunID: "policy-b", RunGroupID: "tuned", MetricName: "train/loss", Step: 2, Value: 0.8},
		{RunID: "policy-a", RunGroupID: "baseline", MetricName: "eval/mean_episode_return", Step: 100, Value: 1520},
		{RunID: "policy-b", RunGroupID: "tuned", MetricName: "eval/mean_episode_return", Step: 100, Value: 1660},
		{RunID: "policy-a-final", RunGroupID: "baseline", MetricName: "final/mean_episode_return", Step: 100, Value: 1535},
		{RunID: "policy-b-final", RunGroupID: "tuned", MetricName: "final/mean_episode_return", Step: 100, Value: 1690},
	}
	groupClasses := map[string]string{"baseline": "group-baseline", "tuned": "group-tuned"}
	chart := buildChart(points, "train/loss", groupClasses)

	decision := buildDecisionMetricContext(points, chart.MetricName, groupClasses)
	compare := buildCompareInsights(decision.Points, decision.MetricName, nil, nil, nil, nil, decision.BestGroupID)
	summary := buildExperimentSummary("sample-policy-sweep", "experiment", &expstore.ExperimentRecord{ExperimentID: "sample-policy-sweep", Project: "stellar-sample"}, nil, nil, decision.Cards, decisionChart(chart, decision.MetricName), nil, decision.BestGroupID, defaultNextCommand("/tmp/tau-expstore", "sample-policy-sweep", decision.MetricName))

	if decision.MetricName != "final/mean_episode_return" {
		t.Fatalf("decision metric=%q, want final/mean_episode_return", decision.MetricName)
	}
	if decision.BestGroupID != "tuned" {
		t.Fatalf("best group=%q, want tuned", decision.BestGroupID)
	}
	for label, got := range map[string]string{
		"compare summary": compare.Summary,
		"current answer":  summary.CurrentAnswer,
		"best evidence":   summary.BestEvidence,
		"next action":     summary.NextAction,
		"next command":    summary.NextCommand,
	} {
		if strings.Contains(got, "train/loss") {
			t.Fatalf("%s should not use graph-focused train/loss as the generic decision metric: %q", label, got)
		}
		if !strings.Contains(got, "final/mean_episode_return") {
			t.Fatalf("%s should use final/mean_episode_return as the generic decision metric: %q", label, got)
		}
	}
}

func TestRequestedMetricKeepsTrainReturnChartDefault(t *testing.T) {
	points := []metricPoint{
		{RunID: "seed-1", RunGroupID: "policy", MetricName: "train/return", Step: 10, Value: 900},
		{RunID: "seed-1", RunGroupID: "policy", MetricName: "eval/mean_episode_return", Step: 10, Value: 1540},
	}
	if got := requestedMetric(points, ""); got != "train/return" {
		t.Fatalf("requested metric=%q, want historical train/return chart default", got)
	}
	points = append(points, metricPoint{RunID: "seed-1-final", RunGroupID: "policy", MetricName: "final/mean_episode_return", Step: 10, Value: 1580})
	decision := buildDecisionMetricContext(points, requestedMetric(points, ""), map[string]string{"policy": "group-policy"})
	if decision.MetricName != "final/mean_episode_return" {
		t.Fatalf("decision metric=%q, want final/mean_episode_return over graph-focused train/return", decision.MetricName)
	}
}

func TestMetricCardClassifiesGenericOutcomes(t *testing.T) {
	for _, metricName := range []string{
		"final/score",
		"eval/mean_episode_return",
		"eval/reward",
		"eval/return",
		"eval/success_rate",
		"eval/aime2025/pass@1",
		"eval/exact_match",
	} {
		if got := metricCard(expstore.MetricRow{MetricName: metricName}); got != "Outcome" {
			t.Fatalf("metricCard(%q)=%q, want Outcome", metricName, got)
		}
	}
}

func TestOutputMediaUsesHydratedSnapshotArtifacts(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	want := "const detailSnapshot = state.fullSnapshot || snapshot;\n  if (isCompactPayload(snapshot) && !state.fullSnapshot) {"
	if !strings.Contains(source, want) {
		t.Fatalf("Output media panel should read hydrated full snapshot artifacts after Load media details; missing:\n%s", want)
	}
}

func TestChartSmoothingRenderingKeepsRawOverlayAndHover(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"smoothed_values",
		"chart-series-raw-overlay",
		"chart-series-smoothed",
		"raw ${metricLabel}",
		"EMA ${formatAxisValue(smoothedPoint.value)}",
		"renderChartSmoothingPill",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("chart smoothing source missing %q", want)
		}
	}
}

func TestChartHoverRenderingAvoidsScrollJankHotPath(t *testing.T) {
	jsAsset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	jsSource := string(jsAsset.Content)
	for _, want := range []string{
		"const chartHoverScrollIdleMs = 140;",
		`class: "chart-hover-capture"`,
		`hoverCapture.addEventListener("mousemove"`,
		`hoverCapture.addEventListener("wheel"`,
		`}, { passive: true });`,
		"function chartPointerFromEvent(event)",
		"function hideChartHover(context)",
		"hoverKey !== context.lastHoverKey",
		"context.tooltip.style.transform = transform;",
		"event.clientY > bounds.bottom",
		"function nearestSeriesHoverCandidate(series, x, y)",
		"function updateChartHoverSeriesState(context, activeSeries)",
		"has-hover-series",
	} {
		if !strings.Contains(jsSource, want) {
			t.Fatalf("chart hover performance source missing %q", want)
		}
	}
	if strings.Contains(jsSource, "context.tooltip.style.left =") || strings.Contains(jsSource, "context.tooltip.style.top =") {
		t.Fatal("chart tooltip hover updates should use transform instead of left/top layout writes")
	}

	cssAsset, ok, err := ReadFrontendAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.css asset missing")
	}
	cssSource := string(cssAsset.Content)
	for _, want := range []string{
		".chart-hover-capture {\n  fill: transparent;\n  pointer-events: all;",
		".chart-series-line {\n  fill: none;",
		"pointer-events: none;",
		"contain: layout style paint;",
		"will-change: transform;",
	} {
		if !strings.Contains(cssSource, want) {
			t.Fatalf("chart hover performance CSS missing %q", want)
		}
	}
	if strings.Contains(cssSource, "filter: drop-shadow") {
		t.Fatal("chart hover dot should avoid SVG drop-shadow filters in the scroll+hover hot path")
	}
}

func TestFocusedSeriesResolutionFrontendSource(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"const focusedSeriesResolutionOptions = [",
		`{ value: "20", label: "Every 20 steps" }`,
		`{ value: "50", label: "Every 50 steps" }`,
		`url.searchParams.set("step_interval", String(options.stepInterval));`,
		`url.searchParams.set("step_interval", state.focusedSeriesControls.stepInterval);`,
		`stepInterval: normalizeStepIntervalControl(url.searchParams.get("step_interval"))`,
		"function focusedSeriesStepInterval(controls, options = {})",
		"function autoFocusedSeriesStepInterval(options = {})",
		"Math.abs(end - start) <= 2000 ? 20 : 50",
		"function chartResolutionLabel(chart)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("focused series resolution source missing %q", want)
		}
	}
	if strings.Contains(jsFunctionBlock(t, source, "text"), "function renderRawMetricQueryAction") {
		t.Fatal("raw Kusto query action must be module-scoped so focused chart rendering can call it")
	}
}

func TestFocusedSeriesViewportBudgetLifecycleAndKustoEvidenceFrontendSource(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"const focusedSeriesViewportMinPoints = 600;",
		"const focusedSeriesViewportMaxPoints = 1500;",
		"Math.round(width * focusedSeriesPointsPerPixel)",
		`text(config.source, "local").trim().toLowerCase() || "local"`,
		"text(controls.startStep, \"\").trim()",
		"focusedSeriesCacheKey(metricName, queryOptions)",
		"raw_query: detail.raw_query || \"\"",
		"Copy raw KQL",
		"run.outcome_state || run.liveness_state",
		`legacy === "stale" ? "not_responding" : legacy`,
		"runContextSummary(run)",
		"copy result URI",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("viewport/lifecycle/evidence source missing %q", want)
		}
	}
}

func TestChartBrushZoomFrontendSource(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	// Drag-to-zoom must be a new *input* for the existing focused-series range
	// fetch, not a new data path: the brush writes startStep/endStep and calls
	// loadFocusedSeriesDetail, exactly like the toolbar form. These assertions
	// pin that contract so the two inputs cannot silently diverge.
	for _, want := range []string{
		"function attachChartBrush(brush)",
		"function applyBrushRange(lo, hi)",
		"function clearBrushRange()",
		`hoverCapture.addEventListener("mousedown"`,
		`hoverCapture.addEventListener("dblclick"`,
		"loadFocusedSeriesDetail().catch(renderError);",
		"domain.xMin + ((x - plot.left) / plotWidth) * (domain.xMax - domain.xMin)",
		"brush: true,",
		"hoverContext.brushing",
		"function renderChartZoomPill()",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("chart brush zoom source missing %q", want)
		}
	}

	css, ok, err := ReadFrontendAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.css asset missing")
	}
	if !strings.Contains(string(css.Content), ".chart-brush-rect {") {
		t.Fatal("chart brush selection rect CSS missing")
	}
}

func TestOutputMediaPanelIsFirstClassSummarySurface(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	mediaPanel := dashboardSectionSourceIndex(source, "media")
	evidencePanel := dashboardSectionSourceIndex(source, "evidence")
	if mediaPanel < 0 || evidencePanel < 0 || mediaPanel > evidencePanel {
		t.Fatalf("Output media panel should render before Evidence: media=%d evidence=%d", mediaPanel, evidencePanel)
	}
	if strings.Contains(source, "renderResearchOutputPanel(snapshot)") || strings.Contains(source, `panel("Research outputs"`) {
		t.Fatalf("Research outputs should not be a standalone dashboard panel")
	}
	for _, want := range []string{
		"artifact-derived compatibility view",
		"selection.steps.length >= 2",
		"step metadata pending",
		"function isOutputMediaArtifact",
		`configuredPanel("media"`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("Output media source missing %q", want)
		}
	}
}

func TestMainDashboardKeepsScalarLineChartsFirstClass(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	linePanel := dashboardSectionSourceIndex(source, "timeline")
	mediaPanel := dashboardSectionSourceIndex(source, "media")
	evidencePanel := dashboardSectionSourceIndex(source, "evidence")
	if linePanel < 0 || mediaPanel < 0 || evidencePanel < 0 {
		t.Fatalf("dashboard panels missing: line=%d media=%d evidence=%d", linePanel, mediaPanel, evidencePanel)
	}
	if linePanel > mediaPanel || linePanel > evidencePanel {
		t.Fatalf("scalar line chart panel should stay first-class before media/evidence panels: line=%d media=%d evidence=%d", linePanel, mediaPanel, evidencePanel)
	}
	for _, want := range []string{
		`configuredPanel("timeline"`,
		"renderMetricChart(chart, {",
		`className: "selected-line-chart"`,
		"renderFocusedSeriesControls",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("line chart source missing %q", want)
		}
	}
}

func TestFrontendIncludesRunLifecycleSearchControls(t *testing.T) {
	jsAsset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	jsSource := string(jsAsset.Content)
	for _, want := range []string{
		"lifecycleFilter",
		"runUpdatedFilter",
		"runUpdatedSort",
		"run-filter-chip",
		"run-updated-controls",
		"Filter runs by last updated time",
		"Sort runs by last updated time",
		"runLastUpdatedLabel",
		"lifecycle_state",
		"updated_at",
		"metric_names",
		"run-status-badge",
		"Run lifecycle filters",
	} {
		if !strings.Contains(jsSource, want) {
			t.Fatalf("run lifecycle/search UI source missing %q", want)
		}
	}

	cssAsset, ok, err := ReadFrontendAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.css asset missing")
	}
	cssSource := string(cssAsset.Content)
	for _, want := range []string{
		".run-filter-chip",
		".run-updated-controls",
		".run-status-badge.succeeded",
		".run-status-badge.failed",
		".run-status-badge.incomplete",
	} {
		if !strings.Contains(cssSource, want) {
			t.Fatalf("run lifecycle/search CSS missing %q", want)
		}
	}
}

func TestFrontendIncludesExperimentFirstNavigation(t *testing.T) {
	jsAsset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	jsSource := string(jsAsset.Content)
	for _, want := range []string{
		"experimentsPath",
		"workspace: root?.dataset.workspace",
		"experimentSearch",
		"renderExperimentSearchBar",
		"renderExperimentRail",
		"selectExperiment",
		"More experiments",
		"data-experiment-search-input",
		`return "Recent experiments";`,
		`url.searchParams.set("workspace", workspace)`,
	} {
		if !strings.Contains(jsSource, want) {
			t.Fatalf("experiment-first frontend source missing %q", want)
		}
	}
	if strings.Contains(jsSource, `url.searchParams.set("lifecycle", "running")`) {
		t.Fatal("empty experiment search should show recent experiments, not only running experiments")
	}

	cssAsset, ok, err := ReadFrontendAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.css asset missing")
	}
	cssSource := string(cssAsset.Content)
	for _, want := range []string{
		".experiment-search",
		".experiment-rail",
		".experiment-row.selected",
		".experiment-more summary",
		".meta-pill.warning",
	} {
		if !strings.Contains(cssSource, want) {
			t.Fatalf("experiment-first frontend CSS missing %q", want)
		}
	}
}

func TestFrontendKeepsHalfWidthDesktopDashboardVisible(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.css asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		".workspace {\n  display: grid;\n  grid-template-columns: clamp(224px, 22vw, 264px) minmax(0, 1fr);",
		".app-topbar {\n    grid-template-columns: minmax(0, 1fr) auto;",
		".output-media-grid,\n  .research-output-grid {",
		".output-media-hero {\n    grid-template-columns: minmax(0, 1fr);",
		"@media (max-width: 640px) {",
		".run-list {\n    max-height: 190px;",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("half-width responsive CSS missing %q", want)
		}
	}

	medium := source[strings.Index(source, "@media (max-width: 1320px)"):]
	if idx := strings.Index(medium, "@media (max-width: 820px)"); idx >= 0 {
		medium = medium[:idx]
	}
	for _, blocked := range []string{
		".workspace {\n    grid-template-columns: minmax(0, 1fr);",
		".variables-rail {\n    position: relative;",
	} {
		if strings.Contains(medium, blocked) {
			t.Fatalf("half-width desktop breakpoint should not collapse the dashboard rail: found %q", blocked)
		}
	}
}

func TestFrontendIncludesScopedRouteHistorySupport(t *testing.T) {
	jsAsset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	jsSource := string(jsAsset.Content)
	for _, want := range []string{
		`function updateURL(options = {})`,
		`function writeURL(url, historyMode = "replace")`,
		`window.history.pushState`,
		`updateURL({ history: "push" })`,
		`function restoreRouteFromLocation()`,
		`window.addEventListener("popstate"`,
		`dashboardSectionsFromURL(url, nextTarget)`,
	} {
		if !strings.Contains(jsSource, want) {
			t.Fatalf("route history source missing %q", want)
		}
	}
}

func TestFrontendExposesVisualReadinessState(t *testing.T) {
	jsAsset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(jsAsset.Content)
	for _, want := range []string{
		`publishVisualState("booting")`,
		`function publishVisualState(status, detail = {})`,
		`window.__stellarVisual = {`,
		`schema_version: "tau.stellar.visual_state.v0"`,
		`publishVisualState("loading")`,
		`publishVisualState("error", { error: error.message || String(error) })`,
		`publishVisualState("ready", {`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("visual readiness source missing %q", want)
		}
	}
}

func TestGraphChartsReserveBottomLabelSpace(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.css asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		".metric-card.large {\n  min-height: 430px;",
		".metric-card-density {\n  display: grid;\n  grid-template-columns: minmax(0, 1fr) auto;",
		".metric-card-density.optimized b {",
		".metric-chart.full {\n  display: grid;\n  gap: 12px;\n  min-height: 0;",
		".metric-chart.full .stellar-line-chart {\n  height: 320px;",
		".metric-chart.full.dashboard-medium-chart .stellar-line-chart {\n  height: 210px;",
		".metric-chart.compact .stellar-line-chart {\n  height: 116px;",
		".selected-line-chart .stellar-line-chart {\n  height: 300px;",
		".chart-legend {\n  display: flex;\n  flex-wrap: wrap;",
		".chart-legend-more {",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("chart spacing CSS missing %q", want)
		}
	}
	jsAsset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	jsSource := string(jsAsset.Content)
	for _, want := range []string{
		"points shown",
		"Showing density-sampled points for speed; raw history is preserved.",
		`h("b", {}, "Sampled view")`,
		`h("b", {}, "All points")`,
		"function filteredRunIDs(snapshot, options = {})",
		"function activeRunFilterLabels(snapshot)",
		"Charts, runs, and evidence share these filters",
		"chartDataset(chart, { runIDs:",
		"const hydratedSnapshot = snapshotWithSummaryDefaults(snapshot, summary)",
		"cache.set(loadedMetric, hydratedSnapshot)",
		"function runtimeDiffsForVisibleRuns(runs)",
		"renderRuntimeDiffSection(detailSnapshot, runs)",
	} {
		if !strings.Contains(jsSource, want) {
			t.Fatalf("chart density copy missing %q", want)
		}
	}
}

func TestResearcherDashboardPresetRendersGraphFirst(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	charts := dashboardSectionSourceIndex(source, "charts")
	timeline := dashboardSectionSourceIndex(source, "timeline")
	mediaPanel := dashboardSectionSourceIndex(source, "media")
	errors := dashboardSectionSourceIndex(source, "errors")
	labels := dashboardSectionSourceIndex(source, "labels")
	repro := dashboardSectionSourceIndex(source, "repro")
	metricCatalog := dashboardSectionSourceIndex(source, "catalog")
	evidencePanel := dashboardSectionSourceIndex(source, "evidence")
	if charts < 0 || timeline < 0 || mediaPanel < 0 || errors < 0 || labels < 0 || repro < 0 || metricCatalog < 0 || evidencePanel < 0 {
		t.Fatalf("dashboard section registry missing graph-first entries: charts=%d timeline=%d media=%d errors=%d labels=%d repro=%d catalog=%d evidence=%d", charts, timeline, mediaPanel, errors, labels, repro, metricCatalog, evidencePanel)
	}
	if !(charts < timeline && timeline < mediaPanel && mediaPanel < errors && errors < labels && labels < repro && repro < metricCatalog && metricCatalog < evidencePanel) {
		t.Fatalf("graph-first dashboard should default to chart grid first, then focused chart, media, support, then catalog/runs/evidence: charts=%d timeline=%d media=%d errors=%d labels=%d repro=%d catalog=%d evidence=%d", charts, timeline, mediaPanel, errors, labels, repro, metricCatalog, evidencePanel)
	}
	for _, want := range []string{
		`defaultTitle: "Scalar chart grid"`,
		`defaultTitle: "Metric catalog"`,
		`defaultTitle: "Error analysis"`,
		`defaultTitle: "Reproducibility / evidence"`,
		"renderDashboardSections(snapshot)",
		"visibleDashboardSectionIDs()",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("researcher dashboard source missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"...researcherPreset.compactHeader",
		`panel("Run summary / decision strip"`,
		"renderRunDecisionPanel",
		"Researcher run summary",
		"renderResearcherDashboardPreset",
	} {
		if strings.Contains(source, unwanted) {
			t.Fatalf("graph-first dashboard should not render Run summary surface; found %q", unwanted)
		}
	}
}

func TestDashboardSectionsAreCustomerConfigurable(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"const dashboardSectionCatalog = [",
		"const dashboardSectionStoragePrefix",
		"function initialDashboardSections()",
		"function renderDashboardSectionControls()",
		"function toggleDashboardSection(id)",
		"function moveDashboardSection(id, delta)",
		"function updateDashboardSectionTitle(id, value)",
		"function resetDashboardSections()",
		"function writeDashboardSectionURL(url)",
		`url.searchParams.set("sections", visibleDashboardSectionIDs().join(","));`,
		"`section.${section.id}.title`",
		"`section.${section.id}.subtitle`",
		"function renderControlsDrawer(snapshot, metricSelect, groupSelect)",
		`h("p", { class: "control-group-title" }, "Customize layout")`,
		`configuredPanel("media"`,
		`{ hideTitlebar: true }`,
		`panel-titlebar-hidden`,
		`sectionTitle: title`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("customer-configurable section source missing %q", want)
		}
	}
}

func TestStellarAutoRefreshFrontendSource(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		`refreshInterval: root?.dataset.refreshInterval || root?.dataset.autoRefreshInterval || ""`,
		"const defaultAutoRefreshIntervalMs = 30000;",
		"function parseAutoRefreshInterval()",
		"function startAutoRefresh()",
		`document.addEventListener("visibilitychange", handleAutoRefreshVisibilityChange);`,
		"window.setTimeout(() => {",
		"if (state.autoRefresh.inFlight || state.focusedSeriesLoading || state.fullSnapshotLoading) {",
		"if (!options.manual && dashboardHasActiveControl()) {",
		"await fetchSnapshot({ autoRefresh: true, silent: true, render: false, loadAutoSeriesDetail: false });",
		"await refreshFocusedSeriesAfterSnapshot();",
		"includeStatic: !autoRefresh && !highMetricCatalog",
		"renderRefreshStatus()",
		"async function bootStellar()",
		"await fetchSnapshot();",
		"startAutoRefresh();",
		"bootStellar().catch(renderError);",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("auto-refresh frontend source missing %q", want)
		}
	}
	boot := "bootStellar().catch(renderError);"
	if strings.Count(source, boot) != 1 || !strings.HasSuffix(strings.TrimSpace(source), boot) {
		t.Fatalf("auto-refresh boot must run exactly once as the final frontend statement")
	}
}

func TestStellarLandingFrontendSource(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"function renderLanding(options = {})",
		"function renderLandingProjectSelect(projectOptions, totalCount)",
		"function renderLandingTagFilter()",
		"function landingVisibleExperiments(experiments)",
		"function renderLandingExperimentCard(experiment)",
		`h("h1", {}, "Choose an experiment")`,
		`h("span", {}, "Project")`,
		`h("span", {}, "Tag")`,
		`url.searchParams.set("project", project);`,
		`url.searchParams.append("tag", tag);`,
		`setOptionalSearchParam(url, "experiment_tag", state.landingTagFilter);`,
		"state.landingProjectFilter = event.target.value;",
		"state.landingTagFilter = event.target.value;",
		`state.landingTagFilter = text(url.searchParams.get("experiment_tag"), "");`,
		"const fromLanding = !state.target;",
		"state.selectedMetrics = [];",
		"if (!state.target) {",
		`url.searchParams.delete("target");`,
		`url.searchParams.delete("pinned");`,
		"  fetchSnapshot().catch(renderError);\n  startAutoRefresh();",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("landing frontend source missing %q", want)
		}
	}
	if strings.Contains(source, "state.target = state.experiments[0].experiment_id") {
		t.Fatal("landing should not auto-select the first experiment")
	}
	if strings.Contains(source, "landing-metric-tags") || strings.Contains(source, "landing-card-kicker") {
		t.Fatal("landing experiment rows should stay compact and avoid bulky card metadata blocks")
	}
	if strings.Contains(source, "${experiment.project || experiment.source") {
		t.Fatal("landing experiment rows should not repeat project names already shown by group headers")
	}
}

func TestStellarTopbarReturnToLandingFrontendSource(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"function returnToLanding()",
		"returnToLanding().catch(renderError);",
		"renderSourceStatus()",
		`class: "topbar-home",`,
		"stopAutoRefresh();",
		"state.autoRefresh.started = false;",
		`state.target = "";`,
		"renderLanding();",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("topbar return-to-landing frontend source missing %q", want)
		}
	}
	topbar := source[strings.Index(source, "function renderTopbar("):]
	topbar = topbar[:strings.Index(topbar, "\nfunction ")]
	if !strings.Contains(topbar, "returnToLanding().catch(renderError);") {
		t.Fatal("renderTopbar must wire the return-to-landing click handler")
	}
	home := source[strings.Index(source, "function returnToLanding()"):]
	if idx := strings.Index(home, "\nfunction selectExperiment("); idx >= 0 {
		home = home[:idx]
	}
	if !strings.Contains(home, "stopAutoRefresh();") {
		t.Fatal("returnToLanding must stop the auto-refresh loop")
	}
	if !strings.Contains(home, "state.autoRefresh.started = false;") {
		t.Fatal("returnToLanding must reset autoRefresh.started so the loop can restart")
	}
	for _, forbidden := range []string{
		"/api/stellar/labels",
		"/api/stellar/dashboards",
		"renderOverlayStoreStatus",
		"overlayStoreStatus",
		"missing overlays",
		"saved dashboard",
		"Relabel run",
	} {
		if strings.Contains(strings.ToLower(source), strings.ToLower(forbidden)) {
			t.Fatalf("read-only frontend must not reference mutable state %q", forbidden)
		}
	}
}

func TestStellarFrontendIncludesRefreshIntervalDataAttribute(t *testing.T) {
	html, err := RenderFrontendHTML(FrontendOptions{
		Target:          "live-target",
		Workspace:       "sample",
		Source:          "kusto",
		RefreshInterval: "15000",
	})
	if err != nil {
		t.Fatal(err)
	}
	source := string(html)
	for _, want := range []string{
		`data-refresh-interval="15000"`,
		`data-workspace="sample"`,
		`data-source="kusto"`,
		`<b>Kusto/ADX</b> source`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("Stellar frontend HTML missing %q:\n%s", want, source)
		}
	}
	for _, forbidden := range []string{"overlay", "/api/stellar/labels", "/api/stellar/dashboards", "saved dashboard"} {
		if strings.Contains(strings.ToLower(source), strings.ToLower(forbidden)) {
			t.Fatalf("Stellar frontend HTML must not advertise mutable state %q:\n%s", forbidden, html)
		}
	}
}

func TestStellarFrontendShellReservesDashboardLayoutDuringLoad(t *testing.T) {
	html, err := RenderFrontendHTML(FrontendOptions{
		Target: "stellar-render-perf",
	})
	if err != nil {
		t.Fatal(err)
	}
	source := string(html)
	for _, want := range []string{
		`<style>`,
		`rel="preload"`,
		`as="style"`,
		`onload="this.onload=null;this.rel='stylesheet'"`,
		`/stellar/assets/app.css?v=`,
		`/stellar/assets/app.js?v=`,
		`id="stellar-root"`,
		`class="stellar-app"`,
		`stellar-loading-shell`,
		`class="workspace"`,
		`class="variables-rail"`,
		`class="report-canvas"`,
		`class="panel-grid"`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("frontend shell should reserve dashboard layout and use versioned async assets; missing %q\n%s", want, source)
		}
	}
}

func TestFrontendLoadingRendererUsesReservedDashboardShell(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		`function renderLoadingShell(target, detail)`,
		`root.className = "stellar-app"`,
		`class: "app-shell stellar-loading-shell"`,
		`class: "variables-rail"`,
		`class: "panel-grid"`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("loading renderer should keep the dashboard layout stable; missing %q", want)
		}
	}
}

func TestResearcherDashboardPresetPromotesVisionMetricsWithoutPinning(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"const maxPinnedMetrics = 14;",
		"const maxResearchPresetMetrics = 32;",
		"graphDefaultMetricOrder",
		"presetMetricSnapshots: new Map()",
		"state.presetMetricSnapshots.has(metricName)",
		"researcherPresetMetricNames(primary)",
		"headlinePrimaryMetricOrder",
		"evalCurveMetricName(snapshot, primaryMetric)",
		`"eval/macro_auprc"`,
		`"final/macro_auprc"`,
		`"detect/macro_auprc"`,
		`"eval/macro_auroc"`,
		`"final/macro_auroc"`,
		`"detect/macro_auroc"`,
		`"eval/macro_f1"`,
		`"final/macro_f1"`,
		`"eval/brier"`,
		`"final/ece"`,
		`"detect/macro_sensitivity"`,
		`"detect/macro_specificity"`,
		`"detect/macro_precision"`,
		`"detect/macro_f1"`,
		`"detect/macro_accuracy"`,
		`"detect/accuracy"`,
		`["atelectasis", "Atelectasis"]`,
		`["pleural_effusion", "Pleural Effusion"]`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("researcher preset source missing %q", want)
		}
	}
	if strings.Contains(source, "state.selectedMetrics = normalizeMetricList([...selectedMetricNames") {
		t.Fatalf("hidden researcher preset metrics should not be written into selected/pinned metrics")
	}
}

func TestResearcherDashboardPresetPromotesGenericOutcomeMetrics(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		`"final/score"`,
		`"eval/score"`,
		`"final/mean_episode_return"`,
		`"eval/mean_episode_return"`,
		`"final/reward"`,
		`"eval/reward"`,
		`"final/return"`,
		`"eval/return"`,
		`"final/success_rate"`,
		`"eval/success_rate"`,
		`"final/win_rate"`,
		`"eval/win_rate"`,
		`"final/pass_rate"`,
		`"eval/pass_rate"`,
		`"eval/aime2025/pass@1"`,
		`"final/exact_match"`,
		`"eval/exact_match"`,
		`"final/accuracy"`,
		`"eval/accuracy"`,
		`return /(auprc|score|reward|return|success_rate|win_rate|pass@1|pass_rate|exact_match|accuracy|macro_f1|\/f1|auroc|auc)/.test(name);`,
		`return /loss|perplexity|cross_entropy|negative_log_likelihood|nll|brier|ece|error|invalid|skipped/i.test(metricName);`,
		`return "Outcome";`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generic dashboard preset source missing %q", want)
		}
	}
}

func TestResearcherDashboardPresetPrefersFinalHeadlineAndSeparatesEvalCurve(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	orderStart := strings.Index(source, "const headlinePrimaryMetricOrder = [")
	orderEnd := strings.Index(source, "const evalCurveMetricOrder = [")
	if orderStart < 0 || orderEnd < 0 || orderStart >= orderEnd {
		t.Fatalf("headline primary order block missing")
	}
	headlineOrder := source[orderStart:orderEnd]
	finalIdx := strings.Index(headlineOrder, `"final/macro_auprc"`)
	detectIdx := strings.Index(headlineOrder, `"detect/macro_auprc"`)
	evalIdx := strings.Index(headlineOrder, `"eval/macro_auprc"`)
	if finalIdx < 0 || detectIdx < 0 || evalIdx < 0 || !(finalIdx < detectIdx && detectIdx < evalIdx) {
		t.Fatalf("headline primary order should prefer final, then detect, then eval AUPRC: final=%d detect=%d eval=%d", finalIdx, detectIdx, evalIdx)
	}
	for _, want := range []string{
		`function primaryMetricName(snapshot)`,
		`function evalCurveMetricName(snapshot, primaryMetric)`,
		`const evalCurve = evalCurveMetricName(snapshot, primary);`,
		`names.push(evalCurve);`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("final/eval preset metric source missing %q", want)
		}
	}
}

func TestGraphFirstDefaultsPrioritizeLargeTrainingValidationDetectionCharts(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	orderStart := strings.Index(source, "const graphDefaultMetricOrder = [")
	orderEnd := strings.Index(source, "function text(value")
	if orderStart < 0 || orderEnd < 0 || orderStart >= orderEnd {
		t.Fatalf("graph default metric order block missing")
	}
	graphOrder := source[orderStart:orderEnd]
	trainIdx := strings.Index(graphOrder, `"train/loss"`)
	evalIdx := strings.Index(graphOrder, `"eval/macro_auprc"`)
	detectIdx := strings.Index(graphOrder, `"detect/macro_auprc"`)
	accuracyIdx := strings.Index(graphOrder, `"detect/macro_accuracy"`)
	if trainIdx < 0 || evalIdx < 0 || detectIdx < 0 || accuracyIdx < 0 || !(trainIdx < evalIdx && evalIdx < detectIdx && detectIdx < accuracyIdx) {
		t.Fatalf("graph defaults should start with train/loss, then validation, then detection including accuracy: train=%d eval=%d detect=%d accuracy=%d", trainIdx, evalIdx, detectIdx, accuracyIdx)
	}
	for _, want := range []string{
		`uniform: true,`,
		`"metric-card-grid graph-card-grid uniform-chart-grid"`,
		`className: options.uniform ? "dashboard-medium-chart" : options.large ? "dashboard-large-chart" : "dashboard-mini-chart"`,
		`showLegend: fullChart`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("graph-first chart source missing %q", want)
		}
	}
}

func TestResearcherDashboardPresetSurfacesDetectionSummaryMetrics(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"const detectionSummaryMetricOrder = [",
		`"detect/macro_auprc"`,
		`"detect/macro_auroc"`,
		`"detect/macro_sensitivity"`,
		`"detect/macro_specificity"`,
		`"detect/macro_precision"`,
		`"detect/macro_f1"`,
		`"detect/macro_accuracy"`,
		`h("h3", {}, "Detection summary")`,
		`summaryMetrics.map((metricName) => renderResearchMetricTile(metricName, { mode: "latest", badge: "Detection" }))`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("detection summary source missing %q", want)
		}
	}
}

func TestResearcherDashboardPresetSkipsErrorPanelWithoutErrorSignals(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		`const predictionState = renderPredictionSummaryState(snapshot);`,
		`if (!summaryMetrics.length && !metrics.length && !predictionState) {`,
		`return null;`,
		`No dedicated error-analysis scalars were found.`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generic error-analysis gating source missing %q", want)
		}
	}
}

func TestResearcherDashboardPresetKeepsValidationAndErrorFamiliesDisjoint(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	validationStart := strings.Index(source, "function isValidationQualityMetric")
	detectionStart := strings.Index(source, "function isDetectionQualityMetric")
	lossStart := strings.Index(source, "function isLossLikeMetric")
	if validationStart < 0 || detectionStart < 0 || lossStart < 0 || validationStart >= detectionStart || detectionStart >= lossStart {
		t.Fatalf("could not locate researcher metric classifier functions")
	}
	validationClassifier := source[validationStart:detectionStart]
	detectionClassifier := source[detectionStart:lossStart]
	for _, unwanted := range []string{"precision", "recall", "sensitivity", "specificity"} {
		if strings.Contains(validationClassifier, unwanted) {
			t.Fatalf("%s should be owned by error analysis, not validation quality", unwanted)
		}
	}
	for _, want := range []string{"brier", "ece", "calibration"} {
		if !strings.Contains(validationClassifier, want) {
			t.Fatalf("%s should remain in validation quality", want)
		}
	}
	if !strings.Contains(detectionClassifier, `if (/^(eval|final)\//.test(name)) {`) ||
		!strings.Contains(detectionClassifier, "return errorTerms.test(name);") {
		t.Fatalf("eval/final error-analysis metrics should use the narrower error term classifier")
	}
}

func TestResearcherDashboardPresetShowsHonestEmptyStates(t *testing.T) {
	asset, ok, err := ReadFrontendAsset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("app.js asset missing")
	}
	source := string(asset.Content)
	for _, want := range []string{
		"Prediction summary details are deferred",
		"Needs prediction summary import",
		"Stellar will not pretend those analyses exist from scalar metrics alone.",
		"Reproducibility evidence is deferred",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("researcher preset empty-state source missing %q", want)
		}
	}
}

func TestBuildChartKeepsEachSeriesWithinViewportBudgetAcrossManyRuns(t *testing.T) {
	points := make([]metricPoint, 0, 50*1000)
	for run := 0; run < 50; run++ {
		runID := fmt.Sprintf("seed-%02d", run)
		for step := 0; step < 1000; step++ {
			points = append(points, metricPoint{
				RunID:      runID,
				RunGroupID: "reference-group",
				MetricName: "train/loss",
				Card:       "Optimization",
				Step:       int64(step),
				Value:      float64((run + step) % 997),
			})
		}
	}

	chart := buildChart(points, "train/loss", map[string]string{"reference-group": "group-a"})
	for _, series := range chart.Series {
		if series.RenderedPoints > chartMaxRenderedPoints {
			t.Fatalf("run %s rendered %d points, want <= per-series budget %d", series.RunID, series.RenderedPoints, chartMaxRenderedPoints)
		}
	}
}

func TestSampleMetricPointsByDensitySupportsSinglePointBudget(t *testing.T) {
	points := []metricPoint{
		{Step: 1, Value: 10},
		{Step: 2, Value: 20},
		{Step: 3, Value: 30},
	}

	got := sampleMetricPointsByDensity(points, 1)

	if len(got) != 1 || got[0].Step != 3 {
		t.Fatalf("single-point budget should keep latest point, got %+v", got)
	}
}

func TestSampleMetricPointsByDensityUsesExtremaPreservingSamples(t *testing.T) {
	points := make([]metricPoint, 0, 10)
	for step := 0; step < 10; step++ {
		points = append(points, metricPoint{Step: int64(step), Value: float64((step % 3) * 100)})
	}

	got := sampleMetricPointsByDensity(points, 5)

	steps := make([]int64, 0, len(got))
	hasMaximum := false
	for _, point := range got {
		steps = append(steps, point.Step)
		if point.Value == 200 {
			hasMaximum = true
		}
	}
	if len(got) != 5 || steps[0] != 0 || steps[len(steps)-1] != 9 || !hasMaximum {
		t.Fatalf("extrema-preserving sampling steps=%v values=%+v", steps, got)
	}
}

func TestSampleMetricPointsByStepIntervalPreservesEndpoints(t *testing.T) {
	points := make([]metricPoint, 0, 2000)
	for step := 0; step < 2000; step++ {
		points = append(points, metricPoint{Step: int64(step), Value: float64(step)})
	}

	got := sampleMetricPointsByStepInterval(points, 20)

	if len(got) != 101 {
		t.Fatalf("sampled points=%d, want 101", len(got))
	}
	if got[0].Step != 0 || got[len(got)-1].Step != 1999 {
		t.Fatalf("sampled interval should preserve endpoints: first=%+v last=%+v", got[0], got[len(got)-1])
	}
	for _, point := range got[1 : len(got)-1] {
		if point.Step%20 != 0 {
			t.Fatalf("interior point should be every 20 steps, got %+v", point)
		}
	}
}

func TestBuildChartWithStepIntervalKeepsRawPointCount(t *testing.T) {
	points := make([]metricPoint, 0, 2000)
	for step := 0; step < 2000; step++ {
		points = append(points, metricPoint{
			RunID:      "run-a",
			RunGroupID: "group-a",
			MetricName: "train/loss",
			Step:       int64(step),
			Value:      float64(step),
		})
	}

	chart := buildChartWithRunColorsBudgetAndInterval(points, "train/loss", map[string]string{"group-a": "group-a"}, nil, 8000, 50)

	if !chart.HasData || len(chart.Series) != 1 {
		t.Fatalf("unexpected chart: %+v", chart)
	}
	series := chart.Series[0]
	if series.PointCount != 2000 {
		t.Fatalf("raw point count=%d, want 2000", series.PointCount)
	}
	if series.RenderedPoints != 41 || len(series.Values) != 41 {
		t.Fatalf("rendered interval points=%d values=%d, want 41", series.RenderedPoints, len(series.Values))
	}
	if series.Values[0].Step != 0 || series.Values[len(series.Values)-1].Step != 1999 {
		t.Fatalf("interval chart should preserve endpoints: first=%+v last=%+v", series.Values[0], series.Values[len(series.Values)-1])
	}
}

func TestBuildChartColorsRunsWithinSameGroup(t *testing.T) {
	points := []metricPoint{
		{RunID: "vit-enc-sweep-06021912-frozen-lr1e3", RunGroupID: "vit-enc-sweep", MetricName: "train/loss", Step: 1, Value: 0.9},
		{RunID: "vit-enc-sweep-06021912-frozen-lr1e3", RunGroupID: "vit-enc-sweep", MetricName: "eval/auroc", Step: 1, Value: 0.71},
		{RunID: "vit-enc-sweep-06021912-frozen-lr1e4", RunGroupID: "vit-enc-sweep", MetricName: "train/loss", Step: 1, Value: 0.8},
		{RunID: "vit-enc-sweep-06021912-frozen-lr1e4", RunGroupID: "vit-enc-sweep", MetricName: "eval/auroc", Step: 1, Value: 0.74},
		{RunID: "vit-enc-sweep-06021912-ft-lr1e5", RunGroupID: "vit-enc-sweep", MetricName: "train/loss", Step: 1, Value: 0.7},
		{RunID: "vit-enc-sweep-06021912-ft-lr5e5", RunGroupID: "vit-enc-sweep", MetricName: "train/loss", Step: 1, Value: 0.6},
	}
	groupClasses := map[string]string{"vit-enc-sweep": "group-vit-enc-sweep"}

	trainChart := buildChart(points, "train/loss", groupClasses)
	if !trainChart.HasData || len(trainChart.Series) != 4 {
		t.Fatalf("unexpected train chart: %+v", trainChart)
	}
	colors := map[string]string{}
	for _, series := range trainChart.Series {
		if series.RunGroupID != "vit-enc-sweep" || series.GroupClass != "group-vit-enc-sweep" {
			t.Fatalf("series lost group metadata: %+v", series)
		}
		if series.Color == "" {
			t.Fatalf("series missing color: %+v", series)
		}
		if otherRun, exists := colors[series.Color]; exists {
			t.Fatalf("runs %s and %s share color %s", otherRun, series.RunID, series.Color)
		}
		colors[series.Color] = series.RunID
	}

	evalChart := buildChart(points, "eval/auroc", groupClasses)
	if !evalChart.HasData || len(evalChart.Series) != 2 {
		t.Fatalf("unexpected eval chart: %+v", evalChart)
	}
	trainColorsByRun := colorsByRun(trainChart.Series)
	for _, series := range evalChart.Series {
		if series.Color != trainColorsByRun[series.RunID] {
			t.Fatalf("run %s color changed across metrics: train=%s eval=%s", series.RunID, trainColorsByRun[series.RunID], series.Color)
		}
	}
}

func TestKustoSourceBuildsDashboardSnapshot(t *testing.T) {
	now := time.Date(2026, 5, 21, 0, 20, 0, 0, time.UTC)
	rows := []KustoMetricRow{
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-1", MetricName: "train/return", Step: 1, WallTime: "2026-05-21T00:00:00Z", Value: 10, Source: "wandb"},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-1", MetricName: "train/return", Step: 2, WallTime: "2026-05-21T00:01:00Z", Value: 20, Source: "wandb"},
		{WorkspaceID: "sample", Cluster: "sample-cluster", Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-1", MetricName: expkusto.RunStatusMetricName, Step: 0, WallTime: "2026-05-21T00:02:00Z", Value: 1, Source: "stellar-online-status", Tags: `{"tau.status.state":"succeeded","tau.status.artifact_uri":"az://results/seed-1"}`},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-2", MetricName: "train/return", Step: 1, WallTime: "2026-05-21T00:19:30Z", Value: 15, Source: "wandb"},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-2", MetricName: "eval/score", Step: 1, WallTime: "2026-05-21T00:19:30Z", Value: 7, Source: "wandb"},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-3", MetricName: "train/return", Step: 1, WallTime: "2026-05-21T00:00:30Z", Value: 12, Source: "wandb"},
	}

	snapshot, err := (KustoSource{Metrics: rows, Now: func() time.Time { return now }}).BuildSnapshot(context.Background(), Options{
		Target:        "sample-project-wandb-migration",
		Metric:        "train/return",
		MaxRuns:       10,
		MaxMetricRows: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TargetType != "experiment" || snapshot.Status.Runs != 3 || len(snapshot.Cards) == 0 {
		t.Fatalf("unexpected Kusto snapshot: %+v", snapshot)
	}
	if !snapshot.Chart.HasData || len(snapshot.Chart.Series) != 3 || snapshot.Chart.Series[0].Overlay.Source != "kusto" {
		t.Fatalf("unexpected Kusto chart: %+v", snapshot.Chart)
	}
	if snapshot.Chart.Series[0].Color == "" || snapshot.Chart.Series[0].Color == snapshot.Chart.Series[1].Color {
		t.Fatalf("expected distinct Kusto chart colors per run: %+v", snapshot.Chart.Series)
	}
	if snapshot.Status.LifecycleCounts["succeeded"] != 1 || snapshot.Status.LifecycleCounts["running"] != 1 || snapshot.Status.LifecycleCounts["stale"] != 1 {
		t.Fatalf("unexpected Kusto lifecycle counts: %+v", snapshot.Status.LifecycleCounts)
	}
	if snapshot.MetricOptions[0].Name == expkusto.RunStatusMetricName {
		t.Fatalf("status marker should not be exposed as a scalar metric option: %+v", snapshot.MetricOptions)
	}
	if strings.Contains(strings.ToLower(strings.Join(snapshot.Warnings, "\n")), "overlay") {
		t.Fatalf("Kusto snapshot must not advertise overlays: %+v", snapshot.Warnings)
	}
	if snapshot.Summary.SeedCoverage != "3 runs across 1 run groups (reference-group=3)" {
		t.Fatalf("unexpected seed coverage: %s", snapshot.Summary.SeedCoverage)
	}
	if snapshot.Runs[0].WorkspaceID != "sample" || snapshot.Runs[0].Cluster != "sample-cluster" || snapshot.Runs[0].ResultURI != "az://results/seed-1" {
		t.Fatalf("missing durable Kusto context/result evidence: %+v", snapshot.Runs[0])
	}
}

func TestKustoSourceLatestTerminalStatusWinsAfterRetry(t *testing.T) {
	rows := []KustoMetricRow{
		{Project: "pretraining", ExperimentID: "modernbert", RunGroupID: "fwe100", RunID: "bounded-retry", MetricName: "train/loss", Step: 1, WallTime: "2026-07-17T20:40:00Z", Value: 2.1},
		{Project: "pretraining", ExperimentID: "modernbert", RunGroupID: "fwe100", RunID: "bounded-retry", MetricName: expkusto.RunStatusMetricName, WallTime: "2026-07-17T20:45:00Z", Value: -1, Tags: `{"tau.status.state":"failed","tau.status.reason":"OOMKilled","tau_retry_attempt":"1"}`},
		{Project: "pretraining", ExperimentID: "modernbert", RunGroupID: "fwe100", RunID: "bounded-retry", MetricName: "train/loss", Step: 2, WallTime: "2026-07-17T20:50:00Z", Value: 1.7},
		{Project: "pretraining", ExperimentID: "modernbert", RunGroupID: "fwe100", RunID: "bounded-retry", MetricName: expkusto.RunStatusMetricName, WallTime: "2026-07-17T20:55:00Z", Value: 1, Tags: `{"tau.status.state":"succeeded","tau.status.reason":"job-entrypoint-exit","tau_retry_attempt":"2"}`},
	}
	snapshot, err := (KustoSource{Metrics: rows}).BuildSnapshot(context.Background(), Options{
		Target:        "modernbert",
		Metric:        "train/loss",
		MaxRuns:       10,
		MaxMetricRows: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 1 || snapshot.Runs[0].LifecycleState != "succeeded" || !snapshot.Runs[0].Successful {
		t.Fatalf("latest retry status was not selected: %+v", snapshot.Runs)
	}
	if snapshot.Status.LifecycleCounts["succeeded"] != 1 || snapshot.Status.LifecycleCounts["failed"] != 0 {
		t.Fatalf("unexpected retry lifecycle counts: %+v", snapshot.Status.LifecycleCounts)
	}
}

func TestKustoSourceRestrictsHostedDataToWorkspace(t *testing.T) {
	rows := []KustoMetricRow{
		{WorkspaceID: "sample", Project: "sample-project", ExperimentID: "shared-experiment", RunGroupID: "default", RunID: "sample-run", MetricName: "train/loss", Step: 1, WallTime: "2026-05-21T00:00:00Z", Value: 1},
		{Project: "sample-project", ExperimentID: "shared-experiment", RunGroupID: "default", RunID: "legacy-sample-run", MetricName: "train/loss", Step: 1, WallTime: "2026-05-21T00:01:00Z", Value: 2, Tags: `{"tau_workspace":"sample"}`},
		{WorkspaceID: "research", Project: "sample-project", ExperimentID: "shared-experiment", RunGroupID: "default", RunID: "research-run", MetricName: "train/loss", Step: 1, WallTime: "2026-05-21T00:02:00Z", Value: 3},
		{Project: "sample-project", ExperimentID: "shared-experiment", RunGroupID: "default", RunID: "unscoped-run", MetricName: "train/loss", Step: 1, WallTime: "2026-05-21T00:03:00Z", Value: 4},
	}
	source := KustoSource{Metrics: rows}

	snapshot, err := source.BuildSnapshot(context.Background(), Options{
		Target:        "shared-experiment",
		Workspace:     "sample",
		Metric:        "train/loss",
		MaxRuns:       10,
		MaxMetricRows: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status.Runs != 2 {
		t.Fatalf("workspace snapshot mixed scoped or legacy-unscoped runs: %+v", snapshot.Status)
	}

	result, err := source.SearchExperiments(context.Background(), expstore.ExperimentSearchOptions{Workspace: "sample", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 1 || result.Experiments[0].RunCount != 2 {
		t.Fatalf("workspace discovery mixed scoped or legacy-unscoped runs: %+v", result)
	}
	runs, err := source.SearchRuns(context.Background(), expstore.RunSearchOptions{
		Target:    "shared-experiment",
		Workspace: "sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 2 {
		t.Fatalf("workspace run search mixed scoped or legacy-unscoped runs: %+v", runs)
	}
}

func TestKustoSourceRejectsConflictingWorkspaceScope(t *testing.T) {
	source := KustoSource{
		WorkspaceID: "sample",
		Metrics: []KustoMetricRow{{
			WorkspaceID: "sample",
			Project:     "vision",
			RunGroupID:  "baseline",
			RunID:       "seed-1",
			MetricName:  "train/loss",
			Step:        1,
			WallTime:    "2026-07-16T00:00:00Z",
			Value:       1,
		}},
	}
	if _, err := source.SearchExperiments(context.Background(), expstore.ExperimentSearchOptions{Workspace: "research"}); err == nil {
		t.Fatal("request workspace replaced configured Kusto workspace boundary")
	}
	if _, err := source.BuildSnapshot(context.Background(), Options{Target: "experiment", Workspace: "research"}); err == nil {
		t.Fatal("snapshot workspace replaced configured Kusto workspace boundary")
	}
}

func TestLocalSnapshotAndSeriesRestrictRunsToWorkspace(t *testing.T) {
	ctx := context.Background()
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "workspace-dashboard",
		Project:     "tau",
		Description: "Can local dashboards enforce workspace scope?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, tc := range []struct {
		runID     string
		workspace string
	}{
		{runID: "sample-run", workspace: "sample"},
		{runID: "z-sample-run", workspace: "sample"},
		{runID: "research-run", workspace: "research"},
	} {
		if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
			Run: expstore.RunRecord{
				RunID:      tc.runID,
				Project:    "tau",
				RunGroupID: "baseline",
				State:      "running",
				CreatedAt:  "2026-07-16T00:00:00Z",
			},
			Tags: []expstore.TagRecord{{
				ScopeType: "run",
				ScopeID:   tc.runID,
				Key:       "tau_workspace",
				Value:     tc.workspace,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := BuildSnapshot(ctx, store, Options{
		Target:        "workspace-dashboard",
		Workspace:     "sample",
		MaxRuns:       10,
		MaxMetricRows: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 2 || snapshot.Runs[0].RunID != "sample-run" || snapshot.Status.Runs != 2 {
		t.Fatalf("workspace snapshot leaked runs: %+v status=%+v", snapshot.Runs, snapshot.Status)
	}
	if _, err := BuildSnapshot(ctx, store, Options{
		Target:        "workspace-dashboard",
		Workspace:     "sample'; DROP TABLE runs; --",
		MaxRuns:       10,
		MaxMetricRows: 100,
	}); !errors.Is(err, expstore.ErrNotFound) {
		t.Fatalf("injection-like workspace error = %v, want ErrNotFound", err)
	}
	snapshot, err = BuildSnapshot(ctx, store, Options{
		Target:        "workspace-dashboard",
		Workspace:     "sample",
		MaxRuns:       10,
		MaxMetricRows: 100,
	})
	if err != nil {
		t.Fatalf("clean workspace snapshot after injection-like input: %v", err)
	}
	if len(snapshot.Runs) != 2 || snapshot.Status.Runs != 2 {
		t.Fatalf("workspace snapshot changed after injection-like input: %+v status=%+v", snapshot.Runs, snapshot.Status)
	}
	merged, err := (MergedSource{
		Store: store,
		Kusto: KustoSource{Metrics: []KustoMetricRow{{
			WorkspaceID: "research",
			Project:     "tau",
			RunGroupID:  "baseline",
			RunID:       "foreign-kusto-run",
			MetricName:  "train/loss",
			Step:        1,
			WallTime:    "2026-07-16T00:00:00Z",
			Value:       1,
		}}},
	}).BuildSnapshot(ctx, Options{
		Target:        "workspace-dashboard",
		Workspace:     "sample",
		MaxRuns:       10,
		MaxMetricRows: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range merged.Runs {
		if run.RunID == "foreign-kusto-run" {
			t.Fatalf("merged snapshot admitted another workspace's Kusto run: %+v", merged.Runs)
		}
	}
	if _, err := (MergedSource{
		Store: store,
		Kusto: KustoSource{Metrics: []KustoMetricRow{{
			WorkspaceID: "research",
			Project:     "tau",
			RunGroupID:  "baseline",
			RunID:       "foreign-kusto-run",
			MetricName:  "train/loss",
			Step:        1,
			WallTime:    "2026-07-16T00:00:00Z",
			Value:       1,
		}}},
	}).BuildSnapshot(ctx, Options{
		Target:        "workspace-dashboard",
		Workspace:     "missing-workspace",
		MaxRuns:       10,
		MaxMetricRows: 100,
	}); !errors.Is(err, expstore.ErrNotFound) {
		t.Fatalf("empty merged workspace snapshot error = %v, want ErrNotFound", err)
	}

	_, err = BuildSeries(ctx, store, SeriesOptions{
		Target:        "workspace-dashboard",
		Workspace:     "sample",
		Metric:        "train/loss",
		RunID:         "research-run",
		MaxRuns:       10,
		MaxMetricRows: 100,
		MaxPoints:     100,
	})
	if !errors.Is(err, expstore.ErrNotFound) {
		t.Fatalf("cross-workspace series error = %v, want ErrNotFound", err)
	}
	if _, err := BuildSeries(ctx, store, SeriesOptions{
		Target:        "workspace-dashboard",
		Workspace:     "sample",
		Metric:        "train/loss",
		RunID:         "z-sample-run",
		MaxRuns:       1,
		MaxMetricRows: 100,
		MaxPoints:     100,
	}); err != nil {
		t.Fatalf("authorized run beyond dashboard max_runs was rejected: %v", err)
	}
}

func TestKustoSourceSearchesExperiments(t *testing.T) {
	now := time.Date(2026, 5, 21, 0, 20, 0, 0, time.UTC)
	rows := []KustoMetricRow{
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-1", MetricName: "train/return", Step: 1, WallTime: "2026-05-21T00:00:00Z", Value: 10, Tags: `{"suite":"migration"}`},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-1", MetricName: "train/return", Step: 2, WallTime: "2026-05-21T00:01:00Z", Value: 20, Tags: `{"suite":"migration"}`},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-1", MetricName: expkusto.RunStatusMetricName, Step: 0, WallTime: "2026-05-21T00:02:00Z", Value: 1, Tags: `{"tau.status.state":"succeeded"}`},
		{Project: "sample-project", ExperimentID: "sample-project-wandb-migration", RunGroupID: "reference-group", RunID: "seed-2", MetricName: "eval/score", Step: 1, WallTime: "2026-05-21T00:00:30Z", Value: 7, Tags: `{"suite":"migration"}`},
		{Project: "sample-project", ExperimentID: "other-experiment", RunGroupID: "control", RunID: "seed-3", MetricName: "train/return", Step: 1, WallTime: "2026-05-20T00:00:00Z", Value: 5},
	}

	result, err := (KustoSource{Metrics: rows, Now: func() time.Time { return now }}).SearchExperiments(context.Background(), expstore.ExperimentSearchOptions{
		Query:     "wandb",
		Tags:      map[string]string{"suite": "migration"},
		Lifecycle: "succeeded",
		MetricFilters: []expstore.MetricFilter{
			{MetricName: "train/return", Op: ">", Value: 15},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 1 {
		t.Fatalf("unexpected Kusto experiments: %+v", result)
	}
	experiment := result.Experiments[0]
	if experiment.ExperimentID != "sample-project-wandb-migration" || experiment.Source != "kusto" || experiment.RunCount != 2 || experiment.RunGroupCount != 1 {
		t.Fatalf("unexpected Kusto experiment summary: %+v", experiment)
	}
	if experiment.LifecycleCounts["succeeded"] != 1 || experiment.LifecycleCounts["stale"] != 1 || len(experiment.MetricNames) != 2 {
		t.Fatalf("unexpected Kusto lifecycle/metrics: %+v", experiment)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "source=kusto derives lifecycle") {
		t.Fatalf("missing lifecycle approximation warning: %+v", result.Warnings)
	}
}

func TestKustoSourceSearchExperimentsSpansProjectsByDefault(t *testing.T) {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	rows := []KustoMetricRow{
		{Project: "tau-submit", ExperimentID: "tau-submit", RunGroupID: "default", RunID: "seed-1", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T21:04:00Z", Value: 1.2},
		{Project: "vit-enc-vision", ExperimentID: "vision-vitenc-public-recipe", RunGroupID: "paper-param-pilot", RunID: "seed-2", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T17:46:59Z", Value: 0.8},
		{Project: "other-project", ExperimentID: "older-control", RunGroupID: "control", RunID: "seed-3", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-10T00:00:00Z", Value: 2.0},
	}
	source := KustoSource{
		Metrics: rows,
		Project: "tau-submit",
		Now:     func() time.Time { return now },
	}

	result, err := source.SearchExperiments(context.Background(), expstore.ExperimentSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 3 {
		t.Fatalf("default Kusto discovery should span projects, got %+v", result.Experiments)
	}
	if !kustoExperimentProjects(result.Experiments)["vit-enc-vision"] {
		t.Fatalf("default Kusto discovery omitted ViT-Enc project: %+v", result.Experiments)
	}

	filtered, err := source.SearchExperiments(context.Background(), expstore.ExperimentSearchOptions{Project: "vit-enc-vision"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Experiments) != 1 || filtered.Experiments[0].Project != "vit-enc-vision" {
		t.Fatalf("request project filter did not isolate ViT-Enc: %+v", filtered.Experiments)
	}
}

func TestKustoSourceSearchRunsHonorsAllowedProjectsForStaticRows(t *testing.T) {
	rows := []KustoMetricRow{
		{Project: "tau-submit", ExperimentID: "tau-submit", RunGroupID: "default", RunID: "seed-1", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T21:04:00Z", Value: 1.2},
		{Project: "vit-enc-vision", ExperimentID: "vision-vitenc-public-recipe", RunGroupID: "paper-param-pilot", RunID: "seed-2", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T17:46:59Z", Value: 0.8},
	}
	source := KustoSource{
		Metrics:         rows,
		AllowedProjects: []string{"vit-enc-vision"},
	}

	result, err := source.SearchRuns(context.Background(), expstore.RunSearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].Project != "vit-enc-vision" || result.Runs[0].RunID != "seed-2" {
		t.Fatalf("allowed project scope did not filter static Kusto run search: %+v", result.Runs)
	}

	blocked, err := source.SearchRuns(context.Background(), expstore.RunSearchOptions{Project: "tau-submit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked.Runs) != 0 {
		t.Fatalf("disallowed request project should not return static Kusto runs: %+v", blocked.Runs)
	}
}

func TestKustoSourceSnapshotRequiresProjectForAmbiguousTarget(t *testing.T) {
	rows := []KustoMetricRow{
		{Project: "tau-submit", ExperimentID: "shared-target", RunGroupID: "default", RunID: "seed-1", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T21:04:00Z", Value: 1.2},
		{Project: "vit-enc-vision", ExperimentID: "shared-target", RunGroupID: "paper-param-pilot", RunID: "seed-2", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T17:46:59Z", Value: 0.8},
	}
	source := KustoSource{Metrics: rows}

	_, err := source.BuildSnapshot(context.Background(), Options{Target: "shared-target", Metric: "pretrain/loss", MaxRuns: 10})
	if err == nil || !strings.Contains(err.Error(), "ambiguous Kusto target") || !strings.Contains(err.Error(), "project=") {
		t.Fatalf("expected project disambiguation error, got %v", err)
	}

	snapshot, err := source.BuildSnapshot(context.Background(), Options{Target: "shared-target", Project: "vit-enc-vision", Metric: "pretrain/loss", MaxRuns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Experiment == nil || snapshot.Experiment.Project != "vit-enc-vision" || len(snapshot.Runs) != 1 || snapshot.Runs[0].RunID != "seed-2" {
		t.Fatalf("project disambiguation returned wrong snapshot: %+v", snapshot)
	}
}

func TestKustoLookbackDurationAcceptsWeeks(t *testing.T) {
	got, err := parseKustoLookbackDuration("2w", time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got != 14*24*time.Hour {
		t.Fatalf("2w parsed as %s, want 336h", got)
	}
}

func kustoExperimentProjects(experiments []expstore.ExperimentSummary) map[string]bool {
	out := map[string]bool{}
	for _, experiment := range experiments {
		out[experiment.Project] = true
	}
	return out
}

func TestKustoSourcePreservesCanonicalIdentity(t *testing.T) {
	ctx := context.Background()
	rows := []KustoMetricRow{
		{WorkspaceID: "sample", SourceStoreID: "source-1", Project: "tau-submit", ExperimentID: "pretrain", RunGroupID: "canonical-group", RunID: "sample-run-001", MetricName: "pretrain/loss", Step: 1, WallTime: "2026-06-16T21:04:00Z", Value: 1.2, Tags: `{"suite":"sample-suite"}`},
		{WorkspaceID: "sample", SourceStoreID: "source-1", Project: "tau-submit", ExperimentID: "pretrain", RunGroupID: "canonical-group", RunID: "sample-run-001", MetricName: "pretrain/lr", Step: 1, WallTime: "2026-06-16T21:04:00Z", Value: 0.001, Tags: `{"suite":"sample-suite"}`},
	}
	source := KustoSource{Metrics: rows}

	snapshot, err := source.BuildSnapshot(ctx, Options{Target: "sample-run-001", Metric: "pretrain/loss", MaxRuns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 1 {
		t.Fatalf("unexpected runs: %+v", snapshot.Runs)
	}
	run := snapshot.Runs[0]
	if run.Project != "tau-submit" || run.RunGroupID != "canonical-group" || run.Tags["suite"] != "sample-suite" {
		t.Fatalf("canonical Kusto identity not preserved: %+v", run)
	}
	if snapshot.RunGroups[0].RunGroupID != "canonical-group" || snapshot.Chart.Series[0].RunGroupID != "canonical-group" {
		t.Fatalf("canonical run_group did not drive grouping/chart: groups=%+v chart=%+v", snapshot.RunGroups, snapshot.Chart.Series)
	}
	if strings.Contains(strings.ToLower(strings.Join(snapshot.Warnings, "\n")), "overlay") {
		t.Fatalf("canonical Kusto snapshot must not mention overlays: %+v", snapshot.Warnings)
	}
	scopedSnapshot, err := source.BuildSnapshot(ctx, Options{
		Target:    "sample-run-001",
		Workspace: "sample",
		Metric:    "pretrain/loss",
		MaxRuns:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if scopedSnapshot.Runs[0].Project != "tau-submit" || scopedSnapshot.Runs[0].RunGroupID != "canonical-group" {
		t.Fatalf("workspace-scoped Kusto read changed canonical identity: %+v", scopedSnapshot.Runs[0])
	}

	result, err := source.SearchExperiments(ctx, expstore.ExperimentSearchOptions{
		Query: "pretrain",
		Tags:  map[string]string{"suite": "sample-suite"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 1 || result.Experiments[0].ExperimentID != "pretrain" || result.Experiments[0].RunGroupCount != 1 {
		t.Fatalf("canonical experiment search not reflected: %+v", result)
	}
	projectResult, err := source.SearchExperiments(ctx, expstore.ExperimentSearchOptions{
		Project: "tau-submit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projectResult.Experiments) != 1 || projectResult.Experiments[0].Project != "tau-submit" {
		t.Fatalf("canonical project search failed: %+v", projectResult.Experiments)
	}
	runSearch, err := source.SearchRuns(ctx, expstore.RunSearchOptions{
		Project: "tau-submit",
		Tags:    map[string]string{"suite": "sample-suite"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runSearch.Runs) != 1 || runSearch.Runs[0].RunID != "sample-run-001" || runSearch.Runs[0].Project != "tau-submit" {
		t.Fatalf("canonical project run search failed: %+v", runSearch.Runs)
	}
	disallowed := KustoSource{
		Metrics:         rows,
		AllowedProjects: []string{"other-project"},
	}
	blocked, err := disallowed.SearchExperiments(ctx, expstore.ExperimentSearchOptions{Project: "tau-submit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked.Experiments) != 0 {
		t.Fatalf("disallowed canonical project should not be returned: %+v", blocked.Experiments)
	}
}

func TestLocalSnapshotIncludesRunLifecycleMetadata(t *testing.T) {
	ctx := context.Background()
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "tau-search-snapshot",
		Project:     "tau",
		Description: "Can Stellar show run lifecycle?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := "tau-search-snapshot"
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "healthy-run",
			Project:    "tau",
			RunGroupID: "baseline",
			State:      "succeeded",
			CreatedAt:  "2026-06-10T17:00:00Z",
		},
		Tags: []expstore.TagRecord{{ScopeType: "run", ScopeID: "healthy-run", Key: "suite", Value: "stellar"}},
		MetricFiles: []expstore.MetricFileRecord{{
			FileID:        "metrics-healthy-run",
			Path:          "metrics/healthy-run.jsonl",
			Format:        "jsonl",
			SchemaVersion: expstore.MetricSchemaVersion,
			Project:       "tau",
			RunGroupID:    "baseline",
			RunID:         "healthy-run",
			RowCount:      1,
			CreatedAt:     "2026-06-10T17:20:00Z",
		}},
		MetricSummaries: []expstore.MetricSummaryRecord{{
			FileID:     "metrics-healthy-run",
			RunID:      "healthy-run",
			Project:    "tau",
			RunGroupID: "baseline",
			MetricName: "train/loss",
			Count:      1,
			UpdatedAt:  "2026-06-10T17:20:00Z",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "failed-run",
			Project:    "tau",
			RunGroupID: "baseline",
			State:      "failed",
			CreatedAt:  "2026-06-10T17:05:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := BuildSnapshot(ctx, store, Options{Target: target, Mode: SnapshotModeSummary, MaxRuns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status.LifecycleCounts["succeeded"] != 1 || snapshot.Status.LifecycleCounts["failed"] != 1 {
		t.Fatalf("unexpected lifecycle counts: %+v", snapshot.Status.LifecycleCounts)
	}
	runByID := map[string]RunView{}
	for _, run := range snapshot.Runs {
		runByID[run.RunID] = run
	}
	if runByID["healthy-run"].LifecycleState != "succeeded" || !runByID["healthy-run"].Successful || runByID["healthy-run"].Tags["suite"] != "stellar" {
		t.Fatalf("healthy run missing lifecycle metadata: %+v", runByID["healthy-run"])
	}
	if runByID["healthy-run"].UpdatedAt != "2026-06-10T17:20:00Z" {
		t.Fatalf("healthy run should expose metric summary updated_at, got %+v", runByID["healthy-run"])
	}
	if runByID["failed-run"].UpdatedAt != "2026-06-10T17:05:00Z" {
		t.Fatalf("failed run should fall back to created_at when no metric update exists, got %+v", runByID["failed-run"])
	}
	if runByID["failed-run"].LifecycleState != "failed" || runByID["failed-run"].Successful {
		t.Fatalf("failed run should not be successful: %+v", runByID["failed-run"])
	}
}

func TestLocalSnapshotIncludesArtifactMetadataLineageAndTablePreview(t *testing.T) {
	ctx := context.Background()
	target := "stellar-artifact-metadata"
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        target,
		Project:     "tau",
		Description: "Can Stellar preserve artifact captions?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tablePath := filepath.Join(store.Root, "artifacts", "captioned-run", "table-step-7.json")
	if err := os.MkdirAll(filepath.Dir(tablePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tablePath, []byte(`{"columns":["image","prediction"],"rows":[{"image":"case-1","prediction":"ok"}],"caption":"validation table","step":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "captioned-run",
			Project:    "tau",
			RunGroupID: "baseline",
			State:      "succeeded",
			CreatedAt:  "2026-06-10T17:00:00Z",
		},
		Artifacts: []expstore.ArtifactRecord{{
			ArtifactID: "artifact-captioned-run-media",
			RunID:      "captioned-run",
			Type:       "image",
			URI:        "artifacts/captioned-run/media-prediction-gallery-step-7.png",
			Name:       "media/prediction-gallery step 7",
			CreatedAt:  "2026-06-10T17:01:00Z",
			Caption:    "validation examples",
			Direction:  "output",
			Alias:      "latest-gallery",
		}, {
			ArtifactID:        "artifact-captioned-run-table",
			RunID:             "captioned-run",
			Type:              "table",
			URI:               "artifacts/captioned-run/table-step-7.json",
			Name:              "examples/table step 7",
			CreatedAt:         "2026-06-10T17:02:00Z",
			Caption:           "validation table",
			Direction:         "input",
			SourceArtifactID:  "artifact-source-run-output",
			SourceRunID:       "source-run",
			SourceDatasetName: "vision",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := BuildSnapshot(ctx, store, Options{Target: target, MaxRuns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Artifacts) != 2 {
		t.Fatalf("snapshot artifacts=%d, want 2: %+v", len(snapshot.Artifacts), snapshot.Artifacts)
	}
	byID := map[string]ArtifactView{}
	for _, artifact := range snapshot.Artifacts {
		byID[artifact.ArtifactID] = artifact
	}
	media := byID["artifact-captioned-run-media"]
	if media.Caption != "validation examples" || media.Direction != "output" || media.Alias != "latest-gallery" {
		t.Fatalf("snapshot lost media metadata: %+v", media)
	}
	table := byID["artifact-captioned-run-table"]
	if table.Direction != "input" || table.SourceArtifactID != "artifact-source-run-output" || table.SourceRunID != "source-run" || table.SourceDatasetName != "vision" {
		t.Fatalf("snapshot lost lineage metadata: %+v", table)
	}
	if table.Table == nil || table.Table.Caption != "validation table" || table.Table.Step != "7" || len(table.Table.Columns) != 2 || table.Table.Rows[0]["prediction"] != "ok" {
		t.Fatalf("snapshot lost table preview: %+v", table.Table)
	}
}

func TestLocalSnapshotIncludesReproContextAndConfigDiffs(t *testing.T) {
	ctx := context.Background()
	target := "stellar-config-repro"
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        target,
		Project:     "tau",
		Description: "Can Stellar compare configs?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, spec := range []struct {
		runID string
		group string
		lr    string
	}{
		{"run-a", "baseline", "0.001"},
		{"run-b", "tuned", "0.002"},
	} {
		if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
			Run: expstore.RunRecord{
				RunID:      spec.runID,
				Project:    "tau",
				RunGroupID: spec.group,
				State:      "succeeded",
				CreatedAt:  "2026-06-10T17:00:00Z",
			},
			RunContext: &expstore.RunContextRecord{
				RunID:        spec.runID,
				Runtime:      `{"python":"3.13"}`,
				Dependencies: `{"packages":[{"name":"lightning","version":"2"}]}`,
				LogURI:       "logs/" + spec.runID + ".txt",
			},
			Configs: []expstore.ConfigRecord{{
				ConfigHash:     "config-" + spec.runID,
				RunID:          spec.runID,
				Format:         "json",
				URI:            "configs/" + spec.runID + ".json",
				NormalizedJSON: `{"lr":` + spec.lr + `,"batch_size":16}`,
				IndexedFields:  `{"lr":` + spec.lr + `,"batch_size":16}`,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := BuildSnapshot(ctx, store, Options{Target: target, MaxRuns: 10})
	if err != nil {
		t.Fatal(err)
	}
	runSystems := map[string]map[string]string{}
	for _, run := range snapshot.Runs {
		runSystems[run.RunID] = map[string]string{}
		for _, field := range run.Systems {
			runSystems[run.RunID][field.Name] = field.Value
		}
	}
	if runSystems["run-a"]["Runtime"] == "" || runSystems["run-a"]["Log URI"] != "logs/run-a.txt" {
		t.Fatalf("missing repro context in systems: %+v", runSystems["run-a"])
	}
	foundPinnedLR := false
	for _, diff := range snapshot.Compare.RuntimeDiffs {
		if diff.Field == "Config: lr" && diff.Pinned {
			foundPinnedLR = true
		}
	}
	if !foundPinnedLR {
		t.Fatalf("missing pinned config lr diff: %+v", snapshot.Compare.RuntimeDiffs)
	}
}

func TestLocalSnapshotCanTargetExplicitExperiment(t *testing.T) {
	ctx := context.Background()
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "explicit-experiment-target",
		Project:     "tau",
		Description: "Can Stellar target an explicit experiment?",
		Group:       "baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, runID := range []string{"included-run", "other-run"} {
		if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
			Run: expstore.RunRecord{
				RunID:      runID,
				Project:    "tau",
				RunGroupID: "baseline",
				State:      "running",
				CreatedAt:  "2026-06-10T17:00:00Z",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AssignRunToExperiment(ctx, expstore.ExperimentRecord{
		ExperimentID: "explicit-experiment",
		Project:      "tau",
		Name:         "Explicit experiment",
		Source:       "explicit",
		CreatedAt:    "2026-06-10T17:05:00Z",
		UpdatedAt:    "2026-06-10T17:05:00Z",
	}, "included-run"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := BuildSnapshot(ctx, store, Options{Target: "explicit-experiment", Mode: SnapshotModeSummary, MaxRuns: 10})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TargetType != "experiment" || snapshot.Experiment == nil || snapshot.Experiment.ExperimentID != "explicit-experiment" {
		t.Fatalf("snapshot did not target explicit experiment: target_type=%s experiment=%+v", snapshot.TargetType, snapshot.Experiment)
	}
	if len(snapshot.Runs) != 1 || snapshot.Runs[0].RunID != "included-run" {
		t.Fatalf("experiment snapshot should include only assigned run: %+v", snapshot.Runs)
	}
	if snapshot.Status.Runs != 1 || snapshot.Status.LifecycleCounts["running"] != 1 {
		t.Fatalf("unexpected experiment snapshot status: %+v", snapshot.Status)
	}
}

func colorsByRun(series []ChartSeries) map[string]string {
	out := map[string]string{}
	for _, item := range series {
		out[item.RunID] = item.Color
	}
	return out
}

func TestLoadKustoMetricRowsNormalizesLegacyExperimentID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		`{"project":"sample-project","question_id":"q1","run_group_id":"g1","run_id":"r1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":1}`,
		`{"project":"sample-project","question_id":"q1","run_group_id":"g1","run_id":"r2","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":2}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := LoadKustoMetricRows(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].RunID != "r2" || rows[1].ExperimentID != "q1" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestParseKustoMetricRowsReadsRESTTables(t *testing.T) {
	raw := []byte(`{
  "Tables": [
    {
      "TableName": "PrimaryResult",
      "Columns": [
        {"ColumnName": "project"},
        {"ColumnName": "experiment_id"},
        {"ColumnName": "run_group_id"},
        {"ColumnName": "run_id"},
        {"ColumnName": "metric_name"},
        {"ColumnName": "step"},
        {"ColumnName": "wall_time"},
        {"ColumnName": "value"}
      ],
      "Rows": [
        ["sample-project", "q1", "g1", "r1", "train/return", 10, "2026-05-21T00:00:00Z", 42.5]
      ]
    }
  ]
}`)
	rows, err := ParseKustoMetricRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != "r1" || rows[0].Step != 10 || rows[0].Value != 42.5 {
		t.Fatalf("unexpected REST table rows: %+v", rows)
	}
}

func TestParseKustoMetricRowsReadsV2DataFrames(t *testing.T) {
	raw := []byte(`[
  {"FrameType":"DataSetHeader","IsProgressive":false,"Version":"v2.0"},
  {
    "FrameType":"DataTable",
    "TableId":0,
    "TableKind":"QueryProperties",
    "TableName":"@ExtendedProperties",
    "Columns":[{"ColumnName":"Key"},{"ColumnName":"Value"}],
    "Rows":[["Visualization", "{}"]]
  },
  {
    "FrameType":"DataTable",
    "TableId":1,
    "TableKind":"PrimaryResult",
    "TableName":"PrimaryResult",
    "Columns": [
      {"ColumnName": "project"},
      {"ColumnName": "experiment_id"},
      {"ColumnName": "run_group_id"},
      {"ColumnName": "run_id"},
      {"ColumnName": "metric_name"},
      {"ColumnName": "step"},
      {"ColumnName": "wall_time"},
      {"ColumnName": "value"}
    ],
    "Rows": [
      ["sample-project", "q1", "g1", "r1", "train/return", 10, "2026-05-21T00:00:00Z", 42.5]
    ]
  },
  {"FrameType":"DataSetCompletion"}
]`)
	rows, err := ParseKustoMetricRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != "r1" || rows[0].MetricName != "train/return" {
		t.Fatalf("unexpected v2 frame rows: %+v", rows)
	}
}

func TestParseKustoMetricRowsReadsV2FragmentedFrames(t *testing.T) {
	// Real azure-kusto-go QueryToJson shape when the response is progressive:
	// PrimaryResult is split across a TableHeader (Columns + TableId) and
	// TableFragment frames (Rows for the matching TableId), so the
	// DataTable-only path would silently return zero rows.
	raw := []byte(`[
  {"FrameType":"DataSetHeader","IsProgressive":true,"Version":"v2.0"},
  {"FrameType":"DataTable","TableId":0,"TableKind":"QueryProperties",
   "Columns":[{"ColumnName":"Key"},{"ColumnName":"Value"}],"Rows":[["Visualization","{}"]]},
  {"FrameType":"TableHeader","TableId":1,"TableKind":"PrimaryResult","TableName":"PrimaryResult",
   "Columns": [
     {"ColumnName": "project"},
     {"ColumnName": "experiment_id"},
     {"ColumnName": "run_group_id"},
     {"ColumnName": "run_id"},
     {"ColumnName": "metric_name"},
     {"ColumnName": "step"},
     {"ColumnName": "wall_time"},
     {"ColumnName": "value"}
   ]},
  {"FrameType":"TableFragment","TableFragmentType":"DataAppend","TableId":1,
   "Rows":[["sample-project","q1","g1","r1","train/return",10,"2026-05-21T00:00:00Z",42.5]]},
  {"FrameType":"TableFragment","TableFragmentType":"DataAppend","TableId":1,
   "Rows":[["sample-project","q1","g1","r1","train/return",20,"2026-05-21T00:01:00Z",43.5]]},
  {"FrameType":"TableCompletion","TableId":1,"RowCount":2},
  {"FrameType":"DataTable","TableId":2,"TableKind":"QueryCompletionInformation",
   "Columns":[{"ColumnName":"EventTypeName"}],"Rows":[["QueryResourceConsumption"]]},
  {"FrameType":"DataSetCompletion","HasErrors":false}
]`)
	rows, err := ParseKustoMetricRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("fragmented frames yielded %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].RunID != "r1" || rows[0].MetricName != "train/return" || rows[0].Value != 42.5 {
		t.Fatalf("unexpected first fragmented row: %+v", rows[0])
	}
	if rows[1].Step != 20 || rows[1].Value != 43.5 {
		t.Fatalf("unexpected second fragmented row: %+v", rows[1])
	}
}

func TestParseKustoMetricRowsReadsCopilotRowsEnvelope(t *testing.T) {
	raw := []byte(`{
  "columns": ["project", "experiment_id", "run_group_id", "run_id", "metric_name", "step", "wall_time", "value"],
  "rows": [
    {"project":"sample-project","experiment_id":"q1","run_group_id":"g1","run_id":"r1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":1}
  ]
}`)
	rows, err := ParseKustoMetricRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != "r1" {
		t.Fatalf("unexpected rows envelope: %+v", rows)
	}
}

func TestKustoSourceExecutesQueryCommand(t *testing.T) {
	script := filepath.Join(t.TempDir(), "kusto-query")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
query="$(cat)"
case "$query" in
  *"column_ifexists('experiment_id', '') == 'sample-project-wandb-migration'"*) ;;
  *) echo "missing scoped target" >&2; exit 7 ;;
esac
printf '%s\n' '{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-live","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":33}'
`), 0o755); err != nil {
		t.Fatal(err)
	}

	snapshot, err := (KustoSource{
		Project:      "sample-project",
		QueryCommand: script,
	}).BuildSnapshot(context.Background(), Options{
		Target: "sample-project-wandb-migration",
		Metric: "train/return",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status.Runs != 1 || len(snapshot.Chart.Series) != 1 || snapshot.Chart.Series[0].RunID != "seed-live" {
		t.Fatalf("unexpected live Kusto snapshot: %+v", snapshot)
	}
	if snapshot.StorePath != "kusto://TauExpMetrics" || snapshot.Status.StorePath != "kusto://TauExpMetrics" {
		t.Fatalf("projection Kusto snapshot should display TauExpMetrics store path: snapshot=%q status=%q", snapshot.StorePath, snapshot.Status.StorePath)
	}
}

func TestKustoSourceBuildSeriesPropagatesRangeBudgetAndRawQuery(t *testing.T) {
	script := filepath.Join(t.TempDir(), "kusto-series-query")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
query="$(cat)"
case "$query" in
  *"let target_points = 900;"*"run_id in ('seed-live')"*"step >= 100"*"step <= 200"*"union endpoints, minima, maxima, milestones"*) ;;
  *) echo "missing focused series scope" >&2; exit 7 ;;
esac
printf '%s\n' '{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-live","metric_name":"train/return","step":100,"wall_time":"2026-05-21T00:00:00Z","value":33,"source_point_count":10000}'
printf '%s\n' '{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-live","metric_name":"train/return","step":200,"wall_time":"2026-05-21T00:01:00Z","value":35,"source_point_count":10000}'
`), 0o755); err != nil {
		t.Fatal(err)
	}

	start, end := int64(100), int64(200)
	detail, err := (KustoSource{
		Project:      "sample-project",
		QueryCommand: script,
	}).BuildSeries(context.Background(), SeriesOptions{
		Target:    "sample-project-wandb-migration",
		Metric:    "train/return",
		RunID:     "seed-live",
		StartStep: &start,
		EndStep:   &end,
		MaxPoints: 900,
		MaxRuns:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if detail.RawQuerySource != "kusto" || !strings.Contains(detail.RawQuery, "point_count=1") || strings.Contains(detail.RawQuery, "let bucketed") {
		t.Fatalf("raw query must expose scoped source rows without display aggregation:\n%s", detail.RawQuery)
	}
	if len(detail.Chart.Series) != 1 || detail.Chart.Series[0].PointCount != 10000 || detail.Chart.Series[0].Sampling.SourcePoints != 10000 {
		t.Fatalf("Kusto source count transparency missing: %+v", detail.Chart)
	}
}

func TestKustoSourceBuildSeriesClampsPreselectionForSmallDisplayBudget(t *testing.T) {
	script := filepath.Join(t.TempDir(), "kusto-series-small-budget")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
query="$(cat)"
case "$query" in
  *"let target_points = 100;"*"run_id in ('seed-live')"*) ;;
  *) echo "small display budget was not clamped for Kusto preselection" >&2; exit 7 ;;
esac
printf '%s\n' '{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-live","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":33,"source_point_count":10000}'
printf '%s\n' '{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-live","metric_name":"train/return","step":2,"wall_time":"2026-05-21T00:01:00Z","value":35,"source_point_count":10000}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	detail, err := (KustoSource{
		Project:      "sample-project",
		QueryCommand: script,
	}).BuildSeries(context.Background(), SeriesOptions{
		Target:    "sample-project-wandb-migration",
		Metric:    "train/return",
		RunID:     "seed-live",
		MaxPoints: 32,
		MaxRuns:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Chart.Series) != 1 || detail.Chart.Series[0].Sampling.RequestedBudget != 32 {
		t.Fatalf("display budget must remain caller-requested after Kusto preselection clamp: %+v", detail.Chart)
	}
}

func TestKustoSourceQueryCommandOverridesMetricsFile(t *testing.T) {
	tempDir := t.TempDir()
	metricsFile := filepath.Join(tempDir, "stale-metrics.jsonl")
	if err := os.WriteFile(metricsFile, []byte(`{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-stale","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":1}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tempDir, "kusto-query")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
	cat >/dev/null
	printf '%s\n' '{"project":"sample-project","experiment_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-live","metric_name":"train/return","step":2,"wall_time":"2026-05-21T00:01:00Z","value":33}'
	`), 0o755); err != nil {
		t.Fatal(err)
	}
	source := KustoSource{
		Project:      "sample-project",
		MetricsFile:  metricsFile,
		QueryCommand: script,
	}

	snapshot, err := source.BuildSnapshot(context.Background(), Options{
		Target: "sample-project-wandb-migration",
		Metric: "train/return",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Chart.Series) != 1 || snapshot.Chart.Series[0].RunID != "seed-live" {
		t.Fatalf("snapshot used stale metrics file instead of live query: %+v", snapshot.Chart.Series)
	}
	runs, err := source.SearchRuns(context.Background(), expstore.RunSearchOptions{
		Query: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if runs.Total != 1 || len(runs.Runs) != 1 || runs.Runs[0].RunID != "seed-live" {
		t.Fatalf("run search used stale metrics file instead of live query: %+v", runs)
	}
}

func TestKustoSourceFocusedMetricSnapshotDiscoversCatalog(t *testing.T) {
	script := filepath.Join(t.TempDir(), "kusto-query")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
query="$(cat)"
case "$query" in
  *"run_id == 'sample-run-001'"*) ;;
  *) echo "missing scoped Vision run target" >&2; exit 7 ;;
esac
case "$query" in
  *"metric_name in ("*)
    printf '%s\n' '{"project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"pretrain/loss","step":1800,"wall_time":"2026-06-16T21:04:00Z","value":1.7}'
    printf '%s\n' '{"project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"tau/run_status","step":0,"wall_time":"2026-06-16T21:05:00Z","value":1}'
    ;;
  *)
    printf '%s\n' '{"project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"pretrain/loss","step":1800,"wall_time":"2026-06-16T21:04:00Z","value":1.7}'
    printf '%s\n' '{"project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"pretrain/lr","step":1800,"wall_time":"2026-06-16T21:04:00Z","value":0.0004}'
    printf '%s\n' '{"project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"pretrain/teacher_entropy_gap","step":1800,"wall_time":"2026-06-16T21:04:00Z","value":0.12}'
    printf '%s\n' '{"project":"tau-submit","experiment_id":"tau-submit","run_group_id":"default","run_id":"sample-run-001","metric_name":"tau/run_status","step":0,"wall_time":"2026-06-16T21:05:00Z","value":1}'
    ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}

	source := KustoSource{
		Project:      "tau-submit",
		Ingestion:    "remote-write",
		QueryCommand: script,
	}
	opts := Options{
		Target: "sample-run-001",
		Metric: "pretrain/loss",
	}
	summary, err := source.BuildSnapshot(context.Background(), Options{
		Target: opts.Target,
		Metric: opts.Metric,
		Mode:   SnapshotModeSummary,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Chart.HasData || len(summary.MetricOptions) != 3 {
		t.Fatalf("summary should expose a lightweight metric catalog without chart payload: chart=%+v options=%+v", summary.Chart, summary.MetricOptions)
	}
	if summary.StorePath != "kusto://ExperimentMetrics" || summary.Status.StorePath != "kusto://ExperimentMetrics" {
		t.Fatalf("remote-write Kusto summary should display ExperimentMetrics store path: snapshot=%q status=%q", summary.StorePath, summary.Status.StorePath)
	}
	for _, metric := range []string{"pretrain/loss", "pretrain/lr", "pretrain/teacher_entropy_gap"} {
		if _, ok := metricOptionByName(summary.MetricOptions, metric); !ok {
			t.Fatalf("summary catalog missing %q: %+v", metric, summary.MetricOptions)
		}
	}

	snapshot, err := source.BuildSnapshot(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Chart.HasData || snapshot.Chart.MetricName != "pretrain/loss" || len(snapshot.Chart.Series) != 1 {
		t.Fatalf("focused Kusto snapshot should keep the requested chart: %+v", snapshot.Chart)
	}
	for _, metric := range []string{"pretrain/loss", "pretrain/lr", "pretrain/teacher_entropy_gap"} {
		option, ok := metricOptionByName(snapshot.MetricOptions, metric)
		if !ok {
			t.Fatalf("focused Kusto snapshot catalog missing %q: %+v", metric, snapshot.MetricOptions)
		}
		if metric == "pretrain/loss" && !option.Selected {
			t.Fatalf("requested metric should be selected in catalog: %+v", snapshot.MetricOptions)
		}
	}
	if _, ok := metricOptionByName(snapshot.MetricOptions, expkusto.RunStatusMetricName); ok {
		t.Fatalf("status marker should not be exposed as a scalar metric option: %+v", snapshot.MetricOptions)
	}

	compact, err := source.BuildSnapshot(context.Background(), Options{
		Target:            opts.Target,
		Metric:            opts.Metric,
		Mode:              SnapshotModeMetric,
		SkipMetricCatalog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !compact.Chart.HasData || len(compact.MetricOptions) != 1 {
		t.Fatalf("compact Kusto metric snapshot should skip the extra catalog query: chart=%+v options=%+v", compact.Chart, compact.MetricOptions)
	}
}

func metricOptionByName(options []MetricOptionView, name string) (MetricOptionView, bool) {
	for _, option := range options {
		if option.Name == name {
			return option, true
		}
	}
	return MetricOptionView{}, false
}

func TestKustoSourceUsesNativeQueryTransport(t *testing.T) {
	var seen string
	source := KustoSource{
		Project: "sample-project",
		NativeQuery: func(_ context.Context, query string) (string, error) {
			seen = query
			return `{"project":"sample-project","question_id":"sample-project-wandb-migration","run_group_id":"h200-rollout","run_id":"seed-native","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":33}`, nil
		},
	}

	snapshot, err := source.BuildSnapshot(context.Background(), Options{
		Target: "sample-project-wandb-migration",
		Metric: "train/return",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range snapshot.Warnings {
		if strings.Contains(warning, "--kusto-endpoint") {
			t.Fatalf("native transport should not warn about missing sources: %+v", snapshot.Warnings)
		}
	}
	if snapshot.Status.Runs != 1 || len(snapshot.Chart.Series) != 1 || snapshot.Chart.Series[0].RunID != "seed-native" {
		t.Fatalf("unexpected native Kusto snapshot: %+v", snapshot)
	}
	if !strings.Contains(seen, "sample-project-wandb-migration") {
		t.Fatalf("native transport received an unscoped query:\n%s", seen)
	}
}

func TestKustoSourceQueryCommandWinsOverNativeQuery(t *testing.T) {
	script := filepath.Join(t.TempDir(), "kusto-query")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
cat >/dev/null
printf '%s\n' '{"project":"sample-project","question_id":"sample-project-wandb-migration","run_group_id":"h200-rollout","run_id":"seed-shell","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":33}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	nativeCalls := 0
	source := KustoSource{
		Project:      "sample-project",
		QueryCommand: script,
		NativeQuery: func(context.Context, string) (string, error) {
			nativeCalls++
			return "", nil
		},
	}

	snapshot, err := source.BuildSnapshot(context.Background(), Options{
		Target: "sample-project-wandb-migration",
		Metric: "train/return",
	})
	if err != nil {
		t.Fatal(err)
	}
	if nativeCalls != 0 {
		t.Fatalf("explicit --kusto-query-command must win over the native transport, native calls=%d", nativeCalls)
	}
	if len(snapshot.Chart.Series) != 1 || snapshot.Chart.Series[0].RunID != "seed-shell" {
		t.Fatalf("snapshot did not come from the shell adapter: %+v", snapshot.Chart.Series)
	}
}

func TestKustoSourceWarnsWhenNoTransportConfigured(t *testing.T) {
	_, warnings, err := (KustoSource{Project: "sample-project"}).loadRowsForRunSearch(context.Background(), expstore.RunSearchOptions{
		Query: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "--kusto-endpoint") {
		t.Fatalf("unconfigured Kusto source must name every accepted transport: %+v", warnings)
	}
}
