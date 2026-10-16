// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Profiler, type ProfilerOnRenderCallback, type ReactNode } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Fleet } from './Fleet';
import { Overview } from './Overview';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import { fleetNodes } from './test/fleet-fixtures';
import type { Cluster, Nodes, NodeUtil, Overview as OverviewData, WorkspaceScope } from './types';

const scope: WorkspaceScope = {
  workspace: 'performance', name: 'Performance', cluster: 'performance-cluster', namespace: '',
  source: 'portal', authorizationMode: 'cluster-wide', availability: 'available', managed: false,
};

function json(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

function largeFleet(nodeCount = 96, siteCount = 12) {
  const nodes: Nodes['nodes'] = Array.from({ length: nodeCount }, (_, index) => {
    const template = fleetNodes.nodes[index % fleetNodes.nodes.length];
    const site = `site-${String(index % siteCount).padStart(2, '0')}`;
    return {
      ...template,
      name: `gpu-node-${String(index).padStart(3, '0')}`,
      agentPool: `pool-${index % 4}`,
      site,
      region: `region-${index % 3}`,
      zone: `zone-${index % 6}`,
      gpuCapacity: 8,
      gpuAllocatable: 8,
      gpuAllocated: index % 3,
      gpuAvailable: 8 - (index % 3),
    };
  });
  const inventory: Nodes = {
    ...fleetNodes,
    scope,
    totalNodes: nodeCount,
    readyNodes: nodeCount,
    gpuNodes: nodeCount,
    totalGPUs: nodeCount * 8,
    gpuAllocatable: nodeCount * 8,
    gpuSchedulable: nodeCount * 8,
    gpuAllocated: nodes.reduce((sum, node) => sum + (node.gpuAllocated || 0), 0),
    gpuAvailable: nodes.reduce((sum, node) => sum + (node.gpuAvailable || 0), 0),
    rdmaAdvertisedGpuNodes: nodeCount,
    totalCPUCores: nodeCount * 40,
    totalMemoryGiB: nodeCount * 320,
    nodes,
  };
  const telemetry: Cluster = {
    window: '15m0s',
    totalGPUs: nodeCount,
    errorGPUs: 0,
    telemetryAvailable: true,
    utilizationObservedGPUs: nodeCount,
    healthObservedGPUs: nodeCount,
    unknownHealthGPUs: 0,
    models: [{ modelName: 'NVIDIA H200', gpus: nodeCount }],
    gpus: nodes.map((node, index) => ({
      cluster: scope.cluster,
      instance: node.name,
      gpu: '0',
      modelName: 'NVIDIA H200',
      utilizationPct: index % 100,
      temperatureCelsius: 50 + (index % 20),
      powerWatts: 400,
      memoryUsedMB: 32_768,
      memoryFreeMB: 111_104,
      correctableRemappedRows: 0,
      uncorrectableRemappedRows: 0,
      rowRemapFailure: 0,
      healthy: true,
    })),
  };
  const nodeUtil: NodeUtil = {
    window: '15m0s',
    queriedAt: '2026-09-14T20:03:30Z',
    availability: 'ready',
    nodes: nodes.map((node, index) => ({
      cluster: scope.cluster,
      instance: node.name,
      cpuCores: node.cpuCores,
      cpuUtilPct: index % 100,
      memUsedPct: 50,
      memTotalBytes: 320 * 1024 ** 3,
      memAvailBytes: 160 * 1024 ** 3,
      cpuCoverage: {
        samples: 20,
        observedCores: node.cpuCores,
        usableCores: node.cpuCores,
        observedSeconds: 560,
        windowCoveragePct: 93,
        counterResets: 0,
      },
    })),
  };
  return { inventory, telemetry, nodeUtil };
}

function overviewData(): OverviewData {
  return {
    cards: {
      queue: {
        admitted: 10,
        pending: 10,
        gpuUsed: 80,
        gpuHeadroom: 80,
        queues: Array.from({ length: 20 }, (_, index) => ({
          namespace: `namespace-${index}`,
          queue: `queue-${index}`,
          clusterQueue: `cluster-queue-${index}`,
          admitted: 1,
          pending: 1,
        })),
      },
    },
    pending: Array.from({ length: 20 }, (_, index) => ({
      name: `pending-${index}`,
      namespace: 'performance',
      queue: 'gpu',
      gpuRequested: 8,
    })),
    active: Array.from({ length: 20 }, (_, index) => ({
      name: `active-${index}`,
      namespace: 'performance',
      kind: 'Job',
      status: 'Running',
      age: '1m',
    })),
    waiting: Array.from({ length: 20 }, (_, index) => ({
      name: `waiting-${index}`,
      namespace: 'performance',
      queue: 'gpu',
      clusterQueue: 'gpu',
    })),
    running: Array.from({ length: 20 }, (_, index) => ({
      name: `running-${index}`,
      namespace: 'performance',
      queue: index % 2 ? 'gpu' : 'cpu',
      clusterQueue: index % 2 ? 'gpu' : 'tau-cpu-cq',
    })),
  };
}

function renderMeasured(children: ReactNode) {
  let commits = 0;
  const onRender: ProfilerOnRenderCallback = () => {
    commits += 1;
  };
  const client = createPortalQueryClient();
  const rendered = render(<QueryClientProvider client={client}><MemoryRouter>
    <WorkspaceProvider scope={scope} managed={false}>
      <Profiler id="performance-guardrail" onRender={onRender}>{children}</Profiler>
    </WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
  return {
    commits: () => commits,
    elements: () => rendered.container.querySelectorAll('*').length,
  };
}

function reportPerformance(name: string, metrics: { elements: number; commits: number }) {
  if (process.env.PERF_REPORT === '1') console.info(`${name}: ${metrics.elements} elements, ${metrics.commits} commits`);
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date('2026-09-14T20:04:00Z'));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('Portal performance guardrails', () => {
  it('bounds Overview DOM growth and React commits for a large fleet', async () => {
    const { inventory, telemetry } = largeFleet();
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/overview')) return Promise.resolve(json(overviewData()));
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(inventory));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(telemetry));
      return Promise.resolve(json({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    const measured = renderMeasured(<Overview persona="platform"/>);
    expect(await screen.findByRole('heading', { name: 'Infrastructure topology' })).toBeVisible();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    expect(document.querySelectorAll('.overview-sites button')).toHaveLength(12);
    expect(document.querySelectorAll('.overview-node')).toHaveLength(8);
    const metrics = { elements: measured.elements(), commits: measured.commits() };
    reportPerformance('Overview', metrics);
    expect(metrics.elements).toBeLessThanOrEqual(700);
    expect(metrics.commits).toBeLessThanOrEqual(3);
  });

  it('bounds Fleet DOM growth and React commits for a large fleet', async () => {
    const { inventory, telemetry, nodeUtil } = largeFleet(2_048, 8);
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(inventory));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(telemetry));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(nodeUtil));
      return Promise.resolve(json({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    const measured = renderMeasured(<Fleet/>);
    expect(await screen.findByRole('heading', { name: 'GPU Dashboard' })).toBeVisible();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    expect(document.querySelectorAll('.fabric-node')).toHaveLength(3 * 48);
    const metrics = { elements: measured.elements(), commits: measured.commits() };
    reportPerformance('Fleet', metrics);
    expect(metrics.elements).toBeLessThanOrEqual(5_100);
    expect(metrics.commits).toBeLessThanOrEqual(3);
  });
});
