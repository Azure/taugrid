---
title: Observability and evidence
weight: 2
description: Immediate lifecycle, durable experiment state, and fleet telemetry
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Use each evidence plane for its intended scope:

| Plane | Use |
|---|---|
| Tau status and logs | Immediate lifecycle of one submitted run |
| Kubernetes events and pod state | Admission, scheduling, startup, and termination |
| Ray and GPU metrics | Runtime and hardware behavior |
| Expstore and durable artifacts | Authoritative experiment, checkpoint, and recovery state |
| adx-mon and ADX/Kusto | Optional hosted scalar and fleet analysis |
| Dashboards and alerts | Consumer views over telemetry |

Scheduled, Running, and useful model progress are different claims. Preserve
raw logs, profiles, and artifacts when a diagnosis depends on them.

For the optional ADX-backed data plane, see
[Prepare ADX/Kusto for TauGrid](../../tasks/platform/prepare-adx-kusto/). For
Portal deployment and its available boards, see
[Configure Portal](../../tasks/platform/enable-portal/).
