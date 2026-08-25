---
title: Identity and security boundaries
weight: 6
description: Separate human Kubernetes authorization from workload cloud identity
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Tau platforms answer two different questions:

1. **Who may submit and inspect Kubernetes workloads?**
2. **Which cloud resources may the workload pod access?**

Human authentication leads to Kubernetes authorization in the target namespace
and queue. Workload identity starts from a pod ServiceAccount and reaches only
the external resources required by that workload.

TauWorkspace can reconcile Kubernetes ServiceAccounts and report readiness. It
does not create an Azure managed identity, federated credential, Key Vault,
storage account, or cloud RBAC assignment.

`workspace-rbac` is the API default and what `tau workspace create` writes: the
controller binds the researcher subject in the workspace namespace.
`cluster-wide` is an explicit opt-out that grants no researcher access, leaving
the workspace to supply routing defaults only; some existing clusters run that
way. The [multiple-workspace lifecycle](../workspaces/#multiple-workspaces) is
Alpha: v0 activates one workspace and blocks additional workspace objects until
the active workspace is removed. Researcher isolation still requires its
negative-access and production rollout gates.

Kubernetes transport authorization is a third, narrower concern. An optional
Portal port-forward Role can authorize a human to open a tunnel without giving
the Portal application that person's identity. The Portal continues to read as
its own ServiceAccount, and every authorized tunnel reaches the same view. See
[single-workspace researcher access](../../tasks/platform/single-workspace-researcher-access/)
for the dedicated-Namespace boundary and rollback lifecycle.
