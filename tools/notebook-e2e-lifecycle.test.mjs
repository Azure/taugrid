// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Mocked API tests for tools/notebook-e2e-lifecycle.mjs. No browser, no server:
// they guard the ownership-sensitive parts (identity ordering, unique notebook
// paths, exact-id teardown) that the real harnesses rely on.

import assert from "node:assert/strict";
import { test } from "node:test";

import {
  createRunIdentity,
  seedAndStartKernel,
  seedNotebook,
  startKernelSession,
  teardownRun,
} from "./notebook-e2e-lifecycle.mjs";

function jsonResponse(body, { ok = true, status = 200 } = {}) {
  return {
    ok,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  };
}

function mockFetch(handler) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push({ url: String(url), init });
    return handler(String(url), init, calls);
  };
  fetchImpl.calls = calls;
  return fetchImpl;
}

const silent = { log: () => {} };

test("run identity exists before any path derived from it", () => {
  // A previous version derived RUN_NOTEBOOK from RUN_ID before RUN_ID was
  // initialised and threw a ReferenceError at module load. Two identities must
  // also never share a path, or Jupyter can hand back a reused session.
  const first = createRunIdentity({ label: "button-e2e", notebook: "examples/notebook-button-e2e.ipynb" });
  const second = createRunIdentity({ label: "button-e2e", notebook: "examples/notebook-button-e2e.ipynb" });

  assert.ok(first.runId);
  assert.ok(first.runNotebook.includes(first.runId));
  assert.ok(first.workspace.includes(first.runId));
  assert.ok(first.sessionName.includes(first.runId));
  assert.notEqual(first.runNotebook, second.runNotebook);
  assert.notEqual(first.workspace, second.workspace);
  assert.equal(first.notebookUrl, `${first.base}/lab/workspaces/${first.workspace}/tree/${first.runNotebook}?token=${first.token}`);
});

test("seedNotebook copies the source notebook to this run's own path", async () => {
  const config = createRunIdentity({ label: "plugin-e2e", notebook: "examples/notebook-panel.ipynb", token: "tok" });
  const fetchImpl = mockFetch((url, init) => {
    if (url.includes("/api/contents/examples/notebook-panel.ipynb")) {
      return jsonResponse({ format: "json", content: { cells: [] } });
    }
    assert.equal(init.method, "PUT");
    return jsonResponse({});
  });

  await seedNotebook(config, { fetchImpl, ...silent });

  const [source, created] = fetchImpl.calls;
  assert.ok(source.url.includes("content=1"));
  assert.ok(created.url.includes(`/api/contents/${config.runNotebook}`));
  const body = JSON.parse(created.init.body);
  assert.equal(body.type, "notebook");
  assert.equal(body.format, "json");
});

test("startKernelSession returns ids only when the session carries this run's path", async () => {
  const config = createRunIdentity({ label: "embed-e2e", notebook: "examples/notebook-embed.ipynb" });
  const matching = mockFetch(() => jsonResponse({ id: "session-1", path: config.runNotebook, kernel: { id: "kernel-1", name: "python3" } }));

  const handle = await startKernelSession(config, { fetchImpl: matching, ...silent });

  assert.deepEqual(handle, { sessionId: "session-1", kernelId: "kernel-1" });
  const [call] = matching.calls;
  assert.equal(JSON.parse(call.init.body).path, config.runNotebook);
});

test("startKernelSession refuses a reused session for another path", async () => {
  const config = createRunIdentity({ label: "embed-e2e", notebook: "examples/notebook-embed.ipynb" });
  const reuse = mockFetch(() => jsonResponse({ id: "victim", path: "examples/someone-else.ipynb", kernel: { id: "kernel-9" } }));

  await assert.rejects(
    () => startKernelSession(config, { fetchImpl: reuse, ...silent }),
    /refusing reused session victim/,
  );
});

test("teardown deletes exactly the ids and paths this run created", async () => {
  const config = createRunIdentity({ label: "button-e2e", notebook: "examples/notebook-button-e2e.ipynb", token: "tok" });
  const fetchImpl = mockFetch(() => jsonResponse({}));

  await teardownRun(config, { sessionId: "session-7", kernelId: "kernel-7" }, { fetchImpl, ...silent });

  const deletes = fetchImpl.calls.map(call => call.url);
  assert.ok(deletes.some(url => url.includes("/api/sessions/session-7")));
  assert.ok(deletes.some(url => url.includes("/api/kernels/kernel-7")));
  assert.ok(deletes.some(url => url.includes(`/api/contents/${config.runNotebook}`)));
  assert.ok(deletes.some(url => url.includes(`/api/workspaces/${config.workspace}`)));
  // Nothing may address an id this run did not create.
  assert.ok(!deletes.some(url => url.includes("session-") && !url.includes("session-7")));
  assert.ok(!deletes.some(url => url.includes("kernel-") && !url.includes("kernel-7")));
});

test("seedAndStartKernel seeds before it starts the session", async () => {
  const config = createRunIdentity({ label: "plugin-e2e", notebook: "examples/notebook-panel.ipynb" });
  const order = [];
  const fetchImpl = mockFetch((url, init) => {
    if (url.includes("/api/contents/examples/notebook-panel.ipynb")) {
      order.push("read");
      return jsonResponse({ format: "json", content: {} });
    }
    if (url.includes("/api/sessions")) {
      order.push("session");
      return jsonResponse({ id: "s", path: config.runNotebook, kernel: { id: "k" } });
    }
    order.push("copy");
    return jsonResponse({});
  });

  const handle = await seedAndStartKernel(config, { fetchImpl, ...silent });

  assert.deepEqual(order, ["read", "copy", "session"]);
  assert.equal(handle.sessionId, "s");
});
