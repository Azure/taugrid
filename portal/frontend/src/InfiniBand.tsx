// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { ReactNode } from 'react';
import { useLocation, useParams } from 'react-router-dom';
import { useBoard } from './data';
import { BoardResult, Empty, KV, Note, PageTitle, ScopedLink, Table, measured, n1, utilizationSummary } from './components';
import type {
  Cluster, GPU, RDMAFreshness, RDMAHistoricalStatus, RDMANode, RDMARdmaDevice, RDMAValidation,
  RDMAValidationDetail, RDMAValidationState, RDMAValidationSummary, Nodes, NodeUtil,
} from './types';

const conditionFreshnessMs = 15 * 60 * 1000;
const futureClockSkewMs = 60 * 1000;
const coverageStatement = 'Point-in-time two-GPU inter-node RDMA validation; this is not continuous InfiniBand or fleet health.';
const gpuConditionTypes = [
  'GPUECCDoubleRetired', 'GPUECCDoubleVolatile', 'GPUNVLinkCRCFlitErrors',
  'GPUNVLinkCRCDataErrors', 'GPUNVLinkReplayErrors', 'GPUThermalViolation',
  'GPUPowerViolation', 'GPUECCSingleVolatileRate', 'GPUECCSingleRetired',
  'GPUPCIeReplayErrors',
] as const;
const ibConditionTypes = ['IBLinkDown', 'IBSymbolError'] as const;

const known = (value: ReactNode) => value === undefined || value === null || value === '' ? 'Unknown' : value;
const list = (value?: string[]) => value?.length ? value.join(', ') : 'Unknown';
const yesNoUnknown = (value?: boolean | null) => value === true ? 'Yes' : value === false ? 'No' : 'Unknown';
const timestamp = (value?: string) => value ? <time dateTime={value}>{new Date(value).toLocaleString()}</time> : 'Unknown';
const bytes = (value?: number | null) => value === undefined || value === null ? 'Unknown' : value.toLocaleString();
const seconds = (value?: number | null) => value === undefined || value === null ? 'Unknown' : `${n1(value)}s`;
const bandwidth = (value?: number | null) => value === undefined || value === null ? 'Unknown' : `${n1(value)} GB/s`;
const historicalLabel = (value: RDMAHistoricalStatus) => value === 'pass' ? 'Passed' : value === 'fail' ? 'Failed' : 'Unknown';
const freshnessLabel = (value: RDMAFreshness) => ({
  fresh: 'Fresh', stale: 'Stale', unknown: 'Unknown', not_applicable: 'Not applicable',
})[value];
const stateLabel = (value: RDMAValidationState) => ({
  passed: 'Passed', failed: 'Failed', running: 'Running', unknown: 'Unknown', stale: 'Stale',
})[value];
const stateTone = (value: RDMAValidationState) =>
  value === 'passed' ? 'done' : value === 'failed' ? 'fail' : value === 'running' || value === 'stale' ? 'queue' : '';

export function ValidationState({ state }: { state: RDMAValidationState }) {
  return <span className={'badge ' + stateTone(state)}>{stateLabel(state)}</span>;
}

function stateMeaning(validation: RDMAValidation) {
  switch (validation.state) {
    case 'passed': return 'Verified evidence passed for this validation run only.';
    case 'failed': return 'This validation run failed; review transport, correctness, exits, and cleanup.';
    case 'running': return 'This validation is still running; no final pass or fail result is available.';
    case 'stale': return `The latest validation is stale. Its immutable historical result was ${historicalLabel(validation.historicalStatus)}.`;
    default: return 'The result is absent, incomplete, malformed, unsupported, or unverified; do not infer success.';
  }
}

function age(value?: number | null) {
  if (value === undefined || value === null || value < 0) return 'Unknown';
  if (value < 60) return `${Math.floor(value)}s`;
  if (value < 3600) return `${Math.floor(value / 60)}m`;
  if (value < 86400) return `${Math.floor(value / 3600)}h ${Math.floor(value % 3600 / 60)}m`;
  return `${Math.floor(value / 86400)}d ${Math.floor(value % 86400 / 3600)}h`;
}

function actualGPUs(validation: RDMAValidation) {
  const nodes = validation.actual?.nodes;
  if (!nodes?.length || !nodes.some(node => node.gpuUuids?.length)) return undefined;
  return nodes.reduce((total, node) => total + (node.gpuUuids?.length || 0), 0);
}

