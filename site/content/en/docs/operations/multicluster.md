---
title: Multi-cluster execution
weight: 4
description: Experimental manager and worker dispatch with explicit dependencies
---

{{< maturity status="experimental" reviewed="2026-07-16" >}}

The MultiKueue direction introduces a manager cluster and explicitly enabled
worker clusters. Dispatch is safe only when a worker satisfies the workload
dependency contract:

- Compatible Kubernetes, Kueue, KubeRay, and CRD versions.
- Required queue, ServiceAccount, Secret, StorageClass, and PVC names.
- Pullable pinned images.
- Dataset and checkpoint locality.
- Required GPU, topology, network, and storage capabilities.

Current boundaries:

- Preflight and worker enablement remain operator surfaces.
- Worker credentials are a platform security boundary.
- Automatic dataset replication is not shipped.
- Running pods and node-local scratch do not migrate.
- General any-worker routing remains experimental.

Do not remove cluster pins from stateful workloads until common storage and
identity contracts are activated and verified on every eligible worker.
