// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { memo, useEffect, useMemo, useState, type ReactNode } from 'react';
import { useLocation } from 'react-router-dom';
import { boardStaleTimeMs, useBoard } from './data';
import { Empty, Note, ScopedLink, Table, measured, n1, utilizationSummary } from './components';
import type { Cluster, GPU, Nodes, NodeUtil } from './types';

const conditionFreshnessMs = 15 * 60 * 1000;
const futureClockSkewMs = 60 * 1000;
const nodeMetricsFreshnessMs = 2 * 60 * 1000;
const initialDetailedSiteLimit = 3;
export const initialFleetNodeLimit = 48;
const initialIndependentEvidenceLimit = 100;
const gpuConditionRequirements = [
  { type: 'DcgmExporterUnavailable' },
  { type: 'NvidiaSmiProblem' },
  { type: 'NvidiaDeviceFilesProblem' },
  { type: 'GPUMissing' },
] as const;
const ibConditionRequirements = [
  { type: 'IBLinkDown', reason: 'IBLinkDownObserved' },
  { type: 'IBSymbolError', reason: 'IBSymbolErrorObserved' },
] as const;

const known = (value: ReactNode) => value === undefined || value === null || value === '' ? 'Unknown' : value;
const isCount = (value: unknown): value is number => typeof value === 'number' && Number.isInteger(value) && value >= 0;

function sourceFreshness(query: { dataUpdatedAt: number; isError: boolean; isFetching: boolean; isStale: boolean }, now: number) {
  if (query.isFetching) return 'Refreshing';
  if (query.isError) return query.dataUpdatedAt > 0
    ? <>Stale · last success <time dateTime={new Date(query.dataUpdatedAt).toISOString()}>{new Date(query.dataUpdatedAt).toLocaleTimeString()}</time></>
    : 'Unavailable';
  if (query.dataUpdatedAt > 0 && (query.isStale || now >= query.dataUpdatedAt + boardStaleTimeMs)) {
    return <>Stale · last success <time dateTime={new Date(query.dataUpdatedAt).toISOString()}>{new Date(query.dataUpdatedAt).toLocaleTimeString()}</time></>;
  }
  return query.dataUpdatedAt > 0
    ? <>Updated <time dateTime={new Date(query.dataUpdatedAt).toISOString()}>{new Date(query.dataUpdatedAt).toLocaleTimeString()}</time></>
    : 'Not loaded';
}

export function InfiniBandFleet() {
  return <FleetInfiniBandEvidence/>;
}

type FleetNode = Nodes['nodes'][number];
type OperationalCondition = NonNullable<FleetNode['operationalConditions']>[number];
type ConditionCategory = OperationalCondition['category'];
type EvidenceState = 'observed_ok' | 'fault' | 'unknown';

function gpuModelLabel(node: FleetNode) {
  const product = node.gpuProduct?.trim();
  if (product) {
    if (/^NVIDIA\b/i.test(product)) return product;
    const knownModel = product.toUpperCase().match(/\b(GB300|GB200|B200|H200|H100|A100|V100)\b/)?.[1];
    return knownModel ? `NVIDIA ${knownModel}` : product;
  }
  const sku = node.sku?.toUpperCase() || '';
  for (const model of ['GB300', 'GB200', 'B200', 'H200', 'H100', 'A100', 'V100']) {
    if (sku.includes(model)) return `NVIDIA ${model}`;
  }
  return 'GPU model Unknown';
}

interface EvidenceSummary {
  state: EvidenceState;
  observed: number;
  expected: number;
  lastObservedAt?: number;
  detail: string;
}

function EvidenceBadge({ state }: { state: EvidenceState }) {
  const label = state === 'observed_ok' ? 'Observed OK' : state === 'fault' ? 'Fault' : 'Unknown';
  const tone = state === 'observed_ok' ? 'done' : state === 'fault' ? 'fail' : 'queue';
  return <span className={'badge ' + tone}>{label}</span>;
}

