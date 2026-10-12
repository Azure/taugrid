// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { NamespaceRow, parseNamespaces } from './model';

export interface DestinationProfile {
  name: string;
  description: string;
  workers: number;
  gpusPerWorker: number;
  cpusPerWorker: string;
  memoryPerWorker: string;
  priority: string | null;
  image: string;
  queue: string;
}

export interface Destinations {
  namespace: string | null;
  namespaces: NamespaceRow[];
  profiles: DestinationProfile[];
  queues: string[];
  defaultQueue: string | null;
  warnings: string[];
  limits: { namespaces: number; profiles: number; queues: number };
}

export function namespaceLabel(namespace: NamespaceRow): string {
  return `${namespace.name} (${namespace.tauEnabled ? 'Tau-labelled' : 'Not labelled for Tau work'})`;
}

export function preferredNamespace(namespaces: NamespaceRow[]): string {
  return (namespaces.find(namespace => namespace.tauEnabled) || namespaces[0])?.name || '';
}

export function parseDestinations(value: unknown): Destinations {
  const record = (item: unknown): item is Record<string, unknown> => typeof item === 'object' && item !== null && !Array.isArray(item);
  const text = (item: unknown): item is string => typeof item === 'string' && item.length > 0;
  const count = (item: unknown): boolean => typeof item === 'number' && Number.isSafeInteger(item) && item >= 0;
  const invalid = (): never => { throw new Error('The server returned an invalid destination catalog. Refresh destinations or ask the operator to update the extension.'); };
  if (!record(value)) return invalid();
  const { namespaces } = parseNamespaces(value);
  if (namespaces.length > 500 || new Set(namespaces.map(row => row.name)).size !== namespaces.length ||
      !(value.namespace === null || namespaces.some(row => row.name === value.namespace)) ||
      !Array.isArray(value.queues) || value.queues.length > 500 || !value.queues.every(text) ||
      new Set(value.queues).size !== value.queues.length ||
      !(value.defaultQueue === null || text(value.defaultQueue)) ||
      !Array.isArray(value.profiles) || value.profiles.length > 200 ||
      !value.profiles.every(profile => record(profile) && text(profile.name) && typeof profile.description === 'string' &&
        count(profile.workers) && count(profile.gpusPerWorker) && text(profile.cpusPerWorker) && text(profile.memoryPerWorker) &&
        text(profile.image) && text(profile.queue) && (profile.priority === null || text(profile.priority))) ||
      new Set(value.profiles.map(profile => profile.name)).size !== value.profiles.length ||
      !Array.isArray(value.warnings) || !value.warnings.every(item => typeof item === 'string') ||
      !record(value.limits) || value.limits.namespaces !== 500 || value.limits.profiles !== 200 || value.limits.queues !== 500) return invalid();
  return value as unknown as Destinations;
}

export interface DestinationState {
  value: Destinations | null;
  busy: boolean;
  error: string | null;
}

export class DestinationSession {
  state: DestinationState = { value: null, busy: false, error: null };
  private request: AbortController | null = null;
  private disposed = false;

  constructor(private get: (namespace: string, signal: AbortSignal) => Promise<unknown>, private changed: (state: DestinationState) => void) {}

  async refresh(namespace: string): Promise<void> {
    if (this.disposed) return;
    this.request?.abort();
    const request = new AbortController();
    this.request = request;
    this.publish({ value: null, busy: true, error: null });
    try {
      const value = parseDestinations(await this.get(namespace, request.signal));
      if (namespace && value.namespace !== namespace) throw new Error('The catalog belongs to a different namespace. Refresh destinations.');
      if (!this.disposed && this.request === request) this.publish({ value, busy: false, error: null });
    } catch (error) {
      if (!this.disposed && this.request === request) this.publish({ value: null, busy: false, error: String(error) });
    }
  }

  dispose(): void {
    this.disposed = true;
    this.request?.abort();
  }

  private publish(state: DestinationState): void {
    this.state = state;
    this.changed(state);
  }
}
