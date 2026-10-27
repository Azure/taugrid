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
const NOTEBOOK = "examples/notebook-embed.ipynb";
const OUT = process.argv[4] || "jupyter-embed-e2e";
// Every run gets its own workspace, session name and notebook path; RUN owns
// all three, so this invocation can only address resources it created.
const RUN = createRunIdentity({ label: "embed-e2e", notebook: NOTEBOOK, server: SERVER, token: TOKEN });
const NOTEBOOK_URL = RUN.notebookUrl;
const RUN_NAME = "notebook-embed-demo";

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

let runHandle = null;

async function main() {
  const playwright = await importPlaywright();
  const context = await launchWorkProfileEdge(playwright, { height: 1000 });
  await seedNotebook(RUN);
  runHandle = await startKernelSession(RUN);
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

  // Require evidence the framed page actually rendered. Matching the product
// name is not evidence: the proxy's own failure banner says "TauGrid portal
// proxy could not reach ...", so /TauGrid/ matched an error page and the run
// passed while nothing had loaded. Require the run name and reject the known
// transport-failure text.
const unreachable = /could not reach|urlopen error|actively refused|connection refused|bad gateway|proxy could not/i;
const rendered = frameText.includes(RUN_NAME);
const ok = !!src && /\/portal\/runs\//.test(src) && rendered && !unreachable.test(frameText);
if (!ok && unreachable.test(frameText)) {
  console.log("the portal proxy could not reach the portal; the iframe rendered an error page, so the embed content is UNTESTED rather than passing");
}
  console.log(`\nembed browser e2e: ${ok ? "PASS" : "FAIL"}`);
  await teardownRun(RUN, runHandle);
  await context.close();
  process.exit(ok ? 0 : 1);
}

// Run only when this file is the process entry point. Importing it (the
// review-pattern checker does, with browser variables cleared) must not launch
// a browser or start a run.
if (isDirectRun(import.meta.url)) {
  main().catch(async (error) => {
    console.error(`embed e2e failed: ${error.message}`);
    await teardownRun(RUN, runHandle).catch(() => {});
    process.exit(1);
  });
}
