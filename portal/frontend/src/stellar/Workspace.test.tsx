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

function run(runID: string, project: string, lifecycleState = 'succeeded', metricNames = ['train/loss']) {
  return {
    run_id: runID, experiment_id: 'exp-one', project, run_group_id: 'group-1',
    state: lifecycleState, lifecycle_state: lifecycleState, successful: lifecycleState === 'succeeded',
    success_reasons: [], owner: 'ada', created_at: '2026-09-21T18:00:00Z',
    started_at: '2026-09-21T18:01:00Z', completed_at: '2026-09-21T19:00:00Z',
    tags: {}, metric_names: metricNames, source: 'kusto',
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
    expect(requests).toHaveLength(4);
    expect(requests[0]).toContain('/experiments/search');
    expect(requests[1]).toContain('/experiments/exp-one/runs');
    expect(requests[2]).toContain('/runs/run-1');
    expect(requests[3]).toContain('/runs/run-1/metrics');

    await user.selectOptions(screen.getByLabelText('Metric'), 'train/loss');
    expect(await screen.findByRole('img', { name: 'train/loss series chart' })).toBeVisible();
    expect(requests).toHaveLength(5);
    expect(requests[4]).toContain('/runs/run-1/series');
    expect(requests[4]).toContain('metric=train%2Floss');
    expect(requests[4]).toContain('max_points=500');
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
});
