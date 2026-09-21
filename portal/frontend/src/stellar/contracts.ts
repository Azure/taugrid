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
  created_at: string;
  updated_at: string;
  run_count: number;
  run_group_count: number;
  lifecycle_counts: Record<string, number>;
  latest_run_at?: string;
  metric_names: string[];
}

export interface ExperimentPage extends ResponseMeta {
  experiments: ExperimentSummary[];
}

export interface RunSummary {
  run_id: string;
  experiment_id?: string;
  project: string;
  run_group_id: string;
  parent_run_id?: string;
  state: string;
  lifecycle_state: string;
  successful: boolean;
  success_reasons: string[];
  owner?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
  result_uri?: string;
  tags: Record<string, string>;
  metric_names: string[];
  source: string;
}

export interface RunPage extends ResponseMeta {
  target: string;
  runs: RunSummary[];
}

export interface MetricCatalogEntry {
  name: string;
  min_step?: number;
  max_step?: number;
  latest_step?: number;
  latest_value?: number;
  updated_at?: string;
}

export interface MetricCatalog extends ResponseMeta {
  run_id: string;
  metrics: MetricCatalogEntry[];
}

export interface RunDetail extends ResponseMeta {
  run: RunSummary;
}

export interface SeriesPoint {
  step: number;
  value: number;
}

export interface MetricSeries extends ResponseMeta {
  target: string;
  metric: string;
  run_id: string;
  points: SeriesPoint[];
  start_step?: number;
  end_step?: number;
  step_interval?: number;
  max_points: number;
  source_points: number;
  returned_points: number;
}

type JSONObject = Record<string, unknown>;

