// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { RunTarget, SubmitPlanSummary, SubmitPreview, parseSubmitPreview, parseSubmitResponse } from './model';

export interface SubmissionPayload {
  notebook: string;
  path: string;
  namespace: string;
  name?: string;
  profile?: string;
  queue?: string;
  /** Files beside the notebook that should ship with it. */
  files?: string[];
  /**
   * "ray" only when the notebook dispatches its GPU work through Ray. The
   * notebook kernel runs on the RayJob's CPU-only head, so the server refuses a
   * GPU profile unless this acknowledgement is sent.
   */
  gpuExecution?: string;
}

export interface SubmissionState {
  preview: SubmitPreview | null;
  busy: boolean;
  submitted: RunTarget | null;
  attempted: RunTarget | null;
  uncertain: boolean;
  error: string | null;
  failure?: SubmissionFailure | null;
}

export interface SubmissionFailure {
  title: string;
  action: string;
  detail: string;
}

/** The failure voice for a genuine authorization refusal. */
function accessRequired(detail: string): SubmissionFailure {
  return { title: 'Access is required', action: 'Sign in to Jupyter or ask the operator for the missing permission. Then refresh and review again.', detail };
}

/**
 * A server API that is not loaded is an operator's gate misconfiguration, not a
 * researcher's sign-in, connectivity, or RBAC problem. A 404 (or a body that is
 * not JSON at all: Jupyter's HTML error and login pages) must say so and name the
 * actor that can fix it, rather than blaming the person looking at the form.
 */
function apiUnavailable(detail: string): SubmissionFailure {
  return {
    title: 'The TauGrid server API is not available on this Jupyter server',
    action: "Ask the operator to check the Jupyter server log for a 'tau.jupyter | extension failed loading' warning - usually TAUGRID_SUBMISSION_ENABLED is set without TAUGRID_RUNTIME_IMAGE.",
    detail
  };
}

interface FailureCause { status?: number; nonJson?: boolean; message?: string; }

const failureCause = (error: unknown): FailureCause =>
  typeof error === 'object' && error !== null ? error as FailureCause : {};

/**
 * Map an endpoint failure to the copy both the destinations and the capabilities
 * surfaces show, so the two call sites cannot drift. The raw server text stays in
 * ``detail`` (rendered inside the expandable block only).
 */
export function endpointFailure(error: unknown): SubmissionFailure {
  const cause = failureCause(error);
  const detail = cause.message || String(error);
  if (cause.status === 401 || cause.status === 403) { return accessRequired(detail); }
  if (cause.status === 404 || cause.nonJson === true) { return apiUnavailable(detail); }
  return { title: 'The Jupyter server could not be reached', action: 'Check the Jupyter connection and retry. If this keeps failing, ask the operator to check the server log.', detail };
}

export function submissionFailure(error: unknown, uncertain = false): SubmissionFailure {
  const cause = error as { status?: number; message?: string } | null;
  const detail = cause?.message || String(error);
  if (uncertain) return { title: 'Submission outcome not confirmed', action: 'Inspect planned run before trying again. Closing this tab does not cancel a server-side submission.', detail };
  if (cause?.status === 413) return { title: 'Notebook package is too large', action: 'Remove selected files, clear notebook outputs, or put larger assets in the runtime image. Then review again.', detail };
  if (cause?.status === 401 || cause?.status === 403) return accessRequired(detail);
  if (cause?.status === 404 || (error as FailureCause | null)?.nonJson === true) return apiUnavailable(detail);
  if (cause?.status === 409) {
    if (/disabled|enable/i.test(detail)) return { title: 'Submission is disabled', action: 'Ask the operator to enable submission after validating the runtime, then check availability again.', detail };
    if (/already exists/i.test(detail)) return { title: 'Run name is already in use', action: 'Inspect the existing run in Runs. Choose a different name only if you intend a new run, then review again.', detail };
    return { title: 'The submission plan needs a new review', action: 'Refresh destinations and review again. If no ready profile is available, ask the operator to publish one.', detail };
  }
  if (cause?.status === 400) return { title: 'The notebook cannot be reviewed yet', action: 'Correct the indicated run name, notebook, or file selection, then review again.', detail };
  return { title: 'Review could not finish', action: 'Check the Jupyter server and cluster connection, then review again. If this notebook was moved or closed, reopen Submit notebook from its toolbar.', detail };
}

