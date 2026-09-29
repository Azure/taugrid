// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createPortalQueryClient, WorkspaceProvider } from '../data';
import type { WorkspaceScope } from '../types';
import { StellarWorkspace } from './Workspace';

const baseScope: WorkspaceScope = {
  workspace: 'alpha', name: 'Alpha', cluster: 'cluster-a', namespace: 'tau',
  source: 'kusto', authorizationMode: 'workspace', availability: 'available', managed: false,
  experimentsNative: { state: 'available', apiBasePath: '/api/v2/stellar' },
};

function metadata(state = 'available', warnings: string[] = []) {
  return {
    generated_at: '2026-09-21T20:00:00Z',
    provenance: { requested_source: 'kusto', served_sources: ['kusto'] },
    freshness: { state: 'fresh', as_of: '2026-09-21T20:00:00Z' },
    availability: { state, reasons: state === 'available' ? [] : ['catalog is incomplete'] },
    partial: state === 'partial',
    warnings,
  };
}

function experiment(experimentID: string, project: string, name: string) {
  return {
    experiment_id: experimentID, project, name, description: '',
    created_at: '2026-09-21T18:00:00Z', updated_at: '2026-09-21T19:00:00Z',
    run_count: 1, run_group_count: 1, lifecycle_counts: { succeeded: 1 },
    latest_run_at: '2026-09-21T19:00:00Z', metric_names: ['train/loss'],
  };
}

function run(runID: string, project: string, lifecycleState = 'succeeded', metricNames = ['train/loss'], experimentID = 'exp-one') {
  return {
    run_id: runID, experiment_id: experimentID, project, run_group_id: 'group-1',
    state: lifecycleState, lifecycle_state: lifecycleState, successful: lifecycleState === 'succeeded',
    success_reasons: [], owner: 'ada', created_at: '2026-09-21T18:00:00Z',
    started_at: '2026-09-21T18:01:00Z', completed_at: '2026-09-21T19:00:00Z',
    tags: {}, metric_names: metricNames, source: 'kusto',
  };
}

