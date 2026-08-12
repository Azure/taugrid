---
title: Workspaces
weight: 2
description: Understand workspace routing, readiness, and the Alpha multi-workspace lifecycle
---

A workspace connects repository intent to one cluster namespace, LocalQueue,
workload ServiceAccount, and authorization contract.

## Single active workspace

{{< maturity status="ga" reviewed="2026-08-12" >}}

TauGrid v0 activates one workspace per cluster. The CLI resolves that workspace
from the checked-in connection descriptor or the cluster's primary-workspace
marker. Researchers do not need to supply a workspace name for normal runs.

The active workspace must be `Ready` before Tau submits a workload. Readiness
confirms the namespace, queue, workload identity configuration, and
authorization contract described in the
[TauWorkspace reference](../../reference/workspace/).

## Multiple workspaces

{{< maturity status="alpha" reviewed="2026-08-12" >}}

A cluster can contain multiple `TauWorkspace` objects, but only one is active.
The controller marks additional workspaces `Degraded` with reason
`AdditionalWorkspaceBlocked`. It does not create their namespace, LocalQueue,
or researcher RoleBinding.

After the active workspace is deleted and its owned resources are cleaned up,
the controller can promote a remaining workspace. This Alpha lifecycle supports
controlled workspace replacement. It does not support concurrent active
workspaces or concurrent tenant isolation.

Platform operators can inspect all workspace objects:

```bash
tau workspace list
tau workspace status <name>
```

Do not create a second workspace to add concurrent capacity. Add capacity to the
active workspace's queue, or use a separate cluster.
