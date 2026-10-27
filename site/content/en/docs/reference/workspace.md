---
title: TauWorkspace reference
weight: 3
description: Kubernetes workspace desired state and readiness
---

{{< maturity status="alpha" reviewed="2026-07-16" >}}

`TauWorkspace` is a platform-authored Kubernetes resource in the TauGrid system namespace (`tau-system` by default). A custom `tau cluster install --namespace <name>` moves the controller and these objects together; administrative workspace commands use `--system-namespace <name>`, and repository descriptors persist the value as `cluster.systemNamespace`. It reconciles or verifies:

- Target namespace and namespaced RBAC.
- Kueue LocalQueue accessibility.
- Workload identity ServiceAccount configuration.
- Readiness and drift conditions.

Platform teams provision Azure resources and cluster-scoped quota policy,
projects own their own code, and platform teams verify the durable storage
PVC separately.

For `tau workspace create`, `--system-namespace` selects where the `TauWorkspace` object lives and defaults to the installed TauGrid system namespace; `--namespace` selects the researcher workload Namespace and defaults to the workspace name. These are intentionally separate because system components share one namespace while workload isolation remains workspace-scoped.

Workspace status is intentionally agent-readable:

```bash
tau workspace list
tau workspace status <name>
```

Workspace RBAC is the API default and what `tau workspace create` writes; the controller binds the researcher subject in the workspace namespace. Cluster-wide authorization is an explicit opt-out: the platform grants researcher access separately. Some existing clusters use this mode. The [multiple-workspace lifecycle](../../developer-guide/concepts/workspaces/#multiple-workspaces) is Alpha: v0 activates one workspace and blocks additional workspace objects until the active workspace is removed.

The controller source and raw Kustomize manifests live in the [`Azure/taugrid`](https://github.com/Azure/taugrid) repository. Versioned MCR release artifacts include the controller image, the standalone controller OCI chart, the TauGrid umbrella OCI chart, and the CRDs packaged by both charts.

Fresh clusters consume the umbrella chart through `tau cluster install`. The standalone chart is only for a separately managed controller and must not be installed alongside an umbrella release that already enables `components.tauCoreController`. See [Install TauGrid](../../platform-admin-guide/kubernetes/#3-install-taugrid) for the supported installation entry point and readiness gate. This page defines the workspace API and readiness contract only; installing the cluster control plane is covered there.

## Workspace readiness and recovery

Start with the condition reason and message, rather than a guessed repair:

```bash
tau workspace status <name> --context <context>
tau workspace status <name> --context <context> -o json
```

The overall phase currently requires `RBACReady` and `QueueReady`; `DriftDetected=True` also makes it `Degraded`. Storage remains platform-owned desired state, tracked outside TauWorkspace's own conditions.

| Condition | Platform action |
| --- | --- |
| `RBACReady=False` | In `workspace-rbac` mode, correct the declared subject/role and let the controller reconcile namespaced RBAC. In `cluster-wide` mode, repair the pre-existing authorization that the platform manages directly. |
| `QueueReady=False` | Restore the named LocalQueue and its accessible backing ClusterQueue; fix that queue directly rather than creating a second queue with a different name to bypass the workspace spec. |
| `DriftDetected=True` | Fix the dependency or workspace spec named in the message, then let the controller restore its owned namespace/RBAC objects. |

During namespace retargeting, unfinished Workloads on retained external queues
can delay cleanup of the old namespace's routing metadata. Submitted, unfinished
Jobs, RayJobs, and RayServices that have not received a derived Workload yet retain routing
metadata on deletion or retargeting, using their queue label and the live queue
selector. Completed or failed batch sources do not block cleanup. A RayJob is finished
only when `status.jobDeploymentStatus` is `Complete` or `Failed`, matching Kueue;
a failed driver with a retry still pending retains its route. RayJob discovery
is skipped when the optional RayJob API is not installed; RayService discovery
is checked independently and also tolerates an absent API. Other discovery errors
block cleanup until the source state can be read. RayServices have no terminal
job state: a queued service retains required routing metadata and unchanged
execution identity until the service is deleted, even if a derived Workload
reports `Finished`. Delete the service before completing workspace
deletion or retargeting. Both Helm and Kustomize grant the controller read-only
discovery of Jobs, RayJobs, and RayServices.
Kueue-managed Deployments submitted with `tau serve deploy --kind=deployment`
also retain routing before their Pod Workloads exist. Discovery uses the Pod
template's managed and queue labels, not just the Deployment's own labels.
A Deployment has no terminal job state: a present managed Deployment retains
its required route even at zero replicas or after a Pod Workload finishes.
Delete the Deployment before completing workspace deletion or retargeting.
Both controller RBAC manifests include read-only Deployment discovery;
unreadable Deployment lists block cleanup.
An unchanged workload identity ServiceAccount is retained until those Workloads finish, while researcher
authorization is reconciled independently. Changing the ServiceAccount name or
client ID, or removing `workloadIdentity`, revokes the obsolete ServiceAccount
before granting the current researcher access to the old namespace, even during
the drain. Pending Jobs that use the revoked account can no longer create Pods;
the operator must cancel or replace affected work before the drain can complete.
Keep the identity unchanged until draining finishes when uninterrupted execution
is required.

If identity cleanup fails or deletion is delayed by a finalizer, researcher
bindings are removed until obsolete identity revocation is confirmed. After
cleanup recovers, the current researcher's binding is restored in the retained
resolved namespace even while its Workloads are unfinished, so the researcher
can inspect or cancel the remaining work. Cluster-wide authorization does not
create a researcher binding.

### Storage readiness is platform-owned

`Ready` proves RBAC and queue reconciliation only, not storage. TauGrid 0.1 has no `StorageReady` condition on `TauWorkspace` or `TauCluster`, so a workspace can reach `Ready` while a workload's configured PVC is missing or unbound. Verify the platform-managed claim directly before handing the workspace to a researcher:

```bash
kubectl get pvc blob-training -n <workspace-namespace> --context <context>
```

Even `Bound` proves existence only; write validation happens at mount time on
the workload's own pod. The platform must also verify that the selected CSI
driver is registered on every node eligible for the workload profile:

```bash
kubectl get csidrivers
kubectl get csinode <eligible-node> \
  -o jsonpath='{range .spec.drivers[*]}{.name}{"\n"}{end}'
```

Repeat the `CSINode` check for every eligible node, then run a mount/read/write
canary with the workload's profile, ServiceAccount, and security settings.
Wrong credentials or read-only mounts surface in the canary or workload.
Missing node registration fails earlier, before container startup, with pod
events such as `driver name file.csi.azure.com not found in the list of
registered CSI drivers`; `file.csi.azure.com` will also be absent from that
node's `CSINode` object.

AKS-managed Azure CSI addons are supported and expected on normal AKS agent
pools, but TauGrid does not support relying on those managed addons on Flex or
other externally registered nodes. TauGrid helps operators set up and validate
a compatible deployment-managed CSI driver; operators retain ownership of the
storage backend, credentials, capacity, data lifecycle, and driver upgrades.
See
[AKS storage drivers and Flex nodes](../../platform-admin-guide/aks-setup/#aks-storage-drivers-and-flex-nodes).

`WorkloadIdentityReady` is diagnostic; the overall phase gate currently
excludes it. Resolve it before handing off workloads that rely on Azure
Workload Identity.

The controller reconciles again periodically. After repairing the named dependency, wait for:

```bash
tau workspace check <name> --context <context>
```

TauGrid's normal submission path always honors workspace readiness, with no bypass flag. Raw Kubernetes clients are governed by ordinary RBAC and Kueue rather than a custom TauGrid admission policy.
