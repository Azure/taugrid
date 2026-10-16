# TauGrid Notebook Plugin - Design

Status: **proposal, awaiting review.** No implementation code is written yet.

Scope: a notebook surface for TauGrid that lets a researcher (a) submit the notebook they are working in as a Tau job and (b) watch one integrated dashboard for that run: Ray dashboard, GPU usage, and the training loss curve.

Primary target editor: **Jupyter Notebook**.  
Secondary compatibility targets (best-effort, not promised supported surfaces): JupyterLab, VS Code notebooks, Google Colab.

This is a **notebook plugin**: an **ipywidgets component** rendered inside notebook outputs. It is not a Codex/DeepSeek/Claude plugin, and it does not add a second web UI.

## Decisions already made (do not re-litigate)

| # | Decision | Consequence |
|---|---|---|
| D1 | **No CLI dependency.** No `tau` binary, no `kubectl` at runtime. | Submit is pure Python against Kubernetes APIs. |
| D2 | **The end user imports nothing.** The deliverable is a button/component. | The one `import` lives in a platform-authored template cell, not in the user's code. |
| D3 | **The job is the current notebook.** | Submit packages the `.ipynb` and runs it on the cluster. No `@tau.train` decorator in the user's notebook. |
| D4 | **ipywidgets, not a JupyterLab prebuilt extension.** | Portable across notebook surfaces with caveats ([ipywidgets Installation](https://ipywidgets.readthedocs.io/en/stable/user_install.html), [VS Code Jupyter notebooks](https://code.visualstudio.com/docs/datascience/jupyter-notebooks), [Colab widgets notebook](https://colab.research.google.com/notebooks/widgets.ipynb)). |
| D5 | **Kubernetes Python SDK** is the cluster client. | Submit/read flows use `CoreV1Api` and `CustomObjectsApi` ([Kubernetes Python Client](https://github.com/kubernetes-client/python), [CustomObjectsApi docs](https://github.com/kubernetes-client/python/blob/master/kubernetes/docs/CustomObjectsApi.md)). |

---

## 1. Context loop trace

What was mined before writing this, and what it settled.

> Reviewer note: this section was audited to remove chat/session metadata.  
> Any repository claim without grep-backed `file:line` confirmation is explicitly marked **ASSUMPTION (UNVERIFIED)**.

| # | Source | Signal | Verification status |
|---|---|---|---|
| 1 | `AGENTS.md`, repo tree | Repo appears multi-module (`cli/`, `core/`, `controllers/`, `portal/`, `sdk/python/`). | **ASSUMPTION (UNVERIFIED)** - add exact `file:line` citations in follow-up pass. |
| 2 | `cli/go.mod` | Prior note claims Kubernetes Go clients are direct deps; Azure SDK usage is scoped to infra/telemetry helpers. | **ASSUMPTION (UNVERIFIED)**. |
| 3 | `core/kube/kubectl.go` | Prior note claims normal manifest/query path shells out to `kubectl`, with narrow direct REST usage. | **ASSUMPTION (UNVERIFIED)**. |
| 4 | `cli/internal/runhistory/kubernetes.go`, `cli/internal/cli/run_submit.go` | Prior note claims typed `kubernetes.Interface` + `dynamic.Interface` are used in submit/history paths. | **ASSUMPTION (UNVERIFIED)**. |
| 5 | `controllers/tau-core/api/v1alpha1/types.go` | Prior note claims Tau CRDs include `TauCluster`, `TauWorkspace`, `TauQuotaRequest`; profile/queue defaults come from `TauCluster.spec`. | **ASSUMPTION (UNVERIFIED)**. |
| 6 | `cli/internal/rayjobrender/render.go` | Prior note claims renderer handles Kueue labels/suspend, topology, GPU claims, TTL, entrypoint staging. | **ASSUMPTION (UNVERIFIED)**. |
| 7 | `core/status/gpu.go` | Prior note claims GPU metric fields include explicit observed booleans to distinguish zero vs missing. | **ASSUMPTION (UNVERIFIED)**. |
| 8 | `portal/internal/expapi/server.go` | Prior note claims `/api/stellar/series` shape used for loss charting. | **ASSUMPTION (UNVERIFIED)**. |
| 9 | `portal/internal/portalapi/rayproxy.go`, `server.go` | Prior note claims Ray dashboard proxy route and `X-Frame-Options: SAMEORIGIN` behavior. | **ASSUMPTION (UNVERIFIED)**. |
| 10 | `sdk/python/python/tau/workloads.py`, `sdk/python/python/tau/_cluster.py` | Prior note claims current Python submit handle shells through CLI and ships cluster-side wrapper code. | **ASSUMPTION (UNVERIFIED)**. |
| 11 | KubeRay upstream docs | KubeRay includes an optional APIServer; V1/V2 lifecycle and compatibility are documented upstream. | **Externally cited** ([KubeRay APIServer README](https://github.com/ray-project/kuberay/blob/master/apiserver/README.md), [KubeRay repo `apiserver/`](https://github.com/ray-project/kuberay/tree/master/apiserver)). |

### Corrections applied in this revision

- Removed non-repository/session-specific claims (for example, “which skills are installed in this chat session”).
- Removed author-internal draft-history language.
- Marked all unverified repo-path assertions as **ASSUMPTION (UNVERIFIED)** pending grep-backed `file:line` citations.
- Added external citations for Kubernetes SDK, KubeRay APIServer docs, ipywidgets portability, notebook execution tooling, and browser framing behavior.

---

## 2. The question this work answers

> Can a researcher who is already working in a notebook get that notebook onto a GPU cluster, and then answer “is my loss going down, are my GPUs working, what is Ray doing” by clicking, without writing YAML, without opening a terminal, and without importing anything?

Today that answer typically spans multiple contexts (terminal, portal, notebook output). The plugin collapses that into one panel.

---

## 3. Personas

| Persona | Goal in this surface | What they will not do |
|---|---|---|
| **Researcher (primary)** - owns a training notebook | Click Submit, then watch loss + GPU until done or broken | Read YAML, open a terminal, learn Kueue, write imports |
| **ML platform engineer (secondary)** - supports researchers | Provide notebook template + RBAC; reproduce a run from a shared notebook | Rebuild a dashboard per incident |
| **Reviewer / collaborator (tertiary)** - reads a shared notebook | See evidence that the run trained and converged | Run anything |

**Who authors the import?** The platform engineer, once, in the notebook template. The researcher never writes it. This is how D2 and D4 coexist: ipywidgets needs code somewhere, but not user-authored code.

---

## 4. Backbone (user activities)

Frame: activity flow. Perspective: primary user. Horizon: single run session.  
Granularity: 5 activities. Scope: happy path + failure recovery. Aggregation: single role.

1. **Open a ready notebook** - start from the platform template, which already includes the TauGrid panel cell.
2. **Submit the notebook** - click Submit; plugin packages notebook and applies workload.
3. **Watch it converge** - loss curve, GPU utilization, step progress.
4. **Inspect cluster state** - Ray dashboard link, pods, queue/admission context.
5. **Recover or hand off** - read failure signal, capture next command, export/share evidence.

Cross-cutting backlog (not backbone): renderer conformance suite, packaging robustness, offline test strategy, docs.

---

## 5. Architecture

### 5.1 Cluster integration target and client choice

D1 and D5 drive one runtime shape: direct Kubernetes API access from Python.

- Use `kubernetes` Python client for cluster I/O ([Kubernetes Python Client](https://github.com/kubernetes-client/python)).
- Use `CoreV1Api` for pods/logs/nodes.
- Use `CustomObjectsApi` for Tau/Kueue/Ray CRDs ([CustomObjectsApi docs](https://github.com/kubernetes-client/python/blob/master/kubernetes/docs/CustomObjectsApi.md)).

Repository-specific details of current CLI internals are still **ASSUMPTION (UNVERIFIED)** in this draft (see §1). The design remains valid because the widget path is explicitly independent of CLI runtime calls.

### 5.1.1 KubeRay APIServer note (correction)

KubeRay does ship an optional APIServer component ([KubeRay APIServer README](https://github.com/ray-project/kuberay/blob/master/apiserver/README.md)).

| Topic | External evidence | Design implication |
|---|---|---|
| APIServer exists | Upstream `apiserver/` docs are present. | Plugin must not assume APIServer absence. |
| V1/V2 lifecycle | Upstream docs discuss V1/V2; exact deprecation wording should be rechecked before implementation freeze. | Do not build a V1-specific client path in Slice 1. |
| Client shape | Upstream docs describe Kubernetes-oriented API compatibility/proxy behavior for V2 (exact wording to confirm in implementation PR). | Keep API base URL configurable; default direct Kubernetes API server path. |

TauGrid-repo-specific deployment claims (“TauGrid does/doesn’t deploy KubeRay APIServer”) remain **ASSUMPTION (UNVERIFIED)** until repo grep provides exact `file:line` evidence.

### 5.2 Submit without CLI, without decorator

D1 + D3 means the plugin must transform a **notebook** into a runnable workload.

```
analysis.ipynb
   │ 1) resolve notebook path (Jupyter Sessions API where available)
   │ 2) fetch notebook bytes (Jupyter Contents API where available)
   ▼
package  ──► staged payload (notebook + thin runner)
   │
   │ 3) resolve profile + queue defaults
   │ 4) render ray.io/v1 RayJob (Python renderer port)
   ▼
apply via CustomObjectsApi  ──► admission  ──► notebook executes on cluster
```

Notebook/session path resolution references:
- [Jupyter Server REST API: Sessions](https://jupyter-server.readthedocs.io/en/latest/developers/rest-api.html#api-sessions)
- [Jupyter Server REST API: Contents](https://jupyter-server.readthedocs.io/en/latest/developers/rest-api.html#api-contents)

Runner choices:
- `nbconvert --execute` path ([nbconvert Execute API](https://nbconvert.readthedocs.io/en/latest/execute_api.html))
- optional `papermill` path if image has it ([Papermill Documentation](https://papermill.readthedocs.io/en/latest/))

This introduces a runtime-image requirement (see §9).

**Renderer parity requirements (ASSUMPTION from prior repo notes; verify against `cli/internal/rayjobrender/render.go` with file:line):**

| Renderer concern | Why it cannot be skipped |
|---|---|
| `spec.suspend: true` and Kueue-compatible ownership | Prevents bypassing fair-share admission. |
| Queue labeling | Required for admission/routing. |
| Profile-derived sizing (workers, GPUs, selectors, priority class) | Keeps submitted shape consistent with platform contract. |
| Runtime environment propagation | Prevents missing dependencies on workers. |
| Staged entrypoint + payload wiring | Needed to execute notebook artifact in-cluster. |
| Head resource zeroing where applicable | Avoids wasting quota on control-only head. |
| GPU claim wiring (including modern claim styles) | Determines whether GPUs attach. |
| TTL/shutdown behavior | Prevents completed jobs from lingering. |

**De-risking plan**

1. **Backend seam** (`CliBackend` existing, `KubernetesBackend` new): runtime calls avoid CLI.
2. **Conformance suite**: render same logical workload in Go and Python, compare normalized RayJob objects in CI.
3. **Scope guardrails**: Slice 1 supports one notebook -> one run shape; unsupported shapes fail fast with explicit messages.

### 5.3 Component architecture

```
┌────────────────── notebook frontend (Notebook primary; others best-effort) ──────────────────┐
│                                                                                               │
│  ┌────────────────────────────── TauGrid panel (ipywidgets) ───────────────────────────────┐  │
│  │ [Submit notebook] notebook: analysis.ipynb profile: ▾ queue: ▾                           │  │
│  │ ----------------------------------------------------------------------------------------- │  │
│  │ loss summary + chart | GPU summary + bars | Ray dashboard link                            │  │
│  └────────────────────────────────────────────────────────────────────────────────────────────┘  │
│                                      │ widget state (comm)                                     │
└──────────────────────────────────────┼──────────────────────────────────────────────────────────┘
                                       ▼
┌──────────────────────────────────── kernel (Python) ───────────────────────────────────────────┐
│ tau.widgets  ── resolve notebook path / manual override                                         │
│              ── package .ipynb + runner                                                         │
│              ── KubernetesBackend: resolve profile, render, apply                               │
│              ── poll status + metrics; push updates to widget                                   │
└──────────────────────────────────────┬──────────────────────────────────────────────────────────┘
                                       │ kubernetes-python SDK
                                       ▼
                         ┌──────────────────────────────────────────┐
                         │ Kubernetes API server + CRDs            │
                         │ tau.azure.com, kueue.x-k8s.io, ray.io/v1│
                         └──────────────────┬───────────────────────┘
                                            │
                                            ▼
                        ┌────────────────────────┐    ┌─────────────────────────┐
                        │ taugrid-portal (HTTP)  │    │ run metrics conventions │
                        │ /api/stellar/series    │    │ stdout / metrics file   │
                        │ /api/portal/ray/proxy  │    │                         │
                        └────────────────────────┘    └─────────────────────────┘
```

Runtime dependencies: `kubernetes`, `ipywidgets`, and SDK dependencies already in wheel (for example `PyYAML` if retained).

### 5.4 Module layout

```
sdk/python/python/tau/
├── _backend.py             # CliBackend (existing) + KubernetesBackend (new)
├── _render.py              # manifest -> ray.io/v1 RayJob / batch/v1 Job
├── _profile.py             # profile + queue resolution from cluster state
├── _notebook_pkg.py        # .ipynb -> staged payload (+ runner)
└── widgets/
    ├── __init__.py         # public: panel(), TauGridPanel
    ├── panel.py            # ipywidgets layout, buttons, event wiring
    ├── kube.py             # kubeconfig/in-cluster config -> API clients
    ├── status.py           # RunStatus/GPUDevice dataclasses; read_run_status()
    ├── metrics.py          # MetricSeries, read_metrics(), loss selection
    ├── render.py           # HTML/SVG fragments for panes
    └── session.py          # notebook path resolution (+ manual override)
```

`_render.py`, `_profile.py`, `_notebook_pkg.py` stay outside `widgets/` because they are SDK capability, not only UI code.

### 5.5 How “no import for the end user” works

ipywidgets still needs executable Python, so platform ships a template notebook with a pre-authored cell:

```python
# Cell 0 - platform-authored template cell
import tau.widgets as tg
tg.panel()
```

Delivery paths (in priority order):

1. **Template notebook** (recommended).
2. **Copy/paste bootstrap cell** for existing notebooks.
3. **Optional toolbar injection later** (out-of-scope for Slice 1; reintroduces Node/frontend extension complexity).

Portability references:
- [ipywidgets Installation](https://ipywidgets.readthedocs.io/en/stable/user_install.html)
- [VS Code Jupyter notebooks](https://code.visualstudio.com/docs/datascience/jupyter-notebooks)
- [Colab widgets notebook](https://colab.research.google.com/notebooks/widgets.ipynb)

### 5.6 Data contracts

> Reviewer note: these contracts are design targets; endpoint/field exactness must be verified against repo handlers before implementation.

**a. Run status** (proposed `RunStatus` shape; consumed by widget)

```
metadata.{name,namespace}
status.{found,workloadKind,state,displayState,startupComplete,startupFailed}
rayJob.{rayClusterName,jobId,jobDeploymentStatus,jobStatus,reason,message}
workloads[].{name,queue,admitted,phase,reason,message}
pods[].{name,phase,node,ready,restarts}
metrics.gpuRuntime.{state,reason,nodesExpected,nodesScraped,devices[].{pod,gpu,
                       utilizationPercent,utilizationObserved,
                       framebufferUsedMiB,framebufferUsedObserved}}
diagnostics[].{code,severity,message,suggestion}
actions[].{description,command.shell}
```

**b. Loss curve** (proposed portal fetch)

`GET {portal}/api/stellar/series?target=<run|experiment>&metric=train/loss&max_points=N`

```
chart.{has_data,metric_name,series[].{run_id,values[].{step,value}}}
```

**c. Live loss without user import**

Two channels, in precedence order:

- **stdout convention**: parse `loss=<float>` or `step=<int> loss=<float>`.
- **metrics file convention**: JSONL at `TAU_METRICS_PATH` (default `/data/metrics.jsonl`), e.g. `{"step":3,"loss":1.25}`.

UI must label the active source to avoid ambiguity.

**d. Ray dashboard URL**

`{portal}/api/portal/ray/proxy/<namespace>/<cluster>/` (exact route to verify in repo).

### 5.7 Ray dashboard framing constraints

If Ray proxy responses include `X-Frame-Options: SAMEORIGIN`, browsers block cross-origin iframe embedding ([MDN: X-Frame-Options](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options)).

Design policy:

- **Default:** open Ray dashboard in a new tab.
- **Embed only when proven embeddable** (same-origin/local URL explicitly provided).
- **Never render a “known-broken” iframe**.

---

## 6. UI experience

### 6.1 The panel

One widget, vertical stack. Loss is the hero; GPU is directly below; lifecycle/Ray are drill-downs.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ TauGrid                                                                      │
│ [Submit notebook]  notebook analysis.ipynb  profile ▾  queue ▾              │
├──────────────────────────────────────────────────────────────────────────────┤
│ analysis-001 / ray [Running] RayJob                                          │
│ Running - 8/8 pods ready - queue research-gpu - updated 14:22:31 UTC        │
├──────────────────────────────────────────────────────────────────────────────┤
│ ▾ Loss (train/loss)                                                          │
│   loss down 88.4%   0.9230 -> 0.1040   latest 0.1040   best 0.0982          │
│   [line chart]                                                               │
│   source: metrics file - 1240 points                                         │
├──────────────────────────────────────────────────────────────────────────────┤
│ ▾ GPU usage                                                                  │
│   avg util 87.3%   gpus observed 8/8   nodes scraped 2/2                     │
│   [per-device bars with numeric percentages]                                 │
├──────────────────────────────────────────────────────────────────────────────┤
│ ▾ Ray dashboard                                                              │
│   Open dashboard ↗                                                           │
│   (optional local port-forward command shown as convenience text)            │
├──────────────────────────────────────────────────────────────────────────────┤
│ ▸ Queue and pods                                                             │
│ ▸ Diagnostics                                                                │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 6.2 The “loss driven down” signature

Primary summary line (words + numbers):

`loss down 88.4%   0.9230 -> 0.1040`

Rules:

- `down` / `up` from first vs latest point.
- Warn color when loss increases.
- Fewer than two points: show “waiting for first steps”.

### 6.3 States

| State | Header | Loss pane | GPU pane | Default-open panes |
|---|---|---|---|---|
| No notebook resolved | muted badge, `notebook path not resolved` | hidden | hidden | submit controls + manual path override |
| Not submitted | muted badge, `not submitted` | hidden | hidden | submit controls |
| Queued | amber `Pending (not yet admitted)` | “no steps logged yet” | “no pods scheduled” | queue |
| Running | blue `Running` | live curve | live bars | loss, gpu, ray |
| Failed | red `Failed` | frozen last curve | last-known or unavailable | diagnostics |
| Complete | green `Complete` | final curve + delta | completed/no-live-samples message | loss, gpu, ray |

### 6.4 Interaction model

- **Submit** (primary): package notebook, resolve profile, render/apply workload.
- **Refresh** (secondary + auto-poll while active): visible/adjustable interval.
- **Stop watching**: stop polling without touching run.
- **Drill-downs**: collapsible panes, state remembered in widget state.
- **Export**: “Save HTML” for reports/PRs.

### 6.5 Visual system

- System UI font stack; monospace for ids/commands.
- Functional state colors only (running/ok/warn/error/idle).
- Dense operational layout (small but readable type).
- Accessibility: textual state labels, keyboard navigable panes, numeric labels on bars, SVG `aria-label`.

---

## 7. Concrete API surface

Template cell (platform-authored, not user-authored):

```python
import tau.widgets as tg
tg.panel()
```

Programmatic usage for platform engineers/CI:

```python
from tau.widgets import TauGridPanel

panel = TauGridPanel(
    notebook="analysis.ipynb",   # optional; resolve from session if omitted
    namespace="ray",
    profile="training-8gpu",     # optional; resolve from cluster defaults if omitted
    queue="research-gpu",        # optional; fallback to workspace/profile default
    portal_url="https://portal.contoso.com",
)
```

`TauGridPanel.submit()` uses `KubernetesBackend` (no CLI dependency), renders workload object(s), applies via Kubernetes APIs, and watches status/metrics.

---

## 8. Slicing

**Slice 1 - walking skeleton (end-to-end demoable)**

- Panel renders in notebook output.
- Submit packages current notebook and applies workload via Kubernetes client.
- Header + loss (metrics-file convention) + GPU + Ray link render.
- Correct states: not-submitted / queued / running / failed.
- Renderer conformance harness exists (Go vs Python normalized comparison).
- Offline tests run with injected transports.
- **Required gating task:** replace §1 assumptions with grep-backed `file:line` citations.

**Slice 2 - live and durable**

- Auto-poll repaint and interval controls.
- Stdout loss parser.
- Portal-series fallback for loss.
- Pod logs integration.
- Save HTML export.
- Pod list/framebuffer summaries.

**Slice 3 - richer surfaces**

- Optional toolbar injection path.
- Deeper portal deep-links.
- Broader submit shapes beyond Slice-1 constraints.

---

## 9. Risks and open questions

**Blocking (decide before Slice 1):**

1. **Runtime image capability:** does the runtime image include `nbconvert` (or `papermill`)?  
   References: [nbconvert Execute API](https://nbconvert.readthedocs.io/en/latest/execute_api.html), [Papermill Documentation](https://papermill.readthedocs.io/en/latest/).
2. **Loss intent:** “loss driven down” means a per-step curve (single-run history), not a 2D loss landscape. Confirm expectation.
3. **Packaging:** ship inside `tau` wheel vs separate `tau-notebook` package.
4. **Repository verification debt:** assumptions in §1/§5 must be converted to exact `file:line` evidence before implementation merge.

**Top risks introduced by the chosen decisions:**

- **Renderer drift:** Python renderer diverges from Go behavior.  
  Mitigation: conformance tests in Slice 1.
- **Loss capture convention fragility:** unsupported print format yields empty curve.  
  Mitigation: explicit docs + visible source label + metrics-file first-class path.
- **Notebook path resolution edge cases:** missing session, renamed file, Colab differences.  
  Mitigation: manual path override + explicit unresolved state.
- **Cross-surface widget variance:** Notebook/JupyterLab/VS Code/Colab differ in widget/runtime behavior.  
  Mitigation: primary-support stance (Notebook), compatibility matrix in docs, smoke tests per surface.

**Deferrable (defaults chosen):**

- Dark mode (default light).
- Total GPU capacity display (used memory first).
- Multi-run overlay (later slice).

---

## 10. What I need from you

1. Confirm runtime-image execution path (`nbconvert` vs `papermill`) and base image update plan.
2. Confirm loss interpretation (curve vs landscape).
3. Confirm packaging (`tau` wheel vs separate package).
4. Confirm Ray behavior default (link-first; embed only when embeddable).
5. Assign owner to complete grep-backed `file:line` verification pass for §1 and §5 assumptions.

Decisions already fixed and not for re-litigation: **no CLI dependency**, **no user-authored import**, **job is current notebook**, **ipywidgets (not native JupyterLab extension)**.

---

## Companion artifacts

- `notebook-plugin-storymap.md` - backbone, tasks, sliced stories
- `notebook-plugin-backlog.md` / `.csv` - WSJF-ranked backlog
- `notebook-plugin-slice-1-acceptance-criteria.md` - Given/When/Then for Slice 1