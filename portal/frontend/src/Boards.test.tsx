// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Kueue, Overview } from './Boards';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { WorkspaceScope } from './types';

const researchScope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'research',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: false,
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