function topologySite(site?: string, provider?: string, mode?: string) {
  if (mode === 'not_applicable') return 'Unbounded site not applicable';
  if (mode === 'incomplete') return site ? `Unbounded site incomplete · ${site}` : 'Unbounded site incomplete';
  if (mode === 'complete') return `${provider === 'unbounded' ? 'Unbounded site' : 'Site'} ${site || 'Unknown'}`;
  return site || 'Unknown';
}

function validationSite(validation?: RDMAValidation | null) {
  const actual = validation?.actual;
  if (actual?.siteMode && actual.siteMode !== 'complete') return undefined;
  return actual?.site;
}

function sourceFreshness(query: { dataUpdatedAt: number; isError: boolean; isFetching: boolean }) {
  if (query.isFetching) return 'Refreshing';
  if (query.isError) return query.dataUpdatedAt > 0
    ? <>Stale · last success <time dateTime={new Date(query.dataUpdatedAt).toISOString()}>{new Date(query.dataUpdatedAt).toLocaleTimeString()}</time></>
    : 'Unavailable';
  return query.dataUpdatedAt > 0
    ? <>Updated <time dateTime={new Date(query.dataUpdatedAt).toISOString()}>{new Date(query.dataUpdatedAt).toLocaleTimeString()}</time></>
    : 'Not loaded';
}

function requestedCoverage(validation: RDMAValidation) {
  const requested = validation.requested;
  if (!requested) return 'Unknown';
  const gpus = requested.nodeCount !== undefined && requested.nodeCount !== null &&
    requested.ranksPerNode !== undefined && requested.ranksPerNode !== null &&
    requested.gpusPerRank !== undefined && requested.gpusPerRank !== null
    ? requested.nodeCount * requested.ranksPerNode * requested.gpusPerRank : undefined;
  return [
    requested.nodeCount === undefined || requested.nodeCount === null ? undefined : `${requested.nodeCount} nodes`,
    gpus === undefined ? undefined : `${gpus} GPUs`,
    topologySite(requested.site, requested.siteProvider, requested.siteMode),
    requested.region ? `Region ${requested.region}` : undefined,
    requested.pool, requested.gpuModel,
  ].filter(Boolean).join(' · ') || 'Unknown';
}

function actualCoverage(validation: RDMAValidation) {
  const nodes = validation.actual?.nodes?.length;
  const gpus = actualGPUs(validation);
  return [
    nodes === undefined ? undefined : `${nodes} nodes`,
    gpus === undefined ? undefined : `${gpus} GPUs`,
    topologySite(validation.actual?.site, validation.actual?.siteProvider, validation.actual?.siteMode),
    validation.actual?.region ? `Region ${validation.actual.region}` : undefined,
    validation.actual?.pool,
    validation.placement?.matchesRequest === true ? 'matches request' :
      validation.placement?.matchesRequest === false ? 'does not match request' : undefined,
  ].filter(Boolean).join(' · ') || 'Unknown';
}

export function InfiniBandFleet() {
  return <FleetInfiniBandEvidence/>;
}

