// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
import { createContext, useContext, type ReactNode } from 'react';
import { QueryCache, QueryClient, useQuery, useQueryClient, type Query, type UseQueryResult } from '@tanstack/react-query';
import type { Directory, WorkspaceScope } from './types';

export class APIError extends Error {
  constructor(public status: number, public state: string, detail: string) { super(`${status} ${detail}`); }
}

export function requestRejected(error: Error | null | undefined): boolean {
  return error instanceof APIError && error.status >= 400 && error.status < 500;
}

export function createPortalQueryClient() {
  const cache = new QueryCache();
  const denied = new WeakSet<Query>();
  cache.subscribe(event => {
    if (event.type !== 'updated') return;
    const { query, action } = event;
    if (action.type === 'success' && !action.manual) denied.delete(query);
    if (requestRejected(query.state.error) || requestRejected(query.state.fetchFailureReason)) denied.add(query);
    // A later failure or cancellation must not restore a rejected payload.
    if (denied.has(query) && query.state.data !== undefined) query.setState({ data: undefined, dataUpdatedAt: 0 });
  });
  return new QueryClient({ queryCache: cache, defaultOptions: { queries: { retry: false, staleTime: 15_000 } } });
}

export function readableQuery<T>(query: UseQueryResult<T, Error>) {
  // A refetch can still have success status while a rejected attempt is retrying.
  const error = requestRejected(query.failureReason) ? query.failureReason : query.error || query.failureReason;
  return { ...query, error, isError: !!error, data: requestRejected(error) ? undefined : query.data };
}

export function staleReadMessage(query: { data: unknown; dataUpdatedAt: number }): string {
  return query.data === undefined ? '' : 'Showing stale data from the last successful read' +
    (query.dataUpdatedAt ? ' at ' + new Date(query.dataUpdatedAt).toLocaleString() : '') + '.';
}

export async function fetchJSON<T>(url: string, signal: AbortSignal): Promise<T> {
  const response = await fetch(url, { signal, headers: { accept: 'application/json' } });
  if (!response.ok) {
    const raw = (await response.text()).trim();
    let detail = raw, state = '';
    try {
      const body: unknown = JSON.parse(raw);
      if (body && typeof body === 'object') {
        if ('error' in body && typeof body.error === 'string') detail = body.error;
        if ('reason' in body && typeof body.reason === 'string') detail = body.reason;
        if ('detail' in body && typeof body.detail === 'string') detail = body.detail;
        if ('code' in body && typeof body.code === 'string') state = body.code;
        if ('state' in body && typeof body.state === 'string') state = body.state;
      }
    } catch (error) {
      // Non-JSON proxy errors are displayed verbatim as text.
      if (!(error instanceof SyntaxError)) throw error;
    }
    throw new APIError(response.status, state, detail);
  }
  return response.json() as Promise<T>;
}

export function scopedURL(path: string, workspace: string, managed: boolean, source?: string): string {
  const url = new URL(path, window.location.origin);
  if (url.protocol !== 'http:' && url.protocol !== 'https:') throw new Error('Unsupported integration URL protocol');
  if (url.origin !== window.location.origin) return url.href;
  if (managed && workspace) url.searchParams.set('workspace', workspace);
  if (source && url.pathname.startsWith('/api/v2/stellar/')) url.searchParams.set('source', source);
  return url.pathname + url.search + url.hash;
}

export function experimentsAPI(scope: WorkspaceScope): string | undefined {
  return scope.experimentsNative?.state === 'available' && scope.experimentsNative.apiBasePath === '/api/v2/stellar'
    ? '/api/v2/stellar/experiments' : undefined;
}

