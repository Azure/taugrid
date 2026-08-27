---
title: Identity and security boundaries
linkTitle: Manage identity and access
weight: 30
description: Separate human Kubernetes authorization from workload cloud identity
aliases:
  - "/docs/concepts/identity/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

TauGrid platforms answer two different questions:

1. **Who may submit and inspect Kubernetes workloads?**
2. **Which cloud resources may the workload pod access?**

Human authentication leads to Kubernetes authorization in the target namespace
and queue. Workload identity starts from a pod ServiceAccount and reaches only
the external resources required by that workload.

TauWorkspace reconciles Kubernetes ServiceAccounts and reports readiness.
Platform teams provision the Azure managed identity, federated credential,
Key Vault, storage account, and cloud RBAC assignment that those
ServiceAccounts federate with.

`workspace-rbac` is the API default and what `tau workspace create` writes: the
controller binds the researcher subject in the workspace namespace.
`cluster-wide` is an explicit opt-out: the workspace supplies routing defaults
only, and the platform grants researcher access separately; some existing
clusters run that way. The [multiple-workspace lifecycle](../../developer-guide/concepts/workspaces/#multiple-workspaces) is
Alpha: v0 activates one workspace and blocks additional workspace objects until
the active workspace is removed. Researcher isolation still requires tests proving
that one workspace cannot access another's resources, plus production rollout
gates.
