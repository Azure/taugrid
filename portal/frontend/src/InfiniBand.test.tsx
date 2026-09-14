// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Fleet } from './Fleet';
import { InfiniBandFleet, InfiniBandValidationDetail } from './InfiniBand';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import type { RDMAValidationDetail, WorkspaceScope } from './types';
import { firstHistoryPage, latestSummary, passedValidation, secondHistoryPage } from './test/rdma-fixtures';

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

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('InfiniBand fleet validation', () => {
  it('integrates the InfiniBand Fleet subtab and states the exact coverage', async () => {
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
      const url = String(input);
      return Promise.resolve(json(url.includes('/summary') ? latestSummary : firstHistoryPage));
    }));
    const client = createPortalQueryClient();
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/fleet?view=infiniband']}>
      <WorkspaceProvider scope={scope} managed={false}><Fleet/></WorkspaceProvider>
    </MemoryRouter></QueryClientProvider>);

    expect(screen.getByRole('tab', { name: 'InfiniBand' })).toHaveAttribute('aria-selected', 'true');
    expect(await screen.findAllByText(/two-GPU inter-node RDMA validation/)).not.toHaveLength(0);
    expect(screen.getByText(/do not imply that a validation covered every GPU/)).toBeVisible();
    expect(screen.getByText(/multi-site distributed training/)).toBeVisible();
  });

  it('shows deterministic loading and empty/unknown states', async () => {
    let resolveFetch: ((response: Response) => void) | undefined;
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(resolve => { resolveFetch = resolve; })));
    const rendered = renderPortal('/portal/fleet');
    expect(screen.getAllByText(/Loading snapshot/)).toHaveLength(2);
    rendered.unmount();
    resolveFetch?.(json({}));

    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => Promise.resolve(json(
      String(input).includes('/summary') ? { latest: null, total: 0 } : { validations: [], nextCursor: null, total: 0 },
    ))));
    renderPortal('/portal/fleet');
    expect(await screen.findByText(/Current state is Unknown/)).toBeVisible();
    expect(screen.getByText(/No InfiniBand validation history is available/)).toBeVisible();
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
    expect(screen.getAllByText('IB')[0]).toBeVisible();
    expect(screen.getAllByText('GPU-aaaaaaaa')[0]).toBeVisible();
    expect(screen.getAllByText('mlx5_0')[0]).toBeVisible();
    expect(screen.getAllByText('13.5 GB/s')[0]).toBeVisible();
    expect(screen.getByText('rank 0: 0, rank 1: 0')).toBeVisible();
    expect(screen.getByText('sha256:' + 'a'.repeat(64))).toBeVisible();
    expect(screen.getByText('sha256:' + 'b'.repeat(64))).toBeVisible();
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

  it('paginates history and navigates to routed validation detail', async () => {
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = String(input);
      if (url.includes('/summary')) return Promise.resolve(json(latestSummary));
      if (url.includes(secondHistoryPage.validations[0].validationId)) return Promise.resolve(json(secondHistoryPage.validations[0]));
      if (url.includes('cursor=page-two')) return Promise.resolve(json(secondHistoryPage));
      return Promise.resolve(json(firstHistoryPage));
    });
    vi.stubGlobal('fetch', fetchMock);
    renderPortal('/portal/fleet');

    expect((await screen.findAllByRole('link', { name: passedValidation.validationId }))[0]).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Next page' }));
    expect(await screen.findByRole('link', { name: secondHistoryPage.validations[0].validationId })).toBeVisible();
    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('cursor=page-two'), expect.any(Object));
    expect(screen.getByText(/Page 2/)).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Previous page' }));
    expect(await screen.findByText(/Page 1/)).toBeVisible();
    await userEvent.click(screen.getByRole('button', { name: 'Next page' }));

    fireEvent.click(screen.getByRole('link', { name: secondHistoryPage.validations[0].validationId }));
    expect(await screen.findByRole('heading', { name: 'Artifact verification and evidence' })).toBeVisible();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/api/portal/rdma-validations/' + secondHistoryPage.validations[0].validationId), expect.any(Object),
    ));
  });
});
