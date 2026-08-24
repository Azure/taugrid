---
title: Enable lifecycle recorder
linkTitle: Record run history
weight: 40
description: Record TauGrid workload lifecycle observations to a prepared ADX/Kusto database
url: "/docs/platform-admin-guide/enable-lifecycle-recorder/"
aliases:
  - "/docs/tasks/platform/enable-lifecycle-recorder/"
---

{{< maturity status="alpha" reviewed="2026-08-26" >}}

The lifecycle recorder is an optional, standalone ADX/Kusto producer; Portal
is one optional consumer of its output. It runs one metadata-only `tau run history record` Deployment and
records observations of Jobs, RayJobs, and Kueue Workloads in one workload
namespace. Portal can optionally read the resulting rows as durable Ray
history, but the recorder is useful independently of Portal.

## Prepare the namespace, adx-mon, and identity

Before enabling the recorder:

1. For a manual Helm or `tau cluster install` workflow, create the workload
   namespace first, usually by creating its TauWorkspace; the recorder chart
   intentionally expects `targetNamespace` to already exist. The repository's
   [full-cluster Terraform workflow](../../examples/full-cluster/) instead
   bootstraps that namespace before installing TauGrid. It can apply a
   configured `bootstrap_workspace` later in the same apply, or let a platform
   owner create a TauWorkspace that adopts and reconciles the namespace after
   the apply.
2. [Prepare ADX/Kusto](../prepare-adx-kusto/), deploy adx-mon, and grant its
   identity the ADX database `Admin` role on `Metrics`. The TauGrid chart
   creates an adx-mon `ManagementCommand` for the lifecycle table, mapping, and
   function, so skip running `taugrid-portal exp kusto schema` manually for
   this deployment path.
3. Create a dedicated managed identity and federate its exact ServiceAccount
   subject:

   ```text
   system:serviceaccount:<system-namespace>:tau-lifecycle-recorder
   ```

4. Grant that identity the ADX database `Ingestor` role. This role is separate
   from Azure RBAC and Kubernetes RBAC. It is enough for queued ingestion;
   adx-mon, rather than the recorder, manages the table, mapping, and function.

The recorder's Kubernetes Role is namespace-scoped, permitting `get/list/watch`
only for Jobs, RayJobs, and Kueue Workloads in `targetNamespace`: metadata-only
access that leaves workload storage, logs, and Secrets untouched.
When `schemaManagement.enabled=true`, the chart additionally creates a Role in
`schemaManagement.namespace` which can only `get/list/watch` the one named
`ManagementCommand`. That read-only permission is used by the init container
to wait for schema readiness; it is strictly limited to that one object, well
short of write access to adx-mon objects or ADX database-management access.

## Merge recorder settings into the canonical platform values

This is an intentionally non-standalone **merge fragment**. Merge it into the
same complete, reviewed `<platform-values.yaml>` that configures the TauGrid
umbrella release. Keep it there rather than saving it as a small independent
values file: `tau
cluster install` resets Helm release values during upgrades, so supplying only
an overlay can remove resources from previously enabled components.

The chart accepts only the released Tau CLI image on MCR. Pin the released image by
tag, or set `tag: ""` and provide its immutable digest.

Use a release that includes the recorder's current queued-ingestion contract
and lifecycle schema management. Schema management is deliberately opt-in:
the configuration below enables it explicitly. It creates the named
`TauExpRunLifecycleMapping` through adx-mon, and the recorder sends an
idempotent ADX extent tag with each batch. Chart and image must be released
together, so keep the released image and Deployment as shipped rather than
substituting an arbitrary private image or hand-editing the Deployment as a
production workaround.

```text
# Merge into the existing <platform-values.yaml> as a partial fragment.
# Preserve components, baselineQueue, Kueue/KubeRay/controller settings, and
# every existing taugrid-core service configuration.
taugrid-core:
  lifecycleRecorder:
    enabled: true
    namespace: <system-namespace>
    targetNamespace: <workspace-namespace>
    cluster: <aks-name>
    workspaceId: <workspace-name>
    kusto:
      endpoint: https://<adx>.<region>.kusto.windows.net
      database: Metrics
      table: TauExpRunLifecycle
    schemaManagement:
      enabled: true
      namespace: <adx-mon-namespace>
      resourceName: taugrid-lifecycle-schema
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

# adx-mon reconciles ManagementCommands asynchronously, exposing no
# completion condition, so the recorder starts immediately and retries its
# first ingestion until the schema is available. Wait for the Deployment to
# become Ready, then inspect adx-mon if it remains NotReady.
kubectl -n <system-namespace> rollout status \
  deploy/tau-lifecycle-recorder --timeout=25m
kubectl -n <adx-mon-namespace> describe managementcommand \
  taugrid-lifecycle-schema
kubectl -n <system-namespace> logs deploy/tau-lifecycle-recorder --tail=100
```

If the recorder remains NotReady, inspect the command and adx-mon logs, then
correct its ADX role, database configuration, or network path. Automatic schema
management currently supports only the adx-mon configured `Metrics` database
and the canonical `TauExpRunLifecycle` table.

> **Known limitation:** adx-mon does not currently publish a reliable success or
> failure status for `ManagementCommand`. The recorder therefore cannot wait for
> schema creation before it starts. It retries ingestion until the schema is
> available, but a short-lived workload that is deleted during this initial
> window can be absent from lifecycle history. This chart will restore a schema
> readiness gate after adx-mon publishes that completion contract.

Submit a workload in `targetNamespace`, then query the configured ADX database:

```kusto
TauExpRunLifecycle
| where cluster == "<aks-name>"
| order by observed_at desc
| take 20
```

Rows appear after the recorder's polling interval and ADX queued-ingestion
latency. Check the ManagementCommand, recorder logs, and managed identities'
database roles if no rows appear. To expose these rows in Portal, follow
[Configure Portal](../enable-portal/) and set its `portal.runHistory.enabled` only
after recorder writes succeed.

For an ADX-side diagnosis that distinguishes ingestion from Kubernetes
discovery, a schema administrator can inspect recent ingestion failures:

```kusto
.show ingestion failures
| where FailedOn > ago(1h)
| where Database == "Metrics" and Table == "TauExpRunLifecycle"
| project FailedOn, ErrorCode, Details
| order by FailedOn desc
```
