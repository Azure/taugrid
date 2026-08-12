---
title: Evaluate locally or on Kind
weight: 3
description: Verify Tau's portable Kubernetes path without Azure
---

{{< maturity status="shipped" reviewed="2026-08-11" >}}

This is TauGrid's **portable Kubernetes evaluation path**, not its first-class
AKS setup path. Kind supplies a local Kubernetes API and Nodes. It does not
supply an Azure subscription or AKS resource, managed Entra integration, AKS
cluster-user credential flow, Azure RBAC, private-cluster networking, managed
identity federation, provider CSI integration, or Azure GPU node lifecycle.

Local dry-run verifies repository and rendering contracts without a cluster:

```bash
tau run validate --config tau.yaml
tau run --config tau.yaml --dry-run=client
```

The in-repository Kind smoke test verifies the open Kubernetes path with Kueue and
KubeRay:

```bash
cd cli
make test-kind-e2e
```

Use Kind to validate Job/RayJob rendering, queue admission, and lifecycle
behavior. It does not verify GPU drivers, cloud identity, production storage, or
live-cluster quota.

The fixture and expected flow live in
[`cli/examples/kind-smoke`](https://github.com/Azure/taugrid/tree/main/cli/examples/kind-smoke).
