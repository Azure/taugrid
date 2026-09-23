// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
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
  cleanup();
  vi.unstubAllGlobals();
});

describe('Platform overview', () => {
  it('shows GPU topology with allocated and available capacity', async () => {
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({ cards: { queue: {
        admitted: 3, pending: 1, gpuUsed: 8, gpuHeadroom: 8,
        queues: [
          { namespace: 'tau-default', queue: 'cpu', clusterQueue: 'tau-cpu-cq', admitted: 2, pending: 0 },
          { namespace: 'tau-default', queue: 'jobqueue', clusterQueue: 'tau-cq', admitted: 1, pending: 0 },
          { namespace: 'aks-ai-runtime-e2e', queue: 'jobqueue', clusterQueue: 'tau-cq', admitted: 0, pending: 1 },
        ],
      } }, running: [
        { name: 'gpu-service', namespace: 'tau-default', queue: 'jobqueue', clusterQueue: 'tau-cq' },
        { name: 'cpu-viewer', namespace: 'tau-default', queue: 'cpu', clusterQueue: 'tau-cpu-cq' },
      ] }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json({
        readyNodes: 2, totalNodes: 2, totalGPUs: 16, gpuNodes: 2, gpuSchedulable: 16, gpuAvailable: 8,
        nodes: [
          { name: 'gpu-a', site: 'west', region: 'westus3', agentPool: 'h100', gpuCapacity: 8, gpuAllocated: 4, gpuAvailable: 4, gpuProduct: 'NVIDIA H100', ready: true },
          { name: 'gpu-a2', site: 'west', region: 'westus3', agentPool: 'h100', gpuCapacity: 8, gpuAllocated: 8, gpuAvailable: 0, gpuProduct: 'NVIDIA H100', ready: true },
          { name: 'gpu-b', site: 'east', region: 'eastus2', agentPool: 'h100', gpuCapacity: 8, gpuAllocated: 4, gpuAvailable: 4, gpuProduct: 'NVIDIA H100', ready: true },
          { name: 'system-a', site: 'west', region: 'westus3', agentPool: 'system', gpuCapacity: 0, gpuAllocated: 0, gpuAvailable: 0, ready: true },
        ],
      }));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json({ window: '15m0s', gpus: [] }));
      return Promise.resolve(json({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal?persona=platform']}>
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    expect(screen.getByRole('button', { name: /west/i })).toBeVisible();
    expect(screen.queryByText('system')).not.toBeInTheDocument();
    expect(screen.queryByText('Follow capacity from GPU sites through admission to active workloads.')).not.toBeInTheDocument();
    expect(screen.getByText('Admitted workloads by resource')).toBeInTheDocument();
    expect(screen.getByText('GPU quota admitted')).toBeInTheDocument();
    expect(screen.getByText('CPU quota admitted')).toBeInTheDocument();
    expect(screen.getByText('gpu-service')).toBeInTheDocument();
    expect(screen.getByText('cpu-viewer')).toBeInTheDocument();
    expect(screen.getByText('Admission reserves quota; it does not prove that the workload is running.')).toBeInTheDocument();
    expect(screen.queryByText('Active work')).not.toBeInTheDocument();
    expect(screen.getByText('CPU queue')).toBeInTheDocument();
    expect(screen.getAllByText('GPU queue')).toHaveLength(2);
    expect(screen.getByText('tau-default/cpu')).toBeInTheDocument();
    expect(screen.queryByRole('complementary', { name: 'Operational evidence' })).not.toBeInTheDocument();
    expect(screen.getAllByLabelText(/allocated GPUs and .* available GPUs/).length).toBeGreaterThan(0);
    expect(screen.getByText('gpu-a')).toBeVisible();
    expect(screen.getByText('gpu-a2')).toBeVisible();
    expect(screen.getByLabelText('gpu-a: 4 allocated GPUs and 4 available GPUs')).toBeVisible();
    expect(screen.getByText('Infrastructure overview: Auto-refreshing every 15 seconds.')).toBeVisible();
    expect(screen.getByLabelText('Infrastructure overview source freshness')).toHaveTextContent('Fleet capacity');
    expect(screen.getByLabelText('Infrastructure overview source freshness')).toHaveTextContent('GPU telemetry');

    await userEvent.click(screen.getByRole('button', { name: 'Refresh Infrastructure overview' }));
    await waitFor(() => {
      expect(fetchMock.mock.calls.filter(([input]) => String(input).includes('/api/portal/overview'))).toHaveLength(2);
      expect(fetchMock.mock.calls.filter(([input]) => String(input).includes('/api/portal/nodes'))).toHaveLength(2);
      expect(fetchMock.mock.calls.filter(([input]) => String(input).includes('/api/portal/cluster'))).toHaveLength(2);
    });
  });

  it('keeps stale source data visible when a coordinated refresh fails', async () => {
    let clusterRequests = 0;
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json({
        cards: { queue: { admitted: 0, pending: 0, gpuUsed: 0, gpuHeadroom: 0, queues: [] } },
        running: [],
      }));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json({
        readyNodes: 1, totalNodes: 1, totalGPUs: 8, gpuNodes: 1, gpuSchedulable: 8, gpuAvailable: 8,
        nodes: [{ name: 'gpu-a', site: 'west', region: 'westus3', agentPool: 'h100', gpuCapacity: 8, gpuAllocated: 0, gpuAvailable: 8, ready: true }],
      }));
      if (url.includes('/api/portal/cluster')) {
        clusterRequests += 1;
        return clusterRequests === 1
          ? Promise.resolve(json({ window: '15m0s', gpus: [{ healthy: true }] }))
          : Promise.resolve(new Response('telemetry backend unavailable', { status: 503 }));
      }
      return Promise.resolve(json({}));
    }));

    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal?persona=platform']}>
      <WorkspaceProvider scope={scope} managed={false}><Overview persona="platform"/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    expect(screen.getByText('unhealthy of 1 observed')).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Refresh Infrastructure overview' }));
    expect(await screen.findByText(/GPU telemetry refresh failed: 503 telemetry backend unavailable/)).toBeVisible();
    expect(screen.getByLabelText('Infrastructure overview source freshness')).toHaveTextContent('GPU telemetry: refresh failed; stale data retained');
    expect(screen.getByText('unhealthy of 1 observed')).toBeVisible();
    expect(screen.getByText('gpu-a')).toBeVisible();
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
