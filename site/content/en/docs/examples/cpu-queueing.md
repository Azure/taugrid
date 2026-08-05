---
title: CPU queueing with Kueue and Ray
linkTitle: CPU queueing
weight: 20
description: Observe Kueue admission and quota behavior with raw RayJobs and no GPU quota.
---

{{< maturity status="experimental" reviewed="2026-07-16" >}}

Use this example when you want to understand the scheduling layer beneath Tau
without waiting for GPU capacity. It submits raw RayJobs to Kueue and makes
admission, pending work, and team borrowing visible.

This is a **platform mechanics example**, not a Tau researcher workflow.

## Prerequisites

- a disposable or explicitly approved Kubernetes context;
- Kueue and KubeRay installed;
- kubectl access that can create namespaces, queues, and RayJobs; and
- enough CPU capacity for two small Ray workloads.

## Run the single-queue demonstration

```bash
git clone https://github.com/Azure/taugrid-examples.git
cd taugrid-examples/aks-blog/kueue-ray-cpu

./demo.sh single
```

The script applies the single-queue resources and enqueues two raw RayJobs. In a
second terminal, watch admission:

```bash
kubectl get workloads -A --watch
kubectl get rayjobs -A
kubectl get pods -A
```

The demo includes a shared-context safety guard. Do not bypass it with
`--allow-shared-context` unless the target cluster is intentionally approved for
these resources.

## What to carry back to Tau

Tau adds reviewed configuration, workspace policy, queue resolution, and
lifecycle commands on top of the objects shown here. Use this example to
understand why a Tau run can be submitted but still wait for admission.

Continue with [queue, quota, and GPU placement](../../concepts/policy-and-placement/)
or the [operator troubleshooting path](../../tasks/operator/troubleshoot/).

See the
[complete example README](https://github.com/Azure/taugrid-examples/tree/main/aks-blog/kueue-ray-cpu)
for the team-borrowing mode and manual manifest path.
