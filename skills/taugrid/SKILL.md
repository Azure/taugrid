---
name: taugrid
description: "Run GPU and AI workloads on Kubernetes with TauGrid — the tau CLI plus Kueue queueing, KubeRay/Ray orchestration, GPU node health, and observability. Use for distributed training, fine-tuning, hyperparameter sweeps, model serving/inference, datasets, run lifecycle, and cluster/workspace onboarding. Trigger on plain-language intent even with no product name: 'run my training script on the cluster', 'fine-tune a 7B model', 'my GPU job is stuck/pending/preempted/OOMKilled', 'deploy this model for inference', 'set up a queue for my team'. Also trigger on tau.yaml, run config, Kueue admission or quota, LocalQueue/ClusterQueue, RayJob, torchrun, Ray Train/Tune, TauWorkspace, taugrid-portal, or Stellar. Use it BEFORE writing YAML destined for tau run, and before suggesting kubectl for a Tau-managed workload."
---

# TauGrid and the tau CLI

TauGrid makes it easier to run GPU workloads on Kubernetes — data preparation,
distributed training, fine-tuning, and inference. It brings together the `tau`
CLI, workload queueing and admission with **Kueue**, Ray cluster orchestration
with **KubeRay**, node-level **GPU health monitoring**, and cluster/workload
**observability** into one stack, so platform teams get an integrated
foundation and researchers can stay in their code instead of Kubernetes
plumbing.

`tau` itself is a CLI, renderer, and observer — **not** a scheduler, operator,
or cloud provisioner. It turns checked-in workload intent into Kubernetes Jobs
or KubeRay RayJobs, submits them through Kueue, and gives one lifecycle surface
for the result.

Getting that boundary right is what separates useful help from confidently
wrong help. When a job is stuck, the answer is usually "Kueue has no quota" or
"the workspace is Degraded" — not something you can fix by editing YAML.

## Who is asking

Adapt to the person, since the same symptom has different owners:

- **Researcher** — authoring `tau.yaml`, submitting runs, reading status/logs,
  retrying, serving a model, pulling experiment evidence. They cannot fix
  quota, node health, or workspace policy; tell them who can.
- **Platform operator** — preparing clusters, bootstrapping workspaces,
  queues, storage, and node health. See
  [references/platform.md](references/platform.md).
