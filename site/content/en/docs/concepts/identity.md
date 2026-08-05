---
title: Identity and security boundaries
weight: 6
description: Separate human Kubernetes authorization from workload cloud identity
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

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
way. What remains future is multi-workspace activation -- v0 activates exactly
one workspace per cluster and blocks any additional one -- plus the
negative-access and production rollout gates for researcher isolation.
