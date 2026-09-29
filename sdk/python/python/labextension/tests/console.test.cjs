const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');
const ts = require('typescript');
const React = require('react');
const { renderToStaticMarkup } = require('react-dom/server');

test('loss evidence rejects malformed samples without rejecting lifecycle', () => {
  const { parseRunStatus } = load('model.ts');
  const evidence = { state: 'ready', source: { type: 'stdout', runUid: 'run', pod: 'driver', podUid: 'pod', container: 'submitter' }, samples: [{ step: 1, value: 0.5 }], checkedAt: '2026-09-22T00:00:00Z', stale: false, limitBytes: 65536, maxPoints: 512, possiblyTruncated: true, truncationReasons: ['tail-window'], message: 'Bounded evidence' };
  assert.deepEqual(parseRunStatus(run({ metrics: evidence })).metrics, evidence);
  for (const samples of [[{ step: -1, value: 1 }], [{ step: 1, value: Infinity }], Array(513).fill({ step: 1, value: 1 })]) {
    const status = parseRunStatus(run({ metrics: { ...evidence, samples } }));
    assert.equal(status.state, 'running');
    assert.equal(status.metrics.state, 'error');
    assert.deepEqual(status.metrics.samples, []);
  }
});

test('loss section renders actual points and accessible table only', () => {
  const { LossSection } = load('loss.tsx');
  const metrics = { state: 'ready', source: { type: 'stdout', pod: 'driver', podUid: 'pod-uid', container: 'submitter' }, samples: [{ step: 1, value: 0.5 }], checkedAt: '2026-09-22T00:00:00Z', stale: false, possiblyTruncated: true, truncationReasons: ['tail-window'], message: 'Bounded evidence' };
  const html = renderToStaticMarkup(React.createElement(LossSection, { metrics }));
  for (const anchor of ['taugrid-loss-curve', 'taugrid-loss-samples', 'taugrid-metrics-source', 'taugrid-metrics-freshness', 'taugrid-metrics-truncation']) assert.ok(html.includes(anchor));
  assert.ok(html.includes('<circle'));
  assert.ok(!html.includes('<polyline'));
  assert.ok(html.includes('<title'));
  assert.ok(html.includes('<details'));
  const empty = renderToStaticMarkup(React.createElement(LossSection, {}));
  assert.ok(empty.includes('taugrid-metrics-empty'));
  assert.ok(!empty.includes('<svg'));
});

test('auto watch starts once, respects pause and stops at terminal and hour cap', async () => {
  const { RunMonitor } = load('monitor.ts');
  let now = 0;
  let response = run();
  const timers = new Map();
  let sequence = 0;
  let calls = 0;
  const monitor = new RunMonitor(async () => { calls++; return response; }, () => {}, { now: () => now, set: callback => { timers.set(++sequence, callback); return sequence; }, clear: id => timers.delete(id) }, true);
  const settle = () => new Promise(resolve => setImmediate(resolve));
  monitor.lookup({ namespace: 'research', kind: 'RayJob', name: 'train' });
  await settle();
  assert.equal(monitor.state.watching, true);
  monitor.setWatching(false);
  monitor.refresh();
  await settle();
  assert.equal(monitor.state.watching, false);
  monitor.setWatching(true);
  now = 3600001;
  monitor.refresh();
  await settle();
  assert.equal(monitor.state.watching, false);
  assert.match(monitor.state.watchMessage, /hour/);
  monitor.setWatching(true);
  response = run({ terminal: true, state: 'succeeded', metrics: { state: 'empty' } });
  const before = calls;
  monitor.refresh();
  await settle();
  assert.equal(calls, before + 1);
  assert.equal(monitor.state.status.metrics.state, 'empty');
  assert.equal(monitor.state.watching, false);
  assert.equal(timers.size, 0);
  monitor.dispose();
});

test('loss curve keeps extreme finite observations finite without inventing points', () => {
  const { LossSection } = load('loss.tsx');
  const samples = [{ step: 0, value: -Number.MAX_VALUE }, { step: Number.MAX_SAFE_INTEGER, value: Number.MAX_VALUE }];
  const html = renderToStaticMarkup(React.createElement(LossSection, { metrics: { samples, truncationReasons: [] } }));
  assert.equal((html.match(/<circle/g) || []).length, 2);
  assert.equal((html.match(/<tr>/g) || []).length, 3);
  assert.ok(html.includes('<polyline'));
  assert.ok(!/NaN|Infinity/.test(html));
});

test('profile and queue changes invalidate review and carry into the reviewed submission', async () => {
  const { SubmissionSession } = load('submission.ts');
  const calls = [];
  const session = new SubmissionSession(async (path, payload) => {
    calls.push({ path, payload });
    return path === 'preview' ? preview : { submitted: true, namespace: plan.namespace, name: plan.name, kind: 'RayJob', payloadDigest: plan.payloadDigest, plan };
  }, () => {});
  await session.review({ notebook: {}, path: 'demo.ipynb', namespace: 'research', profile: 'old', queue: 'old-q' });
  session.invalidate();
  assert.equal(session.state.preview, null);
  assert.equal(await session.submit(plan, true, true), false);
  await session.review({ notebook: {}, path: 'demo.ipynb', namespace: 'research', profile: 'cpu', queue: 'cpu-q' });
  assert.equal(await session.submit(plan, true, true), true);
  assert.equal(calls.at(-1).payload.profile, 'cpu');
  assert.equal(calls.at(-1).payload.queue, 'cpu-q');
  session.dispose();
  const source = fs.readFileSync(path.resolve(__dirname, '../src/submitview.tsx'), 'utf8');
  for (const field of ['profile', 'queue']) {
    assert.ok(source.includes(`data-testid="taugrid-submit-${field}"`));
  }
});

function load(source, overrides = {}) {
  const filename = path.resolve(__dirname, '../src', source);
  const output = ts.transpileModule(fs.readFileSync(filename, 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.React, target: ts.ScriptTarget.ES2022 }
  }).outputText;
  const module = { exports: {} };
  const resolve = name => overrides[name] || (name.startsWith('.')
    ? load(name.slice(2) + (fs.existsSync(path.resolve(__dirname, '../src', name + '.tsx')) ? '.tsx' : '.ts'), overrides)
    : require(name));
  new Function('require', 'module', 'exports', output)(resolve, module, module.exports);
  return module.exports;
}

const run = overrides => ({
  existing: true, name: 'train-42', namespace: 'research', state: 'running',
  readyPods: 1, totalPods: 2, terminal: false, pods: [], diagnostics: [], ...overrides
});

