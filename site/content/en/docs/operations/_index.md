---
title: Operations
linkTitle: Operations
weight: 6
description: Diagnose and operate Tau workloads and integrations
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Start here if a run is stuck or failed:
[**Troubleshooting by lifecycle layer**](troubleshooting/) is the canonical,
diagnose-first decision path -- work it in order instead of guessing which
layer owns the problem.

- [Troubleshooting by lifecycle layer](troubleshooting/) -- start here first
- [Observability and evidence](observability/)
- [Retry and resume](recovery/) -- only after the layer above identifies the failure
- [Multi-cluster execution](multicluster/) (experimental)
