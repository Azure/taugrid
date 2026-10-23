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
| R6 | The collector already owns source checkpoints, typed projection, durable spooling and queued ADX delivery; assess protobuf in the collector module, not `portal/go.mod` | §5 no longer draws the collector as a remote-write forwarder: the adapter is an additional source at the typed-projection boundary, and the spool and queued delivery are reused rather than bypassed. **R10 later narrows the source/checkpoint half of this row**: the JSONL reader is not reusable for a framed source. §6.1 now records that protobuf is **not** in the collector module, so placement changes the dependency cost. |
| R8 | The typed projection does not emit `_step`/`_timestamp` house rows; it emits canonical `MetricEvent` NDJSON, and both spool replay and queued delivery validate that form | §5 names the real type at each boundary: history-shaped scalar records -> typed projection (`ProjectJSONLHistoryRow`) -> canonical `MetricEvent` NDJSON (`tau.experiment.metric.v1`) -> ADX, with a boundary table and code references. §2.1, §3.1 and §4.1 no longer label the projection's output or the ADX payload "house JSONL". |
| R9 | The cited store fixture registers an event-file artifact; it does not establish automatic retention of files consumed by the proposed adapter | §2.4 no longer argues that "already classified as artifacts" preserves dropped non-scalars. The promise is now conditional on durable retention **and** run-associated registration of the original event files, with the fallback stated when either is absent. §6.1, §8 and §10.2 are qualified the same way, and §9 Q6 records the open dependency. |
| R10 (4235906850) | The source checkpoint layer cannot be reused unchanged: `readSource` truncates at the last newline and tracks line counts, while `Runner.project` splits those bytes on newlines and decodes JSON objects, so a framed `tfevents` source can checkpoint mid-frame and make restart fail or misdecode | §5's "reused unchanged" claim is withdrawn. §5.1 adds the framing-aware source/checkpoint abstraction: advance only across a complete TFRecord frame, retain a truncated tail and retry it on the next pass, never checkpoint past it. §5's boundary table and §8 risk 3 are updated to match. |
| R11 (4235906853) | TensorBoard's run identity lives in the event-file directory hierarchy while Tau's deterministic event ID folds in `MetricFileID`, so Tune trial directories can merge into one Tau series and rotation can evade dedup | §2.4.1 maps the relative logdir hierarchy onto Tau project/experiment/group/run and the metric prefix, keeps Tune trial directories distinct, and §2.4.2 defines a run-scoped stable source identity that survives event-file rotation. §2.4.1 additionally requires the adapter to **refuse** when trial separation cannot be proven (R15). |
| R12 (4235906856) | The collector path is opt-in and needs a history path, project/experiment identity, durable storage and ADX configuration; the companion plugin emits `{"step":3,"loss":1.25}` but online rows require `_step`/`_timestamp` | §3.2 specifies the submission contract: what enables and mounts the offload sidecar, which identity is resolved and stamped, and the explicit normalizer that rewrites the plugin's conventions into history-shaped rows. §3 D5 no longer claims the loss pane "works with no opt-in" for the collector path. |
| R13 (4235906860) | "TensorBoard-compatible" overstates the evidence: the cited test only pins Tau card classification for `train/loss`, not run identity, dtype conversion, duplicates, ordering, or non-scalars | §2.4.3 narrows the claim to "scalar events can be normalized into the canonical `MetricEvent` pipeline" and lists the verified rules: finite `float64` values, microsecond timestamps, nonnegative `int64` steps, non-scalars dropped. §2.1 and §2.5 are narrowed to match. |
| R14 (4236026694) | The notebook read path cannot use the legacy series projection, which does not read the canonical `MetricEvent` rows delivered to ADX | §5.2 replaces the legacy reference with `GET /api/v2/stellar/runs/{run_id}/series`, documents the scoped request and its `points` response, and notes that the legacy route is served `Deprecation: true`. The §5 diagram points at the v2 route. |
| R15 (this revision and r4279792645) | §2.4.1's identity mapping assumes the logdir yields one directory per trial arm, but nothing establishes that the platform guarantees it. A flattened tree, several arms writing into one directory, or a retained directory from an earlier attempt can collapse distinct trials onto one `run_group_id`, and the ADX dedup view then merges them into a single series that looks valid. A declared arm count is not proof: it can be satisfied by a stale directory while two current arms share another. | **Decided: the adapter refuses unless the current submission supplies an arm-to-directory ownership map.** §2.4.1 now rejects a missing map, a count-only declaration, duplicate ownership, an incomplete map, or a missing mapped directory without projecting rows. §3.2 makes the current-submission map part of the source declaration. The design therefore no longer depends on a platform guarantee or on stale discovery state; a producer that cannot supply the map is refused. §9 Q7 records the decision and §11 item 10 is corrected. |

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
**not similar in encoding, but overlap in scalar semantics**. That combination
means the right move is a **converter into the house format**, not a parallel UI,
and a converter whose normalization rules are stated rather than assumed (§2.4.3).

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
| Downstream | metrics-offload sidecar → collector source checkpoints → typed projection (`exptelemetry.ProjectJSONLHistoryRow`) → canonical `MetricEvent` NDJSON (`tau.experiment.metric.v1`) → durable spool → queued ADX delivery → Stellar dashboard rows | `metrics/experiment-metrics-collector/collector.go`; `core/exptelemetry/history.go`; `portal/README.md`; `charts/adx-mon/README.md` |

