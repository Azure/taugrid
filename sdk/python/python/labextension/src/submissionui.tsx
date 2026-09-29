// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { RunTarget, SubmitPlanSummary } from './model';
import { SubmissionFailure, SubmissionState } from './submission';

export interface FileList {
  files: { name: string; size: number }[];
  warnings: string[];
  budgetBytes?: number;
}

export function parseFileList(value: unknown): FileList {
  if (typeof value === 'object' && value !== null && 'readable' in value && value.readable === false) {
    throw new Error('The notebook directory is unreadable. Check the path and file permissions, then refresh files.');
  }
  const record = (entry: unknown): entry is Record<string, unknown> => typeof entry === 'object' && entry !== null && !Array.isArray(entry);
  if (!record(value) || !Array.isArray(value.files) || value.files.length > 200 ||
      !value.files.every(entry => record(entry) && typeof entry.name === 'string' &&
        typeof entry.size === 'number' && Number.isSafeInteger(entry.size) && entry.size >= 0)) throw new Error('Invalid file list. Retry files or ask the operator to update the extension.');
  return { files: value.files as FileList['files'], warnings: Array.isArray(value.warnings) ? value.warnings.filter((item): item is string => typeof item === 'string') : [],
    budgetBytes: typeof value.budgetBytes === 'number' ? value.budgetBytes : undefined };
}

export function possibleImports(notebook: string): string[] {
  try {
    const document = JSON.parse(notebook);
    const modules = new Set<string>();
    for (const cell of document.cells || []) {
      if (cell.cell_type !== 'code') continue;
      const source = Array.isArray(cell.source) ? cell.source.join('') : cell.source;
      if (typeof source !== 'string') continue;
      for (const match of source.matchAll(/^\s*(?:from|import)\s+([A-Za-z_]\w*)/gm)) modules.add(`${match[1]}.py`);
    }
    return [...modules];
  } catch { return []; }
}

export function bytes(value: number | undefined): string {
  return value === undefined ? 'Not reported' : `${value.toLocaleString('en-US')} B`;
}

export function FailureNotice({ failure }: { failure: SubmissionFailure }): JSX.Element {
  return <div className="taugrid-error" role="alert"><strong>{failure.title}</strong><p>{failure.action}</p>
    <details className="taugrid-details"><summary>Technical details</summary><p>{failure.detail}</p></details></div>;
}

export function SubmissionReceipt({ plan, notebookName }: { plan: SubmitPlanSummary; notebookName: string }): JSX.Element {
  const gpu = plan.gpusPerWorker?.some(value => value > 0);
  return <section className="taugrid-submit-receipt" data-testid="taugrid-submit-receipt" aria-label="Reviewed execution receipt">
    <h2>Review what will run</h2><p className="taugrid-run-identity"><strong>{plan.namespace}/{plan.name}</strong></p>
    <p>Profile <strong>{plan.profile}</strong> in queue <strong>{plan.queue}</strong>. Queue admission and start time are not guaranteed.</p>
    <dl className="taugrid-plan">
      {Object.entries({ Workers: plan.workers ?? 'Not reported', 'GPUs per worker': plan.gpusPerWorker?.join(', ') ?? 'Not reported',
        'CPUs per worker': plan.cpusPerWorker?.join(', ') ?? 'Not reported', 'Memory per worker': plan.memoryPerWorker?.join(', ') ?? 'Not reported' }).map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}
    </dl>
    {gpu && <p className="taugrid-submit-warning">GPU availability has not been checked. This run may remain queued until matching capacity is available.</p>}
    <h3>Files in this snapshot</h3>
    <p>{notebookName}: {bytes(plan.notebookBytes)} input; {bytes(plan.preparedBytes)} prepared notebook. The package also includes generated runner and context files.</p>
    <ul className="taugrid-package-list">{Object.entries(plan.packagedFileSizes || Object.fromEntries((plan.includedFiles || []).map(name => [name, plan.fileSizes?.[name]]))).map(([name, size]) =>
      <li key={name}><span>{name}{plan.includedFiles?.includes(name) ? '' : ' (prepared / generated)'}</span><strong>{bytes(size)}</strong></li>)}</ul>
    <p>Prepared package: <strong>{bytes(plan.decodedBytes)}</strong> of {bytes(plan.payloadBudgetBytes)} decoded budget.
      Encoded environment: <strong>{bytes(plan.encodedEnvBytes)}</strong> of {bytes(plan.encodedBudgetBytes)} encoded budget. These are separate limits, not estimates.</p>
    <p>Excluded cells: {plan.excludedCells.length ? plan.excludedCells.join(', ') : 'None'}.</p>
    <h3>Execution and cleanup</h3>
    <p>{plan.submissionMode === 'K8sJobMode' ? 'A Kubernetes submitter Job sends the notebook to Ray and forwards the remote driver logs. The driver runs on the Ray head; workers provide the selected compute.' : `Submission mode: ${plan.submissionMode || 'Not reported'}. Ask the operator about its execution behavior.`}</p>
    <p>{plan.shutdownAfterFinish === true ? 'The Ray cluster is shut down after the job finishes.' : 'Cluster shutdown behavior is not confirmed; check with the operator.'} RayJob retention after completion: {plan.retentionSeconds === undefined ? 'Not reported' : `${plan.retentionSeconds} seconds`}. Pod logs may disappear during cleanup; export logs or results you need to keep.</p>
    <p>Runtime image: <code>{plan.submitterImage || 'Not reported'}</code></p>
    <details className="taugrid-details"><summary>Snapshot identity</summary><dl><dt>Plan digest</dt><dd>{plan.planDigest}</dd><dt>Payload digest</dt><dd>{plan.payloadDigest}</dd></dl></details>
  </section>;
}

