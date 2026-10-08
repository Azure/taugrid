// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { describeRun, portalLink, RunStatus } from './model';
import { LossSection } from './loss';

export function RunDetails({ status, stale, statusUrl }: {
  status: RunStatus;
  stale: boolean;
  statusUrl: string;
}): JSX.Element {
  const state = describeRun(status);
  const unavailable = stale || state.label === 'Unreachable';
  const restarts = status.pods.reduce((total, pod) => total + pod.restarts, 0);
  return (
    <article className={`taugrid-run taugrid-tone-${unavailable ? 'error' : state.tone}`}>
      <header className="taugrid-run-head">
        <div className="taugrid-state-line">
          <span className="taugrid-state-mark" aria-hidden="true" />
          <strong>{stale ? 'Status unavailable' : state.label}</strong>
          {stale && <span>Last known state: {state.label}</span>}
        </div>
        <h2 className="taugrid-run-name">{status.name}</h2>
        <p className="taugrid-run-namespace">Namespace <code>{status.namespace}</code></p>
        <p className="taugrid-summary">{stale
          ? 'The last check failed. Values below are from the last successful response, not the current cluster state.'
          : state.detail}</p>
      </header>

      {status.existing && <dl className="taugrid-signals" aria-label={stale ? 'Last known run signals' : 'Run signals'}>
        <div className="taugrid-signal">
          <dt>Admission</dt>
          <dd>{status.admitted === true ? 'Admitted' : status.admitted === false ? 'Not admitted' : 'Not reported'}</dd>
          <dd className="taugrid-signal-detail">Queue <code>{status.queue || 'not reported'}</code></dd>
        </div>
        <div className="taugrid-signal">
          <dt>Pods</dt>
          <dd>{status.readyPods} / {status.totalPods} ready</dd>
          <dd className="taugrid-signal-detail">{restarts} restart{restarts === 1 ? '' : 's'} observed</dd>
        </div>
        <div className="taugrid-signal">
          <dt>Execution</dt>
          <dd>{state.label}</dd>
          <dd className="taugrid-signal-detail">Deployment {status.deploymentStatus || 'not reported'}</dd>
        </div>
      </dl>}

      {status.message && <p className="taugrid-server-message">{status.message}</p>}

      {status.phases && <section className="taugrid-lifecycle" aria-label="Lifecycle evidence">
        <h3>Lifecycle evidence{stale ? ' (last known)' : ''}</h3>
        <ol className="taugrid-phase-list">{status.phases.map(phase => <li key={phase.key} className={`taugrid-phase-${phase.state}`}>
          <div><strong>{phase.label}</strong> <span className="taugrid-phase-state">{phase.state}</span></div>
          <p>{phase.detail}</p>{phase.hint && <p className="taugrid-next-action"><strong>Next action</strong> {phase.hint}</p>}
        </li>)}</ol>
      </section>}

      <LossSection metrics={status.metrics} stale={stale} />
      {status.output && <section className="taugrid-output" aria-label="Results">
        <h3>Results</h3>
        <p>Recorded output path: <code>{status.output.path || 'Not recorded'}</code></p>
        <p>Recorded output PVC: <code>{status.output.pvc || 'Not recorded'}</code></p>
        <p className="taugrid-muted">Metadata does not prove that files exist. This console does not download or proxy artifacts.</p>
        {portalLink(status.output.portalUrl) ? <a href={portalLink(status.output.portalUrl)!} target="_blank" rel="noopener noreferrer">Open configured portal (new tab)</a> :
          <p className="taugrid-muted">No portal configured. Use <code>tau run get {status.name}</code> with the appropriate namespace and CLI credentials.</p>}
      </section>}

      <div className="taugrid-observations">
        <section className="taugrid-diagnostics" aria-label="Diagnostics">
          <h3>Diagnostics <span className="taugrid-count">{status.diagnostics.length}</span></h3>
          {status.diagnostics.length ? <ul className="taugrid-note-list">
            {status.diagnostics.map((note, index) => <li key={`${note.code}-${index}`}
              className={`taugrid-note taugrid-tone-${note.severity === 'error' ? 'error' : ['warn', 'warning'].includes(note.severity) ? 'warning' : 'info'}`}>
              <span className="taugrid-note-severity">{note.severity}</span>
              <p>{note.message}</p>
              {note.suggestion && <p className="taugrid-next-action"><strong>Next action</strong> {note.suggestion}</p>}
            </li>)}
          </ul> : <p className="taugrid-muted">{unavailable
            ? 'No diagnostics are available. Retry the status check.'
            : 'No diagnostics returned. This does not confirm workload health.'}</p>}
        </section>
        {status.existing && <section className="taugrid-pod-section" aria-label="Pods">
          <h3>Pods <span className="taugrid-count">{status.pods.length}</span></h3>
          {status.pods.length ? <ul className="taugrid-pods">
            {status.pods.map(pod => <li key={pod.name} className="taugrid-pod">
              <div className="taugrid-pod-title"><code>{pod.name}</code>
                <span className={`taugrid-readiness ${pod.ready ? 'taugrid-ready' : ''}`}>{pod.ready ? 'Ready' : 'Not ready'}</span>
              </div>
              <dl className="taugrid-pod-facts">
                <div><dt>Phase</dt><dd>{pod.phase || 'Unknown'}</dd></div>
                <div><dt>Role</dt><dd>{pod.role || pod.rayNodeType || 'Not reported'}</dd></div>
                <div><dt>Node</dt><dd><code>{pod.node || 'Not assigned'}</code></dd></div>
                <div><dt>Restarts</dt><dd>{pod.restarts} restart{pod.restarts === 1 ? '' : 's'}</dd></div>
              </dl>
            </li>)}
          </ul> : <p className="taugrid-muted">No pods returned. Check admission and diagnostics before assuming pods have not been created.</p>}
        </section>}
      </div>

      <details className="taugrid-details">
        <summary>Run identifiers and source</summary>
        <dl className="taugrid-identifiers">
          <div><dt>Ray cluster</dt><dd><code>{status.rayClusterName || 'Not reported'}</code></dd></div>
          <div><dt>Ray job ID</dt><dd><code>{status.jobId || 'Not reported'}</code></dd></div>
          <div><dt>Deployment</dt><dd>{status.deploymentStatus || 'Not reported'}</dd></div>
        </dl>
        <a href={statusUrl} target="_blank" rel="noopener noreferrer">Open status JSON <span>(new tab)</span></a>
        <p className="taugrid-muted">Read through this Jupyter server. No Ray dashboard address is provided by the API.</p>
      </details>
    </article>
  );
}