The naming convention is deliberately TensorBoard-*shaped*: the house format
already uses slash-delimited tags like `train/loss`, and the importer's tests
assert "TensorBoard card semantics" for those tags
(`portal/internal/expimport/expimport_test.go:91`). That test pins one thing
only: `ResearchMetricCard` classifies `train/loss` as the `World model` card
(`core/exptelemetry/mapping.go:53-62`). It does **not** cover TensorBoard run
identity, `TensorProto` dtype conversion, duplicate steps, ordering, or
non-scalar payloads. **The accurate statement of compatibility is narrower: the
tag vocabulary overlaps, and TensorBoard scalar events can be normalized into the
existing canonical `MetricEvent` pipeline. Nothing about the container, the run
identity, or the non-scalar payloads is inherited.** The rules a normalizer must
apply are enumerated in §2.4.3.

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

#### 2.4.1 Logical run -> Tau identity

TensorBoard identifies a series by the **relative event-file path beneath
`--logdir`** plus the tag; the tag alone identifies only a metric inside one run.
Tau instead groups a series by run identity and folds a source-file identity into
every event. The identity inputs are real and narrow:

- `MetricEvent.DeterministicEventID` hashes `workspace_id`, `cluster`,
  `namespace`, `source_store_id`, `metric_file_id`, `project`, `experiment_id`,
  `run_group_id`, `run_id`, `metric_name`, `step`, and `wall_time`
  (`core/exptelemetry/metric.go:91-122`). `value`, tags, `exported_at`, and the
  history line number are deliberately excluded, which is why R7 requires the
  fractional `wall_time` to survive.
- The collector supplies that identity from its own options, not from the row:
  `Project`, `Experiment`, `Group`, `Run`, and `HistoryPath` are stamped at
  projection time (`metrics/experiment-metrics-collector/collector.go:463-472`),
  and `MetricFileID` is `logicalMetricFileID(read.path)`
  (`metrics/experiment-metrics-collector/history.go:205-208`).

Without an explicit mapping, two trial directories that both write `train/loss`
project to the same `run_id` and metric name, so the ADX dedup view merges two
distinct trials with overlapping steps. The mapping is therefore part of the
adapter's contract, not a naming preference:

| TensorBoard position | Tau field | Rule |
|---|---|---|
| logdir run directory, e.g. `tune/trial_0001` | `run_group_id` | parent directories below the configured root identify the Tune trial arm; each trial directory is a **distinct** run group and is never folded into one |
| event-file basename within the run directory | `run_id` | one logical run per directory; the file is a rotation segment of that run, not a new run |
| tag, e.g. `train/loss` | `metric_name` | the tag is used verbatim as the series key |
| tag with a run-scoped prefix | `metric_name` prefix | the adapter rewrites the tag to `<prefix>/<tag>` before emitting the row; the collector does not set `JSONLHistoryOptions.MetricPrefix` today (`collector.go:463-472`), so the prefix must be applied by the adapter on the key, not by that flag |
| `project` / `experiment` selected for the run | `project`, `experiment_id` | resolved from the submission contract in §3.2, not inferred from the path |

Tags remain attached to the event for display (`tau.metric.card`,
`jsonl.history_path` and friends, `core/exptelemetry/history.go:105-116`), but
they are not part of `event_id`. The trial separation must therefore live in a
field that *is* part of the identity — `run_group_id` — and not in a tag.

**Trial separation is enforced by refusal, not by assumption.** The mapping above
is implementable only where the adapter can *prove* that each trial arm owns a
distinct directory in the current submission. Where it cannot, the adapter
**refuses the read** instead of projecting the rows, because the failure it would
otherwise produce — two arms merged under one `run_group_id` — is a silently wrong
loss curve, indistinguishable from a valid one. A declared arm count is not proof:
two current arms can share one directory while a retained directory from an earlier
attempt makes discovery find the declared number. The adapter therefore refuses when:

1. the configured `metrics.history` glob resolves event files with **no
   run-relative path segment** below the configured root, so no directory value
   exists to carry `run_group_id`; or
2. the submission contract (§3.2) provides **no current-submission
   arm-to-directory ownership map**; a count-only declaration is not a map; or
