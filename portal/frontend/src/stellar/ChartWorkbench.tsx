// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useId, useMemo, useRef, useState, type PointerEvent } from 'react';
import { useSearchParams } from 'react-router-dom';
import { readableQuery, staleReadMessage, useBoard, useWorkspace } from '../data';
import { stellarURL } from './api';
import type { ScalarChart, ScalarPoint, ScalarRun, ScalarSeries, ScalarSeriesDetail, ScalarSnapshot } from './chart-types';
import './charts.css';

export interface ChartWorkbenchProps {
  target: string;
  snapshot: ScalarSnapshot;
  visibleRunIds: string[];
  metrics: string[];
  onMetricsChange: (metrics: string[]) => void;
  section?: string;
  onMetricFocus?: (metric: string) => void;
}
const MAX_PINS = 14;
const colors = ['#2563eb', '#b91c1c', '#15803d', '#9333ea', '#b45309', '#0e7490'];
const number = (value: unknown): number | undefined => {
  if (value === '' || value == null) return undefined;
  const n = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(n) ? n : undefined;
};
const format = (value: number) => new Intl.NumberFormat(undefined, { maximumSignificantDigits: 6 }).format(value);
const points = (series: ScalarSeries, smooth = false) =>
  (smooth ? series.smoothed_values ?? [] : series.values ?? [])
    .filter(p => Number.isFinite(p.step) && Number.isFinite(p.value)).slice().sort((a, b) => a.step - b.step);
const family = (metric: string) => metric.includes('/') ? metric.split('/')[0] : 'Other';
const metricTitle = (metric: string) => ({ 'train/loss': 'Train loss', 'train/lr': 'Learning rate' }[metric] || metric);
const metricPath = (base: string, metric: string) => stellarURL('snapshot', {
  ...Object.fromEntries(new URLSearchParams(base)), metric, mode: 'metric', include_static: false,
});

export function ChartWorkbench(props: ChartWorkbenchProps) {
  const { scope, managed } = useWorkspace();
  const [params] = useSearchParams();
  const project = params.get('project') ?? '';
  const key = JSON.stringify([scope, managed, project, props.target]);
  const base = new URLSearchParams({ target: props.target, source: scope.source });
  if (project) base.set('project', project);
  return <Workbench key={key} {...props} base={base.toString()} />;
}

