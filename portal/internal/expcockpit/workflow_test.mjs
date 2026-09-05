// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";
import { createDOM } from "./testdom.mjs";

const source = readFileSync(new URL("./assets/app.js", import.meta.url), "utf8");

function harness(url = "http://stellar.test/stellar") {
  const dom = createDOM();
  const storage = new Map();
  const history = [];
  const window = {
    location: new URL(url),
    localStorage: { getItem: (key) => storage.get(key) || null, setItem: (key, value) => storage.set(key, value) },
    history: {
      pushState: (_, __, next) => { history.push(window.location.href); window.location = new URL(next); },
      replaceState: (_, __, next) => { window.location = new URL(next); },
    },
    addEventListener() {}, setTimeout() { return 1; }, clearTimeout() {},
    requestAnimationFrame() { return 1; }, cancelAnimationFrame() {},
  };
  const context = vm.createContext({ ...dom, window, URL, URLSearchParams, TextEncoder, btoa, console, performance,
    fetch: () => new Promise(() => {}), Intl, Map, Set });
  vm.runInContext(source, context);
  const state = vm.runInContext("state", context);
  state.target = "exp-a";
  state.autoRefresh.enabled = false;
  state.experimentsLoading = false;
  const fixture = {
    target: "exp-a", metric_options: [{ name: "eval/score", card: "Quality" }],
    run_groups: [{ run_group_id: "a" }, { run_group_id: "b" }],
    runs: [
      { run_id: "ok", run_group_id: "a", lifecycle_state: "succeeded", metric_names: ["eval/score"], updated_at: new Date().toISOString() },
      { run_id: "bad", run_group_id: "b", lifecycle_state: "failed", metric_names: ["eval/score"], updated_at: "2020-01-01T00:00:00Z" },
    ],
    chart: { has_data: true, metric_name: "eval/score", series: [
      { run_id: "ok", run_group_id: "a", values: [{ step: 0, value: 0.4 }], smoothed_values: [{ step: 0, value: 0.3 }] },
      { run_id: "bad", run_group_id: "b", values: [{ step: 100, value: 0.99 }] },
    ] },
    status: {}, summary: {}, artifacts: [], configs: [], events: [], observations: [],
  };
  state.snapshot = state.summarySnapshot = fixture;
  state.metric = "eval/score";
  state.selectedMetrics = ["eval/score"];
  state.featuredSnapshots.set(state.metric, fixture);
  return { ...context, state, fixture, storage, history, evaluate: (script) => vm.runInContext(script, context) };
}

test("F03 select value is applied after options, including media selectors", () => {
  const h = harness();
  const select = h.h("select", { value: "b" }, h.h("option", { value: "a" }, "A"), h.h("option", { value: "b" }, "B"));
  assert.equal(select.value, "b");
  h.state.landingProjectFilter = "b";
  const projects = h.renderLandingProjectSelect([{ project: "a", count: 1 }, { project: "b", count: 2 }], 3);
  // Exercise the helper through the real media component too.
  h.state.outputMediaRunID = "bad";
  h.state.outputMediaStep = "100";
  const controls = h.renderOutputMediaControls({ selectedTag: "tag-b", tags: ["tag-a", "tag-b"], runs: ["ok", "bad"], steps: ["0", "100"] });
  assert.deepEqual(controls.querySelectorAll("select").map((node) => node.value), ["tag-b", "bad", "100"]);
  assert.equal(projects.querySelector("select").value, "b");
});