3. the ownership map is not complete, one-to-one, and observable: every current
   arm maps to exactly one directory, every mapped directory exists, and no
   directory is mapped to more than one arm.

TensorBoard rotation segments *legitimately* share one directory (§2.4.2), so a
directory receiving writes from two trial arms is **not** distinguishable from a
rotating single run by path alone. The map removes that ambiguity at the
submission boundary: directories discovered but absent from the current map are
treated as retained state from another attempt and are ignored, not counted as
evidence that a current arm owns them. If the producer cannot supply a complete
current map, the adapter refuses rather than infer ownership from the directory
inventory.

Refusal costs runs a permissive adapter would have accepted — including flat,
single-run layouts that are perfectly unambiguous to a human. That is the intended
trade, because the alternative is a series that looks correct and is not.

#### 2.4.2 Stable source identity across event-file rotation

`logicalMetricFileID` hashes the **cleaned filesystem path**
(`metrics/experiment-metrics-collector/history.go:205-208`). TensorBoard rotates
event files — the convention is `events.out.tfevents.<time>.<host>`, and one
logical run may span several files (§2.2) — so a replayed or restarted run gets a
new filename, a new `metric_file_id`, a new `event_id`, and the same logical point
is delivered twice without collapsing in the dedup view. That is the
"rotation/replay" case the review raised, and it is real.

The design therefore keeps **source identity independent of the physical
filename**:

- `metric_file_id` is derived from a **run-scoped logical source key** —
  workspace, project, experiment, run group, run, and the run-relative source
  path — not from the absolute path and not from the rotation segment name. A
  new `events.out.tfevents.*` segment in the same run directory reuses the same
  logical source key.
- The rotation segment (basename and device/inode) is tracked in the
  **checkpoint**, which is the layer that must know the physical file changed:
  `SourceCheckpoint.Path` and `SourceCheckpoint.FileID`
  (`metrics/experiment-metrics-collector/history.go:20-30`). Checkpoint state is
  per segment; event identity is per logical run.
- Because the collector currently re-derives `MetricFileID` from `read.path`
  (`collector.go:471`), this is a change to where the projection gets that value:
  the framing-aware source abstraction in §5.1 must carry the logical source key,
  and `project` must take `metric_file_id` from the source rather than hash the
  path itself.
- Replay of the same logical run then re-derives the same `event_id` for the same
  `(metric_name, step, wall_time)` and the ADX dedup view collapses it, while a
  genuinely distinct trial stays distinct through its `run_group_id`.

`run_id`, `run_group_id`, and `metric_file_id` are all part of
`DeterministicEventID` (`core/exptelemetry/metric.go:97-118`), so §2.4.1 and
§2.4.2 are not cosmetic labeling: they change which points are considered the
same point.

#### 2.4.3 Normalization rules

These are the rules the shared projection actually enforces. The adapter must
satisfy them rather than assume them (`core/exptelemetry/history.go`):

| Rule | Behavior | Evidence |
|---|---|---|
| Step | must be representable as a nonnegative `int64`; negative and non-representable steps are rejected, not clamped | `history.go:144-201` |
| Timestamp | must be a finite numeric epoch-seconds value; converted with `time.UnixMicro(int64(number * 1_000_000))` — **truncated to microseconds**, stored UTC | `history.go:203-208` |
| Value | must be a finite `float64`; NaN and ±Inf are rejected, not skipped | `history.go:91-94`, `history.go:211-250` |
| Non-scalar value | skipped: a key whose value is not numeric is dropped from the metric stream | `history.go:95-97` |
| Metadata key | a key beginning with `_` is metadata and is not a series | `history.go:82` |
| Series key | remaining keys, sorted, each become one `MetricEvent` with `metric_name` = key | `history.go:80-98` |

Three consequences the design states plainly: a TensorBoard `summary.value` with
a non-scalar payload produces **no** metric event; an out-of-range or negative
`Event.step` fails the whole row rather than being repaired; and `Event.wall_time`
loses precision below a microsecond. None of these are covered by
`expimport_test.go:91`.

Non-scalar `value` entries are dropped from the metric stream: the projection
keeps only finite numeric keys and skips the rest
(`core/exptelemetry/history.go:95`). Dropping them is therefore **not** by itself
a preservation guarantee. Being representable as an artifact is not retention:
the store model accepts an `ArtifactRecord` of type `tensorboard` for an event
file (`portal/internal/expstore/store_test.go:875`,
`portal/internal/expstore/adx_export_test.go:347`) and the UI recognizes
"tensorboard event"/"event file" as a non-media artifact kind
(`portal/internal/expcockpit/assets/app.js:5341`), but both are classification,
not offload. Artifact offload only processes artifacts **already registered
against the run** — it enumerates `store.ArtifactsForRun(ctx, opts.RunID)` and
uploads those (`portal/internal/artifactoffload/offload.go:129`), and the
adapter registers nothing.

