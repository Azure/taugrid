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

Workspace RBAC is the API default and what `tau workspace create` writes; the controller binds the researcher subject in the workspace namespace. Cluster-wide authorization is an explicit opt-out under which the platform grants researcher access separately, and some existing clusters run it. The [multiple-workspace lifecycle](../../developer-guide/concepts/workspaces/#multiple-workspaces) is Alpha: v0 activates one workspace and blocks additional workspace objects until the active workspace is removed.

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

`Ready` proves RBAC and queue reconciliation only, not storage. TauGrid 0.1 has no `StorageReady` condition on `TauWorkspace` or `TauCluster`, so a workspace can reach `Ready` while a workload's configured PVC is missing or unbound. Verify the platform-managed claim directly before handing the workspace to a researcher:

```bash
kubectl get pvc blob-training -n <workspace-namespace> --context <context>
```

Even `Bound` proves existence only; write validation happens at mount time on the workload's own pod, so failures such as wrong BlobFuse credentials or a read-only mount surface there rather than in workspace status.

`WorkloadIdentityReady` is diagnostic; the overall phase gate currently
excludes it. Resolve it before handing off workloads that rely on Azure
Workload Identity.

The controller reconciles again periodically. After repairing the named dependency, wait for:

```bash
tau workspace check <name> --context <context>
```

TauGrid's normal submission path always honors workspace readiness, with no bypass flag. Raw Kubernetes clients are governed by ordinary RBAC and Kueue rather than a custom TauGrid admission policy.