- **Contributor** — changing this repository's Go code. See
  [Repository layout](#repository-layout-for-contributors) at the end.

Most users have only the `tau` binary, not this source tree. Answer from
`tau --help`, `tau run schema`, and this skill rather than pointing them at
repository paths they do not have.

## Ground rules that prevent most mistakes

**Verify against the installed binary, not memory.** The CLI surface changed in
v0.5 and binaries in the wild are often stale:

```bash
tau --help                    # actual root commands
tau run schema -o json        # authoritative config schema
tau run explain-config        # field reference with statuses
```

If a command you expect is missing, the binary is old — say so rather than
writing instructions against a surface the user does not have. They upgrade on
Linux or macOS with
`curl -fsSL https://github.com/Azure/taugrid/releases/latest/download/install.sh | sh`;
the installer verifies the matching binary against `SHA256SUMS`.

**`tau` has exactly seven roots:** `cluster`, `workspace`, `run`, `serve`,
`data`, `python`, `version`. Experiment tracking and the observability portal
live in a **separate binary**, `taugrid-portal` — if a user still has
`tau experiment` or `tau portal`, they are on a pre-split build. Pre-v0.5 flat
roots (`submit`, `finetune`, `status`, `logs`, `ray`, `exp`, `queue`, `model`,
`dataset`, …) are deleted, not deprecated.

**`tau run TARGET` is a positional argument, not a subcommand.** The single
easiest thing to get wrong, since it reads identically to a real subcommand:

- `tau run train` → `run` root with `TARGET=train`, resolving `tau/train.yaml`
- `tau run status` → the real `status` subcommand

Subcommands are exactly: `validate`, `schema`, `explain-config`, `list`,
`status`, `logs`, `get`, `cancel`, `resume`, `history`. Anything else is a
target name — which is why `tau run retry` appears to "work" while doing
nothing useful. There is no retry subcommand; retry is config behavior.

**Prefer `tau` over raw `kubectl` for Tau-managed workloads.**
`tau run status` already merges Job/RayJob state, Kueue admission, the startup
phase tree, and pods into one ordered view. Reach for `kubectl` only to confirm
something `tau run status` has already pointed at, and say why you're doing it.

## Authoring a run config

The normal contract is a checked-in **direct run config** — hand-written YAML
that `tau run --config` reads. Minimal shape:

```yaml
name: train                 # run name; also derives the default checkpoint path
engine: rayjob              # job | rayjob
entrypoint: train.py        # resolved relative to the config file

runtime:
  image: <pinned-image>     # pin a tag or digest; never :latest

compute:
  workers: 2                # Ray worker pods
  gpus_per_worker: 8

storage:
  data_pvc: training-data   # mounted at /data
  output: /data/checkpoints/workflows/train
```

Top-level groups: `name`, `engine`, `entrypoint`, `script`, `image`,
`schema_version`, `runtime`, `compute`, `execution`, `policy`, `storage`,
`resilience`, `profiler`, `experiment`, `metrics`, `run`, `workflow`. There is
no `eval` group — evaluation is an ordinary run. Full field list in
[references/run-config.md](references/run-config.md); `tau run schema -o json`
is the final authority since it is generated from the implementation.

**The schema is strict — unknown fields are hard errors.** A typo like
`compute.gpu` fails with `field gpu not found in type runconfig.Compute`.

**`validate` alone is not enough — always follow it with a client dry-run.**
`validate` is offline schema checking; preset resolution and GPU-count
arithmetic happen at *render* time, so a config can validate clean and still be
unrunnable:

```bash
tau run validate --config tau/train.yaml   # schema only
tau run train --dry-run=client             # renders; catches the rest
```

Two failures `validate` will pass:

| Config | `validate` | `--dry-run=client` |
|---|---|---|
| No `policy.preset` or `policy.profile` | is valid | `policy.profile or policy.preset is required` |
| `processes_per_node: 99` on an 8-GPU preset | is valid | `processes_per_node (99) exceeds profile GPU count (8)` |

Both are free and offline, so run both before telling a user a config is good.
Note that dry-run resolves `entrypoint` on disk, so run it from the config's
directory or the script will appear missing.

`--config` takes an explicit path; a bare `TARGET` resolves `tau/TARGET.yaml`.
They are not interchangeable.

### Engine choice constrains everything else

`engine: job` renders a `batch/v1` Job — one pod, or an Indexed Job with
torchrun. `engine: rayjob` renders a KubeRay RayJob (head + `compute.workers`
workers). Mixing the two vocabularies is the most common authoring failure:

| Intent | Correct | Common mistake |
|---|---|---|
| Multi-node PyTorch DDP | `engine: job` + `launcher: torchrun` + `execution.nodes: N` | `execution.nodes` with `engine: rayjob` |
| Ray Train distributed | `engine: rayjob` + `compute.workers: N` | `launcher: torchrun` with `engine: rayjob` |
| GPUs per pod on a Job | `policy.preset` (the node shape) | `compute.gpus_per_worker` with `engine: job` |
| Extra PVC mounts | `engine: job` + `storage.mounts` | `storage.mounts` with `engine: rayjob` |

`compute`'s Ray-shaped fields — `workers`, `gpus_per_worker`, `runtime.pip`,
`head_*`/`worker_*` — are **rejected on `engine: job`**. For a Job, GPU count
comes from the resolved preset, and `execution.processes_per_node` is validated
against it. "8 GPUs per node" feels like a `compute` concern but is a placement
one. `execution.launcher` is engine-scoped: `job` takes `python`/`torchrun`,
`ray` takes `ray-train`/`ray-tune`.

Tau owns the distributed-training env vars. `MASTER_ADDR`, `MASTER_PORT`,
`TAU_WORLD_SIZE`, `TAU_DIST_BACKEND`, `TAU_NUM_WORKERS`, and `NCCL_*` are
rejected in `runtime.env` so rendezvous stays consistent with the rendered
topology. `execution.allow_nccl_override: true` unblocks only the `NCCL_*`
ones, for deliberate tuning.

### Secrets

Never put a secret value in a run config:

- `runtime.env_secret` — `KEY: "secret-name:key"` → `valueFrom.secretKeyRef`.
- `runtime.env_kv` — Azure Key Vault via Secrets Store CSI; all entries must
  resolve to one vault.

Client dry-run redacts both while keeping the dependency shape visible.

## Running and observing

```bash
tau run train                      # submit the checked-in target
tau run status <run-name> --watch  # startup phase tree, live
tau run logs <run-name>            # Ray driver output, or Job pod logs
tau run get <run-name>             # durable results recorded by storage.output
tau run cancel <run-name>          # delete workload; Kueue reclaims quota
tau run list -n <namespace>        # Tau-managed Jobs and RayJobs
```

On a repository's first cluster-backed `tau run`, Tau resolves credentials
through the descriptor's access method: it either isolates an existing
kubeconfig context or obtains AKS credentials with the user's Azure identity.
The dedicated kubeconfig avoids mutating their main one; it is **not** a
researcher-isolation boundary and should not be described as one.

`tau run status` is the canonical lifecycle view. It walks an ordered phase
tree — Submitted, Kueue admission, (RayCluster), pod scheduling, DRA
allocation, image pull, init containers, container start, Ready, (RayJob
status) — where each phase reports pending, active, done, warning, or skipped.

**Read it top to bottom and stop at the first phase that is not `done`.** That
phase is the layer to investigate; everything after it is downstream noise.

Three distinctions that matter when interpreting it:

- **Admitted ≠ scheduled.** Kueue reserved quota; no pod exists yet.
- **Running ≠ progressing.** Containers started; the training loop may be hung.
- **Completed ≠ evidence preserved.** Check that artifacts actually landed.

A `skipped` DRA phase is normal when the workload requests GPUs through the
device plugin rather than DRA. It is not a failure.

## Diagnosing a stuck or failed run

Work the layers in order. Jumping to `kubectl describe pod` when the real
problem is quota (layer 4) or a Degraded workspace (layer 2) wastes time and
produces a misdiagnosis.

| # | Layer | Command | Owner if it fails |
|---|---|---|---|
| 1 | Repo/connection resolution and access | `tau workspace connection` (`--offline` for local configuration only) | Researcher (descriptor) / platform (access) |
| 2 | TauWorkspace readiness | `tau workspace status <name>` | Platform operator |
| 3 | Config validation and render | `tau run validate --config <path>` | Researcher |
| 4 | Kueue admission and quota | `tau run status <run>` (admission phase) | Queue owner |
| 5 | Scheduling, DRA, image pull, init | `tau run status <run> --watch` | Platform or researcher |
| 6 | GPU/node/topology health | `tau cluster validate nodes` / `... topology` | Node-pool operator |
| 7 | Runtime progress and evidence | `tau run logs <run>` + `taugrid-portal experiment status <name>` | Researcher / platform |
| 8 | Recovery | see retry/resume below | — |

Layers 1–3 are offline and cost nothing, so run them first when a symptom is
ambiguous. Full per-layer detail, including the operator-only `kubectl`
commands for each, is in
[references/troubleshooting.md](references/troubleshooting.md).

Two guardrails worth stating to users directly:

- Do not add namespace, queue, kubeconfig, or cloud credentials to a project
  config to work around a platform readiness failure. Those are workspace
  concerns, the submission gate is enforced server-side, and there is no
  client-side bypass.
- Do not resubmit blindly after a failure. Locate the first failed layer, then
  choose retry or resume deliberately.

## Retry and resume

Automatic retry is configuration, not a command:

```yaml
resilience:
  max_retries: 2
  retry_on: ["Preempted", "Evicted"]   # default; OOMKilled is opt-in
  backoff_initial: 30s
  backoff_max: 5m
  checkpoint_path: /data/checkpoints/finetunes/<name>
```

When `max_retries > 0` and no dry-run is set, `tau run` waits for terminal
state, classifies the failure, checks it against `retry_on`, applies bounded
exponential backoff, injects the checkpoint path and attempt number, then
deletes and resubmits. If the reason is not in `retry_on`, Tau exits with an
error naming it — unexpected failures surface instead of looping.

`OOMKilled` is deliberately excluded by default: the same config with the same
memory usually reproduces the same OOM, so retrying without changing anything
just spends queue time to fail identically. Suggest adding it only after
`compute` memory has been raised. `Unknown` is never retryable in either path.

Manual resume:

```bash
tau run resume <run-name> --config tau/train.yaml   # --config is required
```

Resume needs a durable checkpoint under `/data` — a workload that only wrote to
node-local scratch has nothing to resume from, since that state does not
survive workload deletion. If the original failure was `OOMKilled`, resume
requires `--force`.

## Serving a model

`tau serve` renders a KubeRay RayService (default) or a plain Deployment. A
service is not a run — it does not inherit run lifecycle commands.

```bash
tau serve deploy <name> --kind=rayservice --profile <p> --image <pinned> \
  --import-path serve:app --checkpoint <path> --checkpoint-pvc <pvc> \
  --namespace <ns> --context <ctx> --dry-run=client
tau serve status <name> --kind=rayservice -n <ns>
tau serve scale  <name> --kind=deployment --replicas 3 -n <ns>
tau serve delete <name> --kind=rayservice -n <ns>
```

`--checkpoint` mounts the PVC at `/data`, resolves relative paths under
`/data/checkpoints`, and sets `TAU_MODEL_PATH`; the app still owns loading.
Direct `scale` works only for `--kind=deployment` — a RayService's Serve config
is a serialized field, so redeploy or set `--min-replicas`/`--max-replicas` at
creation. `--from-finetune`/`--from-model` read cluster metadata, so they
cannot use client dry-run. Serving does not activate
`tau/workspace.connection.yaml` — pass namespace and context explicitly.

## Datasets and models

```bash
tau data dataset list|show|ref|verify     # curated dataset registry
tau data model list|show|best|alias       # durable checkpoints and aliases
```

The registry is a catalog, not a data plane: records point at storage accessed
by workload identity, and are immutable once registered — only aliases move.
Reference a dataset from a run through its resolved path on the mounted PVC.

## Evidence: experiments and artifacts

Run evidence lives in the **`taugrid-portal`** binary, not `tau`:

```bash
taugrid-portal experiment search              # find indexed runs
taugrid-portal experiment stellar <run-name>  # local dashboard (-o html|json|tui)
taugrid-portal experiment open <run-name>     # serve and open in a browser
taugrid-portal experiment status <name>       # durable lifecycle record
taugrid-portal portal serve                   # unified observability portal
```

The local expstore is the authoritative packet; ADX/Kusto and the hosted
Stellar UI are optional scalar projections. So "the dashboard is empty" is a
projection problem, not necessarily lost data.

Add discovery metadata so runs group correctly:

```yaml
name: <run-name>
script: train.py
compute:
  gpus: 0
experiment:
  project: <project>
  name: <the set of runs being compared>
  group: <named subset>
```

`experiment` has no `question` field — that is an expstore/Stellar concept.
The research question a set of runs answers is carried by `experiment.name`.

## Platform operator work

Operator commands need cluster-admin-level access and are deliberately not
researcher-facing. `tau cluster validate nodes` creates privileged pods.

```bash
tau cluster install --version <version> --values <file>
tau cluster validate nodes --gpu-class <c> --min-healthy <n>
tau cluster validate topology --preset <preset>     # or --cluster-queue (default taugrid-cq)
tau cluster uninstall --yes                         # Helm-owned resources only

tau workspace check <name>                          # exits non-zero unless Ready
tau workspace status <name> -o json
```

`cluster install` and `cluster uninstall` are Helm-only. Platform owners create
`TauWorkspace` resources through reviewed Helm/GitOps/IaC. TauGrid 0.1 does not
create or mutate StorageClasses, PVCs, PVs, CSI configuration, cloud storage,
credentials, or Secrets.

Workspace `Ready` requires `RBACReady` and `QueueReady` true with no drift.
TauGrid 0.1 has no `StorageReady` condition on `TauWorkspace` or `TauCluster`,
so **a `Ready` workspace can still have a missing PVC**. Verify the
platform-managed claim before handing off. `WorkloadIdentityReady` is also
diagnostic only. Degraded-condition recovery and the install/uninstall contract
are in [references/platform.md](references/platform.md).

## Boundaries — what to tell users Tau will not do

Tau owns resolution, validation, rendering, submission, and lifecycle. It does
**not** own:

- Azure/AKS provisioning, node pools, or cloud RBAC
- Kubernetes scheduling, or Kueue quota and admission decisions
- Ray Train, PyTorch, or model-framework behavior
- Project data preparation, model code, or serving application semantics

When a request falls outside these — "make Tau create my GPU node pool", "have
Tau raise my quota" — name the system that actually owns it and who to ask.
Guessing a plausible-sounding flag is worse than saying so.

## Contributing to this repository

Module layout, CI guards, the `tau.azure.com/*` label contract, and how to
verify a change: [references/contributing.md](references/contributing.md).
Users of the `tau` binary do not need it.
