// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const workflow = readFileSync(new URL("../workflows/request-additional-reviewer.yml", import.meta.url), "utf8");
const scriptMarker = "          script: |\n";
const script = workflow.slice(workflow.indexOf(scriptMarker) + scriptMarker.length)
  .trimEnd().split("\n").map((line) => line.slice(12)).join("\n");
const execute = new (Object.getPrototypeOf(async function () {}).constructor)("github", "context", "core", script);

async function run({ pull = {}, requested = [], repository = {}, final = {}, failure } = {}) {
  const initial = { state: "open", draft: false, user: { login: "contributor" }, ...pull };
  const calls = [];
  let reads = 0;
  const context = {
    repo: { owner: "Azure", repo: "taugrid", ...repository },
    payload: { pull_request: { number: 42, title: "$(execute-untrusted-title)", head: { ref: "untrusted/branch" } } },
  };
  const github = {
    rest: {
      pulls: {
        async get(params) {
          calls.push({ method: "get", params });
          reads += 1;
          return { data: reads === 1 ? initial : { ...initial, ...final } };
        },
        async listRequestedReviewers(params) {
          calls.push({ method: "listRequestedReviewers", params });
          return { data: { users: requested.map((login) => ({ login })), teams: [{ slug: "taugrid-maintainers" }] } };
        },
        async requestReviewers(params) {
          if (failure) {
            throw failure;
          }
          calls.push({ method: "requestReviewers", params });
          return { data: {} };
        },
      },
    },
  };
  const notices = [];
  await execute(github, context, { info: (text) => notices.push(text) });
  return { calls, notices };
}

const requests = (result) => result.calls.filter((call) => call.method === "requestReviewers");

test("Requests the individual reviewer without replacing existing reviewers or teams", async () => {
  const result = await run({ requested: ["chokevin", "feiskyer"] });
  assert.deepEqual(requests(result), [{ method: "requestReviewers", params: {
    owner: "Azure", repo: "taugrid", pull_number: 42, reviewers: ["AaronLiChen"],
  } }]);
  assert.equal(result.calls.filter((call) => call.method === "get").length, 2);
});

test("Does not duplicate an existing individual request", async () => {
  const result = await run({ requested: ["chokevin", "AaronLiChen", "feiskyer"] });
  assert.equal(requests(result).length, 0);
  assert.equal(result.calls.filter((call) => call.method === "get").length, 1);
});

test("Recognizes existing requests without case sensitivity", async () => {
  assert.equal(requests(await run({ requested: ["aaronlichen"] })).length, 0);
});

for (const [name, pull] of [
  ["draft", { draft: true }],
  ["closed", { state: "closed" }],
  ["self-authored", { user: { login: "AaronLiChen" } }],
  ["self-authored with different case", { user: { login: "aaronlichen" } }],
]) {
  test(`Does not request review for a ${name} pull request`, async () => {
    const result = await run({ pull });
    assert.equal(requests(result).length, 0);
    assert.equal(result.calls.length, 1);
  });
}

for (const [name, final] of [
  ["becomes a draft", { draft: true }],
  ["closes", { state: "closed" }],
]) {
  test(`Re-fetches eligibility when the pull request ${name}`, async () => {
    const result = await run({ final });
    assert.equal(requests(result).length, 0);
    assert.equal(result.calls.filter((call) => call.method === "get").length, 2);
  });
}

test("Runs only for the upstream repository", async () => {
  for (const repository of [{ owner: "AaronLiChen" }, { repo: "another-repository" }]) {
    assert.deepEqual((await run({ repository })).calls, []);
  }
});

test("Includes bot-authored pull requests without allowing self-review", async () => {
  assert.equal(requests(await run({ pull: { user: { login: "dependabot[bot]" } } })).length, 1);
});

test("Surfaces denied or invalid reviewer requests instead of claiming success", async () => {
  await assert.rejects(run({ failure: new Error("Synthetic reviewer request denied") }), /reviewer request denied/);
});

test("The privileged job does not execute pull request content or check out code", () => {
  const privileged = workflow.split("\n  request:\n")[1];
  assert.ok(privileged);
  assert.match(privileged, /if: github\.event_name == 'pull_request_target' && github\.repository == 'Azure\/taugrid'/);
  assert.match(privileged, /permissions:\n      pull-requests: write\n/);
  assert.doesNotMatch(privileged, /uses: actions\/checkout|\n\s+run:|contents: write|secrets\./);
  assert.doesNotMatch(script, /\$\{\{|\.head\.|\.title|\.body|eval\(|require\(|import\(/);
  assert.match(workflow, /\npermissions: \{\}\n/);
});

test("Keeps pull-request validation unprivileged and disables persisted checkout credentials", () => {
  const unprivileged = workflow.split("\n  validate:\n")[1].split("\n  request:\n")[0];
  assert.match(unprivileged, /if: github\.event_name == 'pull_request'/);
  assert.match(unprivileged, /permissions:\n      contents: read\n/);
  assert.doesNotMatch(unprivileged, /pull-requests: write/);
  assert.match(unprivileged, /persist-credentials: false/);
  assert.match(unprivileged, /run: node --test \.github\/tests\/request-additional-reviewer\.test\.mjs/);
});

test("Uses pinned public actions and serializes repeated events for one pull request", () => {
  const actions = [...workflow.matchAll(/uses: ([^\s]+)@([^\s]+)/g)];
  assert.equal(actions.length, 3);
  for (const [, , revision] of actions) {
    assert.match(revision, /^[a-f0-9]{40}$/);
  }
  assert.match(workflow, /pull_request_target:\n    types: \[opened, reopened, ready_for_review\]/);
  assert.match(workflow, /group: taugrid-review-request-\$\{\{ github\.event\.pull_request\.number \}\}/);
  assert.match(workflow, /cancel-in-progress: false/);
});
