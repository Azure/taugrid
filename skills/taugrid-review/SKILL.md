---
name: taugrid-review
description: "Review a TauGrid pull request for the defect modes a diff-reader misses: authorization whose inputs come from the object being authorized, handlers that authenticate but never authorize, resources created per request and never disposed, resolved values silently replaced by a fallback, incomplete shared metadata, and tests that pass against the wrong layer. Use when reviewing a TauGrid branch, when a change touches the Python SDK's notebook/Jupyter surface, workload rendering, or CI policy, and before claiming a green local check means the change is correct."
---

# Reviewing a TauGrid PR

This skill was adapted from the DeepSeek Harness skills `dsh-code-review`
(manual checks) and `dsh-pre-push-checks` (evidence selection). It was written
for that repository, not this one: its paths (`packages/AGENTS.md`,
`docs/defensive-patterns.md`), its commands (`pnpm run change-scope`,
`vitest related`), and its stack concepts do not exist in TauGrid and were
re-expressed against TauGrid's layout rather than copied. Where a check had no
TauGrid equivalent it was dropped and the omission is stated below. Expect the
voice to differ from the rest of TauGrid's docs; that is the adaptation, not an
oversight.

The reason this skill exists: `scripts/ci/check-review-patterns.py` was built
from one round of defects and reported ten green checks over a tree that still
contained eighteen defects a human found by reading. The checks had ratcheted
against the shapes of the previous round, not against defect modes. This skill
is the stance; the checker is the enforceable subset of it.

## Sources of truth

- [AGENTS.md](../../AGENTS.md) "Review and Validation Discipline": the five
  named defect classes and the rule that syntax checks are not validation.
- [docs/TESTING.md](../../docs/TESTING.md): the layer matrix, what each layer
  explicitly does **not** prove, and the known environment-specific failures.
- [scripts/ci/check-review-patterns.py](../../scripts/ci/check-review-patterns.py):
  the mechanically enforced subset. Its `--list` output is the current map of
  check -> defect; its `--self-test` proves each check can fail.
- [docs/design/](../../docs/design/) and [skills/taugrid/](../taugrid/SKILL.md):
  design intent and the product-facing rules the change must not contradict.

## The five moves

Read the diff with these in mind. Each one is a *stance* that produces a
question; the mechanical checks only cover part of each.

### 1. Follow the denial path to the operation that executes it

Find every place the change decides that something is *not allowed* and follow
that decision to the syscall, API call, or write that actually does the work.
The check is not "is there an authorization call"; it is "does the value that
authorization decided on come from a source the requester cannot influence".

The failure mode this catches: an input to an authorization decision is read
out of the object being authorized. A caller with no team can then list and
submit to team-scoped capacity, because the profile's own team is what the
check filters on, so the profile always authorizes itself.

Also enumerate the *alternate callers* of the operation: a handler that is not
the one under review, a widget path, a CLI path, a background job. In TauGrid
the same submission seam is reached from `tau/jupyter/server.py`, from the
legacy widgets in `tau/widgets/panel.py`, and from `tau/jupyter/submit.py`.
Authentication is not authorization: `@web.authenticated` establishes *who*;
it does not establish *whether*.

### 2. Find the owner of the final emitted or retained result

For every value that leaves the change - a rendered manifest, an HTTP response
body, a label set, a status string - name the owner of the *complete* result,
including wrappers and metadata that are added after the obvious code path.
Probe the boundaries: exact limits, one over, empty, and multibyte.

The failure mode this catches: a value is resolved, then a later expression
replaces it with a fallback, so the emitted result is not the one that was
resolved. In TauGrid this most often shows up as a queue, profile, image, or
priority that was resolved for the specific target and then rendered from the
workspace default instead.

### 3. Trace every current consumer of a shared contract

Before accepting a new or changed field, label, annotation, or status string,
find every reader. A producer that emits half of a shared contract is worse
than one that emits none, because the consumer now sees a row that looks valid
and is missing its identity.

The failure mode this catches: a rendered workload is stamped with the label
that makes a shared consumer *discover* it, but omits the labels that consumer
uses to *identify* it, so the consumer renders an incomplete record.

### 4. Exercise the real entry path

A test that calls a helper is not evidence that the shipped path works. Find
the path a user actually drives - the Jupyter handler that the browser calls,
the subprocess that the renderer describes, the loader that the plugin
mechanism invokes - and require evidence at that layer. A hand-mounted plugin
does not catch invalid plugin exports; a unit test of a string builder does not
prove the string survives the shell it is embedded in.

The failure mode this catches: an entry command is asserted as a Python string
while the shell that consumes it re-splits the value, so the assertion passes
and the workload runs somewhere other than where the test claims.

### 5. Make assertions mean something

For every changed pass condition, ask what it would take for the assertion to
be *true while the change is broken*. A pass condition satisfied by an error
message, a product name, or a value the test itself wrote is not evidence.
Negative controls matter: for each changed acceptance rule, verify that a
deliberately invalid input fails through the real runner.

## Manual checks, re-expressed for TauGrid

These are reviewer requirements. The ones marked **mechanical** are also
enforced by `scripts/ci/check-review-patterns.py`; the rest cannot honestly be
reduced to a structural check, and the reason is given.

