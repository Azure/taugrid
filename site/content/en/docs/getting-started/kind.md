---
title: Evaluate locally or on Kind
weight: 3
description: Prove Tau's portable Kubernetes path without Azure
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Local dry-run proves repository and rendering contracts without a cluster:

```bash
tau run validate --config tau.yaml
tau run --config tau.yaml --dry-run=client
```

The in-repository Kind smoke test proves the open Kubernetes path with Kueue and
KubeRay:

```bash
cd cli
make test-kind-e2e
```

Use Kind to validate Job/RayJob rendering, queue admission, and lifecycle
behavior. It does not prove GPU drivers, cloud identity, production storage, or
live-cluster quota.

The fixture and expected flow live in
[`cli/examples/kind-smoke`](https://github.com/Azure/taugrid/tree/main/cli/examples/kind-smoke).