A histogram or image therefore stays reachable as run evidence only when both
conditions hold: the original `tfevents` file is **durably retained**, and it is
**registered against the run** so the offload pass sees it. Where either
condition is unmet for a given run, the dropped payloads are not recoverable
from TauGrid, and the adapter must surface that rather than assume
preservation. §9 Q6 tracks the dependency.

### 2.5 Verdict

**Encoding: different. Scalar semantics: convertible.** Therefore:

- A **converter** from `tfevents` into history-shaped rows — the records the
  collector's typed projection already consumes — is bounded but not free: one
  reader, one direction, scalar-only, no UI, no service, no new module dependency
  (§6.1). It also needs the framing-aware source and checkpoint abstraction in
  §5.1, because the existing source layer cannot be reused unchanged. The
  projection, not the adapter, produces the canonical `MetricEvent` NDJSON that
  the spool and ADX delivery validate (§5).
- A **parallel TensorBoard UI** would duplicate a display layer TauGrid already
  has, and would introduce a second metrics pipeline whose semantics must then be
  kept in sync with the first — which is precisely the outcome the review asked to
  avoid.
- What is true at the schema level is narrower than "TensorBoard-compatible":
  **TensorBoard scalar events can be normalized into the existing canonical
  `MetricEvent` pipeline** under the rules in §2.4.3. Run identity (§2.4.1),
  source identity across rotation (§2.4.2), non-scalar payloads, dtype coverage,
  duplicates, and ordering are not inherited; each is either designed here or
  explicitly dropped.

---

## 3. Decision

| # | Decision | Consequence |
|---|---|---|
| **D1** | **Broaden the existing metrics path** to accept additional logging conventions, rather than adding a parallel one. | The notebook loss pane, Stellar, and any future consumer all read one format. This is the primary recommendation and answers R2. |
| **D2** | Treat `tfevents` as an **input adapter** that normalizes into history-shaped scalar records, and stop it there. | Scalar loss curves from TensorBoard-producing frameworks (Ray Tune, Lightning, HF, Keras) become available to every existing consumer. The adapter emits the projection's *input*; it does not emit canonical `MetricEvent` NDJSON. No new service. |
| **D3** | **Defer** the per-job TensorBoard sidecar + proxy service. | Avoids a new required service, a new Service per job, and a new proxy route. See §4. |
| **D4** | Treat "local TauGrid without ADX" as a **separate design**, not part of this one. | Keeps this proposal reviewable; see §10.5 for why it interacts with D1. |
| **D5** | Keep the notebook loss pane as the **primary** surface. | No iframe/subpath problem and no extra container. The loss pane is only passive: the collector path that feeds it still requires the opt-in and submission contract in §3.2. |

### 3.1 Where the adapter runs

Three placements, in rough order of preference:

1. **In the metrics path, as a supported input format.** The offload/collector
   sidecar (or its successor) learns to tail `tfevents` and emit history-shaped
   scalar records — the records the typed projection consumes — using the
   frame-aware source layer in §5.1. Matches D1 and R2 most directly.
2. **As a small converter binary/command**, used by the sidecar or by an operator
   for backfill. Lowest blast radius; easy to test against recorded event files.
3. **At import time in `portal/internal/expimport`**, alongside the JSONL
   importer, for the offline/recovery path.

The owning component is `taugrid-metrics-collector` (§7.2). Which placement
variant it uses changes only where the dependency lives, not whether the mapping
works.

### 3.2 Submission contract (how a run actually reaches the adapter)

The collector path is opt-in and gated. A notebook run does not get metrics by
writing a file. The following contract is what has to hold for the loss pane to
have anything to read; every step is verified against `origin/main`:

1. **Enable and mount the collector.** `metrics.offload.enabled` defaults to
   `false` (`core/runconfig/schema.go:148`), so the producer must set it. The
   submission must also provide a pinned sidecar image
   (`metrics.offload.image` or `TAU_METRICS_OFFLOAD_IMAGE`,
   `cli/internal/cli/run_job.go:556-558`) and a writable `storage.output` PVC
   beneath `storage.DurableRoot` (`/data`, `cli/internal/storage/paths.go:12`;
   enforced at `run_job.go:515-521`).
2. **Declare the source paths and trial ownership.** `metrics.history` must name
   at least one JSONL path or glob when offload is enabled
   (`core/runconfig/config.go:702-704`), and each resolved path must stay under
   the durable root (`run_job.go:563-573`). This is where the adapter's source
   patterns are declared. For a multi-arm trial, the same submission must also
   provide the current-submission arm-to-directory ownership map required by
   §2.4.1. The map is generated for this run, not inferred from whatever
   directories happen to exist under the durable root; an arm count alone does
   not satisfy it.
