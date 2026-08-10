// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const root = document.getElementById("stellar-root");

const config = {
  target: root?.dataset.target || "",
  workspace: root?.dataset.workspace || "",
  project: root?.dataset.project || "",
  metric: root?.dataset.metric || "",
  snapshotPath: root?.dataset.snapshotPath || "/api/stellar/snapshot",
  seriesPath: root?.dataset.seriesPath || "/api/stellar/series",
  runsPath: root?.dataset.runsPath || "/api/stellar/runs",
  experimentsPath: root?.dataset.experimentsPath || "/api/stellar/experiments",
  source: root?.dataset.source || "",
  refreshInterval: root?.dataset.refreshInterval || root?.dataset.autoRefreshInterval || "",
};

const initialURL = new URL(window.location.href);
const hasInitialMetricParam = initialURL.searchParams.has("metric");
const hasInitialPinnedParam = initialURL.searchParams.has("pinned");
const autoSeriesDetail = initialURL.searchParams.get("detail") === "series";
const maxPinnedMetrics = 14;
const maxResearchPresetMetrics = 32;
const compactMetricCatalogThreshold = 40;
const collapsedMetricOptionsPerGroup = 18;
const maxStaticChartPointMarkers = 1500;
const chartHoverScrollIdleMs = 140;
const defaultChartRenderedPointBudget = 1600;
const focusedSeriesMaxPoints = 1200;
const focusedSeriesViewportMinPoints = 600;
const focusedSeriesViewportMaxPoints = 1500;
const focusedSeriesPointsPerPixel = 1.5;
const focusedSeriesMaxPointLimit = 12000;
const focusedSeriesResolutionOptions = [
  { value: "auto", label: "Auto resolution" },
  { value: "20", label: "Every 20 steps" },
  { value: "50", label: "Every 50 steps" },
  { value: "100", label: "Every 100 steps" },
  { value: "custom", label: "Custom" },
];
const defaultAutoRefreshIntervalMs = 30000;
const minAutoRefreshIntervalMs = 5000;
const runPageSize = 200;
const maxLoadedRuns = 1000;
const pinnedStoragePrefix = "stellar:pinnedMetrics:";
const dashboardSectionStoragePrefix = "stellar:dashboardSections:";
const runUpdatedFilterOptions = [
  { id: "", label: "Any update time" },
  { id: "1h", label: "Last hour", durationMs: 60 * 60 * 1000 },
  { id: "24h", label: "Last 24h", durationMs: 24 * 60 * 60 * 1000 },
  { id: "7d", label: "Last 7d", durationMs: 7 * 24 * 60 * 60 * 1000 },
  { id: "missing", label: "Missing timestamp" },
];
const runUpdatedSortOptions = [
  { id: "", label: "Default order" },
  { id: "desc", label: "Newest updated" },
  { id: "asc", label: "Oldest updated" },
];

const dashboardSectionCatalog = [
  {
    id: "charts",
    defaultTitle: "Scalar chart grid",
    defaultSubtitle: "Training, validation, and detection metrics are the primary dashboard surface.",
    render: renderMetricDashboardPanel,
  },
  {
    id: "timeline",
    defaultTitle: "Selected metric timeline",
    defaultSubtitle: "",
    render: renderLinePanel,
  },
  {
    id: "media",
    defaultTitle: "Output media",
    defaultSubtitle: "Model outputs as summary streams: browse by tag, run, and step when indexed metadata is available.",
    render: renderOutputMediaPanel,
  },
  {
    id: "errors",
    defaultTitle: "Error analysis",
    defaultSubtitle: "False positives/negatives, threshold behavior, calibration, and worst-label signals.",
    render: renderErrorAnalysisPanel,
  },
  {
    id: "labels",
    defaultTitle: "Per-label validation",
    defaultSubtitle: "Label cards support the main validation charts.",
    render: renderPerLabelQualityPanel,
  },
  {
    id: "repro",
    defaultTitle: "Reproducibility / evidence",
    defaultSubtitle: "Can another run be compared or reproduced?",
    render: renderReproducibilityPanel,
  },
  {
    id: "catalog",
    defaultTitle: "Metric catalog",
    defaultSubtitle: "Search and pin additional charts after the graph-first defaults.",
    render: renderMetricCatalogPanel,
  },
  {
    id: "runs",
    defaultTitle: "Run comparison",
    defaultSubtitle: "Metric values by run",
    render: renderResearchRunTablePanel,
  },
  {
    id: "evidence",
    defaultTitle: "Evidence browser",
    defaultSubtitle: "Configs, runtime context, artifacts, events, and notes",
    render: renderResearchEvidencePanel,
  },
];

const defaultDashboardSectionIDs = dashboardSectionCatalog.map((section) => section.id);

const state = {
  target: config.target,
  metric: hasInitialMetricParam ? config.metric : "",
  experimentSearch: text(initialURL.searchParams.get("experiment_q"), ""),
  landingProjectFilter: text(initialURL.searchParams.get("experiment_project"), ""),
  landingTagFilter: text(initialURL.searchParams.get("experiment_tag"), ""),
  experiments: [],
  experimentsLoading: false,
  experimentsError: "",
  experimentsLastCompletedAt: 0,
  expandedExperimentIDs: initialExpandedExperiments(),
  search: "",
  lifecycleFilter: normalizeLifecycleFilter(initialURL.searchParams.get("lifecycle")),
  runUpdatedFilter: normalizeRunUpdatedFilter(initialURL.searchParams.get("updated")),
  runUpdatedSort: normalizeRunUpdatedSort(initialURL.searchParams.get("updated_sort")),
  metricSearch: "",
  activeMetricFamily: "",
  group: "",
  selectedMetrics: initialPinnedMetrics(),
  metricSelectionInitialized: false,
  hiddenRuns: new Set(),
  additionalRuns: [],
  runSearchLimit: runPageSize,
  runSearchTruncated: false,
  runsLoading: false,
  runsError: "",
  snapshot: null,
  summarySnapshot: null,
  fullSnapshot: null,
  fullSnapshotLoading: false,
  fullSnapshotError: "",
  featuredSnapshots: new Map(),
  featuredErrors: new Map(),
  presetMetricSnapshots: new Map(),
  presetMetricErrors: new Map(),
  focusedSeriesCache: new Map(),
  focusedSeriesLoading: false,
  focusedSeriesError: "",
  focusedSeriesControls: initialFocusedSeriesControls(),
  metricSnapshotLoads: new Map(),
  outputMediaTag: text(initialURL.searchParams.get("media_tag"), ""),
  outputMediaRunID: text(initialURL.searchParams.get("media_run"), ""),
  outputMediaStep: text(initialURL.searchParams.get("media_step"), ""),
  dashboardSections: initialDashboardSections(),
  expandedMetricFamilies: new Set(),
  showAllPinnedCharts: false,
  autoRefresh: initialAutoRefreshState(),
};

publishVisualState("booting");

const featuredMetricCatalog = [
  {
    name: "eval/aime2025/pass@1",
    title: "AIME pass@1",
    family: "Quality",
    format: "percent",
  },
  {
    name: "eval/aime2025/avg@1",
    title: "AIME avg@1",
    family: "Quality",
    format: "percent",
  },
  {
    name: "eval/aime2025/completion_len/mean",
    title: "Completion length",
    family: "Behavior",
    format: "integer",
    unit: "tokens",
  },
  {
    name: "eval/aime2025/is_truncated/mean",
    title: "Truncation rate",
    family: "Behavior",
    format: "percent",
    goal: "minimize",
  },
  {
    name: "eval/aime2025/no_response/mean",
    title: "No-response rate",
    family: "Reliability",
    format: "percent",
    goal: "minimize",
  },
  {
    name: "eval/aime2025/no_response/count",
    title: "No responses",
    family: "Reliability",
    format: "integer",
    goal: "minimize",
  },
  {
    name: "eval/aime2025/failed_rollouts",
    title: "Failed rollouts",
    family: "Reliability",
    format: "integer",
    goal: "minimize",
  },
  {
    name: "final/score",
    title: "Final score (user-defined)",
    family: "Outcome",
    format: "score",
  },
  {
    name: "eval/score",
    title: "Eval score (user-defined)",
    family: "Outcome",
    format: "score",
  },
  {
    name: "final/reward",
    title: "Final reward",
    family: "Outcome",
    format: "score",
  },
  {
    name: "eval/reward",
    title: "Eval reward",
    family: "Outcome",
    format: "score",
  },
  {
    name: "final/mean_episode_return",
    title: "Final mean episode return",
    family: "Outcome",
    format: "integer",
  },
  {
    name: "eval/mean_episode_return",
    title: "Eval mean episode return",
    family: "Outcome",
    format: "integer",
  },
  {
    name: "final/return",
    title: "Final return",
    family: "Outcome",
    format: "integer",
  },
  {
    name: "eval/return",
    title: "Eval return",
    family: "Outcome",
    format: "integer",
  },
  {
    name: "final/success_rate",
    title: "Final success rate",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "eval/success_rate",
    title: "Eval success rate",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "final/win_rate",
    title: "Final win rate",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "eval/win_rate",
    title: "Eval win rate",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "final/pass_rate",
    title: "Final pass rate",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "eval/pass_rate",
    title: "Eval pass rate",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "final/exact_match",
    title: "Final exact match",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "eval/exact_match",
    title: "Eval exact match",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "final/accuracy",
    title: "Final accuracy",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "eval/accuracy",
    title: "Eval accuracy",
    family: "Outcome",
    format: "percent",
  },
  {
    name: "inference/decode_throughput_tps",
    title: "Decode throughput",
    family: "Serving",
    format: "rate",
    unit: "tok/s",
  },
  {
    name: "inference/completed_requests_per_s",
    title: "Completed req/s",
    family: "Serving",
    format: "small-rate",
  },
  {
    name: "inference/prefill_throughput_tps",
    title: "Prefill throughput",
    family: "Serving",
    format: "rate",
    unit: "tok/s",
  },
  {
    name: "eval/aime2025/time",
    title: "Eval wall time",
    family: "Efficiency",
    format: "duration",
    unit: "s",
    goal: "minimize",
  },
  {
    name: "system/gpu_utilization",
    title: "GPU utilization",
    family: "Systems",
    format: "percent-100",
    unit: "%",
  },
  {
    name: "train/loss",
    title: "Train loss",
    family: "Optimization",
    format: "loss",
    goal: "minimize",
  },
  {
    name: "train/lr",
    title: "Learning rate",
    family: "Optimization",
    format: "small-rate",
  },
  {
    name: "train/grad_norm",
    title: "Gradient norm",
    family: "Optimization",
    format: "score",
    goal: "minimize",
  },
  {
    name: "train/step_time_s",
    title: "Step time",
    family: "Optimization",
    format: "duration",
    unit: "s",
    goal: "minimize",
  },
  {
    name: "train/examples_seen",
    title: "Examples seen",
    family: "Throughput",
    format: "integer",
    unit: "examples",
  },
  {
    name: "train/input_tokens",
    title: "Input tokens",
    family: "Throughput",
    format: "integer",
    unit: "tokens",
  },
  {
    name: "train/tokens",
    title: "Tokens",
    family: "Throughput",
    format: "integer",
    unit: "tokens",
  },
  {
    name: "gpu/memory_allocated_gb",
    title: "GPU allocated",
    family: "Systems",
    format: "score",
    unit: "GB",
  },
  {
    name: "gpu/memory_reserved_gb",
    title: "GPU reserved",
    family: "Systems",
    format: "score",
    unit: "GB",
  },
  {
    name: "gpu/max_memory_allocated_gb",
    title: "GPU max allocated",
    family: "Systems",
    format: "score",
    unit: "GB",
  },
  {
    name: "checkpoint/file_count",
    title: "Checkpoint files",
    family: "Checkpoint",
    format: "integer",
    unit: "files",
  },
  {
    name: "checkpoint/bytes",
    title: "Checkpoint bytes",
    family: "Checkpoint",
    format: "integer",
    unit: "bytes",
  },
  {
    name: "inference/time_s",
    title: "Inference time",
    family: "Model diagnostics",
    format: "duration",
    unit: "s",
    goal: "minimize",
  },
  {
    name: "eval/macro_auprc",
    title: "Macro AUPRC",
    family: "Validation quality",
    format: "score",
  },
  {
    name: "final/macro_auprc",
    title: "Final macro AUPRC",
    family: "Validation quality",
    format: "score",
  },
  {
    name: "detect/macro_auprc",
    title: "Detection macro AUPRC",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "eval/macro_auroc",
    title: "Macro AUROC",
    family: "Validation quality",
    format: "score",
  },
  {
    name: "final/macro_auroc",
    title: "Final macro AUROC",
    family: "Validation quality",
    format: "score",
  },
  {
    name: "detect/macro_auroc",
    title: "Detection macro AUROC",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "eval/macro_f1",
    title: "Macro F1",
    family: "Validation quality",
    format: "score",
  },
  {
    name: "final/macro_f1",
    title: "Final macro F1",
    family: "Validation quality",
    format: "score",
  },
  {
    name: "eval/brier",
    title: "Brier score",
    family: "Calibration",
    format: "score",
    goal: "minimize",
  },
  {
    name: "final/brier",
    title: "Final Brier score",
    family: "Calibration",
    format: "score",
    goal: "minimize",
  },
  {
    name: "eval/ece",
    title: "ECE",
    family: "Calibration",
    format: "score",
    goal: "minimize",
  },
  {
    name: "final/ece",
    title: "Final ECE",
    family: "Calibration",
    format: "score",
    goal: "minimize",
  },
  {
    name: "detect/macro_sensitivity",
    title: "Macro sensitivity",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "detect/macro_specificity",
    title: "Macro specificity",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "detect/macro_precision",
    title: "Macro precision",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "detect/macro_f1",
    title: "Detection macro F1",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "detect/macro_accuracy",
    title: "Detection macro accuracy",
    family: "Error analysis",
    format: "score",
  },
  {
    name: "detect/accuracy",
    title: "Detection accuracy",
    family: "Error analysis",
    format: "score",
  },
];

const headlinePrimaryMetricOrder = [
  "final/macro_auprc",
  "detect/macro_auprc",
  "eval/macro_auprc",
  "final/auprc",
  "detect/auprc",
  "eval/auprc",
  "final/mean_episode_return",
  "eval/mean_episode_return",
  "final/reward",
  "eval/reward",
  "final/return",
  "eval/return",
  "final/success_rate",
  "eval/success_rate",
  "final/win_rate",
  "eval/win_rate",
  "final/pass_rate",
  "eval/pass_rate",
  "final/pass@1",
  "eval/pass@1",
  "eval/aime2025/pass@1",
  "final/exact_match",
  "eval/exact_match",
  "final/accuracy",
  "eval/accuracy",
  "final/score",
  "eval/score",
];

const evalCurveMetricOrder = [
  "eval/macro_auprc",
  "eval/auprc",
  "eval/mean_episode_return",
  "eval/reward",
  "eval/return",
  "eval/success_rate",
  "eval/win_rate",
  "eval/pass_rate",
  "eval/pass@1",
  "eval/aime2025/pass@1",
  "eval/exact_match",
  "eval/accuracy",
  "eval/score",
];

const detectionSummaryMetricOrder = [
  "detect/macro_auprc",
  "detect/macro_auroc",
  "detect/macro_sensitivity",
  "detect/macro_specificity",
  "detect/macro_precision",
  "detect/macro_f1",
  "detect/macro_accuracy",
  "detect/accuracy",
];

const graphDefaultMetricOrder = [
  "train/loss",
  "train/lr",
  "eval/mean_episode_return",
  "eval/reward",
  "eval/return",
  "eval/success_rate",
  "eval/win_rate",
  "eval/pass_rate",
  "eval/accuracy",
  "eval/macro_auprc",
  "eval/macro_auroc",
  "eval/macro_f1",
  "eval/brier",
  "eval/ece",
  "eval/score",
  "detect/macro_auprc",
  "detect/macro_auroc",
  "detect/macro_sensitivity",
  "detect/macro_specificity",
  "detect/macro_precision",
  "detect/macro_f1",
  "detect/macro_accuracy",
];

function text(value, fallback = "--") {
  if (value === null || value === undefined || value === "") {
    return fallback;
  }
  return String(value);
}

function focusedSeriesPointBudget(controls = state.focusedSeriesControls) {
  const override = text(controls?.maxPoints, "").trim();
  if (override) {
    return parseIntegerControl(override, "max points");
  }
  const chart = document.querySelector(".focused-chart-stack .stellar-line-chart");
  const width = numberValue(chart?.getBoundingClientRect?.().width) || 800;
  return Math.min(
    focusedSeriesViewportMaxPoints,
    Math.max(focusedSeriesViewportMinPoints, Math.round(width * focusedSeriesPointsPerPixel)),
  );
}

async function copyEvidenceText(value) {
  try {
    await navigator.clipboard.writeText(text(value, ""));
  } catch (error) {
    state.focusedSeriesError = `Copy failed: ${error.message || error}`;
    render();
  }
}

function renderRawMetricQueryAction(chart) {
  const query = text(chart?.raw_query, "");
  if (!query || chart?.raw_query_source !== "kusto") {
    return null;
  }
  return h("button", {
    type: "button",
    class: "zoom-pill",
    title: "Copy the read-only Kusto source-row query for this metric and selected step range.",
    onclick: () => copyEvidenceText(query),
  }, "Copy raw KQL");
}

function runContextSummary(run) {
  const fields = [
    run.project && `project ${run.project}`,
    run.workspace_id && `workspace ${run.workspace_id}`,
    run.cluster && `cluster ${run.cluster}`,
    run.run_group_id && `group ${run.run_group_id}`,
    run.source && `source ${run.source}`,
  ].filter(Boolean);
  return fields.length ? fields.join(" \u00b7 ") : "context unavailable";
}

function list(value) {
  return Array.isArray(value) ? value : [];
}

function initialAutoRefreshState() {
  const intervalMs = parseAutoRefreshInterval();
  return {
    enabled: intervalMs > 0,
    intervalMs,
    timer: 0,
    started: false,
    inFlight: false,
    lastStartedAt: 0,
    lastCompletedAt: 0,
    lastError: "",
    pausedReason: "",
  };
}

function parseAutoRefreshInterval() {
  const setting = autoRefreshIntervalSetting();
  const value = text(setting.value, "").trim().toLowerCase();
  if (!value) {
    return defaultAutoRefreshIntervalMs;
  }
  if (["0", "off", "false", "disabled", "none"].includes(value)) {
    return 0;
  }
  const parsed = value.match(/^([0-9]+(?:\.[0-9]+)?)\s*(ms|msec|millisecond|milliseconds|s|sec|second|seconds|m|min|minute|minutes)?$/);
  if (!parsed) {
    console.warn(`Ignoring invalid Stellar auto-refresh interval ${JSON.stringify(setting.value)}; using ${defaultAutoRefreshIntervalMs}ms.`);
    return defaultAutoRefreshIntervalMs;
  }
  let amount = Number(parsed[1]);
  let unit = parsed[2] || setting.defaultUnit;
  if (!parsed[2] && setting.defaultUnit === "s" && amount >= 1000) {
    unit = "ms";
  }
  if (["m", "min", "minute", "minutes"].includes(unit)) {
    amount *= 60000;
  } else if (["s", "sec", "second", "seconds"].includes(unit)) {
    amount *= 1000;
  }
  if (!Number.isFinite(amount) || amount <= 0) {
    return defaultAutoRefreshIntervalMs;
  }
  return Math.max(minAutoRefreshIntervalMs, Math.round(amount));
}

function autoRefreshIntervalSetting() {
  if (initialURL.searchParams.has("refresh_ms")) {
    return { value: initialURL.searchParams.get("refresh_ms"), defaultUnit: "ms" };
  }
  if (initialURL.searchParams.has("refresh")) {
    return { value: initialURL.searchParams.get("refresh"), defaultUnit: "s" };
  }
  if (initialURL.searchParams.has("auto_refresh")) {
    return { value: initialURL.searchParams.get("auto_refresh"), defaultUnit: "s" };
  }
  return { value: config.refreshInterval, defaultUnit: "ms" };
}

function formatRefreshInterval(ms) {
  if (!ms) {
    return "off";
  }
  if (ms % 60000 === 0) {
    return `${ms / 60000}m`;
  }
  if (ms % 1000 === 0) {
    return `${ms / 1000}s`;
  }
  return `${ms}ms`;
}

function formatClockTime(timestamp) {
  if (!timestamp) {
    return "not updated";
  }
  return new Date(timestamp).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function formatShortDateTime(timestamp) {
  if (!timestamp) {
    return "not updated";
  }
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) {
    return "not updated";
  }
  return date.toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatShortClockTime(timestamp) {
  if (!timestamp) {
    return "not updated";
  }
  return new Date(timestamp).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
}

function initialPinnedMetrics() {
  if (hasInitialPinnedParam) {
    return normalizeMetricList(initialURL.searchParams.get("pinned"));
  }
  if (!hasInitialMetricParam) {
    if (!config.target) {
      return [];
    }
    return loadPinnedMetrics(config.target);
  }
  return [];
}

function initialFocusedSeriesControls() {
  const stepInterval = normalizeStepIntervalControl(initialURL.searchParams.get("step_interval"));
  return {
    runID: text(initialURL.searchParams.get("run_id"), ""),
    startStep: text(initialURL.searchParams.get("start_step"), ""),
    endStep: text(initialURL.searchParams.get("end_step"), ""),
    stepInterval,
    customStepInterval: customStepIntervalValue(stepInterval),
    maxPoints: text(initialURL.searchParams.get("max_points"), ""),
  };
}

function normalizeMetricList(value) {
  const raw = Array.isArray(value) ? value : text(value, "").split(",");
  const seen = new Set();
  const metrics = [];
  let truncated = false;
  for (const item of raw) {
    const name = text(item, "").trim();
    if (!name || seen.has(name)) {
      continue;
    }
    if (metrics.length >= maxPinnedMetrics) {
      truncated = true;
      break;
    }
    seen.add(name);
    metrics.push(name);
  }
  if (truncated) {
    console.warn(`Stellar pinned metrics are capped at ${maxPinnedMetrics}; extra URL/localStorage pins were ignored.`);
  }
  return metrics;
}

function pinnedStorageKey(target) {
  return `${pinnedStoragePrefix}${text(target, "default")}`;
}

function dashboardSectionStorageKey(target) {
  return `${dashboardSectionStoragePrefix}${text(target, "default")}`;
}

function loadPinnedMetrics(target) {
  try {
    return normalizeMetricList(JSON.parse(window.localStorage.getItem(pinnedStorageKey(target)) || "[]"));
  } catch {
    return [];
  }
}

function savePinnedMetrics() {
  try {
    window.localStorage.setItem(pinnedStorageKey(state.target), JSON.stringify(state.selectedMetrics));
  } catch {
    // Private browsing or locked-down contexts may reject localStorage.
  }
}

function initialDashboardSections() {
  return dashboardSectionsFromURL(initialURL, config.target);
}

function dashboardSectionsFromURL(url, target) {
  const saved = loadDashboardSections(target);
  const fromURL = url.searchParams.has("sections");
  const visibleFromURL = parseDashboardSectionList(url.searchParams.get("sections"));
  const order = fromURL
    ? mergeDashboardSectionOrder(visibleFromURL)
    : mergeDashboardSectionOrder(saved.order || defaultDashboardSectionIDs);
  const hidden = new Set(saved.hidden || []);
  if (fromURL) {
    hidden.clear();
    for (const id of defaultDashboardSectionIDs) {
      if (!visibleFromURL.includes(id)) {
        hidden.add(id);
      }
    }
  }
  const titles = { ...(saved.titles || {}) };
  const subtitles = { ...(saved.subtitles || {}) };
  for (const section of dashboardSectionCatalog) {
    const title = url.searchParams.get(`section.${section.id}.title`);
    const subtitle = url.searchParams.get(`section.${section.id}.subtitle`);
    if (title !== null) {
      setDashboardSectionText(titles, section.id, title, section.defaultTitle);
    }
    if (subtitle !== null) {
      setDashboardSectionText(subtitles, section.id, subtitle, section.defaultSubtitle);
    }
  }
  return normalizeDashboardSectionState({ order, hidden: Array.from(hidden), titles, subtitles });
}

function loadDashboardSections(target) {
  try {
    const saved = JSON.parse(window.localStorage.getItem(dashboardSectionStorageKey(target)) || "{}");
    return saved && typeof saved === "object" ? saved : {};
  } catch {
    return {};
  }
}

function saveDashboardSections() {
  try {
    window.localStorage.setItem(dashboardSectionStorageKey(state.target), JSON.stringify({
      order: state.dashboardSections.order,
      hidden: Array.from(state.dashboardSections.hidden),
      titles: state.dashboardSections.titles,
      subtitles: state.dashboardSections.subtitles,
    }));
  } catch {
    // Private browsing or locked-down contexts may reject localStorage.
  }
}

function parseDashboardSectionList(value) {
  return text(value, "").split(",").map((item) => normalizeDashboardSectionID(item)).filter(Boolean);
}

function normalizeDashboardSectionID(value) {
  const id = text(value, "").trim().toLowerCase().replace(/[^a-z0-9_-]+/g, "-");
  return dashboardSectionByID(id) ? id : "";
}

function dashboardSectionByID(id) {
  return dashboardSectionCatalog.find((section) => section.id === id);
}

function mergeDashboardSectionOrder(order) {
  const seen = new Set();
  const normalized = [];
  for (const id of order || []) {
    const sectionID = normalizeDashboardSectionID(id);
    if (sectionID && !seen.has(sectionID)) {
      normalized.push(sectionID);
      seen.add(sectionID);
    }
  }
  for (const id of defaultDashboardSectionIDs) {
    if (!seen.has(id)) {
      normalized.push(id);
    }
  }
  return normalized;
}

function normalizeDashboardSectionState(value) {
  const order = mergeDashboardSectionOrder(value.order || defaultDashboardSectionIDs);
  const hidden = new Set((value.hidden || []).map(normalizeDashboardSectionID).filter(Boolean));
  const titles = {};
  const subtitles = {};
  for (const section of dashboardSectionCatalog) {
    setDashboardSectionText(titles, section.id, value.titles?.[section.id], section.defaultTitle);
    setDashboardSectionText(subtitles, section.id, value.subtitles?.[section.id], section.defaultSubtitle);
  }
  return { order, hidden, titles, subtitles };
}

function setDashboardSectionText(target, id, value, fallback) {
  const normalized = text(value, "").trim();
  if (normalized && normalized !== text(fallback, "")) {
    target[id] = normalized;
  } else {
    delete target[id];
  }
}

function dashboardSectionTitle(id) {
  const section = dashboardSectionByID(id);
  return state.dashboardSections.titles[id] || section?.defaultTitle || id;
}

function dashboardSectionSubtitle(id, fallback = "") {
  const section = dashboardSectionByID(id);
  if (Object.prototype.hasOwnProperty.call(state.dashboardSections.subtitles, id)) {
    return state.dashboardSections.subtitles[id];
  }
  return fallback || section?.defaultSubtitle || "";
}

function visibleDashboardSectionIDs() {
  return state.dashboardSections.order.filter((id) => !state.dashboardSections.hidden.has(id));
}

function writeDashboardSectionURL(url) {
  url.searchParams.set("sections", visibleDashboardSectionIDs().join(","));
  for (const section of dashboardSectionCatalog) {
    const title = state.dashboardSections.titles[section.id] || "";
    const subtitle = state.dashboardSections.subtitles[section.id] || "";
    setOptionalSearchParam(url, `section.${section.id}.title`, title);
    setOptionalSearchParam(url, `section.${section.id}.subtitle`, subtitle);
  }
}

function setOptionalSearchParam(url, name, value) {
  const normalized = text(value, "").trim();
  if (normalized) {
    url.searchParams.set(name, normalized);
  } else {
    url.searchParams.delete(name);
  }
}

function h(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === false || value === null || value === undefined) {
      continue;
    }
    if (key === "class") {
      node.className = value;
    } else if (key === "dataset") {
      Object.assign(node.dataset, value);
    } else if (key.startsWith("on") && typeof value === "function") {
      node.addEventListener(key.slice(2).toLowerCase(), value);
    } else {
      node.setAttribute(key, value === true ? "" : String(value));
    }
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) {
      continue;
    }
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function s(tag, attrs = {}, ...children) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === false || value === null || value === undefined) {
      continue;
    }
    node.setAttribute(key, String(value));
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) {
      continue;
    }
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function clear(node) {
  while (node.firstChild) {
    node.removeChild(node.firstChild);
  }
}

