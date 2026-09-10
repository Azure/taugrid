// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import type { ConfigView, RunView } from './types';

type RecordValue = Record<string, unknown>;
function object(value: unknown): RecordValue | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as RecordValue : undefined;
}
function parse(raw: string | undefined): RecordValue | undefined {
  if (!raw) return undefined;
  try { return object(JSON.parse(raw)); } catch (error) {
    if (!(error instanceof SyntaxError)) throw error;
    return undefined;
  }
}
function launchTag(raw: string | undefined): RecordValue | undefined {
  if (!raw?.startsWith('base64url:')) return parse(raw);
  const encoded = raw.slice('base64url:'.length);
  if (encoded.length > 16384 || encoded.length % 4 === 1 || !/^[A-Za-z0-9_-]+$/.test(encoded)) return undefined;
  const decoded = atob(encoded.replaceAll('-', '+').replaceAll('_', '/'));
  return parse(new TextDecoder().decode(Uint8Array.from(decoded, char => char.charCodeAt(0))));
}
function field(record: RecordValue | undefined, path: string): unknown {
  if (record && path in record) return record[path];
  let value: unknown = record;
  for (const key of path.split('.')) value = object(value)?.[key];
  return value;
}
function text(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() ? value.trim() : undefined;
}
function count(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : undefined;
}
function safeText(value: unknown): string | undefined {
  const result = text(value);
  return result && /^[a-zA-Z0-9_./:@+-]+$/.test(result) && !result.includes('://') &&
    (!result.includes('@') || result.includes('@sha256:')) ? result : undefined;
}
export function storedTauCommand(value: string | undefined): string {
  if (!value?.trim()) return 'Not recorded';
  // A legacy store can contain user-supplied commands. Do not echo arbitrary
  // shell/env payloads or credential-bearing arguments into the summary.
  const allowedFlags = new Set(['--config', '--namespace', '--context', '--dry-run', '--force', '--from']);
  if (!/^tau (?:run|submit) /.test(value) || !/^[a-zA-Z0-9_./: -]+$/.test(value) || value.includes('://') ||
    value.split(/\s+/).some(arg => arg.startsWith('-') && !allowedFlags.has(arg))) {
    return 'Withheld — command may contain credentials or shell content';
  }
  return value;
}
function system(run: RunView, name: string): string | undefined {
  return run.systems?.find(item => item.name === name && item.collection_state !== 'not_collected' &&
    item.value !== 'not collected')?.value || undefined;
}
function configRecord(config: ConfigView): RecordValue | undefined {
  return parse(config.normalized_json) ?? parse(config.indexed_fields);
}

