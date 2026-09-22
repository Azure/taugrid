// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { useEffect, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { boardScopeKey, experimentsAPI, useWorkspace } from '../data';
import { Empty, PageTitle } from '../components';
import type { MetricCatalogEntry, ResponseMeta, RunSummary, SeriesPoint } from './contracts';
import {
  useExperimentsQuery, useMetricCatalogQuery, useMetricSeriesQuery, useRunDetailQuery, useRunResolverQuery, useRunsQuery,
} from './queries';
import { useExperimentURLState } from './url-state';
import './workspace.css';

function QueryState({ name, query, children }: {
  name: string;
  query: { data?: unknown; error: Error | null; isPending: boolean; refetch: () => Promise<unknown> };
  children: () => ReactNode;
}) {
  if (query.error) return <><div className="stellar-state error" role="alert"><strong>{name} unavailable</strong><span>{query.error.message}</span>
    {query.data !== undefined && <span>Showing the last successful response for this workspace.</span>}
    <button type="button" onClick={() => void query.refetch()}>Retry</button></div>{query.data !== undefined && children()}</>;
  if (query.isPending) return <div className="stellar-state" role="status">Loading {name.toLowerCase()}…</div>;
  return query.data === undefined ? null : <>{children()}</>;
}

function DataState({ meta }: { meta: ResponseMeta }) {
  const messages = [
    meta.availability === 'unavailable' ? 'This data source reports that data is unavailable.' : '',
    meta.partial || meta.availability === 'partial' || meta.availability === 'degraded'
      ? 'Partial results: some experiment data is unavailable.' : '',
    ...meta.warnings,
  ].filter(Boolean);
  return <div className={'stellar-data-state ' + (messages.length ? 'warn' : '')} role={messages.length ? 'status' : undefined}>
    <span>Availability: {meta.availability}</span>
    {messages.map(message => <span key={message}>{message}</span>)}
    {meta.provenance && <span>Source: {meta.provenance}</span>}
    {meta.freshness && <span>Freshness: {meta.freshness}</span>}
  </div>;
}

export function StellarWorkspace() {
  const { scope } = useWorkspace();
  if (!experimentsAPI(scope)) return <><PageTitle title="Experiments">Training runs and bounded metric series.</PageTitle>
    <Empty warn><strong>Experiment backend setup required</strong><p>{scope.experimentsNative?.reason || 'Configure an authorized same-origin experiment backend for this workspace.'}</p></Empty></>;
  return <div className="stellar-workspace">
    <PageTitle title="Experiments">Search experiments, select a run, then inspect one bounded metric series.</PageTitle>
    <ExperimentDashboard key={[scope.workspace, scope.source, scope.cluster, scope.namespace].join(':')}/>
  </div>;
}

function ExperimentDashboard() {
  const { state, update } = useExperimentURLState();
  const unresolvedRun = !state.experiment ? state.run : '';
  return <div className="thin-dashboard">
    <RefreshExperimentData/>
    <ExperimentSearch/>
    {state.target ? <LegacyTargetResolver targetID={state.target}/> : unresolvedRun ? <RunTargetResolver runID={unresolvedRun}/> : <>
      {state.experiment && <RunsTable key={`${state.project}:${state.experiment}`} experiment={state.experiment}/>}
      {state.experiment && state.run && <RunDetailPanel key={`${state.project}:${state.experiment}:${state.run}`} experiment={state.experiment} runID={state.run}/>}
      {!state.experiment && <div className="stellar-state">Choose an experiment to load runs.</div>}
    </>}
    {state.experiment && <button className="stellar-clear" type="button" onClick={() => update({ experiment: '' }, false)}>Clear experiment selection</button>}
  </div>;
}

function RefreshExperimentData() {
  const { scope, managed } = useWorkspace();
  const client = useQueryClient();
  const [refreshing, setRefreshing] = useState(false);
  const refresh = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      const prefix = boardScopeKey(scope, managed);
      await client.invalidateQueries({
        queryKey: prefix,
        predicate: query => {
          const path = query.queryKey.at(-1);
          return typeof path === 'string' && path.startsWith('/api/v2/stellar/');
        },
        refetchType: 'active',
      });
    } finally {
      setRefreshing(false);
    }
  };
  return <div className="stellar-refresh">
    <button type="button" aria-label="Refresh experiment data" disabled={refreshing} onClick={() => void refresh()}>
      {refreshing ? 'Refreshing…' : 'Refresh'}
    </button>
  </div>;
}

