// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Mocked API tests for tools/notebook-e2e-lifecycle.mjs. No browser, no server:
// they guard the ownership-sensitive parts (identity ordering, unique notebook
// paths, exact-id teardown) that the real harnesses rely on.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import {
  createRunIdentity,
  launchWorkProfileEdge,
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

test("work profile dir keeps override precedence and falls back to tmpdir", async () => {
  const saved = { ...process.env };
  const launched = [];
  const playwright = {
    chromium: {
      launchPersistentContext: async (dir, options) => {
        launched.push({ dir, options });
        return { close: async () => {} };
      },
    },
  };

  try {
    process.env.PLAYWRIGHT_PROFILE_DIR = "/override/profile";
    process.env.LOCALAPPDATA = "/local/appdata";
    await launchWorkProfileEdge(playwright);
    assert.equal(launched.at(-1).dir, "/override/profile");

    delete process.env.PLAYWRIGHT_PROFILE_DIR;
    await launchWorkProfileEdge(playwright);
    assert.equal(launched.at(-1).dir, path.join("/local/appdata", "tau-jupyter-work-profile"));

    delete process.env.LOCALAPPDATA;
    await launchWorkProfileEdge(playwright);
    assert.equal(launched.at(-1).dir, path.join(os.tmpdir(), "tau-jupyter-work-profile"));
  } finally {
    for (const key of Object.keys(process.env)) {
      if (!(key in saved)) delete process.env[key];
    }
    Object.assign(process.env, saved);
  }
});

// A module-load throw is only observable by importing in a fresh process. An
// earlier version derived the Edge profile from LOCALAPPDATA/TEMP at top level,
// so importing threw a TypeError on any host that defines neither -- before the
// offline lifecycle tests, which never launch a browser, could run. The child
// re-runs this file with those variables cleared; the marker keeps it from
// spawning grandchildren and the five mocked-API lifecycle tests must still run
// and pass there.
const CHILD_MARKER = "TAUGRID_LIFECYCLE_IMPORT_CHILD";

test("module imports and the mocked lifecycle passes with browser env cleared",
  { skip: process.env[CHILD_MARKER] === "1" }, () => {
    const env = { ...process.env };
    for (const name of ["PLAYWRIGHT_PROFILE_DIR", "LOCALAPPDATA", "TEMP"]) delete env[name];
    // The outer node:test runner marks its child files with NODE_TEST_CONTEXT;
    // leaving it set makes the nested runner send results over the parent's IPC
    // channel instead of stdout, hiding the counts asserted below.
    delete env.NODE_TEST_CONTEXT;
    env[CHILD_MARKER] = "1";

    const result = spawnSync(process.execPath, [
      "--test",
      "--test-reporter=spec",
      fileURLToPath(new URL("./notebook-e2e-lifecycle.test.mjs", import.meta.url)),
    ], { env, encoding: "utf8" });

    assert.equal(result.status, 0, `child import/lifecycle run failed\n${result.stdout}\n${result.stderr}`);
    // The identity, profile-dir and five mocked-API lifecycle tests pass; only
    // the child-import test above is skipped.
    assert.match(result.stdout, /pass 7\b/);
    assert.match(result.stdout, /fail 0\b/);
    assert.match(result.stdout, /skipped 1\b/);
  });