type FleetNode = Nodes['nodes'][number];
type OperationalCondition = NonNullable<FleetNode['operationalConditions']>[number];
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
  expectedTypes: readonly string[],
  now = Date.now(),
): EvidenceSummary {
  const byType = new Map<string, OperationalCondition[]>();
  for (const condition of conditions || []) {
    const values = byType.get(condition.type) || [];
    values.push(condition);
    byType.set(condition.type, values);
  }
  let observed = 0;
  let lastObservedAt: number | undefined;
  const faults: string[] = [];
  const unknown: string[] = [];
  for (const type of expectedTypes) {
    const values = byType.get(type) || [];
    if (values.length !== 1) {
      unknown.push(values.length ? `${type} duplicated` : `${type} missing`);
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
    lastObservedAt = lastObservedAt === undefined ? heartbeat : Math.max(lastObservedAt, heartbeat);
    if (condition.status === 'True') faults.push(type);
    else if (condition.status !== 'False') unknown.push(`${type} is ${condition.status || 'Unknown'}`);
  }
  if (faults.length) {
    return {
      state: 'fault', observed, expected: expectedTypes.length, lastObservedAt,
      detail: `${faults.length} fresh fault condition${faults.length === 1 ? '' : 's'}: ${faults.join(', ')}`,
    };
  }
  if (unknown.length) {
    return {
      state: 'unknown', observed, expected: expectedTypes.length, lastObservedAt,
      detail: unknown[0] + (unknown.length > 1 ? ` · ${unknown.length - 1} more coverage gaps` : ''),
    };
  }
  return {
    state: 'observed_ok', observed, expected: expectedTypes.length, lastObservedAt,
    detail: `All ${expectedTypes.length} required condition families reported fresh False`,
  };
}

function telemetrySummary(node: FleetNode, samples: GPU[]): EvidenceSummary {
  const nodeSamples = samples.filter(sample => sample.instance === node.name);
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

function average(values: (number | null)[]) {
  const observed = values.filter(measured);
  return observed.length ? observed.reduce((sum, value) => sum + value, 0) / observed.length : null;
}

function FleetFabricMap({
  nodes, latest, gpuConditions, ibConditions, gpuTelemetry, gpuSamples, nodeUtil,
}: {
  nodes: FleetNode[];
  latest?: RDMAValidation;
  gpuConditions: EvidenceSummary[];
  ibConditions: EvidenceSummary[];
  gpuTelemetry: EvidenceSummary[];
  gpuSamples: GPU[];
  nodeUtil: NodeUtil['nodes'];
}) {
  const testedByName = new Map((latest?.actual?.nodes || []).map(node => [node.name, node]));
  const labeledSiteNodes = nodes.filter(node => node.site).length;
  const conflictingSiteNodes = nodes.filter(node => node.siteLabelConflict).length;
  const useUnboundedSites = labeledSiteNodes > 0;
  const partialSiteCoverage = useUnboundedSites && labeledSiteNodes < nodes.length;
  const sites = new Map<string, { node: FleetNode; index: number }[]>();
  nodes.forEach((node, index) => {
    const site = useUnboundedSites
      ? node.site || 'Unknown'
      : node.region || 'Region Unknown';
    sites.set(site, [...(sites.get(site) || []), { node, index }]);
  });
  const validationSiteName = validationSite(latest);
  const connectionNodes = latest?.actual?.nodes || [];
  const connectionGPUs = connectionNodes.flatMap(node =>
    (node.gpuUuids || []).map(uuid => `${node.name} / ${uuid}`));

  return <section className="fabric-map" aria-label={useUnboundedSites ? 'GPU InfiniBand fabric by Unbounded site' : 'GPU fleet by region and pool'}>
    <div className="fabric-map-head">
      <h3>GPU Dashboard</h3>
      <div className="fabric-legend" aria-label="Fabric map legend">
        <span><i className="legend-line passed"/>Passed run path</span>
        <span><i className="legend-line failed"/>Failed or fault</span>
        <span><i className="legend-line unknown"/>Unknown / unverified</span>
      </div>
    </div>
    {!latest && <div className="fabric-no-link"><EvidenceBadge state="unknown"/>
      <span>No validated GPU-to-GPU InfiniBand path is available in this scope.</span>
    </div>}
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
      {[...sites.entries()].sort(([left], [right]) => left.localeCompare(right)).map(([site, siteNodes], siteIndex) => {
        const pools = new Map<string, { node: FleetNode; index: number }[]>();
        siteNodes.forEach(entry => {
          const pool = entry.node.agentPool || 'Pool Unknown';
          pools.set(pool, [...(pools.get(pool) || []), entry]);
        });
        const siteGPUCount = siteNodes.reduce((total, entry) => total + entry.node.gpuCapacity, 0);
        const siteRDMAGPUs = siteNodes.reduce((total, entry) =>
          total + ((entry.node.rdmaResources || []).length ? entry.node.gpuCapacity : 0), 0);
        const siteLabels = [...new Set(siteNodes.flatMap(entry => entry.node.siteLabel ? [entry.node.siteLabel] : []))].sort();
        const regions = [...new Set(siteNodes.flatMap(entry => entry.node.region ? [entry.node.region] : []))].sort();
        const showConnection = useUnboundedSites && validationSiteName === site && connectionNodes.length > 0;
        const groupLabel = useUnboundedSites ? `Unbounded site ${site}` : `Region ${site}`;
        return <section className={`fabric-site site-tone-${siteIndex % 4}`} aria-label={groupLabel} key={site}>
          <header><div><strong>{site}</strong>
            <span>{useUnboundedSites
              ? `${siteLabels.join(', ') || 'No Unbounded site label'} · ${regions.length ? `Region ${regions.join(', ')}` : 'Region Unknown'}`
              : 'Region placement · Unbounded site Unknown'} · {siteGPUCount} GPUs</span>
          </div>
            <span>{siteRDMAGPUs}/{siteGPUCount} GPUs on RDMA-advertised nodes</span>
          </header>
          {showConnection && <div className={`fabric-connection ${latest?.state || 'unknown'}`}>
            <span className="connection-rail" aria-hidden="true"><i/><i/></span>
            <div><strong>{stateLabel(latest!.state)} · {connectionGPUs.length || 'Unknown'}-GPU run path</strong>
              <span>{connectionGPUs.length ? connectionGPUs.join(' ↔ ') : connectionNodes.map(node => node.name).join(' ↔ ')}</span>
              <small>This path is run evidence only; other GPUs in the site are not implied validated.</small>
            </div>
          </div>}
          <div className="fabric-pools">
            {[...pools.entries()].sort(([left], [right]) => left.localeCompare(right)).map(([pool, poolNodes]) => {
              const models = [...new Set(poolNodes.map(entry => gpuModelLabel(entry.node)))].sort();
              return <section className="fabric-pool" aria-label={`${models.join(', ')}, pool ${pool}`} key={pool}>
                <h4>{models.join(' / ')}<span>Pool {pool} · {poolNodes.length} node{poolNodes.length === 1 ? '' : 's'}</span></h4>
                <div className="fabric-nodes">{poolNodes.sort((left, right) => left.node.name.localeCompare(right.node.name)).map(({ node, index }) => {
                  const rdmaAdvertised = Boolean(node.rdmaResources?.length);
                  const tested = testedByName.get(node.name);
                  const samples = gpuSamples.filter(sample => sample.instance === node.name);
                  const gpuUtilization = average(samples.map(sample => sample.utilizationPct));
                  const gpuTemperature = samples.map(sample => sample.temperatureCelsius).filter(measured);
                  const gpuMemoryUsed = samples.map(sample => sample.memoryUsedMB).filter(measured);
                  const gpuMemoryTotal = samples.flatMap(sample =>
                    measured(sample.memoryUsedMB) && measured(sample.memoryFreeMB)
                      ? [sample.memoryUsedMB + sample.memoryFreeMB] : []);
                  const usage = nodeUtil.find(sample => sample.instance === node.name);
                  return <article className={`fabric-node${tested ? ` tested ${latest?.state || 'unknown'}` : ''}`} key={node.name}>
                    <div className="fabric-node-head"><strong>{node.name}</strong>
                      <span className={`fabric-capability ${rdmaAdvertised ? 'rdma' : 'unknown'}`}>{rdmaAdvertised ? 'RDMA advertised' : 'No RDMA resource'}</span>
                    </div>
                    <span>{node.gpuCapacity} × {gpuModelLabel(node)} · {node.cpuCores} CPU · {n1(node.memoryGiB)} GiB</span>
                    <span>{node.region ? `Region ${node.region}` : 'Region Unknown'} · {node.zone ? `Zone ${node.zone}` : 'Zone Unknown'} · {node.agentPool ? `Pool ${node.agentPool}` : 'Pool Unknown'}</span>
                    {node.siteLabelConflict && <span className="warn">Unbounded site label conflict · canonical value shown</span>}
                    <div className="fabric-metrics">
                      <span><small>GPU load</small><b>{gpuUtilization === null ? 'Unknown' : `${n1(gpuUtilization)}%`}</b><i>{samples.filter(sample => measured(sample.utilizationPct)).length}/{node.gpuCapacity} observed</i></span>
                      <span><small>GPU temp</small><b>{gpuTemperature.length ? `${n1(Math.max(...gpuTemperature))}°C` : 'Unknown'}</b><i>{gpuTemperature.length ? 'max observed' : 'no samples'}</i></span>
                      <span><small>CPU</small><b>{measured(usage?.cpuUtilPct) ? `${n1(usage.cpuUtilPct)}%` : 'Unknown'}</b><i>{usage?.cpuCoverage ? `${n1(usage.cpuCoverage.windowCoveragePct)}% coverage` : 'no coverage'}</i></span>
                      <span><small>Node memory</small><b>{measured(usage?.memUsedPct) ? `${n1(usage.memUsedPct)}%` : 'Unknown'}</b><i>{gpuMemoryUsed.length && gpuMemoryTotal.length ? `GPU ${n1(gpuMemoryUsed.reduce((sum, value) => sum + value, 0) / 1024)} / ${n1(gpuMemoryTotal.reduce((sum, value) => sum + value, 0) / 1024)} GiB` : 'GPU memory Unknown'}</i></span>
                    </div>
                    {tested && <div className={`tested-gpus ${latest?.state || 'unknown'}`}>
                      <strong>{tested.gpuUuids?.length || 'Unknown'} GPU sampled in latest run</strong>
                      <span>{list(tested.gpuUuids)}</span>
                    </div>}
                    <div className="fabric-signals">
                      <span className={gpuConditions[index].state}>GPU/NVLink <b>{evidenceLabel(gpuConditions[index].state)}</b></span>
                      <span className={ibConditions[index].state}>InfiniBand <b>{evidenceLabel(ibConditions[index].state)}</b></span>
                      <span className={gpuTelemetry[index].state}>Telemetry <b>{evidenceLabel(gpuTelemetry[index].state)}</b></span>
                    </div>
                    <ScopedLink to={'/portal/fleet?instance=' + encodeURIComponent(node.name)}>GPU details →</ScopedLink>
                  </article>;
                })}</div>
              </section>;
            })}
          </div>
        </section>;
      })}
    </div>
  </section>;
}

function FleetInfiniBandEvidence() {
  const location = useLocation();
  const focusedInstance = new URLSearchParams(location.search).get('instance') || '';
  const inventoryQuery = useBoard<Nodes>('/api/portal/nodes');
  const telemetryQuery = useBoard<Cluster>('/api/portal/cluster');
  const nodeUtilQuery = useBoard<NodeUtil>('/api/portal/nodeutil');
  const latestQuery = useBoard<RDMAValidationSummary>('/api/portal/rdma-validations/summary');
  const sourceQueries = [inventoryQuery, telemetryQuery, nodeUtilQuery, latestQuery];
  const refreshAll = () => Promise.all(sourceQueries.map(query => query.refetch()));
  const snapshot = inventoryQuery.data;
  const latest = latestQuery.data?.latest;
  const nodes = (snapshot?.nodes || []).filter(node => node.gpuCapacity > 0);
  const telemetry = telemetryQuery.data?.gpus || [];
  const nodeNames = new Set(nodes.map(node => node.name));
  const matchedTelemetry = snapshot ? telemetry.filter(sample => nodeNames.has(sample.instance)) : telemetry;
  const utilization = utilizationSummary(matchedTelemetry);
  const knownHealth = matchedTelemetry.filter(sample => sample.healthy === true || sample.healthy === false);
  const healthFaults = matchedTelemetry.filter(sample => sample.healthy === false);
  const focusedGPUs = focusedInstance ? telemetry.filter(sample => sample.instance === focusedInstance) : [];
  const gpuConditions = nodes.map(node => conditionSummary(node.operationalConditions, gpuConditionTypes));
  const ibConditions = nodes.map(node => conditionSummary(node.operationalConditions, ibConditionTypes));
  const gpuTelemetry = nodes.map(node => telemetrySummary(node, telemetry));
  const gpuConditionCoveredGPUs = nodes.reduce((total, node, index) =>
    total + (gpuConditions[index].state === 'unknown' ? 0 : node.gpuCapacity), 0);
  const ibConditionCoveredGPUs = nodes.reduce((total, node, index) =>
    total + (ibConditions[index].state === 'unknown' ? 0 : node.gpuCapacity), 0);
  const sourceResults = [
    { name: 'inventory', query: inventoryQuery },
    { name: 'GPU telemetry', query: telemetryQuery },
    { name: 'node utilization', query: nodeUtilQuery },
    { name: 'validation', query: latestQuery },
  ];
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
        <dl className="evidence-strip" aria-label="Fleet operational summary">
          <div><dt>Fleet capacity</dt><dd>{snapshot ? `${snapshot.readyNodes}/${snapshot.totalNodes} nodes ready` : 'Unknown'}</dd><span>{snapshot
            ? `${snapshot.totalCPUCores} CPU · ${n1(snapshot.totalMemoryGiB)} GiB`
            : `${nodeUtilQuery.data?.nodes?.length || 0} node utilization records`}</span></div>
          <div><dt>GPU inventory</dt><dd>{snapshot ? `${snapshot.totalGPUs} GPUs` : 'Unknown'}</dd><span>{snapshot
            ? `${nodes.length} GPU nodes · ${snapshot.skus?.length || 0} SKUs`
            : `${telemetry.length} GPU telemetry records`}</span></div>
          <div><dt>GPU utilization</dt><dd>{utilization.average === null ? 'Unknown' : `${n1(utilization.average)}% avg`}</dd><span>{utilization.observed}/{snapshot?.totalGPUs ?? telemetry.length} {snapshot ? 'inventory GPUs' : 'telemetry records'} observed</span></div>
          <div><dt>GPU health telemetry</dt><dd>{healthFaults.length ? `${healthFaults.length} fault` : knownHealth.length ? 'No observed faults' : 'Unknown'}</dd><span>{knownHealth.length}/{snapshot?.totalGPUs ?? telemetry.length} {snapshot ? 'inventory GPUs' : 'telemetry records'} observed</span></div>
          <div><dt>InfiniBand</dt><dd>{snapshot ? `${snapshot.rdmaAdvertisedGpuNodes ?? 'Unknown'}/${snapshot.gpuNodes} RDMA nodes` : 'Unknown'}</dd><span>{snapshot
            ? `GPU/NVLink ${gpuConditionCoveredGPUs}/${snapshot.totalGPUs} · IB ${ibConditionCoveredGPUs}/${snapshot.totalGPUs} GPUs covered`
            : 'Inventory-dependent capability and coverage'}</span></div>
          <div><dt>Latest run</dt><dd>{latest ? stateLabel(latest.state) : 'Unknown'}</dd><span>{latest
            ? `${topologySite(latest.actual?.site, latest.actual?.siteProvider, latest.actual?.siteMode)}${latest.actual?.pool ? ` / ${latest.actual.pool}` : ''}`
            : 'Unknown'}</span></div>
        </dl>
        <div className="source-freshness" aria-label="Fleet data source freshness">
          <span><strong>Inventory</strong> {sourceFreshness(inventoryQuery)}</span>
          <span><strong>GPU telemetry</strong> {sourceFreshness(telemetryQuery)}</span>
          <span><strong>Node utilization</strong> {sourceFreshness(nodeUtilQuery)}</span>
          <span><strong>Validation</strong> {sourceFreshness(latestQuery)}</span>
        </div>
        {!snapshot ? <Empty warn>GPU inventory is unavailable. Telemetry and validation remain visible; fleet denominators, RDMA scheduling capability, and Unbounded site boundaries are Unknown.</Empty>
          : !nodes.length ? <Empty>No GPU or RDMA-capable nodes were reported by the authorized fleet inventory.</Empty>
          : <><FleetFabricMap nodes={nodes} latest={latest || undefined} gpuConditions={gpuConditions} ibConditions={ibConditions}
            gpuTelemetry={gpuTelemetry} gpuSamples={telemetry} nodeUtil={nodeUtilQuery.data?.nodes || []}/>
            </>}
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
      </>}
    </div>
  </section>;
}

export function InfiniBandValidationDetail() {
  const { validationId = '' } = useParams();
  const query = useBoard<RDMAValidationDetail>('/api/portal/rdma-validations/' + encodeURIComponent(validationId), !!validationId);
  return <><div className="page-head"><div><PageTitle title="InfiniBand validation">{coverageStatement}</PageTitle></div>
    <ScopedLink to="/portal/fleet" className="back">← Back to Fleet</ScopedLink></div>
    {!validationId ? <Empty warn>Invalid validation path: a validation ID is required.</Empty>
      : <BoardResult query={query} label="InfiniBand validation detail">{validation => <ValidationDetail validation={validation}/>}</BoardResult>}
  </>;
}

function nodeDevices(nodes?: RDMANode[]) {
  return (nodes || []).flatMap(node => (node.rdmaDevices || []).map(device => [node, device] as [RDMANode, RDMARdmaDevice]));
}

function ValidationDetail({ validation }: { validation: RDMAValidationDetail }) {
  return <><div className="detail-meta"><ValidationState state={validation.state}/>
    <span>Freshness: {freshnessLabel(validation.freshness)}</span>
    <span>Historical result: {historicalLabel(validation.historicalStatus)}</span></div>
    <Note warn={validation.state !== 'passed'}>{stateMeaning(validation)}</Note>

    <h2>Identity and freshness</h2><KV rows={[
      ['Validation ID', validation.validationId], ['Run ID', known(validation.runId)], ['Run attempt', known(validation.runAttempt)],
      ['Schema', known(validation.schemaVersion)], ['Kind', known(validation.kind)], ['Reason code', known(validation.reasonCode)],
      ['Reason', known(validation.reason)], ['Cluster', known(validation.cluster)], ['Workspace', known(validation.workspaceId)],
      ['Namespace', known(validation.namespace)], ['Created', timestamp(validation.createdAt)], ['Started', timestamp(validation.startedAt)],
      ['Admitted', timestamp(validation.admittedAt)], ['Completed', timestamp(validation.completedAt)], ['Observed', timestamp(validation.observedAt)],
      ['Valid until', timestamp(validation.validUntil)], ['Age', age(validation.ageSeconds)], ['Stale after', seconds(validation.staleAfterSeconds)],
    ]}/>

    <h2>Requested and actual placement</h2><Note>{coverageStatement}</Note><KV rows={[
      ['Requested', requestedCoverage(validation)], ['Actual', actualCoverage(validation)],
      ['Requested site provider', known(validation.requested?.siteProvider)],
      ['Requested site mode', known(validation.requested?.siteMode)],
      ['Requested region', known(validation.requested?.region)],
      ['Actual site provider', known(validation.actual?.siteProvider)],
      ['Actual site mode', known(validation.actual?.siteMode)],
      ['Actual region', known(validation.actual?.region)],
      ['Distinct nodes', yesNoUnknown(validation.placement?.distinctNodes)], ['Matches request', yesNoUnknown(validation.placement?.matchesRequest)],
      ['Placement reason', known(validation.placement?.reason)],
    ]}/>
    {!validation.actual?.nodes?.length ? <Empty warn>Node, GPU, and RDMA placement evidence is unavailable.</Empty>
      : <Table headers={['Node', 'UID', 'Unbounded site evidence', 'Region', 'Pool', 'GPU model', 'GPU UUIDs']}
        rows={validation.actual.nodes.map(node => [
          known(node.name), known(node.uid),
          <div className="node-identity">
            <strong>{topologySite(node.site, validation.actual?.siteProvider, validation.actual?.siteMode)}</strong>
            <span>{node.siteSourceKey || (validation.actual?.siteMode ? 'Site source unavailable' : 'Legacy artifact · source unavailable')}</span>
            {node.siteLabelConflict && <small className="warn">Canonical and fallback site labels conflict</small>}
          </div>,
          known(node.region), known(node.pool), known(node.gpuModel), list(node.gpuUuids),
        ])}/>}
    {!!nodeDevices(validation.actual?.nodes).length && <Table headers={['Node', 'Resource', 'Device', 'Interface', 'Port', 'Link layer', 'State']}
      rows={nodeDevices(validation.actual?.nodes).map(([node, device]) => [
        known(node.name), known(device.resourceName), known(device.device), known(device.interface),
        known(device.port), known(device.linkLayer), known(device.state),
      ])}/>}

    <h2>Source and image provenance</h2><KV rows={[
      ['Repository', known(validation.source?.repository)], ['Source revision', known(validation.source?.revision)],
      ['Image repository', known(validation.source?.imageRepository)], ['Image index digest', known(validation.source?.imageIndexDigest)],
      ['Image platform digest', known(validation.source?.imagePlatformDigest)], ['Image config digest', known(validation.source?.imageConfigDigest)],
      ['SBOM manifest digest', known(validation.source?.sbomManifestDigest)],
      ['SBOM layer digest', known(validation.source?.sbomLayerDigest)],
      ['VEX manifest digest', known(validation.source?.vexManifestDigest)],
      ['VEX layer digest', known(validation.source?.vexLayerDigest)],
      ['Signature manifest digest', known(validation.source?.signatureManifestDigest)],
      ['Signature layer digest', known(validation.source?.signatureLayerDigest)],
      ['Signature trust verified', yesNoUnknown(validation.source?.signatureTrustVerified)],
    ]}/>

    <h2>NCCL and transport evidence</h2><KV rows={[
      ['Collective', known([validation.collective?.library, validation.collective?.version, validation.collective?.operation].filter(Boolean).join(' / '))],
      ['Transport backend', known(validation.transport?.backend)], ['NCCL NET', known(validation.transport?.ncclNet)],
      ['NET/IB positive evidence', yesNoUnknown(validation.transport?.ibPositiveEvidence)],
      ['Interfaces', list(validation.transport?.interfaces)], ['RDMA devices', list(validation.transport?.rdmaDevices)],
      ['Socket fallback detected', yesNoUnknown(validation.transport?.socketFallbackDetected)],
    ]}/>
    {!!validation.transport?.evidence?.length && <Table headers={['Observed transport evidence']}
      rows={validation.transport.evidence.map(evidence => [evidence])}/>}
    {!!Object.keys(validation.transport?.environment || {}).length &&
      <Table headers={['Environment', 'Value']} rows={Object.entries(validation.transport?.environment || {})}/>}

    <h2>Parameters and bandwidth</h2><KV rows={[
      ['Operation', known(validation.parameters?.operation)], ['Data type', known(validation.parameters?.dataType)],
      ['Elements', known(validation.parameters?.elements)], ['World size', known(validation.parameters?.worldSize)],
      ['Processes per pod', known(validation.parameters?.processesPerPod)],
      ['Message sizes', validation.parameters?.messageSizesBytes?.length ? validation.parameters.messageSizesBytes.map(bytes).join(', ') : 'Unknown'],
      ['Warmup iterations', known(validation.parameters?.warmupIterations)], ['Iterations', known(validation.parameters?.iterations)],
      ['algbw min / median / mean / max', ['min', 'median', 'mean', 'max'].map(key => bandwidth(validation.summary?.algbwGbps?.[key as 'min'])).join(' / ')],
      ['busbw min / median / mean / max', ['min', 'median', 'mean', 'max'].map(key => bandwidth(validation.summary?.busbwGbps?.[key as 'min'])).join(' / ')],
      ['Duration', seconds(validation.durationSeconds)],
    ]}/>
    {!validation.measurements?.length ? <Empty warn>No per-rank bandwidth measurements were reported.</Empty>
      : <Table headers={['Rank', 'Message bytes', 'Iterations', 'Elapsed', '#algbw GB/s', '#busbw GB/s']}
        rows={validation.measurements.map(measurement => [
          known(measurement.rank), bytes(measurement.messageSizeBytes), known(measurement.iterations),
          seconds(measurement.elapsedSeconds), bandwidth(measurement.algbwGbps), bandwidth(measurement.busbwGbps),
        ])}/>}

    <h2>Correctness and process exits</h2><KV rows={[
      ['Correctness passed', yesNoUnknown(validation.correctness?.passed)], ['Maximum error', known(validation.correctness?.maxError)],
      ['Error count', known(validation.correctness?.errorCount)], ['Job exit code', known(validation.jobExitCode)],
      ['Rank exits', validation.rankExitCodes?.length ? validation.rankExitCodes.map(exit => `rank ${known(exit.rank)}: ${known(exit.code)}`).join(', ') : 'Unknown'],
    ]}/>
    {!!validation.pods?.length && <Table headers={['Pod', 'UID', 'Node', 'Exit']}
      rows={validation.pods.map(pod => [
        known(pod.name), known(pod.uid), known(pod.node), known(pod.exitCode),
      ])}/>}
    {!!validation.ranks?.length && <Table headers={['Rank', 'Pod UID', 'Node', 'Node UID', 'Peer authenticated', 'Memlock soft / hard', 'Exit']}
      rows={validation.ranks.map(rank => [
        known(rank.rank), known(rank.podUid), known(rank.node), known(rank.nodeUid),
        yesNoUnknown(rank.peerAuthenticated), `${bytes(rank.memlockSoftBytes)} / ${bytes(rank.memlockHardBytes)}`, known(rank.exitCode),
      ])}/>}
    {!!validation.errors?.length && <Table headers={['Stage', 'Code', 'Error']}
      rows={validation.errors.map(error => [known(error.stage), known(error.code), known(error.message)])}/>}

    <h2>Cleanup</h2><KV rows={[
      ['Status', known(validation.cleanup?.status)], ['Started', timestamp(validation.cleanup?.startedAt)],
      ['Completed', timestamp(validation.cleanup?.completedAt)], ['Owned resources', list(validation.cleanup?.ownedResources)],
      ['Remaining resources', list(validation.cleanup?.remainingResources)],
      ['Reason', known(validation.cleanup?.reason)],
    ]}/>

    <h2>Artifact verification and evidence</h2><KV rows={[
      ['Verification', known(validation.artifactVerification?.state)],
      ['Content type', known(validation.artifactVerification?.contentType)],
      ['Artifact URI', known(validation.artifactVerification?.uri)],
      ['Artifact SHA-256', known(validation.artifactVerification?.sha256)], ['Artifact size', bytes(validation.artifactVerification?.sizeBytes)],
      ['Verified at', timestamp(validation.artifactVerification?.verifiedAt)], ['Verification reason', known(validation.artifactVerification?.reason)],
      ['Producer', known([validation.producer?.name, validation.producer?.version].filter(Boolean).join(' / '))],
      ['Parser', known([validation.parser?.name, validation.parser?.version].filter(Boolean).join(' / '))],
    ]}/>
    {!validation.evidence?.length ? <Empty warn>No evidence hashes were reported.</Empty>
      : <Table headers={['Evidence', 'URI', 'SHA-256', '#Bytes', 'Captured']}
        rows={validation.evidence.map(evidence => [
          known(evidence.name), known(evidence.uri), known(evidence.sha256),
          bytes(evidence.sizeBytes), timestamp(evidence.capturedAt),
        ])}/>}
  </>;
}
