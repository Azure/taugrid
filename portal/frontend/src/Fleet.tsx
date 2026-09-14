// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useLocation } from 'react-router-dom';
import { useBoard } from './data';
import { BoardResult, Empty, Note, PageTitle, ScopedLink, Subtabs, Table, measured, n1, text, utilizationSummary } from './components';
import type { Cluster, Nodes, NodeUtil } from './types';

const kustoHint = ' — start the portal with a --kusto-query-command.';
export function Fleet() {
  const location = useLocation();
  const requested = new URLSearchParams(location.search).get('view');
  const aliases: Record<string, string> = { '/portal/gpu': 'util', '/portal/nodes': 'compute', '/portal/cluster': 'health' };
  const view = aliases[location.pathname] || (['health', 'util', 'compute'].includes(requested || '') ? requested : 'health');
  return <><PageTitle title="Fleet">GPU health, utilization, and hardware inventory for the whole fleet.</PageTitle>
    <Subtabs base="/portal/fleet" active={view || 'health'} items={[['health', 'Health'], ['util', 'Utilization'], ['compute', 'Compute']]}/>
    {view === 'compute' ? <Compute/> : view === 'util' ? <Utilization/> : <Health/>}</>;
}
function Health() {
  const instance = new URLSearchParams(useLocation().search).get('instance') || '';
  const query = useBoard<Cluster>('/api/portal/cluster' + (instance ? '?instance=' + encodeURIComponent(instance) : ''));
  return <><p className="muted">Latest per-GPU health from GpuHealth() (Metrics ADX), via /api/portal/cluster. Additional network and alert signals will be available in a future update.</p>
    {instance && <Note>scoped to instance <code>{instance}</code> <ScopedLink to="/portal/fleet?view=health" className="back">clear</ScopedLink></Note>}
    <BoardResult query={query} label="Cluster board" hint={kustoHint}>{snap => <>
      <Note>window: {text(snap.window)} · returned GPUs: {snap.totalGPUs} · health observations: {snap.healthObservedGPUs} / {snap.totalGPUs} · observed errors: {snap.healthObservedGPUs > 0 ? snap.errorGPUs : '—'} · unknown health: {snap.unknownHealthGPUs}{!!snap.models?.length && ' · models: ' + snap.models.map(m => m.modelName + '×' + m.gpus).join(', ')}</Note>
      <Note warn={!snap.telemetryAvailable}>Returned GPUs are telemetry observations, not fleet inventory. Unknown readings are unavailable, not measured zero.</Note>
      {!snap.gpus?.length ? <Empty>No GPU samples in the window. The cluster may report no DCGM metrics.</Empty>
        : <Table headers={['Instance', 'GPU', 'Model', '#Util %', '#Temp °C', '#Power W', '#Mem used MB', '#Uncorr. rows', 'Health']}
          rows={snap.gpus.map(g => [text(g.instance), text(g.gpu), text(g.modelName), n1(g.utilizationPct), n1(g.temperatureCelsius), n1(g.powerWatts), n1(g.memoryUsedMB), n1(g.uncorrectableRemappedRows), <span className={g.healthy === false ? 'warn' : g.healthy === true ? '' : 'muted'}>{g.healthy === true ? 'ok (observed)' : g.healthy === false ? 'error' : 'unavailable'}</span>])}/>}
    </>}</BoardResult></>;
}
function Compute() {
  const query = useBoard<Nodes>('/api/portal/nodes');
  return <><p className="muted">Fleet hardware inventory — SKU, agentpool, and CPU/memory/GPU capacity per node, via /api/portal/nodes.</p>
    <BoardResult query={query} label="Nodes board" hint=" — start the portal with Kubernetes access (in-cluster ServiceAccount or --kubeconfig).">{snap => <>
      <Note>nodes: {snap.totalNodes ?? 0} ({snap.readyNodes ?? 0} ready) · GPU nodes: {snap.gpuNodes ?? 0} · GPUs: {snap.totalGPUs ?? 0} · CPU cores: {snap.totalCPUCores ?? 0} · memory: {n1(snap.totalMemoryGiB)} GiB</Note>
      {!snap.totalNodes ? <Empty>No nodes reported. The cluster may be empty or access may be restricted.</Empty> : <>
        {!!snap.skus?.length && <><h3>Fleet composition by SKU</h3><Table headers={['SKU', '#Nodes', '#GPUs']} rows={snap.skus.map(s => [text(s.sku), s.nodes, s.gpus])}/></>}
        <h3>Nodes</h3><Table headers={['Node', 'Pool', 'SKU', 'Region/Zone', '#CPU', '#Mem GiB', '#GPU', 'Status']}
          rows={(snap.nodes || []).map(n => [text(n.name), text(n.agentPool), text(n.sku), [n.region, n.zone].filter(Boolean).join(' / ') || '—', n.cpuCores, n1(n.memoryGiB), n.gpuCapacity ? n.gpuCapacity + (n.gpuProduct ? ' ' + n.gpuProduct : '') : '—', <span className={n.ready ? '' : 'warn'}>{n.ready ? 'Ready' : 'NotReady'}</span>])}/>
      </>}
      {!!snap.daemonSets?.length ? <><h3>Runtime DaemonSets</h3><Table headers={['Namespace', 'DaemonSet', '#Ready / Desired', '#Available', 'Status']}
        rows={snap.daemonSets.map(ds => [text(ds.namespace), text(ds.name), `${ds.ready} / ${ds.desired}`, ds.available, <span className={ds.healthy === false ? 'warn' : ''}>{ds.healthy === false ? 'Not ready' : 'Ready'}</span>])}/></>
        : snap.daemonSetsError && <Note warn>Runtime DaemonSets unavailable: {snap.daemonSetsError}</Note>}
    </>}</BoardResult></>;
}
function Utilization() {
  const query = useBoard<Cluster>('/api/portal/cluster');
  return <><p className="muted">Per-GPU utilization from GpuHealth() (via /api/portal/cluster), with per-node CPU/memory from node-exporter (via /api/portal/nodeutil) below.</p>
    <BoardResult query={query} label="GPU utilization" hint={kustoHint}>{snap => {
      const gpus = [...(snap.gpus || [])].sort((a, b) =>
        (measured(b.utilizationPct) ? b.utilizationPct : -1) - (measured(a.utilizationPct) ? a.utilizationPct : -1));
      if (!gpus.length) return <Empty>No GPU samples in the window. The cluster may report no DCGM metrics.</Empty>;
      const summary = utilizationSummary(gpus);
      return <><Note>window: {text(snap.window)} · returned GPUs: {summary.total} · measured: {summary.observed} / {summary.total} · measured avg: {summary.average === null ? '—' : `${n1(summary.average)}%`} · measured idle (&lt;5%): {summary.idle ?? '—'}</Note>
        <Note warn={!summary.observed}>Unobserved utilization is unavailable, not idle. These observations do not establish fleet inventory coverage.</Note>
        <Note>Heatmap and per-team breakdown will be available in a future update.</Note>
        <Table headers={['Instance', 'GPU', 'Model', '#Util %', '#Mem used MB', '#Power W']} rows={gpus.map(g => [text(g.instance), text(g.gpu), text(g.modelName), <span className={measured(g.utilizationPct) && g.utilizationPct < 5 ? 'muted' : ''}>{n1(g.utilizationPct)}</span>, n1(g.memoryUsedMB), n1(g.powerWatts)])}/></>;
    }}</BoardResult><NodeUtilization/></>;
}
function NodeUtilization() {
  const query = useBoard<NodeUtil>('/api/portal/nodeutil');
  return <><h3>Node resource utilization</h3><Note>Per-node CPU and memory from node-exporter (Metrics ADX), via /api/portal/nodeutil.</Note>
    <BoardResult query={query} label="Node utilization" hint={kustoHint}>{snap => !snap.nodes?.length ? <Empty>No node samples in the window.</Empty> : <>
      <Note>window: {text(snap.window)} · nodes: {snap.nodes.length} · queried: {snap.queriedAt || 'not reported'}</Note>
      <Note>CPU coverage is the usable interval duration over the requested window for observed cores, not fleet inventory coverage. Unknown is not measured zero.</Note>
      <Table headers={['Node', '#Observed cores', '#CPU %', '#Mem %', '#Mem GiB (used/tot)', 'CPU coverage', 'Memory samples']}
        rows={snap.nodes.map(n => {
          const usedGiB = measured(n.memTotalBytes) && measured(n.memAvailBytes) && n.memAvailBytes <= n.memTotalBytes ? (n.memTotalBytes - n.memAvailBytes) / 1024 ** 3 : null;
          const totalGiB = measured(n.memTotalBytes) ? n.memTotalBytes / 1024 ** 3 : null;
          const coverage = n.cpuCoverage;
          return [text(n.instance), n1(n.cpuCores), <span className={measured(n.cpuUtilPct) && n.cpuUtilPct < 5 ? 'muted' : ''}>{n1(n.cpuUtilPct)}</span>, n1(n.memUsedPct), `${n1(usedGiB)} / ${n1(totalGiB)}`,
            coverage ? `${n1(coverage.windowCoveragePct)}% · ${coverage.usableCores}/${coverage.observedCores} cores · ${coverage.samples} samples · ${n1(coverage.observedSeconds)}s · ${coverage.counterResets} counter resets` : 'Not reported',
            `total: ${n.memTotalSampleAt || 'unknown'} · available: ${n.memAvailSampleAt || 'unknown'}`];
        })}/>
    </>}</BoardResult></>;
}
