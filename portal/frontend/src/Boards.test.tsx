// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
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
  it('shows the current estimated GPU cost instead of GPU-hours', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({ cards: { queue: { admitted: 1, pending: 0, gpuUsed: 8, gpuHeadroom: 8 } }, running: [] }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json({
        readyNodes: 2, totalNodes: 2, totalGPUs: 16, gpuNodes: 2, gpuSchedulable: 16, gpuAvailable: 8,
        nodes: [
          { name: 'gpu-a', site: 'west', region: 'westus3', agentPool: 'h100', gpuCapacity: 8, gpuAvailable: 4, gpuProduct: 'NVIDIA H100', ready: true },
          { name: 'gpu-b', site: 'east', region: 'eastus2', agentPool: 'h100', gpuCapacity: 8, gpuAvailable: 4, gpuProduct: 'NVIDIA H100', ready: true },
        ],
      }));
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
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    const costBoard = await screen.findByLabelText('GPU cost');
    expect(await within(costBoard).findByText('Estimated cost')).toBeVisible();
    expect(within(costBoard).getByText('$321.09')).toBeVisible();
    expect(within(costBoard).queryByText('987.6')).not.toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    expect(screen.getByRole('button', { name: /west/i })).toBeVisible();
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
