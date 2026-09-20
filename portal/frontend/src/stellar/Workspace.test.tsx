// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { createPortalQueryClient, WorkspaceProvider } from '../data';
import type { WorkspaceScope } from '../types';
import { StellarWorkspace } from './Workspace';

const scope: WorkspaceScope = {
  workspace: 'research', name: 'Research', cluster: 'research-west', namespace: 'tau-system',
  source: 'local', authorizationMode: 'workspace', availability: 'available', managed: false,
  experimentsNative: { state: 'available', apiBasePath: '/api/v2/stellar' },
};
const page = {
  total: 201, truncated: true, warnings: ['Range A warning'],
  runs: [{ run_id: 'range-a-run', run_group_id: 'group', project: 'research', state: 'running',
    created_at: '2026-09-20T10:00:00Z', metric_names: [] }],
};
function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}
function TimezoneNavigation() {
  const location = useLocation(), navigate = useNavigate();
  return <button onClick={() => {
    const params = new URLSearchParams(location.search);
    params.set('tz', 'utc');
    navigate(location.pathname + '?' + params);
  }}>Display UTC</button>;
}
function renderWorkspace(client = createPortalQueryClient(), initialEntry = '/portal/experiments?target=experiment&sections=&window=24h&refresh=off') {
  return render(<QueryClientProvider client={client}>
    <MemoryRouter initialEntries={[initialEntry]}>
      <WorkspaceProvider scope={scope} managed={false}><StellarWorkspace/><TimezoneNavigation/></WorkspaceProvider>
    </MemoryRouter>
  </QueryClientProvider>);
}
function otherResponse(url: URL) {
  return json(url.pathname.endsWith('/snapshot')
    ? { runs: [], status: { metric_files: 0 }, cards: [], metric_options: [], chart: {}, summary: {} }
    : { experiments: [] });
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it.each([400, 403, 502])('hides retained runs when a different range is pending or fails with %s', async status => {
  let finish: (response: Response) => void = () => {};
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    if (url.searchParams.get('window') === '24h') return Promise.resolve(json(page));
    return new Promise<Response>(resolve => { finish = resolve; });
  }));
  renderWorkspace();
  expect(await screen.findByRole('checkbox', { name: 'range-a-run' })).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: '1h' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  expect(screen.queryByText('Range A warning')).not.toBeInTheDocument();
  expect(screen.getByText('0 loaded of 0 · 0 visible')).toBeInTheDocument();
  await act(async () => finish(json({ error: 'upstream failed' }, status)));
  expect(await screen.findByText(/More runs unavailable/)).not.toHaveTextContent('Showing stale data');
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Retry loading runs' }));
  await act(async () => finish(json({ total: 0, runs: [] })));
  await waitFor(() => expect(screen.queryByText(/More runs unavailable/)).not.toBeInTheDocument());
  expect(screen.getByText('0 loaded of 0 · 0 visible')).toBeInTheDocument();
});

it('retains the same range page after a larger page fails and recovers on retry', async () => {
  let failed = true;
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    if (url.searchParams.get('limit') === '200') return Promise.resolve(json(page));
    return Promise.resolve(failed ? json({ error: 'page unavailable' }, 502)
      : json({ ...page, total: 1, truncated: false, warnings: [] }));
  }));
  renderWorkspace();
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await screen.findByText(/More runs unavailable/);
  expect(screen.getByRole('checkbox', { name: 'range-a-run' })).toBeInTheDocument();
  expect(screen.getByText('1 loaded of 201 · 1 visible')).toBeInTheDocument();
  failed = false;
  fireEvent.click(screen.getByRole('button', { name: 'Retry loading runs' }));
  expect(await screen.findByText('1 loaded of 1 · 1 visible')).toBeInTheDocument();
  expect(screen.queryByText(/More runs unavailable/)).not.toBeInTheDocument();
});

