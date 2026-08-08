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

## Ship an immutable source bundle

Use `run.source_bundle` when a Job or RayJob must run the checked-out project
tree rather than a single inline script. Paths are relative to the directory
containing this config file:

```yaml
run:
  entrypoint: project/train.py
  source_bundle:
    path: project
    excludes: [large-data/**]
    # Optional reproducibility pin:
    digest: sha256:<64 lowercase hex>

storage:
  data_pvc: blob-training # optional; source-bundle mode selects its default when omitted
```

Tau deterministically archives the selected local tree. The **uncompressed
input** is capped at 8 MiB; this is deliberately not the 64 KiB inline-payload
limit. Bundle contents are not placed in the workload manifest. Use
`excludes` for large local data and keep it on a PVC instead. `.git` is
excluded by default. A `digest` pin is optional, but when supplied it must be
`sha256:` followed by exactly 64 lowercase hexadecimal characters; submission
fails if the locally built archive does not match.

`run.source_bundle` and `run.working_dir` are mutually exclusive.
`working_dir` remains the Ray runtime-environment mechanism for its existing
use cases: it fails fast when its compressed inline archive exceeds 64 KiB.
Choose a source bundle for project shipping that needs the 8 MiB input budget
and durable content addressing instead.

On a real submission, Tau stages the zip over `kubectl exec` standard input to
the data PVC at
`/data/tau/source-bundles/sha256/<hex>.zip`. It verifies the exact SHA-256
before and after staging, reuses an already verified digest, and rejects a
corrupt existing target. Jobs and RayJobs verify the requested digest and
validate every archive member before the main container starts. Jobs safely
extract into pod-local runtime storage; Ray receives the verified zip through
its `runtime_env.working_dir` contract. `--dry-run=client` and
`--dry-run=server` render this reference but never stage source bytes.

Bundles are made only from the local working tree. This supports private
repositories without copying repository credentials, remotes, or tokens into
the PVC or workload; it also means a remote repository is not cloned during
submission. The workload image must include `python3`, which Tau uses to verify
the archive in every Job or Ray pod and to extract Job bundles. Multi-node
RayJobs require the selected data PVC to support `ReadWriteMany`. Run
`tau run status <name>` to see the short and full bundle digest, PVC, and
durable path recorded on the workload.

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