for (const [name, apply] of [
  ["lifecycle", (state) => { state.lifecycleFilter = "succeeded"; }],
  ["group", (state) => { state.group = "a"; }],
  ["search", (state) => { state.search = "ok"; }],
  ["recency", (state) => { state.runUpdatedFilter = "24h"; }],
  ["hidden runs", (state) => { state.hiddenRuns.add("bad"); }],
]) {
  test(`F04 ${name} is shared by chart, best/latest and evidence`, () => {
    const h = harness();
    h.fixture.events = [{ event_id: "a", run_id: "ok" }, { event_id: "b", run_id: "bad" }];
    apply(h.state);
    const metricCopy = { ...h.fixture, runs: h.fixture.runs.map((run) => ({ ...run, lifecycle_state: "unknown" })) };
    assert.equal(h.bestSignal(metricCopy, {}).run_id, "ok");
    assert.equal(h.latestSignal(metricCopy, {}).run_id, "ok");
    assert.deepEqual(Array.from(h.chartDataset(metricCopy.chart), (series) => series.runID), ["ok"]);
    assert.deepEqual(Array.from(h.allRunEvidence(metricCopy, "events"), (item) => item.run_id), ["ok"]);
  });
}

test("F04 empty cohort cannot fall back to unfiltered group or sweep best", () => {
  const h = harness();
  h.state.search = "no matches";
  h.fixture.sweep = { best_run: { run_id: "bad", value: 99 } };
  h.fixture.cards = [{ metrics: [{ name: "eval/score", groups: [{ best: 99 }] }] }];
  assert.equal(h.bestSignal(h.fixture, {}), null);
  assert.equal(h.latestSignal(h.fixture, {}), null);
  assert.equal(h.metricValueSummary("eval/score").value, "No selected runs");
  assert.equal(h.metricSignalSummary({}, { step: 0, run_id: "ok", value: 1 }).step, 0);
});

test("F10 comparison URL roundtrips cohort and restores destination preferences", () => {
  const h = harness();
  Object.assign(h.state, { search: "ok", group: "a", lifecycleFilter: "succeeded", runUpdatedFilter: "24h" });
  h.state.hiddenRuns.add("bad");
  h.updateURL();
  const saved = new URL(h.window.location.href);
  h.storage.set(h.pinnedStorageKey("exp-b"), JSON.stringify(["train/loss"]));
  h.storage.set(h.dashboardSectionStorageKey("exp-b"), JSON.stringify({ hidden: ["media"], titles: { charts: "B charts" } }));
  h.restoreTargetPreferences(new URL("http://stellar.test/stellar"), "exp-b");
  assert.equal(h.state.group, "");
  assert.equal(h.state.search, "");
  assert.equal(h.state.focusedSeriesControls.runID, "");
  assert.deepEqual(Array.from(h.state.selectedMetrics), ["train/loss"]);
  assert.ok(h.state.dashboardSections.hidden.has("media"));
  h.restoreTargetPreferences(saved, "exp-a");
  assert.equal(h.state.group, "a");
  assert.equal(h.state.search, "ok");
  assert.ok(h.state.hiddenRuns.has("bad"));
  h.state.group = "absent";
  h.state.focusedSeriesControls.runID = "absent";
  h.reconcileRunControls(h.fixture);
  assert.equal(h.state.group, "");
  assert.equal(h.state.focusedSeriesControls.runID, "");
});

test("F10 explicit empty pins and invalid saved metric preferences resolve predictably", () => {
  const h = harness();
  h.restoreTargetPreferences(new URL("http://stellar.test/stellar?pinned="), "exp-a");
  h.ensureSelectedMetrics(h.fixture);
  h.updateURL();
  assert.equal(h.state.selectedMetrics.length, 0);
  assert.equal(h.window.location.searchParams.get("pinned"), "");
  h.state.selectedMetrics = ["absent"];
  h.state.metric = "absent";
  h.ensureSelectedMetrics(h.fixture);
  assert.deepEqual(Array.from(h.state.selectedMetrics), ["eval/score"]);
  assert.equal(h.state.metric, "eval/score");
});

