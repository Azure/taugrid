// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Fleet } from './Fleet';
import { InfiniBandFleet, InfiniBandValidationDetail } from './InfiniBand';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { RDMAValidationDetail, WorkspaceScope } from './types';
import { firstHistoryPage, fleetGPUHealth, fleetNodes, fleetNodeUtil, latestSummary, passedValidation } from './test/rdma-fixtures';

const scope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'tau-system',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: false,
};

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function renderPortal(initialEntry: string, detailOnly = false) {
  const client = createPortalQueryClient();
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[initialEntry]}>
    <WorkspaceProvider scope={scope} managed={false}>
      <Routes>
        {detailOnly
          ? <Route path="/portal/fleet/infiniband/:validationId" element={<InfiniBandValidationDetail/>}/>
          : <>
            <Route path="/portal/fleet" element={<InfiniBandFleet/>}/>
            <Route path="/portal/fleet/infiniband/:validationId" element={<InfiniBandValidationDetail/>}/>
          </>}
      </Routes>
    </WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
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

describe('InfiniBand fleet validation', () => {
  it('combines fleet capacity, utilization, health, and InfiniBand without subtabs', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    }));
    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/fleet?view=infiniband']}>
      <WorkspaceProvider scope={scope} managed={false}><Fleet/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(screen.queryByRole('tab')).not.toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'GPU Dashboard' })).toBeVisible();
    expect(await screen.findByText(/3\/3 nodes ready/)).toBeVisible();
    expect(screen.getByText('1 free')).toBeVisible();
    expect(screen.getByText(/2 assigned · 3 schedulable/)).toBeVisible();
    expect(screen.getByText('63%')).toBeVisible();
  });

  it('does not claim free GPUs when active assignments are unavailable', async () => {
    const unknownAllocations = {
      ...fleetNodes,
      gpuAllocated: 0,
      gpuAvailable: 0,
      gpuAllocationKnown: false,
      nodes: fleetNodes.nodes.map(node => ({
        ...node,
        gpuAllocated: undefined,
        gpuAvailable: undefined,
      })),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(unknownAllocations));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? { latest: null, total: 0 } : { validations: [], nextCursor: null, total: 0 }));
    }));
    renderPortal('/portal/fleet');

    const availability = (await screen.findByText('GPU availability')).parentElement;
    expect(availability).not.toBeNull();
    expect(within(availability!).getByText('Unknown')).toBeVisible();
    expect(within(availability!).getByText(/3 schedulable · active assignments unavailable/)).toBeVisible();
    expect(screen.queryByText('3 free')).not.toBeInTheDocument();
  });

  it('shows deterministic loading and empty/unknown states', async () => {
    let resolveFetch: ((response: Response) => void) | undefined;
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(resolve => { resolveFetch = resolve; })));
    const rendered = renderPortal('/portal/fleet');
    expect(screen.getAllByText(/Loading snapshot/)).toHaveLength(1);
    rendered.unmount();
    resolveFetch?.(json({}));

    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => Promise.resolve(json(
      String(input).includes('/nodes') ? {
        ...fleetNodes,
        totalNodes: 0,
        gpuNodes: 0,
        totalGPUs: 0,
        gpuAllocatable: 0,
        gpuSchedulable: 0,
        gpuAllocated: 0,
        gpuAvailable: 0,
        rdmaAdvertisedGpuNodes: 0,
        nodes: [],
        skus: [],
      } :
        String(input).includes('/cluster') ? { ...fleetGPUHealth, totalGPUs: 0, gpus: [] } :
          String(input).includes('/nodeutil') ? { ...fleetNodeUtil, nodes: [] } :
        String(input).includes('/summary') ? { latest: null, total: 0 } : { validations: [], nextCursor: null, total: 0 },
    ))));
    renderPortal('/portal/fleet');
    expect(await screen.findByText(/No GPU or RDMA-capable nodes/)).toBeVisible();
    expect(screen.queryByText('Evidence matrix and runtime coverage')).not.toBeInTheDocument();
    expect(screen.queryByText('Validation run details and history')).not.toBeInTheDocument();
  });

  it('keeps Kusto telemetry and validation visible when inventory is unavailable', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json({ error: 'inventory unavailable' }, 503));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(latestSummary));
    }));
    renderPortal('/portal/fleet');

    expect(await screen.findByText(/inventory:.*inventory unavailable/)).toBeVisible();
    expect(screen.getByText('2 GPU telemetry records')).toBeVisible();
    expect(screen.getByText('3 node utilization records')).toBeVisible();
    expect(screen.getByText('39% avg')).toBeVisible();
    expect(screen.getByText('1 fault')).toBeVisible();
    expect(screen.getByText(/fleet denominators, RDMA scheduling capability, and Unbounded site boundaries are Unknown/)).toBeVisible();
    const independent = screen.getByRole('region', { name: 'Independent source evidence' });
    expect(within(independent).getByRole('cell', { name: '63%' })).toBeVisible();
    expect(within(independent).getByRole('cell', { name: '72%' })).toBeVisible();
    expect(screen.queryByRole('heading', { name: 'GPU Dashboard' })).not.toBeInTheDocument();
  });

  it('correlates telemetry by exact cluster and instance identity', async () => {
    const telemetry = {
      ...fleetGPUHealth,
      gpus: [
        ...fleetGPUHealth.gpus,
        { ...fleetGPUHealth.gpus[0], cluster: 'other-cluster', utilizationPct: 99, healthy: false },
      ],
    };
    const nodeUtil = {
      ...fleetNodeUtil,
      nodes: [
        ...fleetNodeUtil.nodes,
        { ...fleetNodeUtil.nodes[0], cluster: 'other-cluster', cpuUtilPct: 99, memUsedPct: 98 },
      ],
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(telemetry));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(nodeUtil));
      return Promise.resolve(json(latestSummary));
    }));
    renderPortal('/portal/fleet');

    const card = (await screen.findByText('h200-node-a', { selector: '.fabric-node-head strong' })).closest('article');
    expect(card).not.toBeNull();
    expect(within(card!).getByText('41%')).toBeVisible();
    expect(within(card!).getByText('63%')).toBeVisible();
    expect(within(card!).queryByText('99%')).not.toBeInTheDocument();
    const independent = screen.getByRole('region', { name: 'Independent source evidence' });
    expect(within(independent).getAllByRole('cell', { name: 'other-cluster' })).toHaveLength(2);
    expect(within(independent).getByRole('cell', { name: '99' })).toBeVisible();
    expect(within(independent).getByRole('cell', { name: '99%' })).toBeVisible();
    expect(within(independent).getByRole('cell', { name: '98%' })).toBeVisible();
  });

  it('does not attach historical validation evidence to a recreated node name', async () => {
    const recreatedNodes = {
      ...fleetNodes,
      nodes: fleetNodes.nodes.map(node => node.name === 'h200-node-a'
        ? { ...node, uid: 'replacement-node-a-uid' }
        : node),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(recreatedNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(latestSummary));
    }));
    renderPortal('/portal/fleet');

    const card = (await screen.findByText('h200-node-a', { selector: '.fabric-node-head strong' })).closest('article');
    expect(card).not.toBeNull();
    expect(within(card!).queryByText(/GPU sampled in latest run/)).not.toBeInTheDocument();
    expect(screen.getByText('Passed · 2-GPU run path')).toBeVisible();
  });

  it('separates fleet RDMA capability, tested topology, and per-GPU health', async () => {
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    });
    vi.stubGlobal('fetch', fetchMock);
    renderPortal('/portal/fleet');

    expect(await screen.findByRole('heading', { name: 'GPU Dashboard' })).toBeVisible();
    expect(screen.queryByText(/Site boundaries use exact/)).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Unbounded site eastus2' })).toBeVisible();
    expect(screen.getByRole('region', { name: 'Unbounded site cluster' })).toBeVisible();
    expect(screen.getAllByText(/Region eastus2euap/)).not.toHaveLength(0);
    expect(screen.getByText('Passed · 2-GPU run path')).toBeVisible();
    expect(screen.getByText(/h200-node-a \/ GPU-aaaaaaaa ↔ h200-node-b \/ GPU-bbbbbbbb/)).toBeVisible();
    expect(screen.getAllByText(/RDMA advertised/, { selector: '.fabric-capability' })).toHaveLength(2);
    expect(screen.getByText(/other GPUs in the site are not implied validated/)).toBeVisible();
    expect(screen.getAllByRole('link', { name: /GPU details/ }).some(link =>
      link.getAttribute('href')?.includes('instance=h200-node-a'))).toBe(true);
    expect(screen.getByRole('heading', { name: /NVIDIA H200.*Pool h200/ })).toBeVisible();
    expect(screen.queryByText('Evidence matrix and runtime coverage')).not.toBeInTheDocument();
    expect(screen.queryByText('Validation run details and history')).not.toBeInTheDocument();

    fetchMock.mockClear();
    await userEvent.click(screen.getByRole('button', { name: 'Refresh GPU dashboard data' }));
    await waitFor(() => {
      const urls = fetchMock.mock.calls.map(([input]) => String(input));
      for (const path of ['/api/portal/nodes', '/api/portal/cluster', '/api/portal/nodeutil', '/api/portal/rdma-validations/summary']) {
        expect(urls.some(url => url.includes(path))).toBe(true);
      }
    });
  });

  it('keeps node capability visible without unsolicited absent-run text', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json({ ...latestSummary, latest: null }));
    }));
    renderPortal('/portal/fleet');

    await screen.findByRole('heading', { name: 'GPU Dashboard' });
    expect(screen.queryByText(/No recorded two-GPU inter-node NCCL validation run/)).not.toBeInTheDocument();
    expect(screen.queryByText('Latest validation')).not.toBeInTheDocument();
    expect(screen.getAllByText(/RDMA advertised/, { selector: '.fabric-capability' })).toHaveLength(2);
  });

  it('labels a raw gpu pool with the inferred NVIDIA model', async () => {
    const modelNodes = {
      ...fleetNodes,
      nodes: fleetNodes.nodes.map(node => node.name === 'a100-node-c'
        ? { ...node, agentPool: 'gpu', gpuProduct: undefined, sku: 'Standard_NC24ads_A100_v4' }
        : node),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(modelNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    }));
    renderPortal('/portal/fleet');

    expect(await screen.findByRole('heading', { name: /NVIDIA A100.*Pool gpu/ })).toBeVisible();
    expect(screen.queryByRole('heading', { name: 'gpu' })).not.toBeInTheDocument();
  });

  it.each(['incomplete', 'not_applicable'] as const)('does not draw a validated site path for %s topology', async siteMode => {
    const summary = {
      ...latestSummary,
      latest: latestSummary.latest && {
        ...latestSummary.latest,
        actual: { ...latestSummary.latest.actual, siteMode },
      },
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? summary : firstHistoryPage));
    }));
    renderPortal('/portal/fleet');

    await screen.findByRole('heading', { name: 'GPU Dashboard' });
    expect(screen.queryByText('Passed · 2-GPU run path')).not.toBeInTheDocument();
  });

  it('keeps partial Unbounded coverage explicit and does not infer site from region', async () => {
    const partialNodes = {
      ...fleetNodes,
      nodes: fleetNodes.nodes.map(node => node.name === 'a100-node-c'
        ? { ...node, region: 'eastus2euap' }
        : node.name === 'h200-node-b'
          ? { ...node, site: undefined, siteLabel: undefined }
          : node),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(partialNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? { latest: null, total: 0 } : { validations: [], nextCursor: null, total: 0 }));
    }));
    renderPortal('/portal/fleet');

    expect(await screen.findByRole('alert')).toHaveTextContent('2/3 GPU nodes are labeled');
    expect(screen.getByRole('region', { name: 'Unbounded site eastus2' })).toBeVisible();
    expect(screen.getByRole('region', { name: 'Unbounded site cluster' })).toBeVisible();
    const unknownSite = screen.getByRole('region', { name: 'Unbounded site Unknown' });
    expect(unknownSite).toBeVisible();
    expect(within(unknownSite).getByText(/No Unbounded site label · Region eastus2euap/)).toBeVisible();
  });

  it('warns when canonical and fallback inventory labels conflict', async () => {
    const conflictNodes = {
      ...fleetNodes,
      nodes: fleetNodes.nodes.map((node, index) => index === 0
        ? { ...node, siteLabel: 'unbounded-cloud.io/site', siteLabelConflict: true }
        : node),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(conflictNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    }));
    renderPortal('/portal/fleet');

    expect(await screen.findByRole('alert')).toHaveTextContent('1/3 GPU nodes have conflicting canonical and fallback site labels');
    expect(screen.getByText(/Unbounded site label conflict · canonical value shown/)).toBeVisible();
  });

  it('omits Unbounded site grouping when no GPU node has a supported label', async () => {
    const unlabeledNodes = {
      ...fleetNodes,
      nodes: fleetNodes.nodes.map(({ site: _site, siteLabel: _siteLabel, ...node }) => node),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(unlabeledNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? { latest: null, total: 0 } : { validations: [], nextCursor: null, total: 0 }));
    }));
    renderPortal('/portal/fleet');

    expect(await screen.findByRole('heading', { name: 'GPU Dashboard' })).toBeVisible();
    expect(screen.getByText(/Unbounded site visualization is unavailable/)).toBeVisible();
    expect(screen.queryByRole('region', { name: /Unbounded site/ })).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Region eastus2euap' })).toBeVisible();
    expect(screen.getByRole('region', { name: 'Region westus3' })).toBeVisible();
  });

  it('opens per-GPU health and utilization inline on the unified Fleet page', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    }));
    renderPortal('/portal/fleet?instance=h200-node-a');

    expect(await screen.findByRole('region', { name: 'GPU details for h200-node-a' })).toBeVisible();
    expect(screen.getByRole('heading', { name: 'GPU details · h200-node-a' })).toBeVisible();
    expect(screen.getByRole('link', { name: 'Clear focus' })).toHaveAttribute('href', '/portal/fleet');
    expect(screen.getByRole('cell', { name: '41' })).toBeVisible();
  });

  it('keeps stale and future continuous evidence Unknown', async () => {
    const staleNodes = {
      ...fleetNodes,
      nodes: fleetNodes.nodes.map((node, index) => ({
        ...node,
        operationalConditions: index === 2
          ? [
            { type: 'GPUECCDoubleRetired', status: 'False', lastHeartbeatTime: '2026-09-14T20:03:00Z' },
            { type: 'GPUECCDoubleRetired', status: 'False', lastHeartbeatTime: '2026-09-14T20:03:00Z' },
          ]
          : node.operationalConditions?.map(condition => ({
            ...condition,
            lastHeartbeatTime: index === 0 ? '2026-09-14T19:30:00Z' : '2026-09-14T20:06:00Z',
          })),
      })),
    };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(staleNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    }));
    renderPortal('/portal/fleet');

    await screen.findByRole('heading', { name: 'GPU Dashboard' });
    expect(screen.getAllByText('Unknown', { selector: '.fabric-signals b' }).length).toBeGreaterThanOrEqual(3);
  });

  it.each([
    ['running', 'unknown', 'fresh', 'Running', 'no final pass or fail'],
    ['passed', 'pass', 'fresh', 'Passed', 'passed for this validation run only'],
    ['failed', 'fail', 'fresh', 'Failed', 'validation run failed'],
    ['stale', 'pass', 'stale', 'Stale', 'latest validation is stale'],
    ['unknown', 'unknown', 'unknown', 'Unknown', 'absent, incomplete, malformed'],
  ] as const)('renders %s as state text independent of color', async (state, historicalStatus, freshness, label, meaning) => {
    const fixture: RDMAValidationDetail = { ...passedValidation, validationId: `validation-${state}`, state, historicalStatus, freshness };
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json(fixture))));
    renderPortal(`/portal/fleet/infiniband/${fixture.validationId}`, true);

    expect((await screen.findAllByText(label, { selector: '.badge' }))[0]).toBeVisible();
    expect(screen.getAllByText(new RegExp(meaning, 'i'))[0]).toBeVisible();
  });

  it('shows a hard error without presenting a validation result', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ detail: 'validation store unavailable' }, 503))));
    renderPortal('/portal/fleet/infiniband/missing', true);

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('InfiniBand validation detail unavailable');
    expect(alert).toHaveTextContent('validation store unavailable');
    expect(screen.queryByRole('heading', { name: 'Artifact verification and evidence' })).not.toBeInTheDocument();
  });

  it.each([
    ['malformed artifact', {
      state: 'unknown', historicalStatus: 'unknown', freshness: 'unknown',
      artifactVerification: { state: 'invalid', reason: 'Canonical result JSON is malformed.' },
    }, /malformed/i],
    ['socket fallback', {
      state: 'failed', historicalStatus: 'fail', freshness: 'fresh',
      transport: { ...passedValidation.transport, backend: 'socket', socketFallbackDetected: true },
      reasonCode: 'socket_fallback_observed', reason: 'NCCL socket fallback was observed.',
    }, /socket fallback was observed/i],
    ['missing bandwidth', {
      state: 'unknown', historicalStatus: 'unknown', freshness: 'fresh',
      summary: undefined, measurements: [], reasonCode: 'missing_bandwidth', reason: 'Bandwidth evidence is missing.',
    }, /No per-rank bandwidth measurements/i],
    ['cleanup incomplete', {
      state: 'failed', historicalStatus: 'fail', freshness: 'fresh',
      cleanup: { status: 'incomplete', remainingResources: ['Job/nccl-rdma'] },
      reasonCode: 'cleanup_incomplete', reason: 'Cleanup did not remove every resource.',
    }, /Job\/nccl-rdma/i],
  ] as const)('renders %s evidence without inferring success', async (_name, override, expected) => {
    const fixture = { ...passedValidation, ...override, validationId: `validation-${_name.replaceAll(' ', '-')}` } as RDMAValidationDetail;
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json(fixture))));
    renderPortal(`/portal/fleet/infiniband/${fixture.validationId}`, true);

    expect((await screen.findAllByText(expected))[0]).toBeVisible();
    expect(screen.queryByText('Passed', { selector: '.badge' })).not.toBeInTheDocument();
  });

  it('renders the required NCCL, placement, provenance, exit, cleanup, and evidence fields', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json(passedValidation))));
    renderPortal(`/portal/fleet/infiniband/${passedValidation.validationId}`, true);

    expect((await screen.findAllByText('NVIDIA H200'))[0]).toBeVisible();
    expect(screen.getByText(/nccl \/ 2\.28\.8 \/ all_reduce/)).toBeVisible();
    expect(screen.getAllByText('unbounded', { exact: true })).toHaveLength(2);
    expect(screen.getAllByText('complete', { exact: true }).length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText('unbounded-cloud.io/site', { exact: true })).toBeVisible();
    expect(screen.getByText('net.unbounded-cloud.io/site', { exact: true })).toBeVisible();
    expect(screen.getAllByText('IB')[0]).toBeVisible();
    expect(screen.getAllByText('GPU-aaaaaaaa')[0]).toBeVisible();
    expect(screen.getAllByText('mlx5_0')[0]).toBeVisible();
    expect(screen.getAllByText('13.5 GB/s')[0]).toBeVisible();
    expect(screen.getByText('rank 0: 0, rank 1: 0')).toBeVisible();
    expect(screen.getByText('sha256:' + 'a'.repeat(64))).toBeVisible();
    expect(screen.getByText('sha256:' + 'b'.repeat(64))).toBeVisible();
  });

  it('shows incomplete and conflicting validation topology explicitly', async () => {
    const conflicting = {
      ...passedValidation,
      state: 'unknown',
      historicalStatus: 'unknown',
      reasonCode: 'topology_evidence_incomplete',
      reason: 'Unbounded site topology evidence is incomplete.',
      actual: {
        ...passedValidation.actual,
        siteMode: 'incomplete',
        nodes: passedValidation.actual?.nodes?.map((node, index) => index === 0
          ? { ...node, siteLabelConflict: true }
          : node),
      },
    } satisfies RDMAValidationDetail;
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json(conflicting))));
    renderPortal(`/portal/fleet/infiniband/${conflicting.validationId}`, true);

    expect(await screen.findByText('Canonical and fallback site labels conflict')).toBeVisible();
    expect(screen.getByText('incomplete', { exact: true })).toBeVisible();
    expect(screen.getAllByText(/Unbounded site incomplete/)).not.toHaveLength(0);
  });

  it('retains and labels a stale snapshot when refresh fails', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(json(passedValidation))
      .mockResolvedValueOnce(json({ detail: 'temporary read failure' }, 503));
    vi.stubGlobal('fetch', fetchMock);
    renderPortal('/portal/fleet/infiniband/' + passedValidation.validationId, true);
    expect(await screen.findByText(passedValidation.validationId, { selector: 'td' })).toBeVisible();

    await userEvent.click(screen.getByRole('button', { name: 'Refresh InfiniBand validation detail' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('refresh failed');
    expect(alert).toHaveTextContent('Showing stale data from the last successful read');
    expect(screen.getByText(passedValidation.validationId, { selector: 'td' })).toBeVisible();
  });

});
