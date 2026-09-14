// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import { APIError, experimentPageURL, readableQuery, requestRejected, staleReadMessage, useBoard, useWorkspace } from '../data';
import { useEvidenceURL } from './evidence-data';
import {
  artifactBytesPath, artifactText, cellText, collectedSystems, configText, errorMetricNames,
  evidenceCoverage, evidenceID, isSupportedArtifact, labelGroups, mediaKind, mediaStep, mediaTag, mergeEvidence, metricGoal,
  metricSignals, runtimeDiffs,
} from './evidence-helpers';
import type { EvidenceArtifact, EvidenceIdentity, EvidenceSection, EvidenceSnapshot, MediaKind } from './evidence-types';
import './evidence.css';

export interface ResearchEvidenceProps {
  target: string;
  visibleRunIds: string[];
  sections?: string[];
  heading?: { id: string; title: string; subtitle?: string; hidden?: boolean };
  initiallyExpanded?: boolean;
  dashboard?: boolean;
}

const sectionTitles: Record<EvidenceSection, string> = {
  media: 'Output media', errors: 'Error analysis', labels: 'Per-label validation',
  repro: 'Reproducibility', evidence: 'Evidence browser',
};
const sectionOrder: EvidenceSection[] = ['media', 'errors', 'labels', 'repro', 'evidence'];

export function ResearchEvidence(props: ResearchEvidenceProps) {
  const { scope, managed } = useWorkspace();
  const [params] = useSearchParams();
  return <EvidenceWorkspace key={JSON.stringify([scope, managed, props.target, params.get('project')])} {...props} />;
}

function EvidenceWorkspace({ target, visibleRunIds, sections, heading, initiallyExpanded, dashboard }: ResearchEvidenceProps) {
  const element = useRef<HTMLDivElement>(null);
  const [inView, setInView] = useState(!dashboard || typeof IntersectionObserver === 'undefined');
  useEffect(() => {
    if (inView || !element.current) return;
    const observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) { setInView(true); observer.disconnect(); }
    }, { rootMargin: '300px' });
    observer.observe(element.current);
    return () => observer.disconnect();
  }, [inView]);
  const scopedURL = useEvidenceURL();
  const [expanded, setExpanded] = useState<EvidenceSection[]>(() => initiallyExpanded ? sectionOrder.filter(section => sections?.includes(section)) : []);
  const [requested, setRequested] = useState(false);
  const active = sectionOrder.filter(section => sections === undefined || sections.includes(section));
  const customHeading = active.length === 1 ? heading : undefined;
  const opened = expanded.filter(section => active.includes(section));
  const hasRuns = visibleRunIds.length > 0;
  const needsDetails = requested || opened.some(section => ['media', 'repro', 'evidence'].includes(section));
  const path = scopedURL('/api/v2/stellar/snapshot?' + new URLSearchParams({ target }));
  const full = readableQuery(useBoard<EvidenceSnapshot>(path, !!target && hasRuns && needsDetails && inView));
  const details = full.data && !['summary', 'metric'].includes(full.data.payload_mode ?? '') ? full.data : undefined;
  const needsMetrics = opened.includes('errors') || opened.includes('labels');
  const summary = readableQuery(useBoard<EvidenceSnapshot>(path + '&mode=summary', !!target && hasRuns && needsMetrics && !details && !requestRejected(full.error) && inView));
  const metricSource = requestRejected(full.error) ? undefined : details || summary.data;
  const coverage = details ? evidenceCoverage(details, visibleRunIds) : undefined;
  const metricCoverage = metricSource ? evidenceCoverage(metricSource, visibleRunIds) : undefined;
  function toggle(section: EvidenceSection) {
    setExpanded(previous => previous.includes(section) ? previous.filter(value => value !== section) : [...previous, section]);
  }
  if (!active.length) return null;
  return <div className={'stellar-evidence' + (dashboard ? ' stellar-dashboard-evidence' : '')} ref={element}>
    {!hasRuns && <p role="status">Select at least one run to inspect evidence. Unscoped evidence is hidden while the run selection is empty.</p>}
    {active.map(section => <EvidencePanel key={section} title={customHeading?.title ?? sectionTitles[section]}
      headingID={customHeading?.id} subtitle={customHeading?.subtitle}
      staticHeading={dashboard} hiddenHeading={customHeading?.hidden}
      expanded={opened.includes(section) && hasRuns} disabled={!hasRuns || !target} onToggle={() => toggle(section)}>
      {section === 'errors' || section === 'labels'
        ? <>
          {!metricSource && summary.isPending && <p role="status">Loading metric catalog…</p>}
          {summary.error && <EvidenceError error={summary.error} retained={staleReadMessage(summary)} retry={() => void summary.refetch()} />}
          {full.error && <EvidenceError error={full.error} retained={staleReadMessage(full)} retry={() => void full.refetch()} />}
          {metricCoverage && <IncompleteEvidence missing={metricCoverage.missing} covered={metricCoverage.covered.length} section={section} />}
          {metricSource && (section === 'labels'
            ? <LabelQuality target={target} snapshot={metricSource} visibleRunIds={visibleRunIds} />
            : <ErrorAnalysis target={target} snapshot={metricSource} visibleRunIds={visibleRunIds} details={details}
              unavailable={full.isError || !!full.data && !details} loading={full.isFetching} load={() => setRequested(true)} />)}
        </>
        : <>
          {!details && !full.isError && <p role="status">{full.isFetching ? 'Loading evidence details…' : 'Evidence details are deferred.'}</p>}
          {full.error && <EvidenceError error={full.error} retained={staleReadMessage(full)} retry={() => void full.refetch()} />}
          {!details && full.data && !full.isError && <p>The source returned a compact snapshot. Detailed evidence is unavailable; this is not evidence of missing predictions or a zero-result run.</p>}
          {coverage && <IncompleteEvidence missing={coverage.missing} covered={coverage.covered.length} section={section} />}
          {details && coverage && coverage.covered.length > 0 && (section === 'media'
            ? <OutputMedia key={JSON.stringify(visibleRunIds)} target={target} snapshot={details} visibleRunIds={coverage.covered} incomplete={coverage.missing.length > 0} />
            : section === 'repro'
              ? <Reproducibility snapshot={details} visibleRunIds={coverage.covered} incomplete={coverage.missing.length > 0} />
              : <EvidenceBrowser snapshot={details} visibleRunIds={coverage.covered} incomplete={coverage.missing.length > 0} />)}
        </>}
    </EvidencePanel>)}
  </div>;
}

