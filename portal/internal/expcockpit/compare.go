// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"sort"
	"strings"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

const (
	CompareSchemaVersion = "tau.exp.compare.v0"
	PlotSchemaVersion    = "tau.exp.plot.v0"
)

type CompareOptions struct {
	Target    string
	Metric    string
	Direction string
}

type Comparison struct {
	SchemaVersion string            `json:"schema_version"`
	Target        string            `json:"target"`
	TargetType    string            `json:"target_type"`
	MetricName    string            `json:"metric_name"`
	Direction     string            `json:"direction"`
	BestGroupID   string            `json:"best_group_id,omitempty"`
	BestRunID     string            `json:"best_run_id,omitempty"`
	RunGroups     int               `json:"run_groups"`
	Runs          int               `json:"runs"`
	MetricFiles   int               `json:"metric_files"`
	Groups        []ComparisonGroup `json:"groups"`
	RunValues     []ComparisonRun   `json:"run_values"`
	Warnings      []string          `json:"warnings,omitempty"`
}

type ComparisonGroup struct {
	RunGroupID string  `json:"run_group_id"`
	RunCount   int     `json:"run_count"`
	LatestStep int64   `json:"latest_step"`
	Min        float64 `json:"min"`
	P25        float64 `json:"p25"`
	Median     float64 `json:"median"`
	P75        float64 `json:"p75"`
	Max        float64 `json:"max"`
	BestValue  float64 `json:"best_value"`
	BestRunID  string  `json:"best_run_id,omitempty"`
	Unit       string  `json:"unit,omitempty"`
}

type ComparisonRun struct {
	RunID      string  `json:"run_id"`
	RunGroupID string  `json:"run_group_id"`
	State      string  `json:"state,omitempty"`
	Owner      string  `json:"owner,omitempty"`
	LatestStep int64   `json:"latest_step"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit,omitempty"`
}

type PlotOptions struct {
	Target string
	Metric string
}

type PlotResult struct {
	SchemaVersion string   `json:"schema_version"`
	Target        string   `json:"target"`
	TargetType    string   `json:"target_type"`
	MetricName    string   `json:"metric_name"`
	Series        int      `json:"series"`
	Warnings      []string `json:"warnings,omitempty"`
}

type comparisonData struct {
	snapshot Snapshot
	points   []metricPoint
	metric   string
	warnings []string
}

type runMetricValue struct {
	run   RunView
	point metricPoint
}

func Compare(ctx context.Context, store *expstore.Store, opts CompareOptions) (Comparison, error) {
	direction, err := normalizeDirection(opts.Direction)
	if err != nil {
		return Comparison{}, err
	}
	data, err := loadComparisonData(ctx, store, opts.Target, opts.Metric)
	if err != nil {
		return Comparison{}, err
	}
	runValues := latestMetricValues(data.snapshot.Runs, data.points, data.metric)
	groups := summarizeComparisonGroups(runValues, direction)
	bestGroupID, bestRunID := bestComparisonWinner(groups, direction)
	return Comparison{
		SchemaVersion: CompareSchemaVersion,
		Target:        data.snapshot.Target,
		TargetType:    data.snapshot.TargetType,
		MetricName:    data.metric,
		Direction:     direction,
		BestGroupID:   bestGroupID,
		BestRunID:     bestRunID,
		RunGroups:     len(groups),
		Runs:          len(runValues),
		MetricFiles:   data.snapshot.Status.MetricFiles,
		Groups:        groups,
		RunValues:     comparisonRuns(runValues),
		Warnings:      data.warnings,
	}, nil
}

func RenderPlotSVG(ctx context.Context, store *expstore.Store, opts PlotOptions) ([]byte, PlotResult, error) {
	data, err := loadComparisonData(ctx, store, opts.Target, opts.Metric)
	if err != nil {
		return nil, PlotResult{}, err
	}
	chart := buildChart(data.points, data.metric, groupClassMap(data.snapshot.RunGroups))
	if !chart.HasData {
		return nil, PlotResult{}, fmt.Errorf("metric %q has no chartable points for target %q", data.metric, data.snapshot.Target)
	}
	model := plotSVGModel{
		SchemaVersion: PlotSchemaVersion,
		Target:        data.snapshot.Target,
		TargetType:    data.snapshot.TargetType,
		MetricName:    data.metric,
		XMin:          chart.XMin,
		XMax:          chart.XMax,
		YMin:          chart.YMin,
		YMax:          chart.YMax,
		Series:        chart.Series,
		Height:        286 + len(chart.Series)*18,
	}
	var buf bytes.Buffer
	if err := plotSVGTemplate.Execute(&buf, model); err != nil {
		return nil, PlotResult{}, err
	}
	return buf.Bytes(), PlotResult{
		SchemaVersion: PlotSchemaVersion,
		Target:        data.snapshot.Target,
		TargetType:    data.snapshot.TargetType,
		MetricName:    data.metric,
		Series:        len(chart.Series),
		Warnings:      data.warnings,
	}, nil
}