export function SubmissionOutcome({ state, onOpenRun, onAcknowledge }: {
  state: SubmissionState; onOpenRun: (target: RunTarget) => void; onAcknowledge: () => void;
}): JSX.Element | null {
  const heading = React.useRef<HTMLHeadingElement>(null);
  const [inspected, setInspected] = React.useState(false);
  React.useEffect(() => { heading.current?.focus(); setInspected(false); }, [state.submitted, state.uncertain]);
  if (state.submitted) return <section className="taugrid-submit-receipt" data-testid="taugrid-submitted">
    <h2 tabIndex={-1} ref={heading}>Notebook submitted</h2><p className="taugrid-run-identity"><strong>{state.submitted.namespace}/{state.submitted.name}</strong></p>
    <p>The server confirmed creation, not admission or execution. Open the run to follow queue status, loss evidence, and logs.</p>
    <button className="taugrid-button taugrid-primary" data-testid="taugrid-open-submitted-run" onClick={() => onOpenRun(state.submitted!)}>Open submitted run</button></section>;
  if (state.uncertain && state.attempted) return <section className="taugrid-submit-receipt" data-testid="taugrid-submit-uncertain">
    <h2 tabIndex={-1} ref={heading}>Inspect before trying again</h2><p className="taugrid-run-identity"><strong>{state.attempted.namespace}/{state.attempted.name}</strong></p>
    <p>The workload may already exist. No automatic retry will run. If the run exists, follow it instead of submitting again. Closing this tab does not cancel submission.</p>
    <div className="taugrid-submit-actions"><button className="taugrid-button taugrid-primary" onClick={() => { onOpenRun(state.attempted!); setInspected(true); }}>Inspect planned run</button>
      <button className="taugrid-button" disabled={!inspected} onClick={onAcknowledge}>I checked; allow another review</button></div>
    <p className="taugrid-muted">If inspection fails or reports not found while the request may still be in flight, wait and check again. This acknowledgement is not proof that no run exists.</p></section>;
  return null;
}

export function SubmissionStatus({ state, confirming, notice = '' }: { state: SubmissionState; confirming: boolean; notice?: string }): JSX.Element {
  const step = state.submitted || state.uncertain || state.attempted || confirming || state.preview ? 3 : state.busy ? 2 : 1;
  const message = state.submitted ? 'Notebook submitted' : state.uncertain ? 'Submission outcome not confirmed. Inspect the planned run.' : confirming ? 'Awaiting confirmation. Nothing has been created.' :
    state.busy ? state.attempted ? 'Submitting notebook. Waiting for the server; do not submit again.' : 'Reviewing submission. Packaging a snapshot and resolving its plan; nothing is created.' :
      state.preview ? notice.startsWith('Submission cancelled') ? notice : 'Review ready. Nothing has been created. Edits in the notebook since this review are not included.' : notice || 'Choose a destination, then review. Nothing has been created.';
  return <><ol className="taugrid-submit-steps" aria-label="Submission steps">
    {['Choose destination', 'Review what will run', 'Submit notebook'].map((label, index) => <li key={label} aria-current={step === index + 1 ? 'step' : undefined}><span>{index + 1}</span>{label}</li>)}
  </ol><p className="taugrid-submit-status" data-testid="taugrid-submit-status" role="status" aria-live="polite">{message}</p></>;
}
