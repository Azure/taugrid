// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { createPortalQueryClient, WorkspaceProvider } from '../data';
import type { WorkspaceScope } from '../types';
import { StellarWorkspace } from './Workspace';
import { filterRuns, reconcilePageRuns, runLifecycle, runTimestamp } from './state';
import type { RunSearchRun, RunView } from './types';

const scope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'tau-system',
  source: 'local', authorizationMode: 'workspace', availability: 'available', managed: false,
  experimentsNative: { state: 'available', apiBasePath: '/api/v2/stellar' },
};
const page = {
  total: 201, truncated: true, warnings: ['Range A warning'],
  runs: [{ run_id: 'range-a-run', run_group_id: 'group', project: 'research', state: 'running',
    created_at: '2026-09-20T10:00:00Z', metric_names: [] }],
};
function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}
function TimezoneNavigation() {
  const location = useLocation(), navigate = useNavigate();
  return <button onClick={() => {
    const params = new URLSearchParams(location.search);
    params.set('tz', 'utc');
    navigate(location.pathname + '?' + params);
  }}>Display UTC</button>;
}
function renderWorkspace(client = createPortalQueryClient(), initialEntry = '/portal/experiments?target=experiment&sections=&window=24h&refresh=off') {
  return render(<QueryClientProvider client={client}>
    <MemoryRouter initialEntries={[initialEntry]}>
      <WorkspaceProvider scope={scope} managed={false}><StellarWorkspace/><TimezoneNavigation/></WorkspaceProvider>
    </MemoryRouter>
  </QueryClientProvider>);
}
function otherResponse(url: URL) {
  return json(url.pathname.endsWith('/snapshot')
    ? { runs: [snapshotRun(), snapshotRun({ run_id: 'outside-range' })], status: { metric_files: 0 }, cards: [], metric_options: [], chart: {}, summary: {} }
    : { experiments: [] });
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('preserves snapshot liveness for in-range runs without adding snapshot-only runs', async () => {
  vi.stubGlobal('innerWidth', 1280);
  const reason = 'run has no recent liveness evidence and no terminal outcome';
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname.endsWith('/runs')) return Promise.resolve(json(page));
    if (url.pathname.endsWith('/snapshot')) return Promise.resolve(json({
      runs: [
        { ...page.runs[0], lifecycle_state: 'stale', liveness_state: 'not_responding',
          lifecycle_reason: reason, lifecycle_source: 'metrics', last_evidence_at: '2026-09-20T10:00:00Z' },
        { ...page.runs[0], run_id: 'outside-range', outcome_state: 'failed' },
      ],
      status: { metric_files: 0 }, cards: [], metric_options: [], chart: {}, summary: {},
    }));
    return Promise.resolve(otherResponse(url));
  }));
  renderWorkspace();
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  const operational = screen.getByRole('region', { name: 'Loaded run operational status' });
  expect(screen.getByText('loaded runs')).toHaveTextContent('1 loaded runs');
  expect(within(operational).getByRole('heading', { name: 'Needs attention' })).toBeVisible();
  expect(within(operational).getByText('active').textContent).toBe('0 active');
  expect(within(operational).getByText('stale').textContent).toBe('1 stale');
  expect(screen.getByTitle(reason)).toHaveTextContent('not responding');
  expect(screen.getByRole('button', { name: 'Not responding 1' })).toBeVisible();
  expect(screen.getByRole('button', { name: 'Running 0' })).toBeVisible();
  expect(screen.queryByRole('checkbox', { name: 'outside-range' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Not responding 1' }));
  expect(screen.getByRole('checkbox', { name: 'range-a-run' })).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Running 0' }));
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  const requests = vi.mocked(fetch).mock.calls.map(([input]) => new URL(String(input), 'http://localhost'));
  expect(requests.filter(url => url.pathname.endsWith('/snapshot'))).toHaveLength(1);
  expect(requests.filter(url => url.pathname.endsWith('/runs'))).toHaveLength(1);
});

function searchRun(overrides: Partial<RunSearchRun> = {}): RunSearchRun {
  return { ...page.runs[0], index_version: 'v0', lifecycle_state: 'running', successful: false, ...overrides };
}
function snapshotRun(overrides: Partial<RunView> = {}): RunView {
  return { ...page.runs[0], systems: [], observe_cli: '', lifecycle_state: 'stale',
    liveness_state: 'not_responding', ...overrides };
}

it('keeps the header aligned with loaded pages rather than totals or visible runs', async () => {
  vi.stubGlobal('innerWidth', 1280);
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    return Promise.resolve(json(url.searchParams.get('limit') === '200' ? page : {
      ...page, truncated: false, runs: [...page.runs, searchRun({ run_id: 'next-page-run' })],
    }));
  }));
  renderWorkspace();
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  expect(screen.getByText('loaded runs')).toHaveTextContent('1 loaded runs');
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await screen.findByRole('checkbox', { name: 'next-page-run' });
  expect(screen.getByText('loaded runs')).toHaveTextContent('2 loaded runs');
  expect(screen.getByText('2 loaded of 201 · 2 visible')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Hide listed' }));
  expect(screen.getByText('2 loaded of 201 · 0 visible')).toBeInTheDocument();
  expect(screen.getByText('loaded runs')).toHaveTextContent('2 loaded runs');
  fireEvent.change(screen.getByRole('searchbox', { name: 'Search runs' }), { target: { value: 'no-match' } });
  expect(screen.getByText('loaded runs')).toHaveTextContent('2 loaded runs');
  expect(vi.mocked(fetch).mock.calls.filter(([input]) => new URL(String(input), 'http://localhost').pathname.endsWith('/runs'))).toHaveLength(2);
});

