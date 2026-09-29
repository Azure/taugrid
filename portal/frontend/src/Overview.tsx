// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { memo, useMemo, useState, type CSSProperties, type ReactNode } from 'react';
import { BoardResult, Empty, PageTitle, ScopedLink, TrackingLink, n1, utilizationSummary } from './components';
import { fleetBoardPaths, useBoard, useBoardPrefetch } from './data';
import type { Cluster, Nodes, Overview as OverviewData } from './types';

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

const QueueBridge = memo(function QueueBridge({ data }: { data: OverviewData }) {
  const queue = data.cards.queue;
  const lanes = queue?.queues?.filter(lane => lane.admitted > 0 || lane.pending > 0) ?? [];
  const unavailable = data.cards.queueUnavailable || (!queue ? 'Queue data unavailable' : '');
  const capacity = queue ? queue.gpuUsed + queue.gpuHeadroom : 0;
  const pressure = queue && capacity > 0 ? queue.gpuUsed / capacity : 0;
  return <div className="overview-queue">
    <div className="overview-stage-title"><span>Scheduler</span><strong>Kueue admission</strong></div>
    {unavailable ? <div className="overview-unavailable">{unavailable}</div> : <>
      <div className="overview-queue-dial">
        <span>GPU reservation</span><strong>{queue!.gpuUsed}<small> / {capacity}</small></strong>
        <div className="overview-pressure" aria-label={`${Math.round(pressure * 100)}% of reported GPU quota reserved`}>
          <span style={{ width: `${Math.max(0, Math.min(100, pressure * 100))}%` }}/>
        </div>
      </div>
      <div className="overview-queue-counts">
        <span><b>{queue!.admitted}</b> admitted</span>
        <span className={queue!.pending > 0 ? 'warning' : ''}><b>{queue!.pending}</b> pending</span>
        <span><b>{queue!.gpuHeadroom}</b> GPUs free</span>
      </div>
      {!!lanes.length && <div className="overview-queue-lanes" aria-label="Queue admission lanes">
        {lanes.map(lane => <div className="overview-queue-lane" key={`${lane.namespace}/${lane.queue}`}>
          <div>
            <strong>{lane.queue === 'cpu' ? 'CPU queue' : lane.queue === 'jobqueue' ? 'GPU queue' : lane.queue}</strong>
            <small>{lane.namespace}/{lane.queue}</small>
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
    <div className="overview-workload-groups">
      <section className="overview-workload-group" aria-label="Active jobs">
        <div className="overview-workload-group-head"><strong>Active jobs</strong><span>{active.length}</span></div>
        {data.activeUnavailable ? <div className="overview-unavailable">{data.activeUnavailable}</div>
          : !active.length ? <p className="overview-workload-empty">No Job or RayJob is currently running.</p>
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
    <p className="overview-stage-note">Active jobs come from Job and RayJob runtime status. Admission reserves quota but does not prove execution. Admission priority controls Kueue ordering and workload preemption; pod priority controls Kubernetes scheduling and pod preemption.</p>
    <div className="overview-stage-links">
      <ScopedLink to="/portal/runs" className="overview-stage-link">Inspect active jobs →</ScopedLink>
      <ScopedLink to="/portal/jobs" className="overview-stage-link">Inspect queue admission →</ScopedLink>
    </div>
  </div>;
});

const FleetTopology = memo(function FleetTopology({ sites, nodeError }: {
  sites: SiteGroup[]; nodeError?: Error | null;
}) {
  const [selectedSite, setSelectedSite] = useState<string>();
  const activeSite = sites.find(site => site.id === selectedSite) || sites[0];
  const prefetchFleet = useBoardPrefetch(fleetBoardPaths);
  return <div className="overview-fleet-stage">
    <div className="overview-stage-title"><span>Capacity</span><strong>GPU sites</strong></div>
    {nodeError ? <div className="overview-unavailable">{nodeError.message}</div>
      : <SiteSelector sites={sites} selected={activeSite?.id} onSelect={setSelectedSite}/>}
    <PoolDetails site={activeSite} prefetchFleet={prefetchFleet}/>
    <ScopedLink to="/portal/fleet" className="overview-stage-link" onIntent={prefetchFleet}>Open fleet detail →</ScopedLink>
  </div>;
});

function Atlas({ platform, data, nodes, cluster, nodeError }: {
  platform: boolean; data: OverviewData; nodes?: Nodes; cluster?: Cluster; nodeError?: Error | null;
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
  const capacity = queue ? queue.gpuUsed + queue.gpuHeadroom : 0;

  return <div className="overview-dashboard">
    <div className="overview-metrics" aria-label="Overview metrics">
      {platform ? <>
        <Metric label="Fleet readiness" value={nodes ? `${nodes.readyNodes}/${nodes.totalNodes}` : '—'} detail="nodes ready" tone={nodes && nodes.readyNodes < nodes.totalNodes ? 'warning' : undefined}/>
        <Metric label="Schedulable GPUs" value={nodes?.gpuSchedulable ?? '—'} detail={`${nodes?.gpuAllocationKnown ? nodes.gpuAvailable : '—'} currently available`}/>
        <Metric label="Observed health" value={fleetSummary.observedHealth ? fleetSummary.unhealthy : '—'} detail={fleetSummary.observedHealth ? `unhealthy of ${fleetSummary.observedHealth} observed` : 'no health observations'} tone={fleetSummary.unhealthy ? 'danger' : undefined}/>
        <Metric label="Queue pressure" value={queue ? `${queue.gpuUsed}/${capacity}` : '—'} detail={`${queue?.pending ?? '—'} workloads pending`} tone={queue?.pending ? 'warning' : undefined}/>
      </> : <>
        <Metric label="Active jobs" value={data.activeUnavailable ? '—' : data.active?.length ?? 0} detail="Job or RayJob reporting Running"/>
        <Metric label="Pending admission" value={queue?.pending ?? '—'} detail="waiting for quota" tone={queue?.pending ? 'warning' : undefined}/>
        <Metric label="GPU reservation" value={queue ? `${queue.gpuUsed}/${capacity}` : '—'} detail="reserved / reported quota"/>
        <Metric label="Measured utilization" value={fleetSummary.utilization.average === null ? '—' : `${n1(fleetSummary.utilization.average)}%`} detail={`${fleetSummary.utilization.observed}/${fleetSummary.utilization.total} GPUs observed`}/>
      </>}
    </div>
    <div className="overview-atlas">
      <section className="overview-map" aria-label="Infrastructure topology">
        <header>
          <h2>Infrastructure topology</h2>
          <span>{sites.length} {sites.length === 1 ? 'site' : 'sites'} · {nodes?.gpuNodes ?? 0} GPU nodes</span>
        </header>
        <div className="overview-flow">
          <FleetTopology sites={sites} nodeError={nodeError}/>
          <QueueBridge data={data}/>
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
  return <><PageTitle title="Overview">{platform
    ? 'See how fleet capacity, scheduler pressure, and active workloads connect.'
    : 'See where your workloads are admitted, what capacity they consume, and where to investigate next.'}</PageTitle>
    <BoardResult query={overview} label="Infrastructure overview"
      sources={[
        { label: 'Fleet capacity', query: nodes },
        { label: 'GPU telemetry', query: cluster },
      ]}
      autoRefreshMs={overviewRefreshMs}
      partial={!!overview.data && !!(overview.data.cards.queueUnavailable || overview.data.activeUnavailable || overview.data.runningUnavailable)}>
      {data => <Atlas platform={platform} data={data} nodes={nodes.data} cluster={cluster.data}
        nodeError={nodes.data ? null : nodes.error}/>}
    </BoardResult>
  </>;
}
