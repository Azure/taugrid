# Direct run configs

Use this reference for hand-written `tau run --config` YAML, not SDK-generated
managed manifests. Query the installed contract before using an unfamiliar
field:

```bash
tau run schema -o json
tau run explain-config
tau run validate --config tau/train.yaml
```

Unknown keys are rejected. Validation includes dispatch and some local import
checks, but does not resolve profiles or prove files, images, storage, and
runtime dependencies work. Follow with client rendering, using an explicit
[snapshot](workload-profiles.md) when network access is not intended.

## Contents

- [Identity and source delivery](#identity-and-source-delivery)
- [Runtime and secrets](#runtime-and-secrets)
- [Compute and execution](#compute-and-execution)
- [Placement policy](#placement-policy)
- [Storage and checkpoints](#storage-and-checkpoints)
- [Retry, metrics, and profiling](#retry-metrics-and-profiling)
- [Worked examples](#worked-examples)
- [Common errors](#common-errors)

## Identity and source delivery

| Field | Contract |
| --- | --- |
| `name` | Stable workload name; an explicit-config positional argument can override it for a direct run |
| `engine` | `job` or `rayjob`; prefer explicit selection, especially for one-worker Ray jobs. `ray` is a compatibility alias |
| `entrypoint` / `script` | Local script resolved relative to the config; with `run.source`, a clean relative path inside the source image |
| `runtime.image` / `image` | Workload image, with `runtime.image` taking precedence; use a pinned tag or digest with the required dependencies |
| `run.source.image` / `.path` | Direct Job only: digest-pinned OCI source image and clean absolute directory copied to `/tau/source` by an init container |
| `run.working_dir` | RayJob only: local project directory packaged for Ray, relative to the config; entrypoint must be inside it |
| `run.working_dir_excludes` | Additional archive exclusions; do not package datasets, credentials, caches, or large checkpoints |
| `runtime.working_dir` | Direct Job only: absolute **container** working directory, not source delivery; incompatible with `run.source` and `run.working_dir` |
| `run.ttl_seconds_after_finished` | Direct Job only: 1–2147483647 seconds; explicit run value overrides workspace retention, otherwise the built-in fallback is 28800 seconds |

Nested `run.name`, `run.engine`, `run.entrypoint`, `run.script`, and
`run.image` also exist; avoid conflicting top-level/nested values.

For a script importing sibling modules, ship the project with Ray's
`run.working_dir`, or use a Job source image. Setting `runtime.working_dir`
does not transfer those modules. The source image must contain `/bin/sh`,
`cp`, and `chmod`; its digest identifies the tree being staged, not the runtime
image. Build/push only when requested.

`schema_version` without an explicit engine marks the managed-manifest path.
Do not add it to a direct config. `workflow.file` delegates to a separate
managed manifest; `workflow.extra_scripts`, `main_script`, and
`upstream_checkpoint` support that path. See [Python SDK](python-sdk.md).

## Runtime and secrets

| Field | Contract |
| --- | --- |
| `runtime.env` | Non-secret string map; one value is capped at 64 KiB and aggregate literal values at 128 KiB |
| `runtime.env_secret` | `NAME: "secret-name:key"` becomes `valueFrom.secretKeyRef`; client dry-run redacts the reference, not just a value |
| `runtime.env_kv` | Managed workflow only; direct Job/RayJob configs reject it |
| `runtime.pip` | Ray runtime-env packages; direct Jobs must bake dependencies into their image |
| `runtime.security.mode` | `restricted` applies Restricted Pod Security fields to generated containers/init containers; the image must support running non-root |

Key Vault-backed managed workflows require tenant/client identity, a pod
ServiceAccount, and one vault across all references; bare secret names also
need `--key-vault`. Do not invent Key Vault support on the direct path.

`MASTER_ADDR`, `MASTER_PORT`, `NCCL_*`, and Tau-managed `TAU_*` keys cannot
normally be supplied in `runtime.env`. Only the exact retry keys
`TAU_RESUME_FROM`, `TAU_RETRY_ATTEMPT`, `TAU_RETRY_MAX`, and
`TAU_RETRY_REASON` are allowed in the Tau namespace.
`execution.allow_nccl_override: true` permits only the NCCL overrides.

## Compute and execution

The selected profile owns cardinality. Explicit GPU/worker/node values must
match it; CPU/memory settings remain workload settings.

| Field | Job | RayJob |
| --- | --- | --- |
| `compute.gpus` | GPUs per pod; explicitly `0` for a CPU Job | Use `gpus_per_worker` |
| `compute.workers` | Rejected | Execution worker pods, excluding the CPU-only head |
| `compute.gpus_per_worker` | Rejected | GPUs per dedicated worker |
| `compute.cpu_request`, `cpu_limit`, `memory_request`, `memory_limit` | Main container | Per-pod defaults |
| `compute.head_cpu_*`, `head_memory_*`, `worker_cpu_*`, `worker_memory_*` | Rejected | Role-specific sizing |
| `execution.launcher` | `python` (default), `torchrun` | `ray-train` (default), `ray-tune` |
| `execution.nodes` | Indexed Job pod count for multi-node torchrun | Do not use for Ray worker count |
| `execution.processes_per_node` | torchrun process count, checked against GPUs per pod | Do not use for Ray Train sizing |

For multi-node DDP set `engine: job`, `launcher: torchrun`, node count, and
processes per node. Use a profile with the corresponding worker/GPU shape and
multi-node placement. Tau renders rendezvous metadata and a headless Service.
The image and platform still need compatible distributed-training libraries
and network access.

`execution.configs` is launcher-specific:

- `python`: script CLI arguments.
- `torchrun`: launcher options, excluding Tau-owned rendezvous/cardinality flags.
- `ray-train`: `torch_config`, `scaling_config`, and `failure_config`, excluding
  Tau-owned `num_workers` and `resources_per_worker`.
- `ray-tune`: search space; also provide `metric`. `mode` is `min`/`max`;
  `num_samples` and `max_concurrent_trials` control the search.

Do not treat `compute.cpu_workers` as direct CPU Job sizing; it is a managed
eval override. A CPU Job uses `compute.gpus: 0` and CPU/memory quantities.
Advanced GPU resource modes (`device-plugin`, `nvidia`, `dra`, `mig`) depend on
the renderer and the platform's matching resource contract; MIG requires
`compute.mig_profile`, and DRA requires the corresponding platform resources.

## Placement policy

Read [Workload profiles](workload-profiles.md) before resolving placement.

- `policy.profile` selects a ready TauCluster workload profile. Omit it only
  when one applicable ready profile can be selected unambiguously.
- `policy.namespace`, `team`, and `lane` scope profile applicability; they are
  not authorization grants. A connected workspace's namespace and queue must
  agree with the run.
- `policy.queue`, `mode`, `topology`, and priority-class settings, when
  explicit, must agree with the authoritative profile.
- `policy.gpu_class`, `shape`, `node_selector`, `clear_node_selector`, and
  `priority_tier` cannot override an authoritative profile. Some still appear
  in the schema for lower-level/compatibility paths; parsing is not permission
  to use them with profile-selected submission.
- `policy.workspace` is an explicit workspace selector, not a substitute for
  the checked-in connection or Kubernetes access.
- `policy.workload_profile_snapshot` is a config-relative path accepted only
  with `--dry-run=client`, requiring explicit namespace/team/lane.
- `policy.preset` is removed. Do not mechanically rename it and assume the old
  catalog name or shape exists on this cluster.

## Storage and checkpoints

| Field | Contract |
| --- | --- |
| `storage.data_pvc` | Existing platform-managed PVC mounted at `/data`; Tau does not create it |
| `storage.result_pvc` | Result-PVC alias; must match `data_pvc` if both are supplied |
| `storage.output` | Durable output file/directory; connected runs validate it against the workspace output root, including client dry-run |
| `storage.checkpoint` | Relative model artifact under the run checkpoint directory, e.g. `last.safetensors`; enables artifact indexing after success |
| `storage.volumes`, `mounts` | Extra direct Job volumes/mounts; RayJobs reject them |
| `storage.image_assets` | Direct Job only: `{name, image, source_path, mount_path}` entries copy digest-pinned image directories to read-only main-container mounts |
| `storage.publish` | Direct `staged` publication exposes pod-local `TAU_OUTPUT_STAGING_DIR`, verifies closed files into durable output, and writes a completion marker |

On a direct Job, `storage.data_pvc` cannot be combined with
`storage.volumes`; use a consistent volume/mount layout rather than both
forms. Validate and render any custom layout.

With a workspace and a durable mount, omitted direct-run output inherits
`<workspace output root>/<run name>`. Without workspace defaults, a PVC-backed
direct run uses `/data/checkpoints/workflows/<name>`. Use the resolved path
reported by Tau, not an assumed legacy finetune directory.

Resume requires application-written durable checkpoints and code that reads
`TAU_RESUME_FROM`. Declaring `storage.checkpoint` indexes an artifact; it does
not create model weights or implement checkpoint loading. Node-local scratch
does not survive replacement.

## Retry, metrics, and profiling

`resilience.max_retries` defaults to zero. When enabled, Tau waits for a
terminal state and can delete/resubmit with exponential backoff.
`retry_on` defaults to `Preempted` and `Evicted`; `OOMKilled` is opt-in and
`Unknown` is not retryable. Defaults are `backoff_initial: 30s`,
`backoff_max: 5m`. Set `checkpoint_path` to the actual durable directory, or
pass it with manual resume's `--from`.

`experiment.project`, `name`, and `group` describe discovery and comparison:
`name` is the experiment; `group` is an arm such as baseline/ablation. There
is no direct `experiment.question`. Prefer `name` over the old `title` alias.

For durable metrics set:

- `metrics.history`: published JSONL paths/globs; relative paths are beneath
  `storage.output`, absolute paths must be under `/data`.
- `metrics.offload.enabled: true` and a pinned `metrics.offload.image`.
- Optionally `metrics.offload.out`, under `/data` or `/var/run/tau`.

This sidecar supports single-pod direct Jobs and RayJob heads, **not multi-node
Indexed Jobs**. Rendering also needs a resolved workspace identity and writable
PVC output; a live connection normally supplies the workspace. An offline
review must supply `policy.workspace` explicitly as metadata, not as proof
that the workspace exists or authorizes the run.
Every row needs integer `_step` and finite positive
Unix-seconds `_timestamp`. Publish closed, uniquely named immutable chunks
before they match a glob, especially on object-backed PVCs. Platform
`TAU_METRICS_OFFLOAD_*` environment overrides take precedence over config.
Keep the original output path/PVC on a metrics-enabled resume so its
session checkpoints remain available.

`profiler.mode` supports `nsys`/`ncu` with engine-specific limits; direct Job
profiling needs durable output and cannot wrap `torchrun`. Check installed
help/reference for rank/warmup/duration support instead of assuming every
engine has the same profiler behavior.

## Worked examples

These examples use illustrative profiles `cpu`, `training-1gpu`, and
`training-2x8`. Replace the profile, pinned image, and PVC with the platform
handoff. The checker stages stub scripts and validates these configs. With an
explicit compatible **synthetic** snapshot, it also checks rendering, not
training or cluster readiness.
Paths below assume the script is next to the config.

### CPU evaluation

No special eval schema is needed for direct execution.

```yaml
name: evaluate
engine: job
entrypoint: evaluate.py
runtime:
  image: <pinned-image>
compute:
  gpus: 0
  cpu_request: "2"
  memory_request: 4Gi
policy:
  profile: cpu
storage:
  data_pvc: training-data
```

### Single-GPU training with secret references

```yaml
name: finetune
engine: job
entrypoint: train.py
runtime:
  image: <pinned-image>
  env:
    LOG_LEVEL: info
  env_secret:
    HF_TOKEN: "hf-credentials:token"
compute:
  gpus: 1
  memory_request: 64Gi
  memory_limit: 96Gi
policy:
  profile: training-1gpu
storage:
  data_pvc: training-data
  checkpoint: last.safetensors
```

### Two-node torchrun: 2 × 8 GPUs

```yaml
name: ddp-train
engine: job
entrypoint: train.py
runtime:
  image: <pinned-image>
compute:
  gpus: 8
execution:
  launcher: torchrun
  nodes: 2
  processes_per_node: 8
policy:
  profile: training-2x8
storage:
  data_pvc: training-data
```

### Ray Train: two execution workers plus a CPU-only head

```yaml
name: ray-train
engine: rayjob
entrypoint: train.py
run:
  working_dir: .
  working_dir_excludes: [".venv", "data", "checkpoints"]
runtime:
  image: <pinned-image>
compute:
  workers: 2
  gpus_per_worker: 8
policy:
  profile: training-2x8
storage:
  data_pvc: training-data
experiment:
  project: research
  name: training-comparison
  group: baseline
```

### Ray Tune on one GPU worker

```yaml
name: tune-lr
engine: rayjob
entrypoint: train.py
runtime:
  image: <pinned-image>
compute:
  workers: 1
  gpus_per_worker: 1
execution:
  launcher: ray-tune
  metric: eval/loss
  mode: min
  num_samples: 2
  max_concurrent_trials: 1
  configs:
    lr: [0.001, 0.0003]
policy:
  profile: training-1gpu
storage:
  data_pvc: training-data
```

### Job with immutable source and read-only image assets

```yaml
name: source-train
engine: job
entrypoint: train.py
run:
  source:
    image: <source-image-digest>
    path: /src
runtime:
  image: <pinned-image>
compute:
  gpus: 1
policy:
  profile: training-1gpu
storage:
  data_pvc: training-data
  image_assets:
    - name: model
      image: <asset-image-digest>
      source_path: /model
      mount_path: /models/base
```

### Single-pod Job with durable metric chunks

The script must actually publish `metrics/*.jsonl` under its output directory.
ADX identity/configuration is a separate platform prerequisite.

```yaml
name: measured-train
engine: job
entrypoint: train.py
runtime:
  image: <pinned-image>
compute:
  gpus: 1
policy:
  profile: training-1gpu
storage:
  data_pvc: training-data
experiment:
  project: research
  name: training-comparison
  group: baseline
metrics:
  history: ["metrics/*.jsonl"]
  offload:
    enabled: true
    image: <portal-image>
```

## Common errors

| Error | Response |
| --- | --- |
| Unknown `preset` or `eval` field | Migrate removed direct fields; use a real TauCluster profile or ordinary eval entrypoint |
| `conflicts with authoritative workload profile` | Compare explicit assertions with the selected profile; do not bypass policy |
| Missing/ambiguous/unready workload profiles | Platform checks TauCluster status, generation, hash, and applicability |
| Snapshot requires client dry-run or explicit scope | Supply namespace/team/lane for offline review; remove snapshot for connected validation/apply |
| `run.entrypoint` missing | Resolve from the config directory; verify the file or immutable source path |
| Local import not shipped | Package Ray project sources or use a Job source image |
| `runtime.env_kv` rejected | Use direct Secret references or the managed-workflow path |
| `storage.output` outside workspace root | Use the assigned workspace result scope |
| RayJob rejects mounts / Job rejects Ray fields | Choose the correct engine-specific fields; do not silence validation |
