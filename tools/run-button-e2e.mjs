// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Browser E2E for the notebook plugin's interactive Submit button.
//
// Opens examples/notebook-button-e2e.ipynb in the live Jupyter server, clicks
// the real "Submit notebook" button, and asserts:
//   1. the status line repaints into run view ("submitted-button-demo") — the
//      click drove the same handler on_click registers;
//   2. the marker cell's output contains the notebook path the fake submit
//      recorded — the chain received the notebook the button represents.
//
// Usage:
//   PLAYWRIGHT_PACKAGE_ROOT=<dir> node run-button-e2e.mjs [server] [token] [out]

import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const SERVER = process.argv[2] || "http://127.0.0.1:8888";
const TOKEN = process.argv[3] || "testplugin";
const NOTEBOOK = "examples/notebook-button-e2e.ipynb";
const OUT = process.argv[4] || "jupyter-button-e2e";
const NOTEBOOK_URL = `${SERVER.replace(/\/$/, "")}/lab/tree/${NOTEBOOK}?token=${TOKEN}`;
const API = `${SERVER.replace(/\/$/, "")}`;
const RUN_MARKER = "submitted-button-demo";

async function importPlaywright() {
  const packageRoot = process.env.PLAYWRIGHT_PACKAGE_ROOT || "";
  if (packageRoot) {
    const requireFromPackageRoot = createRequire(
      pathToFileURL(path.join(packageRoot, "package.json")),
    );
    return requireFromPackageRoot("playwright");
  }
  return await import("playwright");
}

// Work profile: persistent Microsoft Edge context (its own user-data dir that
// keeps cookies across runs). Local server, so bypass proxying entirely.
const PROFILE_DIR =
  process.env.PLAYWRIGHT_PROFILE_DIR ||
  path.join(process.env.LOCALAPPDATA || process.env.TEMP, "tau-jupyter-work-profile");

async function launchWorkProfileEdge(playwright) {
  return await playwright.chromium.launchPersistentContext(PROFILE_DIR, {
    channel: "msedge",
    headless: process.env.HEADLESS === "1",
    viewport: { width: 1440, height: 900 },
    args: [
      "--disable-blink-features=AutomationControlled",
      "--no-proxy-server",
    ],
  });
}

