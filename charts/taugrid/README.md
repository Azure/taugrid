# TauGrid Distribution Chart

Kubernetes-native TauGrid distribution. Installs Kueue, KubeRay, the Tau core
controller, GPU node health monitoring, and a portable baseline queue on fresh
AKS clusters.

## Install

```bash
tau cluster install --version 0.2.3 --values taugrid-values.yaml
```

Or with Helm directly:

```bash
helm upgrade --install taugrid \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid \
  --version 0.2.3 \
  --namespace tau-system --create-namespace \
  --values taugrid-values.yaml \
  --wait --atomic
```

Use `tau cluster explain-values` to print the full field reference.

## Components

| Component | Condition | What it provides |
|---|---|---|
| Kueue | `components.kueue.enabled` | Job scheduling, admission, quota |
| KubeRay Operator | `components.kuberayOperator.enabled` | RayCluster/RayJob/RayService lifecycle |
| tau-core-controller | `components.tauCoreController.enabled` | TauWorkspace reconciliation, Node topology labels |
| taugrid-core | `components.taugridCore.enabled` | Stellar, Portal, image prewarm, ResourceFlavors |
| gpu-monitoring | follows `components.tauCoreController.enabled` | GPU/IB/NVMe node health checks, DCGM, Node conditions |

All components are enabled by default. Disable any with
`--set components.<key>.enabled=false`.

GPU monitoring is deliberately not an independent toggle: a cluster running a
Tau control plane always gets GPU node health signal, so a fleet cannot end up
scheduling GPU work it cannot observe. Disabling the controller disables it too.
To decouple them, set `components.gpuMonitoring.enabled` explicitly — that key
is absent from `values.yaml` precisely so Helm falls through to the controller
toggle, and setting it in a values file takes over permanently.

```bash
# Tau control plane without GPU monitoring (e.g. the cluster already runs it)
--set components.gpuMonitoring.enabled=false

# GPU monitoring without the Tau control plane
--set components.tauCoreController.enabled=false --set components.gpuMonitoring.enabled=true
```

## Values Reference

### `components`

Toggle individual sub-charts. All default to `true`.

| Key | Type | Default | Description |
|---|---|---|---|
| `components.kueue.enabled` | bool | `true` | Install the Kueue job scheduler |
| `components.kuberayOperator.enabled` | bool | `true` | Install the KubeRay operator |
| `components.tauCoreController.enabled` | bool | `true` | Install the Tau core controller |
| `components.taugridCore.enabled` | bool | `true` | Install the taugrid-core services chart |
| `components.gpuMonitoring.enabled` | bool | *(unset)* | Install GPU node health monitoring. Unset by design — falls through to `components.tauCoreController.enabled` |

### `baselineQueue`

A portable Kueue queue bootstrapped on first install. Production operators
should replace this with deliberate capacity policy.

| Key | Type | Default | Description |
|---|---|---|---|
| `baselineQueue.enabled` | bool | `true` | Create the baseline ClusterQueue, LocalQueue, ResourceFlavors, and Topology |
| `baselineQueue.name` | string | `jobqueue` | LocalQueue name (must be a valid DNS label) |
| `baselineQueue.namespaceSelector` | object | `{matchExpressions: [{key: tau.azure.com/workspace, operator: Exists}]}` | Which namespaces get the LocalQueue |
| `baselineQueue.topology.enabled` | bool | `true` | Create a Topology object for hostname-level scheduling |
| `baselineQueue.topology.name` | string | `default-node-topology` | Topology object name |
| `baselineQueue.topology.requiredLevel` | string | `kubernetes.io/hostname` | Topology level Tau copies from managed GPU ResourceFlavors to generated pod templates; custom levels are rendered above the always-present hostname leaf |
| `baselineQueue.flavor.*` | object | `taugrid-default-cpu`, Linux, no tolerations | CPU/memory ResourceFlavor; keep GPU labels and tolerations out |
| `baselineQueue.resources` | list | cpu and memory | CPU/memory admission quotas |
| `baselineQueue.gpu.enabled` | bool | `true` | Add GPU resources and flavors to the node-resource group |
| `baselineQueue.gpu.coveredResources` | list | `nvidia.com/gpu` | GPU resource names covered by the node-resource group |
| `baselineQueue.gpu.flavors` | list | generic `taugrid-default-gpu` | GPU ResourceFlavors and per-flavor quotas |

CPU, memory, and GPU are in one Kueue ResourceGroup because they are all tied to
the selected node pool. The CPU flavor `taugrid-default-cpu` has no GPU selector,
no GPU taint toleration, and zero GPU quota. Each GPU flavor receives the same
CPU/memory quota plus its declared GPU quota, so a GPU pod set receives one
flavor across all requested resources. The fresh-install GPU flavor
`taugrid-default-gpu` is unlabeled and supports `gpu_class: any` only. Once
hardware is known, replace the entire GPU flavor list with class-labeled
flavors; do not retain the generic GPU flavor alongside them.