function object(value: unknown, name: string): JSONObject {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${name} must be an object.`);
  return value as JSONObject;
}

function array(value: unknown, name: string): unknown[] {
  if (!Array.isArray(value)) throw new Error(`${name} must be an array.`);
  return value;
}

function requiredString(body: JSONObject, name: string): string {
  const value = body[name];
  if (typeof value !== 'string') throw new Error(`${name} must be a string.`);
  return value;
}

function optionalString(body: JSONObject, name: string): string | undefined {
  const value = body[name];
  if (value === undefined) return undefined;
  if (typeof value !== 'string') throw new Error(`${name} must be a string.`);
  return value || undefined;
}

function requiredNumber(body: JSONObject, name: string): number {
  const value = body[name];
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new Error(`${name} must be a finite number.`);
  return value;
}

function optionalNumber(body: JSONObject, name: string): number | undefined {
  const value = body[name];
  if (value === undefined) return undefined;
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new Error(`${name} must be a finite number.`);
  return value;
}

function requiredBoolean(body: JSONObject, name: string): boolean {
  const value = body[name];
  if (typeof value !== 'boolean') throw new Error(`${name} must be a boolean.`);
  return value;
}

function stringArray(value: unknown, name: string): string[] {
  return array(value, name).map((entry, index) => {
    if (typeof entry !== 'string') throw new Error(`${name}[${index}] must be a string.`);
    return entry;
  });
}

function stringRecord(value: unknown, name: string): Record<string, string> {
  const body = object(value, name);
  return Object.fromEntries(Object.entries(body).map(([key, entry]) => {
    if (typeof entry !== 'string') throw new Error(`${name}.${key} must be a string.`);
    return [key, entry];
  }));
}

function numberRecord(value: unknown, name: string): Record<string, number> {
  if (value === undefined) return {};
  const body = object(value, name);
  return Object.fromEntries(Object.entries(body).map(([key, entry]) => {
    if (typeof entry !== 'number' || !Number.isFinite(entry)) throw new Error(`${name}.${key} must be a finite number.`);
    return [key, entry];
  }));
}

function decodeMeta(body: JSONObject): ResponseMeta {
  const metadata = object(body.metadata, 'metadata');
  const availability = object(metadata.availability, 'metadata.availability');
  const provenance = object(metadata.provenance, 'metadata.provenance');
  const freshness = object(metadata.freshness, 'metadata.freshness');
  const state = requiredString(availability, 'state');
  const servedSources = stringArray(provenance.served_sources, 'metadata.provenance.served_sources');
  const reasons = availability.reasons === undefined ? [] : stringArray(availability.reasons, 'metadata.availability.reasons');
  const warnings = metadata.warnings === undefined ? [] : stringArray(metadata.warnings, 'metadata.warnings');
  return {
    availability: ['available', 'degraded', 'partial', 'unavailable'].includes(state) ? state as Availability : 'unknown',
    partial: requiredBoolean(metadata, 'partial'),
    provenance: servedSources.length ? servedSources.join(', ') : undefined,
    freshness: [requiredString(freshness, 'state'), requiredString(freshness, 'as_of')].filter(Boolean).join(' · '),
    warnings: [...reasons, ...warnings],
    next_cursor: optionalString(body, 'next_cursor'),
  };
}

function decodeExperiment(value: unknown): ExperimentSummary {
  const body = object(value, 'experiment');
  return {
    experiment_id: requiredString(body, 'experiment_id'),
    project: requiredString(body, 'project'),
    name: requiredString(body, 'name'),
    description: optionalString(body, 'description'),
    created_at: requiredString(body, 'created_at'),
    updated_at: requiredString(body, 'updated_at'),
    run_count: requiredNumber(body, 'run_count'),
    run_group_count: requiredNumber(body, 'run_group_count'),
    lifecycle_counts: numberRecord(body.lifecycle_counts, 'lifecycle_counts'),
    latest_run_at: optionalString(body, 'latest_run_at'),
    metric_names: stringArray(body.metric_names, 'metric_names'),
  };
}

function decodeRun(value: unknown): RunSummary {
  const body = object(value, 'run');
  return {
    run_id: requiredString(body, 'run_id'),
    experiment_id: optionalString(body, 'experiment_id'),
    project: requiredString(body, 'project'),
    run_group_id: requiredString(body, 'run_group_id'),
    parent_run_id: optionalString(body, 'parent_run_id'),
    state: requiredString(body, 'state'),
    lifecycle_state: requiredString(body, 'lifecycle_state'),
    successful: requiredBoolean(body, 'successful'),
    success_reasons: body.success_reasons === undefined ? [] : stringArray(body.success_reasons, 'success_reasons'),
    owner: optionalString(body, 'owner'),
    created_at: requiredString(body, 'created_at'),
    started_at: optionalString(body, 'started_at'),
    completed_at: optionalString(body, 'completed_at'),
    result_uri: optionalString(body, 'result_uri'),
    tags: stringRecord(body.tags, 'tags'),
    metric_names: stringArray(body.metric_names, 'metric_names'),
    source: requiredString(body, 'source'),
  };
}

function decodeMetric(value: unknown): MetricCatalogEntry {
  const body = object(value, 'metric');
  return {
    name: requiredString(body, 'name'),
    min_step: optionalNumber(body, 'min_step'),
    max_step: optionalNumber(body, 'max_step'),
    latest_step: optionalNumber(body, 'latest_step'),
    latest_value: optionalNumber(body, 'latest_value'),
    updated_at: optionalString(body, 'updated_at'),
  };
}

function decodePoint(value: unknown): SeriesPoint {
  const body = object(value, 'point');
  return { step: requiredNumber(body, 'step'), value: requiredNumber(body, 'value') };
}

export function decodeExperimentPage(value: unknown): ExperimentPage {
  const body = object(value, 'experiment search response');
  return { ...decodeMeta(body), experiments: array(body.experiments, 'experiments').map(decodeExperiment) };
}

export function decodeRunPage(value: unknown): RunPage {
  const body = object(value, 'run list response');
  return {
    ...decodeMeta(body),
    target: requiredString(body, 'target'),
    runs: array(body.runs, 'runs').map(decodeRun),
  };
}

export function decodeRunDetail(value: unknown): RunDetail {
  const body = object(value, 'run detail response');
  return { ...decodeMeta(body), run: decodeRun(body.run) };
}

export function decodeMetricCatalog(value: unknown): MetricCatalog {
  const body = object(value, 'metric catalog response');
  return {
    ...decodeMeta(body),
    run_id: requiredString(body, 'run_id'),
    metrics: array(body.metrics, 'metrics').map(decodeMetric),
  };
}

export function decodeMetricSeries(value: unknown): MetricSeries {
  const body = object(value, 'series response');
  return {
    ...decodeMeta(body),
    target: requiredString(body, 'target'),
    metric: requiredString(body, 'metric'),
    run_id: requiredString(body, 'run_id'),
    points: array(body.points, 'points').map(decodePoint),
    start_step: optionalNumber(body, 'start_step'),
    end_step: optionalNumber(body, 'end_step'),
    step_interval: optionalNumber(body, 'step_interval'),
    max_points: requiredNumber(body, 'max_points'),
    source_points: requiredNumber(body, 'source_points'),
    returned_points: requiredNumber(body, 'returned_points'),
  };
}