it('preserves hidden runs and page size across range changes without remounting', async () => {
  const fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    return Promise.resolve(url.pathname.endsWith('/runs') ? json(page) : otherResponse(url));
  });
  vi.stubGlobal('fetch', fetch);
  renderWorkspace();
  fireEvent.click(await screen.findByRole('checkbox', { name: 'range-a-run' }));
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Load 200 more runs' })).toBeEnabled());
  const search = screen.getByRole('searchbox', { name: 'Search runs' });
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: '1h' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  expect(await screen.findByRole('checkbox', { name: 'range-a-run' })).not.toBeChecked();
  expect(screen.getByRole('searchbox', { name: 'Search runs' })).toBe(search);
  const request = fetch.mock.calls.map(([input]) => new URL(String(input), 'http://localhost'))
    .find(url => url.pathname.endsWith('/runs') && url.searchParams.get('window') === '1h');
  expect(request?.searchParams.get('limit')).toBe('400');
});

it('ignores a late page from the previous range', async () => {
  const client = createPortalQueryClient();
  let finishPrevious: ((response: Response) => void) | undefined;
  const currentPage = { total: 1, runs: [{ ...page.runs[0], run_id: 'range-b-run' }] };
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    if (url.searchParams.get('window') === '1h') return Promise.resolve(json(currentPage));
    if (url.searchParams.get('limit') === '200') return Promise.resolve(json(page));
    return new Promise<Response>(resolve => { finishPrevious = resolve; });
  }));
  renderWorkspace(client);
  await screen.findByRole('checkbox', { name: 'range-a-run' });
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await waitFor(() => expect(finishPrevious).toBeTypeOf('function'));
  fireEvent.change(screen.getByLabelText('Range'), { target: { value: '1h' } });
  fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
  await screen.findByRole('checkbox', { name: 'range-b-run' });
  const previousResponse = json(page);
  const consumePrevious = vi.spyOn(previousResponse, 'json');
  await act(async () => finishPrevious!(previousResponse));
  await waitFor(() => expect(consumePrevious).toHaveBeenCalledOnce());
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(screen.queryByRole('checkbox', { name: 'range-a-run' })).not.toBeInTheDocument();
  expect(screen.queryByText('Range A warning')).not.toBeInTheDocument();
  expect(screen.getByRole('checkbox', { name: 'range-b-run' })).toBeInTheDocument();
  expect(screen.getByText('1 loaded of 1 · 1 visible')).toBeInTheDocument();
});

it('preserves the requested interval and retained page on timezone-only navigation', async () => {
  const client = createPortalQueryClient();
  const fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost');
    if (!url.pathname.endsWith('/runs')) return Promise.resolve(otherResponse(url));
    return Promise.resolve(url.searchParams.get('limit') === '200'
      ? json(page) : json({ error: 'page unavailable' }, 502));
  });
  vi.stubGlobal('fetch', fetch);
  renderWorkspace(client, '/portal/experiments?target=experiment&sections=&start=2026-09-16T00:00:00Z&end=2026-09-16T01:00:00Z&tz=local&refresh=off');
  fireEvent.click(await screen.findByRole('checkbox', { name: 'range-a-run' }));
  fireEvent.click(screen.getByRole('button', { name: 'Load 200 more runs' }));
  await screen.findByText(/More runs unavailable/);
  await waitFor(() => expect(client.isFetching()).toBe(0));
  const requests = fetch.mock.calls.map(([input]) => String(input));
  fireEvent.click(screen.getByRole('button', { name: 'Display UTC' }));
  await waitFor(() => expect(screen.getByLabelText('Timezone')).toHaveValue('utc'));
  expect(fetch.mock.calls.map(([input]) => String(input))).toEqual(requests);
  expect(screen.getByRole('checkbox', { name: 'range-a-run' })).not.toBeChecked();
  expect(screen.getByText('1 loaded of 201 · 0 visible')).toBeInTheDocument();
  const request = new URL(requests.find(url => url.includes('/runs'))!, 'http://localhost');
  expect(request.searchParams.get('start')).toBe('2026-09-16T00:00:00Z');
  expect(request.searchParams.get('end')).toBe('2026-09-16T01:00:00Z');
  expect(request.searchParams.has('tz')).toBe(false);
});