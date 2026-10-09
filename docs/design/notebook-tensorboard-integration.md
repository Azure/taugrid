# TauGrid Notebook Metrics Integration - Design

Status: **revised proposal, awaiting review.** No implementation code is written yet.

The file name is retained from the original revision for PR continuity (#285). The
proposal itself has been **reframed** in response to maintainer review: the
recommendation is no longer to add a TensorBoard service. It is to converge on
the metrics path TauGrid already has and treat TensorBoard's `tfevents` as an
input adapter.

Scope: give a researcher working in **Jupyter Notebook** a live training-metric
view of the run they submitted from that notebook. Client integration is
**ipywidgets-based (`tau.widgets`)**; this proposal does **not** add a JupyterLab
TypeScript extension.

Companion: the notebook plugin itself, `notebook-plugin.md`.

---

## 0. Revision summary

| # | Review comment (paraphrased) | What changed in this revision |
|---|---|---|
| R1 | TensorBoard looks orthogonal to how per-job metrics should be displayed, and is not the offline W&B-style format the industry uses | §2 provides the requested format analysis. §3 makes the **house W&B-style JSONL** the target and demotes TensorBoard to an optional input adapter. |
| R2 | Configure the existing metrics offload container to handle different metrics logging, and make it work as broadly as possible | §3 becomes the primary recommendation: broaden the **existing** metrics path. §4.1 explains why this beats a new container. |
| R3 | "I would need some analysis between the tensorboard format and the existing format we are exporting" | **§2 is that analysis.** Verdict in §2.5. |
| R4 | Consider another alternative, or a local TauGrid without a backing ADX cluster | §3 D4 records the local-mode question; §10.5 scopes it as a separate design rather than silently absorbing it. |
| R5 | (self-correction, found while re-verifying against `main`) | §7.2 claimed `portal/README.md` names a `taugrid-metrics-collector` that does not exist in-tree. That was wrong — the component is real at `metrics/experiment-metrics-collector` — so §7.2 now records the correction and §9 Q1 is closed instead of deferred. |
| R7 | Preserve fractional epoch seconds; narrowing `wall_time` to integer seconds collides event IDs for same-tag, same-step observations inside one second | §2.4 keeps the fractional part and documents why: value and history-line tags are excluded from the projection's identity, so a floored timestamp makes the ADX dedup view hide one observation. A regression case for that pair is now required. |
| R6 | The collector already owns source checkpoints, typed projection, durable spooling and queued ADX delivery; assess protobuf in the collector module, not `portal/go.mod` | §5 no longer draws the collector as a remote-write forwarder: the adapter is an additional source at the typed-projection boundary, and the existing checkpoints, spool and queued delivery are reused rather than bypassed. §6.1 now records that protobuf is **not** in the collector module, so placement changes the dependency cost. |

The original proposal (per-job TensorBoard sidecar + portal proxy) is preserved in
§10.1 as a documented alternative with its costs. It is **deferred, not
rejected**: §4.2 lists the conditions that would justify revisiting it.

---

## 1. Why this reframe

Ray workloads commonly produce TensorBoard-compatible output, and TauGrid has no
first-class notebook path for viewing it:

- Ray Tune documents automatic callback loggers and the env-var switch
  `TUNE_DISABLE_AUTO_CALLBACK_LOGGERS=1` to disable them
  ([Ray Tune environment variables](https://docs.ray.io/en/latest/tune/api/env.html),
  [Ray Tune `callback.py`](https://github.com/ray-project/ray/blob/master/python/ray/tune/utils/callback.py)).
- Ray includes code paths that emit a `tensorboard --logdir ...` hint.
  **Status:** PARTIALLY VERIFIED (AIR/Tune output paths verified; Train-specific
  startup emission is **UNVERIFIED** in this review).
- The Ray dashboard has a long-standing gap for training-curve visualization
  ([ray-project/ray#8554](https://github.com/ray-project/ray/issues/8554)).

The question the review raised is not *whether* to surface those metrics, but
*which format TauGrid should standardize on*, and whether a tracker UI belongs in
the platform at all. §2 answers it with evidence.

---

## 2. The format analysis

This is the analysis requested in R3. Conclusion first: the two formats are
**not similar in encoding, but isomorphic in scalar semantics**. That combination
means the right move is a **converter into the house format**, not a parallel UI.

### 2.1 The format TauGrid exports today

TauGrid's training-metric contract is **newline-delimited JSON in a W&B-shaped
schema**.

| Property | Value | Evidence |
|---|---|---|
| Container | JSONL, one JSON object per line | `cli/SDK_GUIDE.md` ("W&B-style `_step` and `_timestamp` fields"); `portal/README.md` ("Online metrics history") |
| Step | `_step`, must be an **integer** | `portal/README.md`: "Online rows still require numeric `_step` and `_timestamp` fields" |
| Time | `_timestamp`, finite positive Unix **seconds** | same |
| Series identity | the JSON object **key**, e.g. `train/loss` | `portal/internal/expimport/expimport_test.go:82` looks up `train/loss` |
| Value domain | scalars imported; non-scalar values tolerated and skipped | `portal/README.md` ("valid rows containing only metadata or non-scalar values do not stop the watcher") |
| Failure mode | malformed JSONL and non-finite numerics are **fatal**; the chunk is not checkpointed | `portal/README.md` |
| Chunking | closed, uniquely named immutable chunks preferred; consumed bytes checkpointed | `skills/taugrid/references/run-config.md` ("Publish closed, uniquely named immutable chunks before they match a glob") |
| Import path | `portal/internal/expimport/jsonl_import.go` — `JSONLImporterVersion = "tau.jsonl.import.v1"` | repo |
| Downstream | metrics-offload sidecar → collector checkpoints → typed projection → spool → queued ADX delivery → Stellar dashboard rows | `metrics/experiment-metrics-collector/`; `portal/README.md`; `charts/adx-mon/README.md` |

The naming convention is deliberately TensorBoard-*shaped*: the house format
already uses slash-delimited tags like `train/loss`, and the importer's tests
assert "TensorBoard card semantics" for those tags
(`portal/internal/expimport/expimport_test.go:91`). **TauGrid's tag vocabulary is
already TensorBoard-compatible. Only the container is not.**

### 2.2 The format TensorBoard writes

`tfevents` is a binary, framed, protobuf-encoded, append-only log.

| Property | Value | Evidence |
|---|---|---|
| Container | TFRecord framing: `uint64` length + **masked CRC32C** of the length + `length`-byte payload + masked CRC32C of the payload | [TFRecord format](https://www.tensorflow.org/tutorials/load_data/tfrecord) |
| Payload | protobuf `Event` message | [`event.proto`](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/compat/proto/event.proto) |
| Step | `Event.step` (int64) | `event.proto` |
| Time | `Event.wall_time` (**double**, Unix seconds) | `event.proto` |
| Series identity | `Event.summary.value[].tag` | [`summary.proto`](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/compat/proto/summary.proto) |
| Scalar payload | `Summary.Value.simple_value` (legacy) or `Summary.Value.tensor` (a `TensorProto`) | `summary.proto` |
| Non-scalar payloads | histograms, images, audio, graph, HParams are first-class siblings on the same stream | `summary.proto` |
| File discovery | `--logdir` glob over event files, convention `events.out.tfevents.<time>.<host>`; one run may span several files | TensorBoard core plugin |
| Reader complexity | TFRecord framing + CRC32C + protobuf varints + `TensorProto` dtype decode + truncated-tail tolerance | derived |

### 2.3 Side by side

| Dimension | House JSONL (TauGrid) | `tfevents` (TensorBoard) | Convergent? |
|---|---|---|---|
| Encoding | UTF-8 text | binary protobuf | **No** |
| Framing | newline | length + masked CRC32C | **No** |
| Step | `_step` int | `Event.step` int64 | **Yes** |
| Timestamp | `_timestamp` numeric seconds | `Event.wall_time` seconds (double) | **Yes**, and the fractional part must be preserved |
| Series identity | object key | `Value.tag` | **Yes** |
| Scalar value | JSON number | `simple_value` float, or `tensor` | **Yes** (`tensor` needs dtype decode) |
| Non-scalar | tolerated, skipped | first-class | Partial |
| Multiple series per record | yes (one object) | yes (`value[]`) | **Yes** |
| Append model | closed immutable chunks | append-only with flush | Compatible for conversion |
| Truncated tail | fatal (malformed row) | expected; readers must tolerate | **No** — adapters must handle |

### 2.4 The mapping

For the scalar subset — which is what a loss curve needs — the mapping is total:

```
Event.step                  -> _step        (int64 -> int)
Event.wall_time             -> _timestamp   (double -> numeric seconds, fractional part kept)
for v in Event.summary.value:
    v.tag                   -> key
    v.simple_value          -> number      (float)
    v.tensor (scalar)       -> number      (TensorProto dtype decode)
```

**Do not floor `wall_time` to whole seconds.** The projection identifies an
observation by the source file, the tag and the step; value and history-line tags
are excluded from that identity. Two observations of the same tag and step inside
one second would therefore collapse to the same event ID, and the ADX dedup view
would show only one of them. A single training step usually emits its metrics in
one burst, so this is a routine case rather than an edge case, and the loss would
be silent. Keep `_timestamp` as numeric seconds with its fractional part, and
require a regression case that ingests two same-tag, same-step observations inside
one second and asserts both survive to the dedup view.

Non-scalar `value` entries are dropped from the metric stream. They are **not**
lost from TauGrid's model: `tfevents` files are already classified as artifacts
by the store and UI (`portal/internal/expstore/store_test.go:722`,
`portal/frontend/src/stellar/evidence-helpers.ts:85`), so a histogram or image
remains reachable as run evidence without being a metric row.

### 2.5 Verdict

**Encoding: different. Semantics: the same.** Therefore:

- A **converter** from `tfevents` into house JSONL is small and well-bounded: one
  reader, one direction, scalar-only, no UI, no service, no new module
  dependency (§6.1).
- A **parallel TensorBoard UI** would duplicate a display layer TauGrid already
  has, and would introduce a second metrics pipeline whose semantics must then be
  kept in sync with the first — which is precisely the outcome the review asked to
  avoid.
- "Coincide the logging as much as possible" is therefore **already true at the
  schema level**. The remaining work is transport, not modeling.

---

## 3. Decision

| # | Decision | Consequence |
|---|---|---|
| **D1** | **Broaden the existing metrics path** to accept additional logging conventions, rather than adding a parallel one. | The notebook loss pane, Stellar, and any future consumer all read one format. This is the primary recommendation and answers R2. |
| **D2** | Treat `tfevents` as an **input adapter** that normalizes into house JSONL. | Scalar loss curves from TensorBoard-producing frameworks (Ray Tune, Lightning, HF, Keras) become available to every existing consumer. No new service. |
| **D3** | **Defer** the per-job TensorBoard sidecar + proxy service. | Avoids a new required service, a new Service per job, and a new proxy route. See §4. |
| **D4** | Treat "local TauGrid without ADX" as a **separate design**, not part of this one. | Keeps this proposal reviewable; see §10.5 for why it interacts with D1. |
| **D5** | Keep the notebook loss pane as the **primary** surface. | No iframe/subpath problem, no extra container, works with no opt-in. |

### 3.1 Where the adapter runs

Three placements, in rough order of preference:

1. **In the metrics path, as a supported input format.** The offload/collector
   sidecar (or its successor) learns to tail `tfevents` and emit house rows.
   Matches D1 and R2 most directly.
2. **As a small converter binary/command**, used by the sidecar or by an operator
   for backfill. Lowest blast radius; easy to test against recorded event files.
3. **At import time in `portal/internal/expimport`**, alongside the JSONL
   importer, for the offline/recovery path.

The owning component is `taugrid-metrics-collector` (§7.2). Which placement
variant it uses changes only where the dependency lives, not whether the mapping
works.

---

## 4. Why the service is deferred

### 4.1 Cost of the adapter vs the service

| | `tfevents` adapter (D2) | TensorBoard sidecar + proxy (original) |
|---|---|---|
| New required service | No | **Yes** — one Service per job + a portal proxy route |
| Governance | Routine change | **Design proposal + maintainer consensus** — `GOVERNANCE.md` routes "new required services or major dependencies" here |
| New container image | No | Yes — pinned TensorBoard image, supply chain review |
| Subpath/iframe risk | None | Root-relative asset rewriting or baked `--path_prefix`; integration-tested per image tag |
| Pod overhead | None | One extra container per job |
| Duplicate display layer | No | Yes — a second metrics UI beside Stellar |
| Multi-run comparison | Free (house rows are durable and comparable) | Per-job boards; cross-run overlays are a separate feature |
| Works after job ends | Yes, from the store | Only while the Service lives |

### 4.2 Conditions to revisit the service

Revisit if any of these becomes true:

- Users need TensorBoard-only features that the house format cannot express
  (histogram/image scrubbing, graph inspection, HParams comparison) **and** the
  artifact path is judged insufficient.
- The `tfevents` adapter proves materially harder than §2.4 implies — e.g.
  frameworks emit scalar shapes the mapping cannot decode.
- A maintainer decides a tracker UI is in scope for TauGrid despite `ROADMAP.md`
  listing framework and serving internals as "Not planned".

---

## 5. Architecture (revised)

The collector is not a remote-write forwarder. `metrics/experiment-metrics-collector`
owns the pipeline end to end, and the adapter has to sit at its input boundary:

```
RayJob
  |- ray-head
  |- metrics-offload / collector   (experiment-metrics-collector)
        |
        |  source checkpoints      (what has already been read from each source)
        |  typed projection        (source records -> house JSONL rows)
        |  durable spool           (spool.go: survive a restart before delivery)
        |  queued ADX delivery     (adx_queued.go: ordered, retried, backpressured)
        |
        |  (D2) tfevents --adapter--> typed projection (same rows, earlier stage)
        v
  ADX (house JSONL rows)
        |
        v
  Stellar / expstore  -->  Portal /api/stellar/series
        |
        v
  Jupyter Notebook panel (ipywidgets / tau.widgets)   <- the loss curve
```

Placement therefore matters more than "add a converter". A `tfevents` reader is an
additional **source** for the typed projection, and the existing checkpoints, spool
and queued delivery must keep working unchanged: a decoder that emits rows through
the same projection inherits retry, ordering and restart safety for free, while one
that writes to ADX directly would bypass all three.

The optional, deferred overlay:

```
  +-----------------------------------------------------------------+
  | (D3, deferred) tensorboard sidecar per job -> Service -> proxy  |
  +-----------------------------------------------------------------+
```

---

## 6. Evidence

### 6.1 In-repo

| Concern | Finding | Location |
|---|---|---|
| House format contract | JSONL with required numeric `_step` / `_timestamp` | `portal/README.md`; `cli/SDK_GUIDE.md` |
| House format importer | `ImportJSONL`, `JSONLImporterVersion = "tau.jsonl.import.v1"` | `portal/internal/expimport/jsonl_import.go` |
| Tag vocabulary already TensorBoard-shaped | `train/loss` asserted against "TensorBoard card semantics" | `portal/internal/expimport/expimport_test.go:82,91` |
| **No `tfevents` decoder exists** | `tfevents` appears only as an artifact filename fixture | `portal/internal/expstore/store_test.go:722`, `portal/internal/expstore/adx_export_test.go:351` |
| Protobuf is **already a direct dependency of the portal** | `google.golang.org/protobuf v1.36.12` | `portal/go.mod` |
| Protobuf is **not** a dependency of the collector | no protobuf entry; the module pulls `azkustodata`, `azkustoingest`, `azcore`, `azidentity` | `metrics/experiment-metrics-collector/go.mod` |
| CRC32C needs no new dependency | `hash/crc32` Castagnoli is stdlib | Go stdlib |
| Event files already modeled as artifacts | store + UI classification | `portal/internal/expstore/store_test.go:722`; `portal/frontend/src/stellar/evidence-helpers.ts:85`; `portal/internal/expcockpit/assets/app.js:5341` |
| Retained from the original proposal | sidecar injection, per-job Service discovery, readiness gating, SSRF-guarded reverse proxy, response rewriting, `framedSameOrigin` | `cli/internal/metricsoffload/render.go`; `portal/internal/portal/ray/ray.go`; `portal/internal/portalapi/rayproxy.go`; `portal/internal/portalapi/kueuevizproxy.go`; `portal/internal/portalapi/server.go` |

**Dependency consequence:** a `tfevents` reader needs a generated `Event`/
`Summary` protobuf type (or a hand-rolled decoder for the scalar subset). CRC32C is
stdlib in every case. Protobuf is already present in `portal/go.mod`, but **not** in
the collector module, so placing the adapter in the collector adds
`google.golang.org/protobuf` to a module that does not have it, while placing it
beside `expimport` does not. That is a real cost difference between the two
placements, and it is the one the reviewer's boundary correction exposes.

### 6.2 External

| Claim | Evidence | Status |
|---|---|---|
| `tfevents` uses TFRecord framing (length + masked CRC32C) | [TFRecord format](https://www.tensorflow.org/tutorials/load_data/tfrecord) | VERIFIED |
| Payload is a protobuf `Event`; `summary_iterator` yields `Event` protos | [`event.proto`](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/compat/proto/event.proto) | VERIFIED |
| Scalars appear as `simple_value` or `tensor` | [`summary.proto`](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/compat/proto/summary.proto) | VERIFIED (exact `TensorProto` dtype coverage for all frameworks **UNVERIFIED** here) |
| Ray Tune auto-callback loggers can be disabled with `TUNE_DISABLE_AUTO_CALLBACK_LOGGERS=1` | [Ray Tune env vars](https://docs.ray.io/en/latest/tune/api/env.html), [Ray Tune `callback.py`](https://github.com/ray-project/ray/blob/master/python/ray/tune/utils/callback.py) | VERIFIED |
| Ray emits a `tensorboard --logdir ...` hint | Ray source search | PARTIALLY VERIFIED (Train-wide startup **UNVERIFIED**) |
| Ray dashboard has no built-in equivalent loss curve | [ray-project/ray#8554](https://github.com/ray-project/ray/issues/8554) | VERIFIED as upstream-gap evidence |
| TensorBoard supports `--path_prefix`; subpath deployments can still break | [core_plugin.py](https://github.com/tensorflow/tensorboard/blob/master/tensorboard/plugins/core/core_plugin.py), TensorBoard `path_prefix` issues | `--path_prefix` VERIFIED; current root-absolute asset set **UNVERIFIED** and would need integration tests |

---

## 7. Repository inconsistencies found while preparing this revision

Reported because they affect how a reviewer reads the evidence:

1. **`portal/README.md:29` overstates `internal/expimport`.** It says the package
   holds "TensorBoard, Weights & Biases, and JSONL metric importers." Code
   inspection finds a JSONL importer (`jsonl_import.go`), a mapping file, and a
   remote-write path — and **no `tfevents` decoding**. The only TensorBoard tie is
   *tag/card naming semantics* (`expimport_test.go:91`). The README line should say
   the importer consumes TensorBoard-*shaped tags*, not event files.
2. **Correction to an earlier revision of this document.** It claimed the
   repository names a `taugrid-metrics-collector` that does not exist in-tree. That
   was wrong, and the error came from searching a checkout that was stale relative
   to `main`. The component is real:
   `metrics/experiment-metrics-collector/` (a Go module with
   `cmd/taugrid-metrics-collector`, plus `collector.go`, `history.go` and
   `spool.go`) and `images/taugrid-metrics-collector/Dockerfile`, listed as a
   first-party image in `scripts/lib/image-specs.sh`. `portal/README.md` is
   accurate. **This answers §9 Q1**: the training-metrics sidecar is the component
   D1/D2 extend.
3. **`monitoring/gpu-metrics-collector` is unrelated.** It scrapes DCGM/node
   exporter endpoints and writes Kubernetes **Node conditions**. It does not read
   training metrics and is not the container meant by R2.

---

## 8. Risks

**Blocking (need a maintainer answer before implementation):**

1. ~~Which component owns metric input~~ — resolved: `taugrid-metrics-collector`.
2. **Scalar decode coverage.** `simple_value` is trivial; `tensor` requires dtype
   decoding. Which frameworks emit which is not established here.
3. **Append-only tail semantics.** `tfevents` files are written incrementally with
   flushes, unlike TauGrid's closed immutable chunks. The adapter must tolerate a
   partially written trailing record or it will fail on live runs.

**Deferrable:**

- Non-scalar `tfevents` payloads (histograms, images) as first-class artifacts.
- Backfill of historical `tfevents` from object storage.
- Cross-run comparison UX for TensorBoard-origin runs.

---

## 9. Open questions for maintainers

1. ~~Which component should accept the new input format?~~ **Answered by §7.2:**
   `taugrid-metrics-collector` (`metrics/experiment-metrics-collector`) is the
   training-metrics sidecar that replaced the legacy portal offload verb, so it is
   the component D1/D2 extend. No maintainer answer needed.
2. **Is a `tfevents` reader acceptable in-tree**, and where should it sit? It needs a
   generated protobuf type. `portal/go.mod` already carries protobuf; the collector
   module does not, so an adapter inside the collector adds a dependency to that
   module, while one beside `expimport` does not. §5 argues for the collector
   boundary because it inherits the existing spool and queued delivery.
3. **Is "make the metrics path broad" the right framing for R2**, or is a
   tracker UI still wanted as a separate, explicitly-scoped product surface?
4. **Should the local-mode question (§10.5) block this**, or run as its own design?
5. **Should `portal/README.md` be corrected** as part of this PR or separately?

---

## 10. Alternatives considered

### 10.1 Per-job TensorBoard sidecar + portal proxy (the original proposal)

Long-lived proposal from the first revision of this document: TensorBoard runs as
a sidecar in the RayJob head pod, is exposed through a per-job Service, and is
proxied to the notebook by the portal; link first, embed only when same-origin;
proxy-side URL rewriting rather than a baked `--path_prefix`; opt-in, mirroring
`metrics.offload`.

- **Pros:** full upstream TensorBoard fidelity, including features the house
  format does not model.
- **Cons:** a new required service and route (GOVERNANCE), a pinned third-party
  image, one extra container per job, per-image subpath integration tests, and a
  second metrics display layer. See §4.1.
- **Status:** deferred, not rejected. Conditions in §4.2.

### 10.2 `tfevents` adapter into the house format (**recommended**)

- **Pros:** no new service; one metrics pipeline; durable and cross-run
  comparable; works after the job ends; reuses existing Stellar rendering; the
  tag vocabulary already matches.
- **Cons:** drops non-scalar payloads from the metric stream (they remain
  artifacts); needs TFRecord/CRC32C/protobuf handling and truncated-tail
  tolerance.
- **Status:** the recommendation (D2).

### 10.3 Link out to a user-run TensorBoard

Zero code, but no discoverability; the user still manages networking and
port-forwarding. Acceptable as a documented workaround; not an integration.

### 10.4 Shared-logdir TensorBoard

One board over a cluster-wide shared PVC. Lower cost, weak isolation without
additional tenancy controls. Rejected for v1 in the original revision — and note
that `ROADMAP.md` places multi-tenancy under "Next", with one active workspace
today, so it cannot be assumed as a safety net.

### 10.5 Local TauGrid without an ADX backend (R4)

Raised in review and **out of scope here**. It is a deployment-topology question
for the metrics path (D1), not a notebook-integration question: if the loss pane
should work without ADX, the decision is where house rows live locally and who
serves them. Recording it as a separate design keeps this proposal reviewable.

---

## 11. What this design needs from reviewers

1. **Confirm the reframe** (§0, §3): house format first, `tfevents` as an adapter,
   TensorBoard service deferred.
2. **Answer §9 Q3** — whether a tracker UI is wanted as a separate surface.
   (§9 Q1 is closed by §7.2.)
3. **Confirm §7.2** — resolved in this revision: `taugrid-metrics-collector`
   exists at `metrics/experiment-metrics-collector`, so no answer is needed and
   §9 Q1 is closed.
4. **Confirm §7.1** — whether the `expimport` README line should be corrected in
   this PR.
5. **Decide §9 Q4** — whether local-mode (§10.5) blocks D1 or proceeds separately.