function IncompleteEvidence({ missing, covered, section }: { missing: string[]; covered: number; section: EvidenceSection }) {
  const { scope, managed } = useWorkspace();
  const [params] = useSearchParams();
  if (!missing.length) return null;
  return <div className="stellar-evidence-notice" role="status">
    <h3>Incomplete evidence</h3>
    <p>This snapshot covers {covered} of {covered + missing.length} selected runs. Evidence for the other runs was not loaded;
      absence cannot be inferred. Any records below cover only the included runs. Open an exact run to inspect its evidence.</p>
    <ul>{missing.map(run => {
      const search = new URLSearchParams({ target: run, sections: section });
      const project = params.get('project');
      if (project) search.set('project', project);
      return <li key={run}><Link to={experimentPageURL(scope, managed, search.toString())!}>Inspect {run}</Link></li>;
    })}</ul>
  </div>;
}

function EvidencePanel({ title, headingID, subtitle, expanded, disabled, onToggle, children, staticHeading, hiddenHeading }: {
  title: string; expanded: boolean; disabled: boolean; onToggle: () => void; children: ReactNode;
  headingID?: string; subtitle?: string;
  staticHeading?: boolean; hiddenHeading?: boolean;
}) {
  const id = useId();
  return <section className="stellar-evidence-section">
    {staticHeading ? <h2 id={headingID} className={hiddenHeading ? 'stellar-sr-only' : undefined}>{title}</h2> : <h2 id={headingID}><button type="button" aria-expanded={expanded} aria-controls={id} disabled={disabled} onClick={onToggle}>
      <span>{title}</span><span aria-hidden="true">{expanded ? '−' : '+'}</span>
    </button></h2>}
    {subtitle && <p className="stellar-evidence-subtitle">{subtitle}</p>}
    {expanded ? <div id={id} className="stellar-evidence-body">{children}</div>
      : <p id={id} className="stellar-evidence-deferred">Details deferred. Expand to inspect the selected runs.</p>}
  </section>;
}

