---
title: CPU queueing with Kueue and Ray
linkTitle: CPU queueing
weight: 20
description: Observe Kueue admission and quota behavior with raw RayJobs and no GPU quota.
---

{{< maturity status="alpha" reviewed="2026-11-18" >}}

CPU-only RayJobs can help explain the scheduling layer beneath TauGrid,
including the distinction between pending work, admission, and pod scheduling.

This is a **platform mechanics inspection guide**, distinct from the TauGrid
researcher workflow. It does not provide runnable workload or queue manifests.

## Prerequisites

- a disposable or explicitly approved Kubernetes context;
- Kueue and KubeRay installed;
- kubectl read access to Workloads, RayJobs, and Pods; and
- platform-approved CPU RayJob and queue manifests, with suitable CPU capacity.

## Inspect an approved demonstration

The previously linked external demonstration could not be retrieved during
review. Its clone and script instructions are not a verified runnable path.
Obtain approved manifests from your platform owner rather than relying on
those instructions from older revisions of this page.

After an approved workload is submitted, watch admission and pod state:

```bash
kubectl get workloads -A --watch
kubectl get rayjobs -A
kubectl get pods -A
```

Do not apply demonstration workloads or queue policy to a shared cluster
without explicit platform approval.

## What to carry back to TauGrid

TauGrid adds reviewed configuration, workspace policy, queue resolution, and
lifecycle commands on top of the objects shown here. Use this example to
understand why a TauGrid run can be submitted but still wait for admission.

Continue with [queue, quota, and GPU placement](../../platform-admin-guide/policy-and-placement/)
or the [operator troubleshooting path](../../platform-admin-guide/troubleshoot/).
