---
title: Experiment evidence and artifacts
weight: 5
description: Local-first run packets with optional hosted scalar projections
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau treats durable evidence as part of the workflow contract:

- Run metadata and immutable input references.
- Scalar metric history and summaries.
- Checkpoints and model outputs.
- Images, tables, profiles, and reports.
- Retry, resume, and terminal-state records.
- Immutable run IDs and the logical-to-physical workload-name mapping.

Expstore is the authoritative local or shared packet for experiment state.
ADX/Kusto is an optional hosted scalar projection for fleet queries and
dashboards. A dashboard is a consumer, not the only copy of artifacts or
recovery state.

Tau never replaces a completed Job or RayJob to reuse a config name. Archiving
marks terminal Kubernetes evidence in place; deleting it is a separate,
explicit, ownership-checked lifecycle action.

The Python `tau.stellar` API, JSONL/TensorBoard/W&B importers, metrics offload,
and hosted Stellar UI all meet at this evidence contract. Stellar and the
observability portal are served by the separate `taugrid-portal` binary, not by
`tau`.

See [Observability](../../operations/observability/) for platform telemetry and
[Retry and resume](../../operations/recovery/) for recovery state.
