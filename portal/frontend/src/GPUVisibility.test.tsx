// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App, tabForPath } from './App';
import { GPUConsumers, WorkspaceGPUWorkloads } from './GPUVisibility';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { GPUInventory, GPUWorkload, WorkspaceScope } from './types';

const scope: WorkspaceScope = {
  workspace: 'alpha', name: 'Alpha', cluster: 'cluster-a', namespace: 'team-a', localQueue: 'gpu',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: true,
};

function workload(overrides: Partial<GPUWorkload> = {}): GPUWorkload {
  return {
    namespace: 'team-a',
    owner: { kind: 'RayJob', name: 'train', uid: 'uid-1', resolution: 'resolved' },
    managed: true,
    queue: 'gpu', workspace: 'alpha', team: 'research', runId: 'run-1',
    podCount: 2, readyPods: 1, pendingPods: 1, boundPods: 1,
    phases: { pending: 1, running: 1 }, nodes: ['gpu-a'],
    resources: [{ name: 'nvidia.com/gpu', count: 8 }],
    pendingResources: [{ name: 'nvidia.com/gpu', count: 4 }],
    boundResources: [{ name: 'nvidia.com/gpu', count: 4 }],
    requestedFullGpus: 8, pendingFullGpus: 4, boundFullGpus: 4,
    countUnknown: false, countUnknownPods: 0, diagnostics: [],
    detail: { resourceUid: 'uid-1', path: '/portal/workloads/uid-1' },
    ...overrides,
  };
}

function snapshot(overrides: Partial<GPUInventory> = {}): GPUInventory {
  return {
    generatedAt: '2026-10-20T21:00:00Z',
    summary: {
      workloads: 1, pods: 2, readyPods: 1, pendingPods: 1, boundPods: 1,
      requestedFullGpus: 8, pendingFullGpus: 4, boundFullGpus: 4, countUnknownPods: 0,
    },
    workloads: [workload()], partial: false, truncated: false,
    sources: [{ source: 'pods', state: 'ready' }], state: 'ready',
    ...overrides,
  };
}

function response(body: unknown, status = 200) {
  return Promise.resolve(new Response(JSON.stringify(body), {
    status, headers: { 'content-type': 'application/json' },
  }));
}

function renderGlobal() {
  render(<QueryClientProvider client={createPortalQueryClient()}><MemoryRouter>
    <GPUConsumers/>
  </MemoryRouter></QueryClientProvider>);
}

function renderWorkspace() {
  render(<QueryClientProvider client={createPortalQueryClient()}><MemoryRouter>
    <WorkspaceProvider scope={scope} managed><WorkspaceGPUWorkloads/></WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('GPU visibility routes and scope', () => {
  it('keeps GPU consumers distinct from Fleet and renders without an authorized workspace', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.startsWith('/api/portal/workspaces')) return response({ workspaces: [], managed: true });
      if (url === '/api/portal/gpu-consumers') return response(snapshot());
      return response({});
    }));

    render(<QueryClientProvider client={createPortalQueryClient()}>
      <MemoryRouter initialEntries={['/portal/gpu-consumers?workspace=not-authorized']}><App/></MemoryRouter>
    </QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'GPU Consumers' })).toBeVisible();
    expect(screen.getByRole('link', { name: 'GPU Consumers' })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'Fleet' })).not.toHaveAttribute('aria-current');
    expect(requests).toContain('/api/portal/gpu-consumers');
    expect(requests.filter(url => url.startsWith('/api/portal/gpu-consumers'))).toEqual(['/api/portal/gpu-consumers']);
    expect(tabForPath('/portal/gpu-consumers')).toBe('platform');
    expect(tabForPath('/portal/gpu')).toBe('platform');
  });

  it('uses resolved workspace query behavior only for the workspace inventory', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      requests.push(String(input));
      return response(snapshot({ scope }));
    }));

    renderWorkspace();

    expect(await screen.findByText('RayJob/train')).toBeVisible();
    expect(requests).toEqual(['/api/portal/workspace-gpu-workloads?workspace=alpha']);
  });
});