test("F10 A to B to Back restores the comparison, layout and selected run range", async () => {
  const h = harness();
  h.state.group = "a";
  h.state.search = "ok";
  h.state.hiddenRuns.add("bad");
  h.state.focusedSeriesControls = { runID: "ok", startStep: "0", endStep: "5", stepInterval: "25", customStepInterval: "25", maxPoints: "" };
  h.updateURL();
  const aURL = h.window.location.href;
  h.evaluate(`
    fetchSnapshotFor = async () => ({ ...state.summarySnapshot, target: state.target, runs: [
      { run_id: "ok", run_group_id: "a", lifecycle_state: "succeeded", metric_names: ["eval/score"] },
      { run_id: "bad", run_group_id: "b", lifecycle_state: "failed", metric_names: ["eval/score"] }
    ], run_groups: [{ run_group_id: "a" }, { run_group_id: "b" }],
      metric_options: [{ name: "eval/score", card: "Quality" }], chart: {}, status: {}, summary: {} });
    fetchExperiments = async () => ({ experiments: [] });
    loadFocusedSeriesDetail = async () => { globalThis.loadedDetail = { ...state.focusedSeriesControls }; };
  `);
  h.selectExperiment("exp-b");
  // Wait for the actual async fetch/render chain to settle without timers.
  for (let i = 0; i < 30; i++) await Promise.resolve();
  assert.equal(h.state.target, "exp-b");
  assert.equal(h.state.group, "");
  assert.equal(h.state.search, "");
  assert.equal(h.state.focusedSeriesControls.runID, "");
  h.window.location = new URL(aURL);
  await h.restoreRouteFromLocation();
  assert.equal(h.state.target, "exp-a");
  assert.equal(h.state.group, "a");
  assert.equal(h.state.search, "ok");
  assert.ok(h.state.hiddenRuns.has("bad"));
  assert.equal(h.evaluate("loadedDetail").stepInterval, "25");
  assert.equal(h.state.focusedSeriesControls.endStep, "5");
});

test("F10 stale evidence response is ignored after leaving target", async () => {
  const h = harness();
  let resolve;
  h.evaluate("render = () => {}");
  h.fetchSnapshotFor = h.evaluate("fetchSnapshotFor = () => pendingEvidence");
  const pending = new Promise((done) => { resolve = done; });
  // Expose only the deferred transport; production async control flow is real.
  h.evaluate("globalThis.setPending = value => { globalThis.pendingEvidence = value; }");
  h.evaluate("setPending")(pending);
  const load = h.loadFullSnapshotDetails();
  h.state.routeVersion++;
  resolve({ target: "old", chart: { metric_name: "old" } });
  await load;
  assert.equal(h.state.fullSnapshot, null);
  assert.equal(h.state.featuredSnapshots.has("old"), false);
});

test("F13 event 41, full config and seventh table column are reachable", () => {
  const h = harness();
  const items = Array.from({ length: 41 }, (_, i) => ({ event_id: String(i), message: `event-${i}` }));
  const section = h.renderEvidenceListSection("Events", "logs", items, h.renderEventEvidenceItem);
  assert.equal(section.textContent.includes("event-40"), false);
  section.querySelectorAll(".record-pagination button")[1].dispatch("click");
  assert.ok(section.textContent.includes("event-40"));
  const long = "x".repeat(500);
  assert.ok(h.renderConfigEvidenceItem({ normalized_json: JSON.stringify({ long }) }).querySelector("pre").textContent.includes(long));
  const table = h.renderArtifactPreview({ artifact_id: "table", table: {
    columns: ["a", "b", "c", "d", "e", "f", "g"],
    rows: Array.from({ length: 26 }, (_, i) => ({ a: i, g: `seventh-${i}` })),
  } }, "");
  assert.equal(table.querySelectorAll("th").length, 7);
  table.querySelectorAll(".record-pagination button")[1].dispatch("click");
  assert.ok(table.textContent.includes("seventh-25"));
});

