// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export interface RunTarget {
  namespace: string;
  name: string;
  kind?: 'Job' | 'RayJob';
}

export interface Phase {
  key: string;
  label: string;
  state: 'done' | 'active' | 'pending' | 'warning' | 'unknown' | 'skipped';
  detail: string;
  hint: string;
}

export interface RunList {
  runs: (RunTarget & { state: string; queue?: string | null; created?: string | null })[];
  warnings: string[];
  truncated: boolean;
}

export interface LogSnapshot {
  pod: string;
  container: string;
  text: string;
  limitBytes: number;
  possiblyTruncated: boolean;
}

const isRecord = (entry: unknown): entry is Record<string, unknown> =>
  typeof entry === 'object' && entry !== null && !Array.isArray(entry);
const strings = (entry: unknown): entry is string[] => Array.isArray(entry) && entry.every(value => typeof value === 'string');
const count = (entry: unknown): boolean => typeof entry === 'number' && Number.isSafeInteger(entry) && entry >= 0;
const optionalText = (entry: Record<string, unknown>, keys: string[]): boolean =>
    keys.every(key => entry[key] == null || typeof entry[key] === 'string');
const kindValid = (entry: unknown): boolean => entry === 'Job' || entry === 'RayJob';

export function portalLink(value: unknown): string | null {
  if (typeof value !== 'string' || /[\s\\]/.test(value)) { return null; }
  try {
    const url = new URL(value);
    return ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password ? url.href : null;
  } catch { return null; }
}

export function parseRuns(value: unknown): RunList {
  if (!isRecord(value) || !Array.isArray(value.runs) || !strings(value.warnings) || typeof value.truncated !== 'boolean' ||
    !value.runs.every(row => isRecord(row) && typeof row.name === 'string' && typeof row.namespace === 'string' &&
      kindValid(row.kind) && typeof row.state === 'string' && ['queue', 'created'].every(key => row[key] == null || typeof row[key] === 'string'))) {
    throw new Error('The Jupyter server returned an invalid run list.');
  }
  return value as unknown as RunList;
}

export function parseLogs(value: unknown): LogSnapshot {
  if (!isRecord(value) || !['pod', 'container', 'text'].every(key => typeof value[key] === 'string') ||
    value.limitBytes !== 65536 || typeof value.possiblyTruncated !== 'boolean' || new TextEncoder().encode(value.text as string).length > 65536) {
    throw new Error('The Jupyter server returned an invalid log snapshot.');
  }
  return value as unknown as LogSnapshot;
}

export interface RunStatus extends RunTarget {
  uid?: string;
  metrics?: LossMetrics;
  phases?: Phase[];
  output?: { path?: string | null; pvc?: string | null; portalUrl?: string | null };
  existing: boolean;
  state?: string | null;
  displayState?: string | null;
  rayClusterName?: string | null;
  jobId?: string | null;
  deploymentStatus?: string | null;
  queue?: string | null;
  admitted?: boolean | null;
  message?: string | null;
  readyPods: number;
  totalPods: number;
  terminal: boolean;
  pods: {
    name: string;
    phase?: string | null;
    node?: string | null;
    ready: boolean;
    restarts: number;
    rayNodeType?: string | null;
    role?: string | null;
    uid?: string | null;
    containers?: string[];
  }[];
  diagnostics: {
    code: string;
    severity: string;
    message: string;
    suggestion?: string | null;
  }[];
}

export function parseRunStatus(value: unknown): RunStatus {
  const output = isRecord(value) ? value.output : undefined;
  if (isRecord(value) && (
    !(value.kind == null || kindValid(value.kind)) ||
    !(value.phases == null || (Array.isArray(value.phases) && value.phases.every(phase => isRecord(phase) &&
      ['key', 'label', 'detail', 'hint'].every(key => typeof phase[key] === 'string') &&
      ['done', 'active', 'pending', 'warning', 'unknown', 'skipped'].includes(String(phase.state))))) ||
    !(output == null || (isRecord(output) && ['path', 'pvc', 'portalUrl'].every(key => output[key] == null || typeof output[key] === 'string'))) ||
    (Array.isArray(value.pods) && value.pods.some(pod => isRecord(pod) && pod.containers != null && !strings(pod.containers)))
  )) { throw new Error('The Jupyter server returned an invalid run status.'); }
  const count = (entry: unknown): boolean =>
    typeof entry === 'number' && Number.isSafeInteger(entry) && entry >= 0;
  if (!isRecord(value) || typeof value.name !== 'string' || typeof value.namespace !== 'string' ||
    typeof value.existing !== 'boolean' || typeof value.terminal !== 'boolean' ||
    !count(value.readyPods) || !count(value.totalPods) ||
    !(value.admitted == null || typeof value.admitted === 'boolean') ||
    !optionalText(value, ['state', 'displayState', 'rayClusterName', 'jobId', 'deploymentStatus', 'queue', 'message']) ||
    !Array.isArray(value.pods) || !value.pods.every(pod => isRecord(pod) &&
      typeof pod.name === 'string' && typeof pod.ready === 'boolean' && count(pod.restarts) &&
      optionalText(pod, ['phase', 'node', 'rayNodeType'])) ||
    !Array.isArray(value.diagnostics) || !value.diagnostics.every(note => isRecord(note) &&
      typeof note.code === 'string' && typeof note.severity === 'string' && typeof note.message === 'string' &&
      optionalText(note, ['suggestion']))) {
    throw new Error('The Jupyter server returned invalid run status. Check the TauGrid server extension version and retry.');
  }
  return { ...value, ...(value.metrics === undefined ? {} : { metrics: parseMetrics(value.metrics) }) } as unknown as RunStatus;
}

