// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Browser E2E for the TensorBoard-style TauGrid embed.
//
// Opens examples/notebook-embed.ipynb in the live Jupyter server, runs the
// cells through the UI, and verifies the cell output contains an iframe that
// loads the TauGrid portal run view (served through the kernel-local proxy).
//
// Usage:
//   PLAYWRIGHT_PACKAGE_ROOT=<dir> node run-embed-e2e.mjs [server] [token] [out]

import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const SERVER = process.argv[2] || "http://127.0.0.1:8888";
const TOKEN = process.argv[3] || "testplugin";
const NOTEBOOK = "examples/notebook-embed.ipynb";
const OUT = process.argv[4] || "jupyter-embed-e2e";
const NOTEBOOK_URL = `${SERVER.replace(/\/$/, "")}/lab/tree/${NOTEBOOK}?token=${TOKEN}`;
const API = `${SERVER.replace(/\/$/, "")}`;
const RUN_NAME = "notebook-embed-demo";

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

const PROFILE_DIR =
  process.env.PLAYWRIGHT_PROFILE_DIR ||
  path.join(process.env.LOCALAPPDATA || process.env.TEMP, "tau-jupyter-work-profile");

async function launchWorkProfileEdge(playwright) {
  return await playwright.chromium.launchPersistentContext(PROFILE_DIR, {
    channel: "msedge",
    headless: process.env.HEADLESS === "1",
    viewport: { width: 1440, height: 1000 },
    args: ["--disable-blink-features=AutomationControlled", "--no-proxy-server"],
  });
}

async function startKernelSession() {
  const sessions = await (await fetch(`${API}/api/sessions?token=${TOKEN}`)).json();
  for (const session of sessions) {
    await fetch(`${API}/api/sessions/${session.id}?token=${TOKEN}`, { method: "DELETE" });
  }
  const kernels = await (await fetch(`${API}/api/kernels?token=${TOKEN}`)).json();
  for (const kernel of kernels) {
    await fetch(`${API}/api/kernels/${kernel.id}?token=${TOKEN}`, { method: "DELETE" });
  }
  await fetch(`${API}/api/workspaces/default?token=${TOKEN}`, { method: "DELETE" });
  const response = await fetch(`${API}/api/sessions?token=${TOKEN}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ kernel: { name: "python3" }, name: "tau-embed-e2e", path: NOTEBOOK, type: "notebook" }),
  });
  if (!response.ok) throw new Error(`kernel session failed: ${response.status}`);
  const session = await response.json();
  console.log(`kernel session: ${session.id} (${session.kernel?.name})`);
}

async function runAllCells(page, cellCount = 3) {
  await page.keyboard.press("Control+Shift+C");
  const palette = page.getByRole("textbox", { name: /command|search/i }).first();
  await palette.waitFor({ timeout: 15_000 }).catch(() => {});
  await palette.fill("select kernel").catch(() => {});
  await page.waitForTimeout(800);
  await page.keyboard.press("Enter");
  await page.waitForTimeout(1_500);
  await page.getByText("python3", { exact: false }).first().click().catch(() => {});
  await page
    .waitForFunction(
      () => {
        const text = document.body.innerText || "";
        return !/No Kernel/.test(text) && !/Kernel Connecting/.test(text);
      },
      null,
      { timeout: 60_000 },
    )
    .catch(() => {});
  for (let i = 0; i < cellCount; i += 1) {
    await page.keyboard.press("Escape");
    await page.keyboard.press("Shift+Enter");
    await page.waitForTimeout(3_000);
  }
}

async function main() {
  const playwright = await importPlaywright();
  const context = await launchWorkProfileEdge(playwright);
  await startKernelSession();
  const page = await context.newPage();
  await page.goto(NOTEBOOK_URL, { waitUntil: "domcontentloaded", timeout: 60_000 });
  await page.waitForSelector(".jp-Notebook", { timeout: 60_000, state: "attached" });
  console.log("notebook open; running cells via the UI");
  await runAllCells(page);

  const iframeSel = 'iframe[src*="/portal/runs/"]';
  await page.waitForSelector(iframeSel, { timeout: 120_000 });
  const src = await page.getAttribute(iframeSel, "src");
  console.log(`iframe src: ${src}`);

  // The framed portal is a real document; wait for it to render the run board.
  const frame = page.frameLocator(iframeSel);
  let frameText = "";
  const deadline = Date.now() + 90_000;
  while (Date.now() < deadline) {
    frameText = await frame.locator("body").innerText().catch(() => "");
    if (/TauGrid/i.test(frameText) || frameText.includes(RUN_NAME)) break;
    await page.waitForTimeout(1_000);
  }
  console.log(`frame text: ${frameText.replace(/\s+/g, " ").slice(0, 400)}`);

  await page.screenshot({ path: `${OUT}.png`, fullPage: true });
  await page.screenshot({ path: `${OUT}-viewport.png`, fullPage: false });

  const ok = !!src && /\/portal\/runs\//.test(src) && (/TauGrid/i.test(frameText) || frameText.includes(RUN_NAME));
  console.log(`\nembed browser e2e: ${ok ? "PASS" : "FAIL"}`);
  await context.close();
  process.exit(ok ? 0 : 1);
}

main().catch(async (error) => {
  console.error(`embed e2e failed: ${error.message}`);
  process.exit(1);
});
