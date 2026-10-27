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

export interface ProfileGapNotice { title: string; action: string; }

/**
 * An empty profile picker has several distinct causes, and only one of them is
 * the researcher's problem. The server's ``warnings`` carry the per-profile
 * reason (``tau/_profile.py`` prefixes each with "is not offered in this scope",
 * which wrongly implies authorization for readiness and renderer failures too),
 * so classify on the reason and name the actor who can actually act.
 */
export function describeProfileGap(warnings: string[]): ProfileGapNotice {
  const text = warnings.join('\n').toLowerCase();
  if (/does not identify a (team|lane)|does not authorize (team|lane)|team-scoped|lane-scoped/.test(text)) {
    return {
      title: 'This Jupyter server is not configured for the team or lane these worker profiles require.',
      action: 'Ask the operator to set TAUGRID_TEAM and TAUGRID_LANE for this Jupyter server, then refresh destinations. Until then, team- and lane-scoped profiles are withheld.'
    };
  }
  if (/invalid profile:/.test(text)) {
    return {
      title: 'A worker profile is ready, but this extension cannot read it.',
      action: 'Ask the operator to update the TauGrid extension or the profile definition, then refresh destinations. The profile itself is ready; this is not a permissions problem.'
    };
  }
  if (/stale|missing condition|observedgeneration|has not published a resolved profile set|is not a mapping|is not a list/.test(text)) {
    return {
      title: 'No worker profile is ready yet.',
      action: 'Ask the operator to check the TauCluster controller and its Ready condition, then refresh destinations. This is a cluster or controller problem, not a permissions problem.'
    };
  }
  if (/does not authorize namespace/.test(text)) {
    return {
      title: 'No worker profiles are available in this namespace.',
      action: 'Ask the operator to publish a ready TauCluster profile for this namespace or grant read permission, then refresh destinations.'
    };
  }
  return {
    title: 'No ready worker profiles are available.',
    action: 'Ask the operator to publish a ready TauCluster profile, then refresh destinations.'
  };
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
  /** The raw failure, kept so the view can classify a 404 or a non-JSON body. */
  cause?: unknown;
}

export class DestinationSession {
  state: DestinationState = { value: null, busy: false, error: null };
  private request: AbortController | null = null;
  private disposed = false;
  private permitted: NamespaceRow[] | null = null;

  constructor(private get: (namespace: string, signal: AbortSignal) => Promise<unknown>, private changed: (state: DestinationState) => void) {}

  async refresh(namespace: string): Promise<void> {
    if (this.disposed) return;
    this.request?.abort();
    const request = new AbortController();
    this.request = request;
    this.publish({ value: null, busy: true, error: null, cause: null });
    const requested = namespace || preferredNamespace(this.permitted || []);
    try {
      const value = parseDestinations(await this.get(requested, request.signal));
      if (namespace && value.namespace !== namespace) throw new Error('The catalog belongs to a different namespace. Refresh destinations.');
      const answered = !namespace && requested && value.namespace !== requested;
      this.permitted = answered ? value.namespaces.filter(option => option.name !== requested) : value.namespaces;
      if (!this.disposed && this.request === request) this.publish({ value, busy: false, error: null, cause: null });
    } catch (error) {
      if (!namespace && requested) this.permitted = this.permitted?.filter(option => option.name !== requested) || null;
      if (!this.disposed && this.request === request) this.publish({ value: null, busy: false, error: String(error), cause: error });
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