function Workbench({ target, snapshot, visibleRunIds, metrics, onMetricsChange, onMetricFocus, base, section }: ChartWorkbenchProps & { base: string }) {
  const [params, setParams] = useSearchParams();
  const [search, setSearch] = useState('');
  const [activeFamily, setFamily] = useState('');
  const [showAll, setShowAll] = useState(false);
  const [catalogLimit, setCatalogLimit] = useState(48);
  const available = useMemo(() => [...new Map((snapshot.metric_options ?? []).map(m => [m.name, m])).values()], [snapshot.metric_options]);
  const selected = [...new Set(metrics)].slice(0, MAX_PINS);
  const requestedFocus = params.get('metric') || '';
  const focus = selected.includes(requestedFocus) ? requestedFocus : selected[0] || '';
  const visible = new Set(visibleRunIds);
  const runs = (snapshot.runs ?? []).filter(run => visible.has(run.run_id));
  const density = readableQuery(useBoard<ScalarSnapshot>(metricPath(base, focus), (!section || section === 'charts') && !!focus && visible.size > 0));
  const densitySeries = (density.data?.chart?.series || []).filter(series => visible.has(series.run_id));
  const rawCount = densitySeries.reduce((sum, series) => sum + (series.point_count ?? series.values?.length ?? 0), 0);
  const hoverCount = densitySeries.reduce((sum, series) => sum + (series.rendered_points ?? series.values?.length ?? 0), 0);
  const matches = available.filter(m => (!activeFamily || family(m.name) === activeFamily) &&
    `${m.name} ${m.card ?? ''}`.toLowerCase().includes(search.toLowerCase()));
  function setFocus(metric: string) {
    if (onMetricFocus) { onMetricFocus(metric); return; }
    setParams(current => {
      const next = new URLSearchParams(current);
      next.set('metric', metric);
      if (!next.has('pinned')) next.set('pinned', selected.join(','));
      return next;
    });
  }
  function pin(metric: string) {
    if (selected.includes(metric)) {
      const remaining = selected.filter(m => m !== metric);
      onMetricsChange(remaining);
    } else if (selected.length < MAX_PINS) onMetricsChange([...selected, metric]);
  }
  return <section className="scalar-workbench" aria-label="Scalar workbench">
    {!section && <div className="scalar-heading"><h2>Scalar charts</h2><span>{runs.length} visible runs · {selected.length}/{MAX_PINS} pinned</span></div>}
    {(!section || section === 'catalog') && <details className="scalar-catalog">
      <summary>Metric catalog <span>{available.length} available · {selected.length} pinned</span></summary>
      <div className="scalar-catalog-controls">
        <label>Search metrics<input type="search" value={search} onChange={e => { setSearch(e.target.value); setCatalogLimit(48); }} placeholder="Name or card" /></label>
        <label>Metric family<select value={activeFamily} onChange={e => { setFamily(e.target.value); setCatalogLimit(48); }}>
          <option value="">All families</option>
          {[...new Set(available.map(m => family(m.name)))].sort().map(name => <option key={name}>{name}</option>)}
        </select></label>
        <button type="button" disabled={!matches.length || selected.length >= MAX_PINS}
          onClick={() => onMetricsChange([...new Set([...selected, ...matches.map(m => m.name)])].slice(0, MAX_PINS))}>Pin matching metrics</button>
      </div>
      <p className="scalar-muted">{matches.length} matching metrics. Pin up to {MAX_PINS}; the first four charts load initially.</p>
      <div className="scalar-catalog-list">
        {matches.slice(0, catalogLimit).map(m => <button type="button" key={m.name} aria-pressed={selected.includes(m.name)}
          disabled={!selected.includes(m.name) && selected.length >= MAX_PINS} onClick={() => pin(m.name)}>{m.name} <span>{selected.includes(m.name) ? 'Unpin' : 'Pin'}</span></button>)}
        {!matches.length && <p>No metrics match this search.</p>}
      </div>
      {matches.length > catalogLimit && <button type="button" onClick={() => setCatalogLimit(limit => limit + 48)}>Show more metrics ({matches.length - catalogLimit} remaining)</button>}
    </details>}
    {(!section || section === 'charts') && <>
    {!selected.length && <p className="scalar-empty">No metrics pinned. Choose metrics from the catalog.</p>}
    <div className="scalar-grid">
      {(showAll ? selected : selected.slice(0, 4)).map(metric => <MetricCard key={metric} metric={metric} path={metricPath(base, metric)}
        enabled={!!target && visibleRunIds.length > 0} snapshot={snapshot} visible={visible} focused={focus === metric}
        onFocus={() => setFocus(metric)} onUnpin={() => pin(metric)} />)}
    </div>
    {selected.length > 4 && <button type="button" className="scalar-show-all" onClick={() => setShowAll(!showAll)}>
      {showAll ? 'Show first four charts' : `Show all ${selected.length} pinned charts`}</button>}
    <div className="scalar-dashboard-summary"><span><b>{focus}</b> focused</span><span><b>{selected.length}/{selected.length}</b> charts</span><span>{available.length} available metrics</span></div>
    {density.error && <QueryError error={density.error} retained={staleReadMessage(density)} retry={() => void density.refetch()} />}
    {density.data && <div className="scalar-dashboard-density"><b>Density</b><span>{densitySeries.length} series</span><span>{rawCount} raw points</span><span>{hoverCount} hover points</span></div>}
    </>}
    {(!section || section === 'timeline') && <FocusedMetric key={focus} base={base} metric={focus} snapshot={snapshot} visible={visible} runs={runs} showHeading={!section} />}
    {(!section || section === 'runs') && <RunComparison base={base} metrics={selected.slice(0, 6)} runs={runs} initiallyOpen={section === 'runs'} />}
  </section>;
}

