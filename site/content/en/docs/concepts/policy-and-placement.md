---
title: Queue, quota, topology, and GPU placement
weight: 4
description: How policy intent becomes admitted and scheduled pods
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau resolves workload policy; upstream systems enforce it.

| Stage | Owner | Decision |
|---|---|---|
| Target resolution | Tau | Requested workers, GPUs, placement, priority, and workspace defaults |
| Queue admission | Kueue | Whether shared quota may be consumed |
| Pod scheduling | Kubernetes | Which nodes satisfy resources, selectors, taints, and topology |
| Device allocation | Device plugin or DRA | Which concrete GPUs are assigned |
| Node scaling | Cluster infrastructure | Whether matching node capacity can appear |

LocalQueues are tenant-facing entry points. ClusterQueues own quota and fairness.
ResourceFlavors describe resource pools. Priority and preemption remain
cluster-owned policy.

A successful preflight is not a capacity reservation. Capacity can change before
admission or scheduling.
