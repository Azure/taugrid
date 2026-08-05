#!/usr/bin/env node

import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const defaultCaptures = [
  {
    name: "stellar-live-experiment-list",
    path: "/stellar",
    kind: "experiment-list",
    api: "/api/v1/stellar/experiments?since=90d",
    readySelector: ".landing-experiment-card",
  },
  {
    name: "stellar-live-nanogpt-experiment",
    path: "/stellar?target=nanogpt-api-surface&metric=val/loss&pinned=train/loss,val/loss,train/tokens_per_second",
    kind: "target-dashboard",
    target: "nanogpt-api-surface",
    api: "/api/v1/stellar/snapshot?target=nanogpt-api-surface",
    readySelector: ".app-shell .panel:not(.stellar-loading-panel)",
  },
  {
    name: "stellar-live-nanogpt-h200-group",
    path: "/stellar?target=h200-328-target&metric=val/loss&pinned=train/loss,val/loss,train/tokens_per_second",
    kind: "target-dashboard",
    target: "h200-328-target",
    api: "/api/v1/stellar/snapshot?target=h200-328-target",
    readySelector: ".app-shell .panel:not(.stellar-loading-panel)",
  },
];

const secretQueryKeys = new Set([
  "access_token",
  "api_key",
  "apikey",
  "authorization",
  "client_secret",
  "code",
  "password",
  "se",
  "sig",
  "skoid",
  "sks",
  "skt",
  "sktid",
  "ske",
  "skv",
  "sp",
  "sr",
  "subscription-key",
  "sv",
  "token",
]);

const sensitiveTextPatterns = [
  /SharedAccessSignature/i,
  /\bsig=[A-Za-z0-9%+/=_-]{12,}/i,
  /\b(access_token|client_secret|api[_-]?key|subscription-key|password)=/i,
  /\bBearer\s+[A-Za-z0-9._~+/=-]{20,}/i,
  /\bAccountKey=[A-Za-z0-9+/=]{20,}/i,
];

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const playwright = await importPlaywright();
  const baseURL = normalizeBaseURL(opts.baseUrl, opts.allowLocalhost);
  const captures = opts.captures.length > 0 ? opts.captures : defaultCaptures;
  const capturedAt = new Date().toISOString();
  const gitCommit = git(["rev-parse", "--short", "HEAD"]);

  if (!opts.cluster || !opts.namespace || !opts.service) {
    throw new Error("--cluster, --namespace, and --service are required provenance fields");
  }
  if (!isLoopbackHost(baseURL.hostname) && !opts.storageState) {
    throw new Error("--storage-state or STELLAR_STORAGE_STATE is required for authenticated hosted captures");
  }

  await fs.mkdir(opts.out, { recursive: true });
  if (opts.storageState) {
    try {
      await fs.access(opts.storageState);
    } catch {
      throw new Error("Playwright storage state is not readable");
    }
  }

  const browser = await playwright.chromium.launch({ headless: true });
  const manifest = {
    schema_version: "tau.stellar.live_screenshot_manifest.v0",
    captured_at: capturedAt,
    provenance: {
      cluster: opts.cluster,
      namespace: opts.namespace,
      service: opts.service,
      base_url: sanitizeURL(baseURL.href),
      git_commit: gitCommit,
      operator: opts.operator,
      authentication: opts.storageState ? "preauthenticated-storage-state" : "none",
    },
    validation: {
      live_service_required: true,
      mocks_rejected: true,
      sample_routes_rejected: true,
      sensitive_query_keys_rejected: Array.from(secretQueryKeys).sort(),
    },
    captures: [],
  };

  try {
    let context;
    try {
      context = await browser.newContext({
        viewport: { width: opts.width, height: opts.height },
        deviceScaleFactor: opts.scale,
        ...(opts.storageState ? { storageState: opts.storageState } : {}),
      });
    } catch (err) {
      if (opts.storageState) {
        throw new Error("Playwright storage state could not be loaded");
      }
      throw err;
    }
    const page = await context.newPage();
    page.setDefaultTimeout(opts.timeoutMs);

    const consoleErrors = [];
    page.on("console", (msg) => {
      if (msg.type() === "error") {
        consoleErrors.push(msg.text());
      }
    });

    await assertLiveEndpoint(page, baseURL);

    for (const capture of captures) {
      const result = await captureOne(page, baseURL, capture, opts);
      if (consoleErrors.length > 0) {
        assertNoSensitiveText(consoleErrors.join("\n"), `console errors for ${capture.name}`);
      }
      manifest.captures.push(result);
    }
  } finally {
    await browser.close();
  }

  if (manifest.captures.length === 0) {
    throw new Error("no screenshots captured");
  }
  const manifestPath = path.join(opts.out, "stellar-screenshot-manifest.json");
  await fs.writeFile(manifestPath, JSON.stringify(manifest, null, 2) + "\n", "utf8");
  console.log(`wrote ${manifest.captures.length} live Stellar screenshots and ${manifestPath}`);
}