it.each(['succeeded', 'failed', 'cancelled'])('preserves explicit %s outcome over liveness', outcome => {
  const [run] = reconcilePageRuns([searchRun()], [snapshotRun({ outcome_state: outcome,
    lifecycle_explicit: true, lifecycle_source: 'local_run_record', lifecycle_reason: 'explicit terminal outcome' })]);
  expect(runLifecycle(run)).toBe(outcome);
  expect(run).toMatchObject({ lifecycle_explicit: true, lifecycle_source: 'local_run_record',
    lifecycle_reason: 'explicit terminal outcome' });
});

it('preserves upstream snapshot timestamps and run details for matched page members', () => {
  const snapshot = snapshotRun({ updated_at: '2026-09-20T12:00:00Z', color: '#123456',
    systems: [{ name: 'GPU', value: 'A100', collection_state: 'available' }],
    configs: [{ config_hash: 'config', run_id: 'range-a-run', format: 'json', uri: 'config.json' }],
    observe_cli: 'tau experiment observe range-a-run', launch: { version: 1, workers: 2 } });
  const [run] = reconcilePageRuns([searchRun()], [snapshot]);
  expect(runTimestamp(run)).toBe(snapshot.updated_at);
  expect(run).toMatchObject(snapshot);
  expect(filterRuns([run], { search: '', group: '', lifecycle: '', updated: '1h', sort: '' },
    Date.parse('2026-09-20T12:30:00Z'))).toHaveLength(1);
});

it('preserves page order, search metrics and inputs with snapshot precedence only for matching identities', () => {
  const metrics = [{ run_id: 'range-a-run', metric_name: 'loss', count: 1, finite_count: 1,
    latest_value: 0.125, updated_at: '2026-09-20T10:00:00Z' }];
  const members = [
    searchRun({ run_id: 'unmatched' }),
    searchRun({ metrics, tags: { owner: 'page' }, success_reasons: ['page reason'] }),
    searchRun({ project: 'other-project' }),
    searchRun({ run_id: 'legacy', lifecycle_state: 'incomplete' }),
  ];
  const snapshot = snapshotRun({ owner: 'snapshot owner', state: 'stale', metric_names: ['other'],
    tags: { owner: 'snapshot' }, successful: true, success_reasons: ['snapshot reason'],
    lifecycle_reason: 'stale evidence', lifecycle_source: 'metrics',
    last_evidence_at: '2026-09-20T10:00:00Z', freshness_seconds: 1200,
    workload_absence_confirmed: false });
  const snapshots = [snapshot, snapshotRun({ run_id: 'outside-range' }),
    snapshotRun({ run_id: 'legacy', lifecycle_state: undefined, liveness_state: undefined })];
  const before = structuredClone({ members, snapshots });
  members.forEach(Object.freeze);
  snapshots.forEach(Object.freeze);
  const result = reconcilePageRuns(members, snapshots);
  expect(result.map(run => [run.project, run.run_id])).toEqual(members.map(run => [run.project, run.run_id]));
  expect(result[0]).toBe(members[0]);
  expect(result[2]).toBe(members[2]);
  expect(result[3].lifecycle_state).toBe('incomplete');
  expect(result[3].successful).toBe(false);
  expect(result[1]).toMatchObject({ ...snapshot, metrics, index_version: members[1].index_version });
  expect(result[1].metrics).toBe(metrics);
  expect({ members, snapshots }).toEqual(before);
  expect(reconcilePageRuns([], snapshots)).toEqual([]);
  expect(reconcilePageRuns(members, [])).toEqual(members);
});

