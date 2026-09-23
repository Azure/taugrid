# TauGrid Distribution Chart

Kubernetes-native TauGrid distribution. Installs Kueue, KubeRay, the Tau core controller, GPU node health monitoring, the operator Portal, and a portable baseline queue on fresh AKS clusters.

## Install

```bash
tau cluster install --version 0.4.2 --values taugrid-values.yaml
```

Or with Helm directly:

```bash
helm upgrade --install taugrid \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid \
  --version 0.4.2 \
  --namespace kueue-system --create-namespace \
  --values taugrid-values.yaml \
  --wait --atomic
```

Use `tau cluster explain-values` to print the full field reference.

### Kueue Extension Controller

The distribution always installs the AKS Kueue Extension Controller (KEC) as
the node-classification, Topology, and ResourceFlavor authority. The release
must be installed in `kueue-system` because the extension ServiceAccount
identity is namespace-bound. Tau emits no node label rules and creates no
competing Topology or ResourceFlavor objects.

The baseline queue always references KEC's `aks-cpu` flavor. Add
inventory-derived GPU flavor names under `baselineQueue.gpu.flavors` when they
should receive quota. An empty list produces a CPU-only queue.

### Cluster-level ADX query connection

Record the existing cluster's **query** connection once in `taugrid-values.yaml`:

```yaml
global:
  adx:
    queryConnection:
      endpoint: https://my-cluster.eastus2.kusto.windows.net
      database: Metrics
      clientID: 11111111-2222-3333-4444-555555555555
```

Pass this file to the install/upgrade command above. Helm persists these
nonsecret values in the release; `tau cluster install --values` forwards them
without a separate CLI registration step. With the default umbrella settings,
Portal inherits the endpoint/database, annotates its `tau-portal` ServiceAccount,
and labels its pod for Azure Workload Identity. Its native React Experiments UI
and same-service `/api/v2/stellar/*` JSON API use the in-process ADX backend:
no query adapter, separate Stellar deployment, or `experimentsBackend` is needed.

Before installation, the platform must separately provision the reader identity,
grant **ADX database Viewer only**, and federate it to the cluster OIDC issuer
with audience `api://AzureADTokenExchange` and subject
`system:serviceaccount:kueue-system:tau-portal`. Adjust that subject if the release
namespace or ServiceAccount name changes. Enable Azure Workload Identity on the
cluster and ensure the pod can reach ADX. Never supply adx-mon's ingestion/admin
identity. Installation creates no Azure resources or federation and grants no
Azure/ADX permissions; it cannot verify the supplied identity's role assignments.
Existing experiment ingestion and tables are also prerequisites. The Cost board
still uses `taugrid-core.portal.kusto.costDatabase` (`CostTracking` by default)
and needs Viewer permission there if used.

Nonempty `taugrid-core.portal.kusto.endpoint` and `.database` override the shared
values independently. An explicit
`taugrid-core.portal.serviceAccount.annotations.azure.workload.identity/client-id`
overrides the shared client ID; an explicitly empty annotation is rejected.
Other `portal.kusto.*` settings, including an intentional `queryCommand`, remain
unchanged. Shared identity inheritance requires a chart-created ServiceAccount
(the umbrella default). For an externally managed ServiceAccount, configure its
name and explicit client-ID annotation in the chart to match the existing object;
the chart does not modify it.

Set all three connection fields together or leave all empty. A partial connection
fails rendering when Kusto Portal is enabled, rather than selecting an unintended
identity. An absent connection retains degraded Kusto APIs; `source=local/auto`
ignores the shared connection and retains its existing store requirements.
The fixed workspace remains `taugrid-default`, the Service remains ClusterIP,
and workspace-directory routing stays disabled. This connection is backend
authentication, not viewer authentication or permission to expose Portal.

## MultiKueue

The supported TauGrid install enables Kueue's MultiKueue capability by default,
including its CRDs, controller feature gate, and prerequisite RBAC. It does not
create operator-owned worker credentials, `MultiKueueCluster`,
`MultiKueueConfig`, `AdmissionCheck`, queue routing, or `multiKueue` workload
profiles. Operators provision those resources explicitly for their topology and
tenant isolation policy.
See [Multi-cluster execution](../../site/content/en/docs/platform-admin-guide/multicluster.md)
for isolation, tenant authorization, and rollback.

