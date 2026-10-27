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

import {
  createRunIdentity,
  importPlaywright,
  isDirectRun,
  launchWorkProfileEdge,
  seedNotebook,
  startKernelSession,
  teardownRun,
} from "./notebook-e2e-lifecycle.mjs";

const SERVER = process.argv[2] || "http://127.0.0.1:8888";
const TOKEN = process.argv[3] || "testplugin";
const NOTEBOOK = "examples/notebook-button-e2e.ipynb";
const OUT = process.argv[4] || "jupyter-button-e2e";
// Every run gets its own workspace, session name and notebook path; RUN owns
// all three, so this invocation can only address resources it created.
const RUN = createRunIdentity({ label: "button-e2e", notebook: NOTEBOOK, server: SERVER, token: TOKEN });
const NOTEBOOK_URL = RUN.notebookUrl;
const RUN_MARKER = "submitted-button-demo";

async function openNotebook(context) {
  const page = await context.newPage();
  await page.goto(NOTEBOOK_URL, { waitUntil: "domcontentloaded", timeout: 60_000 });
  // The windowed-panel wrapper also matches .jp-Notebook and may never be
  // "visible"; attachment is what matters (the document is mounted).
  await page.waitForSelector(".jp-Notebook", { timeout: 60_000, state: "attached" });
  return page;
}

let runHandle = null;

async function main() {
  const playwright = await importPlaywright();
  const context = await launchWorkProfileEdge(playwright);
  const consoleLog = [];
  context.on("page", (page) => {
    page.on("pageerror", (err) => consoleLog.push(`[pageerror] ${err.message.slice(0, 160)}`));
  });
  await seedNotebook(RUN);
  runHandle = await startKernelSession(RUN);
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

  // "unknown" means the panel has no status at all, so it must not satisfy the
  // assertion: a repaint alone does not prove the submit path produced a run state.
  const pass =
    !before &&
    statusText.includes(RUN_MARKER) &&
    statusText.includes("queued") &&
    !statusText.includes("unknown") &&
    markerText.includes("notebook-button-e2e.ipynb") &&
    !markerText.includes("not submitted");
  console.log(`\nplugin button e2e: ${pass ? "PASS" : "FAIL"}`);
  await teardownRun(RUN, runHandle);
  await context.close();
  process.exit(pass ? 0 : 1);
}

// Run only when this file is the process entry point. Importing it (the
// review-pattern checker does, with browser variables cleared) must not launch
// a browser or start a run.
if (isDirectRun(import.meta.url)) {
  main().catch(async (error) => {
    console.error(`button e2e failed: ${error.message}`);
    await teardownRun(RUN, runHandle).catch(() => {});
    process.exit(1);
  });
}