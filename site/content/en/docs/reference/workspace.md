---
title: TauWorkspace reference
weight: 3
description: Kubernetes workspace desired state and readiness
---

{{< maturity status="alpha" reviewed="2026-07-16" >}}

`TauWorkspace` is a platform-authored Kubernetes resource in `tau-platform`.
It reconciles or verifies:

- Target namespace and namespaced RBAC.
- Kueue LocalQueue accessibility.
- Workload identity ServiceAccount configuration.
- Readiness and drift conditions.

It does not provision Azure resources, create cluster-scoped quota policy, own
project code, or verify the durable storage PVC.

Workspace status is intentionally agent-readable:

```bash
tau workspace list
tau workspace status <name>
```

Workspace RBAC is the API default and what `tau workspace create` writes; the
controller binds the researcher subject in the workspace namespace.
Cluster-wide authorization is an explicit opt-out that grants no researcher
access, and some existing clusters run it. The
[multiple-workspace lifecycle](../../concepts/workspaces/#multiple-workspaces)
is Alpha: v0 activates one workspace and blocks additional workspace objects
until the active workspace is removed.

The controller source, image, CRDs, and supported install manifests are not
publicly released yet. Until that
[release gate](../../getting-started/prerequisites/#platform-component-availability)
is complete, a platform owner must supply the controller and this page is an
API reference, not a standalone installation path.

## Recover a Degraded workspace

Start with the condition reason and message, not a guessed repair:

```bash
tau workspace status <name> --context <context>
tau workspace status <name> --context <context> -o json
```

The overall phase currently requires `RBACReady` and `QueueReady`;
`DriftDetected=True` also makes it `Degraded`. Storage remains platform-owned
desired state and is not a TauWorkspace condition.

| Condition | Platform action |
|---|---|
| `RBACReady=False` | In `workspace-rbac` mode, correct the declared subject/role and let the controller reconcile namespaced RBAC. In `cluster-wide` mode, repair the pre-existing authorization; the controller grants none. |
| `QueueReady=False` | Restore the named LocalQueue and its accessible backing ClusterQueue; do not create a second queue with a different name to bypass the workspace spec. |
| `DriftDetected=True` | Fix the dependency or workspace spec named in the message, then let the controller restore its owned namespace/RBAC objects. |

`Ready` does not prove storage works. TauGrid 0.1 has no `StorageReady`
condition on `TauWorkspace` or `TauCluster`, so a workspace can reach `Ready`
while a workload's configured PVC is missing or unbound. Verify the
platform-managed claim directly before handing the workspace to a researcher:

```bash
kubectl get pvc blob-training -n <workspace-namespace> --context <context>
```

Even `Bound` is only an existence proof. Nothing write-validates the volume, so
mount-time failures such as wrong BlobFuse credentials or a read-only mount
surface on the workload's own pod rather than in workspace status.

`WorkloadIdentityReady` is diagnostic and does not gate the overall phase
today. Resolve it before handing off workloads that rely on Azure Workload
Identity.

The controller reconciles again periodically. After repairing the named
dependency, wait for:

```bash
tau workspace check <name> --context <context>
```

Tau's normal submission path has no readiness-bypass flag. Raw Kubernetes
clients are governed by ordinary RBAC and Kueue rather than a custom Tau
admission policy.