function faultEvents(overrides: Record<string, unknown> = {}) {
  return {
    experimentId: 'e1',
    generatedAt: '2026-09-23T20:00:00Z',
    allocatedNodes: ['gpu-node-1'],
    missingNodes: [],
    timeBounds: { startedAt: '2026-09-23T18:00:00Z', active: true },
    coverage: {
      correlation: 'current-only', allocation: 'exact', timeBounds: 'exact',
      evidence: 'current-only', reasons: [],
    },
    provenance: {
      allocation: 'expstore run_context.node_names',
      evidence: 'current Kubernetes Node status.conditions',
      limitation: 'Node conditions are a current-state snapshot, not an event log',
    },
    events: [],
    ...overrides,
  };
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

function LocationView() {
  const location = useLocation();
  return <output aria-label="location">{location.pathname + location.search}</output>;
}

function renderWorkspace(initialEntry = '/portal/experiments', scope = baseScope) {
  const client = createPortalQueryClient();
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[initialEntry]}>
    <WorkspaceProvider scope={scope} managed={scope.managed}><StellarWorkspace/><LocationView/></WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('typed experiment dashboard', () => {
  it('navigates through the canonical endpoints within a five-request budget', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/series?')) return Promise.resolve(json({
        metadata: metadata(), target: 'exp-one', metric: 'train/loss', run_id: 'run-1',
        max_points: 500, source_points: 2, returned_points: 2,
        points: [{ step: 0, value: 2 }, { step: 1, value: 1 }],
      }));
      if (url.includes('/fault-events')) return Promise.resolve(json(faultEvents({ experimentId: 'exp-one' })));
      if (url.includes('/runs/run-1/metrics')) return Promise.resolve(json({
        metadata: metadata(), run_id: 'run-1', metrics: [{ name: 'train/loss', latest_step: 1, latest_value: 1 }],
      }));
      if (url.includes('/runs/run-1')) return Promise.resolve(json({ metadata: metadata(), run: run('run-1', 'vision') }));
      if (url.includes('/experiments/exp-one/runs')) return Promise.resolve(json({
        metadata: metadata(), target: 'exp-one', runs: [run('run-1', 'vision')],
      }));
      return Promise.resolve(json({
        metadata: metadata(), experiments: [experiment('exp-one', 'vision', 'One')],
      }));
    }));
    const user = userEvent.setup();
    renderWorkspace();

    await user.click(await screen.findByRole('button', { name: /One/ }));
    await user.click(await screen.findByRole('button', { name: 'run-1' }));
    expect((await screen.findAllByText('ada')).length).toBeGreaterThan(0);
    await screen.findByLabelText('Metric');
    expect(requests).toHaveLength(5);
    expect(requests[0]).toContain('/experiments/search');
    expect(requests[1]).toContain('/experiments/exp-one/runs');
    expect(requests[2]).toContain('/runs/run-1');
    expect(requests).toContainEqual(expect.stringContaining('/runs/run-1/metrics'));
    expect(requests).toContainEqual(expect.stringContaining('/experiments/exp-one/fault-events'));

    await user.selectOptions(screen.getByLabelText('Metric'), 'train/loss');
    expect(await screen.findByRole('img', { name: 'train/loss series chart' })).toBeVisible();
    expect(requests).toHaveLength(6);
    const seriesRequest = requests.find(url => url.includes('/series?'))!;
    expect(seriesRequest).toContain('metric=train%2Floss');
    expect(seriesRequest).toContain('max_points=500');
    expect(screen.getByLabelText('location')).toHaveTextContent('experiment=exp-one');
    expect(screen.getByLabelText('location')).toHaveTextContent('run=run-1');
    expect(screen.getByLabelText('location')).toHaveTextContent('metric=train%2Floss');
  });

  it('honors deep-linked filters and bounded series controls', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/series?')) return Promise.resolve(json({
        metadata: metadata(), target: 'e1', metric: 'loss', run_id: 'r1',
        start_step: 10, end_step: 20, step_interval: 2, max_points: 40,
        source_points: 2, returned_points: 2, points: [{ step: 10, value: 3 }, { step: 20, value: 2 }],
      }));
      if (url.includes('/runs/r1/metrics')) return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [{ name: 'loss' }] }));
      if (url.includes('/runs/r1')) return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'running', ['loss']) }));
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'running', ['loss'])] }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [
        experiment('e1', 'p', 'First'), experiment('e2', 'p', 'Second'),
      ] }));
    }));
    const user = userEvent.setup();
    renderWorkspace('/portal/experiments?project=p&experiment=e1&run=r1&metric=loss&filter=active&cursor=c2&start_step=10&end_step=20&step_interval=2&max_points=40');

    expect(await screen.findByRole('img', { name: 'loss series chart' })).toBeVisible();
    const seriesRequest = requests.find(url => url.includes('/series?'))!;
    expect(seriesRequest).toContain('start_step=10');
    expect(seriesRequest).toContain('end_step=20');
    expect(seriesRequest).toContain('step_interval=2');
    expect(seriesRequest).toContain('max_points=40');

    await user.click(await screen.findByRole('button', { name: /Second/ }));
    await waitFor(() => expect(screen.getByLabelText('location')).toHaveTextContent('experiment=e2'));
    const location = screen.getByLabelText('location').textContent!;
    expect(location).not.toContain('run=');
    expect(location).not.toContain('metric=');
    expect(location).not.toContain('filter=');
    expect(location).not.toContain('cursor=');
    expect(location).not.toContain('start_step=');
  });

  it('resolves a workload target bookmark into canonical experiment and run state', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/runs/workload-1/metrics')) return Promise.resolve(json({
        metadata: metadata(), run_id: 'workload-1', metrics: [],
      }));
      if (url.includes('/runs/workload-1')) return Promise.resolve(json({
        metadata: metadata(), run: run('workload-1', 'vision', 'running', [], 'experiment-7'),
      }));
      if (url.includes('/experiments/experiment-7/runs')) return Promise.resolve(json({
        metadata: metadata(), target: 'experiment-7',
        runs: [run('workload-1', 'vision', 'running', [], 'experiment-7')],
      }));
      return Promise.resolve(json({
        metadata: metadata(), experiments: [experiment('experiment-7', 'vision', 'Resolved experiment')],
      }));
    }));

    renderWorkspace('/portal/experiments?target=workload-1&project=vision');

    expect(await screen.findByText('No metrics are available for this run.')).toBeVisible();
    await waitFor(() => {
      const location = screen.getByLabelText('location').textContent!;
      expect(location).toContain('project=vision');
      expect(location).toContain('experiment=experiment-7');
      expect(location).toContain('run=workload-1');
      expect(location).not.toContain('target=');
    });
    expect(requests.some(url => url.includes('/runs/workload-1?') && !url.includes('target='))).toBe(true);
  });

  it('resolves a legacy experiment target without treating it as a run', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/experiments/experiment-7/runs')) return Promise.resolve(json({
        metadata: metadata(), target: 'experiment-7', runs: [],
      }));
      if (url.includes('/experiments/search') && url.includes('q=experiment-7')) return Promise.resolve(json({
        metadata: metadata(), experiments: [experiment('experiment-7', 'vision', 'Legacy bookmark')],
      }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [] }));
    }));

    renderWorkspace('/portal/experiments?target=experiment-7&project=vision');

    expect(await screen.findByText('No runs match this experiment and filter.')).toBeVisible();
    await waitFor(() => {
      const location = screen.getByLabelText('location').textContent!;
      expect(location).toContain('project=vision');
      expect(location).toContain('experiment=experiment-7');
      expect(location).not.toContain('target=');
    });
    expect(requests.some(url => url.includes('/runs/experiment-7'))).toBe(false);
  });

  it('checks every legacy search page before falling back to run resolution', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), window.location.origin);
      requests.push(url.pathname + url.search);
      if (url.pathname.endsWith('/experiments/search') && url.searchParams.get('q') === 'experiment-7') {
        if (url.searchParams.get('cursor') === 'page-2') {
          return Promise.resolve(json({
            metadata: metadata(), experiments: [experiment('experiment-7', 'vision', 'Legacy bookmark')],
          }));
        }
        return Promise.resolve(json({
          metadata: metadata(), experiments: [experiment('experiment-70', 'vision', 'Fuzzy match')],
          next_cursor: 'page-2',
        }));
      }
      if (url.pathname.endsWith('/experiments/experiment-7/runs')) {
        return Promise.resolve(json({ metadata: metadata(), target: 'experiment-7', runs: [] }));
      }
      return Promise.resolve(json({ metadata: metadata(), experiments: [] }));
    }));

    renderWorkspace('/portal/experiments?target=experiment-7&project=vision');

    expect(await screen.findByText('No runs match this experiment and filter.')).toBeVisible();
    expect(requests.some(url => url.includes('cursor=page-2'))).toBe(true);
    expect(requests.some(url => url.includes('/runs/experiment-7'))).toBe(false);
  });

  it('does not use cached run data when a legacy target is an experiment', async () => {
    const client = createPortalQueryClient();
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), window.location.origin);
      if (url.pathname.endsWith('/runs/r1')) {
        return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'running', [], 'e1') }));
      }
      if (url.pathname.endsWith('/experiments/search') && url.searchParams.get('q') === 'r1') {
        return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('r1', 'p', 'Experiment r1')] }));
      }
      if (url.pathname.endsWith('/experiments/e1/runs') || url.pathname.endsWith('/experiments/r1/runs')) {
        const target = url.pathname.includes('/experiments/r1/') ? 'r1' : 'e1';
        return Promise.resolve(json({ metadata: metadata(), target, runs: [] }));
      }
      return Promise.resolve(json({ metadata: metadata(), experiments: [] }));
    }));
    const first = render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/experiments?run=r1&project=p']}>
      <WorkspaceProvider scope={baseScope} managed={false}><StellarWorkspace/><LocationView/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);
    await waitFor(() => expect(screen.getByLabelText('location')).toHaveTextContent('experiment=e1'));
    first.unmount();

    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/experiments?target=r1&project=p']}>
      <WorkspaceProvider scope={baseScope} managed={false}><StellarWorkspace/><LocationView/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    await waitFor(() => expect(screen.getByLabelText('location')).toHaveTextContent('experiment=r1'));
    expect(screen.getByLabelText('location')).not.toHaveTextContent('experiment=e1');
  });

  it('clears a failed legacy target when selecting a search result', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), window.location.origin);
      if (url.pathname.endsWith('/runs/missing')) {
        return Promise.resolve(json({
          error: { code: 'NOT_FOUND', message: 'run was not found', classification: 'client', retryable: false },
        }, 404));
      }
      if (url.pathname.endsWith('/experiments/search') && url.searchParams.get('q') === 'missing') {
        return Promise.resolve(json({ metadata: metadata(), experiments: [] }));
      }
      if (url.pathname.endsWith('/experiments/e1/runs')) {
        return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [] }));
      }
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Search result')] }));
    }));
    const user = userEvent.setup();
    renderWorkspace('/portal/experiments?target=missing&project=p');

    expect(await screen.findByRole('alert')).toHaveTextContent('run was not found');
    await user.click(screen.getByRole('button', { name: /Search result/ }));

    expect(await screen.findByText('No runs match this experiment and filter.')).toBeVisible();
    const location = screen.getByLabelText('location').textContent!;
    expect(location).toContain('experiment=e1');
    expect(location).not.toContain('target=');
  });

  it('keeps experiment search visible when an unresolved run link returns not found', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/runs/missing-run')) return Promise.resolve(json({
        error: { code: 'NOT_FOUND', message: 'run was not found', classification: 'client', retryable: false },
      }, 404));
      return Promise.resolve(json({
        metadata: metadata(), experiments: [experiment('e1', 'p', 'Search remains')],
      }));
    }));

    renderWorkspace('/portal/experiments?run=missing-run&project=p');

    expect(await screen.findByRole('alert')).toHaveTextContent('run was not found');
    expect(screen.getByRole('button', { name: /Search remains/ })).toBeVisible();
  });

  it('preserves metric and range controls while resolving a run deep link', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/series?')) return Promise.resolve(json({
        metadata: metadata(), target: 'e1', metric: 'loss', run_id: 'r1',
        start_step: 10, end_step: 20, max_points: 40, source_points: 1, returned_points: 1,
        points: [{ step: 10, value: 3 }],
      }));
      if (url.includes('/runs/r1/metrics')) {
        return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [{ name: 'loss' }] }));
      }
      if (url.includes('/runs/r1')) {
        return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'running', ['loss'], 'e1') }));
      }
      if (url.includes('/experiments/e1/runs')) {
        return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'running', ['loss'], 'e1')] }));
      }
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Resolved')] }));
    }));

    renderWorkspace('/portal/experiments?project=p&run=r1&metric=loss&start_step=10&end_step=20&max_points=40');

    expect(await screen.findByRole('img', { name: 'loss series chart' })).toBeVisible();
    const location = screen.getByLabelText('location').textContent!;
    expect(location).toContain('experiment=e1');
    expect(location).toContain('metric=loss');
    expect(location).toContain('start_step=10');
    expect(location).toContain('end_step=20');
    expect(location).toContain('max_points=40');
    const seriesRequest = requests.find(url => url.includes('/series?'))!;
    expect(seriesRequest).toContain('start_step=10');
    expect(seriesRequest).toContain('end_step=20');
    expect(seriesRequest).toContain('max_points=40');
  });

  it('uses the canonical lifecycle filter for recognized run states', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/experiments/e1/runs')) {
        return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [] }));
      }
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Filtered')] }));
    }));
    const user = userEvent.setup();
    renderWorkspace('/portal/experiments?project=p&experiment=e1');
    await screen.findByText('No runs match this experiment and filter.');

    await user.type(screen.getByLabelText('Filter runs'), 'running');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(requests.some(url =>
      url.includes('/experiments/e1/runs') && url.includes('lifecycle=running'))).toBe(true));
    const filteredRequest = requests.find(url =>
      url.includes('/experiments/e1/runs') && url.includes('lifecycle=running'))!;
    expect(filteredRequest).not.toContain('q=running');
  });

  it('refreshes successful scoped reads and displays newly returned series points', async () => {
    let seriesReads = 0;
    const reads = { experiments: 0, runs: 0, detail: 0, catalog: 0 };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/series?')) {
        seriesReads++;
        const points = seriesReads === 1
          ? [{ step: 0, value: 2 }]
          : [{ step: 0, value: 2 }, { step: 1, value: 1 }];
        return Promise.resolve(json({
          metadata: metadata(), target: 'e1', metric: 'loss', run_id: 'r1',
          max_points: 500, source_points: points.length, returned_points: points.length, points,
        }));
      }
      if (url.includes('/fault-events')) return Promise.resolve(json(faultEvents()));
      if (url.includes('/runs/r1/metrics')) {
        reads.catalog++;
        return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [{ name: 'loss' }] }));
      }
      if (url.includes('/runs/r1')) {
        reads.detail++;
        return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'running', ['loss'], 'e1') }));
      }
      if (url.includes('/experiments/e1/runs')) {
        reads.runs++;
        return Promise.resolve(json({
          metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'running', ['loss'], 'e1')],
        }));
      }
      reads.experiments++;
      return Promise.resolve(json({
        metadata: metadata(), experiments: [experiment('e1', 'p', 'Refreshable')],
      }));
    }));
    const user = userEvent.setup();
    renderWorkspace('/portal/experiments?project=p&experiment=e1&run=r1&metric=loss');

    expect(await screen.findByText('loss · 1 points')).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Refresh experiment data' }));
    expect(await screen.findByText('loss · 2 points')).toBeVisible();
    expect(seriesReads).toBe(2);
    expect(reads).toEqual({ experiments: 2, runs: 2, detail: 2, catalog: 2 });
  });

  it('clears dependent state when the project changes for the same experiment ID', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/experiments/shared/runs')) return Promise.resolve(json({
        metadata: metadata(), target: 'shared', runs: [],
      }));
      return Promise.resolve(json({
        metadata: metadata(), experiments: [
          experiment('shared', 'project-a', 'Shared A'),
          experiment('shared', 'project-b', 'Shared B'),
        ],
      }));
    }));
    const user = userEvent.setup();
    renderWorkspace('/portal/experiments?project=project-a&experiment=shared&run=old-run&metric=loss&filter=active&cursor=c2&start_step=10&end_step=20&step_interval=2&max_points=40');

    await user.click(await screen.findByRole('button', { name: /Shared B/ }));
    await waitFor(() => expect(screen.getByLabelText('location')).toHaveTextContent('project=project-b'));
    const location = screen.getByLabelText('location').textContent!;
    expect(location).toContain('experiment=shared');
    expect(location).not.toContain('run=');
    expect(location).not.toContain('metric=');
    expect(location).not.toContain('filter=');
    expect(location).not.toContain('cursor=');
    expect(location).not.toContain('start_step=');
    expect(location).not.toContain('max_points=');
  });

  it('renders canonical API errors and partial availability explicitly', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({
        error: { code: 'DEPENDENCY_UNAVAILABLE', message: 'run store is offline', classification: 'dependency', retryable: true },
      }, 503));
      return Promise.resolve(json({
        metadata: metadata('partial', ['one shard timed out']),
        experiments: [experiment('e1', 'p', 'Partial experiment')],
      }));
    }));
    const user = userEvent.setup();
    renderWorkspace();

    expect(await screen.findByText(/Partial results/)).toBeVisible();
    expect(screen.getByText('one shard timed out')).toBeVisible();
    expect(screen.getByText('Source: kusto')).toBeVisible();
    await user.click(screen.getByRole('button', { name: /Partial experiment/ }));
    expect(await screen.findByRole('alert')).toHaveTextContent('run store is offline');
  });

  it('renders lifecycle-only runs without requesting or fabricating metric values', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/runs/lifecycle-1/metrics')) return Promise.resolve(json({
        metadata: metadata('unavailable'), run_id: 'lifecycle-1', metrics: [],
      }));
      if (url.includes('/runs/lifecycle-1')) return Promise.resolve(json({
        metadata: metadata(), run: run('lifecycle-1', 'p', 'failed', []),
      }));
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({
        metadata: metadata(), target: 'e1', runs: [run('lifecycle-1', 'p', 'failed', [])],
      }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Lifecycle only')] }));
    }));
    const user = userEvent.setup();
    renderWorkspace();

    await user.click(await screen.findByRole('button', { name: /Lifecycle only/ }));
    await user.click(await screen.findByRole('button', { name: 'lifecycle-1' }));
    expect(await screen.findByText('No metrics are available for this run.')).toBeVisible();
    expect(screen.getAllByText('failed').length).toBeGreaterThan(0);
    expect(requests.some(url => url.includes('/series?'))).toBe(false);
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
  });

  it('keys data by workspace identity and never paints the prior workspace response', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), window.location.origin);
      const workspace = url.searchParams.get('workspace');
      if (url.pathname.endsWith('/runs/r1/metrics')) return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [] }));
      if (url.pathname.endsWith('/runs/r1')) return Promise.resolve(json({
        metadata: metadata(), run: { ...run('r1', 'p', 'running', []), owner: workspace === 'beta' ? 'Beta owner' : 'Alpha owner' },
      }));
      if (url.pathname.endsWith('/runs')) return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'running', [])] }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', `${workspace} experiment`)] }));
    }));
    const client = createPortalQueryClient();
    const renderTree = (scope: WorkspaceScope) => <QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/experiments?experiment=e1&run=r1&project=p']}>
      <WorkspaceProvider scope={scope} managed><StellarWorkspace/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>;
    const rendered = render(renderTree({ ...baseScope, managed: true }));
    expect(await screen.findByText('Alpha owner')).toBeVisible();

    rendered.rerender(renderTree({ ...baseScope, workspace: 'beta', name: 'Beta', cluster: 'cluster-b', managed: true }));
    expect(await screen.findByText('Beta owner')).toBeVisible();
    expect(screen.queryByText('Alpha owner')).not.toBeInTheDocument();
  });

  it('shows a correlated unhealthy condition with node-scoped evidence details', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/fault-events')) return Promise.resolve(json(faultEvents({
        events: [{
          dedupKey: 'gpu-node-1/gpu/XIDError79', node: 'gpu-node-1', scope: 'node',
          category: 'gpu', checkType: 'XIDError79', healthState: 'unhealthy',
          status: 'True', evidenceStatus: 'fresh', reason: 'XIDDetected',
          message: 'GPU reported XID 79', observedAt: '2026-09-23T19:59:00Z',
          transitionAt: '2026-09-23T19:58:00Z',
        }],
      })));
      if (url.includes('/runs/r1/metrics')) return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [] }));
      if (url.includes('/runs/r1')) return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'running', [], 'e1') }));
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'running', [], 'e1')] }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Faulted')] }));
    }));

    renderWorkspace('/portal/experiments?project=p&experiment=e1&run=r1');

    expect(await screen.findByText('XIDError79')).toBeVisible();
    expect(screen.getByText(/GPU reported XID 79/)).toBeVisible();
    expect(screen.getByLabelText('Correlated health summary')).toHaveTextContent('1 unhealthy');
    expect(screen.getByText('Evidence: current-only')).toBeVisible();
    expect(screen.getByText(/Node-scoped conditions correlated/)).toBeVisible();
  });

  it('treats an empty current snapshot as an evidence gap rather than clean history', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/fault-events')) return Promise.resolve(json(faultEvents({
        coverage: {
          correlation: 'unknown', allocation: 'exact', timeBounds: 'exact',
          evidence: 'unknown', reasons: ['allocated nodes have no allowlisted conditions'],
        },
      })));
      if (url.includes('/runs/r1/metrics')) return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [] }));
      if (url.includes('/runs/r1')) return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'running', [], 'e1') }));
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'running', [], 'e1')] }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Empty evidence')] }));
    }));

    renderWorkspace('/portal/experiments?project=p&experiment=e1&run=r1');

    expect(await screen.findByText(/evidence gap, not a verified clean history/)).toBeVisible();
    expect(screen.getByText('allocated nodes have no allowlisted conditions')).toBeVisible();
    expect(screen.getByLabelText('Correlated health summary')).toHaveTextContent('0 unhealthy');
  });

  it.each([
    ['current-only', faultEvents({
      timeBounds: { startedAt: '2026-09-22T18:00:00Z', completedAt: '2026-09-22T19:00:00Z', active: false },
      coverage: {
        correlation: 'current-only', allocation: 'exact', timeBounds: 'exact',
        evidence: 'current-only', reasons: ['completed experiments have no historical Node-condition log'],
      },
    }), /Current snapshot only—not an experiment timeline/],
    ['unavailable', faultEvents({
      allocatedNodes: [], timeBounds: { active: false },
      coverage: {
        correlation: 'unavailable', allocation: 'unavailable', timeBounds: 'unknown',
        evidence: 'unavailable', reasons: ['experiment run allocation is unavailable'],
      },
    }), /Correlated health evidence is unavailable/],
  ])('makes %s historical evidence limitations explicit', async (_state, faults, expected) => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/fault-events')) return Promise.resolve(json(faults));
      if (url.includes('/runs/r1/metrics')) return Promise.resolve(json({ metadata: metadata(), run_id: 'r1', metrics: [] }));
      if (url.includes('/runs/r1')) return Promise.resolve(json({ metadata: metadata(), run: run('r1', 'p', 'succeeded', [], 'e1') }));
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({ metadata: metadata(), target: 'e1', runs: [run('r1', 'p', 'succeeded', [], 'e1')] }));
      return Promise.resolve(json({ metadata: metadata(), experiments: [experiment('e1', 'p', 'Completed')] }));
    }));

    renderWorkspace('/portal/experiments?project=p&experiment=e1&run=r1');

    expect(await screen.findByText(expected)).toBeVisible();
  });
});