test("F13 ninth media record can be paged to and opened", () => {
  const h = harness();
  h.fixture.artifacts = Array.from({ length: 9 }, (_, i) => ({
    artifact_id: String(i), run_id: "ok", tag: "images", type: "image", uri: `https://example.test/${i}.png`,
  }));
  const media = h.renderOutputMediaPanel(h.fixture);
  media.querySelectorAll(".record-pagination button")[1].dispatch("click");
  assert.equal(media.querySelectorAll(".output-media-card").length, 1);
  assert.ok(media.querySelector(".output-media-card a").getAttribute("href"));
});

test("F14 Custom appears immediately and invalid values survive rerender", () => {
  const h = harness();
  let form = h.renderFocusedSeriesControls(h.fixture.chart, {});
  const resolution = form.querySelector("select[name=stepInterval]");
  const custom = form.querySelector("input[name=customStepInterval]");
  assert.ok(custom.parentNode.hidden);
  resolution.value = "custom";
  resolution.dispatch("change");
  assert.equal(custom.parentNode.hidden, false);
  custom.value = "not an integer";
  custom.dispatch("input");
  form.dispatch("submit");
  assert.ok(form.querySelector("#series-control-error").textContent.includes("integer"));
  form = h.renderFocusedSeriesControls(h.fixture.chart, {});
  assert.equal(form.querySelector("input[name=customStepInterval]").value, "not an integer");
  assert.equal(form.querySelector("select[name=stepInterval]").value, "custom");
  h.evaluate("loadFocusedSeriesDetail = async () => {}");
  form.querySelector("input[name=customStepInterval]").value = "25";
  form.dispatch("submit");
  assert.equal(h.state.focusedSeriesControls.stepInterval, "25");
  assert.equal(h.state.focusedSeriesDraft, null);
});

test("F15 single sample comparison represents every run, without a greatest-step reduction", () => {
  const h = harness();
  const node = h.renderMetricCardBody({ name: "eval/score" }, h.fixture, h.chartDataset(h.fixture.chart), true);
  const rows = node.querySelectorAll("tbody tr");
  assert.equal(rows.length, 2);
  assert.ok(rows[0].textContent.includes("ok"));
  assert.ok(rows[1].textContent.includes("bad"));
});

test("F21 all raw/EMA samples and series are keyboard-pageable in bounded tables", () => {
  const h = harness();
  const dataset = Array.from({ length: 9 }, (_, i) => ({
    runID: `run-${i}`, runGroupID: "group", points: Array.from({ length: 12 }, (_, step) => ({ step, value: step + 0.123 })),
    smoothedPoints: [{ step: 0, value: 0.045 }, { step: 20, value: 0.5 }],
  }));
  const node = h.renderSampleTable(dataset, "train/loss");
  assert.ok(node.querySelector("summary"));
  assert.equal(node.querySelectorAll("tbody tr").length, 0);
  node.open = true;
  node.dispatch("toggle");
  assert.equal(node.querySelectorAll("tbody tr").length, 50);
  assert.ok(node.textContent.includes("0.045"));
  const next = node.querySelectorAll(".record-pagination button")[1];
  while (!next.disabled) next.dispatch("click");
  assert.ok(node.textContent.includes("run-8"));
  assert.ok(node.textContent.includes("Not sampled"));
  assert.ok(node.querySelectorAll("tbody tr").length <= 50);
});

test("F19 stable slot keeps media identity; changed slots retain disclosure, focus and scroll", () => {
  const h = harness();
  const slot = h.h("div");
  h.root.append(slot);
  const build = () => h.h("details", {}, h.h("summary", {}, "Evidence"),
    h.h("input", { name: "draft", value: "text" }), h.h("div", { class: "table-scroll" }), h.h("iframe", { src: "/report" }));
  h.updateSlot(slot, { version: 1 }, build);
  const frame = slot.querySelector("iframe");
  const input = slot.querySelector("input");
  slot.querySelector("details").open = true;
  slot.querySelector(".table-scroll").scrollLeft = 65;
  input.focus();
  input.setSelectionRange(1, 3);
  h.updateSlot(slot, { version: 1 }, build);
  assert.equal(slot.querySelector("iframe"), frame);
  assert.equal(h.document.activeElement, input);
  h.updateSlot(slot, { version: 2 }, build);
  assert.equal(slot.querySelector("details").open, true);
  assert.equal(slot.querySelector(".table-scroll").scrollLeft, 65);
  assert.equal(h.document.activeElement.selectionStart, 1);
});

