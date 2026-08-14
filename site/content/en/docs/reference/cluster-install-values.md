---
title: Cluster install values
weight: 3
description: TauGrid distribution chart configurable values
---

{{< maturity status="ga" reviewed="2026-07-31" >}}

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
| `baselineQueue.enabled` | bool | `true` | Create the ClusterQueue, LocalQueue, ResourceFlavors, and Topology |
| `baselineQueue.name` | string | `jobqueue` | LocalQueue name (DNS label) |
| `baselineQueue.namespaceSelector` | object | `matchExpressions: [{key: tau.azure.com/workspace, operator: Exists}]` | Namespaces that receive the LocalQueue |
| `baselineQueue.topology.enabled` | bool | `true` | Create a Topology object for hostname-level scheduling |
| `baselineQueue.topology.name` | string | `default-node-topology` | Topology object name |
| `baselineQueue.topology.requiredLevel` | string | `kubernetes.io/hostname` | Required topology level copied from managed GPU flavors to generated pod templates; custom levels are rendered above the always-present hostname leaf |
| `baselineQueue.flavor.*` | object | `taugrid-default-cpu`, Linux, no tolerations | CPU/memory flavor; keep GPU labels and tolerations out |
| `baselineQueue.resources` | list | cpu: 100000, memory: 100Ti | CPU/memory admission quota |
| `baselineQueue.gpu.enabled` | bool | `true` | Add GPU resources and flavors to the node-resource group |
| `baselineQueue.gpu.coveredResources` | list | `nvidia.com/gpu` | GPU resources covered by the node-resource group |
| `baselineQueue.gpu.flavors` | list | generic `taugrid-default-gpu` | GPU flavors and per-flavor quotas |

CPU, memory, and GPU share one Kueue resource group so each GPU pod set receives
one node flavor across all of its requested resources. `taugrid-default-cpu` has
zero GPU quota, while the generic `taugrid-default-gpu` has CPU/memory plus GPU
quota and supports `gpu_class: any` on a fresh install. When hardware is known,
replace the GPU flavor list with class-specific flavors and label matching nodes
with the canonical A10, A100, H100, H200, GB200, or GB300 class from
`policy.gpu_class`.
Only GPU flavors carry `topologyName` and the managed
`kueue.x-k8s.io/podset-required-topology` metadata annotation. Connected Tau
submission copies that requirement onto generated GPU pod templates when no
explicit placement policy is present. Raw Kubernetes manifests remain
expert-controlled. The CPU/memory flavor remains non-TAS.
For upgrades with saved legacy values, remove GPU resources from
`baselineQueue.resources` and move all GPU class/series labels and GPU-node
tolerations out of `baselineQueue.flavor` before adding their replacements
under `baselineQueue.gpu.flavors`. Declare GPU-node taints under each flavor's
`nodeTaints`. TauGrid fails template rendering if the old mixed values would
duplicate GPU coverage or constrain CPU-only admission.
Do not keep the generic GPU flavor beside class-specific flavors: exact class
quota must not fall back to an unlabeled ResourceFlavor.

### `baselineQueue.gpu.flavors`

Declare GPU-node taints (for example `sku=gpu:NoSchedule`) in each GPU flavor's
`nodeTaints`. This makes the flavor ineligible for CPU-only pods if generic CPU
quota is exhausted. Tau injects `sku=gpu` and `nvidia.com/gpu` tolerations into
GPU workloads, so those workloads remain eligible. Do not repeat a matching
taint under the flavor's `tolerations`: Kueue would then automatically tolerate
it for every pod and remove the CPU-isolation guard.

```yaml
# taugrid-values.yaml
baselineQueue:
  gpu:
    flavors:
      - name: a100-pool
        nodeLabels:
          kubernetes.io/os: linux
          tau.azure.com/gpu-class: a100-80gb
        nodeTaints:
          - key: sku
            value: gpu
            effect: NoSchedule
        tolerations: []
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "1"
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

TauGrid's controller derives the node contract from
`node.kubernetes.io/instance-type` and repairs drift:

| AKS VM size | GPU series | GPU class |
|---|---|---|
| `Standard_NC24ads_A100_v4` | `nc24ads-a100-v4` | `a100-80gb` |
| `Standard_ND96amsr_A100_v4` | `ndm-a100-v4` | `a100-80gb` |
| `Standard_NC40ads_H100_v5` | `nc-h100-v5` | `h100-95gb` |
| `Standard_ND96isr_H200_v5` | `nd-h200-v5` | `h200-141gb` |

**Warning:** Setting `tau-core-controller.tauCluster.nodeLabelRules` replaces
the canonical built-in catalog; it does not merge entries. Class-specific
admission requires the same canonical `tau.azure.com/gpu-class` value on the
nodes and their ResourceFlavors. Prefer
`tau-core-controller.tauCluster.extraNodeLabelRules` when adding reviewed VM
sizes so the built-in mappings remain intact.

CPU-only clusters and GPU pools scaled to zero remain ready when no catalog
entry currently matches.

```yaml
# taugrid-values.yaml — H200 cluster with GPU taints
baselineQueue:
  gpu:
    flavors:
      - name: h200-pool
        nodeLabels:
          kubernetes.io/os: linux
          kueue.azure.com/gpu-series: nd-h200-v5
          tau.azure.com/gpu-class: h200-141gb
        nodeTaints:
          - key: sku
            value: gpu
            effect: NoSchedule
        tolerations: []
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "8"
```

## See Also

- [CLI reference — `tau cluster`](../cli/#tau-cluster)
- [Prerequisites](../../getting-started/prerequisites/)
- Source: [`charts/taugrid/values.yaml`](https://github.com/Azure/taugrid/blob/main/charts/taugrid/values.yaml)
