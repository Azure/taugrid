---
name: taugrid
description: "Author, validate, run, and troubleshoot AI workloads with TauGrid's tau CLI, optional Python SDK, and taugrid-portal. Use for Tau run configs, workspace connections, TauCluster workload profiles, Job/RayJob training and evaluation, model serving, datasets, logs, and experiment evidence. Use before generating YAML for tau run or diagnosing a Tau-managed workload. Generic Kubernetes, Ray, or cloud provisioning requests without a TauGrid connection are outside this skill."
---

# TauGrid

Use `tau` to combine project-owned workload intent with platform-owned
workspace and workload-profile policy. It renders Jobs, RayJobs, RayServices,
or Deployments; Kubernetes, Kueue, and KubeRay own scheduling and reconciliation.
The optional Python SDK owns decorator authoring. `taugrid-portal` is a
separate binary for experiments and the observability portal.

## Start with the installed tools

```bash
tau version
tau --help
tau run schema -o json
tau run explain-config
```

Treat the installed command help and generated schema as authority. Do not
infer a feature from a release number or silently upgrade the user's tools.
The application roots are `cluster`, `workspace`, `run`, `logs`, `serve`,
`data`, `python`, and `version`; shell completion/help are also available.
`tau logs` is supported, while old flat commands such as `tau submit`,
`tau finetune`, `tau status`, and `tau ray` are not the current interface.

Most users have the binaries, not this repository. Give them commands and the
bundled references; use source paths only for contributor work.

Read the reference matching the task, not all of them:

| Task | Reference |
| --- | --- |
| Author YAML, choose an engine, size GPUs, package source, persist output | [Run config](references/run-config.md) |
| Select a profile or render without cluster access | [Workload profiles](references/workload-profiles.md) |
| Serve an image/checkpoint, use literal command arguments, scale an endpoint | [Serving](references/serving.md) |
| Connect, install, create/adopt a workspace, check PVCs or quota | [Platform](references/platform.md) |
| Diagnose startup, historical logs, retries, or missing evidence | [Troubleshooting](references/troubleshooting.md) |
| Use decorators, inspect/build Python workflows, or chain train/eval | [Python SDK](references/python-sdk.md) |
| Change the repository or maintain this skill | [Contributing](references/contributing.md) |

## Establish scope and side effects

Identify the project, connection, workload namespace, and user's role before
cluster operations. A researcher can fix their config or application; changing
queue policy, RBAC, profiles, or node health belongs to the platform owner.
Do not treat a diagnostic request as permission to submit, retry, deploy,
delete, install, or create privileged validation pods.

| Operation | What it actually does |
| --- | --- |
| `tau run validate --config <file>` | Offline parsing, dispatch checks, and some local entrypoint/import checks; no profile or cluster readiness proof |
| `tau run --config <file> --dry-run=client` | Normally contacts the cluster to resolve the workspace and ready profile; renders without applying the workload |
| Client dry-run with `policy.workload_profile_snapshot` | Offline rendering with explicit namespace/team/lane scope; not live authorization or capacity validation |
| `--dry-run=server` | Connected preflight and Kubernetes API-server dry-run; not a scheduling or execution test |
| `tau workspace connection` | Reviews/trusts the repository connection, resolves credentials, contacts Kubernetes, and saves isolated local connection state |
| `tau workspace create` / `adopt` without `--apply` | Connected read-only preflight and manifest preview |
| `tau cluster validate nodes` | Creates privileged validation pods; requires authorization |

`tau workspace connection` has no `--offline` flag. First-time repository trust
requires an interactive terminal before credentials or the cluster are used.
If headless execution asks for that review, ask the user to complete it; do not
edit trust records, read credential stores, or bypass the check.

## Author and render a direct run

Prefer a checked-in direct config for ordinary training, evaluation, or batch
work. In this example `training-1gpu` is an illustrative profile name: replace
it with a ready, applicable profile supplied by the platform.

```yaml
name: train
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
```

The image must contain the required runtime; the PVC must already exist.
With a connected workspace and a durable mount, omitted `storage.output`
inherits the workspace output root plus the run name. Do not copy another
workspace's output path.

```bash
tau run validate --config tau/train.yaml
tau run train --dry-run=client
```

Validate **and** render before claiming a config is ready for submission.
Validation does not resolve live profiles; rendering catches profile/cardinality
conflicts and missing files. To keep the second step offline, use the explicit
snapshot procedure in [Workload profiles](references/workload-profiles.md).

Path rules matter:

- `tau run train` resolves the **target** `tau/train.yaml`; `train` is not a
  subcommand. `tau run status <name>` is a real lifecycle subcommand.
- `tau run --config path/to/config.yaml` selects a file. With explicit
  `--config`, a positional argument is a direct-run name override.