export interface LossMetrics {
  state: 'ready' | 'empty' | 'unavailable' | 'error';
  source: Record<string, string> | null;
  samples: { step: number; value: number }[];
  checkedAt: string | null;
  stale: boolean;
  limitBytes: number;
  maxPoints: number;
  possiblyTruncated: boolean;
  truncationReasons: string[];
  message: string;
  coverage?: Record<string, unknown> | null;
}

export function parseMetrics(value: unknown): LossMetrics {
  if (isRecord(value) && ['ready', 'empty', 'unavailable', 'error'].includes(String(value.state)) &&
    (value.source === null || (isRecord(value.source) && Object.values(value.source).every(entry => typeof entry === 'string'))) &&
    Array.isArray(value.samples) && value.samples.length <= 512 && value.samples.every((sample, index, samples) =>
      isRecord(sample) && typeof sample.step === 'number' && Number.isSafeInteger(sample.step) && sample.step >= 0 &&
      typeof sample.value === 'number' && Number.isFinite(sample.value) && (index === 0 || sample.step > samples[index - 1].step)) &&
    (value.checkedAt === null || (typeof value.checkedAt === 'string' && Number.isFinite(Date.parse(value.checkedAt)))) &&
    typeof value.stale === 'boolean' && value.limitBytes === 65536 && value.maxPoints === 512 &&
    typeof value.possiblyTruncated === 'boolean' && strings(value.truncationReasons) && typeof value.message === 'string' &&
    (value.coverage == null || isRecord(value.coverage))) return value as unknown as LossMetrics;
  return { state: 'error', source: null, samples: [], checkedAt: null, stale: true, limitBytes: 65536, maxPoints: 512,
    possiblyTruncated: true, truncationReasons: [], message: 'Invalid metrics evidence from the server. Lifecycle status remains available.' };
}

export function describeRun(status: RunStatus): {
  label: string;
  detail: string;
  tone: 'neutral' | 'info' | 'warning' | 'error' | 'success';
} {
  switch (status.state?.toLowerCase()) {
    case 'queued':
      return {
        label: status.admitted === false ? 'Waiting for admission' : 'Queued',
        detail: status.admitted === false
          ? 'Kueue has not admitted this run. Check queue capacity and the admission diagnostics.'
          : 'Execution is not running yet. Check admission and pod scheduling below.',
        tone: 'warning'
      };
    case 'running':
      return {
        label: 'Running',
      detail: 'The workload reports an active run. Pod readiness is not a measure of training health.',
        tone: 'info'
      };
    case 'failed':
      return {
        label: 'Failed',
        detail: 'The run stopped with an error. Start with the diagnostics and inspect the affected pod logs.',
        tone: 'error'
      };
    case 'complete':
      return {
        label: 'Finished',
      detail: 'The workload reports completion. Retrieve outputs from the run\'s configured storage.',
        tone: 'success'
      };
    case 'not_submitted':
      return {
        label: 'No such run',
      detail: 'No workload was found. Check the kind, namespace and exact resource name, then check the run again.',
        tone: 'neutral'
      };
    default:
      return {
        label: 'Unreachable',
        detail: 'Run status could not be determined. Check the Jupyter server\'s cluster connection and read permissions, then retry.',
        tone: 'error'
      };
  }
}

export function canWatch(status: RunStatus | null): boolean {
  return Boolean(status?.existing && !status.terminal &&
    ['running', 'queued'].includes(status.state?.toLowerCase() || ''));
}