function EvidenceError({ error, retry, retained = '' }: { error: Error; retry: () => void; retained?: string }) {
  const state = error instanceof APIError && error.status === 403 ? 'Forbidden' : 'Unavailable';
  return <div className="stellar-evidence-error" role="alert">
    <strong>{state}.</strong> {error.message} No alternate source was used. {retained}
    <button type="button" onClick={retry}>Retry</button>
  </div>;
}

function TablePreview({ artifact }: { artifact: EvidenceArtifact }) {
  const table = artifact.table;
  const columns = table?.columns?.slice(0, 8) ?? [];
  const rows = table?.rows ?? [];
  if (!columns.length) return <p>No indexed table columns are available.</p>;
  return <div className="stellar-evidence-table-scroll">
    <table>
      <caption>{table?.caption || artifact.caption || artifact.name || 'Artifact table'} — {rows.length} indexed rows</caption>
      <thead><tr>{columns.map(column => <th key={column} scope="col">{column}</th>)}</tr></thead>
      <tbody>{rows.slice(0, 20).map((row, index) => <tr key={index}>{columns.map(column =>
        <td key={column}>{cellText(row[column])}</td>)}</tr>)}</tbody>
    </table>
    {(rows.length > 20 || (table?.columns?.length ?? 0) > columns.length) && <p>Preview shows up to 20 indexed rows and 8 columns.</p>}
  </div>;
}

async function fetchArtifact(url: string, kind: MediaKind, signal: AbortSignal): Promise<Blob> {
  const response = await fetch(url, { signal, credentials: 'same-origin', redirect: 'error' });
  if (!response.ok) {
    let detail = (await response.text()).slice(0, 1000);
    try {
      const body: unknown = JSON.parse(detail);
      if (body && typeof body === 'object' && 'reason' in body && typeof body.reason === 'string') detail = body.reason;
    } catch (error) {
      // Proxy error bodies may be plain text; other failures must stay visible.
      if (!(error instanceof SyntaxError)) throw error;
    }
    throw new APIError(response.status, '', detail || 'Artifact could not be loaded');
  }
  const type = (response.headers.get('content-type') || '').split(';')[0].trim().toLowerCase();
  const valid = kind === 'image' ? /^image\/(png|jpeg|gif|webp|avif|bmp)$/.test(type)
    : kind === 'video' ? /^video\//.test(type)
      : kind === 'audio' ? /^audio\//.test(type)
        : ['text/plain', 'text/csv', 'application/json', 'application/x-ndjson'].includes(type);
  if (!valid) throw new Error(`Artifact preview unavailable for content type ${type || 'unknown'}. HTML and scripts are never rendered inline.`);
  const maxBytes = kind === 'text' ? 1024 * 1024 : 64 * 1024 * 1024;
  const reader = response.body?.getReader();
  if (!reader) throw new Error('Artifact response has no readable content.');
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > maxBytes) {
        await reader.cancel();
        throw new Error('Artifact exceeds the native preview size limit. Use its authorized download instead.');
      }
      chunks.push(new Uint8Array(value));
    }
  } finally { reader.releaseLock(); }
  return new Blob(chunks, { type });
}

