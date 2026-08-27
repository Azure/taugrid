---
title: What is TauGrid?
weight: 1
description: An open-source AI compute platform for Kubernetes infrastructure you control
aliases:
  - "/docs/overview/what-is-tau/"
---

TauGrid is an open, self-hosted AI compute platform for running AI workloads
on Kubernetes. It brings the experience teams expect from a managed AI
platform (simple submission, elastic distributed compute, shared GPU capacity,
experiment visibility, and operational controls) to clusters they operate in
their own cloud or datacenter.

Researchers work from a repository and the Tau CLI. Platform teams provide
governed workspaces, queues, compute profiles, storage, identity, and
observability. TauGrid connects those two experiences across training,
fine-tuning, batch inference, and serving.

## A complete path from code to compute

```text
repository -> target -> governed workspace -> queued compute -> results
```

The checked-in target describes the workload. TauGrid resolves platform policy,
renders a Kubernetes Job or KubeRay RayJob, submits it through shared queueing,
and provides one lifecycle for status, logs, results, cancellation, retry, and
resume.

## One platform for researchers and operators

- **Researchers move faster.** The Tau CLI turns repository configuration into
  repeatable runs without requiring every project to set up its own cluster access,
  queueing, GPU placement, storage, logs, and recovery.
- **Platform teams stay in control.** Kubernetes remains the source of truth
  for capacity, policy, identity, and workload state. Teams deploy TauGrid into
  their own environment and integrate the storage, networking, and cloud
  services they already operate.
- **Organizations keep their options open.** TauGrid builds on Kubernetes,
  Kueue, KubeRay, and standard container images. Workloads, platform policy,
  and operational evidence remain inspectable and portable.

## Integrated where it matters

TauGrid brings together:

- repository-driven workload configuration and the Tau CLI;
- Kueue admission, quotas, and shared GPU queues;
- KubeRay orchestration for distributed Ray workloads;
- workspace identity, service accounts, and storage contracts;
- GPU topology, placement profiles, and node health;
- run status, logs, retry, resume, and experiment evidence; and
- Portal views for researchers and platform operators.

Each layer keeps a clear operational owner: Kubernetes schedules pods, Kueue
admits workloads, Ray and model frameworks execute training, and TauGrid
ties them together into a single workflow.

Read [Architecture](../architecture/) for component ownership or
[Getting started on Kubernetes](../../platform-admin-guide/kubernetes/) for a
runnable path.