function parseArgs(args) {
  const opts = {
    baseUrl: process.env.STELLAR_BASE_URL || "",
    out: "site/static/images/stellar",
    cluster: process.env.STELLAR_CLUSTER || "",
    namespace: process.env.STELLAR_NAMESPACE || "tau",
    service: process.env.STELLAR_SERVICE || "tau-stellar",
    storageState: process.env.STELLAR_STORAGE_STATE || "",
    operator: process.env.USER || process.env.USERNAME || "",
    captures: [],
    allowLocalhost: false,
    width: 1440,
    height: 1100,
    scale: 1,
    timeoutMs: 45000,
  };

  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    switch (arg) {
      case "--base-url":
        opts.baseUrl = requireValue(args, ++i, arg);
        break;
      case "--out":
        opts.out = requireValue(args, ++i, arg);
        break;
      case "--cluster":
        opts.cluster = requireValue(args, ++i, arg);
        break;
      case "--namespace":
        opts.namespace = requireValue(args, ++i, arg);
        break;
      case "--service":
        opts.service = requireValue(args, ++i, arg);
        break;
      case "--storage-state":
        opts.storageState = requireValue(args, ++i, arg);
        break;
      case "--operator":
        opts.operator = requireValue(args, ++i, arg);
        break;
      case "--capture":
        opts.captures.push(parseCapture(requireValue(args, ++i, arg)));
        break;
      case "--allow-localhost":
        opts.allowLocalhost = true;
        break;
      case "--width":
        opts.width = parsePositiveInt(requireValue(args, ++i, arg), arg);
        break;
      case "--height":
        opts.height = parsePositiveInt(requireValue(args, ++i, arg), arg);
        break;
      case "--scale":
        opts.scale = parsePositiveInt(requireValue(args, ++i, arg), arg);
        break;
      case "--timeout-ms":
        opts.timeoutMs = parsePositiveInt(requireValue(args, ++i, arg), arg);
        break;
      case "--help":
      case "-h":
        printHelp();
        process.exit(0);
        break;
      default:
        throw new Error(`unknown argument ${arg}`);
    }
  }
  return opts;
}

function parseCapture(value) {
  const index = value.indexOf("=");
  if (index < 1 || index === value.length - 1) {
    throw new Error(`--capture must be name=/stellar?...; got ${value}`);
  }
  const name = safeName(value.slice(0, index));
  const route = value.slice(index + 1);
  const url = new URL(route, "http://placeholder");
  const target = url.searchParams.get("target") || "";
  return {
    name,
    path: route,
    kind: target ? "target-dashboard" : "experiment-list",
    target,
    api: target
      ? `/api/v1/stellar/snapshot?target=${encodeURIComponent(target)}`
      : "/api/v1/stellar/experiments?since=90d",
    readySelector: target
      ? ".app-shell .panel:not(.stellar-loading-panel)"
      : ".landing-experiment-card",
  };
}

async function importPlaywright() {
  const packageRoot = process.env.PLAYWRIGHT_PACKAGE_ROOT || "";
  if (packageRoot) {
    const requireFromPackageRoot = createRequire(pathToFileURL(path.join(packageRoot, "package.json")));
    return requireFromPackageRoot("playwright");
  }
  try {
    return await import("playwright");
  } catch (err) {
    throw new Error(
      `Playwright is required. Install it in a throwaway directory and set PLAYWRIGHT_PACKAGE_ROOT to that directory. (${err.message})`,
    );
  }
}

