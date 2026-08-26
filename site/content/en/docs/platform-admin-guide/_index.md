---
title: Platform admin guide
linkTitle: Platform admin guide
weight: 3
description: Install TauGrid, provision workspaces, and operate clusters
sidebar_root_for: self
aliases:
  - "/docs/tasks/platform/"
  - "/docs/tasks/operator/"
  - "/docs/operations/"
---

This guide is for platform owners and on-call operators who install TauGrid,
provision governed workspaces, and keep clusters healthy.

## Installation guides

- [Getting started on Kubernetes](kubernetes/): the canonical, provider-neutral
  installation path for any Kubernetes 1.30+ cluster.
- [Getting started on AKS](aks-setup/): the AKS-specific companion for
  provisioning or reusing an AKS cluster before following the Kubernetes guide.

## Setup guides

- [Enable a workspace](enable-workspace/)
- [Hand off a workspace](handoff/) to a researcher with a reproducible,
  non-secret checklist.
- [Prepare ADX/Kusto for TauGrid](prepare-adx-kusto/)
- [Enable lifecycle recorder](enable-lifecycle-recorder/)
- [Configure Portal](enable-portal/)
- [Enable single-workspace researcher access](single-workspace-researcher-access/)

## Operate

- [Identity and security boundaries](identity/)
- [Queue, quota, topology, and GPU placement](policy-and-placement/)
- [Observability and evidence](observability/)
- [Workload profile migration](workload-profiles/)
- [Multi-cluster execution](multicluster/) (Alpha)

## Troubleshooting

A run stuck or failed? Start with
[Troubleshoot a run](troubleshoot/), the same canonical, layer-by-layer
decision path used across these pages to identify which layer owns the
problem.

- [Troubleshoot a run](troubleshoot/) -- start here first
- [Troubleshooting by lifecycle layer](troubleshooting/)
- [Retry and resume](recovery/) -- only after the layer above identifies the
  failure

## Related

- Look up exact contracts in the [Reference](../reference/) section, such as
  the [install values reference](../reference/cluster-install-values/) and
  [workspace API reference](../reference/workspace/).
- Point developers to the [Developer guide](../developer-guide/) once a
  workspace is ready.
