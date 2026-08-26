# Tau SDK and CLI code-change guide

This guide captures the review bar for the Tau SDK, CLI, expstore, importer,
and telemetry changes. Treat the CLI and Python SDK as user-facing contracts:
command names, flags, JSON shapes, checkpoint files, profile formats, storage
paths, and emitted metrics must remain stable unless the change explicitly
migrates them.

Treat the commands visible in `tau --help` as the primary contract. Hidden
compatibility roots that remain exist
only to keep existing scripts working; do not document them, add features through
them, or make new subprocess callers depend on them. The legacy root
`tau submit` has been removed: config-driven Job execution is owned directly by
`tau run`. Use the
[public object model](https://github.com/Azure/taugrid/blob/main/site/content/en/docs/developer-guide/concepts/object-model.md)
when changing command docs or wiring.

## Start with the contract

Every change should name the user path before naming files:

1. What will a researcher, platform engineer, or background worker run?
2. What local state is authoritative?
3. What remote system, if any, is only a projection or mirror?
4. What happens on retry, restart, partial writes, or repeated invocation?
5. Which outputs are stable contracts versus internal implementation details?

For direct-config versus decorator ownership, stability, and migration, start
from the [config-resolution guide](https://github.com/Azure/taugrid/blob/main/site/content/en/docs/developer-guide/concepts/config-resolution.md).
Keep Python focused on authoring and local workflows while Go owns Kubernetes
execution and lifecycle behavior.

Default training authoring is Ray Train. Tau should own policy-aware RayJob
submission into Kueue lanes, not a replacement trainer setup API. Treat
`@tau.train`, `@tau.eval`, `tau.serve(...)`, `tau python build`,
`tau python submit-build`, and `tau python submit` as stable first-class
Python authoring APIs for code-centric projects that expose reusable handles,
local/remote execution, dynamic handle construction, source staging, or the
supported one-train-to-one-eval checkpoint/result lineage. The repository-target
default remains a checked-in `tau.yaml` plus `tau run`; use SDK staging when a
single direct-config entrypoint is insufficient for the
module/wrapper/extra-script bundle. A `tau.python.build` directory is a
versioned generated artifact, not a second hand-authored config schema. It must
remain byte-stable for identical inputs, contain only relative staged paths and
secret locators, verify staged-file digests before replay, and terminate in Go
Tau for rendering and execution. `tau.serve(...)` creates a separately
submitted handle; build/submit do not orchestrate a train/eval/serve graph.
Do not add Python-owned Kubernetes execution or lifecycle behavior. The existing
train/eval orchestrator's direct RayJob polling and cleanup is tracked debt, not
a pattern to extend.

For local/offline experiment tracking, expstore is the local source of truth for
run packets, artifacts, checkpoints, and recovery state. Hosted scalar
dashboards may explicitly choose ADX/Kusto as their canonical metric source;
adx-mon remains the remote-write ingest path, not a dashboard query source. Do
not make fleet telemetry the only copy of non-scalar run state.

## Keep command files thin

Cobra command files should define the command surface and orchestrate work.
They should not become the home for reusable parsing, file handling, protocol,
or checkpoint logic.

Keep command construction together:

- Command literal, argument validation, output handling, and flag registration
  should stay in the command constructor.
- Follow the local file convention for flags instead of moving flags far away
  from the command they configure.
- If a command needs many options or result fields, move those structs into a
  focused sibling file or domain package.

Move work out of the CLI when the function does more than coordinate a few
steps. Prefer named stages such as read checkpoint, read chunk, import chunk,
project metrics, remote-write, and advance checkpoint. Result aggregation should
be explicit through small result structs or methods, not hidden in a long block
of counter increments.

## Put reusable behavior in reusable packages

Do not name generic helpers after the first command that needed them. If behavior
can apply outside one subcommand, place it under an `internal` package with a
capability name.

Current examples:

| Concern | Preferred home |
|---|---|
| Atomic byte writes, safe path names, short stable hashes | `taucore/fileutil` |
| Kusto query defaults and query shaping | `taucore/expkusto` |
| JSONL input expansion, complete-line tailing, history chunks, JSONL file checkpoints | `taugrid-portal/internal/jsonlutil` |
| Expstore schemas, storage, projections | `taugrid-portal/internal/expstore` |
| Importers and remote-write replay | `taugrid-portal/internal/expimport` |
| User-facing command wiring | each module's own `internal/cli` |

Stellar and the Portal live in `portal`, a separate module
and binary. `core` is the shared library both products link; keep
it minimal (see `core/AGENTS.md`).

Before adding a new helper, search for an existing package or upstream module in
`go.mod`. Use an existing upstream dependency when it is already present and
well-scoped. Do not add a new dependency for a small helper unless it replaces
enough code to justify the operational and review cost.

## Choose file and wire formats deliberately

Use JSON or JSONL for local Tau checkpoints, expstore projections, and CLI
outputs when the value is inspectability and recovery. Include schema versions
for durable files and preserve compatibility for existing checkpoint and
idempotency formats.

Consider protobuf only when there is a durable cross-process or cross-language
wire contract that benefits from generated types, explicit versioning, and
binary compatibility. Do not introduce protobuf just to organize local structs
or private checkpoint files.

For JSONL history and streaming-style inputs:

- Tail only complete newline-terminated records.
- Persist offsets only after the downstream step succeeds.
- Detect truncation, replacement, or prefix mismatch and safely restart from the
  beginning of the file.
- Write derived chunk files and checkpoints atomically.
- Keep chunk-level replay incremental; avoid full-store projection/replay on
  every watch tick.

## Make lifecycle and recovery explicit

Tau often runs inside Kubernetes Jobs, sidecars, or worker pods. New code must
say how it exits and how it resumes:

- Bounded modes need `--max-iterations`, completion sentinels, context
  cancellation, or another explicit stop condition.
- Watchers must not keep Kubernetes Jobs alive forever by default.
- Retries must be idempotent or checkpointed.
- Partial output files should not look complete.
- Local recovery should work without W&B SaaS, ADX, or any remote control plane.

## Preserve naming and telemetry contracts

Names that leave the process are contracts. Be conservative with:

- CLI command names and flags.
- JSON field names and schema versions.
- Metric names and label keys.
- Expstore file names and manifest fields.
- Kusto cluster, database, table, and query defaults.

For Tau metrics, keep the converged shape:

- Prometheus remote-write metric: `experiment_metrics`
- ADX database/table: `Metrics.ExperimentMetrics`
- Remote-write dashboard function: `ExperimentMetricsDashboardRows()`
- Local metrics spool: `TauExpMetrics.jsonl`
- Projection dashboard function: `TauExpMetricsDashboardRows()`
- Stellar run terminal marker: `metric_name="tau/run_status"` on
  `experiment_metrics`, with status details in JSON `tags`
- ADX lifecycle table for controller-owned run state: `Metrics.TauExpRunLifecycle`
- Target Kusto cluster: `https://example.kusto.windows.net`

Treat `TauExpMetrics.jsonl` and the `TauExpMetrics` projection table as
local/offline compatibility names, not the hosted fleet remote-write contract.

### Config-first Job and RayJob JSONL producer

A config-first `engine: job` or `engine: rayjob` workload can opt in to the hosted
scalar path without a manual backfill:

```yaml
metrics:
  history:
    - metrics-history-attempt-*/*.jsonl
  offload:
    enabled: true
```

Relative history paths resolve below `storage.output`; absolute paths must stay
under `/data`. History globs describe published metric chunks. On cache-backed
or object PVCs, every matching file must be closed and immutable before
publication. Write to an open/temp name excluded from the glob, flush as the
producer requires, close it, then atomically rename it to a unique final
`.jsonl` name. Never append to, truncate, or replace a published chunk. Retries
should publish new attempt-scoped chunks, for example
`metrics-history-attempt-<n>/*.jsonl`, and archives must remain outside the
active glob.

The BlobFuse/H200 reference producer writes temp names such as
`.<chunk>.jsonl.tmp-<pid>`, fsyncs and closes the temp file, then atomically
renames it to a final `.jsonl` name encoding monotonic first/last steps. Each
retry creates a new attempt directory and refuses to reuse a pre-existing one;
final names never collide. Prior run directories move beneath `runs/run-*`,
outside the active glob. The retained legacy `metrics-history.jsonl` is never
written.

Immutable chunks are recommended on all storage. Concurrent append tailing is
compatible only when the filesystem explicitly guarantees safe reader/writer
semantics. A legacy literal such as `metrics-history.jsonl` may remain in
`metrics.history` for discovery or fresh-session baselining, but new producers
must not append to it while Tau reads it.

Every non-empty row published to the direct online path must include numeric
W&B-style `_step` and `_timestamp` fields. `_step` must be an integer and
`_timestamp` must be a finite positive Unix epoch-seconds wall time:

```json
{"_step":42,"_timestamp":1770000000.125,"train/loss":1.25}
```

Tau rejects an invalid online chunk before import or remote write so a
successful sample count cannot hide rows that hosted Stellar cannot query.
Generic offline `taugrid-portal experiment import jsonl` remains configurable with
`--step-field` and `--time-field`; direct online producers use the fixed
W&B-style names.

The workload must have an explicit entrypoint, Ready TauWorkspace,
durable writable data PVC/output, and `experiment.project` plus
`experiment.name`. Project, experiment, and optional group values must be
lowercase expstore IDs. Tau propagates the exact experiment ID through the
workload and hosted metric rows. The platform supplies a pinned offloader image through
`TAU_METRICS_OFFLOAD_IMAGE`; endpoint, interval, and source may be supplied by
the corresponding `TAU_METRICS_OFFLOAD_*` environment values. Researcher YAML
cannot embed image, endpoint, credentials, or workspace policy.

Tau gives each fresh Kubernetes submission a metrics session and stores its
expstore and offload checkpoints beneath
`<storage.output>/.tau/metrics/<metrics-session-id>/`. Before the main
entrypoint starts, the sidecar checkpoints matching pre-existing history at its
current end and publishes a pod-local readiness file. This prevents a reused
`storage.output` or a trainer's startup archive step from exposing prior-run
rows to the new session. Source bytes and atomic checkpoint replacements are
flushed before offsets advance, so a retry on a different cache-backed PVC mount
cannot observe history without the checkpoint that already consumed it.

Automatic retries and `tau run resume` reuse the session ID and its durable
checkpoints; a new `tau run` invocation gets a new session even when the
config name and output path are unchanged. Resume rejects output path or PVC
drift because moving the same session would abandon its checkpoints. Retries
add uniquely named chunks without mutating already published history. Completed
scalar chunks are not replayed after sidecar restart.
Scope tags `tau_workspace`, `tau_namespace`, and, when known, `tau_cluster`
are protected and attached before remote write. `tau_retry_attempt` is
propagated when a Tau retry sets it. Terminal observations are checkpointed:
identical retries deduplicate, while a failed attempt followed by success emits
a newer `tau/run_status` marker that Stellar selects as final.
The wrapper emits succeeded/failed status on normal process exit and waits for
the offloader to acknowledge successful terminal publication. Missing
acknowledgement fails the workload instead of hiding a sidecar failure. Graceful
sidecar shutdown emits cancelled after one final drain. SIGKILL or node loss
remains best-effort and requires a future platform lifecycle recorder for a
complete guarantee.

Configs that do not set `metrics.offload.enabled: true` keep their existing
untracked Job/RayJob behavior.

For large artifacts on BlobFuse, Tau cannot make arbitrary application writes
safe. `storage.publish: staged` is the Tau-owned contract: the Job or Ray
driver writes closed regular files beneath `TAU_OUTPUT_STAGING_DIR` on local
`/mnt`. Tau copies each into
`storage.output/.tau-artifacts/<publication-id>`, verifies SHA-256 before and
after rename, and writes the generation-local `.tau-artifacts-complete` marker
last. Retries use a fresh generation, fencing stale pods without requiring a
portable BlobFuse lock. Directories are rejected because BlobFuse has no
portable atomic recursive rename. Ray workers do not receive the head-local
staging variable.

## Test at the right layer

Each code change should add tests where the behavior lives:

1. Utility package tests for reusable helpers.
2. Importer/exporter tests for file formats, idempotency, compatibility, and
   checkpoint behavior.
3. CLI tests for flags, output shape, command orchestration, and manifest
   rendering.
4. End-to-end cluster signoff for upstream runtime, telemetry, manifest, or
   workflow changes.

For Tau Go changes, run focused package tests while iterating and `go test ./...`
under `cli` before closeout when internals changed. For
taugrid upstream runtime changes, local tests are not enough: attempt the
smallest real-cluster path that exercises the changed behavior and save durable
evidence.

## Review checklist

Before asking for review, check:

- Is the command file mostly wiring, or did reusable behavior land in CLI?
- Did generic behavior get a generic package/name?
- Are structs grouped by ownership, not dumped into a large command file?
- Is the data format intentionally JSON/JSONL/protobuf, with compatibility
  stated?
- Did SDK subprocess or Stellar HTTP changes preserve the documented ownership,
  versioning, capability, and error/result contract?
- Are checkpoints advanced only after successful downstream work?
- Are file writes atomic where readers or restarts can observe them?
- Can a Kubernetes Job or sidecar exit cleanly?
- Are tests located at the same layer as the behavior?
- Did the change preserve expstore as local/offline truth for run packets,
  artifacts, and recovery state, or explicitly document a hosted scalar path
  that queries ADX/Kusto?
