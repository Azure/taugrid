// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// JSON contracts from expcockpit/cockpit.go and expstore/types.go.
export interface ExperimentRecord {
  experiment_id: string; project: string; name: string; description?: string;
  source: string; created_at: string; updated_at: string;
}
export interface Experiment extends ExperimentRecord {
  run_count: number; run_group_count: number; state_counts?: Record<string, number>;
  lifecycle_counts?: Record<string, number>; latest_run_at?: string; metric_names?: string[];
}
export interface ExperimentSearchResult {
  schema_version: string; generated_at: string; store_path: string; total: number;
  truncated?: boolean; experiments: Experiment[]; warnings?: string[];
}
export interface RunRecord {
  run_id: string; project: string; experiment_id?: string; run_group_id: string;
  parent_run_id?: string; state: string; owner?: string; created_at: string;
  started_at?: string; completed_at?: string; config_hash?: string; code_sha?: string;
  image_digest?: string; tau_command?: string; result_uri?: string; index_version: string;
}
export interface MetricSummary {
  file_id?: string; run_id: string; project?: string; run_group_id?: string; metric_name: string;
  count: number; finite_count: number; non_finite_count?: number; min_step?: number; max_step?: number;
  latest_step?: number; latest_wall_time?: number; latest_value?: number; min_value?: number;
  max_value?: number; updated_at: string; latest_file_id?: string;
}
export interface RunSearchRun extends RunRecord {
  lifecycle_state: string; successful: boolean; success_reasons?: string[];
  tags?: Record<string, string>; metric_names?: string[]; metrics?: MetricSummary[];
}
export interface RunSearchResult {
  schema_version: string; generated_at: string; store_path: string; target?: string;
  total: number; truncated?: boolean; runs: RunSearchRun[]; warnings?: string[];
}
export interface RunView {
  run_id: string; source?: string; source_store_id?: string; workspace_id?: string; cluster?: string;
  project: string; run_group_id: string; state: string; lifecycle_state?: string; outcome_state?: string;
  liveness_state?: string; lifecycle_reason?: string; lifecycle_source?: string; last_evidence_at?: string;
  freshness_seconds?: number; lifecycle_explicit?: boolean; workload_absence_confirmed?: boolean;
  successful?: boolean; success_reasons?: string[]; owner?: string; created_at: string; updated_at?: string;
  started_at?: string; completed_at?: string; config_hash?: string; code_sha?: string; image_digest?: string;
  tau_command?: string; result_uri?: string; color?: string; tags?: Record<string, string>; metric_names?: string[];
  launch?: Launch;
  systems: FieldView[]; configs?: ConfigView[]; artifacts?: ArtifactView[]; events?: EventView[];
  observations?: ObservationView[]; observe_cli: string;
}
// core/experiment.Launch: resolved submission intent, not an allocation.
export interface Launch {
  version: number; workload_kind?: string; gpus_per_worker?: number; workers?: number; cpu_workers?: number; gpu_total?: number;
  gpu_class?: string; image?: string; entrypoint?: string; launcher?: string; profile?: string;
  gpu_resource_mode?: string; gpu_resource_name?: string; mig_profile?: string;
  workspace?: string; namespace?: string;
  tau_command?: string;
}
export type Run = RunView | RunSearchRun;
export interface RunGroupView {
  run_group_id: string; project: string; name: string; group_class: string; created_at: string; updated_at: string;
}
export interface FieldView { name: string; value: string; collection_state: string }
export interface ConfigView {
  config_hash: string; run_id: string; format: string; uri: string; normalized_json?: string; indexed_fields?: string;
}
export interface EventView {
  event_id: string; run_id: string; time: string; type: string; source: string; severity: string; message: string; payload?: string;
}
export interface ObservationView {
  observation_id: string; idempotency_key?: string; author: string; source: string; type: string;
  scope_type: string; scope_id: string; text: string; evidence?: string; created_at: string;
}
export interface TablePreviewView {
  columns?: string[]; rows?: Record<string, unknown>[]; caption?: string; step?: string;
}
export interface ArtifactView {
  artifact_id: string; run_id: string; type: string; uri: string; name: string; durable_ref?: string;
  content_type?: string; digest?: string; size_bytes?: string; step?: string; tags?: string; rank?: string;
  created_at: string; preview?: string; external_ref?: string; caption?: string; direction?: string;
  alias?: string; source_artifact_id?: string; source_run_id?: string; source_dataset_name?: string;
  source_dataset_version?: string; source_dataset_digest?: string; table?: TablePreviewView;
}
export interface MetricGroupView {
  run_group_id: string; group_class: string; run_count: number; latest_step: string;
  min: string; p25: string; median: string; p75: string; max: string; best: string; best_value: number;
}
export interface MetricView { name: string; unit?: string; groups: MetricGroupView[] }
export interface CardView { name: string; metrics: MetricView[] }
export interface MetricOptionView { name: string; card: string; selected: boolean }
export interface ChartPoint { step: number; value: number }
export interface SamplingMetadata {
  algorithm: string; source_points: number; server_preselected_points: number; preselected_points: number;
  rendered_points: number; requested_budget: number; effective_budget: number; start_step?: number;
  end_step?: number; first_retained: boolean; last_retained: boolean; min_retained: boolean;
  max_retained: boolean; milestone_points?: number; milestones_retained?: number; truncated?: boolean;
}
export interface OverlayMetadata { source: string; start_step: number; end_step: number; sample_count: number }
export interface ChartSeries {
  run_id: string; run_group_id: string; group_class: string; color: string; points?: string;
  values?: ChartPoint[]; smoothed_values?: ChartPoint[]; point_count: number; rendered_points: number;
  decimated?: boolean; overlay?: OverlayMetadata; sampling: SamplingMetadata;
}
export interface ChartSmoothing {
  method: string; alpha: number; dense_point_threshold: number; reason: string; raw_preserved: boolean;
}
export interface ChartView {
  has_data: boolean; metric_name?: string; x_min?: string; x_max?: string; y_min?: string; y_max?: string;
  step_interval?: number; series?: ChartSeries[]; smoothing?: ChartSmoothing;
}
export interface SeriesDetail {
  schema_version: string; generated_at: string; target: string; metric: string; run_id?: string;
  start_step?: number; end_step?: number; step_interval?: number; max_points: number;
  chart: ChartView; raw_query?: string; raw_query_source?: string; warnings?: string[];
}
export interface RunInsight { run_id: string; run_group_id: string; value: string; reason: string }
export interface EventMarker { run_id: string; run_group_id: string; time: string; type: string; severity: string; message: string }
export interface RuntimeDiff { field: string; values: { run_group_id: string; value: string }[]; pinned?: boolean }
export interface CompareInsights {
  summary: string; metric_name?: string; outliers?: RunInsight[]; event_markers?: EventMarker[]; runtime_diffs?: RuntimeDiff[];
}
export interface BestRunView {
  run_id: string; run_group_id: string; group_class: string; metric_name: string; value: string; raw_value: number;
}
export interface SweepRunView {
  rank: number; run_id: string; run_group_id: string; group_class: string; state: string;
  metric: string; metric_width: string; color: string;
}
export interface ParallelAxisView { name: string; kind: string; x: string; min?: string; max?: string; values?: string[] }
export interface ParallelRunSeries {
  run_id: string; run_group_id: string; group_class: string; color: string; points?: string; metric: string; raw_metric: number;
}
export interface ParameterImportanceView {
  name: string; importance: number; importance_label: string; importance_width: string;
  correlation: number; correlation_label: string; correlation_width: string;
}
export interface SweepView {
  has_data: boolean; metric_name?: string; best_run?: BestRunView; runs?: SweepRunView[];
  axes?: ParallelAxisView[]; series?: ParallelRunSeries[]; importance?: ParameterImportanceView[]; config_count: number;
}
export interface ActionView {
  copy_cli: string; open_cli?: string; copy_sql: string; export_packet: string;
  observe_cli: string; next_command?: string; store_path: string;
}
export interface ExperimentSummary {
  status: string; current_answer: string; best_evidence: string; confidence: string;
  seed_coverage: string; blockers: number; decisions: number; next_action: string; next_command?: string;
}
export interface Manifest {
  schema_version: string; kind: string; project?: string; experiment_id?: string; created_at: string;
  updated_at: string; index: string; append_log_dir: string; metrics_dir: string; artifacts_dir: string;
}
export interface Status {
  store_path: string; target: string; target_type: string;
  run_group?: Omit<RunGroupView, 'group_class'>; experiment?: ExperimentRecord; run?: Record<string, unknown>;
  runs: number; run_groups: number; state_counts: Record<string, number>; lifecycle_counts?: Record<string, number>;
  configs: number; metric_files: number; artifacts: number; observations: number; latest_event_at?: string;
}
export interface Snapshot {
  schema_version: string; payload_mode?: string; generated_at: string; store_path: string; target: string;
  target_type: string; manifest: Manifest; status: Status; summary: ExperimentSummary;
  experiment?: ExperimentRecord; run_groups: RunGroupView[]; runs: RunView[]; cards: CardView[];
  chart: ChartView; metric_options: MetricOptionView[]; sweep: SweepView; compare: CompareInsights;
  artifacts: ArtifactView[]; events: EventView[]; observations: ObservationView[]; actions: ActionView;
  best_group_id?: string; seed_coverage: string; warnings?: string[];
}
