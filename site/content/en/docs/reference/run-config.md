---
title: Run config reference
weight: 2
description: Direct config-first workload intent
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

This page documents the **direct run config**: the normal, hand-written
`tau.yaml` that `tau run --config` reads to submit a workload. See
[direct run config vs. managed workflow manifest](../../concepts/glossary/#run-config-vs-manifest)
for how this differs from the SDK-generated, `schema_version: 1` manifest that
`tau.train`/`tau.serve` render. Both can use a `tau.yaml` filename; inspect
the schema, not the filename. Use the managed workflow shape only when Tau
needs to own workflow semantics such as staged train/eval lineage.

Normal research projects check in direct config:

```yaml
name: train
engine: ray
entrypoint: train.py

runtime:
  image: <pinned-image>

compute:
  workers: 2
  gpus_per_worker: 8

storage:
  data_pvc: training-data
  output: /data/checkpoints/workflows/train
```

`storage.data_pvc` names an existing Bound PVC in the resolved workload
namespace. Tau references and mounts that claim; it does not provision or own
the PVC, StorageClass, CSI configuration, or backing storage. The platform
owner chooses the backend and manages its lifecycle.

Direct batch Jobs can stage an immutable reference directory from another image
without creating a ConfigMap:

```yaml
engine: job
storage:
  image_assets:
    - name: pinned-reference-assets
      image: <registry>/<repository>@sha256:<64-hex-digest>
      source_path: /opt/source-assets
      mount_path: /opt/reference
```

Tau renders each asset as a pinned init-container image that runs
`/bin/cp -a` into an `emptyDir`, then mounts that volume read-only in the main
container. Source images must provide `/bin/cp`. Names and paths are validated,
Tau-reserved paths cannot be replaced, and mutable image tags are rejected.
`storage.image_assets` is intentionally limited to direct `engine: job`
configs; managed workflows and Ray configs reject it.

Main field groups:

| Group | Purpose |
|---|---|
| `name`, `engine`, `entrypoint` | Workload identity and execution mode; `script` aliases `entrypoint`, and the nested `run.*` block adds `working_dir` for shipping a whole project directory |
| `runtime` | Image, packages, and literal/secret/Key Vault environment; top-level `image` is a lowest-priority alias for `runtime.image` |
| `compute` | Worker, GPU, CPU, and memory intent |
| `execution` | Launcher, node/process topology, launcher configs, and Ray Tune search settings |
| `policy` | Explicit operator/accounting overrides |
| `storage` | Durable data, output, checkpoint, and extra mounts |
| `metrics` | Published JSONL metric paths and the opt-in offload sidecar |
| `resilience` | Automatic retry filters, backoff, and checkpoint path — see [recovery](../../operations/recovery/) |
| `profiler` | Bounded rank-scoped profiling |
| `experiment` | Project, experiment name, and group |
| `workflow` | Delegation to an SDK-generated managed workflow manifest |

Validate the installed contract. `--config` always names an explicit file;
`tau run` itself instead takes an optional positional `TARGET` (for example
`tau run train`) that resolves `tau/train.yaml` — the two are not
interchangeable, so keep validating against an explicit path:

```bash
tau run validate --config tau.yaml
tau run schema -o json
tau run explain-config
```

For the full submit-to-evidence loop against a named target, see
[first run](../../tasks/researcher/first-run/).

The installed CLI is the final schema authority: use `tau run schema`.
