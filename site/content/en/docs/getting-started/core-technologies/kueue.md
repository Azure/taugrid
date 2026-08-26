---
title: What is Kueue?
linkTitle: What is Kueue?
weight: 2
description: How Kueue shares quota and admits TauGrid workloads
---

[Kueue](https://kueue.sigs.k8s.io/) is a Kubernetes-native queue and admission
system for batch and AI workloads. It decides when a workload may consume
shared quota. After admission, the Kubernetes scheduler decides which nodes run
the pods.

## The main Kueue objects

- A `LocalQueue` is the queue a workspace submits to.
- A `ClusterQueue` combines quota across one or more LocalQueues.
- A `ResourceFlavor` describes a class of capacity, such as CPU nodes or a
  particular GPU class.
- A `Workload` records the resources a Job or RayJob requests and its admission
  state.

TauGrid resolves the workspace LocalQueue and adds it to the rendered workload.
Kueue then evaluates quota, priority, queue order, and resource flavors.

## What waiting means

A queued workload can be healthy even when no pods are running. Kueue keeps it
pending until the required quota becomes available. TauGrid shows that state in
`tau run status` so developers can distinguish queue waiting from pod startup
or application failure.

The default TauGrid queue uses Kueue `BestEffortFIFO`. Older eligible work
usually proceeds first, while priority and quota rules can change the order.
Platform owners can replace the baseline with explicit per-team quotas and GPU
flavors.

See [Queue, quota, topology, and GPU placement](../../../platform-admin-guide/policy-and-placement/)
or try the [CPU queueing example](../../../examples/cpu-queueing/).