3. **Resolve and stamp identity.** With offload enabled the platform requires
   `experiment.project` and `experiment.name`
   (`core/runconfig/config.go:705-712`) and passes `experiment.group` through
   when set (`config.go:719-723`). The CLI then stamps the resolved values onto
   the collector runtime — `RunID`, `Project`, `Experiment`, `Group` (defaulting
   to `default` when no group is set), merged tags, and the source name
   (`run_job.go:575-588`, `run_job.go:614-621`). The collector refuses to start
   without `run`, `project`, `experiment`, `group`, `source`, and `out`
   (`metrics/experiment-metrics-collector/collector.go:60-68`), and the
   projection copies that identity onto every `MetricEvent`
   (`collector.go:463-472`).
4. **Configure delivery.** `metrics.offload.delivery_mode` has one supported
   value, `adx-required` (`core/runconfig/schema.go:152`), and delivery requires
   a cluster URI, database, and client ID
   (`cli/internal/metricsoffload/options.go:131-139`; declared "Required for
   metrics offload" at `schema.go:153-157`). The supported sinks are ADX-only, so
   a run with no ADX has nowhere to deliver — which is exactly why §10.5 is a
   separate design rather than an assumption here.
5. **Emit rows the projection accepts, or normalize them first.** The online row
   contract requires numeric `_step` and `_timestamp`
   (`core/runconfig/schema.go:146`; `portal/README.md:60`). The companion plugin
   proposes the metric-file convention `{"step":3,"loss":1.25}` at
   `docs/design/notebook-plugin.md:268`, which is **not** that shape: with no
   `_step` key, `historyStep` receives `nil` and the row fails
   (`core/exptelemetry/history.go:71-74`, `history.go:185-188`). The design
   therefore introduces an explicit normalizer as the adapter's first stage:

   | Notebook input | Normalized history row handed to the source |
   |---|---|
   | metric-file line `{"step":3,"loss":1.25}` | `{"_step":3,"_timestamp":<wall clock or declared time>,"loss":1.25}` |
   | stdout `step=<int> loss=<float>` | the same normalized row |

   The normalizer owns the `step` -> `_step` and timestamp synthesis exactly once,
   before the record crosses the projection boundary. The adapter does **not**
   emit canonical `MetricEvent` NDJSON directly: the projection remains the
   component that enforces `_step`/`_timestamp`, assigns the deterministic
   `event_id`, and stamps run identity (§5, R8). A collector that is enabled but
   has no producing writer is a valid configuration; it will simply have no rows
   to read.

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
| Multi-run comparison | Free (canonical metric rows in ADX are durable and comparable) | Per-job boards; cross-run overlays are a separate feature |
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
        |  typed projection        (history-shaped scalar records -> canonical MetricEvent NDJSON)
        |  durable spool           (spool.go: survive a restart before delivery)
        |  queued ADX delivery     (adx_queued.go: ordered, retried, backpressured)
        |
        |  (D2) tfevents --adapter--> history-shaped scalar records
        v
  ADX (canonical MetricEvent NDJSON, schema tau.experiment.metric.v1)
        |
        v
  Stellar / expstore  -->  Portal /api/v2/stellar/runs/{run_id}/series
        |
        v
  Jupyter Notebook panel (ipywidgets / tau.widgets)   <- the loss curve
```

The stages carry four distinct types, and only the second is the house
`_step`/`_timestamp` row shape:

| Boundary | What actually crosses it | Defined at |
|---|---|---|
| source file -> checkpoints | for a JSONL source, raw history bytes plus a `SourceCheckpoint` (offset, line count, prefix digest); for a framed source, a frame-aligned offset plus the logical source key (§5.1) | `metrics/experiment-metrics-collector/history.go:20`; §5.1 |
| source record -> typed projection | history-shaped scalar records: one decoded JSON object per line, with `_step`, `_timestamp`, and one numeric key per series | `metrics/experiment-metrics-collector/collector.go:456`; `core/exptelemetry/history.go:52` |
| typed projection -> spool | `[]exptelemetry.MetricEvent` and the canonical NDJSON each event marshals to | `metrics/experiment-metrics-collector/collector.go:446`; `core/exptelemetry/metric.go:46` |
| spool -> ADX | `MetricEventChunk.NDJSON`, re-validated as canonical `MetricEvent` rows before it is queued | `metrics/experiment-metrics-collector/types.go:20`; `spool.go:481`; `adx_queued.go:284` |

Placement therefore matters more than "add a converter". A `tfevents` reader is an
additional **source** that emits history-shaped scalar records, so it enters at the
typed projection's input rather than at its output. The spool and queued delivery
are reused unchanged. **The source/checkpoint layer is not.** The reviewer's reading
is correct: `readSource` reads from the checkpoint offset to EOF and, unless
`includeTrailingRecord` is set, truncates the buffer at the last newline
(`metrics/experiment-metrics-collector/history.go:118-129`), then reports a line
count (`history.go:144-150`); `Runner.project` splits those bytes on newlines and
decodes each one as a JSON object (`collector.go:449-463`). A protobuf payload may
contain `0x0a`, and a live event file may end midway through a TFRecord, so reusing
that reader can checkpoint in the middle of a frame and make restart permanently
fail or misdecode. §5.1 defines the framing-aware abstraction the adapter must use
instead. A decoder that emitted canonical `MetricEvent` NDJSON directly, or wrote
straight to ADX, would additionally bypass the projection that enforces
`_step`/`_timestamp`, assigns the deterministic `event_id`, and stamps run identity.

The projection's output is **not** house JSONL. `Runner.project` returns
`[]exptelemetry.MetricEvent` plus the canonical NDJSON each event marshals to
(`collector.go:446`, `core/exptelemetry/metric.go:177`). The spool validates that
form on load and replay — `decodeCanonicalEvents` re-decodes every line and requires
it to round-trip byte-for-byte (`spool.go:481`) — and `ADXQueuedSink.Deliver`
validates it again before queueing (`adx_queued.go:284`), which is why ADX receives
canonical `MetricEvent` NDJSON rather than `_step`/`_timestamp` rows.

### 5.1 Framing-aware source and checkpoint abstraction (D1)

The JSONL source contract is newline-terminated records with a line-count identity.
A TFRecord-framed source cannot use it, so the design introduces a source
abstraction with the same checkpoint semantics (durable, restart-safe, baseline +
tail) but a frame-aware advance rule:

- **Record boundary.** A record is complete only when all four fields of the
  TFRecord frame are present and verified: the `uint64` length, the masked CRC32C of
  that length, the `length`-byte protobuf payload, and the masked CRC32C of the
  payload (§2.2). Advancing the checkpoint requires a complete frame; a
  length/payload CRC mismatch is a hard decode error, not a partial read, and it
  does not advance the checkpoint.
- **Truncated tail.** When EOF falls inside a frame — no complete length, an
  incomplete payload, or a missing payload CRC — the reader stops at the last
  verified frame boundary. The partial bytes are **retained by the checkpoint** and
  retried on the next pass; the checkpoint offset is the offset just past the last
  complete frame, never past the partial tail. The reader must not emit a record
  from a partial frame and must not treat the tail as a decode failure.
- **No mid-frame checkpoint.** The checkpoint's offset is always a verified frame
  boundary, so a restart re-reads only the retained tail. This is the property the
  current `readSource` cannot provide, because its offset is derived from a
  newline scan (`history.go:122-130`).
- **Completion sentinel.** The existing completion-file contract still terminates
  the run. On sentinel, a genuinely truncated tail is reported rather than silently
  dropped, matching the JSONL rule that an unterminated final row is not read as
  complete data (`portal/README.md:67-69`).
- **Ordering guarantee.** Within one source, frames are delivered to the projection
  strictly in file order, and a source file is read only after its checkpoint prefix
  digest matches (`history.go:107-113`); the spool and ADX sink preserve that order
  per run (`spool.go`, `adx_queued.go`). Across several rotation segments of one
  logical run, the segments are ordered by the TensorBoard write order, and the
  logical-run identity is what makes the union a single series (§2.4.2). The
  guarantee is **per logical run, not global**: two runs may be delivered
  concurrently, and the reader does not promise a total order across runs.
- **Carried identity.** The source carries both the physical segment identity (for
  the checkpoint, `SourceCheckpoint.Path`/`FileID`, `history.go:20-30`) and the
  logical source key that becomes `metric_file_id` (§2.4.2). That is the change
  `collector.go:471` needs: `project` must take `metric_file_id` from the source
  rather than re-hash `read.path`.

The spool, checkpoint storage, and queued ADX delivery are reused; the
source-read and checkpoint-advance logic is new code with its own tests for
mid-frame EOF, CRC mismatch, and restart-after-truncation.

### 5.2 Notebook read API (D5)

The notebook reads the **canonical** Stellar v2 series route, not the legacy
projection:

`GET /api/v2/stellar/runs/{run_id}/series?target=<experiment|run-group|run>&metric=<tag>`

The route is wired at `portal/internal/expapi/server.go:311` and dispatched to
`handleV2Series` (`portal/internal/expapi/v2.go:502-559`). Its scoped request is:

| Parameter | Meaning | Source |
|---|---|---|
| `{run_id}` (path) | exact run; must be the same value as any `run_id` query parameter or the request fails `RUN_MISMATCH` | `v2.go:513-516` |
| `target` (required) | experiment, run group, or run that authorizes the read; must resolve to the run | `v2.go:507-512`, `v2.go:522-525` |
| `metric` (required) | the series tag, e.g. `train/loss` | `v2.go:508-512` |
| `workspace`, `project` | scope the lookup; a wrong workspace is rejected | `v2.go:503`, `v2.go:562-566` |
| `start_step`, `end_step`, `max_points` | bounded window and decimation; `max_points` is range-checked | `v2.go:526`; `portal/README.md:279-281` |

The response is `v2SeriesResponse` (`v2.go:133-145`); the notebook consumes
`points`:

```json
{
  "metadata": {"...": "..."},
  "target": "experiment-alpha",
  "run_id": "run-1",
  "metric": "train/loss",
  "max_points": 2000,
  "source_points": 12345,
  "returned_points": 2000,
  "points": [{"step": 1, "value": 1.25}]
}
```

Each `points` entry is a `v2SeriesPoint` with an `int64` `step` and a `float64`
`value` (`v2.go:128-131`). Two properties matter for the loss pane: the route fails
closed on canonical typed reads — discovery uses `TauExpRunCatalogRows()` and the
bounded series uses `TauExpMetricEventRows()` (`portal/README.md:138-142`) — and the
legacy `GET /api/stellar/series` is served with `Deprecation: true` and a
successor-version link (`server.go:314-328`, `server.go:971`). A collector-only run
therefore cannot be read through the legacy route; the v2 route is the one that
sees the canonical `MetricEvent` rows delivered to ADX.

The companion `notebook-plugin.md` still names the legacy
`GET /api/stellar/series` route (`docs/design/notebook-plugin.md:257`). That
reference is stale for the same reason and should be corrected when that document
is next revised; it is out of scope for this one-file change and is recorded in §7.

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
| Typed projection shared by the importer and the collector | `ProjectJSONLHistoryRow` turns one decoded history row into `[]MetricEvent` | `core/exptelemetry/history.go:52`; `metrics/experiment-metrics-collector/collector.go:463` |
| **Canonical metric boundary** | `MetricEvent` is `tau.experiment.metric.v1`; its marshaled NDJSON is what the spool and ADX delivery validate | `core/exptelemetry/metric.go:18`; `metrics/experiment-metrics-collector/spool.go:481`; `metrics/experiment-metrics-collector/adx_queued.go:284` |
| Tag overlap is narrower than "TensorBoard-compatible" | `train/loss` is asserted against "TensorBoard card semantics", but the test only pins `ResearchMetricCard` -> `World model` for that one tag | `portal/internal/expimport/expimport_test.go:82,91`; `core/exptelemetry/mapping.go:53-62`; §2.4.3 |
| **No `tfevents` decoder exists** | `tfevents` appears only as an artifact filename in fixtures | `portal/internal/expstore/store_test.go:879`, `portal/internal/expstore/adx_export_test.go:351` |
| Protobuf is **already a direct dependency of the portal** | `google.golang.org/protobuf v1.36.12` | `portal/go.mod` |
| Protobuf is **not** a dependency of the collector | no protobuf entry; the module pulls `azkustodata`, `azkustoingest`, `azcore`, `azidentity` | `metrics/experiment-metrics-collector/go.mod` |
| CRC32C needs no new dependency | `hash/crc32` Castagnoli is stdlib | Go stdlib |
| Event files **can** be artifacts, but are not automatically retained | fixtures explicitly register a `Type: "tensorboard"` artifact for a run; the UI treats "tensorboard event"/"event file" as a non-media artifact kind; artifact offload uploads only artifacts already registered for the run | `portal/internal/expstore/store_test.go:875`; `portal/internal/expcockpit/assets/app.js:5341`; `portal/internal/artifactoffload/offload.go:129` |
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
4. **`notebook-plugin.md:257` names the deprecated series route.** The companion
   proposal fetches `GET /api/stellar/series`, which Portal serves with
   `Deprecation: true` (`portal/internal/expapi/server.go:314-328`) and which is not
   the canonical typed read. It should point at
   `GET /api/v2/stellar/runs/{run_id}/series` (§5.2). Raised by R14; left for a
   follow-up because this change is scoped to one file.

---

## 8. Risks

**Blocking (need a maintainer answer before implementation):**

1. ~~Which component owns metric input~~ — resolved: `taugrid-metrics-collector`.
2. **Scalar decode coverage.** `simple_value` is trivial; `tensor` requires dtype
   decoding. Which frameworks emit which is not established here.
3. **Append-only tail semantics.** `tfevents` files are written incrementally with
   flushes, unlike TauGrid's closed immutable chunks. §5.1 now defines the required
   behavior — stop at the last verified frame, retain the partial tail, retry it on
   the next pass — and the source/checkpoint layer must be written to that rule
   rather than reusing the newline reader. The remaining blocking question is
   whether the retained-tail retry is acceptable on the platform's PVCs for very
   long runs.

**Deferrable:**

- Non-scalar `tfevents` payloads (histograms, images) as first-class artifacts.
  Reachability depends on durable retention plus run-associated registration of the
  original event files, which the adapter does not provide by itself (§2.4, §9 Q6).
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
   boundary because it reuses the spool and queued delivery — but not the source
   layer: §5.1 shows the framing-aware reader and checkpoint advance are new code
   either way, so the placement decision is now about where that code lives and
   which module takes the protobuf dependency. R8 fixes the interface it must
   target: history-shaped scalar records into the typed projection, not canonical
   `MetricEvent` NDJSON and not an ADX payload.
3. **Is "make the metrics path broad" the right framing for R2**, or is a
   tracker UI still wanted as a separate, explicitly-scoped product surface?
4. **Should the local-mode question (§10.5) block this**, or run as its own design?
5. **Should `portal/README.md` be corrected** as part of this PR or separately?
6. **Will `tfevents` event files be durably retained and registered against the
   run?** Added by R9, and it changes what §8 and §10.2 can promise. Non-scalar
   payloads survive only under that condition; the adapter cannot create it, and
   artifact offload only uploads artifacts already registered for the run (§2.4,
   §6.1). If the answer is no for a given run, the dropped payloads are not
   recoverable and §4.2's revisit condition applies.
7. **Does the platform guarantee the logdir hierarchy the identity mapping needs?**
   Added by R11. §2.4.1 maps the trial directory to `run_group_id` and the run
   directory to `run_id`, which requires the configured `metrics.history` glob to
   preserve a run-relative path segment per trial. If Tune or another framework
   flattens the tree, or writes several trials into one directory, the mapping
   cannot keep trials distinct and the dedup view will merge them.

   **Decided: the adapter refuses rather than merges.** §2.4.1 now specifies the
   fail-closed rule — a read is rejected and no rows are projected when trial
   separation cannot be proven. That settles the question for implementation: it no
   longer blocks, because the design no longer depends on a platform guarantee it
   cannot verify. What remains is a documentation task rather than a blocker: a
   submission convention must provide a current-submission arm-to-directory
   ownership map. A declared multiplicity or a directory count is not sufficient,
   because stale directories can satisfy the count while current arms share one
   directory.

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
  slash-delimited tag vocabulary overlaps, so most tags need no renaming but the
  run/prefix mapping in §2.4.1 still applies.
- **Cons:** drops non-scalar payloads from the metric stream. They remain reachable
  as artifacts only where the original event files are durably retained and
  registered against the run (§2.4, §9 Q6); otherwise they are lost. Needs
  TFRecord/CRC32C/protobuf handling, the frame-aware checkpoint layer of §5.1,
  and truncated-tail tolerance.
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
should work without ADX, the decision is where canonical metric rows live locally
and who serves them. Recording it as a separate design keeps this proposal
reviewable.

---

## 11. What this design needs from reviewers

1. **Confirm the reframe** (§0, §3): house format first, `tfevents` as an adapter,
   TensorBoard service deferred. The narrow form of the compatibility claim is
   §2.4.3/§2.5, not "TensorBoard-compatible" (R13).
2. **Confirm the framing-aware source abstraction** (§5.1, R10). It withdraws the
   "source layer reused unchanged" claim; the source read and checkpoint advance are
   new code with truncated-tail and CRC-failure tests.
3. **Confirm the identity mapping** (§2.4.1, §2.4.2, R11): trial directory ->
   `run_group_id`, run directory -> `run_id`, and a run-scoped `metric_file_id` that
   survives rotation. This changes where `project` gets `metric_file_id`.
4. **Confirm the submission contract** (§3.2, R12), including the normalizer that
   turns the companion plugin's `{"step":..,"loss":..}` convention into the
   `_step`/`_timestamp` row the projection requires.
5. **Confirm the notebook read surface** (§5.2, R14): the canonical
   `GET /api/v2/stellar/runs/{run_id}/series` and its `points` response, replacing
   the legacy series route. §7.4 records the companion document's stale reference.
6. **Answer §9 Q3** — whether a tracker UI is wanted as a separate surface.
   (§9 Q1 is closed by §7.2.)
7. **Confirm §7.1 and §7.4** — whether the `expimport` README line and the
   `notebook-plugin.md` series route should be corrected in this PR or separately.
8. **Decide §9 Q4** — whether local-mode (§10.5) blocks D1 or proceeds separately.
9. **Answer §9 Q6** — whether `tfevents` event files will be durably retained and
   registered against the run, which is what makes §2.4's non-scalar fallback
   survivable.
10. ~~**Answer §9 Q7** — whether the logdir hierarchy that §2.4.1 depends on is
    guaranteed by the submission convention.~~ **Decided: §2.4.1 now requires the
    adapter to refuse when trial separation cannot be proven, so the design no
    longer depends on the guarantee.** The submission convention must provide a
    current-submission arm-to-directory ownership map; a trial-arm count alone is
    insufficient because retained directories can make the count look valid. The
    map is a documentation task, not a blocker.
