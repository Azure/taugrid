// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type {
  ChartPoint, ChartSeries, ChartSmoothing, ChartView, MetricOptionView, MetricSummary,
  RunView, SamplingMetadata, SeriesDetail, Snapshot,
} from './types';

export type ScalarPoint = ChartPoint;
export interface ScalarSeries extends Pick<ChartSeries, 'run_id'>, Partial<Pick<ChartSeries,
  'run_group_id' | 'color' | 'values' | 'smoothed_values' | 'point_count' | 'rendered_points' | 'decimated'>> {
  sampling?: Partial<Pick<SamplingMetadata,
    'algorithm' | 'source_points' | 'rendered_points' | 'requested_budget' | 'effective_budget' | 'truncated'>>;
}
export interface ScalarChart extends Partial<Pick<ChartView,
  'has_data' | 'metric_name' | 'x_min' | 'x_max' | 'y_min' | 'y_max' | 'step_interval'>> {
  series?: ScalarSeries[];
  smoothing?: Pick<ChartSmoothing, 'method' | 'alpha'> & Partial<Pick<ChartSmoothing, 'reason' | 'raw_preserved'>>;
}
export interface ScalarRun extends Pick<RunView, 'run_id'>, Partial<Pick<RunView,
  'run_group_id' | 'project' | 'workspace_id' | 'cluster' | 'source' | 'owner' | 'state' |
  'lifecycle_state' | 'outcome_state' | 'liveness_state' | 'lifecycle_reason' | 'result_uri' | 'color' | 'metric_names'>> {
  metrics?: (Pick<MetricSummary, 'metric_name'> & Partial<Pick<MetricSummary, 'latest_value' | 'latest_step' | 'finite_count'>>)[];
}
export interface ScalarSnapshot extends Partial<Pick<Snapshot, 'target' | 'payload_mode' | 'warnings'>> {
  runs?: ScalarRun[];
  chart?: ScalarChart;
  metric_options?: (Pick<MetricOptionView, 'name'> & Partial<Pick<MetricOptionView, 'card' | 'selected'>>)[];
}
export interface ScalarSeriesDetail extends Partial<Pick<SeriesDetail,
  'target' | 'metric' | 'run_id' | 'start_step' | 'end_step' | 'step_interval' | 'max_points' | 'raw_query' | 'raw_query_source' | 'warnings'>> {
  chart?: ScalarChart;
}
