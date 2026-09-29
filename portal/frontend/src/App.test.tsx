// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import type { ReactNode } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import {
  WorkspaceProvider, createPortalQueryClient, fleetBoardPaths, useBoardPrefetch,
} from './data';
import type { WorkspaceScope } from './types';
import { fleetGPUHealth, fleetNodes, fleetNodeUtil } from './test/fleet-fixtures';

const scope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'tau-system',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: true,
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

describe('Portal route data loading', () => {
  it('keeps the Fleet prefetch callback stable across rerenders', () => {
    const client = createPortalQueryClient();
    const wrapper = ({ children }: { children: ReactNode }) =>
      <QueryClientProvider client={client}>
        <WorkspaceProvider scope={scope} managed>{children}</WorkspaceProvider>
      </QueryClientProvider>;
    const rendered = renderHook(() => useBoardPrefetch(fleetBoardPaths), { wrapper });
    const initial = rendered.result.current;

    rendered.rerender();

    expect(rendered.result.current).toBe(initial);
  });

  it('prefetches Fleet sources in render priority order and reuses them during navigation', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/api/portal/workspaces')) {
        return Promise.resolve(json({ workspaces: [scope], selected: scope, managed: true }));
      }
      if (url.includes('/api/portal/nodes')) return Promise.resolve(json(fleetNodes));
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json({}));
    }));

    render(<QueryClientProvider client={createPortalQueryClient()}>
      <MemoryRouter initialEntries={['/portal/observability?workspace=research']}>
        <App/>
      </MemoryRouter>
    </QueryClientProvider>);

    expect(await screen.findByRole('heading', { name: 'Observability' })).toBeVisible();
    expect(requests).toEqual(['/api/portal/workspaces?workspace=research']);

    const fleetLink = screen.getByRole('link', { name: 'Fleet' });
    fleetLink.focus();
    fireEvent.pointerDown(fleetLink, { pointerType: 'touch' });
    await userEvent.hover(fleetLink);
    await waitFor(() => expect(requests).toHaveLength(4));
    expect(requests.slice(1)).toEqual([
      '/api/portal/nodes?workspace=research',
      '/api/portal/cluster?workspace=research',
      '/api/portal/nodeutil?workspace=research',
    ]);

    await userEvent.click(screen.getByRole('link', { name: 'Fleet' }));
    expect(await screen.findByRole('heading', { name: 'GPU Dashboard' })).toBeVisible();
    expect(requests).toHaveLength(4);
  });

  it('keeps route-prefetched data isolated by resolved workspace scope', async () => {
    const alpha = { ...scope, workspace: 'alpha', name: 'Alpha', cluster: 'alpha-cluster' };
    const beta = { ...scope, workspace: 'beta', name: 'Beta', cluster: 'beta-cluster' };
    const requests: string[] = [];
    let alphaRequestAborted = false;
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = String(input);
      requests.push(url);
      if (url.includes('/api/portal/workspaces')) {
        const selected = url.includes('workspace=beta') ? beta : alpha;
        return Promise.resolve(json({ workspaces: [alpha, beta], selected, managed: true }));
      }
      if (url.includes('/api/portal/nodes')) {
        const selected = url.includes('workspace=beta') ? beta : alpha;
        if (selected.workspace === 'alpha') {
          return new Promise<Response>((_resolve, reject) => {
            init?.signal?.addEventListener('abort', () => {
              alphaRequestAborted = true;
              reject(new DOMException('Aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(json({
          ...fleetNodes,
          scope: selected,
          nodes: fleetNodes.nodes.map(node => ({ ...node, name: `${selected.workspace}-${node.name}` })),
        }));
      }
      if (url.includes('/api/portal/cluster')) return Promise.resolve(json(fleetGPUHealth));
      if (url.includes('/api/portal/nodeutil')) return Promise.resolve(json(fleetNodeUtil));
      return Promise.resolve(json({}));
    }));

    render(<QueryClientProvider client={createPortalQueryClient()}>
      <MemoryRouter initialEntries={['/portal/observability?workspace=alpha']}>
        <App/>
      </MemoryRouter>
    </QueryClientProvider>);

    const fleetLink = await screen.findByRole('link', { name: 'Fleet' });
    await userEvent.hover(fleetLink);
    await waitFor(() => expect(requests.filter(url => url.includes('/api/portal/nodes'))).toHaveLength(1));

    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Workspace' }), 'beta');
    await waitFor(() => expect(requests.some(url => url.includes('/api/portal/workspaces?workspace=beta'))).toBe(true));
    await waitFor(() => expect(alphaRequestAborted).toBe(true));
    await userEvent.hover(screen.getByRole('link', { name: 'Fleet' }));
    await waitFor(() => expect(requests.filter(url => url.includes('/api/portal/nodes'))).toHaveLength(2));

    expect(requests.filter(url => url.includes('/api/portal/nodes'))).toEqual([
      '/api/portal/nodes?workspace=alpha',
      '/api/portal/nodes?workspace=beta',
    ]);
  });
});
