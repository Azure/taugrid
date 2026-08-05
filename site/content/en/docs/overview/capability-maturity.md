---
title: Capability maturity
weight: 3
description: Shipped, experimental, and future Tau capabilities
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau documentation uses three explicit maturity levels:

| Level | Meaning |
|---|---|
| **Shipped** | A current user or operator contract covered by the release and its validation |
| **Experimental / implementing** | Code exists, but rollout, compatibility, or production gates remain |
| **Future** | Architectural direction only; not an available user contract |

## Shipped

- Repository and project resolution.
- Config-first Job and RayJob submission.
- Workspace connection and readiness checks.
- Status, logs, results, cancellation, retry, and resume.
- Local-first experiment evidence, artifacts, and profiling.
- Cluster and topology validation.

## Experimental or implementing

- Dataset registry and selected production data-plane integrations.
- MultiKueue preflight and constrained worker dispatch.
- Some hosted Stellar and provider-specific rollout surfaces.

## Future

- Multi-workspace activation -- v0 activates exactly one workspace per cluster
  -- and researcher isolation pending its negative-access release gates.
- A general released model-serving quickstart.
- Automatic dataset replication.
- Unrestricted any-worker multi-cluster routing.

Maturity is a product contract. A design document or partial implementation does
not make a capability shipped.