function conditionSummary(
  conditions: OperationalCondition[] | undefined,
  category: ConditionCategory,
  rdmaAdvertised = true,
  now = Date.now(),
): EvidenceSummary {
  const requirements = category === 'gpu' ? gpuConditionRequirements : ibConditionRequirements;
  const relevant = (conditions || []).filter(condition => condition.category === category);
  const byType = new Map<string, OperationalCondition[]>();
  for (const condition of relevant) {
    const values = byType.get(condition.type) || [];
    values.push(condition);
    byType.set(condition.type, values);
  }
  const expectedTypes = new Set([...byType.keys(), ...requirements.map(requirement => requirement.type)]);
  const validByType = new Map<string, OperationalCondition>();
  let observed = 0;
  let lastObservedAt: number | undefined;
  const faults: string[] = [];
  const unknown: string[] = [];
  for (const [type, values] of byType) {
    if (values.length !== 1) {
      unknown.push(`${type} duplicated`);
      for (const condition of values) {
        const heartbeat = Date.parse(condition.lastHeartbeatTime || '');
        if (condition.status === 'True' && Number.isFinite(heartbeat) &&
          heartbeat <= now + futureClockSkewMs && now - heartbeat <= conditionFreshnessMs) {
          faults.push(type);
        }
      }
      continue;
    }
    const condition = values[0];
    const heartbeat = Date.parse(condition.lastHeartbeatTime || '');
    if (!Number.isFinite(heartbeat)) {
      unknown.push(`${type} heartbeat invalid`);
      continue;
    }
    if (heartbeat > now + futureClockSkewMs) {
      unknown.push(`${type} heartbeat is in the future`);
      continue;
    }
    if (now - heartbeat > conditionFreshnessMs) {
      unknown.push(`${type} heartbeat is stale`);
      continue;
    }
    observed++;
    validByType.set(type, condition);
    lastObservedAt = lastObservedAt === undefined ? heartbeat : Math.max(lastObservedAt, heartbeat);
    if (condition.status === 'True') faults.push(type);
    else if (condition.status !== 'False') unknown.push(`${type} is ${condition.status || 'Unknown'}`);
  }
  for (const requirement of requirements) {
    const condition = validByType.get(requirement.type);
    if (!condition) {
      unknown.push(`${requirement.type} missing`);
      continue;
    }
    if ('reason' in requirement && condition.status === 'False' && condition.reason !== requirement.reason) {
      unknown.push(`${requirement.type} metric coverage is unverified`);
    }
  }
  if (faults.length) {
    return {
      state: 'fault', observed, expected: expectedTypes.size, lastObservedAt,
      detail: `${new Set(faults).size} fresh fault condition${new Set(faults).size === 1 ? '' : 's'}: ${[...new Set(faults)].join(', ')}`,
    };
  }
  if (category === 'infiniband' && !rdmaAdvertised) {
    return {
      state: 'unknown', observed, expected: expectedTypes.size, lastObservedAt,
      detail: 'The node does not advertise an RDMA resource, so InfiniBand condition coverage is unverified',
    };
  }
  if (unknown.length) {
    return {
      state: 'unknown', observed, expected: expectedTypes.size, lastObservedAt,
      detail: unknown[0] + (unknown.length > 1 ? ` · ${unknown.length - 1} more coverage gaps` : ''),
    };
  }
  return {
    state: 'observed_ok', observed, expected: expectedTypes.size, lastObservedAt,
    detail: `All ${expectedTypes.size} enabled condition families reported fresh False`,
  };
}

function hasFreshNodeMetrics(node: FleetNode, now = Date.now()) {
  const observedAt = Date.parse(node.metricsObservedAt || '');
  return Number.isFinite(observedAt) &&
    observedAt <= now + futureClockSkewMs &&
    now - observedAt <= nodeMetricsFreshnessMs;
}

function telemetrySummary(node: FleetNode, nodeSamples: GPU[]): EvidenceSummary {
  const byGPU = new Map(nodeSamples.map(sample => [sample.gpu, sample]));
  const values = [...byGPU.values()];
  const faults = values.filter(sample => sample.healthy === false);
  const knownVerdicts = values.filter(sample => sample.healthy === true || sample.healthy === false);
  if (faults.length) {
    return {
      state: 'fault', observed: values.length, expected: node.gpuCapacity,
      detail: `${faults.length} GPU row-remap fault verdict${faults.length === 1 ? '' : 's'}`,
    };
  }
  if (nodeSamples.length !== byGPU.size || values.length !== node.gpuCapacity || knownVerdicts.length !== node.gpuCapacity) {
    return {
      state: 'unknown', observed: values.length, expected: node.gpuCapacity,
      detail: nodeSamples.length !== byGPU.size
        ? 'Duplicate GPU identities were returned in the ADX window'
        : `${knownVerdicts.length}/${node.gpuCapacity} GPUs have complete row-remap verdicts in the ADX window`,
    };
  }
  return {
    state: 'observed_ok', observed: values.length, expected: node.gpuCapacity,
    detail: `All ${node.gpuCapacity} inventory GPUs have observed row-remap verdicts`,
  };
}

function evidenceLabel(state: EvidenceState) {
  return state === 'observed_ok' ? 'Observed OK' : state === 'fault' ? 'Fault' : 'Unknown';
}

function sourceIdentityMatches(cluster: string | undefined, instance: string, inventoryCluster: string, inventoryNodes: Set<string>) {
  return inventoryCluster !== '' && cluster === inventoryCluster && inventoryNodes.has(instance);
}

