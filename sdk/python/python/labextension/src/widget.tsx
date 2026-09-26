// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { ReactWidget } from '@jupyterlab/apputils';
import { apiGet, apiUrl } from './api';
import { LogViewer, RunBrowser } from './explorer';
import { canWatch, Capabilities, NamespaceList, parseCapabilities, parseNamespaces, parseRunStatus, RunTarget } from './model';
import { MonitorState, RunMonitor } from './monitor';
import { RunDetails } from './view';
import { namespaceLabel, preferredNamespace } from './destinations';

export interface NotebookHandle {
  name: string;
  toJSON: () => string;
}


export function useCapabilities(): { value: Capabilities | null; error: string | null; retry: () => void } {
  const [value, setValue] = React.useState<Capabilities | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [attempt, setAttempt] = React.useState(0);
  React.useEffect(() => {
    const request = new AbortController();
    setValue(null);
    setError(null);
    void apiGet<unknown>('capabilities', undefined, request.signal).then(result => {
      if (!request.signal.aborted) setValue(parseCapabilities(result));
    }).catch(reason => {
      if (!request.signal.aborted) setError(String(reason));
    });
    return () => request.abort();
  }, [attempt]);
  return { value, error, retry: () => setAttempt(current => current + 1) };
}

export function useNamespaces(): { value: NamespaceList | null; error: string | null } {
  const [value, setValue] = React.useState<NamespaceList | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  React.useEffect(() => {
    const request = new AbortController();
    setError(null);
    void apiGet<unknown>('namespaces', undefined, request.signal)
      .then(result => { if (!request.signal.aborted) { setValue(parseNamespaces(result)); } })
      .catch(reason => { if (!request.signal.aborted) { setError(String(reason)); } });
    return () => request.abort();
  }, []);
  return { value, error };
}

export function RunsSidebar({ onOpenRun, onAbout }: {
  onOpenRun: (target: RunTarget) => void; onAbout: () => void;
}): JSX.Element {
  const [namespace, setNamespace] = React.useState('');
  const [name, setName] = React.useState('');
  const [kind, setKind] = React.useState<'Job' | 'RayJob'>('RayJob');
  const capabilities = useCapabilities();
  const namespaces = useNamespaces();
  const options = namespaces.value?.namespaces || [];
  const id = React.useId();
  // Prefer a namespace that is actually set up for Tau work, and never require
  // the researcher to guess or type one that cannot host the run.
  React.useEffect(() => {
    if (namespace || !options.length) { return; }
    setNamespace(preferredNamespace(options));
  }, [namespace, options]);
  const selected = options.find(option => option.name === namespace);
  return <section className="taugrid-panel taugrid-sidebar" data-testid="taugrid-runs" aria-label="TauGrid runs">
    <header className="taugrid-heading"><h1>Runs</h1><button className="taugrid-button" onClick={onAbout}>About</button></header>
    <label className="taugrid-field" htmlFor={id + '-namespace'}><span>Namespace</span>
      <input id={id + '-namespace'} className="taugrid-input" list={id + '-namespace-options'}
        value={namespace} placeholder="Filter namespaces" autoComplete="off" spellCheck={false}
        onChange={event => setNamespace(event.target.value)} />
    </label>
    <datalist id={id + '-namespace-options'}>
      {options.map(option => <option key={option.name} value={option.name}>{namespaceLabel(option)}</option>)}
    </datalist>
    <p className="taugrid-muted taugrid-namespace-hint" role="status">
      {namespaces.error ? 'Namespace list unavailable; type a namespace to continue.'
        : !options.length ? 'No namespaces are visible to this server; type one to continue.'
        : selected ? (selected.tauEnabled
          ? 'This namespace is labelled for Tau work; queue admission is not guaranteed.'
          : 'This namespace is not labelled for Tau work, so runs may stay unadmitted.')
        : 'This namespace is not in the visible list.'
      }
      {namespaces.value?.warnings?.length ? ` ${namespaces.value.warnings.join(' ')}` : ''}
    </p>
    <RunBrowser key={namespace.trim()} namespace={namespace.trim()} onSelect={onOpenRun} portalUrl={capabilities.value?.portalUrl || null} />
    <details className="taugrid-details"><summary>Find exact run</summary>
      <form className="taugrid-exact" onSubmit={event => { event.preventDefault(); if (namespace.trim() && name.trim()) onOpenRun({ namespace: namespace.trim(), name: name.trim(), kind }); }}>
        <label className="taugrid-field" htmlFor={id + '-kind'}><span>Kind</span><select id={id + '-kind'} className="taugrid-input" value={kind} onChange={event => setKind(event.target.value as 'Job' | 'RayJob')}><option>RayJob</option><option>Job</option></select></label>
        <label className="taugrid-field" htmlFor={id + '-name'}><span>Run name</span><input id={id + '-name'} className="taugrid-input" value={name} onChange={event => setName(event.target.value)} required /></label>
        <button type="submit" className="taugrid-button" disabled={!namespace.trim() || !name.trim()}>Check run</button>
      </form>
    </details>
  </section>;
}

