// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useQuery } from '@tanstack/react-query';
import { boardScopeKey, fetchJSON, scopedURL, useWorkspace } from '../data';
import { stellarURL } from './api';
import {
  decodeExperimentFaultEvents, decodeExperimentPage, decodeMetricCatalog, decodeMetricSeries, decodeRunDetail, decodeRunPage,
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

export function useLegacyExperimentResolverQuery(experimentID: string, project: string) {
  const { scope, managed } = useWorkspace();
  const initialPath = stellarURL('experiments/search', {
    q: experimentID, project: project || undefined, limit: 50,
  });
  const initialURL = scopedURL(initialPath, scope.workspace, managed, scope.source);
  return useQuery({
    queryKey: [...boardScopeKey(scope, managed), 'legacy-experiment-resolver', initialURL],
    queryFn: async ({ signal }) => {
      let cursor = '';
      let lastPage = decodeExperimentPage(await fetchJSON<unknown>(initialURL, signal));
      const exact = lastPage.experiments.filter(experiment =>
        experiment.experiment_id === experimentID && (!project || experiment.project === project));
      const seen = new Set<string>();
      while (lastPage.next_cursor && (!project || exact.length === 0) && exact.length < 2) {
        if (seen.has(lastPage.next_cursor)) throw new Error('Experiment search returned a repeated cursor.');
        seen.add(lastPage.next_cursor);
        cursor = lastPage.next_cursor;
        const path = stellarURL('experiments/search', {
          q: experimentID, project: project || undefined, limit: 50, cursor,
        });
        const url = scopedURL(path, scope.workspace, managed, scope.source);
        lastPage = decodeExperimentPage(await fetchJSON<unknown>(url, signal));
        exact.push(...lastPage.experiments.filter(experiment =>
          experiment.experiment_id === experimentID && (!project || experiment.project === project)));
      }
      return { ...lastPage, experiments: exact, next_cursor: undefined };
    },
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

const lifecycleFilters = new Map([
  ['pending', 'pending'],
  ['running', 'running'],
  ['failed', 'failed'],
  ['succeeded', 'succeeded'],
  ['success', 'succeeded'],
  ['successful', 'succeeded'],
  ['completed', 'succeeded'],
  ['incomplete', 'incomplete'],
]);

export function useRunsQuery(experiment: string, input: { project: string; filter: string; cursor: string }) {
  const normalizedFilter = input.filter.trim().toLowerCase();
  const lifecycle = lifecycleFilters.get(normalizedFilter);
  return useStellarQuery(stellarURL(`experiments/${segment(experiment)}/runs`, {
    project: input.project || undefined,
    q: lifecycle ? undefined : input.filter || undefined,
    lifecycle,
    limit: 100,
    cursor: input.cursor || undefined,
  }), decodeRunPage, !!experiment);
}

export function useRunDetailQuery(experiment: string, runID: string, project: string) {
  return useStellarQuery(stellarURL(`runs/${segment(runID)}`, {
    project: project || undefined, target: experiment,
  }), decodeRunDetail, !!experiment && !!runID);
}

export function useExperimentFaultEventsQuery(experiment: string) {
  return useStellarQuery(
    `/api/portal/experiments/${segment(experiment)}/fault-events`,
    decodeExperimentFaultEvents,
    !!experiment,
  );
}

export function useRunResolverQuery(runID: string, project: string, enabled = true) {
  return useStellarQuery(stellarURL(`runs/${segment(runID)}`, {
    project: project || undefined,
  }), decodeRunDetail, enabled && !!runID);
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