function IndependentSourceEvidence({ gpuSamples, nodeUtil }: { gpuSamples: GPU[]; nodeUtil: NodeUtil['nodes'] }) {
  const [showAllGPUs, setShowAllGPUs] = useState(false);
  const [showAllNodeUtil, setShowAllNodeUtil] = useState(false);
  if (!gpuSamples.length && !nodeUtil.length) return null;
  const visibleGPUs = showAllGPUs ? gpuSamples : gpuSamples.slice(0, initialIndependentEvidenceLimit);
  const visibleNodeUtil = showAllNodeUtil ? nodeUtil : nodeUtil.slice(0, initialIndependentEvidenceLimit);
  return <section className="focused-gpus" aria-label="Independent source evidence">
    <div><h3>Independent source evidence</h3></div>
    <Note>These measurements are not attached to inventory nodes because exact cluster and instance identity is unavailable or does not match.</Note>
    {!!gpuSamples.length && <Table headers={['Cluster', 'Instance', 'GPU', 'Model', '#Util %', '#Temp °C', 'Health']}
      rows={visibleGPUs.map(gpu => [
        known(gpu.cluster), gpu.instance, gpu.gpu, known(gpu.modelName), n1(gpu.utilizationPct), n1(gpu.temperatureCelsius),
        <span className={gpu.healthy === false ? 'warn' : gpu.healthy === true ? '' : 'muted'}>{gpu.healthy === true ? 'Observed OK' : gpu.healthy === false ? 'Fault' : 'Unknown'}</span>,
      ])}/>}
    {gpuSamples.length > initialIndependentEvidenceLimit && <button type="button" className="btn disclosure-button"
      aria-expanded={showAllGPUs} onClick={() => setShowAllGPUs(value => !value)}>
      {showAllGPUs ? 'Show fewer GPU telemetry records' : `Show all ${gpuSamples.length} GPU telemetry records`}
    </button>}
    {!!nodeUtil.length && <Table headers={['Cluster', 'Instance', 'CPU cores', '#CPU utilization', '#Memory used', '#CPU coverage']}
      rows={visibleNodeUtil.map(node => [
        known(node.cluster), node.instance, node.cpuCores, measured(node.cpuUtilPct) ? `${n1(node.cpuUtilPct)}%` : 'Unknown',
        measured(node.memUsedPct) ? `${n1(node.memUsedPct)}%` : 'Unknown',
        `${n1(node.cpuCoverage.windowCoveragePct)}%`,
      ])}/>}
    {nodeUtil.length > initialIndependentEvidenceLimit && <button type="button" className="btn disclosure-button"
      aria-expanded={showAllNodeUtil} onClick={() => setShowAllNodeUtil(value => !value)}>
      {showAllNodeUtil ? 'Show fewer node utilization records' : `Show all ${nodeUtil.length} node utilization records`}
    </button>}
  </section>;
}

interface IndexedFleetNode {
  node: FleetNode;
  index: number;
}

interface FleetPoolGroup {
  pool: string;
  models: string[];
  nodes: IndexedFleetNode[];
}

interface FleetSiteGroup {
  site: string;
  gpuCount: number;
  rdmaGPUs: number;
  siteLabels: string[];
  regions: string[];
  nodeCount: number;
  pools: FleetPoolGroup[];
}

interface FleetGroupingOperations {
  nodeVisits: number;
  bucketAppends: number;
}

export function buildFleetGroups(
  nodes: FleetNode[],
  operations?: FleetGroupingOperations,
  groupByUnboundedSite = nodes.some(node => Boolean(node.site)),
): FleetSiteGroup[] {
  const sites = new Map<string, {
    gpuCount: number;
    rdmaGPUs: number;
    siteLabels: Set<string>;
    regions: Set<string>;
    nodeCount: number;
    pools: Map<string, { models: Set<string>; nodes: IndexedFleetNode[] }>;
  }>();
  nodes.forEach((node, index) => {
    if (operations) operations.nodeVisits++;
    const site = groupByUnboundedSite ? node.site || 'Unknown' : node.region || 'Region Unknown';
    let siteGroup = sites.get(site);
    if (!siteGroup) {
      siteGroup = {
        gpuCount: 0,
        rdmaGPUs: 0,
        siteLabels: new Set(),
        regions: new Set(),
        nodeCount: 0,
        pools: new Map(),
      };
      sites.set(site, siteGroup);
    }
    siteGroup.nodeCount++;
    siteGroup.gpuCount += node.gpuCapacity;
    if (node.rdmaResources?.length) siteGroup.rdmaGPUs += node.gpuCapacity;
    if (node.siteLabel) siteGroup.siteLabels.add(node.siteLabel);
    if (node.region) siteGroup.regions.add(node.region);
    if (operations) operations.bucketAppends++;

    const pool = node.agentPool || 'Pool Unknown';
    let poolGroup = siteGroup.pools.get(pool);
    if (!poolGroup) {
      poolGroup = { models: new Set(), nodes: [] };
      siteGroup.pools.set(pool, poolGroup);
    }
    poolGroup.models.add(gpuModelLabel(node));
    poolGroup.nodes.push({ node, index });
    if (operations) operations.bucketAppends++;
  });
  return [...sites.entries()]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([site, group]) => ({
      site,
      gpuCount: group.gpuCount,
      rdmaGPUs: group.rdmaGPUs,
      siteLabels: [...group.siteLabels].sort(),
      regions: [...group.regions].sort(),
      nodeCount: group.nodeCount,
      pools: [...group.pools.entries()]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([pool, poolGroup]) => ({
          pool,
          models: [...poolGroup.models].sort(),
          nodes: poolGroup.nodes.sort((left, right) => left.node.name.localeCompare(right.node.name)),
        })),
    }));
}

export function selectInitialFleetNodes(group: FleetSiteGroup, focusedInstance: string) {
  if (group.nodeCount <= initialFleetNodeLimit) return null;
  const visible: IndexedFleetNode[] = [];
  let poolIndex = 0;
  while (visible.length < initialFleetNodeLimit) {
    let added = false;
    for (const pool of group.pools) {
      const entry = pool.nodes[poolIndex];
      if (entry) {
        visible.push(entry);
        added = true;
        if (visible.length === initialFleetNodeLimit) break;
      }
    }
    if (!added) break;
    poolIndex++;
  }
  let focused: IndexedFleetNode | undefined;
  if (focusedInstance) {
    for (const pool of group.pools) {
      focused = pool.nodes.find(entry => entry.node.name === focusedInstance);
      if (focused) break;
    }
  }
  if (focused && !visible.includes(focused)) visible.push(focused);
  return visible;
}

