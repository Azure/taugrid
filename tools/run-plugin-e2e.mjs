// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Browser E2E: load the TauGrid plugin in the running Jupyter Notebook server,
// run the plugin notebook through the UI, and verify the rendered panel in the
// browser DOM (the real frontend path: kernel -> widget manager -> cell output).
//
// Prereq: a Jupyter server at SERVER (default http://127.0.0.1:8888, token
// testplugin) serving sdk/python/python/examples/notebook-panel.ipynb.
//
// Playwright is imported the same way the repo's capture script does:
// a throwaway npm install rooted at PLAYWRIGHT_PACKAGE_ROOT, else bare import.
//
// Usage:
//   PLAYWRIGHT_PACKAGE_ROOT=<dir> node run-plugin-e2e.mjs
//   node run-plugin-e2e.mjs http://127.0.0.1:8888 testplugin

import {
  createRunIdentity,
  importPlaywright,
  launchWorkProfileEdge,
  seedNotebook,
  startKernelSession,
  teardownRun,
} from "./notebook-e2e-lifecycle.mjs";

const SERVER = process.argv[2] || "http://127.0.0.1:8888";
const TOKEN = process.argv[3] || "testplugin";
const NOTEBOOK = "examples/notebook-panel.ipynb";
const OUT = process.argv[4] || "jupyter-plugin-e2e";
// Every run gets its own workspace, session name and notebook path; RUN owns
// all three, so this invocation can only address resources it created.
const RUN = createRunIdentity({ label: "plugin-e2e", notebook: NOTEBOOK, server: SERVER, token: TOKEN });
// Path first, then the token query. Building a BASE with the query and
// appending the notebook path buries the path inside the query string.
// Prefer JupyterLab (/lab/workspaces/<name>/tree/...) over Notebook 7
// (/notebooks/...): the same server, but Lab attaches a pre-created session and
// exposes the kernel picker more reliably through automation.
const NOTEBOOK_URL = RUN.notebookUrl;

const BACKOFF_MS = [250, 500, 1000, 2000, 4000];

async function openNotebook(browser, base, notebook) {
  for (const delay of BACKOFF_MS) {
    const page = await browser.newPage();
    try {
      await page.goto(NOTEBOOK_URL, { waitUntil: "domcontentloaded", timeout: 60_000 });
      // Notebook 7 (JupyterLab-based) mounts the document as .jp-Notebook;
      // classic served .notebook_app. Wait generously: a fresh work profile has
      // an empty HTTP cache, so the first SPA load is slow.
      await page.waitForSelector(".jp-Notebook, .notebook_app, .jp-MainAreaWidget", {
        timeout: 60_000,
        state: "attached",
      });
      await page.waitForSelector(".jp-Notebook", { timeout: 30_000, state: "visible" }).catch(() => {});
      return page;
    } catch (error) {
      await page.screenshot({ path: `${OUT}-open-failure.png`, fullPage: true }).catch(() => {});
      await page.close();
      if (delay === BACKOFF_MS[BACKOFF_MS.length - 1]) throw error;
      console.log(`openNotebook retry in ${delay}ms: ${error.message.split("\n")[0]}`);
      await new Promise((resolve) => setTimeout(resolve, delay));
    }
  }
  throw new Error("unreachable");
}

async function runAllCells(page, cellCount = 4) {
  // The page text must show a running kernel: with "No Kernel" the Shift+Enter
  // presses are silently dropped and no cell ever runs. Notebook 7's
  // Notebook 7 shows "No Kernel" until a kernel is SELECTED; its Restart item
  // is disabled before that. The command palette (Ctrl+Shift+C) is the
  // deterministic in-UI path: run the "Select Kernel" command, pick python3.
  await page.keyboard.press("Control+Shift+C");
  const palette = page.getByRole("textbox", { name: /command|search/i }).first();
  await palette.waitFor({ timeout: 15_000 }).catch(() => {});
  await palette.fill("select kernel").catch(() => {});
  await page.waitForTimeout(800);
  await page.keyboard.press("Enter");
  await page.waitForTimeout(1_500);
  // The kernel picker lists available kernels; python3 is the only one here.
  const kernelRow = page.getByText("python3", { exact: false }).first();
  await kernelRow.click().catch(() => {});
  // Wait until the toolbar stops saying "No Kernel"/"Connecting".
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

  // Run the document: Shift+Enter advances through the cells now that a kernel
  // is selected and attached.
  for (let i = 0; i < cellCount; i += 1) {
    await page.keyboard.press("Escape");
    await page.keyboard.press("Shift+Enter");
    await page.waitForTimeout(2_000);
  }
}

