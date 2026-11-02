// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { memo, useMemo, useState, type CSSProperties, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { BoardResult, Empty, PageTitle, ScopedLink, TrackingLink, n1, utilizationSummary } from './components';
import { fleetBoardPaths, useBoard, useBoardPrefetch } from './data';
import type { Cluster, Nodes, Overview as OverviewData, Quota } from './types';

const overviewRefreshMs = 15_000;

type FleetNode = Nodes['nodes'][number];

interface SiteGroup {
  id: string;
  label: string;
  region: string;
  nodes: FleetNode[];
  pools: PoolGroup[];
  ready: number;
  gpus: number;
  allocated: number | null;
  available: number | null;
}

interface PoolGroup {
  name: string;
  product: string;
  nodes: FleetNode[];
  gpus: number;
}

function groupSites(snapshot: Nodes | undefined): SiteGroup[] {
  const groups = new Map<string, SiteGroup & { allocationKnown: boolean; poolMap: Map<string, PoolGroup> }>();
  for (const node of snapshot?.nodes || []) {
    if (node.gpuCapacity <= 0) continue;
    const label = node.site || node.region || 'Unassigned';
    let group = groups.get(label);
    if (!group) {
      group = {
        id: label,
        label,
        region: node.region || 'Region unknown',
        nodes: [],
        pools: [],
        ready: 0,
        gpus: 0,
        allocated: 0,
        available: 0,
        allocationKnown: true,
        poolMap: new Map(),
      };
      groups.set(label, group);
    } else if (group.region === 'Region unknown' && node.region) {
      group.region = node.region;
    }
    group.nodes.push(node);
    group.ready += node.ready ? 1 : 0;
    group.gpus += node.gpuCapacity;
    if (node.gpuAllocated === undefined || node.gpuAvailable === undefined) {
      group.allocationKnown = false;
      group.allocated = null;
      group.available = null;
    } else if (group.allocationKnown) {
      group.allocated = (group.allocated ?? 0) + node.gpuAllocated;
      group.available = (group.available ?? 0) + node.gpuAvailable;
    }
    const poolName = node.agentPool || node.sku || 'GPU pool';
    let pool = group.poolMap.get(poolName);
    if (!pool) {
      pool = {
        name: poolName,
        product: node.gpuProduct || node.sku || 'GPU model unknown',
        nodes: [],
        gpus: 0,
      };
      group.poolMap.set(poolName, pool);
      group.pools.push(pool);
    } else {
      const product = node.gpuProduct || node.sku;
      if (pool.product === 'GPU model unknown' && product) pool.product = product;
    }
    pool.nodes.push(node);
    pool.gpus += node.gpuCapacity;
  }
  return [...groups.values()].map(({ allocationKnown: _allocationKnown, poolMap: _poolMap, ...group }) => group)
    .sort((a, b) => b.gpus - a.gpus || a.label.localeCompare(b.label));
}

function Metric({ label, value, detail, tone }: { label: string; value: ReactNode; detail: ReactNode; tone?: string }) {
  return <div className={'overview-metric' + (tone ? ' ' + tone : '')}>
    <span>{label}</span><strong>{value}</strong><small>{detail}</small>
  </div>;
}

function gpuCountLabel(count: number) {
  return `${count} GPU${count === 1 ? '' : 's'}`;
}

function SnapshotTime({ at }: { at?: string }) {
  return at ? <span> · Read <time dateTime={at}>{new Date(at).toLocaleTimeString()}</time></span> : null;
}

function PriorityDetail({ workload }: {
  workload: {
    admissionPriorityClass?: string; admissionPriority?: number; podPriorityClasses?: string[];
  };
}) {
  const admission = workload.admissionPriorityClass
    ? `${workload.admissionPriorityClass}${workload.admissionPriority === undefined ? '' : ` (${workload.admissionPriority})`}`
    : workload.admissionPriority === undefined ? 'Admission priority unknown' : `Priority ${workload.admissionPriority}`;
  const pod = workload.podPriorityClasses?.length
    ? workload.podPriorityClasses.join(', ')
    : 'Pod priority unknown';
  return <small className="overview-priority">
    <span>Admission: {admission}</span>
    <span>Pod: {pod}</span>
  </small>;
}

