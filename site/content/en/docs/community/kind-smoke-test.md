---
title: Run the Kind smoke test
linkTitle: Kind smoke test
weight: 2
description: Prove portable TauGrid, Kueue, and KubeRay behavior on a disposable local cluster
aliases:
  - "/docs/tasks/contributor/kind-smoke-test/"
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

Use this contributor smoke test for changes to manifest rendering, submission,
Kueue admission, KubeRay lifecycle handling, or the portable Kubernetes path.
Kind supplies a disposable local Kubernetes API and Nodes for portable-path
validation, distinct from an AKS or provider-integration acceptance test.

From the repository root, local dry-run verifies the checked-in fixture and
rendering contracts entirely offline, with no cluster required:

```bash
make -C cli build
cli/bin/tau run validate --config examples/kind-smoke/tau.yaml
cli/bin/tau run --config examples/kind-smoke/tau.yaml --dry-run=client
```

The full smoke test requires Kind, `kubectl`, Helm, and a healthy Docker or
Podman engine with at least 8 GiB of memory. Run:

```bash
cd cli
make test-kind-e2e
```

The test creates or reuses the `tau-kind` cluster, installs the local TauGrid
distribution with Kueue and KubeRay, submits Job and RayJob fixtures, and checks
queue admission and lifecycle behavior. A successful run exits `0` after the
required workloads and controllers reach their expected states.

It validates queue admission and lifecycle behavior on portable Kubernetes;
validate GPU drivers, cloud identity, production storage, private networking,
provider CSI integration, and live-cluster quota with an AKS-backed run
instead.

The fixture, inspection commands, cleanup controls, and expected flow live in
[`examples/kind-smoke`](https://github.com/Azure/taugrid/tree/main/examples/kind-smoke).