function LegacyTargetResolver({ targetID }: { targetID: string }) {
  const { state, update } = useExperimentURLState();
  const experiments = useExperimentsQuery({ q: targetID, project: state.project, cursor: '' });
  const exact = experiments.data?.experiments.filter(experiment =>
    experiment.experiment_id === targetID && (!state.project || experiment.project === state.project)) || [];
  const run = useRunResolverQuery(targetID, state.project, experiments.isSuccess && exact.length === 0);
  useEffect(() => {
    if (exact.length !== 1) return;
    update({ target: '', project: exact[0].project, experiment: exact[0].experiment_id });
  }, [exact, update]);
  useEffect(() => {
    const detail = run.data?.run;
    if (!detail?.experiment_id) return;
    update({ target: '', project: detail.project, experiment: detail.experiment_id, run: detail.run_id });
  }, [run.data, update]);
  if (exact.length > 1) {
    return <div className="stellar-state error" role="alert">Experiment ID {targetID} exists in multiple projects; specify a project.</div>;
  }
  if (!experiments.isSuccess || exact.length === 1) {
    return <QueryState name="Experiment link" query={experiments}>{() =>
      <div className="stellar-state" role="status">Opening experiment {targetID}…</div>
    }</QueryState>;
  }
  return <ResolvedRunTarget runID={targetID} query={run}/>;
}

function RunTargetResolver({ runID }: { runID: string }) {
  const { state } = useExperimentURLState();
  const query = useRunResolverQuery(runID, state.project);
  return <ResolvedRunTarget runID={runID} query={query}/>;
}

function ResolvedRunTarget({ runID, query }: {
  runID: string;
  query: ReturnType<typeof useRunResolverQuery>;
}) {
  const { update } = useExperimentURLState();
  useEffect(() => {
    const run = query.data?.run;
    if (!run?.experiment_id) return;
    update({
      target: '',
      project: run.project,
      experiment: run.experiment_id,
      run: run.run_id,
    });
  }, [query.data, update]);
  if (query.data && !query.data.run.experiment_id) {
    return <div className="stellar-state error" role="alert">Run detail did not identify an experiment for {runID}.</div>;
  }
  return <QueryState name="Run link" query={query}>{() =>
    <div className="stellar-state" role="status">Opening experiment for {runID}…</div>
  }</QueryState>;
}

function ExperimentSearch() {
  const { state, update } = useExperimentURLState();
  const [draft, setDraft] = useState(state.q);
  const [draftProject, setDraftProject] = useState(state.project);
  const [queryProject, setQueryProject] = useState(state.project);
  useEffect(() => setDraft(state.q), [state.q]);
  useEffect(() => {
    setDraftProject(state.project);
    if (!state.experiment) setQueryProject(state.project);
  }, [state.project, state.experiment]);
  const query = useExperimentsQuery({
    q: state.q,
    // Keep discovery stable when selecting a result whose canonical project was not part of the search.
    project: queryProject,
    cursor: state.experiment ? '' : state.cursor,
  });
  return <section className="thin-panel" aria-labelledby="experiment-search-title">
    <h2 id="experiment-search-title">1. Experiment search</h2>
    <form className="thin-controls" onSubmit={event => {
      event.preventDefault();
      setQueryProject(draftProject);
      update({ q: draft, project: draftProject, experiment: '', cursor: '' }, false);
    }}>
      <label>Search<input aria-label="Search experiments" type="search" value={draft} onChange={event => setDraft(event.target.value)} placeholder="Name or experiment ID"/></label>
      <label>Project<input aria-label="Project" value={draftProject} onChange={event => setDraftProject(event.target.value)} placeholder="All projects"/></label>
      <button type="submit">Search</button>
    </form>
    <QueryState name="Experiment search" query={query}>{() => {
      const data = query.data!;
      return <><DataState meta={data}/>
        {!data.experiments.length ? <Empty>No experiments match this search.</Empty> :
          <ul className="thin-experiment-list">{data.experiments.map(experiment => <li key={`${experiment.project}:${experiment.experiment_id}`}>
            <button type="button" aria-pressed={state.experiment === experiment.experiment_id && state.project === experiment.project}
              onClick={() => update({ experiment: experiment.experiment_id, project: experiment.project, cursor: '' }, false)}>
              <strong>{experiment.name}</strong><span>{experiment.experiment_id}</span>
              <small>{experiment.project || 'default project'}{experiment.run_count === undefined ? '' : ` · ${experiment.run_count} runs`}</small>
            </button>
          </li>)}</ul>}
        {data.next_cursor && !state.experiment && <button type="button" onClick={() => update({ cursor: data.next_cursor! }, false)}>Next experiments</button>}
      </>;
    }}</QueryState>
  </section>;
}

