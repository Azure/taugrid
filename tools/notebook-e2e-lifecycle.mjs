// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Shared, ownership-sensitive lifecycle for the notebook browser E2E harnesses.
//
// Each browser journey (button, embed, plugin) used to copy this code. Keeping
// one copy matters because the run identity has to exist before any path is
// derived from it -- an earlier version derived RUN_NOTEBOOK first and threw a
// ReferenceError at module load -- and because teardown must delete exactly the
// ids this run created.

import { randomUUID } from "node:crypto";
import path from "node:path";

import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

export function createRunIdentity({ label, notebook, server = "http://127.0.0.1:8888", token = "testplugin" }) {
  // Define the run identity first: workspace, session and notebook path all
  // derive from it, so they can never be built out of order.
  const runId = `${Date.now().toString(36)}-${randomUUID().slice(0, 8)}`;
  const workspace = `tau-${label}-${runId}`;
  const sessionName = `tau-${label}-${runId}`;
  // Jupyter's sessions POST reuses an existing session for the same notebook
  // path and still answers 201, so a unique session name and workspace do not
  // prove ownership -- the returned id can belong to a person's live kernel,
  // which this harness would then execute in and delete. Give every run its own
  // notebook path so no existing session can match it.
  // No leading dot: Jupyter's contents API rejects hidden names with 400, so a
  // dot-prefixed copy never reaches the point of creating a session.
  const runNotebook = `examples/tau-${label}-${runId}.ipynb`;
  const base = server.replace(/\/$/, "");
  return {
    label,
    notebook,
    token,
    runId,
    workspace,
    sessionName,
    runNotebook,
    base,
    api: base,
    notebookUrl: `${base}/lab/workspaces/${workspace}/tree/${runNotebook}?token=${token}`,
  };
}

const fetchOf = (deps) => deps.fetchImpl || globalThis.fetch;
const logOf = (deps) => deps.log || console.log;

// Copy the checked-in notebook to this run's own path. The copy is removed with
// the run's workspace during teardown.
export async function seedNotebook(config, deps = {}) {
  const request = fetchOf(deps);
  const log = logOf(deps);
  const source = await request(`${config.api}/api/contents/${config.notebook}?token=${config.token}&content=1`);
  if (!source.ok) throw new Error(`notebook read failed: ${source.status}`);
  const model = await source.json();
  const created = await request(`${config.api}/api/contents/${config.runNotebook}?token=${config.token}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ type: "notebook", format: model.format, content: model.content }),
  });
  if (!created.ok) throw new Error(`notebook copy failed: ${created.status}`);
  log(`run notebook: ${config.runNotebook}`);
}

// Start the kernel through the documented Jupyter REST API. The unique path
// means JupyterLab cannot restore a dead kernel id from a previous run, so no
// stale-record sweep is needed, and none is performed: a sweep would have to
// guess which of the server's sessions belong to this harness.
export async function startKernelSession(config, deps = {}) {
  const request = fetchOf(deps);
  const log = logOf(deps);
  const body = JSON.stringify({
    kernel: { name: "python3" },
    name: config.sessionName,
    path: config.runNotebook,
    type: "notebook",
  });
  const response = await request(`${config.api}/api/sessions?token=${config.token}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  });
  if (!response.ok) {
    const text = await response.text();
    throw new Error(`kernel session create failed: ${response.status} ${text.slice(0, 200)}`);
  }
  const session = await response.json();
  // The path is unique to this run, so an id that does not carry it is not ours.
  if (session.path !== config.runNotebook) {
    throw new Error(`refusing reused session ${session.id} for ${session.path}`);
  }
  log(`kernel session started: ${session.id} (${session.kernel?.name}) in ${config.workspace}`);
  return { sessionId: session.id, kernelId: session.kernel?.id };
}

export async function seedAndStartKernel(config, deps = {}) {
  await seedNotebook(config, deps);
  return startKernelSession(config, deps);
}

// Delete exactly the ids this run created, then its own workspace. Nothing here
// can reach another run's resources or a person's sessions.
export async function teardownRun(config, handle, deps = {}) {
  if (!handle) return;
  const request = fetchOf(deps);
  const log = logOf(deps);
  if (handle.sessionId) {
    const del = await request(`${config.api}/api/sessions/${handle.sessionId}?token=${config.token}`, { method: "DELETE" });
    log(`run session ${handle.sessionId}: ${del.ok ? "deleted" : `delete failed ${del.status}`}`);
  }
  if (handle.kernelId) {
    const del = await request(`${config.api}/api/kernels/${handle.kernelId}?token=${config.token}`, { method: "DELETE" });
    if (del.ok) log(`run kernel ${handle.kernelId}: deleted`);
  }
  const nb = await request(`${config.api}/api/contents/${config.runNotebook}?token=${config.token}`, { method: "DELETE" });
  if (nb.ok) log(`run notebook ${config.runNotebook}: deleted`);
  const ws = await request(`${config.api}/api/workspaces/${config.workspace}?token=${config.token}`, { method: "DELETE" });
  log(`workspace ${config.workspace}: ${ws.ok ? "removed" : `remove skipped ${ws.status}`}`);
}

export async function importPlaywright() {
  const packageRoot = process.env.PLAYWRIGHT_PACKAGE_ROOT || "";
  if (packageRoot) {
    const requireFromPackageRoot = createRequire(pathToFileURL(path.join(packageRoot, "package.json")));
    return requireFromPackageRoot("playwright");
  }
  return await import("playwright");
}

// Work profile: a persistent Microsoft Edge context (its own user-data dir that
// keeps cookies and work logins across runs). Edge is driven via the msedge
// channel, so no bundled Chromium is used.
const PROFILE_DIR =
  process.env.PLAYWRIGHT_PROFILE_DIR ||
  path.join(process.env.LOCALAPPDATA || process.env.TEMP, "tau-jupyter-work-profile");

export async function launchWorkProfileEdge(playwright, { height = 900 } = {}) {
  return await playwright.chromium.launchPersistentContext(PROFILE_DIR, {
    channel: "msedge",
    headless: process.env.HEADLESS === "1",
    viewport: { width: 1440, height },
    args: [
      "--disable-blink-features=AutomationControlled",
      // A work profile usually routes through a corporate proxy, which breaks
      // the kernel's ws://127.0.0.1 WebSocket ("Kernel Connecting" forever).
      // The Jupyter server is local, so bypass proxying entirely.
      "--no-proxy-server",
    ],
  });
}