function CapacityBar({ allocated, available, total }: { allocated: number | null; available: number | null; total: number }) {
  const allocationKnown = allocated !== null && available !== null;
  const allocatedPct = allocationKnown && total > 0 ? Math.max(0, Math.min(100, allocated / total * 100)) : 0;
  return <div className="overview-capacity">
    <div className="overview-capacity-bar" aria-label={allocationKnown
      ? `${allocated} allocated GPUs and ${available} available GPUs`
      : `Allocation unavailable for ${total} GPUs`}>
      {allocationKnown && <span style={{ width: `${allocatedPct}%` }}/>}
    </div>
    <span><b>{allocationKnown ? allocated : '—'}</b> allocated</span>
    <span><b>{allocationKnown ? available : '—'}</b> available</span>
  </div>;
}

function GPUTiles({ node }: { node: FleetNode }) {
  const allocated = node.gpuAllocated;
  const available = node.gpuAvailable;
  const allocationKnown = allocated !== undefined && available !== undefined;
  const style = allocationKnown && node.gpuCapacity > 0 ? {
    '--gpu-allocated': `${Math.max(0, Math.min(100, allocated / node.gpuCapacity * 100))}%`,
    '--gpu-tile-width': `${100 / node.gpuCapacity}%`,
  } as CSSProperties : undefined;
  return <div className="overview-gpu-tiles" aria-label={allocationKnown
    ? `${node.name}: ${allocated} allocated GPUs and ${available} available GPUs`
    : `${node.name}: GPU allocation unavailable`} data-allocation={allocationKnown ? 'known' : 'unknown'} style={style}/>;
}

function SiteSelector({ sites, selected, onSelect }: { sites: SiteGroup[]; selected?: string; onSelect: (id: string) => void }) {
  if (!sites.length) return <Empty>Inventory loaded without site or node detail.</Empty>;
  return <div className="overview-sites" aria-label="GPU sites">
    {sites.map(site => <button key={site.id} type="button"
      className={site.id === selected ? 'active' : ''} aria-pressed={site.id === selected}
      onClick={() => onSelect(site.id)}>
      <span className="overview-site-status">{site.ready === site.nodes.length ? 'Ready' : `${site.ready}/${site.nodes.length} ready`}</span>
      <strong>{site.label}</strong>
      <small>{site.region}</small>
      <span className="overview-site-total">{gpuCountLabel(site.gpus)}</span>
      <CapacityBar allocated={site.allocated} available={site.available} total={site.gpus}/>
    </button>)}
  </div>;
}

function PoolDetails({ site, prefetchFleet }: { site?: SiteGroup; prefetchFleet: () => void }) {
  if (!site) return null;
  return <div className="overview-pools" aria-live="polite">
    <div className="overview-stage-title"><span>Selected site</span><strong>{site.label}</strong></div>
    {site.pools.map(pool => <section key={pool.name} className="overview-pool">
        <div className="overview-pool-head">
          <span><strong>{pool.name}</strong><small>{pool.product}</small></span>
          <b>{gpuCountLabel(pool.gpus)}</b>
        </div>
        <div className="overview-node-list">{pool.nodes.map(node =>
          <ScopedLink key={node.name} to={'/portal/fleet?instance=' + encodeURIComponent(node.name)}
            className="overview-node" onIntent={prefetchFleet}>
            <span><strong>{node.name}</strong><small>{node.ready ? 'Ready' : 'Not ready'} · {node.sku || 'SKU unknown'}</small></span>
            <span className="overview-node-capacity">
              <GPUTiles node={node}/>
              <small>{node.gpuAllocated ?? '—'} allocated · {node.gpuAvailable ?? '—'} available</small>
            </span>
          </ScopedLink>)}
        </div>
      </section>)}
  </div>;
}

