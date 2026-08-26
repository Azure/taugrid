---
title: Configuration resolution
weight: 3
description: How repository and workspace intent become a workload
aliases:
  - "/docs/concepts/config-resolution/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

The normal researcher contract is a checked-in direct run config:

```yaml
name: train
engine: rayjob
entrypoint: train.py

compute:
  workers: 2
  gpus_per_worker: 8

storage:
  data_pvc: training-data
```

TauGrid combines:

1. Explicit [project](../../../reference/glossary/#project) and [target](../../../reference/glossary/#target) selection.
2. [Repository](../../../reference/glossary/#repository) or monorepo discovery.
3. The project's [workspace connection descriptor](../../../reference/glossary/#workspace-connection).
4. Platform-owned [workspace](../../../reference/glossary/#workspace) defaults.
5. Checked-in [workload](../../../reference/glossary/#workload) intent.
6. Temporary explicit operator overrides.

Ambiguity is an error. Dry-run output must keep default sources visible.

```bash
tau run validate --config tau/train.yaml
tau run train --dry-run=client
tau run explain-config
```

See [direct run config vs. managed workflow manifest](../../../reference/glossary/#run-config-vs-manifest)
for how Python SDK-generated manifests relate to this default format.