const plan = { name: 'train-reviewed', namespace: 'research', queue: 'gpu', profile: 'small', planDigest: 'a'.repeat(64), payloadDigest: 'digest', notebookBytes: 20, preparedBytes: 10, encodedEnvBytes: 16, excludedCells: [] };
const preview = { plan, submittable: true, submissionEnabled: true };
const surfaceOverrides = { './api': {}, '@jupyterlab/apputils': { ReactWidget: class {} } };

function reviewHarness({ catalog, files, capability, post, confirm } = {}) {
  const slots = [];
  let cursor = 0;
  const effects = [];
  const cleanups = [];
  const requests = [];
  const hookReact = { ...React,
    useState(initial) {
      const index = cursor++;
      if (!(index in slots)) slots[index] = initial;
      return [slots[index], value => { slots[index] = typeof value === 'function' ? value(slots[index]) : value; }];
    },
    useRef(initial) { const index = cursor++; return slots[index] ||= { current: initial }; },
    useId() { return 'review-test'; },
    useEffect(effect, dependencies) {
      const index = cursor++;
      if (!slots[index] || dependencies.some((value, offset) => value !== slots[index][offset])) {
        slots[index] = dependencies;
        effects.push(() => { cleanups[index]?.(); cleanups[index] = effect(); });
      }
    }
  };
  const { SubmissionReview } = load('submitview.tsx', { react: hookReact,
    './widget': { useCapabilities: () => capability || { value: { submissionEnabled: true, submissionImplemented: true }, retry() {} } },
    './api': {
      apiGet: async (route, query, signal) => {
        requests.push({ route, query, signal });
        return route === 'destinations' ? (typeof catalog === 'function' ? catalog(query) : catalog || destinationCatalog)
          : typeof files === 'function' ? files() : files || { files: [{ name: 'helper.py', size: 10 }], warnings: [], budgetBytes: 1048576 };
      },
      apiPost: async (route, body) => {
        requests.push({ route, body });
        return post ? post(route, body) : route === 'preview' ? preview : { submitted: true, namespace: plan.namespace, name: plan.name, kind: 'RayJob', payloadDigest: plan.payloadDigest, plan };
      }
    }
  });
  const props = { notebook: { name: 'source.ipynb', toJSON: () => JSON.stringify({ cells: [{ cell_type: 'code', source: ['import helper'] }] }) }, onConfirm: confirm || (async () => true), onOpenRun() {}, onAbout() {} };
  let tree;
  const nodes = value => !value || typeof value !== 'object' ? [] : Array.isArray(value) ? value.flatMap(nodes) : [value, ...nodes(value.props?.children)];
  const text = value => typeof value === 'string' || typeof value === 'number' ? String(value) : Array.isArray(value) ? value.map(text).join('') : text(value?.props?.children || '');
  const render = () => { cursor = 0; tree = SubmissionReview(props); effects.splice(0).forEach(effect => effect()); };
  render();
  return {
    requests, props, render, text: () => text(tree),
    all: predicate => nodes(tree).filter(predicate),
    find: predicate => { const result = nodes(tree).find(predicate); assert.ok(result, 'Expected UI control'); return result; },
    async settle() { await new Promise(resolve => setImmediate(resolve)); render(); },
    dispose() { cleanups.forEach(cleanup => cleanup?.()); }
  };
}

test('submission UI selects real choices, invalidates edits and keeps confirmation single-flight', async () => {
  let accept;
  let confirmations = 0;
  const harness = reviewHarness({ confirm: () => { confirmations++; return new Promise(resolve => { accept = resolve; }); } });
  const byId = id => harness.find(node => node.props?.['data-testid'] === id);
  assert.equal(byId('taugrid-review-submission').props.disabled, true);
  await harness.settle();
  assert.equal(byId('taugrid-submit-namespace').type, 'select');
  assert.equal(byId('taugrid-submit-profile').props.value, '');
  assert.equal(byId('taugrid-submit-queue').props.value, 'gpu');
  byId('taugrid-submit-profile').props.onChange({ target: { value: 'small' } });
  harness.render();
  assert.equal(byId('taugrid-review-submission').props.disabled, false);
  const reviewNow = async () => { harness.find(node => node.type === 'form').props.onSubmit({ preventDefault() {} }); await harness.settle(); };
  await reviewNow();
  assert.equal(byId('taugrid-submit-notebook').props.disabled, false);
  byId('taugrid-submit-queue').props.onChange({ target: { value: 'other' } });
  harness.render();
  assert.equal(byId('taugrid-submit-notebook').props.disabled, true);
  assert.match(harness.find(node => node.type?.name === 'SubmissionStatus').props.notice, /Review again/);
  await reviewNow();
  byId('taugrid-submit-notebook').props.onClick();
  byId('taugrid-submit-notebook').props.onClick();
  harness.render();
  assert.equal(confirmations, 1);
  assert.equal(byId('taugrid-submit-profile').props.disabled, true);
  accept(false);
  await harness.settle();
  assert.match(harness.find(node => node.type?.name === 'SubmissionStatus').props.notice, /cancelled/);
  assert.equal(harness.requests.filter(request => request.route === 'submit').length, 0);
  await reviewNow();
  assert.equal(harness.find(node => node.type?.name === 'SubmissionStatus').props.notice, '');
  byId('taugrid-submit-notebook').props.onClick();
  accept(true);
  await harness.settle();
  assert.equal(harness.requests.filter(request => request.route === 'submit').length, 1);
  assert.equal(harness.all(node => node.type === 'form').length, 0);
  assert.equal(harness.find(node => node.type?.name === 'SubmissionOutcome').props.state.submitted.name, plan.name);
  harness.dispose();
});

test('destination refresh invalidates review and can recover a removed namespace', async () => {
  let removed = false;
  const harness = reviewHarness({ catalog: query => {
    if (removed && query?.namespace) throw new Error('Namespace is not visible');
    return removed ? { ...destinationCatalog, namespace: 'replacement', namespaces: [{ name: 'replacement', tauEnabled: true }] } : destinationCatalog;
  } });
  await harness.settle();
  harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-profile').props.onChange({ target: { value: 'small' } });
  harness.render();
  harness.find(node => node.type === 'form').props.onSubmit({ preventDefault() {} });
  await harness.settle();
  removed = true;
  const refresh = () => harness.find(node => node.type === 'button' && node.props.children === 'Refresh destinations').props.onClick();
  refresh();
  harness.render();
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-notebook').props.disabled, true);
  await harness.settle();
  refresh();
  await harness.settle();
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-namespace').props.value, 'replacement');
  harness.dispose();
});