function NativePreview({ artifact }: { artifact: EvidenceArtifact }) {
  const scopedURL = useEvidenceURL();
  const { scope, managed } = useWorkspace();
  const kind = mediaKind(artifact);
  const bytesPath = artifactBytesPath(artifact);
  const path = bytesPath ? scopedURL(bytesPath) : undefined;
  const previewable = ['image', 'video', 'audio', 'text'].includes(kind) && !!path;
  const query = readableQuery(useQuery({
    queryKey: ['stellar-artifact', scope, managed, artifact.run_id, artifact.artifact_id, kind, path],
    queryFn: ({ signal }) => {
      if (!path) throw new Error('Artifact unavailable: the owning run is missing or ambiguous.');
      return fetchArtifact(path, kind, signal);
    },
    enabled: previewable, retry: false, staleTime: 60_000,
  }));
  const [resource, setResource] = useState<{ blob: Blob; url: string; text?: string }>();
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    const blob = query.data;
    if (!blob) { setResource(undefined); return; }
    let active = true;
    setFailed(false);
    if (kind === 'text') {
      void blob.text().then(text => { if (active) setResource({ blob, url: '', text }); });
      return () => { active = false; };
    }
    const url = URL.createObjectURL(blob);
    setResource({ blob, url });
    return () => { active = false; URL.revokeObjectURL(url); };
  }, [query.data, kind]);
  if (artifact.table) return <TablePreview artifact={artifact} />;
  if (!previewable) return <p>Native preview unavailable. This artifact is tracked as metadata.</p>;
  const queryError = query.error ? <EvidenceError error={query.error} retained={staleReadMessage(query)} retry={() => { setFailed(false); void query.refetch(); }} /> : null;
  if (query.error && !query.data) return queryError;
  if (!resource || resource.blob !== query.data) return <>{queryError}<p role="status">Loading {kind} preview…</p></>;
  if (failed) return <div role="alert">The {kind} could not be decoded.
    <button type="button" onClick={() => { setFailed(false); void query.refetch(); }}>Retry preview</button></div>;
  const label = artifact.caption || artifact.name || artifact.artifact_id;
  const preview = kind === 'text' ? <pre className="stellar-evidence-text">{resource.text}</pre>
    : kind === 'image' ? <img className="stellar-evidence-media" src={resource.url} alt={label} loading="lazy" onError={() => setFailed(true)} />
      : kind === 'video' ? <video className="stellar-evidence-media" src={resource.url} controls preload="metadata" aria-label={label} onError={() => setFailed(true)} />
        : <audio src={resource.url} controls preload="metadata" aria-label={label} onError={() => setFailed(true)} />;
  return <>{queryError}{preview}</>;
}

