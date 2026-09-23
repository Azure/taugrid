// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import type { ReactNode } from 'react';
import { Link, useLocation, useNavigate } from 'react-router-dom';
import type { UseQueryResult } from '@tanstack/react-query';
import { experimentPageURL, nativeExperimentURL, readableQuery, staleReadMessage, useScopedURL, useWorkspace } from './data';
import type { GPU, Profiles, Tracking } from './types';

export const measured = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value);
export const n1 = (value: number | null | undefined) => measured(value) ? Math.round(value * 10) / 10 : '—';
export function utilizationSummary(gpus: GPU[]) {
  const values = gpus.map(g => g.utilizationPct).filter(measured);
  return {
    observed: values.length, total: gpus.length,
    average: values.length ? values.reduce((sum, value) => sum + value, 0) / values.length : null,
    idle: values.length ? values.filter(value => value < 5).length : null,
  };
}
export const text = (value: string | undefined) => value || '—';
export function PageTitle({ title, children }: { title: string; children?: ReactNode }) {
  return <><h1>{title}</h1>{children && <p className="muted page-description">{children}</p>}</>;
}
export function Empty({ children, warn = false }: { children: ReactNode; warn?: boolean }) {
  return <div className={'empty' + (warn ? ' warn' : '')}>{children}</div>;
}
export function Note({ children, warn = false }: { children: ReactNode; warn?: boolean }) {
  return <p className={'note' + (warn ? ' warn' : '')}>{children}</p>;
}
export function BoardResult<T>({
  query: result, label, children, hint = '', partial = false, live = false, sources = [], autoRefreshMs = 0,
}: {
  query: UseQueryResult<T, Error>; label: string; children: (data: T) => ReactNode; hint?: string; partial?: boolean; live?: boolean;
  sources?: { label: string; query: UseQueryResult<unknown, Error> }[]; autoRefreshMs?: number;
}) {
  const query = readableQuery(result);
  const related = sources.map(source => ({ label: source.label, query: readableQuery(source.query), result: source.query }));
  const queries = [{ label, query, result }, ...related];
  const hasData = query.data !== undefined;
  const embedded = live && hasData && !query.error;
  const refreshing = queries.some(source => source.query.isFetching);
  const sourceUnavailable = related.some(source => source.query.error);
  const action = query.error || sourceUnavailable || partial ? 'Retry' : 'Refresh';
  const status = embedded ? 'Embedded live dashboard; freshness is managed inside the dashboard.'
    : refreshing ? (hasData ? 'Refreshing all sources; showing previous snapshots where available.' : 'Loading snapshot…')
      : query.error ? (hasData ? 'Stale snapshot; refresh failed.' : 'Unavailable.')
        : sourceUnavailable || partial ? 'Some sources unavailable; see source diagnostics.'
          : autoRefreshMs > 0 ? `Auto-refreshing every ${Math.round(autoRefreshMs / 1000)} seconds.`
          : query.isStale ? 'Stale snapshot; refresh for current data.' : 'Snapshot; not live.';
  return <section className="data-panel" aria-label={label}>
    <div className="panel-status">
      <span role="status">{label}: {status}{!embedded && sources.length === 0 && hasData && query.dataUpdatedAt > 0 && <>
        {' '}Last successful response <time dateTime={new Date(query.dataUpdatedAt).toISOString()}>{new Date(query.dataUpdatedAt).toLocaleString()}</time>.
      </>}</span>
      {!embedded && <button type="button" className="btn" aria-label={action + ' ' + label} disabled={refreshing}
        onClick={() => { void Promise.all(queries.map(source => source.result.refetch())); }}>{refreshing ? 'Refreshing…' : action}</button>}
    </div>
    {sources.length > 0 && <div className="panel-source-status" aria-label={`${label} source freshness`}>
      {queries.map(source => <span key={source.label}>
        <strong>{source.label}</strong>: {source.query.error
          ? source.query.data === undefined ? 'unavailable' : 'refresh failed; stale data retained'
          : source.query.isFetching ? source.query.data === undefined ? 'loading' : 'refreshing'
            : source.query.isStale ? 'stale' : 'current'}
        {source.query.dataUpdatedAt > 0 && <> · <time dateTime={new Date(source.query.dataUpdatedAt).toISOString()}>
          {new Date(source.query.dataUpdatedAt).toLocaleString()}
        </time></>}
      </span>)}
    </div>}
    <div aria-busy={refreshing}>
      {query.error && <div className="empty warn" role="alert">{label} {hasData ? 'refresh failed' : 'unavailable'}: {query.error.message}{hint}. {staleReadMessage(query)}</div>}
      {related.filter(source => source.query.error).map(source =>
        <div className="empty warn" role="alert" key={source.label}>{source.label} {source.query.data === undefined ? 'unavailable' : 'refresh failed'}:
          {' '}{source.query.error?.message}. {staleReadMessage(source.query)}
        </div>)}
      {query.data !== undefined && children(query.data)}
    </div>
  </section>;
}
export function ScopedLink({ to, children, className, title, external = false }: { to: string; children: ReactNode; className?: string; title?: string; external?: boolean }) {
  const scoped = useScopedURL();
  const { scope } = useWorkspace();
  const href = scoped(nativeExperimentURL(to, scope.experimentsUrl));
  if (external || !href.startsWith('/portal') || (href.length > 7 && !['/', '?', '#'].includes(href[7]))) {
    return <a href={href} className={className} title={title} target={external ? '_blank' : undefined} rel={external ? 'noopener noreferrer' : undefined}>{children}</a>;
  }
  return <Link to={href} className={className} title={title}>{children}</Link>;
}
export function Table({ headers, rows }: { headers: string[]; rows: ReactNode[][] }) {
  return <div className="table-scroll"><table className="jobs"><thead><tr>{headers.map((h) => <th key={h} className={h.startsWith('#') ? 'num' : undefined}>{h.replace(/^#/, '')}</th>)}</tr></thead>
    <tbody>{rows.map((row, i) => <tr key={i}>{row.map((cell, j) => <td key={j} className={headers[j]?.startsWith('#') ? 'num' : undefined}>{cell}</td>)}</tr>)}</tbody></table></div>;
}
export function KV({ rows }: { rows: [string, ReactNode][] }) {
  return <table className="jobs"><tbody>{rows.filter(([, v]) => v !== undefined && v !== null && v !== '').map(([k, v]) => <tr key={k}><td className="kv-label">{k}</td><td>{v}</td></tr>)}</tbody></table>;
}
export function Subtabs({ items, active, base }: { items: readonly (readonly [string, string])[]; active: string; base?: string }) {
  const location = useLocation(), navigate = useNavigate();
  const scoped = useScopedURL();
  return <div className="subtabs" role="tablist">{items.map(([id, label]) => <button key={id} type="button" role="tab" aria-selected={active === id} className={active === id ? 'active' : ''} onClick={() => {
    const params = new URLSearchParams(location.search);
    params.set('view', id);
    navigate(scoped((base || location.pathname) + '?' + params + location.hash));
  }}>{label}</button>)}</div>;
}
export function ProfileReadiness({ state }: { state?: Profiles }) {
  return <><h3>Workload profiles</h3>{!state?.available
    ? <Empty warn>Profile readiness unavailable: {state?.error || 'TauCluster status is unavailable'}. Existing workloads and queues remain observable.</Empty>
    : <><Note>TauCluster generation {state.tauClusterGeneration ?? '—'} · profileSetHash {state.profileSetHash || '—'} · read-only readiness; profile selection is not available in Portal.</Note>
      {!state.readyProfiles?.length ? <Empty>No ready workload profiles authorize this namespace/team.</Empty> : <Table headers={['Profile', 'Execution target', 'Placement', 'Queue']} rows={state.readyProfiles.map(p => [text(p.name), text(p.executionTarget), text(p.placement), text(p.defaultLocalQueue)])}/>}</>}</>;
}
export function TrackingLink({ run, label = 'open ↗' }: { run: Tracking; label?: string }) {
  const { scope, managed } = useWorkspace();
  const path = run.experimentPath ? new URL(nativeExperimentURL(run.experimentPath), window.location.origin) : undefined;
  if (path && !path.searchParams.has('target') && run.runId) path.searchParams.set('target', run.runId);
  if (path?.searchParams.has('target')) {
    path.searchParams.set('run', path.searchParams.get('target')!);
    path.searchParams.delete('target');
  }
  return path ? <ScopedLink to={experimentPageURL(scope, managed, path.search)! + path.hash} className="back" title={run.runId}>{label}</ScopedLink>
    : run.experimentTracking === 'available' && scope.experimentsUrl
      ? <ScopedLink to="/portal/experiments" className="back">available ↗</ScopedLink>
      : <span className="muted">{run.experimentTracking || 'untracked'}</span>;
}
export function Stat({ href, label, value, of, sub, dot, tone, bar, unavailable }: {
  href: string; label: string; value?: ReactNode; of?: number; sub?: ReactNode;
  dot?: string; tone?: string; bar?: number; unavailable?: string;
}) {
  return <ScopedLink to={href} className={'stat' + (tone ? ' ' + tone : '')}><div className="label">{label}</div>
    <div className="value">{unavailable ? '—' : <>{value}{of !== undefined && <span className="of"> / {of}</span>}{dot && <span className={'dot ' + dot}/>}</>}</div>
    {!unavailable && bar !== undefined && <div className="bar"><span style={{ width: Math.max(0, Math.min(100, bar * 100)) + '%' }}/></div>}
    {(unavailable || sub) && <p className="sub">{unavailable || sub}</p>}</ScopedLink>;
}