test('file search preserves opt-in selections, refresh clears review, oversized selections block review', async () => {
  const harness = reviewHarness({ files: { files: [{ name: 'helper.py', size: 11 }], warnings: [], budgetBytes: 10 } });
  await harness.settle();
  harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-profile').props.onChange({ target: { value: 'small' } });
  harness.find(node => node.props?.type === 'checkbox').props.onChange({ target: { checked: true } });
  harness.render();
  assert.match(harness.text(), /Selected files exceed/);
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-review-submission').props.disabled, true);
  harness.find(node => node.props?.['data-testid'] === 'taugrid-file-search').props.onChange({ target: { value: 'absent' } });
  harness.render();
  assert.match(harness.text(), /No filenames match/);
  assert.match(harness.text(), /1 selected/);
  harness.find(node => node.type === 'button' && node.props.children === 'Refresh files').props.onClick();
  await harness.settle();
  await harness.settle();
  assert.match(harness.text(), /0 selected/);
  harness.dispose();
});

test('empty or failed destinations never create invented selectable choices', async () => {
  for (const catalog of [{ ...destinationCatalog, profiles: [], queues: [], defaultQueue: 'missing' }, () => { throw new Error('offline'); }]) {
    const harness = reviewHarness({ catalog });
    await harness.settle();
    assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-review-submission').props.disabled, true);
    assert.equal(harness.all(node => node.type === 'option' && node.props.value === 'missing').length, 0);
    assert.ok(harness.all(node => node.type === 'button' && node.props.children === 'Refresh destinations').length);
    harness.dispose();
  }
});

test('unreadable files require explicit notebook-only recovery; capability failure never enables writing', async () => {
  const harness = reviewHarness({ files: { files: [], readable: false, warnings: [] }, capability: { value: null, error: 'offline', retry() {} } });
  await harness.settle();
  harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-profile').props.onChange({ target: { value: 'small' } });
  harness.render();
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-review-submission').props.disabled, true);
  harness.find(node => node.type === 'button' && node.props.children === 'Continue without companion files').props.onClick();
  harness.render();
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-review-submission').props.disabled, false);
  harness.find(node => node.type === 'form').props.onSubmit({ preventDefault() {} });
  await harness.settle();
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-notebook').props.disabled, true);
  assert.match(harness.text(), /Retry capabilities/);
  harness.dispose();
});

test('submission statuses announce every review, confirmation, write and outcome state', () => {
  const { SubmissionStatus } = load('submissionui.tsx');
  const idle = { preview: null, busy: false, submitted: null, attempted: null, uncertain: false, error: null };
  for (const [changes, confirming, notice, expected] of [[{}, false, '', /Choose a destination/],
    [{ busy: true }, false, '', /Reviewing submission/], [{ preview }, false, '', /Review ready/],
    [{ preview }, true, '', /Awaiting confirmation/], [{ preview }, false, 'Submission cancelled. Nothing created.', /Submission cancelled/],
    [{ busy: true, attempted: plan }, false, '', /do not submit again/], [{ uncertain: true, attempted: plan }, false, '', /Inspect the planned run/],
    [{ submitted: plan }, false, '', /Notebook submitted/], [{}, false, 'Choices changed. Review again.', /Choices changed/]]) {
    const html = renderToStaticMarkup(React.createElement(SubmissionStatus, { state: { ...idle, ...changes }, confirming, notice }));
    assert.match(html, expected);
    assert.match(html, /aria-live="polite"/);
    assert.equal((html.match(/aria-current="step"/g) || []).length, 1);
  }
});

test('receipt reports exact package bytes, resources, execution and cleanup without capacity promises', () => {
  const { SubmissionReceipt, SubmissionOutcome, FailureNotice, possibleImports, parseFileList } = load('submissionui.tsx');
  const html = renderToStaticMarkup(React.createElement(SubmissionReceipt, { notebookName: 'source.ipynb', plan: { ...plan,
    workers: 2, gpusPerWorker: [1], cpusPerWorker: ['4'], memoryPerWorker: ['8Gi'],
    fileSizes: { 'helper.py': 7 }, packagedFileSizes: { 'notebook.ipynb': 10, 'runner.py': 23, 'helper.py': 7 }, includedFiles: ['helper.py'],
    decodedBytes: 40, payloadBudgetBytes: 1048576, encodedBudgetBytes: 65536, submissionMode: 'K8sJobMode', retentionSeconds: 600, shutdownAfterFinish: true
  } }));
  for (const expected of [/40 B/, /1,048,576 B/, /65,536 B/, /runner.py/, /helper.py/, /600 seconds/, /shut down/, /GPU availability has not been checked/, /not guaranteed/]) assert.match(html, expected);
  const success = renderToStaticMarkup(React.createElement(SubmissionOutcome, { state: { submitted: plan }, onOpenRun() {}, onAcknowledge() {} }));
  assert.match(success, /Open submitted run/);
  assert.match(success, /research\/train-reviewed/);
  const uncertain = renderToStaticMarkup(React.createElement(SubmissionOutcome, { state: { uncertain: true, attempted: plan }, onOpenRun() {}, onAcknowledge() {} }));
  assert.match(uncertain, /Inspect planned run/);
  assert.match(uncertain, /disabled=""[^>]*>I checked/);
  assert.match(uncertain, /not proof/);
  assert.match(renderToStaticMarkup(React.createElement(FailureNotice, { failure: { title: 'Failed', action: 'Refresh files', detail: '<script>unsafe</script>' } })), /&lt;script&gt;/);
  assert.deepEqual(possibleImports(JSON.stringify({ cells: [{ cell_type: 'code', source: ['import helper\nfrom data import x'] }, { cell_type: 'markdown', source: 'import ignored' }] })), ['helper.py', 'data.py']);
  assert.deepEqual(possibleImports('invalid'), []);
  assert.throws(() => parseFileList({ files: [{ name: 'bad', size: -1 }] }));
  assert.throws(() => parseFileList({ files: [], readable: false }));
});

test('empty choices explain recovery without enabling submission', async () => {
  const harness = reviewHarness({ catalog: { ...destinationCatalog, namespace: null, namespaces: [], profiles: [], queues: [], defaultQueue: null },
    files: { files: [], warnings: [] }, capability: { value: { submissionEnabled: false, submissionImplemented: true }, retry() {} } });
  assert.match(harness.text(), /Loading destinations/);
  await harness.settle();
  for (const expected of [/No namespaces/, /No ready worker profiles/, /No queues/, /No eligible companion files/, /Check submission availability/]) assert.match(harness.text(), expected);
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-review-submission').props.disabled, true);
  assert.equal(harness.find(node => node.props?.['data-testid'] === 'taugrid-submit-notebook').props.disabled, true);
  harness.dispose();
});

