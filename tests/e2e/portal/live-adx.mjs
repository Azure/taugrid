// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { spawn, execFileSync } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const binary = process.argv[2];
const endpoint = process.env.TAU_PORTAL_ADX_ENDPOINT;
const database = process.env.TAU_PORTAL_ADX_DATABASE;
const workspace = process.env.TAU_PORTAL_WORKSPACE;
const cluster = process.env.TAU_PORTAL_CLUSTER;
const evidence = process.env.TAU_PORTAL_EVIDENCE_DIR;
const project = `pr297-e2e-${Date.now()}`;
const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'taugrid-live-'));
fs.mkdirSync(evidence, { recursive: true });
const { chromium } = await import(pathToFileURL(process.env.TAU_PORTAL_PLAYWRIGHT_MODULE).href);
const record = (name, value) => fs.writeFileSync(path.join(evidence, name), JSON.stringify(value, null, 2));
const environment = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith('TAU_') && !name.startsWith('KUBERNETES_') && name !== 'KUBECONFIG'));
const run = args => execFileSync(binary, args, { cwd: directory, env: environment, encoding: 'utf8', timeout: 60000 });
const stamp = Date.now();
record('fixture.json', { project, workspace, cluster, stamp, binarySHA256: createHash('sha256').update(fs.readFileSync(binary)).digest('hex'), expectedDay: ['fresh', 'stale', 'succeeded'], expectedWeek: ['fresh', 'stale', 'outside', 'succeeded'] });
let server;
let browser;
let page;
let base;
let serverLog = '';
const checks = [];
const passed = name => { checks.push(name); console.log(`PASS ${name}`); record('checks.json', checks); };

async function request(route, parameters = {}, status = 200) {
  const response = await fetch(`${base}${route}?${new URLSearchParams({ workspace, ...parameters })}`, { signal: AbortSignal.timeout(90000) });
  const body = await response.text();
  assert.equal(response.status, status, `${route}: ${body}`);
  return JSON.parse(body);
}
const api = (name, parameters = {}, status = 200) => request(`/api/v2/stellar/${name}`, { source: 'kusto', target: project, project, ...parameters }, status);
const ids = result => result.runs.map(run => run.run_id).sort();
const expectedIDs = names => names.map(name => `${project}-${name}`).sort();

function kusto(query, targetDatabase = database) {
  const result = JSON.parse(execFileSync('az', ['rest', '--method', 'post', '--url', `${endpoint}/v1/rest/query`, '--resource', 'https://kusto.kusto.windows.net', '--body', JSON.stringify({ db: targetDatabase, csl: query }), '--output', 'json'], { encoding: 'utf8', timeout: 90000 }));
  const table = result.Tables[0];
  return table.Rows.map(row => Object.fromEntries(table.Columns.map((column, index) => [column.ColumnName, row[index]])));
}