function QuotaSummary({ quota, error, loading }: { quota?: Quota; error?: Error | null; loading: boolean }) {
  const workspace = quota?.workspace;
  const workspaceResources = workspace?.resources ?? [];
  const teamResources = quota?.team?.resources ?? [];
  const rows = (items: typeof workspaceResources, usage: boolean, quotaLabel: string) => items.map(item =>
    <div className="overview-quota-row" key={`${item.flavor}/${item.resource}`}>
      <span><strong>{item.flavor}</strong><small>{item.resource}</small></span>
      <span><b>{usage ? item.used ?? '—' : item.nominal}</b><small>{usage ? `used of ${item.nominal}` : quotaLabel}</small></span>
    </div>);
  return <section className="overview-quota" aria-label="Workspace and team quota">
    <div className="overview-workload-group-head"><strong>Workspace quota</strong>
      {quota?.legacy && <span>Legacy</span>}
    </div>
    {loading && !quota ? <p className="overview-workload-empty">Loading live quota…</p>
      : error && !quota ? <div className="overview-unavailable">Live quota unavailable: {error.message}</div>
        : quota && !workspace ? <div className="overview-unavailable">Live quota response did not include workspace quota.</div>
          : quota && workspace && <>
          <div className="overview-quota-scope">
            <span><b>{workspace.name}</b><small>{workspace.cohort || 'No team Cohort'}</small></span>
            <span><b>{workspace.admittedWorkloads}</b><small>admitted workloads</small></span>
          </div>
          <p className="overview-stage-note">Selected workspace: {quota.scope?.namespace || 'namespace unknown'}/{quota.scope?.localQueue || 'queue unknown'}.
            Resource quota and admitted totals below are shared ClusterQueue status, not pod execution.</p>
          {workspaceResources.length ? <div className="overview-quota-rows">
            {rows(workspaceResources, true, 'workspace quota')}
          </div> : <p className="overview-workload-empty">No resource quota is configured on this workspace queue.</p>}
          {!workspace.usageAvailable && <p className="overview-unavailable">ClusterQueue usage is not reported.</p>}
          {quota.team ? <div className="overview-team-quota">
            <div className="overview-workload-group-head"><strong>Team shared quota</strong>
              {quota.team.weightedShare && <span>share {quota.team.weightedShare}</span>}
            </div>
            {teamResources.length ? <div className="overview-quota-rows">
              {rows(teamResources, quota.team.usageAvailable, 'shared quota')}
            </div> : <p className="overview-workload-empty">No shared resource quota is configured for this team.</p>}
            {!quota.team.usageAvailable && <p className="overview-stage-note">{quota.team.usageUnavailable}</p>}
          </div> : quota.teamUnavailable
            ? <div className="overview-unavailable">{quota.teamUnavailable}</div>
            : <p className="overview-stage-note">{quota.legacy
              ? 'This workspace does not use a TauTeam Cohort; workspace quota remains available.'
              : 'No team shared quota is configured.'}</p>}
        </>}
  </section>;
}

const QueueBridge = memo(function QueueBridge({ data, quota, quotaError, quotaLoading }: {
  data: OverviewData; quota?: Quota; quotaError?: Error | null; quotaLoading: boolean;
}) {
  const queue = data.cards.queue;
  const lanes = queue?.queues ?? [];
  const unavailable = data.cards.queueUnavailable || (!queue ? 'Queue data unavailable' : '');
  const reservationKnown = !!queue?.gpuReservationAvailable && !!queue.gpuNominalAvailable;
  const capacity = queue?.gpuNominal;
  const pressure = reservationKnown && capacity && queue?.gpuReserved !== undefined ? queue.gpuReserved / capacity : 0;
  return <div className="overview-queue">
    <div className="overview-stage-title"><span>Scheduler</span><strong>Kueue admission</strong></div>
    <QuotaSummary quota={quota} error={quotaError} loading={quotaLoading}/>
    {unavailable ? <div className="overview-unavailable">{unavailable}</div> : <>
      <div className="overview-queue-dial">
        <span>ClusterQueue GPU reservation</span><strong>{reservationKnown ? queue!.gpuReserved : '—'}<small> / {queue?.gpuNominalAvailable ? capacity : '—'}</small></strong>
        {reservationKnown ? <div className="overview-pressure" aria-label={`${Math.round(pressure * 100)}% of nominal GPU quota reserved`}>
          <span style={{ width: `${Math.max(0, Math.min(100, pressure * 100))}%` }}/>
        </div> : <p className="overview-unavailable">GPU reservation or nominal quota is not reported.</p>}
      </div>
      <div className="overview-queue-counts">
        <span><b>{queue!.admitted}</b> admitted</span>
        <span className={queue!.pending > 0 ? 'warning' : ''}><b>{queue!.pending}</b> pending</span>
        <span><b>{reservationKnown ? queue!.gpuHeadroom : '—'}</b> GPU quota unreserved</span>
      </div>
      <p className="overview-stage-note">Nominal quota minus reservations is not physical GPU availability or a guarantee of admission.
        GPU quota is counted once per shared ClusterQueue; workload counts cover only the LocalQueues below.<SnapshotTime at={queue?.observedAt}/></p>
      {!!lanes.length && <div className="overview-queue-lanes" aria-label="Queue admission lanes">
        {lanes.map(lane => <div className="overview-queue-lane" key={`${lane.namespace}/${lane.queue}`}>
          <div>
            <strong>{lane.queue === 'cpu' ? 'CPU queue' : lane.queue === 'jobqueue' ? 'GPU queue' : lane.queue}</strong>
            <small title={`${lane.namespace}/${lane.queue}`}>{lane.namespace}/{lane.queue}</small>
          </div>
          <span><b>{lane.admitted}</b> admitted</span>
          <span className={lane.pending > 0 ? 'warning' : ''}><b>{lane.pending}</b> pending</span>
        </div>)}
      </div>}
      <section className="overview-pending" aria-label="Pending admission by priority">
        <div className="overview-workload-group-head">
          <strong>Pending admission by priority</strong><span>{data.pending?.length ?? 0}</span>
        </div>
        {!data.pending?.length ? <p className="overview-workload-empty">No workloads waiting for admission.</p>
          : <div className="overview-pending-list">{data.pending.slice(0, 5).map(workload =>
            <div className="overview-pending-workload" key={`${workload.namespace}/${workload.name}`}>
              <span className="overview-workload-state pending">Waiting</span>
              <div>
                <strong>{workload.name}</strong>
                <small>{workload.namespace} · {workload.queue || 'queue unknown'}
                  {workload.gpuRequested ? ` · ${gpuCountLabel(workload.gpuRequested)}` : ''}</small>
                <PriorityDetail workload={workload}/>
                {!!(workload.reason || workload.message) &&
                  <small className="overview-pending-reason">{workload.reason || workload.message}</small>}
              </div>
            </div>)}</div>}
      </section>
    </>}
    <p className="overview-stage-note">Higher Kueue admission priority is considered before FIFO. Quota, flavors, and admission checks still determine eligibility.</p>
    <ScopedLink to="/portal/jobs" className="overview-stage-link">Inspect queues and quota →</ScopedLink>
  </div>;
});