test('catalog disposal and mismatched namespaces never revive stale options', async () => {
  const { DestinationSession } = load('destinations.ts');
  const mismatched = new DestinationSession(async () => destinationCatalog, () => {});
  await mismatched.refresh('different');
  assert.equal(mismatched.state.value, null);
  assert.match(mismatched.state.error, /different namespace/);
  let resolve;
  const changes = [];
  const disposed = new DestinationSession(() => new Promise(complete => { resolve = complete; }), state => changes.push(state));
  const request = disposed.refresh('research');
  disposed.dispose();
  resolve(destinationCatalog);
  await request;
  assert.equal(changes.length, 1);
  assert.equal(disposed.state.value, null);
});

const destinationCatalog = {
  namespace: 'research', namespaces: [{ name: 'research', tauEnabled: true }],
  profiles: [{ name: 'small', description: 'One worker', workers: 1, gpusPerWorker: 0,
    cpusPerWorker: '4', memoryPerWorker: '8Gi', image: 'runtime', priority: null, queue: 'gpu' }],
  queues: ['gpu', 'other'], defaultQueue: 'gpu', warnings: [],
  limits: { namespaces: 500, profiles: 200, queues: 500 }
};

test('destinations reject malformed or unbounded catalogs and share namespace presentation', () => {
  const { parseDestinations, namespaceLabel, preferredNamespace } = load('destinations.ts');
  assert.deepEqual(parseDestinations(destinationCatalog), destinationCatalog);
  assert.match(namespaceLabel(destinationCatalog.namespaces[0]), /Tau-labelled/);
  assert.equal(preferredNamespace([{ name: 'default', tauEnabled: false }, ...destinationCatalog.namespaces]), 'research');
  for (const bad of [{ queues: ['gpu', 'gpu'] }, { profiles: Array(201).fill(destinationCatalog.profiles[0]) },
    { profiles: [{ ...destinationCatalog.profiles[0], workers: -1 }] }, { namespace: 'hidden' }]) {
    assert.throws(() => parseDestinations({ ...destinationCatalog, ...bad }));
  }
});

test('destination changes and refresh discard stale options without inventing a default queue', async () => {
  const { DestinationSession } = load('destinations.ts');
  const requests = [];
  const session = new DestinationSession((namespace, signal) => new Promise((resolve, reject) => requests.push({ namespace, signal, resolve, reject })), () => {});
  const first = session.refresh('research');
  const second = session.refresh('other');
  assert.equal(requests[0].signal.aborted, true);
  requests[1].resolve({ ...destinationCatalog, namespace: 'other', namespaces: [{ name: 'other', tauEnabled: false }], queues: [] });
  await second;
  requests[0].resolve(destinationCatalog);
  await first;
  assert.equal(session.state.value.namespace, 'other');
  assert.deepEqual(session.state.value.queues, []);
  const third = session.refresh('other');
  assert.equal(session.state.value, null);
  requests[2].reject(new Error('offline'));
  await third;
  assert.match(session.state.error, /offline/);
  session.dispose();
});

test('submission failures retain a cause-specific next action', () => {
  const { submissionFailure } = load('submission.ts');
  for (const [status, message, action] of [[400, 'bad name', /name|notebook|file/i], [403, 'forbidden', /permission/i],
    [413, 'too large', /files|outputs/i], [409, 'submission disabled', /operator/i],
    [409, 'plan changed', /review/i], [502, 'cluster failed', /connection/i]]) {
    assert.match(submissionFailure({ status, message }).action, action);
  }
  assert.match(submissionFailure(new Error('timeout'), true).action, /Inspect planned run/);
});

test('submission snapshots file selections and requires acknowledgement after an uncertain write', async () => {
  const { SubmissionSession } = load('submission.ts');
  const calls = [];
  const session = new SubmissionSession(async (path, body) => { calls.push({ path, body }); if (path === 'submit') throw new Error('lost response'); return preview; }, () => {});
  const files = ['helper.py'];
  await session.review({ notebook: 'snapshot', path: 'source.ipynb', namespace: 'research', files });
  files.push('later.py');
  await session.submit(plan, true, true);
  assert.deepEqual(calls[1].body.files, ['helper.py']);
  await session.review({ notebook: 'retry', path: 'source.ipynb', namespace: 'research' });
  assert.equal(calls.length, 2);
  session.acknowledgeUncertain();
  await session.review({ notebook: 'retry', path: 'source.ipynb', namespace: 'research' });
  assert.equal(calls.length, 3);
});

test('plugin routes discovery left, reuses native tabs and binds notebook toolbar reviews', async () => {
  const commands = new Map();
  const added = [];
  const toolbar = [];
  class SurfaceWidget {
    constructor(element) { this.element = element; this.title = {}; this.id = ''; this.isDisposed = false; this.isAttached = false; this.disposed = { connect() {} }; }
  }
  class WidgetTracker {
    constructor() { this.widgets = []; }
    async add(widget) { this.widgets.push(widget); }
    has(widget) { return this.widgets.includes(widget); }
  }
  const notebook = { id: 'notebook-1', isDisposed: false, context: { path: 'source.ipynb', model: { toJSON: () => ({ cells: [] }) } }, toolbar: { addItem: (name, button) => toolbar.push({ name, button }) } };
  const notebooks = { currentWidget: notebook, forEach: callback => callback(notebook), widgetAdded: { connect() {} }, currentChanged: { connect() {} } };
  const app = { commands: { addCommand: (name, command) => commands.set(name, command), notifyCommandChanged() {} }, shell: { currentWidget: notebook, currentChanged: { connect() {} }, add: (widget, area) => { widget.isAttached = true; added.push({ widget, area }); }, activateById() {} } };
  const restored = [];
  const restorer = { add: (widget, name) => restored.push({ widget, name }), restore: (tracker, options) => { restored.push({ tracker, options }); return Promise.resolve(); } };
  const plugin = load('index.ts', {
    '@jupyterlab/application': {}, '@jupyterlab/launcher': {}, '@jupyterlab/notebook': {},
    '@jupyterlab/apputils': { WidgetTracker, ToolbarButton: class { constructor(options) { Object.assign(this, options); } }, Dialog: { cancelButton() {}, okButton() {} }, showDialog: async () => ({ button: { accept: false } }), ReactWidget: { create: element => element } },
    './widget': { SurfaceWidget, RunsSidebar() {}, RunDetail() {}, RunLogs() {}, About() {}, runWidgetId: target => JSON.stringify(target) },
    './submitview': { SubmissionReview() {}, SubmissionConfirmation() {} }
  }).default;
  plugin.activate(app, restorer, { add() {} }, { addItem() {} }, notebooks);
  await commands.get('taugrid:open').execute();
  assert.equal(added[0].area, 'left');
  assert.equal(added[0].widget.id, 'taugrid-runs');
  const target = { namespace: 'research', kind: 'RayJob', name: 'train' };
  await commands.get('taugrid:open-run').execute(target);
  await commands.get('taugrid:open-run').execute(target);
  assert.equal(added.filter(item => item.area === 'main').length, 1);
  assert.equal(toolbar[0].name, 'taugrid-submit');
  assert.equal(commands.get('taugrid:submit-notebook').isEnabled(), true);
  app.shell.currentWidget = added[1].widget;
  assert.equal(commands.get('taugrid:submit-notebook').isEnabled(), false);
  await toolbar[0].button.onClick();
  const review = added.at(-1).widget;
  assert.equal(review.element.props.notebook.name, 'source.ipynb');
  notebooks.currentWidget = { context: { path: 'other.ipynb' } };
  assert.equal(review.element.props.notebook.toJSON(), '{"cells":[]}');
  assert.equal(restored.filter(item => item.options).length, 1);
});

