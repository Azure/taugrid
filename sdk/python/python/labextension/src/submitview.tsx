// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import * as React from 'react';
import { apiGet, apiPost } from './api';
import { DestinationSession, DestinationState, namespaceLabel } from './destinations';
import { NamespaceRow, RunTarget, SubmitPlanSummary } from './model';
import { SubmissionSession, SubmissionState, SubmissionFailure, submissionFailure } from './submission';
import { bytes, FailureNotice, FileList, parseFileList, possibleImports, SubmissionOutcome, SubmissionReceipt, SubmissionStatus } from './submissionui';
import { NotebookHandle, useCapabilities } from './widget';

function useNotebookFiles(path: string): { value: FileList | null; error: string | null; retry: () => void } {
  const [value, setValue] = React.useState<FileList | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [attempt, setAttempt] = React.useState(0);
  React.useEffect(() => {
    const request = new AbortController();
    setValue(null);
    setError(null);
    void apiGet<unknown>('files', { path }, request.signal)
      .then(result => { if (!request.signal.aborted) setValue(parseFileList(result)); })
      .catch(reason => { if (!request.signal.aborted) setError(String(reason)); });
    return () => request.abort();
  }, [path, attempt]);
  return { value, error, retry: () => setAttempt(current => current + 1) };
}

export function SubmissionConfirmation({ plan }: { plan: SubmitPlanSummary }): JSX.Element {
  return <div className="taugrid-panel taugrid-submit-confirmation"><p>Submit the reviewed notebook snapshot as <strong>{plan.namespace}/{plan.name}</strong>?</p>
    <p>Profile <strong>{plan.profile}</strong>, queue <strong>{plan.queue}</strong>{plan.workers === undefined ? '' : `, ${plan.workers} workers`}.</p>
    <p>Submit notebook creates a RayJob using the Jupyter server's credentials. Edits since review are not included. Cancel returns to the review without creating anything.</p></div>;
}

