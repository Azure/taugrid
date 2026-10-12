// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { canWatch, RunStatus, RunTarget } from './model';

export interface MonitorState {
  target: RunTarget | null;
  status: RunStatus | null;
  error: string | null;
  busy: boolean;
  watching: boolean;
  checkedAt: number | null;
  watchMessage?: string;
}

interface Clock {
  set(callback: () => void): number;
  clear(handle: number): void;
  now(): number;
}

export class RunMonitor {
  state: MonitorState = {
    target: null, status: null, error: null, busy: false, watching: false, checkedAt: null
  };
  private timer: number | undefined;
  private request: AbortController | null = null;
  private disposed = false;
  private autoStarted = false;
  private watchStarted: number | null = null;

  constructor(
    private read: (target: RunTarget, signal: AbortSignal) => Promise<RunStatus>,
    private changed: (state: MonitorState) => void,
    private clock: Clock = {
      set: callback => window.setTimeout(callback, 10000),
      clear: handle => window.clearTimeout(handle),
      now: () => Date.now()
    },
    private autoWatch = false
  ) {}

  lookup(target: RunTarget): void {
    if (this.disposed) { return; }
    this.request?.abort();
    this.autoStarted = false;
    this.watchStarted = null;
    this.request = null;
    this.clearTimer();
    this.state = {
      target, status: null, error: null, busy: false, watching: false, checkedAt: null
    };
    this.refresh();
  }

  refresh(): void {
    if (this.disposed || this.request || !this.state.target) { return; }
    this.clearTimer();
    const request = new AbortController();
    this.request = request;
    this.publish({ busy: true });
    void this.read(this.state.target, request.signal).then(status => {
      if (this.disposed || this.request !== request) { return; }
      const now = this.clock.now();
      let watching = this.state.watching && canWatch(status);
      let watchMessage = this.state.watchMessage;
      if (this.autoWatch && !this.autoStarted && canWatch(status)) {
        this.autoStarted = true;
        this.watchStarted = now;
        watching = true;
      }
      if (watching && this.watchStarted !== null && now - this.watchStarted >= 3600000) {
        watching = false;
        watchMessage = 'Watching paused after one hour. Start watching to continue.';
      }
      if (status.terminal) {
        watchMessage = 'Run finished. Automatic watch stopped; review final evidence and refresh to retry unavailable metrics.';
      }
      this.publish({ status, error: null, checkedAt: now, watching, watchMessage });
    }).catch(error => {
      if (this.disposed || this.request !== request) { return; }
      this.publish({
        error: error instanceof Error ? error.message : String(error), watching: false
      });
    }).finally(() => {
      if (this.disposed || this.request !== request) { return; }
      this.request = null;
      this.publish({ busy: false });
      this.schedule();
    });
  }

  setWatching(watching: boolean): void {
    if (this.disposed) { return; }
    this.autoStarted = true;
    this.watchStarted = watching ? this.clock.now() : null;
    this.publish({ watchMessage: watching ? undefined : 'Watching paused.' });
    this.clearTimer();
    this.publish({ watching: watching && !this.state.error && canWatch(this.state.status) });
    this.schedule();
  }

  dispose(): void {
    this.disposed = true;
    this.request?.abort();
    this.request = null;
    this.clearTimer();
  }

  private publish(update: Partial<MonitorState>): void {
    this.state = { ...this.state, ...update };
    this.changed(this.state);
  }

  private clearTimer(): void {
    if (this.timer !== undefined) { this.clock.clear(this.timer); }
    this.timer = undefined;
  }

  private schedule(): void {
    if (this.state.watching && !this.request && !this.disposed) {
      this.timer = this.clock.set(() => {
        this.timer = undefined;
        if (this.watchStarted !== null && this.clock.now() - this.watchStarted >= 3600000) {
          this.publish({ watching: false, watchMessage: 'Watching paused after one hour. Start watching to continue.' });
          return;
        }
        this.refresh();
      });
    }
  }
}