test('sidebar owns discovery and lookup, never monitoring or submission', () => {
  const { RunsSidebar } = load('widget.tsx', surfaceOverrides);
  const html = renderToStaticMarkup(React.createElement(RunsSidebar, { onOpenRun() {}, onAbout() {} }));
  for (const text of ['taugrid-runs', 'Namespace', 'LocalQueue filter', 'Refresh', 'Find exact run', 'Kind', 'RayJob', 'Check run', 'About']) assert.ok(html.includes(text), text);
  // The namespace is a filterable selection, not a free-text guess.
  assert.ok(html.includes('list="'), 'namespace input must offer a filtered list');
  assert.ok(html.includes('<datalist'), 'namespace options must render');
  assert.ok(html.includes('Filter namespaces'));
  assert.ok(html.includes('type one to continue'));
  for (const text of ['Submit notebook', 'Lifecycle evidence', 'Load logs']) assert.ok(!html.includes(text), text);
});

test('run detail and logs are independent identity-scoped surfaces', () => {
  const { RunDetail, RunLogs, runWidgetId } = load('widget.tsx', surfaceOverrides);
  const target = { namespace: 'research', name: 'train', kind: 'Job' };
  const detail = renderToStaticMarkup(React.createElement(RunDetail, { target, onOpenLogs() {} }));
  assert.ok(detail.includes('taugrid-detail'));
  assert.ok(detail.includes('Refresh run'));
  assert.ok(!detail.includes('List runs'));
  assert.ok(!detail.includes('Submit notebook'));
  const logs = renderToStaticMarkup(React.createElement(RunLogs, { target }));
  assert.ok(logs.includes('taugrid-logs'));
  assert.ok(logs.includes('Refresh pods'));
  assert.notEqual(runWidgetId(target), runWidgetId({ ...target, kind: 'RayJob' }));
  assert.notEqual(runWidgetId(target), runWidgetId({ ...target, namespace: 'another' }));
  assert.notEqual(runWidgetId(target), runWidgetId(target, 'logs'));
});

test('submission review owns plan and source; confirmation names the exact write', () => {
  const { SubmissionReview, SubmissionConfirmation } = load('submitview.tsx', surfaceOverrides);
  const html = renderToStaticMarkup(React.createElement(SubmissionReview, { notebook: { name: 'source.ipynb', toJSON: () => '{}' }, onConfirm: async () => false, onOpenRun() {}, onAbout() {} }));
  for (const text of ['taugrid-review', 'source.ipynb', 'Review submission', 'Submit notebook']) assert.ok(html.includes(text), text);
  assert.ok(!html.includes('List runs'));
  assert.ok(!html.includes('Watch this run'));
  const confirmation = renderToStaticMarkup(React.createElement(SubmissionConfirmation, { plan }));
  for (const text of ['research', 'train-reviewed', 'gpu', 'reviewed notebook snapshot']) assert.ok(confirmation.includes(text), text);
});

test('submission commits immutable reviewed bytes only after confirmation and both gates', async () => {
  const { SubmissionSession } = load('submission.ts');
  const calls = [];
  const session = new SubmissionSession(async (path, body) => { calls.push({ path, body }); return path === 'preview' ? preview : { submitted: true, name: plan.name, namespace: plan.namespace, kind: 'RayJob', payloadDigest: plan.payloadDigest, plan }; }, () => {});
  const source = { notebook: 'original', path: 'source.ipynb', namespace: 'research' };
  await session.review(source);
  source.notebook = 'edited';
  assert.equal(await session.submit(plan, false, true), false);
  assert.equal(await session.submit(plan, true, false), false);
  assert.equal(calls.length, 1);
  assert.equal(await session.submit(plan, true, true), true);
  assert.equal(calls[1].body.notebook, 'original');
  assert.equal(calls[1].body.name, plan.name);
  assert.equal(calls[1].body.confirm, true);
  assert.equal(await session.submit(plan, true, true), false);
  assert.equal(calls.length, 2);
});

test('preview gate, invalidation and stale confirmations cannot submit', async () => {
  const { SubmissionSession } = load('submission.ts');
  let enabled = false;
  const calls = [];
  const session = new SubmissionSession(async (path) => { calls.push(path); return { ...preview, submittable: enabled }; }, () => {});
  await session.review({ notebook: '{}', path: 'source.ipynb', namespace: 'research' });
  assert.equal(await session.submit(plan, true, true), false);
  enabled = true;
  await session.review({ notebook: '{}', path: 'source.ipynb', namespace: 'research' });
  session.invalidate();
  assert.equal(await session.submit(plan, true, true), false);
  assert.deepEqual(calls, ['preview', 'preview']);
});

test('replaced/disposed reviews abort and discard late results', async () => {
  const { SubmissionSession } = load('submission.ts');
  const pending = [];
  const states = [];
  const session = new SubmissionSession((path, body, signal) => new Promise(resolve => pending.push({ signal, resolve })), state => states.push(state));
  const first = session.review({ notebook: 'old', path: 'old.ipynb', namespace: 'ray' });
  const second = session.review({ notebook: 'new', path: 'new.ipynb', namespace: 'ray' });
  assert.equal(pending[0].signal.aborted, true);
  pending[0].resolve(preview);
  await first;
  assert.equal(session.state.preview, null);
  session.dispose();
  const count = states.length;
  assert.equal(pending[1].signal.aborted, true);
  pending[1].resolve(preview);
  await second;
  assert.equal(states.length, count);
});