func loadComparisonData(ctx context.Context, store *expstore.Store, target, metric string) (comparisonData, error) {
	snapshot, err := BuildSnapshot(ctx, store, Options{Target: target, Metric: metric})
	if err != nil {
		return comparisonData{}, err
	}
	metricFiles, err := loadMetricFiles(ctx, store, snapshot.Status, snapshot.Experiment, runIDs(snapshot.Runs))
	if err != nil {
		return comparisonData{}, err
	}
	points, warnings := readMetricPoints(store, metricFiles, 0)
	selected, err := selectMetric(points, metric, snapshot.Target)
	if err != nil {
		return comparisonData{}, err
	}
	return comparisonData{snapshot: snapshot, points: points, metric: selected, warnings: warnings}, nil
}

func selectMetric(points []metricPoint, requested, target string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, point := range points {
			if point.MetricName == requested {
				return requested, nil
			}
		}
		return "", fmt.Errorf("metric %q was not found for target %q", requested, target)
	}
	metric := requestedMetric(points, "")
	if metric == "" {
		return "", fmt.Errorf("no scalar metrics were found for target %q", target)
	}
	return metric, nil
}

func normalizeDirection(direction string) (string, error) {
	direction = strings.TrimSpace(direction)
	if direction == "" {
		return "max", nil
	}
	switch direction {
	case "max", "min":
		return direction, nil
	default:
		return "", fmt.Errorf("--direction must be one of: max, min")
	}
}