function traceToken(value) {
  return text(value, "default").replace(/[^A-Za-z0-9_.-]+/g, "_").slice(0, 120);
}

function traceMark(name) {
  if (!window.performance?.mark) {
    return "";
  }
  const mark = `${name}:${performance.now().toFixed(3)}`;
  performance.mark(mark);
  return mark;
}

function traceMeasure(name, startMark) {
  if (!startMark || !window.performance?.measure) {
    return;
  }
  try {
    performance.measure(name, startMark);
  } catch {
    // Unsupported or cleared user-timing marks should not break the dashboard.
  }
}

function snapshotURL(metric = state.metric, options = {}) {
  const mode = typeof options === "string" ? options : options.mode || "";
  const url = new URL(config.snapshotPath, window.location.origin);
  url.searchParams.set("target", state.target);
  if (metric) {
    url.searchParams.set("metric", metric);
  }
  if (mode) {
    url.searchParams.set("mode", mode);
  }
  if (options.includeStatic === false) {
    url.searchParams.set("include_static", "false");
  }
  applySourceParam(url);
  return url;
}

function runsURL(limit) {
  const url = new URL(config.runsPath, window.location.origin);
  url.searchParams.set("target", state.target);
  url.searchParams.set("limit", String(limit));
  applySourceParam(url);
  return url;
}

function experimentsURL(options = {}) {
  const url = new URL(config.experimentsPath, window.location.origin);
  const query = text(options.query ?? state.experimentSearch, "").trim();
  if (query) {
    url.searchParams.set("q", query);
  }
  const project = text(options.project ?? state.landingProjectFilter, "").trim();
  if (project) {
    url.searchParams.set("project", project);
  }
  const tag = text(options.tag ?? state.landingTagFilter, "").trim();
  if (tag) {
    url.searchParams.append("tag", tag);
  }
  url.searchParams.set("limit", String(options.limit || 100));
  applySourceParam(url);
  return url;
}

function seriesURL(metric = state.metric, options = {}) {
  const url = new URL(config.seriesPath, window.location.origin);
  url.searchParams.set("target", state.target);
  url.searchParams.set("metric", metric);
  url.searchParams.set("max_points", String(options.maxPoints || focusedSeriesMaxPoints));
  if (options.runID) {
    url.searchParams.set("run_id", options.runID);
  }
  if (options.startStep !== undefined && options.startStep !== null) {
    url.searchParams.set("start_step", String(options.startStep));
  }
  if (options.endStep !== undefined && options.endStep !== null) {
    url.searchParams.set("end_step", String(options.endStep));
  }
  if (options.stepInterval) {
    url.searchParams.set("step_interval", String(options.stepInterval));
  }
  applySourceParam(url);
  return url;
}

function applySourceParam(url) {
  const workspace = text(config.workspace, "").trim();
  if (workspace) {
    url.searchParams.set("workspace", workspace);
  }
  const project = text(config.project, "").trim();
  if (project) {
    url.searchParams.set("project", project);
  }
  const source = text(config.source, "").trim();
  if (source) {
    url.searchParams.set("source", source);
  }
}

function usesRemoteExperimentSource() {
  const source = text(config.source, "").trim().toLowerCase();
  return source === "kusto" || source === "auto";
}

async function fetchExperiments(options = {}) {
  const response = await fetch(experimentsURL(options));
  if (!response.ok) {
    throw new Error(`Experiment search failed (${response.status})`);
  }
  return response.json();
}

async function fetchRuns(limit) {
  const response = await fetch(runsURL(limit), { headers: { Accept: "application/json" } });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(body.detail || body.error || `Run search failed (${response.status})`);
  }
  return body;
}

async function refreshExperiments(options = {}) {
  if (state.experimentsLoading) {
    return;
  }
  state.experimentsLoading = true;
  state.experimentsError = "";
  try {
    const result = await fetchExperiments(options);
    const experiments = list(result.experiments);
    if (experiments.length === 0 && state.experiments.length > 0) {
      state.experimentsError = "Experiment refresh returned no results; showing previously loaded experiments.";
    } else {
      state.experiments = experiments;
      state.experimentsLastCompletedAt = Date.now();
    }
  } catch (error) {
    state.experimentsError = error.message || String(error);
  } finally {
    state.experimentsLoading = false;
    if (options.render && state.snapshot) {
      render({ focusExperimentSearch: options.focusExperimentSearch });
    } else if (options.render && !state.target) {
      updateURL();
      renderLanding({ focusExperimentSearch: options.focusExperimentSearch });
    }
  }
}

function updateURL(options = {}) {
  const url = new URL(window.location.href);
  if (!state.target) {
    url.searchParams.delete("target");
    url.searchParams.set("ui", "stellar");
    setOptionalSearchParam(url, "experiment_q", state.experimentSearch);
    setOptionalSearchParam(url, "experiment_project", state.landingProjectFilter);
    setOptionalSearchParam(url, "experiment_tag", state.landingTagFilter);
    setOptionalSearchParam(url, "experiments", Array.from(state.expandedExperimentIDs).join(","));
    for (const param of ["metric", "pinned", "run_id", "lifecycle", "updated", "updated_sort", "start_step", "end_step", "step_interval", "media_tag", "media_run", "media_step", "max_points"]) {
      url.searchParams.delete(param);
    }
    for (const param of Array.from(url.searchParams.keys())) {
      if (param === "sections" || param.startsWith("section.")) {
        url.searchParams.delete(param);
      }
    }
    writeURL(url, options.history);
    return;
  }
  url.searchParams.set("target", state.target);
  url.searchParams.set("ui", "stellar");
  setOptionalSearchParam(url, "experiment_q", state.experimentSearch);
  setOptionalSearchParam(url, "experiment_project", state.landingProjectFilter);
  setOptionalSearchParam(url, "experiment_tag", state.landingTagFilter);
  setOptionalSearchParam(url, "experiments", Array.from(state.expandedExperimentIDs).join(","));
  if (state.metric) {
    url.searchParams.set("metric", state.metric);
  } else {
    url.searchParams.delete("metric");
  }
  if (state.selectedMetrics.length) {
    url.searchParams.set("pinned", state.selectedMetrics.join(","));
  } else {
    url.searchParams.delete("pinned");
  }
  setOptionalSearchParam(url, "run_id", state.focusedSeriesControls.runID);
  setOptionalSearchParam(url, "lifecycle", state.lifecycleFilter);
  setOptionalSearchParam(url, "updated", state.runUpdatedFilter);
  setOptionalSearchParam(url, "updated_sort", state.runUpdatedSort);
  setOptionalSearchParam(url, "start_step", state.focusedSeriesControls.startStep);
  setOptionalSearchParam(url, "end_step", state.focusedSeriesControls.endStep);
  if (state.focusedSeriesControls.stepInterval && state.focusedSeriesControls.stepInterval !== "auto") {
    url.searchParams.set("step_interval", state.focusedSeriesControls.stepInterval);
  } else {
    url.searchParams.delete("step_interval");
  }
  setOptionalSearchParam(url, "media_tag", state.outputMediaTag);
  setOptionalSearchParam(url, "media_run", state.outputMediaRunID);
  setOptionalSearchParam(url, "media_step", state.outputMediaStep);
  writeDashboardSectionURL(url);
  if (text(state.focusedSeriesControls.maxPoints, "")) {
    url.searchParams.set("max_points", text(state.focusedSeriesControls.maxPoints));
  } else {
    url.searchParams.delete("max_points");
  }
  writeURL(url, options.history);
  savePinnedMetrics();
  saveDashboardSections();
}

function writeURL(url, historyMode = "replace") {
  const next = url.toString();
  if (historyMode === "push" && next !== window.location.href) {
    window.history.pushState(null, "", url);
    return;
  }
  window.history.replaceState(null, "", url);
}

async function fetchSnapshotFor(metric, options = {}) {
  const mode = options.mode || "full";
  const mark = traceMark(`stellar.snapshot.${mode}.${traceToken(metric || "default")}`);
  try {
    const response = await fetch(snapshotURL(metric, options), { headers: { Accept: "application/json" } });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(body.detail || body.error || `Snapshot request failed with ${response.status}`);
    }
    return body;
  } finally {
    traceMeasure(`stellar.snapshot.${mode}.${traceToken(metric || "default")}`, mark);
  }
}

async function fetchSeriesFor(metric, options = {}) {
  const mark = traceMark(`stellar.series.${traceToken(metric || "default")}`);
  try {
    const response = await fetch(seriesURL(metric, options), { headers: { Accept: "application/json" } });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(body.detail || body.error || `Series request failed with ${response.status}`);
    }
    return body;
  } finally {
    traceMeasure(`stellar.series.${traceToken(metric || "default")}`, mark);
  }
}

async function fetchSnapshot(options = {}) {
  const autoRefresh = options.autoRefresh === true;
  const shouldRender = options.render !== false;
  const overallMark = traceMark("stellar.fetchSnapshot.total");
  if (!options.silent && !autoRefresh) {
    renderLoading();
  }
  if (!autoRefresh) {
    state.additionalRuns = [];
    state.runSearchLimit = runPageSize;
    state.runSearchTruncated = false;
    state.runsLoading = false;
    state.runsError = "";
    state.summarySnapshot = null;
    state.fullSnapshot = null;
    state.fullSnapshotLoading = false;
    state.fullSnapshotError = "";
    state.focusedSeriesError = "";
    state.featuredSnapshots.clear();
    state.featuredErrors.clear();
    state.presetMetricSnapshots.clear();
    state.presetMetricErrors.clear();
  }

  const focusedMetric = state.metric;
  const summaryMark = traceMark("stellar.fetchSnapshot.summary");
  const primary = await fetchSnapshotFor(focusedMetric, { mode: "summary" });
  traceMeasure("stellar.fetchSnapshot.summary", summaryMark);
  const highMetricCatalog = list(primary.metric_options).length > compactMetricCatalogThreshold;
  const metricMode = highMetricCatalog || autoRefresh ? "metric" : "";
  const primaryMetric = primary.chart?.metric_name || focusedMetric;
  if (autoRefresh) {
    state.featuredErrors.clear();
    state.presetMetricErrors.clear();
  }
  state.summarySnapshot = primary;
  state.snapshot = primary;
  if (!autoRefresh) {
    state.runSearchLimit = Math.max(runPageSize, list(primary.runs).length);
    state.runSearchTruncated = list(primary.runs).length >= runPageSize;
  }
  ensureSelectedMetrics(primary, { preserveSelection: autoRefresh });

  const selected = selectedMetricSpecs(primary);
  const selectedMetricNames = normalizeMetricList([
    focusedMetric,
    ...selected.map((spec) => spec.name),
  ]).filter((name) => availableMetricNames(primary).has(name));
  const selectedMetricSet = new Set(selectedMetricNames);
  const presetMetricNames = researcherPresetMetricNames(primary)
    .filter((name) => availableMetricNames(primary).has(name) && !selectedMetricSet.has(name));
  const metricsToLoad = normalizeMetricList([
    ...selectedMetricNames,
    ...presetMetricNames,
  ]);
  const immediateMetricNames = immediatelyVisibleMetricNames(selectedMetricNames)
    .filter((name) => availableMetricNames(primary).has(name));
  const immediateMetricSet = new Set(immediateMetricNames);
  const backgroundMetricNames = metricsToLoad.filter((name) => !immediateMetricSet.has(name));
  const metricsMark = traceMark("stellar.fetchSnapshot.metricSnapshots");
  await loadMetricSnapshots(immediateMetricNames, {
    selectedMetricSet,
    summary: primary,
    mode: metricMode,
    includeStatic: !autoRefresh && !highMetricCatalog,
    force: autoRefresh,
  });
  traceMeasure("stellar.fetchSnapshot.metricSnapshots", metricsMark);
  const panelMetric = state.metric || state.selectedMetrics[0] || primaryMetric;
  setPanelSnapshotForMetric(panelMetric, primary);
  if (!autoRefresh) {
    applyCachedFocusedSeries(panelMetric);
  }

  updateURL();
  await refreshExperiments({ query: state.experimentSearch, render: false });
  if (backgroundMetricNames.length) {
    const loadOptions = {
      selectedMetricSet,
      summary: primary,
      mode: metricMode,
      includeStatic: !autoRefresh && !highMetricCatalog,
      force: autoRefresh,
      render: shouldRender,
    };
    if (autoRefresh || options.deferBackground === false) {
      await loadMetricSnapshots(backgroundMetricNames, loadOptions);
    } else {
      loadMetricSnapshotsInBackground(backgroundMetricNames, loadOptions);
    }
  }
  if (options.loadAutoSeriesDetail !== false && autoSeriesDetail && state.metric) {
    await loadFocusedSeriesDetail();
  }
  if (!autoRefresh) {
    state.autoRefresh.lastError = criticalRefreshError();
    if (!state.autoRefresh.lastError) {
      state.autoRefresh.lastCompletedAt = Date.now();
    }
  }
  if (shouldRender) {
    render();
  }
  traceMeasure("stellar.fetchSnapshot.total", overallMark);
}

function metricSnapshotLoadKey(metricName, options = {}) {
  return [
    text(options.target || state.target, ""),
    text(metricName, ""),
    text(options.mode, ""),
    options.includeStatic === false ? "no-static" : "static",
  ].join("|");
}

function metricSnapshotRequestMode(summary = state.summarySnapshot || state.snapshot) {
  return list(summary?.metric_options).length > compactMetricCatalogThreshold ? "metric" : "";
}

function immediatelyVisibleMetricNames(metricNames) {
  const visible = state.showAllPinnedCharts ? metricNames : metricNames.slice(0, 4);
  return normalizeMetricList([state.metric, ...visible]);
}

function visiblePinnedMetricNames(snapshot = state.summarySnapshot || state.snapshot) {
  const available = availableMetricNames(snapshot);
  const selected = normalizeMetricList(state.selectedMetrics).filter((name) => available.has(name));
  return immediatelyVisibleMetricNames(selected).filter((name) => available.has(name));
}

async function loadMetricSnapshots(metricNames, options = {}) {
  await Promise.all(normalizeMetricList(metricNames).map((name) => loadMetricSnapshot(name, options)));
}

function loadMetricSnapshotsInBackground(metricNames, options = {}) {
  const names = normalizeMetricList(metricNames);
  if (!names.length) {
    return;
  }
  const target = state.target;
  Promise.all(names.map((name) => loadMetricSnapshot(name, options))).then(() => {
    if (options.render !== false && state.target === target && state.snapshot) {
      render();
    }
  }).catch((error) => {
    console.error("Stellar metric snapshots failed to load", error);
    if (options.render !== false && state.target === target && state.snapshot) {
      render();
    }
  });
}

async function loadMetricSnapshot(metricName, options = {}) {
  metricName = text(metricName, "").trim();
  if (!metricName) {
    return;
  }
  const selectedMetricSet = options.selectedMetricSet || new Set(normalizeMetricList(state.selectedMetrics));
  const selectedMetric = selectedMetricSet.has(metricName);
  if (!options.force && selectedMetric && state.presetMetricSnapshots.has(metricName)) {
    state.featuredSnapshots.set(metricName, state.presetMetricSnapshots.get(metricName));
    state.presetMetricErrors.delete(metricName);
    return;
  }
  if (!options.force && snapshotForMetric(metricName)) {
    return;
  }
  const summary = options.summary || state.summarySnapshot || state.snapshot;
  const requestTarget = state.target;
  const request = {
    mode: options.mode ?? metricSnapshotRequestMode(summary),
    includeStatic: options.includeStatic,
    target: requestTarget,
  };
  const loadKey = metricSnapshotLoadKey(metricName, request);
  const pendingLoad = state.metricSnapshotLoads.get(loadKey);
  if (pendingLoad) {
    await pendingLoad.promise;
    return;
  }
  const loadPromise = (async () => {
    const errors = selectedMetric ? state.featuredErrors : state.presetMetricErrors;
    errors.delete(metricName);
    try {
      const snapshot = await fetchSnapshotFor(metricName, request);
      if (state.target !== requestTarget) {
        return;
      }
      const hydratedSnapshot = snapshotWithSummaryDefaults(snapshot, summary);
      const loadedMetric = hydratedSnapshot.chart?.metric_name || metricName;
      const cache = selectedMetric ? state.featuredSnapshots : state.presetMetricSnapshots;
      cache.set(loadedMetric, hydratedSnapshot);
      if (loadedMetric !== metricName) {
        cache.set(metricName, hydratedSnapshot);
      }
      errors.delete(loadedMetric);
      if (state.metric === loadedMetric || state.metric === metricName) {
        setPanelSnapshotForMetric(loadedMetric, summary);
      }
    } catch (error) {
      errors.set(metricName, error.message || String(error));
    }
  })();
  const loadEntry = { promise: loadPromise };
  state.metricSnapshotLoads.set(loadKey, loadEntry);
  await loadEntry.promise;
  if (state.metricSnapshotLoads.get(loadKey) === loadEntry) {
    state.metricSnapshotLoads.delete(loadKey);
  }
}

function loadVisibleMetricSnapshots() {
  const summary = state.summarySnapshot || state.snapshot;
  if (!summary) {
    return;
  }
  loadMetricSnapshotsInBackground(visiblePinnedMetricNames(summary), {
    selectedMetricSet: new Set(normalizeMetricList(state.selectedMetrics)),
    summary,
    mode: metricSnapshotRequestMode(summary),
    includeStatic: false,
  });
}

function setPanelSnapshotForMetric(metricName, summary = state.summarySnapshot || state.snapshot) {
  metricName = text(metricName, "").trim();
  if (!metricName) {
    state.snapshot = summary || state.snapshot;
    return;
  }
  const snapshot = snapshotForMetric(metricName);
  state.snapshot = snapshot ? snapshotWithSummaryDefaults(snapshot, summary) : summary || state.snapshot;
}

function updateMetricSelectionInPlace() {
  state.metricSelectionInitialized = true;
  const panelMetric = state.metric || state.selectedMetrics[0] || "";
  setPanelSnapshotForMetric(panelMetric);
  applyCachedFocusedSeries(panelMetric);
  updateURL();
  render();
  loadVisibleMetricSnapshots();
}

function startAutoRefresh() {
  if (!state.autoRefresh.enabled || state.autoRefresh.started) {
    return;
  }
  state.autoRefresh.started = true;
  document.addEventListener("visibilitychange", handleAutoRefreshVisibilityChange);
  window.addEventListener("beforeunload", stopAutoRefresh);
  scheduleAutoRefresh();
}

function stopAutoRefresh() {
  clearAutoRefreshTimer();
}

function clearAutoRefreshTimer() {
  if (!state.autoRefresh.timer) {
    return;
  }
  window.clearTimeout(state.autoRefresh.timer);
  state.autoRefresh.timer = 0;
}

function scheduleAutoRefresh(delayMs = state.autoRefresh.intervalMs) {
  if (!state.autoRefresh.enabled) {
    return;
  }
  clearAutoRefreshTimer();
  if (document.visibilityState === "hidden") {
    state.autoRefresh.pausedReason = "hidden";
    return;
  }
  if (state.autoRefresh.pausedReason === "hidden") {
    state.autoRefresh.pausedReason = "";
  }
  const delay = Math.max(minAutoRefreshIntervalMs, Number(delayMs) || state.autoRefresh.intervalMs);
  state.autoRefresh.timer = window.setTimeout(() => {
    refreshNow().catch((error) => {
      state.autoRefresh.lastError = error.message || String(error);
      state.autoRefresh.inFlight = false;
      render();
      scheduleAutoRefresh();
    });
  }, delay);
}

function handleAutoRefreshVisibilityChange() {
  if (!state.autoRefresh.enabled) {
    return;
  }
  if (document.visibilityState === "hidden") {
    clearAutoRefreshTimer();
    state.autoRefresh.pausedReason = "hidden";
    return;
  }
  state.autoRefresh.pausedReason = "";
  refreshNow({ resume: true }).catch((error) => {
    state.autoRefresh.lastError = error.message || String(error);
    state.autoRefresh.inFlight = false;
    render();
    scheduleAutoRefresh();
  });
}

async function refreshNow(options = {}) {
  if (!state.autoRefresh.enabled && !options.manual) {
    return;
  }
  clearAutoRefreshTimer();
  if (!options.manual && document.visibilityState === "hidden") {
    state.autoRefresh.pausedReason = "hidden";
    return;
  }
  if (state.autoRefresh.inFlight || state.focusedSeriesLoading || state.fullSnapshotLoading) {
    scheduleAutoRefresh(minAutoRefreshIntervalMs);
    return;
  }
  if (!options.manual && dashboardHasActiveControl()) {
    state.autoRefresh.pausedReason = "editing";
    scheduleAutoRefresh(minAutoRefreshIntervalMs);
    return;
  }

  state.autoRefresh.pausedReason = "";
  state.autoRefresh.inFlight = true;
  state.autoRefresh.lastStartedAt = Date.now();
  state.autoRefresh.lastError = "";
  if (options.manual) {
    render();
  }

  let shouldRender = false;
  try {
    await fetchSnapshot({ autoRefresh: true, silent: true, render: false, loadAutoSeriesDetail: false });
    const seriesError = await refreshFocusedSeriesAfterSnapshot();
    const refreshError = criticalRefreshError(seriesError);
    if (refreshError) {
      throw new Error(refreshError);
    }
    state.autoRefresh.lastCompletedAt = Date.now();
    shouldRender = true;
  } catch (error) {
    state.autoRefresh.lastError = error.message || String(error);
    shouldRender = true;
  } finally {
    state.autoRefresh.inFlight = false;
    if (shouldRender) {
      render();
    }
    scheduleAutoRefresh();
  }
}

function criticalRefreshError(seriesError = state.focusedSeriesError) {
  return state.experimentsError
    || seriesError
    || state.featuredErrors.values().next().value
    || state.presetMetricErrors.values().next().value
    || "";
}

function dashboardHasActiveControl() {
  const active = document.activeElement;
  if (!active || !root.contains(active)) {
    return false;
  }
  const tag = text(active.tagName, "").toLowerCase();
  return ["input", "select", "textarea"].includes(tag) || active.isContentEditable;
}

async function refreshFocusedSeriesAfterSnapshot() {
  const metricName = state.metric || state.snapshot?.chart?.metric_name;
  if (!metricName || !cachedFocusedSeries(metricName)) {
    return "";
  }
  const retainedDetail = cachedFocusedSeries(metricName);
  await loadFocusedSeriesDetail({ force: true, silent: true });
  if (state.focusedSeriesError && retainedDetail) {
    applyFocusedSeriesDetail(retainedDetail);
  }
  return state.focusedSeriesError;
}

function snapshotWithSummaryDefaults(snapshot, summary) {
  if (!summary || text(snapshot?.payload_mode, "") !== "metric") {
    return snapshot;
  }
  return {
    ...summary,
    ...snapshot,
    experiment: snapshot.experiment || summary.experiment,
    run_groups: list(snapshot.run_groups).length ? snapshot.run_groups : summary.run_groups,
    runs: list(snapshot.runs).length ? snapshot.runs : summary.runs,
    metric_options: list(snapshot.metric_options).length ? snapshot.metric_options : summary.metric_options,
  };
}

function initialExpandedExperiments() {
  return new Set(normalizeMetricList(initialURL.searchParams.get("experiments")));
}

function focusedSeriesCacheKey(metricName, queryOptions = {}) {
  if (!metricName) {
    return "";
  }
  const controls = state.focusedSeriesControls;
  const budget = queryOptions.maxPoints || focusedSeriesPointBudget(controls);
  return [
    text(config.source, "local").trim().toLowerCase() || "local",
    text(config.workspace, "").trim(),
    state.target,
    metricName,
    text(controls.runID, "").trim(),
    text(controls.startStep, "").trim(),
    text(controls.endStep, "").trim(),
    text(controls.stepInterval, "auto").trim() || "auto",
    String(budget),
  ].join("|");
}

function visibleChartSeries(chart, options = {}) {
  const runIDs = options.runIDs;
  return list(chart?.series).filter((series) => {
    if (runIDs) {
      return runIDs.has(series.run_id);
    }
    return !state.hiddenRuns.has(series.run_id);
  });
}

function totalRawChartPoints(chart, options = {}) {
  return visibleChartSeries(chart, options).reduce((total, series) => total + (numberValue(series.point_count) || 0), 0);
}

function totalRenderedChartPoints(chart, options = {}) {
  return visibleChartSeries(chart, options).reduce((total, series) => total + (numberValue(series.rendered_points) || list(series.values).length || 0), 0);
}

function chartDensity(chart, options = {}) {
  const series = visibleChartSeries(chart, options);
  const rawPoints = totalRawChartPoints(chart, options);
  const renderedPoints = totalRenderedChartPoints(chart, options);
  const sampledSeries = series.filter((item) => {
    const raw = numberValue(item.point_count) || 0;
    const rendered = numberValue(item.rendered_points) || list(item.values).length || 0;
    return item.decimated || rendered < raw;
  }).length;
  return {
    seriesCount: series.length,
    rawPoints,
    renderedPoints,
    sampledSeries,
    sampled: sampledSeries > 0 || renderedPoints < rawPoints,
  };
}

function densityRatioLabel(rawPoints, renderedPoints) {
  if (!rawPoints || renderedPoints >= rawPoints) {
    return "full detail";
  }
  const percent = Math.max(1, Math.round((renderedPoints / rawPoints) * 100));
  return `${percent}% rendered`;
}

function densityPill(label, value, options = {}) {
  return h("span", { class: `density-pill ${options.warning ? "warning" : ""}`.trim(), title: options.title || "" },
    h("b", {}, value),
    " ",
    label,
  );
}

function renderDensityStrip(snapshot, options = {}) {
  const chart = snapshot?.chart;
  const density = chartDensity(chart, { runIDs: options.runIDs || filteredRunIDs(snapshot) });
  const warnings = list(snapshot?.warnings);
  if (!density.seriesCount && !warnings.length) {
    return null;
  }
  const pills = [
    density.seriesCount ? densityPill("series", Intl.NumberFormat().format(density.seriesCount)) : null,
    density.rawPoints ? densityPill("raw points", Intl.NumberFormat().format(density.rawPoints)) : null,
    chartResolutionLabel(chart) ? densityPill("resolution", chartResolutionLabel(chart), { title: "Focused detail sampling interval" }) : null,
    density.renderedPoints ? densityPill("hover points", Intl.NumberFormat().format(density.renderedPoints), {
      warning: density.sampled,
      title: densityRatioLabel(density.rawPoints, density.renderedPoints),
    }) : null,
    density.sampledSeries ? densityPill("sampled lines", Intl.NumberFormat().format(density.sampledSeries), { warning: true }) : null,
    ...warnings.slice(0, 3).map((warning) => h("span", { class: "density-pill warning", title: warning }, warning)),
  ];
  return h("div", { class: `density-strip ${options.compact ? "compact" : ""}`.trim() },
    options.compact ? null : h("strong", {}, "Density"),
    ...pills,
  );
}