test('failed writes expose uncertain outcome and cannot be retried without review', async () => {
  const { SubmissionSession } = load('submission.ts');
  const session = new SubmissionSession(async path => { if (path === 'preview') return preview; throw new Error('timeout'); }, () => {});
  await session.review({ notebook: '{}', path: 'source.ipynb', namespace: 'ray' });
  assert.equal(await session.submit(plan, true, true), false);
  assert.equal(session.state.uncertain, true);
  assert.equal(session.state.preview, null);
  assert.deepEqual(session.state.attempted, { namespace: plan.namespace, name: plan.name, kind: 'RayJob' });
  assert.equal(await session.submit(plan, true, true), false);
});

test('copy distinguishes admission, execution, missing and unreachable', () => {
  const { describeRun } = load('model.ts');
  assert.equal(describeRun(run({ state: 'queued', admitted: false })).label, 'Waiting for admission');
  assert.equal(describeRun(run({ state: 'queued', admitted: true })).label, 'Queued');
  assert.equal(describeRun(run({ state: 'not_submitted', existing: false })).label, 'No such run');
  assert.equal(describeRun(run({ state: 'unknown', existing: false })).label, 'Unreachable');
  assert.equal(describeRun(run({ state: 'complete' })).label, 'Finished');
  assert.match(describeRun(run()).detail, /not.*training health/i);
});

test('render includes identifiers, unknown admission, diagnostics and link-out', () => {
  const { RunDetails } = load('view.tsx');
  const html = renderToStaticMarkup(React.createElement(RunDetails, {
    status: run({ jobId: 'job-17', rayClusterName: 'cluster-17', diagnostics: [
      { code: 'restart', severity: 'warning', message: 'Worker restarted', suggestion: 'Inspect worker logs.' }
    ], pods: [{ name: 'worker-1', ready: false, restarts: 3, phase: 'Running', node: 'gpu-1' }] }),
    stale: false, statusUrl: '/user/alice/taugrid/api/status?name=train-42'
  }));
  for (const text of ['taugrid-run', 'train-42', 'job-17', 'cluster-17', 'Not reported',
    'Worker restarted', 'Inspect worker logs.', '3 restarts', 'Not ready', 'Open status JSON']) {
    assert.ok(html.includes(text), text);
  }
  assert.ok(html.includes('/user/alice/taugrid/api/status?name=train-42'));
});

test('stale data is not presented as a current running state', () => {
  const { RunDetails } = load('view.tsx');
  const html = renderToStaticMarkup(React.createElement(RunDetails, {
    status: run(), stale: true, statusUrl: '/status'
  }));
  assert.ok(html.includes('Status unavailable'));
  assert.ok(html.includes('Last known state: Running'));
});

function harness() {
  const { RunMonitor } = load('monitor.ts');
  const pending = [];
  const timers = new Map();
  let nextTimer = 0;
  const monitor = new RunMonitor((target, signal) => new Promise((resolve, reject) => {
    pending.push({ target, signal, resolve, reject });
  }), () => {}, {
    set: callback => { timers.set(++nextTimer, callback); return nextTimer; },
    clear: handle => timers.delete(handle), now: () => 1234
  });
  return { monitor, pending, timers };
}

const settle = () => new Promise(resolve => setImmediate(resolve));

test('browser and logs render labeled native controls and partial-list warnings', () => {
  const { RunBrowser, RunRows, LogViewer, LogContent } = load('explorer.tsx', { './api': {} });
  const browser = renderToStaticMarkup(React.createElement(RunBrowser, { namespace: 'ray', onSelect: () => {}, portalUrl: null }));
  // The list loads itself; the only control is an explicit refresh.
  for (const text of ['Live runs', 'LocalQueue filter', 'Refresh', 'durable history']) { assert.ok(browser.includes(text), text); }
  assert.ok(!browser.includes('List runs'));
  const rows = renderToStaticMarkup(React.createElement(RunRows, { result: {
    runs: [{ namespace: 'ray', name: 'train', kind: 'Job', state: 'queued' }, { namespace: 'ray', name: 'train', kind: 'RayJob', state: 'Running' }],
    warnings: ['RayJob read denied'], truncated: true }, onSelect: () => {} }));
  for (const text of ['Job', 'RayJob', 'RayJob read denied', 'partial', 'train']) { assert.ok(rows.includes(text), text); }
  // Rows are a table, not a card list.
  for (const text of ['<table', '<thead', 'Run', 'Kind', 'State', 'Queue', 'Created']) { assert.ok(rows.includes(text), text); }
  assert.ok(rows.includes('taugrid-run-link'));
  const logs = renderToStaticMarkup(React.createElement(LogViewer, { status: run({ kind: 'Job', pods: [{ name: 'worker', containers: ['main', 'init'], ready: true, restarts: 0 }] }) }));
  for (const text of ['Pod', 'Container', 'Tail lines', 'Previous container', 'Timestamps', 'Load logs', '1000']) { assert.ok(logs.includes(text), text); }
  const content = renderToStaticMarkup(React.createElement(LogContent, { value: { pod: 'worker', container: 'main', text: '<script>alert(1)</script>', limitBytes: 65536, possiblyTruncated: true } }));
  assert.ok(content.includes('&lt;script&gt;'));
  assert.ok(content.includes('truncated'));
  assert.ok(!content.includes('<script>'));
});

test('additive phases, kind and containers validate without breaking old status', () => {
  const { parseRunStatus } = load('model.ts');
  assert.deepEqual(parseRunStatus(run()), run());
  const phases = [{ key: 'pods', label: 'Pods', state: 'unknown', detail: 'Read denied', hint: 'Check RBAC' }];
  assert.deepEqual(parseRunStatus(run({ kind: 'Job', phases })).phases, phases);
  for (const extra of [{ kind: 'Pod' }, { phases: [{}] }, { output: { portalUrl: 5 } },
    { pods: [{ name: 'pod', ready: false, restarts: 0, containers: [5] }] }]) {
    assert.throws(() => parseRunStatus(run(extra)), /invalid run status/i);
  }
});

test('run listing and logs reject malformed payloads, keep explicit partial evidence', () => {
  const { parseRuns, parseLogs } = load('model.ts');
  const result = { runs: [{ name: 'same', namespace: 'ray', kind: 'Job', state: 'queued', queue: null, created: null },
    { name: 'same', namespace: 'ray', kind: 'RayJob', state: 'Running', queue: 'gpu', created: '2026-09-21' }], warnings: ['Job read denied'], truncated: true };
  assert.deepEqual(parseRuns(result), result);
  assert.throws(() => parseRuns({ ...result, runs: [{ name: 'bad', kind: 'Pod' }] }), /invalid/i);
  assert.throws(() => parseLogs({ text: {} }), /invalid/i);
});