it('reconciles the 1000-run page limit without mutating cached rows', () => {
  const members = Array.from({ length: 1000 }, (_, index) => Object.freeze(searchRun({ run_id: `run-${index}` })));
  const snapshots = members.map(run => Object.freeze(snapshotRun({ run_id: run.run_id }))).reverse();
  const durations = Array.from({ length: 100 }, () => {
    const started = performance.now();
    const result = reconcilePageRuns(members, snapshots);
    const elapsed = performance.now() - started;
    expect(result).toHaveLength(1000);
    expect(result.every((run, index) => run.run_id === members[index].run_id && runLifecycle(run) === 'not_responding')).toBe(true);
    return elapsed;
  }).sort((left, right) => left - right);
  expect(members.every(run => run.lifecycle_state === 'running')).toBe(true);
  console.info(`1000-run reconciliation: median=${durations[50].toFixed(3)}ms p95=${durations[95].toFixed(3)}ms (informational)`);
});

it.each(['snapshot-first', 'search-first'])('reconciles independently arriving responses: %s', async order => {
  let finishSnapshot: (response: Response) => void = () => {};
  let finishSearch: (response: Response) => void = () => {};
  const client = createPortalQueryClient();
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname.endsWith('/snapshot')) return new Promise<Response>(resolve => { finishSnapshot = resolve; });
    if (url.pathname.endsWith('/runs')) return new Promise<Response>(resolve => { finishSearch = resolve; });
    return Promise.resolve(otherResponse(url));
  }));
  renderWorkspace(client);
  const snapshot = { runs: [snapshotRun()], status: { metric_files: 0 }, cards: [], metric_options: [], chart: {}, summary: {} };
  await act(async () => {
    if (order === 'snapshot-first') finishSnapshot(json(snapshot));
    else finishSearch(json(page));
  });
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  await act(async () => {
    if (order === 'snapshot-first') finishSearch(json(page));
    else finishSnapshot(json(snapshot));
  });
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  expect(screen.getByRole('heading', { name: 'Needs attention' })).toBeVisible();
  await waitFor(() => expect(client.isFetching()).toBe(0));
  const snapshotQuery = client.getQueryCache().findAll().find(query => query.queryKey.some(part =>
    typeof part === 'string' && part.includes('/snapshot')));
  expect(snapshotQuery).toBeDefined();
  act(() => client.setQueryData(snapshotQuery!.queryKey, { ...snapshot,
    runs: [snapshotRun({ lifecycle_state: 'succeeded', outcome_state: 'succeeded', liveness_state: undefined })] }));
  expect(await screen.findByRole('heading', { name: 'No active runs' })).toBeVisible();
  expect(screen.getByRole('button', { name: 'Succeeded 1' })).toBeInTheDocument();
});

it.each([400, 403, 502])('hides retained runs when a different range is pending or fails with %s', async status => {
  let finish: (response: Response) => void = () => {};
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    if (url.searchParams.get('window') === '24h') return Promise.resolve(json(page));
    return new Promise<Response>(resolve => { finish = resolve; });
  }));
  renderWorkspace();
  expect(await screen.findByRole('checkbox', { name: 'range-a-run' })).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: '1h' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  expect(screen.queryByText('Range A warning')).not.toBeInTheDocument();
  expect(screen.getByText('0 loaded of 0 · 0 visible')).toBeInTheDocument();
  expect(screen.getByText('loaded runs')).toHaveTextContent('0 loaded runs');
  await act(async () => finish(json({ error: 'upstream failed' }, status)));
  expect(await screen.findByText(/More runs unavailable/)).not.toHaveTextContent('Showing stale data');
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Not responding 0' })).toBeInTheDocument();
  expect(screen.queryByRole('checkbox', { name: 'outside-range' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Retry loading runs' }));
  await act(async () => finish(json({ total: 0, runs: [] })));
  await waitFor(() => expect(screen.queryByText(/More runs unavailable/)).not.toBeInTheDocument());
  expect(screen.getByText('0 loaded of 0 · 0 visible')).toBeInTheDocument();
});

it('retains the same range page after a larger page fails and recovers on retry', async () => {
  let failed = true;
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    if (url.searchParams.get('limit') === '200') return Promise.resolve(json(page));
    return Promise.resolve(failed ? json({ error: 'page unavailable' }, 502)
      : json({ ...page, total: 1, truncated: false, warnings: [] }));
  }));
  renderWorkspace();
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await screen.findByText(/More runs unavailable/);
  expect(screen.getByRole('checkbox', { name: 'range-a-run' })).toBeInTheDocument();
  expect(screen.getByText('1 loaded of 201 · 1 visible')).toBeInTheDocument();
  expect(screen.getByRole('heading', { name: 'Needs attention' })).toBeVisible();
  expect(screen.getByRole('button', { name: 'Not responding 1' })).toBeInTheDocument();
  failed = false;
  fireEvent.click(screen.getByRole('button', { name: 'Retry loading runs' }));
  expect(await screen.findByText('1 loaded of 1 · 1 visible')).toBeInTheDocument();
  expect(screen.queryByText(/More runs unavailable/)).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Not responding 1' })).toBeInTheDocument();
});

