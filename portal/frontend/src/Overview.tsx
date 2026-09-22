// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useState, type ReactNode } from 'react';
import { BoardResult, Empty, PageTitle, ScopedLink, TrackingLink, n1, utilizationSummary } from './components';
import { useBoard } from './data';
import type { Cluster, Cost, Nodes, Overview as OverviewData } from './types';

type FleetNode = Nodes['nodes'][number];

interface SiteGroup {
  id: string;
  label: string;
  region: string;
  nodes: FleetNode[];
  ready: number;
  gpus: number;
  available: number | null;
}

function groupSites(snapshot: Nodes | undefined): SiteGroup[] {
  const groups = new Map<string, FleetNode[]>();
  for (const node of (snapshot?.nodes || []).filter(node => node.gpuCapacity > 0)) {
    const label = node.site || node.region || 'Unassigned';
    groups.set(label, [...(groups.get(label) || []), node]);
  }
  return [...groups.entries()].map(([label, nodes]) => ({
    id: label,
    label,
    region: nodes.find(node => node.region)?.region || 'Region unknown',
    nodes,
    ready: nodes.filter(node => node.ready).length,
    gpus: nodes.reduce((sum, node) => sum + node.gpuCapacity, 0),
    available: nodes.every(node => node.gpuAvailable !== undefined)
      ? nodes.reduce((sum, node) => sum + (node.gpuAvailable ?? 0), 0)
      : null,
  })).sort((a, b) => b.gpus - a.gpus || a.label.localeCompare(b.label));
}

function SourceState({ label, state, detail }: { label: string; state: 'ready' | 'warning' | 'unavailable'; detail: ReactNode }) {
  return <div className="overview-source">
    <span className={'overview-source-mark ' + state} aria-hidden="true"/>
    <div><strong>{label}</strong><span>{detail}</span></div>
  </div>;
}

function Metric({ label, value, detail, tone }: { label: string; value: ReactNode; detail: ReactNode; tone?: string }) {
  return <div className={'overview-metric' + (tone ? ' ' + tone : '')}>
    <span>{label}</span><strong>{value}</strong><small>{detail}</small>
  </div>;
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
      <span className="overview-site-capacity"><b>{site.gpus}</b> GPUs <i>{site.available ?? 'Unknown'} available</i></span>
    </button>)}
  </div>;
}

function PoolDetails({ site }: { site?: SiteGroup }) {
  if (!site) return null;
  const pools = new Map<string, FleetNode[]>();
  for (const node of site.nodes) {
    const pool = node.agentPool || node.sku || 'GPU pool';
    pools.set(pool, [...(pools.get(pool) || []), node]);
  }
  return <div className="overview-pools" aria-live="polite">
    <div className="overview-stage-title"><span>Selected site</span><strong>{site.label}</strong></div>
    {[...pools.entries()].map(([name, nodes]) => {
      const gpus = nodes.reduce((sum, node) => sum + node.gpuCapacity, 0);
      const available = nodes.every(node => node.gpuAvailable !== undefined)
        ? nodes.reduce((sum, node) => sum + (node.gpuAvailable ?? 0), 0)
        : null;
      const product = nodes.find(node => node.gpuProduct)?.gpuProduct || nodes.find(node => node.sku)?.sku || 'GPU model unknown';
      return <ScopedLink key={name} to={'/portal/fleet?pool=' + encodeURIComponent(name)} className="overview-pool">
        <span><strong>{name}</strong><small>{product}</small></span>
        <span><b>{available === null ? `—/${gpus}` : `${available}/${gpus}`}</b><small>GPUs available</small></span>
      </ScopedLink>;
    })}
  </div>;
}

function QueueBridge({ data }: { data: OverviewData }) {
  const queue = data.cards.queue;
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
    </>}
    <ScopedLink to="/portal/jobs" className="overview-stage-link">Inspect queues and quota →</ScopedLink>
  </div>;
}