function RunsTable({ experiment }: { experiment: string }) {
  const { state, update } = useExperimentURLState();
  const [draft, setDraft] = useState(state.filter);
  useEffect(() => setDraft(state.filter), [state.filter]);
  const query = useRunsQuery(experiment, { project: state.project, filter: state.filter, cursor: state.cursor });
  return <section className="thin-panel" aria-labelledby="runs-title">
    <h2 id="runs-title">2. Runs</h2>
    <p className="thin-context">{experiment}</p>
    <form className="thin-controls compact" onSubmit={event => { event.preventDefault(); update({ filter: draft, cursor: '' }, false); }}>
      <label>Filter<input aria-label="Filter runs" value={draft} onChange={event => setDraft(event.target.value)} placeholder="Run ID, state, or tag"/></label>
      <button type="submit">Apply</button>
    </form>
    <QueryState name="Runs" query={query}>{() => {
      const data = query.data!;
      return <><DataState meta={data}/>
        {!data.runs.length ? <Empty>No runs match this experiment and filter.</Empty> :
          <table className="thin-table"><thead><tr><th>Run</th><th>State</th><th>Owner</th><th>Updated</th><th>Metrics</th></tr></thead>
            <tbody>{data.runs.map(run => <RunRow key={run.run_id} run={run} selected={state.run === run.run_id} select={() => update({ run: run.run_id }, false)}/>)}</tbody>
          </table>}
        {data.next_cursor && <button type="button" onClick={() => update({ cursor: data.next_cursor! }, false)}>Next runs</button>}
      </>;
    }}</QueryState>
  </section>;
}

function RunRow({ run, selected, select }: { run: RunSummary; selected: boolean; select: () => void }) {
  return <tr className={selected ? 'selected' : ''}>
    <td><button className="stellar-link" type="button" aria-pressed={selected} onClick={select}>{run.run_id}</button></td>
    <td>{run.lifecycle_state || run.state}</td><td>{run.owner || '—'}</td><td>{run.completed_at || run.started_at || run.created_at || '—'}</td><td>{run.metric_names.length || '—'}</td>
  </tr>;
}

function RunDetailPanel({ experiment, runID }: { experiment: string; runID: string }) {
  const { state } = useExperimentURLState();
  const query = useRunDetailQuery(experiment, runID, state.project);
  const catalogQuery = useMetricCatalogQuery(experiment, runID, state.project);
  return <section className="thin-panel" aria-labelledby="run-detail-title">
    <h2 id="run-detail-title">3. Run detail</h2>
    <QueryState name="Run detail" query={query}>{() => {
      const detail = query.data!;
      return <><DataState meta={detail}/>
        <dl className="thin-detail"><div><dt>Run ID</dt><dd>{detail.run.run_id}</dd></div><div><dt>State</dt><dd>{detail.run.state}</dd></div>
          <div><dt>Lifecycle</dt><dd>{detail.run.lifecycle_state}</dd></div><div><dt>Project</dt><dd>{detail.run.project || state.project || '—'}</dd></div>
          <div><dt>Owner</dt><dd>{detail.run.owner || '—'}</dd></div><div><dt>Started</dt><dd>{detail.run.started_at || '—'}</dd></div>
          <div><dt>Completed</dt><dd>{detail.run.completed_at || '—'}</dd></div></dl>
        <QueryState name="Metric catalog" query={catalogQuery}>{() =>
          <><DataState meta={catalogQuery.data!}/><MetricPanel experiment={experiment} runID={runID} catalog={catalogQuery.data!.metrics}/></>
        }</QueryState>
      </>;
    }}</QueryState>
  </section>;
}

