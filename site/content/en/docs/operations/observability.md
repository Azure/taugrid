---
title: Observability and evidence
weight: 2
description: Immediate lifecycle, durable experiment state, and fleet telemetry
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Use each evidence plane for its intended scope:

| Plane | Use |
|---|---|
| Tau status and logs | Immediate admission, scheduling, placement, pod/container, restart, exit, and log state for one submitted run |
| Kubernetes objects and events | Operator-only raw escalation after Tau identifies an unavailable or deeper platform-owned layer |
| Ray and GPU metrics | Runtime and hardware behavior |
| Expstore and durable artifacts | Authoritative experiment, checkpoint, and recovery state |
| adx-mon and ADX/Kusto | Optional hosted scalar and fleet analysis |
| Dashboards and alerts | Consumer views over telemetry |

Scheduled, Running, and useful model progress are different claims. Preserve
raw logs, profiles, and artifacts when a diagnosis depends on them.