function WorkloadFlow({ data }: { data: OverviewData }) {
  return <div className="overview-workloads">
    <div className="overview-stage-title"><span>Active work</span><strong>Admitted workloads</strong></div>
    {data.runningUnavailable ? <div className="overview-unavailable">{data.runningUnavailable}</div>
      : !data.running?.length ? <Empty>No admitted workloads right now.</Empty>
        : <div className="overview-workload-list">{data.running.slice(0, 5).map(run => <div className="overview-workload" key={`${run.namespace}/${run.name}`}>
          <span className="overview-workload-state">Admitted</span>
          <div><strong>{run.job || run.name}</strong><small>{run.namespace} · {run.queue || 'queue unknown'}</small></div>
          <TrackingLink run={run} label="Experiment ↗"/>
        </div>)}</div>}
    <ScopedLink to="/portal/runs" className="overview-stage-link">View all workload activity →</ScopedLink>
  </div>;
}

function EvidenceRail({ platform, overview, nodes, cluster, cost, nodeError, clusterError, costError }: {
  platform: boolean; overview: OverviewData; nodes?: Nodes; cluster?: Cluster; cost?: Cost;
  nodeError?: Error | null; clusterError?: Error | null; costError?: Error | null;
}) {
  const utilization = utilizationSummary(cluster?.gpus || []);
  const observedHealth = (cluster?.gpus || []).filter(gpu => typeof gpu.healthy === 'boolean');
  const unhealthy = observedHealth.filter(gpu => gpu.healthy === false).length;
  return <aside className="overview-evidence" aria-label="Operational evidence">
    <div><h3>Operational evidence</h3><p>Independent sources remain explicit when coverage is partial or unavailable.</p></div>
    <SourceState label="Fleet inventory" state={nodeError ? 'unavailable' : nodes ? 'ready' : 'warning'}
      detail={nodeError?.message || (nodes ? `${nodes.readyNodes}/${nodes.totalNodes} nodes ready` : 'Loading inventory')}/>
    <SourceState label="GPU telemetry" state={clusterError ? 'unavailable' : !utilization.observed ? 'warning' : unhealthy ? 'warning' : 'ready'}
      detail={clusterError?.message || (!utilization.observed ? 'No utilization observations' : `${utilization.observed}/${utilization.total} GPUs measured · ${unhealthy} unhealthy`)}/>
    <SourceState label="Queue state" state={overview.cards.queueUnavailable ? 'unavailable' : 'ready'}
      detail={overview.cards.queueUnavailable || `${overview.cards.queue?.admitted ?? 0} admitted · ${overview.cards.queue?.pending ?? 0} pending`}/>
    {platform && <section aria-label="GPU cost" className="overview-cost">
      <SourceState label="Cost telemetry" state={costError ? 'unavailable' : !cost?.costAvailable ? 'warning' : 'ready'}
        detail={costError?.message || (!cost ? 'Loading cost data' : cost.costAvailable ? `${cost.window || 'Current window'} measured` : 'Cost samples unavailable')}/>
      <div><span>Estimated cost</span><strong>{cost?.costAvailable ? `$${cost.totalEstimatedCostUSD.toFixed(2)}` : '—'}</strong></div>
      <div><span>Observed idle GPUs</span><strong>{cost?.idleAvailable ? cost.idleGPUs.length : '—'}</strong></div>
    </section>}
    <ScopedLink to="/portal/observability" className="overview-stage-link">Review observability sources →</ScopedLink>
  </aside>;
}

