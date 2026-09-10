// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import type {
  EvidenceArtifact, EvidenceCollections, EvidenceConfig, EvidenceIdentity, EvidenceRun,
  EvidenceRuntimeDiff, EvidenceSnapshot, MediaKind,
} from './evidence-types';

export function evidenceID(item: EvidenceIdentity): string {
  return item.artifact_id || (item.config_hash ? JSON.stringify([item.run_id ?? '', item.config_hash]) : '')
    || item.event_id || item.observation_id || JSON.stringify(item);
}

export function mergeEvidence<K extends keyof EvidenceCollections>(
  snapshot: EvidenceSnapshot, key: K, visibleRunIds: string[],
): NonNullable<EvidenceCollections[K]> {
  // Build ownership before filtering: an unscoped top-level duplicate must not
  // accidentally expose an artifact belonging only to a hidden run.
  const selected = new Set(visibleRunIds);
  const owners = new Map<string, Set<string>>();
  const ownershipID = (item: EvidenceIdentity) => item.config_hash || evidenceID(item);
  const runs = snapshot.runs ?? [];
  for (const item of snapshot[key] ?? []) {
    if (!item.run_id) continue;
    const ids = owners.get(ownershipID(item)) ?? new Set<string>();
    ids.add(item.run_id);
    owners.set(ownershipID(item), ids);
  }
  for (const run of runs) {
    for (const item of run[key] ?? []) {
      const ids = owners.get(ownershipID(item)) ?? new Set<string>();
      ids.add(item.run_id || run.run_id);
      owners.set(ownershipID(item), ids);
    }
  }
  const groups = new Set(runs.filter(run => selected.has(run.run_id)).map(run => run.run_group_id));
  const include = (item: EvidenceIdentity, parent?: string) => {
    if (!selected.size) return false;
    if (item.run_id) return selected.has(item.run_id);
    if (item.scope_type === 'run' || (!item.scope_type && item.scope_id)) return selected.has(item.scope_id ?? '');
    if (item.scope_type === 'run_group') return groups.has(item.scope_id);
    if (parent) return selected.has(parent);
    const ids = owners.get(ownershipID(item));
    return !ids?.size || [...ids].some(id => selected.has(id));
  };
  // The overload-free collection helper keeps each wire record type intact.
  const collect = <T extends EvidenceIdentity>(top: T[], perRun: { run: EvidenceRun; items: T[] }[]): T[] => {
    const result = new Map<string, T>();
    for (const item of top) {
      if (!include(item)) continue;
      const ids = [...(owners.get(ownershipID(item)) ?? [])].filter(id => selected.has(id));
      const inferred = !item.run_id && ids.length
        ? (key === 'configs' ? ids : ids.slice(0, 1)).map(run_id => ({ ...item, run_id })) : [item];
      for (const record of inferred) result.set(evidenceID(record), record);
    }
    for (const { run, items } of perRun) {
      if (!selected.has(run.run_id)) continue;
      for (const item of items) {
        if (!include(item, run.run_id)) continue;
        const id = evidenceID({ ...item, run_id: item.run_id || run.run_id });
        result.set(id, { ...item, run_id: item.run_id || run.run_id, ...result.get(id) });
      }
    }
    return [...result.values()].map(item => key === 'artifacts' && (owners.get(ownershipID(item))?.size ?? 0) > 1
      ? { ...item, run_id: undefined } : item);
  };
  const collections: Required<EvidenceCollections> = {
    artifacts: [], configs: [], events: [], observations: [],
  };
  if (key === 'artifacts') collections.artifacts = collect(snapshot.artifacts ?? [], runs.map(run => ({ run, items: run.artifacts ?? [] })));
  if (key === 'configs') collections.configs = collect(snapshot.configs ?? [], runs.map(run => ({ run, items: run.configs ?? [] })));
  if (key === 'events') collections.events = collect(snapshot.events ?? [], runs.map(run => ({ run, items: run.events ?? [] })));
  if (key === 'observations') collections.observations = collect(snapshot.observations ?? [], runs.map(run => ({ run, items: run.observations ?? [] })));
  return collections[key];
}