export interface SubmitPlanSummary {
  workers?: number;
  gpusPerWorker?: number[];
  cpusPerWorker?: string[];
  memoryPerWorker?: string[];
  fileSizes?: Record<string, number>;
  packagedFileSizes?: Record<string, number>;
  decodedBytes?: number;
  payloadBudgetBytes?: number;
  encodedBudgetBytes?: number;
  shutdownAfterFinish?: boolean;
  submissionMode?: string;
  retentionSeconds?: number;
  submitterImage?: string;
  name: string;
  namespace: string;
  queue: string;
  profile: string;
  planDigest: string;
  payloadDigest: string;
  notebookBytes: number;
  preparedBytes: number;
  encodedEnvBytes: number;
  excludedCells: string[];
  /** Files chosen in the review to ship beside the notebook. */
  includedFiles?: string[];
}

export interface SubmitPreview {
  submittable: boolean;
  submissionEnabled: boolean;
  plan: SubmitPlanSummary;
}

export interface SubmitResponse {
  submitted: boolean;
  name: string;
  namespace: string;
  kind: string;
  payloadDigest: string;
  plan: SubmitPlanSummary;
}

export interface NamespaceRow {
  name: string;
  tauEnabled: boolean;
}

export interface NamespaceList {
  namespaces: NamespaceRow[];
  warnings: string[];
}

export function parseNamespaces(value: unknown): NamespaceList {
  if (!isRecord(value) || !Array.isArray(value.namespaces)) {
    throw new Error('invalid namespace list');
  }
  const namespaces = value.namespaces.map(entry => {
    if (!isRecord(entry) || typeof entry.name !== 'string' || typeof entry.tauEnabled !== 'boolean') {
      throw new Error('invalid namespace row');
    }
    return { name: entry.name, tauEnabled: entry.tauEnabled };
  });
  const warnings = Array.isArray(value.warnings) ? value.warnings.filter((item): item is string => typeof item === 'string') : [];
  return { namespaces, warnings };
}

export interface Capabilities {
  submissionEnabled: boolean;
  submissionImplemented: boolean;
  portalUrl: string | null;
}

export function parseCapabilities(value: unknown): Capabilities {
  if (!isRecord(value) || typeof value.submissionEnabled !== 'boolean' || typeof value.submissionImplemented !== 'boolean') {
    throw new Error('The Jupyter server returned invalid capabilities.');
  }
  return { submissionEnabled: value.submissionEnabled, submissionImplemented: value.submissionImplemented, portalUrl: portalLink(value.portalUrl) };
}

export function parseSubmitPreview(value: unknown): SubmitPreview {
  const plan = isRecord(value) ? value.plan : null;
  if (!isRecord(value) || value.submittable !== true || typeof value.submissionEnabled !== 'boolean' || !isRecord(plan) ||
    !['name', 'namespace', 'queue', 'profile', 'payloadDigest'].every(key => typeof plan[key] === 'string' && Boolean(plan[key])) ||
    typeof plan.planDigest !== 'string' || !/^[a-f0-9]{64}$/.test(plan.planDigest) ||
    !['notebookBytes', 'preparedBytes', 'encodedEnvBytes'].every(key => count(plan[key])) || !strings(plan.excludedCells) ||
    !(plan.includedFiles == null || strings(plan.includedFiles)) ||
      !optionalText(plan, ['submissionMode', 'submitterImage']) || !(plan.retentionSeconds == null || count(plan.retentionSeconds)) ||
      !['workers', 'decodedBytes', 'payloadBudgetBytes', 'encodedBudgetBytes'].every(key => plan[key] == null || count(plan[key])) ||
      !(plan.gpusPerWorker == null || (Array.isArray(plan.gpusPerWorker) && plan.gpusPerWorker.every(count))) ||
      !['cpusPerWorker', 'memoryPerWorker'].every(key => plan[key] == null || strings(plan[key])) ||
      !(plan.fileSizes == null || (isRecord(plan.fileSizes) && Object.values(plan.fileSizes).every(count))) ||
      !(plan.packagedFileSizes == null || (isRecord(plan.packagedFileSizes) && Object.values(plan.packagedFileSizes).every(count))) ||
      !(plan.shutdownAfterFinish == null || typeof plan.shutdownAfterFinish === 'boolean')) {
    throw new Error('The Jupyter server returned an invalid submission preview.');
  }
  return value as unknown as SubmitPreview;
}

export function parseSubmitResponse(value: unknown, plan: SubmitPlanSummary): SubmitResponse {
  if (!isRecord(value) || value.submitted !== true || value.name !== plan.name || value.namespace !== plan.namespace ||
    value.kind !== 'RayJob' || value.payloadDigest !== plan.payloadDigest || !isRecord(value.plan) || value.plan.planDigest !== plan.planDigest) {
    throw new Error('The Jupyter server did not confirm the reviewed run. Inspect the planned run before trying again.');
  }
  return value as unknown as SubmitResponse;
}
