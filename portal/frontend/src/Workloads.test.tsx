// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { WorkspaceProvider, createPortalQueryClient } from './data';
import { JobDetailBoard } from './Workloads';
import type { JobDetail, WorkspaceScope } from './types';

const scope: WorkspaceScope = {
  workspace: 'flex', name: 'Flex', cluster: 'cluster-a', namespace: 'team-a',
  source: 'portal', authorizationMode: 'namespace', availability: 'available', managed: true,
};

function detail(overrides: Partial<JobDetail>): JobDetail {
  return {
    name: 'train', namespace: 'team-a', kind: 'RayJob', resourceUid: 'uid-1',
    objectState: 'live', status: 'Running', runId: 'run-1',
    stages: { object: 'live', admission: 'admitted', scheduling: 'scheduled', application: 'running', tracking: 'linked' },
    object: { age: '5m' },
    links: { rayDashboardReachable: false, stellarPath: '/portal/experiments?target=run-1' },
    diagnostics: {
      workloads: { state: 'ready' }, pods: { state: 'ready' }, events: { state: 'empty' },
      tracking: { state: 'ready' }, telemetry: { state: 'empty', message: 'No GPU samples.' },
    },
    workloads: [{ name: 'wl-1', queue: 'gpu', admitted: true, finished: false }],
    pods: [{ name: 'head', phase: 'Running', node: 'gpu-a', restarts: 0, containers: [{ name: 'ray-head', ready: true, restarts: 0, state: 'running' }] }],
    ...overrides,
  };
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

function renderDetail(payload: JobDetail, initialEntry = '/portal/workloads/uid-1') {
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request) => {
    const body = String(input).includes('/logs?')
      ? { pod: 'head', container: 'ray-head', previous: false, content: 'training', tailLines: 200, limitBytes: 262144 }
      : payload;
    return Promise.resolve(new Response(JSON.stringify(body), {
      status: 200, headers: { 'content-type': 'application/json' },
    }));
  }));
  const client = createPortalQueryClient();
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[initialEntry]}>
    <WorkspaceProvider scope={scope} managed><LocationProbe/><Routes><Route path="/portal/workloads/:resourceUID" element={<JobDetailBoard/>}/></Routes></WorkspaceProvider>
  </MemoryRouter></QueryClientProvider>);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Workload detail lifecycle', () => {
  it.each([
    ['pending', detail({ status: 'Pending', stages: { object: 'live', admission: 'pending', scheduling: 'no_pods', application: 'waiting', tracking: 'unlinked' }, pods: [], workloads: [{ name: 'wl-1', queue: 'gpu', admitted: false, finished: false, pendingReason: 'Pending', pendingMessage: 'insufficient quota' }] }), 'insufficient quota'],
    ['running', detail({}), 'running'],
    ['failed', detail({ status: 'Failed', stages: { object: 'live', admission: 'finished', scheduling: 'completed', application: 'failed', tracking: 'linked' }, object: { age: '10m', reason: 'JobFailed', message: 'worker exited' } }), 'failed'],
    ['deleted', detail({ objectState: 'deleted', status: 'Succeeded', stages: { object: 'deleted', admission: 'unavailable', scheduling: 'unavailable', application: 'succeeded', tracking: 'linked' }, pods: [], workloads: [], diagnostics: {
      workloads: { state: 'unavailable', message: 'deleted' }, pods: { state: 'unavailable', message: 'deleted' },
      events: { state: 'unavailable', message: 'expired' }, tracking: { state: 'ready' }, telemetry: { state: 'unavailable', message: 'historical attribution unavailable' },
    } }), 'Object deleted'],
  ])('renders distinct %s evidence without conflating stages', async (_name, payload, expected) => {
    renderDetail(payload);
    expect((await screen.findAllByText(expected, { exact: false })).length).toBeGreaterThan(0);
    expect(screen.getByLabelText('Workload lifecycle')).toHaveTextContent(`Admission ${payload.stages.admission}`);
    expect(screen.getByLabelText('Workload lifecycle')).toHaveTextContent(`Application ${payload.stages.application}`);
  });

  it('shows temperature and power consumption gauges with absolute readings', async () => {
    renderDetail(detail({
      telemetry: {
        start: '2026-09-23T00:00:00Z', end: '2026-09-23T01:00:00Z', gpuCount: 1,
        sampleCount: 80, utilizationSampleCount: 10, averageUtilizationPct: 92, coverage: 'observed',
        gpus: [{
          instance: 'gpu-a', pod: 'worker-0', gpu: '0', samples: 80, utilizationSamples: 10,
          averageUtilizationPct: 92, peakUtilizationPct: 99, maxTemperatureCelsius: 84,
          maxPowerWatts: 675, maxMemoryUsedMB: 74000, maxRowRemapFailure: 0,
        }],
      },
      diagnostics: {
        workloads: { state: 'ready' }, pods: { state: 'ready' }, events: { state: 'empty' },
        tracking: { state: 'ready' }, telemetry: { state: 'ready' },
      },
    }));
    expect(await screen.findByRole('meter', { name: 'Peak GPU temperature: 84.0 °C' })).toHaveAttribute('aria-valuemax', '100');
    expect(screen.getByRole('meter', { name: 'Peak GPU power consumption: 675.0 W' })).toHaveAttribute('aria-valuemax', '1000');
  });

  it('keeps workload and workspace scope in all log navigation links', async () => {
    const payload = detail({
      pods: [{
        name: 'head', phase: 'Running', node: 'gpu-a', restarts: 1,
        containers: [{ name: 'ray-head', ready: true, restarts: 1, state: 'running', previousAvailable: true }],
      }],
    });
    renderDetail(payload, '/portal/workloads/uid-1?view=pods&workspace=flex');

    await userEvent.click(await screen.findByRole('link', { name: 'ray-head' }));
    let url = new URL(screen.getByTestId('location').textContent || '', 'https://portal.example');
    expect(url.pathname).toBe('/portal/workloads/uid-1');
    expect(url.searchParams.get('view')).toBe('logs');
    expect(url.searchParams.get('pod')).toBe('head');
    expect(url.searchParams.get('container')).toBe('ray-head');
    expect(url.searchParams.get('workspace')).toBe('flex');

    await userEvent.click(await screen.findByRole('link', { name: 'Choose another container' }));
    url = new URL(screen.getByTestId('location').textContent || '', 'https://portal.example');
    expect(url.pathname).toBe('/portal/workloads/uid-1');
    expect(url.searchParams.get('view')).toBe('logs');
    expect(url.searchParams.get('pod')).toBeNull();
    expect(url.searchParams.get('workspace')).toBe('flex');

    expect(screen.getByRole('link', { name: 'current' })).toHaveAttribute(
      'href', '/portal/workloads/uid-1?view=logs&pod=head&container=ray-head&workspace=flex',
    );
    expect(screen.getByRole('link', { name: 'previous' })).toHaveAttribute(
      'href', '/portal/workloads/uid-1?view=logs&pod=head&container=ray-head&previous=true&workspace=flex',
    );
  });
});