function QueryError({ error, retry, retained = '' }: { error: Error; retry: () => void; retained?: string }) {
  return <div className="scalar-error" role="alert"><span>{retained} Query failed: {error.message}</span>
    <button type="button" onClick={retry}>Retry query</button></div>;
}
function emptyMessage(metric: string, snapshot: ScalarSnapshot, visible: Set<string>) {
  const runs = (snapshot.runs ?? []).filter(run => visible.has(run.run_id));
  if (!visible.size || (!runs.length && (snapshot.runs ?? []).length)) return 'Filtered out — no visible runs.';
  if (runs.length && runs.every(run => run.metric_names?.length === 0 &&
    /^(succeeded|failed|cancelled|incomplete)$/.test(run.lifecycle_state ?? run.state ?? ''))) return 'Ended before first sample.';
  if (runs.length && runs.every(run => run.metric_names && !run.metric_names.includes(metric))) return 'Not logged for the visible runs.';
  return 'No scalar points match the current run filters or step range.';
}
function dataset(chart: ScalarChart | undefined, visible: Set<string>) {
  return (chart?.series ?? []).filter(s => visible.has(s.run_id)).map(s => ({ ...s, values: points(s), smoothed_values: points(s, true) }))
    .filter(s => s.values.length);
}
function MetricCard({ metric, path, enabled, snapshot, visible, focused, onFocus, onUnpin }: {
  metric: string; path: string; enabled: boolean; snapshot: ScalarSnapshot; visible: Set<string>;
  focused: boolean; onFocus: () => void; onUnpin: () => void;
}) {
  const query = readableQuery(useBoard<ScalarSnapshot>(path, enabled));
  const data = dataset(query.data?.chart, visible);
  const all = data.flatMap(series => series.values.map(point => ({ ...point, run: series.run_id })));
  const latest = all.reduce<(ScalarPoint & { run: string }) | undefined>((a, b) => !a || b.step > a.step ? b : a, undefined);
  return <article className={`scalar-card${focused ? ' is-focused' : ''}`} aria-label={metric}>
    <div className="scalar-card-heading"><button type="button" className="scalar-metric-title" aria-label={metric} aria-pressed={focused} title={`Inspect ${metric} timeline`} onClick={onFocus}><span>{metricTitle(metric)}</span><em>{metric}</em></button>
      <button type="button" className="scalar-pin" aria-label={`Unpin ${metric}`} onClick={onUnpin}>Pinned</button></div>
    {query.error && <QueryError error={query.error} retained={staleReadMessage(query)} retry={() => { void query.refetch(); }} />}
    {!visible.size ? <p className="scalar-empty">Filtered out — no visible runs.</p> :
      query.isPending ? <p role="status" className="scalar-empty">Loading {metric}…</p> :
      query.error && !query.data ? null :
      !data.length ? <p className="scalar-empty">{emptyMessage(metric, snapshot, visible)}</p> :
      data.every(s => s.values.length === 1) && latest ? <div className="scalar-value"><strong>{format(latest.value)}</strong>
        <span>{latest.run} · step {format(latest.step)}</span></div> :
      <ScalarPlot chart={query.data?.chart} visible={visible} metric={metric} />}
    <Density chart={query.data?.chart} visible={visible} warnings={query.data?.warnings} />
  </article>;
}

function Density({ chart, visible, warnings }: { chart?: ScalarChart; visible: Set<string>; warnings?: string[] }) {
  const series = (chart?.series ?? []).filter(s => visible.has(s.run_id));
  const raw = series.reduce((n, s) => n + (s.point_count ?? s.sampling?.source_points ?? s.values?.length ?? 0), 0);
  const rendered = series.reduce((n, s) => n + (s.rendered_points ?? s.sampling?.rendered_points ?? s.values?.length ?? 0), 0);
  const sampled = series.some(s => s.decimated || s.sampling?.truncated) || rendered < raw;
  return <div className="scalar-density">
    {!!series.length && <><span>{format(rendered)} of {format(raw)} points shown{chart?.step_interval ? ` · every ${chart.step_interval} steps` : ''}</span><b>{sampled ? 'Sampled view; raw history preserved' : 'All points'}</b></>}
    {chart?.smoothing && series.some(s => s.smoothed_values?.length) && <span>Server {chart.smoothing.method.toUpperCase()} · α {chart.smoothing.alpha} · raw overlay</span>}
    {warnings?.map((warning, i) => <span className="scalar-warning" key={`${warning}-${i}`}>{warning}</span>)}
  </div>;
}