func latestMetricValues(runs []RunView, points []metricPoint, metric string) []runMetricValue {
	runByID := map[string]RunView{}
	for _, run := range runs {
		runByID[run.RunID] = run
	}
	latest := map[string]metricPoint{}
	for _, point := range points {
		if point.MetricName != metric {
			continue
		}
		prev, ok := latest[point.RunID]
		if !ok || point.Step >= prev.Step {
			latest[point.RunID] = point
		}
	}
	runIDs := make([]string, 0, len(latest))
	for runID := range latest {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	out := make([]runMetricValue, 0, len(runIDs))
	for _, runID := range runIDs {
		run := runByID[runID]
		if run.RunID == "" {
			run = RunView{RunID: runID, RunGroupID: latest[runID].RunGroupID}
		}
		out = append(out, runMetricValue{run: run, point: latest[runID]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].run.RunGroupID != out[j].run.RunGroupID {
			return out[i].run.RunGroupID < out[j].run.RunGroupID
		}
		return out[i].run.RunID < out[j].run.RunID
	})
	return out
}

func summarizeComparisonGroups(values []runMetricValue, direction string) []ComparisonGroup {
	byGroup := map[string][]runMetricValue{}
	for _, value := range values {
		byGroup[value.run.RunGroupID] = append(byGroup[value.run.RunGroupID], value)
	}
	groupIDs := make([]string, 0, len(byGroup))
	for groupID := range byGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	groups := make([]ComparisonGroup, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		groupValues := byGroup[groupID]
		sort.Slice(groupValues, func(i, j int) bool {
			return groupValues[i].run.RunID < groupValues[j].run.RunID
		})
		values := make([]float64, 0, len(groupValues))
		latestStep := int64(0)
		best := groupValues[0]
		unit := ""
		for _, value := range groupValues {
			values = append(values, value.point.Value)
			if value.point.Step > latestStep {
				latestStep = value.point.Step
			}
			if unit == "" {
				unit = value.point.Unit
			}
			if betterRunValue(value, best, direction) {
				best = value
			}
		}
		sort.Float64s(values)
		groups = append(groups, ComparisonGroup{
			RunGroupID: groupID,
			RunCount:   len(groupValues),
			LatestStep: latestStep,
			Min:        values[0],
			P25:        percentile(values, 0.25),
			Median:     percentile(values, 0.5),
			P75:        percentile(values, 0.75),
			Max:        values[len(values)-1],
			BestValue:  best.point.Value,
			BestRunID:  best.run.RunID,
			Unit:       unit,
		})
	}
	return groups
}

func comparisonRuns(values []runMetricValue) []ComparisonRun {
	out := make([]ComparisonRun, 0, len(values))
	for _, value := range values {
		out = append(out, ComparisonRun{
			RunID:      value.run.RunID,
			RunGroupID: value.run.RunGroupID,
			State:      value.run.State,
			Owner:      value.run.Owner,
			LatestStep: value.point.Step,
			Value:      value.point.Value,
			Unit:       value.point.Unit,
		})
	}
	return out
}

func bestComparisonWinner(groups []ComparisonGroup, direction string) (string, string) {
	if len(groups) == 0 {
		return "", ""
	}
	best := groups[0]
	for _, group := range groups[1:] {
		if betterGroupValue(group, best, direction) {
			best = group
		}
	}
	return best.RunGroupID, best.BestRunID
}

func betterRunValue(candidate, incumbent runMetricValue, direction string) bool {
	if candidate.point.Value == incumbent.point.Value {
		return candidate.run.RunID < incumbent.run.RunID
	}
	if direction == "min" {
		return candidate.point.Value < incumbent.point.Value
	}
	return candidate.point.Value > incumbent.point.Value
}

func betterGroupValue(candidate, incumbent ComparisonGroup, direction string) bool {
	if candidate.BestValue == incumbent.BestValue {
		return candidate.RunGroupID < incumbent.RunGroupID
	}
	if direction == "min" {
		return candidate.BestValue < incumbent.BestValue
	}
	return candidate.BestValue > incumbent.BestValue
}

type plotSVGModel struct {
	SchemaVersion string
	Target        string
	TargetType    string
	MetricName    string
	XMin          string
	XMax          string
	YMin          string
	YMax          string
	Series        []ChartSeries
	Height        int
}

var plotSVGTemplate = template.Must(template.New("plot-svg").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"mul": func(a, b int) int { return a * b },
}).Parse(`<svg xmlns="http://www.w3.org/2000/svg" width="900" height="{{.Height}}" viewBox="0 0 900 {{.Height}}" role="img" aria-labelledby="title desc">
  <title id="title">Tau experiment plot: {{.MetricName}}</title>
  <desc id="desc">target {{.Target}} ({{.TargetType}}), metric {{.MetricName}}</desc>
  <metadata>schema={{.SchemaVersion}}</metadata>
  <style>
    text { font: 12px ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; fill: #334155; }
    .axis { stroke: #94a3b8; stroke-width: 1; }
    .grid { stroke: #e2e8f0; stroke-width: 1; }
    .series { fill: none; stroke-width: 2.5; }
  </style>
  <rect x="0" y="0" width="900" height="{{.Height}}" fill="#ffffff"/>
  <text x="28" y="24">metric={{.MetricName}} target={{.Target}}</text>
  <line class="grid" x1="28" y1="28" x2="28" y2="192"/>
  <line class="grid" x1="732" y1="28" x2="732" y2="192"/>
  <line class="grid" x1="28" y1="28" x2="732" y2="28"/>
  <line class="grid" x1="28" y1="192" x2="732" y2="192"/>
  <line class="axis" x1="28" y1="192" x2="732" y2="192"/>
  <line class="axis" x1="28" y1="28" x2="28" y2="192"/>
  <text x="28" y="212">step {{.XMin}}</text>
  <text x="664" y="212">step {{.XMax}}</text>
  <text x="742" y="34">y {{.YMax}}</text>
  <text x="742" y="196">y {{.YMin}}</text>
{{range .Series}}  <polyline class="series" points="{{.Points}}" stroke="{{.Color}}">
    <title>{{.RunID}} ({{.RunGroupID}})</title>
  </polyline>
{{end}}  <text x="28" y="244">runs</text>
{{range $i, $s := .Series}}  <g transform="translate(28 {{add 266 (mul $i 18)}})">
    <line x1="0" y1="-4" x2="22" y2="-4" stroke="{{$s.Color}}" stroke-width="2.5"/>
    <text x="30" y="0">{{$s.RunID}} group={{$s.RunGroupID}}</text>
  </g>
{{end}}</svg>
`))