try {
  for (const [name, age] of [['fresh', 0], ['stale', 2 * 3600000], ['outside', 48 * 3600000], ['succeeded', 0]]) {
    const root = path.join(directory, name);
    fs.mkdirSync(root);
    const history = path.join(root, 'history.jsonl');
    const rows = [0, 1, 2].map(step => ({ _step: step, _timestamp: (stamp - age - 3000 + step * 1000) / 1000, loss: 1 / (step + 1), accuracy: step / 3 }));
    fs.writeFileSync(history, rows.map(row => JSON.stringify(row)).join('\n') + '\n');
    run(['experiment', 'init', project, '--store', path.join(root, 'store'), '--project', project]);
    const args = ['experiment', 'offload', 'metrics', '--store', path.join(root, 'store'), '--out', path.join(root, 'spool'), '--history', history,
      '--project', project, '--experiment', project, '--group', 'acceptance', '--run', `${project}-${name}`,
      '--tag', `tau_workspace=${workspace}`, '--tag', `tau_namespace=${workspace}`, '--tag', `tau_cluster=${cluster}`,
      '--tag', `acceptance_id=${project}`, '--remote-write-endpoint', process.env.TAU_PORTAL_REMOTE_WRITE_URL, '--json'];
    if (name === 'succeeded') {
      const completion = path.join(root, 'completion.json');
      fs.writeFileSync(completion, JSON.stringify({ state: 'succeeded', completed_at: new Date(stamp).toISOString() }));
      args.push('--watch', '--max-iterations', '1', '--completion-file', completion);
    }
    fs.writeFileSync(path.join(evidence, `import-${name}.json`), run(args));
  }
  passed('current binary JSONL import and remote-write accepted');
  server = spawn(binary, ['portal', 'serve', '--source', 'kusto', '--workspace', workspace,
    '--addr', '127.0.0.1:0', '--kubeconfig', path.join(directory, 'no-kubeconfig'),
    '--namespace', workspace, '--cluster', cluster, '--kusto-endpoint', endpoint,
    '--kusto-database', database, '--kusto-ingestion', 'remote-write', '--allowed-project', project,
    '--request-timeout', '90s'], { cwd: directory, env: environment, stdio: ['ignore', 'ignore', 'pipe'] });
  base = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('Portal startup timed out')), 30000);
    server.once('error', reject);
    server.once('exit', code => { clearTimeout(timeout); reject(new Error(`Portal exited ${code}`)); });
    server.stderr.on('data', data => {
      serverLog += data;
      const match = serverLog.match(/serving taugrid-portal portal at (http:\/\/[^/]+)\/portal/);
      if (match) { clearTimeout(timeout); resolve(match[1]); }
    });
  });
  browser = await chromium.launch({ executablePath: process.env.TAU_PORTAL_CHROMIUM, headless: true });
  const context = await browser.newContext();
  await context.tracing.start({ screenshots: true, snapshots: true, sources: true });
  page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  page.setDefaultTimeout(90000);
  const query = { source: 'kusto', target: project, project, workspace, window: '24h', refresh: 'off', sections: 'charts' };
  await page.goto(`${base}/portal/experiments?${new URLSearchParams(query)}`);
  await page.evaluate(async ({ project, workspace }) => {
    const deadline = Date.now() + 240000;
    while (Date.now() < deadline) {
      const response = await fetch(`/api/v2/stellar/runs?${new URLSearchParams({ workspace, source: 'kusto', target: project, project, window: '168h', limit: '100' })}`, { signal: AbortSignal.timeout(60000) });
      if (!response.ok) throw new Error(`ADX visibility query failed: ${response.status}`);
      const result = await response.json();
      if (['fresh', 'stale', 'outside', 'succeeded'].every(name => result.runs?.some(run => run.run_id === `${project}-${name}`))) return;
      await new Promise(resolve => setTimeout(resolve, 5000));
    }
    throw new Error('ADX fixture did not become visible within 240 seconds');
  }, { project, workspace });
  passed('real ADX ingestion visible for all four independent fixture runs');
  const day = await api('runs', { window: '24h', limit: '100' });
  const week = await api('runs', { window: '168h', limit: '100' });
  record('day.json', day); record('week.json', week);
  assert.deepEqual(ids(day), expectedIDs(['fresh', 'stale', 'succeeded']));
  assert.deepEqual(ids(week), expectedIDs(['fresh', 'stale', 'outside', 'succeeded']));
  record('day.json', day); record('week.json', week);
  const snapshot = await api('snapshot', { mode: 'summary' });
  record('snapshot.json', snapshot);
  const fresh = snapshot.runs.find(run => run.run_id === `${project}-fresh`);
  assert.equal(fresh.lifecycle_state, 'running');
  assert.equal(snapshot.runs.find(run => run.run_id === `${project}-stale`).liveness_state, 'not_responding');
  assert.equal(snapshot.runs.find(run => run.run_id === `${project}-succeeded`).outcome_state, 'succeeded');
  await api('runs', { window: 'invalid' }, 400);
  await api('runs', { workspace: 'not-authorized' }, 403);
  assert.deepEqual(ids(await api('runs', { window: '24h' })), ids(day));
  passed('native ADX range membership, lifecycle, workspace isolation and API recovery');

  const raw = kusto(`ExperimentMetrics | where Timestamp > ago(3d) | where tostring(Labels['project']) == '${project}' | project run_id=tostring(Labels.run_id), metric=tostring(Labels.metric_name), Timestamp, Value | order by run_id asc, Timestamp asc`);
  record('raw-adx.json', raw);
  assert.equal(raw.filter(row => row.metric === 'loss').length, 12);
  assert.equal(raw.filter(row => row.metric === 'accuracy').length, 12);
  passed('independent raw ADX query confirms 24 scalar samples');

  const freshSample = raw.find(row => row.run_id === `${project}-fresh` && row.metric === 'loss');
  assert.ok(freshSample);
  const sampleTime = Date.parse(freshSample.Timestamp);
  const subsecond = { target: `${project}-fresh`, start: new Date(sampleTime - 1).toISOString(), end: new Date(sampleTime + 1).toISOString() };
  assert.deepEqual(ids(await api('runs', subsecond)), expectedIDs(['fresh']));
  assert.deepEqual(ids(await api('runs', { ...subsecond, start: new Date(sampleTime + 10).toISOString(), end: new Date(sampleTime + 20).toISOString() })), []);
  record('subsecond.json', subsecond);
  passed('real ADX subsecond interval includes its raw sample and excludes the adjacent empty interval');

  const costRows = kusto(`database('CostTracking').GpuCostHourly | where Timestamp > ago(3d) and Cluster == '${cluster}' and namespace == '${workspace}' and schema_version == 4 | summarize gpuHours=round(sum(gpu_count),1), cost=round(sum(hourly_cost),2) by Timestamp | order by Timestamp desc | take 1`);
  assert.equal(costRows.length, 1, 'real cost data is required; do not accept an empty-table pass');
  const costStart = new Date(costRows[0].Timestamp);
  const costEnd = new Date(costStart.getTime() + 3600000);
  const cost = await request('/api/portal/cost', { start: costStart.toISOString(), end: costEnd.toISOString() });
  record('cost-expected.json', costRows); record('cost-actual.json', cost);
  assert.equal(cost.totalGPUHours, costRows[0].gpuHours);
  assert.equal(cost.totalEstimatedCostUSD, costRows[0].cost);
  const previousStart = new Date(costStart.getTime() - 3600000).toISOString();
  const previousExpected = kusto(`database('CostTracking').GpuCostHourly | where Timestamp >= datetime(${previousStart}) and Timestamp < datetime(${costStart.toISOString()}) and Cluster == '${cluster}' and namespace == '${workspace}' and schema_version == 4 | summarize gpuHours=round(sum(gpu_count),1), cost=round(sum(hourly_cost),2)`);
  const previousCost = await request('/api/portal/cost', { start: previousStart, end: costStart.toISOString() });
  assert.equal(previousCost.totalGPUHours, previousExpected[0].gpuHours);
  assert.equal(previousCost.totalEstimatedCostUSD, previousExpected[0].cost);
  record('cost-end-excluded.json', { expected: previousExpected, actual: previousCost });
  await request('/api/portal/cost', { start: new Date(costStart.getTime() + 1).toISOString(), end: costEnd.toISOString() }, 400);
  passed('real ADX cost aggregation matches independent hourly sum; aligned end bucket excluded; partial hour rejected');

  async function expectCounts(count, stale) {
    await page.getByText(`${count} loaded runs`, { exact: false }).waitFor();
    const disclosure = page.locator('details.stellar-rail-disclosure');
    if (await disclosure.getAttribute('open') === null) await disclosure.locator('summary').click();
    await page.getByText(`${count} loaded of ${count} · ${count} visible`, { exact: true }).waitFor();
    const status = page.getByRole('region', { name: 'Loaded run operational status' });
    assert.match(await status.innerText(), /1\s+active/);
    assert.match(await status.innerText(), new RegExp(`${stale}\\s+stale`));
    assert.match(await status.innerText(), /0\s+query errors/);
  }
  for (const viewport of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport);
    await page.goto(`${base}/portal/experiments?${new URLSearchParams(query)}`);
    await expectCounts(3, 1);
    assert.equal(await page.getByText(`${project}-outside`, { exact: true }).count(), 0);
    await page.getByRole('region', { name: 'Historical time range' }).getByRole('combobox').selectOption('168h');
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    await expectCounts(4, 2);
    await page.getByRole('region', { name: 'Historical time range' }).getByRole('combobox').selectOption('24h');
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    await expectCounts(3, 1);
    assert.equal(await page.getByText(`${project}-outside`, { exact: true }).count(), 0);
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.getByRole('button', { name: 'Refresh', exact: true }).waitFor();
    await expectCounts(3, 1);
    await page.getByText('9 of 9 points shown', { exact: true }).first().waitFor();
    assert.equal(await page.locator('.scalar-card').count(), 2);
    await page.screenshot({ path: path.join(evidence, `experiments-${viewport.width}.png`), fullPage: true });
    record(`browser-${viewport.width}.json`, { text: await page.locator('body').innerText(), viewport });
    passed(`browser ${viewport.width}x${viewport.height}: range switching, independent header/lifecycle totals and refresh`);
  }
  assert.deepEqual(errors, []);
  await context.tracing.stop({ path: path.join(evidence, 'trace.zip') });
  passed('no browser JavaScript exceptions');
} catch (error) {
  record('failure.json', { message: error.message, stack: error.stack });
  if (page) await page.screenshot({ path: path.join(evidence, 'failure.png'), fullPage: true }).catch(() => {});
  throw error;
} finally {
  await browser?.close();
  if (server && server.exitCode === null) {
    const stopped = once(server, 'exit');
    server.kill('SIGTERM');
    await stopped;
  }
  fs.writeFileSync(path.join(evidence, 'portal.log'), serverLog);
  fs.rmSync(directory, { recursive: true, force: true });
}