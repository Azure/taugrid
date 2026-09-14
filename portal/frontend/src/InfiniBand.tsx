// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useState, type ReactNode } from 'react';
import { useParams } from 'react-router-dom';
import { useBoard } from './data';
import { BoardResult, Empty, KV, Note, PageTitle, ScopedLink, Table, n1 } from './components';
import type {
  RDMAFreshness, RDMAHistoricalStatus, RDMANode, RDMARdmaDevice, RDMAValidation,
  RDMAValidationDetail, RDMAValidationPage, RDMAValidationState, RDMAValidationSummary, Nodes,
} from './types';

const pageSize = 20;
const coverageStatement = 'Point-in-time two-GPU inter-node RDMA validation; this is not continuous InfiniBand or fleet health.';
const inventoryCaveat = 'Fleet inventory and site or node-pool labels do not imply that a validation covered every GPU or performed multi-site distributed training.';

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
    requested.site, requested.pool, requested.gpuModel,
  ].filter(Boolean).join(' · ') || 'Unknown';
}

function actualCoverage(validation: RDMAValidation) {
  const nodes = validation.actual?.nodes?.length;
  const gpus = actualGPUs(validation);
  return [
    nodes === undefined ? undefined : `${nodes} nodes`,
    gpus === undefined ? undefined : `${gpus} GPUs`,
    validation.actual?.site, validation.actual?.pool,
    validation.placement?.matchesRequest === true ? 'matches request' :
      validation.placement?.matchesRequest === false ? 'does not match request' : undefined,
  ].filter(Boolean).join(' · ') || 'Unknown';
}

function bandwidthSummary(validation: RDMAValidation) {
  const metric = (name: string, values?: { min?: number | null; mean?: number | null }) => {
    if (values?.mean !== undefined && values.mean !== null) return `${name} avg ${bandwidth(values.mean)}`;
    if (values?.min !== undefined && values.min !== null) return `${name} min ${bandwidth(values.min)}`;
    return `${name} Unknown`;
  };
  return `${metric('algbw', validation.summary?.algbwGbps)} · ${metric('busbw', validation.summary?.busbwGbps)}`;
}

export function InfiniBandFleet() {
  return <><Note>{coverageStatement}</Note><Note>{inventoryCaveat}</Note><FleetInfiniBandTopology/><LatestValidation/><ValidationHistory/></>;
}

function rdmaScheduling(node: Nodes['nodes'][number]) {
  const resources = node.rdmaResources || [];
  if (!resources.length) return 'Not advertised';
  return resources.map(resource =>
    `${resource.name} ${resource.allocatable}/${resource.capacity} allocatable`,
  ).join(', ');
}

function FleetInfiniBandTopology() {
  const inventoryQuery = useBoard<Nodes>('/api/portal/nodes');
  const latestQuery = useBoard<RDMAValidationSummary>('/api/portal/rdma-validations/summary');
  return <><h2>Fleet InfiniBand capability and site</h2>
    <Note>InfiniBand capability is derived from each node's advertised <code>rdma/*</code> scheduling resources. It is inventory, not link health. Per-GPU health remains available from the Health view.</Note>
    <BoardResult query={inventoryQuery} label="InfiniBand fleet inventory" hint=" — start the portal with Kubernetes access (in-cluster ServiceAccount or --kubeconfig).">{snapshot => {
      const latest = latestQuery.data?.latest;
      const validationSite = latest?.actual?.site;
      const testedByName = new Map((latest?.actual?.nodes || []).map(node => [node.name, node]));
      const nodes = (snapshot.nodes || []).filter(node => node.gpuCapacity > 0);
      return <>
        <Note>GPU nodes: {nodes.length} · RDMA advertised: {snapshot.rdmaAdvertisedGpuNodes ?? 'Unknown'} · latest tested site / pool: {known([validationSite, latest?.actual?.pool].filter(Boolean).join(' / '))}</Note>
        {latestQuery.isError && <Note warn>Latest run coverage is unavailable; inventory capability is still shown independently.</Note>}
        {!nodes.length ? <Empty>No GPU or RDMA-capable nodes were reported by the authorized fleet inventory.</Empty>
          : <Table headers={['Node', 'GPU inventory', 'IB / RDMA scheduling', 'Pool / region / zone', 'Validated topology', 'Same validated site', 'GPU health']}
            rows={nodes.map(node => {
              const tested = testedByName.get(node.name);
              const sameSite = tested?.site && validationSite ? tested.site === validationSite ? 'Yes' : 'No' : 'Unknown';
              const inventoryLocation = [
                node.agentPool ? `${node.agentPool}${node.agentPoolLabel ? ` (${node.agentPoolLabel})` : ''}` : undefined,
                node.region ? `${node.region}${node.regionLabel ? ` (${node.regionLabel})` : ''}` : undefined,
                node.zone ? `${node.zone}${node.zoneLabel ? ` (${node.zoneLabel})` : ''}` : undefined,
              ].filter(Boolean).join(' · ') || 'Unknown';
              return [
                node.name,
                `${node.gpuCapacity || 0} ${node.gpuProduct || node.sku || 'GPU model Unknown'}`,
                rdmaScheduling(node),
                inventoryLocation,
                tested ? `${tested.site || 'site Unknown'} / ${tested.pool || 'pool Unknown'} · ${stateLabel(latest?.state || 'unknown')} · ${list(tested.gpuUuids)}` : 'Not tested in latest run',
                sameSite,
                <ScopedLink to={'/portal/fleet?view=health&instance=' + encodeURIComponent(node.name)}>Open per-GPU metrics</ScopedLink>,
              ];
            })}/>}
      </>;
    }}</BoardResult>
  </>;
}