function summarizeGPUMetrics(samples: GPU[]) {
  let utilizationTotal = 0;
  let utilizationCount = 0;
  let maxTemperature: number | null = null;
  let memoryUsedTotal = 0;
  let memoryTotal = 0;
  let memoryCount = 0;
  for (const sample of samples) {
    if (measured(sample.utilizationPct)) {
      utilizationTotal += sample.utilizationPct;
      utilizationCount++;
    }
    if (measured(sample.temperatureCelsius)) {
      maxTemperature = maxTemperature === null
        ? sample.temperatureCelsius
        : Math.max(maxTemperature, sample.temperatureCelsius);
    }
    if (measured(sample.memoryUsedMB) && measured(sample.memoryFreeMB)) {
      memoryUsedTotal += sample.memoryUsedMB;
      memoryTotal += sample.memoryUsedMB + sample.memoryFreeMB;
      memoryCount++;
    }
  }
  return {
    utilization: utilizationCount ? utilizationTotal / utilizationCount : null,
    utilizationCount,
    maxTemperature,
    memoryUsedTotal,
    memoryTotal,
    memoryCount,
  };
}

function FleetNodeCard({
  entry, gpuCondition, ibCondition, gpuTelemetry, samples, usage,
}: {
  entry: IndexedFleetNode;
  gpuCondition: EvidenceSummary;
  ibCondition: EvidenceSummary;
  gpuTelemetry: EvidenceSummary;
  samples: GPU[];
  usage: NodeUtil['nodes'][number] | undefined;
}) {
  const { node } = entry;
  const rdmaAdvertised = Boolean(node.rdmaResources?.length);
  const gpuMetrics = summarizeGPUMetrics(samples);
  const currentNodeMetrics = hasFreshNodeMetrics(node);
  const cpuUtilization = currentNodeMetrics && measured(node.cpuUtilPct) ? node.cpuUtilPct : usage?.cpuUtilPct;
  const memoryUtilization = currentNodeMetrics && measured(node.memUsedPct) ? node.memUsedPct : usage?.memUsedPct;
  const currentMetricsDetail = node.metricsWindow ? `Metrics API · ${node.metricsWindow} window` : 'Metrics API';
  const gpuMemoryDetail = gpuMetrics.memoryCount
    ? `GPU ${n1(gpuMetrics.memoryUsedTotal / 1024)} / ${n1(gpuMetrics.memoryTotal / 1024)} GiB`
    : '';
  return <article className="fabric-node">
    <div className="fabric-node-head"><strong>{node.name}</strong>
      <div className="fabric-node-badges">
        <span className={`fabric-capability ${!node.ready ? 'fault' : node.schedulable === false ? 'warning' : ''}`}>
          {!node.ready ? 'Not Ready' : node.schedulable === false ? 'Scheduling disabled' : 'Ready'}
        </span>
        <span className={`fabric-capability ${rdmaAdvertised ? 'rdma' : 'unknown'}`}>{rdmaAdvertised ? 'RDMA advertised' : 'No RDMA resource'}</span>
      </div>
    </div>
    <span>{node.gpuCapacity} × {gpuModelLabel(node)}{isCount(node.gpuAvailable) && isCount(node.gpuAllocated)
      ? ` · ${node.gpuAvailable} free · ${node.gpuAllocated} assigned`
      : ' · availability Unknown'}</span>
    <span>{node.cpuCores} CPU · {n1(node.memoryGiB)} GiB</span>
    <span>{node.region ? `Region ${node.region}` : 'Region Unknown'} · {node.zone ? `Zone ${node.zone}` : 'Zone Unknown'} · {node.agentPool ? `Pool ${node.agentPool}` : 'Pool Unknown'}</span>
    {node.siteLabelConflict && <span className="warn">Unbounded site label conflict · canonical value shown</span>}
    <div className="fabric-metrics">
      {gpuMetrics.utilization !== null && <span><small>GPU load</small><b>{n1(gpuMetrics.utilization)}%</b><i>{gpuMetrics.utilizationCount}/{node.gpuCapacity} observed</i></span>}
      {gpuMetrics.maxTemperature !== null && <span><small>GPU temp</small><b>{n1(gpuMetrics.maxTemperature)}°C</b><i>max observed</i></span>}
      <span><small>CPU</small><b>{measured(cpuUtilization) ? `${n1(cpuUtilization)}%` : 'Unknown'}</b><i>{currentNodeMetrics && measured(node.cpuUtilPct)
        ? currentMetricsDetail
        : usage?.cpuCoverage ? `ADX · ${n1(usage.cpuCoverage.windowCoveragePct)}% coverage` : 'no current sample'}</i></span>
      <span><small>Node memory</small><b>{measured(memoryUtilization) ? `${n1(memoryUtilization)}%` : 'Unknown'}</b><i>{[
        currentNodeMetrics && measured(node.memUsedPct) ? currentMetricsDetail : measured(usage?.memUsedPct) ? 'ADX fallback' : 'no current sample',
        gpuMemoryDetail,
      ].filter(Boolean).join(' · ')}</i></span>
    </div>
    <div className="fabric-signals">
      <span className={gpuCondition.state}>GPU/NVLink <b>{evidenceLabel(gpuCondition.state)}</b></span>
      <span className={ibCondition.state}>InfiniBand <b>{evidenceLabel(ibCondition.state)}</b></span>
      <span className={gpuTelemetry.state}>Telemetry <b>{evidenceLabel(gpuTelemetry.state)}</b></span>
    </div>
    <ScopedLink to={'/portal/fleet?instance=' + encodeURIComponent(node.name)}>GPU details →</ScopedLink>
  </article>;
}

