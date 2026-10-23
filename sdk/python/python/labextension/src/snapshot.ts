// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export interface SnapshotState<T> {
  busy: boolean;
  value: T | null;
  error: string | null;
}

export class SnapshotRequest<T> {
  private controller: AbortController | null = null;
  private disposed = false;

  constructor(private readonly changed: (state: SnapshotState<T>) => void) {}

  reset(): void {
    this.controller?.abort();
    this.controller = null;
    if (!this.disposed) { this.changed({ busy: false, value: null, error: null }); }
  }

  async load(read: (signal: AbortSignal) => Promise<T>): Promise<void> {
    if (this.disposed) { return; }
    this.reset();
    const controller = new AbortController();
    this.controller = controller;
    this.changed({ busy: true, value: null, error: null });
    try {
      const value = await read(controller.signal);
      if (!controller.signal.aborted) { this.changed({ busy: false, value, error: null }); }
    } catch (error) {
      if (!controller.signal.aborted) { this.changed({ busy: false, value: null, error: error instanceof Error ? error.message : String(error) }); }
    } finally {
      if (this.controller === controller) { this.controller = null; }
    }
  }

  dispose(): void {
    this.disposed = true;
    this.reset();
  }
}
