// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Browser E2E for the TauGrid JupyterLab extension (UX-first surfaces).
//
// Journeys asserted, in order:
//   1. open the runs browser in the LEFT sidebar from the command palette,
//   2. exact lookup by kind/namespace/name opens a native main-area DETAIL tab,
//   3. the detail opens a separate LOGS tab,
//   4. the notebook toolbar submit flow runs review -> confirm -> submitted.
//
// Anchors come from docs/design/notebook-plugin.md ("Parent-owned e2e anchors").
// Set KEEP_OPEN=1 to leave Edge open.

import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const SERVER = process.argv[2] || "http://127.0.0.1:8888";
const TOKEN = process.argv[3] || "testplugin";
const OUT = process.argv[4] || "jupyter-labextension-e2e";
const BASE = SERVER.replace(/\/$/, "");
const LAB_URL = `${BASE}/lab/tree/examples/notebook-load-job.ipynb?token=${TOKEN}`;
const NAMESPACE = "tau-notebook-e2e";
const RUN_NAME = process.env.TAUGRID_E2E_RUN || "notebook-embed-demo";

async function importPlaywright() {
  const root = process.env.PLAYWRIGHT_PACKAGE_ROOT || "";
  if (root) {
    return createRequire(pathToFileURL(path.join(root, "package.json")))("playwright");
  }
  return await import("playwright");
}

const PROFILE_DIR =
  process.env.PLAYWRIGHT_PROFILE_DIR ||
  path.join(process.env.LOCALAPPDATA || process.env.TEMP, "tau-jupyter-work-profile");