export function artifactText(item: EvidenceArtifact): string {
  return [item.type, item.content_type, item.name, item.uri, item.preview, item.external_ref, item.caption, item.artifact_id].join(' ');
}

export function mediaKind(item: EvidenceArtifact): MediaKind {
  const value = artifactText(item).toLowerCase();
  if (/html|report/i.test([item.type, item.content_type].join(' ')) ||
    [item.uri, item.external_ref, item.preview].some(path => /\.html?(?:[?#]|$)/i.test(path || ''))) return 'report';
  if (item.table) return 'table';
  if (/checkpoint|weights|tensorboard|event file/.test(value)) return 'artifact';
  if (/video|\.mp4\b|\.webm\b|\.mov\b|rollout/.test(value)) return 'video';
  if (/audio|\.wav\b|\.mp3\b|\.ogg\b|\.flac\b/.test(value)) return 'audio';
  if (/image|plot|chart|graph|frame|heatmap|gradcam|\.png\b|\.jpe?g\b|\.gif\b|\.webp\b|\.svg\b|\.avif\b/.test(value)) return 'image';
  if (/text|caption|vqa|question|answer|\.txt\b|\.json\b|\.csv\b/.test(value)) return 'text';
  if (/html|report|\.htm\b/.test(value)) return 'report';
  return 'artifact';
}

export function mediaStep(item: EvidenceArtifact): string {
  const explicit = item.step ?? item.global_step ?? item.training_step ?? item.table?.step;
  if (explicit !== undefined && explicit !== '') return String(explicit);
  return artifactText(item).match(/(?:^|[^a-z])(?:global[-_ ]?step|step)[-_/=: ]*(\d+)\b/i)?.[1] ?? '';
}

export function mediaTag(item: EvidenceArtifact): string {
  const explicit = item.tag || item.summary_tag || item.media_tag;
  if (explicit) return explicit;
  return (item.name || item.uri || item.external_ref || item.artifact_id)
    .replace(/(?:global[-_ ]?step|step)[-_/=: ]*\d+\b/ig, '')
    .replace(/\.(png|jpe?g|gif|webp|svg|avif|mp4|webm|mov|wav|mp3|txt|csv|json|html)$/i, '')
    .replace(/[-_/ ]+$/g, '').trim() || 'Output media';
}

export function isSupportedArtifact(item: EvidenceArtifact): boolean {
  return mediaKind(item) !== 'report';
}

export function artifactBytesPath(artifact: EvidenceArtifact): string | undefined {
  const target = artifact.run_id?.trim();
  if (!target || !artifact.artifact_id.trim()) return undefined;
  return '/api/v2/stellar/artifact?' + new URLSearchParams({ target, artifact: artifact.artifact_id });
}

export function evidenceCoverage(snapshot: EvidenceSnapshot, visibleRunIds: string[]) {
  const included = new Set((snapshot.runs ?? []).map(run => run.run_id));
  return {
    covered: visibleRunIds.filter(id => included.has(id)),
    missing: visibleRunIds.filter(id => !included.has(id)),
  };
}

export function configText(config: EvidenceConfig): string {
  for (const value of [config.normalized_json, config.indexed_fields]) {
    if (!value) continue;
    try { return JSON.stringify(JSON.parse(value), null, 2); } catch { /* Try the alternate normalized representation. */ }
  }
  return config.normalized_json || config.indexed_fields || config.uri || config.config_hash;
}

export function cellText(value: unknown): string {
  if (value === undefined || value === null) return '—';
  return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

export function collectedSystems(run: EvidenceRun) {
  return (run.systems ?? []).filter(field => field.collection_state !== 'not_collected' &&
    field.value !== undefined && field.value !== '' && field.value !== 'not collected');
}

export function runtimeDiffs(snapshot: EvidenceSnapshot, visibleRunIds: string[]): EvidenceRuntimeDiff[] {
  const selected = new Set(visibleRunIds);
  const runs = (snapshot.runs ?? []).filter(run => selected.has(run.run_id));
  if (runs.length < 2) return [];
  // Server comparisons may aggregate hidden siblings in the same group. Use
  // those aggregates only when every snapshot run is visible.
  if (runs.length === snapshot.runs?.length && snapshot.compare?.runtime_diffs?.length) return snapshot.compare.runtime_diffs;
  const fields = new Map<string, EvidenceRuntimeDiff['values']>();
  for (const run of runs) {
    const configs = mergeEvidence(snapshot, 'configs', [run.run_id]).filter(config => config.run_id === run.run_id);
    const values = [
      { name: 'State', value: run.lifecycle_state || run.state },
      { name: 'Code SHA', value: run.code_sha },
      { name: 'Image digest', value: run.image_digest },
      ...collectedSystems(run),
      ...configs.map(config => ({ name: 'Config', value: configText(config) })),
    ];
    for (const field of values) {
      if (!field.value) continue;
      const rows = fields.get(field.name) ?? [];
      rows.push({ run_group_id: run.run_id, value: field.value });
      fields.set(field.name, rows);
    }
  }
  return [...fields].filter(([, values]) => new Set(values.map(value => value.value)).size > 1)
    .map(([field, values]) => ({ field, values }));
}

const medicalLabels = ['Atelectasis', 'Cardiomegaly', 'Consolidation', 'Edema', 'Pleural Effusion', 'Pneumonia', 'Pneumothorax', 'No Finding'];
export function metricLabel(name: string): string | undefined {
  const normalized = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  return medicalLabels.find(label => normalized.includes(label.toLowerCase().replace(/[^a-z0-9]/g, '')));
}

export function labelGroups(snapshot: EvidenceSnapshot) {
  return medicalLabels.map(label => ({
    label,
    metrics: (snapshot.metric_options ?? []).map(option => option.name)
      .filter(name => /^(eval|final|test|val|valid)\//i.test(name) && metricLabel(name) === label)
      .sort((a, b) => metricPriority(a) - metricPriority(b) || a.localeCompare(b)).slice(0, 3),
  })).filter(group => group.metrics.length).slice(0, 5);
}

function metricPriority(name: string): number {
  const index = ['auprc', 'auroc', 'f1', 'precision', 'recall', 'brier', 'ece'].findIndex(token => name.toLowerCase().includes(token));
  return index < 0 ? 99 : index;
}

export function errorMetricNames(snapshot: EvidenceSnapshot): string[] {
  return [...new Set((snapshot.metric_options ?? []).map(option => option.name))]
    .filter(name => /^detect\//i.test(name) || /(false_positive|false_negative|precision|recall|threshold|calibration|brier|ece|worst)/i.test(name))
    .sort((a, b) => Number(!/^detect\/macro_/.test(a)) - Number(!/^detect\/macro_/.test(b)) || metricPriority(a) - metricPriority(b) || a.localeCompare(b))
    .slice(0, 8);
}

export function metricGoal(name: string, goal?: string): 'minimize' | 'maximize' {
  if (goal === 'minimize' || goal === 'maximize') return goal;
  return /loss|perplexity|cross_entropy|nll|brier|ece|error|false_positive|false_negative|latency|duration/i.test(name) ? 'minimize' : 'maximize';
}

export function metricSignals(snapshot: EvidenceSnapshot, name: string, visibleRunIds: string[], goal?: string) {
  const selected = new Set(visibleRunIds);
  if (snapshot.chart?.metric_name !== name) return [];
  const minimize = metricGoal(name, goal) === 'minimize';
  return (snapshot.chart.series ?? []).filter(series => selected.has(series.run_id)).map(series => {
    const points = (series.values ?? []).filter((point): point is { step: number; value: number } =>
      typeof point.value === 'number' && Number.isFinite(point.value) && Number.isFinite(point.step));
    const latest = points.reduce<(typeof points)[number] | undefined>((best, point) => !best || point.step >= best.step ? point : best, undefined);
    const best = points.reduce<(typeof points)[number] | undefined>((best, point) =>
      !best || (minimize ? point.value < best.value : point.value > best.value) ? point : best, undefined);
    return { run: series.run_id, latest, best, sampled: series.decimated === true };
  });
}
