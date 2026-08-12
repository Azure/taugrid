---
title: Prerequisites and setup readiness
linkTitle: Prerequisites and readiness
weight: 4
description: What must already exist, how to validate it, and the TauWorkspace readiness gate
---

{{< maturity status="ga" reviewed="2026-08-11" >}}

Researchers and platform owners check the same contract before the first
run: what must already exist, what a passing check verifies, and what a
[TauWorkspace](../../concepts/glossary/#tauworkspace)'s
[status condition](../../concepts/glossary/#status-condition) means. This
page is that shared contract.

## AKS setup versus Kubernetes/TauGrid setup

AKS is TauGrid's first-class provider path, but Tau does not provision AKS.
Use these boundaries consistently:

| Phase | What it owns | Completion gate |
|---|---|---|
| **AKS setup (Azure/provider)** | Subscription and resource group, AKS resource and node pools, API networking/private DNS, managed Entra integration, AKS authorization, OIDC/workload-identity enablement, provider CSI/GPU add-ons, Azure storage/registry/managed identities, quota, cost, and cluster lifecycle | The intended normal cluster-user identity can reach the Kubernetes API, required Nodes are `Ready`, provider capabilities are available, and the non-secret AKS handoff values are recorded. |
| **Kubernetes/TauGrid setup** | Kueue, KubeRay, Tau CRDs and controllers, `TauCluster`, queue policy, topology policy, `TauWorkspace`, Namespace, Kubernetes RBAC, LocalQueue, ServiceAccount, StorageClass/PVC declarations, and platform observability objects | `tau cluster validate installation` passes, required provider-specific validation passes, and `tau workspace check <workspace>` exits `0`. |
| **Researcher workflow** | Local `tau` installation, the research repository, connection descriptor, target configs, application image/code, data contract, and result semantics | `tau run smoke` succeeds from the handed-off repository, followed by the project's own target validation. |

Identity and storage cross the boundary but have different owners on each side:

- **Identity:** AKS setup owns managed Entra, AKS cluster-user credential access,
  OIDC, Azure RBAC when selected, managed identities, and federation. Kubernetes/
  TauGrid setup owns the workspace subject, RoleBinding, ServiceAccount, and
  workload annotations that consume those provider capabilities.
- **Storage:** AKS setup owns the Azure storage service, network path, identity,
  and CSI driver. Kubernetes/TauGrid setup owns the StorageClass and PVC
  declarations. A Tau target only consumes a PVC that the platform has already
  made usable.

Other conformant Kubernetes clusters can supply equivalent controllers,
identity, networking, storage, and accelerator support. That is Tau's portable
Kubernetes surface. The automatic repository connection bootstrap documented
here is AKS-specific today: its descriptor names `cluster.provider: azure`, an
AKS ARM resource ID, and an Entra tenant so Tau can obtain AKS cluster-user
credentials.

## Prerequisite matrix

| Layer | Owner | Examples | Does Tau provision it? |
|---|---|---|---|
| AKS and Azure provider resources | Platform, with Azure tooling | Subscription readiness, AKS cluster and node pools, VNet/subnets, private DNS, managed Entra, AKS/Azure RBAC, storage accounts, managed identities and workload identity federation, container registry | No. Start with [Prepare an Azure subscription](../azure-subscription/), then provision outside Tau with Terraform, Bicep/ARM, Azure CLI, Portal, or an existing platform pipeline. |
| AKS provider add-ons | Platform, with Azure/IaC tooling | GPU drivers/device plugin, storage and Secrets Store CSI drivers, and provider-maintained Node identity labels | No. TauGrid consumes these AKS capabilities but does not provision them. |
| TauGrid Kubernetes control plane | Platform, with Helm or ArgoCD | Kueue controller, KubeRay operator, Tau CRDs/controller, and baseline queue policy | Yes for a fresh cluster through `tau cluster install` (a thin Helm wrapper). Existing managed clusters use independent ArgoCD Applications instead of the umbrella release. |
| Kubernetes workspace onboarding | Platform desired state plus `tau-core-controller` | [TauWorkspace](../../concepts/glossary/#tauworkspace), derived Namespace metadata/RBAC/LocalQueue/ServiceAccount, and platform-owned durable PVC policy | The platform declares TauWorkspace through Helm/Kustomize/GitOps. The controller reconciles derived state; it does not create Azure resources or storage. |
| Project/researcher prerequisites | Researcher | The `tau` CLI and a repository with a checked-in [target](../../concepts/glossary/#target) and `tau/workspace.connection.yaml` | No. Supplied through the platform-to-researcher handoff, not created by TauWorkspace. |

**TauWorkspace itself does not provision Azure resources, the AKS cluster,
node pools, network, Kueue, KubeRay, Azure RBAC, storage accounts, or managed
identities.** The controller reconciles namespace metadata/RBAC, a LocalQueue,
an optional workload-identity ServiceAccount, and declared GPU topology labels.
Storage remains platform-owned. See
[TauWorkspace](../../concepts/glossary/#tauworkspace) and
[workspace](../../concepts/glossary/#workspace) in the glossary.

## Platform component availability

The TauGrid chart installs its pinned Kueue, KubeRay, and Tau controller
dependencies. It does not install provider GPU/CSI drivers or a monitoring
stack. Existing ArgoCD clusters install the same control-plane components as
independent Applications:

| Component | When it is needed | Installation source |
|---|---|---|
| Kueue | Required for queued workloads, shared quota, priority, and admission. | TauGrid dependency or the platform's pinned ArgoCD Application |
| KubeRay operator | Required for RayJob and RayService workloads. | TauGrid dependency or the platform's pinned ArgoCD Application |
| GPU drivers and device plugin | Required for GPU workloads. | The Kubernetes provider's supported GPU enablement path |
| Storage and Secrets Store CSI drivers | Required when configs reference their volumes or secret providers. | The Kubernetes provider and CSI driver documentation |
| Tau workspace controller | Required to reconcile TauWorkspace state and declared Node topology labels. | TauGrid dependency or the platform's independently managed deployment |

Review upstream release notes and pin component versions rather than following
mutable tags.

**GPU monitoring is not GPU enablement.** The chart assumes the NVIDIA driver
and GPU device plugin already expose healthy GPU resources to Kubernetes. It
adds health detection and reporting; it does not install or replace the driver
or device plugin.

A platform owner must supply the workspace controller. Installing Kueue and
KubeRay alone does not create a functional TauWorkspace control plane.

## Canonical setup-validation sequence

Every command below uses a public `tau` root: `cluster`, `workspace`, or
`run`. It never uses a hidden compatibility root. See the
[CLI reference](../../reference/cli/) for the full command tree. Run these
in order; each stage assumes the previous one passed.

1. **Optional repository-only inspection, no cluster access required.**

   ```bash
   tau workspace connection inspect
   ```

   `tau workspace connection inspect` discovers and validates
   `tau/workspace.connection.yaml` without contacting a cluster. This command is
   an optional diagnostic, not an activation prerequisite; `tau run` discovers
   the descriptor automatically.

2. **Kubernetes/TauGrid platform setup, once per cluster after AKS setup.**

   ```bash
   tau cluster install --version <version> --values taugrid-values.yaml
   tau cluster validate nodes --gpu-class <class> --min-healthy <n>
   tau cluster validate topology --preset <preset>
   ```

   `install` performs only `helm upgrade --install`; it does not directly create
   storage or patch Nodes. The installed controller reconciles the reviewed
   `TauCluster.spec.nodes.labelRules`. Existing ArgoCD-managed clusters skip the
   umbrella install and sync their independent Applications. `validate nodes`
   and `validate topology` are read-only checks. See the
   [cluster install values reference](../reference/cluster-install-values/) for
   all configurable fields, or run `tau cluster explain-values`.

3. **Kubernetes/TauGrid workspace setup, once per workspace.**

   ```bash
   kubectl apply -f workspace.yaml
   tau workspace status <workspace> --context <context>
   ```

   Deliver `workspace.yaml` through the platform's reviewed Helm/Kustomize/
   GitOps path; the direct `kubectl` command above is the fresh-cluster
   equivalent. The controller creates and reconciles workspace-derived state.
   [Enable a workspace](../../tasks/platform/enable-workspace/) documents the
   native `TauWorkspace` contract and the full
   platform sequence and the [handoff checklist](../../tasks/platform/handoff/).

4. **Researcher, once handed a repository.**

   ```bash
   tau run smoke
   tau run train --dry-run=client
   ```

   The checked-in descriptor is explicit repository/platform preconfiguration.
   On first use, Tau automatically obtains a normal AKS cluster-user kubeconfig
   with the caller's Azure identity, stores it as a private per-connection file,
   verifies the live workspace contract, and pins that configuration. This works
   without a TTY when the caller already has a usable noninteractive Azure
   identity; authentication still fails normally when it does not.

   The bounded smoke then verifies workspace gating, queue admission, scheduling,
   service-account selection, and container execution. It does not mount the
   workspace PVC or access external data services. Client dry-run does not
   submit a workload, but it still discovers and verifies the workspace
   connection before rendering platform-owned namespace and queue policy.

   Live readiness evidence expires after five minutes. After that, interactive
   and noninteractive commands re-run the same read-only workspace, LocalQueue,
   and authorization checks without fetching credentials. Tau fails closed and
   requires interactive review if the descriptor, cluster/resource, tenant,
   authorization mode, workspace UID, namespace, LocalQueue, or workload service
   account changes.

   `workspace-rbac` is the API default and is what `tau workspace create`
   writes; the controller binds the researcher subject in the workspace
   namespace. `cluster-wide` is an explicit opt-out that grants no researcher
   access, and some existing clusters run it. The
   [multiple-workspace lifecycle](../../concepts/workspaces/#multiple-workspaces)
   is Alpha: v0 activates one workspace and blocks additional workspace objects
   until the active workspace is removed.

## TauWorkspace readiness gate

A [TauWorkspace](../../concepts/glossary/#tauworkspace) reports one of
three phases:

| Phase | Meaning |
|---|---|
| `Pending` | No gating condition has failed, but at least one is still unresolved. |
| `Ready` | All conditions are satisfied; researchers can submit runs. |
| `Degraded` | At least one gating condition is `False`, or `DriftDetected` is `True`. |

The phase currently gates on `RBACReady` and `QueueReady`, and becomes
`Degraded` when drift is detected. The status also reports the diagnostic
`WorkloadIdentityReady` condition, which does not change the overall phase.
The workspace controller emits no `StorageReady` condition, so confirm the
durable PVC is `Bound` yourself rather than inferring it from `Ready`.

Inspect the phase and every condition with:

```bash
tau workspace status <workspace> --context <context>
```

Gate on readiness. This exits non-zero when the workspace is not `Ready`:

```bash
tau workspace check <workspace> --context <context>
```

`tau run` and `tau run smoke` apply the same gate before submitting. When
the workspace is not `Ready`, they fail closed with:

```text
workspace "<workspace>" is not Ready (phase=<phase>)
```

Tau has no bypass flag, but this is intentionally a CLI readiness check rather
than a custom cluster admission policy. Raw Kubernetes clients remain governed
by ordinary RBAC and Kueue. A platform owner should not hand off a repository
until `tau workspace check` exits `0`.