function MetricPanel({ experiment, runID, catalog }: { experiment: string; runID: string; catalog: MetricCatalogEntry[] }) {
  const { state, update } = useExperimentURLState();
  const [range, setRange] = useState({
    startStep: state.startStep?.toString() || '', endStep: state.endStep?.toString() || '',
    stepInterval: state.stepInterval?.toString() || '', maxPoints: state.maxPoints.toString(),
  });
  useEffect(() => setRange({
    startStep: state.startStep?.toString() || '', endStep: state.endStep?.toString() || '',
    stepInterval: state.stepInterval?.toString() || '', maxPoints: state.maxPoints.toString(),
  }), [state.startStep, state.endStep, state.stepInterval, state.maxPoints]);
  const selected = catalog.some(metric => metric.name === state.metric) ? state.metric : '';
  const query = useMetricSeriesQuery(experiment, runID, selected, state.project, {
    startStep: state.startStep, endStep: state.endStep, stepInterval: state.stepInterval, maxPoints: state.maxPoints,
  });
  return <div className="thin-metrics">
    <h2>4. Metrics and chart</h2>
    {!catalog.length ? <Empty>No metrics are available for this run.</Empty> : <>
      <form className="thin-controls metric-controls" onSubmit={event => {
        event.preventDefault();
        update({
          startStep: range.startStep, endStep: range.endStep, stepInterval: range.stepInterval,
          maxPoints: range.maxPoints || '500',
        });
      }}>
        <label>Metric<select aria-label="Metric" value={selected} onChange={event => update({ metric: event.target.value })}><option value="">Choose a metric</option>{catalog.map(metric => <option key={metric.name} value={metric.name}>{metric.name}</option>)}</select></label>
        <label>Start step<input aria-label="Start step" type="number" value={range.startStep} onChange={event => setRange(value => ({ ...value, startStep: event.target.value }))}/></label>
        <label>End step<input aria-label="End step" type="number" value={range.endStep} onChange={event => setRange(value => ({ ...value, endStep: event.target.value }))}/></label>
        <label>Interval<input aria-label="Step interval" type="number" min="1" value={range.stepInterval} onChange={event => setRange(value => ({ ...value, stepInterval: event.target.value }))}/></label>
        <label>Point budget<input aria-label="Maximum points" type="number" min="10" max="5000" value={range.maxPoints} onChange={event => setRange(value => ({ ...value, maxPoints: event.target.value }))}/></label>
        <button type="submit">Apply range</button>
      </form>
      {!selected ? <div className="stellar-state">Choose a metric to request a bounded series.</div> :
        <QueryState name="Metric series" query={query}>{() => <><DataState meta={query.data!}/><MetricChart points={query.data!.points} metric={selected}/></>}</QueryState>}
    </>}
  </div>;
}

function MetricChart({ points, metric }: { points: SeriesPoint[]; metric: string }) {
  if (!points.length) return <Empty>No finite points were returned for {metric}.</Empty>;
  const width = 800, height = 260, pad = 28;
  const xs = points.map(point => point.step), ys = points.map(point => point.value);
  const minX = Math.min(...xs), maxX = Math.max(...xs), minY = Math.min(...ys), maxY = Math.max(...ys);
  const x = (value: number) => pad + (maxX === minX ? .5 : (value - minX) / (maxX - minX)) * (width - pad * 2);
  const y = (value: number) => height - pad - (maxY === minY ? .5 : (value - minY) / (maxY - minY)) * (height - pad * 2);
  const path = points.map((point, index) => `${index ? 'L' : 'M'} ${x(point.step)} ${y(point.value)}`).join(' ');
  return <figure className="thin-chart"><figcaption>{metric} · {points.length} points</figcaption>
    <svg role="img" aria-label={`${metric} series chart`} viewBox={`0 0 ${width} ${height}`}><path d={path} fill="none" stroke="currentColor" strokeWidth="2"/></svg>
    <div><span>step {minX}–{maxX}</span><span>value {minY.toPrecision(4)}–{maxY.toPrecision(4)}</span></div>
  </figure>;
}
