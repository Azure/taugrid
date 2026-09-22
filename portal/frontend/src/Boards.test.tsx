// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Overview } from './Boards';
import { TrackingLink } from './components';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { WorkspaceScope } from './types';

const scope: WorkspaceScope = {
  workspace: 'flex', name: 'Flex', cluster: 'taugrid-flex', namespace: '',
  source: 'portal', authorizationMode: 'cluster-wide', availability: 'available', managed: false,
};

function json(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('Platform overview', () => {
  it('shows GPU topology with allocated and available capacity', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({ cards: { queue: { admitted: 1, pending: 0, gpuUsed: 8, gpuHeadroom: 8 } }, running: [] }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json({
        readyNodes: 2, totalNodes: 2, totalGPUs: 16, gpuNodes: 2, gpuSchedulable: 16, gpuAvailable: 8,
        nodes: [
          { name: 'gpu-a', site: 'west', region: 'westus3', agentPool: 'h100', gpuCapacity: 8, gpuAllocated: 4, gpuAvailable: 4, gpuProduct: 'NVIDIA H100', ready: true },
          { name: 'gpu-b', site: 'east', region: 'eastus2', agentPool: 'h100', gpuCapacity: 8, gpuAllocated: 4, gpuAvailable: 4, gpuProduct: 'NVIDIA H100', ready: true },
          { name: 'system-a', site: 'west', region: 'westus3', agentPool: 'system', gpuCapacity: 0, gpuAllocated: 0, gpuAvailable: 0, ready: true },
        ],
      }));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json({ window: '15m0s', gpus: [] }));
      return Promise.resolve(json({}));
    }));

    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal?persona=platform']}>
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    expect(screen.getByRole('button', { name: /west/i })).toBeVisible();
    expect(screen.queryByText('system')).not.toBeInTheDocument();
    expect(screen.queryByText('Follow capacity from GPU sites through admission to active workloads.')).not.toBeInTheDocument();
    expect(screen.queryByRole('complementary', { name: 'Operational evidence' })).not.toBeInTheDocument();
    expect(screen.getAllByLabelText(/allocated GPUs and .* available GPUs/).length).toBeGreaterThan(0);
  });

  it('emits resolvable canonical run links for workload tracking', () => {
    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter>
      <WorkspaceProvider scope={scope} managed={false}>
        <TrackingLink run={{
          runId: 'train-77',
          experimentPath: '/stellar?target=train-77&project=vision',
          experimentTracking: 'available',
        }}/>
      </WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    const link = screen.getByRole('link', { name: 'open ↗' });
    expect(link).toHaveAttribute('href', '/portal/experiments?project=vision&run=train-77');
    expect(link).not.toHaveAttribute('href', expect.stringContaining('target='));
  });
});