When topology is enabled, only GPU flavors carry `topologyName` and the
`kueue.x-k8s.io/podset-required-topology` resource-metadata annotation. Connected
Tau submission copies that requirement to generated GPU pod templates when the
workload has no explicit placement policy. Explicit topology policy remains
authoritative, and raw Kubernetes manifests are never rewritten. The CPU/memory
flavor remains non-TAS so CPU-only workloads can be admitted.

```yaml
baselineQueue:
  gpu:
    flavors:
      - name: a100-pool # platform-owned; Tau does not parse this name
        nodeLabels:
          kubernetes.io/os: linux
          kueue.azure.com/gpu-series: nc24ads-a100-v4
          tau.azure.com/gpu-class: a100-80gb
        nodeTaints:
          - {key: sku, value: gpu, effect: NoSchedule}
        tolerations: []
        resources:
          - {name: nvidia.com/gpu, nominalQuota: "1"}
```

Canonical classes cover the supported A10, A100, H100, H200, GB200, and GB300
memory variants. Placement and interconnect requirements remain separate
(`independent`, `single-node-nvlink`, `multi-node-nccl`, or `elastic-workers`).

Default queue values:

```yaml
resources:
  - name: cpu
    nominalQuota: "100000"
  - name: memory
    nominalQuota: 100Ti
gpu:
  coveredResources: [nvidia.com/gpu]
  flavors:
    - name: taugrid-default-gpu
      nodeLabels: {kubernetes.io/os: linux}
      nodeTaints: [{key: sku, value: gpu, effect: NoSchedule}]
      tolerations: []
      resources:
        - {name: nvidia.com/gpu, nominalQuota: "1000"}
```

These quotas bound concurrent Kueue admission; Kubernetes scheduling still
enforces the cluster's real capacity.

Upgrades from chart 0.2.0 create the replacement CPU flavor
`taugrid-default-cpu` instead of mutating the old mixed `taugrid-default`
flavor. Stop submissions, hold and drain the ClusterQueue, explicitly cancel
pending workload owners, then replace `spec.resourceGroups` with one group
where the CPU flavor has zero GPU quota and GPU flavors carry CPU, memory, and
GPU quota.
Do not patch a GPU class onto the old sole flavor: that selector would also
apply to CPU-only admission. ResourceFlavor fields may be immutable or protected
while referenced, so replacement names are the safe migration path.

Before running `helm upgrade`, also update saved values: remove GPU resources
from `baselineQueue.resources`, move every GPU class/series label and GPU-node
toleration out of `baselineQueue.flavor`, and declare GPU admission taints under
`baselineQueue.gpu.flavors[].nodeTaints`. The chart fails rendering rather than
emitting a duplicate or CPU-constraining resource contract when legacy mixed
values remain.

**`baselineQueue.gpu.flavors[].nodeTaints`** — Declare the taints associated
with GPU nodes (for example `sku=gpu:NoSchedule`). Kueue then prevents CPU-only
pods from falling through to GPU quota if generic CPU quota is exhausted.
Tau-rendered GPU pods already tolerate `sku=gpu` and `nvidia.com/gpu` taints:

```yaml
baselineQueue:
  gpu:
    flavors:
      - name: a100-pool
        # nodeLabels and resources omitted here
        nodeTaints:
          - key: sku
            value: gpu
            effect: NoSchedule
        tolerations: []
```

Do not repeat an admission taint under the same flavor's `tolerations`; Kueue
would treat it as automatically tolerated and make the GPU flavor eligible for
CPU-only admission. Workloads not rendered by Tau must declare their own
matching pod tolerations.

### `kueue`

Pass-through values for the embedded Kueue chart. The most common override is
the controller image:

| Key | Type | Default | Description |
|---|---|---|---|
| `kueue.controllerManager.manager.image.repository` | string | `mcr.microsoft.com/oss/v2/kueue/kueue` | Kueue controller image |
| `kueue.controllerManager.manager.image.tag` | string | `v0.18.2` | Kueue image tag |
| `kueue.managerConfig.controllerManagerConfigYaml` | string | (embedded) | Full Kueue Configuration YAML |

