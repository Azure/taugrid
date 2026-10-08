// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Open the TauGrid plugin UI in a visible Edge window and leave it open.
// Usage: PLAYWRIGHT_PACKAGE_ROOT=<dir> node tools/show-notebook-ui.mjs

import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const SERVER = process.env.TAUGRID_SERVER || "http://127.0.0.1:8888";
const TOKEN = process.env.TAUGRID_TOKEN || "testplugin";
const NAMESPACE = process.env.TAUGRID_NAMESPACE || "tau-notebook-e2e";
const RUN = process.env.TAUGRID_E2E_RUN || "cpu-loss-demo-2";

const root = process.env.PLAYWRIGHT_PACKAGE_ROOT;
const playwright = root
  ? createRequire(pathToFileURL(path.join(root, "package.json")))("playwright")
  : await import("playwright");

const context = await playwright.chromium.launchPersistentContext(
  path.join(process.env.TEMP, "tau-show-" + Date.now()),
  { channel: "msedge", headless: false, viewport: { width: 1500, height: 950 } }
);
const page = await context.newPage();
await page.goto(`${SERVER}/lab/tree/examples/notebook-load-job.ipynb?token=${TOKEN}`, {
  waitUntil: "domcontentloaded", timeout: 90_000
});
await page.waitForSelector("#jp-main-dock-panel", { timeout: 90_000, state: "attached" });
await page.waitForTimeout(4_000);

// Open the runs sidebar from the palette. The extension may still be activating,
// and the notebook itself is what matters, so failures here are not fatal.
const openRuns = async () => {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    await page.keyboard.press("Control+Shift+C");
    const commands = page.locator("input.lm-CommandPalette-input:visible").first();
    await commands.waitFor({ timeout: 15_000, state: "visible" });
    await commands.fill("TauGrid: Open runs");
    await page.waitForTimeout(400);
    const items = await page.locator(".lm-CommandPalette").innerText().catch(() => "");
    if (items.includes("TauGrid: Open runs")) {
      await page.keyboard.press("Enter");
      return true;
    }
    await page.keyboard.press("Escape");
    await page.waitForTimeout(500);
  }
  return false;
};

try {
  if (!(await openRuns())) throw new Error("TauGrid commands did not appear");
  await page.waitForSelector('[data-testid="taugrid-runs"]', { timeout: 45_000, state: "attached" });
} catch (error) {
  console.log("plugin panel not driven:", error.message, "- leaving the notebook open");
  console.log("notebook open:", await page.title());
  // Keep the browser process alive without an unsettled top-level await.
setInterval(() => {}, 60_000);
}

const sidebar = page.locator('[data-testid="taugrid-runs"]').first();
const input = sidebar.locator('input[id$="-namespace"]');
if (!(await input.isVisible().catch(() => false))) {
  await page.locator("#jp-left-stack .lm-TabBar-tab", { hasText: /Runs|TauGrid/i }).first().click().catch(() => {});
}
await input.waitFor({ timeout: 30_000, state: "visible" });
await input.fill(NAMESPACE);

// Open the run so the detail tab and its loss curve are on screen.
const row = sidebar.locator(".taugrid-runs-table .taugrid-run-link", { hasText: RUN }).first();
await row.waitFor({ timeout: 90_000, state: "attached" });
await row.click({ force: true });
const detail = page.locator('[data-testid="taugrid-detail"]').filter({ hasText: RUN }).first();
await detail.waitFor({ timeout: 90_000, state: "attached" });
await page.locator('[data-testid="taugrid-loss-curve"]').first().waitFor({ timeout: 60_000, state: "attached" });
await page.waitForTimeout(2_000);
console.log("TauGrid UI is on screen:", await page.title());
// Keep the browser process alive without an unsettled top-level await.
setInterval(() => {}, 60_000);