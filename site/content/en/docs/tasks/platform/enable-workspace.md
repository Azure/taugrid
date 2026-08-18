---
title: Enable a workspace
weight: 1
description: Complete platform Day 0 before repository handoff
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

TauWorkspace is a Kubernetes reconciler, not a cloud provisioner. It never provisions Azure resources, the AKS cluster, node pools, network, Kueue, KubeRay, Azure RBAC, storage accounts, or managed identities.

Before handoff, the platform:

1. Completes [AKS setup](../../getting-started/aks-setup/) so the provider-owned cluster, network, node pools, identity, registry, storage service, GPU enablement, and CSI capabilities are ready.
2. Completes [TauGrid setup](../../getting-started/taugrid-setup/) so `tau cluster validate installation --context <context>` exits `0`. Fresh clusters use the released MCR distribution; an existing managed cluster can instead supply equivalent independently owned components.
3. Provisions the platform storage contract separately. TauGrid does not create the Azure storage service, StorageClass, or durable PVC.
4. Declares one TauWorkspace per workspace through the platform's reviewed Helm/Kustomize/GitOps path. The controller creates and reconciles derived Namespace metadata, RBAC, LocalQueue, and an optional workload-identity ServiceAccount:

   ```bash
   kubectl apply -f workspace.yaml
   ```

   The direct command is the fresh-cluster equivalent; ArgoCD customers commit the same native object.

5. Waits for the [workspace readiness gate](../../reference/workspace/#workspace-readiness-and-recovery) to report `Ready`:

   ```bash
   tau workspace status <workspace> --context <context>
   tau workspace check <workspace> --context <context>
   ```

6. Produces the non-secret `tau/workspace.connection.yaml` and proves a bounded smoke path. See the [handoff checklist](../handoff/) for the exact artifact, validation commands, and first smoke step.

Only then should the platform hand the researcher a repository URL and network instructions. The TauWorkspace contract itself stays provider-neutral even though the automatic connection bootstrap documented here is AKS-specific today.
