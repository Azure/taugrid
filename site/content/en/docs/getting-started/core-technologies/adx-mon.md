---
title: What is adx-mon?
linkTitle: What is adx-mon?
weight: 3
description: How adx-mon sends Kubernetes and experiment telemetry to Azure Data Explorer
---

[adx-mon](https://github.com/Azure/adx-mon) is Azure Data Explorer Monitor, an
observability pipeline for Kubernetes clusters. It collects metrics, logs, and
GPU telemetry, batches the data, and sends it to Azure Data Explorer
(ADX/Kusto).

Its main components collect node and cluster signals, ingest batches into ADX,
manage ADX schema commands, and evaluate configured alerts. Platform teams
choose the signals, databases, retention, identities, and access rules.

## How TauGrid uses adx-mon

adx-mon is an optional platform integration. A TauGrid environment can use it
to:

- collect Kubernetes, Kueue, and GPU signals for fleet views;
- receive scalar experiment metrics from a TauGrid metrics sidecar;
- create approved tables, mappings, and functions through its
  `ManagementCommand` resource; and
- send data to ADX for dashboards, searches, and alerts across workspaces.

adx-mon moves data into ADX. The TauGrid Portal and Stellar query ADX through
their own read identities.

TauGrid also saves experiment evidence alongside each run's output files. That local evidence
supports retrieval and comparison, while the optional ADX copy supports
platform-wide analysis.

Platform owners can continue with
[Prepare ADX/Kusto for TauGrid](../../../platform-admin-guide/prepare-adx-kusto/)
and [Observability and evidence](../../../platform-admin-guide/observability/).
