// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
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

function renderDetail(payload: JobDetail) {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(JSON.stringify(payload), {
    status: 200, headers: { 'content-type': 'application/json' },
  }))));
  const client = createPortalQueryClient();
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/portal/workloads/uid-1']}>
    <WorkspaceProvider scope={scope} managed><Routes><Route path="/portal/workloads/:resourceUID" element={<JobDetailBoard/>}/></Routes></WorkspaceProvider>
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
});