test('API encodes kind, queue and bounded log options under the Jupyter base', () => {
  const { apiUrl } = load('api.ts', {
    '@jupyterlab/coreutils': { URLExt: { join: (...parts) => parts.join('/').replace(/\/{2,}/g, '/') } },
    '@jupyterlab/services': { ServerConnection: { makeSettings: () => ({ baseUrl: '/user/me/' }) } }
  });
  assert.equal(apiUrl('status', { namespace: 'ray', name: 'same', kind: 'Job' }), '/user/me/taugrid/api/status?namespace=ray&name=same&kind=Job');
  assert.match(apiUrl('runs', { namespace: 'ray', queue: 'gpu&other' }), /queue=gpu%26other/);
  assert.match(apiUrl('logs', { namespace: 'ray', name: 'same', kind: 'Job', pod: 'worker', container: 'main', tail: '200', previous: 'false' }), /kind=Job&pod=worker&container=main&tail=200&previous=false/);
});

test('lifecycle and results expose evidence, escape paths and only link safe portal URLs', () => {
  const { RunDetails } = load('view.tsx');
  const html = renderToStaticMarkup(React.createElement(RunDetails, {
    status: run({ kind: 'Job', phases: [{ key: 'pods', label: 'Pods', state: 'warning', detail: 'ImagePullBackOff', hint: 'Check image access' }],
      output: { path: '<script>results</script>', pvc: 'output-pvc', portalUrl: 'https://portal.example/' } }), stale: false, statusUrl: '/status'
  }));
  for (const text of ['Lifecycle evidence', 'ImagePullBackOff', 'Check image access', 'output-pvc', '&lt;script&gt;', 'noopener noreferrer']) {
    assert.ok(html.includes(text), text);
  }
  const unsafe = renderToStaticMarkup(React.createElement(RunDetails, {
    status: run({ output: { portalUrl: 'javascript:alert(1)' } }), stale: false, statusUrl: '/status'
  }));
  assert.ok(!unsafe.includes('href="javascript:'));
});

test('snapshot requests abort replacements, discard stale results and clear old logs on error', async () => {
  const { SnapshotRequest } = load('snapshot.ts');
  const states = [];
  const request = new SnapshotRequest(state => states.push(state));
  let finishOld;
  let oldSignal;
  const first = request.load(signal => { oldSignal = signal; return new Promise(resolve => { finishOld = resolve; }); });
  await request.load(async () => 'new');
  assert.equal(oldSignal.aborted, true);
  finishOld('old');
  await first;
  assert.equal(states.at(-1).value, 'new');
  await request.load(async () => { throw new Error('denied'); });
  assert.equal(states.at(-1).value, null);
  assert.equal(states.at(-1).error, 'denied');
  request.dispose();
});

test('invalid status JSON is rejected before rendering', () => {
  const { parseRunStatus } = load('model.ts');
  assert.deepEqual(parseRunStatus(run()), run());
  for (const value of [null, {}, run({ pods: null }), run({ readyPods: -1 }),
    run({ admitted: 'true' }), run({ state: {} }),
    run({ pods: [{ name: 'worker', ready: true, restarts: 'unknown' }] }),
    run({ diagnostics: [{ code: 'bad', severity: 'error', message: {} }] })]) {
    assert.throws(() => parseRunStatus(value), /invalid run status/i);
  }
});

test('API preserves base URL, encodes lookup, and reports HTTP and JSON errors', async () => {
  const calls = [];
  let response = { ok: true, text: async () => JSON.stringify(run()) };
  const previousWindow = global.window;
  global.window = { setTimeout, clearTimeout };
  try {
    const { apiGet, apiUrl } = load('api.ts', {
      '@jupyterlab/coreutils': { URLExt: { join: (...parts) => parts.join('/').replace(/\/{2,}/g, '/') } },
      '@jupyterlab/services': { ServerConnection: {
        makeSettings: () => ({ baseUrl: '/user/alice/' }),
        makeRequest: async (...args) => { calls.push(args); return response; }
      } }
    });
    const target = { namespace: 'research space', name: 'run&name' };
    assert.equal(apiUrl('status', target), '/user/alice/taugrid/api/status?namespace=research+space&name=run%26name');
    assert.deepEqual(await apiGet('status', target), run());
    assert.equal(calls[0][1].cache, 'no-store');
    response = { ok: false, status: 403, text: async () => '' };
    await assert.rejects(apiGet('status', target), /HTTP 403/);
    response = { ok: true, text: async () => '<html>sign in</html>' };
    await assert.rejects(apiGet('status', target), /did not return status JSON/);
  } finally { global.window = previousWindow; }
});

test('API forwards cancellation and bounds requests with a timeout', async () => {
  let timeoutCallback;
  let cleared = 0;
  const previousWindow = global.window;
  global.window = {
    setTimeout: (callback, delay) => { assert.equal(delay, 30000); timeoutCallback = callback; return 1; },
    clearTimeout: () => { cleared++; }
  };
  try {
    const { apiGet } = load('api.ts', {
      '@jupyterlab/coreutils': { URLExt: { join: (...parts) => parts.join('/') } },
      '@jupyterlab/services': { ServerConnection: {
        makeSettings: () => ({ baseUrl: '/' }),
        makeRequest: (url, { signal }) => new Promise((resolve, reject) => {
          const abort = () => reject(new DOMException('Aborted', 'AbortError'));
          if (signal.aborted) { abort(); }
          else { signal.addEventListener('abort', abort, { once: true }); }
        })
      } }
    });
    const request = new AbortController();
    const canceled = apiGet('status', undefined, request.signal);
    request.abort();
    await assert.rejects(canceled, { name: 'AbortError' });
    const timeout = apiGet('status');
    timeoutCallback();
    await assert.rejects(timeout, /within 30 seconds/);
    assert.equal(cleared, 2);
  } finally { global.window = previousWindow; }
});