function renderMetricCardDensity(chart, options = {}) {
  const density = chartDensity(chart, { runIDs: options.runIDs || filteredRunIDSetForChart(chart) });
  if (!density.rawPoints) {
    return null;
  }
  const optimized = density.sampled;
  const resolution = chartResolutionLabel(chart);
  const title = optimized
    ? `${densityRatioLabel(density.rawPoints, density.renderedPoints)}${resolution ? ` at ${resolution}` : ""}. Showing density-sampled points for speed; raw history is preserved.`
    : "All points are shown.";
  return h("div", { class: `metric-card-density ${optimized ? "optimized" : "full"}`.trim(), title },
    h("span", {}, `${Intl.NumberFormat().format(density.renderedPoints)} of ${Intl.NumberFormat().format(density.rawPoints)} points shown${resolution ? ` · ${resolution}` : ""}`),
    optimized ? h("b", {}, "Sampled view") : h("b", {}, "All points"),
  );
}

function cachedFocusedSeries(metricName) {
  const key = focusedSeriesCacheKey(metricName);
  return key ? state.focusedSeriesCache.get(key) : null;
}

function applyCachedFocusedSeries(metricName) {
  if (!metricName) {
    return false;
  }
  const detail = cachedFocusedSeries(metricName);
  if (!detail) {
    return false;
  }
  applyFocusedSeriesDetail(detail);
  return true;
}

function applyFocusedSeriesDetail(detail) {
  const metricName = detail?.chart?.metric_name || detail?.metric || state.metric;
  if (!metricName || !detail?.chart) {
    return;
  }
  const base = state.featuredSnapshots.get(metricName) || state.snapshot || state.summarySnapshot || {};
  const next = {
    ...base,
    chart: {
      ...detail.chart,
      step_interval: detail.step_interval || detail.chart?.step_interval || 0,
      raw_query: detail.raw_query || "",
      raw_query_source: detail.raw_query_source || "",
    },
  };
  state.featuredSnapshots.set(metricName, next);
  if (state.metric === metricName || state.snapshot?.chart?.metric_name === metricName) {
    state.snapshot = snapshotWithSummaryDefaults(next, state.summarySnapshot);
  }
}

async function loadFocusedSeriesDetail(options = {}) {
  const metricName = state.metric || state.snapshot?.chart?.metric_name;
  if (!metricName || state.focusedSeriesLoading) {
    return;
  }
  let queryOptions;
  try {
    queryOptions = focusedSeriesQueryOptions();
  } catch (error) {
    state.focusedSeriesError = error.message || String(error);
    if (!options.silent) {
      render();
    }
    return;
  }
  const cacheKey = focusedSeriesCacheKey(metricName);
  if (!options.force && cacheKey && state.focusedSeriesCache.has(cacheKey)) {
    applyFocusedSeriesDetail(state.focusedSeriesCache.get(cacheKey));
    if (!options.silent) {
      render();
    }
    return;
  }
  state.focusedSeriesLoading = true;
  state.focusedSeriesError = "";
  if (!options.silent) {
    render();
  }
  try {
    const detail = await fetchSeriesFor(metricName, queryOptions);
    if (cacheKey) {
      state.focusedSeriesCache.set(cacheKey, detail);
    }
    applyFocusedSeriesDetail(detail);
  } catch (error) {
    state.focusedSeriesError = error.message || String(error);
  } finally {
    state.focusedSeriesLoading = false;
    if (!options.silent) {
      render();
    }
  }
}

async function bootStellar() {
  if (!state.target) {
    await refreshExperiments({ query: state.experimentSearch, render: false });
    renderLanding();
    return;
  }
  await fetchSnapshot();
  startAutoRefresh();
}

function renderLoading() {
  clear(root);
  root.className = "stellar-app";
  root.append(renderLoadingShell(state.target, `Fetching ${state.target}.`));
  publishVisualState("loading");
}

function renderLoadingShell(target, detail) {
  return h("div", { class: "app-shell stellar-loading-shell", "aria-busy": "true" },
    h("header", { class: "app-topbar" },
      h("div", { class: "topbar-home" }, h("strong", {}, "Stellar")),
      h("div", { class: "topbar-title" },
        h("strong", {}, target || "Stellar"),
        h("span", {}, "Loading experiment dashboard"),
      ),
      h("div", { class: "topbar-actions" },
        renderSourceStatus(),
        h("span", { class: "refresh-status loading" }, "auto ", h("b", {}, "loading")),
      ),
    ),
    h("div", { class: "workspace" },
      h("aside", { class: "variables-rail", "aria-label": "Loading dashboard controls" },
        h("section", { class: "rail-top" },
          h("span", { class: "rail-label" }, "Runs"),
          h("div", { class: "stellar-loading-line wide" }),
          h("div", { class: "stellar-loading-line" }),
        ),
        h("section", { class: "rail-section" },
          h("p", { class: "rail-label" }, "Metric focus"),
          h("div", { class: "stellar-loading-line wide" }),
          h("div", { class: "stellar-loading-line short" }),
        ),
      ),
      h("main", { class: "report-canvas" },
        h("section", { class: "panel-grid" },
          h("article", { class: "panel wide stellar-loading-panel" },
            h("div", { class: "panel-content" }, h("p", {}, detail || "Fetching metrics and chart summaries.")),
          ),
          h("article", { class: "panel stellar-loading-panel" },
            h("div", { class: "panel-content" }, h("p", {}, "Preparing run controls.")),
          ),
          h("article", { class: "panel stellar-loading-panel" },
            h("div", { class: "panel-content" }, h("p", {}, "Loading evidence.")),
          ),
        ),
      ),
    ),
  );
}

function renderError(error) {
  clear(root);
  root.className = "";
  root.append(
    h("section", { class: "stellar-error", "aria-live": "assertive" },
      h("p", { class: "eyebrow" }, "Stellar"),
      h("h1", {}, "Stellar could not load"),
      h("p", {}, error.message || String(error)),
      h("p", {}, "Refresh the page or check the Stellar API response."),
    ),
  );
  publishVisualState("error", { error: error.message || String(error) });
}

function renderLanding(options = {}) {
  const projectOptions = landingProjectOptions(state.experiments);
  const visibleExperiments = landingVisibleExperiments(state.experiments);
  clear(root);
  root.className = "stellar-landing-app";
  root.append(
    h("main", { class: "landing-shell" },
      h("section", { class: "landing-hero" },
        h("p", { class: "eyebrow" }, "Stellar"),
        h("h1", {}, "Choose an experiment"),
        h("p", {}, "Search experiments and open a labeled run dashboard when you are ready to inspect metrics."),
        renderLandingControls(projectOptions, state.experiments.length),
      ),
      state.experimentsError ? h("section", { class: "landing-error" }, experimentsErrorMessage()) : null,
      h("section", { class: "landing-grid", "aria-live": "polite" },
        state.experimentsLoading ? h("article", { class: "landing-empty" }, "Loading experiments...") : null,
        !state.experimentsLoading && visibleExperiments.length === 0
          ? h("article", { class: "landing-empty" }, landingEmptyMessage())
          : null,
        !state.experimentsLoading && visibleExperiments.length > 0 ? renderLandingExperimentHeader() : null,
        ...visibleExperiments.map((experiment) => renderLandingExperimentCard(experiment)),
      ),
    ),
  );
  if (options.focusExperimentSearch) {
    focusInput("[data-experiment-search-input]");
  }
  publishVisualState("ready", {
    view: "landing",
    experimentCount: state.experiments.length,
    experimentsLoading: state.experimentsLoading,
    experimentsError: state.experimentsError,
  });
}

function renderLandingControls(projectOptions, totalCount) {
  return h("div", { class: "landing-controls" },
    renderExperimentSearchBar({ variant: "landing" }),
    renderLandingProjectSelect(projectOptions, totalCount),
    renderLandingTagFilter(),
  );
}

function renderLandingProjectSelect(projectOptions, totalCount) {
  return h("label", { class: "landing-project-select" },
    h("span", {}, "Project"),
    h("select", {
      value: state.landingProjectFilter,
      onchange: (event) => {
        state.landingProjectFilter = event.target.value;
        updateURL();
        refreshExperiments({ render: true }).catch(renderError);
      },
    },
      h("option", { value: "" }, `All projects (${totalCount})`),
      ...projectOptions.map((option) => h("option", { value: option.project }, `${option.project} (${option.count})`)),
    ),
  );
}

function renderLandingTagFilter() {
  return h("label", { class: "landing-project-select landing-tag-filter" },
    h("span", {}, "Tag"),
    h("input", {
      type: "search",
      value: state.landingTagFilter,
      placeholder: "key=value",
      oninput: (event) => {
        state.landingTagFilter = event.target.value;
        updateURL();
      },
      onchange: () => {
        refreshExperiments({ render: true }).catch(renderError);
      },
      onkeydown: (event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          refreshExperiments({ render: true }).catch(renderError);
        }
      },
    }),
  );
}

function landingProjectOptions(experiments) {
  const counts = new Map();
  list(experiments).forEach((experiment) => {
    const project = landingProjectName(experiment);
    counts.set(project, (counts.get(project) || 0) + 1);
  });
  if (state.landingProjectFilter && !counts.has(state.landingProjectFilter)) {
    counts.set(state.landingProjectFilter, 0);
  }
  return [...counts.entries()]
    .map(([project, count]) => ({ project, count }))
    .sort((left, right) => left.project.localeCompare(right.project));
}

function landingVisibleExperiments(experiments) {
  return list(experiments)
    .filter((experiment) => !state.landingProjectFilter || landingProjectName(experiment) === state.landingProjectFilter)
    .sort(compareLandingExperimentRows);
}

function landingProjectName(experiment) {
  return text(experiment.project || experiment.source, "Uncategorized");
}

function compareLandingExperimentRows(left, right) {
  return landingProjectName(left).localeCompare(landingProjectName(right)) || compareLandingExperiments(left, right);
}

function compareLandingExperiments(left, right) {
  const leftName = text(left.name || left.experiment_id, "");
  const rightName = text(right.name || right.experiment_id, "");
  return leftName.localeCompare(rightName) || text(left.experiment_id, "").localeCompare(text(right.experiment_id, ""));
}

function landingEmptyMessage() {
  if ((state.landingProjectFilter || state.landingTagFilter) && state.experimentSearch) {
    return "No experiments matched these filters and search.";
  }
  if (state.landingProjectFilter) {
    return "No experiments matched this project filter.";
  }
  if (state.landingTagFilter) {
    return "No experiments matched this tag filter.";
  }
  return state.experimentSearch ? "No experiments matched this search." : "No experiments found yet.";
}

function renderLandingExperimentHeader() {
  return h("div", { class: "landing-grid-header", "aria-hidden": "true" },
    h("span", {}, "Experiment"),
    h("div", { class: "landing-card-meta landing-grid-meta-header" },
      h("span", {}, "Runs"),
      h("span", {}, "Groups"),
      h("span", {}, "Metrics"),
      h("span", {}, "Latest"),
    ),
    h("span", {}, "Status"),
  );
}

function renderLandingExperimentCard(experiment) {
  const id = experiment.experiment_id;
  const name = experiment.name || id;
  const secondaryParts = [];
  if (!state.landingProjectFilter) {
    secondaryParts.push(landingProjectName(experiment));
  }
  if (id && id !== name) {
    secondaryParts.push(id);
  }
  const secondaryText = secondaryParts.join(" / ");
  const counts = experiment.lifecycle_counts || experiment.state_counts || {};
  const runCount = experiment.run_count || 0;
  const groupCount = experiment.run_group_count || 0;
  const metricCount = list(experiment.metric_names).length;
  const latest = experiment.latest_run_at ? formatShortClockTime(experiment.latest_run_at) : "—";
  const statuses = landingLifecycleStatusData(counts).filter((status) => status.count > 0);
  return h("article", { class: "landing-experiment-card" },
    h("button", {
      type: "button",
      class: "landing-experiment-main",
      title: `Open ${name}`,
      onclick: () => selectExperiment(id),
    },
      h("strong", {}, name),
      secondaryText ? h("span", { class: "landing-card-id" }, secondaryText) : null,
    ),
    h("div", { class: "landing-card-meta" },
      renderLandingStat("Runs", runCount, runCount === 1 ? "run" : "runs"),
      renderLandingStat("Groups", groupCount, groupCount === 1 ? "group" : "groups"),
      renderLandingStat("Metrics", metricCount, metricCount === 1 ? "metric" : "metrics"),
      renderLandingStat("Latest", latest, ""),
    ),
    h("div", { class: "landing-status-summary" },
      ...(statuses.length
        ? statuses.map((status) => renderLandingStatusToken(status))
        : [h("span", { class: "landing-status-empty" }, "—")]),
    ),
  );
}

function renderLandingStat(label, value, mobileLabel = label) {
  return h("span", { class: "landing-card-stat", title: label },
    h("b", {}, String(value)),
    mobileLabel ? h("em", {}, mobileLabel) : null,
  );
}

function renderLandingStatusToken(status) {
  return h("span", {
    class: `landing-status-token ${status.id}`.trim(),
    title: status.label,
    "aria-label": `${status.count} ${status.label}`,
  },
    `${status.shortLabel} ${status.count}`,
  );
}

function landingLifecycleStatusData(counts) {
  return [
    { id: "succeeded", label: "succeeded", shortLabel: "done", count: counts.succeeded || 0 },
    { id: "running", label: "running", shortLabel: "run", count: counts.running || 0 },
    { id: "failed", label: "failed", shortLabel: "fail", count: counts.failed || 0 },
    { id: "pending", label: "pending", shortLabel: "wait", count: counts.pending || 0 },
    { id: "incomplete", label: "incomplete", shortLabel: "inc", count: counts.incomplete || 0 },
  ];
}

function focusInput(selector) {
  const input = root.querySelector(selector);
  if (!input) {
    return;
  }
  input.focus();
  if (typeof input.setSelectionRange === "function") {
    input.setSelectionRange(input.value.length, input.value.length);
  }
}

function render(options = {}) {
  const renderMark = traceMark("stellar.render");
  const snapshot = state.snapshot;
  const visibleRuns = filteredRuns(snapshot);
  clear(root);
  root.className = "stellar-app";
  root.append(
    h("div", { class: "app-shell" },
      renderTopbar(snapshot),
      h("div", { class: "workspace" },
        renderVariablesRail(snapshot),
        h("main", { class: "report-canvas" },
          h("section", { class: "panel-grid" }, ...renderDashboardSections(snapshot)),
        ),
      ),
    ),
  );
  if (options.focusSearch) {
    focusInput("[data-search-input]");
  }
  if (options.focusExperimentSearch) {
    focusInput("[data-experiment-search-input]");
  }
  if (options.focusMetricSearch) {
    focusInput("[data-metric-search-input]");
  }
  traceMeasure("stellar.render", renderMark);
  publishVisualState("ready", {
    view: "dashboard",
    runCount: visibleRuns.length,
    metricOptions: list(snapshot?.metric_options).length,
    sections: visibleDashboardSectionIDs(),
    filters: activeRunFilterLabels(snapshot),
  });
}

function publishVisualState(status, detail = {}) {
  window.__stellarVisual = {
    schema_version: "tau.stellar.visual_state.v0",
    status,
    updated_at: new Date().toISOString(),
    url: window.location.href,
    ready_state: document.readyState,
    target: state.target,
    metric: state.metric,
    selected_metrics: state.selectedMetrics.slice(),
    experiment_search: state.experimentSearch,
    run_search: state.search,
    lifecycle: state.lifecycleFilter,
    auto_refresh: {
      enabled: state.autoRefresh.enabled,
      interval_ms: state.autoRefresh.intervalMs,
      in_flight: state.autoRefresh.inFlight,
      last_error: state.autoRefresh.lastError,
    },
    detail,
  };
}

function renderDashboardSections(snapshot) {
  return visibleDashboardSectionIDs()
    .map((id) => dashboardSectionByID(id)?.render(snapshot))
    .filter(Boolean);
}

function activeRunFilterLabels(snapshot) {
  const labels = [];
  if (state.search.trim()) {
    labels.push(`Run search: ${state.search.trim()}`);
  }
  if (state.lifecycleFilter) {
    labels.push(`Lifecycle: ${state.lifecycleFilter}`);
  }
  if (state.group) {
    labels.push(`Group: ${state.group}`);
  }
  if (state.hiddenRuns.size) {
    labels.push(`${state.hiddenRuns.size} hidden`);
  }
  if (labels.length) {
    labels.push("Charts, runs, and evidence share these filters");
  } else if (snapshot && allRuns(snapshot).length !== filteredRuns(snapshot).length) {
    labels.push("Filtered run set active");
  }
  return labels;
}

function renderTopbar(snapshot) {
  return h("header", { class: "app-topbar" },
    h("button", {
      type: "button",
      class: "topbar-home",
      title: "Return to experiment search",
      "aria-label": "Return to experiment search",
      onclick: () => {
        returnToLanding().catch(renderError);
      },
    },
      h("span", { class: "wandb-mark", "aria-hidden": "true" },
        h("span", { style: "background:#ffbe0b" }),
        h("span", { style: "background:#fb5607" }),
        h("span", { style: "background:#ff006e" }),
        h("span", { style: "background:#8338ec" }),
        h("span", { style: "background:#3a86ff" }),
        h("span", { style: "background:#06d6a0" }),
      ),
      h("span", { class: "topbar-title" },
        h("strong", {}, "Stellar"),
        h("span", { title: snapshot.target }, text(snapshot.target, "experiment")),
      ),
    ),
    renderExperimentSearchBar(),
    h("div", { class: "topbar-actions" },
      renderSourceStatus(),
      statPill("loaded runs", list(snapshot.runs).length),
      statPill("metric files", snapshot.status?.metric_files),
      renderRefreshStatus(),
    ),
  );
}

function renderExperimentSearchBar(options = {}) {
  const landing = options.variant === "landing";
  return h("form", {
    class: landing ? "experiment-search landing-search" : "experiment-search",
    onsubmit: (event) => {
      event.preventDefault();
      refreshExperiments({ render: true, focusExperimentSearch: true }).catch(renderError);
    },
  },
    h("input", {
      type: "search",
      value: state.experimentSearch,
      placeholder: "Search experiments",
      "data-experiment-search-input": true,
      oninput: (event) => {
        state.experimentSearch = event.target.value;
        updateURL();
      },
    }),
    h("button", { type: "submit" }, "Search"),
  );
}

function statPill(label, value) {
  return h("span", { class: "meta-pill" }, h("b", {}, text(value)), " ", label);
}

function renderSourceStatus() {
  const source = text(config.source, "").trim().toLowerCase();
  const label = sourceStatusLabel(source);
  if (!label) {
    return null;
  }
  return h("span", { class: "meta-pill source-status", title: sourceStatusTitle(source) },
    h("b", {}, label),
    " source",
  );
}

function sourceStatusLabel(source) {
  if (source === "kusto") {
    return "Kusto/ADX";
  }
  if (source === "local") {
    return "local expstore";
  }
  if (source === "auto") {
    return "auto";
  }
  return "";
}

function sourceStatusTitle(source) {
  if (source === "kusto") {
    return "Hosted scalar source: authoritative ADX/Kusto rows.";
  }
  if (source === "local") {
    return "Local/offline source: expstore packets, artifacts, and recovery state.";
  }
  if (source === "auto") {
    return "Auto source mode can merge local expstore data and Kusto scalar rows.";
  }
  return "";
}

function renderRefreshStatus() {
  const refresh = state.autoRefresh;
  const status = refresh.inFlight
    ? "refreshing"
    : refresh.pausedReason
      ? `paused: ${refresh.pausedReason}`
      : refresh.lastError
        ? refresh.lastCompletedAt
          ? `stale · updated ${formatClockTime(refresh.lastCompletedAt)}`
          : "degraded"
        : refresh.lastCompletedAt
          ? `updated ${formatClockTime(refresh.lastCompletedAt)}`
          : "waiting";
  const title = refresh.lastError
    ? `Last refresh failed: ${refresh.lastError}${refresh.lastCompletedAt ? ` Showing data last successfully updated ${new Date(refresh.lastCompletedAt).toLocaleString()}.` : ""}`
    : refresh.enabled
      ? `Auto-refreshes every ${formatRefreshInterval(refresh.intervalMs)} while this tab is visible.`
      : "Auto-refresh is disabled for this dashboard.";
  const classes = [
    "refresh-status",
    refresh.enabled ? "enabled" : "disabled",
    refresh.inFlight ? "loading" : "",
    refresh.lastError ? "error" : "",
    refresh.pausedReason ? "paused" : "",
  ].filter(Boolean).join(" ");
  return h("div", { class: classes, title },
    h("span", {}, refresh.enabled ? `auto ${formatRefreshInterval(refresh.intervalMs)}` : "auto off"),
    h("b", {}, status),
    h("button", {
      type: "button",
      class: "refresh-button",
      disabled: refresh.inFlight,
      onclick: () => refreshNow({ manual: true }),
    }, refresh.inFlight ? "Refreshing" : "Refresh"),
  );
}

function experimentsErrorMessage() {
  return `${state.experimentsError}${state.experimentsLastCompletedAt ? ` Last successful update: ${new Date(state.experimentsLastCompletedAt).toLocaleString()}.` : ""}`;
}

function renderVariablesRail(snapshot) {
  const visibleRuns = filteredRuns(snapshot);
  const listedRuns = filteredRuns(snapshot, { includeHidden: true });
  const loadedRuns = list(snapshot.runs).length;
  const canonicalRuns = canonicalRunCount(snapshot, loadedRuns);
  const metricSelect = h("select", {
    onchange: (event) => {
      setFocusMetric(event.target.value);
    },
  });
  for (const option of snapshot.metric_options || []) {
    metricSelect.append(h("option", { value: option.name, selected: option.name === state.metric }, `${option.card} / ${option.name}`));
  }

  const groupSelect = h("select", {
    onchange: (event) => {
      state.group = event.target.value;
      render();
    },
  }, h("option", { value: "" }, "All groups"));
  for (const group of snapshot.run_groups || []) {
    groupSelect.append(h("option", { value: group.run_group_id, selected: state.group === group.run_group_id }, group.name || group.run_group_id));
  }

  return h("aside", { class: "variables-rail" },
    renderExperimentRail(snapshot),
    h("div", { class: "rail-top" },
      h("p", { class: "rail-label" }, `${loadedRuns} loaded of ${canonicalRuns} · ${visibleRuns.length} visible`),
      h("div", { class: "run-toolbar" },
        h("button", { type: "button", class: "icon-button", title: "Show all runs", onclick: showAllRuns }, "all"),
        h("button", { type: "button", class: "icon-button", title: "Hide listed runs", onclick: () => hideRuns(listedRuns) }, "hide"),
      ),
      h("input", {
        type: "search",
        value: state.search,
        placeholder: "Search runs, tags, metrics, state",
        "data-search-input": true,
        oninput: (event) => {
          state.search = event.target.value;
          render({ focusSearch: true });
        },
      }),
      renderLifecycleFilters(snapshot),
      renderRunUpdatedControls(),
      h("div", { class: "run-list" },
        ...listedRuns.map((run) => renderRunRailRow(run)),
      ),
      renderRunLoadMore(),
    ),
    renderControlsDrawer(snapshot, metricSelect, groupSelect),
  );
}

function renderControlsDrawer(snapshot, metricSelect, groupSelect) {
  return h("details", { class: "rail-disclosure controls-drawer" },
    h("summary", {}, "Controls"),
    h("div", { class: "rail-disclosure-body controls-drawer-body" },
      renderControlGroup("Metric and group",
        h("label", {}, "Metric"),
        metricSelect,
        h("label", {}, "Group"),
        groupSelect,
      ),
      renderControlGroup("Query",
        h("div", { class: "query-card" },
          h("code", {}, `target == "${text(snapshot.target)}"`),
          h("code", {}, state.group ? `run_group == "${state.group}"` : "run_group == *"),
          h("code", {}, `metric == "${text(snapshot.chart?.metric_name, state.metric)}"`),
        ),
      ),
      renderControlGroup("Selected metrics",
        ...featuredMetricSpecs(snapshot).map((spec) => checkboxLine(`${spec.family}: ${spec.title}`, !state.featuredErrors.has(spec.name))),
      ),
      renderDashboardSectionControls(),
    ),
  );
}

function renderControlGroup(title, ...children) {
  return h("section", { class: "control-group" },
    h("p", { class: "control-group-title" }, title),
    ...children,
  );
}

function renderExperimentRail(snapshot) {
  const experiments = list(state.experiments);
  const selectedID = snapshot.experiment?.experiment_id || snapshot.target;
  const recent = experiments.slice(0, 3);
  const older = experiments.slice(3);
  const pinnedOlder = older.filter((experiment) => state.expandedExperimentIDs.has(experiment.experiment_id));
  const collapsedOlder = older.filter((experiment) => !state.expandedExperimentIDs.has(experiment.experiment_id));
  const visible = [...recent, ...pinnedOlder];
  return h("div", { class: "rail-section experiment-rail" },
    h("div", { class: "rail-section-heading" },
      h("p", { class: "rail-label" }, experimentRailLabel()),
      h("button", {
        type: "button",
        class: "mini-link-button",
        disabled: state.experimentsLoading,
        onclick: () => refreshExperiments({ render: true }).catch(renderError),
      }, state.experimentsLoading ? "loading" : "refresh"),
    ),
    state.experimentsError ? h("p", { class: "rail-error" }, experimentsErrorMessage()) : null,
    experiments.length === 0 && !state.experimentsLoading ? h("p", { class: "rail-help" }, experimentRailEmptyLabel()) : null,
    h("div", { class: "experiment-list" },
      ...visible.map((experiment) => renderExperimentRow(experiment, selectedID, olderExperimentIDs(older).has(experiment.experiment_id))),
    ),
    collapsedOlder.length > 0 ? h("details", { class: "experiment-more" },
      h("summary", {}, `More experiments (${collapsedOlder.length})`),
      h("div", { class: "experiment-list" },
        ...collapsedOlder.map((experiment) => renderExperimentRow(experiment, selectedID, true)),
      ),
    ) : null,
  );
}

function experimentRailLabel() {
  if (state.experimentSearch) {
    return "Matching experiments";
  }
  return "Recent experiments";
}

function experimentRailEmptyLabel() {
  if (state.experimentSearch) {
    return "No experiments matched this search.";
  }
  return "No experiments found.";
}

function olderExperimentIDs(experiments) {
  return new Set(experiments.map((experiment) => experiment.experiment_id));
}

