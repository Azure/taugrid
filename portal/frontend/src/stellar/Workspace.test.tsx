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

describe('thin experiment dashboard', () => {
  it('navigates through the fixed composition within a five-request budget', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/series?')) return Promise.resolve(json({
        metadata: { availability: { state: 'available' }, provenance: { served_sources: ['kusto'] } },
        metric: 'train/loss', run_id: 'run-1',
        points: [[0, 2], [1, 1]],
      }));
      if (url.includes('/runs/run-1/metrics')) return Promise.resolve(json({
        metadata: { availability: { state: 'available' } }, run_id: 'run-1',
        metrics: [{ name: 'train/loss', unit: 'scalar' }],
      }));
      if (url.includes('/runs/run-1')) return Promise.resolve(json({
        metadata: { availability: { state: 'available' } },
        run: { run_id: 'run-1', project: 'vision', state: 'succeeded', owner: 'ada' },
      }));
      if (url.includes('/experiments/exp-one/runs')) return Promise.resolve(json({
        metadata: { availability: { state: 'available' } },
        runs: [{ run_id: 'run-1', state: 'succeeded', metric_names: ['train/loss'] }],
      }));
      return Promise.resolve(json({
        metadata: { availability: { state: 'available' } },
        experiments: [{ experiment_id: 'exp-one', project: 'vision', name: 'One', run_count: 1 }],
      }));
    }));
    const user = userEvent.setup();
    renderWorkspace();

    await user.click(await screen.findByRole('button', { name: /One/ }));
    await user.click(await screen.findByRole('button', { name: 'run-1' }));
    expect(await screen.findByText('ada')).toBeVisible();
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

  it('honors deep-linked filters, cursor, range, and clears dependent state on experiment changes', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/series?')) return Promise.resolve(json({ metric: 'loss', run_id: 'r1', points: [[10, 3], [20, 2]] }));
      if (url.includes('/runs/r1/metrics')) return Promise.resolve(json({ run_id: 'r1', metrics: ['loss'] }));
      if (url.includes('/runs/r1')) return Promise.resolve(json({ run: { run_id: 'r1', project: 'p', state: 'running' } }));
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({ runs: [{ run_id: 'r1', state: 'running' }] }));
      return Promise.resolve(json({ experiments: [
        { experiment_id: 'e1', project: 'p', name: 'First' },
        { experiment_id: 'e2', project: 'p', name: 'Second' },
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

  it('renders migration errors and partial availability explicitly', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/experiments/e1/runs')) return Promise.resolve(json({ error: { code: 'store_down', message: 'run store is offline' } }, 503));
      return Promise.resolve(json({
        availability: 'partial', partial: true, provenance: 'ADX', freshness: '2026-09-18T10:00:00Z',
        warnings: ['one shard timed out'], experiments: [{ experiment_id: 'e1', project: 'p', name: 'Partial experiment' }],
      }));
    }));
    const user = userEvent.setup();
    renderWorkspace();

    expect(await screen.findByText(/Partial results/)).toBeVisible();
    expect(screen.getByText('one shard timed out')).toBeVisible();
    expect(screen.getByText('Source: ADX')).toBeVisible();
    await user.click(screen.getByRole('button', { name: /Partial experiment/ }));
    expect(await screen.findByRole('alert')).toHaveTextContent('run store is offline');
  });

  it('keys data by workspace identity and never paints the prior workspace response', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), window.location.origin);
      const workspace = url.searchParams.get('workspace');
      if (url.pathname.endsWith('/runs/r1/metrics')) return Promise.resolve(json({ run_id: 'r1', metrics: [] }));
      if (url.pathname.endsWith('/runs/r1')) return Promise.resolve(json({
        run: { run_id: 'r1', project: 'p', state: 'running', owner: workspace === 'beta' ? 'Beta owner' : 'Alpha owner' },
      }));
      if (url.pathname.endsWith('/runs')) return Promise.resolve(json({ runs: [{ run_id: 'r1', state: 'running' }] }));
      return Promise.resolve(json({ experiments: [{ experiment_id: 'e1', project: 'p', name: `${workspace} experiment` }] }));
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