function ArtifactRecord({ artifact, preview = false }: { artifact: EvidenceArtifact; preview?: boolean }) {
  const scopedURL = useEvidenceURL();
  const kind = mediaKind(artifact);
  const bytesPath = artifactBytesPath(artifact);
  if (!isSupportedArtifact(artifact)) return null;
  return <article className="stellar-evidence-record">
    <h4>{artifact.name || artifact.artifact_id || 'Artifact'}</h4>
    <p className="stellar-evidence-meta">{artifact.run_id || 'Shared evidence'} · {kind}
      {mediaStep(artifact) !== '' && <> · step {mediaStep(artifact)}</>}
      {artifact.created_at && <> · {artifact.created_at}</>}</p>
    {artifact.caption && <p>{artifact.caption}</p>}
    {preview && bytesPath && <div className="stellar-record-preview"><NativePreview key={JSON.stringify([artifact.run_id, artifact.artifact_id])} artifact={artifact} /></div>}
    {!bytesPath && <p role="status">Artifact unavailable: the owning run is missing or ambiguous, or the artifact identity is missing. No preview or download was requested.</p>}
    {!preview && artifact.table && <TablePreview artifact={artifact} />}
    <details className="stellar-artifact-details"><summary>Artifact details</summary><dl className="stellar-evidence-metadata">
      {artifact.uri && <><dt>URI</dt><dd>{artifact.uri}</dd></>}
      {artifact.digest && <><dt>Digest</dt><dd>{artifact.digest}</dd></>}
      {artifact.size_bytes && <><dt>Bytes</dt><dd>{artifact.size_bytes}</dd></>}
    </dl></details>
    {bytesPath && <a href={scopedURL(bytesPath)}
      target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">Open artifact in new tab</a>}
  </article>;
}

function OutputMedia({ target, snapshot, visibleRunIds, incomplete }: { target: string; snapshot: EvidenceSnapshot; visibleRunIds: string[]; incomplete: boolean }) {
  const [params, setParams] = useSearchParams();
  const { scope, managed } = useWorkspace();
  const currentScope = (!params.has('workspace') || params.get('workspace') === scope.workspace) &&
    (!params.has('target') || params.get('target') === target);
  const artifacts = mergeEvidence(snapshot, 'artifacts', visibleRunIds);
  const items = artifacts.filter(isSupportedArtifact).filter(item => mediaKind(item) !== 'artifact');
  const tag = currentScope ? params.get('media_tag') || '' : '';
  const run = currentScope ? params.get('media_run') || '' : '';
  const step = currentScope ? params.get('media_step') || '' : '';
  const tags = [...new Set(items.map(mediaTag))].sort();
  const selectedTag = tags.includes(tag) ? tag : tags[0] || '';
  const tagged = items.filter(item => mediaTag(item) === selectedTag);
  const runs = [...new Set(tagged.map(item => item.run_id).filter((id): id is string => !!id))].sort();
  const selectedRun = runs.includes(run) ? run : '';
  const byRun = tagged.filter(item => !selectedRun || item.run_id === selectedRun);
  const steps = [...new Set(byRun.map(mediaStep).filter(value => value !== ''))].sort((a, b) => Number(a) - Number(b));
  const selectedStep = steps.includes(step) ? step : '';
  const filtered = byRun.filter(item => !selectedStep || mediaStep(item) === selectedStep)
    .sort((a, b) => Number(mediaStep(b)) - Number(mediaStep(a)) || (Date.parse(b.created_at || '') || 0) - (Date.parse(a.created_at || '') || 0));
  const [limit, showMore] = useEvidenceLimit(JSON.stringify([selectedTag, selectedRun, selectedStep, filtered.map(evidenceID)]), 8);
  function select(nextTag: string, nextRun: string, nextStep: string) {
    setParams(previous => {
      const next = new URLSearchParams(previous);
      for (const [key, value] of [['media_tag', nextTag], ['media_run', nextRun], ['media_step', nextStep]]) {
        if (value) next.set(key, value); else next.delete(key);
      }
      next.set('target', target);
      if (managed && scope.workspace) next.set('workspace', scope.workspace);
      return next;
    });
  }
  if (!items.length) return incomplete ? <p>No output media is available in the covered runs.</p>
    : artifacts.length ? <p>No supported media is available for the selected runs. Recorded artifacts may not have a native media view.</p>
    : <p>No output media matched the selected runs. Import images, video, audio, tables, or text artifacts to inspect model outputs.</p>;
  return <>
    <div className="stellar-evidence-counts"><span>{items.length} media</span><span>{tags.length} tags</span><span>{new Set(items.map(item => item.run_id).filter(Boolean)).size} runs</span><span>{steps.length} steps</span></div>
    <div className="stellar-evidence-controls">
      <label>Tag<select value={selectedTag} onChange={event => select(event.target.value, '', '')}>
        {tags.map(value => <option key={value}>{value}</option>)}</select></label>
      <label>Run<select value={selectedRun} onChange={event => select(selectedTag, event.target.value, '')}>
        <option value="">All selected runs / shared</option>{runs.map(value => <option key={value}>{value}</option>)}</select></label>
      <label>Step<select value={selectedStep} onChange={event => select(selectedTag, selectedRun, event.target.value)} disabled={!steps.length}>
        <option value="">{steps.length ? 'All steps, newest first' : 'Step not indexed'}</option>
        {steps.map(value => <option key={value} value={value}>Step {value}</option>)}</select></label>
    </div>
    <div className="stellar-evidence-gallery">{filtered.slice(0, limit).map(artifact =>
      <ArtifactRecord key={evidenceID(artifact)} artifact={artifact} preview />)}</div>
    {filtered.length > 8 && <p role="status">Showing {Math.min(limit, filtered.length)} of {filtered.length} media records.</p>}
    {filtered.length > limit && <button type="button" onClick={showMore}>Show more media</button>}
  </>;
}

function Reproducibility({ snapshot, visibleRunIds, incomplete }: { snapshot: EvidenceSnapshot; visibleRunIds: string[]; incomplete: boolean }) {
  const recordedArtifacts = mergeEvidence(snapshot, 'artifacts', visibleRunIds);
  const artifacts = recordedArtifacts.filter(isSupportedArtifact);
  const configs = mergeEvidence(snapshot, 'configs', visibleRunIds);
  const events = mergeEvidence(snapshot, 'events', visibleRunIds);
  const runs = (snapshot.runs ?? []).filter(run => visibleRunIds.includes(run.run_id));
  const manifests = artifacts.filter(item => /manifest|dataset|data\//i.test(artifactText(item)));
  return <>
    <p>Coverage of the {incomplete ? 'covered' : 'selected'} runs, based on collected records—not a guarantee of reproducibility.</p>
    {!artifacts.length && recordedArtifacts.length > 0 && <p>No supported media is available. Artifacts were recorded, but their formats are not included in these counts.</p>}
    <dl className="stellar-evidence-coverage">
      <div><dt>Configs</dt><dd>{configs.length} normalized config records</dd></div>
      <div><dt>Artifacts</dt><dd>{artifacts.length} artifacts / checkpoints</dd></div>
      <div><dt>Logs / events</dt><dd>{events.length} event records</dd></div>
      <div><dt>Environment</dt><dd>{runs.filter(run => collectedSystems(run).length > 0).length} of {runs.length} runs with collected system fields</dd></div>
      <div><dt>Data manifests</dt><dd>{manifests.length} data / manifest artifacts</dd></div>
    </dl>
    {runs.map(run => <details key={run.run_id}><summary>{run.run_id} — environment</summary>
      <dl className="stellar-evidence-metadata">{collectedSystems(run).map((field, index) =>
        <div key={`${field.name}-${index}`}><dt>{field.name}</dt><dd>{field.value}</dd></div>)}</dl>
      {!collectedSystems(run).length && <p>System context was not collected for this run.</p>}
    </details>)}
  </>;
}

function EvidenceBrowser({ snapshot, visibleRunIds, incomplete }: { snapshot: EvidenceSnapshot; visibleRunIds: string[]; incomplete: boolean }) {
  const configs = mergeEvidence(snapshot, 'configs', visibleRunIds);
  const recordedArtifacts = mergeEvidence(snapshot, 'artifacts', visibleRunIds);
  const artifacts = recordedArtifacts.filter(isSupportedArtifact);
  const events = mergeEvidence(snapshot, 'events', visibleRunIds);
  const observations = mergeEvidence(snapshot, 'observations', visibleRunIds);
  const total = configs.length + artifacts.length + events.length + observations.length;
  const diffs = runtimeDiffs(snapshot, visibleRunIds);
  return <>
    <p>{total} attached records for {visibleRunIds.length} {incomplete ? 'covered' : 'selected'} runs. Counts exclude hidden runs.</p>
    {!total && recordedArtifacts.length > 0 && <p>No supported media is available in this browser. Artifacts were recorded, but their formats are not displayed here.</p>}
    {!total && !recordedArtifacts.length && !incomplete && <div className="stellar-evidence-notice"><h3>Metrics-only evidence</h3>
      <p>No config, artifact, event, or observation rows were collected for this selection. This is not a zero-result experiment.
        Import normalized configs, model outputs, logs, or experiment decisions to populate this browser.</p>
      {snapshot.store_path?.startsWith('kusto://') && <p>Kusto-backed metrics and run identity remain authoritative, read-only source evidence.</p>}
    </div>}
    <h3>Runtime / config differences</h3>
    {diffs.length ? <dl className="stellar-evidence-metadata">{diffs.map(diff => <div key={diff.field}>
      <dt>{diff.field}</dt><dd>{diff.values.map((value, index) => <p key={index}>{value.run_group_id}: {value.value}</p>)}</dd>
    </div>)}</dl> : <p>{incomplete ? 'Runtime/config comparison is incomplete.' : 'No runtime/config differences were detected in the selected run set.'}</p>}
    <RecordList title="Configs / hparams" items={configs} renderItem={config =>
      <details key={evidenceID(config)}><summary>{config.run_id || 'Shared config'} · {config.format || 'config'} · {config.config_hash}</summary>
        <pre className="stellar-evidence-text">{configText(config)}</pre></details>} />
    <RecordList title="Artifacts / checkpoints" items={artifacts} renderItem={artifact =>
      <ArtifactRecord key={evidenceID(artifact)} artifact={artifact} />} />
    <RecordList title="Events / logs" items={events} renderItem={event =>
      <article key={evidenceID(event)} className="stellar-evidence-record">
        <h4>{event.message || event.type || 'Event'}</h4><p>{event.run_id || 'Shared event'} · {event.severity || 'info'} · {event.time || 'Time not recorded'}</p>
        {event.payload && <pre className="stellar-evidence-text">{event.payload}</pre>}
      </article>} />
    <RecordList title="Observations / decisions" items={observations} renderItem={observation =>
      <article key={evidenceID(observation)} className="stellar-evidence-record">
        <h4>{observation.type || 'Observation'}</h4><p>{observation.author || observation.source || 'Author not recorded'} · {observation.scope_type || 'run'}: {observation.scope_id || observation.run_id || 'shared'}</p>
        <p>{observation.text}</p>{observation.evidence && <pre className="stellar-evidence-text">{observation.evidence}</pre>}
      </article>} />
  </>;
}

function useEvidenceLimit(identity: string, pageSize: number): [number, () => void] {
  const [page, setPage] = useState({ identity, limit: pageSize });
  const limit = page.identity === identity ? page.limit : pageSize;
  return [limit, () => setPage({ identity, limit: limit + pageSize })];
}

function RecordList<T extends EvidenceIdentity>({ title, items, renderItem }: {
  title: string; items: T[]; renderItem: (item: T) => ReactNode;
}) {
  const [limit, showMore] = useEvidenceLimit(JSON.stringify(items.map(evidenceID)), 40);
  if (!items.length) return null;
  return <section className="stellar-evidence-record-list"><h3>{title} <span>({items.length})</span></h3>
    {items.slice(0, limit).map(renderItem)}
    {items.length > 40 && <p role="status">Showing {Math.min(limit, items.length)} of {items.length} records.</p>}
    {items.length > limit && <button type="button" onClick={showMore}>Show more {title.toLowerCase()}</button>}
  </section>;
}

function MetricSummary({ target, name, snapshot, visibleRunIds }: {
  target: string; name: string; snapshot: EvidenceSnapshot; visibleRunIds: string[];
}) {
  const scopedURL = useEvidenceURL();
  const available = snapshot.chart?.metric_name === name;
  const query = readableQuery(useBoard<EvidenceSnapshot>(scopedURL('/api/v2/stellar/snapshot?' + new URLSearchParams({ target, mode: 'metric', metric: name })), !available && visibleRunIds.length > 0));
  const source = available ? snapshot : query.data;
  const goal = snapshot.metric_options?.find(option => option.name === name)?.goal;
  const descriptive = /threshold/i.test(name) && !goal;
  const signals = source ? metricSignals(source, name, visibleRunIds, goal) : [];
  const missing = source ? evidenceCoverage({ runs: source.runs ?? source.chart?.series }, visibleRunIds).missing : [];
  return <div className="stellar-evidence-metric">
    <h4>{name}</h4>
    {!available && query.isPending && <p role="status">Loading scalar values…</p>}
    {!available && query.error && <EvidenceError error={query.error} retained={staleReadMessage(query)} retry={() => void query.refetch()} />}
    {!!missing.length && <p role="status">Scalar evidence is incomplete for {missing.length} selected runs. Missing values cannot be inferred from this snapshot.</p>}
    {source && (!signals.some(signal => signal.latest) ? !missing.length && <p>No recorded values for the selected runs.</p>
      : <div className="stellar-evidence-table-scroll"><table>
        <caption>{descriptive ? 'Threshold scalar; no inferred optimization goal' : metricGoal(name, goal) === 'minimize' ? 'Lower is better' : 'Higher is better'} · latest by recorded step</caption>
        <thead><tr><th scope="col">Run</th><th scope="col">Latest</th>{!descriptive && <th scope="col">Best recorded</th>}</tr></thead>
        <tbody>{signals.map(signal => <tr key={signal.run}><th scope="row">{signal.run}</th>
          <td>{signal.latest ? `${signal.latest.value.toLocaleString(undefined, { maximumSignificantDigits: 6 })} · step ${signal.latest.step}` : 'Not recorded'}</td>
          {!descriptive && <td>{signal.best ? `${signal.best.value.toLocaleString(undefined, { maximumSignificantDigits: 6 })} · step ${signal.best.step}` : 'Not recorded'}{signal.sampled && ' (sampled)'}</td>}
        </tr>)}</tbody>
      </table></div>)}
  </div>;
}

function LabelQuality({ target, snapshot, visibleRunIds }: { target: string; snapshot: EvidenceSnapshot; visibleRunIds: string[] }) {
  const groups = labelGroups(snapshot);
  if (!groups.length && evidenceCoverage(snapshot, visibleRunIds).missing.length) return <p>The label metric catalog is incomplete for this selection.</p>;
  if (!groups.length) return <p>No label-scoped validation metrics were recorded. Import per-label AUPRC, AUROC, F1, or calibration metrics to compare up to five medical labels.</p>;
  return <div>{groups.map(group => <section key={group.label} className="stellar-evidence-label"><h3>{group.label}</h3>
    {group.metrics.map(name => <MetricSummary key={name} target={target} name={name} snapshot={snapshot} visibleRunIds={visibleRunIds} />)}
  </section>)}</div>;
}

function ErrorAnalysis({ target, snapshot, visibleRunIds, details, unavailable, loading, load }: {
  target: string; snapshot: EvidenceSnapshot; visibleRunIds: string[]; details?: EvidenceSnapshot; unavailable: boolean; loading: boolean; load: () => void;
}) {
  const metrics = errorMetricNames(snapshot);
  const incomplete = evidenceCoverage(snapshot, visibleRunIds).missing.length > 0;
  const predictionIncomplete = details && evidenceCoverage(details, visibleRunIds).missing.length > 0;
  const recordedPredictions = details ? mergeEvidence(details, 'artifacts', evidenceCoverage(details, visibleRunIds).covered)
    .filter(item => /prediction|false[-_ ]?(positive|negative)|confusion|threshold|calibration|error[-_ ]?analysis|classification[-_ ]?report/i.test(artifactText(item))) : [];
  const predictions = recordedPredictions.filter(isSupportedArtifact);
  return <>
    <h3>Validation / detection summaries</h3>
    {metrics.length ? metrics.map(name => <MetricSummary key={name} target={target} name={name} snapshot={snapshot} visibleRunIds={visibleRunIds} />)
      : incomplete ? <p>The error-analysis metric catalog is incomplete for this selection.</p>
        : <p>No dedicated error-analysis scalars were recorded. Import detect/*, precision, recall, false-positive / false-negative, or calibration metrics.</p>}
    {!details && unavailable ? <p>Prediction evidence is unavailable. Missing prediction imports cannot be inferred for this selection.</p>
      : !details ? <div className="stellar-evidence-notice"><h3>Prediction summary details are deferred</h3>
      <p>Scalar metrics do not establish that prediction-level analyses exist. Load evidence before checking for false-positive / false-negative rows, threshold tables, or confusion views.</p>
      <button type="button" onClick={load} disabled={loading}>{loading ? 'Loading prediction details…' : 'Load prediction details'}</button>
    </div> : predictions.length ? <RecordList title="Prediction summaries" items={predictions}
      renderItem={artifact => <ArtifactRecord key={evidenceID(artifact)} artifact={artifact} />}
    /> : predictionIncomplete ? <p>Prediction evidence is incomplete. Missing prediction imports cannot be inferred for this selection.</p>
      : recordedPredictions.length ? <p>No supported prediction media is available. Prediction artifacts were recorded, but their formats are not displayed here.</p>
      : <div className="stellar-evidence-notice"><h3>Needs prediction summary import</h3>
      <p>No prediction-summary artifacts were found for the selected runs. Import prediction rows, threshold tables, or calibration summaries.
        A confusion matrix cannot be reconstructed from scalar metrics alone.</p></div>}
  </>;
}