function FleetSite({
  group, siteIndex, useUnboundedSites, focusedInstance, detailsVisible, onToggleDetails,
  gpuConditions, ibConditions, gpuTelemetry, gpuSamplesByNode, nodeUtilByNode,
}: {
  group: FleetSiteGroup;
  siteIndex: number;
  useUnboundedSites: boolean;
  focusedInstance: string;
  detailsVisible: boolean;
  onToggleDetails?: () => void;
  gpuConditions: EvidenceSummary[];
  ibConditions: EvidenceSummary[];
  gpuTelemetry: EvidenceSummary[];
  gpuSamplesByNode: Map<string, GPU[]>;
  nodeUtilByNode: Map<string, NodeUtil['nodes'][number]>;
}) {
  const [expanded, setExpanded] = useState(false);
  const visibleNodes = useMemo(() => {
    const visible = expanded ? null : selectInitialFleetNodes(group, focusedInstance);
    return visible ? new Set(visible) : null;
  }, [expanded, focusedInstance, group]);
  const groupLabel = useUnboundedSites ? `Unbounded site ${group.site}` : `Region ${group.site}`;
  return <section className={`fabric-site site-tone-${siteIndex % 4}`} aria-label={groupLabel}>
    <header><div><strong>{group.site}</strong>
      <span>{useUnboundedSites
        ? `${group.siteLabels.join(', ') || 'No Unbounded site label'} · ${group.regions.length ? `Region ${group.regions.join(', ')}` : 'Region Unknown'}`
        : 'Region placement · Unbounded site Unknown'} · {group.gpuCount} GPUs</span>
    </div>
      <span>{group.rdmaGPUs}/{group.gpuCount} GPUs on RDMA-advertised nodes</span>
    </header>
    {onToggleDetails && <button type="button" className="fabric-site-toggle"
      aria-expanded={detailsVisible} onClick={onToggleDetails}
      aria-label={detailsVisible
        ? `Hide ${group.nodeCount} node details in site ${group.site}`
        : `Show ${group.nodeCount} node details in site ${group.site}`}>
      {detailsVisible ? `Hide ${group.nodeCount} node details` : `Show ${group.nodeCount} node details`}
    </button>}
    {detailsVisible && <div className="fabric-pools">
      {group.pools.map(poolGroup => {
        const poolNodes = visibleNodes ? poolGroup.nodes.filter(entry => visibleNodes.has(entry)) : poolGroup.nodes;
        if (!poolNodes.length) return null;
        return <section className="fabric-pool" aria-label={`${poolGroup.models.join(', ')}, pool ${poolGroup.pool}`} key={poolGroup.pool}>
          <h4>{poolGroup.models.join(' / ')}<span>Pool {poolGroup.pool} · {poolGroup.nodes.length} node{poolGroup.nodes.length === 1 ? '' : 's'}</span></h4>
          <div className="fabric-nodes">{poolNodes.map(entry =>
            <FleetNodeCard key={entry.node.name} entry={entry}
              gpuCondition={gpuConditions[entry.index]} ibCondition={ibConditions[entry.index]}
              gpuTelemetry={gpuTelemetry[entry.index]} samples={gpuSamplesByNode.get(entry.node.name) || []}
              usage={nodeUtilByNode.get(entry.node.name)}/>,
          )}</div>
        </section>;
      })}
      {group.nodeCount > initialFleetNodeLimit && <button type="button" className="btn disclosure-button"
        aria-expanded={expanded} onClick={() => setExpanded(value => !value)}
        aria-label={expanded ? `Show fewer nodes in site ${group.site}` : `Show all ${group.nodeCount} nodes in site ${group.site}`}>
        {expanded ? 'Show fewer nodes' : `Show all ${group.nodeCount} nodes`}
      </button>}
    </div>}
  </section>;
}

