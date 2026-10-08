# TauGrid Test Handbook

One page for choosing and running the right test layer. If you are about to
change code, find the layer table first, run the layer that matches your change,
and read what that layer explicitly does **not** prove before you claim success.

This handbook is the single source of truth for test layers. It absorbs the
earlier, slice-specific `docs/design/notebook-plugin-map/e2e-test-contract.md`
(which remains as the notebook-slice scenario design) and the scattered testing
notes in `DEVELOPMENT.md` and `AGENTS.md`.

## Pick the layer

| You changed | Run at least | Also consider |
| --- | --- | --- |
| Pure Python function or dataclass | Unit (`python -m pytest`) | Mocked-API if it reads a client |
| A status/reader/HTTP handler in the SDK | Mocked-API | Unit + frontend unit |
| A Go renderer or CLI flag | Go module tests (`go test ./...`) | Offline E2E (`tests/e2e`) |
| The notebook browser harness (`tools/*.mjs`) | Tools lifecycle test (`node --test`) | Browser E2E |
| Frontend TypeScript under `labextension/src` | Frontend unit (`npm test`) | `npm run build` (required) |
| Docs content or navigation | Docs (`make -C site check`) | - |
| A contract shared by CLI and SDK | Both sides' unit tests | Render parity |
| Anything touching a live cluster | Live-cluster (explicit opt-in) | Browser E2E |

## Layers

### 1. Unit (offline, no cluster)

- **Proves:** one function's behavior against fakes; parsing and validation of
  every branch you enumerated.
- **Does not prove:** that the assembled request/response is correct, that a
  module imports, that two files agree on a value, or that a real client works.
- **Prerequisites:** Python 3.10+ with `pip install -e '.[dev,widgets]'` in
  `sdk/python/python`.
- **Command:**
  ```bash
  cd sdk/python/python && python -m pytest
  cd sdk/python/python && python -m ruff check .
  ```
- **Go equivalent:**
  ```bash
  cd cli && go test ./...
  cd core && go test ./...
  cd controllers/tau-core && make test
  ```

### 2. Mocked API (offline, executes the real code path)

- **Proves:** the code that a user actually drives is *executed* end to end with
  a fake transport: handler -> reader -> response shape. Catches initialization
  order, temporal-dead-zone errors, optional-field omissions and identity/dedup
  mistakes that unit tests skip.
- **Does not prove:** Kubernetes semantics, Jupyter session reuse on the real
  server, or browser rendering.
- **Prerequisites:** same Python dev install; `node` for the `tools/` harness
  tests.
- **Commands:**
  ```bash
  cd sdk/python/python && python -m pytest tests/test_jupyter_runs.py tests/test_widgets_load.py
  cd sdk/python/python && python -m pytest tests/test_jupyter_metrics.py tests/test_widgets_core.py
  node --test tools/notebook-e2e-lifecycle.test.mjs
  cd sdk/python/python/labextension && npm test
  ```
- **Rule:** any change to a script or harness under `tools/` must add or extend
  a mocked-API test that *executes* the lifecycle. `node --check` is a syntax
  check only; it passed a file that threw `ReferenceError` at module load.

### 3. Offline integration and render parity (offline)

- **Proves:** a whole component renders or resolves without a cluster; the Go
  and Python renderers agree on the workload contract after normalizing
  generated fields.
- **Does not prove:** server-side admission, image availability, or GPU
  placement.
- **Commands:**
  ```bash
  cd tests/e2e && AI_RUNTIME_E2E=0 go test -count=1 ./...
  cd tests/e2e && go vet ./...
  ```

### 4. Live-cluster (explicit opt-in)

- **Proves:** the change works against a real API server and a real KubeRay /
  Kueue installation, including readiness, ownership chains and status text.
- **Does not prove:** browser behavior, or that a notebook's UI wiring is right.
- **Prerequisites:** a verified kubeconfig; for the notebook plugin, a Jupyter
  server started as shown below.
- **Commands:**
  ```bash
  cd tests/e2e && AI_RUNTIME_E2E=1 go test -v -timeout 15m ./...
  # Notebook server (root dir sdk/python/python):
  cd sdk/python/python
  $env:TAUGRID_SUBMISSION_ENABLED='1'
  $env:TAUGRID_RUNTIME_IMAGE='taugrid-notebook-runtime:local'
  $env:TAUGRID_PORTAL_URL='http://127.0.0.1:8890'
  python -m jupyter lab --no-browser --port 8888 --ServerApp.token=testplugin --ServerApp.disable_check_xsrf=True --ServerApp.allow_origin='*'
  ```
- **Never** run live-cluster tests without confirming the kubeconfig context
  first. They create real workloads.

### 5. Browser E2E (live server + real browser)

- **Proves:** the rendered DOM path, the click handler, and the kernel result
  are wired together in a real browser.
- **Does not prove:** anything about a user's pre-existing sessions unless the
  scenario explicitly tests isolation.
- **Prerequisites:** `playwright` resolvable (set `PLAYWRIGHT_PACKAGE_ROOT` to a
  throwaway install) and Microsoft Edge available.
