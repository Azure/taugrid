// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Kueue } from './Boards';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { WorkspaceScope } from './types';

const scope: WorkspaceScope = {
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
      <WorkspaceProvider scope={scope} managed={false}><Kueue/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(await screen.findByText('Optional KueueViz is not installed on this cluster.')).toBeVisible();
    expect(screen.getByRole('link', { name: 'Open Scheduler' })).toHaveAttribute('href', '/portal/jobs?view=scheduler');
    expect(screen.queryByText('The Kueue (Live) board unavailable.')).not.toBeInTheDocument();
  });
});
