---
title: Configuration resolution
weight: 3
description: How repository and workspace intent become a workload
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

The normal researcher contract is a checked-in direct run config:

```yaml
name: train
engine: ray
entrypoint: train.py

compute:
  workers: 2
  gpus_per_worker: 8

storage:
  data_pvc: training-data
```

Tau combines:

1. Explicit [project](../glossary/#project) and [target](../glossary/#target) selection.
2. [Repository](../glossary/#repository) or monorepo discovery.
3. The project's [workspace connection descriptor](../glossary/#workspace-connection).
4. Platform-owned [workspace](../glossary/#workspace) defaults.
5. Checked-in [workload](../glossary/#workload) intent.
6. Temporary explicit operator overrides.

Ambiguity is an error. Dry-run output must keep default sources visible.

```bash
tau run validate --config tau/train.yaml
tau run train --dry-run=client
tau run explain-config
```

See [direct run config vs. managed workflow manifest](../glossary/#run-config-vs-manifest)
for how Python SDK-generated manifests relate to this default format.