export function launchRows(run: RunView) {
  const tagged = launchTag(run.tags?.['tau.launch']);
  const launch = run.launch?.version === 1 ? object(run.launch) : tagged?.version === 1 ? tagged : undefined;
  const configs = (run.configs ?? []).filter(config => config.run_id === run.run_id).map(configRecord).filter(value => value !== undefined);
  // Multiple config artifacts may describe one run. Use a value only when
  // those records agree; never choose an arbitrary config or sibling run.
  const conflicts: string[] = [];
  const fromConfig = (...paths: string[]): unknown => {
    const values = configs.flatMap(config => paths.map(path => field(config, path)).filter(value => value !== undefined && value !== null && value !== ''));
    const unique = [...new Map(values.map(value => [JSON.stringify(value), value])).values()];
    if (unique.length > 1) { conflicts.push(paths[0]); return undefined; }
    return unique[0];
  };
  const kind = safeText(launch?.workload_kind) ?? safeText(fromConfig('run.engine', 'run.workload_kind', 'engine'));
  const isRay = kind === 'rayjob' || kind === 'rayjob-eval';
  const workers = count(launch?.workers) ?? count(fromConfig(isRay ? 'compute.workers' : 'execution.nodes'));
  const perWorker = count(launch?.gpus_per_worker) ?? count(fromConfig(isRay ? 'compute.gpus_per_worker' : 'compute.gpus'));
  const explicitTotal = count(launch?.gpu_total);
  const product = perWorker !== undefined && workers !== undefined ? perWorker * workers : undefined;
  const total = explicitTotal ?? (product !== undefined && Number.isSafeInteger(product) ? product : undefined);
  const gpuClass = safeText(launch?.gpu_class) ?? safeText(fromConfig('policy.gpu_class'));
  const modeValue = launch ? launch.gpu_resource_mode : fromConfig('compute.gpu_resource_mode');
  const mode = typeof modeValue === 'string' && ['none', 'device-plugin', 'dra', 'mig'].includes(modeValue) ? modeValue : undefined;
  const profileValue = launch ? launch.mig_profile : fromConfig('compute.mig_profile');
  const migProfile = typeof profileValue === 'string' && /^[0-9]+g\.[0-9]+gb$/.test(profileValue) ? profileValue : undefined;
  const resourceValue = launch?.gpu_resource_name;
  const resource = typeof resourceValue === 'string' && /^(?:nvidia\.com\/gpu|nvidia\.com\/mig-[0-9]+g\.[0-9]+gb)$/.test(resourceValue) ? resourceValue : undefined;
  const inconsistent = (mode && mode !== 'mig' && (migProfile || resource?.startsWith('nvidia.com/mig-'))) ||
    (mode === 'mig' && resource === 'nvidia.com/gpu') ||
    (migProfile && resource && resource !== `nvidia.com/mig-${migProfile}`) ||
    ((mode === 'none' || mode === 'dra') && resource);
  const isMIG = !inconsistent && (mode === 'mig' || !!migProfile || !!resource?.startsWith('nvidia.com/mig-'));
  const units = isMIG ? 'MIG slices' : !inconsistent && (mode === 'device-plugin' || mode === 'dra' || resource === 'nvidia.com/gpu' ||
    (total === 0 && (perWorker === undefined || perWorker === 0))) ? 'GPUs' : 'GPU units (unit unspecified)';
  const rows: [string, string | number | undefined][] = [
    ['Workload kind', kind],
    [`Requested ${units} · total`, total],
    [`Requested ${units} · per worker/pod`, perWorker],
    ['Requested workers/pods', workers],
    ['Requested GPU class', gpuClass],
    ['GPU resource mode', inconsistent ? undefined : mode],
    ['GPU resource name', inconsistent ? undefined : resource],
    ['MIG profile', inconsistent ? undefined : migProfile],
    ['Observed GPUs', undefined],
    ['Image', safeText(launch?.image) ?? safeText(fromConfig('runtime.image', 'run.image', 'image')) ?? run.image_digest],
    ['Workload entrypoint', safeText(launch?.entrypoint) ?? safeText(fromConfig('run.entrypoint', 'run.script', 'entrypoint', 'script'))],
    ['Launcher', safeText(launch?.launcher) ?? safeText(fromConfig('execution.launcher'))],
    ['Resource profile', safeText(launch?.profile) ?? safeText(fromConfig('compute.profile', 'policy.profile')) ?? system(run, 'Profile')],
    ['Local queue · recorded', system(run, 'Local queue') ?? safeText(fromConfig('policy.queue'))],
    ['Workspace · recorded', safeText(launch?.workspace) ?? run.workspace_id ?? run.tags?.tau_workspace ?? safeText(fromConfig('policy.workspace'))],
    ['Namespace · recorded', safeText(launch?.namespace) ?? system(run, 'Namespace') ?? safeText(fromConfig('policy.namespace'))],
    ['Nodes · observed names', system(run, 'Nodes')],
  ];
  if (count(launch?.cpu_workers) !== undefined) rows.push(['Additional CPU workers', count(launch?.cpu_workers)]);
  if (!launch && system(run, 'GPU count') !== undefined) rows.push(['Legacy GPU count · basis unspecified', system(run, 'GPU count')]);
  if (!gpuClass && system(run, 'GPU class')) rows.push(['Legacy GPU class · basis unspecified', system(run, 'GPU class')]);
  return {
    rows, conflicts: [...new Set(conflicts)],
    source: launch ? 'Resolved launch record' : configs.length ? 'Recorded config' : 'Legacy run context',
    computedTotal: explicitTotal === undefined && total !== undefined,
    invalidConfig: (run.configs ?? []).some(config => (config.normalized_json || config.indexed_fields) && !configRecord(config)),
    isRay, units,
    command: storedTauCommand(run.tau_command || text(launch?.tau_command)),
  };
}
