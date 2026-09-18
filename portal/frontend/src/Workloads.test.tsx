// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { WorkspaceScope } from './types';
import { RayBoard } from './Workloads';

const scope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'research',
  source: 'portal', authorizationMode: 'workspace', availability: 'available', managed: false,
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Ray history navigation', () => {
  it('preserves the selected historical range when opening a durable RayJob', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify({
      state: 'ready',
      total: 0,
      clusters: [],
      historyState: 'available',
      history: [{
        resourceUid: 'ray-uid',
        name: 'completed-ray',
        namespace: 'research',
        status: 'Succeeded',
        age: '1d',
        runId: 'run-1',
      }],
    }), { status: 200, headers: { 'Content-Type': 'application/json' } }))));
    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/ray?window=168h']}>
      <WorkspaceProvider scope={scope} managed={false}><RayBoard/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole('link', { name: 'completed-ray' }))
      .toHaveAttribute('href', '/portal/ray/history/ray-uid?window=168h');
  });
});