describe('GPU visibility states', () => {
  it('renders Tau and unmanaged owners, source gaps, truncation, DRA unknown counts, and valid detail links', async () => {
    vi.stubGlobal('fetch', vi.fn(() => response(snapshot({
      summary: { ...snapshot().summary, workloads: 2, pods: 3, countUnknownPods: 1 },
      workloads: [
        workload(),
        workload({
          namespace: 'platform', owner: { kind: 'Deployment', name: 'inference', resolution: 'dangling' },
          managed: false, workspace: undefined, queue: undefined, detail: undefined,
          podCount: 1, readyPods: 0, pendingPods: 1, boundPods: 0, nodes: [],
          countUnknown: true, countUnknownPods: 1,
          resources: [{ name: 'claims.vendor.example/gpu', count: 0 }],
          pendingResources: [], boundResources: [],
          diagnostics: [{ code: 'owner_dangling', message: 'The controller owner reference did not resolve by UID.' }],
        }),
      ],
      partial: true, truncated: true,
      sources: [
        { source: 'pods', state: 'ready' },
        { source: 'deployments', state: 'unavailable', message: 'forbidden' },
      ],
    }))));

    renderGlobal();

    expect(await screen.findByText('RayJob/train')).toBeVisible();
    expect(screen.getByText('Deployment/inference')).toBeVisible();
    expect(screen.getByText('Tau')).toBeVisible();
    expect(screen.getByText('Unmanaged')).toBeVisible();
    expect(screen.getByText(/Partial inventory/)).toBeVisible();
    expect(screen.getByText(/Truncated inventory/)).toBeVisible();
    expect(screen.getByText(/use DRA/)).toBeVisible();
    expect(screen.getByText(/did not resolve by UID/)).toBeVisible();
    expect(screen.getByText(/Source deployments is unavailable/)).toBeVisible();
    expect(screen.getByRole('link', { name: 'RayJob/train' })).toHaveAttribute(
      'href', '/portal/workloads/uid-1?workspace=alpha',
    );
    expect(screen.queryByRole('link', { name: 'Deployment/inference' })).not.toBeInTheDocument();
  });

  it('does not link a global Tau owner without workspace routing metadata', async () => {
    vi.stubGlobal('fetch', vi.fn(() => response(snapshot({
      workloads: [workload({ workspace: undefined })],
    }))));

    renderGlobal();

    expect(await screen.findByText('RayJob/train')).toBeVisible();
    expect(screen.queryByRole('link', { name: 'RayJob/train' })).not.toBeInTheDocument();
  });

  it('renders an explicit empty inventory', async () => {
    vi.stubGlobal('fetch', vi.fn(() => response(snapshot({
      summary: { ...snapshot().summary, workloads: 0, pods: 0, readyPods: 0, pendingPods: 0, boundPods: 0, requestedFullGpus: 0, pendingFullGpus: 0, boundFullGpus: 0 },
      workloads: [], state: 'no_data',
    }))));

    renderGlobal();

    expect(await screen.findByText('No active GPU-consuming workloads were found in this scope.')).toBeVisible();
  });

  it.each([
    ['disabled', 503, { state: 'disabled', reason: 'fleet GPU consumers are disabled' }, /disabled/],
    ['forbidden', 403, { state: 'forbidden', reason: 'not authorized to list Pods' }, /403 not authorized.*read RBAC/],
    ['unavailable', 502, { state: 'unavailable', reason: 'inventory could not be read' }, /502 inventory could not be read/],
  ])('renders the fleet %s failure contract', async (_name, status, body, expected) => {
    vi.stubGlobal('fetch', vi.fn(() => response(body, status)));

    renderGlobal();

    expect(await screen.findByRole('alert')).toHaveTextContent(expected);
  });

  it('renders workspace setup-required failures', async () => {
    vi.stubGlobal('fetch', vi.fn(() => response({
      state: 'setup_required', reason: 'workspace GPU workloads require a resolved namespace and LocalQueue',
    }, 503)));

    renderWorkspace();

    expect(await screen.findByRole('alert')).toHaveTextContent(/setup.*namespace and LocalQueue/i);
  });

  it('retains a stale snapshot after refresh fails', async () => {
    let attempt = 0;
    vi.stubGlobal('fetch', vi.fn(() => {
      attempt++;
      return attempt === 1 ? response(snapshot()) : response({ state: 'unavailable', reason: 'Pod read failed' }, 502);
    }));

    renderGlobal();
    expect(await screen.findByText('RayJob/train')).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Refresh GPU consumers' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/refresh failed.*Showing stale data/);
    await waitFor(() => expect(screen.getByText('RayJob/train')).toBeVisible());
  });
});
