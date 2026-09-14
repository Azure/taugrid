// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import type { WorkspaceScope } from '../types';
import type { Run, Snapshot } from './types';

export const MAX_PINS = 14;
export const RUN_PAGE_SIZE = 200;
export const MAX_RUNS = 1000;
export const SECTION_CATALOG = [
  ['charts', 'Scalar chart grid'], ['timeline', 'Selected metric timeline'], ['media', 'Output media'],
  ['errors', 'Error analysis'], ['labels', 'Per-label validation'], ['repro', 'Reproducibility / evidence'],
  ['catalog', 'Metric catalog'], ['runs', 'Run comparison'], ['evidence', 'Evidence browser'],
] as const;
export type SectionID = typeof SECTION_CATALOG[number][0];
export interface Section { id: SectionID; title: string; subtitle: string; visible: boolean }
export function defaultSections(): Section[] {
  return SECTION_CATALOG.map(([id, title]) => ({ id, title, subtitle: '', visible: true }));
}
export function scopeIdentity(scope: WorkspaceScope) {
  return JSON.stringify([scope.workspace, scope.cluster, scope.namespace, scope.localQueue, scope.source,
    scope.resultScope, scope.authorizationMode, scope.experimentsUrl, scope.experimentsNative]);
}
export function preferenceKey(scope: WorkspaceScope, target: string) {
  return 'stellar:native:' + scopeIdentity(scope) + ':' + target;
}
export function metricList(value: unknown): string[] {
  const entries: unknown[] = typeof value === 'string' ? value.split(',') : Array.isArray(value) ? value : [];
  return [...new Set(entries.filter((v): v is string => typeof v === 'string').map(v => v.trim()).filter(Boolean))].slice(0, MAX_PINS);
}
export function defaultMetrics(snapshot?: Snapshot): string[] {
  const available = new Set([...(snapshot?.metric_options || []).map(option => option.name),
    ...(snapshot?.cards || []).flatMap(card => card.metrics.map(metric => metric.name)),
    ...(snapshot?.runs || []).flatMap(run => run.metric_names || [])]);
  const primary = ['train/loss', 'train/lr', 'eval/mean_episode_return', 'eval/reward', 'eval/return',
    'eval/success_rate', 'eval/win_rate', 'eval/pass_rate', 'eval/accuracy', 'eval/macro_auprc',
    'eval/macro_auroc', 'eval/macro_f1', 'eval/brier', 'eval/ece', 'eval/score', 'detect/macro_auprc',
    'detect/macro_auroc', 'detect/macro_sensitivity', 'detect/macro_specificity',
    'detect/macro_precision', 'detect/macro_f1', 'detect/macro_accuracy'];
  return metricList([...primary.filter(name => available.has(name)),
    ...(snapshot?.metric_options || []).filter(option => option.selected).map(option => option.name),
    ...(snapshot?.chart?.metric_name ? [snapshot.chart.metric_name] : []),
    ...available].filter(name => !name.endsWith('/count') || !available.has(name.replace(/\/count$/, '/mean'))));
}
export function readPreferences(key: string): { metrics?: string[]; sections?: Section[] } {
  try {
    const saved: unknown = JSON.parse(localStorage.getItem(key) || '{}');
    if (!saved || typeof saved !== 'object') return {};
    const metrics = 'metrics' in saved && Array.isArray(saved.metrics) ? metricList(saved.metrics) : undefined;
    const raw = 'sections' in saved && Array.isArray(saved.sections) ? saved.sections : [];
    const sections: Section[] = [];
    for (const item of raw) {
      if (!item || typeof item !== 'object') continue;
      const match = defaultSections().find(section => section.id === item.id);
      if (!match || sections.some(section => section.id === match.id)) continue;
      sections.push({ ...match, title: typeof item.title === 'string' ? item.title.slice(0, 120) : match.title,
        subtitle: typeof item.subtitle === 'string' ? item.subtitle.slice(0, 300) : '',
        visible: item.visible !== false });
    }
    return { metrics, sections: sections.length ? [...sections, ...defaultSections().filter(s => !sections.some(v => v.id === s.id))] : undefined };
  } catch { return {}; }
}
export function savePreferences(key: string, metrics: string[] | undefined, sections: Section[]) {
  try { localStorage.setItem(key, JSON.stringify({ metrics, sections })); }
  catch { /* All controls remain usable when storage is blocked. */ }
}
export function sectionsFromURL(params: URLSearchParams, saved?: Section[]): Section[] {
  const defaults = saved || defaultSections();
  const ids = params.has('sections') ? [...new Set((params.get('sections') || '').split(','))] : undefined;
  const ordered = ids ? [...ids.flatMap(id => defaults.filter(s => s.id === id)), ...defaults.filter(s => !ids.includes(s.id))] : defaults;
  return ordered.map(section => ({ ...section, visible: ids ? ids.includes(section.id) : section.visible,
    title: params.get(`section.${section.id}.title`) ?? section.title,
    subtitle: params.get(`section.${section.id}.subtitle`) ?? section.subtitle }));
}
export function runLifecycle(run: Run): string {
  const authoritative = ('outcome_state' in run && run.outcome_state) || ('liveness_state' in run && run.liveness_state);
  if (authoritative) return authoritative.toLowerCase();
  const state = (run.lifecycle_state || (run.successful ? 'succeeded' : run.state) || 'pending').toLowerCase();
  return state === 'stale' ? 'not_responding' : state;
}
export function runTimestamp(run: Run): string {
  return ('updated_at' in run && run.updated_at) || run.completed_at || run.started_at || run.created_at || '';
}
export interface RunFilters { search: string; group: string; lifecycle: string; updated: string; sort: string }
export function filterRuns(runs: Run[], filters: RunFilters, now = Date.now()): Run[] {
  const query = filters.search.trim().toLowerCase();
  const duration: Record<string, number> = { '1h': 3600000, '24h': 86400000, '7d': 604800000 };
  const result = runs.filter(run => {
    if (filters.group && run.run_group_id !== filters.group) return false;
    if (filters.lifecycle && runLifecycle(run) !== filters.lifecycle) return false;
    const time = Date.parse(runTimestamp(run));
    if (filters.updated === 'missing' && Number.isFinite(time)) return false;
    if (duration[filters.updated] && (!Number.isFinite(time) || now - time > duration[filters.updated])) return false;
    return !query || [run.run_id, run.project, 'experiment_id' in run && run.experiment_id,
      run.run_group_id, run.state, run.lifecycle_state, run.owner, runTimestamp(run),
      ...(run.metric_names || []), ...Object.entries(run.tags || {}).flatMap(([k, v]) => [k, v, `${k}=${v}`])]
      .some(value => typeof value === 'string' && value.toLowerCase().includes(query));
  });
  const sort = filters.sort || (filters.updated ? 'desc' : '');
  return !sort ? result : result.sort((a, b) => {
    const delta = (Date.parse(runTimestamp(a)) || 0) - (Date.parse(runTimestamp(b)) || 0);
    return (sort === 'asc' ? delta : -delta) || a.run_id.localeCompare(b.run_id);
  });
}
export function mergeRuns(primary: Run[], additional: Run[]) {
  const seen = new Set<string>();
  return [...primary, ...additional].filter(run => {
    if (!run.run_id || seen.has(run.run_id)) return false;
    seen.add(run.run_id); return true;
  });
}
export function refreshEnabled(params: URLSearchParams) {
  const raw = params.get('refresh_ms') ?? params.get('refresh') ?? params.get('auto_refresh');
  return raw === null || !['0', 'false', 'off', 'no'].includes(raw.toLowerCase());
}
