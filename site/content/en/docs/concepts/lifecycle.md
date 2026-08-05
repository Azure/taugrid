---
title: Run and workload lifecycle
weight: 2
description: Independent transitions from repository resolution to useful progress
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

A Tau run crosses independent control planes:

1. **Resolution:** Tau finds the project, workspace, and target.
2. **Rendering:** Tau validates intent and builds a Job or RayJob.
3. **Admission:** Kueue reserves quota and admits the workload.
4. **Scheduling:** Kubernetes places pods on matching nodes and devices.
5. **Runtime:** Ray or the Job controller starts the application.
6. **Progress:** The model or data process advances and writes evidence.
7. **Completion:** Results, checkpoints, metrics, and terminal state remain inspectable.

These states are not equivalent:

- Admitted does not mean scheduled.
- Scheduled does not mean the GPU is healthy.
- Running does not mean the model is progressing.
- Completed does not prove required artifacts were preserved.

Use the [layered troubleshooting guide](../../operations/troubleshooting/) to
find the first failed transition.
