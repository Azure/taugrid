// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useLocation, useNavigate } from 'react-router-dom';
import { boardScopeKey, experimentsAPI, readableQuery, requestRejected, staleReadMessage, useBoard, useScopedURL, useWorkspace } from '../data';
import { Empty, PageTitle } from '../components';
import { ChartWorkbench } from './ChartWorkbench';
import { ResearchEvidence } from './ResearchEvidence';
import { LaunchSummary } from './LaunchSummary';
import { labelGroups } from './evidence-helpers';
import { stellarURL } from './api';
import {
  defaultMetrics, defaultSections, filterRuns, MAX_PINS, MAX_RUNS, mergeRuns, metricList, preferenceKey, readPreferences,
  refreshEnabled, RUN_PAGE_SIZE, runLifecycle, runTimestamp, savePreferences, scopeIdentity, sectionsFromURL,
  type RunFilters, type Section,
} from './state';
import type { ExperimentSearchResult, Run, RunSearchResult, Snapshot } from './types';
import './workspace.css';

function QueryResult<T>({ query, name, children }: { query: ReturnType<typeof readableQuery<T>>; name: string; children: (data: T) => ReactNode }) {
  return <>{query.error && <div className="empty warn" role="alert">{name} unavailable: {query.error.message}
    {' '}{staleReadMessage(query)} <button type="button" onClick={() => void query.refetch()}>Retry</button></div>}
    {query.data ? children(query.data) : query.isPending ? <div className="empty" role="status">Loading {name.toLowerCase()}…</div> : null}</>;
}
function useURLState() {
  const location = useLocation(), navigate = useNavigate(), scoped = useScopedURL();
  const params = useMemo(() => new URLSearchParams(location.search), [location.search]);
  return { params, update: (values: Record<string, string | null>, replace = true) => {
    const next = new URLSearchParams(location.search);
    const target = 'target' in values ? values.target : params.get('target');
    const project = 'project' in values ? values.project : params.get('project');
    if ((target || '') !== (params.get('target') || '') || (project || '') !== (params.get('project') || '')) {
      for (const name of [...next.keys()]) {
        if (name.startsWith('section.') || name.startsWith('media_') ||
          ['metric', 'pinned', 'sections', 'panel', 'run_id', 'run_q', 'group', 'lifecycle', 'updated', 'updated_sort', 'start_step', 'end_step', 'step_interval', 'max_points', 'detail'].includes(name)) next.delete(name);
      }
    }
    for (const [name, value] of Object.entries(values)) value === null ? next.delete(name) : next.set(name, value);
    navigate(scoped('/portal/experiments' + (next.size ? '?' + next : '') + location.hash), { replace });
  } };
}
function RefreshControls({ target }: { target: string }) {
  const { params, update } = useURLState(), { scope, managed } = useWorkspace(), client = useQueryClient();
  const enabled = refreshEnabled(params);
  const [paused, setPaused] = useState(document.hidden);
  const [refreshing, setRefreshing] = useState(false);
  const prefix = boardScopeKey(scope, managed);
  const prefixIdentity = JSON.stringify(prefix);
  const refresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await client.invalidateQueries({ queryKey: prefix, predicate: query => {
        const path = query.queryKey.at(-1);
        if (typeof path !== 'string' || !path.startsWith('/api/v2/stellar/')) return false;
        const url = new URL(path, window.location.origin);
        return target ? url.searchParams.get('target') === target : url.pathname.endsWith('/experiments');
      }, refetchType: 'active' });
    } finally { setRefreshing(false); }
  };
  useEffect(() => {
    const isPaused = () => document.hidden || !!document.activeElement?.matches('input,textarea,select,[contenteditable="true"]');
    const changed = () => setPaused(isPaused());
    document.addEventListener('visibilitychange', changed);
    document.addEventListener('focusin', changed);
    document.addEventListener('focusout', changed);
    const timer = window.setInterval(() => {
      changed();
      if (enabled && !isPaused()) void refresh();
    }, 30000);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', changed);
      document.removeEventListener('focusin', changed);
      document.removeEventListener('focusout', changed);
    };
    // Restart the timer when the authorized scope, target, or refresh setting changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [prefixIdentity, target, enabled, client, refreshing]);
  return <div className="stellar-refresh">
    <label><input type="checkbox" aria-label="Auto-refresh every 30s" checked={enabled} onChange={e => update({ refresh_ms: null, auto_refresh: null, refresh: e.target.checked ? '30' : 'off' })}/>
      <span className="stellar-refresh-state" role="status" title={enabled && paused ? 'Paused while hidden or editing' : undefined}>
        {enabled ? paused ? 'auto paused' : 'auto 30s' : 'auto off'}
      </span></label><button type="button" disabled={refreshing} onClick={() => void refresh()}>{refreshing ? 'Refreshing…' : 'Refresh'}</button></div>;
}
function StellarHeader({ target }: { target: string }) {
  const { scope } = useWorkspace(), { params, update } = useURLState();
  const summary = readableQuery(useBoard<Snapshot>(stellarURL('snapshot', { target, mode: 'summary', project: params.get('project') || undefined }), !!target));
  const [search, setSearch] = useState(params.get('experiment_q') || '');
  useEffect(() => setSearch(params.get('experiment_q') || ''), [params]);
  return <header className={'stellar-app-topbar' + (target ? '' : ' is-discovery')}>
    <button className="stellar-topbar-home" type="button" aria-label="Return to experiment search" onClick={() => update({ target: null, project: null }, false)}>
      <strong>Experiments</strong>{target && <span title={target}>{target}</span>}
    </button>
    {target && <form className="stellar-experiment-search" onSubmit={event => { event.preventDefault(); update({ experiment_q: search, target: null, project: null }, false); }}>
      <input aria-label="Search experiments" type="search" placeholder="Search experiments" value={search} onChange={event => setSearch(event.target.value)}/>
      <button type="submit">Search</button>
    </form>}
    <div className="stellar-topbar-actions"><span className="stellar-meta-pill"><b>{scope.source === 'local' ? 'local expstore' : scope.source === 'kusto' ? 'Kusto/ADX' : scope.source}</b> source</span>
      {target && <QueryResult query={summary} name="Experiment header">{snapshot => <>
        <span className="stellar-meta-pill"><b>{snapshot.runs.length}</b> loaded runs</span><span className="stellar-meta-pill"><b>{snapshot.status.metric_files}</b> metric files</span>
      </>}</QueryResult>}
      <RefreshControls target={target}/>
    </div>
  </header>;
}
export function StellarWorkspace() {
  const { scope } = useWorkspace(), { params } = useURLState();
  const target = params.get('target') || '';
  if (!experimentsAPI(scope)) return <><PageTitle title="Experiments">Training curves, run comparison, metric summaries.</PageTitle>
    <div className="empty warn" role="alert"><strong>Experiment backend setup required</strong>
      <p>{scope.experimentsNative?.reason || 'Configure an authorized same-origin experiment backend for this workspace. A legacy remote page URL is not a trusted data connection.'}</p>
      <p>No local experiment data was used. Jobs remain available in the Workloads tab.</p></div></>;
  return <div className="stellar-workspace" key={scopeIdentity(scope)}>
    <StellarHeader target={target}/>
    {target ? <TargetWorkspace key={target + ':' + (params.get('project') || '')} target={target}/> : <ExperimentDiscovery/>}
  </div>;
}
function experimentKey(project: string, target: string) {
  return encodeURIComponent(project) + ':' + encodeURIComponent(target);
}
function ExperimentDiscovery() {
  const { params, update } = useURLState();
  const search = params.get('experiment_q') || '', project = params.get('experiment_project') ?? params.get('project') ?? '', tag = params.get('experiment_tag') || '';
  const query = readableQuery(useBoard<ExperimentSearchResult>(stellarURL('experiments', { q: search.trim(), project, tag, limit: 100 })));
  const [target, setTarget] = useState('');
  const expanded = new Set((params.get('experiments') || '').split(',').filter(Boolean));
  return <div className="stellar-discovery">
    <section className="stellar-landing-hero">
    <h1>Choose an experiment</h1>
    <p>Search experiments and open a labeled run dashboard when you are ready to inspect metrics.</p>
    <div className="stellar-filters stellar-landing-controls">
      <form className="stellar-experiment-search" onSubmit={event => { event.preventDefault(); void query.refetch(); }}>
        <input aria-label="Search experiments" type="search" value={search} onChange={e => update({ experiment_q: e.target.value })} placeholder="Search experiments"/>
        <button type="submit">Search</button>
      </form>
      <label>Project<input value={project} onChange={e => update({ experiment_project: e.target.value })} placeholder="All projects"/></label>
      <label className="stellar-landing-tag">Tag<input value={tag} onChange={e => update({ experiment_tag: e.target.value })} placeholder="key=value"/></label>
    </div>
    <div className="stellar-discovery-actions">
    <details className="stellar-open-target"><summary>Open a target</summary><form className="stellar-target" onSubmit={event => { event.preventDefault(); if (target.trim()) update({ target: target.trim() }, false); }}>
      <label>Open a target<input value={target} onChange={event => setTarget(event.target.value)} placeholder="Experiment, run group, or run ID"/></label>
      <button type="submit" disabled={!target.trim()}>Open target</button>
    </form></details>
    {(search || project || tag) && <button className="stellar-link" type="button" onClick={() => update({ experiment_q: null, experiment_project: null, experiment_tag: null, project: null })}>Clear filters</button>}
    </div>
    </section>
    <QueryResult query={query} name="Experiment discovery">{data => <>
      {data.truncated && <p className="muted">First 100 of {data.total} experiments shown; refine your search.</p>}
      {data.warnings?.map(warning => <p className="warn" key={warning}>{warning}</p>)}
      {!data.experiments?.length ? <Empty>No experiments match. Try another project or tag, or open a known target above.</Empty> :
        <div className="stellar-landing-table"><table><thead><tr>{['Experiment', 'Runs', 'Groups', 'Metrics', 'Latest', 'Status'].map(label => <th key={label} scope="col">{label}</th>)}</tr></thead>
          {data.experiments.map(experiment => <tbody key={experimentKey(experiment.project, experiment.experiment_id)}><tr>
          <td><button type="button" className="stellar-experiment-main" title={experiment.name || experiment.experiment_id} aria-label={experiment.name || experiment.experiment_id} onClick={() => update({ target: experiment.experiment_id, project: experiment.project }, false)}><strong>{experiment.name || experiment.experiment_id}</strong>{experiment.name && experiment.name !== experiment.experiment_id && <span>{experiment.experiment_id}</span>}</button></td>
          <td>{experiment.run_count}</td><td>{experiment.run_group_count}</td><td>{experiment.metric_names?.length ?? '—'}</td>
          <td><time dateTime={experiment.latest_run_at}>{experiment.latest_run_at && Number.isFinite(Date.parse(experiment.latest_run_at)) ? new Date(experiment.latest_run_at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) : '—'}</time></td>
          <td><div className="stellar-experiment-states">{Object.entries(experiment.lifecycle_counts || experiment.state_counts || {}).map(([state, count]) => <span className={state} key={state}>{state === 'succeeded' ? 'done' : state.replaceAll('_', ' ')} {count}</span>)}</div><details className="stellar-experiment-details"><summary>Details</summary>
            {experiment.description && <p>{experiment.description}</p>}
            <label><input type="checkbox" checked={expanded.has(experimentKey(experiment.project, experiment.experiment_id))} onChange={e => {
              const key = experimentKey(experiment.project, experiment.experiment_id);
              const next = new Set(expanded); e.target.checked ? next.add(key) : next.delete(key);
              update({ experiments: [...next].join(',') || null });
            }}/> Show runs for {experiment.name || experiment.experiment_id}</label>
          </details></td></tr>
          {expanded.has(experimentKey(experiment.project, experiment.experiment_id)) && <tr><td colSpan={6}><ExperimentRuns target={experiment.experiment_id} project={experiment.project}/></td></tr>}
        </tbody>)}</table></div>}
    </>}</QueryResult>
  </div>;
}
function RecentExperiments({ target }: { target: string }) {
  const { params, update } = useURLState();
  const query = readableQuery(useBoard<ExperimentSearchResult>(stellarURL('experiments', { limit: 100 })));
  return <section className="stellar-recent-experiments"><div><strong>Recent experiments</strong><button type="button" className="stellar-link" onClick={() => void query.refetch()}>refresh</button></div>
    {query.error && <p role="alert" className="warn">Recent experiments unavailable: {query.error.message} {staleReadMessage(query)}</p>}
    {(query.data?.experiments || []).slice(0, 8).map(experiment => <button type="button" className={target === experiment.experiment_id && (params.get('project') || '') === experiment.project ? 'selected' : ''}
      key={experimentKey(experiment.project, experiment.experiment_id)} onClick={() => update({ target: experiment.experiment_id, project: experiment.project, metric: null, pinned: null, panel: null }, false)}>
      <strong>{experiment.name || experiment.experiment_id}</strong>{experiment.name && experiment.name !== experiment.experiment_id && <span>{experiment.experiment_id}</span>}<em>{experiment.run_count} runs</em>
    </button>)}
  </section>;
}
function latestRunValue(run: Run, snapshot: Snapshot | undefined, metric: string) {
  const summary = 'metrics' in run ? run.metrics?.find(value => value.metric_name === metric) : undefined;
  const values = snapshot?.chart?.series?.filter(series => series.run_id === run.run_id).flatMap(series => series.values || []) || [];
  const latest = values.reduce<(typeof values)[number] | undefined>((a, b) => !a || b.step > a.step ? b : a, undefined);
  const summaryValue = summary?.latest_value ?? ((summary?.finite_count ?? 0) > 0 ? 0 : undefined);
  const value = summaryValue !== undefined && (!latest || (summary?.latest_step ?? -Infinity) >= latest.step) ? summaryValue : latest?.value;
  return value !== undefined && Number.isFinite(value) ? value.toFixed(3) : '—';
}
function ExperimentRuns({ target, project }: { target: string; project: string }) {
  const query = readableQuery(useBoard<RunSearchResult>(stellarURL('runs', { target, project, limit: RUN_PAGE_SIZE })));
  const { update } = useURLState();
  return <QueryResult query={query} name={`Runs for ${target}`}>{data => <ul className="stellar-preview-runs">{data.runs?.map(run => <li key={run.run_id}>
    <button type="button" className="stellar-link" onClick={() => update({ target: run.run_id, project }, false)}>{run.run_id}</button> <span>{runLifecycle(run).replaceAll('_', ' ')}</span>
  </li>)}{!data.runs?.length && <li>No runs recorded.</li>}{data.truncated && <li>First 200 runs shown. Open the experiment to load more.</li>}</ul>}</QueryResult>;
}
function TargetWorkspace({ target }: { target: string }) {
  const { scope } = useWorkspace(), { params, update } = useURLState();
  const key = preferenceKey(scope, (params.get('project') || '') + ':' + target);
  const [saved, setSaved] = useState(() => readPreferences(key));
  const [limit, setLimit] = useState(RUN_PAGE_SIZE);
  const [hidden, setHidden] = useState<Set<string>>(() => new Set());
  const [previousPage, setPreviousPage] = useState<{ data: RunSearchResult; dataUpdatedAt: number }>();
  const [railOpen, setRailOpen] = useState(() => window.innerWidth > 1040);
  const [summaryOpen, setSummaryOpen] = useState(false);
  const [launchQueryError, setLaunchQueryError] = useState(false);
  useEffect(() => {
    const media = window.matchMedia?.('(max-width: 1040px)');
    if (!media) return;
    const changed = () => setRailOpen(!media.matches);
    media.addEventListener('change', changed);
    return () => media.removeEventListener('change', changed);
  }, []);
  const query = readableQuery(useBoard<Snapshot>(stellarURL('snapshot', { target, mode: 'summary', project: params.get('project') || undefined })));
  const more = readableQuery(useBoard<RunSearchResult>(stellarURL('runs', { target, limit, project: params.get('project') || undefined }), limit > RUN_PAGE_SIZE));
  useEffect(() => {
    if (requestRejected(more.error) || requestRejected(query.error)) setPreviousPage(undefined);
    else if (more.data) setPreviousPage({ data: more.data, dataUpdatedAt: more.dataUpdatedAt });
  }, [more.data, more.dataUpdatedAt, more.error, query.error]);
  const sections = sectionsFromURL(params, saved.sections);
  const visibleSections = sections.filter(section => section.visible);
  const requestedPanel = params.get('panel') || (
    [...params.keys()].some(name => name.startsWith('media_')) ? 'media' :
    ['detail', 'start_step', 'end_step', 'run_id'].some(name => params.has(name)) ? 'timeline' :
    params.has('metric') ? 'timeline' : undefined);
  const sectionTarget = visibleSections.find(section => section.id === requestedPanel)?.id;
  useEffect(() => {
    if (!sectionTarget || !query.data) return;
    const element = document.getElementById('stellar-section-' + sectionTarget);
    element?.scrollIntoView?.({ block: 'start' });
    element?.focus({ preventScroll: true });
  }, [sectionTarget, params.get('metric'), query.data !== undefined]);
  const explicitPins = params.has('pinned') ? metricList(params.get('pinned')) : params.has('metric') ? metricList(params.get('metric')) : saved.metrics;
  const metrics = explicitPins ?? defaultMetrics(query.data);
  const focusMetric = params.get('metric') || metrics[0] || '';
  const focused = readableQuery(useBoard<Snapshot>(stellarURL('snapshot', { target, source: scope.source, project: params.get('project') || undefined, metric: focusMetric, mode: 'metric', include_static: false }), !!focusMetric && !!query.data));
  const filters: RunFilters = { search: params.get('run_q') || '', group: params.get('group') || '',
    lifecycle: (params.get('lifecycle') || '').replace(/^stale$/, 'not_responding'),
    updated: params.get('updated') || '', sort: params.get('updated_sort') || '' };
  const page = requestRejected(more.error) || !query.data ? undefined : more.data || previousPage?.data;
  const runs = query.data ? mergeRuns(query.data.runs || [], page?.runs || []) : [];
  const augmentedSnapshot = query.data ? { ...query.data, runs: runs.map(run =>
    'systems' in run ? run : { ...run, systems: [], observe_cli: '' }) } : undefined;
  const listed = filterRuns(runs, filters);
  const visibleRunIds = listed.filter(run => !hidden.has(run.run_id)).map(run => run.run_id);
  const total = Math.max(query.data?.status?.runs || 0, page?.total || 0, runs.length);
  const canLoad = limit < MAX_RUNS && (page ? page.truncated : total > runs.length || runs.length >= RUN_PAGE_SIZE);
  function setMetrics(next: string[]) {
    const pins = metricList(next);
    setSaved(value => ({ ...value, metrics: pins }));
    savePreferences(key, pins, sections);
    update({ pinned: pins.join(','), metric: pins.includes(focusMetric) ? focusMetric : pins[0] || null });
  }
  function setSections(next: Section[]) {
    setSaved(value => ({ ...value, sections: next }));
    savePreferences(key, explicitPins, next);
    const values: Record<string, string | null> = { sections: next.filter(section => section.visible).map(section => section.id).join(',') };
    for (const section of next) {
      values[`section.${section.id}.title`] = section.title;
      values[`section.${section.id}.subtitle`] = section.subtitle || null;
    }
    update(values);
  }
  const settings = <div className="stellar-target-heading"><button type="button" className="stellar-link" onClick={() => {
      const changes: Record<string, null> = { target: null, metric: null, pinned: null, run_id: null, lifecycle: null,
        updated: null, updated_sort: null, run_q: null, group: null, sections: null, project: null, panel: null };
      for (const name of params.keys()) if (name.startsWith('section.') || name.startsWith('media_') || ['start_step', 'end_step', 'step_interval', 'max_points', 'detail'].includes(name)) changes[name] = null;
      update(changes, false);
    }}>← All experiments</button>
      <details className="stellar-section-settings"><summary>Customize sections</summary>
        <p className="muted">Layout and up to {MAX_PINS} metric pins are saved for this target and workspace.</p>
        <ol>{sections.map((section, index) => <li key={section.id}>
          <label><input type="checkbox" checked={section.visible} onChange={e => setSections(sections.map(s => s.id === section.id ? { ...s, visible: e.target.checked } : s))}/> Show {defaultSections().find(s => s.id === section.id)?.title}</label>
          <label>Title for {section.id}<input maxLength={120} value={section.title} onChange={e => setSections(sections.map(s => s.id === section.id ? { ...s, title: e.target.value } : s))}/></label>
          <label>Subtitle for {section.id}<input maxLength={300} value={section.subtitle} onChange={e => setSections(sections.map(s => s.id === section.id ? { ...s, subtitle: e.target.value } : s))}/></label>
          <div><button type="button" aria-label={`Move ${section.id} up`} disabled={index === 0} onClick={() => {
            const next = [...sections]; [next[index - 1], next[index]] = [next[index], next[index - 1]]; setSections(next);
          }}>Move up</button> <button type="button" aria-label={`Move ${section.id} down`} disabled={index === sections.length - 1} onClick={() => {
            const next = [...sections]; [next[index], next[index + 1]] = [next[index + 1], next[index]]; setSections(next);
          }}>Move down</button></div>
        </li>)}</ol><button type="button" onClick={() => setSections(defaultSections())}>Reset sections</button>
      </details>
    </div>;
  return <div className="stellar-target-workspace">
    <QueryResult query={query} name="Experiment summary">{snapshot => <>
      <div className="stellar-workbench-layout">
      <aside className="stellar-selection-rail" aria-label="Experiment and run selection">
      <RecentExperiments target={target}/>
      <details className="stellar-rail-disclosure" open={railOpen} onToggle={event => setRailOpen(event.currentTarget.open)}>
        <summary>Runs <span>{visibleRunIds.length} / {total} visible</span></summary>
        <div className="stellar-rail-body">
      <div className="stellar-run-actions"><strong>{runs.length} loaded of {total} · {visibleRunIds.length} visible</strong>
        <button type="button" disabled={!hidden.size} onClick={() => setHidden(new Set())}>Show all</button>
        <button type="button" disabled={!listed.length} onClick={() => setHidden(new Set([...hidden, ...listed.map(run => run.run_id)]))}>Hide listed</button>
      </div>
      <div className="stellar-filters">
        <label className="stellar-run-search"><span className="stellar-sr-only">Search runs</span><input type="search" placeholder="Search runs, tags, metrics, state" value={filters.search} onChange={e => update({ run_q: e.target.value })}/></label>
        <div className="stellar-lifecycle-chips" aria-label="Lifecycle filters">
          {['', 'succeeded', 'running', 'not_responding', 'unknown', 'pending', 'failed', 'cancelled', 'incomplete'].map(state => <button key={state} type="button" aria-pressed={filters.lifecycle === state}
            onClick={() => update({ lifecycle: state || null })}>{state ? state.replaceAll('_', ' ').replace(/^./, c => c.toUpperCase()) : 'All'} {state ? runs.filter(run => runLifecycle(run) === state).length : runs.length}</button>)}
        </div>
        <label>Updated<select value={filters.updated} onChange={e => update({ updated: e.target.value || null })}><option value="">Any update time</option><option value="1h">Last hour</option><option value="24h">Last 24h</option><option value="7d">Last 7d</option><option value="missing">Missing timestamp</option></select></label>
        <label>Sort<select aria-label="Sort runs" value={filters.sort} onChange={e => update({ updated_sort: e.target.value || null })}><option value="">Default order</option><option value="desc">Newest updated</option><option value="asc">Oldest updated</option></select></label>
      </div>
      {(filters.search || filters.group || filters.lifecycle || filters.updated || filters.sort) && <div className="stellar-run-actions">
        <button type="button" className="stellar-link" onClick={() => update({ run_q: null, group: null, lifecycle: null, updated: null, updated_sort: null })}>Clear run filters</button>
      </div>}
      <div className="stellar-run-picker">
        {focused.error && <div role="alert" className="warn">Run metric values unavailable: {focused.error.message} {staleReadMessage(focused)}
          <button type="button" onClick={() => void focused.refetch()}>Retry metric values</button></div>}
        {!listed.length ? <Empty>No loaded runs match these filters.</Empty> : <ul aria-label="Select runs">{listed.map((run, index) => <li className={hidden.has(run.run_id) ? 'is-hidden' : ''} key={run.run_id}>
          <input type="checkbox" aria-label={run.run_id} checked={!hidden.has(run.run_id)} onChange={e => {
            const next = new Set(hidden); e.target.checked ? next.delete(run.run_id) : next.add(run.run_id); setHidden(next);
          }}/><i className="stellar-run-dot" style={{ background: ('color' in run && run.color) || focused.data?.chart.series?.find(series => series.run_id === run.run_id)?.color || ['#2563eb', '#6046ff'][index % 2] }}/>
          <div className="stellar-run-row-main"><div className="stellar-run-row-title"><span title={run.run_id}>{run.run_id}</span><b>{latestRunValue(run, focused.data, focusMetric)}</b></div>
            <div className="stellar-run-tags"><span>{run.run_group_id}</span><span className={'stellar-run-state ' + runLifecycle(run)} title={'lifecycle_reason' in run ? run.lifecycle_reason : undefined}>{runLifecycle(run).replaceAll('_', ' ')}</span>
              <time title={runTimestamp(run)} dateTime={runTimestamp(run)}>{Number.isFinite(Date.parse(runTimestamp(run))) ? 'updated ' + new Date(runTimestamp(run)).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : 'No timestamp'}</time>
            </div></div>
        </li>)}</ul>}
        {more.error && <div role="alert" className="warn">More runs unavailable: {more.error.message} {staleReadMessage({ data: page, dataUpdatedAt: more.data ? more.dataUpdatedAt : previousPage?.dataUpdatedAt || 0 })}
          <button type="button" onClick={() => void more.refetch()}>Retry loading runs</button></div>}
        {page?.warnings?.map(warning => <p className="warn" key={warning}>{warning}</p>)}
        {canLoad && <button type="button" disabled={more.isFetching} onClick={() => setLimit(value => Math.min(MAX_RUNS, value + RUN_PAGE_SIZE))}>{more.isFetching ? 'Loading runs…' : `Load ${Math.min(RUN_PAGE_SIZE, MAX_RUNS - limit)} more runs`}</button>}
        {limit >= MAX_RUNS && <p className="muted">Loaded up to 1,000 runs. Open a run group or a specific run for a narrower comparison.</p>}
      </div>
      </div></details><details className="stellar-controls-disclosure"><summary>Controls</summary>
        <label className="stellar-group-control">Run group<select value={filters.group} onChange={e => update({ group: e.target.value || null })}><option value="">All groups</option>
          {[...new Set(runs.map(run => run.run_group_id))].filter(Boolean).map(group => <option key={group}>{group}</option>)}</select></label>
        {settings}</details></aside>
      <div className="stellar-metric-canvas">
      <section className={'stellar-summary' + (runs.some(run => ['failed', 'not_responding'].includes(runLifecycle(run))) ? ' needs-attention' : '')} aria-label="Loaded run operational status">
        <h2>{runs.some(run => ['failed', 'not_responding'].includes(runLifecycle(run))) ? 'Needs attention' : runs.some(run => ['running', 'pending'].includes(runLifecycle(run))) ? 'Operational' : 'No active runs'}</h2>
        <div className="stellar-operational-counts">{[
          ['active', runs.filter(run => ['running', 'pending'].includes(runLifecycle(run))).length],
          ['stale', runs.filter(run => runLifecycle(run) === 'not_responding').length],
          ['failed', runs.filter(run => runLifecycle(run) === 'failed').length],
          ['missing telemetry', runs.filter(run => !run.metric_names?.length).length],
          ['query errors', Number(!!focused.error) + Number(!!query.error) + Number(launchQueryError)],
        ].map(([label, count]) => <span key={label}><b>{count}</b> {label}</span>)}
        </div>
        <details className="stellar-summary-details" onToggle={event => setSummaryOpen(event.currentTarget.open)}><summary>Experiment summary</summary>
        {summaryOpen && <LaunchSummary target={target} visibleRunIds={visibleRunIds} onQueryError={setLaunchQueryError}/>}
        {snapshot.summary?.current_answer && <p>{snapshot.summary.current_answer}</p>}
        <dl><div><dt>Status</dt><dd>{snapshot.summary?.status || 'Unknown'}</dd></div>
          <div><dt>Loaded runs</dt><dd>{runs.length} / {total}</dd></div>
          <div><dt>Seed coverage</dt><dd>{snapshot.seed_coverage || snapshot.summary?.seed_coverage || 'Not recorded'}</dd></div>
          <div><dt>Confidence</dt><dd>{snapshot.summary?.confidence || 'Not recorded'}</dd></div></dl>
        </details>
      </section>
      {snapshot.warnings?.map(warning => <p className="warn" role="status" key={warning}>{warning}</p>)}
      <div className="stellar-section-grid">{visibleSections.map(section => {
        if (section.id === 'labels' && !labelGroups(snapshot).length && requestedPanel !== 'labels' && !params.has('sections') && section.title === defaultSections().find(s => s.id === 'labels')?.title && !section.subtitle) return null;
        const title = section.title || defaultSections().find(s => s.id === section.id)?.title || section.id;
        const headingId = 'stellar-' + section.id;
        const custom = title !== defaultSections().find(s => s.id === section.id)?.title;
        return <section className={'stellar-section stellar-section-' + section.id} id={'stellar-section-' + section.id} tabIndex={-1} key={section.id} aria-labelledby={headingId}>
          {['charts', 'timeline', 'catalog', 'runs'].includes(section.id)
            ? <><h2 id={headingId} className={custom ? '' : 'stellar-sr-only'}>{title}</h2>
              {section.subtitle && <p className="muted">{section.subtitle}</p>}
              <ChartWorkbench target={target} snapshot={augmentedSnapshot || snapshot} visibleRunIds={visibleRunIds} metrics={metrics} onMetricsChange={setMetrics} section={section.id}
                onMetricFocus={visibleSections.some(s => s.id === 'timeline') ? metric => update({ metric, pinned: metrics.join(','), panel: 'timeline' }, false) : undefined}/></>
            : <ResearchEvidence target={target} visibleRunIds={visibleRunIds} sections={[section.id]}
              initiallyExpanded dashboard heading={{ id: headingId, title, subtitle: section.subtitle, hidden: !custom }}/>}
        </section>;
      })}</div>
      {!sections.some(section => section.visible) && <Empty>All sections are hidden. Use Customize sections to restore your layout.</Empty>}
      </div></div>
    </>}</QueryResult>
  </div>;
}