it('preserves hidden runs and page size across range changes without remounting', async () => {
  const fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    return Promise.resolve(url.pathname.endsWith('/runs') ? json(page) : otherResponse(url));
  });
  vi.stubGlobal('fetch', fetch);
  renderWorkspace();
  fireEvent.click(await screen.findByRole('checkbox', { name: 'range-a-run' }));
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Load 200 more runs' })).toBeEnabled());
  const search = screen.getByRole('searchbox', { name: 'Search runs' });
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: '1h' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  expect(await screen.findByRole('checkbox', { name: 'range-a-run' })).not.toBeChecked();
  expect(screen.getByRole('searchbox', { name: 'Search runs' })).toBe(search);
  const request = fetch.mock.calls.map(([input]) => new URL(String(input), 'http://localhost'))
    .find(url => url.pathname.endsWith('/runs') && url.searchParams.get('window') === '1h');
  expect(request?.searchParams.get('limit')).toBe('400');
});

it('ignores a late page from the previous range', async () => {
  const client = createPortalQueryClient();
  let finishPrevious: ((response: Response) => void) | undefined;
  const currentPage = { total: 1, runs: [{ ...page.runs[0], run_id: 'range-b-run' }] };
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    if (url.searchParams.get('window') === '1h') return Promise.resolve(json(currentPage));
    if (url.searchParams.get('limit') === '200') return Promise.resolve(json(page));
    return new Promise<Response>(resolve => { finishPrevious = resolve; });
  }));
  renderWorkspace(client);
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await waitFor(() => expect(finishPrevious).toBeTypeOf('function'));
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: '1h' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  await screen.findByRole('checkbox', { name: 'range-b-run' });
  const previousResponse = json(page);
  const consumePrevious = vi.spyOn(previousResponse, 'json');
  await act(async () => finishPrevious!(previousResponse));
  await waitFor(() => expect(consumePrevious).toHaveBeenCalledOnce());
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  expect(screen.queryByText('Range A warning')).not.toBeInTheDocument();
  expect(screen.getByRole('checkbox', { name: 'range-b-run' })).toBeInTheDocument();
  expect(screen.getByText('1 loaded of 1 · 1 visible')).toBeInTheDocument();
});

it('preserves the requested interval and retained page on timezone-only navigation', async () => {
  const client = createPortalQueryClient();
  const fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    return Promise.resolve(url.searchParams.get('limit') === '200'
      ? json(page) : json({ error: 'page unavailable' }, 502));
  });
  vi.stubGlobal('fetch', fetch);
  renderWorkspace(client, '/portal/experiments?target=experiment&sections=&start=2026-09-16T00:00:00Z&end=2026-09-16T01:00:00Z&tz=local&refresh=off');
  fireEvent.click(await screen.findByRole('checkbox', { name: 'range-a-run' }));
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await screen.findByText(/More runs unavailable/);
  await waitFor(() => expect(client.isFetching()).toBe(0));
  const requests = fetch.mock.calls.map(([input]) => String(input));
  fireEvent.click(screen.getByRole('button', { name: 'Display UTC' }));
  await waitFor(() => expect(screen.getByLabelText('Timezone')).toHaveValue('utc'));
  expect(fetch.mock.calls.map(([input]) => String(input))).toEqual(requests);
  expect(screen.getByRole('checkbox', { name: 'range-a-run' })).not.toBeChecked();
  expect(screen.getByText('1 loaded of 201 · 0 visible')).toBeInTheDocument();
  const request = new URL(requests.find(url => url.includes('/runs'))!, 'http://localhost');
  expect(request.searchParams.get('start')).toBe('2026-09-16T00:00:00Z');
  expect(request.searchParams.get('end')).toBe('2026-09-16T01:00:00Z');
  expect(request.searchParams.has('tz')).toBe(false);
});