function renderExperimentRow(experiment, selectedID, older) {
  const id = experiment.experiment_id;
  const selected = id === selectedID;
  const counts = experiment.lifecycle_counts || {};
  const activeCount = (counts.running || 0) + (counts.pending || 0);
  return h("article", { class: `experiment-row ${selected ? "selected" : ""}`.trim() },
    h("button", {
      type: "button",
      class: "experiment-row-main",
      title: `Open ${experiment.name || id}`,
      onclick: () => selectExperiment(id),
    },
      h("strong", {}, experiment.name || id),
      h("span", {}, id),
      h("em", {}, `${experiment.run_count || 0} runs${activeCount ? `, ${activeCount} active` : ""}`),
    ),
    older ? h("button", {
      type: "button",
      class: "experiment-pin",
      title: state.expandedExperimentIDs.has(id) ? "Collapse older experiment" : "Keep older experiment visible",
      onclick: () => toggleExpandedExperiment(id),
    }, state.expandedExperimentIDs.has(id) ? "less" : "show") : null,
  );
}

function returnToLanding() {
  stopAutoRefresh();
  state.autoRefresh.started = false;
  state.target = "";
  state.metric = "";
  state.selectedMetrics = [];
  state.metricSelectionInitialized = false;
  state.snapshot = null;
  state.summarySnapshot = null;
  state.fullSnapshot = null;
  state.fullSnapshotLoading = false;
  state.fullSnapshotError = "";
  state.additionalRuns = [];
  state.runSearchLimit = runPageSize;
  state.runSearchTruncated = false;
  state.runsLoading = false;
  state.runsError = "";
  state.hiddenRuns.clear();
  state.focusedSeriesCache.clear();
  state.featuredSnapshots.clear();
  state.featuredErrors.clear();
  state.presetMetricSnapshots.clear();
  state.presetMetricErrors.clear();
  state.metricSnapshotLoads.clear();
  updateURL({ history: "push" });
  return refreshExperiments({ query: state.experimentSearch, render: false }).then(() => {
    renderLanding();
  });
}

function selectExperiment(experimentID) {
  if (!experimentID || experimentID === state.target) {
    return;
  }
  const fromLanding = !state.target;
  state.target = experimentID;
  state.metric = "";
  if (fromLanding) {
    state.selectedMetrics = [];
  }
  state.metricSelectionInitialized = false;
  state.hiddenRuns.clear();
  state.focusedSeriesCache.clear();
  state.featuredSnapshots.clear();
  state.featuredErrors.clear();
  state.presetMetricSnapshots.clear();
  state.presetMetricErrors.clear();
  state.metricSnapshotLoads.clear();
  updateURL({ history: "push" });
  fetchSnapshot().catch(renderError);
  startAutoRefresh();
}

async function restoreRouteFromLocation() {
  const url = new URL(window.location.href);
  const nextTarget = text(url.searchParams.get("target"), "");
  state.experimentSearch = text(url.searchParams.get("experiment_q"), "");
  state.landingProjectFilter = text(url.searchParams.get("experiment_project"), "");
  state.landingTagFilter = text(url.searchParams.get("experiment_tag"), "");
  state.expandedExperimentIDs = new Set(normalizeMetricList(url.searchParams.get("experiments")));
  if (!nextTarget) {
    stopAutoRefresh();
    state.autoRefresh.started = false;
    state.target = "";
    state.metric = "";
    state.selectedMetrics = [];
    state.metricSelectionInitialized = false;
    clearDashboardRuntimeState();
    await refreshExperiments({ query: state.experimentSearch, render: false });
    renderLanding({ focusExperimentSearch: true });
    return;
  }

  const targetChanged = state.target !== nextTarget;
  state.target = nextTarget;
  const hasMetricParam = url.searchParams.has("metric");
  const hasPinnedParam = url.searchParams.has("pinned");
  state.metric = hasMetricParam ? text(url.searchParams.get("metric"), "") : "";
  state.selectedMetrics = hasPinnedParam
    ? normalizeMetricList(url.searchParams.get("pinned"))
    : hasMetricParam ? [] : loadPinnedMetrics(nextTarget);
  state.lifecycleFilter = normalizeLifecycleFilter(url.searchParams.get("lifecycle"));
  state.runUpdatedFilter = normalizeRunUpdatedFilter(url.searchParams.get("updated"));
  state.runUpdatedSort = normalizeRunUpdatedSort(url.searchParams.get("updated_sort"));
  state.focusedSeriesControls = {
    runID: text(url.searchParams.get("run_id"), ""),
    startStep: text(url.searchParams.get("start_step"), ""),
    endStep: text(url.searchParams.get("end_step"), ""),
    stepInterval: normalizeStepIntervalControl(url.searchParams.get("step_interval")),
    customStepInterval: customStepIntervalValue(normalizeStepIntervalControl(url.searchParams.get("step_interval"))),
    maxPoints: text(url.searchParams.get("max_points"), ""),
  };
  state.outputMediaTag = text(url.searchParams.get("media_tag"), "");
  state.outputMediaRunID = text(url.searchParams.get("media_run"), "");
  state.outputMediaStep = text(url.searchParams.get("media_step"), "");
  state.dashboardSections = dashboardSectionsFromURL(url, nextTarget);
  if (targetChanged) {
    state.metricSelectionInitialized = false;
    clearDashboardRuntimeState();
  }
  await fetchSnapshot();
  startAutoRefresh();
}

function clearDashboardRuntimeState() {
  state.snapshot = null;
  state.summarySnapshot = null;
  state.fullSnapshot = null;
  state.fullSnapshotLoading = false;
  state.fullSnapshotError = "";
  state.hiddenRuns.clear();
  state.focusedSeriesCache.clear();
  state.featuredSnapshots.clear();
  state.featuredErrors.clear();
  state.presetMetricSnapshots.clear();
  state.presetMetricErrors.clear();
  state.metricSnapshotLoads.clear();
  state.showAllPinnedCharts = false;
}

function toggleExpandedExperiment(experimentID) {
  if (state.expandedExperimentIDs.has(experimentID)) {
    state.expandedExperimentIDs.delete(experimentID);
  } else {
    state.expandedExperimentIDs.add(experimentID);
  }
  updateURL();
  render();
}

function renderLifecycleFilters(snapshot) {
  const counts = lifecycleCounts(snapshot);
  const options = [
    { id: "", label: "All" },
    { id: "succeeded", label: "Succeeded" },
    { id: "running", label: "Running" },
    { id: "not_responding", label: "Not responding" },
    { id: "unknown", label: "Unknown" },
    { id: "pending", label: "Pending" },
    { id: "failed", label: "Failed" },
    { id: "cancelled", label: "Cancelled" },
    { id: "incomplete", label: "Incomplete" },
  ];
  return h("div", {
    class: "run-filter-chips",
    role: "list",
    "aria-label": "Loaded run lifecycle filters",
    title: "Lifecycle counts cover loaded runs only.",
  },
    ...options.map((option) => {
      const active = state.lifecycleFilter === option.id;
      const count = option.id ? counts[option.id] || 0 : allRuns(snapshot).length;
      return h("button", {
        type: "button",
        class: `run-filter-chip ${active ? "active" : ""}`.trim(),
        "aria-pressed": active,
        onclick: () => {
          state.lifecycleFilter = option.id;
          updateURL();
          render();
        },
      }, `${option.label} ${count}`);
    }),
  );
}

function renderRunUpdatedControls() {
  return h("div", { class: "run-updated-controls", "aria-label": "Run last updated filters" },
    h("label", {}, "Updated",
      h("select", {
        "aria-label": "Filter runs by last updated time",
        onchange: (event) => {
          state.runUpdatedFilter = normalizeRunUpdatedFilter(event.target.value);
          updateURL();
          render();
        },
      }, runUpdatedFilterOptions.map((option) => h("option", {
        value: option.id,
        selected: state.runUpdatedFilter === option.id,
      }, option.label))),
    ),
    h("label", {}, "Sort",
      h("select", {
        "aria-label": "Sort runs by last updated time",
        onchange: (event) => {
          state.runUpdatedSort = normalizeRunUpdatedSort(event.target.value);
          updateURL();
          render();
        },
      }, runUpdatedSortOptions.map((option) => h("option", {
        value: option.id,
        selected: state.runUpdatedSort === option.id,
      }, option.label))),
    ),
  );
}

function lifecycleCounts(snapshot) {
  const counts = {};
  for (const run of allRuns(snapshot)) {
    const lifecycle = runLifecycle(run);
    counts[lifecycle] = (counts[lifecycle] || 0) + 1;
  }
  return counts;
}

function canonicalRunCount(snapshot, loadedRuns) {
  const experimentID = snapshot.experiment?.experiment_id || snapshot.target;
  const project = snapshot.experiment?.project || list(snapshot.runs)[0]?.project;
  const experiment = list(state.experiments).find((candidate) =>
    candidate.experiment_id === experimentID && (!project || candidate.project === project));
  const total = Number(experiment?.run_count);
  return Number.isFinite(total) ? Math.max(total, loadedRuns) : loadedRuns;
}

function renderDashboardSectionControls() {
  return h("section", { class: "control-group dashboard-section-config" },
    h("div", { class: "rail-section-heading" },
      h("p", { class: "control-group-title" }, "Customize layout"),
      h("button", {
        type: "button",
        class: "mini-link-button",
        onclick: () => resetDashboardSections(),
      }, "reset"),
    ),
    h("p", { class: "rail-help" }, "Customize per target. Use sections=... in the URL to share a layout."),
    h("div", { class: "dashboard-section-list" },
      ...state.dashboardSections.order.map((id, index) => renderDashboardSectionControl(id, index)),
    ),
  );
}

function renderDashboardSectionControl(id, index) {
  const section = dashboardSectionByID(id);
  if (!section) {
    return null;
  }
  const hidden = state.dashboardSections.hidden.has(id);
  return h("article", { class: `dashboard-section-row ${hidden ? "muted" : ""}`.trim() },
    h("div", { class: "dashboard-section-row-top" },
      h("label", { class: "dashboard-section-toggle" },
        h("input", {
          type: "checkbox",
          checked: !hidden,
          onchange: () => toggleDashboardSection(id),
        }),
        h("span", {}, section.id),
      ),
      h("div", { class: "dashboard-section-actions" },
        h("button", {
          type: "button",
          class: "icon-button",
          title: "Move section up",
          disabled: index === 0,
          onclick: () => moveDashboardSection(id, -1),
        }, "up"),
        h("button", {
          type: "button",
          class: "icon-button",
          title: "Move section down",
          disabled: index === state.dashboardSections.order.length - 1,
          onclick: () => moveDashboardSection(id, 1),
        }, "down"),
      ),
    ),
    h("input", {
      type: "text",
      value: dashboardSectionTitle(id),
      placeholder: section.defaultTitle,
      title: `Rename ${section.defaultTitle}`,
      oninput: (event) => updateDashboardSectionTitle(id, event.target.value),
      onchange: () => render(),
    }),
  );
}

function toggleDashboardSection(id) {
  if (state.dashboardSections.hidden.has(id)) {
    state.dashboardSections.hidden.delete(id);
  } else {
    state.dashboardSections.hidden.add(id);
  }
  persistDashboardSectionState();
  render();
}

function moveDashboardSection(id, delta) {
  const order = state.dashboardSections.order;
  const index = order.indexOf(id);
  const nextIndex = index + delta;
  if (index < 0 || nextIndex < 0 || nextIndex >= order.length) {
    return;
  }
  [order[index], order[nextIndex]] = [order[nextIndex], order[index]];
  persistDashboardSectionState();
  render();
}

function updateDashboardSectionTitle(id, value) {
  const section = dashboardSectionByID(id);
  if (!section) {
    return;
  }
  setDashboardSectionText(state.dashboardSections.titles, id, value, section.defaultTitle);
  persistDashboardSectionState();
}

function resetDashboardSections() {
  state.dashboardSections = normalizeDashboardSectionState({});
  persistDashboardSectionState();
  render();
}

function persistDashboardSectionState() {
  updateURL();
}

function renderRunRailRow(run) {
  const selectedSpec = metricSpec(state.metric);
  const metric = metricValueForRun(state.metric, run.run_id);
  const hidden = state.hiddenRuns.has(run.run_id);
  const lifecycle = runLifecycle(run);
  return h("article", { class: `run-row ${hidden ? "run-row-hidden" : ""}`.trim() },
    h("input", {
      type: "checkbox",
      checked: !hidden,
      title: hidden ? "Show run" : "Hide run",
      onchange: () => toggleRunVisibility(run.run_id),
    }),
    h("span", { class: "run-dot", style: `background:${run.color || colorForToken(run.run_id || run.run_group_id)}` }),
    h("div", { class: "run-row-main" },
      h("div", { class: "run-row-title" },
        h("span", { title: run.run_id }, run.run_id),
        h("b", {}, formatTableValue(metric, selectedSpec)),
      ),
      h("div", { class: "run-tags" },
        h("span", {}, run.run_group_id),
        h("span", { class: `run-status-badge ${lifecycle}`.trim(), title: runSuccessTitle(run) }, runLifecycleLabel(run)),
        h("span", { class: "run-updated-tag", title: runLastUpdatedTitle(run) }, runLastUpdatedLabel(run)),
      ),
    ),
  );
}

function renderRunLoadMore() {
  if (state.runsError) {
    return h("div", { class: "run-load-status", role: "status" },
      h("span", {}, state.runsError),
      h("button", { type: "button", class: "run-load-more", onclick: () => loadMoreRuns() }, "Retry"),
    );
  }
  if (state.runsLoading || state.runSearchTruncated) {
    const remaining = Math.max(0, maxLoadedRuns - state.runSearchLimit);
    return h("button", {
      type: "button",
      class: "run-load-more",
      disabled: state.runsLoading || remaining === 0,
      onclick: () => loadMoreRuns(),
    }, state.runsLoading ? "Loading runs..." : remaining === 0 ? "Run limit reached" : `Load ${Math.min(runPageSize, remaining)} more runs`);
  }
  return null;
}

async function loadMoreRuns() {
  if (state.runsLoading || state.runSearchLimit >= maxLoadedRuns) {
    return;
  }
  const limit = Math.min(maxLoadedRuns, state.runSearchLimit + runPageSize);
  state.runsLoading = true;
  state.runsError = "";
  render();
  try {
    const result = await fetchRuns(limit);
    state.additionalRuns = list(result.runs);
    state.runSearchLimit = limit;
    state.runSearchTruncated = result.truncated === true;
  } catch (error) {
    state.runsError = error.message || String(error);
  } finally {
    state.runsLoading = false;
    render();
  }
}

function toggleRunVisibility(runID) {
  if (state.hiddenRuns.has(runID)) {
    state.hiddenRuns.delete(runID);
  } else {
    state.hiddenRuns.add(runID);
  }
  render();
}

function showAllRuns() {
  state.hiddenRuns.clear();
  render();
}

function hideRuns(runs) {
  for (const run of runs) {
    state.hiddenRuns.add(run.run_id);
  }
  render();
}

function checkboxLine(label, checked) {
  return h("label", { class: "check-row" },
    h("input", { type: "checkbox", checked }),
    h("span", {}, label),
  );
}

function configuredPanel(sectionID, fallbackTitle, fallbackSubtitle, body, className = "") {
  return panel(
    dashboardSectionTitle(sectionID) || fallbackTitle,
    dashboardSectionSubtitle(sectionID, fallbackSubtitle),
    body,
    className,
    sectionID,
    { hideTitlebar: true },
  );
}

function panel(title, subtitle, body, className = "", sectionID = "", options = {}) {
  const attrs = {
    class: `panel ${className} ${options.hideTitlebar ? "panel-titlebar-hidden" : ""}`.trim(),
    "aria-label": title,
  };
  if (sectionID) {
    attrs.dataset = { sectionId: sectionID, sectionTitle: title };
    attrs.id = `section-${sectionID}`;
  }
  return h("article", attrs,
    options.hideTitlebar ? null : h("div", { class: "panel-titlebar" },
      h("div", {}, h("h2", {}, title), subtitle ? h("p", {}, subtitle) : null),
      h("span", { class: "panel-menu" }, "..."),
    ),
    h("div", { class: "panel-content" }, body),
  );
}

const commonMedicalLabelDisplayNames = new Map([
  ["atelectasis", "Atelectasis"],
  ["cardiomegaly", "Cardiomegaly"],
  ["consolidation", "Consolidation"],
  ["edema", "Edema"],
  ["pleural_effusion", "Pleural Effusion"],
  ["pleural effusion", "Pleural Effusion"],
]);

function renderTrainingHealthPanel(snapshot) {
  const metrics = trainingHealthMetricNames(snapshot).slice(0, 6);
  const warnings = list(snapshot.warnings).slice(0, 2);
  const body = h("div", { class: "researcher-preset" },
    h("div", { class: "section-summary" },
      h("strong", {}, metrics.length ? "Training metrics are available" : "Training health is not instrumented yet"),
      h("span", {}, metrics.length ? "Loss, LR, throughput, and status signals are grouped here automatically." : "Import train/* or status/* scalar metrics to populate this section."),
    ),
    metrics.length
      ? h("div", { class: "research-metric-grid" }, metrics.map((metricName) => renderResearchMetricTile(metricName, {
          mode: isLossLikeMetric(metricName) || isStatusProblemMetric(metricName) ? "latest" : "latest",
        })))
      : h("p", { class: "empty" }, "No train/*, status/*, throughput, skipped, or invalid-label metrics were found."),
    warnings.length ? h("div", { class: "warning-list" }, warnings.map((warning) => h("span", {}, warning))) : null,
  );
  return panel("Training health", "Dense train/loss keeps EMA smoothing and raw hover detail; sparse metrics stay literal.", body, "half researcher-panel");
}

function renderValidationQualityPanel(snapshot) {
  const metrics = validationQualityMetricNames(snapshot).slice(0, 8);
  const labels = labelMetricGroups(snapshot).slice(0, 5);
  const body = h("div", { class: "researcher-preset" },
    metrics.length
      ? h("div", { class: "research-metric-grid validation-grid" }, metrics.map((metricName) => renderResearchMetricTile(metricName, { mode: metricGoal(metricName) === "minimize" ? "best" : "best" })))
      : h("p", { class: "empty" }, "No eval/* or final/* quality metrics were found. Import validation scalars such as macro AUPRC, AUROC, F1, Brier, or ECE."),
    h("section", { class: "label-card-section" },
      h("div", { class: "evidence-section-head" },
        h("h3", {}, "Per-label quality"),
        h("span", {}, labels.length ? "compact label cards" : "no label-scoped metrics found"),
      ),
      labels.length
        ? h("div", { class: "label-card-grid" }, labels.map(renderLabelMetricCard))
        : h("p", { class: "empty" }, "Label-scoped metrics are optional. When Atelectasis, Cardiomegaly, Consolidation, Edema, Pleural Effusion, or other label metrics exist, they appear here."),
    ),
  );
  return panel("Validation quality", "Primary AUPRC first, then AUROC/F1/calibration and label-level signals.", body, "half researcher-panel validation-quality-panel");
}

function renderPerLabelQualityPanel(snapshot) {
  const labels = labelMetricGroups(snapshot).slice(0, 5);
  if (!labels.length) {
    return null;
  }
  const body = h("div", { class: "researcher-preset compact-support-panel" },
    h("section", { class: "label-card-section" },
      h("div", { class: "evidence-section-head" },
        h("h3", {}, "Per-label quality"),
        h("span", {}, "secondary drilldown below chart grid"),
      ),
      h("div", { class: "label-card-grid compact" }, labels.map(renderLabelMetricCard)),
    ),
  );
  return configuredPanel("labels", "Per-label validation", "Label cards support the main validation charts.", body, "half researcher-panel per-label-quality-panel");
}

function renderErrorAnalysisPanel(snapshot) {
  const summaryMetrics = detectionSummaryMetricNames(snapshot);
  const summaryMetricSet = new Set(summaryMetrics);
  const metrics = errorAnalysisMetricNames(snapshot).filter((metricName) => !summaryMetricSet.has(metricName)).slice(0, 6);
  const predictionState = renderPredictionSummaryState(snapshot);
  if (!summaryMetrics.length && !metrics.length && !predictionState) {
    return null;
  }
  const body = h("div", { class: "researcher-preset" },
    summaryMetrics.length
      ? h("section", { class: "detection-summary-section" },
          h("div", { class: "evidence-section-head" },
            h("h3", {}, "Detection summary"),
            h("span", {}, "curated detect/macro_* threshold metrics"),
          ),
          h("div", { class: "research-metric-grid error-grid" }, summaryMetrics.map((metricName) => renderResearchMetricTile(metricName, { mode: "latest", badge: "Detection" }))),
        )
      : h("p", { class: "empty" }, "No dedicated error-analysis scalars were found. Import detect/* metrics or eval/final false-positive, false-negative, threshold, precision, recall, or calibration summaries to populate this section."),
    metrics.length
      ? h("section", { class: "detection-drilldown-section" },
          h("div", { class: "evidence-section-head" },
            h("h3", {}, "Error drilldown"),
            h("span", {}, "false-positive/false-negative, thresholds, worst labels"),
          ),
          h("div", { class: "research-metric-grid error-grid" }, metrics.map((metricName) => renderResearchMetricTile(metricName, { mode: "best" }))),
        )
      : null,
    predictionState,
  );
  return configuredPanel("errors", "Error analysis", "False positives/negatives, threshold behavior, calibration, and worst-label signals.", body, "half researcher-panel error-analysis-panel");
}

function renderReproducibilityPanel(snapshot) {
  const detailSnapshot = state.fullSnapshot || snapshot;
  const counts = evidenceCounts(detailSnapshot);
  const artifacts = allRunEvidence(detailSnapshot, "artifacts");
  const body = h("div", { class: "researcher-preset" },
    h("div", { class: "repro-grid" },
      researcherFact("Configs", counts.configs || snapshot.status?.configs || 0, isCompactPayload(snapshot) && !state.fullSnapshot ? "details deferred" : "normalized configs"),
      researcherFact("Artifacts", counts.artifacts || snapshot.status?.artifacts || 0, "checkpoints, reports, media"),
      researcherFact("Logs/events", counts.events || 0, isCompactPayload(snapshot) && !state.fullSnapshot ? "load details for logs" : "imported event rows"),
      researcherFact("Environment", environmentSummary(detailSnapshot), "run context"),
      researcherFact("Data manifest", dataManifestSummary(detailSnapshot), "data/* metrics or artifacts"),
      researcherFact("Reports", reportArtifactSummary(artifacts), "rendered reports"),
    ),
    isCompactPayload(snapshot) && !state.fullSnapshot ? renderDeferredDetailsNotice("Reproducibility evidence is deferred", "Load evidence details to inspect configs, artifacts, checkpoints, logs, environment, data manifests, and reports.") : null,
  );
  return configuredPanel("repro", "Reproducibility / evidence", "Can another run be compared or reproduced?", body, "half researcher-panel reproducibility-panel");
}

function renderResearchMetricTile(metricName, options = {}) {
  const summary = metricValueSummary(metricName, options);
  const classes = ["research-metric-tile", options.emphasis ? "emphasis" : "", summary.error ? "warning" : ""].filter(Boolean).join(" ");
  return h("article", { class: classes, title: metricName },
    h("span", { class: "state-pill" }, options.badge || summary.spec.family || metricNamespace(metricName)),
    h("h4", {}, options.title || summary.spec.title),
    h("strong", {}, summary.value),
    h("p", {}, summary.detail),
    h("code", {}, metricName),
  );
}

function renderEmptyResearchTile(title, detail) {
  return h("article", { class: "research-metric-tile muted" },
    h("span", { class: "state-pill" }, "not available"),
    h("h4", {}, title),
    h("strong", {}, "--"),
    h("p", {}, detail),
  );
}

function researcherFact(label, value, note) {
  return h("article", { class: "researcher-fact" },
    h("span", {}, label),
    h("strong", {}, text(value)),
    note ? h("em", {}, note) : null,
  );
}

function renderLabelMetricCard(group) {
  return h("article", { class: "label-card" },
    h("h4", {}, group.label),
    h("div", { class: "label-metric-list" },
      group.metrics.slice(0, 3).map((metricName) => {
        const summary = metricValueSummary(metricName, { mode: metricGoal(metricName) === "minimize" ? "best" : "best" });
        return h("span", { title: metricName },
          h("em", {}, metricMeasureLabel(metricName)),
          h("b", {}, summary.value),
        );
      }),
    ),
  );
}

function renderPredictionSummaryState(snapshot) {
  const detailSnapshot = state.fullSnapshot || snapshot;
  const artifacts = allRunEvidence(detailSnapshot, "artifacts").filter(isPredictionSummaryArtifact);
  const hasErrorMetrics = detectionSummaryMetricNames(snapshot).length || errorAnalysisMetricNames(snapshot).length;
  if (isCompactPayload(snapshot) && !state.fullSnapshot) {
    if (!hasErrorMetrics) {
      return null;
    }
    return h("section", { class: "evidence-section evidence-notice prediction-summary-state" },
      h("div", { class: "evidence-section-head" },
        h("h3", {}, "Prediction summary details are deferred"),
        h("span", {}, "load evidence details"),
      ),
      h("p", {}, "Scalar detection metrics can be shown from metric snapshots, but false-positive/false-negative rows, confusion views, and threshold summaries require full evidence details. If none appear after loading, run a prediction-summary import."),
      renderLoadDetailsButton("Load prediction details"),
    );
  }
  if (artifacts.length) {
    return h("section", { class: "evidence-section compact prediction-summary-state" },
      h("div", { class: "evidence-section-head" },
        h("h3", {}, "Prediction summaries"),
        h("span", {}, `${artifacts.length} imported artifacts`),
      ),
      h("div", { class: "evidence-list" }, artifacts.slice(0, 5).map(renderArtifactEvidenceItem)),
    );
  }
  if (!hasErrorMetrics) {
    return null;
  }
  return h("section", { class: "evidence-section evidence-notice prediction-summary-state" },
    h("div", { class: "evidence-section-head" },
      h("h3", {}, "Needs prediction summary import"),
      h("span", {}, "no detailed rows found"),
    ),
    h("p", {}, "No false-positive/false-negative rows, confusion matrix, threshold table, or calibration summary artifacts were found. Stellar will not pretend those analyses exist from scalar metrics alone."),
  );
}

function renderDeferredDetailsNotice(title, body) {
  return h("section", { class: "evidence-section evidence-notice" },
    h("div", { class: "evidence-section-head" },
      h("h3", {}, title),
      h("span", {}, "compact payload"),
    ),
    h("p", {}, body),
    renderLoadDetailsButton("Load evidence details"),
  );
}

function renderLoadDetailsButton(label) {
  return h("button", {
    type: "button",
    class: "metric-catalog-expand",
    disabled: state.fullSnapshotLoading,
    onclick: () => {
      loadFullSnapshotDetails().catch(renderError);
    },
  }, state.fullSnapshotLoading ? "Loading details..." : label);
}

function metricValueSummary(metricName, options = {}) {
  const spec = metricSpecForName(metricName);
  const snapshot = metricSnapshotForName(metricName, state.snapshot);
  const error = metricErrorForName(metricName);
  if (error && !snapshot) {
    return { spec, value: "--", detail: error, error: true };
  }
  if (!snapshot) {
    const summarySignal = metricSummarySignal(state.fullSnapshot || state.summarySnapshot || state.snapshot, spec);
    if (summarySignal) {
      return metricSignalSummary(spec, summarySignal);
    }
    return {
      spec,
      value: "--",
      detail: metricAvailable(metricName) ? "metric detected; values are loading" : "metric not present in this run set",
    };
  }
  const signal = options.mode === "latest" ? latestSignal(snapshot, spec) : bestSignal(snapshot, spec);
  if (!signal) {
    return { spec, value: "--", detail: "no scalar values found yet" };
  }
  return metricSignalSummary(spec, signal);
}

