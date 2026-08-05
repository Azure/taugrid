---
title: Prerequisites and setup readiness
linkTitle: Prerequisites and readiness
weight: 4
description: What must already exist, how to validate it, and the TauWorkspace readiness gate
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Researchers and platform owners check the same contract before the first
run: what must already exist, what a passing check proves, and what a
[TauWorkspace](../../concepts/glossary/#tauworkspace)'s
[status condition](../../concepts/glossary/#status-condition) means. This
page is that shared contract.

## Prerequisite matrix

| Layer | Owner | Examples | Does Tau provision it? |
|---|---|---|---|
| Azure/provider resources | Platform, with Azure tooling | Subscription readiness, AKS cluster, node pools, VNet/subnets, private DNS, Azure RBAC, storage accounts, managed identities and workload identity federation, container registry | No. Start with [Prepare an Azure subscription](../azure-subscription/), then provision outside Tau with Terraform, Bicep/ARM, Azure CLI, Portal, or an existing platform pipeline. |
| TauGrid control plane | Platform, with Helm or ArgoCD | Kueue controller, KubeRay operator, Tau CRDs/controller, and baseline queue policy | Yes for a fresh cluster through `tau cluster install` (a thin Helm wrapper). Existing managed clusters use independent ArgoCD Applications instead of the umbrella release. |
| Provider add-ons and infrastructure | Platform, with provider/IaC tooling | GPU drivers/device plugin, storage and Secrets Store CSI drivers, standard Node identity labels, Azure identities and federation | No. TauGrid consumes these capabilities but does not provision them. |
| Workspace onboarding | Platform desired state plus `tau-core-controller` | [TauWorkspace](../../concepts/glossary/#tauworkspace), derived namespace metadata/RBAC/LocalQueue/ServiceAccount, and platform-owned durable PVC policy | The platform declares TauWorkspace through Helm/Kustomize/GitOps. The controller reconciles derived state; the Tau CLI does not apply workspace scaffolding. |
| Project/researcher prerequisites | Researcher | The `tau` CLI, a repository with a checked-in [target](../../concepts/glossary/#target) and `tau/workspace.connection.yaml`, a reachable kubectl context | No. Supplied through the platform-to-researcher handoff, not created by TauWorkspace. |

**TauWorkspace itself does not provision Azure resources, the AKS cluster,
node pools, network, Kueue, KubeRay, Azure RBAC, storage accounts, or managed
identities.** The controller reconciles namespace metadata/RBAC, a LocalQueue,
an optional workload-identity ServiceAccount, and declared GPU topology labels.
Storage remains platform-owned — see
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
| GPU drivers and device plugin | Required for GPU workloads. | Your Kubernetes provider's supported GPU enablement path |
| Storage and Secrets Store CSI drivers | Required when configs reference their volumes or secret providers. | Your Kubernetes provider and CSI driver documentation |
| Tau workspace controller | Required to reconcile TauWorkspace state and declared Node topology labels. | TauGrid dependency or `applications/tau-core-controller` through ArgoCD |

Review upstream release notes and pin component versions rather than following
mutable tags.

**GPU monitoring is not GPU enablement.** The chart assumes the NVIDIA driver
and GPU device plugin already expose healthy GPU resources to Kubernetes. It
adds health detection and reporting; it does not install or replace the driver
or device plugin.

A platform owner must supply the workspace controller. Installing Kueue and
KubeRay alone does not create a functional TauWorkspace control plane.

## Canonical setup-validation sequence

Every command below uses a public `tau` root — `cluster`, `workspace`, or
`run` — never a hidden compatibility root. See the
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

2. **Platform, once per cluster.**

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

3. **Platform, once per workspace.**

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

   The bounded smoke then proves workspace gating, queue admission, scheduling,
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
   access, and some existing clusters run it. Multi-workspace activation is
   future work: v0 activates exactly one workspace per cluster.

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

Gate on readiness — this exits non-zero when the workspace is not `Ready`:

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
