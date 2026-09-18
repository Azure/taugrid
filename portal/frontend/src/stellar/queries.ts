// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useQuery } from '@tanstack/react-query';
import { boardScopeKey, fetchJSON, scopedURL, useWorkspace } from '../data';
import { stellarURL } from './api';
import {
  decodeExperimentPage, decodeMetricCatalog, decodeMetricSeries, decodeRunDetail, decodeRunPage,
} from './contracts';

function segment(value: string) {
  return encodeURIComponent(value);
}

function useStellarQuery<T>(path: string, decode: (value: unknown) => T, enabled = true) {
  const { scope, managed } = useWorkspace();
  const url = scopedURL(path, scope.workspace, managed, scope.source);
  return useQuery({
    queryKey: [...boardScopeKey(scope, managed), url],
    queryFn: async ({ signal }) => decode(await fetchJSON<unknown>(url, signal)),
    enabled,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

export function useExperimentsQuery(input: { q: string; project: string; cursor: string }) {
  return useStellarQuery(stellarURL('experiments/search', {
    q: input.q || undefined, project: input.project || undefined, limit: 50, cursor: input.cursor || undefined,
  }), decodeExperimentPage);
}

export function useRunsQuery(experiment: string, input: { project: string; filter: string; cursor: string }) {
  return useStellarQuery(stellarURL(`experiments/${segment(experiment)}/runs`, {
    project: input.project || undefined, q: input.filter || undefined, limit: 100, cursor: input.cursor || undefined,
  }), decodeRunPage, !!experiment);
}

export function useRunDetailQuery(experiment: string, runID: string, project: string) {
  return useStellarQuery(stellarURL(`runs/${segment(runID)}`, {
    project: project || undefined, target: experiment,
  }), decodeRunDetail, !!experiment && !!runID);
}

export function useMetricCatalogQuery(experiment: string, runID: string, project: string) {
  return useStellarQuery(stellarURL(`runs/${segment(runID)}/metrics`, {
    project: project || undefined, target: experiment,
  }), decodeMetricCatalog, !!experiment && !!runID);
}

export interface SeriesRange {
  startStep?: number;
  endStep?: number;
  stepInterval?: number;
  maxPoints: number;
}

export function useMetricSeriesQuery(experiment: string, runID: string, metric: string, project: string, range: SeriesRange) {
  return useStellarQuery(stellarURL(`runs/${segment(runID)}/series`, {
    project: project || undefined,
    target: experiment,
    metric,
    start_step: range.startStep,
    end_step: range.endStep,
    step_interval: range.stepInterval,
    max_points: range.maxPoints,
  }), decodeMetricSeries, !!experiment && !!runID && !!metric);
}