async function startKernelSession() {
  // Delete stale sessions/kernels first: prior runs leave records whose
  // kernels were shut down, and the page then adopts a DEAD kernel.
  const sessions = await (await fetch(`${API}/api/sessions?token=${TOKEN}`)).json();
  for (const session of sessions) {
    await fetch(`${API}/api/sessions/${session.id}?token=${TOKEN}`, { method: "DELETE" });
    console.log(`stale session ${session.id}: deleted`);
  }
  const kernels = await (await fetch(`${API}/api/kernels?token=${TOKEN}`)).json();
  for (const kernel of kernels) {
    await fetch(`${API}/api/kernels/${kernel.id}?token=${TOKEN}`, { method: "DELETE" });
    console.log(`stale kernel ${kernel.id}: deleted`);
  }
  // JupyterLab saves executed outputs in its workspace blob and replays them on
  // load, which would pre-satisfy this run's click assertions. Clear it.
  const ws = await fetch(`${API}/api/workspaces/default?token=${TOKEN}`, { method: "DELETE" });
  console.log(`workspace blob: ${ws.ok ? "cleared" : `clear failed ${ws.status}`}`);
  const response = await fetch(`${API}/api/sessions?token=${TOKEN}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ kernel: { name: "python3" }, name: "tau-button-e2e", path: NOTEBOOK, type: "notebook" }),
  });
  if (!response.ok) throw new Error(`kernel session failed: ${response.status}`);
  const session = await response.json();
  console.log(`kernel session: ${session.id} (${session.kernel?.name})`);
}

async function openNotebook(context) {
  const page = await context.newPage();
  await page.goto(NOTEBOOK_URL, { waitUntil: "domcontentloaded", timeout: 60_000 });
  // The windowed-panel wrapper also matches .jp-Notebook and may never be
  // "visible"; attachment is what matters (the document is mounted).
  await page.waitForSelector(".jp-Notebook", { timeout: 60_000, state: "attached" });
  return page;
}

async function main() {
  const playwright = await importPlaywright();
  const context = await launchWorkProfileEdge(playwright);
  const consoleLog = [];
  context.on("page", (page) => {
    page.on("pageerror", (err) => consoleLog.push(`[pageerror] ${err.message.slice(0, 160)}`));
  });
  await startKernelSession();
  const page = await openNotebook(context);

  // Execute the setup cells first: the button only exists after the display
  // cell runs (an opened notebook loads unexecuted). The windowed list renders
  // invisible placeholders for off-screen cells, so click inside the real
  // notebook area and drive selection by keyboard from command mode.
  const cells = page.locator(".jp-Notebook .jp-Cell");
  console.log(`cells: ${await cells.count()}`);
  await page.locator(".jp-Notebook").last().click({ position: { x: 240, y: 240 } });
  await page.keyboard.press("Escape");   // command mode, cell 1 selected
  await page.keyboard.press("Shift+Enter");   // run cell 1, advance
  await page.waitForTimeout(2_000);
  await page.keyboard.press("Shift+Enter");   // run display cell, advance
  await page.waitForTimeout(3_000);

  // The panel's interactive tree carries the real button.
  const submit = page.getByRole("button", { name: /Submit notebook/i }).first();
  try {
    await submit.waitFor({ timeout: 60_000, state: "attached" });
  } catch (error) {
    const state = await page.evaluate(() => ({
      text: (document.body.innerText || "").replace(/\s+/g, " ").slice(0, 1500),
      outputs: [...document.querySelectorAll(".jp-Cell-outputArea, .jp-OutputArea")]
        .map((n) => (n.textContent || "").replace(/\s+/g, " ").trim())
        .filter(Boolean),
      buttons: [...document.querySelectorAll("button")].map((b) => b.textContent.trim()).filter(Boolean),
    }));
    await page.screenshot({ path: `${OUT}-state.png`, fullPage: true });
    console.log("outputs:", JSON.stringify(state.outputs));
    console.log("buttons on page:", JSON.stringify(state.buttons));
    console.log("page text:", state.text);
    throw error;
  }
  console.log(`button found: "${await submit.getAttribute("aria-label") ?? "Submit notebook"}"`);
  const before = await page.evaluate(() => {
    const el = [...document.querySelectorAll(".tg-status")].find(Boolean);
    return el ? el.textContent.includes("submitted-button-demo") : false;
  });

  // Click the real button.
  await submit.click();
  await page.waitForFunction(
    (marker) => document.body.innerText.includes(marker),
    RUN_MARKER,
    { timeout: 30_000 },
  );
  console.log("click registered: status repainted into run view");
  const statusText = await page.evaluate(() => {
    const el = [...document.querySelectorAll(".tg-status")]
      .find((n) => n.textContent.includes("submitted-button-demo"));
    return el ? el.textContent.trim() : "";
  });

  // Run the marker cell: the fake submit's recording must carry the notebook
  // the button represents. Target the cell by its content (placeholders and
  // selection state make a positional Shift+Enter ambiguous).
  const markerCell = page.locator(".jp-CodeCell").filter({ hasText: "recorded" }).last();
  await markerCell.click();
  await page.keyboard.press("Escape");
  await page.keyboard.press("Shift+Enter");
  await page.waitForFunction(
    () => (document.body.innerText || "").includes("notebook-button-e2e.ipynb"),
    null,
    { timeout: 30_000 },
  );
  const markerText = await page.evaluate(() => {
    const areas = [...document.querySelectorAll(".jp-Cell-outputArea")]
      .map((n) => (n.textContent || "").replace(/\s+/g, " ").trim());
    return areas.find((t) => t.includes("notebook-button-e2e")) || "";
  });

  await page.screenshot({ path: `${OUT}.png`, fullPage: true });
  await page.screenshot({ path: `${OUT}-viewport.png` });

  console.log(`before click, status had marker: ${before} (expected false)`);
  console.log(`status after click: ${statusText}`);
  console.log(`recorded chain: ${markerText}`);
  console.log(`screenshots: ${OUT}.png, ${OUT}-viewport.png`);

  const pass =
    !before &&
    statusText.includes(RUN_MARKER) &&
    markerText.includes("notebook-button-e2e.ipynb") &&
    !markerText.includes("not submitted");
  console.log(`\nplugin button e2e: ${pass ? "PASS" : "FAIL"}`);
  await context.close();
  process.exit(pass ? 0 : 1);
}

main().catch(async (error) => {
  console.error(`button e2e failed: ${error.message}`);
  process.exit(1);
});