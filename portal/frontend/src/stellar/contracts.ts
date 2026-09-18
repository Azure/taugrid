// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export type Availability = 'available' | 'degraded' | 'partial' | 'unavailable' | 'unknown';

export interface ResponseMeta {
  availability: Availability;
  partial: boolean;
  provenance?: string;
  freshness?: string;
  warnings: string[];
  next_cursor?: string;
}

export interface ExperimentSummary {
  experiment_id: string;
  project: string;
  name: string;
  description?: string;
  run_count?: number;
  latest_run_at?: string;
}

export interface ExperimentPage extends ResponseMeta {
  experiments: ExperimentSummary[];
}

export interface RunSummary {
  run_id: string;
  state: string;
  owner?: string;
  created_at?: string;
  updated_at?: string;
  metric_names: string[];
  tags: Record<string, string>;
}

export interface RunPage extends ResponseMeta {
  runs: RunSummary[];
}

export interface MetricCatalogEntry {
  name: string;
  unit?: string;
  description?: string;
  min_step?: number;
  max_step?: number;
  point_count?: number;
}

export interface MetricCatalog extends ResponseMeta {
  run_id: string;
  metrics: MetricCatalogEntry[];
}

export interface RunDetail extends ResponseMeta {
  run: RunSummary;
  experiment_id?: string;
  project: string;
  started_at?: string;
  completed_at?: string;
  result_uri?: string;
}

export interface SeriesPoint {
  step: number;
  value: number;
  wall_time?: string;
}

export interface MetricSeries extends ResponseMeta {
  metric: string;
  run_id: string;
  points: SeriesPoint[];
  start_step?: number;
  end_step?: number;
  step_interval?: number;
  max_points?: number;
}

type JSONObject = Record<string, unknown>;

function object(value: unknown): JSONObject {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as JSONObject : {};
}
function string(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}
function number(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}
function boolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined;
}
function array(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}
function field(body: JSONObject, ...names: string[]): unknown {
  for (const name of names) if (body[name] !== undefined) return body[name];
}
function payload(value: unknown): JSONObject {
  const body = object(value);
  const nested = field(body, 'data', 'result', 'payload');
  return nested && typeof nested === 'object' && !Array.isArray(nested) ? object(nested) : body;
}
function strings(value: unknown): string[] {
  return array(value).map(string).filter((entry): entry is string => !!entry);
}
function record(value: unknown): Record<string, string> {
  return Object.fromEntries(Object.entries(object(value)).flatMap(([key, entry]) => {
    const decoded = string(entry);
    return decoded === undefined ? [] : [[key, decoded]];
  }));
}
function descriptor(value: unknown, preferred: string[]): string | undefined {
  const direct = string(value);
  if (direct) return direct;
  const body = object(value);
  const parts = preferred.flatMap(name => {
    const entry = field(body, name);
    return typeof entry === 'number' && Number.isFinite(entry) ? [String(entry)] : string(entry) ? [string(entry)!] : [];
  });
  return parts.length ? parts.join(' · ') : undefined;
}
function meta(value: unknown, data = payload(value)): ResponseMeta {
  const body = object(value);
  const metadata = object(field(body, 'meta', 'metadata'));
  const availability = descriptor(field(data, 'availability'), ['state']) ||
    descriptor(field(metadata, 'availability'), ['state']) ||
    descriptor(field(body, 'availability'), ['state']) || 'unknown';
  const warnings = [
    ...strings(field(data, 'warnings')),
    ...strings(field(metadata, 'warnings')),
    ...strings(field(body, 'warnings')),
  ];
  const partial = boolean(field(data, 'partial')) ?? boolean(field(metadata, 'partial')) ??
    boolean(field(body, 'partial')) ?? availability === 'partial';
  return {
    availability: ['available', 'degraded', 'partial', 'unavailable'].includes(availability)
      ? availability as Availability : 'unknown',
    partial,
    provenance: descriptor(field(data, 'provenance', 'source'), ['source', 'backend', 'store', 'detail']) ||
      descriptor(field(metadata, 'provenance', 'source'), ['source', 'backend', 'store', 'detail']),
    freshness: descriptor(field(data, 'freshness', 'freshness_at', 'generated_at', 'updated_at'), ['as_of', 'generated_at', 'age_seconds', 'state']) ||
      descriptor(field(metadata, 'freshness', 'freshness_at', 'generated_at'), ['as_of', 'generated_at', 'age_seconds', 'state']),
    warnings: [...new Set(warnings)],
    next_cursor: string(field(data, 'next_cursor', 'nextCursor')) ||
      string(field(metadata, 'next_cursor', 'nextCursor')) || string(field(body, 'next_cursor', 'nextCursor')),
  };
}
function decodeExperiment(value: unknown): ExperimentSummary | undefined {
  const body = object(value);
  const experiment_id = string(field(body, 'experiment_id', 'experimentId', 'target', 'id'));
  if (!experiment_id) return undefined;
  return {
    experiment_id,
    project: string(field(body, 'project', 'project_id', 'projectId')) || '',
    name: string(field(body, 'name', 'display_name', 'displayName')) || experiment_id,
    description: string(field(body, 'description', 'summary')),
    run_count: number(field(body, 'run_count', 'runCount', 'runs')),
    latest_run_at: string(field(body, 'latest_run_at', 'latestRunAt', 'updated_at', 'updatedAt')),
  };
}
function decodeRun(value: unknown): RunSummary | undefined {
  const body = object(value);
  const run_id = string(field(body, 'run_id', 'runId', 'id'));
  if (!run_id) return undefined;
  return {
    run_id,
    state: string(field(body, 'lifecycle_state', 'lifecycleState', 'state', 'status')) || 'unknown',
    owner: string(field(body, 'owner', 'created_by', 'createdBy')),
    created_at: string(field(body, 'created_at', 'createdAt')),
    updated_at: string(field(body, 'updated_at', 'updatedAt', 'completed_at', 'completedAt')),
    metric_names: strings(field(body, 'metric_names', 'metricNames', 'metrics')),
    tags: record(field(body, 'tags', 'labels')),
  };
}
function decodeMetric(value: unknown): MetricCatalogEntry | undefined {
  if (typeof value === 'string') return { name: value };
  const body = object(value);
  const name = string(field(body, 'name', 'metric_name', 'metricName', 'id'));
  if (!name) return undefined;
  return {
    name,
    unit: string(field(body, 'unit')),
    description: string(field(body, 'description')),
    min_step: number(field(body, 'min_step', 'minStep')),
    max_step: number(field(body, 'max_step', 'maxStep')),
    point_count: number(field(body, 'point_count', 'pointCount', 'count')),
  };
}
function decodePoint(value: unknown): SeriesPoint | undefined {
  if (Array.isArray(value)) {
    const step = number(value[0]), pointValue = number(value[1]);
    return step === undefined || pointValue === undefined ? undefined : { step, value: pointValue };
  }
  const body = object(value);
  const step = number(field(body, 'step', 'x')), pointValue = number(field(body, 'value', 'y'));
  if (step === undefined || pointValue === undefined) return undefined;
  return { step, value: pointValue, wall_time: string(field(body, 'wall_time', 'wallTime', 'time')) };
}