function metricSignalSummary(spec, signal) {
  const value = formatSignalValue(signal.raw_value ?? signal.value, spec);
  const detail = [
    signal.run_id ? `run ${signal.run_id}` : "",
    signal.run_group_id ? `group ${signal.run_group_id}` : "",
    signal.step ? `step ${signal.step}` : "",
    signal.source ? signal.source : "",
  ].filter(Boolean).join(" / ") || text(signal.value, "scalar metric");
  return { spec, value, detail, runID: signal.run_id || "", step: signal.step || "" };
}

function metricSpecForName(metricName) {
  const base = metricSpecFromSnapshot(state.summarySnapshot || state.snapshot || {}, metricName);
  const label = labelDisplayName(metricName);
  const measure = metricMeasureLabel(metricName);
  const goal = metricGoal(metricName);
  return {
    ...base,
    title: label && measure ? `${label} ${measure}` : base.title,
    family: metricFamilyOverride(metricName, base.family),
    format: base.format || "score",
    goal: base.goal || goal,
  };
}

function metricSnapshotForName(metricName, fallbackSnapshot) {
  const cached = snapshotForMetric(metricName);
  if (cached) {
    return cached;
  }
  if (fallbackSnapshot?.chart?.metric_name === metricName) {
    return fallbackSnapshot;
  }
  return null;
}

function metricErrorForName(metricName) {
  return state.featuredErrors.get(metricName) || state.presetMetricErrors.get(metricName) || "";
}

function metricAvailable(metricName) {
  return availableMetricNames(state.summarySnapshot || state.snapshot || {}).has(metricName);
}

function latestSignal(snapshot, spec) {
  const chart = snapshot?.chart;
  if (!chart?.has_data) {
    return null;
  }
  const candidates = [];
  for (const series of list(chart.series)) {
    if (state.hiddenRuns.has(series.run_id)) {
      continue;
    }
    const values = chartSeriesPointValues(series, "values");
    if (!values.length) {
      continue;
    }
    const latest = values.reduce((best, point) => point.step >= best.step ? point : best, values[0]);
    candidates.push({
      run_id: series.run_id,
      run_group_id: series.run_group_id,
      metric_name: chart.metric_name,
      value: latest.value,
      raw_value: latest.value,
      step: latest.step,
    });
  }
  if (!candidates.length) {
    return null;
  }
  candidates.sort((left, right) => right.step - left.step || compareMetricValues(left.raw_value, right.raw_value, spec.goal));
  return candidates[0];
}

function metricSummarySignal(snapshot, spec) {
  const summary = metricSummary(snapshot, spec.name);
  if (!summary?.group) {
    return null;
  }
  return {
    run_group_id: summary.group.run_group_id,
    metric_name: spec.name,
    value: summary.group.best,
    raw_value: numberValue(summary.group.best),
    source: "summary",
  };
}

function compareMetricValues(left, right, goal) {
  if (left === right) {
    return 0;
  }
  return goal === "minimize" ? left - right : right - left;
}

function researcherPresetMetricNames(snapshot) {
  const names = [];
  const primary = primaryMetricName(snapshot);
  if (primary) {
    names.push(primary);
  }
  const evalCurve = evalCurveMetricName(snapshot, primary);
  if (evalCurve) {
    names.push(evalCurve);
  }
  names.push(...validationQualityMetricNames(snapshot).slice(0, 8));
  names.push(...trainingHealthMetricNames(snapshot).slice(0, 4));
  names.push(...detectionSummaryMetricNames(snapshot));
  names.push(...errorAnalysisMetricNames(snapshot).slice(0, 8));
  for (const group of labelMetricGroups(snapshot)) {
    names.push(...group.metrics.slice(0, 1));
  }
  return normalizeMetricList(names).slice(0, maxResearchPresetMetrics);
}

function primaryMetricName(snapshot) {
  const available = availableMetricNames(snapshot);
  const exact = firstAvailableMetric(available, headlinePrimaryMetricOrder);
  if (exact) {
    return exact;
  }
  const ranked = sortedMetricNames(snapshot, isPrimaryQualityMetric);
  return ranked[0] || snapshot?.chart?.metric_name || "";
}

function evalCurveMetricName(snapshot, primaryMetric) {
  const available = availableMetricNames(snapshot);
  const exact = firstAvailableMetric(available, evalCurveMetricOrder);
  return exact && exact !== primaryMetric ? exact : "";
}

function validationQualityMetricNames(snapshot) {
  return sortedMetricNames(snapshot, isValidationQualityMetric);
}

function trainingHealthMetricNames(snapshot) {
  return sortedMetricNames(snapshot, isTrainingHealthMetric);
}

function errorAnalysisMetricNames(snapshot) {
  return sortedMetricNames(snapshot, isDetectionQualityMetric);
}

function detectionSummaryMetricNames(snapshot) {
  return detectionSummaryMetricOrder.filter((metricName) => availableMetricNames(snapshot).has(metricName));
}

function firstAvailableMetric(available, names) {
  return names.find((name) => available.has(name)) || "";
}

function sortedMetricNames(snapshot, predicate) {
  return [...availableMetricNames(snapshot)]
    .filter(predicate)
    .sort((left, right) => metricResearchPriority(left) - metricResearchPriority(right) || left.localeCompare(right));
}

function labelMetricGroups(snapshot) {
  const groups = new Map();
  for (const metricName of validationQualityMetricNames(snapshot)) {
    const label = labelDisplayName(metricName);
    if (!label) {
      continue;
    }
    if (!groups.has(label)) {
      groups.set(label, []);
    }
    groups.get(label).push(metricName);
  }
  return [...groups.entries()]
    .map(([label, metrics]) => ({
      label,
      metrics: metrics.sort((left, right) => labelMetricPriority(left) - labelMetricPriority(right) || left.localeCompare(right)),
    }))
    .sort((left, right) => medicalLabelOrder(left.label) - medicalLabelOrder(right.label) || left.label.localeCompare(right.label));
}

function environmentSummary(snapshot) {
  const systems = list(snapshot.runs).flatMap((run) => collectedSystems(run));
  const gpu = systems.find((field) => /gpu/i.test(field.name));
  const cluster = systems.find((field) => /cluster/i.test(field.name));
  return gpu?.value || cluster?.value || (isCompactPayload(snapshot) && !state.fullSnapshot ? "details deferred" : "not collected");
}