function LatestValidation() {
  const query = useBoard<RDMAValidationSummary>('/api/portal/rdma-validations/summary');
  return <><h2>Latest validation</h2><BoardResult query={query} label="InfiniBand latest validation">{snapshot => {
    const validation = snapshot.latest;
    if (!validation) return <Empty>No InfiniBand validation result is available in this scope. Current state is Unknown.</Empty>;
    return <section className="validation-summary" aria-label="Latest InfiniBand validation result">
      <div className="validation-summary-head">
        <div><ValidationState state={validation.state}/><strong>{validation.validationId}</strong></div>
        <ScopedLink to={'/portal/fleet/infiniband/' + encodeURIComponent(validation.validationId)}>View technical evidence →</ScopedLink>
      </div>
      <p>{stateMeaning(validation)}</p>
      <dl className="validation-summary-grid">
        <div><dt>Last validated</dt><dd>{timestamp(validation.observedAt || validation.completedAt)}</dd></div>
        <div><dt>Age</dt><dd>{age(validation.ageSeconds)}</dd></div>
        <div><dt>Scope</dt><dd>{known([validation.cluster, validation.workspaceId].filter(Boolean).join(' / '))}</dd></div>
        <div><dt>Tested</dt><dd>{actualCoverage(validation)}</dd></div>
        <div><dt>Transport</dt><dd>{known(validation.transport?.backend)} · socket fallback {yesNoUnknown(validation.transport?.socketFallbackDetected)}</dd></div>
        <div><dt>Bandwidth</dt><dd>{bandwidthSummary(validation)}</dd></div>
      </dl>
      <Note warn={validation.state !== 'passed'}>{known(validation.reason)}</Note>
    </section>;
  }}</BoardResult></>;
}

function ValidationHistory() {
  const [cursor, setCursor] = useState('');
  const [previous, setPrevious] = useState<string[]>([]);
  const path = '/api/portal/rdma-validations?' + new URLSearchParams({
    limit: String(pageSize), ...(cursor ? { cursor } : {}),
  });
  const query = useBoard<RDMAValidationPage>(path);
  return <><h2>Validation history</h2><BoardResult query={query} label="InfiniBand validation history">{page => <>
    {!page.validations?.length ? <Empty>No InfiniBand validation history is available in this scope.</Empty>
      : <Table headers={['Completed', 'Validation', 'State', 'Scope', 'Tested', 'Transport', 'Bandwidth', 'Cleanup', 'Reason']}
        rows={page.validations.map(validation => [
          timestamp(validation.completedAt || validation.startedAt),
          <ScopedLink to={'/portal/fleet/infiniband/' + encodeURIComponent(validation.validationId)}>{validation.validationId}</ScopedLink>,
          <ValidationState state={validation.state}/>,
          known([validation.cluster, validation.workspaceId].filter(Boolean).join(' / ')),
          actualCoverage(validation),
          `${known(validation.transport?.backend)} · socket ${yesNoUnknown(validation.transport?.socketFallbackDetected)}`,
          bandwidthSummary(validation),
          known(validation.cleanup?.status),
          known(validation.reason),
        ])}/>}
    <nav className="pagination" aria-label="InfiniBand validation history pages">
      <button className="btn" type="button" disabled={!previous.length || query.isFetching} onClick={() => {
        const next = [...previous]; setCursor(next.pop() || ''); setPrevious(next);
      }}>Previous page</button>
      <span>Page {previous.length + 1}{page.truncated ? ' · more results available' : ''}</span>
      <button className="btn" type="button" disabled={!page.nextCursor || query.isFetching} onClick={() => {
        if (!page.nextCursor) return;
        setPrevious(items => [...items, cursor]); setCursor(page.nextCursor);
      }}>Next page</button>
    </nav>
  </>}</BoardResult></>;
}

export function InfiniBandValidationDetail() {
  const { validationId = '' } = useParams();
  const query = useBoard<RDMAValidationDetail>('/api/portal/rdma-validations/' + encodeURIComponent(validationId), !!validationId);
  return <><div className="page-head"><div><PageTitle title="InfiniBand validation">{coverageStatement}</PageTitle></div>
    <ScopedLink to="/portal/fleet?view=infiniband" className="back">← Back to InfiniBand</ScopedLink></div>
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
      ['Distinct nodes', yesNoUnknown(validation.placement?.distinctNodes)], ['Matches request', yesNoUnknown(validation.placement?.matchesRequest)],
      ['Placement reason', known(validation.placement?.reason)],
    ]}/>
    {!validation.actual?.nodes?.length ? <Empty warn>Node, GPU, and RDMA placement evidence is unavailable.</Empty>
      : <Table headers={['Node', 'UID', 'Site / pool', 'GPU model', 'GPU UUIDs']}
        rows={validation.actual.nodes.map(node => [
          known(node.name), known(node.uid), known([node.site, node.pool].filter(Boolean).join(' / ')),
          known(node.gpuModel), list(node.gpuUuids),
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
