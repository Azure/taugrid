---
title: Cluster install values
weight: 3
description: TauGrid distribution chart configurable values
---

{{< maturity status="shipped" reviewed="2026-07-31" >}}

This page documents the Helm values accepted by `tau cluster install`. The
distribution chart bundles Kueue, KubeRay, the Tau core controller, and a
portable baseline queue into a single versioned release.

Print this reference from your terminal:

```bash
tau cluster explain-values
```

## Components

Toggle sub-charts with `components.<key>.enabled`:

| Key | Default | Description |
|---|---|---|
| `components.kueue.enabled` | `true` | Kueue job scheduler |
| `components.kuberayOperator.enabled` | `true` | KubeRay operator |
| `components.tauCoreController.enabled` | `true` | Tau core controller (TauWorkspace, TauCluster) |
| `components.taugridCore.enabled` | `true` | Tau services (Stellar, Portal, prewarm) |

## Baseline Queue

A portable Kueue queue bootstrapped on first install. These quotas bound
concurrent Kueue admission; Kubernetes scheduling still enforces real capacity.
Production operators should replace them with deliberate capacity policy.

| Key | Type | Default | Description |
|---|---|---|---|
| `baselineQueue.enabled` | bool | `true` | Create the ClusterQueue, LocalQueue, ResourceFlavor, and Topology |
| `baselineQueue.name` | string | `jobqueue` | LocalQueue name (DNS label) |
| `baselineQueue.namespaceSelector` | object | `matchExpressions: [{key: tau.azure.com/workspace, operator: Exists}]` | Namespaces that receive the LocalQueue |
| `baselineQueue.topology.enabled` | bool | `true` | Create a Topology object for hostname-level scheduling |
| `baselineQueue.topology.name` | string | `default-node-topology` | Topology object name |
| `baselineQueue.flavor.name` | string | `taugrid-default` | ResourceFlavor name |
| `baselineQueue.flavor.nodeLabels` | map | `{kubernetes.io/os: linux}` | Node selector labels |
| `baselineQueue.flavor.tolerations` | list | `[]` | Tolerations injected at admission (see below) |
| `baselineQueue.resources` | list | cpu: 100000, memory: 100Ti, nvidia.com/gpu: 1000 | Admission quota |

The generic `taugrid-default` flavor intentionally has no
`tau.azure.com/gpu-class` node label. It supports `gpu_class: any` only.
Class-specific pools must label the ResourceFlavor and matching nodes with one
of `a100-80gb`, `h100-95gb`, or `h200-141gb`; Tau matches the label exactly and
does not infer hardware from the ResourceFlavor name.

### `baselineQueue.flavor.tolerations`

When the cluster uses GPU taints (e.g., `sku=gpu:NoSchedule`), Kueue excludes
tainted nodes while assigning flavors, and the workload is never admitted —
`couldn't assign flavors to pod set` with a `taint` exclusion count. Kueue
unions a pod set's own tolerations with the flavor's before filtering nodes, so
a workload that already carries matching tolerations is admitted either way;
Tau injects the `sku=gpu` and `nvidia.com/gpu` tolerations into the GPU
workloads it renders. Set this field so workloads Tau does not render are
admitted too.

```yaml
# taugrid-values.yaml
baselineQueue:
  flavor:
    tolerations:
      - key: sku
        value: gpu
        effect: NoSchedule
```

## Sub-Chart Pass-Through

The remaining top-level keys pass values directly to embedded sub-charts:

| Prefix | Sub-chart | Common overrides |
|---|---|---|
| `kueue.*` | Kueue v0.18 | `controllerManager.manager.image`, `managerConfig` |
| `kuberay-operator.*` | KubeRay v1.6 | `image`, `configuration`, `podAnnotations` |
| `tau-core-controller.*` | Tau controller | `platformNamespace`, `image`, `tauCluster.nodeLabelRules` |
| `taugrid-core.*` | Services chart | `prewarm.enabled`, `stellar.enabled`, `portal.enabled` |

## Example: GPU Cluster

```yaml
# taugrid-values.yaml — H200 cluster with GPU taints
baselineQueue:
  flavor:
    nodeLabels:
      kubernetes.io/os: linux
      tau.azure.com/gpu-series: h200
    tolerations:
      - key: sku
        value: gpu
        effect: NoSchedule

tau-core-controller:
  tauCluster:
    nodeLabelRules:
      - sourceLabel: node.kubernetes.io/instance-type
        targetLabel: tau.azure.com/gpu-series
        mappings:
          Standard_ND96isr_H200_v5: h200
```

## See Also

- [CLI reference — `tau cluster`](../cli/#tau-cluster)
- [Prerequisites](../../getting-started/prerequisites/)
- Source: [`charts/taugrid/values.yaml`](https://github.com/Azure/taugrid/blob/main/charts/taugrid/values.yaml)
