// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { Component, useEffect, type ReactNode } from 'react';
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { WorkspaceProvider, nativeExperimentURL, remoteWorkspaceURL, scopedURL, useDirectory } from './data';
import { Empty } from './components';
import type { WorkspaceScope } from './types';
import { CostBoard, ExperimentsBoard, Kueue, Observability, Overview, Services } from './Boards';
import { Fleet } from './Fleet';
import { JobDetailBoard, RayBoard, RayHistoryBoard, RunsBoard } from './Workloads';

const tabs = [
  { id: 'platform', label: 'Platform', items: [['Overview', '/portal'], ['Fleet', '/portal/fleet'], ['Kueue', '/portal/jobs'], ['Ray', '/portal/ray'], ['Observability', '/portal/observability'], ['Cost', '/portal/cost']] },
  { id: 'workloads', label: 'Workloads', items: [['Overview', '/portal'], ['Jobs', '/portal/runs'], ['Services', '/portal/services']] },
  { id: 'experiments', label: 'Experiments', items: [['Experiments', '/portal/experiments']] },
];
const fleetPaths = ['/portal/fleet', '/portal/cluster', '/portal/gpu', '/portal/nodes'];
export function tabForPath(path: string): string | undefined {
  if (path === '/stellar' || path.startsWith('/stellar/')) return 'experiments';
  if (fleetPaths.includes(path) || path === '/portal/kueueviz' || path.startsWith('/portal/ray/')) return 'platform';
  if (path.startsWith('/portal/runs/')) return 'workloads';
  return tabs.find(t => t.items.some(([, p]) => p !== '/portal' && p === path))?.id;
}
class BoardBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};
  static getDerivedStateFromError(error: Error) { return { error }; }
  render() { return this.state.error ? <div className="empty warn" role="alert">Failed to load: {this.state.error.message}</div> : this.props.children; }
}
export function ScopeBanner({ scope }: { scope?: WorkspaceScope }) {
  if (!scope) return <div className="scope-banner unavailable">No authorized workspace is available.</div>;
  const fields = [
    ['workspace', scope.name || scope.workspace], ['cluster', scope.cluster], ['namespace', scope.namespace], ['queue', scope.localQueue],
    ['result scope', scope.resultScope], ['authorization', scope.authorizationMode], ['source', scope.source], ['state', scope.availability],
  ];
  return <div className={'scope-banner' + (scope.availability === 'available' ? '' : ' unavailable')}>{fields.map(([label, value]) => <span key={label}><strong>{label}: </strong>{value || 'not configured'}</span>)}</div>;
}
export function App() {
  const location = useLocation(), navigate = useNavigate();
  const params = new URLSearchParams(location.search);
  const workspace = params.get('workspace') || '';
  const directory = useDirectory(workspace);
  // Never display a cached authorized scope after a failed directory refresh.
  const data = directory.isError ? undefined : directory.data;
  const scope = data?.selected, managed = data?.managed === true;
  const requestedPersona = params.get('persona');
  const persona = tabForPath(location.pathname) || tabs.find(tab => tab.id === requestedPersona)?.id || 'workloads';
  const experimentView = location.pathname === '/portal/experiments' || location.pathname === '/stellar' || location.pathname.startsWith('/stellar/');
  const experimentSurface = !!scope && persona === 'experiments' &&
    ['/portal', '/portal/experiments', '/stellar'].some(path => location.pathname === path || path === '/stellar' && location.pathname.startsWith('/stellar/')) &&
    ['available', 'redirect', 'unreachable', 'unsupported'].includes(scope.availability);
  const activeTab = tabs.find(t => t.id === persona)!;
  const href = (path: string) => scopedURL(path === '/portal' ? `/portal?persona=${persona}` : path, scope?.workspace || workspace, managed);
  const needsSelection = !!(managed && scope && !workspace);
  useEffect(() => {
    if (needsSelection && scope) navigate(scopedURL(location.pathname + location.search + location.hash, scope.workspace, true), { replace: true });
  }, [needsSelection, scope, location.pathname, location.search, location.hash, navigate]);
  useEffect(() => {
    if (location.pathname === '/stellar' || location.pathname.startsWith('/stellar/')) {
      navigate(nativeExperimentURL(location.pathname + location.search + location.hash), { replace: true });
    }
  }, [location.pathname, location.search, location.hash, navigate]);
  useEffect(() => {
    if (scope?.availability === 'redirect' && !experimentSurface) {
      const target = remoteWorkspaceURL(scope, location.pathname, location.search, location.hash);
      if (target) window.location.assign(target);
    }
  }, [scope, location.pathname, location.search, location.hash, experimentSurface]);
  function selectPersona(id: string) {
    if (id === persona) return;
    navigate(href(id === 'experiments' ? '/portal/experiments' : `/portal?persona=${id}`));
  }
  const workspacePicker = <label className="workspace-picker">Workspace <select id="workspace-select" aria-label="Workspace" value={scope?.workspace || workspace} disabled={!data || data.workspaces.length < 2} onChange={e => {
      const params = new URLSearchParams(location.search);
      params.set('workspace', e.target.value);
      params.delete('namespace'); params.delete('cluster');
      if (location.pathname === '/portal/experiments') {
        for (const key of [...params.keys()]) {
          if (['pinned', 'metric', 'sections', 'run_id', 'media_run'].includes(key) || key.startsWith('section.')) params.delete(key);
        }
      }
      navigate(location.pathname + '?' + params + location.hash);
    }}>
      {!scope && <option value={workspace}>{workspace || 'No workspace'}</option>}
      {data?.workspaces.map(w => <option key={w.workspace} value={w.workspace}>{w.name || w.workspace}</option>)}
    </select></label>;
  return <div className={experimentView ? 'portal-stellar-shell' : undefined}>
    <header className="topbar"><div className="brand"><strong>TauGrid</strong><div className="eyebrow">Observability Portal</div></div><div className="spacer"/>
    {workspacePicker}
    {experimentView && <details className="stellar-workspace-scope"><summary>Scope</summary><div className="stellar-scope-menu"><ScopeBanner scope={scope}/></div></details>}
    <div className="tab-toggle" aria-label="Portal persona">{tabs.map(t => <button key={t.id} type="button" aria-pressed={persona === t.id} className={persona === t.id ? 'active' : ''} onClick={() => selectPersona(t.id)}>{t.label}</button>)}</div>
  </header><div className="layout">{persona !== 'experiments' && <aside className="sidebar"><nav aria-label={activeTab.label}><div className="group">{activeTab.items.map(([title, path]) => {
    const active = path === '/portal/fleet' ? fleetPaths.includes(location.pathname)
      : path === '/portal/jobs' ? ['/portal/jobs', '/portal/kueueviz'].includes(location.pathname)
        : location.pathname === path || (path !== '/portal' && location.pathname.startsWith(path + '/'));
    return <Link key={path} to={href(path)} className={active ? 'active' : ''} aria-current={active ? 'page' : undefined}>{title}</Link>;
  })}</div></nav></aside>}<main><div id="scope-banner">{!directory.isPending && (!experimentView || scope?.availability !== 'available') && <ScopeBanner scope={scope}/>}</div><div className="card" id="view">
    {directory.isPending || needsSelection ? <div className="empty" role="status">Loading workspace…</div>
      : directory.error ? <div className="empty warn" role="alert">Failed to load: workspace directory: {directory.error.message}</div>
        : !scope ? <Empty>No authorized workspace is available.</Empty>
          : scope.availability !== 'available' && !experimentSurface ? <Empty warn>{scope.availability === 'redirect' ? 'Redirecting to the workspace Portal…' : `Workspace data is ${scope.availability} on this Portal. No local fallback was used.`}</Empty>
            : <WorkspaceProvider key={scope.workspace + ':' + scope.cluster + ':' + scope.namespace} scope={scope} managed={managed}>
              <BoardBoundary key={location.pathname + persona + ':' + new URLSearchParams(location.search).get('target')}>
                <Routes>
                  <Route path="/portal/runs" element={<RunsBoard/>}/>
                  <Route path="/portal/runs/:namespace/:name" element={<JobDetailBoard/>}/>
                  <Route path="/portal/runs/*" element={<Empty warn>Invalid job path: expected /portal/runs/&lt;namespace&gt;/&lt;name&gt;.</Empty>}/>
                  <Route path="/portal/ray" element={<RayBoard/>}/>
                  <Route path="/portal/ray/history/:resourceUID" element={<RayHistoryBoard/>}/>
                  <Route path="/portal/services" element={<Services/>}/>
                  <Route path="/portal/experiments" element={<ExperimentsBoard/>}/>
                  <Route path="/stellar/*" element={<ExperimentsBoard/>}/>
                  {['jobs', 'kueueviz'].map(path => <Route key={path} path={'/portal/' + path} element={<Kueue/>}/>)}
                  {['fleet', 'cluster', 'gpu', 'nodes'].map(path => <Route key={path} path={'/portal/' + path} element={<Fleet/>}/>)}
                  <Route path="/portal/cost" element={<CostBoard/>}/>
                  <Route path="/portal/observability" element={<Observability/>}/>
                  <Route path="*" element={persona === 'experiments'
                    ? <Navigate replace to={href('/portal/experiments' + location.search + location.hash)}/>
                    : <Overview persona={persona}/>}/>
                </Routes>
              </BoardBoundary>
            </WorkspaceProvider>}
  </div></main></div></div>;
}