## Components

| Component | Condition | What it provides |
|---|---|---|
| Kueue | `components.kueue.enabled` | Job scheduling, admission, quota |
| KubeRay Operator | `components.kuberayOperator.enabled` | RayCluster/RayJob/RayService lifecycle |
| tau-core-controller | `components.tauCoreController.enabled` | TauWorkspace reconciliation, Node topology labels |
| taugrid-core | `components.taugridCore.enabled` | Default Portal plus opt-in Stellar, lifecycle recorder, and image prewarm services |
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

A Kueue ClusterQueue bootstrapped on first install. It references scheduling
objects created by KEC; TauGrid does not create or mutate ResourceFlavors or
Topology objects.

| Key | Type | Default | Description |
|---|---|---|---|
| `baselineQueue.enabled` | bool | `true` | Create the baseline ClusterQueue |
| `baselineQueue.name` | string | `jobqueue` | LocalQueue name (must be a valid DNS label) |
| `baselineQueue.namespaceSelector` | object | `{matchExpressions: [{key: tau.azure.com/workspace, operator: Exists}]}` | Which namespaces get the LocalQueue |
| `baselineQueue.resources` | list | cpu and memory | CPU/memory admission quotas |
| `baselineQueue.gpu.coveredResources` | list | `nvidia.com/gpu` | GPU resource names covered by the node-resource group |
| `baselineQueue.gpu.flavors` | list | `[]` | KEC-generated GPU ResourceFlavor names and per-flavor quotas |

CPU-only admission uses KEC's `aks-cpu` flavor. When GPU flavors are selected,
CPU, memory, and GPU share one resource group: `aks-cpu` receives zero GPU
quota, and each selected GPU flavor receives CPU, memory, and its declared GPU
quota. KEC owns their selectors, topology, taints, and tolerations.

```yaml
baselineQueue:
  gpu:
    flavors:
      - name: aks-h200-ndisr-v5
        resources:
          - {name: nvidia.com/gpu, nominalQuota: "8"}
```

These quotas bound concurrent Kueue admission; Kubernetes scheduling still
enforces the cluster's real capacity.

### `kueue`

Pass-through values for the embedded Kueue chart. The most common override is
the controller image:

| Key | Type | Default | Description |
|---|---|---|---|
| `kueue.controllerManager.manager.image.repository` | string | `mcr.microsoft.com/oss/v2/kueue/kueue` | Kueue controller image |
| `kueue.controllerManager.manager.image.tag` | string | `v0.19.2` | Kueue image tag |
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
| `tau-core-controller.image.repository` | string | `mcr.microsoft.com/aks/ai-runtime/tau-core-controller` | Controller image |
| `tau-core-controller.tauCluster.workloadProfiles` | list | reviewed single-cluster catalog | Complete platform workload-profile catalog; Helm list overrides replace the defaults |

The rendered TauCluster always has `spec.nodes.labelRules: []`; KEC owns node
classification and node-derived scheduling objects.

### `taugrid-core`

Services chart. The TauGrid distribution overrides the standalone child chart so Portal is available by default in the Helm release namespace (`kueue-system` for `tau cluster install`); Stellar, lifecycle recorder, and prewarm remain disabled.

| Key | Type | Default | Description |
|---|---|---|---|
| `taugrid-core.prewarm.enabled` | bool | `false` | GPU image pre-pull DaemonSet |
| `taugrid-core.stellar.enabled` | bool | `false` | Stellar experiment dashboard |
| `taugrid-core.lifecycleRecorder.enabled` | bool | `false` | Run lifecycle recorder |
| `taugrid-core.portal.enabled` | bool | `true` | Unified operator observability portal |
| `taugrid-core.portal.serviceAccount.create` | bool | `true` | Create the dedicated Portal ServiceAccount |
| `taugrid-core.portal.rbac.create` | bool | `true` | Create cluster-wide read-only Kubernetes RBAC for Portal |
| `taugrid-core.portal.entraAuth.enabled` | bool | `false` | Opt-in single-host Entra login via chart-managed oauth2-proxy, Certificate, Gateway and HTTPS HTTPRoute |

