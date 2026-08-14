---
title: Prepare ADX/Kusto for TauGrid
weight: 3
description: Provision and authorize the optional Azure Data Explorer data plane used by TauGrid integrations
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

ADX/Kusto is an optional platform data service. TauGrid does not create its
cluster, databases, Entra identities, federation, or database roles. Prepare
those platform resources once; Portal, lifecycle recorder, and adx-mon are
separate consumers. Their released charts manage the TauGrid/adx-mon schema
objects that they own.

## Provision the service

Choose an approved region, SKU, capacity, network path, retention policy, and
cost owner. This CLI shape is not a universal SKU recommendation:

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

`kind=ReadWrite` is not accepted by current Azure CLI Kusto extensions; a
read-write database is implied by `--read-write-database`.

`Metrics` is needed by Portal Kusto views and lifecycle history. `Logs`,
`CostTracking`, and `Audit` are required only for the corresponding adx-mon
pipelines.

## Create identities and grant roles

For AKS, federate each exact ServiceAccount to a managed identity. Workload
Identity obtains an Entra token; it does not grant ADX database access.

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
commands grant ADX data-plane access only; they do not create the federated
credential or Kubernetes RBAC.

## Let the platform charts manage producer schemas

Do not manually create TauGrid lifecycle tables, mappings, or functions, and
do not make `taugrid-portal exp kusto schema --ingestion lifecycle` a platform
deployment prerequisite. That command remains a development/release artifact
generator; it does not execute KQL.

For a release that includes lifecycle schema management, enable adx-mon first
and grant its identity the ADX database `Admin` role on `Metrics`. Enabling
`lifecycleRecorder` then creates an adx-mon `ManagementCommand` that
idempotently creates or updates `Metrics.TauExpRunLifecycle`, its named JSON
mapping, and `TauExpRunLifecycleDashboardRows()`. The recorder itself retains
only the `Ingestor` role.

Check that automation before enabling a Kusto-backed Portal capability:

```bash
kubectl -n <adx-mon-namespace> get managementcommand \
  <lifecycle-schema-resource-name> \
  -o jsonpath='{.status.conditions[0].status}{" "}{.status.conditions[0].reason}{"\n"}'
```

Wait for a successful status. adx-mon reconciles commands periodically, so a
new command is not necessarily complete immediately after a Helm upgrade. Set
`lifecycleRecorder.schemaManagement.enabled=false` only when an existing
platform-owned automation already manages the identical lifecycle contract.

Stellar uses scalar metric tables supplied by its selected ingestion path
(commonly adx-mon remote-write `Metrics.ExperimentMetrics`). For adx-mon,
follow its published chart guide and enable Metrics/Logs table precreation
before broad collection to avoid ADX control-plane throttling.

Only hand consumers a tested endpoint, database, ServiceAccount subject, and
non-secret identity client ID. Then configure [Portal](../enable-portal/) or
adx-mon.
