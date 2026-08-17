---
title: Enable a workspace
weight: 1
description: Complete platform Day 0 before repository handoff
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

TauWorkspace is a Kubernetes reconciler, not a cloud provisioner. See the
full [TauGrid setup matrix](../../getting-started/taugrid-setup/) for what
layer owns what — TauWorkspace never provisions Azure resources, the AKS
cluster, node pools, network, Kueue, KubeRay, Azure RBAC, storage accounts,
or managed identities.

Before handoff, the platform:

1. Completes [AKS setup](../../getting-started/aks-setup/),
   then provisions or selects the cluster, network, node pools, registry, and
   data services (Azure/provider layer - outside Tau).
2. Installs the
   [in-cluster platform components](../../getting-started/taugrid-setup/#platform-component-availability):
   Kueue, KubeRay, the Tau workspace controller, and baseline queue policy.
   Fresh clusters use the TauGrid distribution; existing managed clusters use
   independent ArgoCD Applications. GPU monitoring and adx-mon are optional.
3. Installs TauGrid on a fresh cluster (or syncs the equivalent ArgoCD
   Applications) and provisions platform storage separately:

   ```bash
   tau cluster install --version <version> --values taugrid-values.yaml
   ```

   See the [cluster install values reference](../../reference/cluster-install-values/)
   for configurable fields, or run `tau cluster explain-values`.

4. Declares one TauWorkspace per workspace through the platform's reviewed
   Helm/Kustomize/GitOps path. The controller creates and reconciles derived
   Namespace metadata, RBAC, LocalQueue, and optional workload-identity
   ServiceAccount:

   ```bash
   kubectl apply -f workspace.yaml
   ```

   The direct command is the fresh-cluster equivalent; ArgoCD customers commit
   the same native object.

5. Waits for the [readiness gate](../../getting-started/taugrid-setup/#tauworkspace-readiness-gate)
   to report `Ready`:

   ```bash
   tau workspace status <workspace> --context <context>
   tau workspace check <workspace> --context <context>
   ```

6. Produces the non-secret `tau/workspace.connection.yaml` and proves a
   bounded smoke path — see the
   [handoff checklist](../handoff/) for the exact artifact, validation
   commands, and first smoke step.

Only then should the platform hand the researcher a repository URL and network
instructions.

The Azure subscription guide shows Terraform, Bicep, ARM JSON, CLI, Portal, and
existing-cluster paths for steps 1-2. The TauWorkspace contract itself stays
provider-neutral.