test('run tabs restore exact identity and recreate after close', async () => {
  const commands = new Map();
  const additions = [];
  let restore;
  let tracked = 0;
  class SurfaceWidget {
    title = {}; isAttached = false; isDisposed = false;
    disposed = { connect: callback => { this.close = () => { this.isDisposed = true; callback(); }; } };
  }
  const plugin = load('index.ts', {
    '@jupyterlab/application': {}, '@jupyterlab/launcher': {}, '@jupyterlab/notebook': {},
    '@jupyterlab/apputils': { WidgetTracker: class {
      widgets = new Set();
      has(widget) { return this.widgets.has(widget); }
      async add(widget) { this.widgets.add(widget); tracked++; }
    } },
    './widget': { SurfaceWidget, RunsSidebar() {}, RunDetail() {}, RunLogs() {},
      runWidgetId: (target, surface) => surface + JSON.stringify(target) },
    './submitview': {}
  }).default;
  plugin.activate({
    commands: { addCommand: (name, options) => commands.set(name, options) },
    shell: { add: (widget, area) => { additions.push({ widget, area }); widget.isAttached = true; }, activateById() {} }
  }, { add() {}, restore: (tracker, options) => { restore = options; return Promise.resolve(); } }, null, null);
  const target = { namespace: 'research', kind: 'Job', name: 'train' };
  await commands.get('taugrid:open-run').execute(target);
  const first = additions.at(-1).widget;
  assert.equal(restore.command, 'taugrid:open-run');
  assert.equal(restore.name(first), first.id);
  assert.deepEqual(restore.args(first), { ...target, surface: 'detail' });
  await commands.get('taugrid:open-run').execute(restore.args(first));
  assert.equal(additions.length, 2);
  first.close();
  await commands.get('taugrid:open-run').execute(target);
  assert.notEqual(additions.at(-1).widget, first);
  await commands.get('taugrid:open-run').execute({ ...target, surface: 'logs' });
  assert.deepEqual(restore.args(additions.at(-1).widget), { ...target, surface: 'logs' });
  assert.deepEqual(additions.map(item => item.area), ['left', 'main', 'main', 'main']);
  assert.equal(tracked, 3);
});

test('submission starts disabled and discovery never contains a write action', () => {
  const { SubmissionReview } = load('submitview.tsx', surfaceOverrides);
  const html = renderToStaticMarkup(React.createElement(SubmissionReview, {
    notebook: { name: 'source.ipynb', toJSON: () => '{}' }, onConfirm() {}, onOpenRun() {}, onAbout() {}
  }));
  assert.match(html, /<button[^>]*disabled=""[^>]*>Submit notebook/);
  assert.ok(html.includes('Checking submission availability'));
});

test('lookup aborts and discards old replies; refresh is single-flight', async () => {
  const { monitor, pending } = harness();
  monitor.lookup({ namespace: 'research', name: 'old' });
  monitor.refresh();
  assert.equal(pending.length, 1);
  monitor.lookup({ namespace: 'research', name: 'new' });
  assert.equal(pending[0].signal.aborted, true);
  pending[1].resolve(run({ name: 'new' }));
  await settle();
  pending[0].resolve(run({ name: 'old' }));
  await settle();
  assert.equal(monitor.state.status.name, 'new');
  assert.equal(monitor.state.checkedAt, 1234);
  monitor.dispose();
});

test('watch schedules after response and stops at terminal or disposal', async () => {
  const { monitor, pending, timers } = harness();
  monitor.lookup({ namespace: 'research', name: 'train-42' });
  pending[0].resolve(run());
  await settle();
  monitor.setWatching(true);
  assert.equal(timers.size, 1);
  const callback = [...timers.values()][0];
  timers.clear();
  callback();
  assert.equal(pending.length, 2);
  assert.equal(timers.size, 0);
  pending[1].resolve(run({ state: 'complete', terminal: true }));
  await settle();
  assert.equal(monitor.state.watching, false);
  assert.equal(timers.size, 0);
  monitor.lookup({ namespace: 'research', name: 'another' });
  monitor.dispose();
  assert.equal(pending[2].signal.aborted, true);
});

test('failed refresh retains stale snapshot, stops watch, and allows retry', async () => {
  const { monitor, pending, timers } = harness();
  monitor.lookup({ namespace: 'research', name: 'train-42' });
  pending[0].resolve(run());
  await settle();
  monitor.setWatching(true);
  monitor.refresh();
  pending[1].reject(new Error('HTTP 503'));
  await settle();
  assert.equal(monitor.state.error, 'HTTP 503');
  assert.equal(monitor.state.status.name, 'train-42');
  assert.equal(monitor.state.watching, false);
  assert.equal(timers.size, 0);
  monitor.refresh();
  pending[2].resolve(run());
  await settle();
  assert.equal(monitor.state.error, null);
  monitor.dispose();
});

test('namespace discovery parses, orders and rejects malformed payloads', () => {
  const { parseNamespaces } = load('model.ts');
  const parsed = parseNamespaces({
    namespaces: [{ name: 'team-a', tauEnabled: true }, { name: 'default', tauEnabled: false }],
    warnings: ['Namespace discovery reached the limit.', 42]
  });
  assert.deepEqual(parsed.namespaces, [
    { name: 'team-a', tauEnabled: true },
    { name: 'default', tauEnabled: false }
  ]);
  // Non-string warnings are dropped rather than rendered.
  assert.deepEqual(parsed.warnings, ['Namespace discovery reached the limit.']);
  assert.throws(() => parseNamespaces({ namespaces: [{ name: 'x' }] }), /invalid namespace row/);
  assert.throws(() => parseNamespaces({ namespaces: 'nope' }), /invalid namespace list/);
  assert.throws(() => parseNamespaces(null), /invalid namespace list/);
});

test('submission rejects malformed previews and unconfirmed identities', async () => {
  const { SubmissionSession } = load('submission.ts');
  for (const result of [null, {}, { ...preview, submittable: false }, { ...preview, plan: { ...plan, planDigest: '' } }, { ...preview, submissionEnabled: 'true' }]) {
    const session = new SubmissionSession(async () => result, () => {});
    await session.review({ notebook: '{}', path: 'demo.ipynb', namespace: 'research' });
    assert.equal(session.state.preview, null);
    assert.ok(session.state.error);
  }
  for (const result of [{ submitted: true }, { submitted: true, namespace: 'other', name: plan.name, kind: 'RayJob' }]) {
    const session = new SubmissionSession(async path => path === 'preview' ? preview : result, () => {});
    await session.review({ notebook: '{}', path: 'demo.ipynb', namespace: 'research' });
    assert.equal(await session.submit(plan, true, true), false);
    assert.equal(session.state.submitted, null);
    assert.equal(session.state.uncertain, true);
  }
});

test('capabilities refuse truthy strings and unsafe portal destinations', () => {
  const { parseCapabilities } = load('model.ts');
  assert.throws(() => parseCapabilities({ submissionEnabled: 'false', submissionImplemented: true }));
  assert.throws(() => parseCapabilities({ submissionEnabled: true }));
  assert.equal(parseCapabilities({ submissionEnabled: false, submissionImplemented: true, portalUrl: 'javascript:alert(1)' }).portalUrl, null);
});
