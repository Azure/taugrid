---
title: Operate source bundles
weight: 3
description: Prepare storage, RBAC, and retention for immutable project archives
---

{{< maturity status="shipped" reviewed="2026-08-07" >}}

Source bundles let researchers submit a local project tree as a deterministic,
content-addressed zip. They are stored on the workspace data PVC, not in the
Kubernetes workload object.

## Prerequisites

The workspace data PVC must be writable by the workload identity. A researcher
can explicitly select it with `storage.data_pvc`; when source-bundle mode omits
that field, Tau uses its bundle-mode data-PVC default. Multi-node RayJobs mount
the same bundle from every node, so their data PVC must support
`ReadWriteMany`. The default Blob-backed `blob-training` PVC supports this.

The workspace role must also be allowed to:

- create, get, list, watch, and delete Pods; and
- create the Pods `exec` subresource.

Tau creates that helper Pod, sends the archive only over `kubectl exec` stdin,
verifies the SHA-256, and removes the helper Pod after staging. A missing one
of these permissions prevents staging before the workload is submitted. The
workload image must provide `python3`; Tau uses that image to verify and, for
Jobs, extract the bundle before execution.

## Storage and lifecycle

The durable bundle location is:

```text
/data/tau/source-bundles/sha256/<hex>.zip
```

The helper reuses a verified matching digest, so repeated runs do not upload
the same bytes. It rejects a path that exists with a different digest. Job and
RayJob workloads independently verify the requested digest and safely extract
the zip into per-workload runtime storage. Job extraction storage is
`emptyDir`, so it disappears with the Job; the durable digest zip is shared and
is **not** deleted when any run completes.

Configure retention or garbage collection for the
`/data/tau/source-bundles/sha256/` prefix. GC must avoid deleting digests still
referenced by live or retained workloads. Treat digest zips as shared
content-addressed objects, not as one-run scratch files.

Tau bounds helper execution to three minutes and normally deletes the Pod. If
the CLI is killed before cleanup runs, the stopped Pod object can remain.
Periodically remove completed Pods labeled
`app.kubernetes.io/name=tau-source-bundle-stage`; deleting them does not delete
the staged bundle.

For researcher configuration, size limits, private-repository behavior, and
digest pins, see [Ship an immutable source bundle](../../reference/run-config/#ship-an-immutable-source-bundle).
