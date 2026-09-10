// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import type { ReactNode } from 'react';
import { useLocation, useParams } from 'react-router-dom';
import { useBoard } from './data';
import { BoardResult, Empty, KV, Note, PageTitle, ScopedLink, Subtabs, Table, TrackingLink, text } from './components';
import type { JobDetail, Ray, RayHistory, Run, Runs, SourceDiagnostic } from './types';

const kubeHint = ' — start the portal with Kubernetes access (in-cluster ServiceAccount or --kubeconfig).';
function RunName({ run, namespace }: { run: Run; namespace?: string }) {
  const ns = run.namespace || namespace;
  return ns && run.name ? <ScopedLink to={'/portal/runs/' + encodeURIComponent(ns) + '/' + encodeURIComponent(run.name)}>{run.name}</ScopedLink> : text(run.name);
}
function Status({ value, tone = '' }: { value?: string; tone?: string }) { return <span className={'badge ' + tone}>{text(value)}</span>; }
function SourceResult({ diagnostic, label, children }: { diagnostic?: SourceDiagnostic; label: string; children: ReactNode }) {
  if (!diagnostic) return <Note warn>{label}: source status not reported by this portal version.</Note>;
  if (diagnostic.state === 'ready' || diagnostic.state === 'empty') return <>{children}</>;
  return <><Note warn={diagnostic.state !== 'not_configured'}>{label} {diagnostic.state === 'not_configured' ? 'not configured' : 'unavailable'}: {diagnostic.message || 'This source could not be read. Retry this detail view.'}
    {diagnostic.stale && <> Showing stale data from the last successful section read{diagnostic.lastSuccessAt ? ' at ' + new Date(diagnostic.lastSuccessAt).toLocaleString() : ''}.</>}</Note>
    {diagnostic.stale && children}</>;
}
function retainJobSections(previous: JobDetail | undefined, next: JobDetail): JobDetail {
  const sameJob = previous && previous.name === next.name && previous.namespace === next.namespace &&
    previous.kind === next.kind && !!next.resourceUid && previous.resourceUid === next.resourceUid;
  let result = next;
  for (const key of ['workloads', 'pods', 'events'] as const) {
    const diagnostic = next.diagnostics?.[key];
    if (!diagnostic) continue;
    const prior = sameJob ? previous.diagnostics?.[key] : undefined;
    if (diagnostic.state === 'ready' || diagnostic.state === 'empty') {
      result = { ...result, diagnostics: { ...result.diagnostics, [key]: { ...diagnostic, stale: false, lastSuccessAt: Date.now() } } };
    } else if (diagnostic.state === 'unavailable' && prior?.lastSuccessAt &&
      (prior.state === 'ready' || prior.state === 'empty' || prior.stale)) {
      // Previous data comes only from this authorized query cache; 4xx errors purge it.
      result = { ...result, [key]: previous?.[key], diagnostics: { ...result.diagnostics,
        [key]: { ...diagnostic, stale: true, lastSuccessAt: prior.lastSuccessAt } } };
    }
  }
  return result;
}
function HistoryDiagnostic({ state, diagnostic, ray = false }: { state?: string; diagnostic?: string; ray?: boolean }) {
  return state === 'history-unavailable' ? <Note warn>{diagnostic || `Durable ${ray ? 'RayJob' : 'run'} history is temporarily unavailable; showing live ${ray ? 'dashboards' : 'Kubernetes workloads'} only.`}</Note> : null;
}
export function RunsBoard() {
  const query = useBoard<Runs>('/api/portal/runs');
  return <><div className="page-head"><div><PageTitle title="Jobs">Tau-managed training and inference workloads.</PageTitle></div>
    <button className="btn-primary" disabled title="Submission from the portal is coming soon — the portal is read-only today.">+ Submit new</button></div>
    <BoardResult query={query} label="Jobs board" hint={kubeHint}>{snap => {
      const runs = snap.runs || [];
      const groups = [
        { name: 'Running', dot: 'green', tone: 'run', rows: runs.filter(r => r.status === 'Running') },
        { name: 'Queued', dot: 'amber', tone: 'queue', rows: runs.filter(r => ['Pending', 'Suspended'].includes(r.status)) },
        { name: 'Recent', dot: 'red', tone: '', rows: runs.filter(r => !['Running', 'Pending', 'Suspended'].includes(r.status)) },
      ];
      return <><Note>namespace: {snap.namespace || 'all namespaces'} · runs: {snap.total ?? 0} · history: {snap.historyState || 'live-only'}</Note>
        <HistoryDiagnostic state={snap.historyState} diagnostic={snap.historyDiagnostic}/>
        {!runs.length ? <Empty>No Tau-managed workloads in {snap.namespace ? 'namespace ' + snap.namespace : 'any namespace'}. Submit one with tau run submit.</Empty>
          : <>{groups.filter(g => g.rows.length).map(g => <section key={g.name}><div className="section-head"><span className={'dot ' + g.dot}/>{g.name}<span className="count">({g.rows.length})</span></div>
            {g.name === 'Recent' ? <Table headers={['Name', 'Kind', 'Result', 'Experiment', '#Age']} rows={g.rows.map(r => [<RunName run={r} namespace={snap.namespace}/>, <Status value={r.kind} tone="kind"/>, <Status value={r.status} tone={r.status === 'Failed' ? 'fail' : 'done'}/>, <TrackingLink run={r}/>, text(r.age)])}/>
              : <div className="joblist">{g.rows.map((r, i) => <div className="jobcard" key={r.namespace + '/' + r.name + i}><div className="top"><span className="name"><RunName run={r} namespace={snap.namespace}/></span><Status value={r.kind} tone="kind"/><Status value={r.status} tone={g.tone}/></div>
                <div className="meta"><span>age {text(r.age)}</span><span>Experiment: </span><TrackingLink run={r}/></div></div>)}</div>}
          </section>)}<Note>Progress, ETA, and training metrics will be available in a future update.</Note></>}
      </>;
    }}</BoardResult></>;
}
export function JobDetailBoard() {
  const { namespace = '', name = '' } = useParams();
  const query = useBoard<JobDetail>('/api/portal/runs/' + encodeURIComponent(namespace) + '/' + encodeURIComponent(name), !!namespace && !!name, retainJobSections);
  const partial = Object.values(query.data?.diagnostics || {}).some(diagnostic => diagnostic.state === 'unavailable');
  const requested = new URLSearchParams(useLocation().search).get('view') || '';
  const active = ['overview', 'pods', 'events', 'results'].includes(requested) ? requested : 'overview';
  return <><div className="page-head"><div><PageTitle title={name || '—'}>namespace: {namespace || '—'}</PageTitle></div><ScopedLink to="/portal/runs" className="back">← Back to Jobs</ScopedLink></div>
    {!namespace || !name ? <Empty warn>Invalid job path: expected /portal/runs/&lt;namespace&gt;/&lt;name&gt;.</Empty> : <BoardResult query={query} label="Job detail" partial={partial} hint=" — the workload may have been garbage-collected, or the portal lacks Kubernetes access.">{snap => <>
      <div className="detail-meta"><Status value={snap.kind} tone="kind"/><Status value={snap.status}/>
        {snap.resourceRelease && <span className={'badge' + (snap.resourceRelease.computeState === 'reusable' ? '' : ' warn')} title={snap.resourceRelease.message}>quota {snap.resourceRelease.quotaState || 'unknown'} · compute {snap.resourceRelease.computeState || 'unknown'}</span>}
        {snap.object?.age && <span>age {snap.object.age}</span>}{snap.runId && <span className="muted">run-id {snap.runId}</span>}
        {snap.links?.stellarPath && <ScopedLink className="btn-primary" to={snap.links.stellarPath}>Open in Experiments</ScopedLink>}
        {snap.links?.rayDashboardPath && (snap.links.rayDashboardReachable ? <ScopedLink to={snap.links.rayDashboardPath} external className="back">Ray dashboard ↗</ScopedLink> : <span className="back disabled-link" title="Ray dashboard not reachable: the cluster head pod is not Ready">Ray dashboard ↗</span>)}
      </div>
      <SourceResult diagnostic={snap.diagnostics?.tracking} label="Experiment tracking">
        {snap.diagnostics?.tracking.state === 'empty' && <Note>{snap.diagnostics.tracking.message || 'No indexed metrics were found; metric offload may be disabled or indexing may still be pending.'}</Note>}
      </SourceResult>
      <Subtabs active={active} items={[['overview', 'Overview'], ['pods', 'Pods'], ['events', 'Events'], ['results', 'Results']]}/>
      {active === 'overview' && <JobOverview snap={snap}/>}
      {active === 'pods' && <SourceResult diagnostic={snap.diagnostics?.pods} label="Pods">{!snap.pods?.length ? <Empty>No pods found for this run — it may not be scheduled yet, or the objects were garbage-collected.</Empty>
        : <Table headers={['Name', 'Phase', 'Node', '#Restarts']} rows={snap.pods.map(p => [text(p.name), <Status value={p.phase}/>, p.nodePath ? <ScopedLink to={p.nodePath}>{text(p.node)}</ScopedLink> : text(p.node), p.restarts ?? 0])}/>}</SourceResult>}
      {active === 'events' && <SourceResult diagnostic={snap.diagnostics?.events} label="Events">{!snap.events?.length ? <Empty>No recent events for this run.</Empty>
        : <Table headers={['Type', 'Reason', 'Message', '#Count', 'Last']} rows={snap.events.map(e => [<Status value={e.type} tone={e.type === 'Warning' ? 'fail' : 'done'}/>, text(e.reason), text(e.message), e.count ?? 0, text(e.last)])}/>}</SourceResult>}
      {active === 'results' && (snap.diagnostics?.tracking.state === 'ready' || snap.diagnostics?.tracking.state === 'empty' ? <>{snap.lifecycle && <><h2>Run results (durable)</h2><KV rows={[
        ['State', snap.lifecycle.effectiveState || snap.lifecycle.state], ['Reason', snap.lifecycle.reason], ['Message', snap.lifecycle.message],
        ['Completed', snap.lifecycle.completionTime], ['Artifact URI', snap.lifecycle.artifactUri], ['Checkpoint URI', snap.lifecycle.checkpointUri],
      ]}/></>}<Note>{snap.links?.stellarPath ? 'Training metrics live in Experiments (use the Open in Experiments link above).' : 'No experiment link is available for this run. See the tracking status above; Jobs remain visible independently of metrics indexing.'}{snap.runId && !snap.lifecycle && " No durable results row for this run-id yet — it appears once the run's terminal lifecycle lands in Kusto."}</Note></>
        : <Note>Durable results are unavailable; see the experiment tracking status above.</Note>)}
    </>}</BoardResult>}</>;
}
function JobOverview({ snap }: { snap: JobDetail }) {
  const o = snap.object;
  const release = snap.resourceRelease;
  return <><h2>Object</h2><KV rows={[
    ['Created', o?.created], ['Deployment status', o?.jobDeploymentStatus], ['Ray cluster', o?.rayClusterName], ['Ray job id', o?.jobId],
    ['Execution target', o?.executionTarget], ['Reason', o?.reason], ['Message', o?.message],
  ]}/>{release && <><h2>Resource release</h2><KV rows={[
    ['Quota', release.quotaState], ['Physical compute', release.computeState], ['Active Ray pods', release.activePods], ['Nodes still held', release.nodes?.join(', ')], ['Diagnostic', release.message],
  ]}/></>}<h2>Kueue admission</h2>
    <SourceResult diagnostic={snap.diagnostics?.workloads} label="Kueue admission">{!snap.workloads?.length ? <Empty>No Kueue Workload is associated with this run.</Empty> : <Table headers={['Workload', 'Queue', 'ClusterQueue', 'Admitted', 'Finished']}
      rows={snap.workloads.map(w => [text(w.name), text(w.queue), text(w.clusterQueue), w.admitted ? 'yes' : 'no', w.finished ? 'yes' : 'no'])}/>}</SourceResult></>;
}
export function RayBoard() {
  const query = useBoard<Ray>('/api/portal/ray');
  return <><PageTitle title="Ray">Per-cluster Ray dashboards discovered from &lt;cluster&gt;-head-svc Services, via /api/portal/ray. The dashboard is proxied live by the portal and is only available while the RayCluster is running; finished RayJobs remain visible under RayJob history.</PageTitle>
    <BoardResult query={query} label="Ray board" hint={kubeHint}>{snap => <><Note>clusters: {snap.total ?? 0}</Note>
      {!snap.clusters?.length ? <Empty>No Ray clusters found. Either no RayClusters are running, or the portal has no Kubernetes access.</Empty>
        : <Table headers={['Cluster', 'Namespace', 'Service', 'Type', 'Dashboard']} rows={snap.clusters.map(c => [text(c.name), text(c.namespace), text(c.service), text(c.type),
          c.proxyPath ? (c.available ? <ScopedLink to={c.proxyPath} external className="back">open ↗</ScopedLink> : <span className="back disabled-link" title="Ray dashboard unreachable: head pod not Ready">open ↗</span>) : <span className="warn">—</span>])}/>}
      <h2>RayJob history</h2><Note>history: {snap.historyState || 'live-only'}</Note><HistoryDiagnostic state={snap.historyState} diagnostic={snap.historyDiagnostic} ray/>
      {!snap.history?.length ? <Empty>{snap.historyState === 'available' ? 'No durable RayJob records in this scope yet.' : 'Durable RayJob history is not configured for this portal; only live dashboards are shown.'}</Empty>
        : <Table headers={['Name', 'Namespace', 'Status', 'Age', 'Run ID']} rows={snap.history.map(r => [r.resourceUid && r.name ? <ScopedLink to={'/portal/ray/history/' + encodeURIComponent(r.resourceUid)}>{r.name}</ScopedLink> : text(r.name), r.namespace || snap.namespace || '—', <Status value={r.status}/>, text(r.age), text(r.runId)])}/>}
    </>}</BoardResult></>;
}
export function RayHistoryBoard() {
  const { resourceUID = '' } = useParams();
  const query = useBoard<RayHistory>('/api/portal/ray/history/' + encodeURIComponent(resourceUID), !!resourceUID);
  return <><ScopedLink to="/portal/ray" className="back">← Ray</ScopedLink><PageTitle title="RayJob history">Durable lifecycle from ADX. This page does not read Kubernetes, so it remains available after RayCluster cleanup.</PageTitle>
    <BoardResult query={query} label="Durable RayJob history">{snap => {
      const last = snap.events?.at(-1);
      return !last ? <Empty>No durable lifecycle observations found.</Empty> : <><h2>Durable run metadata</h2><KV rows={[
        ['Name', last.name], ['Run ID', last.runId], ['Durable ID', last.durableId], ['Resource UID', last.resourceUid], ['Namespace', last.namespace], ['Cluster', last.cluster],
        ['Queue', last.queue], ['Image', last.image], ['Command', last.command], ['Result PVC', last.resultPvc], ['Result path', last.resultPath], ['Artifact URI', last.artifactUri], ['Checkpoint URI', last.checkpointUri],
      ]}/><h2>Lifecycle timeline</h2><Table headers={['Observed', 'State', 'Reason', 'Message']} rows={snap.events.map(e => [text(e.observedAt), <Status value={e.state}/>, text(e.reason), text(e.message)])}/></>;
    }}</BoardResult></>;
}
