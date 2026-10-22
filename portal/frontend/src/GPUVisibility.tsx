// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { Link } from 'react-router-dom';
import { BoardResult, Empty, Note, PageTitle, ScopedLink, Table, text } from './components';
import { APIError, scopedURL, useBoard, useGlobalBoard } from './data';
import type { GPUInventory, GPUResourceQuantity, GPUWorkload } from './types';

function quantities(values: GPUResourceQuantity[]) {
  return values.length ? values.map(value => `${value.name} ${value.count}`).join(', ') : 'none';
}

function metadata(workload: GPUWorkload) {
  return [
    workload.queue && `queue ${workload.queue}`,
    workload.workspace && `workspace ${workload.workspace}`,
    workload.team && `team ${workload.team}`,
    workload.lane && `lane ${workload.lane}`,
    workload.profile && `profile ${workload.profile}`,
    workload.gpuClass && `GPU class ${workload.gpuClass}`,
    workload.runId && `run ${workload.runId}`,
  ].filter(Boolean).join(' · ') || '—';
}

function validDetail(workload: GPUWorkload) {
  const detail = workload.detail;
  return !!detail?.resourceUid && detail.resourceUid === workload.owner.uid &&
    detail.path === `/portal/workloads/${encodeURIComponent(detail.resourceUid)}`;
}

function WorkloadOwner({ workload, global }: { workload: GPUWorkload; global: boolean }) {
  const label = `${workload.owner.kind}/${workload.owner.name}`;
  const content = validDetail(workload) && (!global || !!workload.workspace)
    ? global
      ? <Link to={scopedURL(workload.detail!.path, workload.workspace || '', !!workload.workspace)}>{label}</Link>
      : <ScopedLink to={workload.detail!.path}>{label}</ScopedLink>
    : label;
  return <div className="gpu-owner"><strong>{content}</strong><span>{workload.owner.resolution.replaceAll('_', ' ')}</span>
    {workload.diagnostics.map(diagnostic => <span className="warn" key={diagnostic.code}>{diagnostic.message}</span>)}
  </div>;
}

export function GPUInventorySnapshot({ snapshot, global }: { snapshot: GPUInventory; global: boolean }) {
  const unavailable = snapshot.sources.filter(source => source.state !== 'ready');
  return <div className="gpu-inventory">
    <div className="gpu-summary" aria-label="GPU workload summary">
      <div><span>Owners</span><strong>{snapshot.summary.workloads}</strong></div>
      <div><span>Pods ready</span><strong>{snapshot.summary.readyPods} / {snapshot.summary.pods}</strong></div>
      <div><span>GPU resources</span><strong>{snapshot.summary.requestedFullGpus}</strong><small>full GPUs requested</small></div>
      <div><span>Placement</span><strong>{snapshot.summary.pendingPods} pending · {snapshot.summary.boundPods} bound</strong></div>
    </div>
    <Note>Generated {new Date(snapshot.generatedAt).toLocaleString()} · pending means unbound; bound means a node is assigned.</Note>
    {snapshot.partial && <Empty warn>Partial inventory: one or more owner sources could not be read. Pod and GPU totals may be incomplete.</Empty>}
    {snapshot.truncated && <Empty warn>Truncated inventory: the workload limit was reached. Totals and rows do not represent the full scope.</Empty>}
    {unavailable.map(source => <Note warn key={source.source}>Source {source.source} is {source.state}: {source.message || 'owner resolution is incomplete'}.</Note>)}
    {snapshot.summary.countUnknownPods > 0 && <Note warn>{snapshot.summary.countUnknownPods} pod(s) use DRA or another resource claim whose GPU count is unknown.</Note>}
    {!snapshot.workloads.length ? <Empty>No active GPU-consuming workloads were found in this scope.</Empty>
      : <Table headers={['Owner', 'Access', 'Namespace', 'Pods', 'Requested GPU resources', 'Pending / bound', 'Nodes', 'Queue and workspace']}
        rows={snapshot.workloads.map(workload => [
          <WorkloadOwner workload={workload} global={global}/>,
          <span className={'badge ' + (workload.managed ? 'run' : 'queue')}>{workload.managed ? 'Tau' : 'Unmanaged'}</span>,
          text(workload.namespace),
          <span>{workload.readyPods}/{workload.podCount} ready<br/><span className="muted">{workload.phases.running} running · {workload.phases.pending} pending</span></span>,
          <span>{quantities(workload.resources)}{workload.countUnknown && <><br/><span className="warn">count unknown for {workload.countUnknownPods} pod(s)</span></>}</span>,
          <span>{workload.pendingPods} / {workload.boundPods}<br/><span className="muted">{quantities(workload.pendingResources)} / {quantities(workload.boundResources)}</span></span>,
          workload.nodes.length ? workload.nodes.join(', ') : 'unbound',
          metadata(workload),
        ])}/>}
  </div>;
}

function inventoryHint(global: boolean, error?: Error | null) {
  if (error instanceof APIError) {
    if (error.state === 'disabled') return ' Fleet visibility is intentionally enabled only for authenticated-proxy deployments';
    if (error.state === 'setup_required') return ' Setup required: select a workspace with a resolved namespace and LocalQueue';
    if (error.state === 'forbidden' || error.status === 403) return ' Check the Portal ServiceAccount read RBAC';
    if (error.state === 'unavailable' || error.status === 502) return ' The Kubernetes inventory source is unavailable';
  }
  return global
    ? ' The fleet endpoint is available only in authenticated-proxy deployments and requires cluster-wide read RBAC'
    : ' Workspace GPU visibility requires a server-resolved namespace and LocalQueue plus read RBAC';
}

export function WorkspaceGPUWorkloads() {
  const query = useBoard<GPUInventory>('/api/portal/workspace-gpu-workloads');
  return <section aria-labelledby="workspace-gpu-workloads"><h2 id="workspace-gpu-workloads">Active Tau GPU workloads</h2>
    <p className="muted">Current GPU-consuming owners in this workspace and LocalQueue. Historical and non-GPU Runs remain below.</p>
    <BoardResult query={query} label="Workspace GPU workloads" partial={query.data?.partial}
      hint={inventoryHint(false, query.error)}>{snapshot => <GPUInventorySnapshot snapshot={snapshot} global={false}/>}</BoardResult>
  </section>;
}

export function GPUConsumers() {
  const query = useGlobalBoard<GPUInventory>('/api/portal/gpu-consumers');
  return <><PageTitle title="GPU Consumers">Cluster-wide GPU-consuming owners for authenticated Portal users. This inventory includes Tau-managed and unmanaged workloads and is not workspace-scoped.</PageTitle>
    <BoardResult query={query} label="GPU consumers" partial={query.data?.partial}
      hint={inventoryHint(true, query.error)}>{snapshot => <GPUInventorySnapshot snapshot={snapshot} global/>}</BoardResult>
  </>;
}