const WorkloadFlow = memo(function WorkloadFlow({ data }: { data: OverviewData }) {
  const admitted = data.running ?? [];
  const active = data.active ?? [];
  const waiting = data.waiting ?? [];
  const cpu: typeof admitted = [];
  const gpu: typeof admitted = [];
  for (const run of admitted) {
    (run.queue === 'cpu' || run.clusterQueue === 'tau-cpu-cq' ? cpu : gpu).push(run);
  }
  type WorkloadRow = (typeof admitted)[number] & { pendingReason?: string };
  const group = (label: string, runs: WorkloadRow[], state = 'Quota admitted') => <section className="overview-workload-group">
    <div className="overview-workload-group-head"><strong>{label}</strong><span>{runs.length}</span></div>
    {!runs.length ? <p className="overview-workload-empty">No {state === 'Waiting for quota' ? 'pending' : 'admitted'} workloads.</p>
      : <div className="overview-workload-list">{runs.slice(0, 5).map(run => <div className="overview-workload" key={`${run.namespace}/${run.name}`}>
        <span className="overview-workload-state">{state}</span>
        <div>
          {run.resourceUid ? <ScopedLink to={'/portal/workloads/' + encodeURIComponent(run.resourceUid)}><strong>{run.job || run.name}</strong></ScopedLink> : <strong>{run.job || run.name}</strong>}
          <small>{run.namespace} · {run.queue || 'queue unknown'}{run.pendingReason ? ` · ${run.pendingReason}` : ''}</small>
          <PriorityDetail workload={run}/>
        </div>
        <TrackingLink run={run} label="Experiment ↗"/>
      </div>)}</div>}
  </section>;
  return <div className="overview-workloads">
    <div className="overview-stage-title"><span>Execution</span><strong>Runtime and admission state</strong></div>
    <p className="overview-stage-note">Job and admission scope: {data.runtimeScopes?.length
      ? data.runtimeScopes.map(scope => `${scope.namespace}/${scope.queue}`).join(', ')
      : 'not reported'}.<SnapshotTime at={data.generatedAt}/></p>
    <div className="overview-workload-groups">
      <section className="overview-workload-group" aria-label="Active jobs">
        <div className="overview-workload-group-head"><strong>Active queued jobs</strong><span>{data.activeUnavailable ? '—' : active.length}</span></div>
        {data.activeUnavailable ? <div className="overview-unavailable">{data.activeUnavailable}</div>
          : !active.length ? <p className="overview-workload-empty">No queued Job or RayJob reports Running in these scopes. Other workloads may still be running.</p>
            : <div className="overview-workload-list">{active.slice(0, 5).map(run =>
              <div className="overview-workload" key={`${run.namespace}/${run.kind}/${run.name}`}>
                <span className="overview-workload-state">Running</span>
                <div>
                  <strong>{run.name}</strong>
                  <small>{run.namespace || 'namespace unknown'} · {run.kind} · {run.age}</small>
                </div>
                <TrackingLink run={run} label="Experiment ↗"/>
              </div>)}</div>}
      </section>
      {data.runningUnavailable ? <div className="overview-unavailable">{data.runningUnavailable}</div>
        : <>
        {group('Waiting for quota', waiting, 'Waiting for quota')}
        {group('GPU quota admitted', gpu)}
        {group('CPU quota admitted', cpu)}
        </>}
    </div>
    <p className="overview-stage-note">Active jobs come from Job and RayJob runtime status, filtered by LocalQueue. Deployments, RayServices, standalone pods, and other namespaces or queues are not included. Admission reserves quota but does not prove execution. Admission priority controls Kueue ordering and workload preemption; pod priority controls Kubernetes scheduling and pod preemption.</p>
    <div className="overview-stage-links">
      <ScopedLink to="/portal/runs" className="overview-stage-link">Inspect active jobs →</ScopedLink>
      <ScopedLink to="/portal/jobs" className="overview-stage-link">Inspect queue admission →</ScopedLink>
    </div>
  </div>;
});