- **Commands:**
  ```bash
  node tools/run-plugin-e2e.mjs http://127.0.0.1:8888 testplugin
  node tools/run-button-e2e.mjs http://127.0.0.1:8888 testplugin
  node tools/run-embed-e2e.mjs http://127.0.0.1:8888 testplugin
  ```
- All three share `tools/notebook-e2e-lifecycle.mjs`, which creates a unique
  run identity and notebook path and tears down exactly the ids it created.

### 6. Frontend unit and build

- **Proves:** parsers reject malformed payloads, components render the expected
  labels, and the panel accepts additive server fields.
- **Does not prove:** a real kernel or a real cluster.
- **Commands:**
  ```bash
  cd sdk/python/python/labextension && npm test
  cd sdk/python/python/labextension && npm run build   # required after src changes
  ```

### 7. Docs and repository gates

- **Commands:**
  ```bash
  make -C site check                 # Hugo build, link/content/a11y checks
  python scripts/check-license-headers.py
  cd cli && gofmt -l .               # CI runs this on Linux (LF checkout)
  ```

## Known environment-specific failures

### Windows: `WinError 193` / non-executable fake CLI stubs

On Windows, running `python -m pytest` from `sdk/python/python` reports about 54
failures in `tests/test_build.py`, `tests/test_cluster_wrapper.py`,
`tests/test_eval.py`, `tests/test_stellar.py` and `tests/test_workloads.py`.
They are **not** product regressions. Those tests write a POSIX shell stub named
`tau` (or `taugrid-portal`) to `tmp_path` and exec it; Windows cannot execute a
shebang script, so the process fails with `WinError 193` (or a related
`OSError`). They pass on the Linux CI runners.

- Recognise it by: `[WinError 193] %1 is not a valid Win32 application`, or
  `OSError` raised straight out of `subprocess.run` for a temp file named `tau`.
- Confirm it is pre-existing without touching your working tree:
  ```bash
  git worktree add --detach "$TEMP/taugrid-head" HEAD
  cd "$TEMP/taugrid-head/sdk/python/python"
  $env:PYTHONPATH = (Get-Location).Path
  python -m pytest tests/test_workloads.py tests/test_stellar.py tests/test_eval.py tests/test_cluster_wrapper.py -q
  ```
- Do not re-debug it. Run the Python suites that do not exec a stub
  (`tests/test_jupyter_*.py`, `tests/test_widgets_*.py`) and treat the CI run as
  authoritative for the rest.

## Why review found these and our checks did not

Reviewer comments on PR #300 (head `587326d`). Each row names the check that
would have caught it.

| Review comment | What our checks missed | Check that catches it |
| --- | --- | --- |
| 4217330901 `gpuExecution` omitted | The server treated `gpuExecution` as optional and defaulted to `local`. Unit tests called `build_plan(gpu_execution="ray")` directly, so they never exercised the browser -> server payload that omitted the field. A schema validator accepts an absent optional field. | A mocked-API test that captures the assembled preview/submit request and asserts `gpuExecution` is present and equals the resolved mode; plus a render-parity test against the CLI. |
| 4217330912 pin/assertion mismatch | The dependency pin and the assertion that checks it lived in two places (workflow install list and the verify step). Each looked right in isolation; the floors job was the only place they met, and it does not run locally. | A single source for the floor versions, or a gate that reads the pin and the assertion and compares them. |
| 4217330922 session reuse by path | The harness checked the returned session *name*, but Jupyter reuses an existing session for the same notebook path and still answers 201. No test drove the "server returns an existing session" response. | A mocked-API test that the lifecycle refuses a session whose `path` is not this run's unique notebook path (`tools/notebook-e2e-lifecycle.test.mjs`). |
| 4218389514 TDZ at module init | Validation ran `node --check`, which is a syntax check. `RUN_NOTEBOOK` was derived from `RUN_ID` before `RUN_ID` was declared, so the module threw `ReferenceError` at load; nothing executed it. | A mocked-API test that imports/executes `createRunIdentity` and the lifecycle (`tools/notebook-e2e-lifecycle.test.mjs`). |
| 4218389523 urllib3 deadline | The floor job asserted that urllib3 1.26 has no `HTTPResponse.read1`, but the bounded-reader tests used an `io.BytesIO` fake that *does* implement `read1`. The legacy `read(size)` fallback was never exercised on the floor, and no test asserted the deadline under a slow reader. | A floor-specific test that drives the bounded reader through a urllib3-1.26-shaped response whose `_fp` drips bytes, and asserts the deadline is enforced (see `tests/test_kube_io.py`). |

The common thread: every one of these was found by a reviewer because a check
stopped one layer short of the code path that a user actually runs. Prefer the
layer that *executes* the change end to end.

## Pre-push defect-class review

Before pushing any change, read your own diff and explicitly look for these five
classes. They are the failures review has actually found in this repository.

1. **Initialization order / temporal dead zone.** Any value derived from another
   value at module load must be declared after its input. Run the module; do not
   rely on `node --check`.
2. **A constant duplicated across files that must agree.** Version pins,
   limits, defaults, magic strings shared by a producer and a consumer. Put it
   in one place, or add a test that compares both copies.
