// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { apiGet } from './api';
import { LogSnapshot, parseLogs, parseRuns, portalLink, RunList, RunStatus, RunTarget } from './model';
import { SnapshotRequest, SnapshotState } from './snapshot';

function useSnapshot<T>(): [SnapshotState<T>, React.MutableRefObject<SnapshotRequest<T> | null>] {
  const [state, setState] = React.useState<SnapshotState<T>>({ busy: false, value: null, error: null });
  const request = React.useRef<SnapshotRequest<T> | null>(null);
  React.useEffect(() => {
    const current = new SnapshotRequest<T>(setState);
    request.current = current;
    return () => { current.dispose(); request.current = null; };
  }, []);
  return [state, request];
}

export function RunRows({ result, onSelect }: { result: RunList; onSelect: (target: RunTarget) => void }): JSX.Element {
  const partial = result.truncated || result.warnings.length > 0;
  return <div>
    {partial && <div role="status" className="taugrid-validation">
      <p>This list is partial. Missing rows do not prove that runs are absent.</p>
      {result.warnings.map((message, index) => <p key={index}>{message}</p>)}
      {result.truncated && <p>Discovery reached the 500-object per-kind limit. Use an exact name for runs outside this page.</p>}
    </div>}
    {result.runs.length ? <table className="taugrid-runs-table">
      <caption className="taugrid-muted">Tau-managed runs in this namespace.</caption>
      <thead><tr>
        <th scope="col">Run</th>
        <th scope="col">Kind</th>
        <th scope="col">State</th>
        <th scope="col">Queue</th>
        <th scope="col">Created</th>
      </tr></thead>
      <tbody>{result.runs.map(row => <tr key={`${row.namespace}/${row.kind}/${row.name}`}>
        <td><button type="button" className="taugrid-run-link"
          onClick={() => onSelect({ namespace: row.namespace, name: row.name, kind: row.kind })}>{row.name}</button></td>
        <td><span className="taugrid-badge">{row.kind}</span></td>
        <td>{row.state || 'Unknown'}</td>
        <td>{row.queue || 'None'}</td>
        <td>{row.created || 'Not reported'}</td>
      </tr>)}</tbody>
    </table> : <p className="taugrid-muted">{partial
      ? 'No matching runs in the available portion of this list.'
      : 'No Tau-managed runs match this namespace and queue.'}</p>}
  </div>;
}

export function RunBrowser({ namespace, onSelect, portalUrl }: { namespace: string; onSelect: (target: RunTarget) => void; portalUrl: string | null }): JSX.Element {
  const [queue, setQueue] = React.useState('');
  const [state, request] = useSnapshot<RunList>();
  const id = React.useId();
  const link = portalLink(portalUrl);
  const namespaceValue = namespace.trim();
  const reload = React.useCallback((): void => {
    if (!namespaceValue) { request.current?.reset(); return; }
    void request.current?.load(async signal =>
      parseRuns(await apiGet('runs', { namespace: namespaceValue, queue: queue.trim() }, signal)));
  }, [namespaceValue, queue]);
  // The list follows the namespace and queue without a submit: the sidebar's job
  // is to show what is there. Debounced so typing a namespace does not fire a
  // request per keystroke.
  React.useEffect(() => {
    const timer = window.setTimeout(reload, 250);
    return () => window.clearTimeout(timer);
  }, [reload]);
  return <section className="taugrid-browser" aria-label="Live runs">
    <h2>Live runs</h2>
    <p className="taugrid-muted">Tau-managed Jobs and RayJobs in <code>{namespaceValue || '(enter a namespace)'}</code>, newest first. This is live cluster state, not durable history.</p>
    <form className="taugrid-find" onSubmit={event => { event.preventDefault(); reload(); }}>
      <label className="taugrid-field" htmlFor={`${id}-queue`}><span>LocalQueue filter</span>
        <input id={`${id}-queue`} className="taugrid-input" value={queue} placeholder="All queues" onChange={event => { request.current?.reset(); setQueue(event.target.value); }} />
      </label>
      <button type="submit" className="taugrid-button" disabled={!namespaceValue || state.busy}>Refresh</button>
    </form>
    <div role="status" aria-live="polite">{state.busy ? 'Reading live runs...' : state.value ? `${state.value.runs.length} matching runs returned.` : ''}</div>
    {state.error && <p role="alert" className="taugrid-error-text">{state.error}</p>}
    {state.value && <RunRows result={state.value} onSelect={onSelect} />}
    {link && <a href={link} target="_blank" rel="noopener noreferrer">Open configured portal for history (new tab)</a>}
  </section>;
}