export function SubmissionReview({ notebook, onConfirm, onOpenRun, onAbout }: {
  notebook: NotebookHandle;
  onConfirm: (plan: SubmitPlanSummary) => Promise<boolean>;
  onOpenRun: (target: RunTarget) => void;
  onAbout: () => void;
}): JSX.Element {
  const [namespace, setNamespace] = React.useState('');
  const [name, setName] = React.useState('');
  const [profile, setProfile] = React.useState('');
  const [queue, setQueue] = React.useState('');
  const [state, setState] = React.useState<SubmissionState>({ preview: null, busy: false, submitted: null, attempted: null, uncertain: false, error: null });
  const [destinations, setDestinations] = React.useState<DestinationState>({ value: null, busy: true, error: null });
  const [namespaces, setNamespaces] = React.useState<NamespaceRow[]>([]);
  const [confirming, setConfirming] = React.useState(false);
  const [localFailure, setLocalFailure] = React.useState<SubmissionFailure | null>(null);
  const [notice, setNotice] = React.useState('');
  const [selected, setSelected] = React.useState<string[]>([]);
  const [search, setSearch] = React.useState('');
  const [withoutFiles, setWithoutFiles] = React.useState(false);
  const [imports, setImports] = React.useState<string[]>([]);
  const session = React.useRef<SubmissionSession | null>(null);
  const catalog = React.useRef<DestinationSession | null>(null);
  const confirmingRef = React.useRef(false);
  const capabilities = useCapabilities();
  const candidates = useNotebookFiles(notebook.name);
  const id = React.useId();

  React.useEffect(() => {
    const current = new SubmissionSession(apiPost, setState);
    const discovery = new DestinationSession((namespace, signal) => apiGet('destinations', namespace ? { namespace } : undefined, signal), next => {
      setDestinations(next);
      if (next.value) {
        const value = next.value;
        setNamespace(value.namespace || '');
        setNamespaces(value.namespaces);
        setProfile(previous => value.profiles.some(option => option.name === previous) ? previous : '');
        setQueue(previous => value.queues.includes(previous) ? previous : value.defaultQueue && value.queues.includes(value.defaultQueue) ? value.defaultQueue : '');
      }
    });
    session.current = current;
    catalog.current = discovery;
    void discovery.refresh('');
    try { setImports(possibleImports(notebook.toJSON())); } catch { setImports([]); }
    return () => { current.dispose(); discovery.dispose(); session.current = null; catalog.current = null; };
  }, [notebook]);

  const locked = state.busy || confirming || !!state.submitted || state.uncertain;
  const options = destinations.value;
  const chosen = options?.profiles.find(option => option.name === profile);
  const selectedNamespace = namespaces.find(option => option.name === namespace);
  const fileBytes = candidates.value?.files.filter(file => selected.includes(file.name)).reduce((total, file) => total + file.size, 0) || 0;
  const overBudget = fileBytes > (candidates.value?.budgetBytes || 1048576);
  const canReview = !locked && !destinations.busy && !!chosen && !!namespace && !!options?.queues.includes(queue) && (!!candidates.value || withoutFiles) && !overBudget;
  const plan = state.preview?.plan;
  const available = !!capabilities.value?.submissionEnabled && !!capabilities.value.submissionImplemented;

  const invalidate = (message = 'Choices changed. Review again before submitting.'): void => {
    if (session.current?.state.preview || session.current?.state.busy) setNotice(message);
    session.current?.invalidate();
    setLocalFailure(null);
  };
  const refresh = (nextNamespace = destinations.error ? '' : namespace): void => {
    invalidate('Destinations refreshed. Review again before submitting.');
    void catalog.current?.refresh(nextNamespace);
  };
  const retryFiles = (): void => {
    invalidate('Files refreshed and selections cleared. Select files and review again.');
    setSelected([]);
    setWithoutFiles(false);
    candidates.retry();
    try { setImports(possibleImports(notebook.toJSON())); } catch { setImports([]); }
  };
  const review = (): void => {
    if (!canReview) return;
    setNotice('');
    setLocalFailure(null);
    session.current?.invalidate();
    try {
      const snapshot = notebook.toJSON();
      setImports(possibleImports(snapshot));
      void session.current?.review({ notebook: snapshot, path: notebook.name, namespace, name: name.trim() || undefined, profile, queue, files: [...selected] });
    } catch (error) { setLocalFailure(submissionFailure(error)); }
  };
  const submit = async (): Promise<void> => {
    const current = session.current;
    if (!current || !plan || confirmingRef.current || !available) return;
    confirmingRef.current = true;
    setConfirming(true);
    try {
      const accepted = await onConfirm(plan);
      if (session.current !== current) return;
      if (!accepted) setNotice('Submission cancelled. Nothing has been created; the reviewed snapshot is still available.');
      await current.submit(plan, accepted, available);
    } catch (error) {
      if (session.current === current) setLocalFailure({ title: 'Confirmation could not open', action: 'Try Submit notebook again to open confirmation. Nothing was submitted.', detail: String(error) });
    } finally {
      confirmingRef.current = false;
      if (session.current === current) setConfirming(false);
    }
  };

  return <section className="taugrid-panel taugrid-review" data-testid="taugrid-review" aria-label={'Submit ' + notebook.name}>
    <header className="taugrid-heading"><div><h1>Submit notebook</h1><p>{notebook.name}</p></div><button className="taugrid-button" onClick={onAbout}>About submission</button></header>
    <SubmissionStatus state={state} confirming={confirming} notice={notice} />
    {(localFailure || state.failure) && <FailureNotice failure={(localFailure || state.failure)!} />}
    <SubmissionOutcome state={state} onOpenRun={onOpenRun} onAcknowledge={() => { session.current?.acknowledgeUncertain(); setNotice('Review again only if you have confirmed that another submission is needed.'); }} />
    {!state.submitted && !state.uncertain && <>
      <section className="taugrid-submit-section" aria-label="Choose destination">
        <div className="taugrid-heading"><h2>Choose destination</h2><button className="taugrid-button" disabled={locked || destinations.busy} onClick={() => refresh()}>Refresh destinations</button></div>
        <p>The Jupyter server lists destinations with its credentials. Review is read-only; submitting creates a cluster workload.</p>
        {destinations.busy && <p role="status">Loading destinations…</p>}
        {destinations.error && <FailureNotice failure={{ title: 'Destinations are unavailable', action: 'Check sign-in, cluster connectivity, and namespace read permission with the operator. Refresh destinations to try again.', detail: destinations.error }} />}
        <div className="taugrid-destination-fields">
          <label className="taugrid-field" htmlFor={id + '-namespace'}><span>Namespace</span><select id={id + '-namespace'} data-testid="taugrid-submit-namespace" className="taugrid-input" value={namespace} disabled={locked || destinations.busy} onChange={event => { setNamespace(event.target.value); setQueue(''); refresh(event.target.value); }}>
            <option value="" disabled>Choose a namespace</option>{namespaces.map(option => <option key={option.name} value={option.name}>{namespaceLabel(option)}</option>)}
          </select></label>
          <label className="taugrid-field" htmlFor={id + '-profile'}><span>Worker profile</span><select id={id + '-profile'} data-testid="taugrid-submit-profile" className="taugrid-input" value={profile} disabled={locked || !options} onChange={event => { setProfile(event.target.value); invalidate(); }}>
            <option value="" disabled>Choose a worker profile</option>{options?.profiles.map(option => <option key={option.name} value={option.name}>{option.name}: {option.workers} workers, {option.gpusPerWorker} GPUs each</option>)}
          </select></label>
          <label className="taugrid-field" htmlFor={id + '-queue'}><span>Queue</span><select id={id + '-queue'} data-testid="taugrid-submit-queue" className="taugrid-input" value={queue} disabled={locked || !options} onChange={event => { setQueue(event.target.value); invalidate(); }}>
            <option value="" disabled>Choose a queue</option>{options?.queues.map(option => <option key={option} value={option}>{option}{option === options.defaultQueue ? ' (workspace default)' : ''}</option>)}
          </select></label>
        </div>
        {options && <>
          {!options.namespaces.length && <p role="status">No namespaces are visible. Ask the operator for namespace list permission or a Tau workspace, then refresh destinations.</p>}
          {!options.profiles.length && <p role="status">No ready worker profiles are available. Ask the operator to publish a ready TauCluster profile, then refresh destinations.</p>}
          {!options.queues.length && <p role="status">No queues are visible in this namespace. Choose another namespace or ask the operator to configure a LocalQueue and its read permission, then refresh destinations.</p>}
          {selectedNamespace && <p className="taugrid-muted">{selectedNamespace.tauEnabled ? 'This namespace is Tau-labelled; queue admission is not guaranteed.' : 'This namespace is not labelled for Tau work. Check queue setup with the operator before submitting.'}</p>}
          <p className="taugrid-muted">Workspace default queue: <strong>{options.defaultQueue || 'Not resolved'}</strong>. {queue && queue !== options.defaultQueue ? 'You selected a different queue. Confirm that this queue accepts the profile and workload.' : 'This is the TauCluster workspace default, not a namespace-specific admission guarantee.'}</p>
          {options.warnings.map((warning, index) => <p className="taugrid-submit-warning" key={index}>{warning}</p>)}
          <p className="taugrid-muted">One bounded read: up to {options.limits.namespaces} namespaces, {options.limits.profiles} profiles, and {options.limits.queues} queues for this namespace. Missing an option? Ask the operator; this view does not walk more pages.</p>
        </>}
        {chosen && <div className="taugrid-profile-facts" data-testid="taugrid-profile-consequences"><p>{chosen.description || `The ${chosen.name} profile requests the following worker resources.`}</p>
          <dl className="taugrid-plan">{Object.entries({ Workers: chosen.workers, 'GPUs per worker': chosen.gpusPerWorker, 'CPUs per worker': chosen.cpusPerWorker, 'Memory per worker': chosen.memoryPerWorker }).map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>
          <p>Priority: {chosen.priority || 'No priority class'}. Runtime image: <code>{chosen.image}</code></p>
          {chosen.gpusPerWorker > 0 && <p className="taugrid-submit-warning">GPU availability has not been checked. The queue may wait for matching GPU capacity. Choose a CPU profile if this notebook does not need GPUs.</p>}</div>}
      </section>
      <section className="taugrid-submit-section" aria-label="Notebook and companion files">
        <div className="taugrid-heading"><h2>Notebook and companion files</h2><button className="taugrid-button" disabled={locked} onClick={retryFiles}>Refresh files</button></div>
        <p>The current notebook snapshot always ships. Companion files are opt-in, read at review, and must be flat files beside the notebook.</p>
        {!candidates.value && !candidates.error && <p role="status">Loading files…</p>}
        {candidates.error && <div className="taugrid-error" role="alert"><p>Companion files could not be listed. Check the notebook path and file permissions, then refresh files.</p><details><summary>Technical details</summary><p>{candidates.error}</p></details>
          <button className="taugrid-button" disabled={locked || withoutFiles} onClick={() => { invalidate(); setSelected([]); setWithoutFiles(true); }}>Continue without companion files</button></div>}
        {withoutFiles && <p role="status">Only the notebook will ship. Imports that need local companion files may fail.</p>}
        {candidates.value && <>
          <label className="taugrid-field" htmlFor={id + '-file-search'}><span>Find a companion file</span><input id={id + '-file-search'} data-testid="taugrid-file-search" className="taugrid-input" type="search" value={search} onChange={event => setSearch(event.target.value)} /></label>
          <p className="taugrid-muted">{selected.length} selected, {bytes(fileBytes)} before packaging. Decoded package budget: {bytes(candidates.value.budgetBytes)}; encoded environment budget: 65,536 B. Notebook and encoding overhead also count; review checks both limits.</p>
          {overBudget && <p className="taugrid-error-text" role="alert">Selected files exceed the decoded budget. Remove files or put larger assets in the runtime image.</p>}
          <ul className="taugrid-file-list">{candidates.value.files.filter(file => file.name.toLowerCase().includes(search.toLowerCase())).map(file => <li key={file.name}><label className="taugrid-file">
            <input type="checkbox" disabled={locked} checked={selected.includes(file.name)} onChange={event => { setSelected(current => event.target.checked ? [...current, file.name] : current.filter(name => name !== file.name)); invalidate(); }} />
            <span className="taugrid-file-name">{file.name}{imports.includes(file.name) && <small>Possible local Python import</small>}</span><span>{bytes(file.size)}</span></label></li>)}</ul>
          {!candidates.value.files.length ? <p>No eligible companion files found. Review the notebook alone, or put larger assets in its runtime image.</p> : !candidates.value.files.some(file => file.name.toLowerCase().includes(search.toLowerCase())) ? <p>No filenames match. Clear the search to see all eligible files.</p> : null}
          <p className="taugrid-muted">Import hints come from simple Python imports when opened or refreshed. They are not dependency analysis and never select files for you. At most 200 directory entries are considered; hidden files, folders, and files over 262,144 B are excluded.</p>
          {candidates.value.warnings.map((warning, index) => <p key={index} className="taugrid-submit-warning">{warning}</p>)}
        </>}
      </section>
      <form className="taugrid-submit-section" onSubmit={event => { event.preventDefault(); review(); }}>
        <h2>Review what will run</h2>
        <label className="taugrid-field" htmlFor={id + '-name'}><span>Run name (optional)</span><input id={id + '-name'} className="taugrid-input" value={name} disabled={locked} onChange={event => { setName(event.target.value); invalidate(); }} placeholder="Leave blank for a generated name" /></label>
        <p>Review captures the notebook and selected files without creating a workload. Change any choice to discard the review. To include later notebook edits, review again.</p>
        <button className="taugrid-button" data-testid="taugrid-review-submission" disabled={!canReview}>Review submission</button>
      </form>
      {plan && <SubmissionReceipt plan={plan} notebookName={notebook.name} />}
      <section className="taugrid-submit-section taugrid-write-boundary" aria-label="Submit reviewed notebook"><h2>Submit notebook</h2>
        <p>Creates the reviewed RayJob only after you confirm. No automatic retries.</p>
        {capabilities.error ? <div role="alert"><p>Submission availability could not be checked. Check your Jupyter connection and try again.</p><details><summary>Technical details</summary><p>{capabilities.error}</p></details><button className="taugrid-button" onClick={capabilities.retry} disabled={locked}>Retry capabilities</button></div> : !capabilities.value ? <p>Checking submission availability…</p> : !available ? <div><p>Submission is disabled on this server. You can still review the plan. Ask the operator to enable submission after runtime validation.</p><button className="taugrid-button" disabled={locked} onClick={capabilities.retry}>Check submission availability</button></div> : null}
        <button className="taugrid-button taugrid-primary" data-testid="taugrid-submit-notebook" disabled={locked || !state.preview?.submittable || !state.preview.submissionEnabled || !available} onClick={() => void submit()}>{state.busy && state.attempted ? 'Submitting notebook…' : 'Submit notebook'}</button>
      </section>
    </>}
  </section>;
}