function normalizeBaseURL(value, allowLocalhost) {
  if (!value) {
    throw new Error("--base-url or STELLAR_BASE_URL is required");
  }
  const url = new URL(value);
  if (!["http:", "https:"].includes(url.protocol)) {
    throw new Error("--base-url must be http or https");
  }
  rejectSensitiveURL(url);
  if (isLoopbackHost(url.hostname) && !allowLocalhost) {
    throw new Error("loopback base URLs are rejected for wiki captures; pass --allow-localhost only for local dry-runs");
  }
  url.pathname = url.pathname.replace(/\/+$/, "");
  url.search = "";
  url.hash = "";
  return url;
}

async function assertLiveEndpoint(page, baseURL) {
  const healthURL = new URL("/healthz", baseURL);
  const response = await page.request.get(healthURL.href);
  if (!response.ok()) {
    throw new Error(`live Stellar health check failed: ${response.status()} ${healthURL.href}`);
  }
}

async function captureOne(page, baseURL, capture, opts) {
  const name = safeName(capture.name);
  const route = normalizeRoute(capture.path);
  const targetURL = new URL(route, baseURL);
  rejectSensitiveURL(targetURL);
  rejectMockOrSampleRoute(targetURL);

  const apiURL = new URL(capture.api, baseURL);
  rejectSensitiveURL(apiURL);
  rejectMockOrSampleRoute(apiURL);
  const apiEvidence = await fetchAPIEvidence(page, apiURL, capture);

  await page.goto(targetURL.href, { waitUntil: "networkidle" });
  await page.waitForFunction(() => window.__stellarVisual?.status === "ready");
  await page.locator(capture.readySelector).first().waitFor({ state: "visible" });

  const visual = await page.evaluate(() => window.__stellarVisual);
  assertNoSensitiveText(JSON.stringify(visual), `visual state for ${name}`);
  assertVisualMatchesCapture(visual, capture);

  const bodyText = await page.locator("body").innerText({ timeout: opts.timeoutMs });
  assertNoSensitiveText(bodyText, `rendered page text for ${name}`);
  assertRenderedText(bodyText, capture);

  const screenshotPath = path.join(opts.out, `${name}.png`);
  await page.screenshot({ path: screenshotPath, fullPage: true });
  const bytes = await fs.readFile(screenshotPath);
  if (bytes.length < 10_000) {
    throw new Error(`${screenshotPath} is too small to be a useful live screenshot`);
  }

  return {
    name,
    kind: capture.kind,
    target: capture.target || "",
    page_url: sanitizeURL(targetURL.href),
    api_url: sanitizeURL(apiURL.href),
    file: path.basename(screenshotPath),
    sha256: crypto.createHash("sha256").update(bytes).digest("hex"),
    bytes: bytes.length,
    captured_at: new Date().toISOString(),
    validation: {
      healthz_ok: true,
      api_status: apiEvidence.status,
      api_non_empty: apiEvidence.nonEmpty,
      visual_status: visual.status,
      visual_target: visual.target || "",
      rendered_selector: capture.readySelector,
      sensitive_text_scan: "passed",
      mock_sample_route_scan: "passed",
    },
  };
}

async function fetchAPIEvidence(page, apiURL, capture) {
  const response = await page.request.get(apiURL.href);
  if (!response.ok()) {
    throw new Error(`Stellar API validation failed for ${capture.name}: ${response.status()} ${apiURL.href}`);
  }
  const text = await response.text();
  assertNoSensitiveText(text, `API response for ${capture.name}`);
  const parsed = JSON.parse(text);
  const serialized = JSON.stringify(parsed);
  if (serialized.length < 20) {
    throw new Error(`Stellar API response for ${capture.name} is empty`);
  }
  if (serialized.includes('"error"') && serialized.length < 200) {
    throw new Error(`Stellar API response for ${capture.name} looks like an error payload: ${serialized}`);
  }
  return { status: response.status(), nonEmpty: true };
}

function assertVisualMatchesCapture(visual, capture) {
  if (!visual || visual.status !== "ready") {
    throw new Error(`${capture.name} did not reach Stellar ready visual state`);
  }
  if (capture.kind === "experiment-list") {
    if (visual.detail?.view !== "landing") {
      throw new Error(`${capture.name} rendered ${visual.detail?.view || "<unknown>"} instead of the experiment landing view`);
    }
    if (!Number.isFinite(visual.detail?.experimentCount) || visual.detail.experimentCount <= 0) {
      throw new Error(`${capture.name} did not report any live Stellar experiments`);
    }
  }
  if (capture.kind === "target-dashboard" && visual.detail?.view !== "dashboard") {
    throw new Error(`${capture.name} rendered ${visual.detail?.view || "<unknown>"} instead of the target dashboard view`);
  }
  if (capture.target && visual.target !== capture.target) {
    throw new Error(`${capture.name} rendered target ${visual.target || "<empty>"} instead of ${capture.target}`);
  }
}