3. **A client capability assumed but absent on the supported floor.** Confirm
   the method/attribute exists on the lowest supported dependency, and exercise
   that floor.
4. **A reopened or retried resource that reports success after a failure.** If a
   retry can succeed while the first attempt failed, the wrapper must surface the
   failure, not the retry's success.
5. **Narrowing a value that participates in identity or dedup.** Truncating,
   lowercasing, normalizing or rounding something used as a key changes what
   counts as the same. Check that identity and dedup still use the full value.

## Review comments and claims

- Every review comment gets a reply that states the evidence (command, output,
  file:line). "Fixed" is not an answer.
- Before making a claim about the repository ("X does not exist", "Y is not
  used anywhere"), verify it against the target branch, not the current
  checkout:
  ```bash
  git show origin/main:<path>
  git ls-tree -r origin/main | grep <name>
  ```
  A stale-checkout search is not evidence about the branch under review.

## Browser end-to-end (Playwright + Edge)

The browser layer is the only one that proves the actual JupyterLab UI renders and
responds. It runs against the system Edge through Playwright's `msedge` channel, so
no browser download is required - only the Playwright package.

### One-time setup

```powershell
# Edge must be installed. The channel uses the system binary, not a bundled one.
$pw = "$env:TEMP\tau-playwright"
New-Item -ItemType Directory -Force -Path $pw | Out-Null
Set-Location $pw; npm init -y; npm install playwright
```

The harnesses resolve Playwright from `PLAYWRIGHT_PACKAGE_ROOT`, so the package can
live outside the repository and never lands in a commit.

### Run

```powershell
cd <repo root>
$env:PLAYWRIGHT_PACKAGE_ROOT = "$env:TEMP\tau-playwright"
$env:HEADLESS = "1"                      # omit for a visible browser
$env:TAUGRID_E2E_NAMESPACE = "taugrid-default"   # a stock install has no tau-notebook-e2e
node tools/run-plugin-e2e.mjs  http://127.0.0.1:8888 testplugin tau-plugin-e2e
node tools/run-button-e2e.mjs  http://127.0.0.1:8888 testplugin tau-button-e2e
node tools/run-embed-e2e.mjs   http://127.0.0.1:8888 testplugin tau-embed-e2e
```

Prerequisites: a running cluster with ready workload profiles, and JupyterLab with
the extension loaded and submission enabled:

```powershell
cd sdk/python/python
$env:TAUGRID_SUBMISSION_ENABLED = "1"
$env:TAUGRID_RUNTIME_IMAGE = "taugrid-notebook-runtime:local"
python -m jupyter lab --no-browser --port 8888 --ServerApp.token=testplugin --ServerApp.disable_check_xsrf=True
```

### What this layer proves, and what it does not

| Harness | Proves | Does **not** prove |
|---|---|---|
| `run-plugin-e2e` | The panel renders in a real notebook: cells execute through the UI and the loss chart draws (`svg[role=img]` present). | That the numbers are correct; the chart is checked for existence, not value. |
| `run-button-e2e` | The toolbar button is present and clicking it repaints the run view. | That a run was actually created - it asserts the status surface, not cluster state. |
| `run-embed-e2e` | The iframe is created, points at the portal run URL, and the framed document renders the run board. | Anything about the portal's data. The harness now requires the run name to appear **and** rejects transport-failure text, so an unreachable portal fails the run instead of passing it. |

Two known false-positive shapes are recorded here deliberately, because both have
already produced a misleading `PASS`:

1. `run-embed-e2e` used to accept any frame text containing "TauGrid", and the
   proxy's failure banner contains that word, so it passed on an error page. Fixed:
   it now requires the run name and rejects transport-failure text. Running it
   without a reachable portal reports the embed as untested and exits non-zero, by
   design - a missing portal is not a passing embed. Note the proxy target is not
   `TAUGRID_PORTAL_URL`; it defaults to port 8090, so pointing the capability URL
   elsewhere will not redirect the iframe.
2. `run-button-e2e` used to pass on a repaint alone, so it accepted the run state
   `unknown` - which the panel renders when it has no status at all. The notebook's
   recording fake now seeds a queued status the way the real submit path does, and
   the harness asserts `queued` and rejects `unknown`. A repaint by itself does not
   prove the submit path produced a run state.

### Session and notebook ownership

Jupyter's contents API **rejects hidden names with HTTP 400**, so a per-run notebook
copy must not start with a dot. The shared lifecycle in
`tools/notebook-e2e-lifecycle.mjs` copies the source to
`examples/tau-<label>-<runId>.ipynb`, refuses any session whose `path` is not that
copy, and deletes only exact ids at teardown. Jupyter's sessions POST also reuses an
existing session for the same notebook path and still answers 201, which is why the
per-run path - not just a per-run name - is required.

### Environment-specific notes

- `workspace <name>: remove skipped 404` on teardown is expected: JupyterLab creates
  the workspace lazily, so a headless run may never create one.
- The harnesses pass `--no-proxy-server`; without it a system proxy can intercept
  `127.0.0.1` and the page never loads.
