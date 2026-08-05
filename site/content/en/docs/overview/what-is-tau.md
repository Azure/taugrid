---
title: What is Tau?
weight: 1
description: Tau's purpose, value, and explicit boundaries
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau turns checked-in workload intent into policy-aware Kubernetes Jobs or
KubeRay RayJobs and provides one lifecycle surface for the resulting run.

```text
repository -> project -> target -> run -> Job | RayJob
                          ^
workspace policy --------|
```

## Why teams use it

Research repositories repeatedly need the same integration plumbing: cluster
access, queues, GPU and topology requests, storage mounts, status, logs,
artifacts, retry, and experiment evidence. Tau makes those choices inspectable
and repeatable without introducing a closed scheduler or training framework.

## What Tau owns

- Deterministic project, workspace, and target resolution.
- Configuration validation and dry-run output.
- Job and RayJob rendering and submission.
- Run status, logs, results, cancel, retry, and resume.
- Local-first experiment and artifact contracts.

## What Tau does not own

- Cloud or AKS resource provisioning.
- Kubernetes pod scheduling or Kueue quota decisions.
- Ray Train, PyTorch, or model-framework behavior.
- GPU quota, cluster authorization, or weak-isolation remediation.
- Project data preparation, model code, or serving application semantics.

Read [Architecture](../architecture/) for component ownership or
[Getting started](../../getting-started/) for a runnable path.
