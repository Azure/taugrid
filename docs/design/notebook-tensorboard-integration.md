# TauGrid Notebook TensorBoard Integration - Design

Status: **proposal, awaiting review.** No implementation code is written yet.

Scope: give a researcher working in **Jupyter Notebook** a live TensorBoard view
of the run they submitted from that notebook, by running TensorBoard as a
**per-job sidecar container** and **proxying it to the notebook client** through
the portal.  
Client integration is **ipywidgets-based (`tau.widgets`)**; this proposal does
**not** add a JupyterLab TypeScript extension.

This document is the TensorBoard half of the notebook plugin work. The plugin
half is `notebook-plugin-slice-2.md`.

---

## 1. Problem

Ray workloads commonly produce TensorBoard-compatible outputs, but TauGrid does
not currently provide a first-class notebook path for viewing them.

- Ray Tune documents automatic callback loggers and an env-var switch
  `TUNE_DISABLE_AUTO_CALLBACK_LOGGERS=1` to disable them
  ([Environment variables used by Ray Tune](https://docs.ray.io/en/latest/tune/api/env.html),
  [Ray Tune callback code (`callback.py`)](https://github.com/ray-project/ray/blob/master/python/ray/tune/utils/callback.py)).
- Ray includes code paths that emit a `tensorboard --logdir ...` hint in console
  output
  ([Ray code search: `"tensorboard --logdir"`](https://github.com/ray-project/ray/search?q=%22tensorboard+--logdir%22&type=code)).
  **Status:** PARTIALLY VERIFIED (AIR/Tune output paths verified; Train-specific
  startup emission across all trainers is **UNVERIFIED** in this review).
- Ray dashboard has a long-standing upstream gap for training-curve-style
  visualization
  ([ray-project/ray#8554](https://github.com/ray-project/ray/issues/8554)).

Today the notebook loss pane is non-`tfevents`:

| Source | Story | Format | Evidence status in this document |
|---|---|---|---|
| Metrics file convention | (companion docs) | JSONL at `TAU_METRICS_PATH` | **Assumption A1** (not re-verified here) |
| stdout convention | (companion docs) | `loss=<float>` / `step=<int> loss=<float>` | **Assumption A1** (not re-verified here) |
| Portal series API | existing portal API | `GET /api/stellar/series` | Repo fact (§4) |

**Assumption A1:** metrics-file and stdout conventions remain as currently defined
in companion notebook-plugin docs; they were not re-audited in this proposal.

### 1.1 Evidence that this is net-new

Repository evidence indicates classification/mapping exists, but a `tfevents`
decoder is not established as a verified in-repo component for this proposal.

- `portal/internal/expstore/store_test.go:722` and
  `portal/internal/expstore/adx_export_test.go:351` use an artifact filename
  fixture (`events.out.tfevents.test`), not a decoder.
- `portal/internal/expimport/expimport_test.go:91` checks TensorBoard-card tag
  semantics, not event-file decoding.
- `portal/frontend/src/stellar/evidence-helpers.ts:85` and
  `portal/internal/expcockpit/assets/app.js:5341` classify event files as
  artifacts for display.

**Consequence:** this design does not require a new parser. It runs upstream
TensorBoard and proxies its UI.

---

## 2. Decision

**TensorBoard runs as a sidecar container in the RayJob head pod, and its UI is
proxied to the notebook client through the portal.**

| # | Decision | Consequence |
|---|---|---|
| T1 | **Per-job sidecar**, not a cluster-wide shared logdir | Isolation by construction; N boards for N jobs; one extra container per job. |
| T2 | **Proxy to the client**, not embed by default | Portal currently uses `X-Frame-Options: SAMEORIGIN`; cross-origin iframes are blocked by browser policy ([MDN: X-Frame-Options](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options)). Link first, embed only when same-origin. |
| T3 | **Proxy-side URL rewriting**, not a baked `--path_prefix` | Avoids hard-coupling rendered workloads to one portal route shape at render time; in-repo rewriting precedent exists (§5). |
| T4 | **Opt-in**, mirroring `metrics.offload` | No behavior change for existing users; no default image is set in-repo. |
| T5 | **Read-only** | The board never mutates cluster state, matching portal viewer patterns. |

---

## 3. Architecture

```
RayJob (head pod)
  |- ray-head
  |- metrics-offload        (existing, opt-in)
  |- tensorboard            (new, opt-in)  -- reads the job's tfevents logdir
        |
        v
  Service <job>-tensorboard:6006   (new, labeled tau.azure.com/tensorboard=true)
        |
        v
  Portal  /api/portal/tensorboard/proxy/{ns}/{job}/
        |
        +-- link   (default: opens in a new tab)
        +-- iframe (only when portal origin == notebook origin, or explicit port-forward)
        |
        v
  Jupyter Notebook panel (ipywidgets / tau.widgets)
```

Assumption A2: the head pod has access to the shared volume/path where event
files are written; if not, the sidecar will show an empty board.

---

## 4. Evidence and precedents (the proof)

Every mechanism this design needs already exists in the repository.

| Concern | Precedent | Verified location |
|---|---|---|
| Opt-in sidecar container injection | `BuildContainer(runtime, mounts)` | `cli/internal/metricsoffload/render.go:51` |
| Sidecar wired into the RayJob render | `DriverLogOffloadSidecar` block | `cli/internal/manifest/render.go:1527` |
| Per-job Service discovery by label | `clusterLabel`, `nodeTypeLabel` selectors | `portal/internal/portal/ray/ray.go:42-49` |
| Readiness gating so a dead board is never offered | `readyHeadClusters` | `portal/internal/portal/ray/ray.go:264` |
| Reverse proxy with SSRF guard | `validateRayTarget` | `portal/internal/portalapi/rayproxy.go:114` |
| Proxy route as single source of truth | `RayDashboardPath` | `portal/internal/portal/links/links.go:215-228` |
| Root-absolute/subpath rewrite precedent | `proxyToKueueVizRewrite`, `rewriteKueueVizHTML` | `portal/internal/portalapi/kueuevizproxy.go:95`, `:159` |
| Relaxing `X-Frame-Options` for same-origin embedding | `framedSameOrigin` | `portal/internal/portalapi/server.go:1312` |
| Existing loss-curve API the panel already reads | `/series` route, `SeriesPath` | `portal/internal/expapi/server.go:272`, `:821` |
| TensorBoard files seen as artifacts in UI paths | artifact classification helpers/tests | `portal/frontend/src/stellar/evidence-helpers.ts:85`; `portal/internal/expcockpit/assets/app.js:5341`; `portal/internal/expstore/store_test.go:722`; `portal/internal/expstore/adx_export_test.go:351`; `portal/internal/expimport/expimport_test.go:91` |

### 4.1 Internet evidence for external claims

| External claim | Evidence | Status |
|---|---|---|
| Ray Tune auto callback loggers can be disabled with `TUNE_DISABLE_AUTO_CALLBACK_LOGGERS=1` | [Environment variables used by Ray Tune](https://docs.ray.io/en/latest/tune/api/env.html), [Ray Tune callback code (`callback.py`)](https://github.com/ray-project/ray/blob/master/python/ray/tune/utils/callback.py) | VERIFIED |
| Ray emits a `tensorboard --logdir ...` hint | [Ray code search: `"tensorboard --logdir"`](https://github.com/ray-project/ray/search?q=%22tensorboard+--logdir%22&type=code) | PARTIALLY VERIFIED (AIR/Tune output verified; Train-wide startup behavior **UNVERIFIED**) |
| Ray dashboard does not provide built-in loss curves equivalent to TensorBoard | [ray-project/ray#8554](https://github.com/ray-project/ray/issues/8554) | VERIFIED as upstream gap evidence |
| `tfevents` records use TFRecord framing (length + masked CRC32C) and carry protobuf `Event` payloads | [TFRecord format (TensorFlow)](https://www.tensorflow.org/tutorials/load_data/tfrecord), [summary_iterator returns `Event` protos](https://www.tensorflow.org/api_docs/python/tf/compat/v1/train/summary_iterator), [`event.proto` in TensorBoard](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/compat/proto/event.proto) | VERIFIED |
| TensorBoard supports serving under a subpath via `--path_prefix` | [TensorBoard core plugin flags (`core_plugin.py`)](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/plugins/core/core_plugin.py) | VERIFIED |
| TensorBoard subpath deployments can fail without prefix/path handling (root-relative URL/endpoints concern) | [TensorBoard issues query for `path_prefix`](https://github.com/tensorflow/tensorboard/issues?q=path_prefix) | PARTIALLY VERIFIED; exact current root-absolute asset set is **UNVERIFIED** and must be integration-tested |

---

## 5. The path-prefix problem, and why it is already solved

TensorBoard is frequently deployed behind reverse proxies, and upstream provides
`--path_prefix` for this purpose
([TensorBoard core plugin flags (`core_plugin.py`)](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/plugins/core/core_plugin.py)).

Two implementation options:

| Option | Mechanism | Cost |
|---|---|---|
| A. Bake the prefix at startup | `tensorboard --path_prefix=/api/portal/tensorboard/proxy/{ns}/{job}/` | Couples rendered workloads to the portal route shape at render time. |
| B. Rewrite at the proxy | Rewrite root-relative URLs/endpoints in proxy responses | More proxy code/test surface, but route-decoupled and consistent with existing portal proxy behavior. |

**Recommendation: B**, reusing the KueueViz rewrite pattern
(`portal/internal/portalapi/kueuevizproxy.go:159 rewriteKueueVizHTML`).

**Assumption A3 (must be tested):** un-prefixed TensorBoard responses can include
root-relative paths that fail under nested proxy routes; add integration tests to
prove/guard this behavior for the selected TensorBoard image tag.

---

## 6. Security

- **SSRF guard.** Reuse `validateRayTarget`
  (`portal/internal/portalapi/rayproxy.go:114`): re-validate `{ns}/{job}` against
  discovered Services, do not trust request path text.
- **Header hygiene.** Strip portal cookies/identity headers before upstream dial,
  mirroring Ray proxy behavior.
- **Read-only.** TensorBoard is exposed as viewer-only through the portal proxy.
- **Per-job scoping.** One board per job avoids shared-logdir multi-tenant
  leakage.

---

## 7. Notebook client integration

The notebook panel is the client, implemented with **ipywidgets** (`tau.widgets`).

- **Default: link.** `target="_blank"` with `rel="noopener"`.
- **Embed: only when provable.** Render iframe only when portal origin equals
  notebook origin, or when user provides an explicit same-origin port-forward URL.

Rationale: with `X-Frame-Options: SAMEORIGIN`, cross-origin framing is blocked by
browser policy
([MDN: X-Frame-Options](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options)).

### 7.1 Interaction with the loss pane

TensorBoard is a **drill-down**, not the primary loss pane. The hero chart
remains the native notebook curve path(s); TensorBoard is offered as full-fidelity
inspection when enabled.

This keeps the default path cheap for users who do not opt into the sidecar.

---

## 8. Configuration

Proposed opt-in block, adjacent to existing `metrics` settings:

```yaml
metrics:
  history:
    - metrics-history-attempt-*/*.jsonl
  offload:
    enabled: true
  tensorboard:
    enabled: true
    image: mcr.microsoft.com/aks/ai-runtime/tensorboard@sha256:<digest>
    log_dir: /data/tensorboard
    # path_prefix intentionally unset by default; proxy handles rewrite (T3)
```

Image must be pinned (immutable digest or explicit non-`latest` tag), consistent
with sidecar image hygiene.

---

## 9. Risks

**Blocking (decision needed before implementation):**

1. **Runtime image supply chain.** TensorBoard sidecar image selection/pinning
   needs review.
2. **Logdir visibility.** Sidecar only sees shared/mounted paths; container-local
   trainer paths produce empty board.
3. **Service lifecycle.** Decide ownership/cleanup model for `<job>-tensorboard`
   Service through RayJob lifecycle.

**Deferrable:**

- Multi-run overlays across jobs.
- Authentication on TensorBoard itself (portal proxy remains trust boundary).
- Post-completion Service retention policy.

---

## 10. Open questions for maintainers

1. **New image, or reuse an existing one?**
2. **What logdir contract should sidecar assume?** Fixed convention vs
   user-specified path (with shared-volume validation).
3. **Proxy rewriting or `--path_prefix` default?** This document recommends
   rewriting (§5).
4. **Opt-in per job, or platform default policy?**
5. **Should TensorBoard data also feed Stellar?** Separate additive design.

---

## 11. Alternatives considered

### 11.1 `tfevents` reader feeding Stellar (no new service)

Add a `tfevents` parser to `portal/internal/expimport` that converts event files
to canonical metric rows for Stellar charts.

- **Pros:** no new runtime service, no iframe/subpath serving issues, durable and
  cross-run comparable curves.
- **Cons:** requires implementing and maintaining TensorFlow event decoding:
  TFRecord framing (length + masked CRC32C) plus protobuf `Event` parsing
  ([TFRecord format (TensorFlow)](https://www.tensorflow.org/tutorials/load_data/tfrecord),
  [summary_iterator returns `Event` protos](https://www.tensorflow.org/api_docs/python/tf/compat/v1/train/summary_iterator),
  [`event.proto` in TensorBoard](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/compat/proto/event.proto)).
- **Status:** additive, not competing.

### 11.2 Link out to a user-run TensorBoard

Zero code, but no integration/discoverability; user still manages networking and
port-forwarding.

### 11.3 Shared-logdir TensorBoard

One board for cluster-wide shared PVC. Lower resource cost, but weak isolation
without additional tenancy controls. Rejected for v1.

---

## 12. What this design needs from reviewers

1. Decision on image strategy (§10.1) and logdir contract (§10.2).
2. Confirmation that proxy-side rewriting is preferred over baked
   `--path_prefix` (§10.3).
3. Confirmation that per-job sidecar operational cost is acceptable (§10.4).
4. Direction on whether Stellar-ingestion alternative (§11.1) should run in
   parallel.