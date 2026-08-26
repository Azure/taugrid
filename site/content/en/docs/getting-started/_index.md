---
title: Getting started
linkTitle: Getting started
weight: 1
description: Understand TauGrid's purpose, design principles, and paths for developers and platform teams
sidebar_root_for: self
---

TauGrid is an open, self-hosted AI compute platform for teams that run AI
workloads on Kubernetes. It gives developers a consistent path from a
repository to scheduled compute and saved results. It gives platform teams a
consistent way to provide workspaces, share GPUs, apply policy, and understand
what is happening on the cluster.

## The idea behind TauGrid

Developers should be able to submit training, tuning, batch inference, and
serving workloads without rebuilding the same cluster integration for every
project. Platform teams should be able to operate shared compute without
creating a separate workflow for every research team.

TauGrid connects those needs:

```text
repository -> tau run -> governed workspace -> shared compute -> saved evidence
```

The repository describes the workload. The workspace supplies the namespace,
queue, identity, storage, and platform policy. Kueue admits work when quota is
available. Kubernetes places pods on healthy capacity. TauGrid keeps the
developer experience consistent across that path.

## Design principles

- **Build on Kubernetes.** Cluster state, scheduling, identity, and workload
  resources remain visible through standard Kubernetes APIs.
- **Keep workload intent with the code.** A checked-in target describes how a
  project runs, which makes submissions repeatable across developers and
  environments.
- **Share accelerators through queues.** Kueue admission and platform-owned
  quotas make waiting, priority, and GPU allocation explicit.
- **Give each system a clear job.** TauGrid handles the workflow, Kueue handles
  admission, Kubernetes handles placement, and Ray or the application handles
  execution.
- **Save evidence as part of the run.** Status, logs, metrics, and artifacts
  help teams compare work, diagnose failures, and promote successful results.
- **Support the infrastructure teams already operate.** TauGrid uses
  Kubernetes contracts for compute, storage, networking, and identity across
  cloud and datacenter environments.

## Choose your path

- **Developers and researchers:** start with the
  [Developer guide](../developer-guide/) after receiving a workspace.
- **Platform owners:** use the
  [Platform admin guide](../platform-admin-guide/) to install TauGrid and
  prepare that workspace.

Continue with [What is TauGrid?](what-is-tau/) for the product model,
[Architecture](architecture/) for component ownership, or
[Core technologies](core-technologies/) for Ray, Kueue, adx-mon, and Stellar.