Browser authentication is separately opt-in even though Portal is installed by
default. Follow the [Portal authentication contract](../taugrid-core/README.md#opt-in-entra-authentication)
and [setup guide](../../site/content/en/docs/platform-admin-guide/setup-guides/enable-portal.md#opt-into-chart-managed-entra-browser-login)
before using the [merge example](../../examples/portal-entra-auth/values.yaml).
It requires operator-provisioned Entra identity/federation and enterprise-app
assignments, Workload Identity, cookie Secret, DNS, cert-manager issuer and a
Gateway API controller. It gives shared Portal viewer access, not workspace
authorization, and installs no public Ray head routes or mesh-wide policy.
Merge this configuration into the **complete canonical umbrella values file**
and pass it on every upgrade; a fragment alone with `tau cluster install`
can reset unrelated infrastructure because the CLI uses `--reset-values`.

All enabled system workloads and Services follow the Helm release namespace. Use `tau cluster install --namespace <name>` for a non-default system namespace on a fresh installation. Administrative workspace commands use the same value through `--system-namespace <name>`, and generated workspace connection descriptors persist it as `cluster.systemNamespace`. The gpu-monitoring 0.1.8 subchart rejects a non-empty deprecated `gpu-monitoring.namespace` override and directs operators to the release namespace instead. Cluster-scoped resources remain cluster-scoped, and Kueue keeps its Kubernetes API aggregation binding in `kube-system`.

Do not change the namespace of an existing Helm release in place. Releases from before namespace unification can also contain `TauWorkspace` and `TauQuotaRequest` objects in a legacy namespace. This chart does not migrate those objects automatically; use an explicit reviewed migration before a direct Helm upgrade, or use `tau cluster install` and keep the existing release version when its preflight reports legacy objects.

See `applications/taugrid/deploy/taugrid-core/README.md` for the full
taugrid-core values reference.

### `gpu-monitoring`

Node Problem Detector with GPU/InfiniBand/NVMe health checks, DCGM metrics,
node-exporter, and the metrics collector that writes Kubernetes Node conditions.
Deploys one DaemonSet per entry in `gpu-monitoring.gpuSkus`, each selecting nodes
by `node.kubernetes.io/instance-type`. A cluster with no matching instance types
gets DaemonSets that schedule nothing, so bundling is safe on CPU-only clusters.

| Key | Type | Default | Description |
|---|---|---|---|
| `gpu-monitoring.gpuSkus` | map | 13 profiles covering A10, A100, H100, H200, GB200, and GB300 | Per-SKU DaemonSet definitions |
| `gpu-monitoring.gpuSkus.<profile>.dcgmHealth.source` | string | global `dcgmHealth.source` | Per-profile DCGM health provider (`host-dcgmi` or `exporter`) |
| `gpu-monitoring.gpuSkus.<profile>.dcgmHealth.exporterUrl` | string | global `dcgmHealth.exporterUrl` | Per-profile DCGM exporter endpoint for mixed managed and GPU Operator clusters |
| `gpu-monitoring.daemonset.requireAcceleratorLabel` | bool | `false` | Also require `kubernetes.azure.com/accelerator=nvidia`. Externally-joined GPU nodes never receive that label, so requiring it leaves them unmonitored |
| `gpu-monitoring.namespace` | string | `""` (deprecated) | Must remain empty; gpu-monitoring 0.1.8 rejects overrides and requires Helm `--namespace` for all TauGrid system components |

See `charts/gpu-monitoring/README.md` for the full reference.

Managed GPU profiles use `http://localhost:19400/metrics` by default. Override
only the profiles backed by GPU Operator with its port-9400 Service, and set
that Service's `internalTrafficPolicy` to `Local` so collectors cannot scrape a
different node.

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
      - name: aks-h200-ndisr-v5
        resources:
          - name: nvidia.com/gpu
            nominalQuota: "8"
```

KEC creates the referenced flavor from live AKS node inventory and owns its
selectors, topology, taints, and tolerations.

## Requirements

- Kubernetes >= 1.30
- Helm 3 or 4
