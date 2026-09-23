---
title: Prepare ADX/Kusto for TauGrid
linkTitle: Set up ADX/Kusto
weight: 30
description: Provision and authorize the optional Azure Data Explorer data plane used by TauGrid integrations
url: "/docs/platform-admin-guide/prepare-adx-kusto/"
aliases:
  - "/docs/tasks/platform/prepare-adx-kusto/"
---

{{< maturity status="alpha" reviewed="2026-08-17" >}}

ADX/Kusto is an optional platform data service. Platform teams provision its
cluster, databases, Entra identities, federation, and database roles.
Prepare those platform resources once; Portal, lifecycle recorder, and adx-mon are
separate consumers. Their released charts manage the TauGrid/adx-mon schema
objects that they own.

## Provision the service

Choose an approved region, SKU, capacity, network path, retention policy, and
cost owner. This CLI shape illustrates the required flags; choose SKU,
capacity, and other values to match your own environment:

```bash
az extension add --name kusto
az kusto cluster create --resource-group <resource-group> \
  --name <adx-cluster-name> --location <region> \
  --sku name=<approved-sku> tier=<approved-tier> capacity=<instance-count> \
  --enable-streaming-ingest false
for db in Metrics Logs CostTracking Audit; do
  az kusto database create --resource-group <resource-group> \
    --cluster-name <adx-cluster-name> --database-name "$db" \
    --read-write-database location=<region> \
    soft-delete-period=P31D hot-cache-period=P7D
done
```

Current Azure CLI Kusto extensions reject `kind=ReadWrite`; a read-write
database is implied instead by `--read-write-database`.

`Metrics` is needed by Portal Kusto views and lifecycle history. `Logs`,
`CostTracking`, and `Audit` are required only for the corresponding adx-mon
pipelines.

## Create identities and grant roles

For AKS, federate each exact ServiceAccount to a managed identity. Workload
Identity obtains an Entra token; granting ADX database access is a separate
role-assignment step below.

```bash
az identity federated-credential create --resource-group <resource-group> \
  --identity-name <identity-name> --name <credential-name> \
  --issuer <aks-oidc-issuer> \
  --subject system:serviceaccount:<namespace>:<service-account> \
  --audiences api://AzureADTokenExchange
```

An authorized ADX administrator must separately grant database roles. In
production, use distinct principals: Portal reader (`Viewer`), lifecycle
recorder writer (`Ingestor`), and adx-mon identity (currently ADX database
`Admin` for the databases whose schema it reconciles). Azure RBAC, Kubernetes
RBAC, and ADX database roles are independent.

For example, an ADX administrator can grant the two least-privilege consumer
roles in PowerShell or another approved Azure CLI environment:

```powershell
$tenant = az account show --query tenantId -o tsv

az kusto database-principal-assignment create `
  --resource-group <resource-group> --cluster-name <adx-cluster-name> `
  --database-name Metrics --principal-assignment-name taugrid-portal-viewer `
  --principal-id <portal-managed-identity-client-id> --principal-type App `
  --role Viewer --tenant-id $tenant

az kusto database-principal-assignment create `
  --resource-group <resource-group> --cluster-name <adx-cluster-name> `
  --database-name Metrics --principal-assignment-name taugrid-recorder-ingestor `
  --principal-id <recorder-managed-identity-client-id> --principal-type App `
  --role Ingestor --tenant-id $tenant
```

Use a stable, unique assignment name per database/principal/role. These
commands grant ADX data-plane access only; create the federated
credential and Kubernetes RBAC through the separate steps above.

## Let the platform charts manage producer schemas

Let the platform charts manage TauGrid lifecycle tables, mappings, and
functions automatically; treat
`taugrid-portal exp kusto schema --ingestion lifecycle` as a
development/release artifact generator rather than a platform deployment
prerequisite. It produces KQL text for review, not for direct execution
against ADX.

For a release that includes lifecycle schema management, enable adx-mon first
and grant its identity the ADX database `Admin` role on `Metrics`. Explicitly
enable the lifecycle recorder and schema management together. The schema
template is intentionally gated by both
`lifecycleRecorder.enabled=true` and
`lifecycleRecorder.schemaManagement.enabled=true`; this chart does not expose
a schema-only mode. The recorder also requires an existing target namespace,
cluster label, ADX endpoint, dedicated ServiceAccount, workload identity, and
read-only workload RBAC:

```yaml
lifecycleRecorder:
  enabled: true
  targetNamespace: <workspace-namespace>
  cluster: <cluster-name>
  kusto:
    endpoint: https://<cluster>.<region>.kusto.windows.net
    database: Metrics
    table: TauExpRunLifecycle
  workloadIdentity:
    enabled: true
  serviceAccount:
    create: true
    name: tau-lifecycle-recorder
    annotations:
      azure.workload.identity/client-id: <recorder-ingestion-client-id>
  rbac:
    create: true
  schemaManagement:
    enabled: true
    namespace: <adx-mon-namespace>
    resourceName: taugrid-lifecycle-schema
```

