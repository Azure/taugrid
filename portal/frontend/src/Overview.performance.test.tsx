// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Overview } from './Overview';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { Nodes, WorkspaceScope } from './types';

const scope: WorkspaceScope = {
  workspace: 'scale', name: 'Scale', cluster: 'scale-west', namespace: '',
  source: 'portal', authorizationMode: 'cluster-wide', availability: 'available', managed: false,
};

function json(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

function scaledNodes(): Nodes {
  const nodes = Array.from({ length: 64 }, (_, index) => ({
    name: `gpu-${index}`,
    site: index < 32 ? 'west' : 'east',
    region: index < 32 ? 'westus3' : 'eastus2',
    agentPool: index % 2 ? 'h200-b' : 'h200-a',
    sku: 'Standard_ND96isr_H200_v5',
    cpuCores: 96,
    memoryGiB: 1900,
    gpuCapacity: 8,
    gpuAllocated: index % 8,
    gpuAvailable: 8 - (index % 8),
    gpuProduct: 'NVIDIA H200',
    ready: true,
  }));
  return {
    totalNodes: nodes.length,
    readyNodes: nodes.length,
    gpuNodes: nodes.length,
    totalGPUs: nodes.length * 8,
    gpuAllocatable: nodes.length * 8,
    gpuSchedulable: nodes.length * 8,
    gpuAllocated: nodes.reduce((sum, node) => sum + node.gpuAllocated, 0),
    gpuAvailable: nodes.reduce((sum, node) => sum + node.gpuAvailable, 0),
    gpuAllocationKnown: true,
    totalCPUCores: nodes.length * 96,
    totalMemoryGiB: nodes.length * 1900,
    skus: [],
    nodes,
  };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('Overview performance guardrails', () => {
  it('bounds topology DOM volume while preserving site selection detail', async () => {
    const nodes = scaledNodes();
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({
        cards: { queue: { admitted: 0, pending: 0, gpuUsed: 0, gpuHeadroom: 512, queues: [] } },
        pending: [], active: [], waiting: [], running: [],
      }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(nodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json({ window: '15m', gpus: [] }));
      return Promise.resolve(json({}));
    }));

    const client = createPortalQueryClient();
    const rendered = render(<QueryClientProvider client={client}><MemoryRouter>
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    expect(rendered.container.querySelectorAll('*')).toHaveLength(366);
    expect(rendered.container.querySelectorAll('.overview-gpu-tiles')).toHaveLength(32);
    expect(rendered.container.querySelectorAll('.overview-gpu-tiles > *')).toHaveLength(0);

    await userEvent.click(screen.getByRole('button', { name: /east/i }));
    expect(screen.getByText('gpu-63')).toBeVisible();
    expect(rendered.container.querySelectorAll('.overview-gpu-tiles')).toHaveLength(32);
    expect(screen.getByLabelText('gpu-63: 7 allocated GPUs and 1 available GPUs')).toBeVisible();
  });

  it('renders usable overview data before secondary sources finish', async () => {
    let resolveNodes: ((response: Response) => void) | undefined;
    let resolveCluster: ((response: Response) => void) | undefined;
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({
        cards: { queue: { admitted: 1, pending: 0, gpuUsed: 8, gpuHeadroom: 8, queues: [] } },
        pending: [], active: [], waiting: [], running: [],
      }));
      if (url.includes('/api/portal/nodes')) return new Promise<Response>(resolve => { resolveNodes = resolve; });
      if (url.includes('/api/portal/cluster')) return new Promise<Response>(resolve => { resolveCluster = resolve; });
      return Promise.resolve(json({}));
    }));

    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter>
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    expect(screen.getByText('Queue pressure').parentElement).toHaveTextContent('8/16');
    expect(screen.getByLabelText('Infrastructure overview source freshness')).toHaveTextContent('Fleet capacity: loading');

    resolveNodes?.(json(scaledNodes()));
    resolveCluster?.(json({ window: '15m', gpus: [] }));
    expect(await screen.findByText('gpu-63')).toBeVisible();
  });

  it('refreshes all Overview sources on the 15-second cadence', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({
        cards: { queue: { admitted: 0, pending: 0, gpuUsed: 0, gpuHeadroom: 512, queues: [] } },
        pending: [], active: [], waiting: [], running: [],
      }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(scaledNodes()));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json({ window: '15m', gpus: [] }));
      return Promise.resolve(json({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter>
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(fetchMock).toHaveBeenCalledTimes(3);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    expect(fetchMock).toHaveBeenCalledTimes(6);
  });
});
