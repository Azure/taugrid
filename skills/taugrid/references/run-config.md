# Run config reference

Field reference for **direct** `tau run --config` YAML. SDK-generated managed
manifests (`schema_version: 1`) are a different path and are not covered here.

The installed CLI is the final authority — this file mirrors it for quick
lookup, but regenerate when in doubt:

```bash
tau run schema -o json      # JSON Schema, implementation-generated
tau run explain-config      # same contract as a Markdown table
```

## Contents

- [Validation behavior](#validation-behavior)
- [Identity and entrypoint](#identity-and-entrypoint)
- [runtime](#runtime)
- [compute](#compute)
- [execution](#execution)
- [storage](#storage)
- [policy](#policy)
- [resilience](#resilience)
- [experiment](#experiment)
- [metrics](#metrics)
- [profiler](#profiler)
- [eval, task, workflow](#eval-task-and-workflow)
- [Error message → cause](#error-message--cause)
- [Worked examples](#worked-examples)

## Validation behavior

The parser rejects unknown fields. A typo surfaces as a parse error naming the
Go type, which tells you exactly which group it tried to unmarshal into:

```
field gpu not found in type runconfig.Compute
```

No field is globally required — validity depends on the engine and dispatch
path. `engine: ray` requires a name and an entrypoint; eval configs require
`engine: job`.

Validate offline (no cluster call), then render — both steps are needed:

```bash
tau run validate --config tau/train.yaml   # schema only
tau run train --dry-run=client             # renders locally; catches preset/GPU errors
tau run train --dry-run=server             # render + API-server dry-run, no admission
```

`validate` checks the schema and nothing else. Preset resolution and GPU-count
arithmetic run at render time, so these two pass `validate` and fail the
render:

| Config | `--dry-run=client` error |
|---|---|
| No `policy.preset` or `policy.profile` | `policy.profile or policy.preset is required` |
| `processes_per_node: 99` on an 8-GPU preset | `processes_per_node (99) exceeds profile GPU count (8)` |

Dry-run resolves `entrypoint` on disk unless `run.source` supplies an immutable
Job source tree.

## Identity and entrypoint

| Field | Notes |
|---|---|
| `name` | Stable run/workload name. A positional `tau run NAME` can supply it. Derives the default checkpoint path. |
| `engine` | `job` \| `ray`. Selects dispatch when Tau cannot infer it. Tau infers `ray` when `compute.workers > 1`, `gpus_per_worker != 1`, or `runtime.pip` is set. |
| `entrypoint` | Script path, resolved relative to the config file. With `run.source`, it is instead a clean relative path inside the staged source tree. Alias: `script`. |
| `image` | Image override when `runtime.image` is unset. |

RayJob names are validated against KubeRay's 47-character limit, which is
stricter than the Kubernetes 63-character limit.

`run.*` provides nested aliases for the same identity fields (`run.name`,
`run.engine`, `run.entrypoint`, `run.script`, `run.image`, `run.workload_kind`,
`run.main_script`, `run.smoke_pairs`, `run.source`, `run.working_dir`,
`run.working_dir_excludes`, `run.ttl_seconds_after_finished`). Prefer the
top-level form in new configs; the nested form exists for configs that group
everything under `run:`.

For direct Jobs, `run.source.image` names a digest-pinned OCI image and
`run.source.path` names the absolute directory to copy from that image. Tau uses
an init container and `emptyDir`, mounts a writable per-pod working copy at
`/tau/source`, and executes `entrypoint` from there without putting source bytes
in environment variables. The init container normalizes working-copy
permissions and sets the executable bit before a potentially non-root workload
container starts. The source image must provide `/bin/sh`, `cp`, and `chmod`.
`run.source` requires `engine: job` and cannot be combined with
`run.working_dir`.

Build and push the source image once, then reuse its immutable digest across
runs. Kubernetes pulls it with the workload's configured private-registry
authentication; configs must not contain registry credentials or signed
download URLs.

Build/push the source-only image once, resolve its immutable digest, and write
`<private-repository>@sha256:<digest>` into `run.source.image` for every run.
The source image should copy the checked-out tree to `run.source.path`.

`run.ttl_seconds_after_finished` accepts 1 through 2147483647 seconds for
completed/failed direct Job retention. Omission keeps the 28800-second default.
It does not apply until the Job reaches a terminal condition.

## runtime

| Field | Notes |
|---|---|
| `runtime.image` | Container image override. Pin a tag or digest. |
| `runtime.pip` | Packages installed through Ray `runtime_env` (Ray dispatch). Values are shell-quoted to prevent injection. |
| `runtime.env` | Literal, non-secret env vars. Values over 64 KiB or aggregate literal payloads over 128 KiB are rejected before workload creation; Tau-generated embedded payload entries are also capped at 64 KiB. Use `run.source` or `storage.image_assets` for content. |
| `runtime.env_secret` | `NAME: "secret-name:key"` → `valueFrom.secretKeyRef`. Client dry-run redacts name/key but shows the dependency exists. |
| `runtime.env_kv` | `NAME: "vault/secret"` or bare `"secret"` via Secrets Store CSI. All entries must resolve to one vault. Requires `--key-vault`, `--tenant-id`, `--workload-identity-client-id`, `--service-account`. |

**Reserved keys rejected in `runtime.env`:** `MASTER_ADDR`, `MASTER_PORT`, any
`NCCL_*`, and the whole `TAU_` namespace except the four retry keys below. Tau
sets these to match the rendered topology; overriding them silently breaks
rendezvous. Error:

```
runtime.env contains Tau-managed keys that cannot be overridden: MASTER_ADDR; remove them from runtime.env (settable TAU_ keys: TAU_RESUME_FROM, TAU_RETRY_ATTEMPT, TAU_RETRY_MAX, TAU_RETRY_REASON)
```

The only `TAU_` keys you can set are `TAU_RESUME_FROM`, `TAU_RETRY_ATTEMPT`,
`TAU_RETRY_MAX`, and `TAU_RETRY_REASON` — Tau injects them on retry and your
code reads them back, so they have to survive a round trip. Every other `TAU_`
key is Tau's, including ones added after your CLI shipped.

Spell them exactly. The namespace is matched case-insensitively, so
`tau_resume_from` is rejected rather than accepted as a separate variable —
environment variables are case-sensitive, and code reading `TAU_RESUME_FROM`
would otherwise find nothing on the retry path.

`execution.allow_nccl_override: true` unblocks only the `NCCL_*` keys. The
others stay blocked.

## compute

`compute` fields split by engine. **`engine: job` rejects the Ray-shaped
fields outright** — `workers`, `gpus_per_worker`, `runtime.pip`, and every
`head_*`/`worker_*` sizing field. For a Job, GPU count comes from the resolved
preset/profile, not from `compute`:

```
engine=job cannot set compute.gpus_per_worker=8; use engine: ray or remove compute.gpus_per_worker
engine=job cannot set compute.workers=2; use engine: ray or remove compute.workers
engine=job cannot set runtime.pip; use engine: ray or bake dependencies into the image
```

This catches people out because "8 GPUs per node" feels like a `compute`
concern. It is a placement concern: set `policy.preset` (or a profile) to the
node shape you want, and `execution.processes_per_node` is validated against
the GPU count that preset resolves to.

| Field | Default | Engine | Notes |
|---|---|---|---|
| `workers` | 1 | ray only | Ray worker pod count |
| `gpus_per_worker` | 1 | ray only | GPUs per Ray worker |
| `cpu_workers` | — | eval | CPU eval worker count |
| `workload_kind` | — | both | `job` \| `rayjob` \| `ray-train` \| `ray_train` |
| `gpu_resource_mode` | `device-plugin` | both | `device-plugin` \| `nvidia` \| `dra` \| `mig` |
| `mig_profile` | — | both | e.g. `1g.18gb`, `3g.71gb`. Required when `gpu_resource_mode: mig`. |
| `cpu_request` / `cpu_limit` | — | both | Job container, or per-pod default for Ray |
| `memory_request` / `memory_limit` | — | both | Same scoping |
| `head_cpu_*` / `head_memory_*` | — | ray only | Ray head pod |
| `worker_cpu_*` / `worker_memory_*` | — | ray only | Ray worker pods |

`device-plugin` (standard `nvidia.com/gpu`) is the default and works on any
cluster with the NVIDIA device plugin. `dra` requires ResourceClaimTemplates to
be configured.

## execution

Typed execution topology. `launcher` is **engine-scoped** and cross-engine
combinations are rejected at validation time.

| Field | Default | Notes |
|---|---|---|
| `launcher` | `python` (job) / `ray-train` (ray) | `python` \| `torchrun` \| `ray-train` \| `ray-tune` |
| `nodes` | 1 | Multi-node torchrun pod count. **`engine: job` only** — renders an Indexed Job. |
| `processes_per_node` | — | `torchrun --nproc_per_node`. Validated against resolved GPU count. |
| `configs` | — | Extra launcher config. job+python: script CLI flags. job+torchrun: torchrun flags. ray+ray-train: Ray Train config. ray+ray-tune: search space. |
| `metric` | — | Ray Tune optimization metric. Requires `launcher: ray-tune`. |
| `mode` | `min` | `min` \| `max` |
| `num_samples` | 1 | Tune sampled configurations |
| `max_concurrent_trials` | 1 | Concurrent Tune trials |
| `allow_nccl_override` | false | Opt-in bypass for `NCCL_*` reserved-key validation only |

Valid pairings:

| engine | launcher | scale with |
|---|---|---|
| `job` | `python` | single pod |
| `job` | `torchrun` | `execution.nodes` × `execution.processes_per_node` |
| `ray` | `ray-train` | `compute.workers` × `compute.gpus_per_worker` |
| `ray` | `ray-tune` | `compute.workers` + `execution.num_samples` |

## storage

| Field | Notes |
|---|---|
| `data_pvc` | Primary PVC mounted at `/data` |
| `output` | Durable output path advertised to the workload |
| `result_pvc` | Must match `data_pvc` when both are set |
| `mounts` / `volumes` | Additional mount/volume specs. **`engine: job` only.** |
| `publish` | `staged` — exposes `TAU_OUTPUT_STAGING_DIR` on pod-local `/mnt`, verifies closed regular files into `storage.output`, writes a completion marker |

Ray configs accept `data_pvc` and `output` but reject `mounts`/`volumes`:

```
ray run configs support storage.data_pvc/output, but not storage.volumes/mounts
```

Checkpoints must land on a durable `/data` mount for `tau run resume` to work.
Node-local scratch does not survive workload deletion.

## policy

Placement and accounting overrides. In a workspace-connected repository most of
these are supplied by workspace policy — set them explicitly only to override.

| Field | Notes |
|---|---|
| `workspace` | TauWorkspace name; loads namespace, queue, priority, output-root, scratch defaults |
| `namespace` | Target namespace |
| `queue` | Explicit Kueue LocalQueue. The literal value `auto` is currently rejected. |
| `preset` | Topology/policy preset, e.g. `azure.research.training.l` |
| `priority` / `priority_tier` | Tau priority tier (aliases) |
| `pod_priority_class` | Kubernetes PriorityClass override |
| `workload_priority_class` | Kueue WorkloadPriorityClass override |
| `disable_default_priorities` | Suppress Tau-managed default priority classes |
| `gpu_class` | Hardware class: `any`, `a100-80gb`, `h100-95gb`, or `h200-141gb`. Specific classes match `tau.azure.com/gpu-class` exactly; legacy NVLink/standalone spellings are deprecated aliases. |
| `topology` | Placement/interconnect: `independent`, `single-node-nvlink`, `multi-node-nccl`, or `elastic-workers` |
| `lane`, `mode`, `shape`, `team` | Admission and placement hints |
| `topology_policy` | Kueue topology policy override |
| `node_selector` | Additional node selector labels |
| `clear_node_selector` | Clear profile/topology selectors first. **`engine: job` native dispatch only** — managed workflow, eval, and Ray dispatch cannot clear them. |
| `profile` | Legacy profile name |

## resilience

| Field | Default | Notes |
|---|---|---|
| `max_retries` | 0 | 0 disables automatic retry |
| `retry_on` | `["Preempted","Evicted"]` | Allowed: `Preempted`, `Evicted`, `OOMKilled` |
| `backoff_initial` | `30s` | Go duration; doubles per attempt |
| `backoff_max` | `5m` | Backoff cap |
| `checkpoint_path` | `/data/checkpoints/finetunes/<name>` | Injected as `TAU_RESUME_FROM` |

Injected into each retry attempt: `TAU_RESUME_FROM`, `TAU_RETRY_ATTEMPT`,
`TAU_RETRY_MAX`, `TAU_RETRY_REASON`.

`OOMKilled` is excluded by default on purpose — identical resources reproduce
the identical OOM. Add it only after raising `compute` memory.

## experiment

| Field | Notes |
|---|---|
| `project` | Project name. Metrics offload requires a lowercase identifier: alphanumerics with internal `-`, `_`, or `.`. |
| `name` | Experiment identity — the set of runs being compared. |
| `title` | Deprecated pre-v0.1 spelling of `name`; only used to derive `name` when it is unset. |
| `group` | Run group; same identifier rules as `project` |

There is no `question` field. `question` is an expstore/Stellar concept, not a
run config one; the research question a set of runs answers is carried by
`experiment.name`.

When set, the renderer stamps `tau.azure.com/experiment` and injects
`TAU_EXPERIMENT`/`TAU_GROUP` into the runtime env.

## metrics

Opt-in durable Stellar metrics for **single-pod direct Jobs**.

| Field | Default | Notes |
|---|---|---|
| `history` | — | Published JSONL metric paths or globs |
| `offload.enabled` | false | Render the checkpointed metrics offloader sidecar |

Every online row needs an integer `_step` and a finite positive Unix-seconds
`_timestamp`. Cache-backed or object-PVC producers must close and atomically
rename unique immutable chunks before they match a glob — otherwise the
offloader may read a partially written file. Relative paths resolve beneath
`storage.output`; absolute paths must be under `/data`.

## profiler

| Field | Default | Notes |
|---|---|---|
| `mode` | — | `nsys` \| `ncu` |
| `rank` | `0` | `0`, `0,8`, or `all` |
| `warmup` | — | Warmup before capture |
| `duration` | — | Capture duration |

Profiling requires a data PVC for artifact persistence and cannot be combined
with `launcher: torchrun` (torchrun manages its own worker processes).

## eval and workflow

There is no eval command, `task` field, or `eval.harness`/`eval.model` config.
An evaluation is an ordinary run whose image evaluates instead of trains: point
`script`/`image` at your eval entrypoint, pass the checkpoint and results paths
through `runtime.env`, and set `storage.output` to the results path so
`tau run get` can retrieve it. Put lm-evaluation-harness, promptflow, or your
own metrics code in the image; Tau never builds command lines for third-party
evaluation tools.

Configs that still set `task: eval`, `eval.harness`, or `eval.model` are
rejected with migration guidance.

`workflow.*` delegates rendering to a separate managed workflow manifest:
`file`, `main_script`, `script`, `extra_scripts`, `secret_payload`,
`upstream_checkpoint`, `smoke_pairs`, `workload_kind`. Use this only when Tau
must own workflow semantics such as staged train/eval lineage.

`schema_version` is **reserved** — its presence marks an SDK-generated managed
manifest, which is not this contract. Filename does not distinguish the two;
the schema does.

## Error message → cause

| Message | Cause | Fix |
|---|---|---|
| `field X not found in type runconfig.Y` | Unknown field (typo) | Check the group's field list |
| `engine=job cannot set compute.gpus_per_worker=N` | Ray-shaped field on a Job | Set `policy.preset` to the node GPU shape |
| `engine=job cannot set compute.workers=N` | Ray-shaped field on a Job | Use `execution.nodes` for multi-node torchrun |
| `engine=job cannot set runtime.pip` | Ray-shaped field on a Job | Bake dependencies into the image |
| `execution.nodes is for engine: job; use compute.workers for Ray pod count` | `nodes` with `engine: ray` | Use `compute.workers` |
| `execution.launcher torchrun is for engine: job; Ray Train manages distributed init via TorchConfig` | Cross-engine launcher | Switch engine, or drop `torchrun` |
| `ray run configs support storage.data_pvc/output, but not storage.volumes/mounts` | Extra mounts on Ray | Use `engine: job`, or drop the mounts |
| `runtime.env contains Tau-managed keys that cannot be overridden: …` | Reserved env key | Remove it; Tau sets it |
| `engine=ray requires run.entrypoint` | Missing entrypoint | Set `entrypoint` |
| `ray runs require NAME` | No run name | Set `name`, or pass positionally |
| `eval run configs require engine: job or workload_kind: job` | Eval with Ray | Set `engine: job` |
| `storage.result_pvc cannot differ from storage.data_pvc for Ray run configs` | Mismatched PVCs | Make them equal, or drop `result_pvc` |

## Worked examples

### Single-pod GPU job

`engine: job` rejects `compute.gpus_per_worker` — GPU count comes from the
resolved preset/profile. `compute` still owns CPU and memory sizing.

```yaml
name: finetune-7b
engine: job
entrypoint: train.py
runtime:
  image: <pinned-image>
policy:
  preset: azure.research.training.l    # GPU shape lives here
compute:
  memory_request: 64Gi
  memory_limit: 96Gi
storage:
  data_pvc: training-data
  output: /data/checkpoints/finetune-7b
```

### Multi-node torchrun DDP (16 GPUs = 2 nodes × 8)

Per-node GPU count comes from the **preset**, not `compute` — `engine: job`
rejects `compute.gpus_per_worker`. `processes_per_node` is validated against the
GPU count the preset resolves to, so the two must agree.

```yaml
name: ddp-train
engine: job
entrypoint: train.py
runtime:
  image: <pinned-image>
execution:
  launcher: torchrun
  nodes: 2
  processes_per_node: 8
policy:
  preset: azure.research.training.xl   # 8 full GPUs per node
storage:
  data_pvc: training-data
  output: /data/checkpoints/ddp-train
```

Renders an Indexed Job with `completions: 2`, `parallelism: 2`,
`nvidia.com/gpu: 8` per pod, and a c10d rendezvous over a headless Service.

### Ray Train with retry

```yaml
name: ray-train
engine: ray
entrypoint: train.py
runtime:
  image: <pinned-image>
  pip: ["transformers==4.44.0"]
compute:
  workers: 4
  gpus_per_worker: 8
storage:
  data_pvc: training-data
  output: /data/checkpoints/ray-train
resilience:
  max_retries: 3
  retry_on: ["Preempted", "Evicted"]
experiment:
  project: my-project
  group: baseline
```

### Ray Tune sweep

```yaml
name: tune-lr
engine: ray
entrypoint: train.py
runtime:
  image: <pinned-image>
compute:
  workers: 4
  gpus_per_worker: 1
execution:
  launcher: ray-tune
  metric: eval/loss
  mode: min
  num_samples: 8
  max_concurrent_trials: 4
  configs:
    lr: [0.001, 0.0003, 0.0001]
storage:
  data_pvc: training-data
```

### Secret-backed environment

```yaml
name: train-with-secrets
engine: job
entrypoint: train.py
compute:
  gpus: 1
runtime:
  image: <pinned-image>
  env:
    LOG_LEVEL: info
  env_secret:
    HF_TOKEN: "hf-credentials:token"
storage:
  data_pvc: training-data
```
