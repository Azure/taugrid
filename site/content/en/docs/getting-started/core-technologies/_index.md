---
title: Core technologies
linkTitle: Core technologies
weight: 3
description: Understand the systems TauGrid brings together
---

TauGrid brings several focused systems into one workflow. Each one owns a
different part of the path from submission to saved results.

| Technology | Role in TauGrid |
|---|---|
| [Ray](ray/) | Runs distributed Python, training, tuning, and serving workloads |
| [Kueue](kueue/) | Decides when a workload can use shared cluster quota |
| [adx-mon](adx-mon/) | Optionally sends cluster and experiment telemetry to Azure Data Explorer |
| [Stellar](stellar/) | Helps teams inspect and compare experiment evidence |

Developers mainly interact with TauGrid and Stellar. Platform teams operate
Kueue, KubeRay, and optional adx-mon integrations.