function dataManifestSummary(snapshot) {
  const dataMetric = [...availableMetricNames(snapshot)].find((name) => /^data\//i.test(name));
  if (dataMetric) {
    return shortMetricName(dataMetric);
  }
  const artifact = allRunEvidence(snapshot, "artifacts").find((item) => /manifest|dataset|data/i.test(artifactHaystack(item)));
  return artifact?.name || artifact?.artifact_id || "not recorded";
}

function reportArtifactSummary(artifacts) {
  const reports = artifacts.filter(isReportArtifact);
  return reports.length ? reports.length : "none";
}

function parseConfigPayload(config) {
  for (const raw of [config.normalized_json, config.indexed_fields]) {
    if (!raw) {
      continue;
    }
    try {
      return JSON.parse(raw);
    } catch {
      // Keep scanning other config representations.
    }
  }
  return null;
}

function firstConfigValue(payload, keys) {
  if (!payload || typeof payload !== "object") {
    return "";
  }
  for (const key of keys) {
    const value = nestedConfigValue(payload, key.split("."));
    if (value !== undefined && value !== null && value !== "") {
      if (typeof value === "object") {
        return JSON.stringify(value).slice(0, 80);
      }
      return text(value).slice(0, 80);
    }
  }
  return "";
}

function nestedConfigValue(value, path) {
  let current = value;
  for (const part of path) {
    if (!current || typeof current !== "object" || !(part in current)) {
      return undefined;
    }
    current = current[part];
  }
  return current;
}

function normalizedMetricName(metricName) {
  return text(metricName, "").toLowerCase().replace(/-/g, "_");
}

function metricNamespace(metricName) {
  return text(metricName, "").split("/")[0] || "metric";
}

function isPrimaryQualityMetric(metricName) {
  const name = normalizedMetricName(metricName);
  if (isLossLikeMetric(name)) {
    return false;
  }
  if (!/^(eval|final|detect|test|val|valid)\//.test(name)) {
    return false;
  }
  return /(auprc|score|reward|return|success_rate|win_rate|pass@1|pass_rate|exact_match|accuracy|macro_f1|\/f1|auroc|auc)/.test(name);
}

function isValidationQualityMetric(metricName) {
  const name = normalizedMetricName(metricName);
  if (!/^(eval|final|test|val|valid)\//.test(name)) {
    return false;
  }
  return /(auprc|auroc|auc|f1|accuracy|score|reward|return|success_rate|win_rate|pass@1|pass_rate|exact_match|brier|ece|calibration|loss|perplexity|cross_entropy|nll)/.test(name);
}

function isTrainingHealthMetric(metricName) {
  const name = normalizedMetricName(metricName);
  if (/^(train|status|checkpoint)\//.test(name)) {
    return /(loss|lr|learning_rate|throughput|examples|tokens|step_time|epoch|skipped|invalid|nan|inf|oom|checkpoint|runtime|status|grad)/.test(name);
  }
  return /(train\/loss|train\/lr|invalid.*label|skipped.*label|throughput|examples_seen)/.test(name);
}

function isDetectionQualityMetric(metricName) {
  const name = normalizedMetricName(metricName);
  if (/^detect\//.test(name)) {
    return true;
  }
  const errorTerms = /(false_positive|false_negative|fp\b|fn\b|sensitivity|specificity|precision|recall|threshold|confusion|worst)/;
  if (/^(eval|final)\//.test(name)) {
    return errorTerms.test(name);
  }
  return /(false_positive|false_negative|fp\b|fn\b|sensitivity|specificity|precision|recall|threshold|confusion|calibration|brier|ece|worst)/.test(name);
}

function isLossLikeMetric(metricName) {
  return /loss|perplexity|cross_entropy|negative_log_likelihood|nll|brier|ece|error|invalid|skipped/i.test(metricName);
}

function isStatusProblemMetric(metricName) {
  return /invalid|skipped|nan|inf|oom|failed|error/i.test(metricName);
}

function metricGoal(metricName) {
  return isLossLikeMetric(metricName) || /latency|time|duration/i.test(metricName) ? "minimize" : "maximize";
}

function metricFamilyOverride(metricName, fallback) {
  if (/brier|ece|calibration/i.test(metricName)) {
    return "Calibration";
  }
  if (isDetectionQualityMetric(metricName)) {
    return "Error analysis";
  }
  if (isPrimaryQualityMetric(metricName)) {
    return "Outcome";
  }
  if (isValidationQualityMetric(metricName)) {
    return "Validation quality";
  }
  if (isTrainingHealthMetric(metricName)) {
    return "Training health";
  }
  return fallback;
}

function metricMeasureLabel(metricName) {
  const name = normalizedMetricName(metricName);
  const measures = [
    ["success_rate", "success rate"],
    ["win_rate", "win rate"],
    ["pass@1", "pass@1"],
    ["pass_rate", "pass rate"],
    ["exact_match", "exact match"],
    ["auprc", "AUPRC"],
    ["auroc", "AUROC"],
    ["reward", "reward"],
    ["mean_episode_return", "mean episode return"],
    ["episode_return", "episode return"],
    ["return", "return"],
    ["score", "score"],
    ["specificity", "specificity"],
    ["sensitivity", "sensitivity"],
    ["precision", "precision"],
    ["recall", "recall"],
    ["accuracy", "accuracy"],
    ["macro_f1", "F1"],
    ["f1", "F1"],
    ["brier", "Brier"],
    ["ece", "ECE"],
    ["perplexity", "perplexity"],
  ];
  return measures.find(([token]) => name.includes(token))?.[1] || shortMetricName(metricName);
}

function labelMetricPriority(metricName) {
  const measure = metricMeasureLabel(metricName);
  const index = ["AUPRC", "mean episode return", "episode return", "return", "reward", "success rate", "win rate", "pass@1", "pass rate", "exact match", "accuracy", "score", "AUROC", "F1", "sensitivity", "specificity", "precision", "recall", "Brier", "ECE", "perplexity"].indexOf(measure);
  return index === -1 ? 999 : index;
}

function labelDisplayName(metricName) {
  const normalized = normalizedLabelText(metricName);
  for (const [key, label] of commonMedicalLabelDisplayNames) {
    if (normalized.includes(normalizedLabelText(key))) {
      return label;
    }
  }
  return "";
}

function normalizedLabelText(value) {
  return text(value, "").toLowerCase().replace(/[^a-z0-9]+/g, "");
}

function medicalLabelOrder(label) {
  const order = ["Atelectasis", "Cardiomegaly", "Consolidation", "Edema", "Pleural Effusion"];
  const index = order.indexOf(label);
  return index === -1 ? 999 : index;
}

function isPredictionSummaryArtifact(artifact) {
  return /prediction|false[-_ ]?(positive|negative)|confusion|threshold|calibration|error[-_ ]?analysis|classification[-_ ]?report|summary/i.test(artifactHaystack(artifact));
}

function availableMetricNames(snapshot) {
  return new Set((snapshot.metric_options || []).map((option) => option.name));
}

function defaultMetricSpecs(snapshot) {
  const available = availableMetricNames(snapshot);
  const known = featuredMetricCatalog.filter((spec) => available.has(spec.name));
  const graphDefaults = graphDefaultMetricOrder
    .filter((name) => available.has(name))
    .map((name) => metricSpecFromSnapshot(snapshot, name));
  const graphDefaultNames = new Set(graphDefaults.map((spec) => spec.name));
  const knownNames = new Set(known.map((spec) => spec.name));
  const inferred = (snapshot.metric_options || [])
    .filter((option) => !knownNames.has(option.name))
    .map((option) => metricSpecFromSnapshot(snapshot, option.name));
  const prioritized = inferred.filter((spec) => metricResearchPriority(spec.name) < 100);
  const presetNames = new Set(researcherPresetMetricNames(snapshot));
  const preset = [...presetNames]
    .filter((name) => !graphDefaultNames.has(name))
    .map((name) => metricSpecFromSnapshot(snapshot, name));
  const rest = uniqueMetricSpecs([...preset, ...known, ...(prioritized.length ? prioritized : inferred)])
    .filter((spec) => !graphDefaultNames.has(spec.name))
    .sort((left, right) => metricResearchPriority(left.name) - metricResearchPriority(right.name));
  return uniqueMetricSpecs([...graphDefaults, ...rest])
    .filter((spec) => !hasMeanAlternative(spec.name, available))
    .slice(0, maxPinnedMetrics);
}

function uniqueMetricSpecs(specs) {
  const seen = new Set();
  const out = [];
  for (const spec of specs) {
    if (!spec?.name || seen.has(spec.name)) {
      continue;
    }
    seen.add(spec.name);
    out.push(spec);
  }
  return out;
}

function ensureSelectedMetrics(snapshot, options = {}) {
  const available = availableMetricNames(snapshot);
  const hadSelection = state.selectedMetrics.length > 0;
  if (options.preserveSelection) {
    state.selectedMetrics = normalizeMetricList(state.selectedMetrics);
    state.metricSelectionInitialized = true;
    if (!state.metric && state.selectedMetrics.length) {
      state.metric = state.selectedMetrics[0];
    }
    return;
  }
  const current = normalizeMetricList(state.selectedMetrics).filter((name) => available.has(name));
  if (!hadSelection && !state.metricSelectionInitialized) {
    const defaults = normalizeMetricList([
      state.metric && available.has(state.metric) ? state.metric : "",
      ...defaultMetricSpecs(snapshot).map((spec) => spec.name),
    ]);
    state.selectedMetrics = defaults;
    if (!hasInitialMetricParam && defaults.length && (!state.metric || shouldPromoteResearchMetric(state.metric, defaults))) {
      state.metric = defaults[0];
    }
    state.metricSelectionInitialized = true;
    return;
  }
  if (!hadSelection) {
    state.metricSelectionInitialized = true;
    return;
  }
  if (state.metric && available.has(state.metric) && !current.includes(state.metric)) {
    current.unshift(state.metric);
  }
  state.selectedMetrics = current.length ? normalizeMetricList(current) : normalizeMetricList(state.selectedMetrics);
  state.metricSelectionInitialized = true;
}

function shouldPromoteResearchMetric(metricName, defaults) {
  if (!defaults.length) {
    return false;
  }
  return ["eval/mean_episode_return", "eval/score", "persona_composite"].includes(metricName) && defaults[0] !== metricName;
}

function hasMeanAlternative(metricName, available) {
  return metricName.endsWith("/count") && available.has(metricName.replace(/\/count$/, "/mean"));
}

function metricResearchPriority(metricName) {
  const priorities = new Map([
    ["final/macro_auprc", 5],
    ["detect/macro_auprc", 6],
    ["eval/macro_auprc", 7],
    ["final/macro_auroc", 8],
    ["detect/macro_auroc", 9],
    ["eval/macro_auroc", 10],
    ["final/macro_f1", 11],
    ["eval/macro_f1", 12],
    ["eval/brier", 13],
    ["final/brier", 14],
    ["eval/ece", 15],
    ["final/ece", 16],
    ["detect/macro_sensitivity", 17],
    ["detect/macro_specificity", 18],
    ["detect/macro_precision", 19],
    ["detect/macro_f1", 20],
    ["detect/macro_accuracy", 21],
    ["detect/accuracy", 22],
    ["final/mean_episode_return", 23],
    ["eval/mean_episode_return", 24],
    ["final/reward", 25],
    ["eval/reward", 26],
    ["final/return", 27],
    ["eval/return", 28],
    ["final/success_rate", 29],
    ["eval/success_rate", 30],
    ["final/win_rate", 31],
    ["eval/win_rate", 32],
    ["final/pass_rate", 33],
    ["eval/pass_rate", 34],
    ["final/exact_match", 35],
    ["eval/exact_match", 36],
    ["final/accuracy", 37],
    ["eval/accuracy", 38],
    ["final/score", 39],
    ["eval/score", 40],
    ["eval/aime2025/pass@1", 10],
    ["eval/aime2025/avg@1", 11],
    ["eval/aime2025/failed_rollouts", 20],
    ["eval/aime2025/no_response/mean", 21],
    ["eval/aime2025/no_response/count", 22],
    ["eval/aime2025/is_truncated/mean", 23],
    ["eval/aime2025/completion_len/mean", 30],
    ["inference/decode_throughput_tps", 40],
    ["inference/completed_requests_per_s", 41],
    ["system/gpu_utilization", 50],
    ["eval/aime2025/time", 51],
    ["inference/prefill_throughput_tps", 60],
    ["train/loss", 70],
    ["train/lr", 71],
    ["train/grad_norm", 72],
    ["train/step_time_s", 73],
    ["train/examples_seen", 74],
    ["train/input_tokens", 75],
    ["train/tokens", 76],
    ["gpu/memory_allocated_gb", 77],
    ["gpu/memory_reserved_gb", 78],
    ["gpu/max_memory_allocated_gb", 79],
    ["checkpoint/file_count", 80],
    ["checkpoint/bytes", 81],
    ["inference/time_s", 82],
    ["persona_composite", 91],
  ]);
  if (priorities.has(metricName)) {
    return priorities.get(metricName);
  }
  if (isPrimaryQualityMetric(metricName)) {
    return 12;
  }
  if (isValidationQualityMetric(metricName)) {
    return 18;
  }
  if (isDetectionQualityMetric(metricName)) {
    return 24;
  }
  if (isTrainingHealthMetric(metricName)) {
    return 34;
  }
  if (/^eval\/.*(pass|avg|score|accuracy|reward)/i.test(metricName)) {
    return 15;
  }
  if (/no_response|failed|error|truncat/i.test(metricName)) {
    return 25;
  }
  if (/completion_len|length|tokens/i.test(metricName)) {
    return 35;
  }
  if (/lr|grad_norm|step_time|examples_seen/i.test(metricName)) {
    return 38;
  }
  if (/throughput|requests|latency|time/i.test(metricName)) {
    return 45;
  }
  if (/gpu|utilization|memory/i.test(metricName)) {
    return 65;
  }
  if (/checkpoint/i.test(metricName)) {
    return 80;
  }
  if (/^feature\//i.test(metricName)) {
    return 85;
  }
  if (/radgraph|bertscore|bleu|rouge|caption|report|hallucination|grounded|vqa|retrieval|recall|map|embedding|probe|finding|diagnosis|opacity|quality|image/i.test(metricName)) {
    return 18;
  }
  return 100;
}

function selectedMetricSpecs(snapshot) {
  const available = availableMetricNames(snapshot);
  return state.selectedMetrics
    .filter((name) => available.has(name))
    .map((name) => metricSpecFromSnapshot(snapshot, name));
}

function featuredMetricSpecs(snapshot) {
  return selectedMetricSpecs(snapshot);
}

function metricSpecFromSnapshot(snapshot, metricName) {
  const option = (snapshot.metric_options || []).find((item) => item.name === metricName);
  const known = featuredMetricCatalog.find((spec) => spec.name === metricName);
  if (known) {
    return { ...known };
  }
  return {
    name: metricName,
    title: shortMetricName(metricName),
    family: metricFamilyOverride(metricName, metricFamily(metricName, option)),
  };
}

function metricSpec(metricName) {
  return featuredMetricCatalog.find((spec) => spec.name === metricName) || metricSpecFromSnapshot(state.snapshot, metricName);
}

function snapshotForMetric(metricName) {
  return state.featuredSnapshots.get(metricName) || state.presetMetricSnapshots.get(metricName);
}

function isCompactPayload(snapshot) {
  return ["summary", "metric"].includes(text(snapshot?.payload_mode, ""));
}

async function loadFullSnapshotDetails() {
  if (state.fullSnapshotLoading) {
    return;
  }
  state.fullSnapshotLoading = true;
  state.fullSnapshotError = "";
  render();
  try {
    const metric = state.metric || state.selectedMetrics[0] || "";
    const snapshot = await fetchSnapshotFor(metric);
    state.fullSnapshot = snapshot;
    if (snapshot.chart?.metric_name) {
      state.featuredSnapshots.set(snapshot.chart.metric_name, snapshot);
    }
  } catch (error) {
    state.fullSnapshotError = error.message || String(error);
  } finally {
    state.fullSnapshotLoading = false;
    render();
  }
}

function metricSummary(snapshot, metricName) {
  if (!snapshot) {
    return null;
  }
  for (const card of snapshot.cards || []) {
    for (const metric of card.metrics || []) {
      if (metric.name !== metricName) {
        continue;
      }
      const groups = [...(metric.groups || [])].sort((left, right) => (right.best_value || 0) - (left.best_value || 0));
      return { card: card.name, metric, group: groups[0] };
    }
  }
  return null;
}

function bestSignal(snapshot, spec) {
  if (!snapshot) {
    return null;
  }
  const runs = (snapshot.sweep?.runs || [])
    .filter((run) => (!state.group || run.run_group_id === state.group) && !state.hiddenRuns.has(run.run_id))
    .map((run) => ({ run, value: numberValue(run.metric) }))
    .filter((entry) => entry.value !== null);
  if (runs.length) {
    runs.sort((left, right) => spec.goal === "minimize" ? left.value - right.value : right.value - left.value);
    const best = runs[0];
    return {
      run_id: best.run.run_id,
      run_group_id: best.run.run_group_id,
      metric_name: spec.name,
      value: best.run.metric,
      raw_value: best.value,
    };
  }
  const summary = metricSummary(snapshot, spec.name);
  if (summary?.group) {
    return {
      run_group_id: summary.group.run_group_id,
      metric_name: spec.name,
      value: summary.group.best,
      raw_value: numberValue(summary.group.best),
    };
  }
  return snapshot.sweep?.best_run || null;
}

function metricValueForRun(metricName, runID) {
  const snapshot = snapshotForMetric(metricName);
  const sweepRun = (snapshot?.sweep?.runs || []).find((run) => run.run_id === runID);
  return sweepRun?.metric ?? "";
}

function shortMetricName(name) {
  const known = featuredMetricCatalog.find((spec) => spec.name === name);
  if (known) {
    return known.title;
  }
  return text(name).split("/").slice(-2).join("/");
}

function metricFamily(metricName, option) {
  const known = featuredMetricCatalog.find((spec) => spec.name === metricName);
  if (known?.family) {
    return known.family;
  }
  const card = text(option?.Card || option?.card, "");
  if (card && !/^other metrics?$|^metric$/i.test(card)) {
    return card;
  }
  const firstSegment = text(metricName, "").split("/")[0];
  return firstSegment ? titleCase(firstSegment) : "Other";
}

function titleCase(value) {
  return text(value, "Other").replace(/[-_]+/g, " ").replace(/\b\w/g, (char) => char.toUpperCase());
}

function groupedMetricOptions(snapshot, options = {}) {
  const query = state.metricSearch.trim().toLowerCase();
  const familyFilter = text(options.family, "");
  const groups = new Map();
  for (const option of snapshot.metric_options || []) {
    const spec = metricSpecFromSnapshot(snapshot, option.name);
    const key = spec.family || "Other";
    if (query && !metricMatchesSearch(spec, key, query)) {
      continue;
    }
    if (familyFilter && key !== familyFilter) {
      continue;
    }
    if (!groups.has(key)) {
      groups.set(key, []);
    }
    groups.get(key).push(spec);
  }
  return [...groups.entries()]
    .map(([family, specs]) => ({
      family,
      specs: specs.sort((left, right) => metricResearchPriority(left.name) - metricResearchPriority(right.name) || left.name.localeCompare(right.name)),
    }))
    .sort((left, right) => {
      if (left.family === "Other") {
        return 1;
      }
      if (right.family === "Other") {
        return -1;
      }
      return left.family.localeCompare(right.family);
    });
}

function metricMatchesSearch(spec, family, query) {
  return [spec.name, spec.title, family].some((part) => text(part, "").toLowerCase().includes(query));
}

function metricCatalogCount(groups) {
  return groups.reduce((total, group) => total + group.specs.length, 0);
}

function effectiveMetricFamily(groups) {
  return groups.some((group) => group.family === state.activeMetricFamily) ? state.activeMetricFamily : "";
}

function visibleMetricSpecsForGroups(groups) {
  return groups.flatMap((group) => group.specs).slice(0, maxPinnedMetrics);
}

function pinVisibleMetricGroups(groups) {
  const metrics = visibleMetricSpecsForGroups(groups).map((spec) => spec.name);
  if (!metrics.length) {
    return;
  }
  state.selectedMetrics = normalizeMetricList(metrics);
  if (!state.metric || !metrics.includes(state.metric)) {
    state.metric = metrics[0];
  }
  updateMetricSelectionInPlace();
}

function familyClass(family) {
  return text(family, "metric").toLowerCase().replace(/[^a-z0-9]+/g, "-");
}

const runPalette = ["#3a86ff", "#ff006e", "#06d6a0", "#8338ec", "#ffbe0b", "#fb5607", "#2fb36d", "#ef5da8"];

function colorForToken(token) {
  let hash = 0;
  for (const char of text(token, "run")) {
    hash = ((hash << 5) - hash + char.charCodeAt(0)) | 0;
  }
  return runPalette[Math.abs(hash) % runPalette.length];
}

function numberValue(value) {
  if (typeof value === "number") {
    return Number.isFinite(value) ? value : null;
  }
  const parsed = Number.parseFloat(text(value, "").replace(/,/g, ""));
  return Number.isFinite(parsed) ? parsed : null;
}

function formatSignalValue(value, spec) {
  const numeric = numberValue(value);
  if (numeric === null) {
    return text(value);
  }
  switch (spec.format) {
    case "percent":
      return `${(numeric * 100).toFixed(numeric >= 0.1 ? 1 : 2)}%`;
    case "percent-100":
      return `${numeric.toFixed(numeric >= 10 ? 0 : 1)}%`;
    case "integer":
      return Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(numeric);
    case "rate":
      return Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(numeric);
    case "small-rate":
      return numeric.toFixed(numeric < 1 ? 3 : 2);
    case "duration":
      return `${numeric.toFixed(numeric >= 10 ? 1 : 2)}s`;
    case "loss":
      return numeric.toFixed(3);
    case "score":
      return numeric.toFixed(numeric >= 10 ? 1 : 3);
    default:
      if (Math.abs(numeric) >= 1000) {
        return Intl.NumberFormat(undefined, { maximumFractionDigits: 1, notation: "compact" }).format(numeric);
      }
      if (Math.abs(numeric) >= 10) {
        return numeric.toFixed(1);
      }
      return numeric.toFixed(3);
  }
}

function formatTableValue(value, spec) {
  if (value === null || value === undefined || value === "") {
    return "--";
  }
  return formatSignalValue(value, spec);
}

function filteredRuns(snapshot, options = {}) {
  const query = state.search.trim().toLowerCase();
  const runs = allRuns(snapshot).filter((run) => {
    if (state.group && run.run_group_id !== state.group) {
      return false;
    }
    if (state.lifecycleFilter && runLifecycle(run) !== state.lifecycleFilter) {
      return false;
    }
    if (!runMatchesUpdatedFilter(run)) {
      return false;
    }
    if (!options.includeHidden && state.hiddenRuns.has(run.run_id)) {
      return false;
    }
    if (!query) {
      return true;
    }
    return searchableRunParts(run).some((part) => text(part, "").toLowerCase().includes(query));
  });
  return sortRunsByUpdated(runs);
}

function allRuns(snapshot) {
  const runs = [];
  const seen = new Set();
  for (const run of [...list(snapshot?.runs), ...state.additionalRuns]) {
    if (!run?.run_id || seen.has(run.run_id)) {
      continue;
    }
    seen.add(run.run_id);
    runs.push(run);
  }
  return runs;
}

function filteredRunIDs(snapshot, options = {}) {
  return new Set(filteredRuns(snapshot, options).map((run) => run.run_id).filter(Boolean));
}

function filteredRunIDSetForChart(chart) {
  const snapshot = snapshotForMetric(chart?.metric_name) || state.snapshot;
  return filteredRunIDs(snapshot);
}

function searchableRunParts(run) {
  const tagParts = Object.entries(run.tags || {}).flatMap(([key, value]) => [key, value, `${key}=${value}`]);
  return [
    run.run_id,
    run.project,
    run.experiment_id,
    run.run_group_id,
    run.state,
    run.lifecycle_state,
    run.owner,
    run.updated_at,
    run.created_at,
    run.started_at,
    run.completed_at,
    ...(run.metric_names || []),
    ...tagParts,
  ];
}

function normalizeRunUpdatedFilter(value) {
  const normalized = text(value, "");
  return runUpdatedFilterOptions.some((option) => option.id === normalized) ? normalized : "";
}

function normalizeRunUpdatedSort(value) {
  const normalized = text(value, "");
  return runUpdatedSortOptions.some((option) => option.id === normalized) ? normalized : "";
}

function runMatchesUpdatedFilter(run) {
  const filter = normalizeRunUpdatedFilter(state.runUpdatedFilter);
  if (!filter) {
    return true;
  }
  const updatedMs = runLastUpdatedMs(run);
  if (filter === "missing") {
    return !updatedMs;
  }
  if (!updatedMs) {
    return false;
  }
  const option = runUpdatedFilterOptions.find((candidate) => candidate.id === filter);
  return option?.durationMs ? Date.now() - updatedMs <= option.durationMs : true;
}

function sortRunsByUpdated(runs) {
  const sortMode = normalizeRunUpdatedSort(state.runUpdatedSort) || (state.runUpdatedFilter ? "desc" : "");
  if (!sortMode) {
    return runs;
  }
  return runs.slice().sort((left, right) => {
    const leftMs = runLastUpdatedMs(left);
    const rightMs = runLastUpdatedMs(right);
    if (leftMs !== rightMs) {
      return sortMode === "asc" ? leftMs - rightMs : rightMs - leftMs;
    }
    return text(left.run_id, "").localeCompare(text(right.run_id, ""));
  });
}

function runLastUpdatedTimestamp(run) {
  return text(run.updated_at || run.completed_at || run.started_at || run.created_at, "");
}

function runLastUpdatedMs(run) {
  const timestamp = runLastUpdatedTimestamp(run);
  if (!timestamp) {
    return 0;
  }
  const parsed = Date.parse(timestamp);
  return Number.isFinite(parsed) ? parsed : 0;
}

function runLastUpdatedLabel(run) {
  const updatedMs = runLastUpdatedMs(run);
  return updatedMs ? `updated ${formatShortDateTime(updatedMs)}` : "updated --";
}

function runLastUpdatedTitle(run) {
  const updatedMs = runLastUpdatedMs(run);
  return updatedMs ? `Last updated ${new Date(updatedMs).toLocaleString()}` : "Last updated timestamp not available";
}

function runLifecycle(run) {
  const authoritative = text(run.outcome_state || run.liveness_state, "").toLowerCase();
  if (authoritative) {
    return authoritative;
  }
  const legacy = text(run.lifecycle_state, run.successful ? "succeeded" : run.state || "pending").toLowerCase();
  return legacy === "stale" ? "not_responding" : legacy;
}

function normalizeLifecycleFilter(value) {
  const normalized = text(value, "").trim().toLowerCase();
  return normalized === "stale" ? "not_responding" : normalized;
}

function runLifecycleLabel(run) {
  const lifecycle = runLifecycle(run);
  return lifecycle === "not_responding" ? "Not responding" : lifecycle.replaceAll("_", " ");
}

function runSuccessTitle(run) {
  const details = [`Lifecycle: ${runLifecycleLabel(run)}`];
  if (run.lifecycle_reason) {
    details.push(text(run.lifecycle_reason));
  }
  if (run.lifecycle_source) {
    details.push(`source ${text(run.lifecycle_source)}`);
  }
  if (run.last_evidence_at) {
    details.push(`last evidence ${text(run.last_evidence_at)}`);
  }
  const reasons = list(run.success_reasons);
  if (reasons.length) {
    details.push(`scientific classification: ${reasons.join("; ")}`);
  }
  return details.join(" \u00b7 ");
}

function setFocusMetric(metricName) {
  state.metric = metricName;
  state.focusedSeriesError = "";
  pinMetric(metricName);
  updateMetricSelectionInPlace();
}

function pinMetric(metricName) {
  state.selectedMetrics = normalizeMetricList([metricName, ...state.selectedMetrics]);
}

function togglePinnedMetric(metricName) {
  if (state.selectedMetrics.includes(metricName)) {
    state.selectedMetrics = state.selectedMetrics.filter((name) => name !== metricName);
    if (state.metric === metricName) {
      state.metric = state.selectedMetrics[0] || "";
    }
  } else {
    pinMetric(metricName);
  }
  updateMetricSelectionInPlace();
}

function renderMetricDashboardPanel(snapshot) {
  const specs = selectedMetricSpecs(snapshot);
  const visibleSpecs = visiblePinnedMetricSpecs(snapshot);
  const hiddenCount = specs.length - visibleSpecs.length;
  const body = h("div", { class: "metric-dashboard graph-dashboard" },
    specs.length
      ? h("div", { class: "metric-card-grid graph-card-grid uniform-chart-grid" }, visibleSpecs.map((spec) => renderPinnedMetricCard(spec, {
          uniform: true,
        })))
      : h("p", { class: "empty" }, "No chart metrics are pinned. Stellar will choose graph-first defaults such as train/loss and validation/detection quality metrics when they are available."),
    hiddenCount > 0 ? h("button", {
      type: "button",
      class: "chart-grid-more",
      onclick: () => {
        state.showAllPinnedCharts = true;
        render();
        loadVisibleMetricSnapshots();
      },
    }, `Show all ${specs.length} pinned charts`) : state.showAllPinnedCharts && specs.length > 4 ? h("button", {
      type: "button",
      class: "chart-grid-more",
      onclick: () => {
        state.showAllPinnedCharts = false;
        render();
      },
    }, "Show review charts only") : null,
    h("div", { class: "dashboard-summary compact" },
      statPill("focused", state.metric || "none"),
      statPill("charts", `${visibleSpecs.length}/${specs.length || maxPinnedMetrics}`),
      statPill("available metrics", list(snapshot.metric_options).length),
    ),
    renderDensityStrip(snapshot),
  );
  return configuredPanel("charts", "Scalar chart grid", "Training, validation, and detection metrics are the primary dashboard surface.", body, "full metric-dashboard-panel graph-first-chart-panel");
}

function visiblePinnedMetricSpecs(snapshot) {
  const specs = selectedMetricSpecs(snapshot);
  if (state.showAllPinnedCharts || specs.length <= 4) {
    return specs;
  }
  return specs.slice(0, 4);
}

function renderMetricCatalogPanel(snapshot) {
  const specs = selectedMetricSpecs(snapshot);
  const familyGroups = groupedMetricOptions(snapshot);
  const activeFamily = effectiveMetricFamily(familyGroups);
  const metricGroups = groupedMetricOptions(snapshot, { family: activeFamily });
  const matchingMetrics = metricCatalogCount(metricGroups);
  const totalMetrics = list(snapshot.metric_options).length;
  const body = h("div", { class: "metric-dashboard metric-catalog-dashboard" },
    h("div", { class: "dashboard-summary" },
      statPill("focused", state.metric || "none"),
      statPill("pinned", `${specs.length}/${maxPinnedMetrics}`),
      statPill("available metrics", totalMetrics),
    ),
    renderMetricFamilyTabs(familyGroups, activeFamily),
    h("div", { class: "metric-catalog-toolbar" },
      h("input", {
        type: "search",
        value: state.metricSearch,
        placeholder: "Search metrics, families, or cards",
        "data-metric-search-input": true,
        oninput: (event) => {
          state.metricSearch = event.target.value;
          render({ focusMetricSearch: true });
        },
      }),
      h("span", {}, state.metricSearch ? `${matchingMetrics}/${totalMetrics} metrics` : activeFamily ? `${matchingMetrics}/${totalMetrics} metrics` : `${totalMetrics} metrics`),
      matchingMetrics ? h("button", {
        type: "button",
        class: "metric-catalog-expand",
        title: `Pin up to ${maxPinnedMetrics} metrics from the current ${activeFamily || "catalog"} view`,
        onclick: () => pinVisibleMetricGroups(metricGroups),
      }, `Pin visible ${Math.min(matchingMetrics, maxPinnedMetrics)}`) : null,
    ),
    h("div", { class: "metric-catalog" },
      matchingMetrics
        ? metricGroups.map((group) => renderMetricCatalogGroup(group))
        : h("p", { class: "empty" }, "No metrics match the current search."),
    ),
  );
  return configuredPanel("catalog", "Metric catalog", "Search and pin additional charts after the graph-first defaults.", body, "full metric-catalog-panel");
}

function renderMetricFamilyTabs(groups, activeFamily) {
  const matchingCount = metricCatalogCount(groups);
  if (!groups.length) {
    return null;
  }
  return h("div", { class: "metric-family-tabs", role: "tablist", "aria-label": "Metric families" },
    h("button", {
      type: "button",
      class: `metric-family-tab ${activeFamily ? "" : "active"}`.trim(),
      role: "tab",
      "aria-selected": !activeFamily,
      onclick: () => {
        state.activeMetricFamily = "";
        render();
      },
    },
    h("span", {}, "All metrics"),
    h("em", {}, matchingCount)),
    ...groups.map((group) => h("button", {
      type: "button",
      class: `metric-family-tab ${activeFamily === group.family ? "active" : ""}`.trim(),
      role: "tab",
      "aria-selected": activeFamily === group.family,
      title: group.family,
      onclick: () => {
        state.activeMetricFamily = group.family;
        render();
      },
    },
    h("span", {}, group.family),
    h("em", {}, group.specs.length))),
  );
}

function renderPinnedMetricCard(spec, options = {}) {
  const snapshot = snapshotForMetric(spec.name);
  const chart = snapshot?.chart;
  const dataset = chartDataset(chart, { runIDs: filteredRunIDs(snapshot) });
  const singlePoint = dataset.length > 0 && dataset.every((series) => series.points.length === 1);
  const classes = [
    "metric-card",
    options.large ? "large" : "",
    options.hero ? "hero" : "",
    options.reviewSecondary ? "review-secondary" : "",
    options.uniform ? "uniform" : "",
    state.metric === spec.name ? "focused" : "",
    !snapshot || state.featuredErrors.has(spec.name) || !dataset.length ? "muted" : "",
  ].filter(Boolean).join(" ");
  return h("article", { class: classes },
    h("div", { class: "metric-card-head" },
      h("button", {
        type: "button",
        class: "metric-card-title",
        title: `Focus ${spec.name}`,
        "aria-pressed": state.metric === spec.name,
        onclick: () => setFocusMetric(spec.name),
      },
      h("span", {}, spec.title),
      h("em", {}, spec.name)),
      h("button", {
        type: "button",
        class: "metric-pin-button active",
        title: `Unpin ${spec.name}`,
        onclick: (event) => {
          event.stopPropagation();
          togglePinnedMetric(spec.name);
        },
      }, "Pinned"),
    ),
    renderMetricCardBody(spec, snapshot, dataset, singlePoint, options),
    renderMetricCardDensity(chart, { runIDs: filteredRunIDs(snapshot) }),
  );
}

function renderMetricCardBody(spec, snapshot, dataset, singlePoint, options = {}) {
  if (state.featuredErrors.has(spec.name) && !snapshot) {
    return h("p", { class: "metric-card-state" }, state.featuredErrors.get(spec.name));
  }
  if (!snapshot) {
    return h("p", { class: "metric-card-state" }, "Loading chart...");
  }
  if (!dataset.length) {
    return h("p", { class: "metric-card-state" }, activeRunFilterLabels(snapshot).length ? "No points match the active run filters." : "No points yet.");
  }
  if (singlePoint) {
    const points = dataset.flatMap((series) => series.points.map((point) => ({ series, point })));
    const latest = points.sort((left, right) => right.point.step - left.point.step)[0];
    return h("div", { class: "metric-value-tile" },
      h("strong", {}, formatSignalValue(latest.point.value, spec)),
      h("span", {}, `${latest.series.runID} at step ${formatAxisValue(latest.point.step)}`),
    );
  }
  const fullChart = Boolean(options.large || options.uniform);
  return renderMetricChart(snapshot.chart, {
    className: options.uniform ? "dashboard-medium-chart" : options.large ? "dashboard-large-chart" : "dashboard-mini-chart",
    compact: !fullChart,
    metricName: spec.name,
    showLegend: fullChart,
    runIDs: filteredRunIDs(snapshot),
  });
}

function renderMetricCatalogGroup(group) {
  const expanded = state.expandedMetricFamilies.has(group.family);
  const visibleSpecs = expanded ? group.specs : group.specs.slice(0, collapsedMetricOptionsPerGroup);
  const hiddenCount = group.specs.length - visibleSpecs.length;
  return h("section", { class: "metric-catalog-group" },
    h("h3", {},
      h("span", {}, group.family),
      h("em", {}, `${group.specs.length} metrics`),
    ),
    h("div", { class: "metric-option-grid" },
      visibleSpecs.map((spec) => renderMetricOptionButton(spec)),
    ),
    hiddenCount > 0
      ? h("button", {
          type: "button",
          class: "metric-catalog-expand",
          onclick: () => {
            state.expandedMetricFamilies.add(group.family);
            render();
          },
        }, `Show ${hiddenCount} more ${group.family} metrics`)
      : expanded && group.specs.length > collapsedMetricOptionsPerGroup
        ? h("button", {
            type: "button",
            class: "metric-catalog-expand",
            onclick: () => {
              state.expandedMetricFamilies.delete(group.family);
              render();
            },
          }, `Collapse ${group.family} metrics`)
        : null,
  );
}

function renderMetricOptionButton(spec) {
  const pinned = state.selectedMetrics.includes(spec.name);
  const focused = state.metric === spec.name;
  return h("button", {
    type: "button",
    class: `metric-option-button ${focused ? "focused" : ""} ${pinned ? "pinned" : ""}`.trim(),
    title: `${pinned ? "Pinned" : "Focus and pin"} ${spec.name}`,
    "aria-pressed": focused || pinned,
    onclick: () => setFocusMetric(spec.name),
  },
  h("span", {}, spec.title),
  h("em", {}, pinned ? "pinned" : spec.name));
}

function renderLinePanel(snapshot) {
  const focusedMetric = state.metric || snapshot?.chart?.metric_name || primaryMetricName(snapshot);
  const focusedSnapshot = focusedMetric ? metricSnapshotForName(focusedMetric, snapshot) : snapshot;
  if (!focusedMetric) {
    return configuredPanel("timeline", "Selected metric timeline", "Pick a metric from the dashboard to focus this chart.", h("p", { class: "empty" }, "Dashboard mode starts without a focused metric so multiple pinned metrics are visible first."), "wide");
  }
  const chart = focusedSnapshot?.chart || {};
  if (!chart.has_data || !(chart.series || []).length) {
    return configuredPanel("timeline", "Selected metric timeline", text(chart.metric_name, focusedMetric), h("p", { class: "empty" }, "No time-series points for this metric."), "wide");
  }
  return configuredPanel("timeline", "Selected metric timeline", `TimeSeries ${text(chart.metric_name)}`,
    h("div", { class: "focused-chart-stack" },
      renderFocusedSeriesToolbar(chart),
      renderMetricChart(chart, {
        className: "selected-line-chart",
        compact: false,
        metricName: chart.metric_name,
        showLegend: true,
        brush: true,
      }),
    ),
    "wide",
  );
}

function renderFocusedSeriesToolbar(chart) {
  const rawPoints = totalRawChartPoints(chart);
  const renderedPoints = totalRenderedChartPoints(chart);
  const cacheHit = Boolean(cachedFocusedSeries(chart.metric_name));
  const detailLoaded = renderedPoints > defaultChartRenderedPointBudget;
  return h("div", { class: "focused-series-toolbar" },
    renderDensityStrip({ chart }, { compact: true }),
    renderChartSmoothingPill(chart),
    renderChartZoomPill(),
    renderRawMetricQueryAction(chart),
    state.focusedSeriesError ? h("span", { class: "error-text" }, state.focusedSeriesError) : null,
    rawPoints ? renderFocusedSeriesControls(chart, { cacheHit, detailLoaded }) : null,
  );
}

// renderChartZoomPill advertises the drag-to-zoom affordance and, when a step
// window is active, doubles as a one-click reset. It reads the same
// focusedSeriesControls the brush and the toolbar form write, so it stays in
// sync with either input path.
function renderChartZoomPill() {
  const controls = state.focusedSeriesControls;
  const start = text(controls.startStep, "").trim();
  const end = text(controls.endStep, "").trim();
  const zoomed = Boolean(start || end);
  if (!zoomed) {
    return h("span", {
      class: "zoom-pill",
      title: "Drag across the chart to zoom into a step window; double-click the chart to reset.",
    }, "Drag to zoom");
  }
  const label = `Zoomed ${text(start, "start")}\u2013${text(end, "latest")}`;
  return h("button", {
    type: "button",
    class: "zoom-pill zoom-pill-active",
    title: "Reset zoom to the full step range (double-clicking the chart does the same).",
    onclick: clearBrushRange,
  }, `${label} \u00d7`);
}

function renderChartSmoothingPill(chart) {
  if (!chart?.smoothing) {
    return null;
  }
  return h("span", {
    class: "smoothing-pill",
    title: "Dense scalar smoothing is presentation-only; raw sampled values remain visible as the faint overlay and hover detail.",
  }, `EMA smoothing (${formatAxisValue(chart.smoothing.alpha)}) + raw hover`);
}

function renderFocusedSeriesControls(chart, stateInfo) {
  const controls = state.focusedSeriesControls;
  const selectedResolution = focusedSeriesResolutionSelectValue(controls.stepInterval);
  const runIDs = list(chart.series)
    .map((series) => series.run_id)
    .filter((runID, index, values) => runID && values.indexOf(runID) === index)
    .sort();
  return h("form", {
    class: "focused-series-controls",
    onsubmit: submitFocusedSeriesControls,
  },
    h("label", {}, "Run",
      h("select", { name: "runID" },
        h("option", { value: "", selected: !controls.runID }, "All runs"),
        ...runIDs.map((runID) => h("option", { value: runID, selected: controls.runID === runID }, runID)),
      ),
    ),
    h("label", {}, "Start step",
      h("input", { name: "startStep", inputmode: "numeric", value: controls.startStep, placeholder: text(chart.x_min, "0") }),
    ),
    h("label", {}, "End step",
      h("input", { name: "endStep", inputmode: "numeric", value: controls.endStep, placeholder: text(chart.x_max, "latest") }),
    ),
    h("label", {}, "Resolution",
      h("select", { name: "stepInterval" },
        ...focusedSeriesResolutionOptions.map((option) => h("option", { value: option.value, selected: selectedResolution === option.value }, option.label)),
      ),
    ),
    selectedResolution === "custom" ? h("label", {}, "Custom steps",
      h("input", { name: "customStepInterval", inputmode: "numeric", value: controls.customStepInterval, placeholder: "25" }),
    ) : null,
    h("label", { title: "Optional compatibility override. Blank derives 600-1500 points per series from the rendered chart width." }, "Point cap",
      h("input", { name: "maxPoints", inputmode: "numeric", value: controls.maxPoints, placeholder: "Auto (600-1500)" }),
    ),
    h("button", {
      type: "submit",
      class: "metric-catalog-expand",
      disabled: state.focusedSeriesLoading,
      title: `Load up to ${Intl.NumberFormat().format(focusedSeriesMaxPointLimit)} hoverable points for ${chart.metric_name}`,
    }, state.focusedSeriesLoading ? "Loading detail..." : stateInfo.cacheHit || stateInfo.detailLoaded ? "Apply detail" : "Load detail"),
  );
}

function submitFocusedSeriesControls(event) {
  event.preventDefault();
  const data = new FormData(event.currentTarget);
  let stepInterval;
  try {
    stepInterval = normalizeSubmittedStepInterval(data.get("stepInterval"), data.get("customStepInterval"));
  } catch (error) {
    state.focusedSeriesError = error.message || String(error);
    render();
    return;
  }
  state.focusedSeriesControls = {
    runID: text(data.get("runID"), "").trim(),
    startStep: text(data.get("startStep"), "").trim(),
    endStep: text(data.get("endStep"), "").trim(),
    stepInterval,
    customStepInterval: text(data.get("customStepInterval"), "").trim(),
    maxPoints: text(data.get("maxPoints"), "").trim(),
  };
  updateURL();
  loadFocusedSeriesDetail().catch(renderError);
}

function focusedSeriesQueryOptions() {
  const controls = state.focusedSeriesControls;
  const maxPoints = focusedSeriesPointBudget(controls);
  if (maxPoints < 1 || maxPoints > focusedSeriesMaxPointLimit) {
    throw new Error(`max points must be between 1 and ${Intl.NumberFormat().format(focusedSeriesMaxPointLimit)}`);
  }
  const startStep = parseOptionalIntegerControl(controls.startStep, "start step");
  const endStep = parseOptionalIntegerControl(controls.endStep, "end step");
  if (startStep !== undefined && endStep !== undefined && startStep > endStep) {
    throw new Error("start step must be less than or equal to end step");
  }
  const stepInterval = focusedSeriesStepInterval(controls, { startStep, endStep, chart: state.snapshot?.chart });
  return {
    maxPoints,
    runID: text(controls.runID, "").trim(),
    startStep,
    endStep,
    stepInterval,
  };
}

function parseOptionalIntegerControl(value, label) {
  const raw = text(value, "").trim();
  if (!raw) {
    return undefined;
  }
  return parseIntegerControl(raw, label);
}

function parseIntegerControl(value, label) {
  const raw = text(value, "").trim();
  if (!/^-?\d+$/.test(raw)) {
    throw new Error(`${label} must be an integer`);
  }
  return Number.parseInt(raw, 10);
}

function normalizeStepIntervalControl(value) {
  const raw = text(value, "").trim().toLowerCase();
  if (!raw || raw === "auto") {
    return "auto";
  }
  if (/^\d+$/.test(raw) && Number.parseInt(raw, 10) > 0) {
    return String(Number.parseInt(raw, 10));
  }
  return "auto";
}

function normalizeSubmittedStepInterval(value, customValue) {
  const selected = text(value, "auto").trim();
  if (selected === "custom") {
    const parsed = parseIntegerControl(customValue, "custom step interval");
    if (parsed < 1) {
      throw new Error("custom step interval must be at least 1");
    }
    return String(parsed);
  }
  if (selected === "auto") {
    return "auto";
  }
  const parsed = parseIntegerControl(selected, "step interval");
  if (parsed < 1) {
    throw new Error("step interval must be at least 1");
  }
  return String(parsed);
}

function focusedSeriesResolutionSelectValue(value) {
  const normalized = normalizeStepIntervalControl(value);
  if (["auto", "20", "50", "100"].includes(normalized)) {
    return normalized;
  }
  return "custom";
}

function customStepIntervalValue(value) {
  const normalized = normalizeStepIntervalControl(value);
  return focusedSeriesResolutionSelectValue(normalized) === "custom" ? normalized : "";
}

function focusedSeriesStepInterval(controls, options = {}) {
  const normalized = normalizeStepIntervalControl(controls.stepInterval);
  const stepInterval = normalized === "auto"
    ? autoFocusedSeriesStepInterval(options)
    : parseIntegerControl(normalized, "step interval");
  if (stepInterval < 1) {
    throw new Error("step interval must be at least 1");
  }
  return stepInterval;
}

function autoFocusedSeriesStepInterval(options = {}) {
  const chart = options.chart || {};
  const start = options.startStep ?? numberValue(chart.x_min);
  const end = options.endStep ?? numberValue(chart.x_max);
  if (start !== null && end !== null && Number.isFinite(start) && Number.isFinite(end)) {
    return Math.abs(end - start) <= 2000 ? 20 : 50;
  }
  return 50;
}

function chartResolutionLabel(chart) {
  const interval = numberValue(chart?.step_interval);
  if (!interval || interval <= 1) {
    return "";
  }
  return `every ${Intl.NumberFormat().format(interval)} steps`;
}

function renderResearchRunTablePanel(snapshot) {
  const metricColumns = featuredMetricSpecs(snapshot).slice(0, 6);
  const rows = filteredRuns(snapshot);
  return configuredPanel("runs", "Run comparison", "Metric values by run",
    h("div", { class: "table-scroll" },
      h("table", { class: "run-table research-table" },
        h("thead", {}, h("tr", {},
          h("th", {}, "Run"),
          h("th", {}, "Status"),
          h("th", {}, "Group"),
          ...metricColumns.map((spec) => h("th", { title: spec.name }, spec.title)),
        )),
        h("tbody", {}, rows.map((run) => h("tr", {},
          h("td", {},
            h("div", { class: "run-identity" },
              h("span", {}, run.run_id),
              h("small", {}, runContextSummary(run)),
              run.result_uri ? h("button", {
                type: "button",
                class: "mini-link-button",
                title: `Copy durable result reference: ${text(run.result_uri)}`,
                onclick: () => copyEvidenceText(run.result_uri),
              }, "copy result URI") : null,
            ),
          ),
          h("td", {}, h("span", { class: `run-status-badge ${runLifecycle(run)}`.trim(), title: runSuccessTitle(run) }, runLifecycleLabel(run))),
          h("td", {}, run.run_group_id),
          ...metricColumns.map((spec) => h("td", {}, formatTableValue(metricValueForRun(spec.name, run.run_id), spec))),
        ))),
      ),
    ),
    "full",
  );
}

function renderOutputMediaPanel(snapshot) {
  const detailSnapshot = state.fullSnapshot || snapshot;
  if (isCompactPayload(snapshot) && !state.fullSnapshot) {
    return renderDeferredOutputMediaPanel(snapshot);
  }
  const media = outputMediaSummaries(detailSnapshot);
  if (!media.length) {
    return configuredPanel("media", "Output media", "Media artifacts for the visible run set.", renderCompactStatusRow(
      [
        evidenceStatePill("no media summaries"),
        evidenceCountPill("artifact candidates", researchOutputArtifacts(detailSnapshot).filter(isVisualArtifact).length),
      ],
      "No output media matched the visible run set.",
    ), "full output-media-panel compact-status-panel");
  }

  const selection = outputMediaSelection(media);
  const visibleItems = selection.items.slice(0, 8);
  const hero = visibleItems.find((item) => artifactPreviewSource(item.artifact)) || visibleItems[0];
  const body = h("div", { class: "output-media-browser" },
    h("div", { class: "evidence-summary-strip" },
      evidenceCountPill("media", media.length),
      evidenceCountPill("tags", selection.tags.length),
      evidenceCountPill("runs", new Set(media.map((item) => item.run_id).filter(Boolean)).size),
      selection.steps.length ? evidenceCountPill("steps", selection.steps.length) : evidenceStatePill("step metadata pending"),
      evidenceStatePill("artifact-derived compatibility view"),
    ),
    renderOutputMediaControls(selection),
    hero ? renderOutputMediaHero(hero, selection) : h("p", { class: "empty" }, "No media matched the selected tag, run, and step."),
    visibleItems.length ? h("div", { class: "output-media-grid" }, visibleItems.map(renderOutputMediaCard)) : null,
    selection.items.length > visibleItems.length ? h("p", { class: "more" }, `+${selection.items.length - visibleItems.length} more media records for this selection`) : null,
  );
  return configuredPanel("media", "Output media", "Model outputs as summary streams: browse by tag, run, and step when indexed metadata is available.", body, "full output-media-panel");
}

function renderDeferredOutputMediaPanel(snapshot) {
  const visualCandidates = researchOutputArtifacts(snapshot).filter(isVisualArtifact).length;
  const body = renderCompactStatusRow(
    [
      evidenceCountPill("runs", list(snapshot.runs).length),
      visualCandidates ? evidenceCountPill("compact visual candidates", visualCandidates) : evidenceStatePill("details deferred"),
    ],
    "Load details to inspect output media records.",
    [
      state.fullSnapshotError ? h("p", { class: "error-text" }, state.fullSnapshotError) : null,
      h("button", {
        type: "button",
        class: "metric-catalog-expand",
        disabled: state.fullSnapshotLoading,
        onclick: () => {
          loadFullSnapshotDetails().catch(renderError);
        },
      }, state.fullSnapshotLoading ? "Loading media..." : "Load media summaries"),
    ],
  );
  return configuredPanel("media", "Output media", "Deferred for faster large-metric dashboard load", body, "full output-media-panel compact-status-panel");
}

function renderCompactStatusRow(pills, message, actions = []) {
  return h("div", { class: "compact-status-row" },
    h("div", { class: "evidence-summary-strip" }, ...pills),
    h("p", {}, message),
    actions.length ? h("div", { class: "compact-status-actions" }, ...actions) : null,
  );
}

function outputMediaSelection(media) {
  const tags = uniqueSorted(media.map((item) => item.tag));
  const selectedTag = tags.includes(state.outputMediaTag) ? state.outputMediaTag : defaultOutputMediaTag(media, tags);
  if (state.outputMediaTag !== selectedTag) {
    state.outputMediaTag = selectedTag;
  }
  const tagged = media.filter((item) => item.tag === selectedTag);
  const runs = uniqueSorted(tagged.map((item) => item.run_id).filter(Boolean));
  if (state.outputMediaRunID && !runs.includes(state.outputMediaRunID)) {
    state.outputMediaRunID = "";
  }
  const runFiltered = state.outputMediaRunID ? tagged.filter((item) => item.run_id === state.outputMediaRunID) : tagged;
  const steps = uniqueSorted(runFiltered.map((item) => item.step).filter(Boolean));
  if (steps.length < 2 || (state.outputMediaStep && !steps.includes(state.outputMediaStep))) {
    state.outputMediaStep = "";
  }
  const items = sortMediaSummaries(state.outputMediaStep
    ? runFiltered.filter((item) => item.step === state.outputMediaStep)
    : runFiltered);
  return { tags, runs, steps, selectedTag, items };
}

function defaultOutputMediaTag(media, tags) {
  const ranked = tags.map((tag) => {
    const items = media.filter((item) => item.tag === tag);
    return {
      tag,
      count: items.length,
      steps: new Set(items.map((item) => item.step).filter(Boolean)).size,
    };
  }).sort((left, right) =>
    right.steps - left.steps
    || right.count - left.count
    || left.tag.localeCompare(right.tag));
  return ranked[0]?.tag || "";
}

function renderOutputMediaControls(selection) {
  const controls = [
    h("label", {}, "Tag",
      h("select", {
        value: selection.selectedTag,
        onchange: (event) => {
          state.outputMediaTag = event.target.value;
          state.outputMediaRunID = "";
          state.outputMediaStep = "";
          updateURL();
          render();
        },
      }, selection.tags.map((tag) => h("option", { value: tag }, tag))),
    ),
  ];
  if (selection.runs.length > 1) {
    controls.push(h("label", {}, "Run",
      h("select", {
        value: state.outputMediaRunID,
        onchange: (event) => {
          state.outputMediaRunID = event.target.value;
          state.outputMediaStep = "";
          updateURL();
          render();
        },
      },
        h("option", { value: "" }, "All runs"),
        selection.runs.map((runID) => h("option", { value: runID }, runID)),
      ),
    ));
  }
  if (selection.steps.length >= 2) {
    controls.push(h("label", {}, "Step",
      h("select", {
        value: state.outputMediaStep,
        onchange: (event) => {
          state.outputMediaStep = event.target.value;
          updateURL();
          render();
        },
      },
        h("option", { value: "" }, "Latest / all"),
        selection.steps.map((step) => h("option", { value: step }, `step ${step}`)),
      ),
    ));
  }
  return h("div", { class: "output-media-controls" }, controls);
}

function renderOutputMediaHero(item, selection) {
  return h("article", { class: "output-media-hero" },
    h("div", { class: "output-media-preview" }, renderArtifactPreview(item.artifact, artifactPreviewSource(item.artifact))),
    h("div", { class: "output-media-details" },
      h("span", { class: "state-pill" }, item.kind),
      h("h3", {}, item.tag),
      h("p", {}, item.caption || "Model output media summary"),
      h("div", { class: "runtime-chip-list" },
        h("span", {}, h("em", {}, "run"), h("b", {}, item.run_id || "all")),
        item.step ? h("span", {}, h("em", {}, "step"), h("b", {}, item.step)) : h("span", {}, h("em", {}, "step"), h("b", {}, "not indexed")),
        h("span", {}, h("em", {}, "time"), h("b", {}, item.wall_time || "--")),
        h("span", {}, h("em", {}, "records"), h("b", {}, String(selection.items.length))),
      ),
    ),
  );
}

function renderOutputMediaCard(item) {
  return h("article", { class: "output-media-card" },
    renderArtifactPreview(item.artifact, artifactPreviewSource(item.artifact)),
    h("div", { class: "research-output-meta" },
      h("b", { title: item.tag }, item.tag),
      h("span", {}, `${item.run_id || "run"} / ${item.kind}${item.step ? ` / step ${item.step}` : ""}`),
      h("code", { title: item.artifact.external_ref || item.artifact.uri || "" }, item.wall_time || item.artifact.external_ref || item.artifact.created_at || "--"),
    ),
  );
}

function outputMediaSummaries(snapshot) {
  return sortMediaSummaries(allRunEvidence(snapshot, "artifacts")
    .filter(isOutputMediaArtifact)
    .map(mediaSummaryFromArtifact));
}

function isOutputMediaArtifact(artifact) {
  const haystack = artifactHaystack(artifact);
  if (/checkpoint|weights|model(?! output)|tensorboard event|event file/.test(haystack)) {
    return false;
  }
  return /(^|\W)(media|image|video|gif|audio|gallery|contact[-_ ]?sheet|prediction|rollout|frame|caption|vqa|question|answer)(\W|$)/.test(haystack);
}

function mediaSummaryFromArtifact(artifact) {
  return {
    artifact,
    run_id: artifact.run_id || "",
    tag: mediaTagForArtifact(artifact),
    kind: mediaKindForArtifact(artifact),
    step: mediaStepForArtifact(artifact),
    wall_time: artifact.wall_time || artifact.created_at || "",
    caption: artifact.caption || artifact.description || artifact.name || artifact.external_ref || "",
  };
}

function mediaTagForArtifact(artifact) {
  const explicit = artifact.tag || artifact.summary_tag || artifact.media_tag;
  if (explicit) {
    return text(explicit);
  }
  const source = text(artifact.name || artifact.external_ref || artifact.uri || artifact.artifact_id, "output media");
  return source
    .replace(/\bstep[-_:= ]*\d+\b/ig, "")
    .replace(/\bglobal[-_ ]?step[-_:= ]*\d+\b/ig, "")
    .replace(/\b(epoch|wall[-_ ]?time)[-_:= ][^/ ]+\b/ig, "")
    .replace(/\.(png|jpe?g|gif|webp|svg|mp4|webm|mov|html|txt)$/i, "")
    .replace(/[-_/]+$/g, "")
    .trim() || "output media";
}

function mediaKindForArtifact(artifact) {
  const haystack = artifactHaystack(artifact);
  if (/video|mp4|webm|mov|rollout/.test(haystack)) {
    return "video";
  }
  if (/audio|wav|mp3/.test(haystack)) {
    return "audio";
  }
  if (/html|report|gallery/.test(haystack)) {
    return "html";
  }
  if (/text|caption|vqa|question|answer|txt/.test(haystack)) {
    return "text";
  }
  if (/image|plot|chart|graph|contact[-_ ]?sheet|frame|png|jpe?g|gif|webp|svg/.test(haystack)) {
    return "image";
  }
  return shortArtifactType(artifact);
}

function mediaStepForArtifact(artifact) {
  const explicit = artifact.step ?? artifact.global_step ?? artifact.training_step;
  if (explicit !== undefined && explicit !== null && explicit !== "") {
    return text(explicit);
  }
  const haystack = [artifact.name, artifact.external_ref, artifact.uri, artifact.artifact_id].map((part) => text(part, "")).join(" ");
  const match = haystack.match(/\b(?:global[-_ ]?step|step)[-_/=: ]*(\d+)\b/i);
  return match ? match[1] : "";
}

function sortMediaSummaries(items) {
  return [...items].sort((left, right) =>
    compareStepValues(right.step, left.step)
    || text(right.wall_time, "").localeCompare(text(left.wall_time, ""))
    || text(left.tag).localeCompare(text(right.tag))
    || text(left.run_id).localeCompare(text(right.run_id)));
}

function uniqueSorted(values) {
  return [...new Set(values.map((value) => text(value, "").trim()).filter(Boolean))]
    .sort(compareStepValues);
}

function compareStepValues(left, right) {
  const leftNumber = Number(left);
  const rightNumber = Number(right);
  const leftNumeric = Number.isFinite(leftNumber);
  const rightNumeric = Number.isFinite(rightNumber);
  if (leftNumeric && rightNumeric && leftNumber !== rightNumber) {
    return leftNumber - rightNumber;
  }
  if (leftNumeric !== rightNumeric) {
    return leftNumeric ? -1 : 1;
  }
  return text(left).localeCompare(text(right));
}

function artifactHaystack(artifact) {
  return [artifact.type, artifact.name, artifact.uri, artifact.preview, artifact.external_ref, artifact.caption, artifact.artifact_id].map((part) => text(part, "").toLowerCase()).join(" ");
}

function researchOutputArtifacts(snapshot) {
  const outputs = allRunEvidence(snapshot, "artifacts").filter((artifact) => {
    const haystack = [artifact.type, artifact.name, artifact.uri, artifact.preview, artifact.external_ref].map((part) => text(part, "").toLowerCase()).join(" ");
    return /image|video|plot|chart|graph|rollout|render|episode|frame|pixel|embedding|projection|retrieval|nearest|neighbor|attention|saliency|heatmap|gradcam|diff|caption|vqa|qa|question|answer|png|jpe?g|gif|webp|svg|mp4|webm|mov|html|report|checkpoint|model/.test(haystack);
  });
  return outputs.sort((left, right) =>
    artifactKindRank(left) - artifactKindRank(right)
    || text(right.created_at, "").localeCompare(text(left.created_at, ""))
    || text(left.name || left.artifact_id).localeCompare(text(right.name || right.artifact_id)));
}

function artifactKindRank(artifact) {
  if (isPreviewableArtifact(artifact)) {
    return 0;
  }
  const type = text(artifact.type, "").toLowerCase();
  if (/html|report|diff|caption|vqa|qa|plot|chart|graph|embedding|projection|retrieval|nearest|neighbor/.test(type)) {
    return 1;
  }
  if (/checkpoint|model/.test(type)) {
    return 3;
  }
  return 2;
}

function isPreviewableArtifact(artifact) {
  return isVisualArtifact(artifact) || isLocalReportArtifact(artifact);
}

function isVisualArtifact(artifact) {
  const source = artifactPreviewSource(artifact);
  const haystack = [artifact.type, artifact.name, artifact.uri, artifact.preview, artifact.external_ref].map((part) => text(part, "").toLowerCase()).join(" ");
  return Boolean(source) && /image|video|plot|chart|graph|rollout|render|episode|frame|pixel|embedding|projection|retrieval|nearest|neighbor|attention|saliency|heatmap|gradcam|diff|caption|vqa|qa|question|answer|png|jpe?g|gif|webp|svg|mp4|webm|mov/.test(haystack);
}

function isReportArtifact(artifact) {
  const haystack = [artifact.type, artifact.name, artifact.uri, artifact.preview, artifact.external_ref].map((part) => text(part, "").toLowerCase()).join(" ");
  return /html|report|\.htm/.test(haystack);
}

function isLocalReportArtifact(artifact) {
  return isReportArtifact(artifact) && artifact.artifact_id && hasLocalReportDocumentReference(artifact);
}

function artifactPreviewSource(artifact) {
  for (const candidate of [artifact.preview, artifact.external_ref, artifact.uri]) {
    const value = text(candidate, "");
    if (/^(https?:|data:)/i.test(value)) {
      return value;
    }
  }
  if (artifact.artifact_id && hasLocalArtifactReference(artifact)) {
    const url = new URL("/api/stellar/artifact", window.location.origin);
    url.searchParams.set("target", state.target);
    url.searchParams.set("artifact", artifact.artifact_id);
    return url.toString();
  }
  return "";
}

function hasLocalArtifactReference(artifact) {
  return [artifact.preview, artifact.uri].some((candidate) => {
    const value = text(candidate, "");
    return Boolean(value) && (!/^[a-z][a-z0-9+.-]*:/i.test(value) || /^file:/i.test(value));
  });
}

function hasLocalReportDocumentReference(artifact) {
  const uri = text(artifact.uri, "");
  if (uri && isLocalArtifactReference(uri)) {
    return true;
  }
  const preview = text(artifact.preview, "");
  return !/^(https?:|data:)/i.test(uri) && isLocalArtifactReference(preview);
}

function isLocalArtifactReference(value) {
  return Boolean(value) && (!/^[a-z][a-z0-9+.-]*:/i.test(value) || /^file:/i.test(value));
}

function artifactReportSource(artifact, options = {}) {
  if (isLocalReportArtifact(artifact)) {
    return artifactScopedSource(artifact, options);
  }
  for (const candidate of [artifact.external_ref, artifact.uri, artifact.preview]) {
    const value = text(candidate, "");
    if (/^https?:/i.test(value)) {
      return value;
    }
  }
  return "";
}

function artifactScopedSource(artifact, options = {}) {
  if (!state.target || !artifact.artifact_id) {
    return "";
  }
  const targetKey = base64URLSegment(state.target);
  const artifactKey = base64URLSegment(artifact.artifact_id);
  const url = new URL(`/api/stellar/artifact/bundle/${targetKey}/${artifactKey}/`, window.location.origin);
  if (options.frame) {
    url.searchParams.set("frame", "1");
  }
  return url.toString();
}

function base64URLSegment(value) {
  const bytes = new TextEncoder().encode(text(value, ""));
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function renderArtifactPreview(artifact, source) {
  const label = artifact.name || artifact.artifact_id || "artifact";
  const kind = [artifact.type, artifact.name, artifact.uri, artifact.preview, artifact.external_ref].map((part) => text(part, "").toLowerCase()).join(" ");
  if (isReportArtifact(artifact)) {
    return renderArtifactReportPreview(artifact);
  }
  if (artifact.table) {
    return renderTableArtifactPreview(artifact);
  }
  if (source && /video|mp4|webm|mov/.test(kind)) {
    return h("video", { class: "research-output-preview", src: source, controls: true, preload: "metadata", "aria-label": label });
  }

  function renderTableArtifactPreview(artifact) {
    const table = artifact.table || {};
    const columns = list(table.columns).slice(0, 6);
    const rows = list(table.rows).slice(0, 6);
    if (!columns.length) {
      return h("div", { class: "research-output-placeholder" },
        h("strong", {}, "table"),
        h("span", {}, table.caption || artifact.caption || "table artifact"),
      );
    }
    return h("div", { class: "research-output-preview table-preview" },
      table.caption || artifact.caption ? h("strong", {}, table.caption || artifact.caption) : null,
      h("table", {},
        h("thead", {}, h("tr", {}, columns.map((column) => h("th", {}, column)))),
        h("tbody", {}, rows.map((row) => h("tr", {}, columns.map((column) => h("td", {}, text(row[column], "")))))),
      ),
    );
  }
  if (source && /image|plot|chart|graph|rollout|render|episode|frame|pixel|embedding|projection|retrieval|nearest|neighbor|attention|saliency|heatmap|gradcam|png|jpe?g|gif|webp|svg/.test(kind)) {
    return h("img", { class: "research-output-preview", src: source, alt: label, loading: "lazy" });
  }
  return h("div", { class: "research-output-placeholder" },
    h("strong", {}, shortArtifactType(artifact)),
    h("span", {}, source ? "preview unavailable" : "tracked output"),
  );
}

function renderArtifactReportPreview(artifact) {
  const label = artifact.name || artifact.artifact_id || "report";
  const source = isLocalReportArtifact(artifact) ? artifactReportSource(artifact, { frame: true }) : "";
  if (!source) {
    return h("div", { class: "research-output-placeholder report-placeholder" },
      h("strong", {}, "report"),
      h("span", {}, artifactReportSource(artifact) ? "open external report" : "report artifact"),
    );
  }
  return h("iframe", {
    class: "research-output-preview report-preview",
    src: source,
    title: label,
    sandbox: "",
    loading: "lazy",
    referrerpolicy: "no-referrer",
  });
}

function renderArtifactAction(artifact) {
  if (!isReportArtifact(artifact)) {
    return null;
  }
  const href = artifactReportSource(artifact);
  if (!href) {
    return null;
  }
  return h("a", {
    class: "artifact-action-link",
    href,
    target: "_blank",
    rel: "noopener noreferrer",
  }, "Open report");
}

function shortArtifactType(artifact) {
  const value = [artifact.type, artifact.name, artifact.uri, artifact.external_ref].map((part) => text(part, "")).join(" ");
  if (/checkpoint|model/i.test(value)) {
    return "checkpoint";
  }
  if (/html|report/i.test(value)) {
    return "report";
  }
  if (/video|rollout/i.test(value)) {
    return "video";
  }
  if (/embedding|projection/i.test(value)) {
    return "embedding";
  }
  if (/retrieval|nearest|neighbor/i.test(value)) {
    return "retrieval";
  }
  if (/diff|caption|vqa|question|answer/i.test(value)) {
    return "qualitative";
  }
  if (/image|plot|chart|graph|render|frame|pixel/i.test(value)) {
    return "visual";
  }
  return value.split(/[/:._-]+/).filter(Boolean)[0] || "artifact";
}

function renderResearchEvidencePanel(snapshot) {
  const detailSnapshot = state.fullSnapshot || snapshot;
  if (isCompactPayload(snapshot) && !state.fullSnapshot) {
    return renderDeferredEvidencePanel(snapshot);
  }
  const runs = filteredRuns(detailSnapshot);
  const runIDs = filteredRunIDs(detailSnapshot);
  const counts = evidenceCounts(detailSnapshot, runIDs);
  const evidenceSections = [
    ["Configs / hparams", "normalized run configs and launch parameters", allRunEvidence(detailSnapshot, "configs", { runIDs }), renderConfigEvidenceItem],
    ["Artifacts / checkpoints", "model outputs, checkpoints, reports, and external refs", allRunEvidence(detailSnapshot, "artifacts", { runIDs }), renderArtifactEvidenceItem],
    ["Events / logs", "warnings, failures, restarts, checkpoints, and imported markers", allRunEvidence(detailSnapshot, "events", { runIDs }), renderEventEvidenceItem],
    ["Observations / decisions", "human notes and durable experiment decisions", allRunEvidence(detailSnapshot, "observations", { runIDs }), renderObservationEvidenceItem],
  ];
  const body = h("div", { class: "evidence-browser" },
    h("div", { class: "evidence-summary-strip" },
      evidenceCountPill("runs", runs.length),
      evidenceCountPill("metric files", detailSnapshot.status?.metric_files || 0),
      evidenceCountPill("metric names", list(detailSnapshot.metric_options).length),
      counts.total > 0 ? evidenceCountPill("attached evidence", counts.total) : evidenceStatePill("metrics-only import"),
    ),
    counts.total === 0 ? renderMetricsOnlyNotice(detailSnapshot) : null,
    renderRuntimeDiffSection(detailSnapshot, runs),
    ...evidenceSections
      .filter(([, , items]) => items.length > 0)
      .map(([title, subtitle, items, renderItem]) => renderEvidenceListSection(title, subtitle, items, renderItem)),
  );
  return configuredPanel("evidence", "Evidence browser", counts.total > 0 ? "Configs, runtime context, artifacts, events, and notes" : "Current store contains scalar metrics only", body, "full evidence-browser-panel");
}

function renderDeferredEvidencePanel(snapshot) {
  const body = h("div", { class: "evidence-browser" },
    h("div", { class: "evidence-summary-strip" },
      evidenceCountPill("runs", list(snapshot.runs).length),
      evidenceCountPill("metric files", snapshot.status?.metric_files || 0),
      evidenceCountPill("metric names", list(snapshot.metric_options).length),
      evidenceStatePill("details deferred"),
    ),
    h("section", { class: "evidence-section evidence-notice" },
      h("div", { class: "evidence-section-head" },
        h("h3", {}, "Evidence details are deferred"),
        h("span", {}, text(snapshot.chart?.metric_name, state.metric || "focused metrics")),
      ),
      h("p", {}, "Stellar loaded a lightweight metric view first so large metric catalogs stay responsive. Load full details when you need configs, artifacts, events, observations, and runtime diffs."),
      state.fullSnapshotError ? h("p", { class: "error-text" }, state.fullSnapshotError) : null,
      h("button", {
        type: "button",
        class: "metric-catalog-expand",
        disabled: state.fullSnapshotLoading,
        onclick: () => {
          loadFullSnapshotDetails().catch(renderError);
        },
      }, state.fullSnapshotLoading ? "Loading evidence..." : "Load evidence details"),
    ),
  );
  return configuredPanel("evidence", "Evidence browser", "Deferred for faster large-metric dashboard load", body, "full evidence-browser-panel");
}

function evidenceCountPill(label, count) {
  return h("span", { class: "evidence-count" }, h("b", {}, count), " ", label);
}

function evidenceStatePill(label) {
  return h("span", { class: "evidence-count evidence-state" }, label);
}

function evidenceCounts(snapshot, runIDs = filteredRunIDs(snapshot)) {
  const configs = allRunEvidence(snapshot, "configs", { runIDs }).length;
  const artifacts = allRunEvidence(snapshot, "artifacts", { runIDs }).length;
  const events = allRunEvidence(snapshot, "events", { runIDs }).length;
  const observations = allRunEvidence(snapshot, "observations", { runIDs }).length;
  return {
    configs,
    artifacts,
    events,
    observations,
    total: configs + artifacts + events + observations,
  };
}

function renderMetricsOnlyNotice(snapshot) {
  const kustoBacked = String(snapshot?.store_path || "").startsWith("kusto://");
  const commands = kustoBacked
    ? h("p", {}, "Kusto-backed metrics and run identity are authoritative, read-only source evidence.")
    : h("div", { class: "evidence-command-grid" },
        h("code", {}, "taugrid-portal experiment track RUN --config config.yaml --artifact checkpoint:model=/path/to/model"),
        h("code", {}, "taugrid-portal experiment import jsonl --run RUN --history 'metrics-history.jsonl'"),
        h("code", {}, "taugrid-portal experiment observe --scope run:RUN --type decision --text 'why this run matters'"),
      );
  return h("section", { class: "evidence-section evidence-notice" },
    h("div", { class: "evidence-section-head" },
      h("h3", {}, "Metrics-only store"),
      h("span", {}, "not a zero-result experiment"),
    ),
    h("p", {}, `This store has ${text(snapshot.status?.metric_files, "0")} scalar metric files, but no config, artifact, event, or observation rows. Those panels stay hidden until ingestion records that evidence.`),
    commands,
  );
}

function allRunEvidence(snapshot, key, options = {}) {
  const runIDs = options.runIDs || filteredRunIDs(snapshot);
  const includeItem = (item) => {
    if (!runIDs.size) {
      return false;
    }
    const itemRunID = item.run_id || item.scope_id || "";
    return !itemRunID || runIDs.has(itemRunID);
  };
  const seen = new Set();
  const items = [];
  for (const item of list(snapshot[key])) {
    if (!includeItem(item)) {
      continue;
    }
    const id = evidenceID(item, key);
    if (!seen.has(id)) {
      seen.add(id);
      items.push(item);
    }
  }
  for (const run of list(snapshot.runs)) {
    if (!runIDs.has(run.run_id)) {
      continue;
    }
    for (const item of list(run[key])) {
      const id = evidenceID(item, key);
      if (!seen.has(id)) {
        seen.add(id);
        items.push(item);
      }
    }
  }
  return items;
}

function evidenceID(item, key) {
  return item.artifact_id || item.config_hash || item.event_id || item.observation_id || `${key}:${item.run_id || item.scope_id || ""}:${JSON.stringify(item)}`;
}

function renderRuntimeDiffSection(snapshot, runs = filteredRuns(snapshot)) {
  const diffs = activeRunFilterLabels(snapshot).length
    ? runtimeDiffsForVisibleRuns(runs)
    : list(snapshot.compare?.runtime_diffs);
  const body = diffs.length
    ? h("div", { class: "runtime-diff-list" }, diffs.map((diff) => h("div", { class: "runtime-diff-row" },
      h("b", {}, diff.field),
      h("span", {}, list(diff.values).map((value) => `${value.run_group_id}: ${value.value}`).join(" | ")),
    )))
    : h("p", { class: "empty" }, "No runtime/config differences were detected in the current run set.");
  return h("section", { class: "evidence-section compact" },
    h("div", { class: "evidence-section-head" },
      h("h3", {}, "Runtime diffs"),
      h("span", {}, text(snapshot.compare?.metric_name, state.metric)),
    ),
    body,
  );
}

function runtimeDiffsForVisibleRuns(runs) {
  const fields = new Map();
  const addValue = (field, run, value) => {
    const normalized = text(value, "").trim();
    if (!normalized || normalized === "not collected") {
      return;
    }
    if (!fields.has(field)) {
      fields.set(field, []);
    }
    fields.get(field).push({
      run_group_id: run.run_group_id || run.run_id,
      value: normalized,
    });
  };
  for (const run of runs) {
    addValue("State", run, runLifecycle(run));
    for (const field of collectedSystems(run)) {
      addValue(field.name || "runtime", run, field.value);
    }
  }
  return Array.from(fields.entries()).map(([field, values]) => {
    const unique = new Set(values.map((value) => value.value));
    if (unique.size <= 1) {
      return null;
    }
    return { field, values };
  }).filter(Boolean);
}

function collectedSystems(run) {
  return list(run.systems).filter((field) => {
    const value = text(field.value, "").toLowerCase();
    return value && value !== "not collected" && field.collection_state !== "not_collected";
  });
}

function renderEvidenceListSection(title, subtitle, items, renderItem) {
  return h("section", { class: "evidence-section" },
    h("div", { class: "evidence-section-head" },
      h("h3", {}, title),
      h("span", {}, `${items.length} ${subtitle}`),
    ),
    items.length
      ? h("div", { class: "evidence-list" }, items.slice(0, 40).map(renderItem), items.length > 40 ? h("p", { class: "more" }, `+${items.length - 40} more`) : null)
      : h("p", { class: "empty" }, `No ${title.toLowerCase()} imported for this store yet.`),
  );
}

function renderConfigEvidenceItem(item) {
  return h("article", { class: "evidence-item" },
    h("b", {}, item.run_id || "run config"),
    h("span", {}, item.format || "config"),
    h("code", {}, compactConfig(item)),
  );
}

function renderArtifactEvidenceItem(item) {
  const action = renderArtifactAction(item);
  return h("article", { class: `evidence-item ${action ? "with-action" : ""}`.trim() },
    h("b", {}, item.name || item.artifact_id || "artifact"),
    h("span", {}, `${item.run_id || "run"} / ${item.type || "artifact"}`),
    h("code", {}, item.external_ref || item.uri || item.digest || item.created_at || "--"),
    action,
  );
}

function renderEventEvidenceItem(item) {
  return h("article", { class: `evidence-item severity-${familyClass(item.severity || "info")}` },
    h("b", {}, item.message || item.type || "event"),
    h("span", {}, `${item.run_id || "run"} / ${item.severity || item.source || "event"}`),
    h("code", {}, item.time || item.payload || "--"),
  );
}

function renderObservationEvidenceItem(item) {
  return h("article", { class: "evidence-item" },
    h("b", {}, item.type || "observation"),
    h("span", {}, `${item.author || item.source || "author"} / ${item.scope_type || "scope"}:${item.scope_id || ""}`),
    h("code", {}, item.text || item.evidence || item.created_at || "--"),
  );
}

function compactConfig(item) {
  const raw = item.normalized_json || item.indexed_fields || item.uri || item.config_hash || "";
  if (!raw) {
    return "--";
  }
  try {
    const parsed = JSON.parse(raw);
    return JSON.stringify(parsed).slice(0, 220);
  } catch {
    return text(raw).slice(0, 220);
  }
}

function renderMetricChart(chart, options = {}) {
  const dataset = chartDataset(chart, { runIDs: options.runIDs || filteredRunIDSetForChart(chart) });
  if (!chart?.has_data || !dataset.length) {
    return h("div", { class: `chart-empty ${options.className || ""}`.trim() }, shortMetricName(options.metricName || chart?.metric_name || state.metric));
  }

  const width = 800;
  const height = options.compact ? 260 : 320;
  const margin = options.compact
    ? { top: 18, right: 16, bottom: 28, left: 42 }
    : { top: 24, right: 26, bottom: 40, left: 58 };
  const plot = {
    left: margin.left,
    right: width - margin.right,
    top: margin.top,
    bottom: height - margin.bottom,
  };
  const domain = chartDomain(chart, dataset);
  const project = (point) => ({
    x: projectLinear(point.step, domain.xMin, domain.xMax, plot.left, plot.right),
    y: projectLinear(point.value, domain.yMin, domain.yMax, plot.bottom, plot.top),
  });

  const svg = s("svg", {
    class: "stellar-line-chart",
    viewBox: `0 0 ${width} ${height}`,
    role: "img",
    "aria-label": `${text(options.metricName, chart.metric_name)} line chart`,
  });
  const yTicks = ticks(domain.yMin, domain.yMax, options.compact ? 4 : 5);
  const xTicks = ticks(domain.xMin, domain.xMax, options.compact ? 3 : 5);

  for (const tick of yTicks) {
    const y = projectLinear(tick, domain.yMin, domain.yMax, plot.bottom, plot.top);
    svg.append(
      s("line", { class: "chart-grid-line", x1: plot.left, x2: plot.right, y1: y, y2: y }),
      s("text", { class: "chart-axis-label chart-y-label", x: plot.left - 8, y: y + 4, "text-anchor": "end" }, formatAxisValue(tick)),
    );
  }
  for (const tick of xTicks) {
    const x = projectLinear(tick, domain.xMin, domain.xMax, plot.left, plot.right);
    svg.append(
      s("line", { class: "chart-grid-line chart-grid-vertical", x1: x, x2: x, y1: plot.top, y2: plot.bottom }),
    );
  }
  svg.append(
    s("line", { class: "chart-axis-line", x1: plot.left, x2: plot.right, y1: plot.bottom, y2: plot.bottom }),
    s("line", { class: "chart-axis-line", x1: plot.left, x2: plot.left, y1: plot.top, y2: plot.bottom }),
    s("text", { class: "chart-axis-label chart-x-label", x: plot.left, y: height - 10 }, `step ${formatAxisValue(domain.xMin)}`),
    s("text", { class: "chart-axis-label chart-x-label", x: plot.right, y: height - 10, "text-anchor": "end" }, `step ${formatAxisValue(domain.xMax)}`),
  );

  const rendered = [];
  const totalChartPoints = dataset.reduce((total, series) => total + series.points.length, 0);
  const showPointMarkers = totalChartPoints <= maxStaticChartPointMarkers;
  for (const series of dataset) {
    const projected = series.points.map((point) => ({ ...point, ...project(point) }));
    const smoothedProjected = series.smoothedPoints.map((point) => ({ ...point, ...project(point) }));
    const hasSmoothedLine = Boolean(chart?.smoothing && smoothedProjected.length > 1);
    const renderedSeries = { ...series, points: projected, smoothedPoints: smoothedProjected, line: null, smoothedLine: null };
    rendered.push(renderedSeries);
    if (projected.length > 1) {
      renderedSeries.line = s("polyline", {
        class: hasSmoothedLine ? "chart-series-line chart-series-raw-overlay" : "chart-series-line",
        points: projected.map((point) => `${point.x.toFixed(1)},${point.y.toFixed(1)}`).join(" "),
        stroke: series.color,
      }, s("title", {}, `${series.runID} · ${series.runGroupID}${hasSmoothedLine ? " raw" : ""}`));
      svg.append(renderedSeries.line);
    }
    if (hasSmoothedLine) {
      renderedSeries.smoothedLine = s("polyline", {
        class: "chart-series-line chart-series-smoothed",
        points: smoothedProjected.map((point) => `${point.x.toFixed(1)},${point.y.toFixed(1)}`).join(" "),
        stroke: series.color,
      }, s("title", {}, `${series.runID} · ${series.runGroupID} EMA`));
      svg.append(renderedSeries.smoothedLine);
    }
    if (showPointMarkers) {
      for (const point of projected) {
        svg.append(s("circle", {
          class: "chart-point",
          cx: point.x,
          cy: point.y,
          r: options.compact ? 2.8 : 3.8,
          fill: series.color,
        }, s("title", {}, `${series.runID} step ${point.step}: ${formatAxisValue(point.value)}`)));
      }
    }
  }

  const hoverCapture = s("rect", {
    class: "chart-hover-capture",
    x: plot.left,
    y: plot.top,
    width: plot.right - plot.left,
    height: plot.bottom - plot.top,
  });
  if (options.brush) {
    hoverCapture.classList.add("chart-brush-capture");
  }
  const hoverLine = s("line", { class: "chart-hover-line is-hidden", y1: plot.top, y2: plot.bottom });
  const hoverDot = s("circle", { class: "chart-hover-dot is-hidden", r: options.compact ? 5 : 6 });
  const brushRect = options.brush
    ? s("rect", { class: "chart-brush-rect is-hidden", y: plot.top, height: plot.bottom - plot.top })
    : null;
  svg.append(hoverCapture, hoverLine, hoverDot);
  if (brushRect) {
    svg.append(brushRect);
  }
  const tooltip = h("div", { class: "chart-tooltip is-hidden" });
  const wrapper = h("div", { class: `metric-chart ${options.compact ? "compact" : "full"} ${options.className || ""}`.trim() },
    svg,
    tooltip,
    options.showLegend ? chartLegend(dataset) : null,
  );

  let pendingHoverEvent = null;
  let hoverFrame = 0;
  const hoverContext = {
    svg,
    wrapper,
    rendered,
    width,
    hoverLine,
    hoverDot,
    tooltip,
    metricName: options.metricName || chart.metric_name,
    lastHoverKey: "",
    lastHoverPosition: "",
    lastTooltipTransform: "",
    activeSeriesKey: "",
  };
  let lastHoverPointer = null;
  let hoverSuppressedUntil = 0;
  let hoverResumeTimer = 0;
  const scheduleHover = (pointer) => {
    pendingHoverEvent = pointer;
    if (hoverFrame) {
      return;
    }
    hoverFrame = window.requestAnimationFrame(() => {
      hoverFrame = 0;
      const nextEvent = pendingHoverEvent;
      pendingHoverEvent = null;
      if (nextEvent) {
        updateChartHover(nextEvent, hoverContext);
      }
    });
  };
  const clearHoverWork = () => {
    pendingHoverEvent = null;
    if (hoverFrame) {
      window.cancelAnimationFrame(hoverFrame);
      hoverFrame = 0;
    }
  };
  const clearHoverResumeTimer = () => {
    if (hoverResumeTimer) {
      window.clearTimeout(hoverResumeTimer);
      hoverResumeTimer = 0;
    }
  };
  hoverCapture.addEventListener("mousemove", (event) => {
    const pointer = chartPointerFromEvent(event);
    lastHoverPointer = pointer;
    if (hoverContext.brushing || performance.now() < hoverSuppressedUntil) {
      return;
    }
    scheduleHover(pointer);
  });
  hoverCapture.addEventListener("wheel", (event) => {
    lastHoverPointer = chartPointerFromEvent(event);
    hoverSuppressedUntil = performance.now() + chartHoverScrollIdleMs;
    clearHoverWork();
    hideChartHover(hoverContext);
    clearHoverResumeTimer();
    hoverResumeTimer = window.setTimeout(() => {
      hoverResumeTimer = 0;
      if (lastHoverPointer) {
        scheduleHover(lastHoverPointer);
      }
    }, chartHoverScrollIdleMs);
  }, { passive: true });
  hoverCapture.addEventListener("mouseleave", () => {
    lastHoverPointer = null;
    hoverSuppressedUntil = 0;
    clearHoverResumeTimer();
    clearHoverWork();
    hideChartHover(hoverContext);
  });
  svg.addEventListener("mouseleave", () => {
    lastHoverPointer = null;
    hoverSuppressedUntil = 0;
    clearHoverResumeTimer();
    clearHoverWork();
    hideChartHover(hoverContext);
  });

  if (brushRect) {
    attachChartBrush({
      svg,
      hoverCapture,
      brushRect,
      plot,
      width,
      domain,
      hoverContext,
      clearHoverWork,
      clearHoverResumeTimer,
    });
  }

  return wrapper;
}

// attachChartBrush lets the user drag horizontally across the detail chart to
// pick a step window, then reuses the existing focused-series range fetch to
// reload that window at higher resolution. It is a new *input* for the same
// startStep/endStep controls the toolbar form already drives — no new backend
// path — so brushing, the form, and the URL stay in lockstep. Double-click
// clears the window (reset zoom).
function attachChartBrush(brush) {
  const { svg, hoverCapture, brushRect, plot, width, domain } = brush;
  const plotWidth = plot.right - plot.left;
  const minDragPx = 6; // ignore accidental micro-drags / plain clicks
  let dragging = false;
  let anchorX = 0;

  // Convert a client X into a plot-space SVG x, clamped to the plot band.
  const clientToPlotX = (clientX) => {
    const bounds = svg.getBoundingClientRect();
    if (!bounds.width) {
      return plot.left;
    }
    const svgX = ((clientX - bounds.left) / bounds.width) * width;
    return Math.min(plot.right, Math.max(plot.left, svgX));
  };
  // Invert projectLinear(step → x) back to a step for the current domain.
  const plotXToStep = (x) =>
    domain.xMin + ((x - plot.left) / plotWidth) * (domain.xMax - domain.xMin);

  const showBrush = (x1, x2) => {
    const left = Math.min(x1, x2);
    brushRect.setAttribute("x", left.toFixed(1));
    brushRect.setAttribute("width", Math.abs(x2 - x1).toFixed(1));
    brushRect.classList.remove("is-hidden");
  };
  const hideBrush = () => {
    brushRect.classList.add("is-hidden");
    brushRect.setAttribute("width", "0");
  };

  const onMove = (event) => {
    if (!dragging) {
      return;
    }
    showBrush(anchorX, clientToPlotX(event.clientX));
  };
  const onUp = (event) => {
    window.removeEventListener("mousemove", onMove);
    window.removeEventListener("mouseup", onUp);
    if (!dragging) {
      return;
    }
    dragging = false;
    brush.hoverContext.brushing = false;
    svg.classList.remove("is-brushing");
    const releaseX = clientToPlotX(event.clientX);
    hideBrush();
    if (Math.abs(releaseX - anchorX) < minDragPx) {
      return; // treat as a click, not a zoom
    }
    const lo = Math.round(plotXToStep(Math.min(anchorX, releaseX)));
    const hi = Math.round(plotXToStep(Math.max(anchorX, releaseX)));
    if (!Number.isFinite(lo) || !Number.isFinite(hi) || hi <= lo) {
      return;
    }
    applyBrushRange(lo, hi);
  };

  hoverCapture.addEventListener("mousedown", (event) => {
    if (event.button !== 0) {
      return; // left button only
    }
    event.preventDefault();
    brush.clearHoverWork();
    brush.clearHoverResumeTimer();
    hideChartHover(brush.hoverContext);
    dragging = true;
    brush.hoverContext.brushing = true;
    anchorX = clientToPlotX(event.clientX);
    svg.classList.add("is-brushing");
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  });
  hoverCapture.addEventListener("dblclick", (event) => {
    event.preventDefault();
    clearBrushRange();
  });
}

// applyBrushRange writes a brushed [lo, hi] step window into the focused-series
// controls and reloads detail at that window. Resolution is reset to auto so
// the narrower window is re-sampled finely (autoFocusedSeriesStepInterval picks
// 20 for <=2000-step spans), which is the whole point of zooming in.
function applyBrushRange(lo, hi) {
  state.focusedSeriesControls = {
    ...state.focusedSeriesControls,
    startStep: String(lo),
    endStep: String(hi),
    stepInterval: "auto",
    customStepInterval: "",
  };
  state.focusedSeriesError = "";
  updateURL();
  loadFocusedSeriesDetail().catch(renderError);
}

// clearBrushRange resets the brushed window (reset zoom) and reloads the full
// series, mirroring what clearing the toolbar's Start/End fields would do.
function clearBrushRange() {
  const controls = state.focusedSeriesControls;
  if (!text(controls.startStep, "").trim() && !text(controls.endStep, "").trim()) {
    return; // already full-range; nothing to reset
  }
  state.focusedSeriesControls = {
    ...controls,
    startStep: "",
    endStep: "",
    stepInterval: "auto",
    customStepInterval: "",
  };
  state.focusedSeriesError = "";
  updateURL();
  loadFocusedSeriesDetail().catch(renderError);
}

function hideChartHover(context) {
  context.hoverLine.classList.add("is-hidden");
  context.hoverDot.classList.add("is-hidden");
  context.tooltip.classList.add("is-hidden");
  updateChartHoverSeriesState(context, null);
  context.lastHoverKey = "";
  context.lastHoverPosition = "";
  context.lastTooltipTransform = "";
}

function chartPointerFromEvent(event) {
  return { clientX: event.clientX, clientY: event.clientY };
}

function chartDataset(chart, options = {}) {
  const runIDs = options.runIDs;
  return (chart?.series || []).map((series) => {
    if (runIDs && !runIDs.has(series.run_id)) {
      return null;
    }
    if (!runIDs && state.hiddenRuns.has(series.run_id)) {
      return null;
    }
    const points = chartSeriesPointValues(series, "values");
    const smoothedPoints = chartSeriesPointValues(series, "smoothed_values");
    return {
      runID: series.run_id,
      runGroupID: series.run_group_id,
      color: series.color || colorForToken(series.run_id || series.run_group_id),
      points,
      smoothedPoints,
    };
  }).filter((series) => series && series.points.length);
}

function chartSeriesPointValues(series, key) {
  const values = Array.isArray(series?.[key]) && series[key].length
    ? series[key]
    : key === "values"
      ? parsePointPairs(series.points).map(([x, y]) => ({ step: x, value: y }))
      : [];
  return values
    .map((point) => ({
      step: numberValue(point.step),
      value: numberValue(point.value),
    }))
    .filter((point) => point.step !== null && point.value !== null);
}

function chartDomain(chart, dataset) {
  let xMin = numberValue(chart?.x_min);
  let xMax = numberValue(chart?.x_max);
  let yMin = numberValue(chart?.y_min);
  let yMax = numberValue(chart?.y_max);
  if (xMin === null || xMax === null || yMin === null || yMax === null) {
    let computedXMin = Infinity;
    let computedXMax = -Infinity;
    let computedYMin = Infinity;
    let computedYMax = -Infinity;
    for (const series of dataset) {
      for (const point of series.points) {
        if (point.step < computedXMin) computedXMin = point.step;
        if (point.step > computedXMax) computedXMax = point.step;
        if (point.value < computedYMin) computedYMin = point.value;
        if (point.value > computedYMax) computedYMax = point.value;
      }
    }
    xMin = xMin ?? computedXMin;
    xMax = xMax ?? computedXMax;
    yMin = yMin ?? computedYMin;
    yMax = yMax ?? computedYMax;
  }
  if (xMin === xMax) {
    xMin -= 1;
    xMax += 1;
  }
  if (yMin === yMax) {
    yMin -= 1;
    yMax += 1;
  }
  const yPad = (yMax - yMin) * 0.08;
  return { xMin, xMax, yMin: yMin - yPad, yMax: yMax + yPad };
}

function projectLinear(value, domainMin, domainMax, rangeMin, rangeMax) {
  return rangeMin + ((value - domainMin) / (domainMax - domainMin)) * (rangeMax - rangeMin);
}

function ticks(min, max, count) {
  if (!Number.isFinite(min) || !Number.isFinite(max) || count <= 1) {
    return [];
  }
  const step = (max - min) / (count - 1);
  return Array.from({ length: count }, (_, index) => min + step * index);
}

function formatAxisValue(value) {
  const numeric = numberValue(value);
  if (numeric === null) {
    return text(value);
  }
  const absolute = Math.abs(numeric);
  if (absolute >= 1000000 || (absolute > 0 && absolute < 0.001)) {
    return numeric.toExponential(1);
  }
  if (absolute >= 1000) {
    return Intl.NumberFormat(undefined, { maximumFractionDigits: 1, notation: "compact" }).format(numeric);
  }
  if (absolute >= 10) {
    return numeric.toFixed(absolute >= 100 ? 0 : 1);
  }
  return numeric.toFixed(3).replace(/\.?0+$/, "");
}

function updateChartHover(pointer, context) {
  const closest = closestChartPoint(pointer, context.svg, context.rendered, context.width);
  if (!closest) {
    hideChartHover(context);
    return;
  }
  context.hoverLine.classList.remove("is-hidden");
  context.hoverDot.classList.remove("is-hidden");
  updateChartHoverSeriesState(context, closest.series);
  const hoverPosition = `${closest.point.x}:${closest.point.y}:${closest.series.color}`;
  if (hoverPosition !== context.lastHoverPosition) {
    context.hoverLine.setAttribute("x1", closest.point.x);
    context.hoverLine.setAttribute("x2", closest.point.x);
    context.hoverDot.setAttribute("cx", closest.point.x);
    context.hoverDot.setAttribute("cy", closest.point.y);
    context.hoverDot.setAttribute("fill", closest.series.color);
    context.lastHoverPosition = hoverPosition;
  }
  context.tooltip.classList.remove("is-hidden");
  const metricLabel = shortMetricName(context.metricName);
  const smoothedPoint = closest.series.smoothedPoints?.length
    ? nearestPointByX(closest.series.smoothedPoints, closest.point.x)
    : null;
  const hoverKey = [
    closest.series.runID,
    closest.series.runGroupID,
    closest.point.step,
    closest.point.value,
    smoothedPoint?.value ?? "",
  ].join("|");
  if (hoverKey !== context.lastHoverKey) {
    const rows = [
      h("b", {}, closest.series.runID),
      h("span", {}, smoothedPoint ? `raw ${metricLabel} ${formatAxisValue(closest.point.value)}` : `${metricLabel} ${formatAxisValue(closest.point.value)}`),
    ];
    if (smoothedPoint) {
      rows.push(h("span", {}, `EMA ${formatAxisValue(smoothedPoint.value)}`));
    }
    rows.push(h("span", {}, `step ${formatAxisValue(closest.point.step)} · ${closest.series.runGroupID}`));
    context.tooltip.replaceChildren(...rows);
    context.lastHoverKey = hoverKey;
  }
  const bounds = context.wrapper.getBoundingClientRect();
  const left = Math.min(Math.max(pointer.clientX - bounds.left + 12, 8), Math.max(8, bounds.width - 210));
  const top = Math.min(Math.max(pointer.clientY - bounds.top - 8, 8), Math.max(8, bounds.height - 76));
  const transform = `translate3d(${left}px, ${top}px, 0)`;
  if (transform !== context.lastTooltipTransform) {
    context.tooltip.style.transform = transform;
    context.lastTooltipTransform = transform;
  }
}

function closestChartPoint(event, svg, dataset, width) {
  const bounds = svg.getBoundingClientRect();
  if (
    !bounds.width ||
    event.clientX < bounds.left ||
    event.clientX > bounds.right ||
    event.clientY < bounds.top ||
    event.clientY > bounds.bottom
  ) {
    return null;
  }
  const x = ((event.clientX - bounds.left) / bounds.width) * width;
  const y = ((event.clientY - bounds.top) / bounds.height) * Number(svg.getAttribute("viewBox")?.split(" ")[3] || bounds.height);
  let closest = null;
  let closestDistance = Number.POSITIVE_INFINITY;
  for (const series of dataset) {
    const candidate = nearestSeriesHoverCandidate(series, x, y);
    if (!candidate) {
      continue;
    }
    const distance = candidate.distance;
    if (distance < closestDistance) {
      closestDistance = distance;
      closest = { series, point: candidate.point };
    }
  }
  return closest;
}

function nearestSeriesHoverCandidate(series, x, y) {
  const points = series.points || [];
  if (!points.length) {
    return null;
  }
  const nearestIndex = nearestPointIndexByX(points, x);
  const point = points[nearestIndex];
  const left = points[Math.max(0, nearestIndex - 1)];
  const right = points[Math.min(points.length - 1, nearestIndex + 1)];
  let lineY = point.y;
  if (left && right && left.x !== right.x) {
    const segmentLeft = left.x <= x && x <= point.x ? left : point;
    const segmentRight = point.x <= x && x <= right.x ? right : point;
    if (segmentLeft.x !== segmentRight.x) {
      const ratio = (x - segmentLeft.x) / (segmentRight.x - segmentLeft.x);
      lineY = segmentLeft.y + (segmentRight.y - segmentLeft.y) * Math.min(1, Math.max(0, ratio));
    }
  }
  const xDistance = Math.abs(point.x - x) * 0.35;
  const yDistance = Math.abs(lineY - y);
  return {
    point,
    distance: Math.hypot(xDistance, yDistance),
  };
}

function updateChartHoverSeriesState(context, activeSeries) {
  const activeKey = activeSeries ? `${activeSeries.runID}|${activeSeries.runGroupID}` : "";
  if (activeKey === context.activeSeriesKey) {
    return;
  }
  context.wrapper.classList.toggle("has-hover-series", Boolean(activeKey));
  for (const series of context.rendered) {
    const selected = activeKey && `${series.runID}|${series.runGroupID}` === activeKey;
    series.line?.classList.toggle("is-hover-active", Boolean(selected));
    series.smoothedLine?.classList.toggle("is-hover-active", Boolean(selected));
  }
  context.activeSeriesKey = activeKey;
}

function nearestPointByX(points, x) {
  const index = nearestPointIndexByX(points, x);
  return index < 0 ? null : points[index];
}

function nearestPointIndexByX(points, x) {
  if (!points.length) {
    return -1;
  }
  let low = 0;
  let high = points.length - 1;
  while (low < high) {
    const mid = Math.floor((low + high) / 2);
    if (points[mid].x < x) {
      low = mid + 1;
    } else {
      high = mid;
    }
  }
  const right = points[low];
  const left = points[low - 1];
  if (!left) {
    return low;
  }
  if (!right) {
    return low - 1;
  }
  return Math.abs(left.x - x) <= Math.abs(right.x - x) ? low - 1 : low;
}

function chartLegend(dataset) {
  const visible = dataset.slice(0, 8);
  const hiddenCount = Math.max(0, dataset.length - visible.length);
  return h("div", { class: "chart-legend" },
    ...visible.map((series) => h("span", { title: `${series.runID} · ${series.runGroupID}` },
      h("i", { style: `background:${series.color}` }),
      h("b", {}, series.runID),
      h("em", {}, series.runGroupID),
    )),
    hiddenCount ? h("span", { class: "chart-legend-more" }, `+${hiddenCount} more`) : null,
  );
}

window.addEventListener("popstate", () => {
  restoreRouteFromLocation().catch(renderError);
});

bootStellar().catch(renderError);
