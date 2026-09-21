// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CostBoard, Kueue, Overview } from './Boards';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { WorkspaceScope } from './types';

const researchScope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'research',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: false,
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

it('initializes Cost custom requests to the previous complete UTC hour', async () => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-21T12:34:56.789Z'));
  const fetch = vi.fn(() => Promise.resolve(json({ workspaces: [], idleGPUs: [] })));
  vi.stubGlobal('fetch', fetch);
  render(<QueryClientProvider client={createPortalQueryClient()}><MemoryRouter initialEntries={['/portal/cost?window=168h']}>
    <WorkspaceProvider scope={researchScope} managed={false}><CostBoard/></WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
  await waitFor(() => expect(fetch).toHaveBeenCalledOnce());
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: 'custom' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  const calls = vi.mocked(globalThis.fetch).mock.calls;
  const request = new URL(String(calls[1][0]), 'http://localhost');
  expect(request.searchParams.get('start')).toBe('2026-09-21T11:00:00.000Z');
  expect(request.searchParams.get('end')).toBe('2026-09-21T12:00:00.000Z');
});

it('blocks non-hour Cost drafts before fetching and recovers after correction', async () => {
  const fetch = vi.fn(() => Promise.resolve(json({ workspaces: [], idleGPUs: [] })));
  vi.stubGlobal('fetch', fetch);
  render(<QueryClientProvider client={createPortalQueryClient()}><MemoryRouter initialEntries={['/portal/cost?window=168h']}>
    <WorkspaceProvider scope={researchScope} managed={false}><CostBoard/></WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
  await waitFor(() => expect(fetch).toHaveBeenCalledOnce());
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: 'custom' } });
  fireEvent.change(screen.getByLabelText('Timezone'), { target: { value: 'utc' } });
  fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-21T11:30' } });
  fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-09-21T12:00' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  expect(screen.getByRole('alert')).toHaveTextContent('whole UTC hours');
  expect(fetch).toHaveBeenCalledOnce();
  fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-21T11:00' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
});

it('recovers a directly loaded Cost 400 without rounding a nanosecond bound', async () => {
  const fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    return Promise.resolve(url.searchParams.get('start')?.includes('000000001')
      ? new Response(JSON.stringify({ error: 'Cost ranges must start and end on whole UTC hours.' }), { status: 400, headers: { 'Content-Type': 'application/json' } })
      : json({ workspaces: [], idleGPUs: [] }));
  });
  vi.stubGlobal('fetch', fetch);
  render(<QueryClientProvider client={createPortalQueryClient()}><MemoryRouter initialEntries={['/portal/cost?start=2026-09-21T11:00:00.000000001Z&end=2026-09-21T12:00:00Z&tz=utc']}>
    <WorkspaceProvider scope={researchScope} managed={false}><CostBoard/></WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
  await screen.findByText(/Cost board unavailable/);
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  expect(within(screen.getByRole('region', { name: 'Historical time range' })).getByRole('alert')).toHaveTextContent('whole UTC hours');
  expect(fetch).toHaveBeenCalledOnce();
  fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-21T10:00:00.000' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  await screen.findByText(/No allocation records/);
  expect(fetch).toHaveBeenCalledTimes(2);
});

describe('Kueue board', () => {
  it('explains that the optional live dashboard is not installed without masking Scheduler', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 503 }))));
    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/kueueviz']}>
      <WorkspaceProvider scope={researchScope} managed={false}><Kueue/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByText('Optional KueueViz is not installed on this cluster.')).toBeVisible();
    expect(screen.getByRole('link', { name: 'Open Scheduler' })).toHaveAttribute('href', '/portal/jobs?view=scheduler');
    expect(screen.queryByText('The Kueue (Live) board unavailable.')).not.toBeInTheDocument();
  });
});

const flexScope: WorkspaceScope = {
  workspace: 'flex', name: 'Flex', cluster: 'taugrid-flex', namespace: '',
  source: 'portal', authorizationMode: 'cluster-wide', availability: 'available', managed: false,
};

function json(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

describe('Platform overview', () => {
  it('shows the current estimated GPU cost instead of GPU-hours', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({ cards: { queue: { admitted: 1, pending: 0, gpuUsed: 8, gpuHeadroom: 8 } }, running: [] }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json({ readyNodes: 2, totalNodes: 2, totalGPUs: 16, gpuNodes: 2 }));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json({ window: '15m0s', gpus: [] }));
      if (url.includes('/api/portal/cost')) return Promise.resolve(json({
        window: '168h0m0s',
        totalGPUHours: 987.6,
        totalEstimatedCostUSD: 321.09,
        costAvailable: true,
        gpuHoursAvailable: true,
        costCoverage: { observedSamples: 24, gpuHoursSamples: 24, costSamples: 24, utilizationSamples: 0 },
        idleAvailable: false,
        idleCoverage: { observedGPUs: 0, measuredGPUs: 0, eligibleGPUs: 0, observedSamples: 0, validSamples: 0 },
        workspaces: [],
        idleGPUs: [],
      }));
      return Promise.resolve(json({}));
    }));

    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal?persona=platform']}>
      <WorkspaceProvider scope={flexScope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    const costBoard = await screen.findByLabelText('GPU cost');
    expect(await within(costBoard).findByText('Estimated cost')).toBeVisible();
    expect(within(costBoard).getByText('$321.09')).toBeVisible();
    expect(within(costBoard).queryByText('987.6')).not.toBeInTheDocument();
  });
});