interface Controls { run: string; start: string; end: string; interval: string; custom: string; cap: string }
function readControls(params: URLSearchParams): Controls {
  const interval = params.get('step_interval') || 'auto';
  return { run: params.get('run_id') || '', start: params.get('start_step') || '', end: params.get('end_step') || '',
    interval: ['auto', '20', '50', '100'].includes(interval) ? interval : 'custom',
    custom: ['auto', '20', '50', '100'].includes(interval) ? '' : interval, cap: params.get('max_points') || '1200' };
}
function integer(value: string, label: string): number {
  if (!/^-?\d+$/.test(value.trim()) || !Number.isSafeInteger(Number(value))) throw new Error(`${label} must be a safe integer.`);
  return Number(value);
}
function controlQuery(controls: Controls, chart?: ScalarChart) {
  const start = controls.start.trim() ? integer(controls.start, 'Start step') : undefined;
  const end = controls.end.trim() ? integer(controls.end, 'End step') : undefined;
  if (start !== undefined && end !== undefined && start > end) throw new Error('Start step must be less than or equal to end step.');
  const cap = integer(controls.cap, 'Point cap');
  if (cap < 1 || cap > 12000) throw new Error('Point cap must be between 1 and 12000.');
  const lo = start ?? number(chart?.x_min), hi = end ?? number(chart?.x_max);
  const interval = controls.interval === 'auto' ? (lo !== undefined && hi !== undefined && Math.abs(hi - lo) <= 2000 ? 20 : 50) :
    integer(controls.interval === 'custom' ? controls.custom : controls.interval, 'Step interval');
  if (interval < 1) throw new Error('Step interval must be at least 1.');
  const query = new URLSearchParams({ max_points: String(cap), step_interval: String(interval) });
  if (start !== undefined) query.set('start_step', String(start));
  if (end !== undefined) query.set('end_step', String(end));
  if (controls.run) query.set('run_id', controls.run);
  return query;
}
function FocusedMetric({ base, metric, snapshot, visible, runs, showHeading }: {
  base: string; metric: string; snapshot: ScalarSnapshot; visible: Set<string>; runs: ScalarRun[]; showHeading: boolean;
}) {
  const [params, setParams] = useSearchParams();
  const saved = readControls(params);
  const savedKey = JSON.stringify(saved);
  const [draft, setDraft] = useState(saved);
  const [error, setError] = useState('');
  useEffect(() => { setDraft(readControls(new URLSearchParams(params))); setError(''); }, [savedKey]); // URL owns applied range, including browser history.
  const hasTarget = !!new URLSearchParams(base).get('target');
  const summary = readableQuery(useBoard<ScalarSnapshot>(metricPath(base, metric), hasTarget && !!metric && visible.size > 0));
  let queryArgs = new URLSearchParams(), invalid = '';
  try { queryArgs = controlQuery(saved, summary.data?.chart); } catch (e) { invalid = (e as Error).message; }
  // The original dashboard starts with the unsliced history; explicit detail controls apply resolution.
  if (!['detail', 'run_id', 'start_step', 'end_step', 'step_interval', 'max_points'].some(key => params.has(key))) queryArgs.set('step_interval', '1');
  const runVisible = !saved.run || visible.has(saved.run);
  const detail = readableQuery(useBoard<ScalarSeriesDetail>(stellarURL('series', {
    ...Object.fromEntries(new URLSearchParams(base)), metric, ...Object.fromEntries(queryArgs),
  }),
    hasTarget && !!metric && visible.size > 0 && runVisible && !invalid && !!summary.data));
  const chart = summary.data ? detail.data?.chart : undefined;
  const detailVisible = saved.run ? new Set([saved.run].filter(id => visible.has(id))) : visible;
  function apply(next: Controls) {
    try {
      controlQuery(next, summary.data?.chart);
      setParams(current => {
        const p = new URLSearchParams(current);
        for (const key of ['run_id', 'start_step', 'end_step', 'step_interval', 'max_points']) p.delete(key);
        if (next.run) p.set('run_id', next.run);
        if (next.start.trim()) p.set('start_step', String(integer(next.start, 'Start step')));
        if (next.end.trim()) p.set('end_step', String(integer(next.end, 'End step')));
        p.set('step_interval', next.interval === 'custom' ? next.custom : next.interval);
        p.set('max_points', next.cap);
        return p;
      });
      setError('');
    } catch (e) { setError((e as Error).message); }
  }
  const reset = () => { const next = { ...saved, start: '', end: '', interval: 'auto', custom: '' }; setDraft(next); apply(next); };
  const field = (key: keyof Controls, value: string) => setDraft(current => ({ ...current, [key]: value }));
  return <section className="scalar-detail" aria-label="Selected metric timeline">
    {showHeading && <div className="scalar-heading"><h2>Selected metric timeline</h2><span>{metric || 'Choose a pinned metric'}</span></div>}
    <span className="scalar-drag-label" title="Double-click or reset to see the full range">Drag to zoom</span>
    {!!metric && <>
      <form className="scalar-controls" onSubmit={e => { e.preventDefault(); apply(draft); }} noValidate>
        <label>Run<select aria-label="Run focus" value={draft.run} onChange={e => field('run', e.target.value)}><option value="">All runs</option>
          {saved.run && !visible.has(saved.run) && <option value={saved.run}>{saved.run} (filtered out)</option>}
          {runs.map(run => <option key={run.run_id} value={run.run_id}>{run.run_id}</option>)}</select></label>
        <label>Start step<input inputMode="numeric" value={draft.start} onChange={e => field('start', e.target.value)} placeholder={summary.data?.chart?.x_min ?? 'First'} /></label>
        <label>End step<input inputMode="numeric" value={draft.end} onChange={e => field('end', e.target.value)} placeholder={summary.data?.chart?.x_max ?? 'Latest'} /></label>
        <label>Resolution<select aria-label="Step interval" value={draft.interval} onChange={e => field('interval', e.target.value)}>
          <option value="auto">Auto resolution</option><option value="20">20</option><option value="50">50</option><option value="100">100</option><option value="custom">Custom</option>
        </select></label>
        {draft.interval === 'custom' && <label>Custom steps<input inputMode="numeric" value={draft.custom} onChange={e => field('custom', e.target.value)} /></label>}
        <label>Point cap<input inputMode="numeric" value={draft.cap} onChange={e => field('cap', e.target.value)} /></label>
        <button type="submit">Load detail</button>{(saved.start || saved.end || saved.run) && <button type="button" onClick={reset}>Reset zoom</button>}
      </form>
      {(error || invalid) && <p className="scalar-error" role="alert">{error || invalid}</p>}
      {visible.size > 0 && runVisible && summary.error && <QueryError error={summary.error} retained={staleReadMessage(summary)} retry={() => { void summary.refetch(); }} />}
      {visible.size > 0 && runVisible && detail.error && <QueryError error={detail.error} retained={staleReadMessage(detail)} retry={() => { void detail.refetch(); }} />}
      {!visible.size || !runVisible ? <p className="scalar-empty">Filtered out — no visible runs match this focus.</p> :
        (summary.error && !summary.data) || (detail.error && !detail.data) ? null :
        invalid ? null : detail.isPending ? <p role="status">Loading focused detail…</p> :
        dataset(chart, detailVisible).length ? <ScalarPlot key={JSON.stringify([metric, savedKey, [...visible]])} chart={chart} metric={metric} visible={detailVisible}
          onRunFocus={run => { const next = { ...saved, run }; setDraft(next); apply(next); }}
          onZoom={(start, end) => { const next = { ...saved, start: String(start), end: String(end), interval: 'auto', custom: '' }; setDraft(next); apply(next); }}
          onReset={reset} /> : <p className="scalar-empty">{emptyMessage(metric, snapshot, detailVisible)}</p>}
      {runVisible && !invalid && summary.data && <Density chart={chart} visible={detailVisible} warnings={detail.data?.warnings} />}
      {visible.size > 0 && runVisible && !invalid && summary.data && detail.data?.raw_query_source === 'kusto' && detail.data.raw_query &&
        <CopyText key={detail.data.raw_query} value={detail.data.raw_query} label="Copy raw KQL" />}
    </>}
  </section>;
}