export class SubmissionSession {
  state: SubmissionState = { preview: null, busy: false, submitted: null, attempted: null, uncertain: false, error: null };
  private request: AbortController | null = null;
  private payload: SubmissionPayload | null = null;
  private disposed = false;
  private writing = false;

  constructor(
    private post: (path: string, body: SubmissionPayload & { confirm?: boolean }, signal: AbortSignal) => Promise<unknown>,
    private changed: (state: SubmissionState) => void
  ) {}

  async review(payload: SubmissionPayload): Promise<void> {
    if (this.disposed || this.writing || this.state.submitted || this.state.uncertain) return;
    this.invalidate();
    const request = new AbortController();
    this.request = request;
    const captured = { ...payload, files: payload.files ? [...payload.files] : undefined };
    this.publish({ busy: true, error: null, failure: null });
    try {
      const preview = parseSubmitPreview(await this.post('preview', captured, request.signal));
      if (this.disposed || this.request !== request) return;
      this.payload = captured;
      this.publish({ preview });
    } catch (error) {
      if (!this.disposed && this.request === request) this.publish({ error: String(error), failure: submissionFailure(error) });
    } finally {
      if (!this.disposed && this.request === request) {
        this.request = null;
        this.publish({ busy: false });
      }
    }
  }

  async submit(plan: SubmitPlanSummary, confirmed: boolean, capabilityReady: boolean): Promise<boolean> {
    const preview = this.state.preview;
    if (this.disposed || this.state.busy || this.state.submitted || !confirmed || !capabilityReady ||
        !preview?.submittable || !preview.submissionEnabled || preview.plan !== plan || !this.payload) return false;
    const request = new AbortController();
    this.request = request;
    this.writing = true;
    const attempted: RunTarget = { namespace: plan.namespace, name: plan.name, kind: 'RayJob' };
    const payload = { ...this.payload, namespace: plan.namespace, name: plan.name, planDigest: plan.planDigest, confirm: true };
    this.payload = null;
    this.publish({ preview: null, busy: true, error: null, failure: null, attempted });
    try {
      const result = parseSubmitResponse(await this.post('submit', payload, request.signal), plan);
      if (this.disposed || this.request !== request) return false;
      if (!result.submitted) throw new Error('The server did not confirm submission. Inspect the planned run before reviewing again.');
      this.publish({ submitted: { namespace: result.namespace, name: result.name, kind: result.kind === 'Job' ? 'Job' : 'RayJob' }, uncertain: false });
      return true;
    } catch (error) {
      if (!this.disposed && this.request === request) this.publish({ error: String(error), failure: submissionFailure(error, true), uncertain: true });
      return false;
    } finally {
      this.writing = false;
      if (!this.disposed && this.request === request) {
        this.request = null;
        this.publish({ busy: false });
      }
    }
  }

  invalidate(): void {
    if (this.disposed || this.writing) return;
    this.request?.abort();
    this.request = null;
    this.payload = null;
    this.publish({ preview: null, busy: false, error: null, failure: null });
  }

  acknowledgeUncertain(): void {
    if (!this.state.uncertain || this.state.busy || this.disposed) return;
    this.publish({ uncertain: false, attempted: null, error: null, failure: null });
  }

  dispose(): void {
    this.disposed = true;
    this.request?.abort();
    this.request = null;
    this.payload = null;
  }

  private publish(update: Partial<SubmissionState>): void {
    this.state = { ...this.state, ...update };
    this.changed(this.state);
  }
}