export function LogContent({ value }: { value: LogSnapshot }): JSX.Element {
  return <div>
    <p role="status">Snapshot for <code>{value.pod}/{value.container}</code>. {value.possiblyTruncated ? 'Byte limit reached; output may be truncated.' : 'Only the requested tail is shown; older lines may exist.'}</p>
    <pre className="taugrid-log-text" tabIndex={0} aria-label="Container log snapshot">{value.text || '(No log lines returned)'}</pre>
  </div>;
}

export function LogViewer({ status }: { status: RunStatus }): JSX.Element {
  const pods = status.pods.filter(pod => pod.containers?.length);
  const [pod, setPod] = React.useState(pods[0]?.name || '');
  const selected = pods.find(item => item.name === pod);
  const [container, setContainer] = React.useState(selected?.containers?.[0] || '');
  const [tail, setTail] = React.useState('200');
  const [previous, setPrevious] = React.useState(false);
  const [timestamps, setTimestamps] = React.useState(false);
  const [state, request] = useSnapshot<LogSnapshot>();
  const id = React.useId();
  const load = (event: React.FormEvent): void => {
    event.preventDefault();
    void request.current?.load(async signal => parseLogs(await apiGet('logs', {
      namespace: status.namespace, name: status.name, kind: status.kind || 'RayJob',
      pod, container, tail, previous: String(previous), timestamps: String(timestamps)
    }, signal)));
  };
  return <section className="taugrid-logs" aria-label="Container logs">
    <h3>Container logs</h3>
    <p className="taugrid-muted">Manual snapshot only, at most 64 KiB. No streaming, automatic log checks or durable log archive.</p>
    {!pods.length ? <p>No verified pod/container choices. Refresh run status to retry discovery.</p> : <form className="taugrid-log-form" onSubmit={load}>
      <label className="taugrid-field" htmlFor={`${id}-pod`}><span>Pod</span>
        <select id={`${id}-pod`} className="taugrid-input" value={pod} onChange={event => {
          setPod(event.target.value); setContainer(pods.find(item => item.name === event.target.value)?.containers?.[0] || ''); request.current?.reset();
        }}>{pods.map(item => <option key={item.name} value={item.name}>{item.name}</option>)}</select>
      </label>
      <label className="taugrid-field" htmlFor={`${id}-container`}><span>Container</span>
        <select id={`${id}-container`} className="taugrid-input" value={container} onChange={event => { setContainer(event.target.value); request.current?.reset(); }}>
          {selected?.containers?.map(name => <option key={name} value={name}>{name}</option>)}
        </select>
      </label>
      <label className="taugrid-field" htmlFor={`${id}-tail`}><span>Tail lines</span>
        <input id={`${id}-tail`} className="taugrid-input" type="number" required min={1} max={1000} step={1} value={tail} onChange={event => { setTail(event.target.value); request.current?.reset(); }} />
      </label>
      <label className="taugrid-watch"><input type="checkbox" checked={previous} onChange={event => { setPrevious(event.target.checked); request.current?.reset(); }} />Previous container</label>
      <label className="taugrid-watch"><input type="checkbox" checked={timestamps} onChange={event => { setTimestamps(event.target.checked); request.current?.reset(); }} />Timestamps</label>
      <button type="submit" className="taugrid-button" disabled={state.busy || !container || !selected?.containers?.includes(container)}>{state.error ? 'Retry logs' : 'Load logs'}</button>
    </form>}
    <div role="status" aria-live="polite">{state.busy ? 'Reading bounded log snapshot...' : ''}</div>
    {state.error && <p role="alert" className="taugrid-error-text">{state.error}</p>}
    {state.value && <LogContent value={state.value} />}
  </section>;
}