function Atlas({ platform, data, nodes, cluster, cost, nodeError, clusterError, costError }: {
  platform: boolean; data: OverviewData; nodes?: Nodes; cluster?: Cluster; cost?: Cost;
  nodeError?: Error | null; clusterError?: Error | null; costError?: Error | null;
}) {
  const sites = groupSites(nodes);
  const [selectedSite, setSelectedSite] = useState<string>();
  const activeSite = sites.find(site => site.id === selectedSite) || sites[0];
  const utilization = utilizationSummary(cluster?.gpus || []);
  const observedHealth = (cluster?.gpus || []).filter(gpu => typeof gpu.healthy === 'boolean');
  const unhealthy = observedHealth.filter(gpu => gpu.healthy === false).length;
  const queue = data.cards.queue;
  const capacity = queue ? queue.gpuUsed + queue.gpuHeadroom : 0;

  return <div className="overview-dashboard">
    <div className="overview-metrics" aria-label="Overview metrics">
      {platform ? <>
        <Metric label="Fleet readiness" value={nodes ? `${nodes.readyNodes}/${nodes.totalNodes}` : '—'} detail="nodes ready" tone={nodes && nodes.readyNodes < nodes.totalNodes ? 'warning' : undefined}/>
        <Metric label="Schedulable GPUs" value={nodes?.gpuSchedulable ?? '—'} detail={`${nodes?.gpuAllocationKnown ? nodes.gpuAvailable : '—'} currently available`}/>
        <Metric label="Observed health" value={observedHealth.length ? unhealthy : '—'} detail={observedHealth.length ? `unhealthy of ${observedHealth.length} observed` : 'no health observations'} tone={unhealthy ? 'danger' : undefined}/>
        <Metric label="Queue pressure" value={queue ? `${queue.gpuUsed}/${capacity}` : '—'} detail={`${queue?.pending ?? '—'} workloads pending`} tone={queue?.pending ? 'warning' : undefined}/>
      </> : <>
        <Metric label="Active workloads" value={data.runningUnavailable ? '—' : data.running.length} detail="admitted and unfinished"/>
        <Metric label="Pending admission" value={queue?.pending ?? '—'} detail="waiting for quota" tone={queue?.pending ? 'warning' : undefined}/>
        <Metric label="GPU reservation" value={queue ? `${queue.gpuUsed}/${capacity}` : '—'} detail="reserved / reported quota"/>
        <Metric label="Measured utilization" value={utilization.average === null ? '—' : `${n1(utilization.average)}%`} detail={`${utilization.observed}/${utilization.total} GPUs observed`}/>
      </>}
    </div>
    <div className="overview-atlas">
      <section className="overview-map" aria-label="Infrastructure topology">
        <header>
          <div><h2>Infrastructure topology</h2><p>Follow capacity from GPU sites through admission to active workloads.</p></div>
          <span>{sites.length} {sites.length === 1 ? 'site' : 'sites'} · {nodes?.nodes?.filter(node => node.gpuCapacity > 0).length ?? 0} GPU nodes</span>
        </header>
        <div className="overview-flow">
          <div className="overview-fleet-stage">
            <div className="overview-stage-title"><span>Capacity</span><strong>GPU sites</strong></div>
            {nodeError ? <div className="overview-unavailable">{nodeError.message}</div>
              : <SiteSelector sites={sites} selected={activeSite?.id} onSelect={setSelectedSite}/>}
            <PoolDetails site={activeSite}/>
            <ScopedLink to="/portal/fleet" className="overview-stage-link">Open fleet detail →</ScopedLink>
          </div>
          <QueueBridge data={data}/>
          <WorkloadFlow data={data}/>
        </div>
      </section>
      <EvidenceRail platform={platform} overview={data} nodes={nodes} cluster={cluster} cost={cost}
        nodeError={nodeError} clusterError={clusterError} costError={costError}/>
    </div>
  </div>;
}

export function Overview({ persona }: { persona: string }) {
  const platform = persona === 'platform';
  const overview = useBoard<OverviewData>(platform ? '/api/portal/overview' : '/api/portal/overview?view=workloads');
  const nodes = useBoard<Nodes>('/api/portal/nodes');
  const cluster = useBoard<Cluster>('/api/portal/cluster');
  const costs = useBoard<Cost>('/api/portal/cost', platform);
  return <><PageTitle title="Overview">{platform
    ? 'See how fleet capacity, scheduler pressure, and active workloads connect.'
    : 'See where your workloads are admitted, what capacity they consume, and where to investigate next.'}</PageTitle>
    <BoardResult query={overview} label="Infrastructure overview"
      partial={!!overview.data && !!(overview.data.cards.queueUnavailable || overview.data.runningUnavailable)}>
      {data => <Atlas platform={platform} data={data} nodes={nodes.data} cluster={cluster.data} cost={costs.data}
        nodeError={nodes.error} clusterError={cluster.error} costError={costs.error}/>}
    </BoardResult>
  </>;
}