const FleetFabricMap = memo(function FleetFabricMap({
  nodes, focusedInstance, gpuConditions, ibConditions, gpuTelemetry, gpuSamplesByNode, nodeUtilByNode,
}: {
  nodes: FleetNode[];
  focusedInstance: string;
  gpuConditions: EvidenceSummary[];
  ibConditions: EvidenceSummary[];
  gpuTelemetry: EvidenceSummary[];
  gpuSamplesByNode: Map<string, GPU[]>;
  nodeUtilByNode: Map<string, NodeUtil['nodes'][number]>;
}) {
  let labeledSiteNodes = 0;
  let conflictingSiteNodes = 0;
  for (const node of nodes) {
    if (node.site) labeledSiteNodes++;
    if (node.siteLabelConflict) conflictingSiteNodes++;
  }
  const useUnboundedSites = labeledSiteNodes > 0;
  const partialSiteCoverage = useUnboundedSites && labeledSiteNodes < nodes.length;
  const groups = useMemo(() => buildFleetGroups(nodes, undefined, useUnboundedSites), [nodes, useUnboundedSites]);
  const [expandedSites, setExpandedSites] = useState<Set<string>>(() => new Set());
  const toggleSite = (site: string) => setExpandedSites(current => {
    const next = new Set(current);
    if (next.has(site)) next.delete(site);
    else next.add(site);
    return next;
  });
  return <section className="fabric-map" aria-label={useUnboundedSites ? 'GPU InfiniBand fabric by Unbounded site' : 'GPU fleet by region and pool'}>
    <div className="fabric-map-head">
      <h3>GPU Dashboard</h3>
    </div>
    {!useUnboundedSites && nodes.length > 0 && <div className="fabric-no-link">
      <EvidenceBadge state="unknown"/>
      <span>Unbounded site visualization is unavailable; no GPU node has a supported site label.</span>
    </div>}
    {partialSiteCoverage && <div className="fabric-no-link" role="alert">
      <EvidenceBadge state="unknown"/>
      <span>Unbounded site coverage is partial: {labeledSiteNodes}/{nodes.length} GPU nodes are labeled. Unlabeled nodes remain in the Unknown bucket.</span>
    </div>}
    {conflictingSiteNodes > 0 && <div className="fabric-no-link" role="alert">
      <EvidenceBadge state="unknown"/>
      <span>{conflictingSiteNodes}/{nodes.length} GPU nodes have conflicting canonical and fallback site labels. Canonical values are shown; topology evidence remains conflicted.</span>
    </div>}
    <div className="fabric-sites">
      {groups.map((group, siteIndex) => {
        const hasFocusedNode = group.pools.some(pool =>
          pool.nodes.some(entry => entry.node.name === focusedInstance));
        const detailsVisible = groups.length <= initialDetailedSiteLimit ||
          siteIndex < initialDetailedSiteLimit || expandedSites.has(group.site) || hasFocusedNode;
        const onToggleDetails = groups.length > initialDetailedSiteLimit && !hasFocusedNode
          ? () => toggleSite(group.site)
          : undefined;
        return <FleetSite key={group.site} group={group} siteIndex={siteIndex} useUnboundedSites={useUnboundedSites}
          detailsVisible={detailsVisible} onToggleDetails={onToggleDetails}
          focusedInstance={focusedInstance} gpuConditions={gpuConditions} ibConditions={ibConditions}
          gpuTelemetry={gpuTelemetry} gpuSamplesByNode={gpuSamplesByNode} nodeUtilByNode={nodeUtilByNode}/>;
      })}
    </div>
  </section>;
});

