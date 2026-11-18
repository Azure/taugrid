---
title: Workspaces
weight: 2
description: Understand workspace routing, readiness, and the Alpha multi-workspace lifecycle
aliases:
  - "/docs/concepts/workspaces/"
---

A workspace connects repository intent to one cluster namespace, LocalQueue,
workload ServiceAccount, and authorization contract.

## Workspace selection

{{< maturity status="ga" reviewed="2026-08-12" >}}

A repository resolves one workspace at a time through its checked-in connection
descriptor or exact machine-local assignment. Legacy discovery can still use
the cluster's primary-workspace marker; that marker is not a controller-wide
limit on workspace reconciliation.

The active workspace must be `Ready` before TauGrid submits a workload. Readiness
confirms RBAC and queue reconciliation without detected drift.
Workload identity is a separate diagnostic, and storage remains platform-owned;
see the
[TauWorkspace reference](../../../reference/workspace/).

## Multiple workspaces

{{< maturity status="alpha" reviewed="2026-08-12" >}}

A cluster can contain independently reconciled `TauWorkspace` objects.
When a legacy primary-workspace marker exists, an additional workspace may
retain `V0PrimaryDiscovery=False` with reason `AdditionalWorkspaceBlocked`
to keep older clients from selecting it as primary. That compatibility
condition does not make the workspace `Degraded` or remove its namespace,
LocalQueue, researcher RoleBinding, or ServiceAccount.

Independent reconciliation is not proof of tenant isolation. This lifecycle
remains Alpha; validate researcher access boundaries and queue policy
separately before relying on concurrent workspaces.

Platform operators can inspect all workspace objects:

```bash
tau workspace list
tau workspace status <name>
```

Creating another workspace does not create physical capacity or an independent
quota budget. Review its queue and team policy before handing it to researchers.
