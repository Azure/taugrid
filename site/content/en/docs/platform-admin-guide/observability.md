---
title: Observability and evidence
linkTitle: Monitor the platform
weight: 50
description: Immediate lifecycle, durable experiment state, and fleet telemetry
aliases:
  - "/docs/operations/observability/"
---

{{< maturity status="ga" reviewed="2026-08-25" >}}

Use each evidence plane for its intended scope:

| Plane | Use |
|---|---|
| TauGrid status and logs | Immediate lifecycle of one submitted run |
| Kubernetes events and pod state | Admission, scheduling, startup, and termination |
| Ray and GPU metrics | Runtime and hardware behavior |
| Expstore and durable artifacts | Authoritative experiment, checkpoint, and recovery state |
| adx-mon and ADX/Kusto | Optional hosted scalar and fleet analysis |
| Dashboards and alerts | Consumer views over telemetry |

Scheduled, Running, and useful model progress are different claims. Preserve
raw logs, profiles, and artifacts when a diagnosis depends on them.

## Metrics-store methodology

TauGrid separates durable experiment ownership from fleet analytics:

| Layer | Responsibility |
|---|---|
| Immutable metric files | Preserve normalized scalar history in Parquet |
| Expstore SQLite index | Track runs, metric files, artifacts, idempotency keys, and summaries |
| Metric summaries | Provide finite counts, step ranges, min/max values, and the latest finite point per run and metric |
| ADX/Kusto projection | Support cross-workspace dashboards and fleet queries |

Expstore is authoritative. ADX ingestion uses checkpoints and query-time
deduplication so retries preserve one logical scalar point. Configure ADX
retention, hot-cache duration, identities, and database roles as platform
policy. The workspace's durable storage lifecycle governs local metric files
and artifacts.

Use `project`, `run_group_id`, and `run_id` for experiment identity. Use
`metric_name`, `step`, and `wall_time` to align and compare scalar histories.
Tags add workspace and workload dimensions without changing those core keys.

For a runnable workload that publishes immutable loss and accuracy chunks, see
[Live experiment evidence](../../examples/experiment-evidence/). For the full
local evidence and artifact contract, see
[Experiment evidence and artifacts](../../developer-guide/concepts/evidence/).

For the optional ADX-backed data plane, see
[Prepare ADX/Kusto for TauGrid](../prepare-adx-kusto/). For
Portal deployment and its available boards, see
[Configure Portal](../enable-portal/).
