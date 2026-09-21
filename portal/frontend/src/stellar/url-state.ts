// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useMemo } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { useScopedURL } from '../data';

export interface ExperimentURLState {
  q: string;
  project: string;
  experiment: string;
  run: string;
  metric: string;
  filter: string;
  cursor: string;
  startStep?: number;
  endStep?: number;
  stepInterval?: number;
  maxPoints: number;
}

function optionalNumber(params: URLSearchParams, name: string): number | undefined {
  const raw = params.get(name);
  if (raw === null || raw === '') return undefined;
  const value = Number(raw);
  return Number.isFinite(value) ? value : undefined;
}
function pointBudget(params: URLSearchParams): number {
  const value = optionalNumber(params, 'max_points') ?? 500;
  return Math.min(5000, Math.max(10, Math.round(value)));
}

export function useExperimentURLState() {
  const location = useLocation(), navigate = useNavigate(), scoped = useScopedURL();
  const params = useMemo(() => new URLSearchParams(location.search), [location.search]);
  const state: ExperimentURLState = {
    q: params.get('q') || '',
    project: params.get('project') || '',
    experiment: params.get('experiment') || '',
    run: params.get('run') || '',
    metric: params.get('metric') || '',
    filter: params.get('filter') || '',
    cursor: params.get('cursor') || '',
    startStep: optionalNumber(params, 'start_step'),
    endStep: optionalNumber(params, 'end_step'),
    stepInterval: optionalNumber(params, 'step_interval'),
    maxPoints: pointBudget(params),
  };
  function update(values: Partial<Record<keyof ExperimentURLState, string | number | undefined>>, replace = true) {
    const next = new URLSearchParams(location.search);
    const names: Record<keyof ExperimentURLState, string> = {
      q: 'q', project: 'project', experiment: 'experiment', run: 'run', metric: 'metric',
      filter: 'filter', cursor: 'cursor', startStep: 'start_step', endStep: 'end_step',
      stepInterval: 'step_interval', maxPoints: 'max_points',
    };
    if (values.experiment !== undefined && String(values.experiment) !== state.experiment) {
      for (const name of ['run', 'metric', 'filter', 'cursor', 'start_step', 'end_step', 'step_interval', 'max_points']) next.delete(name);
    } else if (values.run !== undefined && String(values.run) !== state.run) {
      for (const name of ['metric', 'start_step', 'end_step', 'step_interval', 'max_points']) next.delete(name);
    }
    if (values.q !== undefined || values.project !== undefined || values.filter !== undefined) next.delete('cursor');
    for (const [key, value] of Object.entries(values) as [keyof ExperimentURLState, string | number | undefined][]) {
      const name = names[key];
      if (value === undefined || value === '') next.delete(name);
      else next.set(name, String(value));
    }
    navigate(scoped('/portal/experiments' + (next.size ? `?${next}` : '') + location.hash), { replace });
  }
  return { state, update };
}
