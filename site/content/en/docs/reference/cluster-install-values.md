---
title: Cluster install values
weight: 3
description: TauGrid distribution chart configurable values
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

This page documents the Helm values accepted by `tau cluster install`. The distribution chart bundles Kueue, KubeRay, the Tau core controller, GPU monitoring, the `taugrid-core` services chart, and a portable baseline queue into a single versioned release. Portal is enabled by default for the operator quickstart; Stellar, lifecycle recorder, and image prewarm remain disabled until the platform opts in.

Print this reference from your terminal:

```bash
tau cluster explain-values
```

The Helm release namespace is the only namespace setting for TauGrid system workloads and Services. TauGrid requires `kueue-system` because the bundled Kueue Extension Controller ServiceAccount identity is namespace-bound. The first-party charts follow that Helm release namespace, and the deprecated `gpu-monitoring.namespace` override must remain empty. Cluster-scoped resources remain cluster-scoped, and Kueue keeps its Kubernetes API aggregation binding in `kube-system`.

## Kueue Extension Controller

The AKS Kueue Extension Controller (KEC) is TauGrid's sole authority for AKS
node classification, Topology, and ResourceFlavors. The chart always enables
`kueue.aksExtension.enableKueueObjectsAutomation` and requires the entire
release in `kueue-system`:

```bash
tau cluster install \
  --context "$TAU_CONTEXT" \
  --values taugrid-values.yaml
```

TauGrid renders `TauCluster.spec.nodes.labelRules: []` and does not render
competing Topology or ResourceFlavor objects. The CPU baseline uses `aks-cpu`.
KEC GPU flavor names are derived from live inventory, for example
`aks-h200-ndisr-v5`; opt selected flavors into baseline quota:

```yaml
baselineQueue:
  gpu:
    flavors:
      - name: aks-h200-ndisr-v5
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "16"
```

An empty list creates a CPU-only baseline queue. Installation readiness checks
the KEC Deployment, user-node classification, `aks-default`, `aks-cpu`, every
selected GPU flavor, and the active ClusterQueue. KEC `/readyz` alone is not a
semantic readiness signal.

## MultiKueue capability

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `kueue.aksExtension.enableMultiKueue` | bool | `true` | Install the pinned Kueue MultiKueue controller capability |

The standard installation includes MultiKueue API/controller support and
read-only prerequisite observation. Operators separately create worker
credentials, AdmissionChecks, MultiKueueConfigs, MultiKueueClusters, dedicated
queues, and profiles. `TauCluster.status.conditions[MultiKueueReady]` reports
the health of those actual operator-owned prerequisites. See
[Multi-cluster execution](../../platform-admin-guide/multicluster/) before publishing a
MultiKueue profile.

## Components

Toggle sub-charts with `components.<key>.enabled`:

| Key | Default | Description |
| --- | --- | --- |
| `components.kueue.enabled` | `true` | Required Kueue chart containing both Kueue and KEC |
| `components.kuberayOperator.enabled` | `true` | KubeRay operator |
| `components.tauCoreController.enabled` | `true` | Tau core controller (TauWorkspace, TauCluster) |
| `components.taugridCore.enabled` | `true` | Include the services chart, including the default Portal |
| `components.gpuMonitoring.enabled` | unset | GPU monitoring follows `components.tauCoreController.enabled` until explicitly set |

## Baseline Queue

A Kueue ClusterQueue bootstrapped on first install. It references KEC-managed
ResourceFlavors; TauGrid does not create or mutate scheduling primitives.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `baselineQueue.enabled` | bool | `true` | Create the baseline ClusterQueue |
| `baselineQueue.name` | string | `jobqueue` | LocalQueue name (DNS label) |
| `baselineQueue.namespaceSelector` | object | `matchExpressions: [{key: tau.azure.com/workspace, operator: Exists}]` | Namespaces that receive the LocalQueue |
| `baselineQueue.resources` | list | cpu: 100000, memory: 100Ti | CPU/memory admission quota |
| `baselineQueue.gpu.coveredResources` | list | `nvidia.com/gpu` | GPU resources covered by the node-resource group |
| `baselineQueue.gpu.flavors` | list | empty | KEC-generated GPU flavor names and quotas to reference |

CPU-only admission uses `aks-cpu`. When GPU flavors are selected, CPU, memory,
and GPU share one resource group: `aks-cpu` receives zero GPU quota, and each
selected GPU flavor receives CPU, memory, and its declared GPU quota.

## Portal

The following are defaults of the TauGrid umbrella distribution used by `tau cluster install`. The standalone `taugrid-core` chart keeps Portal disabled, so platforms that install that child chart directly must opt in explicitly. Portal runs in the required `kueue-system` release namespace.

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `taugrid-core.portal.enabled` | bool | `true` | Install the operator Portal |
| `taugrid-core.portal.serviceAccount.create` | bool | `true` | Create the dedicated Portal ServiceAccount |
| `taugrid-core.portal.serviceAccount.name` | string | `tau-portal` | Portal ServiceAccount name |
| `taugrid-core.portal.rbac.create` | bool | `true` | Create cluster-wide read-only Kubernetes RBAC for Portal |

These defaults make the Portal shell, Runs and run-detail views, Cluster Nodes view, and live Ray discovery available to an operator through the ClusterIP Service. See [Configure Portal](../../platform-admin-guide/enable-portal/) to separately configure Kusto-backed boards, the scoped computed Jobs board, KueueViz, an authenticated researcher endpoint, and a durable experiment store.

### `baselineQueue.gpu.flavors`

List only ResourceFlavor names that KEC has generated from live AKS GPU node
inventory. TauGrid references these objects and does not own their selectors,
topology, taints, or tolerations.

```yaml
# taugrid-values.yaml
baselineQueue:
  gpu:
    flavors:
      - name: aks-h200-ndisr-v5
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "1"
```

## Sub-Chart Pass-Through

The remaining top-level keys pass values directly to embedded sub-charts:

| Prefix | Sub-chart | Common overrides |
| --- | --- | --- |
| `kueue.*` | Kueue v0.19 | `controllerManager.manager.image`, `managerConfig`, `aksExtension` |
| `kuberay-operator.*` | KubeRay v1.6 | `image`, `configuration`, `podAnnotations` |
| `tau-core-controller.*` | Tau controller | `image`, `tauCluster.workloadProfiles` |
| `taugrid-core.*` | Services chart | `prewarm.enabled`, `stellar.enabled`, `portal.enabled` |
| `gpu-monitoring.*` | GPU monitoring | `gpuSkus`, `daemonset`, `metricsCollector`, `namespace` |

## Example: GPU Cluster

After KEC has generated a GPU ResourceFlavor, reference its exact name:

```yaml
# taugrid-values.yaml: H200 cluster
baselineQueue:
  gpu:
    flavors:
      - name: aks-h200-ndisr-v5
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "8"
```

## See Also

- [CLI reference: `tau cluster`](../cli/#tau-cluster)
- [Install TauGrid](../../platform-admin-guide/kubernetes/#3-install-taugrid)
- Source: [`charts/taugrid/values.yaml`](https://github.com/Azure/taugrid/blob/main/charts/taugrid/values.yaml)