Refer to the [upstream Kueue chart values](https://kueue.sigs.k8s.io/docs/installation/)
for the complete reference.

### `kuberay-operator`

Pass-through values for the embedded KubeRay chart:

| Key | Type | Default | Description |
|---|---|---|---|
| `kuberay-operator.image.repository` | string | `mcr.microsoft.com/oss/v2/kuberay/operator` | KubeRay operator image |
| `kuberay-operator.image.tag` | string | `v1.6.2` | KubeRay operator image tag |
| `kuberay-operator.configuration.enabled` | bool | `true` | Enable RayCluster default container env injection |

Refer to the [upstream KubeRay chart values](https://docs.ray.io/en/latest/cluster/kubernetes/getting-started/raycluster-quick-start.html)
for the complete reference.

### `tau-core-controller`

| Key | Type | Default | Description |
|---|---|---|---|
| `tau-core-controller.platformNamespace` | string | `tau-platform` | Namespace where TauWorkspace CRs live |
| `tau-core-controller.image.repository` | string | `mcr.microsoft.com/aks/ai-runtime/tau-core-controller` | Controller image |
| `tau-core-controller.tauCluster.nodeLabelRules` | list | reviewed AKS GPU catalog | VM-size rules that reconcile GPU class and series labels |
| `tau-core-controller.tauCluster.extraNodeLabelRules` | list | `[]` | Additional cluster-specific GPU label rules |

The controller watches the standard `node.kubernetes.io/instance-type` label
and continuously reconciles both label contracts on current and future nodes:

| AKS VM size | `kueue.azure.com/gpu-series` | `tau.azure.com/gpu-class` |
|---|---|---|
| `Standard_NV6ads_A10_v5` | `nvads-a10-v5` | `a10-4gb` |
| `Standard_NV12ads_A10_v5` | `nvads-a10-v5` | `a10-8gb` |
| `Standard_NV18ads_A10_v5` | `nvads-a10-v5` | `a10-12gb` |
| `Standard_NV36ads_A10_v5`, `Standard_NV36adms_A10_v5`, `Standard_NV72ads_A10_v5` | `nvads-a10-v5` | `a10-24gb` |
| `Standard_NC24ads_A100_v4` | `nc24ads-a100-v4` | `a100-80gb` |
| `Standard_NC48ads_A100_v4`, `Standard_NC96ads_A100_v4` | `nc-a100-v4` | `a100-80gb` |
| `Standard_ND96asr_v4` | `nd-a100-v4` | `a100-40gb` |
| `Standard_ND96amsr_A100_v4` | `ndm-a100-v4` | `a100-80gb` |
| `Standard_NC40ads_H100_v5`, `Standard_NC80adis_H100_v5` | `nc-h100-v5` | `h100-95gb` |
| `Standard_NCC40ads_H100_v5` | `ncc-h100-v5` | `h100-95gb` |
| `Standard_ND96isr_H100_v5` | `nd-h100-v5` | `h100-80gb` |
| `Standard_ND96isr_H200_v5` | `nd-h200-v5` | `h200-141gb` |
| `Standard_ND96isr_GB200_v6`, `Standard_ND128isr_NDR_GB200_v6` | `nd-gb200-v6` | `gb200-192gb` |
| `Standard_ND128isr_GB300_v6`, `Standard_ND128isrG5_GB300_v6` | `nd-gb300-v6` | `gb300-288gb` |

CPU-only clusters and GPU pools scaled to zero remain ready when no catalog
entry currently matches.

Set `nodeLabelRules` only to replace this catalog. Use
`extraNodeLabelRules` to append another reviewed VM-size mapping.

### `taugrid-core`

Services chart (Stellar, Portal, prewarm, ResourceFlavors). All services
default to disabled in the distribution chart.

| Key | Type | Default | Description |
|---|---|---|---|
| `taugrid-core.prewarm.enabled` | bool | `false` | GPU image pre-pull DaemonSet |
| `taugrid-core.stellar.enabled` | bool | `false` | Stellar experiment dashboard |
| `taugrid-core.lifecycleRecorder.enabled` | bool | `false` | Run lifecycle recorder |
| `taugrid-core.portal.enabled` | bool | `false` | Unified observability portal |

See `applications/taugrid/deploy/taugrid-core/README.md` for the full
taugrid-core values reference.

### `gpu-monitoring`

Node Problem Detector with GPU/InfiniBand/NVMe health checks, DCGM exporter,
node-exporter, and the metrics collector that writes Kubernetes Node conditions.
Deploys one DaemonSet per entry in `gpu-monitoring.gpuSkus`, each selecting nodes
by `node.kubernetes.io/instance-type`. A cluster with no matching instance types
gets DaemonSets that schedule nothing, so bundling is safe on CPU-only clusters.

| Key | Type | Default | Description |
|---|---|---|---|
| `gpu-monitoring.gpuSkus` | map | 7 SKUs (a100, h100, h100-nvl-1g, h100-nvl-2g, h200, gb200, spark) | Per-SKU DaemonSet definitions |
| `gpu-monitoring.daemonset.requireAcceleratorLabel` | bool | `false` | Also require `kubernetes.azure.com/accelerator=nvidia`. Externally-joined GPU nodes never receive that label, so requiring it leaves them unmonitored |
| `gpu-monitoring.namespace` | string | `""` (release namespace) | Where the DaemonSets are installed |

See `charts/gpu-monitoring/README.md` for the full reference.

**Do not install this alongside a separately-managed `gpu-monitoring` release.**
The subchart creates cluster-scoped `gpu-monitoring` ServiceAccount, ClusterRole,
and ClusterRoleBinding objects. A cluster that already runs gpu-monitoring
through GitOps (see `applications/gpu-monitoring/`) will collide on those names;
use `--set components.gpuMonitoring.enabled=false` there.

## Minimal Production Example

```yaml
# taugrid-values.yaml
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

The default Tau controller rule for `Standard_ND96isr_H200_v5` supplies both
labels used by this ResourceFlavor.

## Requirements

- Kubernetes >= 1.30
- Helm 3 or 4