async function main() {
  const playwright = await importPlaywright();
  const context = await playwright.chromium.launchPersistentContext(PROFILE_DIR, {
    channel: "msedge",
    headless: process.env.HEADLESS === "1",
    viewport: { width: 1500, height: 950 },
    args: ["--disable-blink-features=AutomationControlled", "--no-proxy-server"]
  });
  const page = await context.newPage();
  const notes = [];
  page.on("pageerror", e => notes.push("[pageerror] " + e.message.slice(0, 160)));
  page.on("console", m => { if (m.type() === "error") { notes.push("[console] " + m.text().slice(0, 160)); } });

  const steps = {};
  const check = async (name, fn) => {
    try {
      steps[name] = await fn();
    } catch (error) {
      steps[name] = false;
      console.log(`step failed [${name}]: ${error.message.split("\n")[0]}`);
      await page.screenshot({ path: `${OUT}-${name}-failure.png`, fullPage: true }).catch(() => {});
    }
  };

  // The extension may still be activating right after load, so retry the palette
  // until the command is actually registered instead of racing activation.
  const runPaletteCommand = async label => {
    for (let attempt = 0; attempt < 20; attempt += 1) {
      await page.keyboard.press("Control+Shift+C");
      const palette = page.locator("input.lm-CommandPalette-input:visible").first();
      await palette.waitFor({ timeout: 15_000, state: "visible" });
      await palette.fill(label);
      await page.waitForTimeout(400);
      const items = await page.locator(".lm-CommandPalette").innerText().catch(() => "");
      if (attempt === 0) {
        console.log("palette items:", items.replace(/\s+/g, " ").slice(0, 160));
      }
      if (items.includes(label)) {
        await page.keyboard.press("Enter");
        return true;
      }
      await page.keyboard.press("Escape");
      await page.waitForTimeout(500);
    }
    return false;
  };

  await page.goto(LAB_URL, { waitUntil: "domcontentloaded", timeout: 90_000 });
  await page.waitForSelector("#jp-main-dock-panel, .jp-LabShell", { timeout: 90_000, state: "attached" });
  await page.waitForTimeout(3_000);
  console.log("jupyterlab loaded");

  // 1. Runs browser in the left sidebar.
  await check("openRuns", async () => {
    // Filter to the exact command; "TauGrid" alone puts About first.
    await runPaletteCommand("TauGrid: Open runs");
    await page.waitForSelector('[data-testid="taugrid-runs"]', { timeout: 45_000, state: "attached" });
    // Expand the left sidebar only if it is collapsed: clicking an already-active
    // tab toggles the sidebar shut.
    const input = page.locator('[data-testid="taugrid-runs"] input').first();
    if (!(await input.isVisible().catch(() => false))) {
      const tab = page.locator("#jp-left-stack .lm-TabBar-tab", { hasText: /Runs|TauGrid/i }).first();
      if (await tab.count()) { await tab.click().catch(() => {}); }
    }
    await page.waitForSelector('[data-testid="taugrid-runs"] input', { timeout: 30_000, state: "visible" });
    const inSidebar = await page.locator("#jp-left-stack [data-testid='taugrid-runs']").count();
    console.log("sidebar html:", (await page.locator('[data-testid="taugrid-runs"]').innerHTML()).replace(/\s+/g, " ").slice(0, 1200));
    return inSidebar > 0;
  });

  // 2. Exact lookup opens a detail tab.
  await check("openDetail", async () => {
    const sidebar = page.locator('[data-testid="taugrid-runs"]');
    await sidebar.locator('input[id$="-namespace"]').fill(NAMESPACE);
    // The table loads itself and follows the namespace; just wait for the row.
    const row = sidebar.locator(".taugrid-runs-table .taugrid-run-link", { hasText: RUN_NAME }).first();
    await row.waitFor({ timeout: 60_000, state: "attached" });
    await row.click({ force: true });
    await page.waitForSelector('[data-testid="taugrid-detail"]', { timeout: 90_000, state: "attached" });
    const detail = page.locator('[data-testid="taugrid-detail"]').filter({ hasText: RUN_NAME }).first();
    const text = (await detail.innerText()).replace(/\s+/g, " ");
    console.log("detail:", text.slice(0, 300));
    return text.includes(RUN_NAME);
  });

  // 2b. When the run reported metrics, the loss curve must actually plot them.
  await check("lossCurve", async () => {
    const curve = page.locator('[data-testid="taugrid-loss-curve"]').first();
    await curve.waitFor({ timeout: 60_000, state: "attached" });
    const points = await curve.locator("circle").count();
    const line = await curve.locator("polyline").count();
    const samples = await page.locator('[data-testid="taugrid-loss-samples"]').first().innerText().catch(() => "");
    console.log("loss curve points:", points, "polyline:", line, "samples table:", samples.replace(/\s+/g, " ").slice(0, 80));
    return points >= 1 && line >= 1;
  });

  // 3. Logs open in their own tab.
  await check("openLogs", async () => {
    await page.getByRole("button", { name: /open logs/i }).first().click();
    await page.waitForSelector('[data-testid="taugrid-logs"]', { timeout: 60_000, state: "attached" });
    const logs = page.locator('[data-testid="taugrid-logs"]').filter({ hasText: RUN_NAME }).first();
    const text = (await logs.innerText()).replace(/\s+/g, " ");
    console.log("logs surface:", text.slice(0, 200));
    return true;
  });

  // 4. Notebook-scoped submit: toolbar -> review -> confirm -> submitted.
  await check("submit", async () => {
    const unique = `e2e-submit-${Date.now().toString(36)}`;
    // Activate the notebook first: the toolbar item and the command are notebook-scoped.
    await page.locator("#jp-main-dock-panel .lm-TabBar-tab", { hasText: "notebook-load-job.ipynb" }).first().click().catch(() => {});
    // Several notebooks may be open; scope to the toolbar item that carries the label.
    const toolbar = page.locator(".jp-NotebookPanel-toolbar .jp-ToolbarButtonComponent, .jp-NotebookPanel-toolbar .jp-Toolbar-item", { hasText: /^submit notebook$/i }).first();
    if (await toolbar.count()) {
      console.log("toolbar submit item present");
      await toolbar.click();
    } else {
      console.log("toolbar item not matched; using the palette command");
      await runPaletteCommand("TauGrid: Submit current notebook");
    }
    await page.waitForSelector('[data-testid="taugrid-review"]', { timeout: 60_000, state: "attached" });
    const review = page.locator('[data-testid="taugrid-review"]');
    const nsBox = review.getByTestId('taugrid-submit-namespace');
    await nsBox.selectOption(NAMESPACE);
    const profileBox = review.getByTestId('taugrid-submit-profile');
    await profileBox.locator('option[value]:not([value=""])').first().waitFor({ state: 'attached' });
    const profiles = await profileBox.locator('option').evaluateAll(options => options.filter(option => option.value && /, 0 GPUs each$/.test(option.textContent || '')).map(option => option.value));
    const submitProfile = process.env.SUBMIT_PROFILE || (profiles.includes('cpu') ? 'cpu' : profiles[0]);
    if (!submitProfile) throw new Error('No CPU profile is visible. Set SUBMIT_PROFILE explicitly for another workload.');
    await profileBox.selectOption(submitProfile);
    const queueBox = review.getByTestId('taugrid-submit-queue');
    if (process.env.SUBMIT_QUEUE) await queueBox.selectOption(process.env.SUBMIT_QUEUE);
    if (!await queueBox.inputValue()) throw new Error('No visible default queue. Set SUBMIT_QUEUE to an available LocalQueue.');
    const nameBox = review.locator('input[id$="-name"]');
    if (await nameBox.count()) { await nameBox.first().fill(unique); }
    await review.getByRole("button", { name: /review submission/i }).click();
    await review.getByTestId('taugrid-submit-receipt').waitFor({ state: 'visible' });
    await review.getByRole("button", { name: /^submit notebook$/i }).click();
    const dialog = page.getByRole("dialog").filter({ hasText: /Submit the reviewed notebook snapshot/i });
    await dialog.waitFor({ timeout: 30_000, state: "visible" });
    await dialog.getByRole("button", { name: /^submit notebook$/i }).click();
    const receipt = review.getByTestId('taugrid-submitted');
    await receipt.waitFor({ state: 'visible', timeout: 120_000 });
    const submitted = await receipt.innerText();
    console.log('submit outcome:', submitted);
    await receipt.getByRole('button', { name: 'Open submitted run', exact: true }).click();
    await page.locator('[data-testid="taugrid-detail"]').filter({ hasText: unique }).waitFor({ state: 'visible' });
    return submitted.includes('Notebook submitted') && submitted.includes(`${NAMESPACE}/${unique}`);
  });

  await page.screenshot({ path: `${OUT}.png`, fullPage: true });
  await page.screenshot({ path: `${OUT}-viewport.png`, fullPage: false });

  const ok = Boolean(steps.openRuns && steps.openDetail && steps.lossCurve && steps.openLogs && steps.submit);
  console.log("steps:", JSON.stringify(steps));
  if (!ok) {
    console.log("page notes (last 12):");
    for (const line of notes.slice(-12)) console.log("  " + line);
  }
  console.log(`\nlabextension browser e2e: ${ok ? "PASS" : "FAIL"}`);
  if (process.env.KEEP_OPEN === "1") {
    console.log("KEEP_OPEN=1: browser left open; kill this job to close it.");
    // Keep the browser process alive without an unsettled top-level await.
    setInterval(() => {}, 60_000);
    return;
  }
  await context.close();
  process.exit(ok ? 0 : 1);
}

main().catch(async error => {
  console.error(`labextension e2e failed: ${error.message}`);
  process.exit(1);
});