function CopyText({ value, label }: { value: string; label: string }) {
  const [message, setMessage] = useState(''), [failed, setFailed] = useState(false);
  return <span className="scalar-copy"><button type="button" onClick={async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('Clipboard is unavailable in this browser.');
      await navigator.clipboard.writeText(value); setFailed(false); setMessage('Copied.');
    } catch (e) { setFailed(true); setMessage(`Copy failed: ${(e as Error).message}`); }
  }}>{label}</button>{message && <span role={failed ? 'alert' : 'status'}>{message}</span>}</span>;
}

function RunComparison({ base, metrics, runs, initiallyOpen }: { base: string; metrics: string[]; runs: ScalarRun[]; initiallyOpen: boolean }) {
  const [open, setOpen] = useState(initiallyOpen);
  return <details className="scalar-comparison" open={open} onToggle={e => setOpen(e.currentTarget.open)}>
    <summary>{initiallyOpen ? 'Compare selected runs' : 'Run comparison'} <span>{runs.length} visible runs · latest values for first six pinned metrics</span></summary>
    {open && <ComparisonTable base={base} metrics={metrics} runs={runs} />}
  </details>;
}
function ComparisonTable({ base, metrics, runs }: { base: string; metrics: string[]; runs: ScalarRun[] }) {
  // Six unconditional hooks keep observer count bounded as the run list grows.
  const enabled = (index: number) => !!metrics[index] && runs.length > 0;
  const path = (index: number) => metricPath(base, metrics[index] || '');
  const queries = [
    useBoard<ScalarSnapshot>(path(0), enabled(0)), useBoard<ScalarSnapshot>(path(1), enabled(1)),
    useBoard<ScalarSnapshot>(path(2), enabled(2)), useBoard<ScalarSnapshot>(path(3), enabled(3)),
    useBoard<ScalarSnapshot>(path(4), enabled(4)), useBoard<ScalarSnapshot>(path(5), enabled(5)),
  ];
  return <div className="table-scroll"><table className="jobs">
      <thead><tr><th scope="col">Run</th><th scope="col">Status</th><th scope="col">Group</th>
        {metrics.map(metric => <th scope="col" key={metric}>{metricTitle(metric)}</th>)}</tr></thead>
      <tbody>{runs.map(run => <tr key={run.run_id}>
        <th scope="row"><span>{run.run_id}</span><small className="scalar-run-context">{[run.project, run.workspace_id, run.cluster, run.source, run.owner].filter(Boolean).join(' · ')}</small>
          {run.result_uri && <CopyText value={run.result_uri} label={`Copy result URI for ${run.run_id}`} />}</th>
        <td title={run.lifecycle_reason}><span className={'scalar-run-status ' + (run.lifecycle_state || run.state || '').toLowerCase()}>{run.lifecycle_state || run.state || 'Unknown'}</span><small className="scalar-run-context">{[run.outcome_state, run.liveness_state].filter(Boolean).join(' · ')}</small></td>
        <td>{run.run_group_id || '—'}</td>{metrics.map((metric, index) => <RunMetric key={metric} query={queries[index]} metric={metric} run={run} />)}
      </tr>)}</tbody>
    </table>{!runs.length && <p className="scalar-empty">Filtered out — no visible runs.</p>}</div>;
}
function RunMetric({ query: result, metric, run }: { query: ReturnType<typeof useBoard<ScalarSnapshot>>; metric: string; run: ScalarRun }) {
  const query = readableQuery(result);
  const values = (query.data?.chart?.series ?? []).filter(s => s.run_id === run.run_id).flatMap(s => points(s));
  const latest = values.reduce<ScalarPoint | undefined>((a, b) => !a || b.step > a.step ? b : a, undefined);
  const runSummary = run.metrics?.find(m => m.metric_name === metric);
  // Go omits zero-valued latest_value; finite_count distinguishes that zero from no finite samples.
  const summaryValue = runSummary?.latest_value === undefined && (runSummary?.finite_count ?? 0) > 0 ? 0 : number(runSummary?.latest_value);
  const summaryStep = number(runSummary?.latest_step);
  const preferSummary = summaryValue !== undefined && (!latest || (summaryStep !== undefined && summaryStep > latest.step));
  const value = preferSummary ? summaryValue : latest?.value ?? summaryValue;
  const latestStep = preferSummary ? summaryStep : latest?.step ?? summaryStep;
  const missing = runSummary ? 'No finite scalar value' : run.metric_names?.includes(metric) ? 'Not in sampled chart' :
    run.metric_names ? 'Not logged' : 'No scalar value available';
  return <td>
    {query.error && <QueryError error={query.error} retained={staleReadMessage(query)} retry={() => { void query.refetch(); }} />}
    {query.isPending ? 'Loading…' : query.error && !query.data ? null :
      value !== undefined ? <span title={`Latest at step ${latestStep ?? 'unknown'}`}>{format(value)}</span> : missing}
  </td>;
}