- `tau run validate train` does **not** discover `tau/train.yaml`; use
  `--config` with validation.
- Relative script, snapshot, and project-directory paths resolve from the
  config file, not the shell's current directory. A root `train.py` referenced
  from `tau/train.yaml` needs `entrypoint: ../train.py`.
- A monorepo can select a catalog project with `tau run ... --project <name>`.
  Resolve ambiguity explicitly, including for lifecycle commands.

## Respect the workload profile

`policy.preset`, local `TopologyPolicy` catalogs, and `--profiles-dir` are
removed. Use `policy.profile`, or let Tau select the unique ready, applicable
profile from `TauCluster/cluster`. Ambiguous or stale status is an error.

The profile owns queue bindings, placement, priority classes, worker count,
and GPUs per worker. Explicit sizing is an **assertion**, not permission to
override it:

| Workload | Direct config | Profile agreement |
| --- | --- | --- |
| Single CPU/GPU pod | `engine: job`, `compute.gpus: 0` or GPU count | GPU count must match |
| Multi-node PyTorch | `engine: job`, `execution.launcher: torchrun` | `execution.nodes` and `compute.gpus` match worker count and GPUs per worker |
| Ray Train/Tune | `engine: rayjob`, Ray launcher | `compute.workers` and `compute.gpus_per_worker` match; head is separate and CPU-only |

Use canonical `rayjob`; `ray` is a compatibility alias. Do not put
`compute.gpus_per_worker`, Ray worker sizing, or `runtime.pip` on a direct Job.
Do not add node selectors or change namespace/queue to bypass profile or
workspace rejection. Ask the platform owner for the appropriate profile.

## Observe, then recover

After an authorized submission:

```bash
tau run status <run-name> --watch
tau run status <run-name> -o json
tau logs <run-name> -f --tail 100
tau run list -o json
tau run get <run-name>
```

`tau logs` can discover a run across local workspace connections; use
`--workspace`, or `--context` and `--namespace`, for an exact target.
`tau run logs` remains available and inherits run-level `--project` routing.
The log command uses Ray driver logs for RayJobs and pod logs for Jobs.

Prefer JSON for agent parsing. Status `--watch` is a human-readable stream,
not JSON; use separate JSON polls or bound a watch with `--max-iterations`.
`--diagnostic-hints` emits scoped Kubernetes follow-up commands and cannot
be combined with watch or JSON (JSON already carries diagnostic commands).

Read startup phases in order, skipping phases marked `skipped`. Investigate
the first genuinely blocked/warning phase. Admission is not scheduling,
running is not application progress, and completion is not artifact durability.
Use scoped `kubectl` inspection when Tau's view lacks the necessary detail,
within the caller's RBAC; raw Kubernetes diagnostics are not inherently
operator-only.

Automatic retry is `resilience.max_retries`, not `tau run retry`. It can wait,
delete, and resubmit; enable it deliberately. Manual resume also deletes and
replaces a failed workload:

```bash
tau run resume <run-name> --config tau/train.yaml \
  --from /data/projects/<workspace>/runs/<run-name>/checkpoints \
  --dry-run=client
```

Resume still inspects the live workload in dry-run mode. Use its actual durable
checkpoint directory; the legacy default may not match workspace-scoped output.
The trainer must load `TAU_RESUME_FROM`. After OOM, change the relevant memory
or workload settings before using `--force`; `Unknown` is not retryable.

## Keep the other contracts separate

- **Serving:** `tau serve deploy` requires an active repository workspace and
  `--profile`. Even its client dry-run is connected. Deploy's explicit
  namespace/context must agree with the connection; status/scale/delete use
  their own target flags. See [Serving](references/serving.md).
- **Secrets:** use `runtime.env_secret` for direct configs, never literal secret
  values. `runtime.env_kv` is managed-workflow-only, not a direct Job/RayJob
  feature. Redaction in output does not make a credentials-bearing config safe.
- **Evaluation:** a direct eval is an ordinary Job/RayJob running evaluation
  code; no direct `eval` group or `tau eval` command exists. The optional SDK's
  `@tau.eval` is a different, managed workflow contract.
- **Evidence:** `taugrid-portal experiment ...` and `taugrid-portal portal serve`
  belong to the separate portal binary. Identify the store/backend before
  diagnosing empty results; local expstore, ADX scalars, historical logs, and
  lifecycle records are different evidence paths.
- **Data:** `tau data dataset` supports catalog operations **and ingest**;
  ingest copies bytes and writes registry state. Model/dataset alias updates
  are mutations, not discovery operations.

Report separately what parsed, rendered, passed connected checks, and actually
ran. Never turn a successful offline fixture or snapshot into a claim about
the user's live GPUs, credentials, storage, or service health.
