// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useState } from 'react';
import { readableQuery, staleReadMessage, useBoard } from '../data';
import { useEvidenceURL } from './evidence-data';
import { launchRows } from './launch-helpers';
import type { RunView, Snapshot } from './types';

export function LaunchSummary({ target, visibleRunIds, onQueryError }: { target: string; visibleRunIds: string[]; onQueryError?: (error: boolean) => void }) {
  const scopedURL = useEvidenceURL();
  // Exactly the same full-snapshot key as ResearchEvidence; the parent mounts
  // this component only when the native disclosure is open.
  const full = readableQuery(useBoard<Snapshot>(scopedURL('/api/v2/stellar/snapshot?' + new URLSearchParams({ target })), visibleRunIds.length > 0));
  const [selected, setSelected] = useState('');
  useEffect(() => {
    onQueryError?.(full.isError);
    return () => onQueryError?.(false);
  }, [full.isError, onQueryError]);
  const runID = visibleRunIds.includes(selected) ? selected : visibleRunIds[0];
  if (!visibleRunIds.length) return <p role="status">Select at least one visible run to inspect launch details.</p>;
  const details = full.data && !['summary', 'metric'].includes(full.data.payload_mode ?? '') ? full.data : undefined;
  const run = details?.runs?.find(item => item.run_id === runID);
  return <div className="stellar-launch-summary">
    <h3>Run launch details</h3>
    <label>Visible run <select aria-label="Run launch details" value={runID} onChange={event => setSelected(event.target.value)}>
      {visibleRunIds.map(id => <option key={id} value={id}>{id}</option>)}
    </select></label>
    <p>{visibleRunIds.length} visible runs · configuration belongs only to the selected run.</p>
    {full.error && <div role="alert" className="warn">Launch details unavailable: {full.error.message}
      {' '}{staleReadMessage(full)} <button type="button" onClick={() => void full.refetch()}>Retry launch details</button></div>}
    {!full.data && !full.error && <p role="status">Loading launch details…</p>}
    {full.data && !details && !full.error && <p role="status">The source returned a compact snapshot. Launch details are unavailable, not evidence of missing configuration.</p>}
    {details?.warnings?.map(warning => <p role="status" className="warn" key={warning}>{warning}</p>)}
    {details && (run ? <RunLaunch key={run.run_id} run={run}/> : <p role="status">This run is outside the detailed snapshot. Open its run target to inspect its launch configuration.</p>)}
  </div>;
}

function RunLaunch({ run }: { run: RunView }) {
  const { rows, source, computedTotal, conflicts, invalidConfig, isRay, units, command } = launchRows(run);
  return <div aria-label={`Launch configuration for ${run.run_id}`}>
    <p><strong>{run.run_id}</strong> · {source}</p>
    <p className="muted">GPU requests are not allocations. Observed GPU count/model is not recorded by this launch contract.
      {isRay && ' Worker counts exclude the CPU-only Ray head; auxiliary CPU workers are listed separately when recorded.'}
      {computedTotal && ` Total is calculated from recorded workers × ${units} per worker.`}</p>
    {conflicts.length > 0 && <p className="warn" role="status">Conflicting config values: {conflicts.join(', ')}. Ambiguous fields are not shown.</p>}
    {invalidConfig && <p className="warn" role="status">A recorded config could not be read as JSON.</p>}
    <dl className="stellar-launch-fields">{rows.map(([name, value]) => <div key={name}><dt>{name}</dt><dd>{value ?? 'Not recorded'}</dd></div>)}</dl>
    <dl><div className="stellar-launch-command"><dt>Stored Tau launch command</dt><dd><code>{command}</code></dd></div></dl>
    <p className="muted">The Tau CLI records a normalized launch command, not the original shell invocation. It may omit flags.
      The workload entrypoint above is a path, not that invocation or a reconstructed workload command.</p>
  </div>;
}