const FleetTopology = memo(function FleetTopology({ sites, nodes, nodeError }: {
  sites: SiteGroup[]; nodes?: Nodes; nodeError?: Error | null;
}) {
  const [selectedSite, setSelectedSite] = useState<string>();
  const activeSite = sites.find(site => site.id === selectedSite) || sites[0];
  const prefetchFleet = useBoardPrefetch(fleetBoardPaths);
  return <div className="overview-fleet-stage">
    <div className="overview-stage-title"><span>Capacity</span><strong>GPU sites</strong></div>
    <p className="overview-stage-note">Cluster-wide Kubernetes nodes and pod requests across all namespaces, including workloads outside Kueue.<SnapshotTime at={nodes?.generatedAt}/></p>
    {nodes?.gpuActivity ? <p className="overview-stage-note" aria-label="Fleet GPU pod activity">
        <strong>Running GPU pods: {nodes.gpuActivity.runningPods}</strong> · {nodes.gpuActivity.pendingPods} pending · {nodes.gpuActivity.otherPods} unknown phase · {nodes.gpuActivity.terminatingPods} terminating.
        Running is pod phase, not application readiness or GPU utilization.
      </p> : <p className="overview-unavailable" aria-label="Fleet GPU pod activity">{nodes?.gpuActivityError || nodes?.gpuAllocationError || 'Fleet GPU pod activity is not reported for this scope.'}</p>}
    {nodeError ? <div className="overview-unavailable">{nodeError.message}</div>
      : <SiteSelector sites={sites} selected={activeSite?.id} onSelect={setSelectedSite}/>}
    <PoolDetails site={activeSite} prefetchFleet={prefetchFleet}/>
    <p className="overview-stage-note">Allocated means GPU requests on non-terminal, node-bound pods, including bound Pending and terminating pods.
      Available means unrequested allocatable GPUs on ready, uncordoned nodes; placement constraints may still prevent scheduling. GPU tiles show counts, not device identity.</p>
    <ScopedLink to="/portal/fleet" className="overview-stage-link" onIntent={prefetchFleet}>Open fleet detail →</ScopedLink>
    <Link to="/portal/gpu-consumers" className="overview-stage-link">Inspect GPU owners (requires fleet access) →</Link>
  </div>;
});

