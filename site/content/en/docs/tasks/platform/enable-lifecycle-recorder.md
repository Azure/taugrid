---
title: Enable lifecycle recorder
weight: 4
description: Record Tau workload lifecycle observations to a prepared ADX/Kusto database
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

The lifecycle recorder is an optional ADX/Kusto producer, not a Portal
component. It runs one metadata-only `tau run history record` Deployment and
records observations of Jobs, RayJobs, and Kueue Workloads in one workload
namespace. Portal can optionally read the resulting rows as durable Ray
history, but the recorder is useful independently of Portal.

## Prepare the namespace, schema, and identity

Before enabling the recorder:

1. Create the workload namespace. Usually, create its TauWorkspace first. The
   recorder chart deliberately does not create `targetNamespace`.
2. [Prepare ADX/Kusto](../prepare-adx-kusto/) and execute the lifecycle schema
   KQL against the selected database. The recorder does not create its table,
   mapping, or function at startup.
3. Create a dedicated managed identity and federate its exact ServiceAccount
   subject:

   ```text
   system:serviceaccount:<control-plane-namespace>:tau-lifecycle-recorder
   ```

4. Grant that identity the ADX database `Ingestor` role. This role is separate
   from Azure RBAC and Kubernetes RBAC. It is enough for queued ingestion; use
   a separate schema principal for table, mapping, and function management.

The recorder's Kubernetes Role is namespace-scoped and permits only
`get/list/watch` for Jobs, RayJobs, and Kueue Workloads in `targetNamespace`.
It does not mount workload storage or read workload logs or Secrets.

## Merge recorder settings into the canonical platform values

This is an intentionally non-standalone **merge fragment**. Merge it into the
same complete, reviewed `<platform-values.yaml>` that configures the TauGrid
umbrella release. Do not save it as a small independent values file: `tau
cluster install` resets Helm release values during upgrades, so supplying only
an overlay can remove resources from previously enabled components.

The chart accepts only the released Tau image on MCR. Pin the released image by
tag, or set `tag: ""` and provide its immutable digest.

```text
# Merge into the existing <platform-values.yaml>; not a complete Helm values file.
# Preserve components, baselineQueue, Kueue/KubeRay/controller settings, and
# every existing taugrid-core service configuration.
taugrid-core:
  lifecycleRecorder:
    enabled: true
    namespace: <control-plane-namespace>
    targetNamespace: <workspace-namespace>
    cluster: <aks-name>
    workspaceId: <workspace-name>
    kusto:
      endpoint: https://<adx>.<region>.kusto.windows.net
      database: Metrics
      table: TauExpRunLifecycle
    image:
      repository: mcr.microsoft.com/aks/ai-runtime/tau
      tag: <released-tag>
      # Alternatively: tag: "" and digest: sha256:<released-digest>
    workloadIdentity:
      enabled: true
    serviceAccount:
      create: true
      name: tau-lifecycle-recorder
      annotations:
        azure.workload.identity/client-id: <recorder-writer-client-id>
    rbac:
      create: true
```

The chart adds the required `azure.workload.identity/use: "true"` Pod label.
The federated credential subject must match both namespaces and the ServiceAccount
name exactly.

## Install and verify

Upgrade the single umbrella release with the complete canonical values file:

```bash
tau cluster install --context <context> --version <taugrid-release-version> \
  --values <platform-values.yaml>

kubectl -n <control-plane-namespace> rollout status \
  deploy/tau-lifecycle-recorder --timeout=180s
kubectl -n <control-plane-namespace> logs deploy/tau-lifecycle-recorder --tail=100
```

Submit a workload in `targetNamespace`, then query the configured ADX database:

```kusto
TauExpRunLifecycle
| where cluster == "<aks-name>"
| order by observed_at desc
| take 20
```

Rows appear after the recorder's polling interval and ADX queued-ingestion
latency. Check the recorder logs and the managed identity's database role if no
rows appear. To expose these rows in Portal, follow [Enable Portal](../enable-portal/)
and set its `portal.runHistory.enabled` only after recorder writes succeed.