export function decodeExperimentPage(value: unknown): ExperimentPage {
  const data = payload(value);
  return { ...meta(value, data), experiments: array(field(data, 'experiments', 'items', 'results')).map(decodeExperiment).filter((item): item is ExperimentSummary => !!item) };
}
export function decodeRunPage(value: unknown): RunPage {
  const data = payload(value);
  return { ...meta(value, data), runs: array(field(data, 'runs', 'items', 'results')).map(decodeRun).filter((item): item is RunSummary => !!item) };
}
export function decodeRunDetail(value: unknown): RunDetail {
  const data = payload(value);
  const rawRun = object(field(data, 'run', 'detail'));
  const run = decodeRun(Object.keys(rawRun).length ? rawRun : data);
  if (!run) throw new Error('Run detail response did not contain a run identifier.');
  return {
    ...meta(value, data),
    run,
    experiment_id: string(field(data, 'experiment_id', 'experimentId', 'target')),
    project: string(field(data, 'project', 'project_id', 'projectId')) ||
      string(field(rawRun, 'project', 'project_id', 'projectId')) || '',
    started_at: string(field(data, 'started_at', 'startedAt')) || string(field(rawRun, 'started_at', 'startedAt')),
    completed_at: string(field(data, 'completed_at', 'completedAt')) || string(field(rawRun, 'completed_at', 'completedAt')),
    result_uri: string(field(data, 'result_uri', 'resultUri')) || string(field(rawRun, 'result_uri', 'resultUri')),
  };
}
export function decodeMetricCatalog(value: unknown): MetricCatalog {
  const data = payload(value);
  const rawCatalogValue = field(data, 'metric_catalog', 'metricCatalog', 'metrics');
  const rawCatalog = Array.isArray(rawCatalogValue) ? rawCatalogValue :
    field(object(rawCatalogValue), 'metrics', 'items', 'entries');
  return {
    ...meta(value, data),
    run_id: string(field(data, 'run_id', 'runId')) || '',
    metrics: array(rawCatalog).map(decodeMetric).filter((item): item is MetricCatalogEntry => !!item),
  };
}
export function decodeMetricSeries(value: unknown): MetricSeries {
  const data = payload(value);
  const nestedSeries = array(field(data, 'series'))[0];
  const series = Object.keys(object(nestedSeries)).length ? object(nestedSeries) : data;
  return {
    ...meta(value, data),
    metric: string(field(data, 'metric', 'metric_name', 'metricName')) || string(field(series, 'metric', 'metric_name')) || '',
    run_id: string(field(data, 'run_id', 'runId')) || string(field(series, 'run_id', 'runId')) || '',
    points: array(field(series, 'points', 'values', 'data')).map(decodePoint).filter((item): item is SeriesPoint => !!item),
    start_step: number(field(data, 'start_step', 'startStep')),
    end_step: number(field(data, 'end_step', 'endStep')),
    step_interval: number(field(data, 'step_interval', 'stepInterval')),
    max_points: number(field(data, 'max_points', 'maxPoints')),
  };
}