function assertRenderedText(bodyText, capture) {
  if (bodyText.trim().length < 20) {
    throw new Error(`${capture.name} rendered an unexpectedly small page body`);
  }
  if (capture.target && !bodyText.includes(capture.target)) {
    throw new Error(`${capture.name} rendered text does not include target ${capture.target}`);
  }
}

function normalizeRoute(value) {
  if (!value.startsWith("/")) {
    throw new Error(`capture route must be a service-relative path: ${value}`);
  }
  return value;
}

function rejectSensitiveURL(url) {
  for (const key of url.searchParams.keys()) {
    if (secretQueryKeys.has(key.toLowerCase())) {
      throw new Error(`sensitive query key ${key} is not allowed in capture URL ${sanitizeURL(url.href)}`);
    }
  }
}

function rejectMockOrSampleRoute(url) {
  const text = `${url.pathname}?${url.searchParams.toString()}`.toLowerCase();
  if (text.includes("/sample") || text.includes("perf_sample") || text.includes("mock") || text.includes("fixture")) {
    throw new Error(`mock/sample route is not allowed for wiki captures: ${sanitizeURL(url.href)}`);
  }
}

function assertNoSensitiveText(value, context) {
  for (const pattern of sensitiveTextPatterns) {
    if (pattern.test(value)) {
      throw new Error(`sensitive content detected in ${context}`);
    }
  }
}

function sanitizeURL(value) {
  const url = new URL(value);
  for (const key of [...url.searchParams.keys()]) {
    if (secretQueryKeys.has(key.toLowerCase())) {
      url.searchParams.set(key, "REDACTED");
    }
  }
  return url.href;
}

function isLoopbackHost(hostname) {
  const value = hostname.toLowerCase();
  return value === "localhost" || value === "127.0.0.1" || value === "::1" || value.endsWith(".localhost");
}

function safeName(value) {
  const name = value.trim();
  if (!/^[a-z0-9][a-z0-9._-]*$/i.test(name)) {
    throw new Error(`invalid capture name ${value}; use letters, numbers, dots, underscores, and dashes`);
  }
  return name;
}

function requireValue(args, index, flag) {
  if (index >= args.length || args[index].startsWith("--")) {
    throw new Error(`${flag} requires a value`);
  }
  return args[index];
}

function parsePositiveInt(value, flag) {
  const parsed = Number.parseInt(value, 10);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    throw new Error(`${flag} must be a positive integer`);
  }
  return parsed;
}

function git(args) {
  const result = spawnSync("git", args, { encoding: "utf8" });
  return result.status === 0 ? result.stdout.trim() : "";
}

function printHelp() {
  console.log(`Capture live Stellar screenshots for Tau wiki assets.

Required:
  --base-url URL       Live tau-stellar service base URL, or STELLAR_BASE_URL
  --cluster NAME       Kubernetes context/cluster provenance, or STELLAR_CLUSTER
  --namespace NAME     Kubernetes namespace provenance, default tau
  --service NAME       Kubernetes service provenance, default tau-stellar

Environment:
  PLAYWRIGHT_PACKAGE_ROOT  Directory containing a throwaway npm install of playwright
  STELLAR_STORAGE_STATE    Private Playwright auth state for hosted captures

Optional:
  --out DIR            Output directory, default site/static/images/stellar
  --storage-state FILE Playwright auth state; required for non-loopback captures
  --capture name=PATH  Repeatable service-relative Stellar page path
  --allow-localhost    Allow loopback URL only for local dry-runs, not wiki captures
  --width N            Browser viewport width, default 1440
  --height N           Browser viewport height, default 1100
  --timeout-ms N       Browser/API timeout, default 45000

Default captures:
  stellar-live-experiment-list=/stellar
  stellar-live-nanogpt-experiment=/stellar?target=nanogpt-api-surface&metric=val/loss&pinned=train/loss,val/loss,train/tokens_per_second
  stellar-live-nanogpt-h200-group=/stellar?target=h200-328-target&metric=val/loss&pinned=train/loss,val/loss,train/tokens_per_second`);
}

main().catch((err) => {
  console.error(err.message);
  process.exit(1);
});
