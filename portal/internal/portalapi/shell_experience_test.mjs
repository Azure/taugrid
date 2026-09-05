// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const html = readFileSync(new URL("assets/index.html", import.meta.url), "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

// Deliberately small DOM adapter: exercise the shipped handlers/renderers,
// without pretending to implement browser layout or accessibility-tree checks.
class Element {
  constructor(tag, document) {
    this.tagName = tag.toUpperCase();
    this.document = document;
    this.attributes = new Map();
    this.children = [];
    this.parentElement = null;
    this.listeners = new Map();
    this.classList = {
      contains: value => this.className.split(/\s+/).includes(value),
      add: value => { if (!this.classList.contains(value)) this.className += " " + value; },
      remove: value => { this.className = this.className.split(/\s+/).filter(x => x !== value).join(" "); },
      toggle: value => {
        const enabled = !this.classList.contains(value);
        this.classList[enabled ? "add" : "remove"](value);
        return enabled;
      },
    };
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  hasAttribute(name) { return this.attributes.has(name); }
  get className() { return this.getAttribute("class") || ""; }
  set className(value) { this.setAttribute("class", value); }
  get target() { return this.getAttribute("target") || ""; }
  get textContent() { return this.children.map(c => typeof c === "string" ? c : c.textContent).join(""); }
  set textContent(value) { this.replaceChildren(String(value)); }
  append(...children) {
    for (const child of children) {
      if (child instanceof Element) {
        child.remove();
        child.parentElement = this;
        this.children.push(child);
      } else this.children.push(String(child));
    }
  }
  replaceChildren(...children) {
    for (const child of this.children) if (child instanceof Element) child.parentElement = null;
    this.children = [];
    this.append(...children);
  }
  remove() {
    if (!this.parentElement) return;
    const siblings = this.parentElement.children;
    siblings.splice(siblings.indexOf(this), 1);
    this.parentElement = null;
  }
  replaceWith(element) {
    const parent = this.parentElement;
    const index = parent.children.indexOf(this);
    this.remove();
    element.remove();
    element.parentElement = parent;
    parent.children.splice(index, 0, element);
  }
  get isConnected() {
    return this === this.document.body || !!this.parentElement?.isConnected;
  }
  matches(selector) {
    const match = selector.match(/^([a-z][a-z0-9]*)?(?:\.([\w-]+))?(?:\[([\w-]+)="([^"]*)"\])?$/i);
    if (!match) throw new Error("Unsupported harness selector: " + selector);
    return (!match[1] || this.tagName === match[1].toUpperCase()) &&
      (!match[2] || this.classList.contains(match[2])) &&
      (!match[3] || this.getAttribute(match[3]) === match[4]);
  }
  querySelectorAll(selector) {
    return this.children.flatMap(child => child instanceof Element
      ? [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)] : []);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) { return this.matches(selector) ? this : this.parentElement?.closest(selector); }
  addEventListener(name, fn) {
    if (!this.listeners.has(name)) this.listeners.set(name, []);
    this.listeners.get(name).push(fn);
  }
  async dispatch(name, event = {}) {
    return Promise.all((this.listeners.get(name) || []).map(fn => fn(event)));
  }
  focus() { this.document.activeElement = this; }
}

const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
const response = (data, status = 200) => ({
  ok: status >= 200 && status < 300, status,
  json: async () => structuredClone(data),
  text: async () => typeof data === "string" ? data : JSON.stringify(data),
});
const scope = workspace => ({ workspace, name: workspace, availability: "available", experimentsUrl: "/stellar" });
const directory = workspace => ({ managed: true, selected: scope(workspace), workspaces: [scope("team-a"), scope("team-b")] });
const flush = async () => { for (let i = 0; i < 6; i++) await new Promise(resolve => setImmediate(resolve)); };

function shell(path = "/portal/services?workspace=team-b", fetcher = () => response({})) {
  const document = {
    title: "", activeElement: null, listeners: new Map(),
    createElement(tag) { return new Element(tag, this); },
    createTextNode(text) { return String(text); },
    getElementById(id) { return this.body.querySelectorAll(`[id="${id}"]`)[0] || null; },
    addEventListener(name, fn) { this.listeners.set(name, fn); },
  };
  document.body = new Element("body", document);
  for (const [tag, id] of [
    ["select", "workspace-select"], ["nav", "tab-toggle"], ["aside", "sidebar"],
    ["button", "nav-toggle"], ["div", "scope-banner"], ["div", "route-status"], ["div", "view"],
  ]) {
    const node = document.createElement(tag);
    node.setAttribute("id", id);
    document.body.append(node);
  }
  let url = new URL(path, "https://portal.example");
  const location = {
    get href() { return url.href; }, get pathname() { return url.pathname; },
    get search() { return url.search; }, get origin() { return url.origin; },
    assign(target) { url = new URL(target, url); },
  };
  let historyIndex = 0;
  const entries = [url.href];
  const windowEvents = new Map();
  const history = {
    pushState(_state, _title, target) {
      url = new URL(target, url);
      entries.splice(++historyIndex, entries.length, url.href);
    },
    replaceState(_state, _title, target) { url = new URL(target, url); entries[historyIndex] = url.href; },
    async back() { url = new URL(entries[--historyIndex]); await windowEvents.get("popstate")(); },
    async forward() { url = new URL(entries[++historyIndex]); await windowEvents.get("popstate")(); },
  };
  const requests = [], intervals = new Map();
  let now = 1000000;
  class Clock extends Date {
    constructor(...args) { super(...(args.length ? args : [now])); }
    static now() { return now; }
  }
  const context = vm.createContext({
    document, location, history, URL, URLSearchParams, console, Date: Clock,
    window: { addEventListener: (name, fn) => windowEvents.set(name, fn) },
    localStorage: { getItem: () => "platform" },
    setInterval(fn) { intervals.set(fn, fn); return fn; },
    clearInterval(fn) { intervals.delete(fn); },
    fetch: async (target, options) => {
      requests.push(String(target));
      const request = new URL(target, url.origin);
      if (request.pathname === "/api/portal/workspaces") {
        const custom = await fetcher(request, options, true);
        return custom || response(directory(request.searchParams.get("workspace") || "team-a"));
      }
      return fetcher(request, options, false);
    },
  });
  vm.runInContext(script.replace(/\n  route\(\);\s*$/, "\n  globalThis.boot = route();"), context);
  return {
    context, document, location, history, requests, entries,
    get view() { return document.getElementById("view"); },
    run(source) { return vm.runInContext(source, context); },
    async click(node, overrides = {}) {
      const event = { target: node, button: 0, defaultPrevented: false,
        preventDefault() { this.defaultPrevented = true; }, ...overrides };
      if (node.tagName === "BUTTON" && !node.disabled) await node.dispatch("click", event);
      else document.listeners.get("click")(event);
      await flush();
      return event;
    },
    advance(ms) { now += ms; for (const fn of intervals.values()) fn(); },
    async navigate(target) { history.pushState({}, "", target); await context.route({ focus: true }); },
  };
}

const withDirectory = fetcher => (url, options, isDirectory) => isDirectory ? null : fetcher(url, options);
const linkNamed = (node, text) => node.querySelectorAll("a").find(a => a.textContent === text);
const panelNamed = (env, label) => env.view.querySelectorAll("section").find(p => p.getAttribute("aria-label") === label);
const buttonNamed = (node, text) => node.querySelectorAll("button").find(b => b.textContent === text);

test("Kueue view links preserve workspace, legacy alias ownership, native modified clicks and history", async () => {
  const env = shell("/portal/jobs?workspace=team-b", withDirectory(() => response({ groups: [] })));
  await env.context.boot;
  const live = linkNamed(env.view, "Live");
  assert.equal(live.getAttribute("href"), "/portal/jobs?view=live&workspace=team-b");
  for (const override of [{ metaKey: true }, { ctrlKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }]) {
    const event = await env.click(live, override);
    assert.equal(event.defaultPrevented, false);
    assert.equal(env.entries.length, 1);
  }
  await env.click(live);
  assert.match(env.location.search, /workspace=team-b/);
  assert.equal(linkNamed(env.view, "Live").getAttribute("aria-current"), "page");
  assert.match(env.view.textContent, /raw cluster-wide scheduler state/);
  assert.ok(env.view.querySelector("iframe").getAttribute("src").includes("workspace=team-b"));
  await env.click(linkNamed(env.view, "Scheduler"));
  assert.match(env.location.search, /view=scheduler&workspace=team-b/);
  await env.history.back();
  assert.match(env.location.search, /view=live&workspace=team-b/);
  await env.navigate("/portal/kueueviz?workspace=team-b");
  assert.equal(linkNamed(env.document.getElementById("sidebar"), "Kueue").getAttribute("aria-current"), "page");
});

test("overview persona is shareable and Back/Forward restore it independently of stored preference", async () => {
  const data = withDirectory(url => response(url.pathname.endsWith("/cluster") ? { gpus: [] } : {}));
  const env = shell("/portal?workspace=team-b", data);
  await env.context.boot;
  assert.equal(env.run("currentTab()"), "workloads");
  const tabs = env.document.getElementById("tab-toggle");
  await env.click(linkNamed(tabs, "Platform"));
  assert.match(env.location.search, /persona=platform/);
  assert.equal(env.run("currentTab()"), "platform");
  assert.equal(linkNamed(env.document.getElementById("sidebar"), "Overview").getAttribute("href"), "/portal?persona=platform&workspace=team-b");
  await env.history.back();
  assert.equal(env.run("currentTab()"), "workloads");
  await env.history.forward();
  assert.equal(env.run("currentTab()"), "platform");
  const shared = shell(env.location.pathname + env.location.search, data);
  await shared.context.boot;
  assert.equal(shared.run("currentTab()"), "platform");
});

test("nested workload and durable Ray routes activate their parent navigation", async () => {
  const env = shell("/portal/runs/team-b/train?view=events&workspace=team-b", withDirectory(() => response({ diagnostics: { events: { state: "empty" } } })));
  await env.context.boot;
  assert.equal(env.run("currentTab()"), "workloads");
  assert.equal(linkNamed(env.document.getElementById("sidebar"), "Jobs").getAttribute("aria-current"), "page");
  assert.equal(linkNamed(env.view, "Events").getAttribute("aria-current"), "page");
  await env.navigate("/portal/ray/history/uid-123?workspace=team-b");
  assert.equal(env.run("currentTab()"), "platform");
  assert.equal(linkNamed(env.document.getElementById("sidebar"), "Ray").getAttribute("aria-current"), "page");
  for (const path of ["/portal/cluster", "/portal/gpu", "/portal/nodes"]) assert.equal(env.run(`tabForPath("${path}")`), "platform");
});

test("directory loading is immediate and obsolete responses cannot overwrite the newer scope", async () => {
  const first = deferred();
  const env = shell("/portal/services?workspace=team-a", (url, _options, isDirectory) => {
    if (isDirectory && url.searchParams.get("workspace") === "team-a") return first.promise;
    return isDirectory ? null : response({});
  });
  assert.match(env.view.textContent, /Loading workspace and page/);
  await env.navigate("/portal/services?workspace=team-b");
  first.resolve(response(directory("team-a")));
  await env.context.boot;
  assert.equal(env.run("activeScope.workspace"), "team-b");
  assert.match(env.document.getElementById("scope-banner").textContent, /team-b/);
  assert.equal(env.document.activeElement, env.view.children[0]);
  assert.match(env.document.getElementById("route-status").textContent, /Services loaded/);
});

test("directory failure exposes Retry without falling back to board data", async () => {
  let attempts = 0;
  const env = shell("/portal/runs?workspace=team-b", (_url, _options, isDirectory) => {
    if (isDirectory) return ++attempts === 1 ? response("denied", 403) : null;
    return response({ runs: [] });
  });
  await env.context.boot;
  assert.match(env.view.textContent, /workspace directory: 403 denied/);
  assert.match(env.document.getElementById("scope-banner").textContent, /Workspace scope unavailable/);
  assert.equal(env.requests.length, 1);
  await env.click(buttonNamed(env.view, "Retry"));
  assert.match(env.view.textContent, /No Tau-managed workloads/);
  assert.equal(env.run("activeScope.workspace"), "team-b");
});

test("workload admission renders before delayed optional GPU telemetry", async () => {
  const gpu = deferred();
  const env = shell("/portal?workspace=team-b", withDirectory(url => {
    if (url.pathname.endsWith("/cluster")) return gpu.promise;
    assert.equal(url.searchParams.get("view"), "workloads");
    return response({ cards: { queue: { admitted: 2, pending: 1, gpuUsed: 4, gpuHeadroom: 2 } }, running: [] });
  }));
  await flush();
  assert.match(panelNamed(env, "Workload admission").textContent, /Admitted workloads2/);
  assert.match(panelNamed(env, "GPU utilization").textContent, /Loading/);
  assert.doesNotMatch(env.view.textContent, /Running now/);
  gpu.resolve(response({ gpus: [{ utilizationPct: 20 }, { utilizationPct: null }], window: "15m" }));
  await env.context.boot;
  assert.match(panelNamed(env, "GPU utilization").textContent, /20%/);
  assert.match(panelNamed(env, "GPU utilization").textContent, /1 \/ 2 returned GPUs measured/);
});

test("platform fleet completes independently while health and cost are still pending", async () => {
  const health = deferred(), cost = deferred();
  const env = shell("/portal?persona=platform&workspace=team-b", withDirectory(url => {
    if (url.pathname.endsWith("/cluster")) return health.promise;
    if (url.pathname.endsWith("/cost")) return cost.promise;
    if (url.pathname.endsWith("/nodes")) return response({ readyNodes: 3, totalNodes: 4, totalGPUs: 8, gpuNodes: 2 });
    return response({ cards: { queue: { gpuHeadroom: 2, gpuUsed: 6 } } });
  }));
  await flush();
  assert.match(panelNamed(env, "Fleet inventory").textContent, /Ready nodes3 \/ 4/);
  assert.match(panelNamed(env, "Queue capacity").textContent, /Headroom2/);
  assert.match(panelNamed(env, "GPU health").textContent, /Loading/);
  cost.resolve(response({ totalGPUHours: 0, idleGPUs: [] }));
  health.resolve(response({ totalGPUs: 0, gpus: [] }));
  await env.context.boot;
  assert.match(panelNamed(env, "GPU health").textContent, /health is unknown/);
  assert.doesNotMatch(env.view.textContent, /all healthy/);
});

test("GPU and node utilization load independently and missing values are not idle or zero", async () => {
  const gpu = deferred();
  const env = shell("/portal/fleet?view=util&workspace=team-b", withDirectory(url => {
    if (url.pathname.endsWith("/cluster")) return gpu.promise;
    return response({ nodes: [{ instance: "cpu-only", cpuUtilPct: null, memUsedPct: null, memTotalBytes: null, memAvailBytes: null,
      cpuCoverage: { samples: 1, usableCores: 0, observedCores: 1, observedSeconds: 0, windowCoveragePct: 0, counterResets: 0 } }] });
  }));
  await flush();
  assert.match(panelNamed(env, "Node utilization").textContent, /cpu-only/);
  assert.match(panelNamed(env, "Node utilization").textContent, /Unknown \/ Unknown/);
  gpu.resolve(response({ window: "15m", gpus: [
    { instance: "unknown", utilizationPct: null }, { instance: "idle", utilizationPct: 0 }, { instance: "busy", utilizationPct: 30 },
  ] }));
  await env.context.boot;
  const panel = panelNamed(env, "GPU utilization");
  assert.match(panel.textContent, /measured: 2 \/ 3/);
  assert.match(panel.textContent, /measured avg: 15% · measured idle \(<5%\): 1/);
  const rows = panel.querySelector("tbody").children;
  assert.match(rows[0].textContent, /busy/);
  assert.match(rows[2].textContent, /unknown.*Unknown/);
});

test("refresh retains the previous successful DOM on error, exposes stale age, and retries only its source", async () => {
  let attempt = 0;
  const env = shell("/portal/fleet?view=compute&workspace=team-b", withDirectory(() => {
    attempt++;
    if (attempt === 2) return response("temporarily unavailable", 503);
    return response({ totalNodes: attempt, nodes: [{ name: "node-" + attempt, ready: true }] });
  }));
  await env.context.boot;
  const panel = panelNamed(env, "Fleet inventory");
  const originalTable = panel.querySelector("table");
  env.advance(61000);
  assert.match(panel.textContent, /Stale snapshot/);
  await env.click(buttonNamed(panel, "Refresh"));
  assert.equal(panel.querySelector("table"), originalTable);
  assert.match(panel.textContent, /Stale data. Refresh failed/);
  assert.match(env.location.search, /view=compute/);
  assert.equal(env.requests.filter(x => x.startsWith("/api/portal/workspaces")).length, 1);
  await env.click(buttonNamed(panel, "Retry"));
  assert.notEqual(panel.querySelector("table"), originalTable);
  assert.match(panel.textContent, /node-3/);
  assert.doesNotMatch(panel.textContent, /Stale data/);
});

test("in-flight refresh retains useful content, prevents duplicate requests and keeps control focus", async () => {
  const refresh = deferred();
  let calls = 0;
  const env = shell("/portal/fleet?view=compute&workspace=team-b", withDirectory(() =>
    ++calls === 1 ? response({ totalNodes: 1, nodes: [{ name: "useful-node", ready: true }] }) : refresh.promise));
  await env.context.boot;
  const panel = panelNamed(env, "Fleet inventory");
  const button = buttonNamed(panel, "Refresh");
  const table = panel.querySelector("table");
  button.focus();
  const pending = button.dispatch("click");
  assert.match(panel.textContent, /Refreshing; showing previous snapshot/);
  assert.equal(panel.querySelector("table"), table);
  await button.dispatch("click");
  assert.equal(calls, 2);
  refresh.resolve(response({ totalNodes: 1, nodes: [{ name: "updated-node", ready: true }] }));
  await pending;
  assert.equal(env.document.activeElement, button);
  assert.ok(button.isConnected);
  assert.match(panel.textContent, /updated-node/);
});

test("scope captured at request start cannot be replaced by an old board response", async () => {
  const old = deferred();
  const env = shell("/portal/runs?workspace=team-a", withDirectory(url =>
    url.searchParams.get("workspace") === "team-a" ? old.promise : response({ runs: [], total: 0, scope: scope("team-b") })));
  await flush();
  await env.navigate("/portal/runs?workspace=team-b");
  old.resolve(response({ runs: [{ name: "old-job", namespace: "team-a" }], scope: scope("team-a") }));
  await env.context.boot;
  assert.equal(env.run("activeScope.workspace"), "team-b");
  assert.doesNotMatch(env.view.textContent, /old-job/);
  assert.match(env.requests.find(x => x.startsWith("/api/portal/runs")), /workspace=team-a/);
});

test("namespaced job identity and neutral unknown lifecycle are visible", async () => {
  const env = shell("/portal/runs?workspace=team-b", withDirectory(() => response({ runs: [
    { name: "train", namespace: "a", status: "Running" },
    { name: "train", namespace: "b", status: "Reconciling" },
    { name: "done", namespace: "b", status: "Complete" },
  ] })));
  await env.context.boot;
  assert.ok(linkNamed(env.view, "a/train"));
  assert.ok(linkNamed(env.view, "b/train"));
  const unknown = env.view.querySelectorAll("span").find(node => node.textContent === "Reconciling");
  assert.ok(unknown.classList.contains("badge"));
  assert.equal(unknown.classList.contains("done"), false);
  assert.match(env.view.textContent, /Other \/ unknown state/);
  assert.match(env.view.textContent, /tau run submit --help/);
});

test("diagnostic failures differ from empty results and retain stale evidence during partial refresh", async () => {
  let attempt = 0;
  const env = shell("/portal/runs/ns/train?view=events&workspace=team-b", withDirectory(() => {
    attempt++;
    return response({
      status: "Running", runId: "r1", links: { stellarPath: "/stellar?target=r1" },
      diagnostics: {
        workloads: { state: "ready" }, pods: { state: "empty" },
        events: attempt === 2 ? { state: "unavailable", message: "Event access denied" } : { state: "ready" },
        tracking: { state: "ready" },
      },
      events: attempt === 2 ? [] : [{ reason: "Scheduled", message: "scheduled-" + attempt }],
    });
  }));
  await env.context.boot;
  assert.ok(linkNamed(env.view, "Open in Experiments"), "active tracked job has metrics link without lifecycle");
  const panel = panelNamed(env, "Job detail");
  await env.click(buttonNamed(panel, "Refresh"));
  assert.match(panel.textContent, /Event access denied/);
  assert.match(panel.textContent, /Showing previous successful data \(stale\)/);
  assert.match(panel.textContent, /scheduled-1/);
  assert.equal(linkNamed(panel, "Events").getAttribute("aria-current"), "page");
  await env.click(buttonNamed(panel, "Retry"));
  assert.match(panel.textContent, /scheduled-3/);
  assert.doesNotMatch(panel.textContent, /Event access denied|stale/);
});

test("Results instructions never name an absent experiment control and explain tracking states", async () => {
  for (const state of ["empty", "unavailable", "not_configured"]) {
    const env = shell("/portal/runs/ns/train?view=results&workspace=team-b", withDirectory(() => response({
      runId: "r1", diagnostics: { tracking: { state, message: "Tracking " + state } },
    })));
    await env.context.boot;
    assert.equal(linkNamed(env.view, "Open in Experiments"), undefined);
    assert.doesNotMatch(env.view.textContent, /use the Open in Experiments/);
    assert.match(env.view.textContent, /No experiment link is available/);
    assert.match(env.view.textContent, new RegExp("Tracking " + state));
  }
});

test("tracking ambiguity removes an old metrics link rather than treating stale identity as proven", async () => {
  let attempts = 0;
  const env = shell("/portal/runs/ns/train?view=results&workspace=team-b", withDirectory(() => {
    const available = ++attempts === 1;
    return response({
      runId: "r1", links: available ? { stellarPath: "/stellar?target=r1" } : {},
      diagnostics: { tracking: available ? { state: "ready" } : { state: "unavailable", message: "Ambiguous indexed identity" } },
    });
  }));
  await env.context.boot;
  assert.ok(linkNamed(env.view, "Open in Experiments"));
  await env.click(buttonNamed(panelNamed(env, "Job detail"), "Refresh"));
  assert.equal(linkNamed(env.view, "Open in Experiments"), undefined);
  assert.match(env.view.textContent, /Ambiguous indexed identity/);
});

test("older diagnostic payloads do not contradict present evidence with invented empty states", async () => {
  const env = shell("/portal/runs/ns/train?workspace=team-b", withDirectory(() => response({
    links: { stellarPath: "/stellar?target=r1" }, workloads: [{ name: "admitted-workload" }],
  })));
  await env.context.boot;
  assert.match(env.view.textContent, /Source status not reported/);
  assert.match(env.view.textContent, /admitted-workload/);
  assert.doesNotMatch(env.view.textContent, /No Kueue Workload is associated|No indexed experiment data/);
});

test("cost no-observation state does not claim every GPU is busy", async () => {
  const env = shell("/portal/cost?workspace=team-b", withDirectory(() => response({ workspaces: [], idleGPUs: [] })));
  await env.context.boot;
  assert.match(env.view.textContent, /total GPU-hours: Unknown · estimated cost: Unknown/);
  assert.match(env.view.textContent, /Insufficient utilization samples/);
  assert.doesNotMatch(env.view.textContent, /every GPU is above|all healthy/);
});

test("cost availability distinguishes measured zero, partial sums and unknown rows", async () => {
  const env = shell("/portal/cost?workspace=team-b", withDirectory(() => response({
    totalGPUHours: 0, gpuHoursAvailable: true, totalEstimatedCostUSD: 0, costAvailable: false,
    costCoverage: { observedSamples: 4, gpuHoursSamples: 2, costSamples: 0, utilizationSamples: 90 },
    workspaces: [{
      workspace: "team-b", gpuHours: 0, gpuHoursAvailable: false, estimatedCostUSD: 0, costAvailable: true,
      avgUtilPct: null, coverage: { observedSamples: 4, gpuHoursSamples: 0, costSamples: 4, utilizationSamples: 0 },
    }],
    idleAvailable: true, idleGPUs: [],
    idleCoverage: { observedGPUs: 3, measuredGPUs: 2, eligibleGPUs: 1, observedSamples: 30, validSamples: 20 },
  })));
  await env.context.boot;
  assert.match(env.view.textContent, /total GPU-hours: 0 · estimated cost: Unknown/);
  assert.match(env.view.textContent, /GPU-hours: Partial: 2 \/ 4 allocation samples/);
  assert.match(env.view.querySelector("tbody").textContent, /team-b.*Unknown\$0.00/);
  assert.match(env.view.textContent, /Partial: 1 \/ 3 observed GPUs have enough samples/);
  assert.match(env.view.textContent, /No idle GPUs among 1 eligible observed GPUs/);
  assert.doesNotMatch(env.view.textContent, /90 \/ 4|every GPU/);
});

test("platform allocation cards preserve unknown hours and idle coverage", async () => {
  const env = shell("/portal?persona=platform&workspace=team-b", withDirectory(url => {
    if (url.pathname.endsWith("/cost")) return response({
      totalGPUHours: 0, gpuHoursAvailable: false, idleAvailable: false, idleGPUs: [],
      costCoverage: { observedSamples: 1, gpuHoursSamples: 0, costSamples: 0, utilizationSamples: 0 },
      idleCoverage: { observedGPUs: 2, measuredGPUs: 0, eligibleGPUs: 0, observedSamples: 2, validSamples: 0 },
    });
    return response({ cards: { queue: {} }, gpus: [] });
  }));
  await env.context.boot;
  const panel = panelNamed(env, "Allocation cost");
  assert.match(panel.textContent, /GPU-hoursUnknown/);
  assert.match(panel.textContent, /Observed idle GPUsUnknown/);
  assert.match(panel.textContent, /0 \/ 2 observed GPUs have enough samples/);
});

test("unsupported destinations expose alternatives and responsive navigation has explicit expanded state", async () => {
  const env = shell("/portal/services?workspace=team-b", withDirectory(() => response({})));
  await env.context.boot;
  assert.match(env.document.getElementById("sidebar").textContent, /ServicesNot available/);
  assert.match(env.view.textContent, /kubectl get services/);
  const toggle = env.document.getElementById("nav-toggle");
  await env.click(toggle);
  assert.equal(toggle.getAttribute("aria-expanded"), "true");
  assert.ok(env.document.getElementById("sidebar").classList.contains("open"));
  await env.navigate("/portal/observability?workspace=team-b");
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  assert.match(env.view.textContent, /kubectl get pods/);
  // Layout is intentionally source-only: these rules provide the escape hatches
  // but do not substitute for a browser measurement at 375px or 200% zoom.
  assert.match(html, /@media \(max-width: 900px\)/);
  assert.match(html, /\.workspace-picker select \{ min-width: 0; width: 100%/);
  assert.match(html, /\.table-scroll \{ max-width: 100%; overflow-x: auto/);
});

test("data tables are independently scrollable keyboard regions", async () => {
  const env = shell("/portal/fleet?view=health&workspace=team-b", withDirectory(() => response({
    gpus: [{ instance: "node", gpu: "0", utilizationPct: null, healthy: null }],
  })));
  await env.context.boot;
  const table = env.view.querySelector("table");
  assert.equal(table.parentElement.getAttribute("tabindex"), "0");
  assert.equal(table.parentElement.getAttribute("role"), "region");
  assert.ok(table.parentElement.classList.contains("table-scroll"));
  assert.match(table.textContent, /Unknown/);
  assert.doesNotMatch(table.textContent, /Healthy \(observed\)|error/);
});

test("native targets, downloads, prevented clicks and unrelated routes are not intercepted", async () => {
  const env = shell("/portal/services?workspace=team-b", withDirectory(() => response({})));
  await env.context.boot;
  for (const attrs of [
    { href: "/portal/runs", target: "_blank" }, { href: "/portal/runs", download: "" },
    { href: "/portal/runs", class: "external" }, { href: "/portal-other" }, { href: "#main" },
  ]) {
    const link = env.document.createElement("a");
    for (const [name, value] of Object.entries(attrs)) link.setAttribute(name, value);
    env.document.body.append(link);
    const event = await env.click(link);
    assert.equal(event.defaultPrevented, false);
  }
  await env.click(linkNamed(env.view, "Ray dashboard discovery"), { defaultPrevented: true });
  assert.equal(env.entries.length, 1);
});