export function experimentPageURL(scope: WorkspaceScope, managed: boolean, search: string): string | undefined {
  const target = new URL('/portal/experiments', window.location.origin);
  const params = new URLSearchParams(search);
  const scopeKeys = new Set(['workspace', 'source', 'namespace', 'cluster', 'team', 'queue', 'embed']);
  for (const name of new Set(params.keys())) {
    if (scopeKeys.has(name)) continue;
    for (const value of params.getAll(name)) target.searchParams.append(name, value);
  }
  return scopedURL(target.pathname + target.search + target.hash, scope.workspace, managed);
}

export function nativeExperimentURL(path: string, configuredPage?: string): string {
  const url = new URL(path, window.location.origin);
  const configured = configuredPage ? new URL(configuredPage, window.location.origin) : undefined;
  const matchesConfigured = configured && url.origin === configured.origin && url.pathname === configured.pathname;
  if (!(url.pathname === '/stellar' || url.pathname.startsWith('/stellar/') || matchesConfigured)) return path;
  const legacyRun = url.pathname.match(/^\/stellar\/runs\/([^/]+)\/?$/);
  if (legacyRun && !url.searchParams.has('target')) url.searchParams.set('target', decodeURIComponent(legacyRun[1]));
  url.searchParams.delete('embed');
  return '/portal/experiments' + url.search + url.hash;
}

export function remoteWorkspaceURL(scope: WorkspaceScope, pathname: string, search: string, hash = '') {
  if (!scope.portalEndpoint) return '';
  const url = new URL(scope.portalEndpoint);
  if (!['https:', 'http:'].includes(url.protocol)) throw new Error('Unsupported portal endpoint protocol');
  url.pathname = url.pathname.replace(/\/$/, '') + pathname;
  url.search = search;
  url.hash = hash;
  url.searchParams.set('workspace', scope.workspace);
  url.searchParams.delete('namespace');
  url.searchParams.delete('cluster');
  return url.href;
}

interface ScopeContext { scope: WorkspaceScope; managed: boolean }
const WorkspaceContext = createContext<ScopeContext | null>(null);
export function WorkspaceProvider({ scope, managed, children }: ScopeContext & { children: ReactNode }) {
  return <WorkspaceContext.Provider value={{ scope, managed }}>{children}</WorkspaceContext.Provider>;
}
export function useWorkspace() {
  const value = useContext(WorkspaceContext);
  if (!value) throw new Error('Workspace authorization must complete before rendering a board');
  return value;
}
export function useScopedURL() {
  const { scope, managed } = useWorkspace();
  return (path: string) => scopedURL(path, scope.workspace, managed, scope.source);
}
export function useDirectory(workspace: string) {
  return useQuery({
    queryKey: ['workspace-directory', workspace],
    queryFn: ({ signal }) => fetchJSON<Directory>('/api/portal/workspaces' + (workspace ? '?workspace=' + encodeURIComponent(workspace) : ''), signal),
    staleTime: 0,
  });
}
export function useBoard<T>(path: string, enabled = true, merge?: (previous: T | undefined, next: T) => T) {
  const { scope, managed } = useWorkspace();
  const client = useQueryClient();
  const url = scopedURL(path, scope.workspace, managed, scope.source);
  const queryKey = [...boardScopeKey(scope, managed), url];
  return useQuery({
    // Include resolved authorization/data-source identity, not just a board name.
    queryKey,
    queryFn: async ({ signal }) => {
      const next = await fetchJSON<T>(url, signal);
      return merge ? merge(client.getQueryData<T>(queryKey), next) : next;
    },
    enabled,
    refetchOnWindowFocus: !path.startsWith('/api/v2/stellar/'),
    refetchOnReconnect: !path.startsWith('/api/v2/stellar/'),
  });
}
export function boardScopeKey(scope: WorkspaceScope, managed: boolean) {
  return ['board', scope.workspace, scope.cluster, scope.namespace, scope.localQueue,
    scope.source, scope.resultScope, scope.authorizationMode, scope.experimentsUrl,
    scope.experimentsNative?.state, scope.experimentsNative?.apiBasePath, managed];
}
