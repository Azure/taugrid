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

Direct Jobs can also stage their complete source tree from an immutable OCI
image:

```yaml
name: source-backed-job
engine: job
entrypoint: experiments/train.py
run:
  ttl_seconds_after_finished: 3600
  source:
    image: <registry>/<repository>@sha256:<64-hex-digest>
    path: /workspace
```

Tau copies `run.source.path` into an `emptyDir` with an init container, mounts
the per-pod working copy at `/tau/source`, and runs `entrypoint` relative to
that directory. Source bytes never travel in environment variables or a
ConfigMap. The source image must provide `/bin/sh`, `cp`, and `chmod`; mutable
tags, absolute entrypoints, Ray dispatch, managed workflows, and combining
`run.source` with `run.working_dir` are rejected before rendering. The init
container normalizes working-copy permissions and sets the executable bit on
the entrypoint before a potentially non-root workload container starts. Build and push the source image once,
then reuse its digest in every run config. Kubernetes uses the workload's
configured private-registry authentication; do not put registry credentials or
signed download URLs in the config.

For example, build a source-only image whose `/workspace` contains the checked
out tree, push it to the platform's private registry, and resolve the immutable
digest before generating run configs:

```bash
docker buildx build --push -t "$SOURCE_REPOSITORY:$GIT_SHA" -f Dockerfile.source .
SOURCE_DIGEST="$(docker buildx imagetools inspect \
  "$SOURCE_REPOSITORY:$GIT_SHA" --format '{{.Manifest.Digest}}')"
printf '%s@%s\n' "$SOURCE_REPOSITORY" "$SOURCE_DIGEST"
```

`run.ttl_seconds_after_finished` is an optional retention period from 1 through
2,147,483,647 seconds for a completed or failed direct Job. It maps to Kubernetes
`spec.ttlSecondsAfterFinished`; omission keeps Tau's 28800-second default. The
TTL does not start while any regular container is still running.

Literal environment values are limited to 64 KiB each and 128 KiB in aggregate
before workload creation. Tau's generated embedded-payload environment entries
are also capped at 64 KiB. Use `run.source` or `storage.image_assets` for
content instead of embedding archives in `runtime.env`.

Main field groups:

| Group | Purpose |
|---|---|
| `name`, `engine`, `entrypoint` | Workload identity and execution mode; `script` aliases `entrypoint`, and the nested `run.*` block adds immutable Job source staging and Ray project-directory shipping |
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
