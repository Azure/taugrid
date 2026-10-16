// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useLocation } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { APIError, useBoard, useScopedURL, useWorkspace } from './data';
import { BoardResult, Empty, Note, PageTitle, ProfileReadiness, ScopedLink, Subtabs, Table, measured, n1, text } from './components';
import type { Cost, CostCoverage, Jobs } from './types';
import { StellarWorkspace } from './stellar/Workspace';

export { Overview } from './Overview';
export function ExperimentsBoard() {
  return <StellarWorkspace/>;
}
export function Kueue() {
  const location = useLocation();
  const live = location.pathname === '/portal/kueueviz' || new URLSearchParams(location.search).get('view') === 'live';
  return <><PageTitle title="Kueue">Kueue queue pressure and admission — a scheduler snapshot plus the live KueueViz dashboard.</PageTitle>
    <Subtabs active={live ? 'live' : 'scheduler'} base="/portal/jobs" items={[['scheduler', 'Scheduler'], ['live', 'Live']]}/>
    {live ? <KueueLive/> : <Scheduler/>}</>;
}
function Scheduler() {
  const query = useBoard<Jobs>('/api/portal/jobs');
  return <><p className="muted">Computed GPU quota and queue pressure for the authorized workspace or configured operator scopes. Use Kueue (Live) for raw cluster-wide scheduler state.</p>
    {query.error instanceof APIError && query.error.status === 503 && query.error.state === 'setup_required' && <Empty><strong>Jobs board setup required</strong><p>Portal is running normally. Configure an authorized workspace scope or explicit operator scopes before enabling this computed board.</p><Note>Helm: portal.jobs.scopeMode=workspace or operator.</Note></Empty>}
    <BoardResult query={query} label="Jobs board">{snap => <><Note>scope: {snap.namespace || 'configured namespaces'}</Note><ProfileReadiness state={snap.workloadProfiles}/>
        {snap.hints?.map(h => <div key={h} className="warn">⚠ {h}</div>)}
        {!snap.groups?.length ? <Empty>No queue groups match. The cluster may have no Kueue queues configured.</Empty>
          : <Table headers={['Namespace', 'Team', 'Lane', 'GPU class', 'Queue', '#Pending', '#Admitted', '#GPU used', '#GPU nominal', '#Headroom']}
            rows={snap.groups.map(g => [text(g.namespace), text(g.team), text(g.lane), text(g.gpuClass), text(g.queue), g.pending, g.admitted, g.gpuUsed, g.gpuNominal,
              <span className={g.queueFound && g.quotaFound && g.pending > 0 && g.gpuHeadroom === 0 ? 'warn' : ''}>{g.gpuHeadroom}</span>])}/>}</>}</BoardResult>
  </>;
}
function KueueLive() {
  const scoped = useScopedURL(), { scope } = useWorkspace();
  const url = scoped('/api/portal/kueueviz/');
  const query = useQuery({
    queryKey: ['kueueviz', scope.workspace, scope.cluster, scope.namespace, url],
    queryFn: async ({ signal }) => {
      const response = await fetch(url, { signal });
      if (response.status === 503) throw new Error('this portal was started without --kueueviz. Enable the KueueViz reverse proxy to use this board.');
      if (!response.ok) throw new APIError(response.status, '', 'The KueueViz backend/frontend Services may not be deployed.');
      return true;
    },
  });
  return <><p className="muted">Live KueueViz dashboard — real-time queues, workloads, cluster-queues over WebSocket.</p><Note>Live KueueViz dashboard, reverse-proxied through the portal — <ScopedLink to="/api/portal/kueueviz/" external>open in a full page ↗</ScopedLink> for more room.</Note>
    <BoardResult query={query} label="The Kueue (Live) board" live>{() => <iframe className="stellar" src={url} title="Kueue (Live) — KueueViz"/>}</BoardResult></>;
}
function allocationCoverage(coverage: CostCoverage | undefined, field: 'gpuHoursSamples' | 'costSamples') {
  if (!coverage || !measured(coverage.observedSamples) || !measured(coverage[field])) return 'Coverage not reported';
  return `${coverage[field] < coverage.observedSamples ? 'Partial: ' : ''}${coverage[field]} / ${coverage.observedSamples} allocation samples measured`;
}
function idleCoverage(snapshot: Cost) {
  const coverage = snapshot.idleCoverage;
  if (!coverage) return 'Idle coverage not reported; unobserved GPUs are unknown.';
  const partial = coverage.eligibleGPUs < coverage.observedGPUs || coverage.validSamples < coverage.observedSamples;
  return `${partial ? 'Partial: ' : ''}${coverage.eligibleGPUs} / ${coverage.observedGPUs} observed GPUs have enough samples · ${coverage.measuredGPUs} measured GPUs · ${coverage.validSamples} / ${coverage.observedSamples} valid readings. Unobserved GPUs are unknown.`;
}
export function CostBoard() {
  const query = useBoard<Cost>('/api/portal/cost');
  return <><PageTitle title="Cost">Allocation-based GPU-hours and estimated cost by TauGrid workspace, via /api/portal/cost. Utilization is shown as an efficiency signal and does not determine cost.</PageTitle>
    <BoardResult query={query} label="Cost board" hint=" — start the portal with a --kusto-query-command.">{snap => <>
      <Note>window: {text(snap.window)} · total GPU-hours: {n1(snap.gpuHoursAvailable ? snap.totalGPUHours : null)} · estimated cost: {snap.costAvailable ? '$' + snap.totalEstimatedCostUSD.toFixed(2) : '—'}</Note>
      <Note>GPU-hours: {allocationCoverage(snap.costCoverage, 'gpuHoursSamples')} · cost: {allocationCoverage(snap.costCoverage, 'costSamples')}. Availability means observed samples, not complete window coverage.</Note>
      {!snap.workspaces?.length ? <Empty>No workspace GPU allocations in the window.</Empty> : <><h3>Cost by workspace</h3>
        <Table headers={['Workspace', 'Namespace', '#GPU-hours', '#Est. cost', '#Peak GPUs', '#Avg util %', 'Coverage']} rows={snap.workspaces.map(w => [text(w.workspace), text(w.namespace), n1(w.gpuHoursAvailable ? w.gpuHours : null), w.costAvailable ? '$' + w.estimatedCostUSD.toFixed(2) : '—', w.peakGPUs.toLocaleString(undefined, { maximumFractionDigits: 2 }), n1(w.avgUtilPct),
          `GPU-hours: ${allocationCoverage(w.coverage, 'gpuHoursSamples')} · cost: ${allocationCoverage(w.coverage, 'costSamples')} · utilization: ${w.coverage?.utilizationSamples ?? 'not reported'} valid readings`])}/></>}
      <h3>Idle / underutilized GPUs</h3><Note>{idleCoverage(snap)}</Note>
      {!snap.idleAvailable ? <Empty warn>Idle telemetry unavailable: insufficient utilization samples to assess idle GPUs.</Empty>
        : !snap.idleGPUs?.length ? <Empty>No idle GPUs among {snap.idleCoverage?.eligibleGPUs ?? 'the eligible'} observed GPUs.</Empty> : <Table headers={['Instance', 'GPU', 'Model', 'Namespace', 'Pod', '#Avg util %', '#Samples']}
        rows={snap.idleGPUs.map(g => [g.instance ? <ScopedLink to={'/portal/cluster?instance=' + encodeURIComponent(g.instance)} className="back">{g.instance}</ScopedLink> : '—', text(g.gpu), text(g.modelName), text(g.namespace), text(g.pod), <span className="warn">{n1(g.avgUtilPct)}</span>, g.samples])}/>}
    </>}</BoardResult></>;
}
export function Services() {
  return <><PageTitle title="Services">Long-running inference and serving endpoints.</PageTitle><Empty>This board is not implemented yet. Serving endpoints (Ray Serve / KServe) have no portal-readable data source today.</Empty></>;
}
export function Observability() {
  return <><PageTitle title="Observability">adx-mon ingestion pipeline health — Collector, Ingestor, Alerter, and AlertRule status.</PageTitle><Empty>This board is not available yet. It will be available in a future update.</Empty></>;
}