function useRun(target: RunTarget, metrics = false): [MonitorState, React.MutableRefObject<RunMonitor | null>] {
  const [state, setState] = React.useState<MonitorState>({ target, status: null, error: null, busy: false, watching: false, checkedAt: null });
  const monitor = React.useRef<RunMonitor | null>(null);
  React.useEffect(() => {
    const current = new RunMonitor(async (identity, signal) => parseRunStatus(await apiGet('status', { ...identity, ...(metrics ? { includeMetrics: 'true' } : {}) }, signal)), setState, undefined, metrics);
    monitor.current = current;
    current.lookup(target);
    return () => { current.dispose(); monitor.current = null; };
  }, [target.namespace, target.kind, target.name, metrics]);
  return [state, monitor];
}

function RunHeading({ target, logs = false }: { target: RunTarget; logs?: boolean }): JSX.Element {
  return <header className="taugrid-heading"><div><h1>{logs ? 'Logs: ' : ''}{target.name}</h1><p>{target.namespace} / {target.kind} / Read only</p></div></header>;
}

function ReadState({ state }: { state: MonitorState }): JSX.Element {
  return <><p role="status" aria-live="polite">{state.busy ? 'Checking run...' : state.checkedAt ? 'Checked ' + new Date(state.checkedAt).toLocaleTimeString() : 'Waiting for first check.'}</p>
    {state.error && <p role="alert" className="taugrid-error-text">{state.error} {state.status ? 'Showing the last successful snapshot. Watch is stopped.' : 'Refresh to retry.'}</p>}</>;
}

export function RunDetail({ target, onOpenLogs }: { target: RunTarget; onOpenLogs: (target: RunTarget) => void }): JSX.Element {
  const [state, monitor] = useRun(target, true);
  return <section className="taugrid-panel" data-testid="taugrid-detail" aria-label={'Run ' + target.name}>
    <RunHeading target={target} />
    <div className="taugrid-actions"><button className="taugrid-button" disabled={state.busy} onClick={() => monitor.current?.refresh()}>Refresh run</button>
      <label className="taugrid-watch"><input type="checkbox" checked={state.watching} disabled={!!state.error || !canWatch(state.status)} onChange={event => monitor.current?.setWatching(event.target.checked)} />Watch this run (every 10 seconds)</label>
      <button className="taugrid-button" disabled={!!state.error || !state.status?.existing} onClick={() => onOpenLogs(target)}>Open logs</button></div>
    <ReadState state={state} />
    <p role="status" data-testid="taugrid-watch-state">{state.error ? 'Watching stopped after a status error.' : state.watchMessage || (state.watching ? 'Watching status and bounded loss samples.' : 'Not watching.')}</p>
    {state.status && <RunDetails status={state.status} stale={!!state.error} statusUrl={apiUrl('status', { ...target, includeMetrics: 'true' })} />}
  </section>;
}

export function RunLogs({ target }: { target: RunTarget }): JSX.Element {
  const [state, monitor] = useRun(target);
  return <section className="taugrid-panel" data-testid="taugrid-logs" aria-label={'Logs for ' + target.name}>
    <RunHeading target={target} logs />
    <button className="taugrid-button" disabled={state.busy} onClick={() => monitor.current?.refresh()}>Refresh pods</button>
    <ReadState state={state} />
    {state.status && !state.error && !state.busy && <LogViewer key={state.checkedAt} status={state.status} />}
  </section>;
}

export function About(): JSX.Element {
  const capabilities = useCapabilities();
  return <div className="taugrid-panel">
    <p>Runs and logs use the Jupyter server's Kubernetes credentials and RBAC. On a shared server, those credentials may be shared. The browser never connects directly to Kubernetes.</p>
    <p role="status">{capabilities.error || (capabilities.value ? capabilities.value.submissionEnabled && capabilities.value.submissionImplemented ? 'Submission is enabled.' : 'Submission is disabled.' : 'Checking server capabilities...')}</p>
    {capabilities.error && <button className="taugrid-button" onClick={capabilities.retry}>Retry capabilities</button>}
    <p>Submission requires the server operator to certify the notebook runtime and enable TAUGRID_SUBMISSION_ENABLED. Reviewing a plan does not create a run.</p>
    <p>Durable history is available through an operator-configured portal link or the CLI's Kusto options. {capabilities.value?.portalUrl ? 'A portal link is configured.' : 'No portal link is configured.'} The browser never queries Kubernetes or portal APIs directly. The server may read bounded portal metrics only with separate operator opt-in and an exact run mapping.</p>
  </div>;
}

export function runWidgetId(target: RunTarget, surface = 'detail'): string {
  return 'taugrid-' + surface + '-' + [target.namespace, target.kind || 'RayJob', target.name].map(value => encodeURIComponent(value)).join(':');
}

export class SurfaceWidget extends ReactWidget {
  constructor(private element: React.ReactElement) {
    super();
    this.addClass('taugrid-widget');
    this.node.tabIndex = -1;
  }
  protected onActivateRequest(): void {
    const control = this.node.querySelector<HTMLElement>('button:not(:disabled), input:not(:disabled)');
    (control || this.node).focus();
  }
  render(): React.ReactElement { return this.element; }
}