let runHandle = null;

async function main() {
  const playwright = await importPlaywright();
  const context = await launchWorkProfileEdge(playwright);
  const consoleLog = [];
  context.on("page", (page) => {
    page.on("console", (msg) => consoleLog.push(`[${msg.type()}] ${msg.text().slice(0, 200)}`));
    page.on("pageerror", (err) => consoleLog.push(`[pageerror] ${err.message.slice(0, 200)}`));
    page.on("requestfailed", (req) =>
      consoleLog.push(`[requestfailed] ${req.url().slice(0, 120)} ${req.failure()?.errorText}`),
    );
  });
  await seedNotebook(RUN);
  runHandle = await startKernelSession(RUN);
  const page = await openNotebook(context, NOTEBOOK_URL);

  // Wait for the kernel to become ready before running cells.
  await page.waitForFunction(
    () => !document.querySelector("[data-jp-kernel-status] .jp-mod-warn"),
    null,
    { timeout: 30_000 },
  ).catch(() => {});

  console.log("notebook open; running all cells via the UI");
  await runAllCells(page);

  // Wait for the panel output to render: the plugin's hero signature plus the
  // loss-down values it states in words and numbers.
  try {
    await page.waitForFunction(
      () => document.body.innerText.includes("loss down"),
      null,
      { timeout: 120_000 },
    );
  } catch (error) {
    // Capture the true page state on timeout: kernel status, rendered text,
    // and any error output. This is what makes the failure diagnosable.
    const state = await page.evaluate(() => {
      const text = (document.body.innerText || "").replace(/\s+/g, " ").slice(0, 1200);
      const outputs = [...document.querySelectorAll(".jp-Cell-outputArea")]
        .map((n) => (n.textContent || "").replace(/\s+/g, " ").trim())
        .filter(Boolean);
      const kernel = [...document.querySelectorAll("[data-jp-kernel-status], .jp-KernelStatus")]
        .map((n) => (n.textContent || "").replace(/\s+/g, " ").trim())
        .filter(Boolean);
      return { text, outputs, kernel };
    });
    await page.screenshot({ path: `${OUT}-state.png`, fullPage: true });
    console.log("kernel status:", JSON.stringify(state.kernel));
    console.log("cell outputs:", JSON.stringify(state.outputs));
    console.log("page text:", state.text);
    console.log("browser console (last 15):");
    for (const line of consoleLog.slice(-15)) console.log(`  ${line}`);
    throw error;
  }

  const heroText = await page.evaluate(() => {
    const el = [...document.querySelectorAll(".tg-loss")]
      .find((n) => /loss down/.test(n.textContent || ""));
    return el ? el.textContent : "";
  });
  const svgCount = await page.evaluate(
    () => document.querySelectorAll(".jp-Cell-outputArea svg[role='img']").length,
  );
  // Dump what actually rendered: output areas carry the panel the plugin drew.
  const rendered = await page.evaluate(() =>
    [...document.querySelectorAll(".jp-OutputArea, .jp-Cell-outputArea, .jp-RenderedHTMLCommon")]
      .map((n) => (n.textContent || "").replace(/\s+/g, " ").trim())
      .filter((t) => t.includes("loss") || t.includes("taugrid")),
  );
  console.log("rendered output areas:", JSON.stringify(rendered));
  const png = await page.screenshot({ fullPage: true, path: `${OUT}.png` });
  await page.screenshot({ fullPage: false, path: `${OUT}-viewport.png` });

  console.log(`hero: ${heroText}`);
  console.log(`svg[role=img] outputs: ${svgCount}`);
  console.log(`screenshots: ${OUT}.png (${png.length} bytes), ${OUT}-viewport.png`);

  const pass = /loss down/.test(heroText) && svgCount >= 1;
  console.log(`\nplugin browser e2e: ${pass ? "PASS" : "FAIL"}`);
  await teardownRun(RUN, runHandle);
  if (process.env.KEEP_OPEN === "1") {
    console.log("KEEP_OPEN=1: browser left open; kill this job to close it.");
    await new Promise(() => {});
  }
  await context.close();
  process.exit(pass ? 0 : 1);
}

main().catch(async (error) => {
  console.error(`browser e2e failed: ${error.message}`);
  await teardownRun(RUN, runHandle).catch(() => {});
  process.exit(1);
});