function Atlas({ platform, data, nodes, cluster, quota, nodeError, quotaError, quotaLoading }: {
  platform: boolean; data: OverviewData; nodes?: Nodes; cluster?: Cluster; quota?: Quota;
  nodeError?: Error | null; quotaError?: Error | null; quotaLoading: boolean;
}) {
  const sites = useMemo(() => groupSites(nodes), [nodes]);
  const fleetSummary = useMemo(() => {
    const gpus = cluster?.gpus || [];
    const observedHealth = gpus.filter(gpu => typeof gpu.healthy === 'boolean');
    return {
      utilization: utilizationSummary(gpus),
      observedHealth: observedHealth.length,
      unhealthy: observedHealth.filter(gpu => gpu.healthy === false).length,
    };
  }, [cluster]);
  const queue = data.cards.queue;
  const reservationKnown = !!queue?.gpuReservationAvailable && !!queue.gpuNominalAvailable;
  const capacity = queue?.gpuNominal;

  return <div className="overview-dashboard">
    <div className="overview-metrics" aria-label="Overview metrics">
      {platform ? <>
        <Metric label="Fleet readiness" value={nodes ? `${nodes.readyNodes}/${nodes.totalNodes}` : '—'} detail="nodes ready" tone={nodes && nodes.readyNodes < nodes.totalNodes ? 'warning' : undefined}/>
        <Metric label="Schedulable GPUs" value={nodes?.gpuSchedulable ?? '—'} detail={`${nodes?.gpuAllocationKnown ? nodes.gpuAvailable : '—'} unrequested across fleet`}/>
        <Metric label="Observed health" value={fleetSummary.observedHealth ? fleetSummary.unhealthy : '—'} detail={fleetSummary.observedHealth ? `unhealthy of ${fleetSummary.observedHealth} observed · ${cluster?.window || 'unknown'} window; sample age not reported` : 'no health observations'} tone={fleetSummary.unhealthy ? 'danger' : undefined}/>
        <Metric label="Queue reservations" value={reservationKnown ? `${queue!.gpuReserved}/${capacity}` : '—'} detail={`${queue?.pending ?? '—'} scoped workloads pending`} tone={queue?.pending ? 'warning' : undefined}/>
      </> : <>
        <Metric label="Active queued jobs" value={data.activeUnavailable ? '—' : data.active?.length ?? 0} detail="Job or RayJob in configured queue scopes"/>
        <Metric label="Pending admission" value={queue?.pending ?? '—'} detail="waiting for quota" tone={queue?.pending ? 'warning' : undefined}/>
        <Metric label="GPU reservation" value={reservationKnown ? `${queue!.gpuReserved}/${capacity}` : '—'} detail="ClusterQueue reserved / nominal quota"/>
        <Metric label="Measured utilization" value={fleetSummary.utilization.average === null ? '—' : `${n1(fleetSummary.utilization.average)}%`} detail={`${fleetSummary.utilization.observed}/${fleetSummary.utilization.total} GPUs observed · ${cluster?.window || 'unknown'} window; sample age not reported`}/>
      </>}
    </div>
    <div className="overview-atlas">
      <section className="overview-map" aria-label="Infrastructure topology">
        <header>
          <h2>Infrastructure topology</h2>
          <span>{nodes ? sites.length : '—'} {sites.length === 1 ? 'site' : 'sites'} · {nodes?.gpuNodes ?? '—'} GPU nodes</span>
        </header>
        <div className="overview-flow">
          <FleetTopology sites={sites} nodes={nodes} nodeError={nodeError}/>
          <QueueBridge data={data} quota={quota} quotaError={quotaError} quotaLoading={quotaLoading}/>
          <WorkloadFlow data={data}/>
        </div>
      </section>
    </div>
  </div>;
}

export function Overview({ persona }: { persona: string }) {
  const platform = persona === 'platform';
  const overview = useBoard<OverviewData>(
    platform ? '/api/portal/overview' : '/api/portal/overview?view=workloads',
    true,
    undefined,
    overviewRefreshMs,
  );
  const nodes = useBoard<Nodes>('/api/portal/nodes', true, undefined, overviewRefreshMs);
  const cluster = useBoard<Cluster>('/api/portal/cluster', true, undefined, overviewRefreshMs);
  const quota = useBoard<Quota>('/api/portal/quota', true, undefined, overviewRefreshMs);
  return <><PageTitle title="Overview">{platform
    ? 'See how fleet capacity, scheduler pressure, and active workloads connect.'
    : 'See where your workloads are admitted, what capacity they consume, and where to investigate next.'}</PageTitle>
    <BoardResult query={overview} label="Infrastructure overview"
      sources={[
        { label: 'Fleet capacity', query: nodes },
        { label: 'GPU telemetry', query: cluster },
        { label: 'Workspace quota', query: quota },
      ]}
      autoRefreshMs={overviewRefreshMs}
      partial={!!overview.data && !!(overview.data.cards.queueUnavailable || overview.data.activeUnavailable || overview.data.runningUnavailable)}>
      {data => <Atlas platform={platform} data={data} nodes={nodes.data} cluster={cluster.data} quota={quota.data}
        nodeError={nodes.data ? null : nodes.error} quotaError={quota.data ? null : quota.error} quotaLoading={quota.isLoading}/>}
    </BoardResult>
  </>;
}