These values create the recorder Deployment plus an adx-mon
`ManagementCommand` that idempotently creates or updates
`Metrics.TauExpRunLifecycle`, its named JSON mapping, and
`TauExpRunLifecycleDashboardRows()`. The recorder identity retains only the
`Ingestor` role; the separate adx-mon identity executes the schema command.
Render the exact objects before deployment:

```bash
helm template taugrid-core charts/taugrid-core \
  --namespace tau-system \
  --values <lifecycle-values.yaml> |
  yq 'select(.kind == "ManagementCommand" or
    (.kind == "Deployment" and .metadata.name == "tau-lifecycle-recorder"))'
```

Check that automation before enabling a Kusto-backed Portal capability:

```bash
kubectl -n <adx-mon-namespace> get managementcommand \
  <lifecycle-schema-resource-name> \
  -o jsonpath='{.status.conditions[0].status}{" "}{.status.conditions[0].reason}{"\n"}'
```

Wait for a successful status. adx-mon v0.3.0 reconciles commands every 10
minutes. The recorder starts immediately and retries ingestion until the
schema is available; use `kubectl rollout status deploy/tau-lifecycle-recorder
--timeout=25m` to wait for it to become Ready. Set
`lifecycleRecorder.schemaManagement.enabled=false` only when an existing
platform-owned automation already manages the identical lifecycle contract.

Stellar uses scalar metric tables supplied by its selected ingestion path
(commonly adx-mon remote-write `Metrics.ExperimentMetrics`). For adx-mon,
follow its published chart guide and enable Metrics/Logs table precreation
before broad collection to avoid ADX control-plane throttling.

The standalone `collector-v1` runtime sends canonical
`tau.experiment.metric.v1` chunks through queued ADX ingestion. Prepare the
`Metrics.TauExpMetricEventsV1` table, its
`TauExpMetricEventsV1Json` mapping, and the stable
`TauExpMetricEventRows()` function through adx-mon before enabling workloads.
Canonical Portal v2 discovery and series reads require the typed catalog
Functions and do not fall back to raw `ExperimentMetrics`.

Enable the complete typed contract in the adx-mon release values:

```yaml
managementCommands:
  typedMetricEventsV1:
    enabled: true
  experimentCatalogV1:
    enabled: true
functions:
  items:
    tauExpMetricEventRows:
      enabled: true
    tauExpSeriesCatalogRows:
      enabled: true
    tauExpRunCatalogRows:
      enabled: true
```

Apply the lifecycle schema from `taugrid-core` with the complete recorder values
above, or leave both recorder/schema switches disabled and provision the
identical `TauExpRunLifecycle` contract through platform-owned automation. Wait
for both adx-mon ManagementCommands and the lifecycle schema command to succeed
before routing Portal traffic or enabling collector sidecars. Verify the
rendered objects and ADX assets:

```bash
kubectl -n <adx-mon-namespace> get managementcommands,functions
kubectl -n <adx-mon-namespace> get managementcommand \
  <adx-mon-release>-typed-metric-events-v1 \
  <adx-mon-release>-experiment-catalog-v1

# Run in the Metrics database with an authorized read-only ADX client.
.show tables | where TableName in ("TauExpMetricEventsV1", "TauExpRunLifecycle")
.show functions | where Name in (
  "TauExpMetricEventRows",
  "TauExpSeriesCatalogRows",
  "TauExpRunCatalogRows"
)
```

Grant the workload identity used by the TauWorkspace ServiceAccount only the
ADX database/table ingestion role required for `TauExpMetricEventsV1`. The
collector uses an Azure Identity Workload Identity credential with the
non-secret user-assigned identity client ID; do not create a client secret or
grant the workload management-command permissions. The adx-mon operator, not
the workload, owns table, mapping, retention, batching, and materialized-view
commands.

Queued submission is not final ingestion acknowledgement. TauGrid's
`adx-queued-v1` sink requests final result reporting and waits for ADX to report
success before persisting its delivery receipt. Monitor final ingestion
failures, status latency/timeouts, throttling, and materialized-view health
for every workload using `adx-required`.

Release owners must publish immutable `tau`, `taugrid-portal`, and
`taugrid-metrics-collector` images from the same reviewed source stack. The
coordinated 0.4.3 chart train also references a publishable
`tau-core-controller:0.4.3` artifact even though this telemetry change does not
modify the controller binary's source dependencies. Hand consumers the
collector digest, tested ADX endpoint, database, ServiceAccount subject, and
non-secret identity client ID. The collector is a `tau run` workload sidecar,
not a Helm-managed Deployment; there is no chart-side runtime selector. Then
configure [Portal](../enable-portal/) or adx-mon.