| Check | TauGrid expression | Status |
| --- | --- | --- |
| Enforcement / denial path | Follow every refusal in the submission and status paths to the Kubernetes write it guards. Verify authorization inputs (`namespace`, `team`, `lane`) come from a trusted source - the request, the namespace label `tau.azure.com/team`, or platform config - never from the profile being authorized. | **mechanical** `authorization-input-provenance`, `handler-authorization-required` |
| Error handling and state guarantees | For every changed failure path, say what remains true afterwards. A *count* of past failures (`status.failed`) is not a terminal-state predicate; it stays positive across a retry and after that retry succeeds. Classify on the current state, not on history. | **mechanical** `terminal-state-from-current-state` |
| Lifecycle and disposal | Anything created per request or per submission - a temp dir, a thread, a watcher, a client - has one owner and is released on success, on failure, and on close. `close()` that stops a watcher but not the staging directory is not cleanup. | **mechanical** `staging-resource-disposal` |
| Bounds and the final operation | Locate the owner of the complete emitted result (see move 2). A resolved value that has a documented precedence must not be replaced by `or <fallback>`. | **mechanical** `resolved-value-not-discarded` |
| Consumer fit | Trace every current consumer of a rendered workload's labels and annotations before accepting a partial set. | **mechanical** `run-metadata-contract-complete` |
| Real entry path | Tests and harnesses must drive the shipped handler, image, or subprocess, and shell-embedded values must be framed rather than interpolated raw. | **mechanical** `entrypoint-shell-framed`; the *behaviour* still needs the live layer |
| Configuration and public choices | Ask what current-consumer evidence supports each default. A feature assumed enabled while its config defaults off is a design defect, not a documentation one. | advisory |
| Docs match the code | Config, defaults, errors, wire fields, and public behavior change in the same diff as the code. | advisory |
| Scope, ownership, necessity | Challenge unrelated work riding the same change (toolchain bumps, generated locks, repo-wide policy) and speculative generality. | advisory report: `blast-radius-report` |
| Test strength | Assertions fail on the intended regression and verify external state, logs, events, or disposal rather than restating the implementation. | advisory |
| Test reliability | Resource-owning or asynchronous tests restore global state and tear down quiescently. | advisory |

### Dropped from the DSH skills

- **Locale ownership of client UI copy.** TauGrid ships one locale; there is no
  dictionary mechanism to enforce, so the check has no equivalent and is
  dropped rather than faked.
- **`pnpm run change-scope` / `vitest related` / snapshot placement.** DSH
  tooling. TauGrid's equivalent scope inspection is `git diff --stat <merge-base>`
  and the per-layer commands in `docs/TESTING.md`; the blast-radius report is a
  judgement input, not a command contract.
- **Agent Notes and bilingual pairing.** No TauGrid counterpart.

## Mechanical subset

```bash
python scripts/ci/check-review-patterns.py            # the enforceable checks
python scripts/ci/check-review-patterns.py --self-test # every check must be able to fail
python scripts/ci/check-review-patterns.py --list      # check -> defect map
```

A check that cannot fail on its own fixture is not a check. When you add a rule
to `AGENTS.md` that is not in that list, either add the check or say in the
rule why it cannot be mechanical.

## Selecting evidence before you push

There is no universal local baseline, and a green aggregate proves less than it
appears to. Pick the narrowest command that would fail for the regression you
introduced, then add the broader layer only for surfaces the diff reaches.

| You changed | Narrowest evidence | Also consider |
| --- | --- | --- |
| A pure Python function or dataclass | `cd sdk/python/python && python -m pytest tests/<owning>_test.py` | `python -m ruff check .` |
| A Jupyter handler, status reader, or SDK HTTP path | `cd sdk/python/python && python -m pytest tests/test_jupyter_*.py tests/test_widgets_*.py` | live cluster layer |
| The notebook browser harness (`tools/*.mjs`) | `node --test tools/notebook-e2e-lifecycle.test.mjs` | `node tools/run-plugin-e2e.mjs <url> <token>` |
| `labextension/src` | `cd sdk/python/python/labextension && npm test` | `npm run build` (required after src changes) |
| A Go renderer, CLI flag, or shared contract | `cd cli && go test ./...` and `cd core && go test ./...` | `cd tests/e2e && AI_RUNTIME_E2E=0 go test -count=1 ./...` |
| A chart | `helm lint charts/<chart>` | `helm unittest charts/<chart>` |
| Docs or navigation | `make -C site check` | - |
| Anything touching a live cluster | `cd tests/e2e && AI_RUNTIME_E2E=1 go test -v -timeout 15m ./...` | browser E2E |

Before a release-bearing change, `make check` is the aggregate gate. Do not run
it reflexively for a single-module change; do run it when the diff is genuinely
cross-cutting. Read `docs/TESTING.md` "Known environment-specific failures"
before treating a local failure as a regression - on Windows, tests that exec a
`python3`/`tau` shell stub fail for platform reasons, and `gofmt -l` flags every
file in a CRLF checkout.

## Reporting findings

State the defect, the location, the impact, and the evidence. Cite file and line
against the branch under review, and verify any claim about the repository with
`git show <ref>:<path>` rather than the working tree. Separate blockers from
suggestions, and omit anything a green mechanical check already covers. When you
receive a review comment, verify it and either fix it or rebut it on technical
grounds - do not agree performatively.
