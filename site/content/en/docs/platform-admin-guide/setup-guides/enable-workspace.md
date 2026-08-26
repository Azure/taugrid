---
title: Enable a workspace
linkTitle: Set up a workspace
weight: 10
description: Prepare a Kubernetes workspace for researchers
url: "/docs/platform-admin-guide/enable-workspace/"
aliases:
  - "/docs/tasks/platform/enable-workspace/"
---

{{< maturity status="ga" reviewed="2026-08-25" >}}

Before researchers can submit workloads, the platform team prepares the
cluster and creates a TauWorkspace. TauGrid then keeps the workspace namespace,
queue access, and Kubernetes permissions in the expected state.

## Prepare the workspace

1. [Prepare the Kubernetes cluster](../kubernetes/#2-prepare-a-kubernetes-cluster).
   Set up the network, node pools, identity, container registry, storage, GPU
   support, and CSI drivers required by your workloads.
2. [Install TauGrid](../kubernetes/#3-install-taugrid), then verify the
   installation:

   ```bash
   tau cluster validate installation --context <context>
   ```

   A successful validation exits with code `0`.

   The standard installation uses the released TauGrid images and charts from
   Microsoft Container Registry. Platform teams with existing Kueue, KubeRay,
   and monitoring installations can connect TauGrid to those components when
   they meet the same requirements.
3. Create durable storage for the workspace. This includes the cloud storage
   service, Kubernetes StorageClass, and PVC used for datasets, checkpoints,
   and run output.
4. Create the TauWorkspace through your usual Helm, Kustomize, or GitOps
   process. TauGrid prepares the namespace, Kubernetes permissions, LocalQueue,
   and optional workload identity ServiceAccount:

   ```bash
   kubectl apply -f workspace.yaml
   ```

   For GitOps, commit the same Kubernetes object to the repository managed by
   your deployment system.

5. Wait for the workspace to become Ready:

   ```bash
   tau workspace status <workspace> --context <context>
   tau workspace check <workspace> --context <context>
   ```

   See [Workspace readiness and recovery](../../reference/workspace/#workspace-readiness-and-recovery)
   if either command reports a problem.
6. Create `tau/workspace.connection.yaml` and run the smoke test in the
   [handoff checklist](../handoff/). This file contains workspace details and
   can be safely committed to the research repository.

After these checks pass, send the researcher the repository URL and access
instructions. TauWorkspace works with Kubernetes clusters across providers.
The current automatic connection setup also includes an AKS-specific path.
