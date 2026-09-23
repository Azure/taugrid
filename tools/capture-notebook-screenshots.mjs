// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Capture documentation screenshots of each TauGrid JupyterLab surface.
// Usage: PLAYWRIGHT_PACKAGE_ROOT=<dir> node tools/capture-notebook-screenshots.mjs

import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const SERVER = process.env.TAUGRID_SERVER || "http://127.0.0.1:8888";
const TOKEN = process.env.TAUGRID_TOKEN || "testplugin";
const NAMESPACE = process.env.TAUGRID_NAMESPACE || "tau-notebook-e2e";
const RUN = process.env.TAUGRID_E2E_RUN || "cpu-loss-demo-2";
const OUT = path.resolve("docs/design/assets");

const root = process.env.PLAYWRIGHT_PACKAGE_ROOT;
const playwright = root
  ? createRequire(pathToFileURL(path.join(root, "package.json")))("playwright")
  : await import("playwright");

const context = await playwright.chromium.launchPersistentContext(
  path.join(process.env.TEMP, "tau-shots-" + Date.now()),
  { channel: "msedge", headless: true, viewport: { width: 1500, height: 950 }, deviceScaleFactor: 2 }
);
const page = await context.newPage();
await page.goto(`${SERVER}/lab/tree/examples/notebook-load-job.ipynb?token=${TOKEN}`, {
  waitUntil: "domcontentloaded", timeout: 90_000
});
await page.waitForSelector("#jp-main-dock-panel", { timeout: 90_000, state: "attached" });
await page.waitForTimeout(4_000);

// 1. Runs sidebar
await page.keyboard.press("Control+Shift+C");
const palette = page.locator("input.lm-CommandPalette-input:visible").first();
await palette.waitFor({ timeout: 30_000, state: "visible" });
await palette.fill("TauGrid: Open runs");
await page.waitForTimeout(500);
await page.keyboard.press("Enter");
await page.waitForSelector('[data-testid="taugrid-runs"]', { timeout: 45_000, state: "attached" });
const sidebarInput = page.locator('[data-testid="taugrid-runs"] input').first();
if (!(await sidebarInput.isVisible().catch(() => false))) {
  await page.locator("#jp-left-stack .lm-TabBar-tab", { hasText: /Runs|TauGrid/i }).first().click().catch(() => {});
}
await sidebarInput.waitFor({ timeout: 30_000, state: "visible" });
const sidebar = page.locator('[data-testid="taugrid-runs"]').first();
await sidebar.locator('input[id$="-namespace"]').fill(NAMESPACE);
// The table loads itself and follows the namespace.
await sidebar.locator(".taugrid-runs-table .taugrid-run-link", { hasText: RUN }).first().waitFor({ timeout: 60_000, state: "attached" });
await page.waitForTimeout(800);
await sidebar.screenshot({ path: path.join(OUT, "notebook-runs-sidebar.png") });
console.log("captured runs sidebar");

// 2. Run detail with the loss curve
await sidebar.locator(".taugrid-runs-table .taugrid-run-link", { hasText: RUN }).first().click({ force: true });
const detail = page.locator('[data-testid="taugrid-detail"]').filter({ hasText: RUN }).first();
await detail.waitFor({ timeout: 90_000, state: "attached" });
await page.locator('[data-testid="taugrid-loss-curve"]').first().waitFor({ timeout: 60_000, state: "attached" });
await page.waitForTimeout(1_200);
await detail.screenshot({ path: path.join(OUT, "notebook-run-detail-loss.png") });
console.log("captured run detail with loss curve");

// 3. Logs
await detail.getByRole("button", { name: /open logs/i }).first().click();
const logs = page.locator('[data-testid="taugrid-logs"]').filter({ hasText: RUN }).first();
await logs.waitFor({ timeout: 60_000, state: "attached" });
await page.waitForTimeout(1_000);
await logs.screenshot({ path: path.join(OUT, "notebook-logs.png") });
console.log("captured logs surface");

// 4. Submit review (from the notebook toolbar)
await page.locator("#jp-main-dock-panel .lm-TabBar-tab", { hasText: "notebook-load-job.ipynb" }).first().click().catch(() => {});
const toolbar = page.locator(".jp-NotebookPanel-toolbar .jp-ToolbarButtonComponent, .jp-NotebookPanel-toolbar .jp-Toolbar-item", { hasText: /submit to taugrid/i }).first();
if (await toolbar.count()) {
  await toolbar.click();
} else {
  await page.keyboard.press("Control+Shift+C");
  const commands = page.locator("input.lm-CommandPalette-input:visible").first();
  await commands.waitFor({ timeout: 20_000, state: "visible" });
  await commands.fill("TauGrid: Submit current notebook");
  await page.waitForTimeout(600);
  await page.keyboard.press("Enter");
}
const review = page.locator('[data-testid="taugrid-review"]').first();
await review.waitFor({ timeout: 60_000, state: "attached" });
await review.getByRole("button", { name: /review submission/i }).click();
await page.waitForTimeout(2_000);
await review.screenshot({ path: path.join(OUT, "notebook-submit-review.png") });
console.log("captured submit review");

// 5. Confirmation dialog
await review.getByRole("button", { name: /^submit notebook$/i }).click();
const dialog = page.getByRole("dialog").filter({ hasText: /confirm submission/i });
await dialog.waitFor({ timeout: 30_000, state: "visible" });
await page.waitForTimeout(600);
await dialog.screenshot({ path: path.join(OUT, "notebook-submit-confirm.png") });
console.log("captured confirmation dialog");

await context.close();
console.log("screenshots written to", OUT);
