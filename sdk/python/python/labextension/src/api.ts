// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { URLExt } from '@jupyterlab/coreutils';
import { ServerConnection } from '@jupyterlab/services';
import { RunTarget } from './model';

export class ApiError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
  }
}

export type ApiQuery = Partial<RunTarget> & { queue?: string; pod?: string; container?: string; tail?: string; previous?: string; timestamps?: string; includeMetrics?: string; path?: string };

export function apiUrl(path: string, target?: ApiQuery): string {
  const settings = ServerConnection.makeSettings();
  const query = target ? `?${new URLSearchParams(Object.entries(target).filter(([, value]) => value !== undefined) as [string, string][])}` : '';
  return URLExt.join(settings.baseUrl, 'taugrid', 'api', path) + query;
}

async function request<T>(path: string, init: RequestInit, target?: ApiQuery): Promise<T> {
  const settings = ServerConnection.makeSettings();
  const controller = new AbortController();
  const abort = (): void => controller.abort();
  init.signal?.addEventListener('abort', abort, { once: true });
  if (init.signal?.aborted) { controller.abort(); }
  let timedOut = false;
  const timeout = window.setTimeout(() => { timedOut = true; controller.abort(); }, 30000);
  try {
    const response = await ServerConnection.makeRequest(apiUrl(path, target), {
      ...init, signal: controller.signal, cache: 'no-store',
      headers: { Accept: 'application/json', ...(init.headers || {}) }
    }, settings);
    const text = await response.text();
    let body: unknown = undefined;
    try {
      body = text ? JSON.parse(text) : undefined;
    } catch {
      if (response.ok) {
        throw new Error('The Jupyter server did not return status JSON. Check your sign-in and the TauGrid server extension.');
      }
      body = text;
    }
    if (!response.ok) {
      const detail = typeof body === 'object' && body !== null && typeof (body as Record<string, unknown>).message === 'string'
        ? String((body as Record<string, unknown>).message)
        : `HTTP ${response.status}`;
      throw new ApiError(detail, response.status);
    }
    return body as T;
  } catch (error) {
    if (timedOut) { throw new Error('The Jupyter server did not respond within 30 seconds. Check its connection and retry.'); }
    throw error;
  } finally {
    window.clearTimeout(timeout);
    init.signal?.removeEventListener('abort', abort);
  }
}

export function apiGet<T>(path: string, target?: ApiQuery, signal?: AbortSignal): Promise<T> {
  return request<T>(path, { method: 'GET', signal }, target);
}

export function apiPost<T>(path: string, body: unknown, signal?: AbortSignal): Promise<T> {
  return request<T>(path, {
    method: 'POST',
    body: JSON.stringify(body ?? {}),
    headers: { 'Content-Type': 'application/json' },
    signal
  });
}
