# TauGrid Distribution Chart

Kubernetes-native TauGrid distribution. Installs Kueue, KubeRay, the Tau core
controller, GPU node health monitoring, and a portable baseline queue on fresh
AKS clusters.

## Install

```bash
tau cluster install --version 0.2.0 --values taugrid-values.yaml
```

Or with Helm directly:

```bash
helm upgrade --install taugrid oci://aksairuntime.azurecr.io/unlisted/aks/ai-runtime/helm/taugrid \
  --version 0.2.0 \
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
| `baselineQueue.enabled` | bool | `true` | Create the baseline ClusterQueue, LocalQueue, and ResourceFlavor |
| `baselineQueue.name` | string | `jobqueue` | LocalQueue name (must be a valid DNS label) |
| `baselineQueue.namespaceSelector` | object | `{matchExpressions: [{key: tau.azure.com/workspace, operator: Exists}]}` | Which namespaces get the LocalQueue |
| `baselineQueue.topology.enabled` | bool | `true` | Create a Topology object for hostname-level scheduling |
| `baselineQueue.topology.name` | string | `default-node-topology` | Topology object name |
| `baselineQueue.flavor.name` | string | `taugrid-default` | ResourceFlavor name |
| `baselineQueue.flavor.nodeLabels` | map | `{kubernetes.io/os: linux}` | Node selector labels for the flavor |
| `baselineQueue.flavor.tolerations` | list | `[]` | Tolerations applied to workloads admitted through this flavor (critical for GPU taints) |
| `baselineQueue.resources` | list | see below | Resource quotas for admission control |

Default resources:

```yaml
resources:
  - name: cpu
    nominalQuota: "100000"
  - name: memory
    nominalQuota: 100Ti
  - name: nvidia.com/gpu
    nominalQuota: "1000"
```

These quotas bound concurrent Kueue admission; Kubernetes scheduling still
enforces the cluster's real capacity.

**`baselineQueue.flavor.tolerations`** — When the cluster uses GPU taints
(e.g., `sku=gpu:NoSchedule`), admitted workloads need matching tolerations.
Set this field so Kueue's ResourceFlavor injects tolerations at admission time:

```yaml
baselineQueue:
  flavor:
    tolerations:
      - key: sku
        value: gpu
        effect: NoSchedule
```

Without this, Kueue excludes tainted nodes while assigning flavors and the
workload is never admitted, reporting `couldn't assign flavors to pod set`
with a `taint` exclusion count. Kueue unions a pod set's own tolerations with
the flavor's before filtering nodes, so a workload that already carries
matching tolerations is admitted either way — Tau injects the `sku=gpu` and
`nvidia.com/gpu` tolerations into the GPU workloads it renders. Set this field
so workloads Tau does not render are admitted too.

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
| `tau-core-controller.image.repository` | string | `aksairuntime.azurecr.io/unlisted/aks/ai-runtime/tau-core-controller` | Controller image |
| `tau-core-controller.tauCluster.nodeLabelRules` | list | `[]` | Node topology label reconciliation rules |

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

## Requirements

- Kubernetes >= 1.30
- Helm 3 or 4