function FleetInfiniBandEvidence() {
  const location = useLocation();
  const focusedInstance = new URLSearchParams(location.search).get('instance') || '';
  const inventoryQuery = useBoard<Nodes>('/api/portal/nodes');
  const telemetryQuery = useBoard<Cluster>('/api/portal/cluster');
  const nodeUtilQuery = useBoard<NodeUtil>('/api/portal/nodeutil');
  const sourceQueries = [inventoryQuery, telemetryQuery, nodeUtilQuery];
  const refreshAll = () => Promise.all(sourceQueries.map(query => query.refetch()));
  const snapshot = inventoryQuery.data;
  const nodes = useMemo(() => (snapshot?.nodes || []).filter(node => node.gpuCapacity > 0), [snapshot]);
  const gpuSchedulable = snapshot?.gpuSchedulable ?? snapshot?.gpuAllocatable ?? snapshot?.totalGPUs ?? 0;
  const gpuAllocationKnown = snapshot?.gpuAllocationKnown === true &&
    isCount(snapshot.gpuAllocated) && isCount(snapshot.gpuAvailable) && isCount(gpuSchedulable);
  const telemetry = telemetryQuery.data?.gpus || [];
  const nodeUtil = nodeUtilQuery.data?.nodes || [];
  const nodeNames = useMemo(() => new Set(nodes.map(node => node.name)), [nodes]);
  const inventoryCluster = snapshot?.scope?.cluster?.trim() || '';
  const canCorrelateInventory = Boolean(snapshot && inventoryCluster);
  const {
    attributedTelemetry, independentTelemetry, independentNodeUtil,
    gpuSamplesByNode, nodeUtilByNode,
  } = useMemo(() => {
    const matchedTelemetry: GPU[] = [];
    const unmatchedTelemetry: GPU[] = [];
    const matchedNodeUtil: NodeUtil['nodes'] = [];
    const unmatchedNodeUtil: NodeUtil['nodes'] = [];
    for (const sample of telemetry) {
      (canCorrelateInventory && sourceIdentityMatches(sample.cluster, sample.instance, inventoryCluster, nodeNames)
        ? matchedTelemetry : unmatchedTelemetry).push(sample);
    }
    for (const sample of nodeUtil) {
      (canCorrelateInventory && sourceIdentityMatches(sample.cluster, sample.instance, inventoryCluster, nodeNames)
        ? matchedNodeUtil : unmatchedNodeUtil).push(sample);
    }
    const samplesByNode = new Map<string, GPU[]>();
    for (const sample of matchedTelemetry) {
      const samples = samplesByNode.get(sample.instance);
      if (samples) samples.push(sample);
      else samplesByNode.set(sample.instance, [sample]);
    }
    return {
      attributedTelemetry: matchedTelemetry,
      independentTelemetry: canCorrelateInventory ? unmatchedTelemetry : telemetry,
      independentNodeUtil: canCorrelateInventory ? unmatchedNodeUtil : nodeUtil,
      gpuSamplesByNode: samplesByNode,
      nodeUtilByNode: new Map(matchedNodeUtil.map(sample => [sample.instance, sample])),
    };
  }, [telemetry, nodeUtil, canCorrelateInventory, inventoryCluster, nodeNames]);
  const summaryTelemetry = canCorrelateInventory ? attributedTelemetry : telemetry;
  const { utilization, knownHealth, healthFaults } = useMemo(() => ({
    utilization: utilizationSummary(summaryTelemetry),
    knownHealth: summaryTelemetry.filter(sample => sample.healthy === true || sample.healthy === false),
    healthFaults: summaryTelemetry.filter(sample => sample.healthy === false),
  }), [summaryTelemetry]);
  const focusedGPUs = canCorrelateInventory && focusedInstance ? gpuSamplesByNode.get(focusedInstance) || [] : [];
  const {
    gpuConditions, ibConditions, gpuTelemetry,
    gpuConditionCoveredGPUs, ibConditionCoveredGPUs,
  } = useMemo(() => {
    const gpuConditions: EvidenceSummary[] = [];
    const ibConditions: EvidenceSummary[] = [];
    const gpuTelemetry: EvidenceSummary[] = [];
    let gpuConditionCoveredGPUs = 0;
    let ibConditionCoveredGPUs = 0;
    const now = Date.now();
    for (const node of nodes) {
      const gpuCondition = conditionSummary(node.operationalConditions, 'gpu', true, now);
      const ibCondition = conditionSummary(
        node.operationalConditions, 'infiniband', Boolean(node.rdmaResources?.length), now,
      );
      gpuConditions.push(gpuCondition);
      ibConditions.push(ibCondition);
      gpuTelemetry.push(telemetrySummary(node, gpuSamplesByNode.get(node.name) || []));
      if (gpuCondition.state !== 'unknown') gpuConditionCoveredGPUs += node.gpuCapacity;
      if (ibCondition.state !== 'unknown') ibConditionCoveredGPUs += node.gpuCapacity;
    }
    return {
      gpuConditions,
      ibConditions,
      gpuTelemetry,
      gpuConditionCoveredGPUs,
      ibConditionCoveredGPUs,
    };
  }, [nodes, gpuSamplesByNode]);
  const sourceResults = [
    { name: 'inventory', query: inventoryQuery },
    { name: 'GPU telemetry', query: telemetryQuery },
    { name: 'node utilization', query: nodeUtilQuery },
  ];
  const [freshnessNow, setFreshnessNow] = useState(() => Date.now());
  useEffect(() => {
    const nextExpiry = Math.min(...sourceQueries
      .map(query => query.dataUpdatedAt > 0 ? query.dataUpdatedAt + boardStaleTimeMs : Number.POSITIVE_INFINITY)
      .filter(expiry => expiry > freshnessNow));
    if (!Number.isFinite(nextExpiry)) return;
    const timeout = window.setTimeout(
      () => setFreshnessNow(Math.max(Date.now(), nextExpiry)),
      Math.max(0, nextExpiry - Date.now() + 1),
    );
    return () => window.clearTimeout(timeout);
  }, [freshnessNow, inventoryQuery.dataUpdatedAt, telemetryQuery.dataUpdatedAt, nodeUtilQuery.dataUpdatedAt]);
  const unavailableSources = sourceResults.filter(source => source.query.isError);
  const hasData = sourceQueries.some(query => query.data !== undefined);
  const isFetching = sourceQueries.some(query => query.isFetching);
  const lastUpdatedAt = Math.max(0, ...sourceQueries.map(query => query.data === undefined ? 0 : query.dataUpdatedAt));
  const status = isFetching
    ? hasData ? 'Refreshing; showing available snapshots.' : 'Loading snapshot…'
    : unavailableSources.length
      ? hasData ? 'Partial snapshot; unavailable evidence stays Unknown.' : 'Unavailable.'
      : 'Snapshot; not live.';
  return <section className="data-panel" aria-label="GPU dashboard data">
    <div className="panel-status">
      <span role="status">GPU dashboard data: {status}{hasData && lastUpdatedAt > 0 && <>
        {' '}Last successful response <time dateTime={new Date(lastUpdatedAt).toISOString()}>{new Date(lastUpdatedAt).toLocaleString()}</time>.
      </>}</span>
      <button type="button" className="btn" aria-label={`${unavailableSources.length ? 'Retry' : 'Refresh'} GPU dashboard data`}
        disabled={isFetching} onClick={() => { void refreshAll(); }}>{isFetching ? 'Refreshing…' : unavailableSources.length ? 'Retry' : 'Refresh'}</button>
    </div>
    <div aria-busy={isFetching}>
      {!!unavailableSources.length && <Note warn>Unavailable: {unavailableSources.map(source =>
        `${source.name}: ${source.query.error?.message || 'request failed'}`).join('; ')}. Available sources remain visible and missing evidence stays Unknown.</Note>}
      {hasData && <>
        {snapshot?.nodeMetricsError && <Note warn>Current Node metrics are incomplete: {snapshot.nodeMetricsError}. Inventory remains visible and exact ADX matches are used as fallback.</Note>}
        <dl className="evidence-strip" aria-label="Fleet operational summary">
          <div><dt>Node health</dt><dd>{snapshot ? `${snapshot.readyNodes}/${snapshot.totalNodes} nodes ready` : 'Unknown'}</dd><span>{snapshot
            ? `${snapshot.totalCPUCores} CPU · ${n1(snapshot.totalMemoryGiB)} GiB`
            : `${nodeUtilQuery.data?.nodes?.length || 0} node utilization records`}</span></div>
          <div><dt>GPU availability</dt><dd>{gpuAllocationKnown ? `${snapshot.gpuAvailable} free` : 'Unknown'}</dd><span>{snapshot
            ? gpuAllocationKnown
              ? `${snapshot.gpuAllocated} assigned · ${gpuSchedulable} schedulable`
              : `${gpuSchedulable} schedulable · active assignments unavailable`
            : `${telemetry.length} GPU telemetry records`}</span></div>
          <div><dt>GPU utilization</dt><dd>{utilization.average === null ? 'Unknown' : `${n1(utilization.average)}% avg`}</dd><span>{utilization.observed}/{canCorrelateInventory ? snapshot?.totalGPUs : telemetry.length} {canCorrelateInventory ? 'inventory GPUs' : 'telemetry records'} observed</span></div>
          <div><dt>GPU health telemetry</dt><dd>{healthFaults.length ? `${healthFaults.length} fault` : knownHealth.length ? 'No observed faults' : 'Unknown'}</dd><span>{knownHealth.length}/{canCorrelateInventory ? snapshot?.totalGPUs : telemetry.length} {canCorrelateInventory ? 'inventory GPUs' : 'telemetry records'} observed</span></div>
          <div><dt>InfiniBand</dt><dd>{snapshot ? `${snapshot.rdmaAdvertisedGpuNodes ?? 'Unknown'}/${snapshot.gpuNodes} RDMA nodes` : 'Unknown'}</dd><span>{snapshot
            ? `GPU/NVLink ${gpuConditionCoveredGPUs}/${snapshot.totalGPUs} · IB ${ibConditionCoveredGPUs}/${snapshot.totalGPUs} GPUs covered`
            : 'Inventory-dependent capability and coverage'}</span></div>
        </dl>
        <div className="source-freshness" aria-label="Fleet data source freshness">
          <span><strong>Inventory</strong> {sourceFreshness(inventoryQuery, freshnessNow)}</span>
          <span><strong>GPU telemetry</strong> {sourceFreshness(telemetryQuery, freshnessNow)}</span>
          <span><strong>Node utilization</strong> {sourceFreshness(nodeUtilQuery, freshnessNow)}</span>
        </div>
        {!snapshot ? <Empty warn>GPU inventory is unavailable. Telemetry remains visible; fleet denominators, RDMA scheduling capability, and Unbounded site boundaries are Unknown.</Empty>
          : !nodes.length ? <Empty>No GPU or RDMA-capable nodes were reported by the authorized fleet inventory.</Empty> : null}
        {focusedInstance && <section className="focused-gpus" aria-label={`GPU details for ${focusedInstance}`}>
          <div><h3>GPU details · {focusedInstance}</h3><ScopedLink to="/portal/fleet">Clear focus</ScopedLink></div>
          {!focusedGPUs.length ? <Empty>No per-GPU telemetry is available for this node in the current window.</Empty>
            : <Table headers={['GPU', 'Model', '#Util %', '#Temp °C', '#Power W', '#Memory MB', '#Uncorrectable rows', 'Health']}
              rows={focusedGPUs.map(gpu => [
                gpu.gpu, known(gpu.modelName), n1(gpu.utilizationPct), n1(gpu.temperatureCelsius), n1(gpu.powerWatts),
                `${n1(gpu.memoryUsedMB)} / ${measured(gpu.memoryUsedMB) && measured(gpu.memoryFreeMB) ? n1(gpu.memoryUsedMB + gpu.memoryFreeMB) : '—'}`,
                n1(gpu.uncorrectableRemappedRows),
                <span className={gpu.healthy === false ? 'warn' : gpu.healthy === true ? '' : 'muted'}>{gpu.healthy === true ? 'Observed OK' : gpu.healthy === false ? 'Fault' : 'Unknown'}</span>,
              ])}/>}
        </section>}
        {snapshot && nodes.length > 0 && <FleetFabricMap nodes={nodes} focusedInstance={focusedInstance}
          gpuConditions={gpuConditions} ibConditions={ibConditions}
          gpuTelemetry={gpuTelemetry} gpuSamplesByNode={gpuSamplesByNode} nodeUtilByNode={nodeUtilByNode}/>}
        <IndependentSourceEvidence gpuSamples={independentTelemetry} nodeUtil={independentNodeUtil}/>
      </>}
    </div>
  </section>;
}