function ScalarPlot({ chart, metric, visible, onZoom, onReset, onRunFocus }: {
  chart?: ScalarChart; metric: string; visible: Set<string>;
  onZoom?: (start: number, end: number) => void; onReset?: () => void; onRunFocus?: (run: string) => void;
}) {
  const titleID = useId(), descriptionID = useId();
  const plot = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(800);
  useEffect(() => {
    if (!plot.current || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(entries => {
      const next = entries[0]?.contentRect.width;
      if (next) setWidth(Math.max(400, Math.min(800, next)));
    });
    observer.observe(plot.current);
    return () => observer.disconnect();
  }, []);
  const series = useMemo(() => dataset(chart, visible), [chart, visible]);
  const multipleGroups = new Set(series.map(item => item.run_group_id)).size > 1;
  const [hover, setHover] = useState<{ run: string; step: number } | null>(null);
  const [brush, setBrush] = useState<{ anchor: number; current: number } | null>(null);
  const [showAllRuns, setShowAllRuns] = useState(false);
  const { xMin, xMax, yMin, yMax, totalPoints } = useMemo(() => {
    let xMin = Infinity, xMax = -Infinity, yMin = Infinity, yMax = -Infinity, totalPoints = 0;
    for (const s of series) {
      totalPoints += s.values.length;
      for (const p of [...s.values, ...s.smoothed_values]) {
        xMin = Math.min(xMin, p.step); xMax = Math.max(xMax, p.step); yMin = Math.min(yMin, p.value); yMax = Math.max(yMax, p.value);
      }
    }
    if (!totalPoints) { xMin = yMin = 0; xMax = yMax = 1; }
    if (xMin === xMax) { xMin -= 1; xMax += 1; }
    if (yMin === yMax) { yMin -= 1; yMax += 1; }
    const pad = (yMax - yMin) * .08;
    return { xMin, xMax, yMin: yMin - pad, yMax: yMax + pad, totalPoints };
  }, [series]);
  const tickInterval = (yMax - yMin) / 4;
  const precision = Math.min(17, Math.max(3, Math.ceil(Math.log10(Math.max(Math.abs(yMin), Math.abs(yMax)) / tickInterval)) + 1));
  const axisFormat = new Intl.NumberFormat(undefined, { notation: 'compact', maximumSignificantDigits: precision });
  const ticks = [0, 1, 2, 3, 4].map(i => {
    const value = yMin + (yMax - yMin) * i / 4;
    return { value, label: value !== 0 && Math.abs(value) < .001 ? value.toExponential(precision - 1) : axisFormat.format(value) };
  });
  const left = Math.max(48, ...ticks.map(tick => tick.label.length * 6 + 12)), right = width - 16, plotWidth = right - left;
  const x = (step: number) => left + (step - xMin) / (xMax - xMin) * plotWidth;
  const y = (value: number) => 254 - (value - yMin) / (yMax - yMin) * 230;
  const stepAt = (px: number) => xMin + (px - left) / plotWidth * (xMax - xMin);
  const pointer = (e: PointerEvent<SVGSVGElement>) => {
    const bounds = e.currentTarget.getBoundingClientRect();
    return { x: Math.max(left, Math.min(right, (e.clientX - bounds.left) / (bounds.width || width) * width)),
      y: (e.clientY - bounds.top) / (bounds.height || 290) * 290 };
  };
  const active = series.find(s => s.run_id === hover?.run);
  const raw = active?.values.find(p => p.step === hover?.step);
  const ema = raw && active?.smoothed_values.reduce<ScalarPoint | undefined>((a, b) => !a || Math.abs(b.step - raw.step) < Math.abs(a.step - raw.step) ? b : a, undefined);
  function nearest(px: number, py: number) {
    let distance = Infinity, result: { run: string; step: number } | null = null;
    for (const s of series) {
      let lo = 0, hi = s.values.length - 1;
      while (lo < hi) { const mid = Math.floor((lo + hi) / 2); if (x(s.values[mid].step) < px) lo = mid + 1; else hi = mid; }
      for (const p of s.values.slice(Math.max(0, lo - 1), lo + 2)) {
        const next = Math.hypot(x(p.step) - px, y(p.value) - py);
        if (next < distance) { distance = next; result = { run: s.run_id, step: p.step }; }
      }
    }
    setHover(result);
  }
  const line = (values: ScalarPoint[]) => values.map(p => `${x(p.step)},${y(p.value)}`).join(' ');
  return <div className="scalar-plot" ref={plot}>
    <svg viewBox={`0 0 ${width} 290`} preserveAspectRatio="none" role="img" tabIndex={0} aria-labelledby={titleID} aria-describedby={descriptionID}
      onFocus={() => { if (!hover && series[0]?.values[0]) setHover({ run: series[0].run_id, step: series[0].values[0].step }); }}
      onKeyDown={e => {
        if (e.key === 'Escape') { setBrush(null); onReset?.(); return; }
        if (e.key === 'Enter' && active) { onRunFocus?.(active.run_id); return; }
        if (!['ArrowRight', 'ArrowLeft', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(e.key) || !series.length) return;
        e.preventDefault();
        let index = Math.max(0, series.findIndex(s => s.run_id === active?.run_id));
        if (e.key === 'ArrowUp') index = (index + series.length - 1) % series.length;
        if (e.key === 'ArrowDown') index = (index + 1) % series.length;
        const s = series[index];
        let pointIndex = Math.max(0, s.values.findIndex(p => p.step === hover?.step));
        if (e.key === 'ArrowRight') pointIndex = Math.min(s.values.length - 1, pointIndex + 1);
        if (e.key === 'ArrowLeft') pointIndex = Math.max(0, pointIndex - 1);
        if (e.key === 'Home') pointIndex = 0;
        if (e.key === 'End') pointIndex = s.values.length - 1;
        setHover({ run: s.run_id, step: s.values[pointIndex].step });
      }}
      onPointerMove={e => { const p = pointer(e); if (brush) setBrush({ ...brush, current: p.x }); else nearest(p.x, p.y); }}
      onPointerDown={e => { if (onZoom && e.button === 0) { e.currentTarget.setPointerCapture?.(e.pointerId); const p = pointer(e); setBrush({ anchor: p.x, current: p.x }); } }}
      onPointerCancel={() => setBrush(null)}
      onLostPointerCapture={() => setBrush(null)}
      onPointerUp={e => {
        if (!brush) return;
        const end = pointer(e).x;
        if (Math.abs(end - brush.anchor) >= 6) {
          const lo = Math.round(stepAt(Math.min(end, brush.anchor))), hi = Math.round(stepAt(Math.max(end, brush.anchor)));
          if (Number.isSafeInteger(lo) && Number.isSafeInteger(hi) && lo < hi) onZoom?.(lo, hi);
        }
        setBrush(null); e.currentTarget.releasePointerCapture?.(e.pointerId);
      }}
      onDoubleClick={onReset}>
      <title id={titleID}>{metric}: {series.length} visible series, steps {format(xMin)} to {format(xMax)}</title>
      <desc id={descriptionID}>Raw samples and server-provided EMA when available. Arrow left/right inspect samples; up/down choose runs. Home/End jump to first/latest sample. Enter focuses a run. Range inputs provide keyboard zoom.</desc>
      {ticks.map(({ value, label }, i) => <g key={i}><line className="scalar-gridline" x1={left} x2={right} y1={y(value)} y2={y(value)} />
        <text x={left - 6} y={y(value) + 4} textAnchor="end">{label}</text></g>)}
      {Array.from({ length: 11 }, (_, i) => <line key={i} className="scalar-gridline scalar-gridline-vertical" x1={left + plotWidth * i / 10} x2={left + plotWidth * i / 10} y1="24" y2="254"/>)}
      <path className="scalar-axis" d={`M${left} 24 V254 H${right}`} fill="none"/>
      <text x={left} y="280">step {format(xMin)}</text><text x={right} y="280" textAnchor="end">step {format(xMax)}</text>
      {series.map((s, i) => <g key={s.run_id} opacity={active && active.run_id !== s.run_id ? .35 : 1}>
        <polyline data-kind="raw" points={line(s.values)} fill="none" stroke={s.color || colors[i % colors.length]} vectorEffect="non-scaling-stroke" strokeWidth={s.smoothed_values.length ? 1 : 2.2} opacity={s.smoothed_values.length ? .45 : .78}>
          <title>{s.run_id} raw</title></polyline>
        {s.smoothed_values.length > 0 && <polyline data-kind="ema" points={line(s.smoothed_values)} fill="none" stroke={s.color || colors[i % colors.length]} strokeWidth="2.5"><title>{s.run_id} server EMA</title></polyline>}
        {totalPoints <= 200 && s.values.map((p, j) => <circle key={j} cx={x(p.step)} cy={y(p.value)} r="2.5" stroke="#fff" fill={s.color || colors[i % colors.length]}><title>{s.run_id} step {p.step}: raw {p.value}</title></circle>)}
      </g>)}
      {raw && <g pointerEvents="none"><line x1={x(raw.step)} x2={x(raw.step)} y1="24" y2="254" className="scalar-crosshair" />
        <circle cx={x(raw.step)} cy={y(raw.value)} r="5" className="scalar-hover-point" /></g>}
      {brush && <rect x={Math.min(brush.anchor, brush.current)} y="24" width={Math.abs(brush.current - brush.anchor)} height="230" className="scalar-brush" />}
    </svg>
    <div className={'scalar-inspector' + (raw ? ' is-active' : '')} role="status" aria-live="polite">
      {raw && active ? <><strong>{active.run_id}</strong><span>step {format(raw.step)}</span><span>Raw {format(raw.value)}</span>
        {ema && <span>EMA {format(ema.value)}{ema.step !== raw.step ? ` (nearest step ${format(ema.step)})` : ''}</span>}</> : null}
    </div>
    <div className="scalar-legend">{(showAllRuns ? series : series.slice(0, 8)).map((s, i) => <span key={s.run_id}>
      <i style={{ background: s.color || colors[i % colors.length] }} />
      {onRunFocus ? <button type="button" onClick={() => onRunFocus(s.run_id)} title={`Focus ${s.run_id}`}>{s.run_id}</button> : <b title={s.run_id}>{s.run_id}</b>}
      {s.run_group_id && multipleGroups && <small>{s.run_group_id}</small>}
    </span>)}
      {series.length > 8 && <button type="button" onClick={() => setShowAllRuns(!showAllRuns)}>{showAllRuns ? 'Show fewer runs' : `Show ${series.length - 8} more runs`}</button>}
    </div>
  </div>;
}