test("F19 full dashboard refresh keeps run checkbox, control draft, media and report instances", () => {
  const h = harness();
  h.fixture.artifacts = [{ artifact_id: "report", run_id: "ok", tag: "report", type: "html media report", uri: "report.html" },
    { artifact_id: "video", run_id: "ok", tag: "video", type: "video", uri: "clip.mp4" }];
  h.state.outputMediaTag = "report";
  h.render();
  const rail = h.root.querySelector(".variables-rail");
  const checkbox = rail.querySelector(".run-list input");
  const drawer = rail.querySelector(".controls-drawer");
  const frame = h.root.querySelector("iframe");
  assert.ok(frame);
  drawer.open = true;
  checkbox.focus();
  checkbox.dispatch("change");
  assert.equal(h.root.querySelector(".run-list input"), checkbox);
  assert.equal(h.document.activeElement, checkbox);
  assert.equal(drawer.open, true);
  checkbox.dispatch("change");
  const currentFrame = h.root.querySelector("iframe");
  h.fixture.runs[0].updated_at = new Date(Date.now() + 1000).toISOString();
  h.fixture.chart.series[0].values[0].value = 0.5;
  h.render();
  assert.equal(h.root.querySelector("iframe"), currentFrame);
  assert.equal(h.root.querySelector(".variables-rail"), rail);
  h.fixture.artifacts.push({ artifact_id: "new", run_id: "ok", tag: "report", type: "html media report", uri: "new.html" });
  h.render();
  assert.equal(h.root.querySelector("iframe"), currentFrame);
  const search = h.root.querySelector("[data-search-input]");
  search.focus();
  search.value = "o";
  search.setSelectionRange(0, 0);
  search.dispatch("input");
  assert.equal(h.document.activeElement, search);
  assert.equal(search.selectionStart, 0);
});

test("F19 invalid query drafts and per-record disclosures survive unrelated data changes", () => {
  const h = harness();
  h.fixture.chart.series[0].point_count = 1;
  h.fixture.events = [{ event_id: "one", message: "one" }, { event_id: "two", message: "two" }];
  h.render();
  const form = h.root.querySelector(".focused-series-controls");
  const cap = form.querySelector("input[name=maxPoints]");
  cap.value = "bad";
  cap.focus();
  cap.dispatch("input");
  form.dispatch("submit");
  const records = h.root.querySelectorAll(".full-record");
  records[0].open = true;
  records[1].open = false;
  h.fixture.runs[0].lifecycle_state = "running";
  h.render();
  assert.equal(h.root.querySelector("input[name=maxPoints]").value, "bad");
  assert.equal(h.document.activeElement.getAttribute("name"), "maxPoints");
  const updated = h.root.querySelectorAll(".full-record");
  assert.equal(updated[0].open, true);
  assert.equal(updated[1].open, false);
});

test("F04 pixel-only legacy geometry is not presented as raw metric evidence", () => {
  const h = harness();
  assert.equal(h.chartSeriesPointValues({ points: "10,20 30,40" }, "values").length, 0);
});

test("F14 empty detail query keeps resolution and range recovery controls reachable", () => {
  const h = harness();
  h.fixture.chart = { has_data: false, metric_name: "eval/score", series: [] };
  h.state.focusedSeriesControls.startStep = "5000";
  const timeline = h.renderLinePanel(h.fixture);
  assert.equal(timeline.querySelector("input[name=startStep]").value, "5000");
  assert.ok(timeline.querySelector("select[name=stepInterval]"));
});
